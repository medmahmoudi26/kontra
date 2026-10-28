package relay

import (
	"strings"
	"testing"

	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/upstream"
)

// Finding security-1 (the gate): a sealed service's error text (an A2A error's message, which may quote the sealed
// input) was logged at Info and stored in task_events as the task's failure text. For a sealed service the gate keeps
// its own words only; an unsealed service's message is kept as before.
func TestReviewSealedFailureText(t *testing.T) {
	f := upstream.Failure{Class: upstream.Protocol, Message: "invalid card number '4111-1111-1111-1111'"}
	for _, mode := range []string{"required", "optional"} {
		r := &run{e: &Executor{svc: store.ServiceRef{Name: "pay", Sealed: mode}}}
		if got := r.failureText(f); strings.Contains(got, "4111") || got == "" {
			t.Fatalf("sealed %s: %q", mode, got)
		}
	}
	r := &run{e: &Executor{svc: store.ServiceRef{Name: "pay"}}}
	if got := r.failureText(f); got != f.Message {
		t.Fatalf("an unsealed service's message: %q", got)
	}
	// The gate's own words for other classes are kept, sealed or not.
	g := upstream.Failure{Class: upstream.Unreachable, Message: "the service could not be reached"}
	r = &run{e: &Executor{svc: store.ServiceRef{Name: "pay", Sealed: "required"}}}
	if got := r.failureText(g); got != g.Message {
		t.Fatalf("an unreachable service: %q", got)
	}
}
