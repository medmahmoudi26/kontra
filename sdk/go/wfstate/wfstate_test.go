package wfstate

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

type counter struct{ n int }

var store = New(func() *counter { return &counter{} })

// bump reports what the counter reads after this execution has bumped it `times` times through
// separate `Of` calls — the shape every caller of this package has: several independent calls in
// one execution that must meet the same value.
func bump(ctx workflow.Context, times int) (int, error) {
	for range times {
		store.Of(ctx).n++
	}
	return store.Of(ctx).n, nil
}

// THE ASSUMPTION THIS PACKAGE RESTS ON, asserted rather than trusted: `workflow.GetInfo` hands back
// one stable `*workflow.Info` per execution. If a future SDK returned a copy, every call would mint
// fresh state, this test would read 1 instead of 3, and the budgets built on it would stop bounding
// anything — silently, which is why it is a test and not a comment.
func TestOneExecutionSharesOneValue(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterWorkflow(bump)
	env.ExecuteWorkflow(bump, 3)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var got int
	require.NoError(t, env.GetWorkflowResult(&got))
	require.Equal(t, 3, got)
}

// TWO EXECUTIONS ARE TWO COUNTERS. A package-level counter — the obvious Go spelling — would read 6
// here, which is one run of a workflow silencing another that happens to share its worker.
func TestTwoExecutionsDoNotShareOne(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	for range 2 {
		env := suite.NewTestWorkflowEnvironment()
		env.RegisterWorkflow(bump)
		env.ExecuteWorkflow(bump, 3)

		require.NoError(t, env.GetWorkflowError())
		var got int
		require.NoError(t, env.GetWorkflowResult(&got))
		require.Equal(t, 3, got, "a second execution started where the first left off")
	}
}

// THE MAP DOES NOT GROW WITH EVERY WORKFLOW THE WORKER EVER RAN. The key is a weak pointer, so a
// finished execution's entry becomes collectable, and the next execution to register sweeps it. A
// strong map here would add an entry per narrating workflow for the life of the process.
//
// Driven through `of` with executions this test OWNS: a `TestWorkflowEnvironment` stays reachable
// after its workflow finishes, so going through `Of` would assert nothing about eviction.
func TestFinishedExecutionsAreNotHeld(t *testing.T) {
	s := New(func() *counter { return &counter{} })
	const runs = 40
	for range runs {
		s.of(&workflow.Info{WorkflowType: workflow.Type{Name: "gone"}})
	}
	require.Equal(t, runs, s.Len())

	// Twice, because the first cycle only queues the weak handles for clearing.
	runtime.GC()
	runtime.GC()

	// One more registration: `of` sweeps on a miss, which is the only thing that prunes.
	live := &workflow.Info{WorkflowType: workflow.Type{Name: "live"}}
	s.of(live)

	require.Equal(t, 1, s.Len(), "finished executions are still holding entries")
	require.NotNil(t, s.of(live), "the live execution was swept with the collected ones")
	runtime.KeepAlive(live)
}
