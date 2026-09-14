// workflow_test.go — the pure bits of the caller's VERBS: the argument shape every subcommand
// takes, the one-argument rule for --input, the refusals `start` makes before it dials anything,
// and the identity it stamps on the orchestrator afterwards. Serving and starting for real are
// validated live; none of that is here.
//
// The other three seams have their own suites, for the same reason the source does:
//
//	identity_test.go        the queues and the digests
//	workflowworker_test.go  the launch profile a served worker gets
//	claimcheck_test.go      the CLI's arm of shared/conformance/codec/fixtures.json
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestLeadingPositional covers the shape every other CLI has trained the hand for: subject
// first, options after. Go's flag package stops at the first positional, so `serve x.py --python
// p` would otherwise parse as three loose arguments and NO flags — and the flag would be dropped
// in silence rather than applied.
func TestLeadingPositional(t *testing.T) {
	cases := []struct {
		in    []string
		first string
		rest  []string
	}{
		{[]string{"sweep.py", "--python", "p"}, "sweep.py", []string{"--python", "p"}},
		{[]string{"--python", "p", "sweep.py"}, "", []string{"--python", "p", "sweep.py"}},
		{[]string{}, "", []string{}},
		{[]string{"--wait"}, "", []string{"--wait"}},
	}
	for _, c := range cases {
		first, rest := leadingPositional(c.in)
		if first != c.first || !reflect.DeepEqual(rest, c.rest) {
			t.Errorf("leadingPositional(%v) = (%q,%v), want (%q,%v)", c.in, first, rest, c.first, c.rest)
		}
	}
}

func TestParseWorkflowInput(t *testing.T) {
	if _, has, err := parseWorkflowInput(""); has || err != nil {
		t.Errorf("no --input means no argument at all, got has=%v err=%v", has, err)
	}

	// A JSON array is ONE list argument, not two arguments.
	arg, has, err := parseWorkflowInput(`["a","b"]`)
	if err != nil || !has {
		t.Fatalf("parse failed: %v", err)
	}
	if !reflect.DeepEqual(arg, []any{"a", "b"}) {
		t.Errorf("array must arrive as one list argument, got %#v", arg)
	}

	if _, _, err := parseWorkflowInput("{not json"); err == nil {
		t.Error("malformed JSON must fail at the flag, not inside the workflow")
	}

	f := filepath.Join(t.TempDir(), "in.json")
	if err := os.WriteFile(f, []byte(`{"depth":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	arg, _, err = parseWorkflowInput("@" + f)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(arg, map[string]any{"depth": float64(2)}) {
		t.Errorf("@file must be read from disk, got %#v", arg)
	}
}

// TestWorkflowStartRefusesWhenNothingServesTheQueue pins the loud refusal the issue demands: start
// takes a FOLDER, derives the queue from its MANIFEST, and refuses BEFORE dialing when no worker
// polls that queue — naming the fix instead of dispatching onto a queue nobody serves (which would
// sit `running` forever). The refusal fires ahead of any Temporal dial, so this stays a unit test.
func TestWorkflowStartRefusesWhenNothingServesTheQueue(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir)

	restore := pollerCount
	defer func() { pollerCount = restore }()

	var asked string
	pollerCount = func(queue string) (int, error) {
		asked = queue
		return 0, nil // nothing is serving
	}

	err := workflowStart([]string{dir})
	if err == nil {
		t.Fatal("start onto a queue with no pollers must refuse, not dispatch and hope")
	}
	if !strings.Contains(err.Error(), "no pollers") || !strings.Contains(err.Error(), "kontra workflow serve") {
		t.Fatalf("refusal must name the fix, got: %v", err)
	}
	if asked != "wf-canary-0.2.0" {
		t.Fatalf("start checked pollers on %q, want the manifest-derived queue", asked)
	}
}

// A folder with no @workflow.defn class in its manifest cannot be started — there is no type to
// dispatch — and start says so instead of dialing Temporal with an empty type.
func TestWorkflowStartRefusesAFolderWithNoWorkflowClass(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "workflow.py"), []byte("# caller\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workflow.json"), []byte(`{"name":"x","version":"0.1.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := workflowStart([]string{dir})
	if err == nil || !strings.Contains(err.Error(), "@workflow.defn") {
		t.Fatalf("want a refusal naming the missing class, got %v", err)
	}
}

// TestRecordRunWorkflowStampsTheCallerIdentity covers the CLI's half of ADR 0029 §2's snapshot.
//
// `kontra workflow start` dials Temporal directly — it never goes through `POST /api/runs`, which is
// where the orchestrator stamps this for itself — so without this call the name a Dataset carries
// would depend on WHICH command started the run. That is the divergence the derived name exists to
// remove, so both paths write the same record.
func TestRecordRunWorkflowStampsTheCallerIdentity(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"runId":"nscheck-1755612727","workflow":"nscheck","version":"0.1.0"}`))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("KONTRA_RUN_TOKEN", "s3cret")
	api := newAuthAPI(srv.URL, os.Getenv("KONTRA_RUN_TOKEN"))
	m := workflowManifest{Name: "nscheck", Version: "0.1.0", Workflow: "NsCheck"}
	if err := recordRunWorkflow(api, "nscheck-1755612727", m); err != nil {
		t.Fatalf("record: %v", err)
	}
	// PUT, so a retried stamp states the same fact rather than racing; keyed by the RUN id, which is
	// the workflow id (ADR 0023 §12) and the key /api/datasets joins the name on.
	if gotMethod != http.MethodPut {
		t.Errorf("method %q, want PUT — the stamp is an upsert", gotMethod)
	}
	if want := "/api/runs/nscheck-1755612727/workflow"; gotPath != want {
		t.Errorf("path %q, want %q", gotPath, want)
	}
	// The MANIFEST's name and version, never the queue the run was served on (ADR 0029 §2).
	if gotBody["workflow"] != "nscheck" || gotBody["version"] != "0.1.0" {
		t.Errorf("body %v, want the manifest's name and version", gotBody)
	}
	// The route is gated by the run token, like start and stop.
	if gotAuth != "Bearer s3cret" {
		t.Errorf("authorization %q, want the run token", gotAuth)
	}
}

// A run id with a slash or a space in it (an operator's `--id`) must not be able to reshape the URL.
func TestRecordRunWorkflowEscapesTheRunID(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	if err := recordRunWorkflow(newAPI(srv.URL), "a/b c", workflowManifest{Name: "n", Version: "1"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if want := "/api/runs/a%2Fb%20c/workflow"; gotPath != want {
		t.Errorf("path %q, want %q", gotPath, want)
	}
}

// A flat `.py` workflow has no manifest, so there is no identity to snapshot — and calling the
// orchestrator with a blank one would be a 400 the operator cannot act on. It sends NOTHING.
func TestRecordRunWorkflowSendsNothingWithoutAManifest(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)

	for _, m := range []workflowManifest{{}, {Name: "nscheck"}, {Version: "0.1.0"}, {Name: " ", Version: " "}} {
		if err := recordRunWorkflow(newAPI(srv.URL), "flat-1755612727", m); err != nil {
			t.Fatalf("record %+v: %v", m, err)
		}
	}
	if called {
		t.Error("a workflow with no manifest identity must not stamp a half one")
	}
}

// `kontra workflow serve` needs the checkout on PYTHONPATH. Without a --repo override it walks
// up from CWD looking for docker-compose.yml, which fails when an operator runs the command from
// inside or beside a workflow folder. --repo must let them name the checkout explicitly, just like
// `kontra infra` and `kontra release` already do.
func TestWorkflowServeArgsAcceptsRepoOverride(t *testing.T) {
	// A checkout root is wherever docker-compose.yml lives AND sdk/python is present.
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte("services:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "sdk", "python", "kontra"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The workflow folder itself is NOT inside the checkout for this test — we want to prove the
	// command no longer depends on walking up from CWD.
	wfDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wfDir, "workflow.py"), []byte("# stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "workflow.json"), []byte(`{"name":"x","version":"0.1.0","workflow":"X"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Run from a directory with NO docker-compose.yml in any parent.
	workDir := t.TempDir()
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	// Keep python resolution deterministic: no venv in the fake repo, and no env override.
	t.Setenv("KONTRA_PYTHON", "")

	_, file, root, _, _, _, err := workflowServeArgs([]string{"--repo", repo, wfDir})
	if err != nil {
		t.Fatalf("workflowServeArgs with --repo should find repo root, got: %v", err)
	}
	if root != repo {
		t.Errorf("root = %q, want %q", root, repo)
	}
	wantFile := filepath.Join(wfDir, "workflow.py")
	if file != wantFile {
		t.Errorf("file = %q, want %q", file, wantFile)
	}
}

// Without --repo, the command must still walk up from CWD and find the checkout the way it
// always has. This guards the refactor: the new helper must not break the existing path.
func TestWorkflowServeArgsWalksUpFromCWDByDefault(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte("services:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "sdk", "python", "kontra"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Put the workflow INSIDE the repo so walking up from it finds docker-compose.yml.
	wfDir := filepath.Join(repo, "examples", "workflows", "firstrun")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "workflow.py"), []byte("# stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "workflow.json"), []byte(`{"name":"x","version":"0.1.0","workflow":"X"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(wfDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	t.Setenv("KONTRA_PYTHON", "")

	_, file, root, _, _, _, err := workflowServeArgs([]string{"."})
	if err != nil {
		t.Fatalf("workflowServeArgs without --repo should walk up from CWD, got: %v", err)
	}
	// macOS symlinks /var to /private/var; EvalSymlinks makes the comparison stable.
	cleanRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	cleanRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if cleanRoot != cleanRepo {
		t.Errorf("root = %q, want %q", cleanRoot, cleanRepo)
	}
	wantFile, err := filepath.EvalSymlinks(filepath.Join(wfDir, "workflow.py"))
	if err != nil {
		t.Fatal(err)
	}
	cleanFile, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	if cleanFile != wantFile {
		t.Errorf("file = %q, want %q", cleanFile, wantFile)
	}
}
