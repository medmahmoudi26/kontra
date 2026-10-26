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
//
// A follow-up turn's dispatch also arms the fence with its work item when the upstream first answers the
// follow-up (ArmDelivery): the next append under the fence that commits then sets that item's
// work_items.delivered_at in the same statement (spec §6.4 "Follow-up delivery", D81).
type Fence struct {
	TaskID     uuid.UUID
	N          int
	DispatchID uuid.UUID

	fenced   atomic.Bool
	delivery atomic.Pointer[uuid.UUID] // the armed follow-up work item; nil when none
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

// ArmDelivery makes the next append under this fence that commits also set work_items.delivered_at of the
// follow-up work item workItemID, in the same statement (D81). A dispatch arms it when the upstream first
// answers the follow-up's SendMessage or SendStreamingMessage, before it yields anything from that answer.
// A fenced append writes nothing, so the delivery stays armed; one that commits disarms it.
func (f *Fence) ArmDelivery(workItemID uuid.UUID) {
	if f != nil && workItemID != uuid.Nil {
		id := workItemID
		f.delivery.Store(&id)
	}
}

// DisarmDelivery withdraws the armed delivery no append has written yet and returns its work item, uuid.Nil
// when none is armed (none was, or an append wrote it). The dispatch then writes it itself (MarkDelivered)
// when the upstream's answer had nothing to log.
func (f *Fence) DisarmDelivery() uuid.UUID {
	if f == nil {
		return uuid.Nil
	}
	if p := f.delivery.Swap(nil); p != nil {
		return *p
	}
	return uuid.Nil
}

// DeliveryArmed reports whether a delivery is armed and not written yet.
func (f *Fence) DeliveryArmed() bool {
	return f != nil && f.delivery.Load() != nil
}

// armedDelivery returns the armed delivery for an append to carry: the pointer to disarm once the append
// commits, and its work item (uuid.Nil when none is armed).
func (f *Fence) armedDelivery() (*uuid.UUID, uuid.UUID) {
	if f == nil {
		return nil, uuid.Nil
	}
	p := f.delivery.Load()
	if p == nil {
		return nil, uuid.Nil
	}
	return p, *p
}

// deliveryWritten disarms p once an append carrying it committed, unless the delivery was armed again
// meanwhile.
func (f *Fence) deliveryWritten(p *uuid.UUID) {
	if f != nil && p != nil {
		f.delivery.CompareAndSwap(p, nil)
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
