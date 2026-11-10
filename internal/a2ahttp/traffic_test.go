package a2ahttp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/sse"
	"kontra.sh/kontra/internal/traffic"
)

// rowsDB collects what the recorder COPYs, as a2a_exchanges would hold it.
type rowsDB struct {
	mu   sync.Mutex
	rows [][]any
}

func (d *rowsDB) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, src pgx.CopyFromSource) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for src.Next() {
		v, err := src.Values()
		if err != nil {
			return 0, err
		}
		d.rows = append(d.rows, v)
	}
	return int64(len(d.rows)), nil
}

// recorded runs the fixture's server with a recorder for the requests do makes, and returns the rows written, as
// column name -> value.
func recorded(t *testing.T, f *fixture, sseImpl Streams, do func()) []map[string]any {
	t.Helper()
	db := &rowsDB{}
	rec := traffic.New(db, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = rec.Run(ctx); close(done) }()
	if sseImpl == nil {
		sseImpl = f.st
	}
	f.srv = New(Deps{Services: f.reg, Handlers: handlers{f.h}, SSE: sseImpl, Traffic: rec,
		Auth: authn{"alice": alice, "bob": bob, "enroller": enroller}})
	do()
	cancel()
	<-done
	cols := []string{"id", "at", "tenant_id", "service", "method", "binding", "principal", "via", "task_id", "context_id",
		"bytes_in", "bytes_out", "duration_ms", "http_status", "rpc_code", "reason"}
	var out []map[string]any
	for _, r := range db.rows {
		m := map[string]any{}
		for i, c := range cols {
			m[c] = r[i]
		}
		out = append(out, m)
	}
	return out
}

// One exchange per call: SendMessage and two GetTask naming the task, a card fetch, a call without A2A-Version
// (-32009) and one the allow list denies, each with its method, outcome and reason; no row holds a body or header.
func TestOneExchangePerCall(t *testing.T) {
	f := newFixture(t, false)
	const secret = "my secret"
	tid := task().ID
	send := `{"message":{"messageId":"m1","role":"ROLE_USER","contextId":"ctx-1","parts":[{"text":"` + secret + `"}]}}`
	rows := recorded(t, f, nil, func() {
		f.do(req{"POST", "/agents/hello", rpc("SendMessage", send, 1), v1("alice", "X-Secret", secret)})
		for range 2 {
			f.do(req{"POST", "/agents/hello", rpc("GetTask", `{"id":"`+string(tid)+`"}`, 2), v1("alice")})
		}
		f.do(req{"GET", "/agents/open/.well-known/agent-card.json", "", nil})
		f.do(req{"POST", "/agents/hello", rpc("SendMessage", send, 3), hdr("Authorization", "Bearer alice")})
		f.do(req{"POST", "/agents/hello", rpc("SendMessage", send, 4), v1("bob")})
		f.do(req{"POST", "/agents/nope/message:send", send, v1("alice")})
	})
	type want struct {
		method, principal, reason string
		status                    int16
		code                      any
		task                      bool
	}
	wants := []want{
		{"SendMessage", "key:kk_alice_00000001", "", 200, nil, true},
		{"GetTask", "key:kk_alice_00000001", "", 200, nil, true},
		{"GetTask", "key:kk_alice_00000001", "", 200, nil, true},
		{"card", "anonymous", "", 200, nil, false},
		{"SendMessage", "key:kk_alice_00000001", "version_unsupported", 200, -32009, false},
		{"SendMessage", "key:kk_bob_00000001", "allow_denied", 404, -32601, false},
		{"SendMessage", "key:kk_alice_00000001", "unknown_service", 404, nil, false},
	}
	if len(rows) != len(wants) {
		t.Fatalf("%d exchanges, want %d: %v", len(rows), len(wants), rows)
	}
	for i, w := range wants {
		r := rows[i]
		if r["method"] != w.method || r["http_status"] != w.status || fmt.Sprint(r["rpc_code"]) != fmt.Sprint(w.code) ||
			fmt.Sprint(r["reason"]) != fmt.Sprint(map[bool]any{true: nil, false: w.reason}[w.reason == ""]) {
			t.Errorf("exchange %d: %v, want %+v", i, r, w)
		}
		if p := fmt.Sprint(r["principal"]); p != w.principal && !(w.principal == "anonymous" && strings.HasPrefix(p, "anonymous")) {
			t.Errorf("exchange %d: principal %v, want %s", i, r["principal"], w.principal)
		}
		if w.task && fmt.Sprint(r["task_id"]) != string(tid) {
			t.Errorf("exchange %d: task %v, want %s", i, r["task_id"], tid)
		}
		for c, v := range r {
			if s := fmt.Sprint(v); strings.Contains(s, secret) || strings.Contains(s, "Bearer") || strings.Contains(s, "alice\"") {
				t.Errorf("exchange %d: column %s holds request content %q", i, c, s)
			}
		}
	}
	if rows[0]["context_id"] != "ctx-1" || rows[0]["service"] != "hello" || rows[0]["binding"] != "jsonrpc" {
		t.Errorf("SendMessage exchange: %v", rows[0])
	}
	if rows[6]["service"] != "nope" || rows[6]["binding"] != "rest" {
		t.Errorf("unknown service exchange: %v", rows[6])
	}
}

// slowStreams holds a subscription open for hold before it ends.
type slowStreams struct {
	*streams
	hold time.Duration
}

func (s slowStreams) Subscribe(w http.ResponseWriter, r *http.Request, st sse.Stream) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(`data: {"jsonrpc":"2.0","id":1,"result":{"task":{"id":"0192f0b4-0000-7000-8000-000000000001","contextId":"c"}}}` + "\n\n"))
	http.NewResponseController(w).Flush()
	time.Sleep(s.hold)
}

// A stream is one exchange, recorded when it ends, with its whole duration and the task its first event names.
func TestStreamRecordedOnceWhenItEnds(t *testing.T) {
	f := newFixture(t, false)
	rows := recorded(t, f, slowStreams{f.st, 2 * time.Second}, func() {
		f.do(req{"POST", "/agents/hello", rpc("SubscribeToTask", `{"id":"0192f0b4-0000-7000-8000-000000000001"}`, 1), v1("alice")})
	})
	if len(rows) != 1 {
		t.Fatalf("%d exchanges, want 1", len(rows))
	}
	r := rows[0]
	if r["method"] != "SubscribeToTask" || r["duration_ms"].(int32) < 2000 || fmt.Sprint(r["task_id"]) != "0192f0b4-0000-7000-8000-000000000001" {
		t.Fatalf("stream exchange %v", r)
	}
}

// The edge's refusals on A2A paths are recorded by RecordRefusal; /mcp and other paths are not exchanges.
func TestRecordRefusal(t *testing.T) {
	f := newFixture(t, false)
	rows := recorded(t, f, nil, func() {
		big := strings.NewReader("x")
		r := httptestRequest("POST", "/agents/hello", big)
		r.ContentLength = 5 << 20
		f.srv.RecordRefusal(r, http.StatusRequestEntityTooLarge, "body_too_large")
		f.srv.RecordRefusal(httptestRequest("POST", "/mcp/tools", big), http.StatusRequestEntityTooLarge, "body_too_large")
		f.srv.RecordRefusal(httptestRequest("GET", "/v1/tunnel", nil), http.StatusTooManyRequests, "rate_limited")
	})
	if len(rows) != 1 || rows[0]["method"] != "unknown" || rows[0]["http_status"] != int16(413) || rows[0]["reason"] != "body_too_large" ||
		rows[0]["service"] != "hello" || rows[0]["bytes_in"] != int64(5<<20) || fmt.Sprint(rows[0]["rpc_code"]) != "-32600" {
		t.Fatalf("refusals recorded: %v", rows)
	}
}

func httptestRequest(method, path string, body *strings.Reader) *http.Request {
	if body == nil {
		return httptest.NewRequest(method, path, nil)
	}
	return httptest.NewRequest(method, path, body)
}

// Through the edge: an oversized body to /agents/hello is one exchange (413, body_too_large, method unknown), the
// same to /mcp none.
func TestEdgeRefusalThroughTheChain(t *testing.T) {
	f := newFixture(t, false)
	rows := recorded(t, f, nil, func() {
		routes := edge.NewRouter()
		f.srv.Register(routes)
		routes.Handle("POST /mcp/{name}", http.NotFoundHandler())
		h := edge.Middleware(edge.MiddlewareConfig{OnRefuse: f.srv.RecordRefusal}, routes)
		over := strings.Repeat("a", 4<<20+1)
		for _, p := range []string{"/agents/hello", "/mcp/tools"} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", p, strings.NewReader(over)))
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("%s: %d", p, w.Code)
			}
		}
	})
	if len(rows) != 1 || rows[0]["service"] != "hello" || rows[0]["http_status"] != int16(413) || rows[0]["method"] != "unknown" {
		t.Fatalf("recorded %v", rows)
	}
}
