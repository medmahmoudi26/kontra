package detect

import (
	"strconv"
	"testing"

	"github.com/medmahmoudi26/kontra-actors/go/desync/probe"
)

/*
THE DISCRIMINATOR, OVER A REAL SOCKET.

`control_test.go` pins what `ScoreControl` does with a given connection. This file pins the thing
that actually matters: that the control TELLS THE TWO SERVERS APART — the one where the gadget
causes the desync, and the one that would have read the body as a request no matter what was in
the Content-Length header.

The second server is not hypothetical. It is paypalobjects.com on 2026-09-18, where a plain
`Content-Length: 0` fired 3 times in 6 while the two obfuscated attacks fired 1 and 2, and it is
why both reports from that campaign were withdrawn. Before this, the scanner had no way to be
told the difference; `TestTheControlIsSilentOnAGenuineDesync` and
`TestTheControlFiresOnAServerThatMerelyPipelines` are that difference, end to end.

`control` below is the attack's matched twin: byte-identical except that the Content-Length line
is well-formed. Same body, same smuggled request, same everything else — the invariant
`technique/control_test.go` enforces for the real corpus, spelled here by hand so this file tests
the ORACLE rather than the corpus.
*/

// control is `attack` with the gadget removed: a well-formed, correct Content-Length.
func control(host string, body string) []byte {
	return []byte("POST /x HTTP/1.1\r\nHost: " + host + "\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body)
}

// run drives one three-step connection and returns what the oracle made of it.
func run(t *testing.T, sc *Scanner, tgt probe.Target, bn, mid []byte, canary string) (
	*probe.ConnObs, FramingBaseline, FramingSignals) {
	t.Helper()
	base := sc.FramingBaselineOf(t.Context(), tgt, bn, 3)
	if !base.Stable {
		t.Fatalf("lab baseline unstable: %v", base.Statuses)
	}
	c := sc.Connection(t.Context(), tgt, []probe.Step{
		{Label: "pre", Raw: bn},
		{Label: "attack", Raw: mid},
		{Label: "post", Raw: bn},
	}, probe.ModePipelined)
	_, _, _, sig, void := AnalyzeFraming(c, base, canary)
	if void.Is {
		t.Fatalf("void: %s", void.Reason)
	}
	return c, base, sig
}

/*
A GENUINE DESYNC SURVIVES ITS CONTROL.

The vulnerable lab disagrees with itself about the MALFORMED header only: `permissiveCL` reads
`\tContent-Length: N` and `strictCL` does not, so the frontend forwards a body the backend never
consumes. Give the same server a well-formed Content-Length and both parsers agree, the body is
consumed as a body, and nothing is left in the buffer.

So the control must stay silent — and if this test ever fails, the control has become strong
enough to withdraw real findings, which is a far more expensive failure than the one it prevents.
*/
func TestTheControlIsSilentOnAGenuineDesync(t *testing.T) {
	l := startCLLab(t, "vulnerable")
	sc, tgt := scannerForLab(), labTarget(l)
	bn := benign(tgt.Host)
	smuggled := "GET /539915-kontra HTTP/1.1\r\nX: X"

	_, base, sig := run(t, sc, tgt, bn, attack(tgt.Host, smuggled), "539915-kontra")
	if sig.Count == 0 {
		t.Fatalf("the attack did not fire on a known CL.0; nothing to control: %+v", sig)
	}

	ctl, _, _ := run(t, sc, tgt, bn, control(tgt.Host, smuggled), "539915-kontra")
	scored := ScoreControl(sig, ctl, base, "539915-kontra")

	if !scored.ControlUsed {
		t.Fatal("the control connection did not complete — the claim is untested, not confirmed")
	}
	if scored.ControlAlsoFired {
		t.Fatalf("the control fired on a server that only desyncs on the GADGET — this would "+
			"withdraw a real finding: %+v", scored)
	}
	if scored.Count != sig.Count {
		t.Fatalf("a surviving claim was weakened: %d -> %d", sig.Count, scored.Count)
	}
	t.Logf("survived: signals=%+v", scored)
}

/*
A SERVER THAT MERELY PIPELINES DOES NOT.

`pipelining` mode is a server whose parsers BOTH ignore Content-Length — so the body is never
consumed by anyone and always becomes the next request on the connection. The attack fires. So
does an ordinary, perfectly well-formed request. The obfuscation explained nothing.

This is the shape that produced 40 `is_proof` leads and two retracted reports, and it is the
whole reason this arm exists. Note what the assertion is: not that the attack stays silent — it
does not, and it should not — but that the SCANNER STOPS CALLING IT A FINDING.
*/
func TestTheControlFiresOnAServerThatMerelyPipelines(t *testing.T) {
	l := startCLLab(t, "pipelining")
	sc, tgt := scannerForLab(), labTarget(l)
	bn := benign(tgt.Host)
	smuggled := "GET /539915-kontra HTTP/1.1\r\nX: X"

	_, base, sig := run(t, sc, tgt, bn, attack(tgt.Host, smuggled), "539915-kontra")
	if sig.Count == 0 {
		t.Skip("the attack did not fire here, so there is no false claim to withdraw")
	}
	t.Logf("attack fired (as it did on paypalobjects.com): %+v", sig)

	ctl, _, _ := run(t, sc, tgt, bn, control(tgt.Host, smuggled), "539915-kontra")
	scored := ScoreControl(sig, ctl, base, "539915-kontra")

	if !scored.ControlUsed {
		t.Fatal("the control connection did not complete")
	}
	if !scored.ControlAlsoFired {
		t.Fatalf("A WELL-FORMED REQUEST GOT THE SAME BEHAVIOUR AND THE SCANNER DID NOT NOTICE. "+
			"This is exactly the 2026-09-18 retraction, reproduced: %+v", scored)
	}
	if scored.Count != 0 {
		t.Fatalf("the claim survived a control that reproduced it: %+v", scored)
	}
	// And the row built from it must not be promotable.
	row := RowFromSmuggle(FramingObs{Host: tgt.Host, Signals: scored, SignalCount: scored.Count})
	if row.SignalCount != 0 || !has(row.Signals, SigControlAlsoFired) {
		t.Fatalf("a withdrawn claim would still reach desync_leads: %+v", row.Signals)
	}
	t.Logf("withdrawn: %v", row.Signals)
}
