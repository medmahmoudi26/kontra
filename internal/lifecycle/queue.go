package lifecycle

import (
	"context"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"

	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// ResendWindow is how young a first task must be for a resend to be answered with it (spec §15).
const ResendWindow = 24 * time.Hour

// Queue is the Temporal-backed a2a-go workqueue.Queue for one service (spec §8.1, §15, D6). It wraps
// workqueue.NewPushQueue with Write below; the HandlerFn a2a-go registers is what the dispatch activity
// calls, with a Heartbeater attached. The handler's concurrency limiter stays off: the Temporal worker's
// activity slots bound concurrency.
type Queue struct {
	svc     store.ServiceRef
	log     *store.Log
	client  *Client
	inner   workqueue.Queue
	handler workqueue.HandlerFn
}

var _ workqueue.Queue = (*Queue)(nil)

// NewQueue returns the queue for svc.
func NewQueue(svc store.ServiceRef, log *store.Log, c *Client) *Queue {
	return &Queue{svc: svc, log: log, client: c}
}

// Write never puts a2a-go's payload into Temporal: the payload carries the whole request and every
// inbound header, Authorization and cookies included.
//
// A new task: answer a resend (same principal, service, messageId and request hash, first task younger
// than ResendWindow) with the existing task and re-issue StartWithMessage with its original work item id;
// otherwise, in one transaction, pre-create the gate task (gate ids, submitted), save the inline push
// config whose URL the push interceptor already checked (spec §10.1), and store the sanitized payload as a
// work item; after commit, StartWithMessage; set payload.TaskID and return it.
//
// A follow-up: store the work item and send kontra.message; a refusal is a2a.ErrUnsupportedOperation.
//
// A cancel payload is never written: CancelTask is decided by the cancel interceptor before a2a-go's
// executor sees it (D4).
func (q *Queue) Write(ctx context.Context, p *workqueue.Payload) (a2a.TaskID, error) {
	return "", unimpl.Error("lifecycle.Queue.Write", "WP-05")
}

// RegisterHandler implements workqueue.Queue; a2a-go calls it once per handler.
func (q *Queue) RegisterHandler(cfg workqueue.HandlerConfig, fn workqueue.HandlerFn) {
	panic(unimpl.Error("lifecycle.Queue.RegisterHandler", "WP-05"))
}

// Handler is the HandlerFn the dispatch activity runs for this service.
func (q *Queue) Handler() workqueue.HandlerFn {
	return q.handler
}

// Sanitize reduces a2a-go's payload to what work_items may hold: the request and the call context with
// every service parameter removed except A2A-Version and A2A-Extensions, plus the owner.
func Sanitize(p *workqueue.Payload, o store.Owner) (store.WorkPayload, error) {
	return store.WorkPayload{}, unimpl.Error("lifecycle.Sanitize", "WP-05")
}

// Rebuild turns a stored work item back into the payload a2a-go's HandlerFn expects.
func Rebuild(taskID a2a.TaskID, wi *store.WorkItem) (*workqueue.Payload, error) {
	return nil, unimpl.Error("lifecycle.Rebuild", "WP-05")
}

// StreamStart is filled by Write for the gate's SSE writer, which attaches it to the request context
// before asking a2a-go to create or continue the task: the SSE stream starts after Version, the task's
// version when the kontra.message Update was accepted (spec §9).
type StreamStart struct {
	TaskID  a2a.TaskID
	Version int64
	New     bool
}

type streamStartKey struct{}

// WithStreamStart returns ctx carrying s for Write to fill.
func WithStreamStart(ctx context.Context, s *StreamStart) context.Context {
	return context.WithValue(ctx, streamStartKey{}, s)
}

// Heartbeater attaches to the dispatch context (workqueue.AttachHeartbeater): every 5 s it records an
// activity heartbeat and returns the context's error, so an abort stops the execution.
type Heartbeater struct{}

var _ workqueue.Heartbeater = Heartbeater{}

// HeartbeatInterval implements workqueue.Heartbeater (spec §6.2: every 5 s, timeout 30 s).
func (Heartbeater) HeartbeatInterval() time.Duration { return 5 * time.Second }

// Heartbeat implements workqueue.Heartbeater.
func (Heartbeater) Heartbeat(ctx context.Context) error {
	return unimpl.Error("lifecycle.Heartbeater.Heartbeat", "WP-05")
}
