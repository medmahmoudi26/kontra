package gate

import (
	"context"
	"errors"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/store"
)

// Sweep intervals: orphans once a minute (spec §14.5, tasks older than 2 minutes in submitted or quoted
// with no workflow end failed: lifecycle_unavailable), retention every 10 minutes (spec §5.4).
const (
	OrphanInterval    = time.Minute
	OrphanAge         = 2 * time.Minute
	RetentionInterval = 10 * time.Minute
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

// RunSweeps runs both sweeps until ctx ends, each once at start and then on its interval. Any replica may
// run them; each pass is idempotent.
func (r *Replica) RunSweeps(ctx context.Context) error {
	orphans := time.NewTicker(r.opts.orphanEvery)
	defer orphans.Stop()
	retention := time.NewTicker(r.opts.retentionEvery)
	defer retention.Stop()
	r.sweepOrphans(ctx)
	r.sweepRetention(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-orphans.C:
			r.sweepOrphans(ctx)
		case <-retention.C:
			r.sweepRetention(ctx)
		}
	}
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
	n, err := SweepRetention(ctx, r.storeLog, time.Now())
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
// deleted with their task_events, attempts, work_items and push_configs, in batches, and MCP sessions closed
// for longer than their service's retention go the same way. It returns how many tasks it deleted.
func SweepRetention(ctx context.Context, log *store.Log, now time.Time) (int, error) {
	total := 0
	for range retentionBatches {
		n, err := log.DeleteExpired(ctx, now, retentionBatch)
		total += n
		if err != nil {
			return total, err
		}
		if n < retentionBatch {
			break
		}
	}
	for range retentionBatches {
		n, err := log.DeleteClosedSessions(ctx, now, config.DefaultTaskRetention, retentionBatch)
		if err != nil {
			return total, err
		}
		if n < retentionBatch {
			break
		}
	}
	return total, nil
}
