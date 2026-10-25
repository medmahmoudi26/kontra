package store

import (
	"context"
	"sync/atomic"

	"github.com/google/uuid"
)

// Fence is one dispatch's write fence (spec §6.4, D77). A dispatch (a new attempt, a follow-up turn on the
// same attempt, a resume) gets it from Log.BeginDispatch, which writes a fresh attempts.dispatch_id, and
// attaches it to the context it runs a2a-go's handler with (WithFence). Every append made under that
// context commits only if, in the same statement, attempt N is the task's latest attempt, has ended_at
// null and still holds DispatchID; otherwise the write is ErrFenced and the fence records it (Fenced),
// so the dispatch can stop. A newer dispatch therefore fences an older one that Temporal timed out but
// whose process is still alive, on another replica or on the same one.
type Fence struct {
	TaskID     uuid.UUID
	N          int
	DispatchID uuid.UUID

	fenced atomic.Bool
}

// Fenced reports whether a write under this fence has been refused because a newer dispatch holds the
// attempt. Once true it stays true.
func (f *Fence) Fenced() bool {
	return f != nil && f.fenced.Load()
}

func (f *Fence) markFenced() {
	if f != nil {
		f.fenced.Store(true)
	}
}

type fenceKey struct{}

// WithFence returns ctx carrying f: Store.Update fences every write made under it.
func WithFence(ctx context.Context, f *Fence) context.Context {
	return context.WithValue(ctx, fenceKey{}, f)
}

// FenceFrom returns the fence ctx carries, nil when none.
func FenceFrom(ctx context.Context) *Fence {
	f, _ := ctx.Value(fenceKey{}).(*Fence)
	return f
}
