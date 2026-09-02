// dispatch_test.go — httptest fakes of the orchestrator API; stdout is swapped so no test
// dials Temporal or Docker.
package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// swap replaces a package var for the test and returns the restore func. Used across most of
// the suite, so it outlives the dispatch tests it was written beside.
func swap[T any](p *T, v T) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

func jsonHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
}

// withStdout captures everything a func writes to the package's stdout var.
func withStdout(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := stdout
	stdout = &buf
	defer func() { stdout = old }()
	fn()
	return buf.String()
}

// --- version resolution ---

func TestResolveActor(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/actors", jsonHandler(`[
		{"key":"beacon@0.1.0","name":"beacon","version":"0.1.0"},
		{"key":"echo@0.1.0","name":"echo","version":"0.1.0"},
		{"key":"echo@0.2.0","name":"echo","version":"0.2.0"}]`))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	api := newAPI(srv.URL)

	if name, version, err := resolveActor(api, "beacon"); err != nil || name != "beacon" || version != "0.1.0" {
		t.Fatalf("bare unique name: got (%q,%q,%v)", name, version, err)
	}
	if name, version, err := resolveActor(api, "echo@0.2.0"); err != nil || name != "echo" || version != "0.2.0" {
		t.Fatalf("pinned version: got (%q,%q,%v)", name, version, err)
	}
	if _, _, err := resolveActor(api, "echo"); err == nil ||
		!strings.Contains(err.Error(), "0.1.0") || !strings.Contains(err.Error(), "0.2.0") {
		t.Fatalf("ambiguous bare name must die listing versions, got %v", err)
	}
	if _, _, err := resolveActor(api, "nope"); err == nil {
		t.Fatal("unknown actor must error")
	}
	if _, _, err := resolveActor(api, "echo@9.9.9"); err == nil {
		t.Fatal("unknown version must error")
	}
}

// --- the retired dispatch verb ---

// `kontra actor <name> dispatch` must EXPLAIN itself rather than fall through to the usage
// line. The word was this CLI's way in for long enough that an operator still types it, and
// "unknown command" would read as a broken build rather than a moved capability.
func TestActorDispatchIsARedirect(t *testing.T) {
	err := cmdActor([]string{"echo", "dispatch", "--input", "-"})
	if err == nil {
		t.Fatal("dispatch must not succeed — the route it posted to is gone")
	}
	for _, want := range []string{"is gone", "kontra workflow serve", "kontra workflow start"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("redirect should mention %q, got: %v", want, err)
		}
	}
}

func TestActorRoutesRegister(t *testing.T) {
	// Not "register" and not "dispatch" → the usage line, which names the one live verb.
	err := cmdActor([]string{"echo"})
	if err == nil || !strings.Contains(err.Error(), "kontra actor register") {
		t.Fatalf("want the register usage line, got %v", err)
	}
}
