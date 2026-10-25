package store

import (
	"context"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/unimpl"
)

// DeleteExpired deletes up to limit terminal tasks past retention_until, with their task_events,
// attempts, work_items and push_configs (cascade) (spec §5.4). Bucket objects are the caller's.
func (l *Log) DeleteExpired(ctx context.Context, now time.Time, limit int) (int, error) {
	return 0, unimpl.Error("store.Log.DeleteExpired", "WP-08")
}

// OrphanCandidates returns tasks in submitted or quoted created before olderThan (spec §14.5); the sweep
// checks each one's workflow and ends those that have none with failed: lifecycle_unavailable.
func (l *Log) OrphanCandidates(ctx context.Context, olderThan time.Time, limit int) ([]uuid.UUID, error) {
	return nil, unimpl.Error("store.Log.OrphanCandidates", "WP-08")
}
