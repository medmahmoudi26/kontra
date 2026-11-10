package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// The tasks of the gate's own /mcp (spec/relay.md §12; M4 WP-04). hire creates an ordinary A2A gate task through
// the service's work queue and marks it tasks.hired (D272); /mcp's MCP Tasks see only those, of the principal's
// buyer, across services. tasks.elicitation_* is no longer read or written (D351: no URL elicitation on /mcp).

// MarkHired marks a task hire created (tasks.hired), in tx: the transaction that creates it.
func (l *Log) MarkHired(ctx context.Context, tx pgx.Tx, taskID uuid.UUID) error {
	tag, err := l.on(tx).Exec(ctx, `update kontra.tasks set hired = true where id = $1`, taskID)
	if err != nil {
		return fmt.Errorf("store: mark task %s hired: %w", taskID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: mark task %s hired: %w", taskID, notFound())
	}
	return nil
}

// LoadHired returns a hire task of the buyer, whatever its service: an unknown task, an unhired one and another
// buyer's are all a2a.ErrTaskNotFound (§12.2: -32602, as an unknown id).
func (l *Log) LoadHired(ctx context.Context, buyer, taskID uuid.UUID) (*TaskRow, error) {
	r, err := scanTask(l.db.QueryRow(ctx, `select `+taskCols+` from kontra.tasks t
		where t.id = $1 and t.buyer_id = $2 and t.hired`, taskID, buyer))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, fmt.Errorf("store: load hire task %s: %w", taskID, err)
	}
	return r, nil
}

// LoadHiredAsked is LoadHired with the row's StatusSeq, read in the same statement (one snapshot: an event and the
// version it sets are one write, §5.2): the seq of the task or status event the row's state and status message come
// from. For a task at input-required it is the event that asked the question the row carries, so a gate id
// k.<taskId>.<seq> built from it names that question and no later one (§12.4; the tasks extension: a key MUST NOT name
// two distinct requests).
func (l *Log) LoadHiredAsked(ctx context.Context, buyer, taskID uuid.UUID) (*TaskRow, int64, error) {
	var asked int64
	r, err := scanTask(scanAlso{l.db.QueryRow(ctx, `select `+taskCols+`,
			(select coalesce(max(e.seq), 0) from kontra.task_events e where e.task_id = t.id and e.kind in ('task', 'status'))
		from kontra.tasks t where t.id = $1 and t.buyer_id = $2 and t.hired`, taskID, buyer), []any{&asked}})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, notFound()
	}
	if err != nil {
		return nil, 0, fmt.Errorf("store: load hire task %s: %w", taskID, err)
	}
	return r, asked, nil
}

// scanAlso is a row whose Scan fills also after the destinations it is given (columns selected after taskCols).
type scanAlso struct {
	pgx.Row
	also []any
}

func (s scanAlso) Scan(dest ...any) error { return s.Row.Scan(append(dest, s.also...)...) }

// HiredPageSize is the most tasks one page of a buyer's hire tasks holds (§12.2's tasks/list).
const HiredPageSize = 50

// hiredKeyset is the keyset position a page of a buyer's hire tasks starts after: the cursor's, else one above every
// task (status_at infinity, the greatest uuid). Always bound, never null: under the generic plan Postgres may switch a
// prepared statement to after five executions, a "$2 is null or (status_at, id) < ($2, $3)" predicate cannot be an
// index condition, so a deep cursor would read every row above it (D333's bound on the rows one request reads).
func hiredKeyset(cursor string) (pgtype.Timestamptz, uuid.UUID, error) {
	if cursor == "" {
		return pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}, uuid.Max, nil
	}
	t, id, err := DecodePageToken(cursor)
	if err != nil {
		return pgtype.Timestamptz{}, uuid.Nil, ErrBadCursor
	}
	return pgtype.Timestamptz{Time: t, Valid: true}, id, nil
}

// ListHired returns one page of the buyer's hire tasks across services, newest first by status_at (then id; the
// tasks_hired index), after cursor ("" for the first page; a ListTasks page token, EncodePageToken), and the next
// page's cursor ("" when it is the last). A cursor the gate did not mint is ErrBadCursor.
func (l *Log) ListHired(ctx context.Context, buyer uuid.UUID, cursor string) ([]*TaskRow, string, error) {
	at, after, err := hiredKeyset(cursor)
	if err != nil {
		return nil, "", err
	}
	rows, err := l.db.Query(ctx, `select `+taskCols+` from kontra.tasks t
		where t.buyer_id = $1 and t.hired and (t.status_at, t.id) < ($2::timestamptz, $3::uuid)
		order by t.status_at desc, t.id desc limit $4`, buyer, at, after, HiredPageSize+1)
	if err != nil {
		return nil, "", fmt.Errorf("store: list hire tasks: %w", err)
	}
	defer rows.Close()
	var out []*TaskRow
	for rows.Next() {
		r, err := scanTask(rows)
		if err != nil {
			return nil, "", fmt.Errorf("store: list hire tasks: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: list hire tasks: %w", err)
	}
	next := ""
	if len(out) > HiredPageSize {
		out = out[:HiredPageSize]
		last := out[len(out)-1]
		next = EncodePageToken(last.StatusAt, last.ID)
	}
	return out, next, nil
}

// StatusSeq is the seq of the task's last task or status row: the event that set its current state (an A2A gate
// task's state changes only by those). For a task at input-required it is the event that asked, whose gate id
// k.<taskId>.<seq> names the question on /mcp (§12.4).
func (l *Log) StatusSeq(ctx context.Context, taskID uuid.UUID) (int64, error) {
	var seq int64
	err := l.db.QueryRow(ctx, `select coalesce(max(seq), 0) from kontra.task_events
		where task_id = $1 and kind in ('task', 'status')`, taskID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("store: the status row of task %s: %w", taskID, err)
	}
	return seq, nil
}

// HireKeyWindow is how long a hire's idempotencyKey answers the task it created (relay.md §12.3 Idempotency, D338):
// a hire with the same key, of the same buyer and principal, created less than this ago.
const HireKeyWindow = 24 * time.Hour

// HireKey is how a hire's idempotencyKey finds the task it created (relay.md §12.3 Idempotency, D338). Lock is the
// key's identity, the same in every period: what the advisory lock of a keyed hire is taken on, so two hires with
// one key serialize even across a period's end. MessageIDs are the A2A messageIds the key mints in the current period
// of HireKeyWindow, in the one before and in the one after (another replica's clock a little ahead); the first is the
// new hire's. A task created less than HireKeyWindow ago was created in one of those periods, so it is found; a task
// of an earlier period carries another messageId, and one of a period's messageId is never older than the window, so a
// key reused after the window never gives a seller one messageId on two tasks.
type HireKey struct {
	Lock       string
	MessageIDs []string
}

// KeyedHire is a hire task an idempotencyKey names (FindHireKeyed): the task, and the resend key its first hire had
// (tasks.first_request), which a hire with the same key and other arguments does not match.
type KeyedHire struct {
	Task         *TaskRow
	FirstRequest [32]byte
}

// FindHireKeyed returns the hire task of buyer and principal whose first_message_id is one of k's MessageIDs (the
// messageIds a hire with an idempotencyKey mints from it, relay.md §12.3) and that was created after since, whatever
// its service; nil when none. It reads the index tasks_hired_by_message (0005).
func (l *Log) FindHireKeyed(ctx context.Context, buyer uuid.UUID, principal string, k HireKey, since time.Time) (*KeyedHire, error) {
	return l.findHireKeyed(ctx, l.db, buyer, principal, k.MessageIDs, since)
}

// FindHireKeyedLocked is FindHireKeyed inside tx, the transaction that creates the task, after taking the
// transaction-scoped advisory lock of (buyer, principal, k.Lock): of two hires with one key arriving together,
// whatever their services and periods, the second waits until the first's transaction ends and then finds its task,
// so one key is never two tasks.
func (l *Log) FindHireKeyedLocked(ctx context.Context, tx pgx.Tx, buyer uuid.UUID, principal string, k HireKey, since time.Time) (*KeyedHire, error) {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, hireKeyLockKey(buyer, principal, k.Lock)); err != nil {
		return nil, fmt.Errorf("store: lock the hire's idempotency key: %w", err)
	}
	return l.findHireKeyed(ctx, tx, buyer, principal, k.MessageIDs, since)
}

// hireKeyLockKey is the advisory lock key of a hire's idempotency key, under a prefix of its own.
func hireKeyLockKey(buyer uuid.UUID, principal, lock string) int64 {
	h := sha256.New()
	for _, part := range []string{"kontra.hire.key", buyer.String(), principal, lock} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func (l *Log) findHireKeyed(ctx context.Context, q querier, buyer uuid.UUID, principal string, messageIDs []string, since time.Time) (*KeyedHire, error) {
	var fr []byte
	r, err := scanTask(scanAlso{q.QueryRow(ctx, `select `+taskCols+`, coalesce(t.first_request, ''::bytea) from kontra.tasks t
		where t.buyer_id = $1 and t.first_message_id = any($2::text[]) and t.hired and t.principal = $3 and t.created_at > $4
		order by t.created_at desc, t.id desc limit 1`, buyer, messageIDs, principal, since), []any{&fr}})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: find a keyed hire: %w", err)
	}
	k := &KeyedHire{Task: r}
	copy(k.FirstRequest[:], fr)
	return k, nil
}

// HiredFilter narrows a page of a buyer's hire tasks (ListHiredFiltered): a service (TenantID and Service, both set or
// both zero), a context id, and the states a task may be in (none: every state).
type HiredFilter struct {
	TenantID  uuid.UUID
	Service   string
	ContextID string
	States    []State
}

// HiredReadBound is the most rows of a buyer's hire tasks one ListHiredFiltered reads (relay.md §12.2's tasks tool,
// D333), as search reads at most 1000 services (D293).
const HiredReadBound = 1000

// ListHiredFiltered returns one page of the buyer's hire tasks that match f, newest first by status_at (then id), after
// cursor ("" for the first page, else a page token EncodePageToken minted), and the next page's cursor ("" when the
// buyer has no task after the page). It walks tasks_hired, reading at most HiredReadBound rows: a read that reaches the
// bound answers what matched, fewer than HiredPageSize or none, with the cursor at the last row it read. A cursor the
// gate did not mint is ErrBadCursor.
func (l *Log) ListHiredFiltered(ctx context.Context, buyer uuid.UUID, f HiredFilter, cursor string) ([]*TaskRow, string, error) {
	at, after, err := hiredKeyset(cursor)
	if err != nil {
		return nil, "", err
	}
	states := make([]string, 0, len(f.States))
	for _, s := range f.States {
		states = append(states, string(s))
	}
	var svc any
	if f.Service != "" {
		svc = f.Service
	}
	var ctxID any
	if f.ContextID != "" {
		ctxID = f.ContextID
	}
	// The light columns of at most HiredReadBound rows, in the index's order, each with whether it matches; the full
	// rows of the matches only.
	rows, err := l.db.Query(ctx, `select t.id, t.status_at,
			($5::text is null or (t.tenant_id = $6 and t.service = $5))
			and ($7::text is null or t.context_id = $7)
			and (cardinality($8::text[]) = 0 or t.state = any($8::text[]))
		from kontra.tasks t
		where t.buyer_id = $1 and t.hired and (t.status_at, t.id) < ($2::timestamptz, $3::uuid)
		order by t.status_at desc, t.id desc limit $4`, buyer, at, after, HiredReadBound, svc, f.TenantID, ctxID, states)
	if err != nil {
		return nil, "", fmt.Errorf("store: list hire tasks: %w", err)
	}
	type light struct {
		id uuid.UUID
		at time.Time
	}
	var (
		matched []light
		last    light
		read    int
		full    bool
	)
	for rows.Next() {
		var (
			r light
			m bool
		)
		if err := rows.Scan(&r.id, &r.at, &m); err != nil {
			rows.Close()
			return nil, "", fmt.Errorf("store: list hire tasks: %w", err)
		}
		if m {
			if len(matched) == HiredPageSize {
				full = true
				break
			}
			matched = append(matched, r)
		}
		read++
		last = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: list hire tasks: %w", err)
	}
	next := ""
	switch {
	case full:
		p := matched[len(matched)-1]
		next = EncodePageToken(p.at, p.id)
	case read == HiredReadBound:
		next = EncodePageToken(last.at, last.id)
	}
	if len(matched) == 0 {
		return nil, next, nil
	}
	ids := make([]uuid.UUID, len(matched))
	for i, m := range matched {
		ids[i] = m.id
	}
	frows, err := l.db.Query(ctx, `select `+taskCols+` from kontra.tasks t where t.id = any($1::uuid[])
		order by t.status_at desc, t.id desc`, ids)
	if err != nil {
		return nil, "", fmt.Errorf("store: list hire tasks: %w", err)
	}
	defer frows.Close()
	out := make([]*TaskRow, 0, len(ids))
	for frows.Next() {
		r, err := scanTask(frows)
		if err != nil {
			return nil, "", fmt.Errorf("store: list hire tasks: %w", err)
		}
		out = append(out, r)
	}
	if err := frows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: list hire tasks: %w", err)
	}
	return out, next, nil
}
