package detect

import (
	"testing"

	"github.com/medmahmoudi26/kontra-actors/go/desync/probe"
)

/*
THE CHECK WHOSE ABSENCE COST TWO REPORTS.

On 2026-09-18 this engine produced 40 `is_proof` leads and two HTML reports against a program and
PayPal. Both were retracted the next day, by one test run by hand: send the same request with a
plain `Content-Length: 0` and see whether it behaves the same. On paypalobjects.com it fired 3
times in 6 while the obfuscated attacks fired 1 and 2 — the gadget was doing nothing, the server
simply reads a pipelined body as a request, and every "proof" was a description of ordinary HTTP.

The scanner could not run that test because it had no control arm, and this file is the pin that
it now does and that it means what it says.
*/

// conn builds a three-step control connection whose step 3 answers `status` with `body`.
func conn(status int, sha string, raw string) *probe.ConnObs {
	return &probe.ConnObs{
		Steps: []probe.StepObs{
			{Label: "pre", Status: 200, RespBytes: 10},
			{Label: "control", Status: 400, RespBytes: 10},
			{Label: "post", Status: status, RespBytes: len(raw), BodySHA: sha,
				RespRaw: []byte(raw)},
		},
	}
}

var baseline = FramingBaseline{Status: 200, BodySHA: "sha-base", Stable: true, N: 3}

// A fired observation, as `AnalyzeFraming` would leave it.
func fired() FramingSignals {
	return FramingSignals{Count: 2, CanaryReflected: true, PostDiffers: true}
}

func TestAControlThatFiresWithdrawsTheWholeClaim(t *testing.T) {
	// The canary comes back on the CONTROL — whose Content-Length is well-formed and correct.
	// The server is not reading Content-Length at all; the body was always going to be read as a
	// request, and the obfuscation explained nothing.
	got := ScoreControl(fired(), conn(200, "sha-base",
		"HTTP/1.1 200 OK\r\n\r\nno resource for /539915-kontra"), baseline, "539915-kontra")

	if !got.ControlUsed || !got.ControlAlsoFired {
		t.Fatalf("control ran and reproduced the behaviour; row must say so: %+v", got)
	}
	if got.Count != 0 {
		t.Fatalf("signal_count = %d, want 0 — a claim the control reproduced is not a claim",
			got.Count)
	}
	// WHOLESALE, not partial. `post_differs` is tempting to keep because something still
	// differed — from the BASELINE, which the control has just shown is the wrong comparison.
	if got.CanaryReflected || got.PostDiffers || got.DualResponse || got.PostTimeout {
		t.Fatalf("signals survived a control that reproduced them: %+v", got)
	}
}

func TestAControlThatStaysSilentLeavesTheClaimStanding(t *testing.T) {
	// Step 3 matches the baseline exactly and the canary is nowhere: with a well-formed
	// Content-Length this server consumed the body as a body. The gadget is load-bearing.
	got := ScoreControl(fired(), conn(200, "sha-base", "HTTP/1.1 200 OK\r\n\r\nhello"),
		baseline, "539915-kontra")

	if !got.ControlUsed || got.ControlAlsoFired {
		t.Fatalf("control ran and stayed silent: %+v", got)
	}
	if got.Count != 2 || !got.CanaryReflected {
		t.Fatalf("a surviving claim must be untouched, got %+v", got)
	}
}

/*
THE THIRD FACT. "We checked and it survived", "we checked and it died" and "WE DID NOT CHECK" are
different, and collapsing the third into the first is the original bug wearing a check's clothing:
the claim stands, the row says it was controlled, and nobody can tell it never was.
*/
func TestAControlThatCouldNotRunIsNotAControlThatStayedSilent(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *probe.ConnObs
	}{
		{"nil", nil},
		{"dial failed", &probe.ConnObs{DialErr: "connection refused"}},
		{"tls failed", &probe.ConnObs{TLSErr: "handshake failure"}},
		{"peer closed before step 3", &probe.ConnObs{Steps: []probe.StepObs{
			{Label: "pre"}, {Label: "control"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreControl(fired(), tc.c, baseline, "539915-kontra")
			if got.ControlUsed {
				t.Fatal("claimed a control ran when it did not — the row would read as tested")
			}
			if got.Count != 2 {
				t.Fatalf("an untested claim must be left exactly as it was, got %+v", got)
			}
		})
	}
}

// The weaker oracles are controlled too. A host whose step 3 differs from the baseline under an
// ORDINARY pipelined body is a host doing that to everyone — which is the innocent explanation
// `post_differs` always had, now observed rather than argued about.
func TestTheWeakOraclesAreControlledToo(t *testing.T) {
	sig := FramingSignals{Count: 1, PostDiffers: true}
	got := ScoreControl(sig, conn(503, "sha-other", "HTTP/1.1 503 Service Unavailable\r\n\r\n"),
		baseline, "539915-kontra")
	if !got.ControlAlsoFired || got.Count != 0 {
		t.Fatalf("a status change the control reproduces is not a finding: %+v", got)
	}
}

// `dual_response` on the control arm is the same withdrawal: trailing bytes after an ordinary
// request mean this server stacks responses for everyone.
func TestTrailingBytesOnTheControlWithdrawTheClaim(t *testing.T) {
	c := conn(200, "sha-base", "HTTP/1.1 200 OK\r\n\r\nhello")
	c.TrailingRaw = []byte("HTTP/1.1 404 Not Found\r\n\r\n")
	got := ScoreControl(fired(), c, baseline, "539915-kontra")
	if !got.ControlAlsoFired || got.Count != 0 {
		t.Fatalf("trailing bytes on an un-obfuscated request are not evidence: %+v", got)
	}
}

// The row has to carry both outcomes as signal NAMES, or the strongest query on this axis is a
// struct access nobody writes.
func TestTheRowNamesBothControlOutcomes(t *testing.T) {
	survived := RowFromSmuggle(FramingObs{Host: "h", Signals: FramingSignals{
		Count: 2, CanaryReflected: true, ControlUsed: true}})
	if !has(survived.Signals, SigControlSurvived) || !has(survived.Signals, SigCanaryReflected) {
		t.Fatalf("a surviving proof must name both, got %v", survived.Signals)
	}

	killed := RowFromSmuggle(FramingObs{Host: "h", Signals: FramingSignals{
		Count: 0, ControlUsed: true, ControlAlsoFired: true}})
	if !has(killed.Signals, SigControlAlsoFired) {
		t.Fatalf("a withdrawn claim must say so: %v", killed.Signals)
	}
	if has(killed.Signals, SigControlSurvived) {
		t.Fatal("a row cannot both survive and fail its control")
	}
	// `signal_count > 0` is the promote query. This row must not reach `desync_leads`.
	if killed.SignalCount != 0 {
		t.Fatalf("a withdrawn claim would be promoted: signal_count = %d", killed.SignalCount)
	}
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
