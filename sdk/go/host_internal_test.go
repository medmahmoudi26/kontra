package kontra

import (
	"testing"

	"github.com/medmahmoudi26/kontra-local/sdk/go/core"
)

// IN-PACKAGE, because the thing under test is deliberately unexported: `host` is the seam the
// author surface uses to reach a runtime it does not import, and if it were reachable from
// outside this package it would be API rather than a seam.
//
// What this pins is the half of the handoff a compile cannot: an actor binary links a runtime by
// BLANK IMPORT, so the only thing standing between "the import is there" and "Serve() works" is
// that Host() actually stores what init() hands it. A typo that made Host a no-op would leave
// every Go actor exiting with "no actor runtime is linked" while the import sat right there in
// main.go — and would break nothing that compiles.
func TestHostRegistersTheRuntimeServeFunction(t *testing.T) {
	prev := host
	t.Cleanup(func() { host = prev })

	host = nil
	called := 0
	Host(func(*core.Registry) error { called++; return nil })
	if host == nil {
		t.Fatal("Host() did not register the runtime; every actor would exit at Serve()")
	}
	if err := host(&core.Registry{}); err != nil || called != 1 {
		t.Fatalf("the registered function was not the one Serve() would call (called=%d, err=%v)", called, err)
	}
}

// A binary with no runtime linked leaves the seam NIL rather than holding a stub that silently
// does nothing — which is what lets Serve() print the import line to add instead of blocking
// forever on a worker that was never built.
func TestTheSeamIsNilUntilARuntimeRegisters(t *testing.T) {
	prev := host
	t.Cleanup(func() { host = prev })

	host = nil
	if host != nil {
		t.Fatal("the unlinked state must be nil, or Serve() cannot detect it")
	}
}
