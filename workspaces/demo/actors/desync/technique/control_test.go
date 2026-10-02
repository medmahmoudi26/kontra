package technique

import (
	"strings"
	"testing"
)

/*
A CONTROL THAT VARIES TWO THINGS ANSWERS NEITHER QUESTION.

That sentence is the whole content of this file, and it is not abstract: the first attempt at
controlling this axis by hand changed the obfuscation AND removed the body, so a silent control
meant either "the gadget mattered" or "bodies matter", with no way to tell. The reports built on
it were retracted.

So the control the corpus ships must differ from its attack in EXACTLY ONE PLACE — the header
line — and these tests are what keeps that true as the family grows.
*/

func rowFor(t *testing.T, variantPrefix string) Row {
	t.Helper()
	for _, r := range CLObfuscation.Expand() {
		if strings.HasPrefix(r.Variant, variantPrefix) {
			return r
		}
	}
	t.Fatalf("no technique with variant prefix %q", variantPrefix)
	return Row{}
}

var binds = Bindings{Host: "api.example.com", Endpoint: "/api/v1/session", Random: "539915"}

func TestTheControlIsTheAttackMinusItsGadget(t *testing.T) {
	r := rowFor(t, "obs-fold-")

	attack, aLen, err := r.RenderAutoCL(binds)
	if err != nil {
		t.Fatal(err)
	}
	control, cLen, err := r.RenderControlAutoCL(binds)
	if err != nil {
		t.Fatal(err)
	}

	// ── THE BODY IS IDENTICAL ────────────────────────────────────────────────────────────────
	// This is the one that was got wrong by hand. The smuggled prefix is what the oracle is
	// looking for; changing it between arms means the arms are not comparable at all.
	aBody, cBody := bodyOf(attack), bodyOf(control)
	if aBody != cBody {
		t.Fatalf("bodies differ — the control is not a control:\n attack:  %q\n control: %q",
			aBody, cBody)
	}
	if aLen != cLen {
		t.Fatalf("body lengths differ: attack %d, control %d", aLen, cLen)
	}

	// ── THE CANARY IS IDENTICAL ──────────────────────────────────────────────────────────────
	// `539915-kontra` is what `CanaryReflected` greps for. A control carrying a different one
	// could never fire, which would make every attack look confirmed.
	if !strings.Contains(cBody, "539915-kontra") {
		t.Fatalf("control lost the canary: %q", cBody)
	}

	// ── THE HEADER LINE IS THE ONLY DIFFERENCE ───────────────────────────────────────────────
	if strings.Contains(string(control), r.HeaderLine) {
		t.Fatalf("control still carries the gadget %q", r.HeaderLine)
	}
	// CASE-INSENSITIVE, because the control KEEPS the attack's casing and that is correct:
	// casing is one of the dimensions this family varies (`Casings`), header names are
	// case-insensitive on the wire, and `content-length: 33` is exactly as well-formed as
	// `Content-Length: 33`. Normalising it in the control would make the arms differ in two
	// places again — the obfuscation and the case — which is the mistake this file exists to
	// prevent.
	if !strings.Contains(strings.ToLower(string(control)), "content-length: "+itoa(cLen)) {
		t.Fatalf("control has no well-formed Content-Length:\n%q", control)
	}

	// Everything outside the header block matches byte for byte. Diffing the request LINES
	// catches an endpoint or cachebuster that drifted.
	if first(attack) != first(control) {
		t.Fatalf("request lines differ:\n attack:  %q\n control: %q", first(attack), first(control))
	}
}

// EVERY member, not just the one variant a reader happens to look at. The family expands to
// thousands of rows across three casings and seven positions, and a control is only a control if
// it holds for the one that fires.
func TestEveryTechniqueHasAControlWithTheSameBody(t *testing.T) {
	rows := CLObfuscation.Expand()
	if len(rows) == 0 {
		t.Fatal("the family expanded to nothing")
	}
	for _, r := range rows {
		if r.ControlText == "" {
			t.Fatalf("%s has no control", r.Variant)
		}
		attack, _, err := r.RenderAutoCL(binds)
		if err != nil {
			continue // a member that cannot render has nothing to control
		}
		control, _, err := r.RenderControlAutoCL(binds)
		if err != nil {
			t.Fatalf("%s: control failed to render: %v", r.Variant, err)
		}
		if bodyOf(attack) != bodyOf(control) {
			t.Fatalf("%s: control body differs from attack body", r.Variant)
		}
		// The control must not still carry the gadget. `Expand` dedupes on the rendered text, so
		// two positions can collapse — but a control that kept its obfuscation would fire for the
		// same reason its attack does and silently withdraw a real finding.
		if strings.Contains(string(control), r.HeaderLine) {
			t.Fatalf("%s: control still carries the gadget %q", r.Variant, r.HeaderLine)
		}
	}
}

/*
THE CONTROL MUST BE WELL-FORMED, which is the property that makes a firing control damning.

`isWellFormed` is what `Expand` uses to REJECT members for testing nothing. The control is
deliberately the exact thing it rejects: if these two definitions ever drift, the "control" grows
a gadget of its own and starts firing for the same reason the attack does — which would silently
withdraw every real finding on the axis.
*/
func TestTheControlIsExactlyWhatExpandRejectsAsUntesting(t *testing.T) {
	for _, c := range CLObfuscation.Casings {
		name := applyCasing(CLObfuscation.HeaderName, c)
		line := name + ": ${cl}"
		if !isWellFormed(line, name) {
			t.Fatalf("the control's header line %q is not well-formed by Expand's own test — "+
				"the two definitions have drifted and the control now carries a gadget", line)
		}
	}
}

func bodyOf(raw []byte) string {
	i := strings.Index(string(raw), "\r\n\r\n")
	if i < 0 {
		return ""
	}
	return string(raw[i+4:])
}

func first(raw []byte) string {
	s := string(raw)
	if i := strings.Index(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
