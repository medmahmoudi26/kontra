package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/medmahmoudi26/kontra/runtime/go/unitstore"
	"github.com/medmahmoudi26/kontra/sdk/go/core"
)

// failingPutter is a store whose every write fails — a disk-full SeaweedFS is a bare 500 on each
// PUT. The isolation counters do not catch it, which is exactly why a failed push must SURFACE
// rather than be swallowed (ADR 0028 §3; peer of Python's FailingUnitStore).
type failingPutter struct{}

func (failingPutter) Put(context.Context, string, []byte) error {
	return errors.New("disk full")
}

// push returns nothing, and a write failure surfaces at the next checkpoint — BEFORE the Unit it
// struck commits. That ordering is load-bearing: a committed Unit is skipped on retry, so
// committing one whose push never persisted would lose the record for good (ADR 0028
// §consequence 5). Peer of test_a_failing_store_surfaces_at_the_checkpoint_and_does_not_commit_the_unit.
func TestAFailingStoreSurfacesAndDoesNotCommitTheUnit(t *testing.T) {
	a, sm := actorWith(t, methodRegistry("m", core.MethodFunc(
		func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
			for range b.All() {
				ds.Push(map[string]any{"u": 1}) // the store is dead; push holds the error
			}
			return b.Err()
		})))
	unitStore = unitstore.New(failingPutter{}, "")

	_, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "m", Units: []any{"a"}, RunID: "r", NodeID: "n"})
	if err == nil || err.Error() != "disk full" {
		t.Fatalf("err = %v, want the store's error surfaced", err)
	}

	// The Unit did NOT commit — a retry re-runs it and re-pushes, so nothing was silently lost.
	slot := unitSlot(BatchID("m", []any{"a"}, nil), 0)
	if ok, _ := sm.Contains(context.Background(), slot); ok {
		t.Errorf("unit committed at %s despite its push failing — the record is now lost on retry", slot)
	}
}

// A broken store is systemic, not one bad Unit: it fails the WHOLE call rather than isolating the
// Unit it struck. Isolating would report a drop and let the Batch 'complete', which is how a whole
// run once reported success against a dead store. Peer of
// test_a_failing_store_fails_the_whole_call_rather_than_isolating_one_unit.
func TestAFailingStoreFailsTheWholeCallRatherThanIsolating(t *testing.T) {
	a, _ := actorWith(t, methodRegistry("m", core.MethodFunc(
		func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
			for unit := range b.All() {
				ds.Push(map[string]any{"u": unit.Value})
			}
			return b.Err()
		})))
	unitStore = unitstore.New(failingPutter{}, "")

	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "m", Units: []any{"a", "b", "c"}, RunID: "r", NodeID: "n"})
	if err == nil {
		t.Fatalf("a dead store must fail the whole call, not isolate: resp=%v", resp)
	}
	if err.Error() != "disk full" {
		t.Errorf("err = %v, want the store's error", err)
	}
}

// A push made from a concurrent task under Units() rides the tail, and is still made DURABLE at
// push time — a downstream cursor sees it before the Batch returns, which is what makes a crawler
// stream as it goes. Peer of test_concurrent_pushes_are_durable_at_push_time_not_at_return.
func TestConcurrentPushesAreDurableAtPushTime(t *testing.T) {
	a, fp := newTestActor(t, func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		var wg sync.WaitGroup
		for _, u := range b.Units() { // no current Unit -> every push rides the tail
			wg.Add(1)
			go func(u *core.Unit) {
				defer wg.Done()
				// No current Unit under Units() -> each push names its own out-of-loop key.
				ds.Push(map[string]any{"u": u.Value}, core.Key(fmt.Sprint(u.Value)))
			}(u)
		}
		wg.Wait()
		return b.Err()
	})

	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Units: []any{"a", "b"}, RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Done || len(resp.Results) != 2 {
		t.Fatalf("results = %v (done=%v), want 2 tail records", resp.Results, resp.Done)
	}
	for _, r := range resp.Results {
		if _, ok := r.(map[string]any)["$ref"]; !ok {
			t.Fatalf("a durable push should commit a $ref, got %v", r)
		}
	}
	if n := len(recordBlobs(fp)); n != 2 {
		t.Errorf("store holds %d record blobs, want 2 — each push must be durable AS it is made", n)
	}
}
