// Package wfstate hangs a library's own state off ONE running workflow execution — the Go answer
// to the thing Python's `lib/hitl.py` and `lib/narrate.py` do with `workflow.instance()`.
//
// WHY A LIBRARY NEEDS THIS AT ALL. `narrate.Say` counts sentences and `hitl.Ask` numbers asks, and
// both have to remember across calls that share nothing but a `workflow.Context`. Python hangs the
// counter on the workflow OBJECT, which is per-execution by construction. A Go workflow is a plain
// function: there is no object, and the two obvious places are both wrong. A package-level counter
// is shared by every execution in the worker process, so one talkative run silences every other run
// on the same host — the exact failure Python's `_STATE_ATTR` comment names. And a value threaded
// through the Context has to be installed by the AUTHOR at the top of their workflow, so the one
// time it is forgotten the budget silently stops existing.
//
// SO THE KEY IS THE EXECUTION'S OWN `*workflow.Info`, and everything below follows from one fact
// about the SDK: `workflow.GetInfo(ctx)` returns the environment's single `*WorkflowInfo`
// (`internal_event_handlers.go`: `func (wc *workflowEnvironmentImpl) WorkflowInfo() *WorkflowInfo {
// return wc.workflowInfo }`), so the pointer is STABLE for the life of one execution instance and
// NEW when that instance is rebuilt. That is precisely the lifetime this state must have:
//
//   - Two coroutines of one workflow see one counter, so concurrent asks number 1, 2, 3 in call
//     order rather than each starting at 1.
//   - Two executions on one worker see two counters, so they cannot read each other's asks.
//   - A REPLAY FROM SCRATCH SEES A FRESH ONE, and this is the load-bearing one. A workflow evicted
//     from the sticky cache and replayed rebuilds its environment, so the counter starts at zero
//     and every `ask-<n>` and every narration-budget decision comes out where it did the first
//     time. A registry keyed on the run id would instead resume mid-count and produce a DIFFERENT
//     command on replay — a nondeterminism error, in a library the author never wired up, on the
//     run that had narrated the most.
//
// THE ASSUMPTION IS PINNED, NOT TRUSTED (`wfstate_test.go`). If a future SDK returned a copy per
// call, every `Of` would mint fresh state and the budget would simply stop bounding anything —
// silently, which is the failure mode this package must not have. The test asserts that two calls
// in one execution share state and that two executions do not.
//
// IT LEAKS NOTHING. The key is a `weak.Pointer`, so an entry does not keep a finished execution's
// info alive, and a collected entry is dropped the next time a new execution registers. A strong
// map here would grow by one entry per narrating workflow for the life of the worker.
package wfstate

import (
	"sync"
	"weak"

	"go.temporal.io/sdk/workflow"
)

// Store is one library's per-execution state: `hitl` keeps its asks in one, `narrate` its budget
// in another. Build ONE per package, at package scope — the map is keyed by execution, so the
// global is an index and never shared state.
type Store[T any] struct {
	fresh func() *T

	// A REAL MUTEX, not a workflow one. Workflow coroutines of a single execution are scheduled
	// cooperatively and cannot race each other, but two EXECUTIONS run on two goroutines and both
	// reach this map. Nothing inside the critical section yields, so it cannot interact with the
	// SDK's deadlock detector.
	mu sync.Mutex
	m  map[weak.Pointer[workflow.Info]]*T
}

// New returns a Store that builds its state with `fresh` the first time an execution asks for it.
func New[T any](fresh func() *T) *Store[T] {
	return &Store[T]{fresh: fresh, m: map[weak.Pointer[workflow.Info]]*T{}}
}

// Of is this execution's state, created on first use.
//
// Called from workflow code and safe to call from anywhere in it: the value it returns is the same
// value every other call in the same execution gets, and no other execution can reach it.
func (s *Store[T]) Of(ctx workflow.Context) *T {
	return s.of(workflow.GetInfo(ctx))
}

// of is Of with the execution's identity handed in, so the eviction rule can be exercised against
// an object the test owns — a `TestWorkflowEnvironment` stays reachable long after its workflow
// finished, which makes it useless for asserting that a finished execution is released.
func (s *Store[T]) of(info *workflow.Info) *T {
	key := weak.Make(info)

	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[key]; ok {
		return v
	}
	// Only on a MISS, which is once per execution: the sweep costs one pass over what is at most
	// the number of executions this worker has cached, and it is what keeps that number from being
	// "every execution this worker has ever run".
	s.sweep()
	v := s.fresh()
	s.m[key] = v
	return v
}

// Len is how many executions this Store currently holds state for — for the tests that assert the
// map does not grow without bound.
func (s *Store[T]) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// sweep drops the executions that have been collected. A weak key does not remove itself.
func (s *Store[T]) sweep() {
	for k := range s.m {
		if k.Value() == nil {
			delete(s.m, k)
		}
	}
}
