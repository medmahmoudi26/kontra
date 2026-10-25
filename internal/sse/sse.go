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
	"context"
	"encoding/json"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Binding is the transport a stream is framed for.
type Binding int

// Bindings.
const (
	JSONRPC  Binding = iota // data: {"jsonrpc":"2.0","id":…,"result":<StreamResponse>}
	HTTPJSON                // data: <StreamResponse>
)

// Writer serves streams from the log.
type Writer struct {
	log   *store.Log
	waker store.TaskWaker
}

// NewWriter returns a writer over the log, woken by the replica's LISTEN connection.
func NewWriter(log *store.Log, waker store.TaskWaker) *Writer {
	return &Writer{log: log, waker: waker}
}

// Stream is one stream request, after authentication and routing.
type Stream struct {
	Binding     Binding
	RequestID   json.RawMessage // the JSON-RPC id, echoed in every envelope
	Service     store.ServiceRef
	Owner       store.Owner
	TaskID      a2a.TaskID
	LastEventID *int64 // set when the request carried Last-Event-ID
}

// Starter creates or continues a task through a2a-go's handler for SendStreamingMessage and reports the
// task id and the version V the stream starts after (lifecycle.StreamStart): 0 for a new task (the
// creation row, seq 1, is the first event), V for a follow-up (the snapshot at V leads, without an id).
type Starter func(ctx context.Context) (taskID a2a.TaskID, after int64, isNew bool, err error)

// SendStreaming serves SendStreamingMessage. It closes after the first row whose state is terminal,
// input-required or quoted (for a follow-up: the first such row with seq > V).
func (w *Writer) SendStreaming(rw http.ResponseWriter, r *http.Request, s Stream, start Starter) {
	panic(unimpl.Error("sse.Writer.SendStreaming", "WP-06"))
}

// Subscribe serves SubscribeToTask, with or without Last-Event-ID. Without it a terminal task is
// UnsupportedOperationError (TCK STREAM-SUB-003); otherwise the current snapshot without an id, then the
// carried rows after its version, open through input-required and auth-required until the terminal row
// (TCK STREAM-SUB-002).
func (w *Writer) Subscribe(rw http.ResponseWriter, r *http.Request, s Stream) {
	panic(unimpl.Error("sse.Writer.Subscribe", "WP-06"))
}

// ParseLastEventID reads the Last-Event-ID header: absent is (0, false, nil); anything that is not a
// positive integer is an error wrapping a2a.ErrInvalidParams.
func ParseLastEventID(h http.Header) (int64, bool, error) {
	return 0, false, unimpl.Error("sse.ParseLastEventID", "WP-06")
}
