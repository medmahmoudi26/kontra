package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra-local/sdk/go/core"
)

// actorWith wires a KontraActor over a fake state store and sets the package registry for this
// test (these tests run sequentially, so mutating the globals is safe). It is the Go peer of
// Python's build_session_factory(reg, store=None)(id, kv=FakeKV()).
func actorWith(t *testing.T, reg2 *core.Registry) (*KontraActor, *fakeSM) {
	t.Helper()
	reg = reg2
	unitStore = nil // inline mode: emitted records ride in the commit, no object store needed
	sm := newFakeSM()
	return New("run1-node1", sm), sm
}

// methodRegistry builds a registry from name -> body pairs, in order.
func methodRegistry(pairs ...any) *core.Registry {
	r := &core.Registry{Name: "t"}
	for i := 0; i < len(pairs); i += 2 {
		r.AddMethod(pairs[i].(string), pairs[i+1].(core.MethodFunc))
	}
	return r
}

// The v2 author's loop end to end: a Method receives the whole Batch and its Dataset, ranges over
// the Batch, and pushes per Unit. What comes back is one result per push, in input order.
func TestAMethodRangesOverItsBatchAndPushesPerUnit(t *testing.T) {
	a, _ := actorWith(t, methodRegistry("shout", core.MethodFunc(
		func(s *core.Session, b *core.Batch, ds *core.Dataset) error {
			for unit := range b.All() {
				ds.Push(map[string]any{"loud": unit.Str("word") + "!"})
			}
			return nil
		})))

	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "shout",
		Units:  []any{map[string]any{"word": "a"}, map[string]any{"word": "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(resp.Results), "[map[loud:a!] map[loud:b!]]"; got != want {
		t.Errorf("results = %s, want %s", got, want)
	}
	if !resp.Done {
		t.Error("batch should report done")
	}
}

// emitter returns a Method that emits one tagged record per Unit.
func emitter(tag string, seen *[]string) core.MethodFunc {
	return func(s *core.Session, b *core.Batch, ds *core.Dataset) error {
		for unit := range b.All() {
			if seen != nil {
				*seen = append(*seen, tag+":"+fmt.Sprint(unit.Value))
			}
			ds.Push(map[string]any{tag: unit.Value})
		}
		return nil
	}
}

// The dispatched name selects which Method runs (ADR 0023 §5).
func TestTheDispatchedNameSelectsWhichMethodRuns(t *testing.T) {
	var calls []string
	a, _ := actorWith(t, methodRegistry(
		"crawl", emitter("crawl", &calls),
		"extract", emitter("extract", &calls)))

	if _, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "extract", Units: []any{"x"}}); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(calls), "[extract:x]"; got != want {
		t.Errorf("calls = %s, want %s", got, want)
	}
}

// An unnamed dispatch against several Methods fails rather than taking declaration order — and it
// fails at the actor, blaming the caller, not silently producing the wrong Method's output.
func TestAnUnnamedDispatchAgainstSeveralMethodsFails(t *testing.T) {
	a, _ := actorWith(t, methodRegistry("crawl", emitter("crawl", nil), "extract", emitter("extract", nil)))

	_, err := a.RunBatch(context.Background(), RunBatchReq{Units: []any{"x"}})
	if err == nil {
		t.Fatal("an unnamed dispatch against two Methods must fail")
	}
	if !strings.Contains(err.Error(), "must name one") {
		t.Errorf("error = %v, want it to tell the caller to name a Method", err)
	}
}

// The point of §9: `extract` sees the resource `load` opened AND what `crawl` left on the Session.
// Two Methods of one Actor are one process and one instance — which is why an Activity is just an
// Actor with one Method and no load/close.
func TestTwoMethodsShareOneLoadedSession(t *testing.T) {
	r := &core.Registry{Name: "t"}
	r.LoadFn = func(s *core.Session) error {
		s.Set("resource", "browser")
		s.Set("seen", []string{})
		return nil
	}
	r.AddMethod("crawl", func(s *core.Session, b *core.Batch, ds *core.Dataset) error {
		for unit := range b.All() {
			seen, _ := s.Get("seen")
			s.Set("seen", append(seen.([]string), fmt.Sprint(unit.Value)))
			res, _ := s.Get("resource")
			ds.Push(map[string]any{"crawled": unit.Value, "with": res})
		}
		return nil
	})
	r.AddMethod("extract", func(s *core.Session, b *core.Batch, ds *core.Dataset) error {
		for unit := range b.All() {
			seen, _ := s.Get("seen")
			res, _ := s.Get("resource")
			ds.Push(map[string]any{"extracted": unit.Value, "after": seen, "with": res})
		}
		return nil
	})
	a, _ := actorWith(t, r)

	first, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "crawl", Units: []any{"a"}, RunID: "r1", NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "extract", Units: []any{"b"}, RunID: "r1", NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(first.Results), "[map[crawled:a with:browser]]"; got != want {
		t.Errorf("crawl results = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(second.Results), "[map[after:[a] extracted:b with:browser]]"; got != want {
		t.Errorf("extract results = %s, want %s — extract must see load's resource and crawl's state", got, want)
	}
}

// The hazard §17 exists to close. Two Methods dispatched under the SAME run/node — which is what a
// caller's scope does once it holds a Session across calls — used to collide in the commit map at
// `u0`. The second Method never ran and the caller got the first Method's output: full, plausible
// and wrong. Keying a committed Unit by the BATCH's content hash plus its index separates them.
func TestASecondMethodUnderOneBatchOwnerDoesNotReplayTheFirst(t *testing.T) {
	a, _ := actorWith(t, methodRegistry("crawl", emitter("crawled", nil), "extract", emitter("extracted", nil)))

	if _, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "crawl", Units: []any{"a"}, RunID: "r1", NodeID: "n1"}); err != nil {
		t.Fatal(err)
	}
	second, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "extract", Units: []any{"b"}, RunID: "r1", NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(second.Results), "[map[extracted:b]]"; got != want {
		t.Errorf("results = %s, want %s", got, want)
	}
}

// The sharper half of the same hazard: identical Units under one owner. Nothing in the payload
// differs except which Method was named, so a hash over the Units alone would still collide and
// `extract` would replay `crawl`'s output.
func TestTwoMethodsHandedTheSameUnitsAreTwoBatches(t *testing.T) {
	a, _ := actorWith(t, methodRegistry("crawl", emitter("crawled", nil), "extract", emitter("extracted", nil)))

	first, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "crawl", Units: []any{"a"}, RunID: "r1", NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "extract", Units: []any{"a"}, RunID: "r1", NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(first.Results), "[map[crawled:a]]"; got != want {
		t.Errorf("crawl results = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(second.Results), "[map[extracted:a]]"; got != want {
		t.Errorf("extract results = %s, want %s — the Method name is part of the Batch hash", got, want)
	}
}

// A crash mid-Batch resumes at the first uncommitted Unit and never re-runs a committed one. This
// is the whole point of the commit map, so it is tested directly rather than inferred.
func TestACrashMidBatchResumesAtTheFirstUncommittedUnit(t *testing.T) {
	var ran []any
	died := false
	a, _ := actorWith(t, methodRegistry("scan", core.MethodFunc(
		func(s *core.Session, b *core.Batch, ds *core.Dataset) error {
			for unit := range b.All() {
				if unit.Index == 1 && !died { // die AFTER unit 0 committed
					died = true
					return &core.SessionLostError{Msg: "host gone"}
				}
				ran = append(ran, unit.Value)
				ds.Push(map[string]any{"got": unit.Value})
			}
			return nil
		})))

	req := RunBatchReq{Method: "scan", Units: []any{"a", "b", "c"}, RunID: "r", NodeID: "n"}
	if _, err := a.RunBatch(context.Background(), req); err == nil {
		t.Fatal("turn 1 must surface the dead resource")
	}
	resp, err := a.RunBatch(context.Background(), req) // the handler's retry, same actor id
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(ran), "[a b c]"; got != want {
		t.Errorf("bodies ran for %s, want %s — a committed Unit must never run twice", got, want)
	}
	if got, want := fmt.Sprint(resp.Results), "[map[got:a] map[got:b] map[got:c]]"; got != want {
		t.Errorf("results = %s, want %s — the replayed commit rides in the envelope", got, want)
	}
}

// A failure inside a Method body is attributed to the Unit the iterator was on (ADR 0023 §13),
// recorded as a failure, and the Method RE-INVOKED with only the remaining Units. Records emitted
// before the failure are already committed and are NOT re-emitted.
func TestAFailureIsBlamedOnItsUnitAndTheMethodResumesWithTheRemainder(t *testing.T) {
	var handed []any
	a, _ := actorWith(t, methodRegistry("scan", core.MethodFunc(
		func(s *core.Session, b *core.Batch, ds *core.Dataset) error {
			for unit := range b.All() { // the NAIVE loop: no error handling at all
				handed = append(handed, unit.Value)
				if unit.Value == "bad" {
					return errors.New("boom")
				}
				ds.Push(map[string]any{"ok": unit.Value})
			}
			return nil
		})))

	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "scan", Units: []any{"a", "bad", "c"}, RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(handed), "[a bad c]"; got != want {
		t.Errorf("Units handed out = %s, want %s — re-invoke gets the REMAINDER, not the whole Batch", got, want)
	}
	if got, want := fmt.Sprint(resp.Results), "[map[ok:a] map[ok:c]]"; got != want {
		t.Errorf("results = %s, want %s — nothing emitted before the failure is re-emitted", got, want)
	}
	if len(resp.Failures) != 1 {
		t.Fatalf("failures = %v, want the one blamed Unit", resp.Failures)
	}
	if got := resp.Failures[0]["unit"]; got != "bad" {
		t.Errorf("blamed Unit = %v, want bad", got)
	}
	if got := resp.Failures[0]["category"]; got != "exhausted" {
		t.Errorf("category = %v, want exhausted (a live resource isolates the Unit)", got)
	}
	if !resp.Done {
		t.Error("every Unit is accounted for (committed or isolated), so the batch is done")
	}
}

// The sharpest thing an author must know and would not guess (ADR 0023 §13): a Method is entered
// several times per Batch, once per isolated failure. Locals reset between entries; the Session
// does not. Anything accumulated across Units belongs on the Session, not in a local.
func TestAuthorLocalsResetAcrossReEntryWhileTheSessionPersists(t *testing.T) {
	a, _ := actorWith(t, methodRegistry("scan", core.MethodFunc(
		func(s *core.Session, b *core.Batch, ds *core.Dataset) error {
			local := 0 // reset on every entry
			for unit := range b.All() {
				local++
				n, _ := s.Get("count") // survives every entry
				if n == nil {
					n = 0
				}
				s.Set("count", n.(int)+1)
				if unit.Value == "bad" {
					return errors.New("boom")
				}
				ds.Push(map[string]any{"local": local, "session": n.(int) + 1})
			}
			return nil
		})))

	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "scan", Units: []any{"a", "bad", "c"}, RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	// Entry 1 saw a(local 1) and bad(local 2); entry 2 starts a fresh local and sees c(local 1).
	// The Session counter kept counting across both entries: a=1, bad=2, c=3.
	if got, want := fmt.Sprint(resp.Results), "[map[local:1 session:1] map[local:1 session:3]]"; got != want {
		t.Errorf("results = %s, want %s", got, want)
	}
}

// A Unit that keeps killing the resource is isolated after maxUnitReloads rather than looping
// fail-scope / reopen / resume / die forever (ADR 0023 §21).
func TestAPoisonUnitIsIsolatedRatherThanReloadingForever(t *testing.T) {
	entries := 0
	a, _ := actorWith(t, methodRegistry("scan", core.MethodFunc(
		func(s *core.Session, b *core.Batch, ds *core.Dataset) error {
			entries++
			for range b.All() {
				return &core.SessionLostError{Msg: "this Unit kills the resource"}
			}
			return nil
		})))

	req := RunBatchReq{Method: "scan", Units: []any{"poison"}, RunID: "r", NodeID: "n"}
	if _, err := a.RunBatch(context.Background(), req); err == nil {
		t.Fatal("the first kill must surface as a dead resource so the handler reloads")
	}
	resp, err := a.RunBatch(context.Background(), req) // the reload; the Unit kills it again
	if err != nil {
		t.Fatalf("the second kill must ISOLATE the Unit, not ask for another reload: %v", err)
	}
	if len(resp.Failures) != 1 || resp.Failures[0]["category"] != "exhausted" {
		t.Fatalf("failures = %v, want one exhausted Unit", resp.Failures)
	}
	if entries != 2 {
		t.Errorf("Method entered %d times, want 2 — a third would be the tight loop §21 stops", entries)
	}
}

// An author NonRetryable is terminal: isolated immediately, with no healthcheck probe and no
// reload (parity with Python's except NonRetryableError branch).
func TestANonRetryableFailureIsolatesTheUnitAsTerminal(t *testing.T) {
	probed := 0
	r := methodRegistry("scan", core.MethodFunc(func(s *core.Session, b *core.Batch, ds *core.Dataset) error {
		for range b.All() {
			return &core.NonRetryableError{Msg: "malformed input"}
		}
		return nil
	}))
	r.HealthcheckFn = func(*core.Session) (any, error) { probed++; return nil, nil }
	a, _ := actorWith(t, r)

	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "scan", Units: []any{"x"}, RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Failures) != 1 || resp.Failures[0]["category"] != "terminal" {
		t.Fatalf("failures = %v, want one terminal Unit", resp.Failures)
	}
	if probed != 0 {
		t.Errorf("healthcheck probed %d times, want 0 — terminal means no reload question to ask", probed)
	}
}

// A panic in an author's Method is classified like any other failure. Without this it would take
// down the worker process and every other Session on it, turning one bad Unit into a fleet-wide
// outage — Python's engine catches Exception, so parity requires it.
func TestAPanicInAMethodIsolatesTheUnitRatherThanKillingTheWorker(t *testing.T) {
	a, _ := actorWith(t, methodRegistry("scan", core.MethodFunc(
		func(s *core.Session, b *core.Batch, ds *core.Dataset) error {
			for unit := range b.All() {
				if unit.Value == "bad" {
					var m map[string]int
					m["boom"] = 1 // assignment to a nil map: a plain author bug
				}
				ds.Push(map[string]any{"ok": unit.Value})
			}
			return nil
		})))

	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "scan", Units: []any{"a", "bad", "c"}, RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(resp.Results), "[map[ok:a] map[ok:c]]"; got != want {
		t.Errorf("results = %s, want %s — the good Units still finish", got, want)
	}
	if len(resp.Failures) != 1 || resp.Failures[0]["unit"] != "bad" {
		t.Fatalf("failures = %v, want the panicking Unit isolated", resp.Failures)
	}
}

// The dispatch payload is a CROSS-LANGUAGE contract: the Go handler writes RunBatchInput
// (handler/activity.go) and this struct reads it. `method` in particular is new in v2 and is what
// selects the Method — a rename on either side does not error, it silently runs the sole Method
// or refuses a multi-Method actor, which reads as an actor bug rather than a lost field.
// Peer of handler/runbatch_test.go::TestTheMethodNameReachesTheActor.
func TestTheDispatchPayloadCarriesTheMethodName(t *testing.T) {
	var req RunBatchReq
	raw := `{"actor_id":"a","units":[{"url":"u"}],"params":{"depth":1},` +
		`"method":"extract","run_id":"r","node_id":"n","run_date":"2026-08-14"}`
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	if req.Method != "extract" {
		t.Errorf("method = %q, want extract", req.Method)
	}
	if req.ActorID != "a" || req.RunID != "r" || req.NodeID != "n" || req.RunDate != "2026-08-14" {
		t.Errorf("lineage fields lost: %+v", req)
	}
	if len(req.Units) != 1 {
		t.Errorf("units = %v, want one", req.Units)
	}
}

// A push from a concurrent task carries its OWN provenance (ADR 0028 §1). This is the rewrite of
// what used to prove per-Unit emit attribution under author-written concurrency: with the record
// decoupled from the Unit, a push made under Units() has no current Unit, so it rides the Batch
// tail and folds into results — and which Unit produced it is knowable only because the author put
// `u.Value` in the record. Under real parallelism the tail order is not input order, so the SET is
// what is asserted; nothing is lost, which is the guarantee that matters. The Units themselves
// commit (empty), so the Batch is done.
func TestAPushFromAConcurrentTaskCarriesItsOwnProvenance(t *testing.T) {
	a, _ := actorWith(t, methodRegistry("scan", core.MethodFunc(
		func(s *core.Session, b *core.Batch, ds *core.Dataset) error {
			units := b.Units() // the author takes the whole Batch
			var wg sync.WaitGroup
			for _, unit := range units {
				wg.Add(1)
				go func(u *core.Unit) {
					defer wg.Done()
					time.Sleep(time.Duration(len(units)-u.Index) * time.Millisecond)
					// No current Unit under Units() -> the push names its own out-of-loop key.
					ds.Push(map[string]any{"from": u.Value}, core.Key(fmt.Sprint(u.Value)))
				}(unit)
			}
			wg.Wait()
			return b.Err()
		})))

	resp, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "scan", Units: []any{"a", "b", "c", "d"}, RunID: "r", NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Done {
		t.Error("every Unit committed, so the batch is done")
	}
	seen := map[string]bool{}
	for _, rec := range resp.Results {
		seen[rec.(map[string]any)["from"].(string)] = true
	}
	if len(seen) != 4 || !seen["a"] || !seen["b"] || !seen["c"] || !seen["d"] {
		t.Errorf("results set = %v, want {a,b,c,d} — a concurrent push was lost to the tail", seen)
	}
}

// A load-only Actor declares no Method: its Units pass through unchanged. This is what keeps an
// Actor that only holds a resource dispatchable at all.
func TestALoadOnlyActorPassesItsUnitsThrough(t *testing.T) {
	r := &core.Registry{Name: "t"}
	r.LoadFn = func(s *core.Session) error { s.Set("opened", true); return nil }
	a, _ := actorWith(t, r)

	resp, err := a.RunBatch(context.Background(), RunBatchReq{Units: []any{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(resp.Results), "[a b]"; got != want {
		t.Errorf("results = %s, want %s", got, want)
	}
}
