package engine

// Resume from the heartbeat's checkpoint (PRD D1, ADR 0060) — the Go peers of the resume tests in
// tests/test_actor_engine.py. What is folded back, what is refused and why loudly, and the one
// property a second death depends on: no beat of a resumed attempt may forget what the previous one
// finished.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra/runtime/go/checkpoint"
	"github.com/medmahmoudi26/kontra/runtime/go/unitstore"
	"github.com/medmahmoudi26/kontra/sdk/go/core"
)

func strUnits(vs ...string) []any {
	out := make([]any, len(vs))
	for i, v := range vs {
		out[i] = map[string]any{"v": v}
	}
	return out
}

// firstAttemptDiesAfter runs attempt 1 of a batch that commits k Units and then loses its resource,
// and returns the checkpoint its last beat carried — what Temporal hands attempt 2.
func firstAttemptDiesAfter(t *testing.T, k int, req RunBatchReq) (checkpoint.Details, *fakePutter) {
	t.Helper()
	ran := 0
	a, fp := newTestActor(t, func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		for unit := range b.All() {
			if ran == k {
				return &core.SessionLostError{Msg: "worker killed"}
			}
			ran++
			ds.Push(map[string]any{"u": unit.Str("v")})
		}
		return b.Err()
	})
	if _, err := a.RunBatch(context.Background(), req); err == nil {
		t.Fatal("attempt 1 must fail")
	}
	return a.Checkpoint(), fp
}

// counting is a Method that records which Units it was handed.
func counting(ran *[]string) core.MethodFunc {
	return func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		for unit := range b.All() {
			*ran = append(*ran, unit.Str("v"))
			ds.Push(map[string]any{"u": unit.Str("v")})
		}
		return b.Err()
	}
}

// retryOn is attempt N: a fresh instance over a fresh state hash (the host drops the session on
// error, and resume must not need what it held), the shared object store, and the prior checkpoint.
func retryOn(m core.MethodFunc, prior checkpoint.Details) *KontraActor {
	reg = methodRegistry("scan", m)
	a := New("run1-node1", newFakeSM())
	a.ResumeFrom(prior)
	return a
}

// THE acceptance property, at the unit level: attempt 1 commits k of n and dies; attempt 2 runs
// EXACTLY the n-k Units that had not committed, and returns n Units of output, none twice — in the
// envelope and in the store — with nothing read from or written to the state hash.
func TestAKilledBatchResumesWithOnlyTheRemainingUnits(t *testing.T) {
	const n, k = 7, 4
	vs := make([]string, n)
	for i := range vs {
		vs[i] = fmt.Sprintf("u%d", i)
	}
	req := RunBatchReq{Units: strUnits(vs...), RunID: "r", NodeID: "n"}
	prior, fp := firstAttemptDiesAfter(t, k, req)
	if ck := checkpoint.FromDetails(&prior); ck == nil || ck.Done.Len() != k || !ck.Done.Has(k-1) {
		t.Fatalf("attempt 1's checkpoint = %+v, want units 0..%d done", prior, k-1)
	}

	var ran []string
	a := retryOn(counting(&ran), prior)
	sm := a.state.(*fakeSM)
	resp, err := a.RunBatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(ran), fmt.Sprint(vs[k:]); got != want {
		t.Errorf("attempt 2 ran %s, want exactly the n-k %s", got, want)
	}
	var rows []string
	for _, r := range resolvedResults(resp, fp) {
		rows = append(rows, r["u"].(string))
	}
	if fmt.Sprint(rows) != fmt.Sprint(vs) {
		t.Errorf("envelope rows = %v, want all %d in input order %v", rows, n, vs)
	}
	if got := len(storeRecords(fp)); got != n {
		t.Errorf("store holds %d rows, want %d — none twice", got, n)
	}
	if len(sm.d) != 0 {
		t.Errorf("the resume touched the state hash: %v", sm.d)
	}
	if got := len(commitBlobs(fp)); got != n {
		t.Errorf("store holds %d commit objects, want one per Unit (%d)", got, n)
	}
}

// Unit indices are positions within ONE batch, so a checkpoint from another batch — handed in as
// though it were this one's previous attempt — must not skip a single Unit.
func TestACheckpointForAnotherBatchIsDiscarded(t *testing.T) {
	other, _ := firstAttemptDiesAfter(t, 2, RunBatchReq{Units: strUnits("x", "y", "z"), RunID: "r", NodeID: "n"})
	var ran []string
	a := retryOn(counting(&ran), other)
	if _, err := a.RunBatch(context.Background(), RunBatchReq{Units: strUnits("a", "b", "c"), RunID: "r", NodeID: "n"}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ran) != "[a b c]" {
		t.Errorf("ran %v, want every Unit — a stale checkpoint was applied by index", ran)
	}
}

// The checkpoint is beaten only after the commit object lands, so a finished Unit with nothing at
// its key is the store having LOST it. Folding it as empty would drop its rows; re-running it would
// hide a store that is losing data. Neither: CommitLostError, before the resource loads.
func TestAMissingCommitObjectIsLoudAndLoadsNothing(t *testing.T) {
	req := RunBatchReq{Units: strUnits("a", "b", "c", "d"), RunID: "r", NodeID: "n"}
	prior, fp := firstAttemptDiesAfter(t, 2, req)
	delete(fp.blobs, unitstore.CommitKey(prior.ManifestRef, 1))

	var ran []string
	a := retryOn(counting(&ran), prior)
	loads := 0
	reg.LoadFn = func(*core.Session) error { loads++; return nil }
	_, err := a.RunBatch(context.Background(), req)
	var lost *CommitLostError
	if !errors.As(err, &lost) {
		t.Fatalf("err = %v, want a CommitLostError", err)
	}
	if !strings.Contains(err.Error(), "unit 1") || !strings.Contains(err.Error(), unitstore.CommitKey(prior.ManifestRef, 1)) {
		t.Errorf("err = %v, want it to name the unit and the key", err)
	}
	var nr core.NonRetryable
	if !errors.As(err, &nr) {
		t.Error("CommitLostError must be NonRetryable: the next attempt reads the same checkpoint and store")
	}
	if len(ran) != 0 || loads != 0 {
		t.Errorf("ran %v with %d loads — nothing may run against a batch whose commits are lost", ran, loads)
	}
}

// A body at unit 1's key that says it is unit 0 — a copy, a skewed prefix, a hand edit. Folding it
// would put unit 0's output in unit 1's place.
func TestACommitObjectNamingAnotherUnitIsLoud(t *testing.T) {
	req := RunBatchReq{Units: strUnits("a", "b", "c"), RunID: "r", NodeID: "n"}
	prior, fp := firstAttemptDiesAfter(t, 2, req)
	fp.blobs[unitstore.CommitKey(prior.ManifestRef, 1)] = fp.blobs[unitstore.CommitKey(prior.ManifestRef, 0)]

	_, err := retryOn(counting(new([]string)), prior).RunBatch(context.Background(), req)
	var lost *CommitLostError
	if !errors.As(err, &lost) || !strings.Contains(err.Error(), "names unit 0, expected 1") {
		t.Fatalf("err = %v, want a CommitLostError naming the mismatch", err)
	}
}

// The previous attempt committed under a prefix and THIS worker has no object store: a skewed
// fleet. Re-running would hide it, so it is refused by name.
func TestCommittedUnitsThisWorkerCannotReadAreLoud(t *testing.T) {
	req := RunBatchReq{Units: strUnits("a", "b", "c"), RunID: "r", NodeID: "n"}
	prior, _ := firstAttemptDiesAfter(t, 2, req)
	a := retryOn(counting(new([]string)), prior)
	unitStore = nil
	_, err := a.RunBatch(context.Background(), req)
	var lost *CommitLostError
	if !errors.As(err, &lost) || !strings.Contains(err.Error(), "no object store configured") {
		t.Fatalf("err = %v, want a CommitLostError naming the missing store", err)
	}
}

// The no-S3 mode, and the decision it embodies: there is nowhere durable to put a finished Unit's
// output, so the checkpoint names WHICH finished with an empty manifest_ref, and a retry handed it
// re-runs every Unit rather than returning a batch with holes where those outputs were.
func TestWithoutAStoreARetryReRunsTheWholeBatch(t *testing.T) {
	var ran []string
	died := false
	a, _ := actorWith(t, methodRegistry("scan", core.MethodFunc(
		func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
			for unit := range b.All() {
				if unit.Index == 2 && !died {
					died = true
					return &core.SessionLostError{Msg: "worker killed"}
				}
				ran = append(ran, unit.Str("v"))
				ds.Push(map[string]any{"u": unit.Str("v")})
			}
			return b.Err()
		})))
	req := RunBatchReq{Units: strUnits("a", "b", "c"), RunID: "r", NodeID: "n"}
	if _, err := a.RunBatch(context.Background(), req); err == nil {
		t.Fatal("attempt 1 must fail")
	}
	prior := a.Checkpoint()
	if prior.ManifestRef != "" || len(prior.Done) != 1 {
		t.Fatalf("no-store checkpoint = %+v, want done units and no manifest_ref", prior)
	}
	retry := New("run1-node1", newFakeSM())
	retry.ResumeFrom(prior)
	resp, err := retry.RunBatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ran) != "[a b a b c]" {
		t.Errorf("ran %v, want attempt 2 to re-run every Unit", ran)
	}
	if len(resp.Results) != 3 {
		t.Errorf("results = %v, want each Unit's output exactly once", resp.Results)
	}
}

// Temporal keeps only the LAST heartbeat, and the host's keepalive beats from the moment RunBatch is
// entered — through the fold and through Load. So everything an attempt publishes BEFORE its first
// commit must already name what the previous attempt finished; otherwise a tick during a long fold
// or a slow Load, followed by a second death, hands attempt 3 a checkpoint missing all of attempt
// 1's work. Looked at in all three windows: before RunBatch, mid-fold, during Load.
func TestAResumedAttemptNeverPublishesLessThanThePreviousOneFinished(t *testing.T) {
	req := RunBatchReq{Units: strUnits("a", "b", "c", "d", "e"), RunID: "r", NodeID: "n"}
	prior, fp := firstAttemptDiesAfter(t, 2, req)

	a := retryOn(counting(new([]string)), prior)
	if got := a.Checkpoint(); fmt.Sprint(got) != fmt.Sprint(prior) {
		t.Errorf("before RunBatch, published %+v, want the prior re-sent as is %+v", got, prior)
	}
	namesBoth := func(d checkpoint.Details) bool {
		ck := checkpoint.Accepted(&d, prior.BatchID)
		return ck != nil && ck.Done.Has(0) && ck.Done.Has(1)
	}
	var midFold []checkpoint.Details
	fp.onGet = func(string) { midFold = append(midFold, a.Checkpoint()) }
	var duringLoad checkpoint.Details
	reg.LoadFn = func(*core.Session) error { duringLoad = a.Checkpoint(); return nil }
	if _, err := a.RunBatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(midFold) == 0 {
		t.Fatal("the fold read nothing — this test is not looking at the window it names")
	}
	for _, d := range midFold {
		if !namesBoth(d) {
			t.Errorf("published mid-fold = %+v, want units 0 and 1 still named", d)
		}
	}
	if !namesBoth(duringLoad) {
		t.Errorf("published during Load = %+v, want this batch's checkpoint already naming units 0 and 1", duringLoad)
	}
}

// A keyed instance that ran ANOTHER batch last must not keep publishing that batch's checkpoint into
// this one's beats: a reader would discard it, and with it the record of what this batch finished.
func TestAReusedInstancePublishesThisBatchBeforeLoad(t *testing.T) {
	a, _ := newTestActor(t, counting(new([]string)))
	if _, err := a.RunBatch(context.Background(), RunBatchReq{Units: strUnits("x"), RunID: "r", NodeID: "n"}); err != nil {
		t.Fatal(err)
	}
	first := a.Checkpoint().BatchID
	a.inst = nil // so the second batch loads again, and Load can look
	var duringLoad string
	reg.LoadFn = func(*core.Session) error { duringLoad = a.Checkpoint().BatchID; return nil }
	if _, err := a.RunBatch(context.Background(), RunBatchReq{Units: strUnits("y"), RunID: "r", NodeID: "n"}); err != nil {
		t.Fatal(err)
	}
	if duringLoad == "" || duringLoad == first {
		t.Errorf("during the second batch's Load the published batch id was %q (the first was %q)", duringLoad, first)
	}
}

// Two attempts that disagree on the prefix (KONTRA_S3_PREFIX skew, a layout change between deploys)
// must not split one batch's commits across two places: the next attempt's checkpoint would then
// point at only half of them. The first prefix written is the batch's.
func TestAPreviousAttemptsPrefixIsKeptForReadingAndWriting(t *testing.T) {
	req := RunBatchReq{Units: strUnits("a", "b", "c"), RunID: "r", NodeID: "n"}
	prior, fp := firstAttemptDiesAfter(t, 1, req)
	moved := "elsewhere/" + prior.ManifestRef
	for k, v := range fp.blobs {
		if strings.HasPrefix(k, prior.ManifestRef) {
			delete(fp.blobs, k)
			fp.blobs[moved+strings.TrimPrefix(k, prior.ManifestRef)] = v
		}
	}
	prior.ManifestRef = moved

	a := retryOn(counting(new([]string)), prior)
	if _, err := a.RunBatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	for k := range fp.blobs {
		if strings.HasPrefix(k, "commits/") {
			t.Errorf("a commit landed under this worker's own prefix (%s), splitting the batch", k)
		}
	}
	if a.Checkpoint().ManifestRef != moved {
		t.Errorf("manifest_ref = %s, want the kept %s", a.Checkpoint().ManifestRef, moved)
	}
}

// The commit map is gone from the state hash: a batch that commits every Unit leaves nothing there.
func TestACommittedBatchLeavesNothingInTheStateHash(t *testing.T) {
	a, fp := newTestActor(t, counting(new([]string)))
	sm := a.state.(*fakeSM)
	if _, err := a.RunBatch(context.Background(), RunBatchReq{Units: strUnits("a", "b"), RunID: "r", NodeID: "n"}); err != nil {
		t.Fatal(err)
	}
	if len(sm.d) != 0 {
		t.Errorf("state hash holds %v after a clean batch, want nothing", sm.d)
	}
	if len(commitBlobs(fp)) != 2 {
		t.Errorf("commit objects = %v, want one per Unit", commitBlobs(fp))
	}
}
