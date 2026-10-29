// Package sse is the gate's own SSE writer for SendStreamingMessage and SubscribeToTask on both bindings,
// fed from task_events and never from the upstream connection (spec/relay.md §9, D2). a2a-go's writer
// emits random event ids and never reads Last-Event-ID, which is why the gate cannot use it here.
//
// Framing: one event per carried row (task, status, artifact), `id: <seq>` then one `data:` line (the
// JSON-RPC envelope on JSON-RPC, the bare StreamResponse on HTTP+JSON); a leading snapshot has no id line;
// `: keep-alive` every 15 s. Ids are seqs: strictly increasing, not contiguous. Errors that can be known
// before the stream (unknown task, terminal task without resume, version, extension, a Last-Event-ID that
// is not a seq of this task) are ordinary JSON-RPC or HTTP errors written before any SSE header.
//
// Resume (`Last-Event-ID: K`, either binding): the snapshot as of K, folded from rows 1..K, as a Task
// event without an id, then every carried row with seq > K exactly once in order, then the live tail. On
// a terminal task with K below its terminal seq the stream ends after the terminal row; with K at or past
// it, UnsupportedOperationError.
package sse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/store"
)

// Binding is the transport a stream is framed for.
type Binding int

// Bindings.
const (
	JSONRPC  Binding = iota // data: {"jsonrpc":"2.0","id":…,"result":<StreamResponse>}
	HTTPJSON                // data: <StreamResponse>
)

// Stream timing defaults.
const (
	// DefaultReread is how often a stream re-reads the log when no wake arrived: the LISTEN connection's
	// notification is a hint, the table is the truth (spec §5.2), and a wake can be lost while the
	// listener reconnects.
	DefaultReread = 5 * time.Second
	// ReadGrace is how long a stream keeps retrying a log read that fails before it ends with an error
	// event; the caller resumes with Last-Event-ID.
	ReadGrace = 30 * time.Second
	// batch is the most rows one read returns (the puller's store.PullBatch).
	batch = 64
	// readBytes bounds the payload bytes one read holds: it ends with the row that reaches it, so a stream
	// holds at most this plus one row (an upstream event is at most 4 MiB, spec §6.6), however far behind
	// the log it starts. A resume from an early seq, or 32 streams of one key (inflight_per_key), no longer
	// hold a task's whole log (limits.task_bytes, 256 MiB) at once.
	readBytes = 4 << 20
)

// logReader is what the writer reads; *store.Log implements it.
type logReader interface {
	LoadScoped(ctx context.Context, svc store.ServiceRef, o store.Owner, taskID uuid.UUID) (*store.TaskRow, error)
	EventsWithin(ctx context.Context, taskID uuid.UUID, after int64, limit int, maxBytes int) ([]store.Event, bool, error)
	SnapshotAt(ctx context.Context, taskID uuid.UUID, seq int64) (*a2a.Task, error)
	Tail(ctx context.Context, taskID uuid.UUID) (int64, error)
	TerminalSeq(ctx context.Context, taskID uuid.UUID) (int64, error)
}

// Options tune a Writer; zero values take the defaults.
type Options struct {
	KeepAlive time.Duration // the `: keep-alive` interval: edge.KeepAlive (15 s, spec §2.2)
	Reread    time.Duration // DefaultReread
	ReadGrace time.Duration // ReadGrace
	// WriteTimeout bounds each write of the stream (an event, a keep-alive): WriteTimeout when zero.
	WriteTimeout time.Duration
	Logger       *slog.Logger
	// Render renders the kontra-blob: parts of every event and snapshot the stream sends as presigned GETs
	// (spec §10.4, D167); a row that names no ref is sent as stored, without being decoded. Nil renders nothing.
	Render *blob.Renderer
}

// WriteTimeout is how long one write of a stream may block: a caller that keeps its connection open but
// stops reading (a zero TCP window) ends its stream after it, releasing the handler, its connection's
// conns_per_ip and max_conns slot, its key's in-flight slot and the rows being sent. The public listener
// has no WriteTimeout of its own (edge.go: a stream lives as long as its task), so the deadline is set per
// write and cleared after it; a long-lived stream that is read is never cut.
const WriteTimeout = 30 * time.Second

func (o Options) withDefaults() Options {
	if o.KeepAlive <= 0 {
		o.KeepAlive = edge.KeepAlive
	}
	if o.Reread <= 0 {
		o.Reread = DefaultReread
	}
	if o.ReadGrace <= 0 {
		o.ReadGrace = ReadGrace
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = WriteTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Writer serves streams from the log.
type Writer struct {
	log   logReader
	waker store.TaskWaker
	opts  Options
}

// NewWriter returns a writer over the log, woken by the replica's LISTEN connection.
func NewWriter(log *store.Log, waker store.TaskWaker) *Writer {
	return NewWriterWithOptions(log, waker, Options{})
}

// NewWriterWithOptions is NewWriter with its timing and logger set.
func NewWriterWithOptions(log *store.Log, waker store.TaskWaker, o Options) *Writer {
	return newWriter(log, waker, o)
}

func newWriter(log logReader, waker store.TaskWaker, o Options) *Writer {
	return &Writer{log: log, waker: waker, opts: o.withDefaults()}
}

// Stream is one stream request, after authentication and routing.
type Stream struct {
	Binding     Binding
	RequestID   json.RawMessage // the JSON-RPC id, echoed in every envelope
	Service     store.ServiceRef
	Owner       store.Owner
	TaskID      a2a.TaskID // SubscribeToTask: the task the request names
	LastEventID *int64     // set when the request carried Last-Event-ID
}

// Start is where a SendStreamingMessage stream starts: what lifecycle.StreamStart reported, filled by the
// work queue's Write in the transaction that created the task or stored the follow-up's work item, never
// read afterwards (a fast dispatch could otherwise put the turn's end inside the leading snapshot, and the
// stream would never close).
type Start struct {
	TaskID a2a.TaskID
	// After is the version the stream starts after: 0 for a new task (the creation row, seq 1, is the
	// first event), V for a follow-up or a resend (the snapshot at V leads, without an id).
	After int64
	New   bool
	// FollowUp is a request that named its task (spec §9 "follow-up"): its leading snapshot never closes
	// the stream. Without it (a resend answered with the task it repeats) a leading snapshot whose state
	// ends a turn closes it, since nothing more of that turn will come.
	FollowUp bool
	// Answer is a task an interceptor answered the request with itself, before the work queue (an accepted
	// review-window message, spec §7): the stream is that one event.
	Answer *a2a.Task
	// Late is an error a2a-go reported after Write accepted the turn (one of its reads of the task's log or
	// snapshot failed). The turn is running and the stream position is known, so the stream is served and
	// Late is only logged: answering it would make an accepted follow-up look failed, and the caller's
	// retry would be refused because the turn is running.
	Late error
}

// Starter creates or continues a task through a2a-go's handler for SendStreamingMessage and reports where
// the stream starts. An error is the request's answer, written before any SSE header.
type Starter func(ctx context.Context) (Start, error)

// HandlerStarter is the Starter of a service's a2a-go handler (the relay's interceptors and work queue
// run, and the call context names SendStreamingMessage): it calls SendStreamingMessage with a call
// context built from params (the request's headers, A2A-Version normalized by the version middleware) and
// a lifecycle.StreamStart for Write to fill, takes the first event (the subscription's snapshot) and stops
// iterating, which closes a2a-go's reader. The stream position is StreamStart's, never the snapshot's.
//
// Write fills StreamStart only when it accepted the turn, and the writer needs nothing from a2a-go after
// that. So a filled StreamStart is served even when a2a-go then fails (its reads after Write: the reader
// it creates again after the task id override, the subscription's snapshot): that error becomes
// Start.Late. Only an error with StreamStart empty (no turn accepted) is the request's answer.
func HandlerStarter(h a2asrv.RequestHandler, req *a2a.SendMessageRequest, params *a2asrv.ServiceParams) Starter {
	return handlerStarter(h, req, params, lifecycle.WithStreamStart)
}

// handlerStarter is HandlerStarter with the way StreamStart is attached to the context given, so a test's
// handler can fill it as Write does.
func handlerStarter(h a2asrv.RequestHandler, req *a2a.SendMessageRequest, params *a2asrv.ServiceParams,
	attach func(context.Context, *lifecycle.StreamStart) context.Context) Starter {
	return func(ctx context.Context) (Start, error) {
		var ss lifecycle.StreamStart
		ctx = attach(ctx, &ss)
		ctx, cc := a2asrv.NewCallContext(ctx, params)
		cc.ProtocolVersion = a2a.Version
		var (
			first a2a.Event
			ferr  error
			got   bool
		)
		for ev, err := range h.SendStreamingMessage(ctx, req) {
			first, ferr, got = ev, err, true
			break
		}
		switch {
		case ss.TaskID != "":
			followUp := req != nil && req.Message != nil && req.Message.TaskID != ""
			return Start{TaskID: ss.TaskID, After: ss.Version, New: ss.New, FollowUp: followUp, Late: ferr}, nil
		case ferr != nil:
			return Start{}, ferr
		case got:
			if t, ok := first.(*a2a.Task); ok && t != nil {
				return Start{Answer: t}, nil
			}
		}
		return Start{}, a2a.NewError(a2a.ErrInternalError, "the request produced no task")
	}
}

// plan is what a stream sends: an optional leading snapshot without an id, then every carried row after
// `after`, until a row whose state `closes` says ends the stream.
type plan struct {
	task       uuid.UUID
	lead       *a2a.Task
	leadCloses bool
	// leadAt, when lead is nil, is the seq whose snapshot leads the stream, read once the stream has begun
	// (SendStreamingMessage continuing a task): the turn was accepted, so that read is retried like every
	// read of the log instead of becoming the request's answer. followUp is Start.FollowUp: a follow-up's
	// leading snapshot never closes the stream.
	leadAt   int64
	followUp bool
	after    int64
	closes   func(a2a.TaskState) bool
}

// endsTurn is SendStreamingMessage's rule: terminal, input-required, or quoted (which A2A shows as
// input-required, spec §5.1).
func endsTurn(s a2a.TaskState) bool { return s.Terminal() || s == a2a.TaskStateInputRequired }

// terminal is SubscribeToTask's rule: open through input-required and auth-required (TCK STREAM-SUB-002).
func terminal(s a2a.TaskState) bool { return s.Terminal() }

// SendStreaming serves SendStreamingMessage. It closes after the first row whose state is terminal,
// input-required or quoted (for a follow-up: the first such row with seq > V).
func (w *Writer) SendStreaming(rw http.ResponseWriter, r *http.Request, s Stream, start Starter) {
	ctx := r.Context()
	st, err := start(ctx)
	if err != nil {
		w.writeError(rw, s, err)
		return
	}
	if st.Answer != nil {
		sw := w.begin(rw, r, s)
		_ = sw.event(0, mustStreamResponse(st.Answer))
		return
	}
	id, err := ids.ParseTaskID(string(st.TaskID))
	if err != nil {
		WriteError(rw, s.Binding, s.RequestID, 0, a2a.NewError(a2a.ErrInternalError, "the work queue named no task"))
		return
	}
	if st.Late != nil {
		w.opts.Logger.Warn("sse: a2a-go failed after the turn was accepted; serving the turn's stream from the log",
			"task", id, "after", st.After, "error", st.Late)
	}
	// The turn was accepted, so from here on the answer is its stream: the leading snapshot at V is read once
	// the stream has begun and retried within ReadGrace like every read of the log. Answering a read that
	// failed would make a running turn look refused (the same database blip that made a2a-go fail, Late,
	// often fails this read too), and the caller's retry would be refused because the turn is running.
	p := plan{task: id, after: st.After, closes: endsTurn, followUp: st.FollowUp}
	if !st.New {
		p.leadAt = st.After
	}
	w.serve(rw, r, s, p)
}

// Subscribe serves SubscribeToTask, with or without Last-Event-ID. Without it a terminal task is
// UnsupportedOperationError (TCK STREAM-SUB-003); otherwise the current snapshot without an id, then the
// carried rows after its version, open through input-required and auth-required until the terminal row
// (TCK STREAM-SUB-002).
func (w *Writer) Subscribe(rw http.ResponseWriter, r *http.Request, s Stream) {
	p, err := w.subscription(r.Context(), s)
	if err != nil {
		w.writeError(rw, s, err)
		return
	}
	w.serve(rw, r, s, p)
}

// writeError answers err before any SSE header (WriteError), logging the cause of an internal error whose
// text the caller does not get (Public).
func (w *Writer) writeError(rw http.ResponseWriter, s Stream, err error) {
	if _, redacted := Public(err); redacted {
		w.opts.Logger.Warn("sse: the stream could not start", "error", err)
	}
	WriteError(rw, s.Binding, s.RequestID, 0, err)
}

func (w *Writer) subscription(ctx context.Context, s Stream) (plan, error) {
	id, err := ids.ParseTaskID(string(s.TaskID))
	if err != nil {
		return plan{}, err // TaskNotFound, before any query (spec §4)
	}
	row, err := w.log.LoadScoped(ctx, s.Service, s.Owner, id)
	if err != nil {
		return plan{}, w.internal(id, err)
	}
	if s.LastEventID == nil {
		if row.State.Terminal() {
			return plan{}, a2a.NewError(a2a.ErrUnsupportedOperation, fmt.Sprintf("the task is %s: a terminal task has no stream to subscribe to", row.State))
		}
		return plan{task: id, lead: row.Snapshot, after: row.Version, closes: terminal}, nil
	}
	k := *s.LastEventID
	tail, err := w.log.Tail(ctx, id)
	if err != nil {
		return plan{}, w.internal(id, err)
	}
	if k < 1 || k > tail {
		return plan{}, a2a.NewError(a2a.ErrInvalidParams, fmt.Sprintf("Last-Event-ID %d is not an event id of this task", k))
	}
	ts, err := w.log.TerminalSeq(ctx, id)
	if err != nil {
		return plan{}, w.internal(id, err)
	}
	if ts > 0 && k >= ts {
		return plan{}, a2a.NewError(a2a.ErrUnsupportedOperation, fmt.Sprintf("the task is %s and event %d is at or past its last event: nothing is missing", row.State, k))
	}
	lead, err := w.log.SnapshotAt(ctx, id, k)
	if err != nil {
		return plan{}, w.internal(id, err)
	}
	return plan{task: id, lead: lead, after: k, closes: terminal}, nil
}

// internal keeps the store's A2A errors (task not found, invalid params) and makes anything else an
// internal error, so a database error's text is not sent to the caller; it logs that error.
func (w *Writer) internal(task uuid.UUID, err error) error {
	var ae *a2a.Error
	if errors.As(err, &ae) || errors.Is(err, a2a.ErrTaskNotFound) || errors.Is(err, a2a.ErrInvalidParams) {
		return err
	}
	w.opts.Logger.Warn("sse: the task's log could not be read", "task", task, "error", err)
	return a2a.NewError(a2a.ErrInternalError, "the task's log could not be read")
}

// ParseLastEventID reads the Last-Event-ID header: absent is (0, false, nil); anything that is not a
// positive integer is an error wrapping a2a.ErrInvalidParams. Repeated headers must agree.
func ParseLastEventID(h http.Header) (int64, bool, error) {
	vals := h.Values("Last-Event-ID")
	if len(vals) == 0 {
		return 0, false, nil
	}
	var k int64
	for i, v := range vals {
		n, ok := positive(strings.TrimSpace(v))
		if !ok || (i > 0 && n != k) {
			return 0, false, a2a.NewError(a2a.ErrInvalidParams, "Last-Event-ID must be one positive integer: an event id this gate sent")
		}
		k = n
	}
	return k, true, nil
}

func positive(s string) (int64, bool) {
	if s == "" || len(s) > 19 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
}

// serve writes the SSE headers, the plan's leading snapshot and the rows after it, live, until the plan's
// closing row, the caller's disconnect, or a log that cannot be read for ReadGrace.
func (w *Writer) serve(rw http.ResponseWriter, r *http.Request, s Stream, p plan) {
	ctx := r.Context()
	var wake <-chan struct{}
	if w.waker != nil {
		// Registered before the first read: a row committed after it wakes the stream, one committed before
		// it is found by the read.
		var stop func()
		wake, stop = w.waker.WaitTask(ctx, p.task)
		defer stop()
	}
	sw := w.begin(rw, r, s)
	keep := time.NewTicker(w.opts.KeepAlive)
	defer keep.Stop()
	if p.lead == nil && p.leadAt > 0 {
		lead, ok := w.readLead(ctx, sw, keep.C, p)
		if !ok {
			return
		}
		p.lead, p.leadCloses = lead, !p.followUp && endsTurn(lead.Status.State)
	}
	if p.lead != nil {
		lead, err := w.opts.Render.Task(ctx, p.lead)
		if err != nil {
			w.opts.Logger.Warn("sse: the snapshot's parts could not be rendered; ending the stream", "task", p.task, "error", err)
			_ = sw.error(a2a.NewError(a2a.ErrInternalError, "the task's parts could not be rendered; subscribe to the task again"))
			return
		}
		if sw.event(0, mustStreamResponse(lead)) != nil || p.leadCloses {
			return
		}
	}
	cursor := p.after
	var failingSince time.Time
	backoff := 100 * time.Millisecond
	for {
		evs, more, err := w.log.EventsWithin(ctx, p.task, cursor, batch, readBytes)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if failingSince.IsZero() {
				failingSince = time.Now()
			}
			if time.Since(failingSince) >= w.opts.ReadGrace {
				w.opts.Logger.Warn("sse: the task's log could not be read; ending the stream", "task", p.task, "error", err)
				_ = sw.error(a2a.NewError(a2a.ErrInternalError, "the task's log could not be read; resume with Last-Event-ID"))
				return
			}
			if !w.wait(ctx, sw, keep.C, nil, backoff) {
				return
			}
			backoff = min(2*backoff, 5*time.Second)
			continue
		}
		var renderErr error
		for i, ev := range evs {
			evs[i] = store.Event{} // the read's rows are let go one by one as they are sent
			if !ev.Kind.Carried() {
				cursor = ev.Seq
				continue // message and metadata rows show in the next snapshot (spec §9)
			}
			state, data, err := rowData(ev)
			if err != nil {
				w.opts.Logger.Error("sse: a row of the task's log is not a stream response", "task", p.task, "seq", ev.Seq, "error", err)
				_ = sw.error(a2a.NewError(a2a.ErrInternalError, "the task's log holds an unreadable event"))
				return
			}
			if data, renderErr = w.opts.Render.StreamResponse(ctx, p.task, data); renderErr != nil {
				break // like a read that failed: the row is sent once it renders
			}
			if sw.event(ev.Seq, data) != nil {
				return
			}
			cursor = ev.Seq
			if state != "" && p.closes(state) {
				return
			}
		}
		if renderErr != nil {
			if ctx.Err() != nil {
				return
			}
			if failingSince.IsZero() {
				failingSince = time.Now()
			}
			if time.Since(failingSince) >= w.opts.ReadGrace {
				w.opts.Logger.Warn("sse: a row's parts could not be rendered; ending the stream", "task", p.task, "after", cursor, "error", renderErr)
				_ = sw.error(a2a.NewError(a2a.ErrInternalError, "the task's parts could not be rendered; resume with Last-Event-ID"))
				return
			}
			w.opts.Logger.Warn("sse: a row's parts could not be rendered; retrying", "task", p.task, "after", cursor, "error", renderErr)
			if !w.wait(ctx, sw, keep.C, nil, backoff) {
				return
			}
			backoff = min(2*backoff, 5*time.Second)
			continue
		}
		failingSince, backoff = time.Time{}, 100*time.Millisecond
		if more {
			continue
		}
		if !w.wait(ctx, sw, keep.C, wake, w.opts.Reread) {
			return
		}
	}
}

// readLead reads the snapshot at p.leadAt for a stream that has begun, retrying a read that fails (backing
// off from 100 ms to 5 s, with keep-alives) for up to ReadGrace; then the stream ends with an error event.
// false when the stream is over (the caller went away, or the error event was sent).
func (w *Writer) readLead(ctx context.Context, sw *streamWriter, keep <-chan time.Time, p plan) (*a2a.Task, bool) {
	began := time.Now()
	backoff := 100 * time.Millisecond
	for {
		lead, err := w.log.SnapshotAt(ctx, p.task, p.leadAt)
		if err == nil {
			return lead, true
		}
		if ctx.Err() != nil {
			return nil, false
		}
		if time.Since(began) >= w.opts.ReadGrace {
			w.opts.Logger.Warn("sse: the task's log could not be read; ending the stream", "task", p.task, "seq", p.leadAt, "error", err)
			_ = sw.error(a2a.NewError(a2a.ErrInternalError, "the task's log could not be read; subscribe to the task again"))
			return nil, false
		}
		if !w.wait(ctx, sw, keep, nil, backoff) {
			return nil, false
		}
		backoff = min(2*backoff, 5*time.Second)
	}
}

// wait blocks until a wake, d, or the caller's disconnect (false), writing keep-alives meanwhile.
func (w *Writer) wait(ctx context.Context, sw *streamWriter, keep <-chan time.Time, wake <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-wake: // a nil channel (no waker) never receives
			return true
		case <-t.C:
			return true
		case <-keep:
			if sw.keepAlive() != nil {
				return false
			}
		}
	}
}

// rowData returns a carried row's state (empty for an artifact row) and its StreamResponse bytes on one
// line. The payload is the stored bytes (json, D49), sent as they are: a NUL or any other character
// survives. Only a payload stored across lines (whitespace between tokens: JSON escapes a newline in a
// string) is compacted, into a copy. The probe decodes the state and skips everything else, so a large
// artifact is not copied to be checked.
func rowData(ev store.Event) (a2a.TaskState, []byte, error) {
	if len(ev.Payload) == 0 {
		return "", nil, errors.New("no inline payload")
	}
	var probe struct {
		Task *struct {
			Status struct {
				State a2a.TaskState `json:"state"`
			} `json:"status"`
		} `json:"task"`
		StatusUpdate *struct {
			Status struct {
				State a2a.TaskState `json:"state"`
			} `json:"status"`
		} `json:"statusUpdate"`
		ArtifactUpdate *struct{} `json:"artifactUpdate"`
	}
	if err := json.Unmarshal(ev.Payload, &probe); err != nil {
		return "", nil, err
	}
	var state a2a.TaskState
	switch {
	case ev.Kind == store.KindTask && probe.Task != nil:
		state = probe.Task.Status.State
	case ev.Kind == store.KindStatus && probe.StatusUpdate != nil:
		state = probe.StatusUpdate.Status.State
	case ev.Kind == store.KindArtifact && probe.ArtifactUpdate != nil:
	default:
		return "", nil, fmt.Errorf("a %s row without its event", ev.Kind)
	}
	if !bytes.ContainsAny(ev.Payload, "\r\n") {
		return state, ev.Payload, nil
	}
	var b bytes.Buffer
	if err := json.Compact(&b, ev.Payload); err != nil {
		return "", nil, err
	}
	return state, b.Bytes(), nil
}

func mustStreamResponse(t *a2a.Task) []byte {
	b, err := json.Marshal(a2a.StreamResponse{Event: t})
	if err != nil {
		// A task decoded from the log always encodes again; this is a bug, not a caller's error.
		panic(fmt.Sprintf("sse: encode a task snapshot: %v", err))
	}
	return b
}

// streamWriter frames events for one response.
type streamWriter struct {
	rw      http.ResponseWriter
	rc      *http.ResponseController
	binding Binding
	id      json.RawMessage
	timeout time.Duration // each write's deadline (Options.WriteTimeout)
}

// arm bounds the writes that follow by the stream's write timeout; disarm lifts the bound again, so a
// stream that waits for its next event is never cut (an HTTP/2 stream is reset when its deadline passes,
// written to or not). A ResponseWriter without deadlines (a test recorder) is written without one.
func (sw *streamWriter) arm()    { _ = sw.rc.SetWriteDeadline(time.Now().Add(sw.timeout)) }
func (sw *streamWriter) disarm() { _ = sw.rc.SetWriteDeadline(time.Time{}) }

// begin writes the SSE headers: from here on errors are events.
func (w *Writer) begin(rw http.ResponseWriter, r *http.Request, s Stream) *streamWriter {
	h := rw.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	h.Del("Content-Length")
	if r.ProtoMajor < 2 {
		h.Set("Connection", "keep-alive")
	}
	rw.WriteHeader(http.StatusOK)
	sw := &streamWriter{rw: rw, rc: http.NewResponseController(rw), binding: s.Binding, id: s.RequestID, timeout: w.opts.WriteTimeout}
	if len(sw.id) == 0 {
		sw.id = json.RawMessage("null")
	}
	sw.arm()
	_ = sw.rc.Flush()
	sw.disarm()
	return sw
}

// event writes one event: `id: <seq>` (none when seq is 0) and one data line. The StreamResponse is
// written as it is, between its prefix and suffix, never copied into a buffer of its own.
func (sw *streamWriter) event(seq int64, streamResponse []byte) error {
	var b bytes.Buffer
	if seq > 0 {
		b.WriteString("id: ")
		b.WriteString(strconv.FormatInt(seq, 10))
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	suffix := "\n\n"
	if sw.binding == JSONRPC {
		b.WriteString(`{"jsonrpc":"2.0","id":`)
		b.Write(sw.id)
		b.WriteString(`,"result":`)
		suffix = "}\n\n"
	}
	sw.arm()
	defer sw.disarm()
	for _, part := range [][]byte{b.Bytes(), streamResponse, []byte(suffix)} {
		if _, err := sw.rw.Write(part); err != nil {
			return err
		}
	}
	return sw.rc.Flush()
}

// error writes an error event after the headers: the JSON-RPC error envelope, or the google.rpc.Status.
func (sw *streamWriter) error(err error) error {
	var data []byte
	if sw.binding == JSONRPC {
		data = bytes.TrimRight(JSONRPCErrorBody(sw.id, ToJSONRPC(public(err))), "\n")
	} else {
		_, body := HTTPError(err)
		data = bytes.TrimRight(body, "\n")
	}
	return sw.write(append(append([]byte("data: "), data...), '\n', '\n'))
}

func (sw *streamWriter) keepAlive() error { return sw.write([]byte(": keep-alive\n\n")) }

func (sw *streamWriter) write(b []byte) error {
	sw.arm()
	defer sw.disarm()
	if _, err := sw.rw.Write(b); err != nil {
		return err
	}
	return sw.rc.Flush()
}
