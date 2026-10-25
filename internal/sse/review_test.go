package sse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/medmahmoudi26/kontra/internal/lifecycle"
)

// dbBlip is what a2a-go reports when its read of the task fails after Write: its own wrapping of a
// Postgres error, an internal address included.
var dbBlip = errors.New("task snapshot loading failed: store: load task: read tcp 10.0.0.5:5432: connection reset by peer")

// unreachable is the shape of lifecycle's error for an Update Temporal never received: the gate's own
// words, then the SDK's text with the dial target.
var unreachable = fmt.Errorf("%w: %w: %s", a2a.NewError(a2a.ErrInternalError, "the task's lifecycle is unavailable"),
	lifecycle.ErrUnavailable, `failed reaching server: connection error: desc = "transport: Error while dialing: dial tcp 10.9.8.7:7233: connect: connection refused"`)

// writeThenFail is a2a-go's SendStreamingMessage as the gate's handler runs it: Write fills StreamStart
// when fill is set (the turn was accepted), then a2a-go yields err.
type writeThenFail struct {
	erring
	fill  *lifecycle.StreamStart
	err   error
	slots chan *lifecycle.StreamStart
}

func (h writeThenFail) SendStreamingMessage(ctx context.Context, _ *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	ss := <-h.slots
	if h.fill != nil {
		*ss = *h.fill
	}
	return func(yield func(a2a.Event, error) bool) { yield(nil, h.err) }
}

// starter is handlerStarter over h, with the StreamStart handed to h as Write receives it.
func starter(h writeThenFail, req *a2a.SendMessageRequest) Starter {
	return handlerStarter(h, req, a2asrv.NewServiceParams(nil), func(ctx context.Context, ss *lifecycle.StreamStart) context.Context {
		h.slots <- ss
		return lifecycle.WithStreamStart(ctx, ss)
	})
}

// logBuffer is a logger whose records a test reads.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A follow-up that Write accepted gets its stream even when a2a-go fails afterwards (its reads after
// Write: the reader created again after the id override, the subscription's snapshot): the error is
// logged, not answered, since the turn is running and a retry would be refused. A request whose Write did
// not accept the turn (StreamStart empty) is answered with its error before any SSE header, and an
// internal error's cause is not in that answer.
func TestHandlerStarterServesAnAcceptedTurn(t *testing.T) {
	l := newMemLog(t)
	l.status(t, a2a.TaskStateWorking)            // 2
	v := l.status(t, a2a.TaskStateInputRequired) // 3 = V
	logs := &logBuffer{}
	w := testWriter(l, Options{Logger: slog.New(slog.NewTextHandler(logs, nil))})
	req := &a2a.SendMessageRequest{Message: a2a.NewMessageForTask(a2a.MessageRoleUser, l.info(), a2a.NewTextPart("blue"))}

	h := writeThenFail{fill: &lifecycle.StreamStart{TaskID: a2a.TaskID(l.taskID.String()), Version: v}, err: dbBlip, slots: make(chan *lifecycle.StreamStart, 1)}
	for _, b := range []Binding{JSONRPC, HTTPJSON} {
		t.Run(fmt.Sprint(b), func(t *testing.T) {
			s := serve(t, func(rw http.ResponseWriter, r *http.Request) {
				w.SendStreaming(rw, r, Stream{Binding: b, RequestID: json.RawMessage(`7`), Service: svc, Owner: ownerA}, starter(h, req))
			}, nil)
			if s.status != 200 || s.header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("an accepted follow-up whose a2a-go read failed: %d %s", s.status, s.header.Get("Content-Type"))
			}
			lead, _ := s.nextData(t)
			if task, ok := decode(t, b, lead.data, "7").(*a2a.Task); lead.id != 0 || !ok || task.Status.State != a2a.TaskStateInputRequired {
				t.Fatalf("lead %+v", lead)
			}
			if b == JSONRPC { // the turn's rows; the second binding's stream reads them from the log
				l.status(t, a2a.TaskStateWorking)   // 4
				l.status(t, a2a.TaskStateCompleted) // 5
			}
			if got := s.rest(t); !eqIDs(eventIDs(got), 4, 5) {
				t.Fatalf("ids %v", eventIDs(got))
			}
		})
	}
	if !strings.Contains(logs.String(), "10.0.0.5:5432") {
		t.Errorf("the late error was not logged: %s", logs)
	}

	// No turn accepted: the error is the answer, written before any SSE header, without its cause.
	for _, c := range []struct {
		err    error
		b      Binding
		status int
		code   int
		msg    string
	}{
		{dbBlip, JSONRPC, 200, -32603, "internal error"},
		{dbBlip, HTTPJSON, 500, 0, "internal error"},
		{unreachable, JSONRPC, 200, -32603, "the task's lifecycle is unavailable"},
		{unreachable, HTTPJSON, 503, 0, "the task's lifecycle is unavailable"},
		{a2a.NewError(a2a.ErrUnsupportedOperation, "a turn is running"), JSONRPC, 200, -32004, "a turn is running"},
		{a2a.NewError(a2a.ErrUnsupportedOperation, "a turn is running"), HTTPJSON, 400, 0, "a turn is running"},
	} {
		h := writeThenFail{err: c.err, slots: make(chan *lifecycle.StreamStart, 1)}
		rec := httptest.NewRecorder()
		w.SendStreaming(rec, httptest.NewRequest("POST", "/", nil), Stream{Binding: c.b, RequestID: json.RawMessage(`8`)}, starter(h, req))
		body := rec.Body.String()
		if rec.Code != c.status || rec.Header().Get("Content-Type") != "application/json" || !strings.Contains(body, c.msg) {
			t.Errorf("%v on %v: %d %s", c.err, c.b, rec.Code, body)
		}
		if strings.Contains(body, "10.0.0.5") || strings.Contains(body, "10.9.8.7") || strings.Contains(body, "dial tcp") {
			t.Errorf("%v on %v: the cause reached the caller: %s", c.err, c.b, body)
		}
		if c.code != 0 {
			var resp struct {
				Error RPCError `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Error.Code != c.code {
				t.Errorf("%v: %s", c.err, body)
			}
		}
	}
}

// Public keeps a caller's error as it is and an internal error's own words only; the code, the reason,
// the details and the 503 of an Update not accepted in time do not change.
func TestPublic(t *testing.T) {
	for _, c := range []struct {
		err      error
		msg      string
		redacted bool
	}{
		{dbBlip, "internal error", true},
		{unreachable, "the task's lifecycle is unavailable", true},
		{fmt.Errorf("failed to create work item: %w", unreachable), "the task's lifecycle is unavailable", true},
		{fmt.Errorf("x: %w: dial tcp 10.0.0.1:1", a2a.ErrServerError), "server error", true},
		{fmt.Errorf("lost: %w", context.DeadlineExceeded), "context deadline exceeded", true},
		{a2a.NewError(a2a.ErrInternalError, "admission unavailable"), "admission unavailable", false},
		{a2a.ErrInternalError, "internal error", false},
		{fmt.Errorf("store: task 0192 of buyer b: %w", a2a.ErrTaskNotFound), "store: task 0192 of buyer b: task not found", false},
		{a2a.NewError(a2a.ErrUnsupportedOperation, "a turn is running: refused"), "a turn is running: refused", false},
	} {
		pub, redacted := Public(c.err)
		if pub.Error() != c.msg || redacted != c.redacted {
			t.Errorf("%q: %q %v, want %q %v", c.err, pub, redacted, c.msg, c.redacted)
		}
		if a, b := ToJSONRPC(c.err), ToJSONRPC(pub); a.Code != b.Code || !sameData(t, a, b) {
			t.Errorf("%q: JSON-RPC %+v, public %+v", c.err, a, b)
		}
		if a, _ := ToHTTP(c.err); a != func() int { s, _ := ToHTTP(pub); return s }() {
			t.Errorf("%q: HTTP status changed", c.err)
		}
	}
	if pub, _ := Public(unreachable); !Unavailable(pub) || !errors.Is(pub, a2a.ErrInternalError) {
		t.Error("the public error lost its sentinels")
	}
	withDetails := fmt.Errorf("wrapped: %w", a2a.NewError(a2a.ErrInternalError, "own words").WithDetails(map[string]any{"k": "v"}))
	if a, b := ToJSONRPC(withDetails), ToJSONRPC(public(withDetails)); !sameData(t, a, b) || b.Message != "own words" {
		t.Errorf("details: %+v, public %+v", a, b)
	}
	status, body := HTTPError(unreachable)
	if status != 503 || bytes.Contains(body, []byte("10.9.8.7")) || !bytes.Contains(body, []byte("the task's lifecycle is unavailable")) {
		t.Errorf("HTTPError: %d %s", status, body)
	}
}

// sameData reports whether two JSON-RPC errors carry the same details (the ErrorInfo timestamp aside).
func sameData(t *testing.T, a, b RPCError) bool {
	t.Helper()
	ja, err := json.Marshal(a.Data)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := json.Marshal(b.Data)
	if err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(normalize(t, ja), normalize(t, jb))
}

// discard is a ResponseWriter that only counts what is written.
type discard struct {
	h http.Header
	n int
}

func (d *discard) Header() http.Header {
	if d.h == nil {
		d.h = http.Header{}
	}
	return d.h
}
func (d *discard) Write(b []byte) (int, error) { d.n += len(b); return len(b), nil }
func (d *discard) WriteHeader(int)             {}
func (d *discard) Flush()                      {}

// A carried row is sent as it was stored: checking it and framing it copy nothing of its size (spec §9
// rows are up to one upstream event, 4 MiB; a task's parts up to limits.task_bytes).
func TestRowSentWithoutCopies(t *testing.T) {
	l := newMemLog(t)
	big := strings.Repeat("x", 4<<20)
	l.artifact(t, big)
	ev := l.rows[1]
	_, data, err := rowData(ev)
	if err != nil || &data[0] != &ev.Payload[0] {
		t.Fatalf("rowData copied a one-line payload (%v)", err)
	}
	for _, b := range []Binding{JSONRPC, HTTPJSON} {
		d := &discard{}
		sw := &streamWriter{rw: d, rc: http.NewResponseController(d), binding: b, id: json.RawMessage(`1`)}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for i := 0; i < 4; i++ {
			_, data, err := rowData(ev)
			if err != nil {
				t.Fatal(err)
			}
			if err := sw.event(ev.Seq, data); err != nil {
				t.Fatal(err)
			}
		}
		runtime.ReadMemStats(&after)
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 1<<20 {
			t.Errorf("%v: checking and writing a 4 MiB row four times allocated %d bytes", b, alloc)
		}
		if d.n < 4*len(ev.Payload) {
			t.Errorf("%v: wrote %d bytes", b, d.n)
		}
	}
}

// A stream reads the log a budget of bytes at a time, not a number of rows: however many large rows it
// is behind, one read holds at most readBytes plus one row.
func TestReadsBoundedByBytes(t *testing.T) {
	l := newMemLog(t)
	row := strings.Repeat("y", 1500<<10)
	for i := range 12 {
		l.artifact(t, fmt.Sprintf("%d-%s", i, row)) // 2..13
	}
	l.status(t, a2a.TaskStateCompleted) // 14
	w := testWriter(l, Options{})
	k := int64(1)
	s := serve(t, func(rw http.ResponseWriter, r *http.Request) {
		w.Subscribe(rw, r, Stream{Binding: HTTPJSON, Service: svc, Owner: ownerA, TaskID: a2a.TaskID(l.taskID.String()), LastEventID: &k})
	}, nil)
	got := s.rest(t)
	if len(got) != 14 || got[0].id != 0 || got[13].id != 14 {
		t.Fatalf("%d events, last %d", len(got), got[len(got)-1].id)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if one := len(l.rows[2].Payload); l.maxRead > readBytes+one {
		t.Errorf("one read held %d bytes, budget %d plus one row of %d", l.maxRead, readBytes, one)
	}
}
