package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
)

// AcceptWait is how long the gate waits for a kontra-lifecycle worker to accept an Update; a timeout is
// -32603 / HTTP 503 (spec §14.1). CancelWait bounds kontra.cancel once accepted (spec §8).
const (
	AcceptWait = 10 * time.Second
	CancelWait = 60 * time.Second
)

// Errors of the request-path calls. Each error Client returns wraps one of these and the a2a-go sentinel
// the caller is answered with, so callers test either.
var (
	// ErrRefused is an Update the workflow refused: its validator (spec §7's table on workflow state, a
	// work item already accepted, no review window open), or a workflow that has ended. It is answered
	// UnsupportedOperationError (wrapped a2a.ErrUnsupportedOperation).
	ErrRefused = errors.New("lifecycle: the task's lifecycle refused the update")
	// ErrClosed is a workflow that has ended (it also wraps ErrRefused): a resend of a finished task
	// re-issues the start and is answered with the task as it is (spec §14.5).
	ErrClosed = errors.New("lifecycle: the task's lifecycle has ended")
	// ErrUnavailable is an Update not accepted, or not completed, within its wait: no kontra-lifecycle
	// worker took it, or Temporal is unreachable. The caller gets -32603 (HTTP 503; it wraps
	// a2a.ErrInternalError), and a work item is kept: the workflow may still accept it (D66).
	ErrUnavailable = errors.New("lifecycle: the task's lifecycle did not answer in time")
)

func refused(msg string, cause error) error {
	return fmt.Errorf("%w: %w: %s", a2a.NewError(a2a.ErrUnsupportedOperation, msg), ErrRefused, causeText(cause))
}

func closed(cause error) error {
	return fmt.Errorf("%w: %w: %w: %s", a2a.NewError(a2a.ErrUnsupportedOperation, "the task takes no more messages"), ErrRefused, ErrClosed, causeText(cause))
}

func unavailable(cause error) error {
	return fmt.Errorf("%w: %w: %s", a2a.NewError(a2a.ErrInternalError, "the task's lifecycle is unavailable"), ErrUnavailable, causeText(cause))
}

func notCancelable(cause error) error {
	return fmt.Errorf("%w: %s", a2a.NewError(a2a.ErrTaskNotCancelable, "the task is no longer cancelable"), causeText(cause))
}

func causeText(err error) string {
	if err == nil {
		return "no cause"
	}
	return err.Error()
}

// Dial connects to the gate's own namespace (spec §14.4: no node or caller ever reaches it). The SDK logs
// through slog's default logger.
func Dial(ctx context.Context, address, namespace string) (client.Client, error) {
	if address == "" || namespace == "" {
		return nil, errors.New("lifecycle: Temporal needs an address and a namespace")
	}
	c, err := client.DialContext(ctx, client.Options{
		HostPort: address, Namespace: namespace, Logger: tlog.NewStructuredLogger(slog.Default()),
	})
	if err != nil {
		return nil, fmt.Errorf("lifecycle: dial Temporal at %s, namespace %s: %w", address, namespace, err)
	}
	return c, nil
}

// Client is the gate's request-path use of Temporal: the work queue, the cancel interceptor, the review
// interceptor, the push config store and the dispatch's kontra.dispatched signal go through it.
type Client struct {
	c client.Client
}

// NewClient wraps a Temporal client.
func NewClient(c client.Client) *Client {
	return &Client{c: c}
}

// Temporal is the wrapped client.
func (c *Client) Temporal() client.Client { return c.c }

// StartWithMessage is UpdateWithStart: start TaskLifecycle (conflict policy use-existing, reuse policy
// reject-duplicate) and send kontra.message with UpdateID = workItemID, waiting up to AcceptWait for its
// completion (the handler only queues the turn, so completion follows acceptance at once). Repeating it
// with the same work item id is idempotent (spec §14.5). A workflow that has ended (reject-duplicate) is
// ErrClosed; a refusal ErrRefused; no answer in time ErrUnavailable.
func (c *Client) StartWithMessage(ctx context.Context, in Input, workItemID string) (MessageResult, error) {
	if _, err := uuid.Parse(workItemID); err != nil {
		return MessageResult{}, fmt.Errorf("lifecycle: work item id %q: %w", workItemID, err)
	}
	ctx, cancel := context.WithTimeout(ctx, AcceptWait)
	defer cancel()
	op := c.c.NewWithStartWorkflowOperation(client.StartWorkflowOptions{
		ID:                       WorkflowID(a2a.TaskID(in.TaskID)),
		TaskQueue:                QueueLifecycle,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}, WorkflowType, in)
	h, err := c.c.UpdateWithStartWorkflow(ctx, client.UpdateWithStartWorkflowOptions{
		StartWorkflowOperation: op,
		UpdateOptions: client.UpdateWorkflowOptions{
			UpdateName: UpdateMessage, UpdateID: workItemID, Args: []any{MessageUpdate{WorkItemID: workItemID}},
			WaitForStage: client.WorkflowUpdateStageCompleted,
		},
	})
	var res MessageResult
	if err == nil {
		err = h.Get(ctx, &res)
	}
	if err != nil {
		return MessageResult{}, classify(err, "the task takes no message now")
	}
	return res, nil
}

// Message sends kontra.message for a follow-up (UpdateID = u.WorkItemID), waiting up to AcceptWait for its
// completion. A validator refusal, or a workflow that has ended, is an error wrapping
// a2a.ErrUnsupportedOperation (and ErrRefused); a refusal by a workflow that has not processed the turn end
// u.After names also wraps errBehind (Queue.message may send it again); no answer in time wraps
// ErrUnavailable.
func (c *Client) Message(ctx context.Context, taskID a2a.TaskID, u MessageUpdate) (MessageResult, error) {
	var res MessageResult
	err := c.update(ctx, AcceptWait, taskID, UpdateMessage, u.WorkItemID, u, &res)
	if err != nil {
		return MessageResult{}, classify(err, "the task takes no message now")
	}
	return res, nil
}

// ReviewMessage sends kontra.review_message and waits up to AcceptWait for its result (spec §7, §14.1),
// with relay.ReviewMessenger's error contract: a refusal (no window open, the work item already accepted,
// the workflow closed) wraps a2a.ErrUnsupportedOperation; anything else counts as a timeout.
func (c *Client) ReviewMessage(ctx context.Context, taskID a2a.TaskID, u ReviewMessageUpdate) (ReviewMessageResult, error) {
	var res ReviewMessageResult
	err := c.update(ctx, AcceptWait, taskID, UpdateReviewMessage, u.WorkItemID, u, &res)
	if err != nil {
		return ReviewMessageResult{}, classify(err, "no review window is open")
	}
	return res, nil
}

// update sends an Update and waits for its completion within wait.
func (c *Client) update(ctx context.Context, wait time.Duration, taskID a2a.TaskID, name, id string, arg, out any) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	h, err := c.c.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID: WorkflowID(taskID), UpdateName: name, UpdateID: id, Args: []any{arg},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return err
	}
	return h.Get(ctx, out)
}

// classify maps an Update's error: the workflow's own refusal (an application error from a validator or a
// handler), a workflow that has ended (not found, or the start refused as a duplicate of a closed run), or
// no answer in time.
func classify(err error, msg string) error {
	var (
		app     *temporal.ApplicationError
		nf      *serviceerror.NotFound
		started *serviceerror.WorkflowExecutionAlreadyStarted
	)
	switch {
	case errors.As(err, &app) && app.Type() == ErrTypeBehind:
		return fmt.Errorf("%w: %w", refused(msg, err), errBehind)
	case errors.As(err, &app):
		return refused(msg, err)
	case errors.As(err, &started), errors.As(err, &nf):
		return closed(err)
	}
	// Not accepted or not completed in time (client.WorkflowUpdateServiceTimeoutOrCanceledError), or
	// Temporal unreachable.
	return unavailable(err)
}

// Cancel sends kontra.cancel, waits up to AcceptWait for its acceptance and then up to CancelWait for its
// result; past it ok is false and the caller answers with the stored task as it is (spec §8). A task that
// has ended (the validator's refusal, or a workflow that has closed) is an error wrapping
// a2a.ErrTaskNotCancelable; a cancel not accepted in time wraps ErrUnavailable.
func (c *Client) Cancel(ctx context.Context, taskID a2a.TaskID) (res CancelResult, ok bool, err error) {
	id, err := uuid.NewV7()
	if err != nil {
		return CancelResult{}, false, err
	}
	actx, cancel := context.WithTimeout(ctx, AcceptWait)
	defer cancel()
	h, err := c.c.UpdateWorkflow(actx, client.UpdateWorkflowOptions{
		WorkflowID: WorkflowID(taskID), UpdateName: UpdateCancel, UpdateID: "cancel-" + id.String(),
		Args: []any{CancelUpdate{}}, WaitForStage: client.WorkflowUpdateStageAccepted,
	})
	if err != nil {
		return CancelResult{}, false, classifyCancel(err)
	}
	gctx, gcancel := context.WithTimeout(ctx, CancelWait)
	defer gcancel()
	err = h.Get(gctx, &res)
	switch {
	case err == nil:
		return res, true, nil
	case gctx.Err() != nil && ctx.Err() == nil:
		return CancelResult{}, false, nil
	}
	return CancelResult{}, false, classifyCancel(err)
}

func classifyCancel(err error) error {
	var (
		app *temporal.ApplicationError
		nf  *serviceerror.NotFound
	)
	switch {
	case errors.As(err, &app) && app.Type() == ErrTypeNotCancelable, errors.As(err, &nf):
		return notCancelable(err)
	case errors.As(err, &app):
		return refused("the task cannot be canceled now", err)
	}
	return unavailable(err)
}

// SignalPush sends kontra.push. A workflow that does not exist (yet, or any more) is not an error: the
// workflow lists push configs at start and after each dispatch (spec §10.2).
func (c *Client) SignalPush(ctx context.Context, taskID a2a.TaskID, s PushSignal) error {
	err := c.c.SignalWorkflow(ctx, WorkflowID(taskID), "", SignalPush, s)
	var nf *serviceerror.NotFound
	if errors.As(err, &nf) {
		return nil
	}
	return err
}

// SignalDispatched sends kontra.dispatched {n}.
func (c *Client) SignalDispatched(ctx context.Context, taskID a2a.TaskID, n int) error {
	return c.c.SignalWorkflow(ctx, WorkflowID(taskID), "", SignalDispatched, DispatchedSignal{N: n})
}

// WorkflowExists reports whether the task's workflow exists, running or closed, for the orphan sweep
// (spec §14.5).
func (c *Client) WorkflowExists(ctx context.Context, taskID a2a.TaskID) (bool, error) {
	_, err := c.c.DescribeWorkflowExecution(ctx, WorkflowID(taskID), "")
	var nf *serviceerror.NotFound
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &nf):
		return false, nil
	}
	return false, err
}
