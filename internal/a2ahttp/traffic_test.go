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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/sse"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/traffic"
)

// rowsDB collects what the recorder COPYs, as a2a_exchanges and a2a_exchange_frames would hold it.
type rowsDB struct {
	mu     sync.Mutex
	rows   [][]any
	frames [][]any
}

func (d *rowsDB) CopyFrom(_ context.Context, table pgx.Identifier, _ []string, src pgx.CopyFromSource) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for src.Next() {
		v, err := src.Values()
		if err != nil {
			return 0, err
		}
		if table[len(table)-1] == "a2a_exchange_frames" {
			d.frames = append(d.frames, v)
		} else {
			d.rows = append(d.rows, v)
		}
		n++
	}
	return int64(n), nil
}

// exchangeColumns are a2a_exchanges' columns in the recorder's COPY order.
var exchangeColumns = []string{"id", "at", "tenant_id", "service", "method", "binding", "principal", "via", "task_id", "context_id",
	"bytes_in", "bytes_out", "duration_ms", "http_status", "rpc_code", "reason",
	"leg", "gate_task_id", "callee_task_id", "a2a_version", "request_body", "request_size", "request_truncated",
	"response_body", "response_size", "response_truncated", "frames_dropped"}

// recorded runs the fixture's server with a recorder for the requests do makes, and returns the rows written, as
// column name -> value.
func recorded(t *testing.T, f *fixture, sseImpl Streams, do func()) []map[string]any {
	t.Helper()
	rows, _ := recordedWith(t, f, sseImpl, 0, do)
	return rows
}

// recordedWith is recorded with bodies kept up to bodyMax (0: off); it also returns the frames written, each as
// exchange_id, n, offset_ms, sse_id, data, size, truncated.
func recordedWith(t *testing.T, f *fixture, sseImpl Streams, bodyMax int, do func()) ([]map[string]any, [][]any) {
	t.Helper()
	db := &rowsDB{}
	rec := traffic.New(db, nil).KeepBodies(bodyMax)
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
	var out []map[string]any
	for _, r := range db.rows {
		m := map[string]any{}
		for i, c := range exchangeColumns {
			m[c] = r[i]
		}
		out = append(out, m)
	}
	return out, db.frames
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

// D356: the exchange's id is minted when the call arrives; the handler sees it in its context, and the stored row has
// it. Every caller-leg row says leg caller and the A2A version the call claimed.
func TestExchangeIDMintedOnArrival(t *testing.T) {
	f := newFixture(t, false)
	send := `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}`
	rows := recorded(t, f, nil, func() {
		f.do(req{"POST", "/agents/hello", rpc("SendMessage", send, 1), v1("alice")})
	})
	c, n := f.h.last()
	if n != 1 || c.exchange == uuid.Nil {
		t.Fatalf("the handler saw exchange %v (%d calls)", c.exchange, n)
	}
	if len(rows) != 1 || rows[0]["id"] != c.exchange {
		t.Fatalf("stored id %v, the handler saw %v", rows[0]["id"], c.exchange)
	}
	if rows[0]["leg"] != "caller" || rows[0]["gate_task_id"] != nil || rows[0]["a2a_version"] != "1.0" {
		t.Errorf("leg %v, gate task %v, version %v", rows[0]["leg"], rows[0]["gate_task_id"], rows[0]["a2a_version"])
	}
}

// With bodies on, a plain call keeps its request and answer as sent (credentials redacted, never a header); a cut
// answer is marked truncated with its full size; with bodies off nothing is kept.
func TestBodiesKept(t *testing.T) {
	f := newFixture(t, false)
	send := `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hello"}]}}`
	rows, _ := recordedWith(t, f, nil, 1<<20, func() {
		f.do(req{"POST", "/agents/hello", rpc("SendMessage", send, 1), v1("alice", "X-Secret", "hdr-secret")})
	})
	if len(rows) != 1 {
		t.Fatalf("%d exchanges", len(rows))
	}
	r := rows[0]
	reqBody, _ := r["request_body"].([]byte)
	respBody, _ := r["response_body"].([]byte)
	if !strings.Contains(string(reqBody), `"text":"hello"`) || r["request_truncated"] != false || r["request_size"] != int64(len(reqBody)) {
		t.Errorf("request body %q size %v truncated %v", reqBody, r["request_size"], r["request_truncated"])
	}
	if !strings.Contains(string(respBody), `"jsonrpc":"2.0"`) || r["response_size"] != r["bytes_out"] {
		t.Errorf("response body %q size %v (bytes_out %v)", respBody, r["response_size"], r["bytes_out"])
	}
	for c, v := range r {
		if s := fmt.Sprint(v); strings.Contains(s, "hdr-secret") || strings.Contains(s, "Bearer") {
			t.Errorf("column %s holds a header: %q", c, s)
		}
	}

	// Cut at a small body_max: truncated, with the full size.
	rows, _ = recordedWith(t, f, nil, 1024, func() {
		long := `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"` + strings.Repeat("x", 4000) + `"}]}}`
		f.do(req{"POST", "/agents/hello", rpc("SendMessage", long, 1), v1("alice")})
	})
	if b, _ := rows[0]["request_body"].([]byte); len(b) != 1024 || rows[0]["request_truncated"] != true || rows[0]["request_size"].(int64) < 4000 {
		t.Errorf("cut request: %d bytes, truncated %v, size %v", len(b), rows[0]["request_truncated"], rows[0]["request_size"])
	}

	// Bodies off: nothing kept.
	rows = recorded(t, f, nil, func() {
		f.do(req{"POST", "/agents/hello", rpc("SendMessage", send, 1), v1("alice")})
	})
	if rows[0]["request_body"] != nil || rows[0]["response_body"] != nil {
		t.Errorf("bodies off kept %v / %v", rows[0]["request_body"], rows[0]["response_body"])
	}
}

// A sealed service's calls keep no bodies, even with bodies on.
func TestSealedServiceKeepsNoBodies(t *testing.T) {
	f := newFixture(t, false)
	sealed := *f.reg["default/hello"]
	sealed.Ref.Name, sealed.Ref.Sealed = "vault", "optional"
	f.reg["default/vault"] = &sealed
	send := `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hello"}]}}`
	rows, _ := recordedWith(t, f, nil, 1<<20, func() {
		f.do(req{"POST", "/agents/vault", rpc("SendMessage", send, 1), v1("alice")})
	})
	if len(rows) != 1 || rows[0]["request_body"] != nil || rows[0]["response_body"] != nil {
		t.Fatalf("sealed service kept bodies: %v", rows)
	}
}

// A push config's token and credentials are never stored, in the request or the answer.
func TestPushConfigRedacted(t *testing.T) {
	f := newFixture(t, false)
	tid := string(task().ID)
	cfg := `{"taskId":"` + tid + `","url":"https://hook.example/x","token":"t0k","authentication":{"scheme":"Bearer","credentials":"s3cret"}}`
	send := `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]},"configuration":{"taskPushNotificationConfig":{"url":"https://hook.example/y","token":"t0k","authentication":{"scheme":"Bearer","credentials":"s3cret"}}}}`
	rows, _ := recordedWith(t, f, nil, 1<<20, func() {
		f.do(req{"POST", "/agents/hello", rpc("CreateTaskPushNotificationConfig", cfg, 1), v1("alice")})
		f.do(req{"POST", "/agents/hello/tasks/" + tid + "/pushNotificationConfigs", cfg, v1("alice")})
		f.do(req{"POST", "/agents/hello", rpc("SendMessage", send, 2), v1("alice")})
	})
	if len(rows) != 3 {
		t.Fatalf("%d exchanges", len(rows))
	}
	for i, r := range rows {
		for c, v := range r {
			if s := fmt.Sprintf("%s", v); strings.Contains(s, "t0k") || strings.Contains(s, "s3cret") {
				t.Errorf("exchange %d: column %s holds a credential: %q", i, c, s)
			}
		}
		if b, _ := r["request_body"].([]byte); !strings.Contains(string(b), traffic.Redacted) || !strings.Contains(string(b), "hook.example") {
			t.Errorf("exchange %d: request body %q, want the url kept and the credentials redacted", i, b)
		}
	}
}

// framesStreams answers a subscription with a snapshot, a keep-alive and two events.
type framesStreams struct{ *streams }

func (s framesStreams) Subscribe(w http.ResponseWriter, r *http.Request, st sse.Stream) {
	w.Header().Set("Content-Type", "text/event-stream")
	ev := func(id, data string) {
		if id != "" {
			_, _ = w.Write([]byte("id: " + id + "\n"))
		}
		_, _ = w.Write([]byte("data: " + data + "\n\n"))
		http.NewResponseController(w).Flush()
	}
	ev("", `{"jsonrpc":"2.0","id":1,"result":{"task":{"id":"0192f0b4-0000-7000-8000-000000000001","contextId":"c"}}}`)
	_, _ = w.Write([]byte(": keep-alive\n\n"))
	ev("2", `{"jsonrpc":"2.0","id":1,"result":{"statusUpdate":{"taskId":"0192f0b4-0000-7000-8000-000000000001","contextId":"c","status":{"state":"TASK_STATE_WORKING"}}}}`)
	ev("3", `{"jsonrpc":"2.0","id":1,"result":{"artifactUpdate":{"taskId":"0192f0b4-0000-7000-8000-000000000001","contextId":"c","artifact":{"artifactId":"a","parts":[{"text":"`+strings.Repeat("y", 3000)+`"}]}}}}`)
}

// A stream keeps its frames in order, each with its SSE id and offset, keep-alives left out, a long one cut; no
// response body.
func TestStreamFramesKept(t *testing.T) {
	f := newFixture(t, false)
	rows, frames := recordedWith(t, f, framesStreams{f.st}, 1024, func() {
		f.do(req{"POST", "/agents/hello", rpc("SubscribeToTask", `{"id":"0192f0b4-0000-7000-8000-000000000001"}`, 1), v1("alice")})
	})
	if len(rows) != 1 || rows[0]["response_body"] != nil {
		t.Fatalf("stream exchange %v", rows)
	}
	if len(frames) != 3 {
		t.Fatalf("%d frames, want 3: %v", len(frames), frames)
	}
	for i, fr := range frames {
		if fr[0] != rows[0]["id"] || fr[1] != int32(i) {
			t.Errorf("frame %d: exchange %v n %v", i, fr[0], fr[1])
		}
	}
	if frames[0][3] != nil || frames[1][3] != "2" || frames[2][3] != "3" {
		t.Errorf("sse ids %v %v %v", frames[0][3], frames[1][3], frames[2][3])
	}
	if !strings.Contains(string(frames[1][4].([]byte)), "TASK_STATE_WORKING") || frames[1][6] != false {
		t.Errorf("frame 1: %s", frames[1][4])
	}
	if d := frames[2][4].([]byte); len(d) != 1024 || frames[2][6] != true || frames[2][5].(int64) < 3000 {
		t.Errorf("frame 2: %d bytes, truncated %v, size %v", len(d), frames[2][6], frames[2][5])
	}
}

// D357: the call's method and exchange reach the handler as the caller's origin, recorded or not.
func TestRequestOrigin(t *testing.T) {
	f := newFixture(t, false)
	send := `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}`
	recorded(t, f, nil, func() {
		f.do(req{"POST", "/agents/hello", rpc("SendMessage", send, 1), v1("alice")})
	})
	c, _ := f.h.last()
	if c.origin == nil || c.origin.Actor != store.ActorCaller || c.origin.Type != "SendMessage" || c.origin.Method != "SendMessage" ||
		c.origin.ExchangeID != c.exchange.String() {
		t.Errorf("recorded: origin %+v, exchange %v", c.origin, c.exchange)
	}
	// Unrecorded: the method still, no exchange.
	f = newFixture(t, false)
	f.do(req{"POST", "/agents/hello/message:send", send, v1("alice")})
	c, _ = f.h.last()
	if c.origin == nil || c.origin.Type != "SendMessage" || c.origin.ExchangeID != "" {
		t.Errorf("unrecorded: origin %+v", c.origin)
	}
}
