package detect

import (
	"github.com/medmahmoudi26/kontra-actors/go/desync/probe"
	"strconv"
	"testing"
)

// The gate's two arms, exercised directly against the lab. The CONTROL arm is what makes this
// more than "does it still do it": it puts a benign request where the attack goes, so anything it
// fires on is noise that already survived the stability gate.
func TestGateArmsSeparateSignalFromNoise(t *testing.T) {
	for _, mode := range []string{"vulnerable", "patched"} {
		l := startCLLab(t, mode)
		sc, tgt := scannerForLab(), labTarget(l)
		bn := benign(tgt.Host)
		base := sc.FramingBaselineOf(t.Context(), tgt, bn, 3)
		if !base.Stable && mode != "erratic" {
			t.Fatalf("%s: baseline unstable %v", mode, base.Statuses)
		}
		body := "GET /smuggled-kontra HTTP/1.1\r\nX: X"
		atk := []byte("POST /x HTTP/1.1\r\nHost: " + tgt.Host + "\r\n\tContent-Length: " +
			strconv.Itoa(len(body)) + "\r\n\r\n" + body)

		run := func(mid []byte) int {
			hits := 0
			for i := 0; i < 8; i++ {
				c := sc.Connection(t.Context(), tgt, []probe.Step{
					{Label: "pre", Raw: bn}, {Label: "attack", Raw: mid}, {Label: "post", Raw: bn},
				}, probe.ModePipelined)
				if _, _, _, sig, void := AnalyzeFraming(c, base, ""); !void.Is && sig.Count > 0 {
					hits++
				}
			}
			return hits
		}
		ctrl, attack := run(bn), run(atk)
		t.Logf("%-11s control=%d/8  attack=%d/8", mode, ctrl, attack)
		if ctrl != 0 {
			t.Errorf("%s: CONTROL arm fired %d/8 — the gate is measuring noise, not the attack", mode, ctrl)
		}
		if mode == "vulnerable" && attack < 6 {
			t.Errorf("vulnerable: attack arm only %d/8; the gate would reject a real finding", attack)
		}
		if mode == "patched" && attack != 0 {
			t.Errorf("patched: attack arm fired %d/8 — false positive", attack)
		}
	}
}
