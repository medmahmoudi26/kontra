package main

// smuggle is the FRAMING-axis Method, and `corpus` is how its payload set becomes queryable.
//
// It is the sibling of `scan`, and the three differences are the whole design:
//
//	                scan (injection)               smuggle (framing)
//	input Unit      a crawled exchange             a host
//	payload         a vector spliced INTO it       a whole rendered request
//	oracle          matched control, in the body   the socket, on the NEXT request
//
// They share the scanner, and that is not incidental. `rate_ms` is a per-HOST minimum gap and
// both axes point at the same origins; a second scanner would double the rate an operator asked
// for, silently, and only under mixed load.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	kontra "github.com/medmahmoudi26/kontra/sdk/go"

	"github.com/medmahmoudi26/kontra-actors/go/desync/detect"
	"github.com/medmahmoudi26/kontra-actors/go/desync/probe"
	"github.com/medmahmoudi26/kontra-actors/go/desync/technique"
	"github.com/medmahmoudi26/kontra-actors/go/desync/vectors"
)

// Host is one target. Deliberately NOT a URL and NOT a crawled exchange: the framing axis renders
// its own request, so all it needs is where to send it and what the crawl learned about getting
// through the edge.
type Target struct {
	Program string `json:"program"`
	Host    string `json:"host"` // 'api.example.com' or a bare '203.0.113.10'
	Port    int    `json:"port"`
	Scheme  string `json:"scheme"`

	// SNI and HostHeader exist for the bare-IP case, which is not an edge case in this corpus —
	// #3475402 is a $3000 critical that only reproduces against backend IPs with the original
	// hostname still in the Host header.
	SNI        string `json:"sni,omitempty"`
	HostHeader string `json:"host_header,omitempty"`

	// Endpoint and HeaderBlock are SLOT BINDINGS from the crawl, not injection points. A bare
	// `POST / HTTP/1.1` may route to a different backend than the crawl reached.
	Endpoint    string `json:"endpoint,omitempty"`
	HeaderBlock string `json:"header_block,omitempty"`

	// Class narrows the sweep to the class this host already reacted to in the screen. Empty in
	// the screen phase, where every tier-1 representative is sent.
	Class string `json:"class,omitempty"`
}

func (h Target) target() probe.Target {
	t := probe.Target{Host: h.Host, Port: h.Port, Scheme: h.Scheme, SNI: h.SNI}
	if t.Port == 0 {
		if t.Scheme == "http" {
			t.Port = 80
		} else {
			t.Port = 443
		}
	}
	if t.Scheme == "" {
		t.Scheme = "https"
	}
	return t
}

// hostHeader is what goes in the request. For a bare IP this is the ORIGINAL hostname, because
// the vhost behind the IP routes on it — sending the IP would land on a default vhost that is not
// the application under test.
func (h Target) hostHeader() string {
	if h.HostHeader != "" {
		return h.HostHeader
	}
	return h.Host
}

// smuggle is the Method: the author's loop over the Batch, `concurrency` hosts in flight.
func smuggle(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
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
			errs[i] = smuggleOne(s, u, ds)
		}(i, u)
	}
	wg.Wait()
	return errors.Join(errs...)
}

func smuggleOne(s *kontra.Session, in *kontra.Unit, ds *kontra.Dataset) error {
	r, ok := resolvedOf(s)
	if !ok || r.sc == nil {
		return errors.New("scanner not available")
	}
	var h Target
	if err := in.Into(&h); err != nil {
		// Malformed input is TERMINAL: a retry re-parses the same bytes and fails identically.
		return kontra.NonRetryable("bad host unit: " + err.Error())
	}
	if h.Host == "" {
		return kontra.NonRetryable("host unit has no host")
	}

	ctx := context.Background()
	t := h.target()
	// NAMED BEFORE THE FIRST BYTE, so a beat during a stall says which target it stalled on
	// rather than which one it last finished.
	r.prog.last.Store(h.Host + orDefault(h.Endpoint, "/"))
	r.prog.program.Store(h.Program)
	defer r.prog.hosts.Add(1)
	bind := technique.Bindings{
		Host:        h.hostHeader(),
		Endpoint:    requestTarget(h.Endpoint),
		HeaderBlock: h.HeaderBlock,
	}

	// THE STABILITY GATE, FIRST AND ALWAYS. Every signal below is "step 3 differed from the
	// baseline" — which a host that differs from itself satisfies for free. A refusal is an
	// OBSERVATION, not a silent skip: a host recorded as clean when it was never scannable is
	// the same lie as a bounded sweep that reads as complete.
	benign := benignRequest(bind, r.cacheBuster())
	base := r.sc.FramingBaselineOf(ctx, t, benign, r.p.Repeat)

	// THE BASELINE IS ALWAYS A ROW NOW, not only when it fails.
	//
	// It costs `Repeat` real requests against a real origin whether or not the host turns out to
	// be scannable, and pushing it only on the unstable branch meant that on every host that
	// WORKED — the overwhelming majority — those requests existed in no row at all. They could
	// not be recovered from the technique rows either: `Baseline: base` is copied onto every one
	// of them, so summing it multiplies the true cost by the technique count (×5 in the screen,
	// ×3,625 in the sweep).
	//
	// The dedup key is intrinsic and per (host, endpoint), so one baseline row lands per target
	// however many techniques follow it, and `sum(requests)` counts it exactly once.
	baseErr := ""
	if !base.Stable {
		baseErr = fmt.Sprintf("baseline not reproducible — refusing to scan (statuses %v)",
			base.Statuses)
	}
	ds.Push(rowUnit(detect.RowFromSmuggle(detect.FramingObs{
		Schema: detect.SchemaFraming, TS: detect.NowRFC3339(), Axis: string(technique.AxisSmuggle),
		Program: h.Program, Host: h.Host, Port: t.Port, Scheme: t.Scheme, Phase: r.p.Phase,
		Endpoint: bind.Endpoint,
		Erratic:  !base.Stable, Baseline: base, RateMs: r.p.RateMs,
		Requests: base.Requests, Egress: r.egress,
		Error:    baseErr,
	}), r.p.KeepRaw), kontra.Key(h.Host+"#"+bind.Endpoint+"#framing#baseline"))
	if !base.Stable {
		r.prog.erratic.Add(1)
		return nil
	}

	techs := r.techniquesFor(r.p.Phase, h.Class)
	if len(techs) == 0 {
		ds.Push(rowUnit(detect.RowFromSmuggle(detect.FramingObs{
			Schema: detect.SchemaFraming, TS: detect.NowRFC3339(), Axis: string(technique.AxisSmuggle),
			Program: h.Program, Host: h.Host, Port: t.Port, Scheme: t.Scheme, Phase: r.p.Phase,
			Endpoint: bind.Endpoint,
			Baseline: base, RateMs: r.p.RateMs,
			Error: "no techniques selected for phase " + r.p.Phase + " class " + h.Class,
		}), r.p.KeepRaw), kontra.Key(h.Host+"#"+bind.Endpoint+"#framing#empty"))
		return nil
	}

	for _, tech := range techs {
		// Fresh per technique for two reasons: a CDN caching our probe would make every later
		// probe read a cached answer as if it were live, AND this value is the canary — the
		// smuggled path that makes a reflection self-proving.
		bind.Random = r.cacheBuster()
		canary := bind.Random + "-kontra"
		raw, _, err := tech.RenderAutoCL(bind)
		if err != nil {
			ds.Push(rowUnit(detect.RowFromSmuggle(detect.FramingObs{
				Schema: detect.SchemaFraming, TS: detect.NowRFC3339(), Axis: string(technique.AxisSmuggle),
				Program: h.Program, Host: h.Host, Phase: r.p.Phase, Endpoint: bind.Endpoint,
				TechniqueID: tech.RequestHex[:16], Variant: tech.Variant,
				Error: "render: " + err.Error(),
			}), r.p.KeepRaw), kontra.Key(h.Host+"#"+bind.Endpoint+"#framing#render#"+tech.Variant))
			continue
		}

		// THE ORACLE. Three steps, one socket, all written before any is read. ModePipelined is
		// not an optimisation — the smuggled prefix has to still be in the server's buffer when
		// step 3 lands, and a write-read-write-read cycle gives the server time to discard it.
		conn := r.sc.Connection(ctx, t, []probe.Step{
			{Label: "pre", Raw: benign},
			{Label: "attack", Raw: raw},
			{Label: "post", Raw: benign},
		}, probe.ModePipelined)

		pre, atk, post, sig, void := detect.AnalyzeFraming(conn, base, canary)

		// ── THE MATCHED CONTROL, ONLY WHERE IT CHANGES THE ANSWER ────────────────────────
		//
		// A second connection costs as much as the first, and 97% of techniques fire nothing —
		// controlling those would triple the campaign's traffic to confirm silence. So this runs
		// exactly where the claim exists to be tested, which is also what the splitting axis
		// does ("the control is only fetched when the mutated sample ALREADY differs").
		//
		// FRESH BINDINGS ARE NOT USED, DELIBERATELY. `bind` still holds this technique's
		// `Random`, so the control carries the SAME canary and the SAME cachebuster as the
		// attack. Minting a new one would mean the two arms differed in two ways — the
		// obfuscation and the canary — which is the exact mistake that produced the retracted
		// reports: a control that varies more than one thing answers no question at all.
		r.prog.probes.Add(1)
		// WHAT THIS TECHNIQUE COST, counted off the socket rather than assumed from the plan.
		// The attack connection is three steps and the control is three more, but a peer that
		// hangs up after the first makes that a lie — which is exactly why this reads the
		// connection's own tally.
		sentReqs := conn.RequestsSent
		var ctlRaw []byte
		var ctlPost detect.StepSummary
		if sig.Count > 0 {
			r.prog.signals.Add(1)
			raw, _, cerr := tech.RenderControlAutoCL(bind)
			if cerr == nil {
				ctlRaw = raw
				ctl := r.sc.Connection(ctx, t, []probe.Step{
					{Label: "pre", Raw: benign},
					{Label: "control", Raw: raw},
					{Label: "post", Raw: benign},
				}, probe.ModePipelined)
				sentReqs += ctl.RequestsSent
				sig = detect.ScoreControl(sig, ctl, base, canary)
				ctlPost = detect.SummarizeStep(ctl, "post")
				if sig.ControlUsed {
					r.prog.controls.Add(1)
				}
				if sig.ControlAlsoFired {
					// COUNTED WHERE IT IS DECIDED. `withdrawn` is the number this engine most
					// needs to show: it is how many claims it declined to make, and a campaign
					// that cannot report that has no evidence its oracle is discriminating.
					r.prog.withdrawn.Add(1)
				} else if sig.Count > 0 {
					// SURVIVED ITS OWN CONTROL. This is the only counter in the beat that means
					// "found something worth a human's time", and it is the pair of `withdrawn`:
					// a run reporting neither is a run whose oracle nobody can judge while it
					// is still running.
					r.prog.found.Add(1)
				}
			}
		}

		obs := detect.FramingObs{
			Schema: detect.SchemaFraming, TS: detect.NowRFC3339(), Axis: string(technique.AxisSmuggle),
			Program: h.Program, Host: h.Host, Port: t.Port, Scheme: t.Scheme, Phase: r.p.Phase,
			Endpoint:    bind.Endpoint,
			TechniqueID: tech.RequestHex[:16], Class: tech.Class, Family: tech.Family,
			Variant: tech.Variant, Tier: tech.Tier, HeaderLine: tech.HeaderLine,
			SentRaw: raw, NormalRaw: benign,
			Canary: canary, Baseline: base, Pre: pre, Attack: atk, Post: post,
			ControlRaw: ctlRaw, ControlPost: ctlPost,
			Signals: sig, Void: void, Voided: void.Is, VoidReason: void.Reason,
			SignalCount: sig.Count, ALPN: conn.ALPN, TrailingBytes: len(conn.TrailingRaw),
			ClosedAfter: conn.ClosedAfter, RateMs: r.p.RateMs,
			// The traffic this row is responsible for, and the address it left from. Neither is
			// derivable from the other columns: `requests` is not len(steps) on a connection the
			// peer closed early, and `egress` was declared on Row and never assigned by either
			// axis, which made a multi-machine fleet's whole point unverifiable from its output.
			Requests: sentReqs, Egress: r.egress,
			// WHY THIS HOST IS BEING GIVEN MORE ROOM, on the row that was probed while it was.
			// Without it, a throttled host and a fast one produce observations that look alike,
			// and `rate_ms` alone says what was ASKED for, never what was actually sent.
			BackoffReason: r.sc.BackoffReason(h.Host),
		}

		// EMITTED PER TECHNIQUE, not collected and returned. A host in the sweep phase takes
		// thousands of techniques; pushing each as it exists makes it durable the moment it
		// happens, so a Worker dying at technique 3,000 loses nothing before it. The key is
		// intrinsic, so a resumed re-probe re-pushes onto the same slot rather than a second row.
		//
		// THE ENDPOINT IS IN THE KEY, and leaving it out would have quietly undone the entire
		// path fix: this actor is now dispatched one Unit per (host, endpoint), so `/` and
		// `/api/v1/session` on the same host arrive as two Units running the same technique list.
		// Keyed on host alone they land on the SAME slot and the second silently overwrites the
		// first — a run that probed twelve paths would report one, and the one it reported would
		// be whichever finished last.
		ds.Push(rowUnit(detect.RowFromSmuggle(obs), r.p.KeepRaw),
			kontra.Key(fmt.Sprintf("%s#framing#%s#%s#%s",
				h.Host, bind.Endpoint, r.p.Phase, tech.Variant)))
	}
	return nil
}

// requestTarget normalises a Target's endpoint into something the technique templates can
// substitute safely.
//
// EVERY TEMPLATE APPENDS ITS OWN QUERY: the slot is used as `${endpoint}?cb=${random}`, and the
// cachebuster is load-bearing twice over — it defeats an edge cache that would otherwise serve
// probe 2 the answer to probe 1, and its value IS the canary that makes a reflection self-proving.
// An endpoint arriving with a query already on it (`/search?q=1`) would render
// `/search?q=1?cb=539915`, where `cb` is no longer a parameter but part of `q`'s value — the
// cachebuster silently stops busting anything and the canary stops being findable where the
// oracle looks for it.
//
// `injection_points` strips the query when it derives a path, so today nothing sends one. This is
// here because `paths_from` is an operator-supplied dataset name and the next one might not.
//
// A fragment cannot appear on the wire at all; if one arrives it came from a crawler that kept it
// and it goes the same way.
func requestTarget(ep string) string {
	if i := strings.IndexAny(ep, "?#"); i >= 0 {
		ep = ep[:i]
	}
	if ep == "" || !strings.HasPrefix(ep, "/") {
		return "/"
	}
	return ep
}

// benignRequest is steps 1 and 3: an ordinary, well-formed request to the same endpoint. It is
// byte-identical in both positions on purpose — that identity is what makes a difference at step
// 3 attributable to step 2 and to nothing else.
func benignRequest(b technique.Bindings, random string) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "GET %s?cb=%s HTTP/1.1\r\n", orDefault(b.Endpoint, "/"), random)
	fmt.Fprintf(&sb, "Host: %s\r\n", b.Host)
	sb.WriteString(b.HeaderBlock)
	sb.WriteString("Connection: keep-alive\r\n\r\n")
	return []byte(sb.String())
}

// corpus is a Method with no network at all: it emits the expanded technique corpus so the
// `techniques` Dataset is queryable. The ACTOR is the source of truth (the families are compiled
// in, so the actor VERSION names the corpus version) and the Dataset is its projection — which is
// the right way round, because a corpus that could drift from the code that sends it is a corpus
// nobody can trust a finding against.
// A Unit selects a SHARD of the corpus. `{"shard": 3, "of": 16}` emits every sixteenth row.
type CorpusShard struct {
	Shard int `json:"shard"`
	Of    int `json:"of"`
}

func corpus(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
	// THE UNIT IS A SHARD, BECAUSE ONE PAYLOAD CANNOT HOLD THE CORPUS.
	//
	// This Method used to ignore its Batch and emit everything on whatever Unit arrived. At 3,630
	// framing rows that fit; adding the 8,700 fold vectors took the returned payload to 4,416,752
	// bytes against Temporal's 4,194,304-byte gRPC ceiling, and the activity failed with
	// ResourceExhausted — then retried, forever, at 0% CPU, looking exactly like a hang. Nothing
	// in the corpus was wrong; the *shape of the emit* was.
	//
	// So the caller dispatches N Units and each returns its slice. That is what Batches are FOR,
	// and it makes the corpus size-independent: adding a family or a tier grows the number of
	// shards a caller asks for, never the size of one message.
	shards := []CorpusShard{}
	for _, u := range b.Units() {
		var c CorpusShard
		if err := u.Into(&c); err != nil || c.Of < 1 {
			c = CorpusShard{Shard: 0, Of: 1} // an unsharded ask still works, it just may not fit
		}
		shards = append(shards, c)
	}
	if len(shards) == 0 {
		shards = []CorpusShard{{Shard: 0, Of: 1}}
	}

	// One pass, emitting only the rows this Batch's shards own. `i` counts every candidate row so
	// the shard split is stable across calls — it is positional, not random.
	i := -1
	mine := func() bool {
		i++
		for _, c := range shards {
			if c.Of <= 1 || i%c.Of == c.Shard {
				return true
			}
		}
		return false
	}

	for _, f := range technique.Families {
		for _, row := range f.Expand() {
			if !mine() {
				continue
			}
			// KEYED ON (family, variant), NOT ON A HEX PREFIX. Every row in a family shares a
			// template, so the first N characters of RequestHex are IDENTICAL across all of
			// them — `POST ${endpoint}?cb=...`. Keying on that prefix pushed 3,630 rows onto
			// one slot and the Dataset came back with a single row. The same trap CONTEXT.md
			// records for the derived name: "the run-id part is a DIGEST, not a prefix".
			ds.Push(corpusUnit(row), kontra.Key(row.Family+"#"+row.Variant))
		}
	}

	// BOTH AXES, OR THE CORPUS DESCRIBES HALF AN ENGINE.
	//
	// `techniques` published the framing families and nothing else, so it held 7 rows — all
	// `axis = framing` — while the fold axis was the one that found the the target chain. A reader
	// asking "what does desync@1.0.0 send?" got an answer that omitted every payload behind the
	// campaign's actual finding, and a finding could not be traced to the vector that produced it
	// because the vector was not in the table.
	//
	// The fold vectors are GENERATED, not templated: `vectors.Generate` enumerates the codepoint
	// space by tier. That is why they were missed — there is no `Family.Expand()` to loop over —
	// but generated is not the same as undocumented, and the projection has to cover both or the
	// dataset quietly means "the framing corpus" while being named `techniques`.
	// `tier` here is a CEILING, not a selector — `Generate` skips anything above it. Passing 0
	// therefore published 14 hardcoded vectors and silently dropped the entire generated space,
	// which is the same "looks like a corpus, describes almost nothing" failure this block exists
	// to fix. TierFull is the whole ~4,700: this Method sends no packets, and the corpus is
	// supposed to be the complete statement of what the actor CAN send, not what one scan chose.
	for _, v := range vectors.Generate(vectors.TierFull, nil) {
		if !mine() {
			continue
		}
		ds.Push(corpusUnit(technique.Row{
			// The vector's own encoded form IS the payload, so it is what `request_text` carries;
			// there is no rendered request until a point and an exchange are chosen.
			RequestText: v.Encoded,
			RequestHex:  v.WireBytes,
			Axis:        technique.AxisSplit,
			Class:       string(v.Class),
			Family:      string(v.Family),
			Variant:     v.ID,
			Tier:        v.Tier,
			// The matched control is the load-bearing half of this axis, so it travels with the
			// row rather than being reconstructible only by whoever remembers the +1 rule.
			HeaderLine: v.Control,
			Slots:      []string{"point"},
			// `Known` marks a codepoint confirmed working in the wild, which is this axis's
			// equivalent of the framing families' h1-report provenance.
			Provenance:  provenanceOfVector(v),
			Explanation: v.Note,
		}), kontra.Key("fold#"+v.ID))
	}
	return nil
}

// provenanceOfVector gives a fold vector the same "why is this in the corpus" answer the framing
// families carry. The framing side cites an h1 report per variant; the fold side is GENERATED
// from a rule, so the honest provenance is the rule itself — plus a marker for the codepoints
// already confirmed in the wild, which is what `Known` records.
func provenanceOfVector(v vectors.Vector) string {
	if v.Known {
		return "rule:U+xx0A narrows to LF; confirmed in the wild"
	}
	return "rule:U+xx0A narrows to LF"
}
