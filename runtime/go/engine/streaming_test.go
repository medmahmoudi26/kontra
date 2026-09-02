package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra-local/runtime/go/unitstore"
	"github.com/medmahmoudi26/kontra-local/sdk/go/core"
)

// fakeSM is an in-memory engine.StateStore that JSON-roundtrips values (mimicking the real
// store's serialization), so the Unit scratch blob + commit records behave as in production.
type fakeSM struct{ d map[string][]byte }

func newFakeSM() *fakeSM { return &fakeSM{d: map[string][]byte{}} }

func (f *fakeSM) set(k string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f.d[k] = b
	return nil
}
func (f *fakeSM) Add(_ context.Context, k string, v any) error { return f.set(k, v) }
func (f *fakeSM) Set(_ context.Context, k string, v any) error { return f.set(k, v) }
func (f *fakeSM) SetWithTTL(_ context.Context, k string, v any, _ time.Duration) error {
	return f.set(k, v)
}
func (f *fakeSM) Get(_ context.Context, k string, reply any) error {
	b, ok := f.d[k]
	if !ok {
		return errors.New("not found")
	}
	return json.Unmarshal(b, reply)
}
func (f *fakeSM) Contains(_ context.Context, k string) (bool, error) { _, ok := f.d[k]; return ok, nil }
func (f *fakeSM) Remove(_ context.Context, k string) error           { delete(f.d, k); return nil }
func (f *fakeSM) Save(_ context.Context) error                       { return nil }
func (f *fakeSM) Touch(_ context.Context) error                      { return nil }
func (f *fakeSM) Drop(_ context.Context) error                       { clear(f.d); return nil }

// fakePutter records emitted-record blobs in memory (a stand-in for S3).
type fakePutter struct{ blobs map[string][]byte }

func (p *fakePutter) Put(_ context.Context, key string, data []byte) error {
	if p.blobs == nil {
		p.blobs = map[string][]byte{}
	}
	p.blobs[key] = data
	return nil
}

// newTestActor wires a KontraActor with a fake state store + id over a single-Method registry,
// and sets the package `reg` + `unitStore` for this test (these tests run sequentially, so
// mutating the globals is safe).
//
// The construction is the whole of it: there is no runtime to inject an id and a state manager,
// so New takes both. That is the same shape as Python's build_session_factory(...)(id,
// kv=FakeKV()), which is not a coincidence — the two hosts are peers.
func newTestActor(t *testing.T, m core.MethodFunc) (*KontraActor, *fakePutter) {
	t.Helper()
	reg = methodRegistry("scan", m)
	fp := &fakePutter{}
	unitStore = unitstore.New(fp, "")
	return New("run1-node1", newFakeSM()), fp
}

func subunitKeys(fp *fakePutter) []string {
	var out []string
	for k := range fp.blobs {
		if strings.HasPrefix(k, unitPrefixForTest("r", "n", 0)) {
			out = append(out, k)
		}
	}
	return out
}

// A Method emits each record as its own durable blob; the Unit's committed output is the list of
// refs.
func TestRunBatchStreamsEmittedRecords(t *testing.T) {
	a, fp := newTestActor(t, func(s *core.Session, b *core.Batch, ds *core.Dataset) error {
		for unit := range b.All() {
			ds.Push(map[string]any{"finding": unit.Str("t") + "-a"})
			ds.Push(map[string]any{"finding": unit.Str("t") + "-b"})
		}
		return nil
	})
	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Units: []any{map[string]any{"t": "x"}}, RunID: "r", NodeID: "n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := subunitKeys(fp); len(got) != 2 {
		t.Fatalf("expected 2 blobs under %s, got %d (%v)", unitPrefixForTest("r", "n", 0), len(got), fp.blobs)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %v, want 2 emitted refs", resp.Results)
	}
	for _, r := range resp.Results {
		if _, ok := r.(map[string]any)["$ref"]; !ok {
			t.Fatalf("emitted result missing $ref: %v", r)
		}
	}
}

// With no object store configured (no KONTRA_S3_ENDPOINT), Emit collects records INLINE — the
// Unit's output is the records themselves, not $refs (the no-S3 dev/test mode).
func TestRunBatchEmitsInlineWhenNoStore(t *testing.T) {
	a, _ := newTestActor(t, func(s *core.Session, b *core.Batch, ds *core.Dataset) error {
		for range b.All() {
			ds.Push(map[string]any{"n": 1})
			ds.Push(map[string]any{"n": 2})
		}
		return nil
	})
	unitStore = nil // no object store -> inline
	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Units: []any{map[string]any{}}, RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %v, want 2 inline records", resp.Results)
	}
	for _, r := range resp.Results {
		m, _ := r.(map[string]any)
		if _, isRef := m["$ref"]; isRef || m["n"] == nil {
			t.Fatalf("expected an inline record with field n, got %v", r)
		}
	}
}

// The resume payoff: a death mid-scan resumes at the next phase (a unit.State() cursor) and does
// NOT re-emit the earlier phase — yet that phase's record is still durable (it was emitted), so
// the Unit's blob prefix ends up holding every finding.
func TestRunBatchResumeSkipsButKeepsEmitted(t *testing.T) {
	died := false
	phases := []string{"A", "B"}
	a, fp := newTestActor(t, func(s *core.Session, b *core.Batch, ds *core.Dataset) error {
		for unit := range b.All() {
			resumable := s.EmitDurable() // true here — a store is configured, so the cursor is safe
			var cur struct {
				Phase int `json:"phase"`
			}
			if resumable {
				_, _ = unit.State().Get("phase", &cur)
			}
			for i := cur.Phase; i < len(phases); i++ {
				if i == 1 && !died { // die entering phase 1, AFTER phase 0 emitted + checkpointed
					died = true
					return &core.SessionLostError{Msg: "die after phase 0"}
				}
				ds.Push(map[string]any{"finding": phases[i]})
				if resumable {
					cur.Phase = i + 1
					_ = unit.State().Set("phase", cur)
				}
			}
		}
		return nil
	})

	// turn 1: emits A, checkpoints phase=1, then dies -> SessionLost, instance nulled, uncommitted
	if _, err := a.RunBatch(context.Background(), RunBatchReq{
		Units: []any{map[string]any{}}, RunID: "r", NodeID: "n"}); err == nil {
		t.Fatal("expected a SessionLost error on turn 1")
	}
	if got := subunitKeys(fp); len(got) != 1 {
		t.Fatalf("turn 1 should have emitted A only, got %d blobs", len(got))
	}

	// turn 2: re-open, resume at phase 1 (A NOT re-scanned/re-emitted), emit B, commit
	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Units: []any{map[string]any{}}, RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	// the prefix now holds BOTH A (from turn 1, never lost) and B (turn 2) — no loss on resume
	if got := subunitKeys(fp); len(got) != 2 {
		t.Fatalf("expected 2 blobs after resume (A persisted + B), got %d", len(got))
	}
	// turn 2's committed refs are its own emissions (B) — the streaming cursor reads the full
	// prefix for completeness; the envelope is provisional (at-least-once), as documented.
	if len(resp.Results) != 1 {
		t.Fatalf("turn 2 committed refs = %v, want 1 (B)", resp.Results)
	}
}

// The inline-mode safety gate: without a store, EmitDurable() is false, so a resumable Method MUST
// disable its phase cursor and re-scan atomically — otherwise a durable cursor would advance past
// records that inline Emit only held in memory (lost on a crash). Here the retry re-scans BOTH
// phases and re-emits everything, so nothing is lost.
func TestRunBatchResumeInlineReScansNoLoss(t *testing.T) {
	died := false
	phases := []string{"A", "B"}
	a, _ := newTestActor(t, func(s *core.Session, b *core.Batch, ds *core.Dataset) error {
		for unit := range b.All() {
			resumable := s.EmitDurable() // false — no store, so the cursor is UNSAFE and disabled
			var cur struct {
				Phase int `json:"phase"`
			}
			if resumable {
				_, _ = unit.State().Get("phase", &cur)
			}
			for i := cur.Phase; i < len(phases); i++ {
				if i == 1 && !died {
					died = true
					return &core.SessionLostError{Msg: "die after phase 0"}
				}
				ds.Push(map[string]any{"finding": phases[i]})
				if resumable {
					cur.Phase = i + 1
					_ = unit.State().Set("phase", cur)
				}
			}
		}
		return nil
	})
	unitStore = nil // inline mode -> EmitDurable() false -> cursor disabled

	// turn 1: emit A inline (lost on the death), die at phase 1; NO cursor was checkpointed
	if _, err := a.RunBatch(context.Background(), RunBatchReq{
		Units: []any{map[string]any{}}, RunID: "r", NodeID: "n"}); err == nil {
		t.Fatal("expected a SessionLost error on turn 1")
	}
	// turn 2: cursor disabled -> re-scan from phase 0 -> re-emit A AND B -> no loss
	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Units: []any{map[string]any{}}, RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("inline resume must re-scan and re-emit BOTH A and B, got %v", resp.Results)
	}
}

// unitPrefixForTest derives a Unit's blob directory from the shared key builder, so the
// partitioned layout is asserted in exactly one place (runtime/go/unitstore) and these tests
// cannot drift from it.
func unitPrefixForTest(run, node string, unit int) string {
	k := unitstore.BlobKey("", "", run, node, unit, "x")
	return k[:strings.LastIndex(k, "/")+1]
}
