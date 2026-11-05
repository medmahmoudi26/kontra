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
)

// The tasks of the gate's own /mcp (spec/relay.md §12; M4 WP-04). hire creates an ordinary A2A gate task through
// the service's work queue and marks it tasks.hired (D272); /mcp's MCP Tasks see only those, of the principal's
// buyer, across services. A hire's resend key is its first_request, SHA-256(ref ‖ 0x00 ‖ JCS(arguments)) (§12.3
// Retries), matched on (tenant, service, buyer, principal, first_request) while the task is quoted, or less than the
// retry window after it left quoted. A quoted hire task sent to a session that declared elicitation.url carries
// that URL elicitation's id and the session (tasks.elicitation_id, tasks.elicitation_session), and the one GET
// stream of the session that claims it (tasks.elicitation_notified_at, D288) writes its
// notifications/elicitation/complete.

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

// hireRetryWhere selects the hire task a hire with the same resend key answers (§12.3 Retries): created quoted
// (quote_expires_at is set only then), and still quoted or out of quoted (approved: submitted_at; declined,
// expired or canceled: terminal_at) less than $6 microseconds ago.
const hireRetryWhere = `t.tenant_id = $1 and t.service = $2 and t.buyer_id = $3 and t.principal = $4
	  and t.first_request = $5 and t.hired and t.quote_expires_at is not null
	  and (t.state = 'quoted' or coalesce(t.submitted_at, t.terminal_at) > now() - $6::bigint * interval '1 microsecond')`

// FindHireRetry returns the task a hire with resend key key answers (§12.3 Retries): a hire task of the same tenant,
// service, buyer and principal with that first_request, quoted, or out of quoted less than window ago; nil when
// none. Any other hire is a new task.
func (l *Log) FindHireRetry(ctx context.Context, svc ServiceRef, o Owner, key [32]byte, window time.Duration) (*TaskRow, error) {
	return l.findHireRetry(ctx, l.db, svc, o, key, window)
}

// FindHireRetryLocked is FindHireRetry inside tx, the transaction that creates the task, after taking the
// transaction-scoped advisory lock of the resend key: of two identical hires arriving together, the second waits
// until the first's transaction ends and then finds its task, so one hire is never two tasks.
func (l *Log) FindHireRetryLocked(ctx context.Context, tx pgx.Tx, svc ServiceRef, o Owner, key [32]byte, window time.Duration) (*TaskRow, error) {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, hireLockKey(svc, o, key)); err != nil {
		return nil, fmt.Errorf("store: lock the hire's resend key: %w", err)
	}
	return l.findHireRetry(ctx, tx, svc, o, key, window)
}

// hireLockKey is the advisory lock key of a hire's resend key, under a prefix of its own (resendLockKey's are
// "kontra.resend").
func hireLockKey(svc ServiceRef, o Owner, key [32]byte) int64 {
	h := sha256.New()
	for _, part := range []string{"kontra.hire", svc.TenantID.String(), svc.Name, o.BuyerID.String(), o.Principal} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	h.Write(key[:])
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func (l *Log) findHireRetry(ctx context.Context, q querier, svc ServiceRef, o Owner, key [32]byte, window time.Duration) (*TaskRow, error) {
	r, err := scanTask(q.QueryRow(ctx, `select `+taskCols+` from kontra.tasks t where `+hireRetryWhere+`
		order by t.created_at desc, t.id desc limit 1`,
		svc.TenantID, svc.Name, o.BuyerID, o.Principal, key[:], window.Microseconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: find a hire's retry: %w", err)
	}
	return r, nil
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

// ListHired returns one page of the buyer's hire tasks across services, newest first by status_at (then id; the
// tasks_hired index), after cursor ("" for the first page; a ListTasks page token, EncodePageToken), and the next
// page's cursor ("" when it is the last). A cursor the gate did not mint is ErrBadCursor.
func (l *Log) ListHired(ctx context.Context, buyer uuid.UUID, cursor string) ([]*TaskRow, string, error) {
	var at, after any
	if cursor != "" {
		t, id, err := DecodePageToken(cursor)
		if err != nil {
			return nil, "", ErrBadCursor
		}
		at, after = t, id
	}
	rows, err := l.db.Query(ctx, `select `+taskCols+` from kontra.tasks t
		where t.buyer_id = $1 and t.hired
		  and ($2::timestamptz is null or (t.status_at, t.id) < ($2::timestamptz, $3::uuid))
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

// RecordElicitation stores the URL elicitation a quoted hire task's -32042 carries (§12.3): its id, newID unless one
// is stored already (a retry answers the same one), and the session that receives it, which replaces an earlier
// session (the retry's session is the one waiting now). It reports the stored id, and ok false when the task is no
// longer quoted, or its notification was claimed (the caller answers the task as it is now).
func (l *Log) RecordElicitation(ctx context.Context, taskID uuid.UUID, newID string, session [32]byte) (string, bool, error) {
	var id string
	err := l.db.QueryRow(ctx, `update kontra.tasks set elicitation_id = coalesce(elicitation_id, $2), elicitation_session = $3
		where id = $1 and state = 'quoted' and elicitation_notified_at is null
		returning elicitation_id`, taskID, newID, session[:]).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: record the URL elicitation of task %s: %w", taskID, err)
	}
	return id, true, nil
}

// Elicitation is a URL elicitation a session received: its task, its id, and the task's state.
type Elicitation struct {
	TaskID        uuid.UUID
	ElicitationID string
	State         State
}

// SessionElicitations are the URL elicitations sent to session (a hire task's -32042, §12.3) whose
// notifications/elicitation/complete nobody has claimed: the tasks still quoted (to wait on) and those that left
// quoted less than window ago (to notify). buyer is the session's: the tasks_hired index finds the buyer's tasks.
func (l *Log) SessionElicitations(ctx context.Context, session [32]byte, buyer uuid.UUID, window time.Duration) ([]Elicitation, error) {
	rows, err := l.db.Query(ctx, `select t.id, t.elicitation_id, t.state from kontra.tasks t
		where t.buyer_id = $1 and t.hired and t.elicitation_session = $2 and t.elicitation_notified_at is null
		  and (t.state = 'quoted' or coalesce(t.submitted_at, t.terminal_at) > now() - $3::bigint * interval '1 microsecond')
		order by t.id`, buyer, session[:], window.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("store: the URL elicitations of a session: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Elicitation, error) {
		var e Elicitation
		err := r.Scan(&e.TaskID, &e.ElicitationID, &e.State)
		return e, err
	})
}

// ClaimElicitationComplete claims the notifications/elicitation/complete of a task that left quoted for one GET
// stream of session (D288): the update that sets elicitation_notified_at wins only while it is null, so exactly
// one stream of the session, on any replica, writes it. It returns the elicitation id and whether this call won.
func (l *Log) ClaimElicitationComplete(ctx context.Context, taskID uuid.UUID, session [32]byte) (string, bool, error) {
	var id string
	err := l.db.QueryRow(ctx, `update kontra.tasks set elicitation_notified_at = now()
		where id = $1 and elicitation_session = $2 and elicitation_notified_at is null and state <> 'quoted'
		returning elicitation_id`, taskID, session[:]).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: claim the elicitation/complete of task %s: %w", taskID, err)
	}
	return id, true, nil
}

// HireKeyWindow is how long a hire's idempotencyKey answers the task it created (relay.md §12.3 Idempotency, D338):
// a hire with the same key, of the same buyer and principal, created less than this ago.
const HireKeyWindow = 24 * time.Hour

// KeyedHire is a hire task an idempotencyKey names (FindHireKeyed): the task, and the resend key its first hire had
// (tasks.first_request), which a hire with the same key and other arguments does not match.
type KeyedHire struct {
	Task         *TaskRow
	FirstRequest [32]byte
}

// FindHireKeyed returns the hire task of buyer and principal whose first_message_id is messageID (the messageId a hire
// with an idempotencyKey mints from it, relay.md §12.3) and that was created after since, whatever its service; nil
// when none. It reads the index tasks_hired_by_message (0005).
func (l *Log) FindHireKeyed(ctx context.Context, buyer uuid.UUID, principal, messageID string, since time.Time) (*KeyedHire, error) {
	return l.findHireKeyed(ctx, l.db, buyer, principal, messageID, since)
}

// FindHireKeyedLocked is FindHireKeyed inside tx, the transaction that creates the task, after taking the
// transaction-scoped advisory lock of (buyer, principal, messageID): of two hires with one key arriving together,
// whatever their services, the second waits until the first's transaction ends and then finds its task, so one key is
// never two tasks.
func (l *Log) FindHireKeyedLocked(ctx context.Context, tx pgx.Tx, buyer uuid.UUID, principal, messageID string, since time.Time) (*KeyedHire, error) {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, hireKeyLockKey(buyer, principal, messageID)); err != nil {
		return nil, fmt.Errorf("store: lock the hire's idempotency key: %w", err)
	}
	return l.findHireKeyed(ctx, tx, buyer, principal, messageID, since)
}

// hireKeyLockKey is the advisory lock key of a hire's idempotency key, under a prefix of its own.
func hireKeyLockKey(buyer uuid.UUID, principal, messageID string) int64 {
	h := sha256.New()
	for _, part := range []string{"kontra.hire.key", buyer.String(), principal, messageID} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func (l *Log) findHireKeyed(ctx context.Context, q querier, buyer uuid.UUID, principal, messageID string, since time.Time) (*KeyedHire, error) {
	var fr []byte
	r, err := scanTask(scanAlso{q.QueryRow(ctx, `select `+taskCols+`, coalesce(t.first_request, ''::bytea) from kontra.tasks t
		where t.buyer_id = $1 and t.first_message_id = $2 and t.hired and t.principal = $3 and t.created_at > $4
		order by t.created_at desc, t.id desc limit 1`, buyer, messageID, principal, since), []any{&fr}})
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
	var at, after any
	if cursor != "" {
		t, id, err := DecodePageToken(cursor)
		if err != nil {
			return nil, "", ErrBadCursor
		}
		at, after = t, id
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
		where t.buyer_id = $1 and t.hired
		  and ($2::timestamptz is null or (t.status_at, t.id) < ($2::timestamptz, $3::uuid))
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
