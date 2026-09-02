package engine

// A checkpoint is an input index AND an output offset (slice 06, ADR 0028 §1). These are the Go
// peers of tests/test_checkpoint_offset.py: two save points kept apart on purpose (the input
// cursor and the output offset), and the tests that matter really kill a host mid-Batch and count
// rows rather than assert the commit map's contents. A drift from the Python host is a mismatch
// here, not two green suites that disagree.
//
// The out-of-loop cases pin the mechanism ADR 0028 settled after three inference attempts failed:
// an out-of-loop push is identified by an EXPLICIT KEY the author supplies (core.Key), reconciled
// first-write-wins across an isolation re-invoke, never inferred from content or control-flow
// position. The four cases that broke the three prior mechanisms all hold here simultaneously.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra-local/sdk/go/core"
)

// storeRecords decodes every distinct record the fake store holds — the durable row count a reader
// sees, independent of the response envelope.
func storeRecords(fp *fakePutter) []map[string]any {
	var out []map[string]any
	for _, v := range fp.blobs {
		var recs []map[string]any
		if err := json.Unmarshal(v, &recs); err == nil {
			out = append(out, recs...)
		}
	}
	return out
}

// resolvedResults decodes the call's results into records, following the {"$ref": …} entries a
// store-backed tail returns back through the fake store — so results and storeRecords compare like
// for like (peer of Python's _resolved_results).
func resolvedResults(resp *RunBatchResp, fp *fakePutter) []map[string]any {
	var out []map[string]any
	for _, r := range resp.Results {
		m, _ := r.(map[string]any)
		if ref, ok := m["$ref"].(map[string]any); ok {
			key, _ := ref["key"].(string)
			var recs []map[string]any
			if err := json.Unmarshal(fp.blobs[key], &recs); err == nil && len(recs) == 1 {
				out = append(out, recs[0])
			}
			continue
		}
		out = append(out, m)
	}
	return out
}

func badUnits() []any {
	return []any{map[string]any{"v": "a"}, map[string]any{"v": "bad"}, map[string]any{"v": "c"}}
}

func countMap(recs []map[string]any, field string, want any) int {
	n := 0
	for _, r := range recs {
		if v, ok := r[field]; ok && fmt.Sprint(v) == fmt.Sprint(want) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------------------------
// The FOUR cases that must hold SIMULTANEOUSLY (ADR 0028 §consequence 6).
// ---------------------------------------------------------------------------------------------

// CASE 1. A constant push before the loop, keyed, appears exactly once across the isolation
// re-invoke a mid-Batch failure triggers. Peer of test_case1.
func TestCase1ConstantUnconditionalOutOfLoopPush(t *testing.T) {
	a, fp := newTestActor(t, func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		ds.Push(map[string]any{"pre": "once"}, core.Key("pre"))
		for unit := range b.All() {
			if unit.Str("v") == "bad" {
				return errors.New("boom")
			}
			ds.Push(map[string]any{"u": unit.Str("v")})
		}
		return b.Err()
	})

	resp, err := a.RunBatch(context.Background(), RunBatchReq{Units: badUnits(), RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if got := countMap(resolvedResults(resp, fp), "pre", "once"); got != 1 {
		t.Errorf("pre appears %d times in results, want 1", got)
	}
	if got := countMap(storeRecords(fp), "pre", "once"); got != 1 {
		t.Errorf("pre appears %d times in the store, want 1 — no orphan blob", got)
	}
	if len(resp.Failures) != 1 || resp.Failures[0]["unit"].(map[string]any)["v"] != "bad" {
		t.Errorf("failures = %v, want the one isolated Unit", resp.Failures)
	}
}

// CASE 2. A one-time push guarded by inst state runs on entry 1 and NOT on entry 2 (the inst's
// fields survive the in-memory re-invoke). truncate-and-re-execute LOST it; the key mechanism
// retains its slot, so it must NOT be lost. Peer of test_case2.
func TestCase2SelfGuardedOutOfLoopPush(t *testing.T) {
	hdrDone := false // survives the re-invoke, like an inst field — the exact shape of the bug
	a, fp := newTestActor(t, func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		if !hdrDone {
			ds.Push(map[string]any{"header": true}, core.Key("hdr"))
			hdrDone = true
		}
		for unit := range b.All() {
			if unit.Str("v") == "bad" {
				return errors.New("boom")
			}
			ds.Push(map[string]any{"u": unit.Str("v")})
		}
		return b.Err()
	})

	resp, err := a.RunBatch(context.Background(), RunBatchReq{Units: badUnits(), RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if got := countMap(resolvedResults(resp, fp), "header", true); got != 1 {
		t.Errorf("header appears %d times in results, want 1 — a rebuilt tail drops the guarded push", got)
	}
	if got := countMap(storeRecords(fp), "header", true); got != 1 {
		t.Errorf("header appears %d times in the store, want 1", got)
	}
}

// CASE 3. A push whose content varies across the re-invoke (n moves while author locals reset)
// re-pushes with different bytes on entry 2. accumulate-and-content-dedup DUPLICATED it; keyed
// first-write-wins drops entry 2's re-push BEFORE the write: exactly one `seen` record, its first
// value (0), in results AND the store — no orphan. Peer of test_case3.
func TestCase3ContentVaryingOutOfLoopPush(t *testing.T) {
	n := 0
	a, fp := newTestActor(t, func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		ds.Push(map[string]any{"seen": n}, core.Key("seen"))
		for unit := range b.All() {
			if unit.Str("v") == "bad" {
				return errors.New("boom")
			}
			n++
			ds.Push(map[string]any{"u": unit.Str("v")})
		}
		return b.Err()
	})

	resp, err := a.RunBatch(context.Background(), RunBatchReq{Units: badUnits(), RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	seenValues := func(recs []map[string]any) []float64 {
		var vs []float64
		for _, r := range recs {
			if v, ok := r["seen"].(float64); ok {
				vs = append(vs, v)
			}
		}
		sort.Float64s(vs)
		return vs
	}
	if got := seenValues(resolvedResults(resp, fp)); fmt.Sprint(got) != "[0]" {
		t.Errorf("results seen=%v, want [0] — the content-varying re-push must fold to one", got)
	}
	if got := seenValues(storeRecords(fp)); fmt.Sprint(got) != "[0]" {
		t.Errorf("store seen=%v, want [0] — no orphan blob from the superseded content", got)
	}
}

// CASE 4 — the prefix shift that broke the (group, ordinal) mechanism. A `warn` push appears ONLY
// on the re-invoke, ahead of an unconditional `data` push. Under a positional ordinal, warn drew
// the ordinal data had filled (first-write-wins DROPPED warn) and data slid to a new ordinal
// (DUPLICATED data) — the documented [data,data] + warn-lost failure. With an explicit key, warn
// carries a key nobody wrote (kept) and data a key already written (skipped): ONE data AND ONE
// warn, both present, each once. Peer of test_case4.
func TestCase4PrefixShiftKeepsBothPushes(t *testing.T) {
	bad := false // survives the re-invoke, like an inst field
	a, fp := newTestActor(t, func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		if bad {
			ds.Push(map[string]any{"warn": "retry"}, core.Key("warn")) // only on the re-invoke
		}
		ds.Push(map[string]any{"data": "always"}, core.Key("data")) // unconditional, follows it
		for unit := range b.All() {
			bad = true
			if unit.Str("v") == "bad" {
				return errors.New("boom")
			}
			ds.Push(map[string]any{"u": unit.Str("v")})
		}
		return b.Err()
	})

	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Units: []any{map[string]any{"v": "bad"}, map[string]any{"v": "c"}}, RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	results := resolvedResults(resp, fp)
	if got := countMap(results, "data", "always"); got != 1 {
		t.Errorf("data appears %d times in results, want 1 — the positional scheme duplicated it", got)
	}
	if got := countMap(results, "warn", "retry"); got != 1 {
		t.Errorf("warn appears %d times in results, want 1 — the positional scheme lost it", got)
	}
	store := storeRecords(fp)
	if got := countMap(store, "data", "always"); got != 1 {
		t.Errorf("data appears %d times in the store, want 1", got)
	}
	if got := countMap(store, "warn", "retry"); got != 1 {
		t.Errorf("warn appears %d times in the store, want 1", got)
	}
}

// ---------------------------------------------------------------------------------------------
// Extras: two isolations, keyed-after beside keyed-before, several keys, a goroutine under
// Units(), and the unkeyed-push-panics case.
// ---------------------------------------------------------------------------------------------

// Two Units isolate in one Batch, so the body is entered three times; a constant keyed pre push
// folds to exactly one across all three entries. Peer of
// test_a_constant_keyed_push_survives_two_isolations_in_one_batch.
func TestAConstantKeyedPushSurvivesTwoIsolations(t *testing.T) {
	a, fp := newTestActor(t, func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		ds.Push(map[string]any{"pre": "once"}, core.Key("pre"))
		for unit := range b.All() {
			if v := unit.Str("v"); v == "x" || v == "y" {
				return errors.New("boom")
			}
			ds.Push(map[string]any{"u": unit.Str("v")})
		}
		return b.Err()
	})

	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Units: []any{map[string]any{"v": "a"}, map[string]any{"v": "x"}, map[string]any{"v": "y"}, map[string]any{"v": "c"}},
		RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Failures) != 2 {
		t.Fatalf("failures = %v, want the two isolated Units", resp.Failures)
	}
	if got := countMap(resolvedResults(resp, fp), "pre", "once"); got != 1 {
		t.Errorf("pre appears %d times, want 1 across two re-invokes", got)
	}
}

// A keyed pre push and a keyed post push share the tail across an isolation re-invoke. The pre push
// runs on both entries (folds to one by its key); the post push runs ONLY on the entry where the
// loop completes. Distinct keys keep them in distinct slots, so both survive exactly once. Peer of
// test_a_keyed_push_after_the_loop_coexists_with_a_keyed_push_before_it.
func TestAKeyedPostPushCoexistsWithAKeyedPrePush(t *testing.T) {
	n := 0
	a, fp := newTestActor(t, func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		ds.Push(map[string]any{"header": n}, core.Key("hdr")) // content-varying -> folds to one
		for unit := range b.All() {
			if unit.Str("v") == "bad" {
				return errors.New("boom")
			}
			n++
			ds.Push(map[string]any{"u": unit.Str("v")})
		}
		ds.Push(map[string]any{"summary": "done"}, core.Key("sum")) // only when the loop completes
		return b.Err()
	})

	resp, err := a.RunBatch(context.Background(), RunBatchReq{Units: badUnits(), RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	results := resolvedResults(resp, fp)
	var headers []float64
	var summaries []string
	for _, r := range results {
		if h, ok := r["header"].(float64); ok {
			headers = append(headers, h)
		}
		if s, ok := r["summary"].(string); ok {
			summaries = append(summaries, s)
		}
	}
	if fmt.Sprint(headers) != "[0]" {
		t.Errorf("headers=%v, want [0]", headers)
	}
	if fmt.Sprint(summaries) != "[done]" {
		t.Errorf("summaries=%v, want [done] — the post push survived beside the retained pre record", summaries)
	}
}

// Three distinct out-of-loop keys in one entry, all content-varying. Each folds to its first value:
// three records survive, not six, not one. Peer of test_several_distinct_keys_in_one_entry.
func TestSeveralDistinctKeysEachFoldToOne(t *testing.T) {
	n := 0
	a, fp := newTestActor(t, func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		ds.Push(map[string]any{"alpha": n}, core.Key("alpha"))
		ds.Push(map[string]any{"beta": n}, core.Key("beta"))
		ds.Push(map[string]any{"gamma": n}, core.Key("gamma"))
		for unit := range b.All() {
			if unit.Str("v") == "bad" {
				return errors.New("boom")
			}
			n++
			ds.Push(map[string]any{"u": unit.Str("v")})
		}
		return b.Err()
	})

	resp, err := a.RunBatch(context.Background(), RunBatchReq{Units: badUnits(), RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	results := resolvedResults(resp, fp)
	for _, field := range []string{"alpha", "beta", "gamma"} {
		if got := countMap(results, field, float64(0)); got != 1 {
			t.Errorf("%s appears %d times, want 1 (its first value)", field, got)
		}
	}
}

// A goroutine spawned under Units() has no current Unit, so its push rides the tail and must carry
// a key. Keyed per Unit, every record lands exactly once and the whole Batch commits at Method
// exit. Peer of test_a_keyed_push_from_a_task_spawned_under_batch_units.
func TestAKeyedPushFromAGoroutineUnderUnits(t *testing.T) {
	a, _ := actorWith(t, methodRegistry("m", core.MethodFunc(
		func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
			for _, u := range b.Units() {
				ds.Push(map[string]any{"u": u.Str("v")}, core.Key("u-"+u.Str("v")))
			}
			return b.Err()
		})))

	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "m",
		Units:  []any{map[string]any{"v": "a"}, map[string]any{"v": "b"}, map[string]any{"v": "c"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Done {
		t.Error("a whole-Batch Method must report done")
	}
	var got []string
	for _, r := range resp.Results {
		got = append(got, r.(map[string]any)["u"].(string))
	}
	sort.Strings(got)
	if fmt.Sprint(got) != "[a b c]" {
		t.Errorf("results = %v, want [a b c]", got)
	}
}

// The refusal. A push with no current Unit and no key is an author error: it panics at the call
// site, which the driver's recover converts to a whole-call error (the loud peer of Python raising
// MissingPushKey). The message names the fix. Peer of
// test_an_unkeyed_out_of_loop_push_raises_at_the_call_site.
func TestAnUnkeyedOutOfLoopPushIsRefused(t *testing.T) {
	a, _ := newTestActor(t, func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		ds.Push(map[string]any{"pre": "unkeyed"}) // no key, no current Unit -> panics
		for unit := range b.All() {
			ds.Push(map[string]any{"u": unit.Str("v")})
		}
		return b.Err()
	})

	_, err := a.RunBatch(context.Background(), RunBatchReq{
		Units: []any{map[string]any{"v": "a"}, map[string]any{"v": "b"}}, RunID: "r", NodeID: "n"})
	if err == nil {
		t.Fatal("an unkeyed out-of-loop push must fail the call, not be accepted with a guessed key")
	}
	if !strings.Contains(err.Error(), "key") {
		t.Errorf("error = %v, want it to name the fix (a key)", err)
	}
}

// ---------------------------------------------------------------------------------------------
// Really kill a host mid-Batch and count rows (ADR 0028 §consequence 5)
// ---------------------------------------------------------------------------------------------

// A host dies mid-Batch with records already durable, the handler retries the same actor id, and
// the run produces EXACTLY the expected rows — no duplicate, no loss. Peer of
// test_a_host_killed_mid_batch_and_retried_produces_the_exact_row_count.
func TestAHostKilledMidBatchAndRetriedProducesTheExactRowCount(t *testing.T) {
	died := false
	a, fp := newTestActor(t, func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		for unit := range b.All() {
			if unit.Str("v") == "c" && !died {
				died = true
				return &core.SessionLostError{Msg: "host struck"} // nulls the instance, handler retries
			}
			ds.Push(map[string]any{"u": unit.Str("v")})
		}
		return b.Err()
	})

	units := []any{
		map[string]any{"v": "a"}, map[string]any{"v": "b"},
		map[string]any{"v": "c"}, map[string]any{"v": "d"},
	}
	if _, err := a.RunBatch(context.Background(), RunBatchReq{Units: units, RunID: "r", NodeID: "n"}); err == nil {
		t.Fatal("expected the host death to surface as an error on the first attempt")
	}

	resp, err := a.RunBatch(context.Background(), RunBatchReq{Units: units, RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Done || len(resp.Results) != 4 {
		t.Fatalf("results = %v (done=%v), want exactly 4 rows", resp.Results, resp.Done)
	}
	var got []string
	for _, r := range storeRecords(fp) {
		got = append(got, r["u"].(string))
	}
	sort.Strings(got)
	if fmt.Sprint(got) != "[a b c d]" {
		t.Errorf("durable rows = %v, want [a b c d] — no duplicate, no loss", got)
	}
}

// ---------------------------------------------------------------------------------------------
// Output from a failed Unit stays; the Unit is still dropped (ADR 0028 §consequence 3)
// ---------------------------------------------------------------------------------------------

// A Unit that pushes records and then fails leaves those records in the store — findings from
// failures ARE the output — and is still reported as dropped. Peer of
// test_records_pushed_by_a_unit_that_then_raises_are_kept_and_the_unit_is_dropped.
func TestRecordsPushedByAUnitThatThenFailsAreKeptAndTheUnitIsDropped(t *testing.T) {
	a, fp := newTestActor(t, func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		for unit := range b.All() {
			ds.Push(map[string]any{"finding": unit.Str("v") + "-1"})
			ds.Push(map[string]any{"finding": unit.Str("v") + "-2"})
			if unit.Str("v") == "boom" {
				return errors.New("bad after two findings")
			}
		}
		return b.Err()
	})

	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Units: []any{map[string]any{"v": "a"}, map[string]any{"v": "boom"}, map[string]any{"v": "c"}},
		RunID: "r", NodeID: "n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Failures) != 1 || resp.Failures[0]["unit"].(map[string]any)["v"] != "boom" {
		t.Fatalf("failures = %v, want the one dropped Unit", resp.Failures)
	}
	keptBoom := 0
	for _, r := range storeRecords(fp) {
		if s, _ := r["finding"].(string); strings.HasPrefix(s, "boom-") {
			keptBoom++
		}
	}
	if keptBoom != 2 {
		t.Errorf("store holds %d of boom's findings, want 2 kept", keptBoom)
	}
	if len(resp.Results) != 4 { // only the committed Units a,c: a-1,a-2,c-1,c-2
		t.Errorf("results = %v, want 4 (the dropped Unit's rows are not folded into the return)", resp.Results)
	}
}

// ---------------------------------------------------------------------------------------------
// A Method taking the whole Batch commits everything at Method exit
// ---------------------------------------------------------------------------------------------

// An author who takes Units() to run them concurrently has no iterator position to commit by, so
// nothing commits until the Method returns — and then everything does. Each push carries a key (no
// current Unit). Peer of test_a_whole_batch_method_commits_everything_at_method_exit.
func TestAWholeBatchMethodCommitsEverythingAtMethodExit(t *testing.T) {
	a, _ := actorWith(t, methodRegistry("m", core.MethodFunc(
		func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
			for _, u := range b.Units() { // no position -> the whole Batch commits at exit
				ds.Push(map[string]any{"u": u.Str("v")}, core.Key("u-"+u.Str("v")))
			}
			return b.Err()
		})))

	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "m",
		Units:  []any{map[string]any{"v": "a"}, map[string]any{"v": "b"}, map[string]any{"v": "c"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Done {
		t.Error("a whole-Batch Method must report done — everything commits at exit")
	}
	var got []string
	for _, r := range resp.Results {
		got = append(got, r.(map[string]any)["u"].(string))
	}
	sort.Strings(got)
	if fmt.Sprint(got) != "[a b c]" {
		t.Errorf("results = %v, want [a b c]", got)
	}
}
