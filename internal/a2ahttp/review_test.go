package a2ahttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.temporal.io/sdk/client"

	"github.com/medmahmoudi26/kontra/internal/edge"
	"github.com/medmahmoudi26/kontra/internal/lifecycle"
)

// temporalDown is the error the lifecycle client returns for a follow-up's Update when Temporal is
// unreachable, as a2a-go's work queue call wraps it, and the address the client dialed.
func temporalDown(t *testing.T) (error, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	c, err := client.NewLazyClient(client.Options{HostPort: addr, Namespace: "kontra"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = lifecycle.NewClient(c).Message(context.Background(), "0192f0b4-0000-7000-8000-000000000001",
		lifecycle.MessageUpdate{WorkItemID: "0192f0b4-0000-7000-8000-000000000002"})
	if !errors.Is(err, lifecycle.ErrUnavailable) || !strings.Contains(err.Error(), addr) {
		t.Fatalf("the lifecycle client with Temporal down: %v", err)
	}
	return fmt.Errorf("failed to create work item: %w", err), addr
}

// An internal error's cause never reaches the caller, on any route: a follow-up whose Update Temporal never
// received is 503 (HTTP+JSON) or -32603 (JSON-RPC) without the dial target, through a2a-go's transports
// and the gate's SSE routes alike; a database error is -32603 / 500 without its text. A caller's own
// error keeps its words.
func TestInternalCausesNotSent(t *testing.T) {
	down, addr := temporalDown(t)
	db := errors.New("store: load task: read tcp 10.0.0.5:5432: connection reset by peer")
	f := newFixture(t, false)
	follow := `{"message":{"messageId":"m2","role":"ROLE_USER","taskId":"0192f0b4-0000-7000-8000-000000000001","parts":[{"text":"more"}]}}`
	for _, c := range []struct {
		err    error
		r      req
		status int
		want   string
	}{
		{down, req{"POST", "/agents/hello/message:send", follow, v1("alice")}, 503, `"the task's lifecycle is unavailable"`},
		{down, req{"POST", "/agents/hello/message:stream", follow, v1("alice")}, 503, `"the task's lifecycle is unavailable"`},
		{down, req{"POST", "/agents/hello", rpc("SendMessage", follow, 1), v1("alice")}, 200, `"code":-32603,"message":"the task's lifecycle is unavailable"`},
		{down, req{"POST", "/agents/hello", rpc("SendStreamingMessage", follow, 2), v1("alice")}, 200, `"code":-32603,"message":"the task's lifecycle is unavailable"`},
		{down, req{"POST", "/agents/hello/tasks/0192f0b4-0000-7000-8000-000000000001:cancel", "", v1("alice")}, 503, `"the task's lifecycle is unavailable"`},
		{db, req{"GET", "/agents/hello/tasks/0192f0b4-0000-7000-8000-000000000001", "", v1("alice")}, 500, `"message":"internal error"`},
		{db, req{"POST", "/agents/hello", rpc("GetTask", `{"id":"0192f0b4-0000-7000-8000-000000000001"}`, 3), v1("alice")}, 200, `"code":-32603,"message":"internal error"`},
		{db, req{"POST", "/agents/hello", rpc("ListTasks", `{}`, 4), v1("alice")}, 200, `"code":-32603,"message":"internal error"`},
		{a2a.NewError(a2a.ErrTaskNotFound, "no task 0192 here"), req{"GET", "/agents/hello/tasks/x", "", v1("alice")}, 404, `"no task 0192 here"`},
		{a2a.NewError(a2a.ErrUnsupportedOperation, "a turn is running"), req{"POST", "/agents/hello", rpc("SendMessage", follow, 5), v1("alice")}, 200, `"code":-32004,"message":"a turn is running"`},
	} {
		f.h.err = c.err
		w := f.do(c.r)
		body := w.Body.String()
		if w.Code != c.status || !strings.Contains(body, c.want) {
			t.Errorf("%s %s: %d %s, want %d with %s", c.r.method, c.r.path, w.Code, body, c.status, c.want)
		}
		for _, leak := range []string{addr, "dial tcp", "10.0.0.5", "reset by peer", "failed to create work item"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s %s: the body holds %q: %s", c.r.method, c.r.path, leak, body)
			}
		}
	}
}

// The edge alone bounds a body, by limits.body: with it raised above 4 MiB, a body of 5 MiB reaches the
// service on JSON-RPC, on message:stream and on message:send alike, and one over the limit is the edge's
// 413 on all three.
func TestBodyLimitIsTheEdges(t *testing.T) {
	f := newFixture(t, false)
	rt := edge.NewRouter()
	f.srv.Register(rt)
	h := edge.Middleware(edge.MiddlewareConfig{Limits: edge.Limits{Body: 8 << 20}}, rt)
	parts := func(n int) string {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"text":"` + strings.Repeat("x", 200<<10) + `"}`)
		}
		return `{"message":{"messageId":"m-big","role":"ROLE_USER","parts":[` + b.String() + `]}}`
	}
	for _, c := range []struct {
		n    int
		want int
	}{{25, 200}, {45, 413}} { // about 5 MiB, then about 9 MiB
		msg := parts(c.n)
		for _, r := range []req{
			{"POST", "/agents/hello", rpc("SendMessage", msg, 1), v1("alice")},
			{"POST", "/agents/hello/", rpc("SendStreamingMessage", msg, 2), v1("alice")},
			{"POST", "/agents/hello/message:stream", msg, v1("alice")},
			{"POST", "/agents/hello/message:send", msg, v1("alice")},
		} {
			f.h.err = nil
			_, before := f.h.last()
			hr := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
			hr.Header = r.header
			w := httptest.NewRecorder()
			h.ServeHTTP(w, hr)
			_, after := f.h.last()
			reached := after > before
			if c.want == 200 && (w.Code != 200 || !reached) {
				t.Errorf("%d KiB on %s: %d, reached the service %v: %.200s", len(r.body)>>10, r.path, w.Code, reached, w.Body)
			}
			if c.want == 413 && (w.Code != 413 || reached) {
				t.Errorf("%d KiB on %s: %d, reached the service %v", len(r.body)>>10, r.path, w.Code, reached)
			}
			if c.want == 413 && strings.HasPrefix(r.path, "/agents/hello") && !strings.Contains(r.path, ":") &&
				!bytes.Contains(w.Body.Bytes(), []byte(`"code":-32600`)) {
				t.Errorf("JSON-RPC 413 without a JSON-RPC error: %s", w.Body)
			}
		}
	}
}
