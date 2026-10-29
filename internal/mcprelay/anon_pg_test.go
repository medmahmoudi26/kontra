package mcprelay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

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
// anything is forwarded or metered, in both eras. Other requests (tools/list) take no slot.
func TestPlainCallsAreCapped(t *testing.T) {
	e := newEnv(t)
	up, url := dualEra(t, mcptest.Tool("echo", ""))
	e.service("svc", url, nil)
	calls := &fakeCalls{}
	r := e.replica("a", replicaOpts{withDeps: func(_ *replica, deps *Deps, _ *pg.Listener, _ *slog.Logger) { deps.Calls = calls }})
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	c.initialize(r.url + "/mcp/svc")
	modern := newModern(t, key, r.url+"/mcp/svc", false)

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
	if inFlight, taken, released := calls.counts(); inFlight != 0 || taken != 2 || released != 2 {
		t.Fatalf("slots: %d in flight, %d taken, %d released", inFlight, taken, released)
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
