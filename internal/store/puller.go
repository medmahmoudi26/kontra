package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/eventqueue"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/ids"
)

// TaskWaker wakes a reader when the log of a task may have grown. pg.Listener implements it: the
// channel receives on a kontra_task_events notification for the task, and on every reconnect or idle
// re-read of the listener; the func releases the registration.
type TaskWaker interface {
	WaitTask(ctx context.Context, taskID uuid.UUID) (<-chan struct{}, func())
}

// Puller timing.
const (
	// PullBatch is the most rows one Pull reads.
	PullBatch = 64
	// PullWait is how long a Pull that found nothing waits for a wake before it reads once more and
	// returns (the listener's own idle wake is every 25 s; a2a-go calls Pull again after PollInterval).
	PullWait = 10 * time.Second
	// PullBackoffMax bounds the delay a Pull adds after consecutive database errors (a2a-go retries every
	// PollInterval with no backoff of its own).
	PullBackoffMax = 5 * time.Second
	pullBackoffMin = 100 * time.Millisecond
)

// Puller implements a2a-go's eventqueue.Puller over task_events (spec §15). It feeds a2a-go's own live
// subscriptions (a blocking SendMessage, a CancelTask's wait); the gate's SSE writer reads the log itself.
//
//   - It starts at the log's tail as of its creation (a2a-go always passes a nil first cursor, and
//     creates the reader before the work queue Write), so an old final event never closes a new turn.
//   - It skips message rows (only callers' own messages are ever message rows: a follow-up would
//     otherwise be returned as the result and close the reader) and metadata rows, advancing its cursor
//     past them; the MCP kinds are never A2A events and are skipped too.
//   - It long-polls on the TaskWaker without holding a pool connection, and backs off on database errors
//     inside Pull (a2a-go retries every PollInterval with no backoff).
//   - Every message carries TaskVersion = seq (≥ 1), which is how a2a-go drops what its snapshot holds.
type Puller struct {
	log    *Log
	taskID uuid.UUID
	start  int64
	wake   <-chan struct{}
	stop   func()
	wait   time.Duration

	mu     sync.Mutex
	errors int
}

var _ eventqueue.Puller = (*Puller)(nil)

// NewPullerProvider returns the provider for eventqueue.NewPullQueueManager (PollInterval 100 ms,
// InactivityTimeout 0, spec §15). The provider reads the tail and registers with the waker at once, so a
// row committed after the reader was created is never missed. waker may be nil: Pull then re-reads every
// PullWait.
func NewPullerProvider(log *Log, waker TaskWaker) eventqueue.PullerProvider {
	return newPullerProvider(log, waker, PullWait)
}

func newPullerProvider(log *Log, waker TaskWaker, wait time.Duration) eventqueue.PullerProvider {
	return func(ctx context.Context, taskID a2a.TaskID) (eventqueue.Puller, error) {
		id, err := ids.ParseTaskID(string(taskID))
		if err != nil {
			return nil, err
		}
		p := &Puller{log: log, taskID: id, wait: wait, stop: func() {}}
		if waker != nil {
			// Registered on a context of its own: the reader outlives the provider call; Close releases it.
			p.wake, p.stop = waker.WaitTask(context.Background(), id)
		}
		if p.start, err = log.Tail(ctx, id); err != nil {
			p.stop()
			return nil, err
		}
		return p, nil
	}
}

// Pull returns the carried rows after cursor (nil means the creation tail) and the next cursor (int64).
// With nothing new it waits on the waker, re-reading on every wake, until a row arrives or PullWait
// passes; it may then return no messages, but the cursor it returns is never nil.
func (p *Puller) Pull(ctx context.Context, taskID a2a.TaskID, cursor eventqueue.PullCursor) (*eventqueue.PullResponse, error) {
	after := p.start
	switch c := cursor.(type) {
	case nil:
	case int64:
		after = c
	default:
		return nil, fmt.Errorf("store: puller cursor %T is not one this puller issued", cursor)
	}
	if string(taskID) != p.taskID.String() {
		return nil, fmt.Errorf("store: puller of task %s asked for %s", p.taskID, taskID)
	}
	resp, n, err := p.read(ctx, after)
	if err != nil || n > 0 {
		return resp, err
	}
	// Nothing yet: wait for a wake and read again, until a row arrives or PullWait passes. A wake can be
	// stale (a notification for a row already read), so one wake does not end the wait.
	timer := time.NewTimer(p.wait)
	defer timer.Stop()
	for {
		expired := false
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.wake: // a nil channel (no waker) never receives
		case <-timer.C:
			expired = true
		}
		if resp, n, err = p.read(ctx, after); err != nil || n > 0 || expired {
			return resp, err
		}
	}
}

// read returns the carried rows after `after` and how many rows (carried or not) it read.
func (p *Puller) read(ctx context.Context, after int64) (*eventqueue.PullResponse, int, error) {
	evs, err := p.log.Events(ctx, p.taskID, after, PullBatch)
	if err != nil {
		p.backoff(ctx)
		return nil, 0, err
	}
	p.mu.Lock()
	p.errors = 0
	p.mu.Unlock()
	resp := &eventqueue.PullResponse{Cursor: after}
	for _, ev := range evs {
		resp.Cursor = ev.Seq
		if !ev.Kind.Carried() {
			continue
		}
		if len(ev.Payload) == 0 {
			return nil, 0, fmt.Errorf("store: task %s row %d has no inline payload", p.taskID, ev.Seq)
		}
		var sr a2a.StreamResponse
		if err := json.Unmarshal(ev.Payload, &sr); err != nil {
			return nil, 0, fmt.Errorf("store: task %s row %d: %w", p.taskID, ev.Seq, err)
		}
		resp.Messages = append(resp.Messages, &eventqueue.Message{
			Event: sr.Event, TaskVersion: taskstore.TaskVersion(ev.Seq), Protocol: a2a.Version,
		})
	}
	return resp, len(evs), nil
}

func (p *Puller) backoff(ctx context.Context) {
	p.mu.Lock()
	p.errors++
	d := pullBackoffMin << min(p.errors-1, 6)
	p.mu.Unlock()
	t := time.NewTimer(min(d, PullBackoffMax))
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Close releases the puller's waker registration.
func (p *Puller) Close(ctx context.Context) error {
	p.stop()
	return nil
}
