package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lastDuckJSON must return the SELECT's array even when earlier statements in the prelude also
// emit rows. `CREATE OR REPLACE SECRET` returns `[{"Success":true}]`, so duckdb -json output is
// two arrays; an earlier version unmarshalled the whole blob as one array, failed, ignored the
// error, and reported every dataset as "not found". This is that regression, pinned.
func TestLastDuckJSONTakesTheFinalArray(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			"secret-then-select",
			"[{\"Success\":true}]\n[{\"schema_name\":\"standalone\"}]",
			`[{"schema_name":"standalone"}]`,
		},
		{
			"pretty-printed multi-line array",
			"[{\"Success\":true}]\n[{\"a\":1},\n{\"a\":2}]",
			"[{\"a\":1},\n{\"a\":2}]",
		},
		{"single array", `[{"x":1}]`, `[{"x":1}]`},
		{"no array at all", "", "[]"},
		{"only a success row object stream", `{"Success":true}`, "[]"},
	}
	for _, c := range cases {
		got, err := lastDuckJSON([]byte(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(got) != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// A dataset name resolves standalone-first: an operator who just `dataset create`d a list means
// that list, not an actor's output that happens to share the name.
func TestDatasetSchemaResolutionOrder(t *testing.T) {
	if len(datasetSchemas) < 2 || datasetSchemas[0] != lakeSchema || datasetSchemas[1] != "output" {
		t.Fatalf("resolution order must be [standalone, output], got %v", datasetSchemas)
	}
}

// datasetPrelude turns --version/--dt into a partition-pruning WHERE on the view, so `dataset
// query` and `dispatch --query` scope to one version/dispatch without the operator hand-writing
// the predicate. This exercises the SQL construction against a real DuckLake (an output-shaped
// table partitioned by version/dt), including the standalone guard.
func TestDatasetPreludeVersionDtScoping(t *testing.T) {
	if _, err := duckdbBin(); err != nil {
		t.Skip("duckdb not on PATH")
	}
	dir := t.TempDir()
	dsn := filepath.Join(dir, "cat.ducklake")
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	// Build an output-schema table partitioned by (version, dt), two versions across two dts.
	setup := fmt.Sprintf(
		"INSTALL ducklake; LOAD ducklake; ATTACH 'ducklake:%s' AS lake (DATA_PATH '%s/'); "+
			"CREATE SCHEMA lake.output; "+
			"CREATE TABLE lake.output.crawl4ai AS SELECT 'a' AS host, '1.0.0' AS version, '2026-08-03T17-00-00' AS dt LIMIT 0; "+
			"ALTER TABLE lake.output.crawl4ai SET PARTITIONED BY (version, dt); "+
			"INSERT INTO lake.output.crawl4ai VALUES "+
			"('a','1.0.0','2026-08-03T17-00-00'),('b','1.0.0','2026-08-04T09-00-00'),('c','2.0.0','2026-08-04T09-00-00');",
		dsn, data)
	if _, err := runDuck(setup); err != nil {
		t.Fatalf("setup: %v", err)
	}
	catDSN := dsn // plain path; attachSQL adds the ducklake: prefix (double-prefixing breaks the ATTACH)

	count := func(sel datasetSel) int {
		sel.catalog, sel.dataPath = catDSN, data+"/"
		prelude, err := datasetPrelude("crawl4ai", sel)
		if err != nil {
			t.Fatalf("prelude(%+v): %v", sel, err)
		}
		out, err := duckdbJSON(prelude + `SELECT count(*) AS n FROM "crawl4ai";`)
		if err != nil {
			t.Fatalf("query(%+v): %v", sel, err)
		}
		arr, _ := lastDuckJSON(out)
		var rows []struct {
			N int `json:"n"`
		}
		if err := json.Unmarshal(arr, &rows); err != nil || len(rows) == 0 {
			t.Fatalf("bad count json %q: %v", arr, err)
		}
		return rows[0].N
	}

	if n := count(datasetSel{}); n != 3 {
		t.Errorf("no filter: got %d, want 3", n)
	}
	if n := count(datasetSel{version: "1.0.0"}); n != 2 {
		t.Errorf("--version 1.0.0: got %d, want 2", n)
	}
	if n := count(datasetSel{dt: "2026-08-04"}); n != 2 {
		t.Errorf("--dt whole-day: got %d, want 2", n)
	}
	if n := count(datasetSel{dt: "2026-08-04T09"}); n != 2 {
		t.Errorf("--dt date+hour: got %d, want 2", n)
	}
	if n := count(datasetSel{version: "2.0.0", dt: "2026-08-04"}); n != 1 {
		t.Errorf("--version + --dt: got %d, want 1", n)
	}
	if n := count(datasetSel{dt: "1999-01-01"}); n != 0 {
		t.Errorf("non-matching dt: got %d, want 0", n)
	}
}

// `kontra dataset ls` (over the API) prints the DERIVED, run-grain name (ADR 0029 §2) VERBATIM from
// the server — the CLI never re-derives it, which is what keeps the string identical to the Datasets
// page's. A row whose Run does not resolve carries no name and shows `-`, distinct from one that
// does. This pins the column and that the server's string reaches the operator untouched.
func TestDatasetListViaAPIRendersTheDerivedName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/datasets" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// One output Dataset the server named + tagged, and one standalone list it could not (no Run).
		_, _ = w.Write([]byte(`[
			{"kind":"output","name":"nscheck","version":"0.1.0","dt":"2026-08-19T14-32-07","rows":623,
			 "state":"open","datasetName":"wf-nscheck-0.1.0--2026-08-19T14-32-07Z--a3f9c1",
			 "runId":"a3f9c1e2-run","tags":["prod","nightly"]},
			{"kind":"standalone","name":"domains","version":"","dt":"","rows":119,"state":""}
		]`))
	}))
	defer srv.Close()

	out := withStdout(t, func() {
		if err := datasetListViaAPI(srv.URL); err != nil {
			t.Fatalf("datasetListViaAPI: %v", err)
		}
	})

	if !strings.Contains(out, "RUN-GRAIN-NAME") || !strings.Contains(out, "TAGS") {
		t.Errorf("listing is missing the RUN-GRAIN-NAME/TAGS columns:\n%s", out)
	}
	// The server's exact name string, unmodified by the CLI.
	if !strings.Contains(out, "wf-nscheck-0.1.0--2026-08-19T14-32-07Z--a3f9c1") {
		t.Errorf("run-grain name missing or altered:\n%s", out)
	}
	// The tag SET, verbatim from the record.
	if !strings.Contains(out, "prod,nightly") {
		t.Errorf("tags column missing the record's tags:\n%s", out)
	}
	// The row with no resolvable Run shows a `-`, not a blank the eye reads as the row above it.
	domains := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "domains") {
			domains = line
		}
	}
	if domains == "" || !strings.HasSuffix(strings.TrimSpace(domains), "-") {
		t.Errorf("standalone row should end in a bare `-` for its empty name/tags, got %q", domains)
	}
}

// The RUN column answers "which run made this?" — the question a flat listing could not answer at
// all before: on the local controller `tmp_4e9b1b23` and `lame_demo` both printed an empty run and
// no name. A temp and a single-Run partition print the id; a partition several Runs promoted into
// prints the COUNT, because printing the first of several would be a lie.
func TestDatasetListViaAPIRendersTheRunColumn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/datasets" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"kind":"output","name":"tmp_nscheck-1787150959_4e9b1b23","version":"0.1.0",
			 "dt":"2026-08-19T14-49-20","rows":623,"state":"sealed","temporary":true,
			 "owner":"nscheck-1787150959","runId":"nscheck-1787150959",
			 "contributingRuns":["nscheck-1787150959"],
			 "datasetName":"wf-nscheck-0.1.0--2026-08-19T14-49-20Z--4e9b1b"},
			{"kind":"output","name":"lame","version":"0","dt":"2026-08-16T17-37-53","rows":1246,
			 "state":"sealed","contributingRuns":["nscheck-1786819045","nscheck-1786831339"]}
		]`))
	}))
	defer srv.Close()

	out := withStdout(t, func() {
		if err := datasetListViaAPI(srv.URL); err != nil {
			t.Fatalf("datasetListViaAPI: %v", err)
		}
	})

	if !strings.Contains(out, "RUN\t") && !strings.Contains(out, "RUN ") {
		t.Errorf("listing is missing the RUN column:\n%s", out)
	}
	// The temp names its owning Run outright — the whole point of a run-derived identity.
	if !strings.Contains(out, "nscheck-1787150959") {
		t.Errorf("temp row should print its owning run:\n%s", out)
	}
	// The many-Run durable row counts them instead of naming one.
	if !strings.Contains(out, "2 runs") {
		t.Errorf("a partition several Runs wrote should print the count:\n%s", out)
	}
	if strings.Contains(out, "nscheck-1786819045") {
		t.Errorf("a many-Run row must NOT print one of its Runs as if it were the Run:\n%s", out)
	}
}

// The run-grain name column shows the RENAME when the record holds one (ADR 0029 §4), not the
// derived default — but the server still ships the derived string beside it, so the CLI chooses
// which to print and never loses the fallback.
func TestDatasetListViaAPIShowsRenameOverDerived(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/datasets" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"kind":"output","name":"lame","version":"0.1.0","dt":"2026-08-19T14-32-07","rows":623,
			 "state":"open","datasetName":"wf-lame-0.1.0--2026-08-19T14-32-07Z--a3f9c1",
			 "runId":"a3f9c1e2-run","renamedTo":"the-interesting-sweep"}
		]`))
	}))
	defer srv.Close()

	out := withStdout(t, func() {
		if err := datasetListViaAPI(srv.URL); err != nil {
			t.Fatalf("datasetListViaAPI: %v", err)
		}
	})
	if !strings.Contains(out, "the-interesting-sweep") {
		t.Errorf("rename should be shown as the run-grain name:\n%s", out)
	}
	if strings.Contains(out, "wf-lame-0.1.0--2026-08-19T14-32-07Z--a3f9c1") {
		t.Errorf("the derived name must be overridden by the rename in the column:\n%s", out)
	}
}

// `kontra dataset tag`/`rename` resolve the Run the record is keyed by from `/api/datasets/runs`,
// then mutate it — a tag ADDED and REMOVED (a set), a rename PUT. This pins the requests each verb
// makes end to end against a stub, including that the resolved run id lands in the path.
func TestDatasetTagAndRenameHitTheRecordRoutes(t *testing.T) {
	const runID = "a3f9c1e2-7b04-4a1d-9c88-0f21e6b3d5aa"
	type call struct{ method, path, body string }
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := ""
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			body = strings.TrimSpace(string(b))
		}
		calls = append(calls, call{r.Method, r.URL.Path, body})
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/datasets/runs" {
			// One dispatch resolves, so the verbs never have to disambiguate.
			_, _ = w.Write([]byte(`{"runs":[{"runId":"` + runID + `","version":"0.1.0","dt":"2026-08-19T14-32-07"}]}`))
			return
		}
		// Every mutation answers with the post-state deviation.
		_, _ = w.Write([]byte(`{"runId":"` + runID + `","tags":["prod"]}`))
	}))
	defer srv.Close()

	// tag --add prod --remove old : one resolve, one POST, one DELETE.
	out := withStdout(t, func() {
		if err := datasetTag([]string{"nscheck", "--add", "prod", "--remove", "old", "--api", srv.URL}); err != nil {
			t.Fatalf("datasetTag: %v", err)
		}
	})
	if len(calls) != 3 {
		t.Fatalf("tag made %d calls, want 3 (resolve, add, remove): %+v", len(calls), calls)
	}
	if calls[0].method != http.MethodGet || !strings.HasPrefix(calls[0].path, "/api/datasets/runs") {
		t.Errorf("first call should resolve the run: %+v", calls[0])
	}
	if calls[1].method != http.MethodPost || calls[1].path != "/api/datasets/runs/"+runID+"/tags" {
		t.Errorf("add should POST the resolved run's tags: %+v", calls[1])
	}
	if !strings.Contains(calls[1].body, `"tag":"prod"`) {
		t.Errorf("add body should carry the tag: %q", calls[1].body)
	}
	if calls[2].method != http.MethodDelete || calls[2].path != "/api/datasets/runs/"+runID+"/tags/old" {
		t.Errorf("remove should DELETE the named tag: %+v", calls[2])
	}
	if !strings.Contains(out, "tags [prod]") {
		t.Errorf("tag should print the post-state set: %q", out)
	}

	// rename --to with --run : resolution skipped, exactly one PUT carrying the name.
	calls = nil
	if err := datasetRename([]string{"nscheck", "--to", "the-sweep", "--api", srv.URL, "--run", runID}); err != nil {
		t.Fatalf("datasetRename: %v", err)
	}
	if len(calls) != 1 || calls[0].method != http.MethodPut || calls[0].path != "/api/datasets/runs/"+runID+"/name" {
		t.Fatalf("rename --to should PUT the name once with --run: %+v", calls)
	}
	if !strings.Contains(calls[0].body, `"name":"the-sweep"`) {
		t.Errorf("rename body should carry the new name: %q", calls[0].body)
	}
}

// A tag verb with nothing to do, and a rename with neither/both of --to/--reset, are refused before
// any request — a no-op mutation should never reach the server as one.
func TestDatasetTagRenameArgGuards(t *testing.T) {
	if err := datasetTag([]string{"nscheck", "--api", "http://unused"}); err == nil {
		t.Error("tag with no --add/--remove should error")
	}
	if err := datasetRename([]string{"nscheck", "--api", "http://unused"}); err == nil {
		t.Error("rename with neither --to nor --reset should error")
	}
	if err := datasetRename([]string{"nscheck", "--to", "x", "--reset", "--api", "http://unused"}); err == nil {
		t.Error("rename with both --to and --reset should error")
	}
}
