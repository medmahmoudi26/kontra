package sse

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/store"
)

var (
	ownerA = store.Owner{BuyerID: uuid.MustParse("0192f0b4-0000-7000-8000-00000000000a"), Principal: "key:kk_a_00000001", Via: store.ViaAPIKey}
	ownerB = store.Owner{BuyerID: uuid.MustParse("0192f0b4-0000-7000-8000-00000000000b"), Principal: "key:kk_b_00000001", Via: store.ViaAPIKey}
	svc    = store.ServiceRef{TenantID: uuid.MustParse("0192f0b4-0000-7000-8000-0000000000ee"), Tenant: "default", Name: "hello", Kind: "a2a"}
)

// memLog is an in-memory task log: rows in seq order, folded with the store's own Folder.
type memLog struct {
	mu      sync.Mutex
	taskID  uuid.UUID
	owner   store.Owner
	rows    []store.Event
	fail    error // EventsWithin fails with it while set
	snapErr error // SnapshotAt fails with it while set
	snaps   int   // SnapshotAt calls
	reads   int
	maxRead int // the most payload bytes one EventsWithin returned
	wakeups chan struct{}
}

func newMemLog(t *testing.T) *memLog {
	id := uuid.Must(uuid.NewV7())
	l := &memLog{taskID: id, owner: ownerA, wakeups: make(chan struct{}, 1)}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	msg.TaskID, msg.ContextID = a2a.TaskID(id.String()), "ctx-1"
	l.add(t, store.KindTask, &a2a.Task{ID: a2a.TaskID(id.String()), ContextID: "ctx-1",
		Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}, History: []*a2a.Message{msg}})
	return l
}

func (l *memLog) info() a2a.TaskInfo {
	return a2a.TaskInfo{TaskID: a2a.TaskID(l.taskID.String()), ContextID: "ctx-1"}
}

// add appends a row and wakes the stream.
func (l *memLog) add(t *testing.T, kind store.EventKind, ev any) int64 {
	t.Helper()
	var payload []byte
	var err error
	if kind == store.KindMetadata {
		payload, err = json.Marshal(map[string]any{"metadata": ev})
	} else {
		payload, err = json.Marshal(a2a.StreamResponse{Event: ev.(a2a.Event)})
	}
	if err != nil {
		t.Fatal(err)
	}
	return l.addRaw(kind, payload)
}

func (l *memLog) addRaw(kind store.EventKind, payload []byte) int64 {
	l.mu.Lock()
	seq := int64(len(l.rows) + 1)
	l.rows = append(l.rows, store.Event{TaskID: l.taskID, Seq: seq, Kind: kind, Payload: payload})
	l.mu.Unlock()
	select {
	case l.wakeups <- struct{}{}:
	default:
	}
	return seq
}

func (l *memLog) status(t *testing.T, st a2a.TaskState) int64 {
	return l.add(t, store.KindStatus, a2a.NewStatusUpdateEvent(l.info(), st, nil))
}

func (l *memLog) artifact(t *testing.T, text string) int64 {
	return l.add(t, store.KindArtifact, &a2a.TaskArtifactUpdateEvent{TaskID: l.info().TaskID, ContextID: "ctx-1",
		Artifact: &a2a.Artifact{ID: a2a.ArtifactID("a-" + text), Parts: a2a.ContentParts{a2a.NewTextPart(text)}}})
}

func (l *memLog) message(t *testing.T, text string) int64 {
	return l.add(t, store.KindMessage, a2a.NewMessageForTask(a2a.MessageRoleUser, l.info(), a2a.NewTextPart(text)))
}

func (l *memLog) fold(seq int64) (*a2a.Task, error) {
	var f store.Folder
	for _, r := range l.rows[:seq] {
		if err := f.Apply(r); err != nil {
			return nil, err
		}
	}
	return f.Snapshot()
}

func (l *memLog) LoadScoped(_ context.Context, s store.ServiceRef, o store.Owner, id uuid.UUID) (*store.TaskRow, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if id != l.taskID || o.BuyerID != l.owner.BuyerID || s.Name != svc.Name {
		return nil, a2a.NewError(a2a.ErrTaskNotFound, "task not found")
	}
	snap, err := l.fold(int64(len(l.rows)))
	if err != nil {
		return nil, err
	}
	st, err := store.StateFromA2A(snap.Status.State)
	if err != nil {
		return nil, err
	}
	return &store.TaskRow{ID: id, State: st, Version: int64(len(l.rows)), Snapshot: snap, Owner: l.owner}, nil
}

// EventsWithin is store.Log.EventsWithin over the rows: at most limit rows, ending with the one that
// brings their payload bytes to maxBytes, at least one.
func (l *memLog) EventsWithin(_ context.Context, id uuid.UUID, after int64, limit int, maxBytes int) ([]store.Event, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads++
	if l.fail != nil {
		return nil, false, l.fail
	}
	var (
		out  []store.Event
		read int
		more bool
	)
	for _, r := range l.rows {
		if r.Seq <= after {
			continue
		}
		if len(out) == limit || read >= maxBytes {
			more = true
			break
		}
		out = append(out, r)
		read += len(r.Payload)
	}
	l.maxRead = max(l.maxRead, read)
	return out, more, nil
}

func (l *memLog) SnapshotAt(_ context.Context, id uuid.UUID, seq int64) (*a2a.Task, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.snaps++
	if l.snapErr != nil {
		return nil, l.snapErr
	}
	if seq < 1 || seq > int64(len(l.rows)) {
		return nil, fmt.Errorf("not a seq: %w", a2a.ErrInvalidParams)
	}
	return l.fold(seq)
}

func (l *memLog) Tail(context.Context, uuid.UUID) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int64(len(l.rows)), nil
}

func (l *memLog) TerminalSeq(context.Context, uuid.UUID) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var ts int64
	for _, r := range l.rows {
		if state, _, err := rowData(r); err == nil && state.Terminal() {
			ts = r.Seq
		}
	}
	return ts, nil
}

// WaitTask implements store.TaskWaker over the log's own wake channel.
func (l *memLog) WaitTask(context.Context, uuid.UUID) (<-chan struct{}, func()) {
	return l.wakeups, func() {}
}

// event is one SSE event as a buyer reads it.
type event struct {
	id      int64 // 0: no id line
	data    string
	comment string
}

// readEvents parses an SSE body into events (comments are events with only comment set) until EOF.
func readEvents(body io.Reader, out chan<- event) {
	defer close(out)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var cur event
	has := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if has {
				out <- cur
			}
			cur, has = event{}, false
		case strings.HasPrefix(line, ":"):
			out <- event{comment: strings.TrimSpace(strings.TrimPrefix(line, ":"))}
		case strings.HasPrefix(line, "id: "):
			cur.id, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			has = true
		case strings.HasPrefix(line, "data: "):
			if cur.data != "" {
				cur.data += "\n" // a second data line: the framing test fails on it
			}
			cur.data += strings.TrimPrefix(line, "data: ")
			has = true
		}
	}
}

// streamRes is one stream as the buyer saw it.
type streamRes struct {
	status int
	header http.Header
	events chan event
	cancel func()
}

func (s *streamRes) next(t *testing.T) (event, bool) {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		return ev, ok
	case <-time.After(10 * time.Second):
		t.Fatal("no event within 10 s")
	}
	return event{}, false
}

// nextData skips keep-alives.
func (s *streamRes) nextData(t *testing.T) (event, bool) {
	t.Helper()
	for {
		ev, ok := s.next(t)
		if !ok || ev.comment == "" {
			return ev, ok
		}
	}
}

// rest drains the stream to its end (the server closed it).
func (s *streamRes) rest(t *testing.T) []event {
	t.Helper()
	var out []event
	for {
		ev, ok := s.nextData(t)
		if !ok {
			return out
		}
		out = append(out, ev)
	}
}

func serve(t *testing.T, h http.HandlerFunc, header http.Header) *streamRes {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	s := &streamRes{status: resp.StatusCode, header: resp.Header, events: make(chan event, 64), cancel: cancel}
	go func() {
		readEvents(resp.Body, s.events)
		resp.Body.Close()
	}()
	t.Cleanup(cancel)
	return s
}

func testWriter(l *memLog, o Options) *Writer {
	if o.KeepAlive == 0 {
		o.KeepAlive = time.Hour
	}
	if o.Reread == 0 {
		o.Reread = time.Hour // wakes only: a missed wake would hang the test
	}
	return newWriter(l, l, o)
}

// decode returns the StreamResponse of a data line of binding b, checking the envelope.
func decode(t *testing.T, b Binding, data string, wantID string) a2a.Event {
	t.Helper()
	if strings.Contains(data, "\n") {
		t.Fatalf("data is not one line: %q", data)
	}
	raw := []byte(data)
	if b == JSONRPC {
		var env struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(raw, &env); err != nil || env.JSONRPC != "2.0" || string(env.ID) != wantID {
			t.Fatalf("envelope %s: %v", data, err)
		}
		raw = env.Result
	}
	var sr a2a.StreamResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return sr.Event
}

func stateOf(ev a2a.Event) a2a.TaskState {
	switch v := ev.(type) {
	case *a2a.Task:
		return v.Status.State
	case *a2a.TaskStatusUpdateEvent:
		return v.Status.State
	}
	return ""
}

func eventIDs(evs []event) []int64 {
	out := make([]int64, len(evs))
	for i, e := range evs {
		out[i] = e.id
	}
	return out
}

func eqIDs(a []int64, b ...int64) bool {
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func TestParseLastEventID(t *testing.T) {
	for _, c := range []struct {
		vals []string
		k    int64
		ok   bool
		bad  bool
	}{
		{nil, 0, false, false},
		{[]string{"7"}, 7, true, false},
		{[]string{" 12 "}, 12, true, false},
		{[]string{"5", "5"}, 5, true, false},
		{[]string{"0"}, 0, false, true},
		{[]string{"-3"}, 0, false, true},
		{[]string{"+3"}, 0, false, true},
		{[]string{"3.0"}, 0, false, true},
		{[]string{"abc"}, 0, false, true},
		{[]string{""}, 0, false, true},
		{[]string{"5", "6"}, 0, false, true},
		{[]string{"99999999999999999999"}, 0, false, true},
		{[]string{"0192f0b4-0000-7000-8000-00000000000a"}, 0, false, true},
	} {
		h := http.Header{}
		for _, v := range c.vals {
			h.Add("Last-Event-ID", v)
		}
		k, ok, err := ParseLastEventID(h)
		if c.bad {
			if !errors.Is(err, a2a.ErrInvalidParams) {
				t.Errorf("%q: %v, want InvalidParams", c.vals, err)
			}
			continue
		}
		if err != nil || k != c.k || ok != c.ok {
			t.Errorf("%q: %d %v %v", c.vals, k, ok, err)
		}
	}
}

// SendStreamingMessage on a new task: the creation row first with id: 1, one single-line data per carried
// row with its seq as the id, message and metadata rows never sent, and the stream closes after the first
// row that ends the turn (spec §9). Both bindings.
func TestSendStreamingNewTask(t *testing.T) {
	for _, b := range []Binding{JSONRPC, HTTPJSON} {
		t.Run(fmt.Sprint(b), func(t *testing.T) {
			l := newMemLog(t)
			w := testWriter(l, Options{})
			start := func(context.Context) (Start, error) {
				return Start{TaskID: a2a.TaskID(l.taskID.String()), New: true}, nil
			}
			s := serve(t, func(rw http.ResponseWriter, r *http.Request) {
				w.SendStreaming(rw, r, Stream{Binding: b, RequestID: json.RawMessage(`42`), Service: svc, Owner: ownerA}, start)
			}, nil)
			if s.status != 200 || s.header.Get("Content-Type") != "text/event-stream" || s.header.Get("Cache-Control") != "no-cache" {
				t.Fatalf("%d %v", s.status, s.header)
			}
			first, _ := s.nextData(t)
			if first.id != 1 {
				t.Fatalf("first event id %d, want 1 (the creation row)", first.id)
			}
			if task, ok := decode(t, b, first.data, "42").(*a2a.Task); !ok || task.Status.State != a2a.TaskStateSubmitted {
				t.Fatalf("first event %s", first.data)
			}
			l.status(t, a2a.TaskStateWorking)                             // 2
			l.add(t, store.KindMetadata, map[string]any{"kontra.x": "y"}) // 3: not carried
			l.artifact(t, "chunk")                                        // 4
			l.message(t, "a caller's message")                            // 5: not carried
			l.status(t, a2a.TaskStateInputRequired)                       // 6: ends the turn
			l.status(t, a2a.TaskStateWorking)                             // 7: never sent
			got := s.rest(t)
			if !eqIDs(eventIDs(got), 2, 4, 6) {
				t.Fatalf("ids %v, want [2 4 6]", eventIDs(got))
			}
			if st := stateOf(decode(t, b, got[2].data, "42")); st != a2a.TaskStateInputRequired {
				t.Errorf("last event %s", st)
			}
			if _, ok := decode(t, b, got[1].data, "42").(*a2a.TaskArtifactUpdateEvent); !ok {
				t.Errorf("event 4 is not the artifact: %s", got[1].data)
			}
		})
	}
}

// A follow-up's stream leads with the snapshot at V without an id (it never closes the stream), then rows
// with seq > V, closing after the first one that ends the turn.
func TestSendStreamingFollowUp(t *testing.T) {
	l := newMemLog(t)
	l.status(t, a2a.TaskStateWorking)            // 2
	v := l.status(t, a2a.TaskStateInputRequired) // 3 = V
	w := testWriter(l, Options{})
	start := func(context.Context) (Start, error) {
		return Start{TaskID: a2a.TaskID(l.taskID.String()), After: v, FollowUp: true}, nil
	}
	s := serve(t, func(rw http.ResponseWriter, r *http.Request) {
		w.SendStreaming(rw, r, Stream{Binding: HTTPJSON, Service: svc, Owner: ownerA}, start)
	}, nil)
	lead, _ := s.nextData(t)
	task, ok := decode(t, HTTPJSON, lead.data, "").(*a2a.Task)
	if lead.id != 0 || !ok || task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("lead %+v", lead)
	}
	l.message(t, "blue")                // 4
	l.status(t, a2a.TaskStateWorking)   // 5
	l.artifact(t, "done")               // 6
	l.status(t, a2a.TaskStateCompleted) // 7
	l.status(t, a2a.TaskStateWorking)   // never sent (the store would refuse it anyway)
	if got := s.rest(t); !eqIDs(eventIDs(got), 5, 6, 7) {
		t.Fatalf("ids %v", eventIDs(got))
	}
}

// A resend answered with the task it repeats: a leading snapshot that already ends a turn closes the
// stream; one that does not is followed by the rows after it.
func TestSendStreamingResend(t *testing.T) {
	l := newMemLog(t)
	v := l.status(t, a2a.TaskStateCompleted)
	w := testWriter(l, Options{})
	s := serve(t, func(rw http.ResponseWriter, r *http.Request) {
		w.SendStreaming(rw, r, Stream{Binding: JSONRPC, RequestID: json.RawMessage(`"r"`), Service: svc, Owner: ownerA}, func(context.Context) (Start, error) {
			return Start{TaskID: a2a.TaskID(l.taskID.String()), After: v}, nil
		})
	}, nil)
	got := s.rest(t)
	if len(got) != 1 || got[0].id != 0 || stateOf(decode(t, JSONRPC, got[0].data, `"r"`)) != a2a.TaskStateCompleted {
		t.Fatalf("resend of a completed task: %+v", got)
	}
}

// An interceptor's own answer (an accepted review-window message) is the stream's one event.
func TestSendStreamingAnswer(t *testing.T) {
	l := newMemLog(t)
	w := testWriter(l, Options{})
	answer := &a2a.Task{ID: "t", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}
	s := serve(t, func(rw http.ResponseWriter, r *http.Request) {
		w.SendStreaming(rw, r, Stream{Binding: HTTPJSON}, func(context.Context) (Start, error) { return Start{Answer: answer}, nil })
	}, nil)
	got := s.rest(t)
	if len(got) != 1 || got[0].id != 0 || decode(t, HTTPJSON, got[0].data, "").(*a2a.Task).ID != "t" {
		t.Fatalf("%+v", got)
	}
}

// An error before the first event is an ordinary error, before any SSE header: JSON-RPC at HTTP 200 with
// the request's id, HTTP+JSON with its status, 503 for an Update not accepted in time.
func TestSendStreamingErrorBeforeStream(t *testing.T) {
	l := newMemLog(t)
	w := testWriter(l, Options{})
	for _, c := range []struct {
		b      Binding
		err    error
		status int
		code   int
	}{
		{JSONRPC, a2a.NewError(a2a.ErrUnsupportedOperation, "running"), 200, -32004},
		{HTTPJSON, a2a.NewError(a2a.ErrUnsupportedOperation, "running"), 400, 0},
		{HTTPJSON, a2a.NewError(a2a.ErrTaskNotFound, "no"), 404, 0},
		{JSONRPC, fmt.Errorf("x: %w: %w", a2a.ErrInternalError, lifecycleUnavailable()), 200, -32603},
		{HTTPJSON, fmt.Errorf("x: %w: %w", a2a.ErrInternalError, lifecycleUnavailable()), 503, 0},
	} {
		rec := httptest.NewRecorder()
		w.SendStreaming(rec, httptest.NewRequest("POST", "/", nil), Stream{Binding: c.b, RequestID: json.RawMessage(`9`)},
			func(context.Context) (Start, error) { return Start{}, c.err })
		if rec.Code != c.status || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Header().Get("Content-Type"))
		}
		if c.b == JSONRPC {
			var resp struct {
				ID    json.RawMessage `json:"id"`
				Error RPCError        `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Error.Code != c.code || string(resp.ID) != "9" {
				t.Errorf("%v: %s", c.err, rec.Body)
			}
		}
	}
}

// SubscribeToTask without Last-Event-ID: the current snapshot without an id, then the rows after its
// version; the stream stays open through input-required and auth-required and closes after the terminal
// row. A terminal task is UnsupportedOperationError before any header; another buyer's task does not exist.
func TestSubscribe(t *testing.T) {
	l := newMemLog(t)
	l.status(t, a2a.TaskStateWorking)                                // 2
	l.add(t, store.KindMetadata, map[string]any{"kontra.note": "n"}) // 3
	w := testWriter(l, Options{})
	s := serve(t, func(rw http.ResponseWriter, r *http.Request) {
		w.Subscribe(rw, r, Stream{Binding: JSONRPC, RequestID: json.RawMessage(`1`), Service: svc, Owner: ownerA, TaskID: a2a.TaskID(l.taskID.String())})
	}, nil)
	lead, _ := s.nextData(t)
	task := decode(t, JSONRPC, lead.data, "1").(*a2a.Task)
	if lead.id != 0 || task.Status.State != a2a.TaskStateWorking || task.Metadata["kontra.note"] != "n" {
		t.Fatalf("lead %s", lead.data)
	}
	l.status(t, a2a.TaskStateInputRequired) // 4: stays open
	l.status(t, a2a.TaskStateAuthRequired)  // 5: stays open
	l.artifact(t, "x")                      // 6
	l.status(t, a2a.TaskStateCompleted)     // 7: closes
	if got := s.rest(t); !eqIDs(eventIDs(got), 4, 5, 6, 7) {
		t.Fatalf("ids %v", eventIDs(got))
	}
	// Terminal now: refused before any SSE header.
	rec := httptest.NewRecorder()
	w.Subscribe(rec, httptest.NewRequest("GET", "/", nil), Stream{Binding: HTTPJSON, Service: svc, Owner: ownerA, TaskID: a2a.TaskID(l.taskID.String())})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "UNSUPPORTED_OPERATION") || rec.Header().Get("Content-Type") == "text/event-stream" {
		t.Errorf("terminal: %d %s", rec.Code, rec.Body)
	}
	for _, c := range []struct {
		o  store.Owner
		id a2a.TaskID
	}{{ownerB, a2a.TaskID(l.taskID.String())}, {ownerA, "not-a-uuid"}, {ownerA, a2a.TaskID(uuid.Must(uuid.NewV7()).String())}} {
		rec = httptest.NewRecorder()
		w.Subscribe(rec, httptest.NewRequest("GET", "/", nil), Stream{Binding: JSONRPC, RequestID: json.RawMessage(`3`), Service: svc, Owner: c.o, TaskID: c.id})
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"code":-32001`) {
			t.Errorf("%v %s: %d %s", c.o.Principal, c.id, rec.Code, rec.Body)
		}
	}
}

// Resume with Last-Event-ID: K: the snapshot as of K without an id, then every carried row after K once, in
// order; on a terminal task with K below the terminal seq the stream ends after the terminal row; K at or
// past it is UnsupportedOperationError; K not a seq of the task is InvalidParams.
func TestResume(t *testing.T) {
	l := newMemLog(t)
	l.status(t, a2a.TaskStateWorking)                             // 2
	l.artifact(t, "one")                                          // 3
	l.add(t, store.KindMetadata, map[string]any{"kontra.k": "v"}) // 4
	l.artifact(t, "two")                                          // 5
	l.message(t, "follow")                                        // 6
	w := testWriter(l, Options{})
	sub := func(k int64, b Binding) *streamRes {
		return serve(t, func(rw http.ResponseWriter, r *http.Request) {
			w.Subscribe(rw, r, Stream{Binding: b, RequestID: json.RawMessage(`5`), Service: svc, Owner: ownerA, TaskID: a2a.TaskID(l.taskID.String()), LastEventID: &k})
		}, nil)
	}
	s := sub(3, HTTPJSON)
	lead, _ := s.nextData(t)
	want, _ := l.SnapshotAt(context.Background(), l.taskID, 3)
	gotTask := decode(t, HTTPJSON, lead.data, "").(*a2a.Task)
	wb, _ := json.Marshal(want)
	gb, _ := json.Marshal(gotTask)
	if lead.id != 0 || string(wb) != string(gb) {
		t.Fatalf("lead\n got  %s\n want %s", gb, wb)
	}
	if len(gotTask.Artifacts) != 1 || gotTask.Metadata["kontra.k"] != nil {
		t.Errorf("the snapshot as of 3 holds one artifact and not the metadata of row 4: %s", gb)
	}
	l.status(t, a2a.TaskStateCompleted) // 7
	if got := s.rest(t); !eqIDs(eventIDs(got), 5, 7) {
		t.Fatalf("ids after 3: %v", eventIDs(got))
	}
	// A message row after the terminal row (an accepted review message) is a seq of the task.
	l.message(t, "review") // 8
	// K = terminal - 1: the snapshot as of 6, then only the terminal row.
	s = sub(6, JSONRPC)
	got := s.rest(t)
	if len(got) != 2 || got[0].id != 0 || got[1].id != 7 {
		t.Fatalf("resume at terminal-1: %+v", got)
	}
	if hist := decode(t, JSONRPC, got[0].data, "5").(*a2a.Task).History; hist[len(hist)-1].Parts[0].Text() != "follow" {
		t.Errorf("the snapshot as of 6 ends its history with the message of row 6")
	}
	for _, c := range []struct {
		k    int64
		want string
	}{{7, "UNSUPPORTED_OPERATION"}, {8, "UNSUPPORTED_OPERATION"}, {9, "INVALID_PARAMS"}, {1000, "INVALID_PARAMS"}} {
		k := c.k
		rec := httptest.NewRecorder()
		w.Subscribe(rec, httptest.NewRequest("GET", "/", nil), Stream{Binding: HTTPJSON, Service: svc, Owner: ownerA, TaskID: a2a.TaskID(l.taskID.String()), LastEventID: &k})
		if !strings.Contains(rec.Body.String(), c.want) || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("K=%d: %d %s", c.k, rec.Code, rec.Body)
		}
	}
	// Another buyer's task with a valid seq: TaskNotFound.
	k := int64(2)
	rec := httptest.NewRecorder()
	w.Subscribe(rec, httptest.NewRequest("GET", "/", nil), Stream{Binding: HTTPJSON, Service: svc, Owner: ownerB, TaskID: a2a.TaskID(l.taskID.String()), LastEventID: &k})
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "TASK_NOT_FOUND") {
		t.Errorf("other buyer: %d %s", rec.Code, rec.Body)
	}
}

// `: keep-alive` every KeepAlive while nothing happens; the stream still carries the rows after them.
func TestKeepAlive(t *testing.T) {
	l := newMemLog(t)
	w := testWriter(l, Options{KeepAlive: 30 * time.Millisecond})
	s := serve(t, func(rw http.ResponseWriter, r *http.Request) {
		w.Subscribe(rw, r, Stream{Binding: HTTPJSON, Service: svc, Owner: ownerA, TaskID: a2a.TaskID(l.taskID.String())})
	}, nil)
	if lead, _ := s.next(t); lead.id != 0 || lead.data == "" {
		t.Fatalf("lead %+v", lead)
	}
	for i := 0; i < 3; i++ {
		if ev, _ := s.next(t); ev.comment != "keep-alive" {
			t.Fatalf("event %d: %+v, want a keep-alive", i, ev)
		}
	}
	l.status(t, a2a.TaskStateFailed)
	if ev, _ := s.nextData(t); ev.id != 2 {
		t.Fatalf("after keep-alives: %+v", ev)
	}
	if _, ok := s.nextData(t); ok {
		t.Error("the stream stayed open after the terminal row")
	}
}

// Without a wake, the stream re-reads the log every Reread.
func TestRereadWithoutWake(t *testing.T) {
	l := newMemLog(t)
	w := newWriter(l, nil, Options{KeepAlive: time.Hour, Reread: 20 * time.Millisecond})
	s := serve(t, func(rw http.ResponseWriter, r *http.Request) {
		w.Subscribe(rw, r, Stream{Binding: HTTPJSON, Service: svc, Owner: ownerA, TaskID: a2a.TaskID(l.taskID.String())})
	}, nil)
	s.nextData(t)
	l.status(t, a2a.TaskStateCanceled)
	if got := s.rest(t); !eqIDs(eventIDs(got), 2) {
		t.Fatalf("ids %v", eventIDs(got))
	}
}

// A log that cannot be read for ReadGrace ends the stream with an error event; a shorter failure is
// retried and the stream goes on without a gap.
func TestReadFailures(t *testing.T) {
	l := newMemLog(t)
	w := testWriter(l, Options{ReadGrace: 300 * time.Millisecond})
	s := serve(t, func(rw http.ResponseWriter, r *http.Request) {
		w.Subscribe(rw, r, Stream{Binding: JSONRPC, RequestID: json.RawMessage(`1`), Service: svc, Owner: ownerA, TaskID: a2a.TaskID(l.taskID.String())})
	}, nil)
	s.nextData(t)
	l.mu.Lock()
	l.fail = errors.New("connection refused")
	l.mu.Unlock()
	l.status(t, a2a.TaskStateWorking) // 2, read once the log is back
	time.Sleep(100 * time.Millisecond)
	l.mu.Lock()
	l.fail = nil
	l.mu.Unlock()
	if ev, _ := s.nextData(t); ev.id != 2 {
		t.Fatalf("after a short failure: %+v", ev)
	}
	l.mu.Lock()
	l.fail = errors.New("connection refused")
	l.mu.Unlock()
	l.status(t, a2a.TaskStateWorking)
	ev, _ := s.nextData(t)
	var env struct {
		Error RPCError `json:"error"`
	}
	if ev.id != 0 || json.Unmarshal([]byte(ev.data), &env) != nil || env.Error.Code != -32603 || strings.Contains(env.Error.Message, "connection refused") {
		t.Fatalf("after ReadGrace: %+v", ev)
	}
	if _, ok := s.nextData(t); ok {
		t.Error("the stream stayed open after its error event")
	}
}

// The caller going away ends the stream's goroutine.
func TestDisconnectEndsStream(t *testing.T) {
	l := newMemLog(t)
	w := testWriter(l, Options{})
	done := make(chan struct{})
	s := serve(t, func(rw http.ResponseWriter, r *http.Request) {
		defer close(done)
		w.Subscribe(rw, r, Stream{Binding: HTTPJSON, Service: svc, Owner: ownerA, TaskID: a2a.TaskID(l.taskID.String())})
	}, nil)
	s.nextData(t)
	s.cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end when its caller went away")
	}
}

// Every data line is one line, whatever the stored bytes look like (json is stored verbatim, D49), and a
// NUL in a text part reaches the caller.
func TestRowDataOneLine(t *testing.T) {
	l := newMemLog(t)
	pretty := "{\n  \"statusUpdate\": {\n    \"taskId\": \"" + l.taskID.String() + "\",\n    \"contextId\": \"ctx-1\",\n    \"status\": {\"state\": \"TASK_STATE_WORKING\",\n \"message\": {\"messageId\": \"m\", \"role\": \"ROLE_AGENT\", \"parts\": [{\"text\": \"a\\u0000b\\nc\"}]}}\n  }\n}"
	l.addRaw(store.KindStatus, []byte(pretty))
	state, data, err := rowData(l.rows[1])
	if err != nil || state != a2a.TaskStateWorking || strings.ContainsAny(string(data), "\n\r") {
		t.Fatalf("%s %q %v", state, data, err)
	}
	var sr a2a.StreamResponse
	if err := json.Unmarshal(data, &sr); err != nil {
		t.Fatal(err)
	}
	if txt := sr.Event.(*a2a.TaskStatusUpdateEvent).Status.Message.Parts[0].Text(); txt != "a\x00b\nc" {
		t.Errorf("text %q", txt)
	}
	if _, _, err := rowData(store.Event{Kind: store.KindStatus, Payload: []byte(`{"artifactUpdate":{}}`)}); err == nil {
		t.Error("a status row without its event was accepted")
	}
}

// stalledClient sends a request to addr and never reads the answer (a zero TCP window once the buffers
// fill).
func stalledClient(t *testing.T, addr string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.(*net.TCPConn).SetReadBuffer(4096)
	if _, err := io.WriteString(c, "POST / HTTP/1.1\r\nHost: gate\r\nContent-Length: 0\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
}

// smallBuffers is a listener whose connections send through a small kernel buffer, so a peer that stops
// reading blocks the server's writes after a few kilobytes rather than megabytes.
type smallBuffers struct{ net.Listener }

func (l smallBuffers) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4096)
	}
	return c, err
}

// A caller that keeps its connection open but stops reading ends its stream after the write timeout: the
// handler returns, releasing its connection and in-flight slots and the rows it holds, although the task
// is still running (security: a stalled reader held them until the drain's server Close).
func TestStalledReaderEndsStream(t *testing.T) {
	l := newMemLog(t)
	for i := range 4 {
		l.artifact(t, strings.Repeat(string(rune('a'+i)), 512<<10))
	}
	w := testWriter(l, Options{WriteTimeout: 300 * time.Millisecond})
	done := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		defer close(done)
		w.Subscribe(rw, r, Stream{Binding: HTTPJSON, Service: svc, Owner: ownerA, TaskID: a2a.TaskID(l.taskID.String())})
	}))
	srv.Listener = smallBuffers{srv.Listener}
	srv.Start()
	t.Cleanup(srv.Close)
	stalledClient(t, srv.Listener.Addr().String())
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the stream of a caller that stopped reading never ended")
	}
}

// A stream that is read is not cut by the write timeout, however long it waits between events.
func TestWriteTimeoutDoesNotCutAnIdleStream(t *testing.T) {
	l := newMemLog(t)
	w := testWriter(l, Options{WriteTimeout: 100 * time.Millisecond, KeepAlive: time.Hour})
	s := serve(t, func(rw http.ResponseWriter, r *http.Request) {
		w.Subscribe(rw, r, Stream{Binding: HTTPJSON, Service: svc, Owner: ownerA, TaskID: a2a.TaskID(l.taskID.String())})
	}, nil)
	s.nextData(t)
	time.Sleep(500 * time.Millisecond) // five write timeouts without a write
	l.status(t, a2a.TaskStateCompleted)
	if got := s.rest(t); len(got) != 1 {
		t.Fatalf("after an idle wait: %d events", len(got))
	}
}
