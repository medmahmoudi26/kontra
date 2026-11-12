package relay

import (
	"context"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/store"
)

// resendAnswer answers a blocking SendMessage that repeats an earlier request (a resend: the same principal,
// service, messageId and request, spec §15) whose task stands at input-required or auth-required, with that task as
// it stands (spec §9: a blocking call returns on the first input-required or auth-required state; D290, the blocking
// counterpart of D108's resend stream, which closes at once on the same states).
//
// Without it the work queue answers the resend with its task and a2a-go subscribes to that task's log at its
// tail: the turn the request started ended before the subscription, nothing more of it will come, and the call
// waits until the task's next turn, its deadline or the caller's timeout.
// The pinned A2A TCK found it: its message ids are per session, not per call (a2a-tck tck/requirements/base.py,
// tck_id), so every prerequisite input-required task after the first is a resend, and each such SendMessage hung
// for the TCK's 30 s read timeout (M4 WP-02).
//
// A task reaches input-required or auth-required only by a turn its workflow ran, so the start that the work queue
// re-issues for a resend (spec §14.5) is moot there. A resend of a terminal task is answered by a2a-go itself (its subscription
// ends at a terminal snapshot) after the work queue's re-issue, and one of a submitted or working task goes through
// the work queue (a lost start is re-issued, and the call waits for the turn). It runs after Admission and the other
// checks, as the work queue's own resend lookup does, and its lookup is the work queue's (store.Log.FindResend, the
// request's hash as the interceptors before it left it).
//
// A resend of a message answered with the callee's Message (a provisional record that ended, D362) is answered with
// that same Message, without the work queue: nothing is dispatched again.
func (i *Interceptors) resendAnswer(ctx context.Context, cc *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	if cc.Method() != "SendMessage" {
		return ctx, nil, nil // a stream of a resend: D108 (internal/sse)
	}
	r, ok := req.Payload.(*a2a.SendMessageRequest)
	if !ok || r == nil || r.Message == nil || r.Message.TaskID != "" || r.Message.ID == "" {
		return ctx, nil, nil
	}
	hash, err := store.RequestHashOf(ctx, r)
	if err != nil {
		return ctx, nil, err
	}
	o, err := CallerOwner(ctx)
	if err != nil {
		return ctx, nil, err
	}
	prev, err := i.Log.FindResend(ctx, i.Svc, o, r.Message.ID, hash, time.Now().Add(-lifecycle.ResendWindow))
	if err != nil || prev == nil || prev.Snapshot == nil {
		return ctx, nil, err
	}
	if msg, ok := store.MessageAnswer(prev); ok {
		// D362: the earlier request was answered with the callee's Message, and its record has ended: the resend gets
		// the same Message, and nothing is dispatched again.
		if msg, err = i.Render.Message(ctx, prev.ID, msg); err != nil {
			return ctx, nil, a2a.NewError(a2a.ErrInternalError, "the message's parts could not be rendered")
		}
		msg.TaskID = ""
		return ctx, msg, nil
	}
	if prev.ExposedAt == nil {
		return ctx, nil, nil // still provisional: the work queue answers it, and finishedTurn settles the answer
	}
	switch prev.State {
	case store.StateInputRequired, store.StateAuthRequired:
		return ctx, prev.Snapshot, nil
	}
	return ctx, nil, nil
}
