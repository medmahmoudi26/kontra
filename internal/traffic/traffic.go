// Package traffic is the A2A traffic record (spec/relay.md §13.6; the a2a-traffic capability): one metadata-only
// exchange per A2A call the gate receives, refused calls included, in a2a_exchanges (migration 0006).
//
// An exchange holds the time, the service as requested, the method, the binding, the caller's principal name and via,
// the task and context ids when the call names or creates one, the sizes, the duration and the outcome (HTTP status,
// JSON-RPC error code, the gate's reason). Never a body, a header, a part, a credential or a credential's hash.
//
// Recording never affects a call: Record hands the exchange to a bounded channel and returns; one goroutine per
// replica (Run) writes batches with COPY. A full channel, or a batch the database refuses, is dropped and counted
// (Dropped, kontra_traffic_dropped_total), never waited for.
//
// The two points that record are internal/a2ahttp (every request to an A2A route, around its handler) and the edge's
// own refusals (413, 429, 408) on A2A paths, which never reach a handler.
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

// Bounds of the writer (design §6): the channel, and a batch's rows and age.
const (
	QueueSize  = 4096
	BatchRows  = 512
	FlushEvery = 250 * time.Millisecond
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
	At         time.Time // when the call arrived
	TenantID   *uuid.UUID
	Service    string // as requested
	Method     string
	Binding    string
	Principal  string // "" before authentication
	Via        string // with Principal
	TaskID     *uuid.UUID
	ContextID  string
	BytesIn    int64
	BytesOut   int64
	Duration   time.Duration
	HTTPStatus int
	RPCCode    *int
	Reason     string
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
}

// New returns a recorder writing to db; Run must run for anything to be written.
func New(db Copier, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.Default()
	}
	return &Recorder{db: db, log: log, ch: make(chan Exchange, QueueSize), every: FlushEvery, rows: BatchRows}
}

// Record queues e, or drops and counts it when the queue is full. It never blocks.
func (r *Recorder) Record(e Exchange) {
	if r == nil {
		return
	}
	select {
	case r.ch <- e:
	default:
		r.dropped.Add(1)
	}
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
	"bytes_in", "bytes_out", "duration_ms", "http_status", "rpc_code", "reason"}

func (r *Recorder) write(ctx context.Context, batch []Exchange) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	rows := make([][]any, len(batch))
	for i, e := range batch {
		rows[i] = e.row()
	}
	if _, err := r.db.CopyFrom(ctx, pgx.Identifier{"kontra", "a2a_exchanges"}, columns, pgx.CopyFromRows(rows)); err != nil {
		r.dropped.Add(int64(len(batch)))
		r.log.Warn("traffic: a batch of exchanges was not stored and is dropped", "exchanges", len(batch), "error", err)
	}
}

// row is e as a2a_exchanges stores it, every value inside its column's CHECK.
func (e Exchange) row() []any {
	id := uuid.Must(uuid.NewV7())
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
	return []any{id, e.At, e.TenantID, clip(e.Service, 256), method, binding, principal, via, e.TaskID,
		null(e.ContextID, 256), max(e.BytesIn, 0), max(e.BytesOut, 0), int32(min(max(e.Duration.Milliseconds(), 0), 1<<31-1)),
		int16(status), rpc, null(e.Reason, 64)}
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
// tenant of the service, and the gate's reason for a refusal.
type Note struct {
	mu        sync.Mutex
	principal string
	via       string
	tenant    *uuid.UUID
	reason    string
}

type noteKey struct{}

// WithNote returns ctx carrying a new Note.
func WithNote(ctx context.Context) (context.Context, *Note) {
	n := &Note{}
	return context.WithValue(ctx, noteKey{}, n), n
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

// Fill copies what the note holds into e.
func (n *Note) Fill(e *Exchange) {
	n.mu.Lock()
	defer n.mu.Unlock()
	e.Principal, e.Via, e.TenantID, e.Reason = n.principal, n.via, n.tenant, n.reason
}
