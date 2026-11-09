package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/temporal"

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/store"
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

// PatchRefusals counts the operator patches apply_metadata refused and dropped, by the patch's
// source (a hook's type name, kontra.annotate) and the refusal's class: the metric of
// spec §13.3, kontra_gate_operator_patches_refused_total, so a hook or control plane that keeps sending
// refused patches raises an alert. Safe for concurrent use; the zero value is ready.
type PatchRefusals struct {
	mu sync.Mutex
	n  map[[2]string]int64
}

// PatchRefusal is one series of PatchRefusals.
type PatchRefusal struct {
	Source, Refusal string
	N               int64
}

// Add counts one refused patch.
func (p *PatchRefusals) Add(source, refusal string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n == nil {
		p.n = map[[2]string]int64{}
	}
	p.n[[2]string{source, refusal}]++
}

// Counts returns every series, sorted by source then refusal.
func (p *PatchRefusals) Counts() []PatchRefusal {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PatchRefusal, 0, len(p.n))
	for k, n := range p.n {
		out = append(out, PatchRefusal{Source: k[0], Refusal: k[1], N: n})
	}
	slices.SortFunc(out, func(a, b PatchRefusal) int {
		if c := strings.Compare(a.Source, b.Source); c != 0 {
			return c
		}
		return strings.Compare(a.Refusal, b.Refusal)
	})
	return out
}

// LifecycleActivities are the activities on kontra-lifecycle, runnable on any replica. Every error they
// return crosses Temporal history as a failure message, so none carries caller or service content: they
// name ids, states and the gate's own refusals.
type LifecycleActivities struct {
	Log      *store.Log
	Backends BackendResolver
	// Sessions chooses the session of a tunnel service's attempt (spec §6.1; *tunnelgate.Picker). Without
	// it a tunnel service's prepare fails service_unavailable.
	Sessions SessionPicker
	Push     PushConfigLister // nil: no push configs are listed
	// Rules are the §13.1 key and size rules apply_metadata enforces on every operator patch,
	// and that pick the operator-written keys of a TaskRef.
	Rules hooks.KeyRules
	// Refusals counts the refused patches (spec §13.3's metric); nil counts nothing.
	Refusals *PatchRefusals
	Logger   *slog.Logger
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

// Prepare picks the backend (spec §6.1), ends the previous attempt when its dispatch was lost
// (PreviousOutcome), writes attempt N and returns the queue to dispatch on and the stored deadline. It is
// retried, so every write is idempotent: BeginRetry accepts an attempt that already exists with the same
// backend, and ends an open previous attempt only once.
//
// A direct service's attempt is dispatched on kontra-direct. A tunnel service's is placed on the session
// Sessions chooses (§6.1: the candidate query, round-robin, the context's previous session first, another
// than AvoidSession when one exists): the attempt records tunnel_session, key_id and replica_id, and is
// dispatched on QueueTunnel(replica), which only the replica holding the session polls (§14.2). With no
// candidate the attempt is written ended with outcome no_session and NoSession is answered: nothing is
// dispatched, and the workflow retries while task.retries allows (§6.4). A retried prepare answers what
// the attempt row says, so the session a first try wrote is the one dispatched to.
//
// A retry (N > 1) is refused, ErrTypeAttemptHasUpstream, when attempt N-1 records a first upstream task id:
// a dispatch Temporal timed out but whose process is still alive (D77) can record it after the workflow
// read attempt_state and decided to retry. Ending N-1 and writing N happen in one transaction with that
// check, so the old dispatch either recorded its id first (and the retry is refused) or is fenced from
// recording it (spec §6.4: a retry is only ever made before the first upstream task id).
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
	o := store.Outcome(in.PreviousOutcome)
	if o != "" && !o.Valid() {
		o = store.OutcomeFailed
	}
	var res PrepareResult
	switch backend {
	case store.BackendDirect:
		if res, err = a.prepareDirect(ctx, id, in.N, o); err != nil {
			return PrepareResult{}, err
		}
	case store.BackendTunnel:
		if res, err = a.prepareTunnel(ctx, row, in, o); err != nil {
			return PrepareResult{}, err
		}
	default:
		return PrepareResult{}, nonRetryable(ErrTypeServiceUnavailable, fmt.Errorf("service %s: backend %q is not served by this gate", row.Service, backend))
	}
	if row.Deadline != nil {
		res.Deadline = row.Deadline.UTC()
	}
	return res, nil
}

// beginRetry is BeginRetry with its refusals as the workflow reads them.
func (a *LifecycleActivities) beginRetry(ctx context.Context, att store.Attempt, prev store.Outcome) error {
	err := a.Log.BeginRetry(ctx, att, prev)
	if errors.Is(err, store.ErrAttemptHasUpstream) {
		return nonRetryable(ErrTypeAttemptHasUpstream,
			fmt.Errorf("attempt %d of task %s has a first upstream task id: it is not retried", att.N-1, att.TaskID))
	}
	if err != nil {
		return taskGone(err)
	}
	return nil
}

func (a *LifecycleActivities) prepareDirect(ctx context.Context, id uuid.UUID, n int, prev store.Outcome) (PrepareResult, error) {
	if err := a.beginRetry(ctx, store.Attempt{TaskID: id, N: n, Backend: store.BackendDirect}, prev); err != nil {
		return PrepareResult{}, err
	}
	return PrepareResult{Queue: QueueDirect, Backend: string(store.BackendDirect)}, nil
}

// prepareTunnel places attempt N of a tunnel service (spec §6.1, §6.4): on the chosen session, or written
// no_session when there is no candidate. A retried prepare whose first try wrote the attempt answers that
// attempt, without choosing again.
func (a *LifecycleActivities) prepareTunnel(ctx context.Context, row *store.TaskRow, in PrepareInput, prev store.Outcome) (PrepareResult, error) {
	id := row.ID
	if existing, err := a.Log.GetAttempt(ctx, id, in.N); err == nil && existing.Backend == store.BackendTunnel {
		return tunnelResult(existing)
	} else if err != nil && !errors.Is(err, store.ErrAttemptNotFound) {
		return PrepareResult{}, err
	}
	if a.Sessions == nil {
		return PrepareResult{}, nonRetryable(ErrTypeServiceUnavailable, fmt.Errorf("service %s: this gate places no tunnel attempts", row.Service))
	}
	req := SessionRequest{TenantID: row.TenantID, Service: row.Service, TaskID: id, N: in.N,
		BuyerID: row.Owner.BuyerID, ContextID: row.ContextID}
	if in.AvoidSession != "" {
		if s, err := uuid.Parse(in.AvoidSession); err == nil {
			req.Avoid = &s
		}
	}
	att := store.Attempt{TaskID: id, N: in.N, Backend: store.BackendTunnel}
	picked, err := a.Sessions.PickSession(ctx, req)
	switch {
	case errors.Is(err, ErrNoSession):
		att.Outcome = store.OutcomeNoSession
	case err != nil:
		return PrepareResult{}, err // a read that failed: prepare is retried
	default:
		att.TunnelSession, att.KeyID, att.ReplicaID = &picked.SessionID, picked.KeyID, picked.ReplicaID
	}
	if err := a.beginRetry(ctx, att, prev); err != nil {
		return PrepareResult{}, err
	}
	written, err := a.Log.GetAttempt(ctx, id, in.N)
	if err != nil {
		return PrepareResult{}, err
	}
	res, err := tunnelResult(written)
	if err == nil {
		log := a.logger().With("task_id", id.String(), "attempt", in.N)
		if res.NoSession {
			log.Info("no tunnel session serves the service; the attempt is no_session", "service", row.Service)
		} else {
			log.Info("tunnel attempt placed", "tunnel_session", res.SessionID, "replica", written.ReplicaID)
		}
	}
	return res, err
}

// tunnelResult is prepare's answer for a tunnel attempt as written.
func tunnelResult(att *store.Attempt) (PrepareResult, error) {
	res := PrepareResult{Backend: string(store.BackendTunnel)}
	switch {
	case att.Outcome == store.OutcomeNoSession:
		res.NoSession = true
	case att.TunnelSession == nil || att.ReplicaID == "":
		return PrepareResult{}, fmt.Errorf("attempt %d of task %s is a tunnel attempt without its session", att.N, att.TaskID)
	default:
		res.Queue, res.SessionID = QueueTunnel(att.ReplicaID), att.TunnelSession.String()
	}
	return res, nil
}

// AttemptState reads the task's state and, for N > 0, attempt N's first_upstream_id_at, replica_id and
// whether it ended: the retry and cancel decisions are taken on what Postgres recorded, never on heartbeat
// details, which a crash loses (spec §14.2).
//
// A task that is terminal while attempt N is still open had its terminal event logged by a dispatch that
// died before it ended its attempt (the Dispatcher ends it after a2a-go's handler returns): the workflow
// learns of the end here and never from that dispatch, so AttemptState ends the attempt itself, with the
// outcome the dispatch would have written (store.TerminalOutcome; spec §4, D77: ended_at and outcome are
// written when the attempt ends). The workflow never runs it beside write_terminal, which ends the latest
// attempt itself when it writes the terminal event.
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
	if row.State.Terminal() && att.EndedAt == nil {
		var reason string
		if row.Snapshot != nil {
			reason, _ = row.Snapshot.Metadata[store.MetaFailure].(string)
		}
		o := store.TerminalOutcome(row.State, store.FailureReason(reason))
		if err := a.Log.EndAttempt(ctx, id, in.N, o); err != nil {
			return AttemptState{}, err
		}
		a.logger().Info("ended an attempt its dispatch left open at the task's end", "task_id", id.String(), "attempt", in.N, "outcome", o)
		if att, err = a.Log.GetAttempt(ctx, id, in.N); err != nil {
			return AttemptState{}, err
		}
	}
	st.Backend, st.ReplicaID, st.Ended = string(att.Backend), att.ReplicaID, att.EndedAt != nil
	if att.FirstUpstreamIDAt != nil {
		t := att.FirstUpstreamIDAt.UTC()
		st.FirstUpstreamIDAt = &t
	}
	return st, nil
}

// ListPushConfigs returns the task's live push configs, at start and after each dispatch, to recover a
// lost kontra.push signal (spec §10.2), each named by its ConfigRef: the result crosses history, a config
// id may be the caller's. They are sorted, so the workflow starts them in a stable order.
func (a *LifecycleActivities) ListPushConfigs(ctx context.Context, taskID string) ([]string, error) {
	id, err := parseTask(taskID)
	if err != nil {
		return nil, err
	}
	if a.Push == nil {
		return nil, nil
	}
	cfgs, err := a.Push.LiveConfigIDs(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, ConfigRef(id, c))
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// WriteTerminal writes the terminal event through the store when no handler can (spec §14.2). It is
// idempotent: a task that has already ended is left as it is, and its terminal state returned. With
// EndAttempt it then ends the task's latest attempt if no dispatch ended it, with interrupted when the task
// was waiting at input-required (D77), but only when the task's terminal event is the one this write
// describes (this call's, or a retried call's that committed): a dispatch that logged its own terminal
// event and has not ended its attempt yet (the Dispatcher does after the handler returns) keeps the attempt
// for its own outcome, which EndAttempt would otherwise lose (it keeps an attempt's first outcome).
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
	if in.EndAttempt != "" && endedBy(after, w) {
		prior := before.State
		if in.EndAttemptAsIs {
			prior = "" // the outcome stands, whatever the task waited for
		}
		if err := a.endLatest(ctx, id, prior, store.Outcome(in.EndAttempt)); err != nil {
			return TerminalResult{}, err
		}
	}
	return TerminalResult{Seq: seq, State: string(after.State)}, nil
}

// endedBy reports whether the task's terminal event is the one w describes: the same state and, for failed
// and canceled, the same kontra.failure or kontra.canceled (keys only the gate writes, spec §2.5).
func endedBy(row *store.TaskRow, w store.TerminalWrite) bool {
	if row.State != w.State {
		return false
	}
	var md map[string]any
	if row.Snapshot != nil {
		md = row.Snapshot.Metadata
	}
	switch w.State {
	case store.StateFailed:
		r, _ := md[store.MetaFailure].(string)
		return r == string(w.Reason)
	case store.StateCanceled:
		c, _ := md[store.MetaCanceled].(string)
		return c == w.Canceled
	}
	return true
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
	// An MCP task keeps the patch's mcpMeta too (spec §13.3); an A2A task's store drops it.
	seq, err := a.Log.ApplyPatch(ctx, id, in.Patch.Metadata, in.Patch.MCPMeta)
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
	a.Refusals.Add(source, class)
	err := pgx.BeginFunc(ctx, a.Log.DB(), func(tx pgx.Tx) error {
		_, err := audit.Append(ctx, tx, EventPatchRefused, audit.Fields{"task_id": taskID, "source": source, "refusal": class})
		return err
	})
	if err != nil {
		a.logger().Warn("audit row for a refused patch", "task_id", taskID.String(), "error", err)
	}
	return nonRetryable(ErrTypePatchRefused, fmt.Errorf("patch from %s refused: %s", source, class))
}

// TaskRef builds the hooks' TaskRef from Postgres (spec §13.3): ids, the task's state and only the
// operator-written metadata keys (under the operator prefixes), at most 16 KiB, never
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
		if slices.ContainsFunc(a.Rules.OperatorPrefixes, func(p string) bool { return p != "" && strings.HasPrefix(k, p) }) {
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

// ReconnectText is the gate's waiting status text (D341): one sentence the buyer reads in band.
func ReconnectText(wait time.Duration) string {
	return "The agent's node disconnected. Waiting up to " + HumanDuration(wait) + " for it to reconnect."
}

// HumanDuration writes d for a person: whole hours in words, minutes (rounded from 2 minutes on), else seconds
// ("15 minutes", "1 hour", "7 minutes" for 7m23s, "90 seconds").
func HumanDuration(d time.Duration) string {
	plural := func(n int64, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return plural(int64(d/time.Hour), "hour")
	case d >= 2*time.Minute:
		return plural(int64((d+30*time.Second)/time.Minute), "minute")
	case d >= time.Minute && d%time.Minute == 0:
		return plural(int64(d/time.Minute), "minute")
	}
	return plural(int64(d/time.Second), "second")
}

// ReconnectWait logs the waiting status of attempt N's reconnect wait (relay.md §6.4, D341): working, the gate's text,
// metadata["kontra.reconnect"] = {state: waiting, since, until}; nothing for a task at input-required or
// auth-required, whose status stays the question. Idempotent (store.LogReconnectWait), so it is retried like every
// kontra-lifecycle activity.
func (a *LifecycleActivities) ReconnectWait(ctx context.Context, in ReconnectWaitInput) (ReconnectWaitResult, error) {
	id, err := parseTask(in.TaskID)
	if err != nil {
		return ReconnectWaitResult{}, err
	}
	w, err := a.Log.LogReconnectWait(ctx, id, in.Since, in.Until, ReconnectText(in.Until.Sub(in.Since)))
	if err != nil {
		return ReconnectWaitResult{}, taskGone(err)
	}
	a.logger().Info("the task waits for its node to reconnect", "task_id", id.String(), "attempt", in.N,
		"until", in.Until.UTC().Format(time.RFC3339), "status_logged", w.Logged, "state", w.State)
	return ReconnectWaitResult{Logged: w.Logged}, nil
}

// ReattachSession looks for a session attempt N may resume on (relay.md §6.4, D342): a connected session of the
// attempt's node key (attempts.key_id), or of a key it was rotated to, passing §6.1's checks for the task's service
// (key not revoked nor past a rotation's overlap, binding not revoked, a live replica that is not draining). Never
// another node's session: another node runs another agent instance. With Bind it records the session on the attempt
// (tunnel_session, key_id, replica_id) and answers its kontra-tunnel-<replica>, where the resume dispatch writes its
// own fence. Gone says why none can ever come: the key and the keys it was rotated to all revoked, the binding to the
// service revoked, or (with Bind) the attempt ended meanwhile.
func (a *LifecycleActivities) ReattachSession(ctx context.Context, in ReattachInput) (ReattachResult, error) {
	id, err := parseTask(in.TaskID)
	if err != nil {
		return ReattachResult{}, err
	}
	row, err := a.Log.Load(ctx, id)
	if err != nil {
		return ReattachResult{}, taskGone(err)
	}
	att, err := a.Log.GetAttempt(ctx, id, in.N)
	if errors.Is(err, store.ErrAttemptNotFound) {
		return ReattachResult{Gone: ReattachAttemptEnded}, nil
	}
	if err != nil {
		return ReattachResult{}, err
	}
	if att.Backend != store.BackendTunnel {
		return ReattachResult{Gone: ReattachAttemptEnded}, nil
	}
	if in.Bind && att.EndedAt != nil {
		return ReattachResult{Gone: ReattachAttemptEnded}, nil
	}
	c, gone, err := a.Log.FindReattachSession(ctx, row.TenantID, row.Service, att.KeyID)
	switch {
	case err != nil:
		return ReattachResult{}, err
	case gone != "":
		a.logger().Info("no session of the task's node can serve it again", "task_id", id.String(), "attempt", in.N,
			"key_id", att.KeyID, "gone", string(gone))
		return ReattachResult{Gone: string(gone)}, nil
	case c == nil:
		return ReattachResult{}, nil
	}
	if in.Bind {
		err := a.Log.RebindAttempt(ctx, id, in.N, c.SessionID, c.KeyID, c.ReplicaID)
		if errors.Is(err, store.ErrAttemptNotOpen) || errors.Is(err, store.ErrAttemptNotFound) {
			return ReattachResult{Gone: ReattachAttemptEnded}, nil
		}
		if err != nil {
			return ReattachResult{}, err
		}
		a.logger().Info("the task's node is back: resuming on its session", "task_id", id.String(), "attempt", in.N,
			"tunnel_session", c.SessionID.String(), "replica", c.ReplicaID, "key_id", c.KeyID)
	}
	return ReattachResult{Queue: QueueTunnel(c.ReplicaID), SessionID: c.SessionID.String(), ReplicaID: c.ReplicaID}, nil
}
