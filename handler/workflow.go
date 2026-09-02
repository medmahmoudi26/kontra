package main

import (
	"fmt"
	"strconv"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/medmahmoudi26/kontra-local/handler/internal/identity"
	"github.com/medmahmoudi26/kontra-local/handler/internal/wire"
)

// RunWorkflow is the backing workflow for the kontra.actor:run Nexus op. It rehydrates the
// batch if it arrived as a ref, runs it on the pinned actor instance (retryable = exactly-once
// reload), drives the best-effort close, and returns the result as a CAS ref (the op is
// BareRef-typed and the orchestrator always sets return_ref).
func (a *Activities) RunWorkflow(ctx workflow.Context, in wire.EntryInput) (*wire.BareRef, error) {
	units := in.Units

	// 1. Rehydrate the batch from a ref when it arrived out-of-band.
	//
	// The canonical shape is a bare list, which is what step 5 now stores. An ENVELOPE is
	// tolerated too — a ref minted before this change, or one a caller carried across a
	// version boundary — because the alternative is a raw
	// "cannot unmarshal object into Go value of type []interface {}" surfacing from an
	// activity result decode, which names neither the cause nor the fix.
	if len(units) == 0 && in.InputRef != nil {
		fetchAO := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: 5 * time.Minute,
		})
		var fetched any
		if err := workflow.ExecuteActivity(fetchAO, identity.FetchBlobActivity, *in.InputRef).Get(ctx, &fetched); err != nil {
			return nil, err
		}
		switch v := fetched.(type) {
		case []any:
			units = v
		case map[string]any:
			list, ok := v["results"].([]any)
			if !ok {
				return nil, temporal.NewNonRetryableApplicationError(
					"input_ref addresses an object with no `results` list; an input ref must "+
						"address a bare list of units", "BadInputRef", nil)
			}
			units = list
		default:
			return nil, temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("input_ref addresses a %T; expected a list of units", fetched),
				"BadInputRef", nil)
		}
	}

	// 2. Derive the actor id: idempotency key, else the Session, else run/node joined. It MUST
	//    be present (the orchestrator always mints a run_id): a fixed fallback would make
	//    id-less runs share one actor instance and read each other's committed state, so fail
	//    loud instead.
	//
	//    The Session comes second and not first because a key is a claim on a SHARED identity
	//    (ADR 0023 §10): two scopes on `crawler["acme.com"]` are two Sessions of one virtual
	//    object, and its durable object_state is scoped to the key (ADR 0022). An unkeyed scope
	//    takes the Session id, which gives every Method call in it the same instance — a
	//    per-dispatch node id would give each call its own, which is a Session in name only.
	//    Congruent with actorkit's `session_actor_id`, written independently on that side.
	actorID := in.IdempotencyKey
	if actorID == "" {
		actorID = in.SessionID
	}
	if actorID == "" {
		actorID = joinNonEmpty(in.RunID, in.NodeID)
	}
	if actorID == "" {
		return nil, temporal.NewNonRetryableApplicationError(
			"run has no idempotency_key/run_id/node_id to key the actor", "BadInput", nil)
	}

	// 2b. Tag this backing workflow with search attributes for the visibility-backed monitor
	//     (roadmap platform-x100 #04). KontraRunId lets the orchestrator DISCOVER every
	//     handler workflow of a run (incl. streaming chunks) via ListWorkflowExecutions —
	//     the read path for both `kontra runs` execution views and the #06 heartbeat
	//     read-back; KontraActor answers "which runs used actor X". Values are deterministic
	//     (in.RunID from the arg; handlerActorName snapshotted at boot, uniform per deploy),
	//     and the whole upsert is GetVersion-gated so in-flight histories — which carry no
	//     upsert command — replay identically. Best-effort: a not-yet-registered SA logs and
	//     continues rather than wedging the run (registration self-heals at orchestrator boot).
	if workflow.GetVersion(ctx, "kontra-search-attributes", workflow.DefaultVersion, 1) >= 1 {
		updates := []temporal.SearchAttributeUpdate{
			temporal.NewSearchAttributeKeyKeyword("KontraRunId").ValueSet(in.RunID),
		}
		if handlerActorName != "" {
			updates = append(updates, temporal.NewSearchAttributeKeyKeyword("KontraActor").ValueSet(handlerActorName))
		}
		if err := workflow.UpsertTypedSearchAttributes(ctx, updates...); err != nil {
			workflow.GetLogger(ctx).Warn("upsert search attributes failed (non-fatal)", "error", err)
		}
	}

	// 3. Run the batch. ONE activity, on the actor's own worker (ADR 0018).
	//
	//    The activity is scheduled BY NAME onto a queue the actor process polls directly — there
	//    is no sidecar and no Go implementation of RunBatch here. WHICH queue is the Session's
	//    business (ADR 0023 §6): a scoped dispatch names its Session and goes to that Session's
	//    own queue, polled by the single worker that activated it. Only an unscoped dispatch
	//    goes to the actor's shared "-sessions" queue. The stem is derived from this workflow's
	//    own task queue rather than from env, because workflow code must stay deterministic.
	//
	//    The multi-turn loop is gone with the sidecar. It existed only because the handler
	//    could not observe the actor: a turn was time-boxed so a wedged host returned rather
	//    than being killed by StartToClose, and HeartbeatTimeout could not help because the
	//    keepalive ticker beat even while the PUT hung. The actor heartbeats for itself now —
	//    once per committed unit — so HeartbeatTimeout is a true liveness check and
	//    StartToClose stops being a guillotine. A resource therefore loads exactly ONCE per
	//    batch, by construction.
	opts := runActivityOptions(workflow.GetInfo(ctx).TaskQueueName, in.SessionID)
	runAO := workflow.WithActivityOptions(ctx, opts)
	batch := RunBatchInput{
		ActorID: actorID,
		Units:   units,
		Params:  in.Params,
		RunID:   in.RunID,
		NodeID:  in.NodeID,
		// Which Method this dispatch means (ADR 0023 §5). Forwarded verbatim — the handler
		// does not know the actor's registry, so resolving (and refusing) is the callee's.
		Method: in.Method,
		// The RUN's date, not the worker's: taken from the workflow start so every worker on
		// this run writes to one dt partition even if the run crosses midnight. It comes from
		// workflow history, so it is stable across replays and activity retries.
		RunDate: workflow.GetInfo(ctx).WorkflowStartTime.UTC().Format("2006-01-02"),
	}
	var result map[string]any
	runErr := workflow.ExecuteActivity(runAO, "RunBatch", batch).Get(ctx, &result)
	// Isolation is not an error and will not fail the run, so it has to be VISIBLE. A node
	// that discarded every unit used to render identically to one that succeeded — that is how
	// a 15,814-target run reported `completed` in ~7 minutes having scanned almost nothing.
	// The count also rides out in the result envelope, which is what the orchestrator reads.
	if n := isolatedUnits(result); n > 0 {
		workflow.GetLogger(ctx).Warn("units permanently dropped",
			"node", in.NodeID, "isolated", n, "total", len(units))
	}

	// 4. Close the actor on EVERY exit path (a failed run is exactly when the loaded resource
	//    must not leak); bounded retry so a hung host can't wedge the workflow.
	//
	//    UNLESS THE CALL WAS SCOPED. A Session's lifetime belongs to the caller's `async with`
	//    (ADR 0023 §4), and closing here would tear the resource down between two Method calls
	//    of one scope: the browser `crawl` opened would be gone by `extract`, and `self.*` with
	//    it. That is the silent reset §7 refuses — so the scope's close is the caller's own
	//    CloseSession, and this path stays exactly as it was for an unscoped dispatch.
	if in.SessionID == "" {
		closeAO := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			TaskQueue:           opts.TaskQueue,
			StartToCloseTimeout: 30 * time.Second,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
		})
		_ = workflow.ExecuteActivity(closeAO, "Close",
			map[string]any{"actor_id": actorID}).Get(ctx, nil)
	}
	if runErr != nil {
		return nil, runErr
	}

	// 5. Store the result and return the ref.
	//
	// THE REF ADDRESSES A BARE LIST OF UNITS, not the {done, results, failures, opens}
	// envelope. That envelope is the callee<->handler contract and is unchanged; what changed
	// is only what goes BEHIND the ref. The reason is that an input ref must address a bare
	// list (step 1 above rehydrates one into []any), so a result ref addressing an envelope
	// could never be fed to the next Method — the caller had to fetch the rows and re-send
	// them, which is the ADR 0007 violation "a Method returns a Batch" removes.
	//
	// Failures do not fit in the same blob any more, and the Nexus op returns exactly one
	// BareRef. They ride as a SECOND CAS object named on the ref's meta, so a caller can see
	// THAT units were dropped, and how many, without dereferencing anything — and fetch the
	// rows only when it actually wants them.
	storeAO := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
	})
	emitted, _ := result["results"].([]any)
	if emitted == nil {
		emitted = []any{}
	}
	var ref wire.BareRef
	if err := workflow.ExecuteActivity(storeAO, identity.StoreBlobActivity, emitted).Get(ctx, &ref); err != nil {
		return nil, err
	}
	if ref.Meta == nil {
		ref.Meta = map[string]string{}
	}
	ref.Meta["n"] = strconv.Itoa(len(emitted))
	ref.Meta["done"] = strconv.FormatBool(result["done"] == true)
	// WHICH MACHINE RAN THE METHOD. It rides the meta beside `n` and `isolated` for the same
	// reason they do — a caller reads it without dereferencing anything — and it is what a
	// published row's provenance ultimately is: on a four-Machine `nscheck` run every one of
	// `lame`'s 1,246 rows said `node='w'`, so "which Machine produced this verdict" had no
	// answer at all.
	//
	// IT COMES OUT OF THE ENVELOPE, NOT OUT OF THIS PROCESS, and that is the whole correctness
	// of it. This workflow polls the actor's SHARED queue, which EVERY Machine's handler polls;
	// RunBatch is scheduled onto a different queue, polled by every Machine's actor host (or, in
	// a scope, by the single host that answered the open). Nothing pins the two together — one
	// Machine's handler routinely drives another Machine's Method — so this process's own
	// hostname would be a Machine picked at random, wearing measured clothes. It would also be
	// non-deterministic workflow code: a replay landing on a different handler would answer
	// differently for the same history.
	//
	// Absent when the actor host is older than this contract: the key is then simply not
	// written, and every reader downstream reports unrecorded rather than inventing a Machine.
	if m, _ := result["machine"].(string); m != "" {
		ref.Meta["machine"] = m
	}
	if n := isolatedUnits(result); n > 0 {
		var fref wire.BareRef
		drops, _ := result["failures"].([]any)
		if err := workflow.ExecuteActivity(
			storeAO, identity.StoreBlobActivity, drops).Get(ctx, &fref); err != nil {
			return nil, err
		}
		ref.Meta["isolated"] = strconv.Itoa(n)
		ref.Meta["failures"] = fref.Sha256
	}
	return &ref, nil
}

// sessionScheduleToStart bounds how long a scoped call waits to be PICKED UP. It is the whole
// mechanism behind "losing the host fails the scope" (ADR 0023 §7): a Session's queue has
// exactly one poller, so when that process dies nothing will ever take the task, and without
// this the caller's Run would sit on it for the StartToClose hour. A ScheduleToStart timeout is
// not retried by Temporal, which is right — retrying a queue nobody polls only spends the wait
// again. The caller catches it, reopens, and re-dispatches the Batch it was on.
const sessionScheduleToStart = time.Minute

// runActivityOptions addresses one RunBatch. `queue` is the handler's own task queue — the
// actor's shared queue — and `sessionID` is the scope the dispatch named, if any.
//
// Split out of the workflow because the addressing IS the decision and a wrong queue has no
// loud failure mode: a scoped call that lands on the shared queue runs, against a process that
// never activated this Session and holds none of its state, and reports success.
func runActivityOptions(queue, sessionID string) workflow.ActivityOptions {
	opts := workflow.ActivityOptions{
		// The actor's sessions queue: every worker of this version polls it, so a batch waiting
		// here is waiting for capacity and must not be bounded by ScheduleToStart. Derived
		// through identity rather than spelled inline — the suffix has one writer now.
		TaskQueue: identity.SessionsQueueOf(queue),
		// Generous, because it is no longer the thing that catches a wedge — the heartbeat is.
		// A legitimately long batch (a slow crawl of a big seed set) must not be killed for
		// taking its time while it is visibly making progress.
		StartToCloseTimeout: time.Hour,
		// Tight, because it is now a REAL liveness signal. The actor beats per committed unit,
		// so silence for two minutes means stuck, not busy.
		HeartbeatTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 10,
			InitialInterval: time.Second,
		},
	}
	if sessionID != "" {
		// The Session's own queue (ADR 0023 §6) — `{shared}-s-{sessionId}`, which the caller and
		// the actor host derive independently in three other languages. conformance/queues.json
		// §session is what holds the four to one answer.
		opts.TaskQueue = identity.SessionQueue(queue, sessionID)
		opts.ScheduleToStartTimeout = sessionScheduleToStart
	}
	return opts
}

// isolatedUnits counts units this turn PERMANENTLY DROPPED. Separate from committedUnits
// because the two answer opposite questions: committed is "how far along", isolated is
// "how much did we throw away". They were conflated — failures were summed INTO Done — so a
// node that isolated every unit reported 100% progress and then `completed`. That is exactly
// how a 15,814-target run reported success in ~7 minutes having scanned almost nothing.
func isolatedUnits(env map[string]any) int {
	if f, ok := env["failures"].([]any); ok {
		return len(f)
	}
	return 0
}
