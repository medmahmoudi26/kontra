// runs.go — `kontra runs`: full visibility into runs and their output.
//
//	kontra runs list                                       # runs newest-first: status, stages, output path
//	kontra runs --run-id <id>                              # one run's detail: per-node status + unit counts
//	kontra runs --run-id <id> --query "SELECT ..."         # run SQL over the run's output (DuckDB/S3)
//	kontra runs --run-id <id> --query "..." --export f.csv # write the query result to a file
//	kontra runs --run-id <id> --duckdb                     # materialize output & open it in DuckDB's UI
//	kontra runs --run-id <id> --state                      # live per-node state (real-time)
//	kontra runs --state                                    # live state of ALL runs (real-time)
//
// Every data path reads the STREAMED per-unit blobs straight from the object store
// (units/<run>/<node>*/**/*.json on SeaweedFS) through DuckDB+httpfs — deterministic, works
// mid-run, and independent of the DuckLake catalog's listing state. Each node becomes a view/table
// named after it (crawl, bust, …); a crawl unit is one page, a cachebuster unit is one finding.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// monRun mirrors one row of GET /api/runs — every caller workflow execution Temporal holds.
//
// IT USED TO READ `GET /api/executions`, which went with the interpreter, so `kontra runs list`
// answered `HTTP 404: not found` for every invocation. A dead command is worse than a missing one:
// it looks like the cluster is broken rather than like the CLI is.
//
// The field names follow the surviving route rather than the dead one. `startedAt`/`closedAt` are
// not `createdAt`/`updatedAt` renamed — they are the caller workflow's own execution window, and
// `closedAt` is 0 while a run is open, which is what lets the RAN column say `open` instead of
// printing the epoch. There is no `workflowId` because the Run IS the workflow id (ADR 0023 §12).
type monRun struct {
	RunID      string `json:"runId"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	Tenant     string `json:"tenant"`
	StartedAt  int64  `json:"startedAt"`
	ClosedAt   int64  `json:"closedAt"`
	Dispatches int    `json:"dispatches"`
}

// monHeartbeat mirrors one node's live RunBatch heartbeat from GET
// /api/runs/:runId/heartbeats (roadmap platform-x100 #06): per-TURN {done,total} progress,
// plus the activity attempt and last-beat time. Coarser than the S3 blob count, shown beside it.
type monHeartbeat struct {
	Node     string `json:"node"`
	Done     int    `json:"done"`
	Total    int    `json:"total"`
	Attempt  int    `json:"attempt"`
	LastBeat int64  `json:"lastBeat"`
	// Isolated: units this node PERMANENTLY DROPPED. Rendered as its own column because
	// `completed` with 0 blobs and `completed` after discarding every unit used to look
	// identical — that ambiguity is what let a 15,814-target run report success in 7 minutes.
	Isolated int `json:"isolated"`
}

// execFilter is the optional tenant/status narrowing for the execution list, passed through
// to GET /api/runs as query params.
type execFilter struct{ tenant, status string }

func (f execFilter) query() string {
	v := url.Values{}
	if f.tenant != "" {
		v.Set("tenant", f.tenant)
	}
	if f.status != "" {
		v.Set("status", f.status)
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// monDispatch mirrors a GET /api/datasets/runs row — the ADDRESSING surface, read from the
// materialization ledger.
//
// `/api/datasets` is the wrong source for this column: it is keyed on (name, version, dt) and
// carries no run id, because a dataset is addressed by dispatch time, not by a UUID. The ledger
// is where the two are still connected, and it is a single indexed query rather than a catalog
// walk.
type monDispatch struct {
	Actor   string `json:"actor"`
	Version string `json:"version"`
	RunID   string `json:"runId"`
	Dt      string `json:"dt"`
}

func cmdMonitor(args []string) error {
	// Allow a leading bare subcommand (`list`) before flags.
	sub, rest := "", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, rest = args[0], args[1:]
	}
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	runID := fs.String("run-id", "", "run id to inspect")
	query := fs.String("query", "", "SQL over the run's output (one view per node, e.g. crawl, bust)")
	export := fs.String("export", "", "write the --query result to this file (.csv/.parquet/.json by extension)")
	duckUI := fs.Bool("duckdb", false, "materialize the run's output and open it in the DuckDB UI")
	state := fs.Bool("state", false, "live run/node state, refreshing in real time")
	interval := fs.Duration("interval", 2*time.Second, "refresh interval for --state")
	tenant := fs.String("tenant", "", "filter the run list by tenant (list / --state without a run id)")
	statusFilter := fs.String("status", "", "filter the run list by status: running|completed|failed|cancelled")
	apiURL := fs.String("api", orchestratorURL(), "orchestrator base URL")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	api := newAPI(*apiURL)
	rid := strings.TrimSpace(*runID)
	filter := execFilter{tenant: strings.TrimSpace(*tenant), status: strings.TrimSpace(*statusFilter)}

	switch {
	case *state:
		return monitorState(api, rid, *interval, filter) // works with or without a run id
	case rid != "" && (*query != "" || *export != ""):
		return monitorQuery(rid, *query, *export)
	case rid != "" && *duckUI:
		return monitorOpenUI(rid)
	case rid != "":
		return monitorDetail(api, rid)
	case sub == "list" || sub == "":
		return monitorList(api, filter)
	default:
		return errors.New("usage: kontra runs [list] | --run-id <id> [--query SQL [--export FILE] | --duckdb | --state] | --state")
	}
}

// monitorList prints runs newest-first with their materialized stages and S3 output prefix.
// The run rows come from Temporal Visibility (GET /api/runs), so status is Temporal's own — no
// SQLite status cache. The stages/OUTPUT columns still read S3 unchanged.
func monitorList(api *apiClient, filter execFilter) error {
	var runs []monRun
	if err := api.getJSON("/api/runs"+filter.query(), &runs); err != nil {
		return fmt.Errorf("GET /api/runs failed: %w", err)
	}
	// Best-effort: map runId -> materialized "actor@version/node" stages (empty if the catalog
	// hasn't listed them yet — a run still in flight, or nothing materialized).
	var ds struct {
		Runs []monDispatch `json:"runs"`
	}
	_ = api.getJSON("/api/datasets/runs?limit=500", &ds)
	stages := map[string][]string{}
	for _, d := range ds.Runs {
		// One dataset per actor now, so the stage is the actor — not one entry per graph node.
		stages[d.RunID] = appendUniq(stages[d.RunID], fmt.Sprintf("%s@%s", d.Actor, d.Version))
	}

	w := tabwriter.NewWriter(stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "RUN-ID\tTYPE\tSTATUS\tSTARTED\tCLOSED\tDISPATCHES\tSTAGES (actor@ver/node)\tOUTPUT (S3)")
	for _, r := range runs {
		st := strings.Join(stages[r.RunID], ", ")
		if st == "" {
			st = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\tunits/run=%s/\n",
			r.RunID, dash(r.Type), r.Status, tsShort(r.StartedAt), closedCol(r.ClosedAt), r.Dispatches, st, r.RunID)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "(no runs yet)")
	}
	fmt.Fprintln(stdout, "\ntips:")
	// One view per run, named `run`, with shard/actor as columns — not a view per node. The old
	// advice ("FROM bust") named a graph node that may or may not hold the actor you assume.
	fmt.Fprintln(stdout, "  kontra runs --run-id <id> --query \"SELECT * FROM run LIMIT 20\"    # SQL over output")
	fmt.Fprintln(stdout, "     one view per RUN; shard and actor are columns:  ... FROM run WHERE shard = '0007'")
	fmt.Fprintln(stdout, "  kontra runs --run-id <id> --duckdb                                 # open in DuckDB UI")
	fmt.Fprintln(stdout, "  kontra runs --state                                                # live states")
	return nil
}

// monitorDetail prints one run's record + reconciled per-node status and streamed unit counts.
func monitorDetail(api *apiClient, runID string) error {
	var status struct {
		Status string            `json:"status"`
		Nodes  map[string]string `json:"nodes"`
	}
	statusErr := api.getJSON("/api/runs/"+url.PathEscape(ensureWF(runID))+"/status", &status)
	counts, _ := nodeCounts(runID)    // best-effort: streamed blobs on S3
	hbs := nodeHeartbeats(api, runID) // best-effort: live per-turn heartbeats (#06)

	overall := status.Status
	if statusErr != nil {
		overall = "(status unavailable — reading S3 only)"
	}
	fmt.Fprintf(stdout, "run %s  status=%s\n", runID, overall)

	names := unionNodeKeys(status.Nodes, counts, hbs)
	lost := 0
	w := tabwriter.NewWriter(stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "  NODE\tSTATE\tPROGRESS\tATTEMPT\tLAST-BEAT\tISOLATED\tOUTPUT BLOBS")
	for _, n := range names {
		st := status.Nodes[n]
		if st == "" {
			st = "-"
		}
		prog, att, beat := heartbeatCols(hbs[n], hasKey(hbs, n))
		iso := isolatedCol(hbs[n], hasKey(hbs, n))
		if hbs[n].Isolated > 0 {
			lost += hbs[n].Isolated
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\t%d\n", n, st, prog, att, beat, iso, counts[n])
	}
	w.Flush()
	// A run that dropped units must NEVER read as a plain success. `completed` and
	// `completed, having discarded everything` were indistinguishable, and that is how a
	// 15,814-target run was reported as finished in ~7 minutes.
	if lost > 0 {
		fmt.Fprintf(stdout, "\n!! %d unit(s) PERMANENTLY DROPPED — this run lost work; status %q is not the whole story\n", lost, overall)
	}
	// A run written before the layout change lives under the bare-run prefix instead; print
	// whichever actually holds this run's blobs rather than a path that may not exist.
	if lay, lerr := runLayout(runID); lerr == nil && lay.legacy && !lay.hive {
		fmt.Fprintf(stdout, "\noutput (S3): units/%s/   (legacy layout)\n", runID)
	} else {
		fmt.Fprintf(stdout, "\noutput (S3): units/run=%s/\n", runID)
	}
	fmt.Fprintf(stdout, "  kontra runs --run-id %s --query \"SELECT ...\"   |   --duckdb   |   --state\n", runID)
	return nil
}

// monitorQuery runs SQL over the run's output and either prints it or writes it to --export.
// The run is exposed as ONE view named `run`, with shard and actor as columns. It used to be one
// view per node, which meant `FROM n1` also globbed n10-n19 and answering "how many rows did this
// run produce" required unioning every view by hand — forget one and you get a plausible
// undercount instead of an error. Both of those cost retracted numbers.
func monitorQuery(runID, query, export string) error {
	lay, err := runLayout(runID)
	if err != nil {
		return err
	}
	if !lay.any() {
		return fmt.Errorf("run %s has no output blobs yet — nothing to query", runID)
	}
	prelude := s3SetupSQL() + runViewSQL(s3Bucket(), runID, lay, "VIEW")
	if strings.TrimSpace(query) == "" {
		query = "SELECT * FROM run LIMIT 100"
	}

	if export != "" {
		q := strings.TrimRight(strings.TrimSpace(query), ";")
		sql := prelude + fmt.Sprintf("COPY (%s) TO '%s';", q, sqlEscape(export))
		if _, err := duckdbJSON(sql); err != nil {
			return fmt.Errorf("export failed: %w", err)
		}
		sz := int64(0)
		if fi, err := os.Stat(export); err == nil {
			sz = fi.Size()
		}
		fmt.Fprintf(stdout, "exported → %s (%s)\n", export, humanBytes(sz))
		return nil
	}

	fmt.Fprintf(stdout, "# run %s   view: run   (columns: shard, actor%s)\n", runID, layoutNote(lay))
	return duckdbDisplay(prelude, query)
}

// monitorOpenUI materializes the run's output into a local .duckdb file (real tables, so the UI is
// fast and keeps working even if S3 hiccups) and opens it in the DuckDB UI. The UI serves on
// http://localhost:4213/ — on a headless box, port-forward that; locally it opens the browser.
func monitorOpenUI(runID string) error {
	lay, err := runLayout(runID)
	if err != nil {
		return err
	}
	if !lay.any() {
		return fmt.Errorf("run %s has no output blobs yet — nothing to open", runID)
	}
	dbfile := fmt.Sprintf("kontra-%s.duckdb", shortID(runID))
	_ = os.Remove(dbfile)

	bin, err := duckdbBin()
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "materializing run %s%s → %s ...\n", runID, layoutNote(lay), dbfile)
	build := exec.Command(bin, dbfile, "-c", s3SetupSQL()+runViewSQL(s3Bucket(), runID, lay, "TABLE"))
	build.Stdout, build.Stderr = stdout, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("materialize failed: %w", err)
	}
	abs, _ := filepath.Abs(dbfile)
	fmt.Fprintf(stdout, "opening DuckDB UI → http://localhost:4213/   (db: %s)\n", abs)
	fmt.Fprintf(stdout, "table: run   —   ctrl-c to stop the UI\n")
	ui := exec.Command(bin, "-ui", dbfile)
	ui.Stdin, ui.Stdout, ui.Stderr = os.Stdin, stdout, os.Stderr
	return ui.Run()
}

// monitorState refreshes run state in place until interrupted. With a run id it shows that run's
// per-node status (from the orchestrator) and streamed unit counts (from S3); without one it lists
// every run's status. This is the real-time execution view — the run/node state, live.
func monitorState(api *apiClient, runID string, interval time.Duration, filter execFilter) error {
	if interval < 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	for {
		fmt.Fprint(stdout, "\033[H\033[2J") // home + clear
		fmt.Fprintf(stdout, "kontra runs --state   %s   (refresh %s · ctrl-c to exit)\n\n",
			time.Now().Format("15:04:05"), interval)
		if runID != "" {
			renderRunState(api, runID)
		} else {
			renderAllRuns(api, filter)
		}
		time.Sleep(interval)
	}
}

func renderRunState(api *apiClient, runID string) {
	var status struct {
		Status string            `json:"status"`
		Nodes  map[string]string `json:"nodes"`
	}
	statusErr := api.getJSON("/api/runs/"+url.PathEscape(ensureWF(runID))+"/status", &status)
	counts, _ := nodeCounts(runID)
	hbs := nodeHeartbeats(api, runID) // live per-turn heartbeats (#06), alongside S3 counts

	overall := status.Status
	if statusErr != nil {
		overall = "(status unavailable — S3 only)"
	}
	fmt.Fprintf(stdout, "run %s   status=%s\n\n", runID, overall)
	w := tabwriter.NewWriter(stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tSTATE\tPROGRESS\tATTEMPT\tLAST-BEAT\tISOLATED\tOUTPUT BLOBS")
	for _, n := range unionNodeKeys(status.Nodes, counts, hbs) {
		st := status.Nodes[n]
		if st == "" {
			st = "-"
		}
		prog, att, beat := heartbeatCols(hbs[n], hasKey(hbs, n))
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\n", n, st, prog, att, beat, counts[n])
	}
	w.Flush()
}

func renderAllRuns(api *apiClient, filter execFilter) {
	var runs []monRun
	if err := api.getJSON("/api/runs"+filter.query(), &runs); err != nil {
		fmt.Fprintf(stdout, "GET /api/runs failed: %v\n", err)
		return
	}
	w := tabwriter.NewWriter(stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "RUN-ID\tTYPE\tSTATUS\tSTARTED\tCLOSED\tDISPATCHES")
	for _, r := range runs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\n",
			r.RunID, dash(r.Type), r.Status, tsShort(r.StartedAt), closedCol(r.ClosedAt), r.Dispatches)
	}
	w.Flush()
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "(no runs yet)")
	}
}

// nodeHeartbeats fetches a run's live per-node heartbeats (#06). Best-effort: any error
// (no live turn, run pre-#04, orchestrator/Temporal hiccup) yields an empty map so the
// monitor still renders status + S3 counts.
func nodeHeartbeats(api *apiClient, runID string) map[string]monHeartbeat {
	var resp struct {
		Nodes map[string]monHeartbeat `json:"nodes"`
	}
	_ = api.getJSON("/api/runs/"+url.PathEscape(strings.TrimPrefix(runID, "orch-"))+"/heartbeats", &resp)
	return resp.Nodes
}

// isolatedCol renders the ISOLATED cell. A dash means "no heartbeat yet", NOT zero — the two
// must look different, because reading an unknown as zero is the exact mistake this column
// exists to prevent.
func isolatedCol(hb monHeartbeat, ok bool) string {
	if !ok {
		return "-"
	}
	if hb.Isolated == 0 {
		return "0"
	}
	return fmt.Sprintf("%d !", hb.Isolated) // the bang is deliberate: this run lost work
}

// heartbeatCols renders the PROGRESS/ATTEMPT/LAST-BEAT cells for a node, or dashes when it
// has no live heartbeat.
func heartbeatCols(hb monHeartbeat, ok bool) (prog, attempt, lastBeat string) {
	if !ok {
		return "-", "-", "-"
	}
	return fmt.Sprintf("%d/%d", hb.Done, hb.Total), fmt.Sprintf("%d", hb.Attempt), tsBeat(hb.LastBeat)
}

// --- DuckDB plumbing (reads streamed blobs from SeaweedFS; no catalog needed) ---

// duckdbBin resolves the duckdb executable: PATH first, then the standard install location.
func duckdbBin() (string, error) {
	if p, err := exec.LookPath("duckdb"); err == nil {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".duckdb", "cli", "latest", "duckdb")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("duckdb not found on PATH — install it (https://install.duckdb.org) and retry")
}

// s3SetupSQL is the httpfs+S3 prelude for anonymous SeaweedFS (keys are required but ignored).
func s3SetupSQL() string {
	return strings.Join([]string{
		"INSTALL httpfs", "LOAD httpfs",
		fmt.Sprintf("SET s3_endpoint='%s'", s3HostPort()),
		"SET s3_use_ssl=false", "SET s3_url_style='path'",
		"SET s3_access_key_id='any'", "SET s3_secret_access_key='any'",
	}, "; ") + "; "
}

// Two blob layouts coexist on S3, and the reader has to handle both.
//
//	legacy  units/<run>/<node>/u<i>/<sha>.json
//	hive    units/run=<run>/dt=<date>/actor=<actor>/shard=<nnnn>/unit=<nnnnn>/<sha>.json
//
// The legacy layout encodes identity positionally, which is what made a view named `n1` glob
// `n1*` and silently return n10-n19's rows as well — two reported figures had to be retracted
// over it. The hive layout is read with hive_partitioning=true, so run/shard/actor/dt come back
// as typed COLUMNS and DuckDB prunes whole files from the WHERE clause, instead of this code
// parsing path segments by ordinal and hoping the shape never changes.
//
// Months of output already sit on S3 in the legacy layout, so this cannot be a flag day. A run
// that straddled the change is read as the union of both.
type unitLayout struct{ legacy, hive bool }

func (l unitLayout) any() bool { return l.legacy || l.hive }

func legacyGlob(bucket, runID string) string {
	return fmt.Sprintf("s3://%s/units/%s/**/*.json", bucket, sqlEscape(runID))
}

// hiveGlob is a pure literal prefix — no wildcard before run= — because that is the only shape
// an object store can prune a LIST with. Measured on this bucket (188,275 objects): 0.17s here
// versus 8.4s for `units/*/*/run=<run>/**`, which lists everything. --state re-runs this on
// every refresh tick, so the difference is the difference between a live view and a stalled one.
func hiveGlob(bucket, runID string) string {
	return fmt.Sprintf("s3://%s/units/run=%s/**/*.json", bucket, sqlEscape(runID))
}

// runLayout reports which layouts hold blobs for a run. glob() returns zero rows rather than
// erroring when nothing matches, so both probes fit in one round trip.
func runLayout(runID string) (unitLayout, error) {
	b := s3Bucket()
	sql := s3SetupSQL() + fmt.Sprintf(
		"SELECT (SELECT count(*) FROM glob('%s')) AS legacy, (SELECT count(*) FROM glob('%s')) AS hive;",
		legacyGlob(b, runID), hiveGlob(b, runID))
	out, err := duckdbJSON(sql)
	if err != nil {
		return unitLayout{}, fmt.Errorf("probe run layout: %w", err)
	}
	var rows []struct {
		Legacy int `json:"legacy"`
		Hive   int `json:"hive"`
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		if err := json.Unmarshal([]byte(s), &rows); err != nil {
			return unitLayout{}, fmt.Errorf("parse run layout: %w", err)
		}
	}
	if len(rows) == 0 {
		return unitLayout{}, nil
	}
	return unitLayout{legacy: rows[0].Legacy > 0, hive: rows[0].Hive > 0}, nil
}

// unitSelect is one layout's contribution to the run view. Both arms project the same identity
// columns — shard, actor, unit — so the union has a single shape no matter where a row came from.
// In the legacy layout the actor is genuinely unknowable from the key, which is the defect the
// hive layout exists to fix; it reports NULL rather than inventing a value.
func unitSelect(bucket, runID string, hive bool) string {
	if hive {
		// hive_types_autocast=0 keeps every partition value VARCHAR. Left on, DuckDB types
		// shard=0007 as an integer and then fails the whole read when a graph-named shard like
		// shard=crawl shows up in the same run — and a mixed fleet is the normal case.
		return fmt.Sprintf("SELECT * FROM read_json_auto('%s', hive_partitioning=true, "+
			"hive_types_autocast=0, union_by_name=true, ignore_errors=true, sample_size=-1)",
			hiveGlob(bucket, runID))
	}
	return fmt.Sprintf("SELECT * EXCLUDE (filename), "+
		"split_part(split_part(filename,'/',6),'.',1) AS shard, "+
		"NULL::VARCHAR AS actor, '%s' AS run "+
		"FROM read_json_auto('%s', filename=true, union_by_name=true, ignore_errors=true, "+
		"sample_size=-1)", sqlEscape(runID), legacyGlob(bucket, runID))
}

// runViewSQL builds ONE view over the whole run, with shard as a column rather than a namespace.
// This replaces the per-node views that forced a hand-rolled union over every view to answer
// "how many rows did this run produce" — and returned a plausible undercount when you forgot.
//
// sample_size=-1 makes DuckDB read every row before fixing a column's type. Without it, a chunk
// whose first rows look numeric infers INT128 and then aborts the ENTIRE run's query with
// `Could not convert string ” to INT128` — one poisoned chunk taking down all the others.
func runViewSQL(bucket, runID string, lay unitLayout, keyword string) string {
	var arms []string
	if lay.hive {
		arms = append(arms, unitSelect(bucket, runID, true))
	}
	if lay.legacy {
		arms = append(arms, unitSelect(bucket, runID, false))
	}
	// BY NAME so the two layouts' differing column sets align by name, not by position.
	return fmt.Sprintf("CREATE %s run AS %s; ", keyword, strings.Join(arms, " UNION ALL BY NAME "))
}

// shardExpr extracts the shard from a file path for each layout. Counting via glob() touches
// object metadata only, so it stays cheap enough for the live refresh loop.
func shardExpr(hive bool) string {
	if hive {
		return "regexp_extract(file, '/shard=([^/]+)/', 1)"
	}
	return "split_part(split_part(file,'/',6),'.',1)"
}

// shardCountSQL counts blobs per shard across whichever layouts hold the run.
func shardCountSQL(bucket, runID string, lay unitLayout) string {
	var arms []string
	if lay.hive {
		arms = append(arms, fmt.Sprintf("SELECT %s AS node FROM glob('%s') t(file)",
			shardExpr(true), hiveGlob(bucket, runID)))
	}
	if lay.legacy {
		arms = append(arms, fmt.Sprintf("SELECT %s AS node FROM glob('%s') t(file)",
			shardExpr(false), legacyGlob(bucket, runID)))
	}
	return fmt.Sprintf("SELECT node, count(*) AS units FROM (%s) WHERE node <> '' GROUP BY 1 ORDER BY 1;",
		strings.Join(arms, " UNION ALL "))
}

// nodeCounts returns the number of streamed output blobs per shard on S3. This counts files
// (object metadata only — ~0.1s regardless of payload size), so it stays cheap enough for a live
// refresh loop at recon scale; a blob may hold >1 record, so use --query for exact row counts.
func nodeCounts(runID string) (map[string]int, error) {
	m := map[string]int{}
	lay, err := runLayout(runID)
	if err != nil || !lay.any() {
		return m, err
	}
	out, err := duckdbJSON(s3SetupSQL() + shardCountSQL(s3Bucket(), runID, lay))
	if err != nil {
		return m, err
	}
	var rows []struct {
		Node  string `json:"node"`
		Units int    `json:"units"`
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		if err := json.Unmarshal([]byte(s), &rows); err != nil {
			return m, err
		}
	}
	for _, r := range rows {
		m[r.Node] += r.Units // += : a straddling run reports the same shard from both layouts
	}
	return m, nil
}

// layoutNote tells the operator which layout(s) they are reading, so a legacy run that shows
// actor=NULL reads as expected rather than as a bug.
func layoutNote(l unitLayout) string {
	switch {
	case l.hive && l.legacy:
		return ", layout: hive+legacy"
	case l.legacy:
		return ", layout: legacy (actor unknown)"
	case l.hive:
		return ", layout: hive"
	}
	return ""
}

// duckdbJSON runs SQL and returns stdout as JSON (for parsing). stderr surfaces only on error.
func duckdbJSON(sql string) ([]byte, error) {
	bin, err := duckdbBin()
	if err != nil {
		return nil, err
	}
	var out, errb bytes.Buffer
	cmd := exec.Command(bin, "-json", "-c", sql)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// duckdbDisplay runs `query` and streams the rendered result to the user (duckbox table).
//
// The prelude is run with output redirected to /dev/null, because the DuckDB shell renders EVERY
// row-returning statement — and `CREATE OR REPLACE SECRET` returns `Success | true`. Left alone,
// that box prints above every listing as if it were part of the answer. Same reasoning as
// lastDuckJSON on the machine-readable path; this is the human-readable half.
func duckdbDisplay(prelude, query string) error {
	bin, err := duckdbBin()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "-c", ".output /dev/null", "-c", prelude, "-c", ".output", "-c", query)
	cmd.Stdout, cmd.Stderr = stdout, os.Stderr
	return cmd.Run()
}

// --- small helpers ---

func ensureWF(runID string) string {
	if strings.HasPrefix(runID, "orch-") {
		return runID
	}
	return "orch-" + runID
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "orch-")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ident sanitizes a node name into a SQL identifier.
func ident(node string) string {
	r := strings.Map(func(c rune) rune {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			return c
		default:
			return '_'
		}
	}, node)
	if r == "" || (r[0] >= '0' && r[0] <= '9') {
		r = "n_" + r
	}
	return r
}

func sqlEscape(s string) string { return strings.ReplaceAll(s, "'", "''") }

// unionNodeKeys returns the sorted union of the status, S3-count, and heartbeat node keys —
// so a node shows up whether it has a status, streamed output, or only a live heartbeat.
func unionNodeKeys(status map[string]string, counts map[string]int, hbs map[string]monHeartbeat) []string {
	set := map[string]bool{}
	for k := range status {
		set[k] = true
	}
	for k := range counts {
		set[k] = true
	}
	for k := range hbs {
		set[k] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func hasKey(m map[string]monHeartbeat, k string) bool { _, ok := m[k]; return ok }

// dash renders an empty string as "-" for table cells (e.g. a run with no tenant).
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// tsBeat formats a heartbeat epoch-ms as a wall-clock time, or "-" when never beaten.
func tsBeat(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format("15:04:05")
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// s3HostPort / s3Bucket derive the object-store address DuckDB should hit — the browser/host-facing
// endpoint, defaulting to the local control plane.
func s3HostPort() string {
	e := envOr("KONTRA_S3_PUBLIC_ENDPOINT", envOr("KONTRA_S3_ENDPOINT", "http://localhost:8333"))
	e = strings.TrimPrefix(strings.TrimPrefix(e, "http://"), "https://")
	return strings.TrimRight(e, "/")
}

func s3Bucket() string { return envOr("KONTRA_S3_BUCKET", "kontra") }

func tsShort(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04")
}

// closedCol renders a run's close time, where 0 means STILL RUNNING rather than "unknown".
//
// `tsShort` prints `-` for a zero, which reads as missing data. A run that has not closed is not
// missing its close time; it does not have one yet, and on this column that is the single most
// useful thing it could say. Two different facts, two different words.
func closedCol(ms int64) string {
	if ms == 0 {
		return "open"
	}
	return tsShort(ms)
}

func appendUniq(xs []string, x string) []string {
	for _, e := range xs {
		if e == x {
			return xs
		}
	}
	return append(xs, x)
}
