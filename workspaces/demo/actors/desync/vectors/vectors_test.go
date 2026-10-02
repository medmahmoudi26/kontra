package vectors

import (
	"strings"
	"testing"
)

// TestRulePredictsEveryKnownBypass is the load-bearing test. Six payloads were found by
// hand over nearly three years, one per patch cycle. If the generator does not produce
// all six from first principles, the rule is wrong and the whole approach is a wordlist
// with extra steps.
func TestRulePredictsEveryKnownBypass(t *testing.T) {
	want := map[string]string{
		"%0A":          "raw LF",
		"%C4%8A":       "U+010A",
		"%DC%8A":       "U+070A — the unreported the target bypass",
		"%E0%AC%8A":    "U+0B0A",
		"%E5%98%8A":    "U+560A",
		"%F0%9F%98%8A": "U+1F60A",
		"%C4%8D":       "U+010D, the CR twin from PortSwigger's own docs",
	}
	got := map[string]Vector{}
	for _, v := range Generate(TierFull, nil) {
		got[v.Encoded] = v
	}
	for enc, why := range want {
		v, ok := got[enc]
		if !ok {
			t.Errorf("generator missed %s (%s)", enc, why)
			continue
		}
		if !v.Known {
			t.Errorf("%s (%s) should be flagged Known", enc, why)
		}
		if v.Tier != TierQuick {
			t.Errorf("%s is publicly confirmed but sits in tier %d; it must be in the cheap sweep", enc, v.Tier)
		}
	}
}

// TestEveryFoldEndsInTheTargetByte — the invariant the whole family rests on.
func TestEveryFoldEndsInTheTargetByte(t *testing.T) {
	for _, v := range Generate(TierFull, nil) {
		if v.Family != FamilyFold {
			continue
		}
		var cp rune
		if _, err := fmtSscan(v.Codepoint, &cp); err != nil {
			t.Fatalf("bad codepoint %q: %v", v.Codepoint, err)
		}
		wantByte := byte(0x0A)
		if v.Class == ClassCR {
			wantByte = 0x0D
		}
		if byte(cp&0xFF) != wantByte {
			t.Fatalf("%s: U+%04X low byte is 0x%02X, want 0x%02X", v.ID, cp, cp&0xFF, wantByte)
		}
	}
}

// TestTwoByteFoldSpaceIsExactlySeven pins the arithmetic: a +0x100 step in the codepoint
// is a +4 step in the UTF-8 lead byte, so the two-byte space is C4 C8 CC D0 D4 D8 DC and
// nothing else. Two of those seven are publicly known; the other five are predictions.
func TestTwoByteFoldSpaceIsExactlySeven(t *testing.T) {
	var lead []string
	for _, v := range Generate(TierQuick, []Class{ClassLF}) {
		if v.Family == FamilyFold && strings.Count(v.WireBytes, " ") == 1 {
			lead = append(lead, strings.Fields(v.WireBytes)[0])
		}
	}
	want := []string{"c4", "c8", "cc", "d0", "d4", "d8", "dc"}
	if len(lead) != len(want) {
		t.Fatalf("2-byte LF fold space = %v, want %v", lead, want)
	}
	seen := map[string]bool{}
	for _, l := range lead {
		seen[l] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Fatalf("missing lead byte %s (have %v)", w, lead)
		}
	}
}

// TestTiersBoundScanCost — tier 1 has to stay cheap enough to sweep a whole scope.
func TestTiersBoundScanCost(t *testing.T) {
	q := len(Generate(TierQuick, nil))
	s := len(Generate(TierStandard, nil))
	f := len(Generate(TierFull, nil))
	t.Logf("tier sizes: quick=%d standard=%d full=%d", q, s, f)
	if q > 60 {
		t.Fatalf("tier 1 is %d vectors; too expensive for a full-scope sweep", q)
	}
	if !(q < s && s < f) {
		t.Fatalf("tiers must be strictly nested: %d %d %d", q, s, f)
	}
}

// TestNoDuplicateEncodings — a duplicate is a wasted request on every target in scope.
func TestNoDuplicateEncodings(t *testing.T) {
	seen := map[string]string{}
	for _, v := range Generate(TierFull, nil) {
		if prev, dup := seen[v.Encoded]; dup {
			t.Fatalf("duplicate encoding %s from %s and %s", v.Encoded, prev, v.ID)
		}
		seen[v.Encoded] = v.ID
	}
}

// TestAskingForCRLFDoesNotSilentlyRunTheDefaultSweep is the regression this class exists for.
//
// `ClassCRLF` was declared and unreachable. `Generate`'s class map was `map[byte]Class` — one
// byte per class — so a two-byte sequence had nowhere to live, and both `selectVectors` copies
// parsed only "lf" and "cr". The token was therefore DROPPED, which left the class list empty,
// and an empty list means "both": a caller who asked for CRLF got the default LF/CR sweep and a
// run that reported success over ground it had never sent a byte at.
//
// So it is not enough to assert that CRLF generates something. It has to generate something
// DISJOINT from the default, or the old behaviour would pass this test.
func TestAskingForCRLFDoesNotSilentlyRunTheDefaultSweep(t *testing.T) {
	def := map[string]bool{}
	for _, v := range Generate(TierQuick, nil) {
		def[v.Encoded] = true
	}
	crlf := Generate(TierQuick, []Class{ClassCRLF})
	if len(crlf) == 0 {
		t.Fatal("class=crlf generated no vectors at all — the bounded sweep that covers nothing")
	}
	for _, v := range crlf {
		if v.Class != ClassCRLF {
			t.Fatalf("%s came back as class %q from a crlf-only request", v.ID, v.Class)
		}
		if def[v.Encoded] {
			t.Fatalf("%s (%s) is also in the DEFAULT sweep — crlf is being silently reinterpreted",
				v.ID, v.Encoded)
		}
	}
}

// TestEveryCRLFVectorNarrowsToCarriageReturnLineFeed pins the rule the class claims.
//
// A CRLF vector is only a CRLF vector if the bytes a narrowing backend is left holding are
// exactly 0x0D 0x0A — and its matched control is only a control if the same construction one
// value higher is NOT. Checked on the decoded wire bytes, because that is the input the
// conversion actually operates on.
func TestEveryCRLFVectorNarrowsToCarriageReturnLineFeed(t *testing.T) {
	for _, v := range Generate(TierFull, []Class{ClassCRLF}) {
		if v.Family != FamilyFold && v.Family != FamilyRaw {
			continue // overlong and double-encoded forms are about DECODE passes, not narrowing
		}
		low := lowBytesOf(v.WireBytes)
		if len(low) != 2 || low[0] != 0x0D || low[1] != 0x0A {
			t.Fatalf("%s: wire %q narrows to % x, want 0d 0a", v.ID, v.WireBytes, low)
		}
		ctl := lowBytesOf(hexOf(decodePercent(v.Control)))
		if len(ctl) == 2 && ctl[0] == 0x0D && ctl[1] == 0x0A {
			t.Fatalf("%s: the matched control %s narrows to a CRLF too — it controls for nothing",
				v.ID, v.Control)
		}
	}
}

// lowBytesOf narrows a UTF-8 wire string the way a lossy backend does: one byte per codepoint,
// the least-significant one. That is the whole primitive this package is built on.
func lowBytesOf(wire string) []byte {
	var out []byte
	var b []byte
	for _, f := range strings.Fields(wire) {
		var v int
		sscanHex(f, &v)
		b = append(b, byte(v))
	}
	for _, r := range string(b) {
		out = append(out, byte(r&0xFF))
	}
	return out
}

// TestAnUnknownClassIsRefusedRatherThanIgnored — invariant 8, at the parse boundary.
func TestAnUnknownClassIsRefusedRatherThanIgnored(t *testing.T) {
	for _, bad := range []string{"crfl", "lf,cr,nope", "CR LF", "0x0a"} {
		if _, err := ParseClasses(bad); err == nil {
			t.Fatalf("ParseClasses(%q) was accepted; an unknown token must not widen the sweep", bad)
		}
	}
	// And the three real ones, in any case, in any order, deduplicated.
	got, err := ParseClasses(" CRLF , lf ,LF")
	if err != nil {
		t.Fatalf("ParseClasses rejected a valid list: %v", err)
	}
	if len(got) != 2 || got[0] != ClassCRLF || got[1] != ClassLF {
		t.Fatalf("ParseClasses gave %v, want [crlf lf] — case-insensitive and deduplicated", got)
	}
	if got, err := ParseClasses(""); err != nil || got != nil {
		t.Fatalf("an empty class must mean the default (nil), got %v, %v", got, err)
	}
}

// TestTheDefaultSweepIsUnchangedByTheCRLFWiring. The corpus is content-addressed downstream and
// a campaign compares against previous ones, so `lf,cr` must produce exactly what it always did.
func TestTheDefaultSweepIsUnchangedByTheCRLFWiring(t *testing.T) {
	for _, tier := range []int{TierQuick, TierStandard, TierFull} {
		implicit := Generate(tier, nil)
		explicit := Generate(tier, []Class{ClassLF, ClassCR})
		if len(implicit) != len(explicit) {
			t.Fatalf("tier %d: default is %d vectors, lf+cr is %d", tier, len(implicit), len(explicit))
		}
		for i := range implicit {
			if implicit[i].Encoded != explicit[i].Encoded || implicit[i].ID != explicit[i].ID {
				t.Fatalf("tier %d row %d: default %s/%s vs explicit %s/%s",
					tier, i, implicit[i].ID, implicit[i].Encoded, explicit[i].ID, explicit[i].Encoded)
			}
		}
		for _, v := range implicit {
			if v.Class == ClassCRLF {
				t.Fatalf("tier %d: %s — crlf leaked into the default sweep", tier, v.ID)
			}
		}
	}
}

func fmtSscan(s string, cp *rune) (int, error) {
	var v int
	n, err := sscanHex(strings.TrimPrefix(s, "U+"), &v)
	*cp = rune(v)
	return n, err
}

func sscanHex(s string, v *int) (int, error) {
	var n int
	for _, c := range s {
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		default:
			return n, nil
		}
		*v = *v<<4 | d
		n++
	}
	return n, nil
}

// TestTheDerivedScreenFollowsTheClass — the screen must not be LF-shaped for a non-LF class.
//
// The actor's default screen names LF-class vector IDs. A caller who sets `class=crlf` and leaves
// the screen alone therefore selects NOTHING with it, and both obvious behaviours are wrong:
// failing open promotes the screen to the whole corpus (measured: 2 vectors to 31, sent at up to
// 40 points per exchange), and failing closed refuses a Session over a default nobody typed.
// `ScreenOf` is the third answer — derive an equivalent screen for the classes actually selected.
func TestTheDerivedScreenFollowsTheClass(t *testing.T) {
	for _, tc := range []struct{ name string; classes []Class; want int }{
		{"lf", []Class{ClassLF}, 2},
		{"crlf", []Class{ClassCRLF}, 2},
		{"lf+cr+crlf", []Class{ClassLF, ClassCR, ClassCRLF}, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all := Generate(TierQuick, tc.classes)
			screen := ScreenOf(all)
			if len(screen) != tc.want {
				t.Fatalf("ScreenOf gave %d vectors, want %d (one raw + one fold per class)", len(screen), tc.want)
			}
			// It must be a real SUBSET — a screen containing something the sweep will not send is
			// a screen that can react to a vector nothing follows up.
			in := map[string]bool{}
			for _, v := range all {
				in[v.ID] = true
			}
			seenRaw, seenFold := map[Class]bool{}, map[Class]bool{}
			for _, v := range screen {
				if !in[v.ID] {
					t.Fatalf("%s is not in the selected corpus", v.ID)
				}
				switch v.Family {
				case FamilyRaw:
					seenRaw[v.Class] = true
				case FamilyFold:
					seenFold[v.Class] = true
					if !v.Known {
						t.Errorf("%s: the screen's fold should be a KNOWN vector where one exists", v.ID)
					}
				default:
					t.Fatalf("%s: family %q has no place in a screen", v.ID, v.Family)
				}
			}
			for _, c := range tc.classes {
				if !seenRaw[c] || !seenFold[c] {
					t.Fatalf("class %q: raw=%v fold=%v — a screen missing either cannot screen",
						c, seenRaw[c], seenFold[c])
				}
			}
		})
	}
	// And it is empty only when there is nothing to screen, never silently.
	if got := ScreenOf(nil); len(got) != 0 {
		t.Fatalf("ScreenOf(nil) gave %d vectors", len(got))
	}
}
