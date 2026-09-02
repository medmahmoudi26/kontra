package kontra

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra-local/sdk/go/core"
)

// The entrypoint is Serve(), and Run() is GONE rather than aliased (ADR 0023 §23).
//
// An alias would keep the wrong verb reachable and therefore keep it in examples, in READMEs and
// in the next actor somebody writes. `run` names the CALLER's direction — "make this actor do the
// work" — and this call does the opposite: it boots a worker and blocks. Python made exactly this
// correction; leaving Go's old name behind would make the two SDKs disagree about what the word
// means, which is worse than the break.
func TestServeIsTheEntrypointAndRunIsGone(t *testing.T) {
	at := reflect.TypeOf(&Actor{})
	if _, ok := at.MethodByName("Serve"); !ok {
		t.Error("(*Actor).Serve is the entrypoint and must exist")
	}
	if _, ok := at.MethodByName("Run"); ok {
		t.Error("(*Actor).Run is back — Serve replaced it, and an alias keeps the wrong verb alive")
	}
}

// The step chaining is gone with it: there is no way to register a body that consumes the
// previous one's output, because declared topology is the graph drawn in decorators (ADR 0023
// §16). What an author registers now is named, independently dispatchable Methods.
func TestThereIsNoDeclaredTopologyLeftToRegister(t *testing.T) {
	at := reflect.TypeOf(&Actor{})
	for _, gone := range []string{"Step", "Arun"} {
		if _, ok := at.MethodByName(gone); ok {
			t.Errorf("(*Actor).%s is back — declaration order is not a data-flow contract", gone)
		}
	}
}

// An Actor declares several named Methods and a dispatch selects one.
func TestAnActorDeclaresSeveralNamedMethodsAndTheDispatchSelectsOne(t *testing.T) {
	var ran string
	a := New()
	a.Method("crawl", func(*Session, *Batch, *Dataset) error { ran = "crawl"; return nil })
	a.Method("extract", func(*Session, *Batch, *Dataset) error { ran = "extract"; return nil })

	m, err := a.reg.ResolveMethod("extract")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Fn(core.NewSession(nil, "t"), core.NewBatch(nil, nil), core.NewDataset(nil)); err != nil {
		t.Fatal(err)
	}
	if ran != "extract" {
		t.Errorf("ran %q, want extract", ran)
	}
}

// Registering one name twice fails in main(), at the line that is wrong, rather than sending a
// dispatch to whichever body happened to register last.
func TestOneNameCannotBeClaimedTwice(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(strings.ToLower(reflectString(r)), "twice") {
			t.Errorf("recover() = %v, want a panic about a name declared twice", r)
		}
	}()
	a := New()
	a.Method("crawl", func(*Session, *Batch, *Dataset) error { return nil })
	a.Method("crawl", func(*Session, *Batch, *Dataset) error { return nil })
}

func reflectString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return ""
}

// An author must be able to test a Method without a worker. The whole point of the author owning
// the loop is that the loop is ORDINARY CODE — but ordinary code you cannot call is not testable,
// and until these two constructors existed a Go actor's Methods could only be exercised by booting
// Temporal. Neither Session nor Batch is constructible from outside: both live in internal/, and a
// zero-value Session nil-panics on Set because its mutex is unset.
func TestAMethodCanBeTestedWithoutAWorker(t *testing.T) {
	s := NewSession(map[string]any{"tag": "t"})
	s.Set("resource", "loaded")

	b := TestBatch(map[string]any{"host": "a.test"}, map[string]any{"host": "b.test"})
	ds := TestDataset() // the destination the caller hands over, substituted for the lake
	method := func(s *Session, b *Batch, ds *Dataset) error {
		for unit := range b.All() {
			res, _ := s.Get("resource")
			ds.Push(map[string]any{"seen": unit.Str("host"), "with": res})
		}
		return b.Err()
	}
	if err := method(s, b, ds); err != nil {
		t.Fatal(err)
	}

	want := "[map[seen:a.test with:loaded] map[seen:b.test with:loaded]]"
	if got := fmt.Sprint(ds.Records()); got != want {
		t.Errorf("Records() = %s, want %s", got, want)
	}
}

// Push names no Unit, so concurrency no longer buys Unit-ordered output (ADR 0028 §1): an author
// who fans their Units across goroutines gets the records in push order, and the provenance they
// care about is inside the record. The guarantee this asserts is that NONE is lost under real
// parallelism — the set is complete — not that the order is the input's.
func TestConcurrentPushesLandInPushOrderNotUnitOrder(t *testing.T) {
	b := TestBatch("a", "b", "c")
	ds := TestDataset()
	units := b.Units()
	var wg sync.WaitGroup
	for _, u := range units {
		wg.Add(1)
		go func(u *Unit) {
			defer wg.Done()
			time.Sleep(time.Duration(len(units)-u.Index) * time.Millisecond)
			ds.Push(map[string]any{"u": u.Value}) // provenance in the record, not the position
		}(u)
	}
	wg.Wait()

	seen := map[string]bool{}
	for _, rec := range ds.Records() {
		seen[rec.(map[string]any)["u"].(string)] = true
	}
	if len(seen) != 3 || !seen["a"] || !seen["b"] || !seen["c"] {
		t.Errorf("pushed set = %v, want {a,b,c} — a concurrent push lost some", seen)
	}
}

// Each Method declares its OWN input and output, because each has its own signature. Declaring
// them costs no extra top-level lines: the options ride the registration call that already exists.
func TestEachMethodDeclaresItsOwnTakesAndEmits(t *testing.T) {
	type target struct{ Host string }
	type page struct{ Body string }
	type title struct{ Text string }

	a := New()
	a.Method("fetch", func(*Session, *Batch, *Dataset) error { return nil }, Takes(target{}), Emits(page{}))
	a.Method("title", func(*Session, *Batch, *Dataset) error { return nil }, Takes(page{}), Emits(title{}))

	fetch, err := a.reg.ResolveMethod("fetch")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fetch.Takes.(target); !ok {
		t.Errorf("fetch takes %T, want target", fetch.Takes)
	}
	if _, ok := fetch.Emits.(page); !ok {
		t.Errorf("fetch emits %T, want page", fetch.Emits)
	}
	// The second Method's types are its own — one Actor-level pair could not say this.
	titleM, err := a.reg.ResolveMethod("title")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := titleM.Takes.(page); !ok {
		t.Errorf("title takes %T, want page — it consumes what fetch emits", titleM.Takes)
	}
}

// A Method that declares neither still registers and stays dispatchable by name.
func TestAMethodThatDeclaresNoTypesStillRegisters(t *testing.T) {
	a := New()
	a.Method("run", func(*Session, *Batch, *Dataset) error { return nil })

	m, err := a.reg.ResolveMethod("run")
	if err != nil {
		t.Fatal(err)
	}
	if m.Takes != nil || m.Emits != nil {
		t.Errorf("undeclared types must stay nil, got %v/%v", m.Takes, m.Emits)
	}
}

// Actor-level typing is GONE, not merely discouraged. Since an Actor has many Methods with
// different signatures, one Actor-level pair cannot describe it — keeping `a.IO` would leave a
// second way to declare types that is wrong for every actor with more than one Method.
func TestActorLevelIOIsGone(t *testing.T) {
	if _, ok := reflect.TypeOf(&Actor{}).MethodByName("IO"); ok {
		t.Error("(*Actor).IO is back — what a Method takes and emits belongs on the Method")
	}
	// Params stays: run-wide config is not a per-Method signature.
	if _, ok := reflect.TypeOf(&Actor{}).MethodByName("Params"); !ok {
		t.Error("(*Actor).Params must stay — params are Actor-level")
	}
}
