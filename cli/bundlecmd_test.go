package main

import (
	"errors"
	"strings"
	"testing"
)

// A NEW WORD WIRED TO NOTHING is a switch-statement bug, and `dispatch`'s own header says it was
// split out of `main` so that exact mistake could be tested. `bundle` is a new word.
func TestBundleIsARoutedCommand(t *testing.T) {
	err := dispatch([]string{"bundle"})
	if err == nil {
		t.Fatal("`kontra bundle` with no subject succeeded; it should say what the subjects are")
	}
	if errors.Is(err, errUsage) {
		t.Fatal("`kontra bundle` fell through to the unknown-command branch, so it is not wired up")
	}
	for _, want := range []string{"orchestrator", "verify"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the usage line does not mention %q: %v", want, err)
		}
	}
}

func TestBundleNamesItsSubjectsWhenGivenAWrongOne(t *testing.T) {
	// The example used to be "spa", which is now a REAL subject (issue 14 ships the browser
	// bundle separately) — and the test did not merely start passing for the wrong reason, it
	// started BUILDING one into the checkout every time the suite ran. A test whose bogus input
	// becomes real is a test that runs the feature it was written to refuse.
	err := dispatch([]string{"bundle", "everything"})
	if err == nil {
		t.Fatal("an unknown subject was accepted")
	}
	for _, want := range []string{"orchestrator", "spa"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not list %q as something that CAN be bundled, so a guess stays a guess: %v", want, err)
		}
	}
}

// Help is the one surface nothing else checks — see `usageText`'s own comment. A command that
// reaches the switch and not this block is a command nobody finds.
func TestBundleIsDocumented(t *testing.T) {
	for _, want := range []string{"kontra bundle orchestrator", "kontra bundle spa", "kontra bundle verify"} {
		if !strings.Contains(usageText, want) {
			t.Errorf("usageText does not advertise %q", want)
		}
	}
	// And it says what the artifact IS, because "bundle" already means an actor's Bundle in this
	// CLI (`kontra build`) and two nouns under one word is how an operator builds
	// the wrong thing.
	if !strings.Contains(usageText, "Node runtime") {
		t.Error("usageText does not say what an appliance bundle contains, and `bundle` is an overloaded word here")
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{0: "0 B", 999: "999 B", 1 << 20: "1.0 MB", 125758995: "119.9 MB"}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}
