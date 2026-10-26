package gate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/store"
)

// Sweep intervals: orphans once a minute (spec §14.5, tasks older than 2 minutes in submitted or quoted
// with no workflow end failed: lifecycle_unavailable), retention every 10 minutes (spec §5.4), and the ends
// of rotation overlaps once a minute (spec/tunnel.md §12: a rotated key's revoked_at is set when its 24 h
// overlap passes; the registry already stops using the key at overlap_until, enroll.Revocations.KeyUsable).
const (
	OrphanInterval    = time.Minute
	OrphanAge         = 2 * time.Minute
	RetentionInterval = 10 * time.Minute
	OverlapInterval   = time.Minute
	// IdleInterval is how often the MCP sessions idle past their service's session_idle_s are closed (spec
	// §11.2).
	IdleInterval = time.Minute
)

// Per-pass bounds. A pass checks at most OrphanBudget candidates (one Temporal DescribeWorkflowExecution
// each), in pages of orphanPage, and continues where the last one stopped, so a gate with many tasks
// waiting in submitted reaches every candidate in turn. Retention deletes in batches of retentionBatch, at
// most retentionBatches of them a pass.
const (
	OrphanBudget     = 1000
	orphanPage       = 100
	retentionBatch   = 500
	retentionBatches = 100
)

// WorkflowChecker tells the orphan sweep whether a task's workflow exists; lifecycle.Client implements it.
type WorkflowChecker interface {
	WorkflowExists(ctx context.Context, taskID a2a.TaskID) (bool, error)
}

// RunSweeps runs the sweeps until ctx ends, each once at start and then on its interval. Any replica may
// run them; each pass is idempotent.
func (r *Replica) RunSweeps(ctx context.Context) error {
	orphans := time.NewTicker(r.opts.orphanEvery)
	defer orphans.Stop()
	retention := time.NewTicker(r.opts.retentionEvery)
	defer retention.Stop()
	overlaps := time.NewTicker(OverlapInterval)
	defer overlaps.Stop()
	idle := time.NewTicker(IdleInterval)
	defer idle.Stop()
	keys := enroll.NewStore(r.pool, enroll.Options{Logger: r.log})
	r.sweepOrphans(ctx)
	r.sweepRetention(ctx)
	r.sweepOverlaps(ctx, keys)
	r.sweepIdle(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-orphans.C:
			r.sweepOrphans(ctx)
		case <-retention.C:
			r.sweepRetention(ctx)
		case <-overlaps.C:
			r.sweepOverlaps(ctx, keys)
		case <-idle.C:
			r.sweepIdle(ctx)
		}
	}
}

// sweepIdle closes the MCP sessions idle past their service's session_idle_s, with a best-effort upstream DELETE
// (spec §11.2): those whose tunnels this replica holds, and those of direct backends.
func (r *Replica) sweepIdle(ctx context.Context) {
	if r.mcp == nil {
		return
	}
	if _, err := r.mcp.SweepIdle(ctx); err != nil && ctx.Err() == nil {
		r.log.Warn("mcp idle sweep", "error", err)
	}
}

func (r *Replica) sweepOverlaps(ctx context.Context, keys *enroll.Store) {
	ended, err := SweepOverlaps(ctx, keys)
	if err != nil && ctx.Err() == nil {
		r.log.Warn("overlap sweep", "error", err, "ended", len(ended))
	} else if len(ended) > 0 {
		r.log.Info("overlap sweep: rotated node keys past their 24 h overlap revoked", "keys", ended)
	}
}

// SweepOverlaps is one pass of the overlap sweep (spec/tunnel.md §12): every rotated node key whose
// overlap_until has passed gets revoked_at, an audit row and NOTIFY kontra_revoke 'key:<key_id>', so the
// replica holding its session closes it. It returns the keys it ended.
func SweepOverlaps(ctx context.Context, keys *enroll.Store) ([]string, error) {
	return keys.EndOverlaps(ctx)
}

func (r *Replica) sweepOrphans(ctx context.Context) {
	n, err := SweepOrphans(ctx, r.storeLog, r.lifecycle, time.Now().Add(-r.opts.orphanAge), &r.orphanFrom, OrphanBudget)
	if err != nil && ctx.Err() == nil {
		r.log.Warn("orphan sweep", "error", err, "ended", n)
	} else if n > 0 {
		r.log.Info("orphan sweep: tasks without a workflow ended failed: lifecycle_unavailable", "ended", n)
	}
}

func (r *Replica) sweepRetention(ctx context.Context) {
	n, err := SweepRetention(ctx, r.storeLog, time.Now(), r.objects())
	if err != nil && ctx.Err() == nil {
		r.log.Warn("retention sweep", "error", err, "deleted", n)
	} else if n > 0 {
		r.log.Info("retention sweep: tasks past retention_until deleted with their rows", "deleted", n)
	}
}

// SweepOrphans is one pass of the orphan sweep (spec §14.5, D63): every task in submitted or quoted created
// before olderThan whose workflow does not exist is ended `failed: lifecycle_unavailable` through the store
// (store.Log.FailOrphan, which also ends its push configs). It checks at most budget candidates, starting
// after *cursor and leaving *cursor where it stopped (the zero value once it has reached the end). A
// candidate whose workflow cannot be asked about (Temporal unreachable) is never failed: the pass stops and
// the next one asks again. It returns how many tasks it ended.
func SweepOrphans(ctx context.Context, log *store.Log, wf WorkflowChecker, olderThan time.Time, cursor *store.OrphanCandidate, budget int) (int, error) {
	ended := 0
	for budget > 0 {
		page, err := log.OrphanCandidates(ctx, olderThan, *cursor, min(orphanPage, budget))
		if err != nil {
			return ended, err
		}
		if len(page) == 0 {
			*cursor = store.OrphanCandidate{}
			return ended, nil
		}
		for _, c := range page {
			exists, err := wf.WorkflowExists(ctx, a2a.TaskID(c.ID.String()))
			if err != nil {
				return ended, err
			}
			*cursor = c
			if exists {
				continue
			}
			ok, err := log.FailOrphan(ctx, c.ID)
			switch {
			case errors.Is(err, a2a.ErrTaskNotFound):
				// Deleted meanwhile (retention): nothing to end.
			case err != nil:
				return ended, err
			case ok:
				ended++
			}
		}
		budget -= len(page)
	}
	return ended, nil
}

// SweepRetention is one pass of the retention sweep (spec §5.4): terminal tasks past retention_until are
// deleted with their task_events, attempts, work_items, push_configs and blobs rows, in batches, each task's
// bucket objects first when the gate has a bucket (objects, nil without one; D167), and MCP sessions closed for
// longer than their service's retention go the same way. It returns how many tasks it deleted.
//
// A task whose objects the bucket would not delete keeps its rows and is skipped by the pass's later batches; the
// pass goes on, sweeps the sessions, and reports those tasks in its error. Once a batch's worth of tasks failed
// so in one pass (the bucket is likely down), it deletes no more tasks until the next pass.
func SweepRetention(ctx context.Context, log *store.Log, now time.Time, objects store.ObjectDeleter) (int, error) {
	return retentionPass(ctx, log, now, objects, retentionBatch, retentionBatches)
}

func retentionPass(ctx context.Context, log *store.Log, now time.Time, objects store.ObjectDeleter, batch, batches int) (int, error) {
	var (
		total int
		kept  []uuid.UUID
		first error
		errs  []error
	)
	for range batches {
		b, err := log.DeleteExpired(ctx, now, batch, objects, kept)
		total += b.Deleted
		for _, k := range b.Kept {
			kept = append(kept, k.ID)
			if first == nil {
				first = k.Err
			}
		}
		if err != nil {
			errs = append(errs, err)
			break
		}
		// Another full batch may wait, unless this one was the last, another pass holds some of its tasks (it
		// deletes them; the next batch would find them again), or too many failed.
		if b.Found < batch || b.Deleted+len(b.Kept) < b.Found || len(kept) >= batch || ctx.Err() != nil {
			break
		}
	}
	if len(kept) > 0 {
		errs = append(errs, fmt.Errorf("the bucket objects of %d expired tasks could not be deleted; their rows are kept for the next pass (%w)", len(kept), first))
	}
	for range batches {
		n, err := log.DeleteClosedSessions(ctx, now, config.DefaultTaskRetention, batch)
		if err != nil {
			errs = append(errs, err)
			break
		}
		if n < batch {
			break
		}
	}
	return total, errors.Join(errs...)
}
