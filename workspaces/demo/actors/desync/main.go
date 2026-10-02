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
	"log"
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

	// MaxSeconds is the WALL-CLOCK budget for one Unit — one host — and it exists because
	// `max_points` is not a bound on time.
	//
	// A point's cost is not fixed. `probe` escalates to the control vector whenever a sample moved,
	// and on a host that answers inconsistently EVERY probe moves, so every one of them doubles.
	// `rate_ms` then paces the whole inflated sequence against that single host. MEASURED on
	// campaign-1790647335-hunt-crlf-shutterfly: 6,000 controls sent and `done: 0` — not one Unit
	// committed in thirty minutes — until the heartbeat expired, the activity retried, and the
	// 90-minute Nexus budget killed the operation with every observation still uncommitted. Three
	// consecutive times, after which the caller abandoned the axis. The splitting axis produced
	// zero rows for the whole campaign while sending thousands of requests.
	//
	// A CAP THAT COMMITS IS WORTH MORE THAN A SWEEP THAT NEVER RETURNS. On expiry the Unit pushes a
	// row saying how far it got and returns nil, exactly as the `max_points` truncation above does:
	// partial coverage that is ANNOUNCED beats total coverage that is discarded. The alternative —
	// what happened — is a scan that says nothing about ground it spent real requests on.
	//
	// 0 means unlimited, which is right for `foldscan` against one target on a laptop and wrong for
	// anything holding a Fleet.
	MaxSeconds   int    `json:"max_seconds"`   // wall-clock budget per unit, 0 = unlimited (default 600)
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

// defaultMaxSeconds: the wall-clock budget one Unit gets when the caller sets no dial.
//
// TEN MINUTES, CHOSEN AGAINST THE TWO DEADLINES ABOVE IT rather than picked for feel. The handler's
// per-Unit liveness bound is the activity's `heartbeatTimeout` — 30 minutes on a hunt — and the
// actor beats ONCE PER COMMITTED UNIT, so a Unit that cannot finish inside that window can never
// prove it is alive. Above this sits the caller's `call_minutes` (90) as the Nexus
// schedule-to-close for the whole Batch: at 30 concurrent Units, a 100-Unit page is four waves, so
// 10 minutes a Unit is 40 minutes of the 90 available and leaves room for the slow tail.
//
// It is deliberately far below the heartbeat rather than just under it. The bound that matters is
// not "does one Unit fit" but "does a WAVE fit", and a budget tuned to the former fails the latter
// on the first host that uses all of it.
const defaultMaxSeconds = 600

// defaultScreen is the two-vector screen the actor uses when a caller names none. It is
// LF-CLASS BY CONSTRUCTION, which is why `Load` has to recognise it by value: a caller who
// changed `class` and left this alone did not ask for an LF screen, and must get neither a
// refusal nor the whole corpus. See the screen block in `loadScanner`.
const defaultScreen = "raw-lf,fold-lf-u070a"

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

	// A SELECTION THAT NAMES NOTHING IS A REFUSAL, and `selectVectors` now says which token did
	// it. This used to read `if len(vecs) == 0` — a guard that could not fire for the case that
	// actually happened, because an unknown class left the list empty and `Generate` reads empty
	// as "both", so the run went out with the default sweep under a class name it never honoured.
	vecs, err := selectVectors(p.Tier, p.Class, p.Only)
	if err != nil {
		return kontra.NonRetryable(err.Error())
	}
	if len(vecs) == 0 {
		return kontra.NonRetryable("no vectors selected: tier/class/only exclude everything")
	}
	// THE SCREEN HAS TO FOLLOW THE CLASS, and the default could not.
	//
	// `defaultScreen` names LF-class vector IDs, so the moment a caller asked for any other class
	// the default selected NOTHING — and both possible behaviours there are wrong. Failing open
	// promotes the screen to the whole corpus (2 vectors to 31, at up to 40 points per exchange,
	// a 15.5x traffic increase nobody asked for). Failing closed refuses a Session over a default
	// the caller never typed. So a default that cannot resolve is DERIVED for the classes actually
	// selected — the raw control probe plus the highest-prior fold, per class — and said out loud.
	//
	// An explicitly-set `screen` that matches nothing stays an error: that is a typo, and the
	// caller is the only one who can fix it.
	screenVecs := vecs
	if s := strings.TrimSpace(p.Screen); s != "" {
		sel, serr := selectVectors(p.Tier, p.Class, s)
		switch {
		case serr == nil:
			screenVecs = sel
		case s == defaultScreen:
			screenVecs = vectors.ScreenOf(vecs)
			if len(screenVecs) == 0 {
				return kontra.NonRetryable("no screening vectors could be derived for class " + p.Class)
			}
			ids := make([]string, 0, len(screenVecs))
			for _, v := range screenVecs {
				ids = append(ids, v.ID)
			}
			log.Printf("screen: the default names lf-class vectors and class is %q — "+
				"screening with the derived set instead: %s", p.Class, strings.Join(ids, ","))
		default:
			return kontra.NonRetryable("screen: " + serr.Error())
		}
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
	return summarizeBatchErrs(errs)
}

// summarizeBatchErrs reduces a Batch's per-Unit errors to ONE bounded error.
//
// WHY NOT `errors.Join`. Join concatenates every message, and these Units fail in CORRELATED ways:
// when the scanner is unavailable, or a class selects nothing, EVERY Unit in the Batch returns the
// same sentence. A Batch of a few hundred exchanges therefore produced a failure of a few hundred
// identical paragraphs, and Temporal refused to record it:
//
//	RunBatch attempt 8 of max 10 — lastFailure: "Failure exceeds size limit."
//
// That is the worst possible failure mode, because the size limit HIDES THE CAUSE: the operator
// sees only that the failure was too big to write down, retries reproduce it exactly, the Batch
// voids, and the axis yields nothing. On campaign-smuggle-1790626548 the CL.0 axis returned 243
// observations while the splitting axis returned zero, and nothing anywhere said why.
//
// So: dedupe (correlated failures collapse to one line plus a count), cap the number of distinct
// messages, and cap the total length. A truncated error still names the cause; an unrecordable one
// names nothing.
func summarizeBatchErrs(errs []error) error {
	const maxKinds, maxLen = 5, 8 << 10

	// The marker `kontra.NonRetryable` puts on its errors. Matched structurally rather than by
	// importing the concrete type, so this keeps working if the SDK moves it.
	type nonRetryable interface{ KontraNonRetryable() }

	seen := map[string]int{}
	order := make([]string, 0, maxKinds)
	failed, terminal := 0, 0
	for _, e := range errs {
		if e == nil {
			continue
		}
		failed++
		var nr nonRetryable
		if errors.As(e, &nr) {
			terminal++
		}
		m := e.Error()
		if _, ok := seen[m]; !ok && len(order) < maxKinds {
			order = append(order, m)
		}
		seen[m]++
	}
	if failed == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d unit(s) failed", failed, len(errs))
	if len(seen) > len(order) {
		fmt.Fprintf(&b, " (%d distinct causes, showing %d)", len(seen), len(order))
	}
	for _, m := range order {
		if n := seen[m]; n > 1 {
			fmt.Fprintf(&b, "\n  [x%d] %s", n, m)
		} else {
			fmt.Fprintf(&b, "\n  %s", m)
		}
		if b.Len() > maxLen {
			b.WriteString("\n  … truncated")
			break
		}
	}

	// A BATCH THAT CANNOT SUCCEED MUST NOT BE RETRIED. `splitOne` already marks malformed input
	// terminal — "a retry re-parses the same bytes and fails identically" — but that marker was
	// lost the moment the per-Unit errors were folded into one, so a Batch of permanently-bad
	// Units burned its whole retry budget re-sending nothing. Measured: every Unit failing on
	// `cannot unmarshal string into Go struct field Message.request.headers` still went round
	// again, against live third-party hosts, for no possible gain.
	//
	// ONLY WHEN EVERY FAILURE IS TERMINAL. One transient failure among them means a retry can
	// still recover that Unit, and the batch deserves its budget.
	if terminal == failed {
		return kontra.NonRetryable(b.String())
	}
	return errors.New(b.String())
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

	// THE SPLITTING AXIS WAS FLYING BLIND. Every counter in the heartbeat — hosts, probes,
	// signals, controls, found — was written only by the framing axis and by `reach`, so a run
	// whose whole job is this Method reported `hosts:0 probes:0` from start to finish. That is
	// indistinguishable from a wedged Worker, and it is the only live signal there is: the Method
	// logs nothing per host and Temporal shows one long activity. Mirrors frame.go exactly.
	r.prog.last.Store(u.URL)
	r.prog.program.Store(r.p.Program)
	defer r.prog.hosts.Add(1)

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
		}, r.p.Program, r.p.Phase, r.egress, r.p.RateMs, r.p.KeepRaw), kontra.Key(u.ID+"#cap"))
	}

	// Stability gate on the UNMODIFIED request. Every signal downstream is a difference from
	// this baseline, so a host that will not reproduce its own response cannot be scanned —
	// every vector would look like it did something.
	base := r.sc.SampleRawN(ctx, u, inject.Apply(u, inject.Point{}, ""), "baseline", r.p.Repeat)

	// ALWAYS A ROW, for the reason the framing axis's baseline is one: it costs `Repeat` real
	// requests whether or not the exchange turns out to be scannable, and pushing it only on the
	// erratic branch meant those requests existed in no row at all on every exchange that worked.
	// The key is per-Unit, so one baseline row lands however many (point, vector) probes follow
	// it, and `sum(requests)` counts it exactly once.
	baseErr := ""
	if !base.Stable {
		baseErr = "baseline not reproducible — refusing to scan (statuses " + fmt.Sprint(base.Statuses) + ")"
	}
	ds.Push(observationUnit(detect.Observation{
		Schema: detect.SchemaObservation, TS: nowRFC3339(), UnitID: u.ID, URL: u.URL,
		Erratic: !base.Stable, Base: base, Error: baseErr,
	}, r.p.Program, r.p.Phase, r.egress, r.p.RateMs, r.p.KeepRaw), kontra.Key(u.ID+"#baseline"))
	if !base.Stable {
		r.prog.erratic.Add(1)
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

	// THE UNIT'S DEADLINE, started AFTER the baseline. The baseline is not optional work a budget
	// may eat into — every signal below is a difference from it, so a Unit that spent its whole
	// allowance establishing one would emit rows with nothing to compare against.
	var deadline time.Time
	if r.p.MaxSeconds > 0 {
		deadline = time.Now().Add(time.Duration(r.p.MaxSeconds) * time.Second)
	}

	for i := cur.Point; i < len(pts); i++ {
		p := pts[i]

		// OUT OF TIME — COMMIT WHAT WE HAVE AND SAY SO.
		//
		// Checked BETWEEN points, never inside one: a point half-probed is a point whose sweep ran
		// and whose control did not, and that is precisely the shape that reads as a finding. The
		// granularity of an honest answer here is the point, so the budget can only be enforced at
		// that boundary — which means it is a floor on the overrun, not a ceiling. One slow point
		// can carry the Unit past its deadline and that is the correct trade.
		//
		// The row is keyed like the `max_points` truncation above and worded the same way, because
		// it is the same claim: this host was covered THIS FAR and no further. Returning nil is the
		// whole point — the Unit commits, the handler heartbeats, and every observation already
		// pushed becomes durable instead of being thrown away by a timeout.
		if !deadline.IsZero() && time.Now().After(deadline) {
			ds.Push(observationUnit(detect.Observation{
				Schema: detect.SchemaObservation, TS: nowRFC3339(), UnitID: u.ID, URL: u.URL,
				Error: fmt.Sprintf("budget: %ds elapsed, probed %d of %d points — %d unprobed",
					r.p.MaxSeconds, i, len(pts), len(pts)-i),
			}, r.p.Program, r.p.Phase, r.egress, r.p.RateMs, r.p.KeepRaw), kontra.Key(u.ID+"#budget"))
			break
		}

		// PHASE A — screen the point with a couple of high-prior vectors. Most points are
		// inert, and sweeping every vector at every point costs points × vectors requests for
		// nothing.
		live := r.p.NoScreen
		if !r.p.NoScreen {
			for _, v := range r.screenVecs {
				obs := r.probe(ctx, u, p, v, base, r.p.ScreenRepeat)
				ds.Push(observationUnit(obs, r.p.Program, r.p.Phase, r.egress, r.p.RateMs, r.p.KeepRaw), kontra.Key(fmt.Sprintf("%s#p%d#screen#%s", u.ID, i, v.ID)))
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
				ds.Push(observationUnit(obs, r.p.Program, r.p.Phase, r.egress, r.p.RateMs, r.p.KeepRaw), kontra.Key(fmt.Sprintf("%s#p%d#sweep#%s", u.ID, i, v.ID)))
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
	r.prog.probes.Add(1)
	obs := detect.Observation{
		Schema: detect.SchemaObservation, TS: nowRFC3339(), UnitID: u.ID, URL: u.URL,
		Point: p, Vector: v, Base: base, Mut: mut, Signals: detect.Analyze(base, mut, v),
	}
	if obs.Signals.Count > 0 && v.Control != "" {
		r.prog.signals.Add(1)
		obs.Control = r.sc.SampleRawN(ctx, u,
			inject.Apply(u, p, inject.RenderControl(v, p.Render)), p.ID+"/control", reps)
		obs.Signals = detect.Analyze(obs.Control, mut, v)
		obs.Signals.ControlUsed = true
		r.prog.controls.Add(1)

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
func observationUnit(o detect.Observation, program, phase, egress string, rateMs int, keepRaw bool) map[string]any {
	r := detect.RowFromSplit(o)
	// `Egress` is stamped here for the same reason `Program` and `Phase` are: it is a fact about
	// the WORKER, not about the observation, so the row builder cannot know it. It was declared on
	// Row and assigned by neither axis, which left a fleet's source-address spread — the entire
	// reason to run twelve machines instead of one — unprovable from the dataset it produced.
	r.Program, r.Phase, r.RateMs, r.Egress = program, phase, rateMs, egress
	return rowUnit(r, keepRaw)
}

// paramsOf reads the run params with the same defaults the CLI uses. A zero value from the
// dispatcher means "unset", not "zero" — a rate of 0 would remove the rate limit entirely,
// which is the one default that must not be reachable by omission.
func paramsOf(s *kontra.Session) Params {
	p := Params{
		Tier: vectors.TierQuick, Class: "lf,cr", MaxPoints: 40,
		Screen: defaultScreen, Repeat: 3, ScreenRepeat: 2,
		MaxSeconds: defaultMaxSeconds,
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
	// NEGATIVE ONLY. Unlike every clamp around it, ZERO IS A REAL ANSWER here — it means "no budget,
	// run to completion", which is what `foldscan` against a single target wants. `json.Unmarshal`
	// leaves the default in place for an ABSENT key, so omission still gets the 10 minutes; only a
	// caller who wrote `"max_seconds": 0` gets the unbounded sweep, and that is a thing they had to
	// ask for. A negative is not an answer at all.
	if p.MaxSeconds < 0 {
		p.MaxSeconds = defaultMaxSeconds
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
	// "split" IS A LEGAL PHASE, and leaving it out was a live correctness bug, not a cosmetic one.
	//
	// `hunt` dispatches phase C with `phase: "split"`; this coerced it to "screen", so every
	// fold-axis row landed stamped `phase='screen'` with `class` in {lf, cr}. `hunt`'s Phase-B
	// reactive query then selects `WHERE phase='screen' AND signal_count > 0`, GROUPs BY `class`,
	// and feeds each group back as a SMUGGLING Target — so a splitting-axis hit became a Target
	// with `Class='lf'`, `techniquesFor` dropped all 3,625 sweep rows on `t.Class != "lf"`, and
	// that host got a "no techniques selected" row and zero probes.
	//
	// Within a single run the ordering hides it, because phase C runs after phase B. Against a
	// PERSISTENT `observations` dataset — which is what a 463-program campaign has — the next run
	// reads the previous one's split rows and silently blanks the smuggling sweep for exactly the
	// hosts that reacted. The workflow's reactive query now also filters `axis = 'smuggle'`, so
	// this is closed on both sides.
	switch p.Phase {
	case "sweep", "split":
	default:
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
//
// IT RETURNS AN ERROR NOW, and that is the point of the change. Both the class list and the
// `only` list used to fail open: an unrecognised class was dropped (leaving an empty list, which
// `Generate` reads as "both", so `class=crlf` quietly ran the default LF/CR sweep), and an
// `only` naming nothing matched nothing. Either way a caller who asked for one thing got another
// and the run said it succeeded. Both are now refusals that name the token.
//
// Matching is case-insensitive on BOTH lists, once, here. The two copies of this function had
// already drifted on exactly that — this one compared vector IDs case-sensitively while
// `cmd/foldscan` upper-cased both sides — so `--only FOLD-LF-U070A` selected the vector under
// one binary and nothing under the other, against a doc promising they were the same scanner.
func selectVectors(tier int, class, only string) ([]vectors.Vector, error) {
	classes, err := vectors.ParseClasses(class)
	if err != nil {
		return nil, err
	}
	all := vectors.Generate(tier, classes)
	if strings.TrimSpace(only) == "" {
		return all, nil
	}
	want := map[string]bool{}
	for _, id := range strings.Split(only, ",") {
		if id = strings.ToUpper(strings.TrimSpace(id)); id != "" {
			want[id] = true
		}
	}
	var out []vectors.Vector
	for _, v := range all {
		if want[strings.ToUpper(v.ID)] || want[strings.ToUpper(v.Encoded)] {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("only=%q selected no vector at tier %d, class %q — nothing would be sent", only, tier, class)
	}
	return out, nil
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
