package core

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// recordingSink is an in-memory Sink: it remembers what was entered, emitted and committed, in
// order, so a test can read the framework's side of the author's loop.
type recordingSink struct {
	entered   []int
	committed []int
	recorded  []string
}

func (s *recordingSink) Enter(u *Unit) { s.entered = append(s.entered, u.Index) }

func (s *recordingSink) Record(u *Unit, rec any) (any, error) {
	s.recorded = append(s.recorded, fmt.Sprintf("u%d:%v", u.Index, rec))
	return rec, nil
}

func (s *recordingSink) Commit(u *Unit) error {
	s.committed = append(s.committed, u.Index)
	return nil
}

func items(vals ...any) []Item {
	out := make([]Item, len(vals))
	for i, v := range vals {
		out[i] = Item{Index: i, Value: v}
	}
	return out
}

// The author ranges over the Batch and pushes to its Dataset — the shape ADR 0023 §23 and ADR 0028
// require of Go. A push made while a Unit is the iterator's current Unit is attributed to that Unit
// (durability unchanged, ADR 0028 §1), and asking for the next Unit is what commits the last one.
func TestTheAuthorRangesOverTheBatchAndPushesToItsDataset(t *testing.T) {
	sink := &recordingSink{}
	b := NewBatch(sink, items("a", "b"))
	ds := NewDataset(b)

	for unit := range b.All() {
		ds.Push(unit.Value.(string) + "!")
	}
	if err := b.Settle(); err != nil {
		t.Fatal(err)
	}

	if got, want := fmt.Sprint(sink.recorded), "[u0:a! u1:b!]"; got != want {
		t.Errorf("pushed records = %s, want %s — each attributed to the Unit it was pushed under", got, want)
	}
	if got, want := fmt.Sprint(sink.committed), "[0 1]"; got != want {
		t.Errorf("committed = %s, want %s", got, want)
	}
}

// concurrentSink is a Sink safe for the framework's own side, so a race the test reports is the
// UNIT's, not the double's.
type concurrentSink struct {
	mu       sync.Mutex
	recorded int
}

func (s *concurrentSink) Enter(*Unit) {}

func (s *concurrentSink) Record(_ *Unit, rec any) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recorded++
	return rec, nil
}

func (s *concurrentSink) Commit(*Unit) error { return nil }

// Push is safe from concurrent goroutines (ADR 0023 §23; ADR 0028). Run under -race, and with real
// parallelism rather than an assertion about one: the author owns the loop, so an author who fans
// their Units out across goroutines is doing the thing §18 exists to allow, and a lost record here
// is a Unit that reports success having produced nothing.
//
// Because the author took the whole Batch (Units()), there is no current Unit, so every push rides
// the TAIL (ADR 0028 §1) under an explicit key rather than attributing to a Unit — provenance the
// author cares about goes inside the record. The guarantee under concurrency is that pushes are
// serialized, so NONE is lost or torn; their order is lock-acquisition order, so this asserts the
// SET, not the sequence.
func TestConcurrentPushesAreSafeAndNoneIsLost(t *testing.T) {
	const units, perUnit = 8, 50
	sink := &concurrentSink{}
	vals := make([]any, units)
	for i := range vals {
		vals[i] = i
	}
	b := NewBatch(sink, items(vals...))
	ds := NewDataset(b)

	var wg sync.WaitGroup
	for _, unit := range b.Units() { // the author takes the whole Batch and fans out
		wg.Add(1)
		go func(u *Unit) {
			defer wg.Done()
			var inner sync.WaitGroup
			for j := 0; j < perUnit; j++ { // ...and fans out again WITHIN one Unit
				inner.Add(1)
				go func(seed, k int) {
					defer inner.Done()
					// No current Unit under Units() -> each push names its own out-of-loop key.
					ds.Push(map[string]any{"seed": seed, "k": k}, Key(fmt.Sprintf("%d-%d", seed, k)))
				}(u.Index, j)
			}
			inner.Wait()
		}(unit)
	}
	wg.Wait()

	if sink.recorded != units*perUnit {
		t.Errorf("sink saw %d records, want %d — a serialized push must never drop one", sink.recorded, units*perUnit)
	}
	// All of it rides the tail, folded into Emitted after the (empty) per-Unit output. Nothing is
	// lost, which is the whole claim; the order is not asserted, because real parallelism makes it
	// lock-acquisition order.
	seen := map[[2]int]bool{}
	for _, rec := range b.Emitted() {
		m := rec.(map[string]any)
		seen[[2]int{m["seed"].(int), m["k"].(int)}] = true
	}
	if len(seen) != units*perUnit {
		t.Errorf("tail holds %d distinct records, want %d — a concurrent push lost some", len(seen), units*perUnit)
	}
}

// The Unit the iterator was on when the Method failed is the one to blame (ADR 0023 §13). It is
// taken OUT of the committable set so the host can record it as a failure, while every other Unit
// the author already finished stays committed — what they emitted is durable, and re-running them
// would be a second execution of finished work.
func TestBlameNamesTheUnitTheIteratorWasOn(t *testing.T) {
	sink := &recordingSink{}
	b := NewBatch(sink, items("a", "b", "c"))
	ds := NewDataset(b)

	for unit := range b.All() {
		if unit.Index == 1 {
			break // the author's body failed here
		}
		ds.Push(unit.Value)
	}

	blamed, err := b.Blame()
	if err != nil {
		t.Fatal(err)
	}
	if blamed == nil || blamed.Index != 1 {
		t.Fatalf("blamed = %v, want unit 1", blamed)
	}
	if got, want := fmt.Sprint(sink.committed), "[0]"; got != want {
		t.Errorf("committed = %s, want %s — unit 0 finished, unit 1 is blamed, unit 2 was never taken", got, want)
	}
	if b.Pending() != 1 {
		t.Errorf("pending = %d, want 1 — the remainder a re-invoked Method receives", b.Pending())
	}
}

// An author who takes the whole Batch to run it concurrently has no position, so there is nothing
// to blame — and inventing one would pin a failure on whichever Unit happened to be pulled last.
func TestHoldingTheBatchLeavesNothingToBlame(t *testing.T) {
	sink := &recordingSink{}
	b := NewBatch(sink, items("a", "b"))

	units := b.Units()
	if len(units) != 2 {
		t.Fatalf("Units() = %d, want 2", len(units))
	}
	if len(sink.committed) != 0 {
		t.Errorf("holding the Batch must commit nothing until the Method returns, got %v", sink.committed)
	}

	blamed, err := b.Blame()
	if err != nil {
		t.Fatal(err)
	}
	if blamed != nil {
		t.Errorf("blamed = %v, want nil", blamed)
	}
	// Nothing is committed either: with no position there is no Unit that is known-finished, so a
	// Method that failed while holding the Batch fails the whole call and the retry redoes it.
	if len(sink.committed) != 0 {
		t.Errorf("committed = %v, want none — a failure with nothing to blame commits nothing", sink.committed)
	}
}

// A push made with no current Unit — before the loop, after it, or between Units — rides the tail
// (ADR 0028 §1) under an explicit key and folds into the output after the per-Unit records, in
// first-write order of its key. It commits no Unit of its own.
func TestPushOutsideTheLoopRidesTheTail(t *testing.T) {
	sink := &recordingSink{}
	b := NewBatch(sink, items("a", "b"))
	ds := NewDataset(b)

	ds.Push("before", Key("before")) // no current Unit yet
	n := 0
	for range b.All() {
		n++
	}
	ds.Push(map[string]any{"total": n}, Key("total")) // loop ended -> no current Unit
	if err := b.Settle(); err != nil {
		t.Fatal(err)
	}

	if got, want := fmt.Sprint(b.Tail()), "[before map[total:2]]"; got != want {
		t.Errorf("tail = %s, want %s", got, want)
	}
	// The two Units committed empty — the tail belongs to no Unit's commit.
	if got, want := fmt.Sprint(sink.committed), "[0 1]"; got != want {
		t.Errorf("committed = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(b.Emitted()), "[before map[total:2]]"; got != want {
		t.Errorf("Emitted() = %s, want %s — per-Unit output (none) then the tail", got, want)
	}
}

// An out-of-loop push with no key is an author error: it PANICS at the call site (ADR 0028), the
// loud peer of Python raising MissingPushKey — never accepted with a generated key. The hosted
// engine's recover turns this into a whole-call error; here, unhosted, it panics directly.
func TestAnUnkeyedOutOfLoopPushPanics(t *testing.T) {
	b := NewBatch(&recordingSink{}, items("a"))
	ds := NewDataset(b)
	defer func() {
		p := recover()
		if p == nil {
			t.Fatal("an unkeyed out-of-loop push must panic, not ride the tail on an inferred identity")
		}
		if s, ok := p.(string); !ok || !strings.Contains(s, "key") {
			t.Errorf("panic = %v, want it to name the fix (a key)", p)
		}
	}()
	ds.Push("before") // no current Unit, no key -> panics
}

// A collector Dataset (no Batch behind it) is the substitute a Method body is tested with — no
// host, no sink, no store. It records what was pushed, in push order, and Records() reads it back.
// This is the forcing argument for a parameter over a bare emit callable (ADR 0028).
func TestACollectorDatasetRecordsWhatWasPushed(t *testing.T) {
	ds := CollectingDataset()
	ds.Push(map[string]any{"host": "a", "ok": true})
	ds.Push(map[string]any{"host": "down", "ok": false})

	if got, want := fmt.Sprint(ds.Records()),
		"[map[host:a ok:true] map[host:down ok:false]]"; got != want {
		t.Errorf("Records() = %s, want %s", got, want)
	}
}

// The success path for a held Batch: the whole thing commits when the Method returns.
func TestAHeldBatchCommitsWhenTheMethodReturns(t *testing.T) {
	sink := &recordingSink{}
	b := NewBatch(sink, items("a", "b"))
	ds := NewDataset(b)

	for _, u := range b.Units() {
		ds.Push(u.Value, Key(fmt.Sprint(u.Value))) // no current Unit under Units() -> keyed
	}
	if err := b.Settle(); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(sink.committed), "[0 1]"; got != want {
		t.Errorf("committed = %s, want %s", got, want)
	}
}
