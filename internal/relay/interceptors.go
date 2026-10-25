package relay

import (
	"context"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/medmahmoudi26/kontra/internal/hooks"
	"github.com/medmahmoudi26/kontra/internal/lifecycle"
	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Canceller is the kontra.cancel Update; lifecycle.Client implements it.
type Canceller interface {
	Cancel(ctx context.Context, taskID a2a.TaskID) (lifecycle.CancelResult, bool, error)
}

// Interceptors builds the call interceptors of one service, in order (spec §15):
//
//  1. Identity: CallContext.User from the edge's principal (Name = buyer id, attributes: principal, via,
//     tenant), so the store scopes reads and the identity crosses the work queue.
//  2. Anonymous refusals (spec §2.3): ListTasks and push configs are UnsupportedOperationError for the
//     anonymous principal.
//  3. Caller keys (spec §2.5): every kontra. key leaves message, part and request metadata, except the
//     sealing keys and caller_metadata.
//  4. Admission (spec §13.1) on SendMessage for a new task: the built-in allow in M1.
//  5. Follow-up checks (spec §7): a message with a taskId is refused UnsupportedOperationError unless the
//     task is input-required; a taskId/contextId disagreement is InvalidParams; a terminal task is
//     UnsupportedOperationError (a2a-go itself answers InvalidParams there); a review-window message is
//     stored as a work item and sent as kontra.review_message, answered here as an early response.
//  6. Inline push config (spec §10.1): the URL is checked now, so a refused URL is a synchronous
//     InvalidParams and nothing is stored; the work queue saves it with the task.
//  7. CancelInterceptor.
type Interceptors struct {
	Svc      store.ServiceRef
	Log      *store.Log
	Rules    hooks.KeyRules
	Admit    hooks.Admission
	Canceler Canceller
}

// Chain returns the interceptors in order.
func (i *Interceptors) Chain() []a2asrv.CallInterceptor {
	panic(unimpl.Error("relay.Interceptors.Chain", "WP-04"))
}

// CancelInterceptor answers CancelTask itself (spec §8, D4): a terminal task is TaskNotCancelableError;
// otherwise the kontra.cancel Update, awaited up to 60 s, whose outcome is the canceled task, the
// upstream's error while the task continues, or past 60 s the stored task as it is.
type CancelInterceptor struct {
	a2asrv.PassthroughCallInterceptor
	Log      *store.Log
	Canceler Canceller
}

var _ a2asrv.CallInterceptor = (*CancelInterceptor)(nil)

// Before implements a2asrv.CallInterceptor.
func (c *CancelInterceptor) Before(ctx context.Context, callCtx *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	return ctx, nil, unimpl.Error("relay.CancelInterceptor.Before", "WP-04")
}
