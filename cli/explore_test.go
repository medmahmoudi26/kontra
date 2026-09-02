// explore_test.go — the orchestrator is faked with httptest; TMPDIR is redirected so no test
// touches the real /tmp. The generated init script IS the contract, so most assertions read the
// SQL it produces — and three tests hand that SQL to a real DuckDB when one is on PATH, because
// a script that parses in a Go test and not in duckdb is worth nothing.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
)

// A dispatch mid-finalization, covering every way a DATASET can present itself. A dataset is
// ONE ACTOR'S output — a sharded dispatch is many `nodes` and one dataset, which is why nothing
// below is addressed by node id:
//
//	cachebuster  FAILED but partially written → a view, AND an error that must reach the operator
//	crawl4ai     complete, two files          → a view, and rows
//	httpx        complete with no files       → the legitimate EMPTY result: no view, still a row
//	subfinder    not materialized yet         → pending: no view, still a row
//
// `pending`/`failed` are derived server-side from the same datasets (data/explore.ts), so they
// name actors that already have a row — they are carried here to keep the fixture honest.
//
// The presigned URLs deliberately carry the physical layout (`output/<actor>/version=…/dt=…`),
// because a real one does — that is what makes "the view is named for the ACTOR, never for the
// physical table" a real assertion rather than a spelling check. cachebuster's error carries a
// newline and a quote pair for the same reason: those are what actually break generated SQL.
const exploreFixture = `{
  "runId": "` + fixtureRunID + `",
  "runStartedAt": 1785700000000,
  "dt": "` + fixtureDt + `",
  "expiresAt": %d,
  "lifecycle": "finalizing",
  "execution": "completed",
  "materialization": {"total":4,"pending":1,"running":0,"complete":2,"failed":1,"rows":22,"bytes":5000},
  "datasets": [
    {"actor":"cachebuster","version":"1.2.0","dt":"` + fixtureDt + `","view":"cachebuster",
     "state":"failed","rows":0,
     "error":"materialize: Could not convert string '' to INT128\n  at insertBatch()",
     "columns":[],"urls":["%s"],"nodes":["n3"]},
    {"actor":"crawl4ai","version":"1.0.0","dt":"` + fixtureDt + `","view":"crawl4ai",
     "state":"complete","rows":22,"error":null,
     "columns":[{"name":"url","type":"VARCHAR"},{"name":"status","type":"BIGINT"}],
     "urls":["%s","%s"],"nodes":["n1","n2"]},
    {"actor":"httpx","version":"2.0.1","dt":"` + fixtureDt + `","view":"httpx",
     "state":"complete","rows":0,"error":null,"columns":[],"urls":[],"nodes":["n4"]},
    {"actor":"subfinder","version":"0.3.0","dt":"` + fixtureDt + `","view":"subfinder",
     "state":"pending","rows":0,"error":null,"columns":[],"urls":[],"nodes":["n5"]}
  ],
  "pending": ["subfinder"],
  "failed": ["cachebuster"]
}`

const (
	// The run id is server-minted and UUID-shaped: `looksLikeRunID` is what decides whether a
	// selector is resolved through /api/datasets/runs or sent straight to /explore.
	fixtureRunID = "11111111-2222-3333-4444-555555555555"
	fixtureDt    = "2026-08-03T17-00-00"

	fixtureURLc = "https://objects.example/output/cachebuster/version=1.2.0/dt=2026-08-03T17-00-00/part-0.parquet?X-Amz-Signature=dead"
	fixtureURL1 = "https://objects.example/output/crawl4ai/version=1.0.0/dt=2026-08-03T17-00-00/part-0.parquet?X-Amz-Signature=beef"
	fixtureURL2 = "https://objects.example/output/crawl4ai/version=1.0.0/dt=2026-08-03T17-00-00/part-1.parquet?X-Amz-Signature=cafe"
)

// One row of GET /api/datasets/runs — the addressing surface (the server's DispatchRef). `nodes`
// is an INT COUNT here, not the manifest's node-id array: two shards, one dataset.
const fixtureRuns = `{"runs":[{"actor":"crawl4ai","version":"1.0.0","runId":"` + fixtureRunID + `",
  "runStartedAt":1785700000000,"dt":"` + fixtureDt + `","nodes":2,"rows":22,"state":"complete"}]}`

// fixtureBody renders the manifest. Pass one URL to point every dataset at a real file (the
// end-to-end test does); pass none for the unreachable defaults.
func fixtureBody(served ...string) string {
	u := []string{fixtureURLc, fixtureURL1, fixtureURL2}
	if len(served) == 1 {
		u = []string{served[0], served[0], served[0]}
	}
	return fmt.Sprintf(exploreFixture, time.Now().Add(15*time.Minute).UnixMilli(), u[0], u[1], u[2])
}

// exploreServer fakes the two endpoints `kontra explore` talks to — /api/datasets/runs, which
// turns an actor name into a run id, and /api/runs/<id>/explore, which mints the manifest — and
// records what the CLI actually sent.
type exploreServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []string      // request-line URIs, in order
	auth string        // Authorization on the last request
	runs func() string // /api/datasets/runs body
}

func newExploreServer(t *testing.T, manifest func() string) *exploreServer {
	t.Helper()
	es := &exploreServer{runs: func() string { return fixtureRuns }}
	es.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		es.mu.Lock()
		es.reqs = append(es.reqs, r.RequestURI)
		es.auth = r.Header.Get("Authorization")
		runs := es.runs
		es.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/datasets/runs" {
			io.WriteString(w, runs())
			return
		}
		io.WriteString(w, manifest())
	}))
	t.Cleanup(es.Close)
	return es
}

// sent returns what the CLI put on the wire, copied under the lock the handler writes with —
// the assertions run on the test goroutine, the handler on the server's.
func (es *exploreServer) sent() ([]string, string) {
	es.mu.Lock()
	defer es.mu.Unlock()
	return append([]string(nil), es.reqs...), es.auth
}

// noRuns makes /api/datasets/runs resolve nothing — an actor that never materialized.
func (es *exploreServer) noRuns() {
	es.mu.Lock()
	defer es.mu.Unlock()
	es.runs = func() string { return `{"runs":[]}` }
}

// printInit runs `kontra explore … --print-init` with stdout captured and TMPDIR redirected.
func printInit(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	var buf bytes.Buffer
	defer swap[io.Writer](&cliio.Stdout, &buf)()
	defer swap[io.Writer](&cliio.Stderr, io.Discard)()
	err := cmdExplore(append(args, "--print-init"))
	return buf.String(), err
}

// --- the generated script ---

// sqlCode reduces the script to what actually EXECUTES: comments and string literals removed,
// one line at a time. Both directions matter — a `--` inside a presigned URL must not swallow
// the statement, and an apostrophe inside a prose comment ("this run's output") must not swallow
// the twenty lines after it. Statements are generated one per line, so a line is the unit.
func sqlCode(script string) string {
	lines := strings.Split(script, "\n")
	for n, line := range lines {
		var b strings.Builder
		in := false
		for i := 0; i < len(line); i++ {
			switch {
			case line[i] == '\'':
				if in && i+1 < len(line) && line[i+1] == '\'' { // '' is an escaped quote
					i++
					continue
				}
				in = !in
			case !in && line[i] == '-' && i+1 < len(line) && line[i+1] == '-':
				i = len(line) // the rest of the line is a comment
			case !in:
				b.WriteByte(line[i])
			}
		}
		lines[n] = b.String()
	}
	return strings.Join(lines, "\n")
}

func TestExplorePrintInitBuildsOneViewPerDataset(t *testing.T) {
	srv := newExploreServer(t, func() string { return fixtureBody() })
	t.Setenv("KONTRA_EXPLORE_TOKEN", "tok")
	t.Setenv("KONTRA_STATE_TOKEN", "")

	script, err := printInit(t, fixtureRunID, "--api", srv.URL)
	if err != nil {
		t.Fatalf("print-init: %v", err)
	}

	// One view per DATASET with files — reading exactly the URLs the API handed back. Two
	// files from two shards are ONE view: the operator queries an actor, not a node.
	if !strings.Contains(script, `CREATE OR REPLACE VIEW "crawl4ai" AS SELECT * FROM read_parquet(['`+fixtureURL1+`', '`+fixtureURL2+`']);`) {
		t.Errorf("crawl4ai's view must read exactly the two presigned URLs:\n%s", script)
	}
	if !strings.Contains(script, `CREATE OR REPLACE VIEW "cachebuster" AS SELECT * FROM read_parquet(['`+fixtureURLc+`']);`) {
		t.Errorf("a partially-written FAILED dataset still has readable files — it must get a view:\n%s", script)
	}
	// httpx is `complete` with no files (a real empty result) and subfinder has not been written
	// yet. A view over zero URLs aborts DuckDB's whole init file, and there is nothing to read.
	for _, v := range []string{`VIEW "httpx"`, `VIEW "subfinder"`} {
		if strings.Contains(script, v) {
			t.Errorf("a dataset with no files must not get a view (%s):\n%s", v, script)
		}
	}
	if !strings.Contains(script, "CREATE OR REPLACE VIEW kontra_datasets AS") {
		t.Error("the landing view kontra_datasets is missing")
	}
	if n := strings.Count(script, "CREATE OR REPLACE VIEW"); n != 3 {
		t.Errorf("want 3 views (crawl4ai, cachebuster, kontra_datasets), got %d:\n%s", n, script)
	}
	// The resource posture is a stated property of this command, not an accident.
	for _, want := range []string{
		"SET autoinstall_known_extensions=false;", "SET autoload_known_extensions=false;",
		"INSTALL httpfs;", "LOAD httpfs;", "SET memory_limit='2GB';", "SET threads=2;",
		"SET max_temp_directory_size='2GB';", "SET temp_directory='",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("init script missing %q", want)
		}
	}
	// The default path must not pull the web UI in: that extension fetches assets online.
	if strings.Contains(sqlCode(script), "LOAD ui") {
		t.Error("the default (non --ui) script must not load the ui extension")
	}
}

// `ds_<hash>` was the physical table name and `output.<actor>` is the current one. Neither is
// something an operator should ever have to type: the view is the ACTOR.
var physicalTable = regexp.MustCompile(`ds_[0-9a-f]{16}`)

func TestExploreNeverNamesThePhysicalTable(t *testing.T) {
	srv := newExploreServer(t, func() string { return fixtureBody() })
	t.Setenv("KONTRA_EXPLORE_TOKEN", "tok")
	t.Setenv("KONTRA_STATE_TOKEN", "")

	script, err := printInit(t, fixtureRunID, "--api", srv.URL)
	if err != nil {
		t.Fatalf("print-init: %v", err)
	}
	if physicalTable.MatchString(script) {
		t.Errorf("a hashed physical table name leaked into the script:\n%s", script)
	}
	if code := sqlCode(script); strings.Contains(code, "output.") {
		t.Errorf("the schema-qualified physical table leaked into executable SQL:\n%s", code)
	}
	if !strings.Contains(script, `CREATE OR REPLACE VIEW "crawl4ai" AS`) {
		t.Errorf("the view must be named for the actor:\n%s", script)
	}
	// The physical layout IS in the presigned URL — that is the object's real path, and it is
	// opaque to the operator. That is why the check above reads executable SQL, not the file.
	if !strings.Contains(script, "output/crawl4ai/version=1.0.0/") {
		t.Fatal("fixture is broken: the presigned URLs should carry the physical path")
	}
}

func TestExploreGeneratesNoUnboundedSelect(t *testing.T) {
	srv := newExploreServer(t, func() string { return fixtureBody() })
	t.Setenv("KONTRA_EXPLORE_TOKEN", "tok")
	t.Setenv("KONTRA_STATE_TOKEN", "")

	script, err := printInit(t, fixtureRunID, "--api", srv.URL)
	if err != nil {
		t.Fatalf("print-init: %v", err)
	}
	starter := false
	for _, line := range strings.Split(script, "\n") {
		if !strings.Contains(line, "SELECT *") {
			continue
		}
		// A CREATE VIEW body is a definition, not a scan — it is evaluated by whatever queries
		// it, and the queries this file suggests are exactly what is under test here.
		if strings.HasPrefix(strings.TrimLeft(strings.TrimSpace(line), "- "), "CREATE") {
			continue
		}
		if !strings.Contains(line, "LIMIT") {
			t.Errorf("unbounded SELECT * in generated SQL: %q", line)
		}
		if strings.Contains(line, "LIMIT 500") {
			starter = true
		}
	}
	if !starter {
		t.Error("no bounded starter query (SELECT * ... LIMIT 500) was generated")
	}
	// cachebuster has files too, but it FAILED — the starter query points at the first dataset
	// that is actually complete.
	if !strings.Contains(script, `SELECT * FROM "crawl4ai" LIMIT 500;`) {
		t.Errorf("the starter query should point at the first COMPLETE dataset's view:\n%s", script)
	}
}

// --- addressing: an actor at an hour, never a copied UUID ---

func TestExploreResolvesAnActorSelector(t *testing.T) {
	srv := newExploreServer(t, func() string { return fixtureBody() })
	t.Setenv("KONTRA_EXPLORE_TOKEN", "tok")
	t.Setenv("KONTRA_STATE_TOKEN", "")

	// What a person actually types. No run id anywhere on the command line.
	script, err := printInit(t, "crawl4ai@1.0.0", "--api", srv.URL, "--dt", "2026-08-03")
	if err != nil {
		t.Fatalf("print-init: %v", err)
	}
	reqs, _ := srv.sent()
	// The whole translation in one line: actor@version split, --dt passed through, and the
	// run id the resolver got back landing in the explore path.
	want := "/api/datasets/runs?actor=crawl4ai&version=1.0.0&dt=2026-08-03 " +
		"/api/runs/" + fixtureRunID + "/explore?ttl=900"
	if got := strings.Join(reqs, " "); got != want {
		t.Errorf("wrong request sequence:\n got  %s\n want %s", got, want)
	}
	if !strings.Contains(script, `CREATE OR REPLACE VIEW "crawl4ai" AS`) {
		t.Errorf("the resolved run's datasets must become views:\n%s", script)
	}
}

// A bare run id still works — for scripts, and for the run a dispatch just printed. It must NOT
// be resolved: there is no actor to look up, and a lookup would only produce a confusing miss.
func TestExploreRunIDSkipsResolution(t *testing.T) {
	srv := newExploreServer(t, func() string { return fixtureBody() })
	srv.noRuns() // if the CLI resolved, it would fail here rather than reaching /explore
	t.Setenv("KONTRA_EXPLORE_TOKEN", "tok")
	t.Setenv("KONTRA_STATE_TOKEN", "")

	if _, err := printInit(t, fixtureRunID, "--api", srv.URL); err != nil {
		t.Fatalf("print-init: %v", err)
	}
	reqs, _ := srv.sent()
	if got, want := strings.Join(reqs, " "), "/api/runs/"+fixtureRunID+"/explore?ttl=900"; got != want {
		t.Errorf("a run id must go straight to the manifest:\n got  %s\n want %s", got, want)
	}
}

func TestExploreUnknownSelectorPointsAtList(t *testing.T) {
	srv := newExploreServer(t, func() string { return fixtureBody() })
	srv.noRuns()
	t.Setenv("KONTRA_EXPLORE_TOKEN", "tok")
	t.Setenv("KONTRA_STATE_TOKEN", "")

	_, err := printInit(t, "no-such-actor", "--api", srv.URL, "--dt", "2026-08-03")
	if err == nil {
		t.Fatal("an actor with no materialized output must be an error, not an empty workspace")
	}
	// Naming what was asked for AND the command that answers "what is there?" — the miss is
	// almost always a wrong hour, which `--list` shows and a bare "not found" does not.
	for _, want := range []string{"no materialized output", "no-such-actor", "2026-08-03", "--list"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q, got: %v", want, err)
		}
	}
	// It failed on the ADDRESS: no manifest was requested, so nothing was presigned.
	if reqs, _ := srv.sent(); len(reqs) != 1 {
		t.Errorf("only the resolve call may be made, got %v", reqs)
	}
}

// --- admission ---

func TestExploreWithoutTokenMakesNoRequest(t *testing.T) {
	srv := newExploreServer(t, func() string { return fixtureBody() })
	t.Setenv("KONTRA_EXPLORE_TOKEN", "")
	t.Setenv("KONTRA_STATE_TOKEN", "")
	// The token is DISCOVERED from the checkout's .env when the environment has none, so this
	// has to run somewhere without one — otherwise the repo's own .env satisfies the very thing
	// the test is asserting is absent, and it passes while proving nothing. (Same trap as the
	// ambient-KONTRA_STATE_TOKEN one in exploreRoutes.test.ts.)
	t.Chdir(t.TempDir())
	// …and no `docker` to ask either: discovery falls back to the RUNNING orchestrator's env,
	// so on any machine with the stack up this test would otherwise find a real token and prove
	// nothing. Both escape hatches have to be shut for "no token configured" to be reachable.
	t.Setenv("PATH", "")

	// A run id goes straight to the token-gated endpoint, so this is the token check alone.
	_, err := printInit(t, fixtureRunID, "--api", srv.URL)
	if err == nil {
		t.Fatal("a missing token must be an error, not a silent unauthenticated attempt")
	}
	for _, want := range []string{"KONTRA_EXPLORE_TOKEN", "KONTRA_STATE_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %s, got: %v", want, err)
		}
	}
	if reqs, _ := srv.sent(); len(reqs) != 0 {
		t.Errorf("no request may be sent without a token, got %v", reqs)
	}
}

func TestExploreSendsBearerAndTTL(t *testing.T) {
	srv := newExploreServer(t, func() string { return fixtureBody() })
	// Clear the higher-precedence var explicitly. `exploreToken` prefers KONTRA_EXPLORE_TOKEN,
	// so a developer with it exported (sourcing .env is the normal way to run against a live
	// stack) would have this test send THEIR token and still pass on a weaker assertion —
	// or, as happened, fail confusingly. t.Setenv only sets; it does not isolate.
	t.Setenv("KONTRA_EXPLORE_TOKEN", "")
	t.Setenv("KONTRA_STATE_TOKEN", "fallback-tok")

	if _, err := printInit(t, fixtureRunID, "--api", srv.URL, "--ttl", "120"); err != nil {
		t.Fatalf("print-init: %v", err)
	}
	reqs, auth := srv.sent()
	if auth != "Bearer fallback-tok" {
		t.Errorf("KONTRA_STATE_TOKEN must be used as the fallback bearer, got %q", auth)
	}
	if got, want := strings.Join(reqs, " "), "/api/runs/"+fixtureRunID+"/explore?ttl=120"; got != want {
		t.Errorf("wrong request:\n got  %s\n want %s", got, want)
	}

	// KONTRA_EXPLORE_TOKEN wins when both are set — the same order the server checks.
	t.Setenv("KONTRA_EXPLORE_TOKEN", "explore-tok")
	t.Setenv("KONTRA_STATE_TOKEN", "")
	if _, err := printInit(t, fixtureRunID, "--api", srv.URL); err != nil {
		t.Fatalf("print-init: %v", err)
	}
	if _, auth := srv.sent(); auth != "Bearer explore-tok" {
		t.Errorf("KONTRA_EXPLORE_TOKEN must take precedence, got %q", auth)
	}
}

// --- failed output must never read as empty output ---

func TestExploreFailedDatasetsAppearInKontraDatasets(t *testing.T) {
	srv := newExploreServer(t, func() string { return fixtureBody() })
	t.Setenv("KONTRA_EXPLORE_TOKEN", "tok")
	t.Setenv("KONTRA_STATE_TOKEN", "")

	script, err := printInit(t, fixtureRunID, "--api", srv.URL)
	if err != nil {
		t.Fatalf("print-init: %v", err)
	}
	stmt := kontraDatasetsStatement(t, script)
	// The failed dataset, WITH its error text — dropping either is exactly how missing output
	// starts looking like empty output.
	if !strings.Contains(stmt, "'cachebuster'") || !strings.Contains(stmt, "'failed'") {
		t.Errorf("the failed dataset cachebuster is missing from kontra_datasets:\n%s", stmt)
	}
	if !strings.Contains(stmt, "Could not convert string") {
		t.Errorf("cachebuster's error text must reach the operator:\n%s", stmt)
	}
	// Not-yet-written and empty-but-successful are different answers, and both are answers.
	if !strings.Contains(stmt, "'subfinder'") || !strings.Contains(stmt, "'pending'") {
		t.Errorf("a pending dataset must still get a row:\n%s", stmt)
	}
	if !strings.Contains(stmt, "'httpx'") {
		t.Errorf("a complete dataset with no files must still get a row:\n%s", stmt)
	}
}

// The generated VALUES table has to survive a real parser with a real quote in the error text —
// asserting on the Go string alone would pass on SQL DuckDB refuses to run.
func TestExploreKontraDatasetsViewExecutes(t *testing.T) {
	requireDuckDB(t)
	srv := newExploreServer(t, func() string { return fixtureBody() })
	t.Setenv("KONTRA_EXPLORE_TOKEN", "tok")
	t.Setenv("KONTRA_STATE_TOKEN", "")

	script, err := printInit(t, fixtureRunID, "--api", srv.URL)
	if err != nil {
		t.Fatalf("print-init: %v", err)
	}
	out, err := duckdbJSON(kontraDatasetsStatement(t, script) +
		` SELECT actor, state, coalesce(error,'') AS error, "rows" FROM kontra_datasets ORDER BY actor;`)
	if err != nil {
		t.Fatalf("generated kontra_datasets is not valid SQL: %v", err)
	}
	var rows []struct {
		Actor, State, Error string
		Rows                int64
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("parse duckdb output: %v\n%s", err, out)
	}
	if len(rows) != 4 {
		t.Fatalf("every recorded dataset must have a row, got %d: %+v", len(rows), rows)
	}
	byActor := map[string]string{}
	for _, r := range rows {
		byActor[r.Actor] = r.State
	}
	for actor, want := range map[string]string{
		"cachebuster": "failed", "crawl4ai": "complete", "httpx": "complete", "subfinder": "pending",
	} {
		if byActor[actor] != want {
			t.Errorf("dataset %s: state %q, want %q", actor, byActor[actor], want)
		}
	}
	// ORDER BY actor: cachebuster, crawl4ai, httpx, subfinder.
	if rows[1].Rows != 22 {
		t.Errorf("crawl4ai row count lost: %+v", rows[1])
	}
	// The quote pair inside the error survives escaping — and it is a newline in the manifest,
	// collapsed here so one statement stays one line.
	if want := "materialize: Could not convert string '' to INT128 at insertBatch()"; rows[0].Error != want {
		t.Errorf("cachebuster error:\n got  %q\n want %q", rows[0].Error, want)
	}
}

// A run with nothing materialized still has to produce a workspace that OPENS: DuckDB exits on
// the first bad statement in an init file, so an empty VALUES list would cost the operator the
// whole session instead of showing them an empty table.
func TestExploreKontraDatasetsHandlesARunWithNoDatasets(t *testing.T) {
	requireDuckDB(t)
	empty := &exploreManifest{RunID: fixtureRunID, Lifecycle: "executing", Execution: "running"}
	script := exploreInitSQL(empty, t.TempDir(), false)
	if strings.Contains(script, "VALUES )") || strings.Contains(script, "VALUES ()") {
		t.Fatalf("empty VALUES list generated:\n%s", script)
	}
	out, err := duckdbJSON(kontraDatasetsStatement(t, script) + ` SELECT actor, state FROM kontra_datasets;`)
	if err != nil {
		t.Fatalf("kontra_datasets for a run with no datasets is not valid SQL: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "[]" {
		t.Errorf("want an empty result, got %s", got)
	}
	if strings.Contains(script, `SELECT * FROM ""`) {
		t.Error("a starter query was generated with no view to point at")
	}
}

// kontraDatasetsStatement pulls the one-line CREATE for the landing view out of the script.
func kontraDatasetsStatement(t *testing.T, script string) string {
	t.Helper()
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "CREATE OR REPLACE VIEW kontra_datasets") {
			return line
		}
	}
	t.Fatalf("no kontra_datasets statement in:\n%s", script)
	return ""
}

// --- the finalizing notice ---

func TestExploreSummaryReportsFinalizingAndFailures(t *testing.T) {
	var m exploreManifest
	if err := json.Unmarshal([]byte(fixtureBody()), &m); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	exploreSummary(&buf, &m)
	got := buf.String()

	// "still being written" must be said, with the counts that make it checkable.
	for _, want := range []string{"FINALIZING", "4 total", "2 complete", "1 pending", "1 failed", "22 rows"} {
		if !strings.Contains(got, want) {
			t.Errorf("finalizing notice missing %q in:\n%s", want, got)
		}
	}
	// …and it continues: the readable datasets are listed anyway.
	if !strings.Contains(got, "crawl4ai") || !strings.Contains(got, "22") {
		t.Errorf("complete datasets must still be shown while finalizing:\n%s", got)
	}
	// The failed dataset is counted, flagged loudly, and shown with its error.
	if !strings.Contains(got, "1 dataset(s) FAILED") {
		t.Errorf("a failed dataset must be called out, not left as a table row:\n%s", got)
	}
	if !strings.Contains(got, "MISSING, not empty") {
		t.Errorf("the failed-vs-empty distinction must be stated:\n%s", got)
	}
	if !strings.Contains(got, "Could not convert string") {
		t.Errorf("the failure's error text must be shown:\n%s", got)
	}
	// A dataset with no files must not advertise a view that was never created.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "SELECT * FROM httpx") || strings.Contains(line, "SELECT * FROM subfinder") {
			t.Errorf("no files means no view — it must not be advertised: %q", line)
		}
	}
}

// --- workspace: 0600, and swept ---

func TestExploreWorkspaceIsPrivateAndCleanedUp(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	ws, err := newExploreWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.writeInit("SELECT 1;\n"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(ws.initPath)
	if err != nil {
		t.Fatal(err)
	}
	// The script holds the presigned URLs; on a shared box 0644 hands them to everyone.
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("init script mode %04o, want 0600", perm)
	}
	if perm := dirPerm(t, ws.dir); perm != 0o700 {
		t.Errorf("workspace dir mode %04o, want 0700", perm)
	}
	ws.cleanup()
	if _, err := os.Stat(ws.dir); !os.IsNotExist(err) {
		t.Errorf("workspace must be removed on exit, stat gave %v", err)
	}
}

func TestExploreCommandLeavesNothingBehind(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	srv := newExploreServer(t, func() string { return fixtureBody() })
	t.Setenv("KONTRA_EXPLORE_TOKEN", "tok")
	t.Setenv("KONTRA_STATE_TOKEN", "")

	var buf bytes.Buffer
	defer swap[io.Writer](&cliio.Stdout, &buf)()
	defer swap[io.Writer](&cliio.Stderr, io.Discard)()
	if err := cmdExplore([]string{fixtureRunID, "--api", srv.URL, "--print-init"}); err != nil {
		t.Fatalf("print-init: %v", err)
	}
	if got := leftoverExploreDirs(t, tmp); len(got) != 0 {
		t.Errorf("workspace left behind after a clean exit: %v", got)
	}
}

func TestScavengeStaleExploreDirs(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, exploreDirPrefix+"old")
	fresh := filepath.Join(root, exploreDirPrefix+"new")
	other := filepath.Join(root, "not-ours")
	// Same prefix, NOT our workspace: control/orchestrator/src/exploreRoutes.test.ts mkdtemps
	// `kontra-explore-*` into the very same TMPDIR. Sweeping by name would eat it.
	lookalike := filepath.Join(root, exploreDirPrefix+"AbC123")
	for _, d := range []string{stale, fresh, other, lookalike} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{stale, fresh} {
		if err := os.Mkdir(filepath.Join(d, exploreSpillDir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(lookalike, "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Workspaces whose process was killed before the defer ran: still 0600, still full of
	// (by now stale) URLs, and nothing else will ever remove them.
	old := time.Now().Add(-2 * time.Hour)
	for _, d := range []string{stale, other, lookalike} {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}

	scavengeExploreDirs(root, time.Hour)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a stale workspace must be swept, stat gave %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a fresh workspace must survive — another explore may be using it: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("only kontra-explore-* dirs are ours to delete: %v", err)
	}
	if _, err := os.Stat(lookalike); err != nil {
		t.Errorf("a same-prefix dir we did not create must survive: %v", err)
	}
}

// --- cross-run catalog mode ---

func TestExploreCatalogRequiresItsOwnCredentials(t *testing.T) {
	srv := newExploreServer(t, func() string { return fixtureBody() })
	t.Setenv("KONTRA_CATALOG_PG", "")
	// AND NO APPLIANCE CATALOG on this machine either, which is the other source `crossRunCatalog`
	// accepts. Without this the test reads the DEVELOPER'S OWN ~/.kontra/data/datasets.ducklake.
	t.Setenv("KONTRA_HOME", t.TempDir())
	t.Setenv("KONTRA_EXPLORE_TOKEN", "tok")
	t.Setenv("KONTRA_STATE_TOKEN", "")

	_, err := printInit(t, "--catalog", "--api", srv.URL)
	if err == nil {
		t.Fatal("--catalog without read-only credentials must fail")
	}
	for _, want := range []string{"KONTRA_CATALOG_PG", "read-only"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q, got: %v", want, err)
		}
	}
	if reqs, _ := srv.sent(); len(reqs) != 0 {
		t.Errorf("--catalog is cross-run: it must not call the exact-run endpoint (%v)", reqs)
	}
}

func TestExploreCatalogAttachesReadOnly(t *testing.T) {
	srv := newExploreServer(t, func() string { return fixtureBody() })
	t.Setenv("KONTRA_CATALOG_PG", "postgres://reader@lake/kontra")
	t.Setenv("KONTRA_DUCKLAKE_DATA_PATH", "")
	t.Setenv("KONTRA_S3_BUCKET", "kontra")
	t.Setenv("KONTRA_S3_PUBLIC_ENDPOINT", "")
	t.Setenv("KONTRA_S3_ENDPOINT", "http://objects.example:8333")
	t.Setenv("KONTRA_S3_ACCESS_KEY", "ro-key")
	t.Setenv("KONTRA_S3_SECRET_KEY", "ro-secret")
	t.Setenv("KONTRA_S3_REGION", "eu-west-1")

	script, err := printInit(t, "--catalog", "--api", srv.URL)
	if err != nil {
		t.Fatalf("catalog print-init: %v", err)
	}
	// Note the DOUBLED scheme: DuckLake consumes `postgres:` to pick its metadata backend and
	// hands the rest to the postgres extension, so a libpq URI needs the prefix in front of it.
	// Without it DuckLake reads the DSN as a file path — verified live, where it failed with
	// `Cannot open database "/cwd/postgres://reader@lake/kontra"`.
	//
	// DATA_PATH is the BUCKET ROOT: tables live at `output/<actor>/version=…/dt=…`, and a
	// DATA_PATH that disagrees with what the catalog recorded makes ATTACH itself refuse.
	if !strings.Contains(script, "ATTACH 'ducklake:postgres:postgres://reader@lake/kontra' AS lake (DATA_PATH 's3://kontra/', READ_ONLY);") {
		t.Errorf("the ATTACH must be READ_ONLY, at the bucket root, with the provisioned DSN:\n%s", script)
	}
	for _, want := range []string{
		"KEY_ID 'ro-key'", "SECRET 'ro-secret'", "ENDPOINT 'objects.example:8333'", "REGION 'eu-west-1'",
		"SET allow_community_extensions=false;", "SET autoinstall_known_extensions=false;",
		"CREATE OR REPLACE VIEW kontra_datasets AS",
		// Identity IS the layout: the actor is the table name and version/dt are partition
		// values on each file, so listing them is a pivot over catalog metadata — no data file
		// opened, and nothing to decode.
		"ducklake_file_partition_value", "partition_key_index",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("catalog script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "_kontra_dataset") {
		t.Errorf("the hashed dataset registry is gone — nothing may still join it:\n%s", script)
	}
	if reqs, _ := srv.sent(); len(reqs) != 0 {
		t.Errorf("--catalog must not call the exact-run endpoint (%v)", reqs)
	}
}

// A plain libpq DSN must still ATTACH. DuckLake selects its metadata backend from a scheme
// prefix; without one it treats the DSN as a file path and fails with a "database does not
// exist" error naming the whole connstring — which reads like a missing file, not a missing
// prefix. Caught live: `--catalog` was unusable with the DSN form the docs suggest.
func TestCatalogDSNGetsItsSchemePrefix(t *testing.T) {
	cases := map[string]string{
		"dbname=k host=h user=u":   "postgres:dbname=k host=h user=u",
		"postgresql://u:p@h/db":    "postgres:postgresql://u:p@h/db",
		"postgres://u:p@h/db":      "postgres:postgres://u:p@h/db",
		"postgres:dbname=k host=h": "postgres:dbname=k host=h",
		"sqlite:/tmp/cat.db":       "sqlite:/tmp/cat.db",
	}
	for in, want := range cases {
		if got := ducklakeCatalogDSN(in); got != want {
			t.Errorf("ducklakeCatalogDSN(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCatalogInitAttachesWithAScheme(t *testing.T) {
	t.Setenv("KONTRA_CATALOG_PG", "dbname=kontra_ducklake host=127.0.0.1 user=kontra")
	script, err := catalogInitSQL(t.TempDir())
	if err != nil {
		t.Fatalf("catalogInitSQL: %v", err)
	}
	if !strings.Contains(script, "ATTACH 'ducklake:postgres:dbname=kontra_ducklake") {
		t.Fatalf("ATTACH is missing the postgres: scheme:\n%s", script)
	}
}

// --- end to end: manifest → view → query, through a real DuckDB over real HTTP ---

func TestExploreSQLQueriesTheRunsOutput(t *testing.T) {
	requireDuckDB(t)
	data := t.TempDir()
	pq := filepath.Join(data, "part-0.parquet")
	if _, err := duckdbJSON(fmt.Sprintf(
		"COPY (SELECT 'https://a' AS url UNION ALL SELECT 'https://b') TO '%s' (FORMAT parquet);", sqlEscape(pq))); err != nil {
		t.Skipf("cannot write a parquet fixture: %v", err)
	}
	if _, err := duckdbJSON("INSTALL httpfs; LOAD httpfs;"); err != nil {
		t.Skipf("httpfs unavailable (offline?): %v", err)
	}

	// The object store stands in for presigned URLs: same shape (query string and all), and
	// DuckDB has to range-read it over HTTP exactly as it would the real thing.
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, pq)
	}))
	t.Cleanup(files.Close)
	// Every dataset points at the served file: one unreadable URL anywhere aborts DuckDB's whole
	// init file (it exits on init errors), which is precisely why explore re-fetches rather than
	// caching, and why the dataset states are printed by the CLI before DuckDB starts.
	served := files.URL + "/part-0.parquet?X-Amz-Signature=beef"
	srv := newExploreServer(t, func() string { return fixtureBody(served) })

	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("KONTRA_EXPLORE_TOKEN", "tok")
	t.Setenv("KONTRA_STATE_TOKEN", "")
	var buf bytes.Buffer
	defer swap[io.Writer](&cliio.Stdout, &buf)()
	defer swap[io.Writer](&cliio.Stderr, io.Discard)()

	// crawl4ai's two shards, both the 2-row fixture → 4 rows through the ONE generated view.
	if err := cmdExplore([]string{"crawl4ai", "--api", srv.URL,
		"--sql", `SELECT count(*)::VARCHAR || ' rows seen' AS n FROM "crawl4ai";`}); err != nil {
		t.Fatalf("explore --sql: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "4 rows seen") {
		t.Errorf("the generated view must read the dispatch's parquet over HTTP, got:\n%s", buf.String())
	}
}

// --- helpers ---

func requireDuckDB(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("duckdb"); err != nil {
		if _, err := duckdbBin(); err != nil {
			t.Skip("duckdb not installed — skipping the SQL execution check")
		}
	}
}

func dirPerm(t *testing.T, dir string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func leftoverExploreDirs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), exploreDirPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// The metadata schema is the catalog's, and the two backends have NO schema in common. Measured
// both ways: against the live Postgres catalog `public` answers rows and `main` answers `Catalog
// Error: … schema "main" does not exist`; against a file catalog it is exactly reversed. So a
// constant here is a query that works on one backend and fails on the other.
func TestLakeMetaSchemaFollowsTheBackend(t *testing.T) {
	cases := map[string]string{
		"postgres:dbname=kontra_ducklake host=db user=kontra": "public",
		"postgresql://kontra@db/kontra_ducklake":              "public",
		"/root/.kontra/data/datasets.ducklake":                "main",
		"datasets.ducklake":                                   "main",
	}
	for dsn, want := range cases {
		if got := lakeMetaSchema(dsn); got != want {
			t.Errorf("lakeMetaSchema(%q) = %q, want %q", dsn, got, want)
		}
		if got, wantQ := lakeMeta(dsn, "ducklake_table"), "__ducklake_metadata_lake."+want+".ducklake_table"; got != wantQ {
			t.Errorf("lakeMeta(%q) = %q, want %q", dsn, got, wantQ)
		}
	}
}

// A FILE PATH MUST NOT BE PREFIXED. `ducklake:postgres:/path/to/datasets.ducklake` hands the
// postgres extension a libpq DSN made of a filename, which is the mirror of the bug
// ducklakeCatalogDSN was written to prevent — and it is reachable now that the appliance's default
// catalog IS a path.
func TestDucklakeCatalogDSNLeavesAFilePathAlone(t *testing.T) {
	for _, path := range []string{"/root/.kontra/data/datasets.ducklake", "datasets.ducklake", "./cat.ducklake"} {
		if got := ducklakeCatalogDSN(path); got != path {
			t.Errorf("ducklakeCatalogDSN(%q) = %q, want it untouched", path, got)
		}
	}
}
