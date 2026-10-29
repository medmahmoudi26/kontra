package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The sweeps of spec §5.4 (retention) and §14.5 (orphans). Any replica runs them and every statement is
// idempotent, so replicas need no coordination: two passes that meet skip each other's rows (retention) or
// find the task already ended (orphans).

// ObjectDeleter removes every bucket object of a task (blob.Store.DeleteTask, spec §5.4, D167).
type ObjectDeleter interface {
	DeleteTask(ctx context.Context, tenant, task uuid.UUID) error
}

// ObjectDeleteTimeout bounds the deletion of one task's objects by the retention sweep: a bucket that hangs on one
// prefix fails that task, which keeps its rows for a later pass, instead of holding the sweep.
const ObjectDeleteTimeout = time.Minute

// objectDeleters is how many tasks' objects one DeleteExpired batch deletes at once (one LIST, then the deletes,
// each).
const objectDeleters = 8

// ExpiredBatch is what one DeleteExpired batch did.
type ExpiredBatch struct {
	// Deleted is how many tasks it deleted.
	Deleted int
	// Found is how many expired tasks it took up: fewer than its limit means no other expired task is left, but
	// those it was told to skip and those another pass holds.
	Found int
	// Kept are the tasks whose bucket objects could not be deleted, with the error: their rows are kept, so a later
	// pass tries again.
	Kept []KeptTask
}

// KeptTask is an expired task whose objects could not be deleted.
type KeptTask struct {
	ID  uuid.UUID
	Err error
}

// DeleteExpired deletes up to limit terminal tasks whose retention_until is at or before now, with their
// task_events, attempts, work_items, push_configs and blobs rows (each references tasks on delete cascade) (spec
// §5.4). Rows another pass is deleting are skipped, not waited for. It runs on ctx, not on the writers' timeout: a
// batch of tasks with long logs may take a while, and the sweep decides how long it waits.
//
// With a bucket (objects not nil) a task's objects go first, then its rows (D167): objects.DeleteTask is called
// for each expired task, by prefix, whether or not it has a blobs row (an object stored for an event that was
// then refused has none), up to objectDeleters at once and each within ObjectDeleteTimeout, and only the tasks
// whose objects are gone are deleted. A task whose objects could not be deleted keeps its rows and is reported in
// Kept, not as an error: the batch goes on, and the caller passes it in skip to the next batch of the same pass, so
// one task the bucket refuses (an object lock, an access-denied prefix) does not head every batch. The error is
// the database's.
func (l *Log) DeleteExpired(ctx context.Context, now time.Time, limit int, objects ObjectDeleter, skip []uuid.UUID) (ExpiredBatch, error) {
	if limit <= 0 {
		return ExpiredBatch{}, nil
	}
	if objects == nil {
		tag, err := l.db.Exec(ctx, `delete from kontra.tasks t
			 using (select id from kontra.tasks
			         where retention_until is not null and retention_until <= $1
			         order by retention_until
			         limit $2
			         for update skip locked) x
			 where t.id = x.id`, now, limit)
		if err != nil {
			return ExpiredBatch{}, fmt.Errorf("store: delete expired tasks: %w", err)
		}
		n := int(tag.RowsAffected())
		return ExpiredBatch{Deleted: n, Found: n}, nil
	}
	if skip == nil {
		skip = []uuid.UUID{} // a NULL array would match nothing in "not (id = any(...))"
	}
	rows, err := l.db.Query(ctx, `select id, tenant_id from kontra.tasks
		 where retention_until is not null and retention_until <= $1 and not (id = any($3::uuid[]))
		 order by retention_until
		 limit $2`, now, limit, skip)
	if err != nil {
		return ExpiredBatch{}, fmt.Errorf("store: expired tasks: %w", err)
	}
	type expired struct{ id, tenant uuid.UUID }
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (expired, error) {
		var x expired
		err := r.Scan(&x.id, &x.tenant)
		return x, err
	})
	if err != nil {
		return ExpiredBatch{}, fmt.Errorf("store: expired tasks: %w", err)
	}
	out := ExpiredBatch{Found: len(found)}
	errs := make([]error, len(found))
	sem := make(chan struct{}, objectDeleters)
	var wg sync.WaitGroup
	for n, x := range found {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			dctx, cancel := context.WithTimeout(ctx, ObjectDeleteTimeout)
			defer cancel()
			if err := objects.DeleteTask(dctx, x.tenant, x.id); err != nil {
				errs[n] = fmt.Errorf("store: delete the bucket objects of task %s: %w", x.id, err)
			}
		}()
	}
	wg.Wait()
	var gone []uuid.UUID
	for n, x := range found {
		if errs[n] != nil {
			out.Kept = append(out.Kept, KeptTask{ID: x.id, Err: errs[n]})
			continue
		}
		gone = append(gone, x.id)
	}
	if len(gone) == 0 {
		return out, nil
	}
	tag, err := l.db.Exec(ctx, `delete from kontra.tasks t
		 using (select id from kontra.tasks
		         where id = any($1::uuid[]) and retention_until is not null and retention_until <= $2
		         for update skip locked) x
		 where t.id = x.id`, gone, now)
	if err != nil {
		return out, fmt.Errorf("store: delete expired tasks: %w", err)
	}
	out.Deleted = int(tag.RowsAffected())
	return out, nil
}

// DeleteClosedSessions deletes up to limit mcp_sessions rows closed for longer than the retention of their
// service (spec §5.4: "mcp_sessions closed for longer than the retention go the same way"). A session of the
// gate's own /mcp names no service and keeps defaultRetention. It returns how many it deleted.
func (l *Log) DeleteClosedSessions(ctx context.Context, now time.Time, defaultRetention time.Duration, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	tag, err := l.db.Exec(ctx, `delete from kontra.mcp_sessions m
		 using (select m2.client_sid_hash from kontra.mcp_sessions m2
		          left join kontra.services s on s.tenant_id = m2.tenant_id and s.name = m2.service
		         where m2.closed_at is not null
		           and m2.closed_at + coalesce(s.retention_s * interval '1 second', $2::bigint * interval '1 microsecond') <= $1
		         limit $3
		         for update of m2 skip locked) x
		 where m.client_sid_hash = x.client_sid_hash`, now, defaultRetention.Microseconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("store: delete closed MCP sessions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// OrphanCandidate is a task the orphan sweep checks for a workflow: its id and creation time, which is also
// the sweep's keyset cursor.
type OrphanCandidate struct {
	ID        uuid.UUID
	CreatedAt time.Time
}

// OrphanCandidates returns up to limit tasks in submitted or quoted created before olderThan, in creation
// order after the cursor after (the zero value starts at the oldest) (spec §14.5). The sweep checks each
// one's workflow and ends those that have none with FailOrphan. A task that waits in submitted for a long
// time while its workflow runs is a candidate at every pass; the cursor lets a sweep with a per-pass budget
// reach every candidate in turn.
func (l *Log) OrphanCandidates(ctx context.Context, olderThan time.Time, after OrphanCandidate, limit int) ([]OrphanCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := l.db.Query(ctx, `select id, created_at from kontra.tasks
		 where terminal_at is null and state in ('submitted', 'quoted') and created_at < $1
		   and (created_at, id) > ($2, $3)
		 order by created_at, id
		 limit $4`, olderThan, after.CreatedAt, after.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: orphan candidates: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (OrphanCandidate, error) {
		var c OrphanCandidate
		err := r.Scan(&c.ID, &c.CreatedAt)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: orphan candidates: %w", err)
	}
	return out, nil
}

// FailOrphan ends a task whose workflow does not exist with `failed: lifecycle_unavailable` (spec §14.5,
// D63), written through the log like every terminal event, and in the same transaction marks the task's
// push configs failed: no workflow will ever deliver them, and until then they would count toward the
// buyer's delivering configs for the task's whole retention (spec §10.2). It reports whether it ended the
// task: a task no longer in submitted or quoted (its workflow started meanwhile and moved it, or another
// replica's sweep ended it) is left as it is. A task that does not exist is a2a.ErrTaskNotFound.
func (l *Log) FailOrphan(ctx context.Context, taskID uuid.UUID) (bool, error) {
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	var ended bool
	err := l.inTx(ctx, func(tx pgx.Tx) error {
		ended = false
		row, err := l.load(ctx, tx, taskID, " for update")
		if err != nil {
			return err
		}
		if row.State != StateSubmitted && row.State != StateQuoted {
			return nil
		}
		// An MCP task's failure is its MCP status row with its calls row (spec §11.8 case 2).
		wr, err := l.terminalWrite(ctx, tx, row, TerminalWrite{TaskID: taskID, State: StateFailed, Reason: ReasonLifecycleUnavailable})
		if err != nil {
			return fmt.Errorf("store: fail orphan %s: %w", taskID, err)
		}
		if _, err := l.Append(ctx, tx, wr); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update kontra.push_configs set failed_at = now()
			 where task_id = $1 and failed_at is null`, taskID); err != nil {
			return fmt.Errorf("store: end the push configs of orphan %s: %w", taskID, err)
		}
		ended = true
		return nil
	})
	if err != nil {
		if errors.Is(err, a2a.ErrTaskNotFound) {
			return false, err
		}
		return false, fmt.Errorf("store: fail orphan %s: %w", taskID, err)
	}
	return ended, nil
}
