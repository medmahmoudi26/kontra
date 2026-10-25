package gate

import (
	"context"
	"time"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Sweep intervals: orphans once a minute (spec §14.5, tasks older than 2 minutes in submitted or quoted
// with no workflow end failed: lifecycle_unavailable), retention every 10 minutes (spec §5.4).
const (
	OrphanInterval    = time.Minute
	OrphanAge         = 2 * time.Minute
	RetentionInterval = 10 * time.Minute
)

// RunSweeps runs both sweeps until ctx ends. Any replica may run them; each pass is idempotent.
func (r *Replica) RunSweeps(ctx context.Context) error {
	return unimpl.Error("gate.Replica.RunSweeps", "WP-08")
}
