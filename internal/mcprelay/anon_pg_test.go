package mcprelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/pg"
)

// fakeCalls is an anonymous tenant's calls cap (anon.Caps.Call) as the relay sees it: at most limit slots, counted
// here; full refuses every call.
type fakeCalls struct {
	mu       sync.Mutex
	limit    int
	inFlight int
	taken    int
	released int
	asked    []string
}

func (f *fakeCalls) Call(_ context.Context, _ uuid.UUID, slug string) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, slug)
	if f.inFlight >= f.limit {
		return nil, fmt.Errorf("%w (calls %d)", anon.ErrTooManyCalls, f.limit)
	}
	f.inFlight++
	f.taken++
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.inFlight--
			f.released++
			f.mu.Unlock()
		})
	}, nil
}

func (f *fakeCalls) set(limit int) {
	f.mu.Lock()
	f.limit = limit
	f.mu.Unlock()
}

func (f *fakeCalls) counts() (inFlight, taken, released int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight, f.taken, f.released
}

// spec/tunnel.md §6.2 (D287): a plain tools/call to an anonymous tenant's service takes one of its calls slots for
// as long as its answer is relayed; with none free it is 429 with -32600 "rate limited" and Retry-After, before
// anything is forwarded or metered, in both eras. tools/list, answered from the gate's cache, takes no slot.
func TestPlainCallsAreCapped(t *testing.T) {
	e := newEnv(t)
	up, url := dualEra(t, mcptest.Tool("echo", ""))
	e.service("svc", url, nil)
	calls := &fakeCalls{limit: 100}
	r := e.replica("a", replicaOpts{withDeps: func(_ *replica, deps *Deps, _ *pg.Listener, _ *slog.Logger) { deps.Calls = calls }})
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	c.initialize(r.url + "/mcp/svc")
	modern := newModern(t, key, r.url+"/mcp/svc", false)
	_, taken0, released0 := calls.counts() // the initialize's slot (D304)

	refused := func(what string, rep reply) {
		t.Helper()
		code, _ := rpcError(t, rep)
		msg, _ := rep.msg(t)["error"].(map[string]any)["message"].(string)
		if rep.status != http.StatusTooManyRequests || code != -32600 || msg != "rate limited" || rep.header.Get("Retry-After") != "5" {
			t.Fatalf("%s: %d %v %s", what, rep.status, rep.header, rep.body)
		}
	}
	// No slot free (calls in flight elsewhere, on any replica).
	calls.set(0)
	before := up.Count("tools/call")
	refused("legacy, full", c.call(r.url+"/mcp/svc", 2, "echo", "x"))
	refused("modern, full", modern.req(3, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "x"}}))
	if up.Count("tools/call") != before {
		t.Fatal("a refused call reached the upstream")
	}
	if n := scalarInt(t, e, `select count(*) from kontra.calls`); n != 0 {
		t.Fatalf("%d calls rows for refused calls", n)
	}
	// A free slot: the call goes through and gives its slot back once answered; tools/list takes none.
	calls.set(1)
	if rep := c.call(r.url+"/mcp/svc", 4, "echo", "y"); rep.status != http.StatusOK {
		t.Fatalf("legacy with a slot: %d %s", rep.status, rep.body)
	}
	if rep := modern.req(5, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "y"}}); rep.status != http.StatusOK {
		t.Fatalf("modern with a slot: %d %s", rep.status, rep.body)
	}
	if rep := c.do(http.MethodPost, r.url+"/mcp/svc", `{"jsonrpc":"2.0","id":6,"method":"tools/list"}`, nil); rep.status != http.StatusOK {
		t.Fatalf("tools/list: %d", rep.status)
	}
	if inFlight, taken, released := calls.counts(); inFlight != 0 || taken-taken0 != 2 || released-released0 != 2 {
		t.Fatalf("slots: %d in flight, %d taken, %d released", inFlight, taken-taken0, released-released0)
	}
	calls.mu.Lock()
	asked := calls.asked
	calls.mu.Unlock()
	for _, s := range asked {
		if s != "default" {
			t.Errorf("the cap was asked for tenant %q", s)
		}
	}
	if n := scalarInt(t, e, `select count(*) from kontra.calls`); n != 2 {
		t.Fatalf("%d calls rows, want 2", n)
	}
}

// spec/tunnel.md §6.2 (D304): not only tools/call. Every other request the relay forwards to an anonymous tenant's
// service takes one of its calls slots while its answer is relayed (an initialize, resources/read, prompts/get,
// completion/complete, a method the gate does not know; on a modern request too), and a GET stream holds one for as long
// as it is open; with none free each is 429 with -32600 "rate limited" and Retry-After, and the upstream sees nothing.
// A notification and a client response take none.
func TestEveryRelayedRequestIsCapped(t *testing.T) {
	e := newEnv(t)
	up, url := dualEra(t, mcptest.Tool("echo", ""))
	e.service("svc", url, nil)
	calls := &fakeCalls{limit: 100}
	r := e.replica("a", replicaOpts{withDeps: func(_ *replica, deps *Deps, _ *pg.Listener, _ *slog.Logger) { deps.Calls = calls }})
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	svcURL := r.url + "/mcp/svc"
	c.initialize(svcURL)
	if inFlight, taken, released := calls.counts(); inFlight != 0 || taken != 1 || released != 1 {
		t.Fatalf("the initialize's slot: %d in flight, %d taken, %d released", inFlight, taken, released)
	}
	modern := newModern(t, key, svcURL, false)
	refused := func(what string, rep reply) {
		t.Helper()
		code, _ := rpcError(t, rep)
		msg, _ := rep.msg(t)["error"].(map[string]any)["message"].(string)
		if rep.status != http.StatusTooManyRequests || code != -32600 || msg != "rate limited" || rep.header.Get("Retry-After") != "5" {
			t.Fatalf("%s: %d %v %s", what, rep.status, rep.header, rep.body)
		}
	}
	methods := []string{"resources/read", "prompts/get", "completion/complete", "x/anything"}
	upstream := func() int {
		n := 0
		for _, m := range append([]string{"initialize"}, methods...) {
			n += up.Count(m)
		}
		return n
	}

	// No slot free.
	calls.set(0)
	before := upstream()
	params := map[string]map[string]any{"resources/read": {"uri": "file:///x"}, "prompts/get": {"name": "p"},
		"completion/complete": {"ref": map[string]any{"type": "ref/prompt", "name": "p"}, "argument": map[string]any{"name": "a", "value": "v"}},
		"x/anything":          {}}
	for i, m := range methods {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 10 + i, "method": m, "params": params[m]})
		refused("legacy "+m, c.do(http.MethodPost, svcURL, string(b), nil))
		refused("modern "+m, modern.req(20+i, m, params[m]))
	}
	refused("legacy GET stream", c.do(http.MethodGet, svcURL, "", map[string]string{"Accept": "text/event-stream"}))
	other := newClient(t, key)
	refused("initialize", other.do(http.MethodPost, svcURL, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25",`+
		`"capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, map[string]string{"MCP-Protocol-Version": ""}))
	if n := upstream() - before; n != 0 {
		t.Fatalf("%d refused requests reached the upstream", n)
	}
	// A notification and a client response take no slot.
	for _, b := range []string{`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":1,"progress":1}}`,
		`{"jsonrpc":"2.0","id":"upstream-1","result":{}}`} {
		if rep := c.do(http.MethodPost, svcURL, b, nil); rep.status != http.StatusAccepted {
			t.Fatalf("%s: %d %s", b, rep.status, rep.body)
		}
	}

	// One slot: a request goes through (the upstream's own -32601) and gives it back; an open GET stream holds it, so
	// that a request meanwhile is refused, and frees it when it closes.
	calls.set(1)
	if rep := c.do(http.MethodPost, svcURL, `{"jsonrpc":"2.0","id":30,"method":"resources/read","params":{"uri":"file:///x"}}`, nil); rep.status != http.StatusOK ||
		up.Count("resources/read") != 1 {
		t.Fatalf("resources/read with a slot: %d %s", rep.status, rep.body)
	}
	if inFlight, _, _ := calls.counts(); inFlight != 0 {
		t.Fatalf("%d slots in flight after the answer", inFlight)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, svcURL, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", c.sid)
	req.Header.Set("MCP-Protocol-Version", mcptest.Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("GET stream: %v %v", resp, err)
	}
	eventually(t, 5*time.Second, "the GET stream holds a slot", func() bool { n, _, _ := calls.counts(); return n == 1 })
	refused("prompts/get while the stream is open", c.do(http.MethodPost, svcURL, `{"jsonrpc":"2.0","id":31,"method":"prompts/get","params":{"name":"p"}}`, nil))
	cancel()
	resp.Body.Close()
	eventually(t, 5*time.Second, "the closed GET stream frees its slot", func() bool { n, _, _ := calls.counts(); return n == 0 })
	if rep := c.do(http.MethodPost, svcURL, `{"jsonrpc":"2.0","id":32,"method":"prompts/get","params":{"name":"p"}}`, nil); rep.status != http.StatusOK {
		t.Fatalf("prompts/get after the stream closed: %d %s", rep.status, rep.body)
	}
	if inFlight, taken, released := calls.counts(); inFlight != 0 || taken != released {
		t.Fatalf("slots: %d in flight, %d taken, %d released", inFlight, taken, released)
	}
}

// anonRefused writes §6.2's answers: a runtime cap 429 with -32600 and Retry-After, a caller's call over the storage
// limit -32602 "anonymous storage limit"; anything else is not its.
func TestAnonRefused(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   int
		msg    string
	}{
		{fmt.Errorf("x: %w", anon.ErrTooManyTasks), 429, -32600, "rate limited"},
		{anon.ErrTooManyCalls, 429, -32600, "rate limited"},
		{fmt.Errorf("%w (1 stored)", anon.ErrStorageFull), 200, -32602, "anonymous storage limit"},
	} {
		w := httptest.NewRecorder()
		if !anonRefused(w, []byte(`7`), c.err) {
			t.Fatalf("%v not answered", c.err)
		}
		rep := reply{status: w.Code, header: w.Header(), body: w.Body.Bytes()}
		code, _ := rpcError(t, rep)
		msg := rep.msg(t)["error"].(map[string]any)["message"]
		if rep.status != c.status || code != c.code || msg != c.msg || (c.status == 429) != (rep.header.Get("Retry-After") == "5") ||
			rep.msg(t)["id"] != float64(7) {
			t.Errorf("%v: %d %v %s", c.err, rep.status, rep.header, rep.body)
		}
	}
	if anonRefused(httptest.NewRecorder(), nil, errors.New("other")) {
		t.Error("another error answered")
	}
}

func scalarInt(t *testing.T, e *env, sql string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
