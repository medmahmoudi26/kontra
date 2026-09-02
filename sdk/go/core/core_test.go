package core

import (
	"sync"
	"testing"
)

// fakeUnitState is a keyed in-memory stand-in for the host's durable per-Unit blob, so the
// accessor semantics can be tested without a state manager.
type fakeUnitState struct {
	mu sync.Mutex
	m  map[string]any
}

func (f *fakeUnitState) Get(key string, out any) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[key]
	if !ok {
		return false, nil
	}
	if p, ok := out.(*any); ok {
		*p = v
	}
	return true, nil
}

func (f *fakeUnitState) Set(key string, value any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]any{}
	}
	f.m[key] = value
	return nil
}

func (f *fakeUnitState) Delete(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, key)
	return nil
}

// An unbound Unit (a plain Method test, no host) must no-op: Get reports absent, Set/Delete are
// no-ops — the Go peer of Python's None-safe self.unit_state.
func TestUnitStateUnboundIsNoop(t *testing.T) {
	u := &Unit{}
	var out any
	if ok, err := u.State().Get("k", &out); ok || err != nil {
		t.Fatalf("unbound Get = (%v, %v), want (false, nil)", ok, err)
	}
	if err := u.State().Set("k", "x"); err != nil {
		t.Fatalf("unbound Set = %v, want nil", err)
	}
	if err := u.State().Delete("k"); err != nil {
		t.Fatalf("unbound Delete = %v, want nil", err)
	}
}

// Two Units must not cross-wire their resume scratch: each Unit's State() resolves to its OWN
// bound slot, which is why the scratch hangs off the Unit and not off the one shared Session.
func TestTwoUnitsDoNotShareScratch(t *testing.T) {
	a, b := &Unit{Index: 0}, &Unit{Index: 1}
	a.BindState(&fakeUnitState{})
	b.BindState(&fakeUnitState{})

	if err := a.State().Set("cursor", "A"); err != nil {
		t.Fatal(err)
	}
	if err := b.State().Set("cursor", "B"); err != nil {
		t.Fatal(err)
	}

	var out any
	if ok, _ := a.State().Get("cursor", &out); !ok || out != "A" {
		t.Fatalf("unit a scratch = (%v, %v), want A", out, ok)
	}
	if ok, _ := b.State().Get("cursor", &out); !ok || out != "B" {
		t.Fatalf("unit b scratch = (%v, %v), want B", out, ok)
	}
}

// A Unit can hold SEVERAL scratch keys at once: each is independently get/set/deletable through
// the one bound slot.
func TestUnitStateHoldsMultipleKeys(t *testing.T) {
	u := &Unit{}
	u.BindState(&fakeUnitState{})
	st := u.State()
	if err := st.Set("frontier", "F"); err != nil {
		t.Fatal(err)
	}
	if err := st.Set("cursor", 7); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete("frontier"); err != nil { // delete one key; the other survives
		t.Fatal(err)
	}
	var cur any
	if ok, _ := st.Get("cursor", &cur); !ok || cur != 7 {
		t.Fatalf("cursor = (%v, %v), want 7", cur, ok)
	}
	var fr any
	if ok, _ := st.Get("frontier", &fr); ok {
		t.Fatalf("frontier should be deleted, got %v", fr)
	}
}

// Unbound global_state (a plain Method test) must no-op across all ops — the Go peer of Python's
// None-safe self.global_state.
func TestGlobalStateUnboundIsNoop(t *testing.T) {
	s := NewSession(nil, "run-1")
	var out any
	if ok, err := s.GlobalState().Get("k", &out); ok || err != nil {
		t.Fatalf("unbound Get = (%v, %v), want (false, nil)", ok, err)
	}
	if err := s.GlobalState().Set("k", 1); err != nil {
		t.Fatal(err)
	}
	if added, err := s.GlobalState().AddToSet("s", "a"); added || err != nil {
		t.Fatalf("unbound AddToSet = (%v, %v), want (false, nil)", added, err)
	}
	if n, err := s.GlobalState().Incr("c", 1); n != 0 || err != nil {
		t.Fatalf("unbound Incr = (%v, %v), want (0, nil)", n, err)
	}
	if ok, err := s.GlobalState().CompareAndSet("k", nil, 1); ok || err != nil {
		t.Fatalf("unbound CompareAndSet = (%v, %v), want (false, nil)", ok, err)
	}
}

// The Session is ONE object for the whole scope, so what a Method leaves on it is visible to the
// next Method of the same Session — the point of many Methods per Actor (ADR 0023 §9).
//
// This is what REPLACED `session_state` when §19 retired it: the durable tier existed to carry
// facts across a session's calls, and the Session's own fields already do that, in the same
// process, on the same instance. The one path that loses them is host death, and that fails the
// scope (§7) — so there was never a reader the durable version served and this one did not.
func TestWhatAMethodLeavesOnTheSessionIsVisibleToTheNext(t *testing.T) {
	s := NewSession(nil, "run-1")

	s.Set("browser", "handle-1")
	if got, ok := s.Get("browser"); !ok || got != "handle-1" {
		t.Fatalf("Session.Get = (%v, %v), want handle-1", got, ok)
	}

	// A key nobody set reads absent rather than panicking — a Method must be unit-testable
	// against a Session it did not load.
	if _, ok := s.Get("never-set"); ok {
		t.Error("an unset key must read absent")
	}
}
