package detect

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Row is the ONE shape that lands in `observations`, whichever axis produced it.
//
// WHY ONE TABLE. The two axes hunt different bugs with different oracles — `fold` splices a
// codepoint into a crawled exchange and reads a matched-control differential; `frame` renders a
// whole request and reads the socket. Their evidence is genuinely different and stays different,
// in the nested blocks below. But every question worth asking across a campaign is asked across
// BOTH: which hosts are erratic, what did we refuse to claim, which techniques ever fire, what
// did this program look like last month. Two tables makes every one of those a UNION, and a
// UNION is the thing people stop writing.
//
// SIGNALS IS A LIST OF NAMES, NOT A COLUMN PER ORACLE. The two axes have different oracles and
// will grow more; a boolean column each means the schema changes every time somebody learns
// something, and a query written against the old shape silently stops matching. A list is one
// column, `list_contains(signals, 'canary_reflected')` reads the same on both axes, and a new
// oracle is a new STRING rather than a migration.
//
// STILL NO VERDICT. Every field here is a fact about what was sent and what came back.
// Classification stays in SQL over the dataset, later, so detection can improve without touching
// the network again.

const SchemaRow = "observation/v1"

// Axis names the bug class, and it is the discriminator every query starts from.
const (
	AxisSplit   = "split"   // a codepoint narrows to a control byte and splits the request
	AxisSmuggle = "smuggle" // two parsers disagree about where the body ends
)

// Signal names. One vocabulary across both axes: a name means the same thing wherever it appears,
// which is what lets a single query span them.
const (
	// Proof, not evidence — the only signals here that are. Each means a value this scanner
	// minted came back from the target, and there is no innocent path for those bytes.
	SigCanaryReflected = "canary_reflected"   // frame: the smuggled path is in the response
	SigAuthorityInject = "authority_injected" // fold: the injected Host became the URL authority

	// Differences. Each has innocent explanations left and is weaker on its own.
	SigPostDiffers     = "post_differs"   // frame: request 3 answered unlike the baseline
	SigDualResponse    = "dual_response"  // frame: bytes nobody asked for
	SigPostTimeout     = "post_timeout"   // frame: request 3 never answered, attack was accepted
	SigPostParseErr    = "post_parse_err" // frame: response glued to another
	SigClosedAfter     = "closed_after_attack"
	SigControlSurvived = "control_survived" // BOTH axes: the reaction survived its matched control
	// `control_also_fired` is the only name here that means the scanner WITHDREW a claim. The
	// same thing happened without the gadget, so the gadget explained nothing — see
	// `FramingSignals.ControlAlsoFired`. It rides on a row whose `signal_count` is 0 and it is
	// the audit trail for every claim this engine decided not to make.
	SigControlAlsoFired = "control_also_fired"
	SigHeaderInjected  = "header_injected"  // fold: the canary came back, but not as an authority
	SigShapeAnomaly    = "shape_anomaly"    // fold: cl_mismatch, truncated_body, leaked_headers…
	SigBodyDiffers     = "body_differs"
	SigStatusChanged   = "status_changed"
	SigNewHeaders      = "new_headers"
)

// Row is one probe: one target, one technique, one answer.
type Row struct {
	Schema string `json:"schema"`
	TS     string `json:"ts"`
	Axis   string `json:"axis"`

	Program string `json:"program,omitempty"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Scheme  string `json:"scheme"`
	Phase   string `json:"phase,omitempty"` // screen | sweep

	// Endpoint is the path this probe was sent to — the request-target on the smuggling axis,
	// and the crawled URL's path on the splitting axis. ONE COLUMN FOR BOTH, for the same reason
	// `host` is: "which paths has this program actually been tested on" must be one query.
	//
	// Its absence is what let the first campaign report 40 hosts scanned while every single
	// probe went to `/`. A dataset that cannot express where it looked cannot be asked what it
	// missed.
	Endpoint string `json:"endpoint,omitempty"`

	// WHAT WAS SENT, in the same three fields whichever axis sent it.
	//
	//	           fold                          frame
	//	Technique  vector id (fold-lf-u070a)      variant (obs-fold-20-preserve)
	//	Gadget     the encoding (%DC%8A)          the header line (Content-Length \r\n : N)
	//	Point      where it went (path:suffix)    — (the whole request is the payload)
	Technique string `json:"technique,omitempty"`
	Gadget    string `json:"gadget,omitempty"`
	Point     string `json:"point,omitempty"`
	Class     string `json:"class,omitempty"`
	Tier      int    `json:"tier,omitempty"`

	// THERE IS NO `SentRaw` HERE ANY MORE, AND ITS ABSENCE IS THE POINT.
	//
	// It was `[]byte`, so `encoding/json` wrote it as base64 — which made `sent_raw` the
	// byte-for-byte twin of `triggered_request_b64` below, and `smuggle.sent_raw` a third copy of
	// the same string. Measured across the first campaign: 820 of 833 rows had all three
	// identical, 300 KB of the same bytes written three times, and a schema in which the obvious
	// question "which one do I replay?" had three equally correct answers.
	//
	// `triggered_request_b64` is the survivor because it is the one whose NAME says what it is
	// and whose partner column (`original_request_b64`) makes it a diff rather than a blob.

	// THE TWO REQUESTS, SIDE BY SIDE AND EACH IN ITS OWN FIELD.
	//
	// A desync lead is a CLAIM ABOUT A DIFFERENCE, so the difference has to be readable. Carrying
	// only the mutated bytes makes every reader reconstruct the normal request from a vector id
	// and a point id before they can see what actually changed — and the fold rows carried
	// neither, so `sent_raw` was empty on the entire axis that found the the target chain.
	//
	// OriginalRequest is what a normal client sends. TriggeredRequest is what produced the
	// signal. Diffing them is the first thing a human does and the first thing a triage agent
	// does, and both of them should be able to do it from the row.
	//
	// EACH REQUEST TWICE: ONE TO READ, ONE TO REPLAY. Not redundancy — they fail at opposite ends.
	//
	// The `_request` fields are RENDERED FOR A HUMAN: real CRLF stays a line break so the header
	// block's shape is visible, but a BARE CR or LF is escaped `\r` / `\n` rather than normalised,
	// because a lone LF where a CRLF belongs is frequently the entire bug and a renderer that
	// prints both the same way erases it while appearing to show it.
	//
	// That rendering is deliberately NOT resendable, and the `_b64` fields are why it does not
	// have to be. A desync payload is made of exactly the bytes a clipboard destroys — bare LF,
	// obs-fold tabs, control characters that narrow to a newline. Pasting the readable form into
	// a replay tab gets you a request with its bug normalised out, which is worse than useless:
	// it reproduces nothing and reads as a false positive. Base64 is byte-exact, survives every
	// terminal, browser and column on the way, and decodes to the request as it went on the wire:
	//
	//	kontra dataset query desync_leads -sql \
	//	  "SELECT triggered_request_b64 FROM desync_leads WHERE is_proof LIMIT 1" | base64 -d
	//
	// then paste into Caido's replay. The readable field is for deciding WHICH row to do that to.
	OriginalRequest     string `json:"original_request,omitempty"`
	OriginalRequestB64  string `json:"original_request_b64,omitempty"`
	TriggeredRequest    string `json:"triggered_request,omitempty"`
	TriggeredRequestB64 string `json:"triggered_request_b64,omitempty"`
	// The matched control's request — the same shape one value higher. It is what turns "this
	// looks odd" into "this is the injection", so a lead that has one carries it, and it is the
	// request a triager resends FIRST to confirm the difference is the injection and not the host.
	ControlRequest    string `json:"control_request,omitempty"`
	ControlRequestB64 string `json:"control_request_b64,omitempty"`
	// Canary is the value this scanner minted for this probe — the smuggled path (frame) or the
	// injected authority (fold). Recorded so a reader can grep the raw response for it and reach
	// the same verdict without trusting the analyzer.
	Canary string `json:"canary,omitempty"`

	// THE THREE OUTCOMES, KEPT APART. "we could not test this" and "this is clean" are different
	// facts, and collapsing them is how a scan reports clean about ground it never covered.
	Erratic     bool     `json:"erratic"`
	Voided      bool     `json:"voided"`
	VoidReason  string   `json:"void_reason,omitempty"`
	Signals     []string `json:"signals"`
	SignalCount int      `json:"signal_count"`

	// Axis-specific evidence. Exactly one is populated, named by Axis.
	Split   *Observation `json:"split,omitempty"`
	Smuggle *FramingObs  `json:"smuggle,omitempty"`

	// Requests is how many requests this row actually put on the wire — every step of every
	// connection it opened, including the baseline repeats it consumed and the matched control
	// it escalated to.
	//
	// A FLAT COLUMN, for the same reason `voided` and `signal_count` are: the question is asked
	// in SQL, by a reader who is not going to reach into a nested struct to ask it. Before this
	// column the only answer to "how much traffic did that campaign send" was COUNT(*) times a
	// guess at the step count — which over-counts every row that never got past the dial and
	// under-counts every row that fired, because firing costs a whole extra connection.
	//
	// `SELECT sum(requests) FROM observations WHERE run_id = ?` is the number, and it survives
	// worker death, resume and retry in a way the in-memory heartbeat counters do not.
	Requests int `json:"requests"`

	// Provenance: which Machine went out to which address, and how hard it was allowed to push.
	//
	// `Egress` WAS DECLARED HERE AND NEVER ASSIGNED on either axis — the only writer was the
	// `reach` Method, into a different dataset. A fleet is spread across machines specifically to
	// spread the source addresses a target sees, and with this column empty that spread was
	// unfalsifiable: nothing in `observations` could say which address any probe left from, so
	// "we used twelve machines for IP diversity" was a claim about the invoice, not the traffic.
	Egress string `json:"egress,omitempty"`
	Node   string `json:"node,omitempty"`
	RateMs int    `json:"rate_ms,omitempty"`
	ALPN   string `json:"alpn,omitempty"`
	Error  string `json:"error,omitempty"`

	// Elided names what `Elide` dropped from this row, in words, or is empty on a row that kept
	// everything. It is a COLUMN rather than a log line because the reader who needs it is
	// looking at the row a month later asking why `pre.resp_raw` is empty, and "the server sent
	// nothing" and "we did not keep it" are answers they must be able to tell apart without
	// knowing which version of this actor wrote the row.
	Elided string `json:"elided,omitempty"`
}

// Retention bounds. Deliberately generous: the point is to stop writing megabytes of somebody's
// marketing page per technique, not to make evidence unreadable.
const (
	// EvidenceCap bounds a raw stream on a row that FIRED. 64 KB holds any status line, any
	// header block, and enough body for a stacked or truncated response to be obvious.
	EvidenceCap = 64 << 10
	// CleanCap bounds the attack step's stream on a row that found nothing. 4 KB is the edge's
	// answer — a 400 page, a redirect, a WAF block — which is what bulk-reading clean rows is
	// for.
	CleanCap = 4 << 10
)

// Elide decides how much of this row is EVIDENCE and drops the rest. Called once, at push time,
// after the signals are known — which is the only moment the question can be answered.
//
// ── WHY THIS EXISTS ─────────────────────────────────────────────────────────────────────────────
//
// The first campaign wrote 6.3 MB of Parquet for 833 rows: 7.8 KB per row, of which 92% was raw
// HTTP responses and another 6% was the same request written out five times. That is survivable
// at screen grain, where one host costs one row per tier-1 technique. It is not survivable at
// SWEEP grain, which is the phase that finds things: `CL.0` alone is 3,630 techniques, so one
// host is 3,630 rows and a hundred hosts is 363,000 — 2.8 GB, per program, for a table whose
// interesting rows are the 27% that carry a signal.
//
// ── WHAT IT WILL NOT DO ─────────────────────────────────────────────────────────────────────────
//
// It never touches a fact an oracle read. `Status`, `RespBytes`, `BodyLen`, `BodySHA`,
// `ElapsedMs`, `ParseErr`, `PeerClosed`, `PeerReset`, `Timeout` are all kept on every step of
// every row, so every query that decides whether something happened reads exactly what it read
// before. What goes is the BODY BYTES those facts were derived from, and only where no signal
// fired — plus, everywhere, the duplicate copies of the request.
//
// It never silently drops. Every removal increments `RespRawElided` or is named in `Elided`.
//
// `keep` is the operator's override (`params.keep_raw`), for a deep-dive run against a handful of
// hosts where the whole stream is the point. It skips the clean-row rules and keeps the caps.
func (r *Row) Elide(keep bool) {
	// ── ALWAYS: THE DUPLICATE REQUESTS ──────────────────────────────────────────────────────
	// The nested observation carries the same bytes the top-level `_b64` columns do — measured
	// at 820 of 833 rows byte-identical. These are the copies, not the originals, so they go
	// first and they go on every row, signalling or not.
	if r.Smuggle != nil {
		r.Smuggle.SentRaw = nil
		r.Smuggle.NormalRaw = nil
	}

	fired := r.SignalCount > 0 || r.Erratic || r.Voided

	if fired || keep {
		// A ROW WORTH READING KEEPS ITS STREAMS, capped only where a body is too large to be
		// evidence of anything a SHA does not already settle.
		if r.Smuggle != nil {
			r.Smuggle.Pre.keepHead(EvidenceCap)
			r.Smuggle.Attack.keepHead(EvidenceCap)
			r.Smuggle.Post.keepHead(EvidenceCap)
		}
		if n := r.elidedBytes(); n > 0 {
			r.Elided = "oversized response bodies trimmed to 64 KB"
		}
		return
	}

	// ── CLEAN ROW ───────────────────────────────────────────────────────────────────────────
	//
	// Nothing fired, the host reproduced its baseline, and the probe was not voided. This row's
	// job is to prove the ground was covered — which `host`, `technique`, `status` and
	// `body_sha` do completely. It does not need the bytes.
	//
	// `pre` and `post` are the BENIGN request's answer, twice: the same page the baseline
	// already summarised, and the single largest thing in the lake. They go entirely.
	// `attack` keeps a head, because "what does this edge say to a malformed request" is the one
	// question people really do ask across thousands of clean rows.
	if r.Smuggle != nil {
		r.Smuggle.Pre.dropRaw()
		r.Smuggle.Post.dropRaw()
		r.Smuggle.Attack.keepHead(CleanCap)
	}

	// The RENDERED requests go; the `_b64` twins stay. They carry identical information —
	// `renderRequest` is a pure function of the same bytes — so this costs a reader nothing but
	// a `base64 -d`, and it is only the readable form that exists to be skimmed in a grid, which
	// is not something anyone does to a row that found nothing.
	r.OriginalRequest = ""
	r.TriggeredRequest = ""
	r.ControlRequest = ""

	r.Elided = "clean row: baseline and post response bodies, rendered requests " +
		"(replay from *_request_b64)"
}

// elidedBytes totals what came off this row's steps, for deciding whether to say so.
func (r *Row) elidedBytes() int {
	if r.Smuggle == nil {
		return 0
	}
	return r.Smuggle.Pre.RespRawElided + r.Smuggle.Attack.RespRawElided + r.Smuggle.Post.RespRawElided
}

// RowFromSmuggle lifts a framing observation into the shared shape.
func RowFromSmuggle(o FramingObs) Row {
	var sigs []string
	add := func(on bool, name string) {
		if on {
			sigs = append(sigs, name)
		}
	}
	add(o.Signals.CanaryReflected, SigCanaryReflected)
	add(o.Signals.PostDiffers, SigPostDiffers)
	add(o.Signals.DualResponse, SigDualResponse)
	add(o.Signals.PostTimeout, SigPostTimeout)
	add(o.Signals.PostParseErr, SigPostParseErr)
	add(o.Signals.ClosedAfterAttack, SigClosedAfter)
	// SURVIVING A CONTROL IS ITSELF A SIGNAL NAME, so the strongest query on this axis —
	// "reacted, and the reaction was the gadget's" — is a `list_contains` like every other, and
	// the weaker "reacted, untested" is its absence rather than a column nobody remembers.
	add(o.Signals.ControlUsed && !o.Signals.ControlAlsoFired, SigControlSurvived)
	// AND SO IS FAILING ONE. This row's `signal_count` is zero, so it will never be promoted —
	// but it is the row that proves the host was looked at properly, and a campaign that cannot
	// show its rejected claims is a campaign whose accepted ones nobody should believe.
	add(o.Signals.ControlAlsoFired, SigControlAlsoFired)

	return Row{
		Schema: SchemaRow, TS: o.TS, Axis: AxisSmuggle,
		Program: o.Program, Host: o.Host, Port: o.Port, Scheme: o.Scheme, Phase: o.Phase,
		Endpoint:  o.Endpoint,
		Technique: o.Variant, Gadget: o.HeaderLine, Class: o.Class, Tier: o.Tier,
		Canary: o.Canary,
		// Same two fields as the fold axis, meaning the same thing: what a normal client sends
		// versus what produced the signal. The frame axis sends both down ONE socket, so here
		// `original` is literally steps 1 and 3 and `triggered` is step 2 between them.
		OriginalRequest:     renderRequest(o.NormalRaw),
		OriginalRequestB64:  b64(o.NormalRaw),
		TriggeredRequest:    renderRequest(o.SentRaw),
		TriggeredRequestB64: b64(o.SentRaw),
		// THE THIRD REQUEST, in the columns the splitting axis already uses for exactly this —
		// the request that turns "this host reacted" into "this host reacted to the gadget".
		// Empty on a row whose attack fired nothing, because no control was run there.
		ControlRequest:    renderRequest(o.ControlRaw),
		ControlRequestB64: b64(o.ControlRaw),
		Erratic:             o.Erratic, Voided: o.Voided, VoidReason: o.VoidReason,
		Signals: sigs, SignalCount: o.SignalCount,
		Smuggle: &o, ALPN: o.ALPN, RateMs: o.RateMs, Egress: o.Egress, Error: o.Error,
		Requests: o.Requests,
	}
}

// RowFromSplit lifts a fold observation into the shared shape.
//
// `control_survived` is the load-bearing one and it is deliberately NOT just "something differed".
// A fold vector's difference only means anything against its MATCHED control — the same encoding
// one value higher — because comparing against the plain baseline makes every host that echoes
// its own URL look vulnerable. FOLDSCAN records that distinction taking false positives from 26
// to 0, so the signal name says which comparison was made.
func RowFromSplit(o Observation) Row {
	var sigs []string
	// `ControlUsed` ALONE IS THE OPPOSITE OF A FINDING, and mapping it straight to
	// `control_survived` labelled every rejected row as if it had passed.
	//
	// Read `probe()`: the control is fetched only when the mutated sample ALREADY differed from
	// the baseline, and the signals are then RE-SCORED against that control. So `ControlUsed`
	// means "this row differed from baseline and we went and checked it properly" — the flag says
	// which comparison was made, not how it came out. The outcome is `Count`, the re-scored one.
	//
	// `ControlUsed && Count == 0` is therefore the discrimination DOING ITS JOB: a difference that
	// did not survive its matched control is a host reacting to an odd byte, which is exactly the
	// noise FOLDSCAN's 26-false-positives-to-0 came from suppressing. Emitting `control_survived`
	// there inverted the meaning of the one signal the fold axis exists to produce, and every one
	// of the first 240 rows carried it while `signal_count` sat at 0 next to it.
	if o.Signals.ControlUsed && o.Signals.Count > 0 {
		sigs = append(sigs, SigControlSurvived)
	}
	if o.Signals.StatusChanged {
		sigs = append(sigs, SigStatusChanged)
	}
	if o.Signals.BodyChangedNormalized {
		sigs = append(sigs, SigBodyDiffers)
	}
	if len(o.Signals.ShapeAnomaly) > 0 {
		sigs = append(sigs, SigShapeAnomaly)
	}
	if len(o.Signals.NewHeaders) > 0 {
		sigs = append(sigs, SigNewHeaders)
	}

	// THE PROOF SIGNAL. Everything above is a difference; this is the one that no innocent path
	// explains — a canary this scanner minted came back as the AUTHORITY of a URL the server
	// built, which means our bytes became a Host header on its backend request.
	//
	// It was declared in this file from the start and NEVER EMITTED: the confirm stage lived in
	// `cmd/foldscan` and the `fold` Method never ran it, so the automatic pipeline could produce
	// observations forever and never a proof. A constant that nothing sets is indistinguishable
	// from a detector that never fires, and it read as the second one for the whole campaign.
	var canary string
	if c := o.Confirm; c != nil {
		canary = c.Canary
		if c.Reflected && c.ReflectedIn == "authority" {
			sigs = append(sigs, SigAuthorityInject)
		} else if c.Reflected {
			// Reflected in a header, or in the body where the control arm stayed clean. Real
			// evidence, weaker claim — named apart so a query can ask for proof alone.
			sigs = append(sigs, SigHeaderInjected)
		}
	}

	r := Row{
		Schema: SchemaRow, TS: o.TS, Axis: AxisSplit,
		Host: hostOf(o.URL), Scheme: schemeOf(o.URL), Port: portOf(o.URL),
		Endpoint:  pathOf(o.URL),
		Technique: o.Vector.ID, Gadget: o.Vector.Encoded, Point: o.Point.ID,
		Class:               string(o.Vector.Class),
		OriginalRequest:     renderRequest(o.Base.RequestRaw),
		OriginalRequestB64:  b64(o.Base.RequestRaw),
		TriggeredRequest:    renderRequest(o.Mut.RequestRaw),
		TriggeredRequestB64: b64(o.Mut.RequestRaw),
		ControlRequest:      renderRequest(o.Control.RequestRaw),
		ControlRequestB64:   b64(o.Control.RequestRaw),
		Erratic:             o.Erratic, Signals: sigs,
		// COUNTED FROM THE NAMES, not from the analyzer's own tally. The two disagreed: the
		// first 240 fold rows carried `signals: ['control_survived']` next to `signal_count: 0`,
		// because the list was built here and the count came from `Signals.Count` upstream. Any
		// consumer trusting the count saw nothing while the list said something, and the leads
		// query (`signal_count > 0`) is a consumer. One derivation or they drift again.
		SignalCount: len(sigs),
		Canary:      canary,
		Split:       &o, Error: o.Error,
		// EVERY ARM THIS OBSERVATION PAID FOR. A split row is not one request: it is the mutated
		// sample's repeats, plus the matched control's repeats when something moved, plus BOTH of
		// Confirm's arms when the control survived. The baseline is added only on the row that IS
		// the baseline (see below) — it is shared across every (point, vector) on the exchange, so
		// adding it to each probe row would multiply one host's real cost by the vector count.
		Requests: splitRequests(o),
	}
	if o.Erratic {
		r.VoidReason = o.Error
	}
	return r
}

// hostOf / schemeOf / portOf split a fold observation's absolute URL into the same three columns
// the framing axis carries natively. Parsed here rather than threaded through the fold path
// because a Row's `host` has to mean the same thing on both axes or every cross-axis query is
// wrong in a way nothing raises.
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Hostname()
	}
	return ""
}

func schemeOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Scheme
	}
	return ""
}

// pathOf is the splitting axis's `endpoint`: the crawled URL's path, with its query, so it means
// the same thing as the smuggling axis's request-target and the two are comparable in one column.
//
// The QUERY IS PART OF IT and not an oversight. `/search?q=` and `/search` routinely reach
// different code, and on this axis the query string is frequently where the payload went — an
// endpoint column that dropped it would group probes that landed in different places.
func pathOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}

func portOf(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	if p := u.Port(); p != "" {
		n, _ := strconv.Atoi(p)
		return n
	}
	if u.Scheme == "http" {
		return 80
	}
	return 443
}

// renderRequest turns sent bytes into a request a human can READ AND RESEND.
//
// NOT `printableReq`, which flattens CRLF to " | " and truncates at 400 bytes. That is a log line.
// This is evidence: `original_request` and `triggered_request` exist so a reader can diff them and
// so a triage agent can replay them, and both need the request to survive the trip intact.
//
// The rules follow from what this engine injects:
//
//	CRLF stays a real line break, so the value displays as an HTTP request rather than one long
//	  line — the header block's SHAPE is frequently the finding (an obs-fold continuation, a
//	  header smuggled after a bare LF), and shape is invisible when it is all one line.
//	A BARE CR OR LF IS ESCAPED, never normalised. `\r\n` and a lone `\n` are the entire difference
//	  between a normal request and a CL.0 probe; a renderer that prints both as a newline erases
//	  the bug while appearing to show it.
//	Other control bytes become \xNN so a codepoint that narrows to one is visible as itself.
//
// Capped generously and MARKED when it truncates, because a silently cut PoC is a PoC that does
// not reproduce and gives no hint why.
func renderRequest(raw []byte) string {
	const cap = 16384
	if len(raw) == 0 {
		return ""
	}
	truncated := false
	if len(raw) > cap {
		raw, truncated = raw[:cap], true
	}
	var b strings.Builder
	b.Grow(len(raw) + 32)
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '\r' && i+1 < len(raw) && raw[i+1] == '\n':
			b.WriteString("\r\n") // a real line ending, kept whole
			i++
		case c == '\r':
			b.WriteString("\\r") // BARE CR — one of the bugs, not a line ending
		case c == '\n':
			b.WriteString("\\n") // BARE LF — likewise
		case c == '\t':
			b.WriteString("\\t") // obs-fold continuations are made of these
		case c < 0x20 || c == 0x7f:
			b.WriteString(fmt.Sprintf("\\x%02x", c))
		default:
			b.WriteByte(c)
		}
	}
	if truncated {
		b.WriteString("\n… [truncated at 16384 bytes]")
	}
	return b.String()
}

// b64 is the REPLAYABLE form: the request exactly as it went on the wire.
//
// The whole point is that it is not rendered. Every transformation `renderRequest` makes for
// readability — escaping a bare LF, marking a truncation — destroys reproduction, and these
// payloads are made of precisely those bytes. Decode and paste:
//
//	... | base64 -d   ->  Caido replay
//
// Empty in, empty out: a sample that never sent (an erratic host, a render failure) gets no
// column value rather than the base64 of nothing, so `WHERE triggered_request_b64 <> ”` means
// "this row has a request you can actually resend".
func b64(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// splitRequests is the traffic one splitting-axis observation is responsible for.
//
// THE BASELINE BELONGS TO EXACTLY ONE ROW. It is sampled once per exchange and every probe row
// carries a copy of it, so charging it to each would multiply a host's real cost by the size of
// the vector set. A row with no Vector is the baseline row (or the refusal/cap row that stands in
// its place), and that is the one that owns those requests; every other row owns only its own arms.
func splitRequests(o Observation) int {
	n := o.Mut.Requests + o.Control.Requests + confirmRequests(o.Confirm)
	if o.Vector.ID == "" {
		n += o.Base.Requests
	}
	return n
}

// confirmRequests is what the confirmation step put on the wire, or zero when it never ran.
//
// Nil-safe because `Observation.Confirm` is a pointer that is set only on the rows that earned
// the escalation — which is the minority, and the reason the column cannot be a constant.
func confirmRequests(c *Confirmation) int {
	if c == nil {
		return 0
	}
	return c.Sample.Requests + c.ControlSample.Requests
}
