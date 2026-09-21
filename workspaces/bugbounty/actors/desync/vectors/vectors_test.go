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
		"%DC%8A":       "U+070A — the unreported voapi bypass",
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
