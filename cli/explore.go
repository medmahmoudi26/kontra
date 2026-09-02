// explore.go — `kontra explore <run>`: open ONE run's typed output in DuckDB, on the
// operator's own workstation, with no catalog or object-store credential on the box.
//
//	kontra explore <run>                     # one view per node + a local DuckDB shell
//	kontra explore <run> --sql "SELECT ..."   # one-shot, non-interactive
//	kontra explore <run> --ui                 # the DuckDB web UI (fetches its assets over the network)
//	kontra explore <run> --print-init         # just show the generated init script
//	kontra explore --catalog                  # cross-run, READ-ONLY, separately provisioned creds
//
// The orchestrator returns short-lived presigned GET URLs for THIS RUN'S parquet files only
// (control/orchestrator/src/data/explore.ts). Those URLs ARE the credential — object-level, not
// row-level, and time-boxed. So the generated init script is written 0600, removed on exit,
// and passed to DuckDB as a FILE and never inlined into argv: /proc/<pid>/cmdline is
// world-readable and would undo both.
//
// The one thing this command must never do is make FAILED output look like EMPTY output.
// Every node the run recorded appears in `kontra_nodes` — including the ones with no files —
// with its state and error text, and the CLI prints that table itself before DuckDB starts so
// it survives a workspace that fails to open.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
)

// Local posture for the operator's workstation (plan §3). memoryLimit bounds DuckDB's buffer
// manager, threads bounds its parallelism, and the spill is quota'd inside the session's own
// 0600 dir so a runaway sort fills that quota instead of the host's disk.
const (
	exploreDefaultTTL  = 900   // seconds; the server clamps the request to [60, 900]
	exploreRowLimit    = 500   // every generated query is bounded — no unbounded SELECT *
	exploreMemoryLimit = "2GB" // bounds DuckDB's buffer manager, NOT process RSS
	exploreThreads     = 2
	exploreTempQuota   = "2GB"
	exploreStaleAge    = time.Hour
	exploreDirPrefix   = "kontra-explore-"
	// exploreSpillDir doubles as the marker that identifies a workspace as ours; see
	// scavengeExploreDirs.
	exploreSpillDir = "duckdb-temp"
)

// --- wire types (control/orchestrator/src/data/explore.ts names, verbatim) ---

type exploreColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// exploreDataset is ONE ACTOR'S output in a dispatch — the grain an operator addresses, and
// what becomes a view. A dispatch that sharded an actor across five graph nodes is five nodes
// and ONE dataset; the node ids are folded in for diagnosis and never used to name anything.
type exploreDataset struct {
	Actor   string `json:"actor"`
	Version string `json:"version"`
	// Dt is the dispatch time (YYYY-MM-DDTHH-MM-SS), which is also the directory the output
	// lives under. It is how the operator addressed this — never a run UUID.
	Dt      string          `json:"dt"`
	State   string          `json:"state"`
	Rows    int64           `json:"rows"`
	Error   *string         `json:"error"`
	Columns []exploreColumn `json:"columns"`
	URLs    []string        `json:"urls"`
	Nodes   []string        `json:"nodes"`
	// View is the name to create — the actor name, minted server-side. The physical table is
	// `output.<actor>` under `output/<actor>/version=…/dt=…/`; a node id never appears.
	View string `json:"view"`
}

// exploreMaterialization is the run's typed-output progress — what makes "still finalizing"
// a number rather than a feeling.
type exploreMaterialization struct {
	Total    int   `json:"total"`
	Pending  int   `json:"pending"`
	Running  int   `json:"running"`
	Complete int   `json:"complete"`
	Failed   int   `json:"failed"`
	Rows     int64 `json:"rows"`
	Bytes    int64 `json:"bytes"`
}

type exploreManifest struct {
	RunID     string `json:"runId"`
	ExpiresAt int64  `json:"expiresAt"`
	// executing | finalizing | completed | output_failed (data/materialization.ts).
	Lifecycle       string                 `json:"lifecycle"`
	Execution       string                 `json:"execution"`
	Materialization exploreMaterialization `json:"materialization"`
	// RunStartedAt / Dt are how the run was ADDRESSED — actor@version at an hour. The run id
	// is carried for the API and for diagnosis; it is not something a person has to know.
	RunStartedAt int64  `json:"runStartedAt"`
	Dt           string `json:"dt"`
	// Datasets is ONE ENTRY PER ACTOR and is what becomes a view. A run that dispatched an
	// actor across five shards is five `Nodes` and ONE dataset.
	Datasets []exploreDataset `json:"datasets"`
	// Nodes the run recorded that have NO readable files: still being written, or exhausted.
	Pending []string `json:"pending"`
	Failed  []string `json:"failed"`
}

// actorRun is one row of GET /api/datasets/runs — the addressing surface. Mirrors the
// server's DispatchRef: `nodes` is a COUNT (a sharded dispatch of one actor is many nodes,
// one dataset), `rows` the committed row total, `state` the worst node state.
type actorRun struct {
	Actor        string `json:"actor"`
	Version      string `json:"version"`
	RunID        string `json:"runId"`
	RunStartedAt int64  `json:"runStartedAt"`
	Dt           string `json:"dt"`
	Nodes        int    `json:"nodes"`
	Rows         int64  `json:"rows"`
	State        string `json:"state"`
}

// looksLikeRunID reports whether a selector is a raw run id rather than an actor name.
// Run ids are server-minted UUIDs; an actor selector is `name` or `name@version`.
func looksLikeRunID(s string) bool {
	return regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`).MatchString(s)
}

// resolveSelector turns what a HUMAN types into the run id the API needs.
//
// `crawl4ai`, `crawl4ai@1.0.0`, optionally narrowed by `--dt 2026-08-02` or `--dt
// 2026-08-02T19`. A raw run id still works for scripts and for the run a dispatch just
// printed, but nobody has to keep one. This is the whole point: the acceptance bar is that
// no generated query uses a copied UUID, and that starts with not making someone find one.
func resolveSelector(api *apiClient, selector, dt string) (*actorRun, error) {
	actor, version := selector, ""
	if i := strings.IndexByte(selector, '@'); i >= 0 {
		actor, version = selector[:i], selector[i+1:]
	}
	q := "/api/datasets/runs?actor=" + url.QueryEscape(actor)
	if version != "" {
		q += "&version=" + url.QueryEscape(version)
	}
	if dt != "" {
		q += "&dt=" + url.QueryEscape(dt)
	}
	var out struct {
		Runs []actorRun `json:"runs"`
	}
	if err := api.getJSON(q, &out); err != nil {
		return nil, err
	}
	if len(out.Runs) == 0 {
		where := selector
		if dt != "" {
			where += " at " + dt
		}
		return nil, fmt.Errorf("no materialized output for %s\n"+
			"  list what IS there:  kontra explore --list", where)
	}
	// Newest first from the server. Ambiguity is reported, never silently resolved: picking
	// one of several runs and not saying so is how an operator reads yesterday's data.
	if len(out.Runs) > 1 {
		fmt.Fprintf(cliio.Stderr, "%d runs match %s — using the newest (%s). Narrow with --dt:\n",
			len(out.Runs), selector, out.Runs[0].Dt)
		for i, r := range out.Runs {
			if i >= 5 {
				fmt.Fprintf(cliio.Stderr, "  … and %d older\n", len(out.Runs)-5)
				break
			}
			fmt.Fprintf(cliio.Stderr, "  %s@%s  --dt %s\n", r.Actor, r.Version, r.Dt)
		}
	}
	return &out.Runs[0], nil
}

// exploreList prints every actor/version/hour that has output — the answer to "what is
// there?" when you know nothing at all.
func exploreList(api *apiClient, dt string) error {
	q := "/api/datasets/runs?limit=200"
	if dt != "" {
		q += "&dt=" + url.QueryEscape(dt)
	}
	var out struct {
		Runs []actorRun `json:"runs"`
	}
	if err := api.getJSON(q, &out); err != nil {
		return err
	}
	if len(out.Runs) == 0 {
		fmt.Fprintln(cliio.Stdout, "no materialized output yet")
		return nil
	}
	fmt.Fprintf(cliio.Stdout, "  %-28s %-10s %-16s %s\n", "ACTOR", "VERSION", "WHEN (dt)", "OPEN IT WITH")
	for _, r := range out.Runs {
		fmt.Fprintf(cliio.Stdout, "  %-28s %-10s %-16s kontra explore %s@%s --dt %s\n",
			r.Actor, r.Version, r.Dt, r.Actor, r.Version, r.Dt)
	}
	return nil
}

func cmdExplore(args []string) error {
	// A leading bare run id before the flags (`kontra explore r-1 --sql …`), same shape as
	// `kontra runs list …` and `kontra workflow serve <folder> …`.
	selector, rest := "", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		selector, rest = args[0], args[1:]
	}
	fs := flag.NewFlagSet("explore", flag.ContinueOnError)
	apiURL := fs.String("api", orchestratorURL(), "orchestrator base URL")
	query := fs.String("sql", "", "run this SQL over the run's output and exit (non-interactive)")
	webUI := fs.Bool("ui", false, "open the DuckDB web UI instead of the local shell (fetches UI assets over the network)")
	ttl := fs.Int("ttl", exploreDefaultTTL, "presigned URL lifetime in seconds (the server clamps it)")
	catalog := fs.Bool("catalog", false, "cross-run READ-ONLY mode over the shared DuckLake catalog")
	dt := fs.String("dt", "", "narrow to a date or hour: 2026-08-02 or 2026-08-02T19")
	list := fs.Bool("list", false, "list every actor/version/hour that has output, and exit")
	printInit := fs.Bool("print-init", false, "write the generated DuckDB init script to stdout and exit")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *query != "" && *webUI {
		return errors.New("--sql is one-shot and --ui is a session: pick one")
	}
	if selector == "" {
		selector = fs.Arg(0)
	}
	selector = strings.TrimSpace(selector)
	if *list {
		return exploreList(newAPI(*apiURL), *dt)
	}

	// --print-init and --sql make stdout a PAYLOAD (a script, a result set) that gets piped or
	// redirected; their notices go to stderr so the pipe stays clean. Same rule as the banner.
	notices := cliio.Stdout
	if *printInit || *query != "" {
		notices = cliio.Stderr
	}

	// Resolve everything that can fail BEFORE creating a workspace: a missing token or a
	// down orchestrator should not leave a temp dir behind.
	var manifest *exploreManifest
	if *catalog {
		if selector != "" {
			return fmt.Errorf("--catalog is the CROSS-run mode: drop the selector (%q) or drop --catalog", selector)
		}
		if err := catalogPreflight(); err != nil {
			return err
		}
	} else {
		if selector == "" {
			return errors.New("usage: kontra explore <actor[@version]> [--dt 2026-08-02[T19]] [--sql \"SELECT ...\"] [--ui]\n" +
				"       kontra explore --list          # what output exists\n" +
				"       kontra explore --catalog       # cross-run, read-only")
		}
		// An actor name is the ADDRESS; a run id is accepted but never required.
		runID := strings.TrimPrefix(selector, "orch-")
		if !looksLikeRunID(runID) {
			r, err := resolveSelector(newAPI(*apiURL), selector, *dt)
			if err != nil {
				return err
			}
			runID = r.RunID
		} else if *dt != "" {
			return errors.New("--dt narrows an actor selector; it does nothing next to an explicit run id")
		}
		m, err := exploreFetch(*apiURL, runID, *ttl)
		if err != nil {
			return err
		}
		manifest = m
	}

	// Sweep crash leftovers before adding one of our own (see scavengeExploreDirs).
	scavengeExploreDirs(os.TempDir(), exploreStaleAge)
	ws, err := newExploreWorkspace()
	if err != nil {
		return err
	}
	defer ws.cleanup()

	script := ""
	if *catalog {
		script, err = catalogInitSQL(ws.tempDir)
		if err != nil {
			return err
		}
	} else {
		script = exploreInitSQL(manifest, ws.tempDir, *webUI)
	}
	if err := ws.writeInit(script); err != nil {
		return err
	}

	if *printInit {
		fmt.Fprint(cliio.Stdout, script)
		return nil
	}
	if manifest != nil {
		exploreSummary(notices, manifest)
	} else {
		fmt.Fprintf(notices, "catalog mode: lake ATTACHed READ_ONLY — start with `SELECT actor, version, node, tbl FROM kontra_datasets LIMIT %d;`\n", exploreRowLimit)
	}

	switch {
	case *query != "":
		return duckdbWithInit(ws.initPath, "-c", *query)
	case *webUI:
		// Loud, because the default path deliberately makes no network call beyond the
		// presigned object GETs and this one breaks that property.
		fmt.Fprintln(cliio.Stderr, "warning: --ui loads DuckDB's `ui` extension, which fetches its frontend assets from ui.duckdb.org at runtime — the default shell does not")
		fmt.Fprintln(notices, "opening the DuckDB UI → http://localhost:4213/   (ctrl-c to stop it)")
		return duckdbWithInit(ws.initPath, "-ui")
	default:
		return duckdbWithInit(ws.initPath)
	}
}

// --- the manifest ---

// errNoExploreToken names BOTH vars: the endpoint mints presigned URLs, so it fails closed,
// and silently proceeding would just turn a missing local secret into a confusing 401.
var errNoExploreToken = errors.New(
	"no explore token — set KONTRA_EXPLORE_TOKEN (or KONTRA_STATE_TOKEN) to the same value the orchestrator runs with:\n" +
		"    export KONTRA_EXPLORE_TOKEN='…'\n" +
		"  the endpoint mints presigned URLs for a run's output, so it is token-gated and fails closed")

// exploreToken reads the token the endpoint expects, preferring KONTRA_EXPLORE_TOKEN and
// falling back to KONTRA_STATE_TOKEN — the same order as the server (control/orchestrator/src/auth.ts).
func exploreToken() string {
	if t := cliutil.EnvOr("KONTRA_EXPLORE_TOKEN", os.Getenv("KONTRA_STATE_TOKEN")); t != "" {
		return t
	}
	// Same reasoning as the catalog DSN: the value is already in the checkout's .env, and
	// making an operator export it before every command is busywork the tool can do itself.
	// Preference order matches checkBearer's on the server.
	if root, ok := findUp(".env"); ok {
		env := dotEnv(filepath.Join(root, ".env"))
		if t := env["KONTRA_EXPLORE_TOKEN"]; t != "" {
			return t
		}
		if t := env["KONTRA_STATE_TOKEN"]; t != "" {
			return t
		}
	}
	// Outside the checkout, ask the running orchestrator — the same last resort the catalog DSN
	// uses, and what lets `kontra dataset query` take the fast path from any directory.
	if t := envFromRunningStack("KONTRA_EXPLORE_TOKEN"); t != "" {
		return t
	}
	return envFromRunningStack("KONTRA_STATE_TOKEN")
}

// exploreFetch pulls one run's manifest. The token is resolved BEFORE the request: without it
// there is nothing to send, and asking anyway only writes a 401 into the orchestrator's log.
func exploreFetch(apiURL, runID string, ttl int) (*exploreManifest, error) {
	token := exploreToken()
	if token == "" {
		return nil, errNoExploreToken
	}
	if ttl <= 0 {
		ttl = exploreDefaultTTL
	}
	path := "/api/runs/" + url.PathEscape(runID) + "/explore?ttl=" + strconv.Itoa(ttl)
	var m exploreManifest
	if err := newAuthAPI(apiURL, token).getJSON(path, &m); err != nil {
		return nil, exploreHTTPError(runID, err)
	}
	return &m, nil
}

// exploreHTTPError translates the endpoint's refusals into something actionable. 503 is a
// DEPLOYMENT fact (the orchestrator has no token configured), not a local one — saying so
// saves an hour spent re-checking the workstation's env.
func exploreHTTPError(runID string, err error) error {
	he := (*httpError)(nil)
	if !errors.As(err, &he) {
		return err
	}
	switch he.status {
	case 401:
		return fmt.Errorf("explore rejected (401): the token in KONTRA_EXPLORE_TOKEN/KONTRA_STATE_TOKEN does not match the orchestrator's")
	case 404:
		return fmt.Errorf("no explore endpoint for run %s (404) — an orchestrator predating the typed-output surface, or an unknown run id", runID)
	case 503:
		return fmt.Errorf("the orchestrator has explore DISABLED (503): set KONTRA_EXPLORE_TOKEN (or KONTRA_STATE_TOKEN) in ITS environment and restart it — it fails closed rather than presigning unauthenticated")
	case 502:
		return fmt.Errorf("the orchestrator could not build the manifest for %s (502) — object store or catalog unreachable: %s", runID, he.body)
	}
	return err
}

// --- what the operator sees before any SQL ---

// exploreRow is one line of `kontra_nodes` AND of the table the CLI prints — one shape, so
// the two can never disagree about a node's state.
type exploreRow struct {
	actor, version, dt, state, view, err string
	rows                                 int64
	queryable                            bool // has at least one presigned URL → gets a view
}

// exploreRows flattens the manifest into one row per DATASET (per actor). Each dataset already
// folds its nodes; a failed shard makes the whole dataset's state `failed`, and a dataset with
// no files is shown with its state, never omitted — a dataset the operator cannot see is one
// they read as "found nothing", which is the ambiguity ADR 0017 exists to remove.
func exploreRows(m *exploreManifest) []exploreRow {
	rows := make([]exploreRow, 0, len(m.Datasets))
	for _, d := range m.Datasets {
		e := ""
		if d.Error != nil {
			e = *d.Error
		}
		rows = append(rows, exploreRow{
			actor: d.Actor, version: d.Version, dt: d.Dt, state: d.State, view: d.View,
			err: e, rows: d.Rows, queryable: len(d.URLs) > 0,
		})
	}
	return rows
}

// exploreSummary prints the run's lifecycle, when its URLs die, and every node's state. The
// CLI prints this rather than leaving it to the init script because DuckDB EXITS on an
// init-file error: one unreadable file would otherwise take the node states down with it.
func exploreSummary(w io.Writer, m *exploreManifest) {
	fmt.Fprintf(w, "run %s   lifecycle=%s   execution=%s   urls expire %s (%s)\n",
		m.RunID, m.Lifecycle, m.Execution, tsBeat(m.ExpiresAt), expiryIn(m.ExpiresAt))

	// Not an error, and not a reason to stop: the complete nodes below are readable NOW.
	if m.Lifecycle == "finalizing" {
		mz := m.Materialization
		fmt.Fprintf(w, "\n.. still FINALIZING — typed output is still being written; what follows is what is readable now\n")
		fmt.Fprintf(w, "   materialization: %d total · %d complete · %d running · %d pending · %d failed  (%d rows, %s)\n",
			mz.Total, mz.Complete, mz.Running, mz.Pending, mz.Failed, mz.Rows, humanBytes(mz.Bytes))
		fmt.Fprintf(w, "   re-run `kontra explore %s` once it settles to pick up the rest\n", m.RunID)
	}

	rows := exploreRows(m)
	tw := tabwriter.NewWriter(w, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "\n  DATASET\tVERSION\tWHEN (dt)\tSTATE\tROWS\tQUERY\tERROR")
	failed := 0
	for _, r := range rows {
		if r.state == "failed" {
			failed++
		}
		q := "SELECT * FROM " + r.view
		if !r.queryable {
			q = "-" // no files → no view was created; saying otherwise invites "table does not exist"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			dash(r.actor), dash(r.version), dash(r.dt), r.state, r.rows, q, dash(r.err))
	}
	tw.Flush()

	// Loud on purpose: a failed dataset is MISSING output, and a query over the rest returning
	// fewer rows than expected must not read as "the scan found less".
	if failed > 0 {
		fmt.Fprintf(w, "\n!! %d dataset(s) FAILED to materialize — that output is MISSING, not empty (`SELECT * FROM kontra_datasets;`)\n", failed)
	}
}

// expiryIn renders how long the presigned URLs have left — the actual security boundary, so
// it is stated rather than implied.
func expiryIn(ms int64) string {
	d := time.Until(time.UnixMilli(ms)).Truncate(time.Second)
	if d <= 0 {
		return "EXPIRED — re-run kontra explore"
	}
	return "in " + d.String()
}

// --- the generated init script ---

// exploreInitSQL builds the exact-run workspace: pinned extensions, a bounded resource
// posture, one clearly named view per node with files, and `kontra_nodes` over every node the
// run recorded. Every statement is one line — the script is read by operators and asserted on
// by tests, and a wrapped CREATE VIEW is neither.
func exploreInitSQL(m *exploreManifest, tempDir string, withUI bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- kontra explore — run %s (lifecycle=%s, execution=%s)\n", m.RunID, m.Lifecycle, m.Execution)
	fmt.Fprintf(&b, "-- Presigned URLs below expire %s (%s). They ARE the read credential for this run's\n", tsBeat(m.ExpiresAt), expiryIn(m.ExpiresAt))
	b.WriteString("-- output: this file is mode 0600 and is removed on exit. Re-run `kontra explore` for fresh ones.\n\n")

	b.WriteString(exploreExtensionSQL(withUI))
	b.WriteString(exploreResourceSQL(tempDir))

	b.WriteString("-- One view per ACTOR that has readable output. The name is the actor; the physical\n")
	b.WriteString("-- table lives under output/<actor>/version=…/dt=…/ and is never something you type.\n")
	for _, d := range m.Datasets {
		if len(d.URLs) == 0 {
			continue // a complete dataset with no files is a real EMPTY result — kontra_datasets reports it
		}
		quoted := make([]string, len(d.URLs))
		for i, u := range d.URLs {
			quoted[i] = "'" + sqlEscape(u) + "'"
		}
		// ident() is a guard, not a rename: the server already sanitises the view name, and
		// this is identity on every name it can produce. It exists so a name arriving over the
		// network can never carry a quote into a generated CREATE VIEW.
		fmt.Fprintf(&b, "CREATE OR REPLACE VIEW \"%s\" AS SELECT * FROM read_parquet([%s]);\n",
			ident(d.View), strings.Join(quoted, ", "))
	}

	b.WriteString("\n-- The landing table: every actor this dispatch RECORDED, with files or not. A literal\n")
	b.WriteString("-- VALUES table so a failed or empty dataset still has a row — failed output must never\n")
	b.WriteString("-- be indistinguishable from empty output.\n")
	b.WriteString(kontraDatasetsSQL(exploreRows(m)))

	b.WriteString("\n-- Start here (bounded on purpose — generated SQL never scans a dataset unbounded):\n")
	b.WriteString("--   SELECT actor, version, dt, state, \"rows\", error FROM kontra_datasets ORDER BY actor;\n")
	if v := firstQueryableView(m); v != "" {
		fmt.Fprintf(&b, "--   SELECT * FROM \"%s\" LIMIT %d;\n", v, exploreRowLimit)
	} else {
		b.WriteString("--   (no dataset has readable files yet — kontra_datasets says which are pending or failed)\n")
	}
	return b.String()
}

// exploreExtensionSQL pins the extension surface: nothing installs or loads implicitly, and
// the community repository is off. httpfs is the only extension the default path needs.
func exploreExtensionSQL(withUI bool) string {
	var b strings.Builder
	b.WriteString("-- Extensions are pinned: no implicit install, no implicit load, no community repo.\n")
	b.WriteString("SET autoinstall_known_extensions=false;\n")
	b.WriteString("SET autoload_known_extensions=false;\n")
	b.WriteString("SET allow_community_extensions=false;\n")
	b.WriteString("INSTALL httpfs;\n")
	b.WriteString("LOAD httpfs;\n")
	if withUI {
		// --ui only: with autoload off, `duckdb -ui` cannot pull this in by itself.
		b.WriteString("INSTALL ui;\n")
		b.WriteString("LOAD ui;\n")
	}
	return b.String() + "\n"
}

// exploreResourceSQL is the workstation posture. memory_limit is NOT an RSS cap — it bounds
// DuckDB's buffer manager only, and the process can and will exceed it; use a cgroup or
// ulimit if you need a hard boundary. The spill lives inside this session's 0600 dir so it is
// swept with everything else, and is quota'd so a runaway sort fills the quota, not the disk.
func exploreResourceSQL(tempDir string) string {
	return fmt.Sprintf(`-- Resource posture. memory_limit bounds DuckDB's BUFFER MANAGER, not process RSS:
-- it is not an OOM guard. Use a cgroup/ulimit if you need a hard cap.
SET memory_limit='%s';
SET threads=%d;
SET temp_directory='%s';
SET max_temp_directory_size='%s';

`, exploreMemoryLimit, exploreThreads, sqlEscape(tempDir), exploreTempQuota)
}

// kontraDatasetsSQL renders the dataset rows as a VALUES table. Literal rather than a scan: it
// has to describe datasets with NO files (failed or still pending), which nothing on the object
// store can answer.
func kontraDatasetsSQL(rows []exploreRow) string {
	cols := `AS t("actor","version","dt","state","rows","error","view")`
	if len(rows) == 0 {
		// A dispatch with no materialization records at all. An empty VALUES list is a syntax
		// error, so the view is a typed skeleton — `SELECT … FROM kontra_datasets` must still work.
		return "CREATE OR REPLACE VIEW kontra_datasets AS SELECT NULL::VARCHAR AS actor, NULL::VARCHAR AS version, " +
			"NULL::VARCHAR AS dt, NULL::VARCHAR AS state, 0::BIGINT AS \"rows\", " +
			"NULL::VARCHAR AS error, NULL::VARCHAR AS view WHERE false;\n"
	}
	tuples := make([]string, len(rows))
	for i, r := range rows {
		tuples[i] = fmt.Sprintf("(%s, %s, %s, %s, %d::BIGINT, %s, %s)",
			sqlText(r.actor), sqlText(r.version), sqlText(r.dt), sqlText(r.state),
			r.rows, sqlText(r.err), sqlText(r.view))
	}
	return "CREATE OR REPLACE VIEW kontra_datasets AS SELECT * FROM (VALUES " +
		strings.Join(tuples, ", ") + ") " + cols + ";\n"
}

// sqlText renders a string as a SQL literal, or a typed NULL when empty. Newlines and tabs
// are collapsed: an actor failure summary can be a stack trace, and one statement stays one
// line — both for the operator reading the script and for the generator's own tests.
func sqlText(s string) string {
	if s == "" {
		return "NULL::VARCHAR"
	}
	s = strings.Join(strings.Fields(s), " ")
	return "'" + sqlEscape(s) + "'"
}

// firstQueryableView picks the view the starter query points at: a COMPLETE node with files,
// falling back to any node with files (a pre-ADR-0017 run reports state "unknown").
func firstQueryableView(m *exploreManifest) string {
	fallback := ""
	for _, d := range m.Datasets {
		if len(d.URLs) == 0 {
			continue
		}
		if d.State == "complete" {
			return ident(d.View)
		}
		if fallback == "" {
			fallback = ident(d.View)
		}
	}
	return fallback
}

// --- cross-run catalog mode ---

// crossRunCatalog resolves the catalog this mode attaches: the operator's own read-only Postgres,
// or — since ADR 0031 §1b made the catalog a file — the one the appliance owns on this machine.
//
// THE TWO CASES ARE DIFFERENT IN KIND, WHICH IS WHY BOTH EXIST. A Postgres catalog is a SERVER
// somebody else's writer is also attached to, so the credential is the boundary and this mode
// refuses to run without one that is not the writer's. A file catalog has no credential to
// separate: it is a file on this operator's disk, and DuckDB's own exclusive lock means the
// control plane holding it will simply refuse this process rather than share it. Read-only
// SEPARATION is what the rule is about, and the file backend gets it from the lock instead.
func crossRunCatalog() string {
	if pg := os.Getenv("KONTRA_CATALOG_PG"); pg != "" {
		return pg
	}
	return applianceCatalog()
}

// catalogPreflight refuses the mode when there is no catalog it may attach. For a SERVER catalog
// that means its OWN credential, and that is not a convenience check: --catalog attaches the
// catalog every run writes through, and the orchestrator's DSN on a workstation is one mistyped
// statement away from mutating the lake. For the appliance's FILE catalog it means the file
// exists — see crossRunCatalog for why the credential rule has nothing to bite on there.
func catalogPreflight() error {
	if crossRunCatalog() == "" {
		return errors.New("--catalog needs a catalog to attach, and there is none here.\n" +
			"  On this machine's own appliance it is <data-dir>/datasets.ducklake (KONTRA_DATA_DIR or\n" +
			"  $KONTRA_HOME/data) and it does not exist yet — nothing has materialized a Dataset. Note that\n" +
			"  a file catalog is single-writer: STOP the control plane first, or DuckDB refuses this process\n" +
			"  with `Conflicting lock is held in … (PID n)`.\n" +
			"  Against a SHARED Postgres catalog it needs SEPARATELY PROVISIONED read-only credentials:\n" +
			"    export KONTRA_CATALOG_PG='postgres://<read-only role>@host/db'   # NOT the orchestrator's writer DSN\n" +
			"    export KONTRA_S3_ENDPOINT=… KONTRA_S3_ACCESS_KEY=… KONTRA_S3_SECRET_KEY=… KONTRA_S3_BUCKET=… KONTRA_S3_REGION=…\n" +
			"  that mode attaches the catalog every run writes through; reusing the writer's credentials there\n" +
			"  puts the whole lake one statement away from a workstation. Exact-run output needs no credential:\n" +
			"  use `kontra explore <run-id>`")
	}
	return nil
}

// catalogInitSQL builds the cross-run workspace. Everything about it is narrower than the
// exact-run path except its reach: READ_ONLY on the ATTACH, pinned extensions, and a secret
// built from credentials the operator provisioned for reading.
//
// Note what is NOT set: `disabled_filesystems='LocalFileSystem'` would be the real local-FS
// switch, but it also blocks the spill directory the memory posture depends on, so the
// bounded temp dir is kept instead. enable_external_access stays true — httpfs and the
// postgres catalog are external by definition, and this mode is nothing without them.
func catalogInitSQL(tempDir string) (string, error) {
	pg := crossRunCatalog()
	if pg == "" {
		return "", catalogPreflight()
	}
	var b strings.Builder
	b.WriteString("-- kontra explore --catalog — CROSS-RUN, READ-ONLY.\n")
	b.WriteString("-- This file holds the read-only catalog and object-store credentials: mode 0600, removed on\n")
	b.WriteString("-- exit. The ATTACH is READ_ONLY; do not point this at the orchestrator's writer DSN.\n\n")
	b.WriteString("-- Extensions are pinned: no implicit install, no implicit load, no community repo.\n")
	b.WriteString("SET autoinstall_known_extensions=false;\n")
	b.WriteString("SET autoload_known_extensions=false;\n")
	b.WriteString("SET allow_community_extensions=false;\n")
	b.WriteString("INSTALL httpfs;\nLOAD httpfs;\n")
	b.WriteString("INSTALL postgres;\nLOAD postgres;\n")
	b.WriteString("INSTALL ducklake;\nLOAD ducklake;\n\n")
	b.WriteString(exploreResourceSQL(tempDir))

	fmt.Fprintf(&b, "CREATE OR REPLACE SECRET kontra_explore_s3 (TYPE s3, KEY_ID '%s', SECRET '%s', ENDPOINT '%s', REGION '%s', USE_SSL %t, URL_STYLE 'path');\n",
		sqlEscape(s3AccessKey()), sqlEscape(s3SecretKey()), sqlEscape(s3HostPort()), sqlEscape(catalogRegion()), s3UseSSL())
	dsn := ducklakeCatalogDSN(pg)
	fmt.Fprintf(&b, "ATTACH 'ducklake:%s' AS lake (DATA_PATH '%s', READ_ONLY);\n",
		sqlEscape(dsn), sqlEscape(lakeDataPath("")))

	// Identity IS the layout now: the table name is the actor (schema `output`), and every
	// dispatch is a version=…/dt=… partition. `kontra_datasets` lists them from catalog
	// metadata alone — no data file opened, no registry, no hash to decode. Each file carries
	// two partition values pivoted on `partition_key_index` (0 = version, 1 = dt).
	b.WriteString("\n-- Every actor dataset, from catalog metadata only (no data file is opened):\n")
	b.WriteString("CREATE OR REPLACE VIEW kontra_datasets AS\n")
	b.WriteString("WITH pf AS (\n")
	b.WriteString("  SELECT df.data_file_id, t.table_name AS actor, df.record_count AS rows,\n")
	b.WriteString("         max(CASE WHEN pv.partition_key_index=0 THEN pv.partition_value END) AS version,\n")
	b.WriteString("         max(CASE WHEN pv.partition_key_index=1 THEN pv.partition_value END) AS dt\n")
	// The metadata schema is the CATALOG'S, never a constant: `public` on Postgres, `main` on a
	// file catalog, and a file catalog answers `Catalog Error: … schema "public" does not exist`.
	// See lakeMetaSchema — this and cli/dataset.go must read the same fact the same way.
	b.WriteString("  FROM " + lakeMeta(dsn, "ducklake_data_file") + " df\n")
	b.WriteString("  JOIN " + lakeMeta(dsn, "ducklake_table") + " t ON t.table_id=df.table_id AND t.end_snapshot IS NULL\n")
	b.WriteString("  JOIN " + lakeMeta(dsn, "ducklake_schema") + " sc ON sc.schema_id=t.schema_id AND sc.schema_name='output'\n")
	b.WriteString("  JOIN " + lakeMeta(dsn, "ducklake_file_partition_value") + " pv ON pv.data_file_id=df.data_file_id\n")
	b.WriteString("  WHERE df.end_snapshot IS NULL\n")
	b.WriteString("  GROUP BY df.data_file_id, t.table_name, df.record_count\n")
	b.WriteString(")\n")
	b.WriteString("SELECT actor, version, dt, sum(rows) AS rows FROM pf GROUP BY actor, version, dt;\n")
	b.WriteString("\n-- Start here (bounded on purpose — this catalog spans every dispatch):\n")
	fmt.Fprintf(&b, "--   SELECT actor, version, dt, \"rows\" FROM kontra_datasets ORDER BY dt DESC LIMIT %d;\n", exploreRowLimit)
	fmt.Fprintf(&b, "--   SELECT * FROM lake.output.\"<actor>\" WHERE version='<v>' AND dt='<dt>' LIMIT %d;\n", exploreRowLimit)
	return b.String(), nil
}

// ducklakeCatalogDSN normalises KONTRA_CATALOG_PG into the form DuckLake's ATTACH expects.
//
// DuckLake picks its metadata backend from a SCHEME PREFIX on the string after `ducklake:`.
// Without one it treats the whole DSN as a FILE PATH — the failure is a bewildering
// `Cannot open database "/cwd/dbname=kontra_ducklake host=..." in read-only mode: database
// does not exist`, which reads like a missing file rather than a missing prefix. The
// orchestrator's own catalog string (see LakeConfig.catalog) always carries `postgres:`;
// an operator exporting a plain libpq DSN would not know to.
//
// Both libpq forms are accepted and left otherwise untouched: everything after the prefix
// is handed to the postgres extension verbatim.
func ducklakeCatalogDSN(dsn string) string {
	// A libpq URI must be prefixed even though it already begins with "postgres" —
	// DuckLake consumes the scheme and hands the REST to the postgres extension, so
	// `ducklake:postgres://u@h/db` would pass it a bare `//u@h/db`. Checked before the
	// prefix scan below, which would otherwise see "postgres:" and return early.
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return "postgres:" + dsn
	}
	// A FILE PATH IS ALREADY THE FORM ATTACH EXPECTS, and prefixing it would be the exact failure
	// this function exists to prevent, in reverse: `ducklake:postgres:/home/…/datasets.ducklake`
	// hands the postgres extension a libpq DSN made of a filename. A libpq DSN always carries a
	// `key=value` and a URI always carries `://`; a string with neither is a path.
	if !strings.Contains(dsn, "=") && !strings.Contains(dsn, "://") {
		return dsn
	}
	for _, scheme := range []string{"postgres:", "sqlite:", "mysql:", "ducklake:"} {
		if strings.HasPrefix(dsn, scheme) {
			return dsn
		}
	}
	return "postgres:" + dsn
}

func catalogRegion() string { return cliutil.EnvOr("KONTRA_S3_REGION", "us-east-1") }

// s3UseSSL follows the configured endpoint's scheme; SeaweedFS in this deployment is plain
// HTTP and an https:// endpoint must not be downgraded to match it.
func s3UseSSL() bool {
	return strings.HasPrefix(cliutil.EnvOr("KONTRA_S3_PUBLIC_ENDPOINT", cliutil.EnvOr("KONTRA_S3_ENDPOINT", "")), "https://")
}

// --- the workspace (0600, swept) ---

// exploreWorkspace is the private scratch dir one explore session lives in: the init script,
// which holds the presigned URLs, and DuckDB's spill.
type exploreWorkspace struct{ dir, initPath, tempDir string }

func newExploreWorkspace() (*exploreWorkspace, error) {
	dir, err := os.MkdirTemp("", exploreDirPrefix) // 0700
	if err != nil {
		return nil, fmt.Errorf("create explore workspace: %w", err)
	}
	tmp := filepath.Join(dir, exploreSpillDir)
	if err := os.Mkdir(tmp, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("create explore workspace: %w", err)
	}
	return &exploreWorkspace{dir: dir, initPath: filepath.Join(dir, "init.sql"), tempDir: tmp}, nil
}

// writeInit writes the script 0600. The URLs inside it are the credential, and a workstation
// is a shared box more often than anyone admits.
func (w *exploreWorkspace) writeInit(script string) error {
	if err := os.WriteFile(w.initPath, []byte(script), 0o600); err != nil {
		return fmt.Errorf("write explore init script: %w", err)
	}
	return nil
}

func (w *exploreWorkspace) cleanup() { _ = os.RemoveAll(w.dir) }

// scavengeExploreDirs removes leftover explore workspaces older than maxAge.
//
// Deleting the script is NOT the security boundary — the short URL expiry is. A crash, a
// SIGINT while DuckDB is in the foreground, a killed terminal: none of those run the deferred
// cleanup, so leftovers are swept on every start rather than trusted to have been removed.
// Best-effort by design: an unreadable TMPDIR must never block an explore.
//
// A dir is only removed once it is IDENTIFIED as ours by the spill dir every workspace is
// born with. The prefix alone is not enough — control/orchestrator/src/exploreRoutes.test.ts already
// mkdtemps `kontra-explore-*` into the same TMPDIR, and a sweeper that deletes by name guess
// eventually deletes something it did not create.
func scavengeExploreDirs(root string, maxAge time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), exploreDirPrefix) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if fi, err := os.Stat(filepath.Join(dir, exploreSpillDir)); err != nil || !fi.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(dir)
	}
}

// duckdbWithInit launches DuckDB against the generated script. The script is passed as a FILE
// and never inlined into argv: it carries the presigned URLs, /proc/<pid>/cmdline is
// world-readable, and argv would undo the 0600 the file was written with.
//
// DuckDB EXITS if any statement in an init file fails, so an unreadable file (an expired URL,
// a deleted object) takes the whole workspace down — which is why the node states are printed
// by the CLI first, and why the fix is to re-run rather than to cache.
func duckdbWithInit(initPath string, args ...string) error {
	bin, err := duckdbBin()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, append([]string{"-init", initPath}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, cliio.Stdout, os.Stderr
	return cmd.Run()
}
