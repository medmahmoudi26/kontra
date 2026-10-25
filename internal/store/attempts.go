package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Backend is attempts.backend.
type Backend string

// Backends.
const (
	BackendDirect Backend = "direct"
	BackendTunnel Backend = "tunnel" // M2
)

// Outcome is attempts.outcome.
type Outcome string

// Attempt outcomes (0001_init.sql).
const (
	OutcomeCompleted           Outcome = "completed"
	OutcomeFailed              Outcome = "failed"
	OutcomeCanceled            Outcome = "canceled"
	OutcomeRejected            Outcome = "rejected"
	OutcomeInterrupted         Outcome = "interrupted"
	OutcomeNoSession           Outcome = "no_session"
	OutcomeTunnelLost          Outcome = "tunnel_lost"
	OutcomeUpstreamUnreachable Outcome = "upstream_unreachable"
	OutcomeUpstreamError       Outcome = "upstream_error"
	OutcomeServiceUnavailable  Outcome = "service_unavailable"
	OutcomeTimeout             Outcome = "timeout"
	OutcomeSealedRejected      Outcome = "sealed_rejected"
	OutcomeAborted             Outcome = "aborted"
)

// Valid reports whether o is one of the outcomes attempts.outcome accepts.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeCompleted, OutcomeFailed, OutcomeCanceled, OutcomeRejected, OutcomeInterrupted, OutcomeNoSession,
		OutcomeTunnelLost, OutcomeUpstreamUnreachable, OutcomeUpstreamError, OutcomeServiceUnavailable,
		OutcomeTimeout, OutcomeSealedRejected, OutcomeAborted:
		return true
	}
	return false
}

// Attempt is one placement of a gate task on one upstream (spec §4). Upstream ids live only here.
type Attempt struct {
	TaskID            uuid.UUID
	N                 int
	Backend           Backend
	TunnelSession     *uuid.UUID
	KeyID             string
	ReplicaID         string    // where the latest dispatch runs; routes abort and forward_cancel
	DispatchID        uuid.UUID // the latest dispatch's fence (D77); uuid.Nil before the first dispatch
	UpstreamTaskID    string
	UpstreamContextID string
	UpstreamSession   string
	StartedAt         time.Time
	FirstUpstreamIDAt *time.Time
	EndedAt           *time.Time
	Outcome           Outcome
}

const attemptCols = `a.task_id, a.n, a.backend, a.tunnel_session, coalesce(a.key_id, ''), coalesce(a.replica_id, ''),
	a.dispatch_id, coalesce(a.upstream_task_id, ''), coalesce(a.upstream_context_id, ''),
	coalesce(a.upstream_session, ''), a.started_at, a.first_upstream_id_at, a.ended_at, coalesce(a.outcome, '')`

func scanAttempt(row pgx.Row) (*Attempt, error) {
	var (
		a        Attempt
		dispatch *uuid.UUID
	)
	if err := row.Scan(&a.TaskID, &a.N, &a.Backend, &a.TunnelSession, &a.KeyID, &a.ReplicaID, &dispatch,
		&a.UpstreamTaskID, &a.UpstreamContextID, &a.UpstreamSession, &a.StartedAt, &a.FirstUpstreamIDAt,
		&a.EndedAt, &a.Outcome); err != nil {
		return nil, err
	}
	if dispatch != nil {
		a.DispatchID = *dispatch
	}
	return &a, nil
}

// BeginAttempt inserts attempt n (prepare, spec §14.2). n must be the task's next attempt (1, or one
// more than its latest). prepare is retried, so inserting an attempt that already exists with the same
// backend is not an error. An attempt n > 1 is a retry: BeginAttempt applies BeginRetry's guard to it.
func (l *Log) BeginAttempt(ctx context.Context, a Attempt) error {
	return l.BeginRetry(ctx, a, "")
}

// BeginRetry inserts attempt n as BeginAttempt does. For n > 1, a retry (spec §6.4: "a retry is only ever
// made before the first upstream task id of the current attempt exists"), it first locks attempt n-1 in the
// same transaction: when n-1 records a first upstream task id, nothing is written and the answer is
// ErrAttemptHasUpstream; otherwise n-1, when still open, is ended with previous (failed when previous is
// empty), and attempt n inserted. A dispatch of n-1 that Temporal timed out but whose process is alive
// (D77) can record its first upstream id until this transaction commits, and never after it
// (SetFirstUpstreamID is fenced): either the retry sees the id and refuses, or the dispatch is fenced.
func (l *Log) BeginRetry(ctx context.Context, a Attempt, previous Outcome) error {
	if a.N < 1 {
		return fmt.Errorf("store: attempt %d", a.N)
	}
	if a.Backend != BackendDirect && a.Backend != BackendTunnel {
		return fmt.Errorf("store: backend %q", a.Backend)
	}
	if previous != "" && !previous.Valid() {
		return fmt.Errorf("store: attempt outcome %q", previous)
	}
	if previous == "" {
		previous = OutcomeFailed
	}
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	var inserted bool
	err := l.inTx(ctx, func(tx pgx.Tx) error {
		inserted = false
		if a.N > 1 {
			var hasUpstream, ended bool
			err := tx.QueryRow(ctx, `select first_upstream_id_at is not null, ended_at is not null from kontra.attempts
					where task_id = $1 and n = $2 for update`, a.TaskID, a.N-1).Scan(&hasUpstream, &ended)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				// No attempt n-1: the insert below refuses an attempt out of sequence (or a missing task).
			case err != nil:
				return err
			case hasUpstream:
				return fmt.Errorf("store: attempt %d of task %s: %w", a.N-1, a.TaskID, ErrAttemptHasUpstream)
			case !ended:
				if _, err := tx.Exec(ctx, `update kontra.attempts set ended_at = now(), outcome = $3
						where task_id = $1 and n = $2 and ended_at is null`, a.TaskID, a.N-1, string(previous)); err != nil {
					return err
				}
			}
		}
		tag, err := tx.Exec(ctx, `insert into kontra.attempts (task_id, n, backend, tunnel_session, key_id, replica_id)
				select $1::uuid, $2::integer, $3::text, $4::uuid, $5::text, $6::text
				 where $2::integer = coalesce((select max(n) from kontra.attempts where task_id = $1::uuid), 0) + 1
				on conflict (task_id, n) do nothing`,
			a.TaskID, a.N, string(a.Backend), a.TunnelSession, nullString(a.KeyID), nullString(a.ReplicaID))
		if err != nil {
			return err
		}
		inserted = tag.RowsAffected() == 1
		return nil
	})
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, ErrAttemptHasUpstream):
		return err
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		return notFound()
	case err != nil:
		return fmt.Errorf("store: begin attempt %d of task %s: %w", a.N, a.TaskID, err)
	case inserted:
		return nil
	}
	got, err := l.GetAttempt(ctx, a.TaskID, a.N)
	switch {
	case errors.Is(err, ErrAttemptNotFound):
		if _, lerr := l.Load(ctx, a.TaskID); lerr != nil {
			return lerr
		}
		return fmt.Errorf("store: attempt %d of task %s is not its next attempt", a.N, a.TaskID)
	case err != nil:
		return err
	case got.Backend != a.Backend:
		return fmt.Errorf("store: attempt %d of task %s already exists with backend %s", a.N, a.TaskID, got.Backend)
	}
	return nil
}

// BeginDispatch starts a dispatch on attempt n (spec §4, §6.4, §14.2): before any upstream call it records
// replicaID, where the dispatch runs, and a fresh dispatch_id, which fences every earlier dispatch of the
// task. It returns the fence the dispatch attaches to its context (WithFence). Attempt n must be the
// task's latest and not ended, else ErrAttemptNotOpen (ErrAttemptNotFound when it does not exist).
func (l *Log) BeginDispatch(ctx context.Context, taskID uuid.UUID, n int, replicaID string) (*Fence, error) {
	if replicaID == "" {
		return nil, errors.New("store: a dispatch needs its replica id")
	}
	dispatch, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	tag, err := l.writeExec(ctx, `update kontra.attempts a set replica_id = $3, dispatch_id = $4
			where a.task_id = $1 and a.n = $2 and a.ended_at is null
			  and not exists (select 1 from kontra.attempts b where b.task_id = a.task_id and b.n > a.n)`,
		taskID, n, replicaID, dispatch)
	if err != nil {
		return nil, fmt.Errorf("store: begin dispatch on attempt %d of task %s: %w", n, taskID, err)
	}
	if tag.RowsAffected() == 0 {
		if _, err := l.GetAttempt(ctx, taskID, n); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("store: attempt %d of task %s: %w", n, taskID, ErrAttemptNotOpen)
	}
	return &Fence{TaskID: taskID, N: n, DispatchID: dispatch}, nil
}

// SetFirstUpstreamID records the attempt's first upstream task id and context id, and
// first_upstream_id_at, before the event that carried them is logged (spec §4), under the dispatch's fence
// f: the write commits only while attempt f.N is the task's latest, has ended_at null and holds
// f.DispatchID, the condition every append of the dispatch carries (§6.4, D77). A dispatch the workflow has
// given up on (a heartbeat timeout while its process lives on) is therefore refused, ErrFenced, and f
// records it: the dispatch stops before it logs or signals anything, and a retry of the attempt (BeginRetry)
// never follows an attempt whose upstream task exists. It is a no-op when the same id is already recorded
// and ErrUpstreamIDMismatch when a different one is.
func (l *Log) SetFirstUpstreamID(ctx context.Context, f *Fence, upstreamTaskID, upstreamContextID string) error {
	if f == nil {
		return errors.New("store: a first upstream task id is recorded under a dispatch's fence")
	}
	if upstreamTaskID == "" {
		return errors.New("store: an empty upstream task id")
	}
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	tag, err := l.writeExec(ctx, `update kontra.attempts a
			set upstream_task_id = $3, upstream_context_id = $4, first_upstream_id_at = now()
			where a.task_id = $1 and a.n = $2 and a.upstream_task_id is null
			  and a.ended_at is null and a.dispatch_id = $5
			  and not exists (select 1 from kontra.attempts b where b.task_id = a.task_id and b.n > a.n)`,
		f.TaskID, f.N, upstreamTaskID, nullString(upstreamContextID), f.DispatchID)
	if err != nil {
		return fmt.Errorf("store: first upstream id of attempt %d of task %s: %w", f.N, f.TaskID, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	a, err := l.GetAttempt(ctx, f.TaskID, f.N)
	if err != nil {
		return err
	}
	latest, err := l.LatestAttempt(ctx, f.TaskID)
	if err != nil {
		return err
	}
	if a.EndedAt != nil || a.DispatchID != f.DispatchID || latest.N != f.N {
		f.markFenced()
		return fmt.Errorf("store: first upstream id of attempt %d of task %s: %w", f.N, f.TaskID, ErrFenced)
	}
	if a.UpstreamTaskID != upstreamTaskID {
		return fmt.Errorf("store: attempt %d of task %s: %w", f.N, f.TaskID, ErrUpstreamIDMismatch)
	}
	return nil
}

// EndAttempt records the attempt's outcome and ended_at, only when the attempt ends (never when a turn
// stops at input-required, D77). Ending an attempt that has ended already keeps its first outcome and is
// not an error (a retried activity).
func (l *Log) EndAttempt(ctx context.Context, taskID uuid.UUID, n int, o Outcome) error {
	if !o.Valid() {
		return fmt.Errorf("store: attempt outcome %q", o)
	}
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	tag, err := l.writeExec(ctx, `update kontra.attempts set ended_at = now(), outcome = $3
			where task_id = $1 and n = $2 and ended_at is null`, taskID, n, string(o))
	if err != nil {
		return fmt.Errorf("store: end attempt %d of task %s: %w", n, taskID, err)
	}
	if tag.RowsAffected() == 0 {
		_, err := l.GetAttempt(ctx, taskID, n)
		return err
	}
	return nil
}

// GetAttempt returns attempt n. attempt_state reads it: the retry decision is taken on what Postgres
// recorded, never on heartbeat details (spec §14.2).
func (l *Log) GetAttempt(ctx context.Context, taskID uuid.UUID, n int) (*Attempt, error) {
	a, err := scanAttempt(l.db.QueryRow(ctx, `select `+attemptCols+` from kontra.attempts a
		where a.task_id = $1 and a.n = $2`, taskID, n))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: attempt %d of task %s: %w", n, taskID, ErrAttemptNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: attempt %d of task %s: %w", n, taskID, err)
	}
	return a, nil
}

// LatestAttempt returns the task's latest attempt, ErrAttemptNotFound when it has none.
func (l *Log) LatestAttempt(ctx context.Context, taskID uuid.UUID) (*Attempt, error) {
	a, err := scanAttempt(l.db.QueryRow(ctx, `select `+attemptCols+` from kontra.attempts a
		where a.task_id = $1 order by a.n desc limit 1`, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: task %s: %w", taskID, ErrAttemptNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: latest attempt of task %s: %w", taskID, err)
	}
	return a, nil
}

// PreviousContextAttempt returns the attempt that last recorded an upstream context id among the
// caller's tasks in the same gate context, service and backend, for the upstream context id reuse of spec
// §4 (nil when there is none). Contexts are scoped by (tenant, service, buyer, context id), so another
// buyer's identical context string is never found (D53).
func (l *Log) PreviousContextAttempt(ctx context.Context, svc ServiceRef, o Owner, contextID string, backend Backend) (*Attempt, error) {
	a, err := scanAttempt(l.db.QueryRow(ctx, `select `+attemptCols+` from kontra.attempts a
		join kontra.tasks t on t.id = a.task_id
		where t.tenant_id = $1 and t.service = $2 and t.buyer_id = $3 and t.context_id = $4
		  and a.backend = $5 and a.upstream_context_id is not null
		order by a.first_upstream_id_at desc, a.task_id desc, a.n desc limit 1`,
		svc.TenantID, svc.Name, o.BuyerID, contextID, string(backend)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: previous attempt in context: %w", err)
	}
	return a, nil
}
