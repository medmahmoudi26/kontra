package engine

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/medmahmoudi26/kontra/runtime/go/checkpoint"
)

// THE CHECKPOINT THE HOST SHIPS WITH EACH BEAT — WHICH Units finished, not merely how many.
//
// `SetHeartbeat` carries three counters, and no number among them says which Units committed, so
// nothing can resume from a beat alone. The checkpoint says which, in the encoding the Python peer
// and the orchestrator share (shared/conformance/checkpoint.json), and it rides in the activity's
// own history rather than in a cache that can evict it (ADR 0059).
//
// These assert the publication seam. The ENCODING is asserted against the corpus in
// runtime/go/checkpoint; what is pinned here is that the engine fills it from the commits it
// actually made.

// A runner with the two slot maps the checkpoint is built from, and nothing else — the maps are
// what `checkpoint()` reads, so this is the whole input.
func runnerWith(bid string, committed []int, isolated []int) *batchRun {
	r := &batchRun{
		bid:       bid,
		slots:     map[int][]any{},
		failSlots: map[int]map[string]any{},
	}
	for _, i := range committed {
		r.slots[i] = []any{"out"}
	}
	for _, i := range isolated {
		r.failSlots[i] = map[string]any{"error": "nope"}
	}
	return r
}

func TestTheRunnerReportsWhichUnitsCommitted(t *testing.T) {
	// Non-contiguous on purpose: a range set that only ever saw a prefix would pass a contiguous
	// case while merging wrongly, and a prefix is the easy case a retry rarely hits.
	d := runnerWith("b1", []int{0, 1, 2, 5}, []int{3}).checkpoint()

	if d.V != checkpoint.Version {
		t.Errorf("version: got %d want %d", d.V, checkpoint.Version)
	}
	if d.BatchID != "b1" {
		t.Errorf("batch id: got %q", d.BatchID)
	}
	if want := []checkpoint.Range{{Lo: 0, Hi: 2}, {Lo: 5, Hi: 5}}; !reflect.DeepEqual(d.Done, want) {
		t.Errorf("done:\n  got  %v\n  want %v", d.Done, want)
	}
	if want := []int{3}; !reflect.DeepEqual(d.Failed, want) {
		t.Errorf("failed: got %v want %v", d.Failed, want)
	}
}

// A GO MAP ITERATES IN RANDOM ORDER, so a checkpoint built from one must be sorted on the way out —
// otherwise two encodings of the same progress differ, and the comparison with the Python peer
// becomes a coin flip rather than a check.
func TestTheEncodingDoesNotDependOnMapOrder(t *testing.T) {
	r := runnerWith("b1", []int{9, 4, 0, 1, 2, 3, 7}, []int{8, 5, 6})
	first, err := json.Marshal(r.checkpoint())
	if err != nil {
		t.Fatal(err)
	}
	// Twenty passes over the same maps. Go deliberately randomises map iteration per range
	// statement, so an unsorted implementation fails this within a few attempts rather than never.
	for i := 0; i < 20; i++ {
		again, err := json.Marshal(r.checkpoint())
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("map order leaked into the encoding on pass %d:\n  %s\n  %s", i, first, again)
		}
	}
}

func TestAnUnstartedActorPublishesNothingAReaderWouldBelieve(t *testing.T) {
	// The zero Details has V == 0, which every reader of the contract discards. That is the right
	// answer for an actor that has not begun a batch: a checkpoint nobody wrote is not evidence.
	a := &KontraActor{}
	d := a.Checkpoint()
	if d.V != 0 {
		t.Errorf("an unstarted actor claimed version %d", d.V)
	}
	if checkpoint.FromDetails(&d) != nil {
		t.Error("a reader believed a checkpoint nobody wrote")
	}
	if got := checkpoint.ResumeFrom(&d, "b1", 3); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Errorf("resuming from nothing must start over: got %v", got)
	}
}

func TestPublishThenReadBackIsWhatTheHostDoes(t *testing.T) {
	// The host reads `a.Checkpoint()` inside the heartbeat callback, which the runner invokes
	// immediately after publishing. This is that sequence.
	a := &KontraActor{}
	a.publishCheckpoint(runnerWith("b7", []int{0, 1}, []int{2}).checkpoint())

	d := a.Checkpoint()
	ck := checkpoint.FromDetails(&d)
	if ck == nil {
		t.Fatal("the host would have discarded what the engine just published")
	}
	if ck.BatchID != "b7" || !ck.Done.Has(0) || !ck.Done.Has(1) || !ck.Failed[2] {
		t.Errorf("round trip lost something: %+v", ck)
	}
	// And the whole point: a reader can now say what is LEFT, which no counter could.
	if got := checkpoint.ResumeFrom(&d, "b7", 4); !reflect.DeepEqual(got, []int{3}) {
		t.Errorf("resume: got %v want [3]", got)
	}
}

// Publishing is on the commit path, so it must not be the thing that fails a Unit that already
// committed — the same posture `heartbeat` and `reportProgress` have.
func TestPublishingIsSafeOnAZeroValueActor(t *testing.T) {
	(&KontraActor{}).publishCheckpoint(checkpoint.Details{V: 1, BatchID: "b"}) // must not panic
}
