package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/blob"
)

// WriteTimeout bounds every write. Writes run on a context detached from the caller's cancellation (D8),
// so a terminal event is never lost because the request that caused it ended; the deadline of their own
// keeps a dead primary from stalling the writer until the connection bounds fire (pg-store brief §5.4).
const WriteTimeout = 10 * time.Second

// Log is the unscoped internal API over tasks and task_events. Its Append is the only code that writes
// the projection and the log; Store, the work queue, write_terminal and apply_metadata all go through it.
type Log struct {
	db           *pgxpool.Pool
	writeTimeout time.Duration
	appendStmt   string // appendSQL; the NOTIFY benchmark swaps in a variant without pg_notify
}

// LogOption configures a Log.
type LogOption func(*Log)

// WithWriteTimeout replaces WriteTimeout (tests).
func WithWriteTimeout(d time.Duration) LogOption { return func(l *Log) { l.writeTimeout = d } }

// NewLog returns the internal API over db.
func NewLog(db *pgxpool.Pool, opts ...LogOption) *Log {
	l := &Log{db: db, writeTimeout: WriteTimeout, appendStmt: appendSQL}
	for _, o := range opts {
		o(l)
	}
	return l
}

// DB is the pool the log writes through.
func (l *Log) DB() *pgxpool.Pool { return l.db }

// querier is what the pool and a transaction both offer.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (l *Log) on(tx pgx.Tx) querier {
	if tx != nil {
		return tx
	}
	return l.db
}

// clientMargin is how much longer than the server-side statement_timeout a writer waits for the server's
// answer, so a write the server aborted reports the server's error rather than the client's deadline.
const clientMargin = 2 * time.Second

// writeCtx detaches ctx from its cancellation (its values stay) and bounds it by the write timeout plus
// clientMargin; the statements themselves are bounded on the server (writeRow, writeExec, inTx).
func (l *Log) writeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), l.writeTimeout+clientMargin)
}

// stmtTimeoutSQL bounds, on the server, the statements after it in the same transaction.
const stmtTimeoutSQL = `select set_config('statement_timeout', $1, true)`

func (l *Log) timeoutMillis() string {
	return strconv.FormatInt(max(l.writeTimeout.Milliseconds(), 1), 10)
}

// timedOut maps the server's statement_timeout (57014) on a write to an error that also wraps
// context.DeadlineExceeded: the write did not commit, and callers test one condition.
func (l *Log) timedOut(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		return fmt.Errorf("store: write ran out of its %v: %w (%w)", l.writeTimeout, context.DeadlineExceeded, err)
	}
	return err
}

// writeRow runs one write statement and scans its one row (pgx.ErrNoRows when it returns none).
//
// On its own (tx nil) the statement is sent in one round trip after a transaction-local
// statement_timeout of the write timeout, in the same implicit transaction (one Sync): the server aborts
// a write that runs out of time, so a write that returned an error never commits later. With only a client
// deadline, an autocommit statement waiting on a row lock is abandoned by the client but still runs, and
// commits, when the lock goes. In a caller's transaction the commit is the caller's, and the server rolls
// back a transaction whose connection the client gave up on.
func (l *Log) writeRow(ctx context.Context, tx pgx.Tx, sql string, args []any, dest ...any) error {
	if tx != nil {
		return l.timedOut(tx.QueryRow(ctx, sql, args...).Scan(dest...))
	}
	return l.timedOut(retry(ctx, nil, func() error {
		b := &pgx.Batch{}
		b.Queue(stmtTimeoutSQL, l.timeoutMillis())
		b.Queue(sql, args...)
		br := l.db.SendBatch(ctx, b)
		_, err := br.Exec()
		if err == nil {
			err = br.QueryRow().Scan(dest...)
		}
		if cerr := br.Close(); err == nil {
			err = cerr
		}
		return err
	}))
}

// writeExec is writeRow for a statement without a result row.
func (l *Log) writeExec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	var tag pgconn.CommandTag
	err := retry(ctx, nil, func() error {
		b := &pgx.Batch{}
		b.Queue(stmtTimeoutSQL, l.timeoutMillis())
		b.Queue(sql, args...)
		br := l.db.SendBatch(ctx, b)
		_, err := br.Exec()
		if err == nil {
			tag, err = br.Exec()
		}
		if cerr := br.Close(); err == nil {
			err = cerr
		}
		return err
	})
	return tag, l.timedOut(err)
}

// inTx runs fn in a transaction whose statements the server bounds by the write timeout.
func (l *Log) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return l.timedOut(retry(ctx, nil, func() error {
		return pgx.BeginFunc(ctx, l.db, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, stmtTimeoutSQL, l.timeoutMillis()); err != nil {
				return err
			}
			return fn(tx)
		})
	}))
}

// retry runs fn, again (at most twice more) when it failed before anything reached the server
// (pgconn.SafeToRetry: a dead pooled connection). Inside a caller's transaction it never retries.
func retry(ctx context.Context, tx pgx.Tx, fn func() error) error {
	var err error
	for range 3 {
		if err = fn(); err == nil || tx != nil || !pgconn.SafeToRetry(err) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

// notFound is the one answer for a task that is unknown, malformed or outside the caller's scope (spec
// §4): the same message as ids.ParseTaskID's, so a caller cannot tell them apart.
func notFound() error { return a2a.NewError(a2a.ErrTaskNotFound, "task not found") }

// micros turns a duration into whole microseconds for SQL (nil when zero: the services row decides).
func micros(d time.Duration) any {
	if d <= 0 {
		return nil
	}
	return d.Microseconds()
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Truncate(time.Microsecond)
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// StatusTime is a status timestamp as tasks.status_at stores it: truncated to microseconds, so a page
// token minted from it matches the stored value (spec §5.2, pg-store brief §6.3). Nil is the zero time.
func StatusTime(ts *time.Time) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.UTC().Truncate(time.Microsecond)
}

// Write is one change to a task (spec §5.2).
type Write struct {
	TaskID      uuid.UUID
	PrevVersion int64 // must equal tasks.version; 0 is refused
	Kind        EventKind
	// State is the state after the event; empty leaves tasks.state as it is (artifact, message and
	// metadata rows).
	State State
	// StatusAt is the logged status.timestamp (truncated to microseconds here). Zero leaves status_at as it
	// is, except on a status or task row, which then stamps the commit time.
	StatusAt   time.Time
	Snapshot   *a2a.Task // the full projection after the event
	Payload    json.RawMessage
	PayloadRef string
	// AfterTerminalOK is set only by apply_metadata and an accepted review-window message, the two
	// writers allowed after a terminal event (spec §5.1).
	AfterTerminalOK bool
	// Timeout and Retention fix tasks.deadline at the transition into submitted and
	// tasks.retention_until at the terminal event. Zero takes the services row's timeout_s / retention_s.
	Timeout   time.Duration
	Retention time.Duration
	// Scope, when set, confines the write to the caller's tasks (Store); a task outside it is
	// a2a.ErrTaskNotFound.
	Scope *Scope
	// Fence, when set, makes the write commit only while the dispatch still holds its attempt (D77).
	Fence *Fence
	// Deliver, when set, is a follow-up work item of the task whose work_items.delivered_at the write sets,
	// in the same statement, if the write commits and the item has none yet (spec §6.4, D81). Unset, Append
	// takes the delivery the Fence has armed (Fence.ArmDelivery), if any.
	Deliver uuid.UUID
	// Call, when set, is the calls row of a gate-owned MCP task (spec §11.5 step 6, §11.8), written in the same
	// statement as the terminal event, so exactly when it commits: the terminal rule lets one terminal event per
	// task commit, so a task has one row whoever ends it (the dispatch, write_terminal, the orphan sweep).
	Call *Call
}

// Call is the calls row of a gate-owned MCP task (spec/relay.md §11.4, §11.5 step 6, §11.8): the tool, the
// creating session's hash, the byte counts and the outcome. Its tenant, service, buyer and principal are the
// task row's and its duration runs from the task's creation to the terminal event, all read in the statement.
type Call struct {
	Tool       string
	MCPSession []byte // the creating session's hash; nil for a modern request (D160's virtual session is WP-07's)
	BytesIn    int64
	BytesOut   int64
	IsError    bool
}

// MaxCallTool is the longest tool name a calls row holds (0001: 1 to 128 characters).
const MaxCallTool = 128

func (w *Write) validate() error {
	if w.PrevVersion < 1 {
		// a2a-go's in-memory store reads 0 as "skip the check"; this store never writes unchecked.
		return fmt.Errorf("store: PrevVersion %d is never a stored version: %w", w.PrevVersion, taskstore.ErrConcurrentModification)
	}
	switch w.Kind {
	case KindTask, KindStatus, KindArtifact, KindMessage, KindMetadata, KindInputRequest, KindInputResponse, KindResult:
	default:
		return fmt.Errorf("store: event kind %q", w.Kind)
	}
	if w.State != "" && !w.State.Valid() {
		return fmt.Errorf("store: state %q", w.State)
	}
	if w.Snapshot == nil {
		return errors.New("store: a write needs the projection after the event")
	}
	if len(w.Payload) == 0 && w.PayloadRef == "" {
		return errors.New("store: a write needs a payload or a payload_ref")
	}
	if w.Timeout < 0 || w.Retention < 0 {
		return errors.New("store: negative timeout or retention")
	}
	return nil
}

// Appended is what Append returns: the new seq (= version) and the stored deadline, which the workflow
// arms its task.timeout timer from (spec §14.1).
type Appended struct {
	Seq      int64
	Deadline *time.Time
	// Delivered is true when this write set the delivered_at of its Deliver work item (D81): false when it
	// carried none, or the item already had one.
	Delivered bool
}

// appendCTEs is the one write of spec §5.2 up to its result: the versioned UPDATE with the terminal rule,
// the caller scope and the dispatch fence, and the task_events INSERT at seq = the new version. The fence's
// attempts row is read FOR SHARE, so a BeginDispatch that writes a newer dispatch_id waits for an append
// already past the check, and an append that starts after it sees the new value.
const appendCTEs = `
with u as (
	update kontra.tasks t set
		snapshot = $6::json,
		state = coalesce($4::text, t.state),
		status_at = coalesce($5::timestamptz, case when $3::text in ('status', 'task') then now() else t.status_at end),
		version = t.version + 1,
		updated_at = now(),
		submitted_at = case when $4::text = 'submitted' and t.submitted_at is null then now() else t.submitted_at end,
		deadline = case when $4::text = 'submitted' and t.deadline is null
			then now() + coalesce($10::bigint * interval '1 microsecond',
				(select s.timeout_s * interval '1 second' from kontra.services s where s.tenant_id = t.tenant_id and s.name = t.service))
			else t.deadline end,
		terminal_at = case when $4::text in ('completed', 'failed', 'canceled', 'rejected') and t.terminal_at is null
			then now() else t.terminal_at end,
		retention_until = case when $4::text in ('completed', 'failed', 'canceled', 'rejected') and t.retention_until is null
			then now() + coalesce($11::bigint * interval '1 microsecond',
				(select s.retention_s * interval '1 second' from kontra.services s where s.tenant_id = t.tenant_id and s.name = t.service))
			else t.retention_until end
	where t.id = $1 and t.version = $2
	  and ($12::uuid is null or (t.tenant_id = $12::uuid and t.service = $13::text and t.buyer_id = $14::uuid))
	  and (t.state not in ('completed', 'failed', 'canceled', 'rejected')
	       or ($3::text in ('metadata', 'message') and coalesce($4::text, t.state) = t.state and $9::bool))
	  and ($15::integer is null or exists (
		select 1 from kontra.attempts a
		 where a.task_id = t.id and a.n = $15::integer and a.ended_at is null and a.dispatch_id = $16::uuid
		   and not exists (select 1 from kontra.attempts b where b.task_id = a.task_id and b.n > a.n)
		   for share of a))
	returning t.id, t.version, t.deadline
), e as (
	insert into kontra.task_events (task_id, seq, kind, payload, payload_ref)
	select u.id, u.version, $3::text, $7::json, $8::text from u
	returning task_id, seq`

// appendSQL is the one write of spec §5.2 as a single statement: appendCTEs and pg_notify. Zero rows is a
// refusal, classified by classifySQL.
const appendSQL = appendCTEs + appendSelect

// appendSelect ends an append statement that records no delivery.
const appendSelect = `)
select e.seq, u.deadline, pg_notify('kontra_task_events', e.task_id::text || ':' || e.seq::text), false
  from e join u on u.id = e.task_id`

// blobsCTE writes the blobs rows of the large parts an event names (spec §10.4, D167) in the event's own
// statement, so a row exists exactly when its event commits; it writes nothing when the UPDATE was refused. The
// rows' arrays are the statement's last three parameters, numbered from $%d.
const blobsCTE = `), b as (
	insert into kontra.blobs (ref, tenant_id, task_id, size)
	select x.ref, x.tenant_id, u.id, x.size from u, unnest($%d::text[], $%d::uuid[], $%d::bigint[]) as x(ref, tenant_id, size)
	on conflict (ref) do nothing
	returning ref`

// appendBlobs adds blobsCTE to an append statement, before its final select, binding the rows' arrays after
// args.
func appendBlobs(stmt string, args []any, rows []blob.Row) (string, []any) {
	refs, tenants, _, sizes := blob.Columns(rows)
	n := len(args) + 1
	cte := fmt.Sprintf(blobsCTE, n, n+1, n+2)
	i := strings.LastIndex(stmt, ")\nselect ")
	return stmt[:i] + cte + stmt[i:], append(args, refs, tenants, sizes)
}

// callsCTE writes the calls row of an MCP task's terminal event (Write.Call) in the event's own statement; it
// writes nothing when the UPDATE was refused. Its parameters are numbered from $%d: the row's id, the tool, the
// session hash, bytes in, bytes out and is_error.
const callsCTE = `), c as (
	insert into kontra.calls (id, tenant_id, service, tool, buyer_id, principal, mcp_session, task_id, bytes_in,
		bytes_out, duration_ms, is_error)
	select $%[1]d::uuid, t.tenant_id, t.service, $%[2]d::text, t.buyer_id, t.principal, $%[3]d::bytea, u.id,
		$%[4]d::bigint, $%[5]d::bigint,
		least(greatest((extract(epoch from (now() - t.created_at)) * 1000)::bigint, 0), 2147483647)::integer, $%[6]d::bool
	  from u join kontra.tasks t on t.id = u.id
	returning id`

// appendCall adds callsCTE to an append statement, before its final select.
func appendCall(stmt string, args []any, c *Call) (string, []any, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", nil, err
	}
	tool := c.Tool
	if r := []rune(tool); len(r) > MaxCallTool {
		tool = string(r[:MaxCallTool])
	}
	if tool == "" {
		tool = "unknown"
	}
	n := len(args) + 1
	cte := fmt.Sprintf(callsCTE, n, n+1, n+2, n+3, n+4, n+5)
	i := strings.LastIndex(stmt, ")\nselect ")
	return stmt[:i] + cte + stmt[i:], append(args, id, tool, c.MCPSession, max(c.BytesIn, 0), max(c.BytesOut, 0), c.IsError), nil
}

// appendDeliverSQL is appendSQL for a write that also records a follow-up's delivery (D81), in the same
// statement: delivered_at of work item $17 is set from the tasks row the UPDATE wrote, so a refused (fenced,
// stale, terminal) write sets nothing. Only a write carrying a delivery pays for the extra UPDATE.
const appendDeliverSQL = appendCTEs + `), d as (
	update kontra.work_items w set delivered_at = now()
	  from u
	 where w.id = $17::uuid and w.task_id = u.id and w.delivered_at is null
	returning w.id
)
select e.seq, u.deadline, pg_notify('kontra_task_events', e.task_id::text || ':' || e.seq::text), exists (select 1 from d)
  from e join u on u.id = e.task_id`

// classifySQL tells why appendSQL wrote nothing: no such task (or outside the scope), a fence that no
// longer holds, or else the version moved or the terminal rule refused the write.
const classifySQL = `
select t.version, t.state,
       ($2::uuid is null or (t.tenant_id = $2::uuid and t.service = $3::text and t.buyer_id = $4::uuid)),
       ($5::integer is null or exists (
		select 1 from kontra.attempts a
		 where a.task_id = t.id and a.n = $5::integer and a.ended_at is null and a.dispatch_id = $6::uuid
		   and not exists (select 1 from kontra.attempts b where b.task_id = a.task_id and b.n > a.n)))
  from kontra.tasks t where t.id = $1`

func scopeArgs(s *Scope) (tenant, service, buyer any) {
	if s == nil {
		return nil, nil, nil
	}
	return s.TenantID, s.Service, s.BuyerID
}

func fenceArgs(f *Fence) (n, dispatch any) {
	if f == nil {
		return nil, nil
	}
	return f.N, f.DispatchID
}

// Append runs the one transaction of spec §5.2 for w, in tx when tx is not nil (the caller commits; the
// NOTIFY is delivered at its commit). Zero rows is classified: a task that does not exist or is outside
// w.Scope is a2a.ErrTaskNotFound; a fence that no longer holds is ErrFenced (and the fence records it);
// otherwise the version moved or the terminal rule refused the write, taskstore.ErrConcurrentModification.
// A stale version is never merged. Payload is stored verbatim (json, D49).
//
// A write that carries a follow-up's delivery (w.Deliver, or the delivery w.Fence has armed) sets the work
// item's delivered_at in the same statement, so it commits exactly when the event does (D81). Once a write
// carrying the fence's armed delivery succeeds on its own (tx nil), the delivery is disarmed; in a caller's
// transaction it stays armed, since only the caller knows whether that transaction commits.
//
// The blobs rows pending on ctx (blob.Pending) for the large parts w.Payload names are written in the same
// statement, and only when the event is (spec §10.4, D167).
func (l *Log) Append(ctx context.Context, tx pgx.Tx, w Write) (Appended, error) {
	if err := w.validate(); err != nil {
		return Appended{}, err
	}
	if w.Fence != nil && w.Fence.TaskID != w.TaskID {
		// A dispatch only ever writes its own task; anything else is a bug that must not reach the log.
		w.Fence.markFenced()
		return Appended{}, fmt.Errorf("store: a dispatch of task %s wrote task %s: %w", w.Fence.TaskID, w.TaskID, ErrFenced)
	}
	snap, err := json.Marshal(w.Snapshot)
	if err != nil {
		return Appended{}, fmt.Errorf("store: encode snapshot: %w", err)
	}
	var payload any
	if len(w.Payload) > 0 {
		payload = []byte(w.Payload)
	}
	var state any
	if w.State != "" {
		state = string(w.State)
	}
	st, ss, sb := scopeArgs(w.Scope)
	fn, fd := fenceArgs(w.Fence)
	args := []any{
		w.TaskID, w.PrevVersion, string(w.Kind), state, nullTime(w.StatusAt), snap, payload,
		nullString(w.PayloadRef), w.AfterTerminalOK, micros(w.Timeout), micros(w.Retention),
		st, ss, sb, fn, fd,
	}
	stmt := l.appendStmt
	var armed *uuid.UUID
	if w.Deliver == uuid.Nil {
		armed, w.Deliver = w.Fence.armedDelivery()
	}
	if w.Deliver != uuid.Nil {
		stmt, args = appendDeliverSQL, append(args, w.Deliver)
	}
	if rows := blob.PendingRows(ctx, w.TaskID, w.Payload); len(rows) > 0 {
		stmt, args = appendBlobs(stmt, args, rows)
	}
	if w.Call != nil {
		if !w.State.Terminal() {
			return Appended{}, errors.New("store: a calls row is written with a terminal event only")
		}
		if stmt, args, err = appendCall(stmt, args, w.Call); err != nil {
			return Appended{}, err
		}
	}

	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	var out Appended
	err = l.writeRow(ctx, tx, stmt, args, &out.Seq, &out.Deadline, nil, &out.Delivered)
	if err == nil {
		if tx == nil {
			w.Fence.deliveryWritten(armed)
		}
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Appended{}, fmt.Errorf("store: append to task %s: %w", w.TaskID, err)
	}
	return Appended{}, l.classify(ctx, l.on(tx), w)
}

func (l *Log) classify(ctx context.Context, q querier, w Write) error {
	st, ss, sb := scopeArgs(w.Scope)
	fn, fd := fenceArgs(w.Fence)
	var (
		version             int64
		state               State
		inScope, fenceHolds bool
	)
	err := q.QueryRow(ctx, classifySQL, w.TaskID, st, ss, sb, fn, fd).Scan(&version, &state, &inScope, &fenceHolds)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return notFound()
	case err != nil:
		return fmt.Errorf("store: classify a refused write to task %s: %w", w.TaskID, err)
	case !inScope:
		return notFound()
	case !fenceHolds:
		w.Fence.markFenced()
		return fmt.Errorf("store: task %s attempt %d: %w", w.TaskID, w.Fence.N, ErrFenced)
	case version != w.PrevVersion:
		return fmt.Errorf("store: task %s is at version %d, the write expected %d: %w", w.TaskID, version, w.PrevVersion, taskstore.ErrConcurrentModification)
	default:
		return fmt.Errorf("store: task %s is %s; a %s write is refused after the terminal event: %w", w.TaskID, state, w.Kind, taskstore.ErrConcurrentModification)
	}
}

// NewTask is a gate task as the work queue pre-creates it (spec §15): gate ids, `submitted` (or `quoted`),
// the caller's first message in history.
type NewTask struct {
	ID             uuid.UUID
	Service        ServiceRef
	Protocol       Protocol
	Owner          Owner
	ContextID      string
	State          State // submitted or quoted (any state through Store.Create)
	Snapshot       *a2a.Task
	FirstMessageID string   // empty: no resend key
	FirstRequest   [32]byte // SHA-256 of the request, for resend detection (spec §15, RequestHash)
	QuoteExpiresAt *time.Time
	// Payload is the `task` row's payload when it is not the A2A StreamResponse of Snapshot: an MCP task's
	// (spec §11.9, NewMCPTask).
	Payload json.RawMessage
}

func (t *NewTask) validate() error {
	switch {
	case t.ID == uuid.Nil:
		return errors.New("store: a new task needs an id")
	case t.Snapshot == nil:
		return errors.New("store: a new task needs its snapshot")
	case t.Snapshot.ID != a2a.TaskID(t.ID.String()):
		return fmt.Errorf("store: snapshot id %q is not the task id %s", t.Snapshot.ID, t.ID)
	case t.Snapshot.ContextID != t.ContextID:
		return fmt.Errorf("store: snapshot context id %q is not the task's %q", t.Snapshot.ContextID, t.ContextID)
	case t.Protocol != ProtocolA2A && t.Protocol != ProtocolMCP:
		return fmt.Errorf("store: protocol %q", t.Protocol)
	case !t.State.Valid():
		return fmt.Errorf("store: state %q", t.State)
	case t.State == StateQuoted && t.QuoteExpiresAt == nil:
		return errors.New("store: a quoted task needs its quote expiry")
	case t.Owner.Principal == "" || !validVia(t.Owner.Via):
		return fmt.Errorf("store: owner principal %q via %q", t.Owner.Principal, t.Owner.Via)
	case t.Service.TenantID == uuid.Nil || t.Service.Name == "":
		return errors.New("store: a new task needs its service")
	}
	// Postgres would refuse these, a 500 to the caller; a caller-supplied value is InvalidParams (spec §4:
	// preserve or reject). The columns' CHECKs count characters, as Postgres length() does, not bytes.
	if !storableText(t.ContextID) {
		return fmt.Errorf("store: contextId must be UTF-8 text without U+0000: %w", a2a.ErrInvalidParams)
	}
	if n := utf8.RuneCountInString(t.ContextID); n < 1 || n > 256 {
		return fmt.Errorf("store: contextId must be 1 to 256 characters, got %d: %w", n, a2a.ErrInvalidParams)
	}
	if !storableText(t.FirstMessageID) {
		return fmt.Errorf("store: messageId must be UTF-8 text without U+0000: %w", a2a.ErrInvalidParams)
	}
	if n := utf8.RuneCountInString(t.FirstMessageID); n > 256 {
		return fmt.Errorf("store: messageId must be at most 256 characters, got %d: %w", n, a2a.ErrInvalidParams)
	}
	return nil
}

// storableText reports whether s can be a Postgres text value: valid UTF-8 without U+0000. The server
// refuses anything else (SQLSTATE 22021), in a column and in a query parameter alike; only json columns
// keep a NUL, as the escape \u0000 (D49).
func storableText(s string) bool { return utf8.ValidString(s) && strings.IndexByte(s, 0) < 0 }

// createSQL inserts the task at version 1 and its `task` row at seq 1, and notifies, in one statement.
// Every state but quoted counts as submitted at creation: submitted_at and the deadline are fixed then.
const createSQL = `
with t as (
	insert into kontra.tasks (id, tenant_id, service, protocol, buyer_id, principal, via, context_id,
		first_message_id, first_request, state, version, snapshot, status_at, quote_expires_at,
		deadline, submitted_at, terminal_at, retention_until)
	select $1::uuid, s.tenant_id, s.name, $4::text, $5::uuid, $6::text, $7::text, $8::text, $9::text, $10::bytea,
		$11::text, 1, $12::json, coalesce($13::timestamptz, now()), $14::timestamptz,
		case when $11::text <> 'quoted' then now() + coalesce($15::bigint * interval '1 microsecond', s.timeout_s * interval '1 second') end,
		case when $11::text <> 'quoted' then now() end,
		case when $11::text in ('completed', 'failed', 'canceled', 'rejected') then now() end,
		case when $11::text in ('completed', 'failed', 'canceled', 'rejected')
			then now() + coalesce($16::bigint * interval '1 microsecond', s.retention_s * interval '1 second') end
	  from kontra.services s where s.tenant_id = $2::uuid and s.name = $3::text
	returning id, deadline
), e as (
	insert into kontra.task_events (task_id, seq, kind, payload)
	select t.id, 1, 'task', $17::json from t
	returning task_id, seq
)
select e.seq, t.deadline, pg_notify('kontra_task_events', e.task_id::text || ':' || e.seq::text)
  from e join t on t.id = e.task_id`

// createBlobsSQL is createSQL that also writes the blobs rows of the large parts of the caller's first message
// (spec §10.4, D167), in the statement that creates the task; the rows' arrays are $18 to $20.
var createBlobsSQL = strings.Replace(createSQL, ")\nselect e.seq", `), b as (
	insert into kontra.blobs (ref, tenant_id, task_id, size)
	select x.ref, x.tenant_id, t.id, x.size from t, unnest($18::text[], $19::uuid[], $20::bigint[]) as x(ref, tenant_id, size)
	on conflict (ref) do nothing
	returning ref
)
select e.seq`, 1)

// CreateTask inserts tasks at version 1 and task_events seq 1 (kind task) in tx (or on its own when tx is
// nil) and notifies. A duplicate id is taskstore.ErrTaskAlreadyExists; a contextId outside 1..256
// characters, a messageId over 256, or either one not UTF-8 text without U+0000, is a2a.ErrInvalidParams;
// a service that does not exist is an error. The blobs rows pending on ctx for the large parts of the first
// message are written in the same statement (spec §10.4, D167).
func (l *Log) CreateTask(ctx context.Context, tx pgx.Tx, t NewTask) (Appended, error) {
	if err := t.validate(); err != nil {
		return Appended{}, err
	}
	snap, err := json.Marshal(t.Snapshot)
	if err != nil {
		return Appended{}, fmt.Errorf("store: encode snapshot: %w", err)
	}
	payload := []byte(t.Payload)
	switch {
	case t.Protocol == ProtocolMCP && len(payload) == 0:
		return Appended{}, errors.New("store: an MCP task's task row needs its MCP payload (NewMCPTask)")
	case len(payload) == 0:
		if payload, err = json.Marshal(a2a.StreamResponse{Event: t.Snapshot}); err != nil {
			return Appended{}, fmt.Errorf("store: encode the task row: %w", err)
		}
	}
	var firstMsg, firstReq any
	if t.FirstMessageID != "" {
		firstMsg, firstReq = t.FirstMessageID, t.FirstRequest[:]
	}
	args := []any{
		t.ID, t.Service.TenantID, t.Service.Name, string(t.Protocol), t.Owner.BuyerID, t.Owner.Principal,
		t.Owner.Via, t.ContextID, firstMsg, firstReq, string(t.State), snap,
		nullTime(StatusTime(t.Snapshot.Status.Timestamp)), t.QuoteExpiresAt,
		micros(t.Service.Timeout), micros(t.Service.Retention), payload,
	}
	stmt := createSQL
	if rows := blob.PendingRows(ctx, t.ID, payload); len(rows) > 0 {
		refs, tenants, _, sizes := blob.Columns(rows)
		stmt, args = createBlobsSQL, append(args, refs, tenants, sizes)
	}
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	var out Appended
	err = l.writeRow(ctx, tx, stmt, args, &out.Seq, &out.Deadline, nil)
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return out, nil
	case errors.Is(err, pgx.ErrNoRows):
		return Appended{}, fmt.Errorf("store: service %s/%s does not exist", t.Service.TenantID, t.Service.Name)
	case errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "tasks_pkey":
		return Appended{}, fmt.Errorf("store: task %s: %w", t.ID, taskstore.ErrTaskAlreadyExists)
	default:
		return Appended{}, fmt.Errorf("store: create task %s: %w", t.ID, err)
	}
}

// taskCols are the columns scanTask reads, from kontra.tasks aliased t.
const taskCols = `t.id, t.tenant_id, t.service, t.protocol, t.buyer_id, t.principal, t.via, t.context_id,
	coalesce(t.first_message_id, ''), t.state, t.version, t.snapshot, t.status_at, t.quote_expires_at, t.deadline,
	t.created_at, t.submitted_at, t.terminal_at, t.retention_until`

func scanTask(row pgx.Row) (*TaskRow, error) {
	var (
		r    TaskRow
		snap []byte
	)
	err := row.Scan(&r.ID, &r.TenantID, &r.Service, &r.Protocol, &r.Owner.BuyerID, &r.Owner.Principal,
		&r.Owner.Via, &r.ContextID, &r.FirstMessageID, &r.State, &r.Version, &snap, &r.StatusAt,
		&r.QuoteExpiresAt, &r.Deadline, &r.CreatedAt, &r.SubmittedAt, &r.TerminalAt, &r.RetentionUntil)
	if err != nil {
		return nil, err
	}
	if r.Snapshot, err = decodeTask(snap); err != nil {
		return nil, fmt.Errorf("store: task %s snapshot: %w", r.ID, err)
	}
	return &r, nil
}

func decodeTask(b []byte) (*a2a.Task, error) {
	var t a2a.Task
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// Load returns the task row without caller scope; an unknown task is a2a.ErrTaskNotFound.
func (l *Log) Load(ctx context.Context, taskID uuid.UUID) (*TaskRow, error) {
	return l.load(ctx, l.db, taskID, "")
}

func (l *Log) load(ctx context.Context, q querier, taskID uuid.UUID, lock string) (*TaskRow, error) {
	r, err := scanTask(q.QueryRow(ctx, `select `+taskCols+` from kontra.tasks t where t.id = $1`+lock, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, fmt.Errorf("store: load task %s: %w", taskID, err)
	}
	return r, nil
}

// LoadService returns the ServiceRef of a services row, with the policy its tasks are created and
// dispatched with (timeout, retries, retention): what a dispatch needs when all it has is a task row's
// tenant and service. A service that does not exist is an error; a disabled one is returned as it is.
func (l *Log) LoadService(ctx context.Context, tenantID uuid.UUID, name string) (ServiceRef, error) {
	ref := ServiceRef{TenantID: tenantID, Name: name}
	var timeoutS, retentionS int64
	err := l.db.QueryRow(ctx, `select t.slug, s.kind, s.timeout_s, s.retries, s.retention_s, s.sealed
		from kontra.services s join kontra.tenants t on t.id = s.tenant_id
		where s.tenant_id = $1 and s.name = $2`, tenantID, name).Scan(&ref.Tenant, &ref.Kind, &timeoutS, &ref.Retries, &retentionS, &ref.Sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServiceRef{}, fmt.Errorf("store: service %s/%s does not exist", tenantID, name)
	}
	if err != nil {
		return ServiceRef{}, fmt.Errorf("store: load service %s/%s: %w", tenantID, name, err)
	}
	ref.Timeout, ref.Retention = time.Duration(timeoutS)*time.Second, time.Duration(retentionS)*time.Second
	ref.Sealed = SealedRef(ref.Sealed)
	return ref, nil
}

// LoadScoped returns the task row if it belongs to the caller's scope, else an error wrapping
// a2a.ErrTaskNotFound. The SSE writer and push read through it.
func (l *Log) LoadScoped(ctx context.Context, svc ServiceRef, o Owner, taskID uuid.UUID) (*TaskRow, error) {
	r, err := scanTask(l.db.QueryRow(ctx, `select `+taskCols+` from kontra.tasks t
		where t.id = $1 and t.tenant_id = $2 and t.service = $3 and t.buyer_id = $4`,
		taskID, svc.TenantID, svc.Name, o.BuyerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, fmt.Errorf("store: load task %s: %w", taskID, err)
	}
	return r, nil
}

// FindResend returns the task a resend is answered with: same tenant, service, buyer and principal,
// the same first messageId and request hash, created after since (24 h, spec §15). Nil when none. A
// messageId that is not UTF-8 text without U+0000 is a2a.ErrInvalidParams, before any query: CreateTask
// refuses it too, and Postgres would refuse the query (a 500 to the caller).
func (l *Log) FindResend(ctx context.Context, svc ServiceRef, o Owner, messageID string, requestHash [32]byte, since time.Time) (*TaskRow, error) {
	if messageID == "" {
		return nil, nil
	}
	if !storableText(messageID) {
		return nil, fmt.Errorf("store: messageId must be UTF-8 text without U+0000: %w", a2a.ErrInvalidParams)
	}
	return l.findResend(ctx, l.db, svc, o, messageID, requestHash, since)
}

// FindResendLocked is FindResend inside tx, the transaction that creates the task, after taking the
// transaction-scoped advisory lock of the resend key (tenant, service, buyer, principal, messageId). Of two
// identical requests arriving together, which both miss FindResend, the second waits here until the first's
// transaction ends and then finds its task, so the request is not made into two tasks and dispatched twice
// (spec §15). The lock is held until tx ends.
func (l *Log) FindResendLocked(ctx context.Context, tx pgx.Tx, svc ServiceRef, o Owner, messageID string, requestHash [32]byte, since time.Time) (*TaskRow, error) {
	if messageID == "" {
		return nil, nil
	}
	if !storableText(messageID) {
		return nil, fmt.Errorf("store: messageId must be UTF-8 text without U+0000: %w", a2a.ErrInvalidParams)
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, resendLockKey(svc, o, messageID)); err != nil {
		return nil, fmt.Errorf("store: lock the resend key: %w", err)
	}
	return l.findResend(ctx, tx, svc, o, messageID, requestHash, since)
}

// resendLockKey is the advisory lock key of a resend key: the first 8 bytes of a SHA-256 over its parts,
// under a prefix of its own. Two keys that collide only wait for each other.
func resendLockKey(svc ServiceRef, o Owner, messageID string) int64 {
	h := sha256.New()
	for _, part := range []string{"kontra.resend", svc.TenantID.String(), svc.Name, o.BuyerID.String(), o.Principal, messageID} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func (l *Log) findResend(ctx context.Context, q querier, svc ServiceRef, o Owner, messageID string, requestHash [32]byte, since time.Time) (*TaskRow, error) {
	r, err := scanTask(q.QueryRow(ctx, `select `+taskCols+` from kontra.tasks t
		where t.tenant_id = $1 and t.service = $2 and t.buyer_id = $3 and t.principal = $4
		  and t.first_message_id = $5 and t.first_request = $6 and t.created_at > $7
		order by t.created_at desc, t.id desc limit 1`,
		svc.TenantID, svc.Name, o.BuyerID, o.Principal, messageID, requestHash[:], since))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: find resend: %w", err)
	}
	return r, nil
}

type requestHashKey struct{}

// WithRequestHash attaches the hash of a request as the caller sent it (RequestHash, after §2.5's stripping),
// before the relay replaced its large parts by kontra-blob: refs of its own (spec §10.4): a resend of the request
// is given other refs, so only this hash recognizes it (RequestHashOf).
func WithRequestHash(ctx context.Context, h [32]byte) context.Context {
	return context.WithValue(ctx, requestHashKey{}, h)
}

// RequestHashOf is the hash attached to ctx by WithRequestHash, or else RequestHash(req): tasks.first_request,
// and the key of a resend (spec §15).
func RequestHashOf(ctx context.Context, req *a2a.SendMessageRequest) ([32]byte, error) {
	if h, ok := ctx.Value(requestHashKey{}).([32]byte); ok {
		return h, nil
	}
	return RequestHash(req)
}

// RequestHash is tasks.first_request for an A2A task (spec §15, D74): SHA-256 of Go's json.Marshal of the
// typed request, after the caller-key stripping of §2.5 (the caller strips first). The whole request is
// hashed, any inline push config included, so a resend that differs in anything is a new task.
func RequestHash(req *a2a.SendMessageRequest) ([32]byte, error) {
	if req == nil {
		return [32]byte{}, errors.New("store: no request to hash")
	}
	b, err := json.Marshal(req)
	if err != nil {
		return [32]byte{}, fmt.Errorf("store: encode request: %w", err)
	}
	return sha256.Sum256(b), nil
}

// Events returns up to limit rows (1..1000) with seq > after, in seq order.
func (l *Log) Events(ctx context.Context, taskID uuid.UUID, after int64, limit int) ([]Event, error) {
	return l.events(ctx, l.db, taskID, after, limit)
}

func (l *Log) events(ctx context.Context, q querier, taskID uuid.UUID, after int64, limit int) ([]Event, error) {
	if limit < 1 || limit > 1000 {
		limit = 1000
	}
	rows, err := q.Query(ctx, `select seq, kind, payload, coalesce(payload_ref, ''), created_at
		from kontra.task_events where task_id = $1 and seq > $2 order by seq limit $3`, taskID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: events of task %s: %w", taskID, err)
	}
	evs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		ev := Event{TaskID: taskID}
		var payload []byte
		if err := r.Scan(&ev.Seq, &ev.Kind, &payload, &ev.PayloadRef, &ev.CreatedAt); err != nil {
			return Event{}, err
		}
		if payload != nil {
			ev.Payload = payload
		}
		return ev, nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: events of task %s: %w", taskID, err)
	}
	return evs, nil
}

// EventsWithin is Events bounded by bytes as well as rows, for a reader that holds a read while a slow
// caller takes it (the gate's SSE writer): rows with seq > after in seq order, at most limit (1..1000) of
// them, ending with the row that brings their payload bytes to maxBytes or more, and always at least one
// row when there is one. Postgres is asked only for the rows whose stored payloads (pg_column_size, which
// reads no TOAST) before them stay under maxBytes, so a read of large incompressible rows transfers about
// the budget; the bytes are counted again as they arrive, so a compressible payload, whose stored size is
// a fraction of its bytes, cannot make a read hold many times the budget. more is true when rows after
// the last one returned may exist: the read stopped at limit, at the budget, or where the stored sizes
// reached it.
func (l *Log) EventsWithin(ctx context.Context, taskID uuid.UUID, after int64, limit int, maxBytes int) (evs []Event, more bool, err error) {
	if limit < 1 || limit > 1000 {
		limit = 1000
	}
	maxBytes = max(maxBytes, 1)
	rows, err := l.db.Query(ctx, `select seq, kind, payload, coalesce(payload_ref, ''), created_at, stored from (
			select seq, kind, payload, payload_ref, created_at, coalesce(pg_column_size(payload), 0)::bigint as stored,
				sum(coalesce(pg_column_size(payload), 0)) over (order by seq) - coalesce(pg_column_size(payload), 0) as before
			from kontra.task_events where task_id = $1 and seq > $2 order by seq limit $3) e
		where before < $4 order by seq`, taskID, after, limit, int64(maxBytes))
	if err != nil {
		return nil, false, fmt.Errorf("store: events of task %s: %w", taskID, err)
	}
	defer rows.Close()
	var read, stored int64
	for read < int64(maxBytes) && rows.Next() {
		ev := Event{TaskID: taskID}
		var (
			payload []byte
			size    int64
		)
		if err := rows.Scan(&ev.Seq, &ev.Kind, &payload, &ev.PayloadRef, &ev.CreatedAt, &size); err != nil {
			return nil, false, fmt.Errorf("store: events of task %s: %w", taskID, err)
		}
		if payload != nil {
			ev.Payload = payload
		}
		read += int64(len(payload))
		stored += size
		evs = append(evs, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: events of task %s: %w", taskID, err)
	}
	more = len(evs) == limit || read >= int64(maxBytes) || stored >= int64(maxBytes)
	return evs, more, nil
}

// Tail returns max(seq) for the task, 0 when it has no rows yet.
func (l *Log) Tail(ctx context.Context, taskID uuid.UUID) (int64, error) {
	var tail int64
	if err := l.db.QueryRow(ctx, `select coalesce(max(seq), 0) from kontra.task_events where task_id = $1`, taskID).Scan(&tail); err != nil {
		return 0, fmt.Errorf("store: tail of task %s: %w", taskID, err)
	}
	return tail, nil
}

// TerminalSeq returns the seq of the task's terminal row, 0 while it is not terminal. After the terminal
// row only message and metadata rows are ever written (spec §5.1), so it is the last task, status or
// artifact row of a terminal task.
func (l *Log) TerminalSeq(ctx context.Context, taskID uuid.UUID) (int64, error) {
	return terminalSeq(ctx, l.db, taskID)
}

func terminalSeq(ctx context.Context, q querier, taskID uuid.UUID) (int64, error) {
	var seq int64
	err := q.QueryRow(ctx, `select coalesce(max(e.seq), 0) from kontra.task_events e
		join kontra.tasks t on t.id = e.task_id
		where e.task_id = $1 and t.terminal_at is not null and e.kind in ('task', 'status', 'artifact')`, taskID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("store: terminal seq of task %s: %w", taskID, err)
	}
	return seq, nil
}
