package main

import (
	"testing"

	"github.com/medmahmoudi26/kontra-actors/go/desync/vectors"
)

// TestTheDefaultScreenCannotResolveForANonLFClass is the exact interaction that broke a live run.
//
// `defaultScreen` names LF-class vector IDs. A campaign that set `class=crlf` and left the screen
// alone therefore hit a selection that matches nothing — and the two obvious reactions are both
// wrong. Fail open and the screen silently becomes the whole corpus: measured below as a 15x jump,
// sent at up to `max_points` injection points on every crawled exchange. Fail closed and the
// Session is refused over a default the caller never typed, which is what happened on
// campaign-1790559894 and cost that run its entire CRLF axis.
//
// This test asserts the premise (the default really does resolve to nothing) and the remedy
// (a derived screen exists and is small), so neither can drift back.
func TestTheDefaultScreenCannotResolveForANonLFClass(t *testing.T) {
	// The premise: the shipped default is LF-shaped.
	if _, err := selectVectors(vectors.TierQuick, "lf", defaultScreen); err != nil {
		t.Fatalf("the default screen must resolve for the default class: %v", err)
	}
	if _, err := selectVectors(vectors.TierQuick, "crlf", defaultScreen); err == nil {
		t.Fatal("the default screen resolved for class=crlf — this test's premise is stale, " +
			"and the fail-open branch in loadScanner is now unreachable")
	}

	// The remedy: a derived screen, and a SMALL one. The whole point of screening is that it
	// costs a couple of requests per point rather than the corpus.
	all := vectors.Generate(vectors.TierQuick, []vectors.Class{vectors.ClassCRLF})
	screen := vectors.ScreenOf(all)
	if len(screen) == 0 {
		t.Fatal("no screen could be derived for class=crlf — loadScanner would refuse the Session")
	}
	if len(screen) >= len(all) {
		t.Fatalf("derived screen is %d of %d vectors; a screen that is the corpus is not a screen",
			len(screen), len(all))
	}
	// And it must not be quietly enormous: the fail-open bug was a 2 -> 31 jump.
	if len(screen) > 4 {
		t.Fatalf("derived screen is %d vectors; the shipped default is 2", len(screen))
	}
}

// TestAnExplicitScreenTypoIsStillAnError — the guard the derivation must not weaken.
func TestAnExplicitScreenTypoIsStillAnError(t *testing.T) {
	if _, err := selectVectors(vectors.TierQuick, "lf", "fold-lf-u070z"); err == nil {
		t.Fatal("a misspelled screen vector was accepted; it would silently widen the sweep")
	}
}
