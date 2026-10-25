package store

import (
	"context"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/eventqueue"
	"github.com/google/uuid"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// TaskWaker wakes a reader when the log of a task may have grown. pg.Listener implements it.
type TaskWaker interface {
	WaitTask(ctx context.Context, taskID uuid.UUID) (<-chan struct{}, func())
}

// Puller implements a2a-go's eventqueue.Puller over task_events (spec §15). It feeds a2a-go's own live
// subscriptions (a blocking SendMessage, a CancelTask's wait); the gate's SSE writer reads the log itself.
//
//   - It starts at the log's tail as of its creation (a2a-go always passes a nil first cursor, and
//     creates the reader before the work queue Write), so an old final event never closes a new turn.
//   - It skips message rows (a caller's own follow-up would otherwise be returned as the result and
//     close the reader) and metadata rows, advancing its cursor past them.
//   - It long-polls on the TaskWaker without holding a pool connection, and backs off on database errors
//     inside Pull (a2a-go retries every PollInterval with no backoff).
//   - Every message carries TaskVersion = seq (≥ 1), which is how a2a-go drops what its snapshot holds.
type Puller struct {
	log   *Log
	waker TaskWaker
	start int64
}

var _ eventqueue.Puller = (*Puller)(nil)

// NewPullerProvider returns the provider for eventqueue.NewPullQueueManager (PollInterval 100 ms,
// InactivityTimeout 0, spec §15).
func NewPullerProvider(log *Log, waker TaskWaker) eventqueue.PullerProvider {
	return func(ctx context.Context, taskID a2a.TaskID) (eventqueue.Puller, error) {
		return nil, unimpl.Error("store.PullerProvider", "WP-02")
	}
}

// Pull returns the carried rows after cursor (nil means the creation tail) and the next cursor (int64).
func (p *Puller) Pull(ctx context.Context, taskID a2a.TaskID, cursor eventqueue.PullCursor) (*eventqueue.PullResponse, error) {
	return nil, unimpl.Error("store.Puller.Pull", "WP-02")
}

// Close releases the puller's waker registration.
func (p *Puller) Close(ctx context.Context) error {
	return unimpl.Error("store.Puller.Close", "WP-02")
}
