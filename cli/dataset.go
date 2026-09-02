// dataset.go — `kontra dataset`: the ONE surface over data, input and output alike.
//
//	kontra dataset list                                 # every dataset: standalone + output
//	kontra dataset query <name> --sql "…" [--version V] [--dt PREFIX] [--export FILE]
//	kontra dataset create|anew <file> <name>            # load a standalone dataset
//	kontra dataset delete <name>
//	kontra dataset tag <name> [--dt PREFIX] --add T [--add T2]... [--remove T3]...
//	kontra dataset rename <name> [--dt PREFIX] (--to NEWNAME | --reset)
//
// A dataset is anything queryable, and it is the currency for BOTH input and output:
//   - standalone/<name>            — you loaded it (a bbscope dump, a scope list)
//   - output/<actor>/version=/dt=  — an actor produced it
//
// You shape a dataset with `dataset query` until the SQL is right, then hand that same query to
// `batches()` in your workflow: `catalog.dataset(name).batches(200, order_by=…, query="<that SQL>")`.
// The rows the query returns ARE the units the actor receives — the query you proved is the query
// that pages. (There is no `dispatch --input/--query`: ADR 0023 §12 deleted the verb with the
// interpreter, and paging became the caller's loop.)
//
// WHERE THE QUERY RUNS. On the orchestrator by default — it already holds an open, attached
// connection, so the same query costs ~50 ms there against ~1.3 s here, almost all of which is
// this process loading three DuckDB extensions and attaching the catalog, twice. That is a
// deliberate move away from the plan's "controller query RAM is zero": the server executes it
// inside a read-only, filesystem-less sandbox with a memory ceiling it cannot spill past
// (control/orchestrator/src/data/queryEngine.ts). `--local` runs DuckDB here instead, and an unreachable
// orchestrator falls back to it automatically.
//
// The old split between `kontra db` (standalone) and `kontra runs --query` (output) is gone:
// both are `dataset query` now, and `db`/`explore` remain as thin aliases so muscle memory keeps
// working.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
)

// lastDuckJSON returns the LAST top-level JSON array in duckdb -json output.
//
// duckdb -json emits one array PER statement that returns rows, and our prelude runs several:
// `CREATE OR REPLACE SECRET` alone returns `[{"Success":true}]`, so the SELECT's result is not
// the whole output — it is the last of two. Decoding the stream and keeping the last array is
// robust to however many statements happen to emit; unmarshalling the raw bytes as one array
// (which an earlier version did, then ignored the error) silently yielded nothing.
func lastDuckJSON(out []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	var last json.RawMessage
	for {
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			break // EOF or trailing noise — stop at the last good value
		}
		t := bytes.TrimSpace(v)
		if len(t) > 0 && t[0] == '[' {
			last = v
		}
	}
	if last == nil {
		return []byte("[]"), nil
	}
	return last, nil
}

// datasetSchemas is the resolution order for a bare dataset name: an uploaded list first, then an
// actor's output. A name that exists in both is vanishingly unlikely (actor names vs list names),
// and standalone-wins is the least surprising for the operator who just `dataset create`d one.
var datasetSchemas = []string{lakeSchema, "output"}

func cmdDataset(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kontra dataset list|query|create|anew|delete|tag|rename ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return datasetList(rest)
	case "query":
		return datasetQueryCmd(rest)
	case "create", "anew":
		return cmdDB(append([]string{sub}, rest...)) // reuse db.go's ingest — one implementation
	case "delete":
		return datasetDelete(rest)
	case "tag":
		return datasetTag(rest)
	case "rename":
		return datasetRename(rest)
	default:
		return fmt.Errorf("unknown dataset subcommand %q (want list|query|create|anew|delete|tag|rename)", sub)
	}
}

// stringList collects a REPEATED flag: `--add a --add b` yields ["a","b"]. Go's flag package has no
// native repeatable string, so a tag verb that takes several tags in one call needs this.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// datasetList shows every dataset — uploaded lists AND actor output — from catalog metadata only.
// It folds `kontra db list` (which showed only standalone) and the output side of `kontra runs`.
//
// ON THE ORCHESTRATOR BY DEFAULT, the same move `dataset query` makes: the server's `/api/datasets`
// already reads the catalog AND the per-Dataset lifecycle/ownership markers in the object store, so
// it is the only surface that can say a Dataset is TEMPORARY and name its owning Run. `--local` (or
// an explicit --catalog/--data-path, which name a lake that is not this stack's) runs DuckDB here,
// and so does an unreachable orchestrator — the list still works with the stack down, only without
// the owner, which the marker objects hold and a bare catalog scan cannot see.
func datasetList(args []string) error {
	fs := flag.NewFlagSet("dataset list", flag.ContinueOnError)
	catalog := fs.String("catalog", "", "DuckLake catalog DSN (default: discovered from the checkout or the running stack)")
	dataPath := fs.String("data-path", "", "lake DATA_PATH (default: KONTRA_DUCKLAKE_DATA_PATH or s3://<bucket>/)")
	local := fs.Bool("local", false, "list via local DuckDB instead of the orchestrator (works with the stack down; no owner column)")
	apiURL := fs.String("api", orchestratorURL(), "orchestrator base URL")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !*local && *catalog == "" && *dataPath == "" {
		err := datasetListViaAPI(*apiURL)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errAPIUnavailable) {
			return err // a real answer from the server (a 502 from the lake) — report it as-is
		}
	}

	dsn, err := lakeCatalog(*catalog)
	if err != nil {
		return err
	}
	// standalone: one row per uploaded list. output: one row per (actor, version, dt) dispatch,
	// pivoted from catalog partition metadata — no data file is opened either way. A `tmp_`-named
	// output Dataset is marked `temp` here so an offline list still distinguishes it; the owner
	// lives in a marker object this scan cannot read, so it is shown only over the API above.
	prelude := attachSQL(dsn, lakeDataPath(*dataPath)) + `CREATE SCHEMA IF NOT EXISTS lake.output; `
	sql := `WITH standalone AS (
		   SELECT 'standalone' AS kind, table_name AS name, NULL AS version, NULL AS dt
		   FROM duckdb_tables() WHERE database_name='lake' AND schema_name='` + lakeSchema + `'
		 ), out AS (
		   SELECT 'output' AS kind, t.table_name AS name,
		          max(CASE WHEN pv.partition_key_index=0 THEN pv.partition_value END) AS version,
		          max(CASE WHEN pv.partition_key_index=1 THEN pv.partition_value END) AS dt
		   FROM ` + lakeMeta(dsn, "ducklake_data_file") + ` df
		   JOIN ` + lakeMeta(dsn, "ducklake_table") + ` t ON t.table_id=df.table_id AND t.end_snapshot IS NULL
		   JOIN ` + lakeMeta(dsn, "ducklake_schema") + ` sc ON sc.schema_id=t.schema_id AND sc.schema_name='output'
		   JOIN ` + lakeMeta(dsn, "ducklake_file_partition_value") + ` pv ON pv.data_file_id=df.data_file_id
		   WHERE df.end_snapshot IS NULL
		   GROUP BY df.data_file_id, t.table_name
		 )
		 SELECT CASE WHEN name LIKE 'tmp\_%' ESCAPE '\' THEN 'temp' ELSE kind END AS kind,
		        name, version, dt
		 FROM (
		   SELECT kind, name, version, dt FROM standalone
		   UNION ALL SELECT DISTINCT kind, name, version, dt FROM out
		 )
		 ORDER BY kind, name, dt DESC NULLS FIRST;`
	return duckdbDisplay(prelude, sql)
}

// datasetInfoDTO is one row of the orchestrator's `/api/datasets` — the shape of TS DatasetInfo.
// Only the fields the list renders are decoded.
type datasetInfoDTO struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Dt        string `json:"dt"`
	Rows      int64  `json:"rows"`
	State     string `json:"state"`
	Temporary bool   `json:"temporary"`
	Owner     string `json:"owner"`
	// The DERIVED, run-grain name (ADR 0029 §2). The server renders it through the one
	// `data/datasetName.ts` function, so this column is byte-identical to the Datasets page's —
	// this listing never re-derives it. Empty when the row's Run does not resolve (a standalone
	// list; a durable Dataset promoted in from a temp).
	DatasetName string `json:"datasetName"`
	// The ONE Run behind this row — the KEY a tag/rename addresses (ADR 0029 §4). Empty where the
	// name is, and empty on purpose when SEVERAL Runs wrote the partition (see ContributingRuns).
	RunID string `json:"runId"`
	// Every Run whose rows are in this partition, from the lake's own per-file `run_id` statistics.
	// A durable Dataset accumulates from many Runs, so this is the honest plural and RunID is the
	// singular that exists only when there IS one. Absent for a table with no `run_id` column.
	ContributingRuns []string `json:"contributingRuns"`
	// The tag SET (ADR 0029 §1) and the operator RENAME (§4), from the Dataset record. Absent when
	// the Dataset is untagged / un-renamed — only deviation is stored, so the default carries no row.
	Tags      []string `json:"tags"`
	RenamedTo string   `json:"renamedTo"`
}

// effectiveName is what a Dataset is CALLED: the operator RENAME when the record holds one (ADR 0029
// §4), else the DERIVED default (§2). The rename overrides, but the derived string is never lost —
// the server ships both, and the surfaces render `renamedTo ?? datasetName`.
func (d datasetInfoDTO) effectiveName() string {
	if d.RenamedTo != "" {
		return d.RenamedTo
	}
	return d.DatasetName
}

// run is the RUN column: which **Run** produced these rows, which is the question a Dataset in a
// flat list could not answer at all before — `tmp_4e9b1b23` and `lame_demo` both listed with an
// empty run and no name.
//
// IT IS ONE RUN OR A COUNT, NEVER A GUESS. A temporary Dataset has exactly one owning Run and a
// dispatch partition has exactly one that wrote it, so those print the id. A durable Dataset
// accumulates — the partition several promotions landed in has several — so that prints
// `N runs` and points at `kontra dataset provenance` for the breakdown. Printing the first of
// several would be the lie ADR 0017's rule exists to prevent.
func (d datasetInfoDTO) run() string {
	if d.RunID != "" {
		return d.RunID
	}
	if n := len(d.ContributingRuns); n > 1 {
		return fmt.Sprintf("%d runs", n)
	}
	return "-"
}

// datasetListViaAPI lists via the orchestrator, which is the only surface that can read a
// Dataset's temporary/owner markers (they live beside the data in the object store, not in the
// catalog). Ungated, like the preview beside it — no operator SQL crosses the wire.
func datasetListViaAPI(apiURL string) error {
	var infos []datasetInfoDTO
	if err := newAPI(apiURL).getJSON("/api/datasets", &infos); err != nil {
		if isUnreachable(err) {
			return fmt.Errorf("%w: %v", errAPIUnavailable, err)
		}
		return err
	}
	w := tabwriter.NewWriter(cliio.Stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "KIND\tNAME\tVERSION\tDT\tROWS\tSTATE\tRUN\tRUN-GRAIN-NAME\tTAGS")
	for _, d := range infos {
		kind := d.Kind
		// The marker, never the name prefix: a temp is temporary because its Run recorded it so.
		if d.Temporary {
			kind = "temp"
		}
		run := d.run()
		// The run-grain name: the RENAME when the record holds one (ADR 0029 §4), else the DERIVED
		// default (§2) verbatim from the server — the handle you point at while a Dataset is still
		// being written. Empty for rows with no resolvable Run.
		name := d.effectiveName()
		if name == "" {
			name = "-"
		}
		// The tag SET (ADR 0029 §1) as the record holds it; `-` for an untagged Dataset.
		tags := "-"
		if len(d.Tags) > 0 {
			tags = strings.Join(d.Tags, ",")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			kind, d.Name, d.Version, d.Dt, d.Rows, d.State, run, name, tags)
	}
	return w.Flush()
}

// datasetDelete removes a dataset by name, routing by WHAT it is:
//
//   - a temporary Dataset (slice 03) is the one OUTPUT dataset deletable by name, and only the
//     orchestrator can tell a temp from a durable output — it reads the owner marker beside the
//     data, which a bare catalog scan cannot see. So a delete-by-name goes to the API first, which
//     drops the rows + catalog entry, cleans the markers, and reports what it freed.
//   - a durable OUTPUT dataset is refused there (409): actor output is aged out by retention, not
//     deleted by a name. The server's message is surfaced verbatim.
//   - a standalone list is an operator INPUT and is dropped locally, unchanged — `--local` (or an
//     explicit --catalog/--data-path) forces this path, and so does an unreachable orchestrator.
//
// The explicit-only orphan policy lives here and in the API: nothing sweeps a temp on a clock or at
// its Run's close, because a temp exists to outlive its fleet so triage can happen later.
func datasetDelete(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: kontra dataset delete <name> [--local]")
	}
	name, rest := args[0], args[1:]
	fs := flag.NewFlagSet("dataset delete", flag.ContinueOnError)
	local := fs.Bool("local", false, "delete via local DuckDB instead of the orchestrator (standalone lists only; a temp needs the owner marker only the server reads)")
	catalog := fs.String("catalog", "", "DuckLake catalog DSN (default: discovered from the checkout or the running stack)")
	dataPath := fs.String("data-path", "", "lake DATA_PATH (default: KONTRA_DUCKLAKE_DATA_PATH or s3://<bucket>/)")
	apiURL := fs.String("api", orchestratorURL(), "orchestrator base URL")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	if !*local && *catalog == "" && *dataPath == "" {
		err := datasetDeleteViaAPI(*apiURL, name)
		if err == nil {
			return nil
		}
		// A real answer from the server (a 409 durable refusal, a 502) is returned as-is; only an
		// unreachable orchestrator falls back to the local standalone path below.
		if !errors.Is(err, errAPIUnavailable) {
			return err
		}
	}

	// Local: a standalone list drops here; an OUTPUT dataset is refused rather than reported
	// "deleted" having dropped nothing, and a temp cannot be told from durable output offline (the
	// owner marker is in the object store), so it names the orchestrator as the surface that can.
	if dsn, err := lakeCatalog(*catalog); err == nil {
		if schema, sErr := datasetSchemaOf(name, dsn, *dataPath); sErr == nil && schema != lakeSchema {
			return fmt.Errorf("%q is an actor OUTPUT dataset; a temporary one is deleted through the orchestrator "+
				"(it alone reads the owner marker) — start the stack.\n"+
				"  Durable actor output is aged out by retention (snapshot expiry + file cleanup), not deleted by name", name)
		}
	}
	return dbDelete(*catalog, *dataPath, name)
}

// datasetDeleteViaAPI deletes a temporary Dataset over the orchestrator — the same route slice 04's
// UI button calls. The server refuses a durable Dataset (no owner marker), and that refusal comes
// back as a 409 whose JSON `error` is unwrapped and shown, rather than a bare "HTTP 409: {…}".
func datasetDeleteViaAPI(apiURL, name string) error {
	var res struct {
		Deleted bool   `json:"deleted"`
		Name    string `json:"name"`
		Rows    int64  `json:"rows"`
		Bytes   int64  `json:"bytes"`
		Owner   string `json:"owner"`
	}
	if err := newAPI(apiURL).deleteJSON("/api/datasets/"+url.PathEscape(name), &res); err != nil {
		if isUnreachable(err) {
			return fmt.Errorf("%w: %v", errAPIUnavailable, err)
		}
		var he *httpError
		if errors.As(err, &he) {
			var body struct {
				Error string `json:"error"`
			}
			if json.Unmarshal([]byte(he.body), &body) == nil && body.Error != "" {
				return errors.New(body.Error)
			}
		}
		return err
	}
	fmt.Fprintf(cliio.Stdout, "deleted temporary dataset %s (owner %s) — freed %d row%s, %s\n",
		res.Name, res.Owner, res.Rows, plural(int(res.Rows)), humanBytes(res.Bytes))
	return nil
}

// datasetDeviationDTO is the shape every tag/rename route answers with — the Run's whole record
// AFTER the mutation (ADR 0029 §1, §4), so the CLI prints the post-state rather than guessing it.
// `tags: []` with no `renamedTo` is the emptied record; the store keeps no row, the route reports it.
type datasetDeviationDTO struct {
	RunID     string   `json:"runId"`
	Tags      []string `json:"tags"`
	RenamedTo string   `json:"renamedTo"`
}

// datasetTag adds and/or removes TAGS on a Dataset (ADR 0029 §1). Tags are a SET: `--add` is
// idempotent and `--remove` takes one member, so two operators tagging the same Dataset converge
// rather than clobber. The record is keyed by the Run (§4), which the operator names the usual
// kontra way — `<actor>` plus `--version`/`--dt` — resolved to one dispatch through the same
// `/api/datasets/runs` surface the rest of the tooling uses; `--run` addresses it directly.
//
// This never touches Temporal: the record is a durable store the orchestrator owns, so tagging works
// whether or not the Run is still alive — which is the common case, an operator tagging long after
// the run has closed.
func datasetTag(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: kontra dataset tag <name> [--version V] [--dt PREFIX] [--run RUNID] --add TAG [--add TAG]... [--remove TAG]...")
	}
	name, rest := args[0], args[1:]
	fs := flag.NewFlagSet("dataset tag", flag.ContinueOnError)
	var add, remove stringList
	fs.Var(&add, "add", "add a tag (repeatable)")
	fs.Var(&remove, "remove", "remove a tag (repeatable)")
	version := fs.String("version", "", "narrow to one actor version when several ran")
	dt := fs.String("dt", "", "narrow to one dispatch (prefix: date, date+hour, or full)")
	run := fs.String("run", "", "address the record by run id directly, skipping name resolution")
	apiURL := fs.String("api", orchestratorURL(), "orchestrator base URL")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if len(add) == 0 && len(remove) == 0 {
		return errors.New("nothing to do: pass --add TAG and/or --remove TAG")
	}
	runID, err := resolveRecordRun(*apiURL, *run, name, *version, *dt)
	if err != nil {
		return err
	}

	api := newAPI(*apiURL)
	base := "/api/datasets/runs/" + url.PathEscape(runID) + "/tags"
	var dev datasetDeviationDTO
	for _, t := range add {
		if err := api.postJSON(base, map[string]any{"tag": t}, &dev); err != nil {
			return unwrapAPIError(err)
		}
	}
	for _, t := range remove {
		if err := api.deleteJSON(base+"/"+url.PathEscape(t), &dev); err != nil {
			return unwrapAPIError(err)
		}
	}
	printDeviation(name, runID, dev)
	return nil
}

// datasetRename stores an explicit name that overrides the derived default (ADR 0029 §4), or with
// `--reset` drops it so the derived name (§2) stands again. Unlike a tag, a rename is a single
// authoritative choice, not a set — `--to` replaces. Same Run addressing as `dataset tag`.
func datasetRename(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: kontra dataset rename <name> [--version V] [--dt PREFIX] [--run RUNID] (--to NEWNAME | --reset)")
	}
	name, rest := args[0], args[1:]
	fs := flag.NewFlagSet("dataset rename", flag.ContinueOnError)
	to := fs.String("to", "", "the new name (overrides the derived default)")
	reset := fs.Bool("reset", false, "drop the rename, restoring the derived name")
	version := fs.String("version", "", "narrow to one actor version when several ran")
	dt := fs.String("dt", "", "narrow to one dispatch (prefix: date, date+hour, or full)")
	run := fs.String("run", "", "address the record by run id directly, skipping name resolution")
	apiURL := fs.String("api", orchestratorURL(), "orchestrator base URL")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if (*to == "") == !*reset {
		return errors.New("pass exactly one of --to <name> or --reset")
	}
	runID, err := resolveRecordRun(*apiURL, *run, name, *version, *dt)
	if err != nil {
		return err
	}

	api := newAPI(*apiURL)
	path := "/api/datasets/runs/" + url.PathEscape(runID) + "/name"
	var dev datasetDeviationDTO
	if *reset {
		if err := api.deleteJSON(path, &dev); err != nil {
			return unwrapAPIError(err)
		}
	} else {
		if err := api.putJSON(path, map[string]any{"name": *to}, &dev); err != nil {
			return unwrapAPIError(err)
		}
	}
	printDeviation(name, runID, dev)
	return nil
}

// resolveRecordRun turns the operator's address into the Run id the record is keyed by. `--run`
// wins outright; otherwise the actor `name` (+ optional version/dt) is resolved to exactly one
// dispatch through `/api/datasets/runs`. Ambiguity is an error that LISTS the candidates and how to
// narrow them, rather than a silent pick — tagging the wrong run is exactly the mistake to prevent.
func resolveRecordRun(apiURL, run, name, version, dt string) (string, error) {
	if run != "" {
		return run, nil
	}
	v := url.Values{}
	v.Set("actor", name)
	if version != "" {
		v.Set("version", version)
	}
	if dt != "" {
		v.Set("dt", dt)
	}
	var res struct {
		Runs []struct {
			RunID   string `json:"runId"`
			Version string `json:"version"`
			Dt      string `json:"dt"`
		} `json:"runs"`
	}
	if err := newAPI(apiURL).getJSON("/api/datasets/runs?"+v.Encode(), &res); err != nil {
		return "", unwrapAPIError(err)
	}
	switch len(res.Runs) {
	case 0:
		return "", fmt.Errorf("no run wrote %q — `kontra dataset list` shows what exists", name)
	case 1:
		return res.Runs[0].RunID, nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%q matches %d runs — narrow with --version/--dt, or pass --run:", name, len(res.Runs))
		for _, r := range res.Runs {
			fmt.Fprintf(&b, "\n  --dt %s   (v%s, run %s)", r.Dt, r.Version, r.RunID)
		}
		return "", errors.New(b.String())
	}
}

// printDeviation reports a Run's record after a mutation — its tag SET and any rename — so the
// operator reads the post-state, not a hopeful echo of what they asked for.
func printDeviation(name, runID string, dev datasetDeviationDTO) {
	tags := "(none)"
	if len(dev.Tags) > 0 {
		tags = strings.Join(dev.Tags, ", ")
	}
	rename := ""
	if dev.RenamedTo != "" {
		rename = fmt.Sprintf(", renamed to %q", dev.RenamedTo)
	}
	fmt.Fprintf(cliio.Stdout, "%s (run %s): tags [%s]%s\n", name, runID, tags, rename)
}

// unwrapAPIError turns the orchestrator's `{"error":"…"}` body into a plain error, so a 400 (an
// empty tag) reads as its one sentence rather than `HTTP 400: {"error":"…"}`. The same unwrapping
// `datasetDeleteViaAPI` does for its 409 — refusals here are the server's judgement, shown verbatim.
func unwrapAPIError(err error) error {
	var he *httpError
	if errors.As(err, &he) {
		var body struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(he.body), &body) == nil && body.Error != "" {
			return errors.New(body.Error)
		}
	}
	return err
}

// datasetQueryCmd queries ONE dataset by name. The name resolves to a view so the operator's SQL
// references it as a bare table — and that SAME query string is what feeds `dispatch --query`.
func datasetQueryCmd(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(`usage: kontra dataset query <name> --sql "SELECT ... FROM <name> ..." [--export <file>]`)
	}
	name, rest := args[0], args[1:]
	fs := flag.NewFlagSet("dataset query", flag.ContinueOnError)
	sqlText := fs.String("sql", "", "SQL over the dataset (referenced as a bare table by its name)")
	version := fs.String("version", "", "output datasets: only this actor version")
	dt := fs.String("dt", "", "output datasets: only this dispatch time (prefix: date, date+hour, or full)")
	export := fs.String("export", "", "write the result to a file (.csv/.parquet/.json) instead of the screen")
	local := fs.Bool("local", false, "run DuckDB here instead of on the orchestrator (works with the stack down)")
	catalog := fs.String("catalog", "", "DuckLake catalog DSN (default: discovered from the checkout or the running stack)")
	dataPath := fs.String("data-path", "", "lake DATA_PATH (default: KONTRA_DUCKLAKE_DATA_PATH or s3://<bucket>/)")
	apiURL := fs.String("api", orchestratorURL(), "orchestrator base URL")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	q := strings.TrimSpace(*sqlText)
	if q == "" {
		q = fmt.Sprintf(`SELECT * FROM "%s" LIMIT 100`, ident(name))
	}

	// THE ORCHESTRATOR ANSWERS THIS ~25x FASTER, because it already holds an open connection.
	// Locally every invocation pays the same fixed cost twice: ~410 ms to INSTALL/LOAD three
	// extensions and ~95 ms to ATTACH the catalog, once to resolve which schema the name lives
	// in and once to run the query — about 1.3 s before any work happens. The same query over
	// the API is ~50 ms.
	//
	// `--local` (or an explicit --catalog/--data-path, which name a lake that is not this
	// stack's) keeps the standalone path, and so does an unreachable orchestrator — the CLI has
	// always worked with the stack down and still does.
	if !*local && *catalog == "" && *dataPath == "" {
		sel := datasetSel{version: *version, dt: *dt}
		err := datasetQueryViaAPI(*apiURL, name, q, sel, *export)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errAPIUnavailable) {
			return err // a real answer from the server: a SQL error, a 401 — report it as-is
		}
	}

	prelude, err := datasetPrelude(name, datasetSel{version: *version, dt: *dt, catalog: *catalog, dataPath: *dataPath})
	if err != nil {
		return err
	}
	if *export != "" {
		// `COPY … TO '<file>'` infers the format from the extension — no option list. The
		// earlier version passed readerFor(*export), which is the INPUT-side reader builder:
		// it stat'd a file that does not exist yet, and even past that emitted
		// `TO 'f.parquet' (read_parquet('f.parquet'))`, which DuckDB rejects. monitor.go had
		// it right all along; this now matches.
		if _, err := runDuck(prelude + fmt.Sprintf("COPY (%s) TO '%s';", strings.TrimRight(q, "; "), sqlEscape(*export))); err != nil {
			return fmt.Errorf("export failed: %w", err)
		}
		size := int64(0)
		if fi, statErr := os.Stat(*export); statErr == nil {
			size = fi.Size()
		}
		fmt.Fprintf(cliio.Stdout, "wrote %s (%s)\n", *export, humanBytes(size))
		return nil
	}
	return duckdbDisplay(prelude, strings.TrimRight(q, "; ")+";")
}

// datasetSel scopes a dataset query. version/dt narrow an OUTPUT dataset to one version and/or one
// dispatch — they are the partition columns, so filtering on them PRUNES partitions rather than
// scanning the whole actor table. catalog/dataPath are the usual lake overrides.
type datasetSel struct {
	version, dt       string
	catalog, dataPath string
}

// datasetPrelude builds the DuckDB preamble that makes `name` queryable as a bare table: attach the
// lake, then a view aliasing `name` to whichever schema holds it (standalone or output). The view
// is why the query an operator tests is portable verbatim into `dispatch --query`.
//
// --version / --dt fold straight into the VIEW's WHERE. Because version and dt ARE the partition
// columns, that prunes partitions: `--dt 2026-08-03` reads only that day's directories, not the
// whole actor table. They only make sense for OUTPUT datasets (a standalone list has neither
// column), so using them on a standalone dataset is refused rather than erroring deep in DuckDB.
func datasetPrelude(name string, sel datasetSel) (string, error) {
	dsn, err := lakeCatalog(sel.catalog)
	if err != nil {
		return "", err
	}
	schema, err := datasetSchemaOf(name, dsn, sel.dataPath)
	if err != nil {
		return "", err
	}
	if schema != "output" && (sel.version != "" || sel.dt != "") {
		return "", fmt.Errorf("--version/--dt only apply to actor OUTPUT datasets; %q is a %s dataset with no version/dt",
			name, schema)
	}
	where := []string{}
	if sel.version != "" {
		where = append(where, fmt.Sprintf("version = '%s'", sqlEscape(sel.version)))
	}
	if sel.dt != "" {
		// PREFIX match: `2026-08-03` a whole day, `2026-08-03T17` an hour, a full stamp one dispatch.
		where = append(where, fmt.Sprintf("dt LIKE '%s%%'", sqlEscape(sel.dt)))
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	return attachSQL(dsn, lakeDataPath(sel.dataPath)) +
		fmt.Sprintf(`CREATE OR REPLACE VIEW "%s" AS SELECT * FROM lake.%s."%s"%s; `,
			ident(name), schema, ident(name), clause), nil
}

// datasetSchemaOf finds which schema a dataset lives in, so a bare name resolves without the
// operator knowing whether they uploaded it or an actor produced it. Errors (not guesses) when the
// name matches nothing — a silent empty result is how a typo reads as "found nothing".
func datasetSchemaOf(name, dsn, dataPath string) (string, error) {
	probe := attachSQL(dsn, lakeDataPath(dataPath)) + `CREATE SCHEMA IF NOT EXISTS lake.output; ` +
		fmt.Sprintf(`SELECT schema_name FROM duckdb_tables() WHERE database_name='lake' AND table_name='%s';`, sqlEscape(name))
	out, err := duckdbJSON(probe)
	if err != nil {
		return "", err
	}
	arr, err := lastDuckJSON(out)
	if err != nil {
		return "", err
	}
	var rows []struct {
		Schema string `json:"schema_name"`
	}
	if err := json.Unmarshal(arr, &rows); err != nil {
		return "", fmt.Errorf("could not read dataset catalog: %w", err)
	}
	found := map[string]bool{}
	for _, r := range rows {
		found[r.Schema] = true
	}
	for _, s := range datasetSchemas {
		if found[s] {
			return s, nil
		}
	}
	return "", fmt.Errorf("no dataset named %q (looked in %s) — `kontra dataset list` shows what exists",
		name, strings.Join(datasetSchemas, ", "))
}

// errAPIUnavailable marks "the orchestrator isn't reachable" as distinct from "the orchestrator
// answered, and the answer was no". Only the former may fall back to local DuckDB: silently
// re-running a query locally after the server rejected it would turn a 401 into a result.
var errAPIUnavailable = errors.New("orchestrator unreachable")

// datasetQueryViaAPI runs the query on the orchestrator, which already has the lake attached.
//
// Identical semantics to the local path — datasets are addressed by bare name, `--version`/`--dt`
// prune partitions — because it is the same resolution, just on a connection that is already
// open. The engine there is sandboxed (read-only, no local filesystem, configuration locked),
// which is also why `--local` exists: a query that legitimately needs more memory than the
// workbench allows can still be run here.
func datasetQueryViaAPI(apiURL, name, q string, sel datasetSel, export string) error {
	token := exploreToken()
	if token == "" {
		return fmt.Errorf("%w: no KONTRA_EXPLORE_TOKEN", errAPIUnavailable)
	}
	api := newAuthAPI(apiURL, token)

	scope := map[string]any{"name": name}
	if sel.version != "" {
		scope["version"] = sel.version
	}
	if sel.dt != "" {
		scope["dt"] = sel.dt
	}
	body := map[string]any{"sql": strings.TrimRight(q, "; "), "limit": 5000}
	if sel.version != "" || sel.dt != "" {
		body["scope"] = scope
	}

	if export != "" {
		return exportViaAPI(apiURL, token, name, q, sel, export)
	}

	var res struct {
		Columns []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"columns"`
		Rows      [][]any `json:"rows"`
		ElapsedMs int     `json:"elapsedMs"`
		Truncated bool    `json:"truncated"`
	}
	if err := api.postJSON("/api/datasets/query", body, &res); err != nil {
		if isUnreachable(err) {
			return fmt.Errorf("%w: %v", errAPIUnavailable, err)
		}
		return err
	}

	cols := make([]string, len(res.Columns))
	for i, c := range res.Columns {
		cols[i] = c.Name
	}
	printRows(cols, res.Rows)
	note := fmt.Sprintf("%d row%s · %dms", len(res.Rows), plural(len(res.Rows)), res.ElapsedMs)
	if res.Truncated {
		note += " · capped — use --export for the whole result"
	}
	fmt.Fprintln(cliio.Stdout, note)
	return nil
}

// exportViaAPI streams the FULL result to a file. Not row-capped: the cap belongs to the screen.
func exportViaAPI(apiURL, token, name, q string, sel datasetSel, export string) error {
	v := url.Values{}
	v.Set("sql", strings.TrimRight(q, "; "))
	v.Set("format", exportFormatFor(export))
	v.Set("filename", strings.TrimSuffix(filepath.Base(export), filepath.Ext(export)))
	v.Set("name", name)
	if sel.version != "" {
		v.Set("version", sel.version)
	}
	if sel.dt != "" {
		v.Set("dt", sel.dt)
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(apiURL, "/")+"/api/datasets/export?"+v.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errAPIUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("export failed: %s — %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	f, err := os.Create(export)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "wrote %s (%s)\n", export, humanBytes(n))
	return nil
}

// exportFormatFor maps the file extension to the server's format name. `.json` means an array
// and `.jsonl`/`.ndjson` newline-delimited — the same distinction DuckDB's COPY makes.
func exportFormatFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".parquet":
		return "parquet"
	case ".json":
		return "json"
	case ".jsonl", ".ndjson":
		return "jsonl"
	default:
		return "csv"
	}
}

// isUnreachable distinguishes a dead socket from an HTTP error. `do` wraps transport failures
// with the URL, so a status-carrying error (which reads "unexpected status …") is the server
// talking and must NOT trigger a local retry.
func isUnreachable(err error) bool {
	s := err.Error()
	return strings.Contains(s, "connection refused") ||
		strings.Contains(s, "no such host") ||
		strings.Contains(s, "context deadline exceeded") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "EOF")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// printRows renders a result as an aligned table — the same shape DuckDB's own box output has,
// without needing DuckDB here to draw it.
func printRows(cols []string, rows [][]any) {
	w := tabwriter.NewWriter(cliio.Stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(cols, "\t"))
	sep := make([]string, len(cols))
	for i, c := range cols {
		sep[i] = strings.Repeat("-", len(c))
	}
	fmt.Fprintln(w, strings.Join(sep, "\t"))
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i := range cols {
			if i < len(r) {
				cells[i] = cellString(r[i])
			}
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	_ = w.Flush()
}

// cellString renders one cell. JSON numbers decode as float64, so an integer would print as
// `200` only by luck — 1e6 would come out `1e+06`. Formatted with %v after an integral check.
func cellString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case map[string]any, []any:
		b, _ := json.Marshal(t)
		return string(b)
	default:
		return fmt.Sprintf("%v", t)
	}
}
