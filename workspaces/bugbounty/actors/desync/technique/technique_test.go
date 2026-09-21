package technique

import (
	"strings"
	"testing"
)

// TestFamiliesRegenerateEveryReportedRequest is the load-bearing one, and it is the offline half
// of the acceptance test (ACCEPTANCE.md, Assertion 1).
//
// Nine findings, five programs, three years, one per patch cycle — every one of them must fall
// out of a family's enumeration. If they do not, this is a wordlist with extra steps and the
// whole "extract the idea, not the request" decision was wrong.
//
// Same contract as vectors.TestRulePredictsEveryKnownBypass, deliberately.
func TestFamiliesRegenerateEveryReportedRequest(t *testing.T) {
	for _, f := range Families {
		rows := f.Expand()
		if len(rows) == 0 {
			t.Fatalf("family %s expanded to nothing", f.ID)
		}
		for _, seed := range f.Seeds {
			var found *Row
			for i := range rows {
				if strings.Replace(rows[i].HeaderLine, "${cl}", seed.CL, 1) == seed.Line {
					found = &rows[i]
					break
				}
			}
			if found == nil {
				t.Errorf("family %s does NOT regenerate %s: %q", f.ID, seed.Report, seed.Line)
				continue
			}
			if found.Tier != 1 {
				t.Errorf("%s regenerates %s but at tier %d; a paid finding must be tier 1 or the "+
					"host screen will never send it", f.ID, seed.Report, found.Tier)
			}
			if !strings.Contains(found.Provenance, seed.Report) {
				t.Errorf("%s regenerates %s but provenance says %q", f.ID, seed.Report, found.Provenance)
			}
		}
	}
}

// TestSeedsAreTheScreen asserts the tier-1 set is small enough to send to every host in scope and
// large enough to cover every distinct gadget that has been paid for. The screen is 9 probes per
// host in the design; a tier-1 set that quietly grew to 200 would turn a 2-minute screen into a
// 40-minute one on 37,000 hosts.
func TestSeedsAreTheScreen(t *testing.T) {
	var tier1 int
	for _, f := range Families {
		for _, r := range f.Expand() {
			if r.Tier == 1 {
				tier1++
			}
		}
	}
	if tier1 == 0 {
		t.Fatal("no tier-1 rows: the screen would send nothing")
	}
	if tier1 > 20 {
		t.Errorf("tier-1 set is %d rows; the screen sends this to EVERY host, so it is a "+
			"per-host cost. Re-check tierOf.", tier1)
	}
	t.Logf("tier-1 screen set: %d rows", tier1)
}

// TestNoWellFormedMembers guards the one silent failure in expansion: a member that renders to an
// ordinary `Content-Length: 5` tests nothing, and a screen carrying it burns a probe on every
// host in scope while reporting clean.
func TestNoWellFormedMembers(t *testing.T) {
	for _, f := range Families {
		for _, r := range f.Expand() {
			for _, c := range []Casing{CasePreserve, CaseLower, CaseUpper} {
				if r.HeaderLine == applyCasing(f.HeaderName, c)+": ${cl}" {
					t.Errorf("%s expanded a well-formed line: %q", r.Variant, r.HeaderLine)
				}
			}
		}
	}
}

// TestEveryRowRendersToWireBytes is the counterpart to probe's TestRawBytesReachTheWireVerbatim:
// a row with an unbound slot renders a literal "${tenant}" onto the wire and the probe silently
// tests nothing. Render must refuse rather than send it.
func TestEveryRowRendersToWireBytes(t *testing.T) {
	b := Bindings{
		Host: "example.com", Endpoint: "/api/v2/user",
		HeaderBlock: "User-Agent: kontra\r\n", Random: "847213", CL: "10",
	}
	for _, f := range Families {
		rows := f.Expand()
		for _, r := range rows[:min(50, len(rows))] {
			raw, err := r.Render(b)
			if err != nil {
				t.Fatalf("%s: %v", r.Variant, err)
			}
			if strings.Contains(string(raw), "${") {
				t.Errorf("%s: unsubstituted slot survived into wire bytes: %q", r.Variant, raw)
			}
			if !strings.HasPrefix(string(raw), "POST /api/v2/user?cb=847213 HTTP/1.1\r\n") {
				t.Errorf("%s: request line wrong: %q", r.Variant, firstLine(string(raw)))
			}
		}
	}
}

// TestUnboundSlotRefuses is the same guard from the other side.
func TestUnboundSlotRefuses(t *testing.T) {
	r := Row{RequestText: "GET ${endpoint} HTTP/1.1\r\nX-Tenant: ${tenant}\r\n\r\n"}
	if _, err := r.Render(Bindings{Endpoint: "/"}); err == nil {
		t.Fatal("Render accepted an unbound slot; the probe would have sent a literal ${tenant}")
	}
}

// TestCLIsNeverAutoComputed is the trap smuggler.py sets for a reader who treats __REPLACE_CL__
// as a convenience. A deliberately WRONG Content-Length is the attack. If Render ever computes
// len(body) instead of taking the caller's value, every framing technique in the corpus is
// disarmed and the scan reports clean at full speed.
func TestCLIsNeverAutoComputed(t *testing.T) {
	rows := CLObfuscation.Expand()
	raw, err := rows[0].Render(Bindings{
		Host: "example.com", Endpoint: "/", Random: "1", HeaderBlock: "", CL: "999999",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "999999") {
		t.Fatalf("caller's CL was not honoured; got %q", raw)
	}
}

func firstLine(s string) string {
	if i := strings.Index(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
