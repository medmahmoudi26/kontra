package catalog

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

// THE GOLDENS ARE THE PYTHON PEER'S, taken verbatim from tests/test_dispatch_summary.py — the
// weaker half of the discipline identity_conformance_test.go applies to the queue derivations, and
// for the same reason: there is no shared code across this boundary and a drift has no loud failure
// mode. Weaker because a golden copied by hand only holds while somebody keeps copying it; the
// queue derivations have a corpus both sides execute, and this one does not yet. A Go dispatch whose Summary
// drifted would put a line into history that `backend/src/transcript.ts` reads as a dispatch
// that named no Method, on a surface whose whole job is saying which Method ran.

func TestADispatchNamesItsActorVersionAndMethod(t *testing.T) {
	got := DispatchSummary("crawler", "0.1.0", "crawl", "", 12)
	if want := "crawl · crawler@0.1.0 · 12 units"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAKeyedDispatchSaysWhichIdentityItClaimed(t *testing.T) {
	got := DispatchSummary("crawler", "0.1.0", "crawl", "acme.com", 12)
	if want := "crawl · crawler@0.1.0[acme.com] · 12 units"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// THE CROSS-LANGUAGE INVARIANT, and the reason the `@` is emitted even with no version. The reader
// takes the FIRST field as the Method when it has the shape of one, so a version-less Actor that
// rendered as a bare `echo` would be reported as a Method called `echo`.
func TestADispatchThatNamedNoMethodNeverLooksLikeOneThatDid(t *testing.T) {
	cases := []struct{ got, want string }{
		{DispatchSummary("echo", "", "", "", 5), "echo@ · 5 units"},
		{DispatchSummary("crawler", "0.1.0", "", "", 5), "crawler@0.1.0 · 5 units"},
		// units < 0 is the peer of Python's `units=None`: no count, not a count of zero.
		{DispatchSummary("echo", "0.1.0", "", "", -1), "echo@0.1.0"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
		if first := strings.Split(c.got, summarySep)[0]; !strings.Contains(first, "@") {
			t.Errorf("first field %q of a Method-less line reads as a Method name", first)
		}
	}
}

func TestAVersionLessActorStillCarriesItsAtSignWhenAMethodIsNamed(t *testing.T) {
	if got, want := DispatchSummary("echo", "", "say", "", 5), "say · echo@ · 5 units"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAnOrdinaryDispatchIsNowhereNearTheCap(t *testing.T) {
	if n := len(DispatchSummary("crawler", "0.1.0", "crawl", "acme.com", 12)); n >= 64 {
		t.Errorf("an ordinary line is %d bytes; the cap stops being a guard", n)
	}
}

// Four fields, four different authors: the Actor's name and version come from a manifest, the
// Method from an author's registration, the key from whatever an operator bound, and the count from
// the Batch. Each of them is long here, and all of them at once.
func TestNothingAnyCallerCanWritePutsTheLineOverTheCap(t *testing.T) {
	cases := []struct {
		actor, version, method, key string
		units                       int
	}{
		{strings.Repeat("a", 300), "0.1.0", strings.Repeat("m", 300), strings.Repeat("k", 3000), 1234567890},
		{"crawler", "0.1.0-rc.1+build.9999", "crawl", "https://" + strings.Repeat("x", 4000), 0},
		{strings.Repeat("日本語のアクター", 20), "0.1.0", strings.Repeat("検索", 40), strings.Repeat("キー", 500), 999999},
		{strings.Repeat("a", 300), "", "", "", -1},
		// Python's peer uses 10**30; Go's int stops at MaxInt64, which is the widest count this
		// SDK can be handed and therefore the honest worst case here.
		{strings.Repeat("a", 300), "0.1.0" + strings.Repeat("9", 300), strings.Repeat("m", 300), "acme.com", math.MaxInt64},
	}
	for _, c := range cases {
		line := DispatchSummary(c.actor, c.version, c.method, c.key, c.units)
		if len(line) > SummaryBudget {
			t.Errorf("%.8s…: %d bytes over the %d-byte budget", c.actor, len(line), SummaryBudget)
		}
		if !utf8.ValidString(line) {
			t.Errorf("%.8s…: the line was cut inside a character", c.actor)
		}
	}
}

// BUILT TO FIT, and this is the test that tells the two approaches apart. A line composed first and
// cut to 200 bytes afterwards would end mid-word with the unit count gone.
func TestALongActorAndALongMethodBothStillRead(t *testing.T) {
	line := DispatchSummary(
		strings.Repeat("a-very-long-actor-name-", 6), "0.1.0",
		strings.Repeat("an_extremely_long_method_name", 4), "", 37)
	if len(line) > SummaryBudget {
		t.Fatalf("%d bytes over budget", len(line))
	}
	fields := strings.Split(line, summarySep)
	if len(fields) != 3 {
		t.Fatalf("got %d fields: %q", len(fields), line)
	}
	if !strings.HasPrefix(fields[0], "an_extremely_long_method_name") {
		t.Errorf("the Method was not paid first: %q", fields[0])
	}
	if !strings.HasPrefix(fields[1], "a-very-long-actor-name-") {
		t.Errorf("the Actor did not survive: %q", fields[1])
	}
	if fields[2] != "37 units" {
		t.Errorf("the count was pushed off the end: %q", fields[2])
	}
	// A field that was shortened SAYS it was shortened. A silent cut looks exactly like a
	// legitimately short name.
	if !strings.HasSuffix(fields[0], ellipsis) || !strings.HasSuffix(fields[1], ellipsis) {
		t.Errorf("a shortened field did not say so: %q", line)
	}
}

// The key is the one field whose length is an operator's to choose. Its room is what is LEFT OVER —
// reserving the count first is the only reason a 4 KB key cannot report a sweep with no size.
func TestTheUnitCountSurvivesAKeyNobodyInThisRepoControls(t *testing.T) {
	line := DispatchSummary("crawler", "0.1.0", "crawl", "https://"+strings.Repeat("x", 4000), 623)
	if len(line) > SummaryBudget {
		t.Fatalf("%d bytes over budget", len(line))
	}
	if !strings.HasPrefix(line, "crawl · crawler@0.1.0[https://xxx") {
		t.Errorf("got %q", line)
	}
	if !strings.HasSuffix(line, "…] · 623 units") {
		t.Errorf("the count did not survive the key: %q", line)
	}
}

// WHAT THE TWO CAPS BUY: however long all three are, the key still has room to say something.
func TestTheKeyAlwaysHasRoomAtTheRealBudget(t *testing.T) {
	line := DispatchSummary(strings.Repeat("a", 300), "0.1.0"+strings.Repeat("9", 300),
		strings.Repeat("m", 300), "acme.com", math.MaxInt64)
	if len(line) > SummaryBudget {
		t.Fatalf("%d bytes over budget", len(line))
	}
	if !strings.Contains(line, "[acme.com]") {
		t.Errorf("the key was squeezed out at the real budget: %q", line)
	}
}

// `[…]` says nothing true about which identity was claimed, and costs the bytes of saying it.
// Unreachable at the real budget, so it is exercised at a smaller one.
func TestAKeyWithNoRoomLeftIsDroppedRatherThanRenderedAsBrackets(t *testing.T) {
	line := dispatchSummary("crawler", "0.1.0", "crawl", "acme.com", 12, 40)
	if len(line) > 40 {
		t.Fatalf("%d bytes over the 40-byte budget", len(line))
	}
	if strings.Contains(line, "[") {
		t.Errorf("an empty bracket was rendered: %q", line)
	}
	if want := "crawl · crawler@0.1.0 · 12 units"; line != want {
		t.Errorf("got %q, want %q", line, want)
	}
}

// The budget is in BYTES and a name may not be ASCII, so slicing the encoded form can land inside a
// character. A mojibake byte in a bar label is the kind of thing nobody reports and everybody stops
// trusting.
func TestAShortenedFieldIsCutOnACharacterBoundary(t *testing.T) {
	line := DispatchSummary("クローラー", "0.1.0", "検索", strings.Repeat("日本語のキー", 100), 12)
	if len(line) > SummaryBudget {
		t.Fatalf("%d bytes over budget", len(line))
	}
	if !utf8.ValidString(line) {
		t.Errorf("a partial character survived the cut: %q", line)
	}
	if !strings.HasPrefix(line, "検索 · クローラー@0.1.0[日本語のキー") {
		t.Errorf("got %q", line)
	}
}

// Not a production case — the budget is a constant — but it is the boundary the arithmetic is
// easiest to get wrong at, and the answer must never be a lone ellipsis pretending to be a name.
func TestABudgetTooSmallForAFieldLeavesItOutInsteadOfLying(t *testing.T) {
	for budget := range 40 {
		line := dispatchSummary("crawler", "0.1.0", "crawl", "acme.com", 12, budget)
		if len(line) > budget {
			t.Errorf("budget %d: %q is %d bytes", budget, line, len(line))
		}
		if strings.Trim(line, " ·") == ellipsis {
			t.Errorf("budget %d: the whole line is an ellipsis", budget)
		}
	}
}
