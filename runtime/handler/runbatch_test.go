package main

// The run path's contract after ADR 0018: ONE RunBatch per Batch, scheduled BY NAME onto the
// actor's own task queue, with Close on every exit path.
//
// This replaces `turns_test.go` and its time-boxed turn loop. What survives from it is the part
// that was never about the transport: a run must not leak a loaded resource, and a failed run is
// exactly when that matters most.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"github.com/medmahmoudi26/kontra/runtime/handler/internal/identity"
	"github.com/medmahmoudi26/kontra/runtime/handler/internal/wire"
)

// runEnv wires a TestWorkflowEnvironment with the activities the run path touches. RunBatch and
// Close are registered BY NAME because the handler no longer implements them — the actor's own
// worker does, and the workflow only ever names them.
//
// `store` is optional: pass one to observe what the workflow blobbed. Without it StoreBlob is
// permissive and asserts nothing — which is how the run path's entire output contract went
// uncovered until storeEnv existed.
func runEnv(t *testing.T, runBatch func(RunBatchInput) (map[string]any, error),
	store ...func(any) wire.BareRef) (
	env *testsuite.TestWorkflowEnvironment, runs *int, closes *int, queues *[]string) {
	t.Helper()
	ts := &testsuite.WorkflowTestSuite{}
	env = ts.NewTestWorkflowEnvironment()
	a := &Activities{}
	env.RegisterActivityWithOptions(a.StoreBlob, activity.RegisterOptions{Name: identity.StoreBlobActivity})

	runs, closes, queues = new(int), new(int), new([]string)

	env.RegisterActivityWithOptions(
		func(ctx context.Context, in RunBatchInput) (map[string]any, error) { return nil, nil },
		activity.RegisterOptions{Name: "RunBatch"})
	env.RegisterActivityWithOptions(
		func(ctx context.Context, in map[string]any) (map[string]any, error) { return nil, nil },
		activity.RegisterOptions{Name: "Close"})
	// Registered here, unmocked, because the test env panics on any RegisterActivity that
	// FOLLOWS an OnActivity — a test wanting to stub the fetch can only reach it via OnActivity
	// after this point.
	env.RegisterActivityWithOptions(
		func(ctx context.Context, ref wire.BareRef) (any, error) { return nil, nil },
		activity.RegisterOptions{Name: identity.FetchBlobActivity})

	env.OnActivity("RunBatch", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in RunBatchInput) (map[string]any, error) {
			*runs++
			*queues = append(*queues, activity.GetInfo(ctx).TaskQueue)
			return runBatch(in)
		})
	env.OnActivity("Close", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in map[string]any) (map[string]any, error) {
			*closes++
			*queues = append(*queues, activity.GetInfo(ctx).TaskQueue)
			return map[string]any{"closed": true}, nil
		})
	if len(store) > 0 && store[0] != nil {
		env.OnActivity(identity.StoreBlobActivity, mock.Anything, mock.Anything).Return(
			func(ctx context.Context, payload any) (wire.BareRef, error) {
				return store[0](payload), nil
			})
	} else {
		env.OnActivity(identity.StoreBlobActivity, mock.Anything, mock.Anything).
			Return(wire.BareRef{}, nil).Maybe()
	}
	return env, runs, closes, queues
}

// storeEnv is runEnv with a CAPTURING StoreBlob: it records every payload the workflow stored,
// in order, and returns a distinguishable ref per call.
//
// runEnv's `.Return(wire.BareRef{}, nil).Maybe()` asserts nothing about its argument, which
// meant every test here passed no matter WHAT the returned ref addressed — the one thing the
// run path's whole output contract rests on. This is the first coverage of it.
func storeEnv(t *testing.T, runBatch func(RunBatchInput) (map[string]any, error)) (
	*testsuite.TestWorkflowEnvironment, *[]any) {
	t.Helper()
	stored := new([]any)
	env, _, _, _ := runEnv(t, runBatch, func(payload any) wire.BareRef {
		*stored = append(*stored, payload)
		kind := "envelope"
		if _, ok := payload.([]any); ok {
			kind = "units"
		}
		return wire.BareRef{
			Sha256: fmt.Sprintf("sha%d", len(*stored)),
			Meta:   map[string]string{"kind": kind},
		}
	})
	return env, stored
}

func TestTheResultRefAddressesABareListOfUnits(t *testing.T) {
	// The governing invariant of "a Method returns a Batch": what the ref addresses must be
	// the same shape an input ref accepts (a bare list), or chaining forces the caller to
	// fetch every row and re-send it — the ADR 0007 violation this removed.
	env, stored := storeEnv(t, func(in RunBatchInput) (map[string]any, error) {
		return map[string]any{
			"done":     true,
			"results":  []any{map[string]any{"a": 1}, map[string]any{"b": 2}},
			"failures": []any{},
			"opens":    1,
		}, nil
	})
	env.ExecuteWorkflow(new(Activities).RunWorkflow, entry(t))
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	require.Len(t, *stored, 1, "a clean batch stores exactly one blob")
	list, ok := (*stored)[0].([]any)
	require.True(t, ok, "the stored payload must be a bare list, got %T", (*stored)[0])
	require.Len(t, list, 2)

	var ref wire.BareRef
	require.NoError(t, env.GetWorkflowResult(&ref))
	require.Equal(t, "units", ref.Meta["kind"])
	require.Equal(t, "2", ref.Meta["n"])
	require.Equal(t, "true", ref.Meta["done"])
	require.NotContains(t, ref.Meta, "isolated", "a clean batch names no failures")
	require.NotContains(t, ref.Meta, "failures")
}

func TestTheResultRefNamesTheMachineThatRanTheMethod(t *testing.T) {
	// Provenance travels with the Batch: the caller's Batch reads this key off the meta with no
	// fetch and publish writes it as the row's Machine. Without it a four-Machine run lands
	// every row carrying one node — measured on `nscheck`: 1,246 rows, `node='w'`, one Machine
	// where there were four.
	//
	// IT COMES OFF THE ENVELOPE. Reading this process's own hostname instead would name the
	// Machine whose HANDLER took the workflow task, which is not the Machine whose ACTOR ran the
	// Method — they poll different queues and nothing pins them together.
	env, _ := storeEnv(t, func(in RunBatchInput) (map[string]any, error) {
		return map[string]any{
			"done":     true,
			"results":  []any{map[string]any{"a": 1}},
			"failures": []any{},
			"machine":  "kf-dns-01",
		}, nil
	})
	env.ExecuteWorkflow(new(Activities).RunWorkflow, entry(t))
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var ref wire.BareRef
	require.NoError(t, env.GetWorkflowResult(&ref))
	require.Equal(t, "kf-dns-01", ref.Meta["machine"])
}

func TestAnActorHostThatNamesNoMachineLeavesTheKeyOff(t *testing.T) {
	// An actor host older than this contract. Unrecorded must stay unrecorded all the way down:
	// the key is absent, the caller's Batch reads "", and the publish activity writes SQL NULL.
	// Stamping the empty string here would be the same mistake `'w'` was — a value competing
	// with the real ones.
	env, _ := storeEnv(t, func(in RunBatchInput) (map[string]any, error) {
		return map[string]any{"done": true, "results": []any{}, "failures": []any{}}, nil
	})
	env.ExecuteWorkflow(new(Activities).RunWorkflow, entry(t))
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var ref wire.BareRef
	require.NoError(t, env.GetWorkflowResult(&ref))
	require.NotContains(t, ref.Meta, "machine")
}

func TestFailuresRideAsTheirOwnBlobNamedOnTheRef(t *testing.T) {
	// The Nexus op returns exactly one BareRef, so once it addresses only the units there is
	// no second slot. A caller must still be able to see THAT units were dropped, and how
	// many, without dereferencing anything.
	env, stored := storeEnv(t, func(in RunBatchInput) (map[string]any, error) {
		return map[string]any{
			"done":    false,
			"results": []any{map[string]any{"a": 1}},
			"failures": []any{
				map[string]any{"unit": "x", "error": map[string]any{"message": "boom"}},
			},
		}, nil
	})
	env.ExecuteWorkflow(new(Activities).RunWorkflow, entry(t))
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	require.Len(t, *stored, 2, "units and failures are two blobs")
	units, ok := (*stored)[0].([]any)
	require.True(t, ok)
	require.Len(t, units, 1)
	drops, ok := (*stored)[1].([]any)
	require.True(t, ok, "failures are stored as a bare list too")
	require.Len(t, drops, 1)

	var ref wire.BareRef
	require.NoError(t, env.GetWorkflowResult(&ref))
	require.Equal(t, "1", ref.Meta["isolated"])
	require.Equal(t, "sha2", ref.Meta["failures"], "the second blob's sha, not the first's")
	require.Equal(t, "false", ref.Meta["done"])
}

func TestAnInputRefMayAddressALegacyEnvelope(t *testing.T) {
	// A ref minted before the split, or carried across a version boundary. The alternative is
	// a raw "cannot unmarshal object into Go value of type []interface {}" from an activity
	// result decode, which names neither the cause nor the fix.
	var seen []any
	env, _ := storeEnv(t, func(in RunBatchInput) (map[string]any, error) {
		seen = in.Units
		return map[string]any{"done": true, "results": []any{}, "failures": []any{}}, nil
	})
	env.OnActivity(identity.FetchBlobActivity, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, ref wire.BareRef) (any, error) {
			return map[string]any{"results": []any{"u1", "u2"}, "failures": []any{}}, nil
		})

	env.ExecuteWorkflow(new(Activities).RunWorkflow, wire.EntryInput{
		RunID: "r", NodeID: "n1", InputRef: &wire.BareRef{Sha256: "old"},
	})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []any{"u1", "u2"}, seen, "the envelope's results become the batch")
}

func entry(t *testing.T) wire.EntryInput {
	t.Helper()
	return wire.EntryInput{RunID: "r", NodeID: "n1", Units: []any{map[string]any{"id": 1}}}
}

func TestOneActivityPerBatch(t *testing.T) {
	// The turn loop is gone. A host that returns `done:false` — which nothing does any more,
	// but which an old host might — must NOT cause a second call: a resource loads exactly once
	// per batch by construction now, and re-invoking would silently reload a browser mid-run.
	env, runs, closes, _ := runEnv(t, func(in RunBatchInput) (map[string]any, error) {
		return map[string]any{"done": false, "results": []any{}, "failures": []any{}}, nil
	})
	env.ExecuteWorkflow(new(Activities).RunWorkflow, entry(t))

	require.True(t, env.IsWorkflowCompleted())
	require.Equal(t, 1, *runs, "RunBatch must run exactly once per Batch")
	require.Equal(t, 1, *closes)
}

func TestAnUnscopedBatchGoesToTheActorsSharedQueue(t *testing.T) {
	// No Session id on the dispatch means no scope was opened: the batch runs on the queue every
	// worker of this actor polls, bracketed by load and close — which is what an Activity is in
	// v2 (ADR 0023 §9). If the workflow schedules onto its OWN queue instead, nothing is
	// listening: the run hangs until StartToClose with no error that names the cause.
	env, _, _, queues := runEnv(t, func(in RunBatchInput) (map[string]any, error) {
		return map[string]any{"done": true, "results": []any{}, "failures": []any{}}, nil
	})
	env.ExecuteWorkflow(new(Activities).RunWorkflow, entry(t))

	require.True(t, env.IsWorkflowCompleted())
	require.NotEmpty(t, *queues)
	for _, q := range *queues {
		require.Contains(t, q, "-sessions", "activity scheduled off the actor's queue")
	}
}

func TestAScopedBatchIsAddressedToItsOwnSessionQueue(t *testing.T) {
	// ADR 0023 §6. The queue name is the address, and the Session id in the dispatch is what
	// names it — the handler no longer derives one queue for every Session of an actor by
	// appending a suffix. Landing on the shared queue instead is not an error anybody sees: the
	// batch runs, against a process that never activated this Session and holds none of its
	// state, and reports success.
	env, _, _, queues := runEnv(t, func(in RunBatchInput) (map[string]any, error) {
		return map[string]any{"done": true, "results": []any{}, "failures": []any{}}, nil
	})
	in := entry(t)
	in.SessionID = "3f9a2b1c4d5e"
	env.ExecuteWorkflow(new(Activities).RunWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.NotEmpty(t, *queues)
	for _, q := range *queues {
		require.True(t, strings.HasSuffix(q, "-s-3f9a2b1c4d5e"), "scheduled onto %q", q)
		require.NotContains(t, q, "-sessions", "a scoped call must not go to the shared queue")
	}
}

func TestAScopedBatchLeavesTheSessionOpen(t *testing.T) {
	// The caller's `async with` owns the lifetime now (ADR 0023 §4). Closing after every Method
	// call would tear the resource down between two calls of one scope — the browser `crawl`
	// opened would be gone by `extract`, and `self.*` with it, which is precisely the silent
	// reset §7 refuses.
	env, _, closes, _ := runEnv(t, func(in RunBatchInput) (map[string]any, error) {
		return map[string]any{"done": true, "results": []any{}, "failures": []any{}}, nil
	})
	in := entry(t)
	in.SessionID = "3f9a2b1c4d5e"
	env.ExecuteWorkflow(new(Activities).RunWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.Equal(t, 0, *closes, "a scoped batch must not close the Session it was called in")
}

func TestAScopedBatchLeavesTheSessionOpenEvenWhenItFails(t *testing.T) {
	// A failed Batch is not a failed Session. The caller sees the error inside its scope and may
	// well call another Method on the same instance to find out why; ending the Session under it
	// would answer a different question.
	env, _, closes, _ := runEnv(t, func(in RunBatchInput) (map[string]any, error) {
		return nil, context.DeadlineExceeded
	})
	in := entry(t)
	in.SessionID = "3f9a2b1c4d5e"
	env.ExecuteWorkflow(new(Activities).RunWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	require.Equal(t, 0, *closes)
}

func TestTheSessionIdIsTheActorWhenNothingIsKeyed(t *testing.T) {
	// Every call in one scope must meet the SAME instance, and the actor id is what picks it.
	// A per-dispatch node id would give each Method call its own instance, which is a Session in
	// name only.
	var seen string
	env, _, _, _ := runEnv(t, func(in RunBatchInput) (map[string]any, error) {
		seen = in.ActorID
		return map[string]any{"done": true, "results": []any{}, "failures": []any{}}, nil
	})
	in := entry(t)
	in.SessionID = "3f9a2b1c4d5e"
	env.ExecuteWorkflow(new(Activities).RunWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.Equal(t, "3f9a2b1c4d5e", seen)
}

func TestAKeyOutranksTheSessionId(t *testing.T) {
	// A key is a claim on a shared identity (ADR 0023 §10) and it is what `object_state` is
	// scoped to (ADR 0022). Two scopes on one key are two Sessions of ONE virtual object, so the
	// key — not the Session — has to be the actor id, or the durable state of `crawler["acme.com"]`
	// would be a different namespace every time somebody opened it.
	var seen string
	env, _, _, _ := runEnv(t, func(in RunBatchInput) (map[string]any, error) {
		seen = in.ActorID
		return map[string]any{"done": true, "results": []any{}, "failures": []any{}}, nil
	})
	in := entry(t)
	in.SessionID = "3f9a2b1c4d5e"
	in.IdempotencyKey = "acme.com"
	env.ExecuteWorkflow(new(Activities).RunWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.Equal(t, "acme.com", seen)
}

func TestAnOrphanedSessionQueueIsBoundedByScheduleToStart(t *testing.T) {
	// Losing the host fails the scope (ADR 0023 §7). What makes that a decision rather than an
	// accident is this bound: the Session's queue has exactly one poller, so when that process
	// dies the queue is orphaned and the call would otherwise sit there until StartToClose — an
	// hour of a caller's Run spent waiting on a worker that is never coming back.
	scoped := runActivityOptions("crawler-0.1.0", "3f9a")
	require.Equal(t, "crawler-0.1.0-s-3f9a", scoped.TaskQueue)
	require.Positive(t, scoped.ScheduleToStartTimeout, "an orphaned session queue would hang")

	// The shared queue is a different case and takes no such bound: every worker of the actor
	// polls it, and a batch queued while the fleet scales up is waiting for capacity, not for a
	// dead process.
	shared := runActivityOptions("crawler-0.1.0", "")
	require.Equal(t, "crawler-0.1.0-sessions", shared.TaskQueue)
	require.Zero(t, shared.ScheduleToStartTimeout)
}

func TestCloseRunsWhenTheBatchFails(t *testing.T) {
	// A failed run is exactly when a loaded resource must not leak — a browser left open on a
	// fleet Machine survives the run that opened it.
	env, _, closes, _ := runEnv(t, func(in RunBatchInput) (map[string]any, error) {
		return nil, context.DeadlineExceeded
	})
	env.ExecuteWorkflow(new(Activities).RunWorkflow, entry(t))

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	require.Equal(t, 1, *closes, "Close must run on every exit path")
}

func TestTheMethodNameReachesTheActor(t *testing.T) {
	// An actor declares as many Methods as it has jobs (ADR 0023 §5) and the caller names one.
	// The actor's registry resolves that name off the RunBatch payload, so a workflow that drops
	// it does not fail — it runs the sole Method, or refuses a multi-Method actor with an error
	// that blames the author rather than the lost field.
	var seen string
	env, _, _, _ := runEnv(t, func(in RunBatchInput) (map[string]any, error) {
		seen = in.Method
		return map[string]any{"done": true, "results": []any{}, "failures": []any{}}, nil
	})
	in := entry(t)
	in.Method = "extract"
	env.ExecuteWorkflow(new(Activities).RunWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.Equal(t, "extract", seen)
}

func TestRunDateComesFromTheWorkflowNotTheWorker(t *testing.T) {
	// Every worker on one run must write to ONE dt partition, even if the run crosses midnight
	// and even across an activity retry on a different machine.
	var seen string
	env, _, _, _ := runEnv(t, func(in RunBatchInput) (map[string]any, error) {
		seen = in.RunDate
		return map[string]any{"done": true, "results": []any{}, "failures": []any{}}, nil
	})
	env.ExecuteWorkflow(new(Activities).RunWorkflow, entry(t))

	require.True(t, env.IsWorkflowCompleted())
	require.Regexp(t, `^\d{4}-\d{2}-\d{2}$`, seen)
}
