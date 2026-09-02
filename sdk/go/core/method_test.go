package core

import (
	"fmt"
	"strings"
	"testing"
)

func registryWith(names ...string) *Registry {
	r := &Registry{Name: "t"}
	for _, n := range names {
		r.AddMethod(n, func(*Session, *Batch, *Dataset) error { return nil })
	}
	return r
}

// A dispatch names the Method it means, and the registry resolves that name (ADR 0023 §5).
func TestANamedDispatchPicksThatMethod(t *testing.T) {
	r := registryWith("crawl", "extract")
	m, err := r.ResolveMethod("extract")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m.Name != "extract" {
		t.Errorf("resolved %q, want extract", m.Name)
	}
}

// An Actor with ONE Method needs no name on the wire — which is what makes the retired Activity
// kind (one Method, no load/close) an ordinary Actor.
func TestTheSoleMethodNeedsNoName(t *testing.T) {
	m, err := registryWith("crawl").ResolveMethod("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m.Name != "crawl" {
		t.Errorf("resolved %q, want crawl", m.Name)
	}
}

// Declaration order is NOT a tiebreak (ADR 0023 §16): a caller who forgot the name would
// otherwise silently get whichever body the author happened to write first.
func TestAnUnnamedDispatchAgainstSeveralMethodsRefuses(t *testing.T) {
	_, err := registryWith("crawl", "extract").ResolveMethod("")
	if err == nil {
		t.Fatal("resolve() with two methods and no name must refuse, not pick one")
	}
	if !strings.Contains(err.Error(), "must name one") {
		t.Errorf("error %q should tell the caller to name a Method", err)
	}
}

// A typo names what does exist, so the author reads the fix off the error.
func TestAnUnknownMethodNameSaysWhatDoesExist(t *testing.T) {
	_, err := registryWith("crawl").ResolveMethod("craw")
	if err == nil || !strings.Contains(err.Error(), "have: crawl") {
		t.Errorf("error = %v, want it to list the declared Methods", err)
	}
}

// A load-only Actor declares no Methods at all: its batch passes its Units through unchanged.
func TestALoadOnlyActorResolvesToNoMethod(t *testing.T) {
	m, err := (&Registry{}).ResolveMethod("")
	if m != nil || err != nil {
		t.Errorf("resolve() = (%v, %v), want (nil, nil)", m, err)
	}
}

// Two Methods cannot claim one dispatch name. It fails at registration — in main(), before the
// worker polls anything — rather than sending a dispatch to whichever body registered last.
func TestTwoMethodsCannotClaimOneDispatchName(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("registering a duplicate Method name must panic at registration")
		} else if !strings.Contains(fmt.Sprint(r), "declared twice") {
			t.Errorf("panic %v should say the name was declared twice", r)
		}
	}()
	registryWith("crawl", "crawl")
}
