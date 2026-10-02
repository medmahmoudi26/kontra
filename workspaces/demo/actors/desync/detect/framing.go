package detect

// The FRAMING axis: does the front-end forward a body the back-end does not consume?
//
// This file is the sibling of the fold-vector analysis in detect.go, and the difference is the
// ORACLE, not the payload. A fold vector's signal is a DIFFERENCE IN THE BODY against a matched
// control — the same encoding one value higher — because the injected byte lands somewhere the
// response reflects. A framing technique's effect is not in its own response at all: its signal
// is that THE NEXT REQUEST ON THE CONNECTION was answered wrong.
//
// So the unit of observation is the connection, and the shape is fixed:
//
//	[1] benign   must match the baseline. if it does not, the socket was already bad
//	             and nothing after it means anything.
//	[2] attack   may return anything. a 400 here is NOT a finding — the request IS
//	             malformed, and a front-end rejecting it is the correct behaviour.
//	[3] benign   THE SIGNAL. identical bytes to [1]. if this one differs, something
//	             step [2] left in the buffer changed how the server read it.
//
// All three are written before any is read (probe.ModePipelined). That is not an optimisation:
// the smuggled prefix has to still be in the server's buffer when [3] lands, and a
// write-read-write-read cycle gives the server time to consume and discard it.
//
// ── AND THEN THE SAME CONNECTION AGAIN, WITHOUT THE GADGET ──────────────────────────────────────
//
// This file used to say, right here, that "a framing technique has no matched control (there is
// no 'one value higher' than `Connection: Content-Length`)". That was true about the GADGET and
// false about the CLAIM, and the difference cost two retracted reports.
//
// The four steps above establish that the body was read as a request. The claim is that the
// OBFUSCATION caused it — and the control for that is not a different gadget, it is no gadget:
// the identical request with a well-formed, correct `Content-Length`, same body, same canary. A
// server that reads THAT body as a request is not reading Content-Length at all. It is
// pipelining, which is ordinary, which is why `Content-Length: 0` fired on paypalobjects.com more
// often than either obfuscated attack did.
//
// It runs only when the attack already fired, on its own connection, and it can only take signals
// away. See `ScoreControl`.

import (
	"context"
	"time"

	"github.com/medmahmoudi26/kontra-actors/go/desync/probe"
)

// SchemaFraming is the row type for the framing axis.
const SchemaFraming = "framing/v1"

// FramingBaseline is what the benign request does on this host when nothing is wrong. Every
// signal below is a difference from it, so a host that cannot reproduce its own answer cannot be
// scanned — every technique would look like it did something.
type FramingBaseline struct {
	N int `json:"n"`
	// Requests is how many of those N attempts actually reached the wire. A dial or TLS failure
	// costs an attempt and sends nothing, so `N` is the plan and this is the traffic.
	Requests int      `json:"requests"`
	Statuses []int    `json:"statuses"`
	Status   int      `json:"status"`   // modal
	BodySHA  string   `json:"body_sha"` // of the first repeat
	BodyLen  int      `json:"body_len"`
	Stable   bool     `json:"stable"`
	MeanMs   float64  `json:"mean_ms"`
	Errors   []string `json:"errors,omitempty"`
}

// StepSummary is one step of the pipelined probe, reduced to what a SQL query needs. RespRaw is
// carried through because rule 2 says the scanner never throws the bytes away: a parse failure
// with non-empty raw bytes is a signal, not noise — it is what a stacked or truncated response
// looks like.
//
// RESPRAW IS THE MOST EXPENSIVE FIELD IN THE LAKE AND IT IS ALMOST ALL BASELINE HTML. Measured
// over the first campaign's 833 rows: `pre.resp_raw` alone was 13.8 MB of an 18.8 MB total, with
// `attack` at 2.2 MB and `post` at 1.4 MB — 92% of every byte written, for what is mostly the
// same marketing page fetched once per technique. At sweep grain (one host x 3,630 CL.0
// techniques) that is gigabytes per program, so the sweep phase could not be run at all.
//
// The fix is NOT to stop capturing: the oracles read these bytes (the canary scan walks every
// step's raw stream), so they must exist in memory. It is to decide at ROW-BUILD time, once the
// signals are known, how much of them is EVIDENCE — see `Row.Elide`. RespBytes/BodyLen/BodySHA/
// ParseErr are kept unconditionally, so every fact an oracle used survives whatever is dropped,
// and RespRawElided says how many bytes went, so a reader is never guessing whether an empty
// `resp_raw` means "the server sent nothing" or "we did not keep it".
type StepSummary struct {
	Label      string  `json:"label"`
	Status     int     `json:"status"`
	RespBytes  int     `json:"resp_bytes"`
	BodyLen    int     `json:"body_len"`
	BodySHA    string  `json:"body_sha,omitempty"`
	ElapsedMs  float64 `json:"elapsed_ms"`
	ParseErr   string  `json:"parse_err,omitempty"`
	ReadErr    string  `json:"read_err,omitempty"`
	PeerClosed bool    `json:"peer_closed,omitempty"`
	PeerReset  bool    `json:"peer_reset,omitempty"`
	Timeout    bool    `json:"timeout,omitempty"`
	RespRaw    []byte  `json:"resp_raw,omitempty"`
	// RespRawElided is how many bytes of RespRaw were dropped before this row was written.
	// Non-zero means the stream was longer than what you are looking at. Zero next to an empty
	// RespRaw means the peer really did send nothing.
	RespRawElided int `json:"resp_raw_elided,omitempty"`
}

// keepHead trims a captured stream to its first n bytes and records the loss.
//
// HEAD, NOT A SAMPLE FROM THE MIDDLE. Everything that identifies a response — the status line,
// the header block, the framing headers the whole axis is about — is at the front, and a body
// long enough to need trimming is a body whose interesting part is its SHA, which is kept.
func (s *StepSummary) keepHead(n int) {
	if len(s.RespRaw) <= n {
		return
	}
	s.RespRawElided += len(s.RespRaw) - n
	s.RespRaw = s.RespRaw[:n]
}

// dropRaw removes the stream entirely, leaving the count behind.
func (s *StepSummary) dropRaw() {
	if len(s.RespRaw) == 0 {
		return
	}
	s.RespRawElided += len(s.RespRaw)
	s.RespRaw = nil
}

// FramingSignals is which oracles fired. Deliberately a set of independent booleans and not a
// verdict: classification happens in SQL over the dataset, later, so detection logic can improve
// without touching the network again.
type FramingSignals struct {
	Count int `json:"count"`

	// PostDiffers is THE signal. Step 3 was byte-identical to step 1 and was answered
	// differently. Nothing about step 3 was malformed.
	PostDiffers bool `json:"post_differs"`

	// PostStatusChanged is the narrow form: the status alone moved. Separated from PostDiffers
	// (which includes a body change at the same status) because a 200-with-different-body is a
	// much weaker claim on a host that serves anything dynamic.
	PostStatusChanged bool `json:"post_status_changed"`

	// DualResponse: the server sent bytes nobody asked for. Three requests went out and the
	// parser consumed three responses, so anything in TrailingRaw is a fourth — which is what a
	// smuggled request being ANSWERED looks like.
	DualResponse bool `json:"dual_response"`

	// PostParseErr with bytes on the wire: the reader could not find a status line where one
	// should be, because the response it is looking at is glued to the tail of another.
	PostParseErr bool `json:"post_parse_err"`

	// PostTimeout: step 3's answer never came. The back-end is still waiting for the body length
	// the front-end promised it.
	PostTimeout bool `json:"post_timeout"`

	// ClosedAfterAttack: the peer hung up between step 2 and step 3 rather than answering. Weak
	// on its own (plenty of front-ends close on a malformed request) which is why it does not
	// count toward Count unless something else fired.
	ClosedAfterAttack bool `json:"closed_after_attack"`

	// ControlAlsoFired is THE KILL SWITCH, and it is the most important field in this struct.
	//
	// Every other signal here says "the attack did something". This one says the SAME THING
	// HAPPENED WITHOUT THE ATTACK — the control request is byte-identical except that its
	// Content-Length is well-formed and correct, so a server that reads its body as a request
	// is a server ignoring Content-Length entirely. That is pipelining. It is what every HTTP
	// client that sends two requests on one socket relies on, and it is not a desync.
	//
	// Two reports went out of this system without this check and both were retracted. The test
	// that killed them took one minute to run by hand and the scanner could not run it at all.
	//
	// It is scored in `ControlUsed`/`ControlAlsoFired` rather than by silently zeroing the
	// signals, because "we checked and it survived", "we checked and it died" and "we did not
	// check" are three different facts and a triage queue that cannot tell them apart is the
	// queue that shipped the retractions.
	ControlUsed      bool `json:"control_used"`
	ControlAlsoFired bool `json:"control_also_fired"`

	// CanaryReflected is PROOF, not evidence, and it is the only signal here that is.
	//
	// Every other oracle in this file is a DIFFERENCE — the post response is not what the
	// baseline was — and a difference always has innocent explanations left (a nonce in the
	// body, a per-connection request id, a load balancer picking another origin). This one has
	// none: the smuggled path is a random string this scanner invented microseconds earlier and
	// sent ONLY inside the body of request 2. A server that names it in the answer to request 3
	// read that body as a request line. There is no other way for those bytes to be there.
	//
	// Found on api.example.com, 2026-09-05:
	//	  Could not find resource for full path: http://localhost:8080/918722-kontra
	CanaryReflected bool `json:"canary_reflected"`
}

// Void reports whether this observation can carry a claim at all. Kept separate from the signals
// because "we could not test this host" and "this host is clean" are different facts, and
// collapsing them is how a scan reports clean about ground it never covered.
type Void struct {
	Is     bool   `json:"is"`
	Reason string `json:"reason,omitempty"`
}

// FramingObs is one row: one host x one technique.
type FramingObs struct {
	Schema string `json:"schema"`
	TS     string `json:"ts"`
	Axis   string `json:"axis"`

	Program string `json:"program,omitempty"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Scheme  string `json:"scheme"`
	Phase   string `json:"phase"` // screen | sweep

	// Endpoint is the request-target every step of this probe was sent to. RECORDED, because
	// until it was, this axis could not say which path it had tested — and the answer, for the
	// whole first campaign, was `/` and only `/`: `scope_*.endpoint` is blank on every row, so
	// `orDefault(b.Endpoint, "/")` chose root for every host, and the sweep phase's own query
	// then spelled `'/' AS endpoint` literally. Two hosts behind the same edge but different
	// backends were probed identically and recorded identically.
	//
	// It is also what makes the sweep phase correct. The sweep re-probes what the screen moved
	// on, and "what moved" is a (host, endpoint) pair — routing to the backend that disagrees is
	// frequently the whole reason one path desyncs and another does not.
	Endpoint string `json:"endpoint,omitempty"`

	TechniqueID string `json:"technique_id,omitempty"`
	Class       string `json:"class,omitempty"`
	Family      string `json:"family,omitempty"`
	Variant     string `json:"variant,omitempty"`
	Tier        int    `json:"tier,omitempty"`
	HeaderLine  string `json:"header_line,omitempty"`

	// SentRaw is the exact attack bytes. Without it a lead is not reproducible by hand, and a
	// lead nobody can reproduce by hand is not a lead.
	SentRaw []byte `json:"sent_raw,omitempty"`
	// NormalRaw is the BENIGN request sent as steps 1 and 3 of the same socket. It is the
	// baseline the attack is a deviation from, so it is what `original_request` renders — a
	// framing lead whose row shows only the attack asks the reader to imagine the control.
	NormalRaw []byte `json:"normal_raw,omitempty"`

	// Canary is the smuggled path. Recorded so a reader can grep the raw response for it and
	// reach the same verdict without trusting the analyzer.
	Canary string `json:"canary,omitempty"`

	Erratic bool `json:"erratic"`

	// Voided/VoidReason are FLAT, and SignalCount below is too, because every query that
	// matters filters on exactly these three. `void.is` is struct access on a SQL keyword and
	// `signals.count` on a SQL function name — both either fail to parse or quietly mean
	// something else. The nested structs stay for detail; these are what SQL reads.
	Voided      bool   `json:"voided"`
	VoidReason  string `json:"void_reason,omitempty"`
	SignalCount int    `json:"signal_count"`

	Void     Void            `json:"void"`
	Baseline FramingBaseline `json:"baseline"`
	Pre      StepSummary     `json:"pre"`
	Attack   StepSummary     `json:"attack"`
	Post     StepSummary     `json:"post"`
	Signals  FramingSignals  `json:"signals"`

	// ControlRaw and ControlPost are the matched control's arm: the request that was sent with a
	// WELL-FORMED Content-Length, and what step 3 answered on that connection.
	//
	// Present only on observations where the attack already fired — the control costs a whole
	// second connection and a host that reacted to nothing has nothing to control for. That is
	// the same economy the splitting axis uses, and for the same reason: "the control is only
	// fetched when the mutated sample ALREADY differed from the baseline".
	ControlRaw  []byte      `json:"control_raw,omitempty"`
	ControlPost StepSummary `json:"control_post,omitempty"`

	ALPN          string `json:"alpn,omitempty"`
	TrailingBytes int    `json:"trailing_bytes"`
	ClosedAfter   int    `json:"closed_after_step"`
	RateMs        int    `json:"rate_ms"`
	BackoffReason string `json:"backoff_reason,omitempty"`
	Egress        string `json:"egress,omitempty"`
	// Requests is what this observation put on the wire: the attack connection's steps plus the
	// matched control's, when one ran. See Row.Requests for why it is counted and not computed.
	Requests int    `json:"requests"`
	Error    string `json:"error,omitempty"`
}

// Connection runs one paced, multi-step probe on a single socket.
//
// It exists because the pacer lives here and MUST be shared: `rate_ms` is a per-host minimum gap,
// and the framing axis and the injection axis both point at the same origins. A framing scanner
// with its own pacer would double the rate an operator asked for, silently and only under mixed
// load — the worst possible shape for a bug.
//
// Note it paces ONCE per connection, not per step. The three steps are one burst by design; a gap
// between them would break the very property being measured.
func (s *Scanner) Connection(ctx context.Context, t probe.Target, steps []probe.Step,
	mode probe.Mode) *probe.ConnObs {

	po := probe.DefaultOptions()
	po.Mode = mode
	po.ReadTimeout = s.opts.Timeout
	po.DialTimeout = s.opts.Timeout
	po.KeepRawResponses = true

	s.pace(t.Host)
	o := probe.Run(ctx, t, steps, po)
	// FOLD THE ANSWER BACK INTO THE PACING before returning it. Doing this here rather than at
	// the call sites is the point: `Connection` is the only way this axis opens a socket, so an
	// axis cannot forget to back off, and a new one gets it for free.
	s.observe(t.Host, o)
	return o
}

// FramingBaselineOf sends the benign request `n` times on `n` fresh connections and reports
// whether the host reproduces itself.
//
// SEPARATE CONNECTIONS, deliberately. The baseline has to describe the host, not a socket — and
// three benign requests pipelined on one socket would silently also be testing whether the host
// handles pipelining, which many do not. That would mark half the internet erratic.
func (s *Scanner) FramingBaselineOf(ctx context.Context, t probe.Target, benign []byte,
	n int) FramingBaseline {

	if n < 1 {
		n = 1
	}
	out := FramingBaseline{N: n}
	var bodies []string
	for i := 0; i < n; i++ {
		c := s.Connection(ctx, t, []probe.Step{{Label: "baseline", Raw: benign}}, probe.ModeSequential)
		out.Requests += c.RequestsSent
		if c.DialErr != "" {
			out.Errors = append(out.Errors, "dial: "+c.DialErr)
			continue
		}
		if c.TLSErr != "" {
			out.Errors = append(out.Errors, "tls: "+c.TLSErr)
			continue
		}
		st := c.Steps[0]
		out.Statuses = append(out.Statuses, st.Status)
		out.MeanMs += st.ElapsedMs
		bodies = append(bodies, st.BodySHA)
		if i == 0 {
			out.BodySHA, out.BodyLen = st.BodySHA, st.BodyLen
		}
	}
	if n := len(out.Statuses); n > 0 {
		out.MeanMs /= float64(n)
		out.Status = modal(out.Statuses)
		out.Stable = allSame(out.Statuses) && allSameStrings(bodies)
	}
	return out
}

// AnalyzeFraming reduces one connection to signals.
//
// THE ASYMMETRY IS THE WHOLE THING. Step 2 is allowed to fail in any way at all — it is a
// malformed request and a front-end rejecting it is correct behaviour, not evidence. Only step 3
// carries a claim, because step 3 is byte-identical to step 1 and step 1 already agreed with the
// baseline.
func AnalyzeFraming(c *probe.ConnObs, base FramingBaseline, canary string) (pre, atk, post StepSummary,
	sig FramingSignals, void Void) {

	if c.DialErr != "" {
		return pre, atk, post, sig, Void{true, "dial: " + c.DialErr}
	}
	if c.TLSErr != "" {
		return pre, atk, post, sig, Void{true, "tls: " + c.TLSErr}
	}
	// A negotiated h2 means the HTTP/1.1 bytes written were nonsense and anything derived from
	// them is invalid. probe offers http/1.1 only unless a caller opts in, so this should be
	// unreachable — which is exactly why it is worth asserting rather than assuming.
	if c.ALPN == "h2" {
		return pre, atk, post, sig, Void{true, "negotiated h2; the h1 bytes were nonsense"}
	}
	if len(c.Steps) < 3 {
		return pre, atk, post, sig, Void{true, "connection carried fewer than 3 steps"}
	}

	pre, atk, post = summarize(c.Steps[0]), summarize(c.Steps[1]), summarize(c.Steps[2])

	// The pre-flight control. If the FIRST benign request already disagrees with the baseline,
	// this socket was never in a known state and step 3 cannot be attributed to step 2. Void,
	// not clean.
	if pre.Status != base.Status || (base.BodySHA != "" && pre.BodySHA != base.BodySHA) {
		return pre, atk, post, sig, Void{true, "pre-flight benign request did not match baseline"}
	}

	// DID THE SERVER TELL US IT REFUSED? This is the discrimination the whole oracle turns on,
	// and getting it wrong makes a PATCHED server look vulnerable.
	//
	// The correct behaviour for a server that will not parse a malformed Content-Length is to
	// answer 4xx and close the connection (RFC 9112 §6.3: reject rather than guess). That leaves
	// step 3 unanswered — which is byte-for-byte what "the back-end is still waiting for a body"
	// also looks like. The difference is that here the server SAID SO first.
	//
	// So a rejected attack earns no signal from silence afterwards. It still earns one from a
	// step 3 that was actually ANSWERED and answered differently, because a server that refused
	// the attack has no excuse for a changed answer to a request it accepted.
	rejected := atk.Status >= 400
	postAnswered := post.RespBytes > 0

	switch {
	case postAnswered:
		sig.PostStatusChanged = post.Status != base.Status
		sig.PostDiffers = sig.PostStatusChanged ||
			(base.BodySHA != "" && post.BodySHA != "" && post.BodySHA != base.BodySHA)
		sig.PostParseErr = post.ParseErr != ""
	case rejected:
		// Silence after an explicit refusal is the refusal, not a finding. Nothing fires.
	default:
		// The server ACCEPTED the attack (2xx/3xx) and then went quiet on a request it had
		// already proved it could answer. That is the back-end waiting for a body the front-end
		// promised and never sent.
		sig.PostTimeout = true
	}

	sig.DualResponse = len(c.TrailingRaw) > 0
	sig.ClosedAfterAttack = c.ClosedAfter == 1

	// Checked on EVERY step's raw bytes, not just step 3: a stacked response can put the
	// smuggled answer anywhere in the stream, and which step the parser attributed it to is an
	// artifact of where it happened to resynchronise.
	if canary != "" {
		for _, st := range c.Steps {
			if len(st.RespRaw) > 0 && indexOf(st.RespRaw, []byte(canary)) >= 0 {
				sig.CanaryReflected = true
				break
			}
		}
		if !sig.CanaryReflected && indexOf(c.TrailingRaw, []byte(canary)) >= 0 {
			sig.CanaryReflected = true
		}
	}

	for _, fired := range []bool{sig.PostDiffers, sig.PostParseErr, sig.PostTimeout, sig.DualResponse,
		sig.CanaryReflected} {
		if fired {
			sig.Count++
		}
	}
	// ClosedAfterAttack is a corroborator, never a finding on its own: plenty of front-ends
	// simply hang up on a malformed request, and counting it alone would flag every one of them.
	if sig.Count > 0 && sig.ClosedAfterAttack {
		sig.Count++
	}
	return pre, atk, post, sig, Void{}
}

// ScoreControl re-scores an observation against its matched control, and it is the only function
// here that can take a signal AWAY.
//
// ── WHAT IT IS ASKING ───────────────────────────────────────────────────────────────────────────
//
// The attack fired. The claim implied by that is "the obfuscated Content-Length made this server
// read my body as a request". `ctl` is the same connection run again with the Content-Length
// WELL-FORMED and correct — same endpoint, same header block, same body, same canary, only the
// gadget removed. So:
//
//	control silent   the gadget is load-bearing. The claim stands and `ControlUsed` records that
//	                 it was actually tested rather than assumed.
//	control FIRES    the server does this to a perfectly ordinary request. It is not reading
//	                 Content-Length, the body was always going to be read as a request, and the
//	                 obfuscation is irrelevant. Every signal is withdrawn.
//
// ── WHY WITHDRAWING IS WHOLESALE AND NOT PARTIAL ────────────────────────────────────────────────
//
// It is tempting to keep `post_differs` on the grounds that something still differed. It did —
// from the BASELINE, which is a comparison the control has just shown is the wrong one. Once the
// control reproduces the behaviour, every oracle in this file is measuring the server's ordinary
// response to a pipelined body, and the honest count is zero.
//
// `Count` going to zero is what keeps the row out of `desync_leads` (the promote query is
// `signal_count > 0`) without deleting anything: the row stays, the individual signals stay
// readable, and `control_also_fired` says exactly why nobody was paged. A row that was silently
// dropped could not be audited, and auditing these is how the last campaign's mistake was found.
func ScoreControl(sig FramingSignals, ctl *probe.ConnObs, base FramingBaseline,
	canary string) FramingSignals {

	// A CONTROL THAT COULD NOT RUN IS NOT A CONTROL THAT STAYED SILENT, and conflating the two
	// would be the original bug wearing a check's clothing: the claim would stand, the row would
	// say `control_used`, and nobody could tell it had never been tested. `ControlUsed` stays
	// false and the attack's signals are left exactly as they were — unconfirmed, which is what
	// they are.
	//
	// Three steps are required because the oracle is step 3. A connection the peer closed after
	// step 2 answered nothing to compare.
	if ctl == nil || ctl.DialErr != "" || ctl.TLSErr != "" || len(ctl.Steps) < 3 {
		return sig
	}
	sig.ControlUsed = true

	post := summarize(ctl.Steps[len(ctl.Steps)-1])
	fired := false

	// THE CANARY IS THE DECIDING ONE and it is checked across every step, exactly as the attack
	// arm checks it: a stacked response can surface the smuggled answer anywhere in the stream.
	if canary != "" {
		for _, st := range ctl.Steps {
			if len(st.RespRaw) > 0 && indexOf(st.RespRaw, []byte(canary)) >= 0 {
				fired = true
				break
			}
		}
		if !fired && indexOf(ctl.TrailingRaw, []byte(canary)) >= 0 {
			fired = true
		}
	}
	// The weaker oracles count too. A control whose step 3 comes back unlike the baseline is a
	// host that answers its own benign request inconsistently under a pipelined body — which is
	// the innocent explanation `post_differs` always had, now observed directly.
	if !fired && post.RespBytes > 0 {
		fired = post.Status != base.Status ||
			(base.BodySHA != "" && post.BodySHA != "" && post.BodySHA != base.BodySHA)
	}
	if !fired && len(ctl.TrailingRaw) > 0 {
		fired = true
	}

	if !fired {
		return sig
	}

	sig.ControlAlsoFired = true
	sig.PostDiffers = false
	sig.PostStatusChanged = false
	sig.PostParseErr = false
	sig.PostTimeout = false
	sig.DualResponse = false
	sig.CanaryReflected = false
	sig.ClosedAfterAttack = false
	sig.Count = 0
	return sig
}

// indexOf is a plain substring scan. bytes.Index would do, but this file deliberately imports
// nothing that could normalise or re-encode what came off the wire.
func indexOf(h, n []byte) int {
	if len(n) == 0 || len(h) < len(n) {
		return -1
	}
outer:
	for i := 0; i+len(n) <= len(h); i++ {
		for j := range n {
			if h[i+j] != n[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}

// SummarizeStep is `summarize` for a caller outside this package: the named step of a connection,
// or a zero summary when the connection never got that far. Used by the control arm, which needs
// to record what its step 3 answered without duplicating the reduction.
func SummarizeStep(c *probe.ConnObs, label string) StepSummary {
	if c == nil {
		return StepSummary{Label: label}
	}
	for _, st := range c.Steps {
		if st.Label == label {
			return summarize(st)
		}
	}
	return StepSummary{Label: label}
}

func summarize(st probe.StepObs) StepSummary {
	return StepSummary{
		Label: st.Label, Status: st.Status, RespBytes: st.RespBytes, BodyLen: st.BodyLen,
		BodySHA: st.BodySHA, ElapsedMs: st.ElapsedMs, ParseErr: st.ParseErr, ReadErr: st.ReadErr,
		PeerClosed: st.PeerClosed, PeerReset: st.PeerReset, Timeout: st.Timeout,
		RespRaw: st.RespRaw,
	}
}

func allSameStrings(v []string) bool {
	for i := 1; i < len(v); i++ {
		if v[i] != v[0] {
			return false
		}
	}
	return true
}

// NowRFC3339 is the one timestamp spelling both axes use.
func NowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }
