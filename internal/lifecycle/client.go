package lifecycle

import (
	"context"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.temporal.io/sdk/client"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// AcceptWait is how long the gate waits for a kontra-lifecycle worker to accept an Update; a timeout is
// -32603 / HTTP 503 (spec §14.1). CancelWait bounds kontra.cancel once accepted (spec §8).
const (
	AcceptWait = 10 * time.Second
	CancelWait = 60 * time.Second
)

// Dial connects to the gate's own namespace (spec §14.4: no node or caller ever reaches it).
func Dial(ctx context.Context, address, namespace string) (client.Client, error) {
	return nil, unimpl.Error("lifecycle.Dial", "WP-05")
}

// Client is the gate's request-path use of Temporal: the work queue, the cancel interceptor, the push
// config store and the dispatch's kontra.dispatched signal go through it.
type Client struct {
	c client.Client
}

// NewClient wraps a Temporal client.
func NewClient(c client.Client) *Client {
	return &Client{c: c}
}

// StartWithMessage is UpdateWithStart: start TaskLifecycle (conflict policy use-existing, reuse policy
// reject-duplicate) and send kontra.message with UpdateID = workItemID, waiting for acceptance. Repeating
// it with the same work item id is idempotent (spec §14.5).
func (c *Client) StartWithMessage(ctx context.Context, in Input, workItemID string) (MessageResult, error) {
	return MessageResult{}, unimpl.Error("lifecycle.Client.StartWithMessage", "WP-05")
}

// Message sends kontra.message for a follow-up. A validator refusal is an error wrapping
// a2a.ErrUnsupportedOperation.
func (c *Client) Message(ctx context.Context, taskID a2a.TaskID, workItemID string) (MessageResult, error) {
	return MessageResult{}, unimpl.Error("lifecycle.Client.Message", "WP-05")
}

// Cancel sends kontra.cancel and waits up to CancelWait; past it ok is false and the caller answers with
// the stored task as it is (spec §8).
func (c *Client) Cancel(ctx context.Context, taskID a2a.TaskID) (res CancelResult, ok bool, err error) {
	return CancelResult{}, false, unimpl.Error("lifecycle.Client.Cancel", "WP-05")
}

// SignalPush sends kontra.push. A workflow that does not exist yet is not an error: the workflow lists
// push configs at start (spec §10.2).
func (c *Client) SignalPush(ctx context.Context, taskID a2a.TaskID, s PushSignal) error {
	return unimpl.Error("lifecycle.Client.SignalPush", "WP-05")
}

// SignalDispatched sends kontra.dispatched {n}.
func (c *Client) SignalDispatched(ctx context.Context, taskID a2a.TaskID, n int) error {
	return unimpl.Error("lifecycle.Client.SignalDispatched", "WP-05")
}

// WorkflowExists reports whether the task's workflow exists, for the orphan sweep (spec §14.5).
func (c *Client) WorkflowExists(ctx context.Context, taskID a2a.TaskID) (bool, error) {
	return false, unimpl.Error("lifecycle.Client.WorkflowExists", "WP-05")
}
