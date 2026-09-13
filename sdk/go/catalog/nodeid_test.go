package catalog

import (
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// issue F6 — a node id that costs no history event.
//
// `dispatch` minted its suffix with `workflow.SideEffect(… workflow.Now(ctx) …)`, which wrote a
// `MarkerRecorded` on EVERY dispatch to persist a value `workflow.Now` is already contractually
// required to reproduce on replay. Python's equivalent path pays nothing.
//
// WHAT IT DID NOT BUY, and this is the part worth pinning: the marker did not prevent the collision
// it looks like it is guarding. Every call inside ONE workflow task sees the same instant, marker or
// no marker — so two dispatches in one task shared a suffix before this change. The counter here is
// what actually distinguishes those, and the test says so rather than leaving it implied.
func seqs(t *testing.T, calls int, sameHandle bool) []string {
	t.Helper()
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()

	var got []string
	wf := func(ctx workflow.Context) error {
		h := Actor("probe", "0.1.0")
		for i := 0; i < calls; i++ {
			if !sameHandle {
				h = Actor("probe", "0.1.0")
			}
			got = append(got, h.nextNodeSeq(ctx))
		}
		return nil
	}
	env.RegisterWorkflow(wf)
	env.ExecuteWorkflow(wf)
	if !env.IsWorkflowCompleted() {
		t.Fatal("the workflow never completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestNodeSeqIsDistinctAcrossCallsOnOneHandle(t *testing.T) {
	// THE CASE THE OLD MARKER GOT WRONG. A loop over one handle dispatches repeatedly inside one
	// workflow task, and `workflow.Now` is frozen for that task — so every suffix was the same.
	got := seqs(t, 5, true)
	if len(got) != 5 {
		t.Fatalf("expected 5 suffixes, got %d", len(got))
	}
	seen := map[string]bool{}
	for _, s := range got {
		if seen[s] {
			t.Fatalf("two dispatches on one handle produced the same suffix: %q in %v", s, got)
		}
		seen[s] = true
	}
}

func TestNodeSeqIsDeterministicAcrossRuns(t *testing.T) {
	// Replay executes workflow code from the top. If the suffix were drawn from anything the
	// workflow does not reproduce — a clock read, a package-level counter shared with another
	// execution — two identical runs would disagree here, and the second would be a
	// non-determinism failure in production rather than a failing test.
	a := seqs(t, 4, true)
	b := seqs(t, 4, true)
	if len(a) != len(b) {
		t.Fatalf("different lengths: %v vs %v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("suffix %d differs between identical runs: %q vs %q", i, a[i], b[i])
		}
	}
}

func TestNodeSeqCarriesBothSources(t *testing.T) {
	// The suffix is `<history length>-<this handle's count>`, base 36. Asserted as a shape rather
	// than as literal values: the history length is the SDK's to decide and pinning it would make
	// this test about the test environment.
	got := seqs(t, 1, true)[0]
	if !strings.Contains(got, "-") {
		t.Fatalf("the suffix carries only one source: %q", got)
	}
	if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
		t.Fatalf("a half-empty suffix: %q", got)
	}
}

// The residual, written down rather than left for somebody to rediscover: two dispatches in ONE
// workflow task through SEPARATE handles can still collide, because each fresh handle starts its
// count at zero and the history length has not moved. That is exactly as true as it was with the
// marker — this test exists so the next reader learns it from a test rather than from a duplicate
// node id in a Dataset.
func TestSeparateHandlesInOneTaskStillCollide(t *testing.T) {
	got := seqs(t, 3, false)
	if got[0] != got[1] || got[1] != got[2] {
		t.Skipf("separate handles happened to differ (%v) — the residual is bounded, not guaranteed", got)
	}
	t.Logf("known residual: separate handles in one task share %q — pass WithNodeID to guarantee distinct ids", got[0])
}
