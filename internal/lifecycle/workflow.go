package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/medmahmoudi26/kontra/internal/hooks"
	"github.com/medmahmoudi26/kontra/internal/ids"
	"github.com/medmahmoudi26/kontra/internal/store"
)

// Activity timings (spec §6.2, §8, §10.2, §13.3, §14.2).
const (
	dispatchScheduleToStart   = 15 * time.Second
	dispatchHeartbeat         = 30 * time.Second
	dispatchSlack             = 30 * time.Second // start-to-close = time to the deadline + 30 s
	ctlScheduleToStart        = 10 * time.Second
	abortStartToClose         = 10 * time.Second
	forwardCancelStartToClose = 30 * time.Second
	lifecycleStartToClose     = 10 * time.Second
	lifecycleMaxInterval      = 30 * time.Second
	hookStartToClose          = time.Minute
	hookMaxInterval           = 5 * time.Minute
	reviewMessageStartToClose = 8 * time.Second
	pushHeartbeat             = 60 * time.Second
	pushMaxInterval           = 5 * time.Minute
	pushBudget                = 24 * time.Hour
	maxReviewWindow           = 604800 * time.Second
	maxResumeBackoff          = 30 * time.Second
	maxRetryShift             = 20 // 2^20 s: the deadline caps a retry's wait long before
	listBudget                = 5 * time.Minute
)

// Behaviour changes behind workflow.GetVersion (spec §14.4, D46). A history recorded before a change
// replays with workflow.DefaultVersion and keeps the earlier behaviour.
const (
	// changeListPushAtStart: list_push_configs runs at the workflow's start, not only after the first
	// prepare (spec §10.2). Before it, a task that ended before its first prepare pushed nothing.
	changeListPushAtStart = "list-push-at-start"
	// changeDispatchNotStarted: a dispatch that was never started (schedule-to-start timeout) after the
	// attempt's first upstream task id is dispatched again as it was, never turned into a resume.
	changeDispatchNotStarted = "dispatch-not-started"
	// changePushRestart: a push_webhook that ended because its config was deleted or marked failed gives
	// the config's place back, and the task's configs are listed again. Before it, a config created again
	// with the same id, or a failed config set again, never got another push_webhook in the run.
	changePushRestart = "push-restart"
)

// phase is the task's state as the workflow knows it, from its own Updates and activity results.
type phase int

const (
	phaseQuoted   phase = iota // waiting for kontra.approval or the quote's expiry
	phaseNew                   // submitted, its first turn not dispatched yet
	phaseRunning               // a turn is dispatched, retried or re-attached
	phaseWaiting               // input-required: waits for the next kontra.message
	phaseTerminal              // the terminal event is in the log
)

// call is one dispatch activity.
type call struct {
	cancel workflow.CancelFunc
	resume bool // dispatched with resume (D42)
	done   bool
	res    DispatchResult
	err    error
}

// cancelOp is one kontra.cancel in flight; concurrent cancels share it (spec §8).
type cancelOp struct {
	started time.Time
	done    bool
	res     CancelResult
	err     error
}

// approvalOp is one kontra.approval in flight.
type approvalOp struct {
	u    ApprovalUpdate
	done bool
	res  ApprovalResult
	err  error
}

// heldPatch is an operator patch waiting for no dispatch, abort or forward_cancel to be in flight (spec
// §13.3 "Ordering").
type heldPatch struct {
	source string
	patch  hooks.Patch
}

// stepKind is what a turn does next.
type stepKind int

const (
	stepDispatch stepKind = iota // dispatch the current attempt
	stepRetry                    // prepare a new attempt, then dispatch it
	stepResume                   // re-attach to the attempt's upstream task (D42)
	stepAgain                    // dispatch again what was never started, resume or not
	stepDone                     // the turn ended: input-required, or the task is terminal
)

type step struct {
	kind   stepKind
	wait   time.Duration
	prev   string // the outcome prepare records on the attempt a lost dispatch left open
	resume bool   // stepAgain: the dispatch that was not started was a resume
}

// lc is one run of TaskLifecycle. Every field is workflow state: it changes only in workflow code, so a
// replay rebuilds it from history.
type lc struct {
	in     Input
	logger log.Logger

	phase     phase
	gone      bool // the task row does not exist: nothing is written, no hook runs
	deadline  time.Time
	timerSet  bool
	timerStop workflow.CancelFunc
	timedOut  bool

	n       int    // the current attempt, 0 before the first prepare
	queue   string // its dispatch queue
	backend string
	resumes int // successive resume (or not started) dispatches of the current attempt in this turn

	turns    int             // kontra.message Updates accepted (the turn numbers)
	started  int             // turns started
	accepted map[string]bool // work items accepted in this run (D66)
	pending  []string        // accepted turns not started yet (at most one)
	answered int64           // MessageUpdate.After of the last follow-up accepted: the turn end it answered

	listedAtStart bool // list_push_configs ran at the start (changeListPushAtStart)

	disp     *call
	ctl      int // abort and forward_cancel in flight
	applying bool
	held     []heldPatch

	cancel       *cancelOp
	approval     *approvalOp
	quoteExpired bool

	windowDecided  bool // ReviewWindow answered (or will never be asked)
	reviewOpen     bool
	reviewInFlight int
	disputed       bool
	closed         bool

	pushes       map[string]bool // configs whose push_webhook this run started
	pushPending  int
	listing      int // list_push_configs in flight
	hooksPending int
}

// TaskLifecycle is the per-task workflow, id task-<task_id>, on kontra-lifecycle (spec §14.1).
//
// Sequence: (quoted: wait for kontra.approval or the quote's expiry) → attempt loop → terminal →
// OnTerminal → (review window; never open in open source) → the end, once no push_webhook it started is
// pending (D78). The task.timeout is a workflow timer at the deadline the store fixed at submission, never
// Temporal's execution timeout. Each turn is one dispatch; interrupted (input-required) waits for the next
// kontra.message. On a dispatch failure the workflow reads attempt_state (Postgres, not heartbeat details)
// and applies the retry table of spec §6.4: retry with a new attempt before the first upstream task id;
// after it, a direct backend is re-attached with a new dispatch of the same attempt, backing off and
// capped (D42), and a tunnel backend fails tunnel_lost. Patches and annotations that arrive while a
// dispatch, abort or forward_cancel runs are held and applied when it returns (§13.3). Every behaviour
// change goes behind workflow.GetVersion and is replay-tested (spec §14.4, D46).
//
// Events never enter its history: inputs, Updates, signals and activity results carry ids, states, small
// outcomes and operator patches of at most 16 KiB (D59).
func TaskLifecycle(ctx workflow.Context, in Input) error {
	if _, err := ids.ParseTaskID(in.TaskID); err != nil {
		return temporal.NewNonRetryableApplicationError("TaskLifecycle: not a gate task id", ErrTypeTaskGone, nil)
	}
	l := &lc{in: in, logger: workflow.GetLogger(ctx), accepted: map[string]bool{}, pushes: map[string]bool{}}
	if l.in.Retries < 0 {
		l.in.Retries = 0
	}
	l.phase = phaseNew
	if in.QuoteExpiresAt != nil {
		l.phase = phaseQuoted
	}
	if err := l.register(ctx); err != nil {
		return err
	}
	workflow.Go(ctx, l.signals)
	workflow.Go(ctx, l.patcher)
	// A new task's inline push config is saved without kontra.push (Queue.Write): this listing is what
	// starts its push_webhook, whatever happens to the task before its first dispatch (spec §10.2).
	if workflow.GetVersion(ctx, changeListPushAtStart, workflow.DefaultVersion, 1) >= 1 {
		l.listedAtStart = true
		l.listPush(ctx)
	}
	if l.phase == phaseQuoted {
		l.quoted(ctx)
	}
	if l.phase != phaseTerminal {
		l.active(ctx)
	}
	l.ended(ctx)
	return ctx.Err()
}

// register sets the Update handlers before the workflow first blocks: the first kontra.message arrives
// with the start (UpdateWithStart). Validators read workflow state only (D66).
func (l *lc) register(ctx workflow.Context) error {
	for _, u := range []struct {
		name      string
		handler   any
		validator any
	}{
		{UpdateMessage, l.onMessage, l.validateMessage},
		{UpdateCancel, l.onCancel, l.validateCancel},
		{UpdateApproval, l.onApproval, l.validateApproval},
		{UpdateReviewMessage, l.onReviewMessage, l.validateReviewMessage},
		{UpdateAnnotate, l.onAnnotate, l.validateAnnotate},
	} {
		if err := workflow.SetUpdateHandlerWithOptions(ctx, u.name, u.handler, workflow.UpdateHandlerOptions{Validator: u.validator}); err != nil {
			return err
		}
	}
	return nil
}

func refusal(msg string) error { return temporal.NewApplicationError(msg, ErrTypeRefused) }

func notCancelableErr(state string) error {
	return temporal.NewApplicationError("the task is "+state, ErrTypeNotCancelable)
}

func isType(err error, t string) bool {
	var ae *temporal.ApplicationError
	return errors.As(err, &ae) && ae.Type() == t
}

func isTimeout(err error, t enums.TimeoutType) bool {
	var te *temporal.TimeoutError
	return errors.As(err, &te) && te.TimeoutType() == t
}

func terminalState(s string) bool { return store.State(s).Terminal() }

func (l *lc) rules() hooks.KeyRules {
	return hooks.KeyRules{OperatorPrefixes: l.in.Hooks.OperatorPrefixes}
}

func (l *lc) cancelPending() bool { return l.cancel != nil && !l.cancel.done }

func (l *lc) busy() bool { return (l.disp != nil && !l.disp.done) || l.ctl > 0 }

// --- Updates ---

// validateMessage is §7's table on workflow state (D66): refused unless the task is new (or quoted, its
// first message) or waits at input-required, no turn is running or queued, no cancel is in flight, and
// the work item has not been accepted in this run.
//
// A refusal because a turn is running is final (ErrTypeRefused) when the caller read no turn end later
// than the one the last accepted follow-up answered: another follow-up of the same turn end was accepted
// first (spec §7: two racing follow-ups cannot both be accepted). It is ErrTypeBehind when the caller
// read a later turn end (MessageUpdate.After): the running turn has logged its end and the workflow has
// not processed the dispatch's return yet, and the work queue may send the Update again (Queue.message).
func (l *lc) validateMessage(ctx workflow.Context, u MessageUpdate) error {
	switch {
	case u.WorkItemID == "":
		return refusal("a message needs its work item")
	case l.accepted[u.WorkItemID]:
		return refusal("this work item was already accepted")
	case l.phase == phaseTerminal:
		return refusal("the task has ended")
	case len(l.pending) > 0, l.phase == phaseRunning:
		if u.After > l.answered {
			return temporal.NewApplicationError("a turn is running; the task's lifecycle has not seen it end", ErrTypeBehind)
		}
		return refusal("a turn is running; the task takes no input until it asks for some")
	case l.cancelPending():
		return refusal("the task is being canceled")
	case l.turns == 0:
		return nil // the task's first message, with the start (new or quoted)
	case l.phase == phaseWaiting:
		return nil
	case l.phase == phaseQuoted:
		return refusal("a quoted task is accepted on the operator's page, not in the protocol")
	}
	return refusal("the task takes no input now")
}

// onMessage queues the turn and returns its number; the turn's first dispatch consumes the work item
// (D66). It never blocks, so the client's wait for completion is a wait for acceptance.
func (l *lc) onMessage(ctx workflow.Context, u MessageUpdate) (MessageResult, error) {
	l.accepted[u.WorkItemID] = true
	l.answered = max(l.answered, u.After)
	l.turns++
	l.pending = append(l.pending, u.WorkItemID)
	return MessageResult{Turn: l.turns}, nil
}

// validateCancel refuses only a task that has ended (spec §14.1).
func (l *lc) validateCancel(ctx workflow.Context, u CancelUpdate) error {
	if l.phase == phaseTerminal {
		return notCancelableErr("terminal")
	}
	return nil
}

// onCancel joins the cancel in flight, or starts one, and waits for its result (spec §8: concurrent
// cancels are coalesced; one after it completed starts anew).
func (l *lc) onCancel(ctx workflow.Context, u CancelUpdate) (CancelResult, error) {
	if l.cancel == nil || l.cancel.done {
		l.cancel = &cancelOp{started: workflow.Now(ctx)}
	}
	op := l.cancel
	if err := workflow.Await(ctx, func() bool { return op.done }); err != nil {
		return CancelResult{}, err
	}
	return op.res, op.err
}

// validateApproval refuses unless the task is quoted and its quote has not expired, and refuses metadata
// outside the §13.2 rules.
func (l *lc) validateApproval(ctx workflow.Context, u ApprovalUpdate) error {
	if l.phase != phaseQuoted || l.quoteExpired || (l.approval != nil && !l.approval.done) {
		return refusal("the task is not quoted")
	}
	if err := l.rules().CheckOperatorMaps(u.Metadata, u.MCPMeta); err != nil {
		return refusal(err.Error())
	}
	return nil
}

func (l *lc) onApproval(ctx workflow.Context, u ApprovalUpdate) (ApprovalResult, error) {
	op := &approvalOp{u: u}
	l.approval = op
	if err := workflow.Await(ctx, func() bool { return op.done }); err != nil {
		return ApprovalResult{}, err
	}
	return op.res, op.err
}

// validateReviewMessage refuses once the review window is decided closed (or a dispute closed it), and a
// work item already accepted in this run; the caller_metadata keys are at most 16 KiB (D59).
//
// SPEC-CONFLICT: spec §14.1's validator refuses "unless the window is open". The caller sends a review
// message once it read completed from the log, which can be before the workflow learned that the turn
// ended (the dispatch returns a moment after logging it) and before OnTerminal and ReviewWindow ran (a
// remote hook can take seconds): read literally, a message sent into a window about to open would be
// refused. The closest faithful behaviour: before ReviewWindow has answered, the message is accepted and
// its handler waits for the window; a window that does not open answers accepted: false, which the caller
// sees exactly as a refusal (the interceptor deletes the work item and answers UnsupportedOperationError).
func (l *lc) validateReviewMessage(ctx workflow.Context, u ReviewMessageUpdate) error {
	switch {
	case (l.windowDecided && !l.reviewOpen) || l.disputed:
		return refusal("no review window is open")
	case u.WorkItemID == "" || l.accepted[u.WorkItemID]:
		return refusal("this work item was already accepted")
	}
	if b, err := json.Marshal(u.Metadata); err != nil || len(b) > hooks.MaxMapBytes {
		return refusal("caller metadata over 16 KiB")
	}
	return nil
}

// onReviewMessage runs OnReviewMessage (8 s, no retry; a failure is accept: false); on accept it appends
// the message from the work item, consuming it in the same transaction, applies the hook's patch and
// dispute flag, and returns the seq (spec §7, §14.1, D66). A window that does not open answers
// accepted: false.
func (l *lc) onReviewMessage(ctx workflow.Context, u ReviewMessageUpdate) (ReviewMessageResult, error) {
	l.accepted[u.WorkItemID] = true
	if err := workflow.Await(ctx, func() bool { return l.windowDecided }); err != nil {
		return ReviewMessageResult{}, err
	}
	if !l.reviewOpen || l.disputed {
		return ReviewMessageResult{Accepted: false}, nil
	}
	l.reviewInFlight++
	defer func() { l.reviewInFlight-- }()
	ref, ok := l.taskRef(ctx)
	if !ok {
		return ReviewMessageResult{Accepted: false}, nil
	}
	md := u.Metadata
	if md == nil {
		md = map[string]any{}
	}
	// Bounded from its scheduling, not only from its start: on a remote hook queue nobody polls, the
	// activity never starts, and its timeout is the answer accept: false (spec §14.1).
	hctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue: l.in.Hooks.hooksQueue(), StartToCloseTimeout: reviewMessageStartToClose,
		ScheduleToCloseTimeout: reviewMessageStartToClose, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	var hr hooks.ReviewMessageResult
	err := workflow.ExecuteActivity(hctx, HookOnReviewMessage, ref,
		hooks.ReviewMessageArgs{WorkItemID: u.WorkItemID, MessageID: u.MessageID, Metadata: md}).Get(ctx, &hr)
	if err != nil || !hr.Accept {
		return ReviewMessageResult{Accepted: false}, nil
	}
	var seq int64
	if err := l.call(ctx, ActAppendReviewMessage, &seq, ReviewAppendInput{TaskID: l.in.TaskID, WorkItemID: u.WorkItemID}); err != nil {
		return ReviewMessageResult{Accepted: false}, nil
	}
	if hr.Patch != nil {
		l.apply(ctx, HookOnReviewMessage, *hr.Patch)
	}
	if hr.Dispute {
		l.disputed = true
	}
	return ReviewMessageResult{Accepted: true, Seq: &seq}, nil
}

// AnnotateResult is kontra.annotate's result, {}.
type AnnotateResult struct{}

func (l *lc) validateAnnotate(ctx workflow.Context, u AnnotateUpdate) error {
	if err := l.rules().CheckOperatorMaps(u.Metadata, u.MCPMeta); err != nil {
		return refusal(err.Error())
	}
	return nil
}

// onAnnotate holds the patch; the patcher applies it once no dispatch, abort or forward_cancel is in
// flight (spec §13.3 "Ordering").
func (l *lc) onAnnotate(ctx workflow.Context, u AnnotateUpdate) (AnnotateResult, error) {
	l.held = append(l.held, heldPatch{source: UpdateAnnotate, patch: hooks.Patch{Metadata: u.Metadata, MCPMeta: u.MCPMeta}})
	return AnnotateResult{}, nil
}

// --- signals, patches, pushes, hooks ---

// signals receives kontra.push, kontra.dispatched, kontra.close and kontra.input_response.
func (l *lc) signals(ctx workflow.Context) {
	stop := false
	sel := workflow.NewSelector(ctx)
	sel.AddReceive(workflow.GetSignalChannel(ctx, SignalPush), func(c workflow.ReceiveChannel, _ bool) {
		var s PushSignal
		c.Receive(ctx, &s)
		if !s.Deleted {
			// A deleted config's push_webhook ends by itself (spec §10.2).
			l.startPush(ctx, s.ConfigID)
		}
	})
	sel.AddReceive(workflow.GetSignalChannel(ctx, SignalDispatched), func(c workflow.ReceiveChannel, _ bool) {
		var s DispatchedSignal
		c.Receive(ctx, &s)
		n := s.N
		l.hooksPending++
		workflow.Go(ctx, func(gc workflow.Context) {
			defer func() { l.hooksPending-- }()
			if ref, ok := l.taskRef(gc); ok {
				_ = l.hook(gc, HookOnDispatched, ref, hooks.DispatchedArgs{Attempt: n}, nil)
			}
		})
	})
	sel.AddReceive(workflow.GetSignalChannel(ctx, SignalClose), func(c workflow.ReceiveChannel, _ bool) {
		var s struct{}
		c.Receive(ctx, &s)
		l.closed = true
	})
	sel.AddReceive(workflow.GetSignalChannel(ctx, SignalInputResponse), func(c workflow.ReceiveChannel, _ bool) {
		// MCP (M3): the running dispatch writes the response upstream; the signal only updates state.
		var s InputResponseSignal
		c.Receive(ctx, &s)
	})
	sel.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) { stop = true })
	for !stop {
		sel.Select(ctx)
	}
}

// patcher applies held patches one at a time, whenever nothing that holds the task is in flight.
func (l *lc) patcher(ctx workflow.Context) {
	for {
		if err := workflow.Await(ctx, func() bool { return len(l.held) > 0 && !l.busy() }); err != nil {
			return
		}
		p := l.held[0]
		l.held = l.held[1:]
		l.applying = true
		l.apply(ctx, p.source, p.patch)
		l.applying = false
	}
}

// apply logs one metadata event through apply_metadata; a refused patch is dropped (spec §13.3).
func (l *lc) apply(ctx workflow.Context, source string, p hooks.Patch) {
	if len(p.Metadata) == 0 && len(p.MCPMeta) == 0 {
		return
	}
	var seq int64
	if err := l.call(ctx, ActApplyMetadata, &seq, ApplyMetadataInput{TaskID: l.in.TaskID, Patch: p, Source: source}); err != nil {
		l.logger.Warn("operator patch dropped", "source", source, "error", err)
	}
}

// lifecycleCtx is the options of the activities on kontra-lifecycle: 10 s, retried, at most 30 s apart.
func lifecycleCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue: QueueLifecycle, StartToCloseTimeout: lifecycleStartToClose,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: lifecycleMaxInterval},
	})
}

// call runs a lifecycle-queue activity. An activity that found no task row marks the run gone.
func (l *lc) call(ctx workflow.Context, name string, out any, args ...any) error {
	err := workflow.ExecuteActivity(lifecycleCtx(ctx), name, args...).Get(ctx, out)
	if isType(err, ErrTypeTaskGone) {
		l.gone = true
		l.phase = phaseTerminal
	}
	return err
}

// hook runs a Lifecycle hook by type name on the hooks' queue: (TaskRef, args) → result, unlimited
// retry, 1 s doubling to 5 min (spec §13.3).
func (l *lc) hook(ctx workflow.Context, name string, ref hooks.TaskRef, args, out any) error {
	hctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue: l.in.Hooks.hooksQueue(), StartToCloseTimeout: hookStartToClose,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: hookMaxInterval},
	})
	err := workflow.ExecuteActivity(hctx, name, ref, args).Get(ctx, out)
	if err != nil {
		l.logger.Warn("lifecycle hook failed", "hook", name, "error", err)
	}
	return err
}

// taskRef builds the hooks' TaskRef from Postgres (spec §13.3), right before each hook, so a hook sees
// the task as the log has it.
//
// SPEC-CONFLICT: spec §13.3 says the TaskRef is "built by the activity from Postgres (not carried in
// workflow history)", but a hook is an activity the workflow schedules with (TaskRef, args) as its input,
// on the operator's queue for a remote hook, so its input is in history whatever builds it. The closest
// faithful behaviour: the workflow never keeps a TaskRef in its own state or input; a task_ref activity
// builds it from Postgres for each hook call, and it holds only ids, the state and the operator-written
// metadata (≤ 16 KiB), which D59 lets cross history; never caller or service content.
func (l *lc) taskRef(ctx workflow.Context) (hooks.TaskRef, bool) {
	var ref hooks.TaskRef
	if err := l.call(ctx, ActTaskRef, &ref, l.in.TaskID); err != nil {
		l.logger.Warn("task_ref", "error", err)
		return ref, false
	}
	return ref, true
}

// pushBound is the deadline a push_webhook's budget counts from: tasks.deadline once known, else an
// upper bound (a quoted task's deadline is fixed at its approval).
func (l *lc) pushBound(ctx workflow.Context) time.Time {
	if !l.deadline.IsZero() {
		return l.deadline
	}
	bound := workflow.Now(ctx)
	if l.in.QuoteExpiresAt != nil && l.in.QuoteExpiresAt.After(bound) {
		bound = *l.in.QuoteExpiresAt
	}
	return bound.Add(time.Duration(l.in.TimeoutSeconds) * time.Second)
}

// startPush starts one push_webhook for the config on kontra-push unless one this run started is still
// running, or delivered the terminal snapshot (spec §10.2: one activity per live config; it ends after the
// terminal snapshot or when the config is deleted). configID is the config's ConfigRef. A push_webhook that
// ended because its config was deleted or failed frees the config's place (changePushRestart): kontra.push
// "starts or ends a push_webhook" (spec §14.1), so the config created again with the same id, or set again
// after it failed, gets a new one; and since a kontra.push that arrived while the old activity still ran
// was dropped, the configs are listed once more when it ends.
func (l *lc) startPush(ctx workflow.Context, configID string) {
	if configID == "" || l.pushes[configID] || l.gone {
		return
	}
	l.pushes[configID] = true
	l.pushPending++
	bound := l.pushBound(ctx)
	budget := bound.Sub(workflow.Now(ctx))
	if budget < 0 {
		budget = 0
	}
	pctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue: QueuePush, HeartbeatTimeout: pushHeartbeat, StartToCloseTimeout: budget + pushBudget,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: pushMaxInterval},
	})
	f := workflow.ExecuteActivity(pctx, ActPushWebhook, PushInput{TaskID: l.in.TaskID, ConfigID: configID, Deadline: bound})
	workflow.Go(ctx, func(gc workflow.Context) {
		defer func() { l.pushPending-- }()
		var r PushResult
		if err := f.Get(gc, &r); err != nil {
			l.logger.Warn("push_webhook ended with an error", "config", configID, "error", err)
			return
		}
		if (r.Stopped == PushStoppedDeleted || r.Stopped == PushStoppedFailed) &&
			workflow.GetVersion(gc, changePushRestart, workflow.DefaultVersion, 1) >= 1 {
			delete(l.pushes, configID)
			l.listPush(gc) // counted in l.listing before pushPending drops: the run cannot end in between
		}
	})
}

// listPush runs list_push_configs and starts a push_webhook for each live config (spec §10.2: at start and
// after each dispatch, which recovers a lost kontra.push). It runs beside the turns, never delaying them,
// and gives up after 5 minutes (the next dispatch, or a kontra.push, lists or starts them again).
func (l *lc) listPush(ctx workflow.Context) {
	if l.gone {
		return
	}
	l.listing++
	workflow.Go(ctx, func(gc workflow.Context) {
		defer func() { l.listing-- }()
		lctx := workflow.WithActivityOptions(gc, workflow.ActivityOptions{
			TaskQueue: QueueLifecycle, StartToCloseTimeout: lifecycleStartToClose, ScheduleToCloseTimeout: listBudget,
			RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: lifecycleMaxInterval},
		})
		var cfgs []string
		if err := workflow.ExecuteActivity(lctx, ActListPushConfigs, l.in.TaskID).Get(gc, &cfgs); err != nil {
			l.logger.Warn("list_push_configs", "error", err)
			return
		}
		for _, id := range cfgs {
			l.startPush(gc, id)
		}
	})
}

// --- the task's phases ---

// armTimer starts the task.timeout timer at the stored deadline, once (spec §6.5, §14.1).
func (l *lc) armTimer(ctx workflow.Context) {
	if l.timerSet || l.deadline.IsZero() {
		return
	}
	l.timerSet = true
	d := l.deadline.Sub(workflow.Now(ctx))
	tctx, stop := workflow.WithCancel(ctx)
	l.timerStop = stop
	workflow.Go(tctx, func(c workflow.Context) {
		if d > 0 {
			if err := workflow.NewTimerWithOptions(c, d, workflow.TimerOptions{Summary: "task.timeout"}).Get(c, nil); err != nil {
				return
			}
		}
		l.timedOut = true
	})
}

// quoted waits for kontra.approval, the quote's expiry or a cancel (spec §13.2).
func (l *lc) quoted(ctx workflow.Context) {
	d := l.in.QuoteExpiresAt.Sub(workflow.Now(ctx))
	tctx, stop := workflow.WithCancel(ctx)
	defer stop()
	workflow.Go(tctx, func(c workflow.Context) {
		if d > 0 {
			if err := workflow.NewTimerWithOptions(c, d, workflow.TimerOptions{Summary: "quote.expiry"}).Get(c, nil); err != nil {
				return
			}
		}
		l.quoteExpired = true
	})
	for l.phase == phaseQuoted && ctx.Err() == nil {
		_ = workflow.Await(ctx, func() bool {
			return l.cancelPending() || (l.approval != nil && !l.approval.done) || l.quoteExpired
		})
		switch {
		case l.approval != nil && !l.approval.done:
			l.approve(ctx, l.approval)
		case l.cancelPending():
			tr := l.writeTerminal(ctx, WriteTerminalInput{State: string(store.StateCanceled), Canceled: store.CanceledClient})
			l.finishEnded(l.cancel, tr.State)
		case l.quoteExpired:
			l.writeTerminal(ctx, WriteTerminalInput{State: string(store.StateCanceled), Canceled: store.CanceledQuoteExpired})
		}
	}
}

// approve applies an approval: accepted, its metadata and the move to submitted (one activity, which
// returns the stored deadline); declined, canceled: declined.
func (l *lc) approve(ctx workflow.Context, op *approvalOp) {
	defer func() { op.done = true }()
	if !op.u.Accepted {
		tr := l.writeTerminal(ctx, WriteTerminalInput{State: string(store.StateCanceled), Canceled: store.CanceledDeclined})
		op.res = ApprovalResult{State: tr.State}
		return
	}
	var r SubmitResult
	err := l.call(ctx, ActSubmit, &r, SubmitInput{TaskID: l.in.TaskID, Metadata: op.u.Metadata, TimeoutSeconds: l.in.TimeoutSeconds})
	if err != nil {
		op.err = refusal("the approval could not be applied")
		return
	}
	l.deadline = r.Deadline
	l.armTimer(ctx)
	l.phase = phaseNew
	op.res = ApprovalResult{State: string(store.StateSubmitted)}
}

// active runs the turns until the task is terminal.
func (l *lc) active(ctx workflow.Context) {
	// The first turn arrives with the workflow's start (UpdateWithStart).
	_ = workflow.Await(ctx, func() bool { return len(l.pending) > 0 || l.cancelPending() })
	for l.phase != phaseTerminal && ctx.Err() == nil {
		switch {
		case l.timedOut:
			l.timeout(ctx)
			continue
		case l.cancelPending():
			l.cancelIdle(ctx)
			continue
		case len(l.pending) == 0:
			_ = workflow.Await(ctx, func() bool { return len(l.pending) > 0 || l.timedOut || l.cancelPending() })
			continue
		}
		wid := l.pending[0]
		l.pending = l.pending[1:]
		l.started++
		if l.started > MaxTurns {
			// The attempt waits at input-required: write_terminal ends it, interrupted (D77).
			l.fail(ctx, store.ReasonUpstreamError, fmt.Sprintf("the task reached its limit of %d turns", MaxTurns), string(store.OutcomeUpstreamError))
			continue
		}
		l.phase = phaseRunning
		if l.n == 0 {
			if !l.prepare(ctx, 1, "") {
				continue
			}
			if !l.listedAtStart {
				l.listPush(ctx)
			}
		}
		l.runTurn(ctx, wid)
	}
}

// prepare writes attempt n and learns its queue (and, the first time, the deadline that arms the timer).
// false when the task ended instead.
func (l *lc) prepare(ctx workflow.Context, n int, prev string) bool {
	budget := time.Duration(l.in.TimeoutSeconds) * time.Second
	if !l.deadline.IsZero() {
		budget = l.deadline.Sub(workflow.Now(ctx))
	}
	budget = max(budget, lifecycleStartToClose)
	pctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue: QueueLifecycle, StartToCloseTimeout: lifecycleStartToClose, ScheduleToCloseTimeout: budget,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: lifecycleMaxInterval},
	})
	var r PrepareResult
	err := workflow.ExecuteActivity(pctx, ActPrepare, PrepareInput{TaskID: l.in.TaskID, N: n, PreviousOutcome: prev}).Get(ctx, &r)
	switch {
	case err == nil:
	case isType(err, ErrTypeTaskGone):
		l.gone, l.phase = true, phaseTerminal
		return false
	case isType(err, ErrTypeServiceUnavailable):
		l.fail(ctx, store.ReasonServiceUnavailable, "", string(store.OutcomeServiceUnavailable))
		return false
	default:
		// No attempt could be placed before the task's deadline.
		l.logger.Warn("prepare failed", "attempt", n, "error", err)
		l.timedOut = true
		l.timeout(ctx)
		return false
	}
	l.n, l.queue, l.backend, l.resumes = n, r.Queue, r.Backend, 0
	if l.deadline.IsZero() {
		l.deadline = r.Deadline
		if l.deadline.IsZero() {
			l.deadline = workflow.Now(ctx).Add(time.Duration(l.in.TimeoutSeconds) * time.Second)
		}
		l.armTimer(ctx)
	}
	return true
}

// runTurn runs one turn: dispatch, then the retry table (spec §6.4), until the turn ends at
// input-required or the task is terminal.
func (l *lc) runTurn(ctx workflow.Context, wid string) {
	next := step{kind: stepDispatch}
	l.resumes = 0
	for l.phase != phaseTerminal && ctx.Err() == nil {
		if next.wait > 0 {
			until := workflow.Now(ctx).Add(next.wait)
			woken, _ := workflow.AwaitWithTimeout(ctx, next.wait, func() bool { return l.timedOut || l.cancelPending() })
			next.wait = 0
			if woken {
				next.wait = until.Sub(workflow.Now(ctx))
			}
		}
		if l.timedOut {
			l.timeout(ctx)
			return
		}
		if l.cancelPending() {
			// No dispatch runs now: between attempts, or waiting to re-attach.
			l.cancelIdle(ctx)
			continue
		}
		if next.wait > 0 {
			continue
		}
		if next.kind == stepRetry {
			if !l.prepare(ctx, l.n+1, next.prev) {
				return
			}
		}
		l.dispatch(ctx, wid, next.kind == stepResume || (next.kind == stepAgain && next.resume))
		if l.phase == phaseTerminal || ctx.Err() != nil {
			return
		}
		l.listPush(ctx)
		next = l.decide(ctx)
		if next.kind == stepDone {
			return
		}
	}
}

// dispatch runs one dispatch activity on the attempt's queue and waits for it, handling a cancel and the
// task.timeout while it runs.
func (l *lc) dispatch(ctx workflow.Context, wid string, resume bool) {
	_ = workflow.Await(ctx, func() bool { return !l.applying })
	stc := max(l.deadline.Sub(workflow.Now(ctx))+dispatchSlack, dispatchSlack)
	dctx, cancel := workflow.WithCancel(ctx)
	dctx = workflow.WithActivityOptions(dctx, workflow.ActivityOptions{
		TaskQueue: l.queue, ScheduleToStartTimeout: dispatchScheduleToStart, StartToCloseTimeout: stc,
		HeartbeatTimeout: dispatchHeartbeat, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
		WaitForCancellation: true,
	})
	c := &call{cancel: cancel, resume: resume}
	l.disp = c
	f := workflow.ExecuteActivity(dctx, ActDispatch, DispatchInput{TaskID: l.in.TaskID, N: l.n, WorkItemID: wid, Resume: resume})
	workflow.Go(ctx, func(gc workflow.Context) {
		var r DispatchResult
		err := f.Get(gc, &r)
		c.res, c.err, c.done = r, err, true
	})
	for !c.done {
		if err := workflow.Await(ctx, func() bool {
			return c.done || (l.timedOut && l.phase != phaseTerminal) || l.cancelPending()
		}); err != nil {
			return
		}
		switch {
		case c.done:
		case l.timedOut && l.phase != phaseTerminal:
			l.timeout(ctx)
		case l.cancelPending() && l.phase == phaseTerminal:
			l.finish(l.cancel, CancelResult{}, notCancelableErr("terminal"))
		case l.cancelPending():
			l.cancelDispatching(ctx, c)
		}
	}
}

// decide applies the retry table (spec §6.4) to the dispatch that returned.
func (l *lc) decide(ctx workflow.Context) step {
	c := l.disp
	if c.err == nil {
		switch c.res.Outcome {
		case DispatchTerminal:
			l.phase = phaseTerminal
			return step{kind: stepDone}
		case DispatchInterrupted:
			l.phase = phaseWaiting
			return step{kind: stepDone}
		case DispatchRetryable:
			reason := store.FailureReason(c.res.Reason)
			if !reason.Valid() {
				reason = store.ReasonUpstreamUnreachable
			}
			st, ok := l.attemptState(ctx)
			if !ok {
				return step{kind: stepDone}
			}
			// The dispatch ended its attempt itself (spec §4).
			return l.retry(ctx, st, reason, time.Duration(c.res.RetryAfterSeconds)*time.Second, "")
		case DispatchTunnelLost:
			return l.lost(ctx, store.ReasonTunnelLost)
		}
	} else {
		notStarted := isTimeout(c.err, enums.TIMEOUT_TYPE_SCHEDULE_TO_START)
		l.logger.Info("dispatch lost", "attempt", l.n,
			"heartbeat_timeout", isTimeout(c.err, enums.TIMEOUT_TYPE_HEARTBEAT), "schedule_to_start_timeout", notStarted)
		if notStarted && workflow.GetVersion(ctx, changeDispatchNotStarted, workflow.DefaultVersion, 1) >= 1 {
			return l.notStarted(ctx, c)
		}
	}
	// A heartbeat or schedule-to-start timeout, a lost context, an error of the dispatch itself: the
	// replica or its context was lost. Postgres says whether an upstream task exists.
	return l.lost(ctx, "")
}

// lost is the retry table for a dispatch whose replica or context was lost: before the first upstream task
// id a new attempt; after it, a direct backend is re-attached (D42) and a tunnel fails tunnel_lost.
//
// SPEC-CONFLICT: spec §6.4's table names no failure reason for retries exhausted by lost dispatches of a
// direct backend (a dying gate replica, or no replica polling kontra-direct within 15 s). The closest of
// the closed set, upstream_unreachable ("the service could not be reached"), is written, and recorded as
// the dead attempt's outcome; a tunnel backend's is tunnel_lost.
func (l *lc) lost(ctx workflow.Context, hint store.FailureReason) step {
	st, ok := l.attemptState(ctx)
	if !ok {
		return step{kind: stepDone}
	}
	return l.lostWith(ctx, st, hint)
}

// lostWith is lost on an attempt_state already read.
func (l *lc) lostWith(ctx workflow.Context, st AttemptState, hint store.FailureReason) step {
	tunnel := st.Backend == string(store.BackendTunnel) || l.backend == string(store.BackendTunnel)
	if st.FirstUpstreamIDAt != nil {
		if tunnel {
			l.fail(ctx, store.ReasonTunnelLost, "", string(store.OutcomeTunnelLost))
			return step{kind: stepDone}
		}
		if st.Ended {
			// The attempt was ended without a terminal event: a resume could not open it again.
			l.fail(ctx, store.ReasonUpstreamError, "the relay could not follow the task", "")
			return step{kind: stepDone}
		}
		l.resumes++
		if l.resumes > MaxResumes {
			l.fail(ctx, store.ReasonUpstreamError, "re-attach limit reached", string(store.OutcomeUpstreamError))
			return step{kind: stepDone}
		}
		return step{kind: stepResume, wait: min(time.Second<<(l.resumes-1), maxResumeBackoff)}
	}
	reason := store.ReasonUpstreamUnreachable
	if tunnel {
		reason = store.ReasonTunnelLost
	}
	if hint != "" {
		reason = hint
	}
	return l.retry(ctx, st, reason, 0, string(reason))
}

// notStarted is the retry table for a dispatch that was never started (schedule-to-start timeout, 15 s):
// no gate replica polled the attempt's queue, so the activity never ran. It consumed no work item, sent
// nothing upstream and wrote nothing.
//
// SPEC-CONFLICT: spec §6.4's table does not name this case; its "dispatch heartbeat timeout or lost
// context, direct backend" row re-attaches after the first upstream task id, and the plan applied it to a
// schedule-to-start timeout too. For a follow-up turn that is wrong: a resume skips the work item's
// consume and re-attaches without sending, finds the upstream still at input-required and ends the turn,
// so the follow-up is in the log (a2a-go appends it to history) but the seller never received it. A new
// attempt is no fix either: after the first upstream id §6.4 allows none. The closest faithful behaviour:
// before the first upstream id, a new attempt as before; after it, a direct backend dispatches the same
// turn again on the same attempt as it was (a follow-up's dispatch stays the turn's first, which consumes
// the still unconsumed item and sends it; a resume stays a resume), backing off and capped like resumes
// (D42), and a tunnel backend fails tunnel_lost. When the cap is reached by a dispatch that was not
// started, the task fails upstream_unreachable ("no gate replica took the turn's dispatch"), the reason
// lost() already writes when lost dispatches of a direct backend exhaust the retries.
func (l *lc) notStarted(ctx workflow.Context, c *call) step {
	st, ok := l.attemptState(ctx)
	if !ok {
		return step{kind: stepDone}
	}
	tunnel := st.Backend == string(store.BackendTunnel) || l.backend == string(store.BackendTunnel)
	if st.FirstUpstreamIDAt == nil {
		return l.lostWith(ctx, st, "")
	}
	switch {
	case tunnel:
		l.fail(ctx, store.ReasonTunnelLost, "", string(store.OutcomeTunnelLost))
		return step{kind: stepDone}
	case st.Ended:
		l.fail(ctx, store.ReasonUpstreamError, "the relay could not follow the task", "")
		return step{kind: stepDone}
	}
	l.resumes++
	if l.resumes > MaxResumes {
		l.fail(ctx, store.ReasonUpstreamUnreachable, "no gate replica took the turn's dispatch", string(store.OutcomeUpstreamUnreachable))
		return step{kind: stepDone}
	}
	return step{kind: stepAgain, resume: c.resume, wait: min(time.Second<<(l.resumes-1), maxResumeBackoff)}
}

// retry starts a new attempt after 1 s, 2 s, 4 s … (at least the upstream's Retry-After), capped at the
// deadline by the timer, while task.retries allows one (spec §6.4); else the task fails with reason. A
// retry is only ever made before the first upstream task id of the current attempt.
func (l *lc) retry(ctx workflow.Context, st AttemptState, reason store.FailureReason, retryAfter time.Duration, prev string) step {
	switch {
	case terminalState(st.TaskState):
		l.phase = phaseTerminal
		return step{kind: stepDone}
	case st.FirstUpstreamIDAt != nil, l.n >= 1+l.in.Retries:
		l.fail(ctx, reason, "", string(reason))
		return step{kind: stepDone}
	}
	wait := time.Second << min(l.n-1, maxRetryShift)
	wait = max(wait, retryAfter)
	return step{kind: stepRetry, wait: wait, prev: prev}
}

// attemptState reads attempt_state for the current attempt; false when the task ended (or is gone).
func (l *lc) attemptState(ctx workflow.Context) (AttemptState, bool) {
	var st AttemptState
	if err := l.call(ctx, ActAttemptState, &st, AttemptRef{TaskID: l.in.TaskID, N: l.n}); err != nil {
		l.logger.Warn("attempt_state", "error", err)
		return st, false
	}
	if terminalState(st.TaskState) {
		l.phase = phaseTerminal
		return st, false
	}
	return st, true
}

// writeTerminal runs write_terminal; the task is terminal afterwards (or gone).
func (l *lc) writeTerminal(ctx workflow.Context, in WriteTerminalInput) TerminalResult {
	in.TaskID = l.in.TaskID
	var r TerminalResult
	if err := l.call(ctx, ActWriteTerminal, &r, in); err != nil {
		l.logger.Warn("write_terminal", "error", err)
	}
	l.phase = phaseTerminal
	return r
}

// fail writes `failed: <reason>`, ending the latest attempt with endAttempt when no dispatch ended it.
func (l *lc) fail(ctx workflow.Context, reason store.FailureReason, text, endAttempt string) {
	l.writeTerminal(ctx, WriteTerminalInput{State: string(store.StateFailed), Reason: string(reason), Text: text, EndAttempt: endAttempt})
}

// timeout is the task.timeout (spec §6.5): forward_cancel when an upstream task exists, without waiting for
// its answer, then failed: timeout.
func (l *lc) timeout(ctx workflow.Context) {
	if l.phase == phaseTerminal {
		return
	}
	st, ok := l.attemptState(ctx)
	if !ok {
		return
	}
	if st.FirstUpstreamIDAt != nil {
		// forward_cancel is scheduled first; its answer is not waited for.
		queue, tunnel := l.cancelRoute(st, l.disp != nil && !l.disp.done)
		f := l.startForwardCancel(ctx, queue)
		l.ctl++
		workflow.Go(ctx, func(gc workflow.Context) {
			defer func() { l.ctl-- }()
			if _, err := l.awaitForwardCancel(gc, f, queue, tunnel); err != nil {
				l.logger.Info("forward_cancel at the deadline", "error", err)
			}
		})
	}
	l.fail(ctx, store.ReasonTimeout, "", string(store.OutcomeTimeout))
	if l.disp != nil && !l.disp.done {
		// The terminal rule already refuses its appends; this stops it.
		l.disp.cancel()
	}
}

// ctlCtx is the options of abort and forward_cancel: schedule-to-start 10 s, never retried by Temporal
// (a per-replica queue's retry would go back to the same queue, spec §14.2).
func ctlCtx(ctx workflow.Context, queue string, startToClose time.Duration) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue: queue, ScheduleToStartTimeout: ctlScheduleToStart, StartToCloseTimeout: startToClose,
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
	})
}

// forwardCancel runs forward_cancel: on kontra-ctl-<replica> while a dispatch runs (falling back to
// kontra-direct-ctl when that queue does not start it within 10 s, D79), else on kontra-direct-ctl; a
// tunnel attempt on its session's replica's control queue (M2).
func (l *lc) forwardCancel(ctx workflow.Context, st AttemptState, running bool) (ForwardCancelResult, error) {
	_ = workflow.Await(ctx, func() bool { return !l.applying })
	l.ctl++
	defer func() { l.ctl-- }()
	queue, tunnel := l.cancelRoute(st, running)
	return l.awaitForwardCancel(ctx, l.startForwardCancel(ctx, queue), queue, tunnel)
}

// cancelRoute is the queue forward_cancel goes to first, and whether the attempt is a tunnel's.
func (l *lc) cancelRoute(st AttemptState, running bool) (string, bool) {
	tunnel := st.Backend == string(store.BackendTunnel)
	if st.ReplicaID != "" && (tunnel || running) {
		return QueueCtl(st.ReplicaID), tunnel
	}
	return QueueDirectCtl, tunnel
}

func (l *lc) startForwardCancel(ctx workflow.Context, queue string) workflow.Future {
	return workflow.ExecuteActivity(ctlCtx(ctx, queue, forwardCancelStartToClose), ActForwardCancel, AttemptRef{TaskID: l.in.TaskID, N: l.n})
}

// awaitForwardCancel waits for forward_cancel; a direct task's that its replica's control queue did not
// start within 10 s goes to kontra-direct-ctl (the dispatching replica is gone and its dispatch's heartbeat
// has not timed out yet, D79).
func (l *lc) awaitForwardCancel(ctx workflow.Context, f workflow.Future, queue string, tunnel bool) (ForwardCancelResult, error) {
	var r ForwardCancelResult
	err := f.Get(ctx, &r)
	if err != nil && !tunnel && queue != QueueDirectCtl && isTimeout(err, enums.TIMEOUT_TYPE_SCHEDULE_TO_START) {
		err = l.startForwardCancel(ctx, QueueDirectCtl).Get(ctx, &r)
	}
	return r, err
}

// abort stops the running dispatch in process through its replica's registry (spec §8, D44), and asks
// Temporal to cancel it too, as a backstop. With no replica recorded yet, Temporal's cancellation alone
// (an activity that has not started completes at once). An abort not started within 10 s means the
// replica is gone: the caller then waits for the dispatch's heartbeat timeout (D79).
func (l *lc) abort(ctx workflow.Context, c *call, st AttemptState) {
	if st.ReplicaID == "" {
		c.cancel()
		return
	}
	l.ctl++
	f := workflow.ExecuteActivity(ctlCtx(ctx, QueueCtl(st.ReplicaID), abortStartToClose), ActAbort, AttemptRef{TaskID: l.in.TaskID, N: l.n})
	c.cancel()
	err := f.Get(ctx, nil)
	l.ctl--
	if err != nil {
		l.logger.Info("abort did not run; waiting for the dispatch to stop", "replica", st.ReplicaID, "error", err)
	}
}

func (l *lc) finish(op *cancelOp, res CancelResult, err error) {
	if op == nil || op.done {
		return
	}
	op.res, op.err, op.done = res, err, true
}

// finishEnded completes a cancel with the task's terminal state: canceled, or not cancelable when it ended
// otherwise (the cancel lost a race with the task's end).
func (l *lc) finishEnded(op *cancelOp, state string) {
	if state == string(store.StateCanceled) {
		l.finish(op, CancelResult{Outcome: CancelCanceled}, nil)
		return
	}
	if state == "" {
		state = "terminal"
	}
	l.finish(op, CancelResult{}, notCancelableErr(state))
}

// unreachable is the answer when forward_cancel could not reach the service: the task continues.
func unreachable() CancelResult {
	return CancelResult{Outcome: CancelNotCancelable, UpstreamError: &UpstreamError{Code: -32603, Message: "the service could not be reached to cancel the task"}}
}

// cancelDispatching is §8's cancel table while a dispatch runs.
func (l *lc) cancelDispatching(ctx workflow.Context, c *call) {
	op := l.cancel
	st, ok := l.attemptState(ctx)
	if !ok {
		l.finishEnded(op, st.TaskState)
		return
	}
	if st.FirstUpstreamIDAt == nil {
		// No upstream task yet: abort the dispatch, and write canceled only once it has stopped.
		l.abort(ctx, c, st)
		_ = workflow.Await(ctx, func() bool { return c.done })
		st2, ok := l.attemptState(ctx)
		if !ok {
			l.finishEnded(op, st2.TaskState)
			return
		}
		if st2.FirstUpstreamIDAt != nil {
			// The dispatch recorded a first upstream task id meanwhile: tell the upstream first.
			if _, err := l.forwardCancel(ctx, st2, false); err != nil {
				l.logger.Info("forward_cancel after abort", "error", err)
			}
		}
		tr := l.writeTerminal(ctx, WriteTerminalInput{State: string(store.StateCanceled), Canceled: store.CanceledClient, EndAttempt: string(store.OutcomeAborted)})
		l.finishEnded(op, tr.State)
		return
	}
	r, err := l.forwardCancel(ctx, st, true)
	switch {
	case err != nil:
		l.logger.Info("forward_cancel failed; the task continues", "error", err)
		l.finish(op, unreachable(), nil)
	case r.Outcome == CancelCanceled:
		// The running dispatch logs the upstream's canceled status as the terminal event; the cancel
		// completes once it is in the log, within its 60 s (D79).
		if remaining := CancelWait - workflow.Now(ctx).Sub(op.started); remaining > 0 {
			_, _ = workflow.AwaitWithTimeout(ctx, remaining, func() bool { return c.done })
		}
		if st3, ok := l.attemptState(ctx); !ok {
			l.finishEnded(op, st3.TaskState)
			return
		}
		// The upstream answered canceled but the dispatch has not logged it: the gate writes it.
		tr := l.writeTerminal(ctx, WriteTerminalInput{State: string(store.StateCanceled), Canceled: store.CanceledClient, EndAttempt: string(store.OutcomeCanceled)})
		if !c.done {
			c.cancel()
		}
		l.finishEnded(op, tr.State)
	default:
		// The upstream refused (TaskNotCancelableError, UnsupportedOperationError): the task continues.
		l.finish(op, CancelResult{Outcome: r.Outcome, UpstreamError: r.UpstreamError}, nil)
	}
}

// cancelIdle is §8's cancel table while no dispatch runs: before the first dispatch, between attempts,
// waiting to re-attach, or at input-required.
func (l *lc) cancelIdle(ctx workflow.Context) {
	op := l.cancel
	if l.phase == phaseTerminal {
		l.finishEnded(op, "")
		return
	}
	st, ok := l.attemptState(ctx)
	if !ok {
		l.finishEnded(op, st.TaskState)
		return
	}
	if l.n == 0 || st.FirstUpstreamIDAt == nil {
		tr := l.writeTerminal(ctx, WriteTerminalInput{State: string(store.StateCanceled), Canceled: store.CanceledClient, EndAttempt: string(store.OutcomeAborted)})
		l.finishEnded(op, tr.State)
		return
	}
	r, err := l.forwardCancel(ctx, st, false)
	switch {
	case err != nil:
		l.logger.Info("forward_cancel failed; the task continues", "error", err)
		l.finish(op, unreachable(), nil)
	case r.Outcome == CancelCanceled:
		// No dispatch runs to log the upstream's canceled status: the workflow writes it (spec §8).
		tr := l.writeTerminal(ctx, WriteTerminalInput{State: string(store.StateCanceled), Canceled: store.CanceledClient, EndAttempt: string(store.OutcomeCanceled)})
		l.finishEnded(op, tr.State)
	default:
		l.finish(op, CancelResult{Outcome: r.Outcome, UpstreamError: r.UpstreamError}, nil)
	}
}

// ended runs after the terminal event: once nothing holds the task, the held patches, OnTerminal, the
// review window and OnReviewClosed; then the workflow ends once no push_webhook, hook or Update handler
// it started is pending (D78).
func (l *lc) ended(ctx workflow.Context) {
	l.phase = phaseTerminal
	if l.timerStop != nil {
		l.timerStop()
	}
	if l.cancelPending() {
		st, _ := l.attemptState(ctx)
		l.finishEnded(l.cancel, st.TaskState)
	}
	if op := l.approval; op != nil && !op.done {
		// Accepted while a cancel or the quote's expiry was ending the task.
		op.err, op.done = refusal("the task is no longer quoted"), true
	}
	if l.disp != nil && !l.disp.done {
		l.disp.cancel()
	}
	_ = workflow.Await(ctx, func() bool { return !l.busy() })
	if !l.gone && ctx.Err() == nil {
		_ = workflow.Await(ctx, func() bool { return len(l.held) == 0 && !l.applying })
		l.terminalHooks(ctx)
	}
	l.windowDecided = true
	_ = workflow.Await(ctx, func() bool {
		return l.pushPending == 0 && l.listing == 0 && l.hooksPending == 0 && len(l.held) == 0 && !l.applying &&
			workflow.AllHandlersFinished(ctx)
	})
}

// terminalHooks runs OnTerminal, then ReviewWindow and, when it opens, the window and OnReviewClosed
// (spec §13.3).
func (l *lc) terminalHooks(ctx workflow.Context) {
	ref, ok := l.taskRef(ctx)
	if !ok {
		return
	}
	var p hooks.Patch
	if err := l.hook(ctx, HookOnTerminal, ref, struct{}{}, &p); err == nil {
		l.apply(ctx, HookOnTerminal, p)
	}
	var w hooks.ReviewWindow
	err := l.hook(ctx, HookReviewWindow, ref, struct{}{}, &w)
	l.reviewOpen = err == nil && w.Open
	l.windowDecided = true
	if !l.reviewOpen {
		return
	}
	d := min(max(time.Duration(w.Seconds)*time.Second, 0), maxReviewWindow)
	_, _ = workflow.AwaitWithTimeout(ctx, d, func() bool { return l.disputed })
	l.reviewOpen = false
	_ = workflow.Await(ctx, func() bool { return l.reviewInFlight == 0 })
	if ref, ok = l.taskRef(ctx); !ok {
		return
	}
	var closed hooks.Patch
	if err := l.hook(ctx, HookOnReviewClosed, ref, hooks.ReviewClosedArgs{Disputed: l.disputed}, &closed); err != nil {
		return
	}
	l.apply(ctx, HookOnReviewClosed, closed)
	if closed.HoldOpenSeconds > 0 {
		_, _ = workflow.AwaitWithTimeout(ctx, time.Duration(closed.HoldOpenSeconds)*time.Second, func() bool { return l.closed })
	}
}
