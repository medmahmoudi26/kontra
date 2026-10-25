package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/temporal"

	"github.com/medmahmoudi26/kontra/internal/audit"
	"github.com/medmahmoudi26/kontra/internal/hooks"
	"github.com/medmahmoudi26/kontra/internal/ids"
	"github.com/medmahmoudi26/kontra/internal/store"
)

// DispatchActivities run on the dispatching replica (kontra-direct, kontra-tunnel-<id>) and on its
// control queue (kontra-ctl-<id>). internal/relay implements them.
type DispatchActivities interface {
	// Dispatch writes attempts.replica_id before any upstream call, loads the work item, and runs a2a-go's
	// handler for it with the relay executor, heartbeating every 5 s (timeout 30 s). It discards the
	// handler's result and returns the outcome the per-process registry recorded.
	Dispatch(ctx context.Context, in DispatchInput) (DispatchResult, error)
	// Abort cancels the running dispatch's context through the replica's registry (spec §8, D44).
	Abort(ctx context.Context, in AttemptRef) error
	// ForwardCancel sends CancelTask upstream for the attempt's upstream task and returns the answer.
	ForwardCancel(ctx context.Context, in AttemptRef) (ForwardCancelResult, error)
}

// PushActivities is push_webhook (spec §10.2); internal/push implements it.
type PushActivities interface {
	PushWebhook(ctx context.Context, in PushInput) (PushResult, error)
}

// BackendResolver tells prepare how a service is reached: the service registry implements it.
type BackendResolver interface {
	BackendFor(ctx context.Context, tenantID uuid.UUID, service string) (store.Backend, error)
}

// PushConfigLister lists a task's live push config ids (list_push_configs); internal/push implements it.
type PushConfigLister interface {
	LiveConfigIDs(ctx context.Context, taskID uuid.UUID) ([]string, error)
}

// EventPatchRefused is the audit event of an operator patch apply_metadata refused (spec §13.3: a patch
// over the limit or with a key outside the §13.2 rules is dropped, with a metric and an audit row).
const EventPatchRefused = "lifecycle.patch_refused"

// LifecycleActivities are the activities on kontra-lifecycle, runnable on any replica. Every error they
// return crosses Temporal history as a failure message, so none carries caller or service content: they
// name ids, states and the gate's own refusals.
type LifecycleActivities struct {
	Log      *store.Log
	Backends BackendResolver
	Push     PushConfigLister // nil: no push configs are listed
	// Rules are the §13.2 key and size rules apply_metadata and submit enforce on every operator patch,
	// and that pick the operator-written keys of a TaskRef.
	Rules  hooks.KeyRules
	Logger *slog.Logger
}

func (a *LifecycleActivities) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

// nonRetryable is an activity error Temporal does not retry; the workflow reads errType.
func nonRetryable(errType string, err error) error {
	return temporal.NewNonRetryableApplicationError(err.Error(), errType, nil)
}

// taskGone maps a store error for a task that does not exist to ErrTypeTaskGone, and leaves any other
// error retryable.
func taskGone(err error) error {
	if errors.Is(err, a2a.ErrTaskNotFound) {
		return nonRetryable(ErrTypeTaskGone, errors.New("the task does not exist"))
	}
	return err
}

func parseTask(taskID string) (uuid.UUID, error) {
	id, err := ids.ParseTaskID(taskID)
	if err != nil {
		return uuid.Nil, nonRetryable(ErrTypeTaskGone, fmt.Errorf("task id %q is not a gate task id", taskID))
	}
	return id, nil
}

// Prepare picks the backend (spec §6.1; direct in M1), ends the previous attempt when its dispatch was
// lost (PreviousOutcome), writes attempt N and returns the queue to dispatch on and the stored deadline.
// It is retried, so every write is idempotent: BeginAttempt accepts an attempt that already exists with
// the same backend, and EndAttempt keeps an attempt's first outcome.
func (a *LifecycleActivities) Prepare(ctx context.Context, in PrepareInput) (PrepareResult, error) {
	id, err := parseTask(in.TaskID)
	if err != nil {
		return PrepareResult{}, err
	}
	if in.N < 1 {
		return PrepareResult{}, nonRetryable(ErrTypeTaskGone, fmt.Errorf("attempt %d", in.N))
	}
	row, err := a.Log.Load(ctx, id)
	if err != nil {
		return PrepareResult{}, taskGone(err)
	}
	if a.Backends == nil {
		return PrepareResult{}, nonRetryable(ErrTypeServiceUnavailable, errors.New("no service registry"))
	}
	backend, err := a.Backends.BackendFor(ctx, row.TenantID, row.Service)
	if err != nil {
		// The registry knows no backend for the service: it was removed or disabled (spec §6.4,
		// service_unavailable). The registry is in memory, so this is not a transient failure.
		return PrepareResult{}, nonRetryable(ErrTypeServiceUnavailable, fmt.Errorf("service %s: %v", row.Service, err))
	}
	if backend != store.BackendDirect {
		// Tunnel sessions (spec §6.1's candidate query, no_session) are M2.
		return PrepareResult{}, nonRetryable(ErrTypeServiceUnavailable, fmt.Errorf("service %s: backend %q is not served by this gate yet", row.Service, backend))
	}
	if in.N > 1 && in.PreviousOutcome != "" {
		o := store.Outcome(in.PreviousOutcome)
		if !o.Valid() {
			o = store.OutcomeFailed
		}
		if err := a.Log.EndAttempt(ctx, id, in.N-1, o); err != nil && !errors.Is(err, store.ErrAttemptNotFound) {
			return PrepareResult{}, err
		}
	}
	if err := a.Log.BeginAttempt(ctx, store.Attempt{TaskID: id, N: in.N, Backend: backend}); err != nil {
		return PrepareResult{}, taskGone(err)
	}
	res := PrepareResult{Queue: QueueDirect, Backend: string(backend)}
	if row.Deadline != nil {
		res.Deadline = row.Deadline.UTC()
	}
	return res, nil
}

// AttemptState reads the task's state and, for N > 0, attempt N's first_upstream_id_at, replica_id and
// whether it ended: the retry and cancel decisions are taken on what Postgres recorded, never on heartbeat
// details, which a crash loses (spec §14.2).
func (a *LifecycleActivities) AttemptState(ctx context.Context, in AttemptRef) (AttemptState, error) {
	id, err := parseTask(in.TaskID)
	if err != nil {
		return AttemptState{}, err
	}
	row, err := a.Log.Load(ctx, id)
	if err != nil {
		return AttemptState{}, taskGone(err)
	}
	st := AttemptState{TaskState: string(row.State)}
	if in.N < 1 {
		return st, nil
	}
	att, err := a.Log.GetAttempt(ctx, id, in.N)
	if errors.Is(err, store.ErrAttemptNotFound) {
		return st, nil
	}
	if err != nil {
		return AttemptState{}, err
	}
	st.Backend, st.ReplicaID, st.Ended = string(att.Backend), att.ReplicaID, att.EndedAt != nil
	if att.FirstUpstreamIDAt != nil {
		t := att.FirstUpstreamIDAt.UTC()
		st.FirstUpstreamIDAt = &t
	}
	return st, nil
}

// ListPushConfigs returns the task's live push config ids, at start and after each dispatch, to recover
// a lost kontra.push signal (spec §10.2). They are sorted, so the workflow starts them in a stable order.
func (a *LifecycleActivities) ListPushConfigs(ctx context.Context, taskID string) ([]string, error) {
	id, err := parseTask(taskID)
	if err != nil {
		return nil, err
	}
	if a.Push == nil {
		return nil, nil
	}
	out, err := a.Push.LiveConfigIDs(ctx, id)
	if err != nil {
		return nil, err
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// WriteTerminal writes the terminal event through the store when no handler can (spec §14.2). It is
// idempotent: a task that has already ended is left as it is, and its terminal state returned. With
// EndAttempt it then ends the task's latest attempt if no dispatch ended it, with interrupted when the task
// was waiting at input-required (D77).
func (a *LifecycleActivities) WriteTerminal(ctx context.Context, in WriteTerminalInput) (TerminalResult, error) {
	id, err := parseTask(in.TaskID)
	if err != nil {
		return TerminalResult{}, err
	}
	before, err := a.Log.Load(ctx, id)
	if err != nil {
		return TerminalResult{}, taskGone(err)
	}
	w := store.TerminalWrite{
		TaskID: id, State: store.State(in.State), Reason: store.FailureReason(in.Reason), Canceled: in.Canceled, Text: in.Text,
	}
	if !w.State.Terminal() || (w.State == store.StateFailed && !w.Reason.Valid()) {
		return TerminalResult{}, nonRetryable(ErrTypeTaskGone, fmt.Errorf("write_terminal: %q/%q is not a terminal write", in.State, in.Reason))
	}
	seq, err := a.Log.WriteTerminal(ctx, w)
	if err != nil {
		return TerminalResult{}, taskGone(err)
	}
	after, err := a.Log.Load(ctx, id)
	if err != nil {
		return TerminalResult{}, taskGone(err)
	}
	if in.EndAttempt != "" {
		if err := a.endLatest(ctx, id, before.State, store.Outcome(in.EndAttempt)); err != nil {
			return TerminalResult{}, err
		}
	}
	return TerminalResult{Seq: seq, State: string(after.State)}, nil
}

func (a *LifecycleActivities) endLatest(ctx context.Context, id uuid.UUID, before store.State, o store.Outcome) error {
	att, err := a.Log.LatestAttempt(ctx, id)
	if errors.Is(err, store.ErrAttemptNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if att.EndedAt != nil {
		return nil
	}
	if before == store.StateInputRequired {
		o = store.OutcomeInterrupted
	}
	if !o.Valid() {
		o = store.OutcomeFailed
	}
	return a.Log.EndAttempt(ctx, id, att.N, o)
}

// ApplyMetadata logs one metadata event (spec §13.3, §14.2). A patch over 16 KiB or with a key outside the
// §13.2 rules is refused non-retryably and dropped, with an audit row. mcpMeta is rendered into MCP
// results (§13.3), which M1 does not serve: it is checked here and not stored.
func (a *LifecycleActivities) ApplyMetadata(ctx context.Context, in ApplyMetadataInput) (int64, error) {
	id, err := parseTask(in.TaskID)
	if err != nil {
		return 0, err
	}
	if err := a.Rules.CheckOperatorMaps(in.Patch.Metadata, in.Patch.MCPMeta); err != nil {
		return 0, a.refusePatch(ctx, id, in.Source, err)
	}
	seq, err := a.Log.ApplyMetadata(ctx, id, in.Patch.Metadata)
	switch {
	case errors.Is(err, a2a.ErrInvalidParams):
		return 0, a.refusePatch(ctx, id, in.Source, err)
	case err != nil:
		return 0, taskGone(err)
	}
	return seq, nil
}

// refusePatch records the refusal in the audit chain and returns the non-retryable error. The audit row
// carries the task, the patch's source and the class of refusal, never the patch.
func (a *LifecycleActivities) refusePatch(ctx context.Context, taskID uuid.UUID, source string, cause error) error {
	class := "invalid"
	switch {
	case errors.Is(cause, hooks.ErrForbiddenKey):
		class = "forbidden_key"
	case errors.Is(cause, hooks.ErrMapTooLarge), errors.Is(cause, a2a.ErrInvalidParams):
		class = "too_large"
	}
	a.logger().Warn("operator patch refused and dropped", "task_id", taskID.String(), "source", source, "refusal", class)
	err := pgx.BeginFunc(ctx, a.Log.DB(), func(tx pgx.Tx) error {
		_, err := audit.Append(ctx, tx, EventPatchRefused, audit.Fields{"task_id": taskID, "source": source, "refusal": class})
		return err
	})
	if err != nil {
		a.logger().Warn("audit row for a refused patch", "task_id", taskID.String(), "error", err)
	}
	return nonRetryable(ErrTypePatchRefused, fmt.Errorf("patch from %s refused: %s", source, class))
}

// Submit is the approval handler's submitted write (spec §13.2, §14.1): the approval's metadata as one
// metadata event, then the task's move from quoted to submitted, at which the store fixes tasks.deadline
// (its task.timeout clock starts, §5.2). It returns the deadline. A retry after a partial write finds the
// task submitted and returns its deadline (or applies the same metadata patch again, which changes nothing).
func (a *LifecycleActivities) Submit(ctx context.Context, in SubmitInput) (SubmitResult, error) {
	id, err := parseTask(in.TaskID)
	if err != nil {
		return SubmitResult{}, err
	}
	if err := a.Rules.CheckOperatorMaps(in.Metadata, nil); err != nil {
		return SubmitResult{}, a.refusePatch(ctx, id, "kontra.approval", err)
	}
	row, err := a.Log.Load(ctx, id)
	if err != nil {
		return SubmitResult{}, taskGone(err)
	}
	if row.State == store.StateQuoted && len(in.Metadata) > 0 {
		if _, err := a.Log.ApplyMetadata(ctx, id, in.Metadata); err != nil {
			if errors.Is(err, a2a.ErrInvalidParams) {
				return SubmitResult{}, a.refusePatch(ctx, id, "kontra.approval", err)
			}
			return SubmitResult{}, taskGone(err)
		}
	}
	var last error
	for range 8 {
		row, err := a.Log.Load(ctx, id)
		if err != nil {
			return SubmitResult{}, taskGone(err)
		}
		switch {
		case row.State.Terminal():
			return SubmitResult{}, nonRetryable(ErrTypeTaskGone, fmt.Errorf("the task is %s", row.State))
		case row.State != store.StateQuoted:
			if row.Deadline == nil {
				return SubmitResult{}, fmt.Errorf("task %s is %s without a deadline", id, row.State)
			}
			return SubmitResult{Deadline: row.Deadline.UTC()}, nil
		}
		ev := a2a.NewStatusUpdateEvent(row.Snapshot, a2a.TaskStateSubmitted, nil)
		next, err := a2aevent.ApplyShallowUpdate(row.Snapshot, ev)
		if err != nil {
			return SubmitResult{}, err
		}
		payload, err := json.Marshal(a2a.StreamResponse{Event: ev})
		if err != nil {
			return SubmitResult{}, err
		}
		ap, err := a.Log.Append(ctx, nil, store.Write{
			TaskID: id, PrevVersion: row.Version, Kind: store.KindStatus, State: store.StateSubmitted,
			StatusAt: store.StatusTime(ev.Status.Timestamp), Snapshot: next, Payload: payload,
			Timeout: time.Duration(in.TimeoutSeconds) * time.Second,
		})
		if err == nil {
			if ap.Deadline == nil {
				return SubmitResult{}, fmt.Errorf("task %s: the submitted write returned no deadline", id)
			}
			return SubmitResult{Deadline: ap.Deadline.UTC()}, nil
		}
		if !errors.Is(err, taskstore.ErrConcurrentModification) {
			return SubmitResult{}, err
		}
		last = err
	}
	return SubmitResult{}, last
}

// TaskRef builds the hooks' TaskRef from Postgres (spec §13.3): ids, the task's state and only the
// operator-written metadata keys (the operator prefixes and kontra.keyRetainUntil), at most 16 KiB, never
// caller or service metadata.
func (a *LifecycleActivities) TaskRef(ctx context.Context, taskID string) (hooks.TaskRef, error) {
	id, err := parseTask(taskID)
	if err != nil {
		return hooks.TaskRef{}, err
	}
	row, err := a.Log.Load(ctx, id)
	if err != nil {
		return hooks.TaskRef{}, taskGone(err)
	}
	svc, err := a.Log.LoadService(ctx, row.TenantID, row.Service)
	if err != nil {
		return hooks.TaskRef{}, err
	}
	ref := hooks.TaskRef{
		TaskID: row.ID.String(), TenantID: row.TenantID.String(), Service: row.Service, Kind: svc.Kind,
		Protocol: string(row.Protocol), BuyerID: row.Owner.BuyerID.String(), Principal: row.Owner.Principal,
		Via: row.Owner.Via, ContextID: row.ContextID, State: string(row.State),
	}
	if row.Snapshot != nil {
		ref.Metadata = a.operatorKeys(row.Snapshot.Metadata)
	}
	return ref, nil
}

// operatorKeys returns the operator-written keys of md, in key order, as many as fit in 16 KiB.
func (a *LifecycleActivities) operatorKeys(md map[string]any) map[string]any {
	var keys []string
	for k := range md {
		if k == hooks.KeyRetainUntil || slices.ContainsFunc(a.Rules.OperatorPrefixes, func(p string) bool { return p != "" && strings.HasPrefix(k, p) }) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var out map[string]any
	for _, k := range keys {
		if out == nil {
			out = map[string]any{}
		}
		out[k] = md[k]
		if b, err := json.Marshal(out); err != nil || len(b) > hooks.MaxMapBytes {
			delete(out, k)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AppendReviewMessage is kontra.review_message's append (spec §7, §14.1, D66): it consumes the work item
// and appends its message in one transaction and returns the row's seq, again when retried after an append
// that committed. A work item that is missing, another task's, or consumed by a dispatch is
// ErrTypeWorkItemGone: the Update answers accepted: false.
func (a *LifecycleActivities) AppendReviewMessage(ctx context.Context, in ReviewAppendInput) (int64, error) {
	id, err := parseTask(in.TaskID)
	if err != nil {
		return 0, err
	}
	wid, err := uuid.Parse(in.WorkItemID)
	if err != nil {
		return 0, nonRetryable(ErrTypeWorkItemGone, fmt.Errorf("work item id %q", in.WorkItemID))
	}
	seq, err := a.Log.AppendReviewMessage(ctx, id, wid)
	switch {
	case errors.Is(err, store.ErrWorkItemNotFound), errors.Is(err, store.ErrWorkItemConsumed):
		return 0, nonRetryable(ErrTypeWorkItemGone, fmt.Errorf("work item %s is not available to task %s", wid, id))
	case errors.Is(err, taskstore.ErrConcurrentModification):
		// The terminal rule refused it (the task is not in a state that takes it): nothing was written.
		return 0, nonRetryable(ErrTypeWorkItemGone, fmt.Errorf("task %s refused the review message", id))
	case err != nil:
		return 0, taskGone(err)
	}
	return seq, nil
}
