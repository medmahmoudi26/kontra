// desync — a kontra actor that probes crawled HTTP exchanges for header-injection and
// request-smuggling primitives, and emits OBSERVATIONS rather than verdicts.
//
// The input is a crawled exchange, not a URL, and that is the whole design (see package
// `unit`): the request says where you CAN inject, the response says where you SHOULD — the
// headers a host actually reads are named in `Access-Control-Allow-Headers`, `Vary` and
// `Set-Cookie`, and no static wordlist knows that this host takes `X-Tenant-Id`. So this
// actor's natural upstream is a crawler: `webcrawl` emits one object per request and per
// response, and a graph edge feeds them here.
//
// WHAT IT EMITS. One `detect.Observation` per (point, vector) probe, emitted on its Unit the
// moment it exists. Classification into CL.0 / splitting / response-splitting happens
// LATER, in SQL over the dataset — re-scanning 37k hosts costs hours and goodwill, re-querying
// a dataset costs nothing. Every column exists so detection can improve without touching the
// network again, which only works if the scanner never throws the raw bytes away.
//
// THE COST MODEL IS THE DESIGN. A full sweep is points × vectors requests per unit, which is
// why the Method screens each point with a couple of high-prior vectors first and only sweeps the
// ones that reacted. `max_points` bounds the rest — and a truncated enumeration is EMITTED as a
// `capped` observation, never dropped silently, because a bounded sweep that reads as complete
// is how a scan reports "clean" for ground it never covered.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	kontra "github.com/medmahmoudi26/kontra/sdk/go"
	// The actor runtime, imported for its side effect: it registers the Temporal actor
	// host behind a.Serve(). The SDK declares the seam and never imports across it —
	// runtime/ -> sdk/ is one-way — so this line is what puts a host in the binary.
	_ "github.com/medmahmoudi26/kontra/runtime/go"

	"github.com/medmahmoudi26/kontra-actors/go/desync/detect"
	"github.com/medmahmoudi26/kontra-actors/go/desync/inject"
	"github.com/medmahmoudi26/kontra-actors/go/desync/technique"
	"github.com/medmahmoudi26/kontra-actors/go/desync/unit"
	"github.com/medmahmoudi26/kontra-actors/go/desync/vectors"
)

// Params are the run-wide dials the dispatcher and the graph UI see. They mirror the
// `foldscan validator` flags, because the CLI and the actor must not drift into two different
// scanners — the CLI is how a vector gets debugged before a fleet runs it.
type Params struct {
	Tier         int    `json:"tier"`          // 1=quick 2=+3-byte 3=+astral (default 1)
	Class        string `json:"class"`         // control chars to target (default "lf,cr")
	Only         string `json:"only"`          // restrict to these vector IDs/encodings
	Points       string `json:"points"`        // restrict to these point IDs (substring match)
	MaxPoints    int    `json:"max_points"`    // cap on injection points per unit (default 40)
	Screen       string `json:"screen"`        // vectors used to screen each point
	NoScreen     bool   `json:"no_screen"`     // skip screening; sweep every vector at every point
	Repeat       int    `json:"repeat"`        // repeats per sample; >=2 enables the contamination oracle
	ScreenRepeat int    `json:"screen_repeat"` // repeats per screening sample
	RateMs       int    `json:"rate_ms"`       // minimum gap between requests to ONE host (default 1000)
	TimeoutMs    int    `json:"timeout_ms"`    // per-request timeout (default 10000)

	// Backoff lets a host widen its OWN gap when it answers 429/503/5xx or resets the socket,
	// and `backoff_max_ms` caps how far. This is what makes a low `rate_ms` askable: the
	// configured rate says what we are willing to send, not what the far side can take.
	// Pointer-typed because `false` and "unset" are different answers and a bool cannot tell
	// them apart — unset means ON.
	Backoff      *bool `json:"backoff,omitempty"`
	BackoffMaxMs int   `json:"backoff_max_ms"`

	// Concurrency is Units in flight, honoured by the Method's own loop — the framework stopped
	// owning the window (ADR 0023 §18). It does NOT relax the rate limit: `rate_ms` is per-host
	// and the pacer is shared across every Unit, so raising this widens the sweep across
	// DIFFERENT hosts rather than hitting one harder.
	Concurrency int `json:"concurrency"`

	// Phase selects the framing axis's two-phase screen: "screen" sends only tier-1 rows (the
	// ones that regenerate a paid finding), "sweep" sends the rest of a reacting class.
	Phase string `json:"phase"`

	// Program partitions every row. The fold axis takes a crawled Unit, which knows its
	// URL but not whose scope it came from, so the caller states it once per run.
	Program string `json:"program"`

	// MaxTechniques bounds one sweep. 0 is uncapped.
	MaxTechniques int `json:"max_techniques"`

	// KeepRaw turns off the clean-row elision in `detect.Row.Elide` and writes the full captured
	// stream on every row. OFF by default, and the default is the one you want for a campaign:
	// 92% of the first campaign's bytes were raw response bodies on rows where nothing fired,
	// which is what made the sweep phase — the phase that actually finds things — too expensive
	// to run. Ask for it on a deep dive against a handful of hosts, where the whole stream is
	// the point and the row count is small enough that it costs nothing.
	KeepRaw bool `json:"keep_raw"`

	// Gate dials. Mode selects which half of the severity oracle this call is.
	Mode        string `json:"mode"`          // reproduce | poison | observe
	BatchPerArm int    `json:"batch_per_arm"` // probes per arm, per call
	Rounds      int    `json:"rounds"`        // poison/observe rounds
}

// defaultConcurrency: Units probed at once when the caller sets no dial.
const defaultConcurrency = 8

func main() {
	a := kontra.New()
	a.Load(loadScanner)
	// HTTP REQUEST SPLITTING. Take a request the crawl really made, put one Unicode codepoint
	// where the server will read it, and see whether the request line comes apart.
	a.Method("split", split,
		kontra.Does("HTTP request splitting. Smuggle a Unicode codepoint whose low byte is 0x0A "+
			"into a crawled request; if the server narrows it to a newline, the request line "+
			"splits and the attacker writes the next header — proven when an injected Host "+
			"comes back as the authority of a URL the server built."),
		kontra.Takes(unit.Exchange{}), kontra.Emits(detect.Row{}))

	// HTTP REQUEST SMUGGLING. Send a request whose length two parsers read differently, then ask
	// what the NEXT request on that socket gets back.
	a.Method("smuggle", smuggle,
		kontra.Does("HTTP request smuggling. Send a request whose body length the front-end and "+
			"back-end disagree about, so bytes the front-end forwards are read by the back-end "+
			"as the start of somebody else's request — proven when a path only this scanner "+
			"knows comes back on the next request."),
		kontra.Takes(Target{}), kontra.Emits(detect.Row{}))

	a.Method("reach", reach,
		kontra.Does("Who a lead actually hurts. Re-runs it against a control arm to check the "+
			"signal reproduces, then from a second source address to tell an attacker-only bug "+
			"from one that reaches other users."),
		kontra.Takes(Lead{}), kontra.Emits(Verdict{}))

	// TYPED, because the Actors page renders `Takes` as the method's input shape and a
	// `map[string]any` renders as "declares no fields" — on the one method whose entire job is to
	// describe what this actor sends. A shard is its input; saying so is free.
	a.Method("corpus", corpus,
		kontra.Does("Publish every payload this actor version can send — both axes, splitting and "+
			"smuggling — so a finding is traceable to the exact bytes and to the report the "+
			"technique came from. Sends nothing; takes a shard so the corpus travels in pieces."),
		kontra.Takes(CorpusShard{}), kontra.Emits(technique.Row{}))
	a.Healthcheck(scannerAlive)
	a.Params(Params{})
	a.Serve()
}

// resolved is the run's configuration after defaults, built ONCE in Load. The vector sets are
// generated here too: `vectors.Generate` at tier 3 is thousands of vectors and regenerating it
// per unit would dominate the CPU of a scan whose cost is supposed to be network-bound.
type resolved struct {
	sc         *detect.Scanner
	vecs       []vectors.Vector
	screenVecs []vectors.Vector
	// techs is the whole expanded framing corpus and tier1 is the screen set. Both are built
	// ONCE in Load for the same reason the vector sets are: 3,630 rows regenerated per Unit
	// would dominate the CPU of a scan whose cost is supposed to be network-bound.
	techs []technique.Row
	tier1 []technique.Row
	// egress is this Machine's outbound address, resolved ONCE. It is what makes the
	// severity oracle answerable: two rows with the same egress cannot distinguish
	// ip-locked from global, and the caller refuses rather than guessing.
	egress string
	p      Params

	// LIVE COUNTERS, so a beat says what this Worker is DOING and not only what it was built
	// with.
	//
	// `scannerAlive` used to return corpus sizes and the egress address — all fixed at load —
	// so every beat printed the identical line:
	//
	//	progress: map[egress:172.20.0.20 screen:2 techniques:3630 tier1:5 vectors:31]
	//
	// Against a sweep that runs for an hour that is indistinguishable from a hang, and it was
	// the only signal available: the Method logs nothing per host, and Temporal shows one
	// in-flight Nexus operation whether the Worker is probing or wedged.
	//
	// Atomic rather than mutex-guarded because the Method runs `concurrency` hosts at once and
	// a beat must never contend with the thing it is reporting on.
	prog progress
}

// progress is what a beat reports. Counters are cumulative for the SESSION, which is the unit an
// operator watching `kontra workers` actually sees.
type progress struct {
	hosts     atomic.Int64 // targets finished (a target is one host x one endpoint)
	probes    atomic.Int64 // techniques sent
	signals   atomic.Int64 // probes whose oracle fired before the control ran
	controls  atomic.Int64 // control arms run — one per firing probe
	withdrawn atomic.Int64 // claims the control reproduced, and therefore killed
	erratic   atomic.Int64 // hosts refused because they would not reproduce their own baseline
	last      atomic.Value // string: the most recent target, so a stall names the host it stalled on
	// program is the BOUNTY PROGRAM the current target belongs to.
	//
	// A beat that says `hosts:394 probes:1720` describes the machine; it does not answer the
	// question an operator running a 454-program campaign actually asks, which is "what is this
	// worker on right now". `last` gives the host and path, and without the program beside it a
	// reader still has to go look up which of 454 scopes `www.visamiddleeast.com` came from.
	//
	// Every input struct this actor takes already carries it (`Lead.Program`, `Target.Program`),
	// so this is threading a field that was always present through to the one place it is read.
	program atomic.Value // string
	// found is proofs promoted so far THIS SESSION — the only number in the beat that answers
	// "is this finding anything", rather than "is this still alive".
	found atomic.Int64
}

const stateKey = "desync.resolved"

// loadScanner builds the shared scanner and vector sets.
//
// ONE scanner for the whole session, shared by every concurrent Unit, because `rate_ms` is a
// per-HOST minimum gap and crawler-fed input puts many units on the same host. A scanner per
// scanner-per-Unit would pace each unit independently and hit an origin N times harder than the operator
// asked for. `detect.Scanner` is safe for concurrent use for exactly this reason.
func loadScanner(s *kontra.Session) error {
	p := paramsOf(s)

	o := detect.DefaultOptions()
	o.Rate = time.Duration(p.RateMs) * time.Millisecond
	o.Timeout = time.Duration(p.TimeoutMs) * time.Millisecond
	o.Repeats = p.Repeat
	o.Backoff = p.Backoff == nil || *p.Backoff
	if p.BackoffMaxMs > 0 {
		o.MaxBackoff = time.Duration(p.BackoffMaxMs) * time.Millisecond
	}

	vecs := selectVectors(p.Tier, p.Class, p.Only)
	if len(vecs) == 0 {
		return kontra.NonRetryable("no vectors selected: tier/class/only exclude everything")
	}
	screenVecs := selectVectors(p.Tier, p.Class, p.Screen)
	if len(screenVecs) == 0 {
		screenVecs = vecs
	}

	all, tier1 := expandCorpus()
	if len(tier1) == 0 {
		return kontra.NonRetryable("framing corpus has no tier-1 rows: the screen would send nothing")
	}

	s.Set(stateKey, &resolved{sc: detect.New(o), vecs: vecs, screenVecs: screenVecs,
		techs: all, tier1: tier1, egress: egressAddr(), p: p})
	return nil
}

// scannerAlive is the liveness authority. There is no long-lived socket or engine here, so the
// only way the resource is "dead" is if Load never completed — which a reload fixes. The Method
// never decides this itself; it returns the error and the host probes.
func scannerAlive(s *kontra.Session) (any, error) {
	r, ok := resolvedOf(s)
	if !ok || r.sc == nil {
		return nil, errors.New("scanner not loaded")
	}
	// WHAT IT IS DOING FIRST, what it was built with after. A reader scanning a wall of beats is
	// looking for the numbers that MOVE; the corpus sizes are context and never change.
	out := map[string]any{
		"hosts":      r.prog.hosts.Load(),
		"probes":     r.prog.probes.Load(),
		"signals":    r.prog.signals.Load(),
		"controls":   r.prog.controls.Load(),
		"withdrawn":  r.prog.withdrawn.Load(),
		"erratic":    r.prog.erratic.Load(),
		"techniques": len(r.techs), "tier1": len(r.tier1),
		"vectors": len(r.vecs), "screen": len(r.screenVecs),
		"egress": r.egress,
	}
	// The host a stall stalled ON. Absent until the first target, so an idle Worker does not
	// claim to be working on something.
	if v, ok := r.prog.last.Load().(string); ok && v != "" {
		out["at"] = v
	}
	// WHICH SCOPE `at` BELONGS TO. Same absent-until-known rule: a worker that has not taken a
	// target yet must not name a program, because "idle" and "working on visa" have to stay
	// distinguishable in a wall of beats.
	if v, ok := r.prog.program.Load().(string); ok && v != "" {
		out["program"] = v
	}
	out["found"] = r.prog.found.Load()
	return out, nil
}

// scan is the Method: the author's loop over the Batch, `concurrency` exchanges in flight. The
// shared scanner paces every one of them against the same per-host rate limit, which is why the
// concurrency widens the sweep across hosts rather than hitting one harder.
func split(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
	units := b.Units() // taken all at once, because these Units run concurrently
	sem := make(chan struct{}, paramsOf(s).Concurrency)
	var wg sync.WaitGroup
	errs := make([]error, len(units))
	for i, u := range units {
		wg.Add(1)
		go func(i int, u *kontra.Unit) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			errs[i] = splitOne(s, u, ds)
		}(i, u)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// scanOne probes ONE crawled exchange and pushes every observation to the output Dataset.
func splitOne(s *kontra.Session, in *kontra.Unit, ds *kontra.Dataset) error {
	r, ok := resolvedOf(s)
	if !ok || r.sc == nil {
		return errors.New("scanner not available")
	}

	u, err := decodeUnit(in)
	if err != nil {
		// Malformed input is TERMINAL: a retry re-parses the same bytes and fails identically,
		// so isolate the Unit instead of burning the batch's retry budget on it.
		return kontra.NonRetryable("bad input unit: " + err.Error())
	}

	ctx := context.Background()
	pts := inject.Enumerate(u, inject.DefaultOptions())
	if r.p.Points != "" {
		pts = filterPoints(pts, r.p.Points)
	}

	// A truncated enumeration is REPORTED, not dropped. A bounded sweep that reads as complete
	// is how a scan says "clean" about ground it never covered.
	if len(pts) > r.p.MaxPoints {
		dropped := len(pts) - r.p.MaxPoints
		pts = pts[:r.p.MaxPoints]
		// Under Units() there is no current Unit, so every observation rides the Batch tail under
		// an explicit key (ADR 0028). The keys are intrinsic — UnitID plus what the observation is
		// about — so a resumed re-probe re-pushes onto the same slot rather than a second row.
		ds.Push(observationUnit(detect.Observation{
			Schema: detect.SchemaObservation, TS: nowRFC3339(), UnitID: u.ID, URL: u.URL,
			Error: fmt.Sprintf("capped: %d points enumerated, probing first %d — %d dropped",
				len(pts)+dropped, r.p.MaxPoints, dropped),
		}, r.p.Program, r.p.Phase, r.p.RateMs, r.p.KeepRaw), kontra.Key(u.ID+"#cap"))
	}

	// Stability gate on the UNMODIFIED request. Every signal downstream is a difference from
	// this baseline, so a host that will not reproduce its own response cannot be scanned —
	// every vector would look like it did something.
	base := r.sc.SampleRawN(ctx, u, inject.Apply(u, inject.Point{}, ""), "baseline", r.p.Repeat)
	if !base.Stable {
		ds.Push(observationUnit(detect.Observation{
			Schema: detect.SchemaObservation, TS: nowRFC3339(), UnitID: u.ID, URL: u.URL,
			Erratic: true, Base: base,
			Error: "baseline not reproducible — refusing to scan (statuses " + fmt.Sprint(base.Statuses) + ")",
		}, r.p.Program, r.p.Phase, r.p.RateMs, r.p.KeepRaw), kontra.Key(u.ID+"#baseline"))
		return nil
	}

	// RESUMABLE PER UNIT, but only when emits are DURABLE. The cursor skips points already
	// probed on an earlier attempt, which is correct precisely because each observation was
	// emitted as its own durable blob as it was produced. Without a blob store (inline
	// dev/test mode) emits are not durable until the Unit commits, so a cursor that skipped
	// them would silently lose them — disable it and re-probe atomically instead.
	//
	// This is worth more here than in most actors: a re-probe is not just slow, it is another
	// N requests at someone else's origin.
	resumable := s.EmitDurable()
	var cur struct {
		Point int `json:"point"`
	}
	if resumable {
		_, _ = in.State().Get("point", &cur)
	}

	for i := cur.Point; i < len(pts); i++ {
		p := pts[i]

		// PHASE A — screen the point with a couple of high-prior vectors. Most points are
		// inert, and sweeping every vector at every point costs points × vectors requests for
		// nothing.
		live := r.p.NoScreen
		if !r.p.NoScreen {
			for _, v := range r.screenVecs {
				obs := r.probe(ctx, u, p, v, base, r.p.ScreenRepeat)
				ds.Push(observationUnit(obs, r.p.Program, r.p.Phase, r.p.RateMs, r.p.KeepRaw), kontra.Key(fmt.Sprintf("%s#p%d#screen#%s", u.ID, i, v.ID)))
				if obs.Signals.Count > 0 {
					live = true
				}
			}
		}

		// PHASE B — a point that reacted earns the full vector sweep.
		if live {
			for _, v := range r.vecs {
				if !r.p.NoScreen && containsVector(r.screenVecs, v.ID) {
					continue // already probed in phase A
				}
				obs := r.probe(ctx, u, p, v, base, r.p.Repeat)
				ds.Push(observationUnit(obs, r.p.Program, r.p.Phase, r.p.RateMs, r.p.KeepRaw), kontra.Key(fmt.Sprintf("%s#p%d#sweep#%s", u.ID, i, v.ID)))
			}
		}

		if resumable {
			cur.Point = i + 1
			_ = in.State().Set("point", cur) // checkpoint only when emits are durable
		}
	}

	// last_url: session_state (tier 2) retired with ADR 0023 §19 — the Session's own in-memory
	// field serves (every reader is in this process; host death fails the scope). LWW, so
	// re-setting on a re-run is resume-safe.
	s.Set("last_url", u.URL)
	return nil // the observations were STREAMED via push
}

// probe runs one (point, vector) and escalates to the control vector when something moved.
//
// The control is the discriminator that makes a signal mean anything: it renders the SAME
// vector with an inert codepoint, so a difference that survives it is the injection and not the
// host reacting to any odd byte at all.
func (r *resolved) probe(ctx context.Context, u unit.Exchange, p inject.Point, v vectors.Vector,
	base detect.Sample, reps int) detect.Observation {

	mut := r.sc.SampleRawN(ctx, u, inject.Apply(u, p, inject.Render(v, p.Render)), p.ID+"/"+v.ID, reps)
	obs := detect.Observation{
		Schema: detect.SchemaObservation, TS: nowRFC3339(), UnitID: u.ID, URL: u.URL,
		Point: p, Vector: v, Base: base, Mut: mut, Signals: detect.Analyze(base, mut, v),
	}
	if obs.Signals.Count > 0 && v.Control != "" {
		obs.Control = r.sc.SampleRawN(ctx, u,
			inject.Apply(u, p, inject.RenderControl(v, p.Render)), p.ID+"/control", reps)
		obs.Signals = detect.Analyze(obs.Control, mut, v)
		obs.Signals.ControlUsed = true

		// ESCALATE TO PROOF, and only from here. A difference that survived its matched control
		// is the only thing worth two more requests: it has already outlived the explanation that
		// covers most of them (the host reacting to any odd byte), and until it is confirmed it
		// is still just a difference. Everything that failed the control stops at this line.
		//
		// This is the step the Method was missing. `Confirm` existed, `authority_injected` was
		// declared, and neither was ever reached from the automatic path — so the pipeline could
		// scan a whole program and emit observations that no query could ever promote to a lead.
		if obs.Signals.Count > 0 {
			c := r.sc.Confirm(ctx, u, obs)
			obs.Confirm = &c
		}
	}
	return obs
}

// ---------------------------------------------------------------- plumbing

// decodeUnit turns the wire Unit (a plain map) into the typed input contract. It round-trips
// through JSON rather than reaching for individual fields so the whole nested request/response
// shape arrives intact — which is the point of taking an exchange rather than a URL.
func decodeUnit(in *kontra.Unit) (unit.Exchange, error) {
	var u unit.Exchange
	b, err := json.Marshal(in.Value)
	if err != nil {
		return u, err
	}
	if err := json.Unmarshal(b, &u); err != nil {
		return u, err
	}
	if err := u.Normalize(); err != nil {
		return u, err
	}
	return u, nil
}

// observationUnit is the reverse: the typed observation onto the wire.
// observationUnit lifts a fold observation into the SHARED row shape (detect/row.go), so both
// axes land in `observations` with one schema. The fold detail is not lost — it rides in the
// row's `fold` block — but every cross-axis question is one query instead of a UNION.
func observationUnit(o detect.Observation, program, phase string, rateMs int, keepRaw bool) map[string]any {
	r := detect.RowFromSplit(o)
	r.Program, r.Phase, r.RateMs = program, phase, rateMs
	return rowUnit(r, keepRaw)
}

// paramsOf reads the run params with the same defaults the CLI uses. A zero value from the
// dispatcher means "unset", not "zero" — a rate of 0 would remove the rate limit entirely,
// which is the one default that must not be reachable by omission.
func paramsOf(s *kontra.Session) Params {
	p := Params{
		Tier: vectors.TierQuick, Class: "lf,cr", MaxPoints: 40,
		Screen: "raw-lf,fold-lf-u070a", Repeat: 3, ScreenRepeat: 2,
		RateMs: 1000, TimeoutMs: 10000, Concurrency: defaultConcurrency,
		Phase: "screen", Mode: "reproduce", BatchPerArm: 20, Rounds: 20,
	}
	raw, err := json.Marshal(s.Params)
	if err != nil {
		return p
	}
	_ = json.Unmarshal(raw, &p) // absent keys leave the defaults in place
	if p.Tier < 1 {
		p.Tier = vectors.TierQuick
	}
	if p.MaxPoints < 1 {
		p.MaxPoints = 40
	}
	if p.Repeat < 1 {
		p.Repeat = 3
	}
	if p.Concurrency < 1 {
		p.Concurrency = defaultConcurrency
	}
	if p.ScreenRepeat < 1 {
		p.ScreenRepeat = 2
	}
	if p.RateMs < 1 {
		p.RateMs = 1000
	}
	if p.TimeoutMs < 1 {
		p.TimeoutMs = 10000
	}
	if p.Phase != "sweep" {
		p.Phase = "screen"
	}
	switch p.Mode {
	case "poison", "observe", "reproduce":
	default:
		p.Mode = "reproduce"
	}
	if p.BatchPerArm < 1 {
		p.BatchPerArm = 20
	}
	if p.Rounds < 1 {
		p.Rounds = 20
	}
	return p
}

func resolvedOf(s *kontra.Session) (*resolved, bool) {
	v, ok := s.Get(stateKey)
	if !ok {
		return nil, false
	}
	r, ok := v.(*resolved)
	return r, ok
}

// selectVectors mirrors the CLI's selection so a vector debugged with `foldscan` is the same
// vector the fleet sends.
func selectVectors(tier int, class, only string) []vectors.Vector {
	var classes []vectors.Class
	for _, c := range strings.Split(class, ",") {
		switch strings.TrimSpace(c) {
		case "lf":
			classes = append(classes, vectors.ClassLF)
		case "cr":
			classes = append(classes, vectors.ClassCR)
		}
	}
	all := vectors.Generate(tier, classes)
	if only == "" {
		return all
	}
	want := map[string]bool{}
	for _, id := range strings.Split(only, ",") {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}
	var out []vectors.Vector
	for _, v := range all {
		if want[v.ID] || want[v.Encoded] {
			out = append(out, v)
		}
	}
	return out
}

func filterPoints(pts []inject.Point, filter string) []inject.Point {
	var out []inject.Point
	for _, p := range pts {
		if strings.Contains(p.ID, filter) {
			out = append(out, p)
		}
	}
	return out
}

func containsVector(vs []vectors.Vector, id string) bool {
	for _, v := range vs {
		if v.ID == id {
			return true
		}
	}
	return false
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }
