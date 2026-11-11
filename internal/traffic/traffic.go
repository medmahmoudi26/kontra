// Package traffic is the A2A traffic record (spec/relay.md §13.6; the a2a-traffic capability): one exchange per A2A
// call on either leg, refused calls included, in a2a_exchanges (migrations 0006, 0007): the calls the gate receives
// (leg caller) and the calls it makes to a service's agent (leg callee, D355).
//
// An exchange holds the time, the leg, the service as requested, the method, the binding, the A2A version, the
// caller's principal name and via, the task and context ids when the call names or creates one (on the callee leg the
// agent's, with the gate task beside them), the sizes, the duration and the outcome (HTTP status, JSON-RPC error code,
// the gate's reason). With bodies on (traffic.bodies, D358) it also holds its request body and its response body or
// stream frames, each cut at traffic.body_max, credential members redacted (Redact). Never a header, a credential or a
// credential's hash.
//
// Recording never affects a call: Record hands the exchange to a bounded channel and returns; one goroutine per
// replica (Run) writes batches with COPY. A full channel, or a batch the database refuses, is dropped and counted
// (Dropped, kontra_traffic_dropped_total), never waited for. Queued bodies have a byte budget: past it an exchange is
// queued without them (BodiesDropped, kontra_traffic_bodies_dropped_total).
//
// The points that record are internal/a2ahttp (every request to an A2A route, around its handler), the edge's own
// refusals (413, 429, 408) on A2A paths, which never reach a handler, and the callee leg's RoundTripper (callee.go).
package traffic

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Bounds of the writer (design §6): the channel, and a batch's rows and age; and the bytes of bodies and frames queued
// at once (D358).
const (
	QueueSize   = 4096
	BatchRows   = 512
	FlushEvery  = 250 * time.Millisecond
	BodyBudget  = 64 << 20
	FramesTimes = 16 // an exchange's frames, in all, at most FramesTimes × body_max
	// writeTimeout bounds one batch's COPY; a batch that does not land in time is dropped.
	writeTimeout = 5 * time.Second
)

// Bindings.
const (
	BindingJSONRPC = "jsonrpc"
	BindingREST    = "rest"
)

// MethodUnknown is the method of a call whose method was never read (an edge refusal) or is not one of A2A's.
const MethodUnknown = "unknown"

// Legs (D355).
const (
	LegCaller = "caller" // a call the gate received
	LegCallee = "callee" // a call the gate made to a service's agent
)

// Gate reasons the A2A routes record (the edge's are edge.Reason*).
const (
	ReasonUnauthenticated    = "unauthenticated"
	ReasonUnknownService     = "unknown_service"
	ReasonAllowDenied        = "allow_denied"
	ReasonVersionUnsupported = "version_unsupported"
	ReasonExtensionRequired  = "extension_required"
	ReasonInvalidRequest     = "invalid_request"
	ReasonAdmissionDenied    = "admission_denied"
)

// Exchange is one A2A call.
type Exchange struct {
	ID           uuid.UUID // minted when the call arrived (NewID, D356); minted at write when zero
	At           time.Time // when the call arrived
	Leg          string    // LegCaller (the default) or LegCallee
	TenantID     *uuid.UUID
	Service      string // as requested
	Method       string
	Binding      string
	A2AVersion   string // "1.0", "0.3", or "" when not known
	Principal    string // "" before authentication
	Via          string // with Principal
	TaskID       *uuid.UUID
	ContextID    string
	GateTaskID   *uuid.UUID // the callee leg's gate task
	CalleeTaskID string     // the callee leg's agent task id (TaskID stays nil there)
	BytesIn      int64
	BytesOut     int64
	Duration     time.Duration
	HTTPStatus   int
	RPCCode      *int
	Reason       string
	// With bodies on: the request body, the response body (nil for a stream), the stream's frames.
	Request       *Body
	Response      *Body
	Frames        []Frame
	FramesDropped int
}

// Body is a kept request or response body: Data cut at body_max and redacted, Size the full size.
type Body struct {
	Data      []byte
	Size      int64
	Truncated bool
}

// Frame is one kept SSE event of a stream: its data, its SSE id, and when it was sent from the call's start.
type Frame struct {
	Offset    time.Duration
	SSEID     string
	Data      []byte
	Size      int64
	Truncated bool
}

// NewID mints an exchange id: UUIDv7, so ids order by arrival.
func NewID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

// bodyBytes is what e's bodies and frames hold in memory.
func (e *Exchange) bodyBytes() int64 {
	var n int64
	if e.Request != nil {
		n += int64(len(e.Request.Data))
	}
	if e.Response != nil {
		n += int64(len(e.Response.Data))
	}
	for _, f := range e.Frames {
		n += int64(len(f.Data))
	}
	return n
}

// stripBodies drops e's bodies and frames, keeping its metadata.
func (e *Exchange) stripBodies() {
	e.Request, e.Response, e.Frames, e.FramesDropped = nil, nil, nil, 0
}

// Copier is the one database call the writer makes; *pgxpool.Pool implements it.
type Copier interface {
	CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, rows pgx.CopyFromSource) (int64, error)
}

// Recorder is a replica's exchange writer. A nil *Recorder records nothing (traffic.record off).
type Recorder struct {
	db      Copier
	log     *slog.Logger
	ch      chan Exchange
	dropped atomic.Int64
	every   time.Duration
	rows    int

	bodyMax       int   // 0: bodies off
	budget        int64 // bytes of bodies and frames queued at once
	queued        atomic.Int64
	bodiesDropped atomic.Int64
}

// New returns a recorder writing to db; Run must run for anything to be written. It keeps no bodies until KeepBodies.
func New(db Copier, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.Default()
	}
	return &Recorder{db: db, log: log, ch: make(chan Exchange, QueueSize), every: FlushEvery, rows: BatchRows, budget: BodyBudget}
}

// KeepBodies turns bodies on (traffic.bodies), each body or frame cut at max bytes. Call it before the first Record.
func (r *Recorder) KeepBodies(max int) *Recorder {
	if r != nil && max > 0 {
		r.bodyMax = max
	}
	return r
}

// BodyMax is how much of each body or frame is kept: 0 when bodies are off (or r is nil).
func (r *Recorder) BodyMax() int {
	if r == nil {
		return 0
	}
	return r.bodyMax
}

// Record queues e, or drops and counts it when the queue is full. It never blocks. Past the body budget, e is queued
// without its bodies and frames, and counted in BodiesDropped.
func (r *Recorder) Record(e Exchange) {
	if r == nil {
		return
	}
	if r.bodyMax == 0 {
		e.stripBodies()
	}
	n := e.bodyBytes()
	if n > 0 {
		if r.queued.Add(n) > r.budget {
			r.queued.Add(-n)
			e.stripBodies()
			r.bodiesDropped.Add(1)
			n = 0
		}
	}
	select {
	case r.ch <- e:
	default:
		r.queued.Add(-n)
		r.dropped.Add(1)
	}
}

// BodiesDropped is how many exchanges this replica queued without their bodies because the body budget was full.
func (r *Recorder) BodiesDropped() int64 {
	if r == nil {
		return 0
	}
	return r.bodiesDropped.Load()
}

// Dropped is how many exchanges this replica has dropped since it started.
func (r *Recorder) Dropped() int64 {
	if r == nil {
		return 0
	}
	return r.dropped.Load()
}

// Run writes queued exchanges in batches until ctx ends, then writes what is queued once more.
func (r *Recorder) Run(ctx context.Context) error {
	t := time.NewTicker(r.every)
	defer t.Stop()
	batch := make([]Exchange, 0, r.rows)
	flush := func(ctx context.Context) {
		if len(batch) > 0 {
			r.write(ctx, batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case <-ctx.Done():
			// What is queued when the replica stops is written once more, outside ctx.
			bg := context.WithoutCancel(ctx)
			for drained := false; !drained; {
				select {
				case e := <-r.ch:
					batch = append(batch, e)
					if len(batch) == r.rows {
						flush(bg)
					}
				default:
					drained = true
				}
			}
			flush(bg)
			return nil
		case e := <-r.ch:
			batch = append(batch, e)
			if len(batch) >= r.rows {
				flush(ctx)
			}
		case <-t.C:
			flush(ctx)
		}
	}
}

var columns = []string{"id", "at", "tenant_id", "service", "method", "binding", "principal", "via", "task_id", "context_id",
	"bytes_in", "bytes_out", "duration_ms", "http_status", "rpc_code", "reason",
	"leg", "gate_task_id", "callee_task_id", "a2a_version", "request_body", "request_size", "request_truncated",
	"response_body", "response_size", "response_truncated", "frames_dropped"}

var frameColumns = []string{"exchange_id", "n", "offset_ms", "sse_id", "data", "size", "truncated"}

// write stores a batch: its exchanges, then their frames, two COPYs of one batch (the frames' foreign key needs their
// exchanges first). Frames the database refuses are dropped with the exchanges' metadata kept.
func (r *Recorder) write(ctx context.Context, batch []Exchange) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	var held int64
	rows := make([][]any, len(batch))
	var frames [][]any
	for i := range batch {
		if batch[i].ID == uuid.Nil {
			batch[i].ID = NewID()
		}
		held += batch[i].bodyBytes()
		rows[i] = batch[i].row()
		frames = append(frames, batch[i].frameRows()...)
	}
	defer r.queued.Add(-held)
	if _, err := r.db.CopyFrom(ctx, pgx.Identifier{"kontra", "a2a_exchanges"}, columns, pgx.CopyFromRows(rows)); err != nil {
		r.dropped.Add(int64(len(batch)))
		r.log.Warn("traffic: a batch of exchanges was not stored and is dropped", "exchanges", len(batch), "error", err)
		return
	}
	if len(frames) == 0 {
		return
	}
	if _, err := r.db.CopyFrom(ctx, pgx.Identifier{"kontra", "a2a_exchange_frames"}, frameColumns, pgx.CopyFromRows(frames)); err != nil {
		r.bodiesDropped.Add(int64(len(batch)))
		r.log.Warn("traffic: a batch's stream frames were not stored and are dropped", "frames", len(frames), "error", err)
	}
}

// frameRows are e's frames as a2a_exchange_frames stores them.
func (e Exchange) frameRows() [][]any {
	rows := make([][]any, 0, len(e.Frames))
	for i, f := range e.Frames {
		var sseID any
		if f.SSEID != "" {
			sseID = clip(f.SSEID, 64)
		}
		data := f.Data
		if data == nil {
			data = []byte{}
		}
		rows = append(rows, []any{e.ID, int32(i), int32(min(max(f.Offset.Milliseconds(), 0), 1<<31-1)), sseID, data,
			max(f.Size, int64(len(data))), f.Truncated})
	}
	return rows
}

// row is e as a2a_exchanges stores it, every value inside its column's CHECK.
func (e Exchange) row() []any {
	id := e.ID
	if id == uuid.Nil {
		id = NewID()
	}
	null := func(s string, max int) any {
		if s == "" {
			return nil
		}
		return clip(s, max)
	}
	binding := e.Binding
	if binding != BindingREST {
		binding = BindingJSONRPC
	}
	method := clip(e.Method, 64)
	if method == "" {
		method = MethodUnknown
	}
	principal, via := null(e.Principal, 256), null(e.Via, 16)
	if principal == nil || via == nil || !knownVia[e.Via] {
		principal, via = nil, nil
	}
	status := e.HTTPStatus
	if status < 100 || status > 599 {
		status = 200 // a handler that wrote nothing was answered 200 by net/http
	}
	var rpc any
	if e.RPCCode != nil {
		rpc = *e.RPCCode
	}
	leg, gateTask, calleeTask, taskID := LegCaller, any(nil), any(nil), any(e.TaskID)
	if e.Leg == LegCallee {
		leg, taskID = LegCallee, nil
		if e.GateTaskID != nil {
			gateTask = *e.GateTaskID
		}
		calleeTask = null(e.CalleeTaskID, 256)
	}
	body := func(b *Body) (data, size any, truncated bool) {
		if b == nil {
			return nil, nil, false
		}
		d := b.Data
		if d == nil {
			d = []byte{}
		}
		return d, max(b.Size, int64(len(d))), b.Truncated
	}
	reqBody, reqSize, reqTrunc := body(e.Request)
	respBody, respSize, respTrunc := body(e.Response)
	return []any{id, e.At, e.TenantID, clip(e.Service, 256), method, binding, principal, via, taskID,
		null(e.ContextID, 256), max(e.BytesIn, 0), max(e.BytesOut, 0), int32(min(max(e.Duration.Milliseconds(), 0), 1<<31-1)),
		int16(status), rpc, null(e.Reason, 64),
		leg, gateTask, calleeTask, null(e.A2AVersion, 16), reqBody, reqSize, reqTrunc, respBody, respSize, respTrunc, int32(max(e.FramesDropped, 0))}
}

var knownVia = map[string]bool{"api_key": true, "oidc": true, "mcp": true, "node": true, "anonymous": true}

// clip cuts s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// Sweep deletes, in batches of limit, the exchanges older than before (traffic.retention), whatever became of their
// tasks. It returns how many it deleted.
func Sweep(ctx context.Context, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, before time.Time, limit int) (int, error) {
	total := 0
	for {
		tag, err := db.Exec(ctx, `delete from kontra.a2a_exchanges where id in (
			select id from kontra.a2a_exchanges where at < $1 order by at limit $2)`, before, limit)
		if err != nil {
			return total, fmt.Errorf("traffic: sweep: %w", err)
		}
		n := int(tag.RowsAffected())
		total += n
		if n < limit {
			return total, nil
		}
	}
}

// Note is what a call's handlers tell the recorder while the call runs (WithNote): the caller once authenticated, the
// tenant of the service, the gate's reason for a refusal, and that the call's bodies may not be kept (a sealed
// service). It also carries the exchange's id, minted when the call arrived (D356).
type Note struct {
	id        uuid.UUID
	mu        sync.Mutex
	principal string
	via       string
	tenant    *uuid.UUID
	reason    string
	noBodies  bool
}

type noteKey struct{}

// WithNote returns ctx carrying a new Note with a fresh exchange id.
func WithNote(ctx context.Context) (context.Context, *Note) {
	n := &Note{id: NewID()}
	return context.WithValue(ctx, noteKey{}, n), n
}

// ID is the exchange's id.
func (n *Note) ID() uuid.UUID { return n.id }

// ExchangeID is the id of the exchange ctx's call is recorded as, when it is recorded.
func ExchangeID(ctx context.Context) (uuid.UUID, bool) {
	if n := noteOf(ctx); n != nil {
		return n.id, true
	}
	return uuid.Nil, false
}

// SetNoBodies says the call's bodies may not be kept (a sealed service's call).
func SetNoBodies(ctx context.Context) {
	if n := noteOf(ctx); n != nil {
		n.mu.Lock()
		n.noBodies = true
		n.mu.Unlock()
	}
}

func noteOf(ctx context.Context) *Note {
	n, _ := ctx.Value(noteKey{}).(*Note)
	return n
}

// SetCaller records the call's principal.
func SetCaller(ctx context.Context, principal, via string) {
	if n := noteOf(ctx); n != nil {
		n.mu.Lock()
		n.principal, n.via = principal, via
		n.mu.Unlock()
	}
}

// SetTenant records the tenant of the service the call reached.
func SetTenant(ctx context.Context, id uuid.UUID) {
	if n := noteOf(ctx); n != nil {
		n.mu.Lock()
		n.tenant = &id
		n.mu.Unlock()
	}
}

// SetReason records the gate's reason for refusing the call; the first reason set is kept.
func SetReason(ctx context.Context, reason string) {
	if n := noteOf(ctx); n != nil {
		n.mu.Lock()
		if n.reason == "" {
			n.reason = reason
		}
		n.mu.Unlock()
	}
}

// Fill copies what the note holds into e, and drops e's bodies when the call's may not be kept.
func (n *Note) Fill(e *Exchange) {
	n.mu.Lock()
	defer n.mu.Unlock()
	e.ID = n.id
	e.Principal, e.Via, e.TenantID, e.Reason = n.principal, n.via, n.tenant, n.reason
	if n.noBodies {
		e.stripBodies()
	}
}

// NoBodies reports whether the call's bodies may not be kept.
func (n *Note) NoBodies() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.noBodies
}
