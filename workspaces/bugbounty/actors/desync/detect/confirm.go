package detect

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/medmahmoudi26/kontra-actors/go/desync/inject"
	"github.com/medmahmoudi26/kontra-actors/go/desync/unit"
)

// nowRFC3339 is defined here rather than imported: `main` has its own copy, and the detect
// package must not depend on its caller.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Confirm escalates a surviving fold observation into PROOF, or into nothing.
//
// WHY THIS EXISTS AT ALL. Everything the analyzer produces is a DIFFERENCE: a status moved, a
// body changed, a header appeared. Differences have innocent explanations — and the whole reason
// the matched control is there is that most of them turn out to be a host reacting to an odd
// byte. A difference is a reason to look harder. It is not a finding, and a scanner that promotes
// one to a lead has just handed a human a pile of 404s to read.
//
// This is the step that produces something no innocent path explains. The payload is
// `fold + "Host:" + canary + fold`: it makes the injected bytes become a HEADER in the secondary
// request, so a positive says "I control the Host of its backend request" rather than "the parser
// got upset". THE CANARY IS THE PROOF — a value this scanner minted, in a place only a folded
// newline could have put it, recorded on the row so a reader can reach the same verdict without
// trusting the analyzer.
//
// IT STOPS SHORT OF A SECOND REQUEST LINE, DELIBERATELY. There is no smuggled request here: the
// prefix is never completed, no response queue is poisoned, and no other user's traffic is
// touched. That boundary is the same one the whole engine is built on and it is not negotiable
// for a stronger signal.
//
// THE CONTROL ARM IS THE DISCRIMINATOR, AGAIN. It carries the SAME canary through the SAME point
// with the non-folding twin. Any target that echoes the request reflects the canary in BOTH arms;
// only a target that folded the byte into a real newline turns it into a header the backend acted
// on. This replaced an earlier "the canary must follow ://" heuristic that was tuned to one
// target's error format and silently confirmed nothing anywhere else — a detector that works on
// the host it was written against and nowhere else is worse than none, because it reports clean.
//
// LIVES HERE, NOT IN THE CLI. `cmd/foldscan` drove this by hand and the `fold` Method did not run
// it at all, which is why the pipeline could produce 240 observations and not one proof: the
// evidence existed and the automatic path never asked for it. One implementation, both callers.
func (s *Scanner) Confirm(ctx context.Context, u unit.Exchange, obs Observation) Confirmation {
	canary := "c" + randHex(6) + ".probe.invalid"
	v := obs.Vector
	fold := inject.Render(v, obs.Point.Render)
	ctlFold := inject.RenderControl(v, obs.Point.Render)
	suffix := "Host:" + canary

	raw := inject.Apply(u, obs.Point, fold+suffix+fold)
	sample := s.SampleRaw(ctx, u, raw, "confirm")
	ctl := s.SampleRaw(ctx, u, inject.Apply(u, obs.Point, ctlFold+suffix+ctlFold), "confirm-control")

	c := Confirmation{
		Schema: SchemaConfirmation, TS: nowRFC3339(), UnitID: obs.UnitID, URL: u.URL,
		Point: obs.Point, Vector: v, Probe: printableReq(raw), Canary: canary,
		Sample: sample, ControlSample: ctl,
	}

	can := strings.ToLower(canary)
	body := strings.ToLower(sample.BodyPreview)
	ctlBody := strings.ToLower(ctl.BodyPreview)

	// The weak form, kept because it is data and never evidence on its own: any target that
	// reflects the request path echoes the canary for every probe ever sent.
	c.EchoedAnywhere = strings.Contains(body, can)
	if c.EchoedAnywhere && !strings.Contains(ctlBody, can) {
		c.Reflected, c.ReflectedIn, c.HeaderInject = true, "body-vs-control", true
	}

	// THE STRONGEST FORM, and the one worth reporting: the canary is the AUTHORITY of a URL the
	// SERVER built. No amount of request echoing produces `://<our canary>` — the server had to
	// have taken our bytes as a Host header and then constructed a URL from it. Checked after the
	// body-vs-control arm so it overwrites the weaker attribution rather than losing to it.
	if strings.Contains(body, "://"+can) {
		c.Reflected, c.ReflectedIn, c.HeaderInject = true, "authority", true
	}
	if !c.Reflected {
		for _, h := range sample.HeaderNames {
			if strings.Contains(strings.ToLower(h), can) {
				c.Reflected, c.ReflectedIn, c.HeaderInject = true, "header", true
			}
		}
	}
	// Not reflected, but the confirmation payload restored the baseline status that the plain
	// vector had changed: the Host header landed and was ACTED ON, without the response naming
	// the canary. Recorded as header-injection, never as reflection.
	if !c.Reflected && sample.Status == obs.Base.Status && obs.Signals.StatusChanged {
		c.HeaderInject = true
	}
	return c
}

// randHex is the canary's entropy. crypto/rand rather than math/rand because two concurrent
// probes drawing the same canary would each "confirm" on the other's reflection.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Unreachable in practice; a zeroed canary is still unique per (point, vector) key and
		// still compared against its own control arm, so it degrades to weaker, not wrong.
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

// printableReq renders the sent bytes for a human. The control characters this engine injects are
// the entire point, so they are made visible (`|` for CRLF) rather than stripped — a PoC nobody
// can read off the row is a PoC nobody will reproduce.
func printableReq(raw []byte) string {
	s := strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			return '.'
		}
		return r
	}, string(raw))
	return trunc(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " | "), "\n", " | "), 400)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
