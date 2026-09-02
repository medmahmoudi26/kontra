package catalog

// The caller's half of ADR 0028 §4 on the Go side: a Method call reaches a Method off a bare
// handle (no Open) and hands back results, drops and an error as three DISTINCT values — the peer
// of Python's test_the_handle_is_callable_and_a_method_call_returns_results_and_dropped and
// test_the_same_spelling_inside_a_scope_pins_to_the_session.
//
// The Nexus dispatch is the one seam a call reaches, so it is mocked (OnNexusOperation, the peer of
// the Python suite stubbing its own wire seam): the mock captures the EntryInput — which is where the
// routing is checked — and returns a chosen ref, so the two claims (no scope -> shared queue, empty
// session id; an open scope -> that session's id) are asserted on the bytes that go on the wire.

import (
	"testing"

	"github.com/nexus-rpc/sdk-go/nexus"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// runOp is the operation the dispatch targets — service kontra.actor, operation run, EntryInput in,
// BareRef out. Declared with the same names dispatch uses so the mock matches the real call.
var runOp = nexus.NewOperationReference[EntryInput, BareRef](RunOperation)

// callResult drives one workflow that calls fn (a handle or session Call) and returns the three
// values it produced plus the EntryInput the dispatch put on the wire.
type callResult struct {
	n, dropped int
	actor, ver string
	entry      EntryInput
	err        error
}

func runCall(t *testing.T, ref BareRef, fn func(workflow.Context) (*Batch, *Dropped, error)) callResult {
	t.Helper()
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	var captured EntryInput
	env.OnNexusOperation(ServiceName, runOp, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { captured = args.Get(1).(EntryInput) }).
		Return(&nexus.HandlerStartOperationResultSync[BareRef]{Value: ref}, nil)

	var res callResult
	wf := func(ctx workflow.Context) error {
		b, d, err := fn(ctx)
		res.err = err
		if err != nil {
			return err
		}
		res.n, res.dropped = b.N, d.Len()
		res.actor, res.ver = b.actor, b.version
		return nil
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)
	if !env.IsWorkflowCompleted() {
		t.Fatal("the calling workflow never completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	res.entry = captured
	return res
}

// A ref with four committed units and one dropped — the shape a Method call hands back.
func refWithDrop() BareRef {
	return BareRef{Sha256: "abc", Meta: map[string]string{
		"n": "4", "isolated": "1", "failures": "deadbeef", "done": "true",
	}}
}

// The hole this slice fills: a Go caller could reach a Method only through a Session, because Call
// hung off *Session. Now a bare handle calls it — no Open — and gets (results, dropped, err), with
// the dispatch riding the actor's SHARED queue (an empty session id), not a per-session one.
func TestAHandleCallReachesAMethodWithNoOpen(t *testing.T) {
	handle := Actor("nscheck", "0.1.0")
	res := runCall(t, refWithDrop(), func(ctx workflow.Context) (*Batch, *Dropped, error) {
		return handle.Call(ctx, "delegation", []any{map[string]any{"domain": "a"}})
	})

	if res.n != 4 || res.dropped != 1 {
		t.Errorf("results.N=%d dropped.Len=%d, want 4 and 1", res.n, res.dropped)
	}
	if res.entry.Method != "delegation" {
		t.Errorf("method on the wire = %q, want delegation", res.entry.Method)
	}
	if res.entry.SessionID != "" {
		t.Errorf("session id = %q, want empty — no scope means the shared queue", res.entry.SessionID)
	}
	// The results Batch remembers who produced it, so it can chain and its rows dereference on the
	// right queue — the identity the old hand-assembled batchFromRef re-passed by hand.
	if res.actor != "nscheck" || res.ver != "0.1.0" {
		t.Errorf("results identity = %s@%s, want nscheck@0.1.0", res.actor, res.ver)
	}
}

// The same spelling inside an opened Session carries the session id, so it lands on that Session's
// own queue and reaches the one pinned process (ADR 0023 §6) — the same call, the same three
// values, a different route.
func TestTheSameCallInsideAnOpenedSessionPinsToTheSession(t *testing.T) {
	s := &Session{handle: Actor("crawler", "0.1.0"), id: "sid123"} // stand in for an opened scope
	res := runCall(t, BareRef{Meta: map[string]string{"n": "2"}}, func(ctx workflow.Context) (*Batch, *Dropped, error) {
		return s.Call(ctx, "crawl", []any{1, 2})
	})

	if res.n != 2 || res.dropped != 0 {
		t.Errorf("results.N=%d dropped.Len=%d, want 2 and 0", res.n, res.dropped)
	}
	if res.entry.Method != "crawl" {
		t.Errorf("method on the wire = %q, want crawl", res.entry.Method)
	}
	if res.entry.SessionID != "sid123" {
		t.Errorf("session id = %q, want sid123 — an open scope pins to its own queue", res.entry.SessionID)
	}
}

// Dropped everything vs found nothing: on the survivors alone they are identical (both N=0), and
// the drops are where they diverge — the failure ADR 0028 §4's return shape exists to prevent.
// Drops and errors are separate types, so a caller cannot read one as the other.
func TestDroppedEverythingIsDistinguishableFromFoundNothing(t *testing.T) {
	foundNothing := runCall(t, BareRef{Meta: map[string]string{"n": "0"}},
		func(ctx workflow.Context) (*Batch, *Dropped, error) {
			return Actor("a", "1").Call(ctx, "m", []any{1})
		})
	droppedAll := runCall(t, BareRef{Meta: map[string]string{"n": "0", "isolated": "3", "failures": "ff"}},
		func(ctx workflow.Context) (*Batch, *Dropped, error) {
			return Actor("a", "1").Call(ctx, "m", []any{1})
		})

	if foundNothing.n != 0 || droppedAll.n != 0 {
		t.Fatalf("both must be empty on results: %d and %d", foundNothing.n, droppedAll.n)
	}
	if foundNothing.dropped != 0 {
		t.Errorf("found nothing: dropped = %d, want 0", foundNothing.dropped)
	}
	if droppedAll.dropped != 3 {
		t.Errorf("dropped everything: dropped = %d, want 3", droppedAll.dropped)
	}
}
