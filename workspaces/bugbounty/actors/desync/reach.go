package main

// impact is the GATE: does the signal reproduce, and who does it hurt?
//
// Two questions with two different answers, and only the first one is about the finding:
//
//	mode=reproduce   two arms on the SAME host from THIS machine. The control arm is
//	                 benign/benign/benign; the attack arm is benign/attack/benign. Count how
//	                 often step 3 moved in each. A lead that fires as often with no attack in
//	                 the middle is noise that survived the stability gate.
//
//	mode=poison      send the attack repeatedly. Reports nothing but its own egress address.
//	mode=observe     poll the same endpoint from a DIFFERENT machine and count anomalies
//	                 against its own baseline, while the poisoner works.
//
// WHAT IT NEVER DOES. terminator's investigator answers "does it reproduce" by counting how many
// attack requests it takes to CAPTURE ANOTHER USER'S REQUEST (confirm_run.py:41,
// `count_attack_requests_to_steal`). Nothing here completes a smuggled request or reads anybody
// else's traffic: the smuggled prefix is an incomplete request for a path that does not exist,
// and the only thing measured is whether OUR OWN next request came back wrong. Turning that into
// proof of impact is manual, per-target and authorised.
//
// THE OBSERVER IS THE SEVERITY ORACLE. Three of the corpus's findings were downgraded to
// "IP-locked": exploitation confined to the attacker's own source address, because the front-end
// pools its back-end connections per client IP. The same bug against a globally pooled front-end
// is a critical. The only way to tell is to poison from one address and watch from another, which
// is why the gate runs sessions=1 on a spread fleet — see the workflow.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	kontra "github.com/medmahmoudi26/kontra/sdk/go"

	"github.com/medmahmoudi26/kontra-actors/go/desync/detect"
	"github.com/medmahmoudi26/kontra-actors/go/desync/probe"
	"github.com/medmahmoudi26/kontra-actors/go/desync/technique"
)

// Lead is one promoted observation the gate is asked to settle.
type Lead struct {
	LeadID  string `json:"lead_id"`
	Program string `json:"program"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Scheme  string `json:"scheme"`

	HostHeader  string `json:"host_header,omitempty"`
	SNI         string `json:"sni,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	HeaderBlock string `json:"header_block,omitempty"`

	Class   string `json:"class"`
	Variant string `json:"variant"` // which expanded row fired
}

func (l Lead) target() Target {
	return Target{Program: l.Program, Host: l.Host, Port: l.Port, Scheme: l.Scheme,
		SNI: l.SNI, HostHeader: l.HostHeader, Endpoint: l.Endpoint, HeaderBlock: l.HeaderBlock,
		Class: l.Class}
}

// Verdict is one arm's worth of gate evidence. Deliberately counts, not verdicts: the decision
// (does the interval clear the region of practical equivalence?) is the workflow's, in SQL, over
// however many of these rows accumulated.
type Verdict struct {
	Schema string `json:"schema"`
	TS     string `json:"ts"`
	Axis   string `json:"axis"`
	Mode   string `json:"mode"`

	LeadID  string `json:"lead_id"`
	Program string `json:"program"`
	Host    string `json:"host"`
	Class   string `json:"class"`
	Variant string `json:"variant"`

	// Egress is THIS machine's outbound address. It is the whole point of the observer arm: two
	// rows with the same egress cannot answer the ip-locked question, and the workflow refuses
	// rather than guessing.
	Egress string `json:"egress"`
	Node   string `json:"node,omitempty"`

	Arm      string                 `json:"arm,omitempty"` // control | attack
	N        int                    `json:"n"`             // probes in this arm
	Hits     int                    `json:"hits"`          // how many showed the signal
	Erratic  bool                   `json:"erratic"`
	Baseline detect.FramingBaseline `json:"baseline"`

	// Canary is the smuggled path, derived from the lead so the poisoner and the observer agree
	// on it without talking to each other.
	Canary string `json:"canary,omitempty"`
	// ObserverSawPoison is the severity answer: a response arrived on THIS machine's connection
	// that names the canary the OTHER machine smuggled.
	ObserverSawPoison bool `json:"observer_saw_poison"`
	Anomalies         int  `json:"anomalies"`
	RoundsCompleted   int  `json:"rounds_completed"`

	Error string `json:"error,omitempty"`
}

// canaryFor is derived from the lead id so the poisoner and the observer compute the same value
// with no shared state and no coordination round-trip.
func canaryFor(leadID string) string {
	sum := sha256.Sum256([]byte("kontra-desync-canary/" + leadID))
	return hex.EncodeToString(sum[:6])
}

// egressAddr is this machine's outbound source address.
//
// A UDP "dial" sends no packets — it only asks the kernel which local address it would route
// from — so this costs nothing and, unlike an external echo service, cannot fail closed or leak
// which hosts we are about to scan to a third party.
func egressAddr() string {
	c, err := net.Dial("udp", "1.1.1.1:80")
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

func reach(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
	units := b.Units()
	sem := make(chan struct{}, paramsOf(s).Concurrency)
	var wg sync.WaitGroup
	errs := make([]error, len(units))
	for i, u := range units {
		wg.Add(1)
		go func(i int, u *kontra.Unit) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			errs[i] = reachOne(s, u, ds)
		}(i, u)
	}
	wg.Wait()
	return errors.Join(errs...)
}

func reachOne(s *kontra.Session, in *kontra.Unit, ds *kontra.Dataset) error {
	r, ok := resolvedOf(s)
	if !ok || r.sc == nil {
		return errors.New("scanner not available")
	}
	var l Lead
	if err := in.Into(&l); err != nil {
		return kontra.NonRetryable("bad lead unit: " + err.Error())
	}

	ctx := context.Background()
	h := l.target()
	t := h.target()

	// SAY WHAT THIS IS WORKING ON. `reach` shipped with NO instrumentation of any kind — not one
	// `r.prog.*` reference in this file — so a verify run beat `hosts:0 probes:0` from start to
	// finish. Measured: a 27-minute run against visa's proofs was indistinguishable from a wedged
	// worker, because "doing nothing" and "not counting anything" render identically.
	//
	// Stored BEFORE the baseline rather than after the verdict, because the baseline is the slow
	// part (`Repeat` round-trips against a live host) and a stall during it is precisely when a
	// reader needs the target named.
	r.prog.last.Store(l.Host + orDefault(l.Endpoint, "/"))
	r.prog.program.Store(l.Program)
	// One target finished, however it ends — including the erratic and no-technique exits below,
	// which are outcomes rather than failures to count.
	defer r.prog.hosts.Add(1)
	bind := technique.Bindings{Host: h.hostHeader(), Endpoint: requestTarget(h.Endpoint),
		HeaderBlock: h.HeaderBlock}
	benign := benignRequest(bind, r.cacheBuster())

	base := r.sc.FramingBaselineOf(ctx, t, benign, r.p.Repeat)
	out := Verdict{
		Schema: "impact/v1", TS: detect.NowRFC3339(), Axis: "framing", Mode: r.p.Mode,
		LeadID: l.LeadID, Program: l.Program, Host: l.Host, Class: l.Class, Variant: l.Variant,
		Egress: r.egress, Node: os.Getenv("KONTRA_NODE"), Baseline: base,
		Canary: canaryFor(l.LeadID),
	}
	if !base.Stable {
		out.Erratic = true
		r.prog.erratic.Add(1)
		out.Error = fmt.Sprintf("baseline not reproducible (statuses %v)", base.Statuses)
		ds.Push(asMap(out, "impact/v1"), kontra.Key(l.LeadID+"#"+r.p.Mode+"#"+r.egress))
		return nil
	}

	tech := r.techniqueByVariant(l.Variant)
	if tech == nil {
		out.Error = "no technique with variant " + l.Variant
		ds.Push(asMap(out, "impact/v1"), kontra.Key(l.LeadID+"#"+r.p.Mode+"#"+r.egress))
		return nil
	}

	switch r.p.Mode {
	case "poison":
		out.RoundsCompleted = r.poison(ctx, t, bind, *tech, out.Canary, r.p.Rounds)
	case "observe":
		out.Anomalies, out.ObserverSawPoison, out.RoundsCompleted =
			r.observe(ctx, t, benign, base, out.Canary, r.p.Rounds)
	default: // reproduce
		// TWO ARMS, SAME MACHINE, SAME PACER. The control arm sends a benign request where the
		// attack would be, so anything it fires on is noise that survived the stability gate —
		// which is exactly the thing a single-arm "does it still do it?" check cannot see.
		ctrlHits := r.arm(ctx, t, bind, benign, base, nil, r.p.BatchPerArm)
		atkHits := r.arm(ctx, t, bind, benign, base, tech, r.p.BatchPerArm)

		// BOTH ARMS ARE PROBES SENT. Counting only the attack arm would halve the rate an
		// operator reads off the beat, and the rate is what any time-remaining estimate divides by.
		r.prog.probes.Add(int64(2 * r.p.BatchPerArm))
		r.prog.controls.Add(1)
		// A LEAD THAT SURVIVES ITS CONTROL. The comparison the workflow makes in SQL is
		// `attack.hits > control.hits`; the beat reports the same thing so that "is this finding
		// anything" is answerable while the run is still going, not only after it lands.
		if atkHits > ctrlHits {
			r.prog.signals.Add(1)
			r.prog.found.Add(1)
		} else if atkHits > 0 {
			// Fired, and the control fired at least as often — the claim the gate kills.
			r.prog.withdrawn.Add(1)
		}

		out.Arm, out.N, out.Hits = "control", r.p.BatchPerArm, ctrlHits
		ds.Push(asMap(out, "impact/v1"), kontra.Key(l.LeadID+"#control#"+r.egress))
		out.Arm, out.Hits = "attack", atkHits
		ds.Push(asMap(out, "impact/v1"), kontra.Key(l.LeadID+"#attack#"+r.egress))
		return nil
	}
	ds.Push(asMap(out, "impact/v1"), kontra.Key(l.LeadID+"#"+r.p.Mode+"#"+r.egress))
	return nil
}

// arm runs n probes and counts how many fired. `tech == nil` is the control arm: a benign request
// goes where the attack would, so the middle step is harmless and the sequence is otherwise
// identical — same socket shape, same pipelining, same pacing.
func (r *resolved) arm(ctx context.Context, t probe.Target, bind technique.Bindings,
	benign []byte, base detect.FramingBaseline, tech *technique.Row, n int) int {

	hits := 0
	for i := 0; i < n; i++ {
		mid, canary := benign, ""
		if tech != nil {
			bind.Random = r.cacheBuster()
			raw, _, err := tech.RenderAutoCL(bind)
			if err != nil {
				continue
			}
			mid = raw
			canary = bind.Random + "-kontra"
		}
		c := r.sc.Connection(ctx, t, []probe.Step{
			{Label: "pre", Raw: benign}, {Label: "attack", Raw: mid}, {Label: "post", Raw: benign},
		}, probe.ModePipelined)
		if _, _, _, sig, void := detect.AnalyzeFraming(c, base, canary); !void.Is && sig.Count > 0 {
			hits++
		}
	}
	return hits
}

// poison sends the attack repeatedly with a FIXED canary path, so an observer elsewhere can
// recognise what it is seeing. It still never completes the smuggled request.
func (r *resolved) poison(ctx context.Context, t probe.Target, bind technique.Bindings,
	tech technique.Row, canary string, rounds int) int {

	done := 0
	for i := 0; i < rounds; i++ {
		bind.Random = canary // FIXED, not random: this is the whole channel to the observer
		raw, _, err := tech.RenderAutoCL(bind)
		if err != nil {
			continue
		}
		benign := benignRequest(bind, canary)
		r.sc.Connection(ctx, t, []probe.Step{
			{Label: "pre", Raw: benign}, {Label: "attack", Raw: raw}, {Label: "post", Raw: benign},
		}, probe.ModePipelined)
		done++
	}
	return done
}

// observe polls the endpoint from THIS machine and reports whether it ever received a response
// that names the other machine's canary.
//
// A NEGATIVE IS WEAK AND THE CALLER MUST TREAT IT THAT WAY. Two egress addresses test a NECESSARY
// condition for global impact, not a sufficient one: the observer may simply never land on the
// poisoned connection. That is why the workflow maps "never saw it" to `ip-locked` only after a
// round count it can defend, and to `undetermined` otherwise.
func (r *resolved) observe(ctx context.Context, t probe.Target, benign []byte,
	base detect.FramingBaseline, canary string, rounds int) (anomalies int, saw bool, done int) {

	for i := 0; i < rounds; i++ {
		c := r.sc.Connection(ctx, t, []probe.Step{{Label: "poll", Raw: benign}},
			probe.ModeSequential)
		done++
		if len(c.Steps) == 0 {
			continue
		}
		st := c.Steps[0]
		if st.Status != base.Status || (base.BodySHA != "" && st.BodySHA != "" &&
			st.BodySHA != base.BodySHA) {
			anomalies++
		}
		// The strong form: our response names a path only the OTHER machine ever sent.
		if len(st.RespRaw) > 0 && containsCanary(st.RespRaw, canary) {
			saw = true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return anomalies, saw, done
}

func containsCanary(raw []byte, canary string) bool {
	return len(canary) > 0 && bytesContains(raw, []byte(canary))
}

func bytesContains(h, n []byte) bool {
	if len(n) == 0 || len(h) < len(n) {
		return false
	}
outer:
	for i := 0; i+len(n) <= len(h); i++ {
		for j := range n {
			if h[i+j] != n[j] {
				continue outer
			}
		}
		return true
	}
	return false
}

func (r *resolved) techniqueByVariant(v string) *technique.Row {
	for i := range r.techs {
		if r.techs[i].Variant == v {
			return &r.techs[i]
		}
	}
	return nil
}
