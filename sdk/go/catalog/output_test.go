package catalog

// The Go caller's half of slices 07 and 08: a Method call names its output Dataset (Into) and can
// be taken as an iterator (Iter). Both seams are the ones a call reaches — the Nexus dispatch and
// the publishBatch activity — so both are mocked, and the assertions are on the bytes each leg puts
// on the wire and on how the input is partitioned across dispatches.

import (
	"context"
	"testing"

	"github.com/nexus-rpc/sdk-go/nexus"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// callEnv wires a workflow with the two mocks a Method call reaches: the Nexus dispatch (returning
// `ref`, capturing every EntryInput) and the publishBatch activity (capturing every payload). fn is
// the caller's body.
func callEnv(
	t *testing.T, ref BareRef, fn func(workflow.Context) error,
) (dispatched []EntryInput, published []map[string]any) {
	t.Helper()
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	env.OnNexusOperation(ServiceName, runOp, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { dispatched = append(dispatched, args.Get(1).(EntryInput)) }).
		Return(&nexus.HandlerStartOperationResultSync[BareRef]{Value: ref}, nil)

	env.RegisterActivityWithOptions(
		func(ctx context.Context, in map[string]any) (map[string]any, error) {
			published = append(published, in)
			return map[string]any{"rows": 1}, nil
		}, activity.RegisterOptions{Name: PublishBatchActivity})

	env.RegisterWorkflow(fn)
	env.ExecuteWorkflow(fn)
	if !env.IsWorkflowCompleted() {
		t.Fatal("the calling workflow never completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	return dispatched, published
}

// A ref with three committed units on a named Machine — enough that a publish actually runs.
func refOnMachine() BareRef {
	return BareRef{Sha256: "abc", Meta: map[string]string{"n": "3", "machine": "kf-dns-01"}}
}

func TestIntoPublishesTheResultsIntoAWriter(t *testing.T) {
	// The caller names the output Dataset with Into (ADR 0028 §2) and the call publishes for it,
	// forwarding the Batch's own Machine — so `w.Write(...)` leaves the caller's loop.
	out := Dataset("lame").Writer()
	_, published := callEnv(t, refOnMachine(), func(ctx workflow.Context) error {
		_, _, err := Actor("nscheck", "0.1.0").Call(ctx, "ask", []any{1, 2, 3}, Into(out))
		return err
	})
	if len(published) != 1 {
		t.Fatalf("published %d times, want 1", len(published))
	}
	if published[0]["dataset"] != "lame" || published[0]["machine"] != "kf-dns-01" {
		t.Errorf("published %v, want dataset lame on kf-dns-01", published[0])
	}
}

func TestIntoAcceptsABareHandleNeedingNoWriter(t *testing.T) {
	_, published := callEnv(t, refOnMachine(), func(ctx workflow.Context) error {
		_, _, err := Actor("nscheck", "0.1.0").Call(ctx, "ask", []any{1}, Into(Dataset("lame")))
		return err
	})
	if len(published) != 1 || published[0]["dataset"] != "lame" {
		t.Errorf("a bare handle destination did not publish: %v", published)
	}
}

func TestOmittingIntoPublishesNothing(t *testing.T) {
	_, published := callEnv(t, refOnMachine(), func(ctx workflow.Context) error {
		_, _, err := Actor("a", "1").Call(ctx, "m", []any{1, 2})
		return err
	})
	if len(published) != 0 {
		t.Errorf("published %d times with no Into, want 0", len(published))
	}
}

func TestIterPartitionsTheInputAtTheBoundedWidth(t *testing.T) {
	// The iterating form (ADR 0023 §8) yields one (results, error) per bounded chunk. A 450-unit
	// input is dispatched as 200 + 200 + 50 — the chunks partition the input (their sizes sum to
	// it) and none exceeds SafePageMax, which is the bound that keeps the workflow-event count to
	// ceil(n/200) rather than one per record.
	units := make([]any, 450)
	for i := range units {
		units[i] = i
	}
	dispatched, _ := callEnv(t, BareRef{Meta: map[string]string{"n": "1"}}, func(ctx workflow.Context) error {
		for _, err := range Actor("a", "1").Iter(ctx, "m", units) {
			if err != nil {
				return err
			}
		}
		return nil
	})

	var sizes []int
	total := 0
	for _, e := range dispatched {
		sizes = append(sizes, len(e.Units))
		total += len(e.Units)
		if len(e.Units) > SafePageMax {
			t.Errorf("a chunk of %d exceeds the bound of %d", len(e.Units), SafePageMax)
		}
	}
	if total != 450 {
		t.Errorf("the chunks summed to %d, want the whole 450", total)
	}
	if len(sizes) != 3 {
		t.Errorf("dispatched %d chunks (%v), want 3 at width %d", len(sizes), sizes, SafePageMax)
	}
}

func TestBreakingOutOfIterStopsDispatchingTheRest(t *testing.T) {
	// Break is defined (ADR 0023 §8): each chunk is a complete call that finished before it was
	// yielded, so leaving the range early never dispatches the chunks after it.
	units := make([]any, 450)
	for i := range units {
		units[i] = i
	}
	dispatched, _ := callEnv(t, BareRef{Meta: map[string]string{"n": "1"}}, func(ctx workflow.Context) error {
		for _, err := range Actor("a", "1").Iter(ctx, "m", units) {
			if err != nil {
				return err
			}
			break
		}
		return nil
	})
	if len(dispatched) != 1 {
		t.Errorf("dispatched %d chunks after a break, want only the first", len(dispatched))
	}
}

func TestIterPublishesEachChunkIntoTheNamedDataset(t *testing.T) {
	// Slices 07 and 08 compose: iterating with Into streams each chunk to the Dataset as it lands.
	units := make([]any, 450)
	for i := range units {
		units[i] = i
	}
	out := Dataset("lame").Writer()
	_, published := callEnv(t, refOnMachine(), func(ctx workflow.Context) error {
		for _, err := range Actor("nscheck", "0.1.0").Iter(ctx, "ask", units, Into(out)) {
			if err != nil {
				return err
			}
		}
		return nil
	})
	if len(published) != 3 {
		t.Errorf("published %d times, want one per bounded chunk (3)", len(published))
	}
}
