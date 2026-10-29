package mcprelay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// M3 review round 4: regression tests of the gate's MCP relay.

// r4-relay-1 (D265), the POST half: a tools/call's event stream reaches the client without the upstream's event ids,
// so that no stock client resumes it with a GET (the TypeScript SDK resumes any cut POST stream that carried an id):
// stock server-everything@1.0.0, whose event store sends a priming event with an id on every POST stream, as a direct
// backend. The calls row is the one response the client reads.
func TestReview4CallStreamIsNotResumable(t *testing.T) {
	u := startEverything(t)
	e := newEnv(t)
	r := e.replica("a", replicaOpts{})
	e.service("ev", u, nil)
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	endpoint := r.url + "/mcp/ev"
	c.initialize(endpoint)
	if rep := c.do(http.MethodPost, endpoint, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); rep.status != http.StatusAccepted {
		t.Fatalf("initialized: %d %s", rep.status, rep.body)
	}
	rep := c.do(http.MethodPost, endpoint, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"trigger-long-running-operation","arguments":{"duration":1,"steps":1}}}`, nil)
	if rep.status != http.StatusOK || !strings.HasPrefix(rep.header.Get("Content-Type"), "text/event-stream") ||
		!strings.Contains(string(rep.body), "Long running operation completed") {
		t.Fatalf("the call: %d %s", rep.status, rep.body)
	}
	for _, l := range strings.Split(string(rep.body), "\n") {
		if l == "id" || strings.HasPrefix(l, "id:") {
			t.Fatalf("the call's stream carries an event id a client would resume it with: %q\n%s", l, rep.body)
		}
	}
	rows := e.calls("ev")
	if len(rows) != 1 || rows[0].isError == nil || *rows[0].isError {
		t.Fatalf("calls rows %+v, want one with is_error false", rows)
	}
}

// r4-relay-1 (D265), the GET half: an upstream that answers a resumed GET with a response (a replayed call's, or a
// response a seller puts there so that the client takes it while the POST's stream is metered another): the GET
// stream reaches the client up to that response, never the response.
func TestReview4GETCarriesNoResponse(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &m)
			if m.Method != "initialize" {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "up-1")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(m.ID)+`,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"up","version":"1"}}}`)
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "id: ev-2\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\",\"data\":\"BEFORE\"}}\n\n")
			_, _ = io.WriteString(w, "id: ev-3\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"REPLAYED-RESULT\"}],\"isError\":false}}\n\n")
			_, _ = io.WriteString(w, "id: ev-4\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\",\"data\":\"AFTER\"}}\n\n")
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(up.Close)
	e := newEnv(t)
	r := e.replica("a", replicaOpts{})
	e.service("up", up.URL+"/mcp", nil)
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	endpoint := r.url + "/mcp/up"
	c.initialize(endpoint)
	rep := c.do(http.MethodGet, endpoint, "", map[string]string{"Accept": "text/event-stream", "Last-Event-ID": "ev-1"})
	if rep.status != http.StatusOK || !strings.Contains(string(rep.body), "BEFORE") {
		t.Fatalf("the GET: %d %s", rep.status, rep.body)
	}
	if strings.Contains(string(rep.body), "REPLAYED-RESULT") || strings.Contains(string(rep.body), "AFTER") {
		t.Fatalf("the GET stream relays a response:\n%s", rep.body)
	}
}

// r4-relay-2 (D249): the texts an operator reads give the gate's eviction bound, EvictIdle (2 minutes since D249), not
// D231's 5 minutes: the schema's description of max_sessions_per_principal, config.Service's doc and tunnel.md §10.
func TestReview4EvictIdleIsWhatTheTextsSay(t *testing.T) {
	want := "idle for 2 minutes"
	if EvictIdle.Minutes() != 2 {
		t.Fatalf("EvictIdle is %v: change the texts below with it", EvictIdle)
	}
	for _, f := range []struct{ path, around string }{
		{"../../spec/schema/config.json", "max_sessions_per_principal"},
		{"../config/config.go", "MaxSessionsPerPrincipal"},
		{"../../spec/tunnel.md", "for that principal's new one"},
	} {
		b, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		i := strings.Index(s, f.around)
		if i < 0 {
			t.Fatalf("%s: no %q", f.path, f.around)
		}
		window := s[max(0, i-800):min(len(s), i+800)]
		if strings.Contains(window, "idle for 5 minutes") || !strings.Contains(window, want) {
			t.Errorf("%s near %q does not say %q", f.path, f.around, want)
		}
	}
}
