// Package detect is the VALIDATOR stage: it decides what to send, and records what came
// back, without ever deciding what it means.
//
// The oracles are ports of the ones in PortSwigger's http-terminator validator:
//
//	stability gate   — terminator blacklists "erratic domains" before scanning them.
//	                   A host whose baseline is not reproducible cannot support a
//	                   differential, and scanning it produces nothing but diffs.
//	contamination    — terminator's ContaminationScan sends a request repeatedly and
//	                   treats INCONSISTENT status codes as the signal. It needs no
//	                   knowledge of what a vulnerable response looks like, which is
//	                   what lets it catch classes nobody has named.
//	response shape   — terminator's FindingType enum, ported: dual response, CL
//	                   mismatch, truncated body, leaked headers, unclosed HTML.
//	differential     — baseline vs mutated, the oracle every report in the corpus
//	                   actually used by hand (403 -> 400 "No Host").
//
// Nothing here emits a verdict. Signals is a record of what fired; ranking and
// classification belong in SQL over the dataset, where they can be revised without
// spending another request.
package detect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/medmahmoudi26/kontra-actors/go/desync/inject"
	"github.com/medmahmoudi26/kontra-actors/go/desync/probe"
	"github.com/medmahmoudi26/kontra-actors/go/desync/unit"
	"github.com/medmahmoudi26/kontra-actors/go/desync/vectors"
)

// ---------------------------------------------------------------- input schema

// probeTargetOf derives where to send from the unit's absolute URL. It is separate from
// the request bytes on purpose: the corpus reached backends by IP after the edge was
// patched (H1 #3475402), which means the socket destination and the Host header are
// independent facts.
func probeTargetOf(u unit.Exchange) probe.Target {
	return probe.Target{Host: u.Hostname(), Port: u.Port(), Scheme: u.Scheme()}
}

// --------------------------------------------------------------- output schema

const (
	SchemaObservation  = "foldscan.observation/v1"
	SchemaConfirmation = "foldscan.confirmation/v1"
)

// Sample aggregates N repeats of one request.
type Sample struct {
	Path string `json:"probe_label"`
	N    int    `json:"n"`
	// Requests is how many of the N repeats actually reached the wire. `N` is what was asked
	// for; a dial or TLS failure costs a repeat and sends nothing, so only this one can be summed
	// into a campaign's traffic total.
	Requests     int      `json:"requests"`
	Statuses     []int    `json:"statuses"`
	Status       int      `json:"status"` // modal
	Stable       bool     `json:"stable"` // every repeat agreed on status AND body hash
	BodyLen      int      `json:"body_len"`
	BodySHA      string   `json:"body_sha"`
	BodyPreview  string   `json:"body_preview"`
	HeaderNames  []string `json:"header_names,omitempty"`
	ContentLen   int      `json:"declared_content_length"`
	ElapsedMsAvg float64  `json:"elapsed_ms_avg"`
	Errors       []string `json:"errors,omitempty"`

	// ShapeSource is the first repeat's raw response, kept in memory so the shape
	// oracles run over real bytes. It is not serialised: the derived signals are what
	// the dataset needs, and the raw stream belongs in blob storage, not a JSONL line.
	ShapeSource []byte `json:"-"`

	// RequestRaw is the EXACT bytes this sample sent. In memory only, for the same reason as
	// ShapeSource — but unlike it, these bytes do reach the lake: `Row` lifts the baseline's and
	// the mutated's into two named top-level fields, because "what was sent normally" and "what
	// was sent to trigger it" are the two halves of a reproducible lead and a human should not
	// have to reconstruct either from a vector id and a point id.
	RequestRaw []byte `json:"-"`
}

// Signals is what fired. Every field is an observation, not a conclusion.
type Signals struct {
	StatusChanged bool `json:"status_changed"`
	StatusFrom    int  `json:"status_from"`
	StatusTo      int  `json:"status_to"`
	BodyChanged   bool `json:"body_changed"`
	// BodyChangedNormalized is the one that counts. Every representation of what we
	// injected — the percent-encoded string, its wire bytes, and the control character
	// it folds to — is redacted from BOTH bodies before comparison. A target that
	// echoes the path back differs for a reason that is not a parser bug, and without
	// this every reflective 404 on the internet is a finding.
	BodyChangedNormalized bool     `json:"body_changed_normalized"`
	BodyLenDelta          int      `json:"body_len_delta"`
	NewHeaders            []string `json:"new_headers,omitempty"`
	Contamination         bool     `json:"contamination"` // mutated repeats disagreed
	ShapeAnomaly          []string `json:"shape_anomaly,omitempty"`
	// ControlUsed records that the differential was taken against the matched control
	// rather than the plain baseline. Signals without it are screening noise: a target
	// that echoes the path differs from "/" for every vector ever sent.
	ControlUsed bool `json:"control_used"`
	Count       int  `json:"signal_count"`
}

// Observation is one line of validator stdout: one target x one vector.
type Observation struct {
	Schema string `json:"schema"`
	TS     string `json:"ts"`
	UnitID string `json:"unit_id,omitempty"`
	URL    string `json:"url"`
	// Point is where the payload went. A finding is meaningless without it: "this host
	// folds U+070A" is a different claim in the path than in X-Tenant-Id, and only the
	// second one tells you the application reads that header.
	Point   inject.Point   `json:"point"`
	Vector  vectors.Vector `json:"vector"`
	Erratic bool           `json:"erratic"` // baseline not reproducible; nothing was scanned
	Base    Sample         `json:"baseline"`
	// Control is the matched negative — the same encoding one value higher. It is only
	// fetched when the mutated sample already differs from the plain baseline, so the
	// common case (no difference at all) still costs one sample.
	Control Sample  `json:"control,omitempty"`
	Mut     Sample  `json:"mutated"`
	Signals Signals `json:"signals"`
	// Confirm is the proof arm, present only on observations that survived their matched
	// control and were therefore worth escalating. Nil is the common and correct case: most
	// points are inert, and paying two more requests to confirm a difference that already
	// failed its control would be spending someone else's origin on noise.
	Confirm *Confirmation `json:"confirm,omitempty"`
	Error   string        `json:"error,omitempty"`
}

// Confirmation is one line of investigator stdout.
type Confirmation struct {
	Schema string `json:"schema"`
	TS     string `json:"ts"`
	UnitID string `json:"unit_id,omitempty"`
	URL    string `json:"url"`
	// Point is where the confirmation payload went — the same point the observation
	// fired on, carried through so a confirmed finding is self-describing.
	Point     inject.Point   `json:"point"`
	Vector    vectors.Vector `json:"vector"`
	Probe     string         `json:"probe"`  // the mutated request, as text
	Canary    string         `json:"canary"` // the value injected
	Reflected bool           `json:"reflected"`
	// EchoedAnywhere is the weak form: the canary appears somewhere in the response,
	// which any target that reflects the request path does for every probe ever sent.
	// Kept because it is data, but it is never evidence on its own.
	EchoedAnywhere bool   `json:"echoed_anywhere"`
	ReflectedIn    string `json:"reflected_in,omitempty"` // "authority" | "header"
	HeaderInject   bool   `json:"header_injection_confirmed"`
	Sample         Sample `json:"sample"`
	// ControlSample is the same canary delivered through the same point with the
	// non-folding twin. A canary that shows up in both arms was echoed, not injected.
	ControlSample Sample `json:"control_sample"`
	Error         string `json:"error,omitempty"`
}

// ------------------------------------------------------------------- options

type Options struct {
	Repeats   int           // per sample; >=2 enables the contamination oracle
	Rate      time.Duration // minimum gap between requests to one host
	Timeout   time.Duration
	UserAgent string
	KeepRaw   bool

	// Backoff widens `Rate` for a host that is telling us to slow down, and is what makes a
	// fast `Rate` safe to ask for. Off leaves the gap exactly as configured.
	Backoff    bool
	MaxBackoff time.Duration // ceiling on the added gap; 0 means DefaultMaxBackoff
}

// DefaultMaxBackoff caps how far one host can widen its own gap. A ceiling exists because the
// alternative is a host that 503s once during a deploy and is then effectively unscanned for the
// rest of the run, reported as covered.
const DefaultMaxBackoff = 30 * time.Second

func DefaultOptions() Options {
	return Options{
		Repeats: 3,
		// One request per second per domain, per the paper's own guidance for
		// scanning third-party infrastructure.
		Rate:      time.Second,
		Timeout: 10 * time.Second,
		// ON BY DEFAULT. An operator who lowers `Rate` is asking to go faster, not asking to
		// keep hammering a host that is failing — and the second thing is what actually gets a
		// scanner blocked.
		Backoff:    true,
		MaxBackoff: DefaultMaxBackoff,
		UserAgent:  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36",
	}
}

// ------------------------------------------------------------------ probing

// Scanner holds per-host pacing state, and is SAFE FOR CONCURRENT USE.
//
// That matters because the pacer has to be shared to mean anything: `Rate` is a minimum gap
// between requests to ONE HOST, and crawler-fed input puts many units on the same host. A
// per-goroutine Scanner would pace each unit independently and hit a host N times harder than
// the rate says — the one number an operator sets to stay welcome on someone's origin.
//
// Repeats is deliberately NOT held here. It used to be, with a SetRepeats() the caller flipped
// between phases; shared across goroutines that is a silent corruption, not a crash — a sample
// labelled N=3 that was actually drawn twice. It is a per-call argument now.
type Scanner struct {
	opts Options

	mu      sync.Mutex
	last    map[string]time.Time
	penalty map[string]time.Duration // added to Rate for this host
	why     map[string]string        // what earned the penalty, for the row
}

func New(o Options) *Scanner {
	return &Scanner{
		opts:    o,
		last:    map[string]time.Time{},
		penalty: map[string]time.Duration{},
		why:     map[string]string{},
	}
}

// Repeats is the configured default sample size, for callers that do not override per call.
func (s *Scanner) Repeats() int {
	if s.opts.Repeats < 1 {
		return 1
	}
	return s.opts.Repeats
}

// pace blocks until this host may be hit again. The sleep happens OUTSIDE the lock: holding it
// across the wait would serialize every host behind the slowest one, turning a per-host rate
// limit into a global one.
func (s *Scanner) pace(host string) {
	s.mu.Lock()
	now := time.Now()
	var wait time.Duration
	gap := s.opts.Rate + s.penalty[host]
	if prev, ok := s.last[host]; ok {
		if w := gap - now.Sub(prev); w > 0 {
			wait = w
		}
	}
	// Reserve this host's slot before releasing, so a concurrent caller queues behind us rather
	// than reading a stale `last` and firing immediately.
	s.last[host] = now.Add(wait)
	s.mu.Unlock()

	if wait > 0 {
		time.Sleep(wait)
	}
}

// ------------------------------------------------------------------ backoff
//
// THE HOST SETS THE RATE, NOT ONLY THE OPERATOR.
//
// `rate_ms` is a fixed gap, so it describes what we are willing to send and nothing about what
// the far side can take. A run configured at 100ms that meets a 429 keeps sending at 100ms —
// which is both the fastest way to get a source address blocked and, worse, a way to produce a
// page of observations that say `erratic` about a host that was merely being throttled.
//
// So a host widens its own gap. It is exponential because the signal is "too fast" and halving
// the rate is the only response that is certainly enough; it decays on clean traffic because a
// single 503 during somebody's deploy should not silently cost the rest of the run.

// slowFor reads one probe and returns why this host should be given more room, or "".
//
// It deliberately does NOT treat a 4xx as backpressure: the attack step of the framing oracle is
// a MALFORMED request, and a front-end answering 400 to it is the correct behaviour, not a
// complaint. Only 429 and 5xx-availability answers, and socket-level refusals, count.
func slowFor(o *probe.ConnObs) string {
	if o == nil {
		return ""
	}
	if o.DialErr != "" {
		return "dial refused: " + o.DialErr
	}
	for _, st := range o.Steps {
		switch {
		case st.Status == 429:
			return "429 too many requests"
		case st.Status == 503:
			return "503 service unavailable"
		case st.Status == 502 || st.Status == 504:
			return fmt.Sprintf("%d upstream unavailable", st.Status)
		case st.PeerReset:
			return "connection reset by peer"
		case st.Timeout:
			return "read timeout"
		}
	}
	return ""
}

// observe folds one probe's outcome back into this host's pacing, and returns the reason in
// force. Called by every path that opens a socket, so no axis can forget it.
func (s *Scanner) observe(host string, o *probe.ConnObs) string {
	reason := slowFor(o)
	if !s.opts.Backoff {
		// REPORT WITHOUT PACING. An operator who turned this off should still be able to ask the
		// dataset "which hosts were pushing back while we ignored them" — so the reason is
		// recorded and the penalty is not. Returning the reason without storing it, which is
		// what this did first, left `BackoffReason` empty and the question unanswerable.
		if reason != "" {
			s.mu.Lock()
			s.why[host] = reason
			s.mu.Unlock()
		}
		return reason
	}
	maxB := s.opts.MaxBackoff
	if maxB <= 0 {
		maxB = DefaultMaxBackoff
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if reason == "" {
		// DECAY, NOT RESET. Halving keeps a host that alternates good and throttled answers from
		// oscillating between full speed and full penalty, which would send exactly the bursts
		// the penalty exists to stop.
		if p := s.penalty[host]; p > 0 {
			p /= 2
			if p < time.Millisecond {
				delete(s.penalty, host)
				delete(s.why, host)
			} else {
				s.penalty[host] = p
			}
		}
		return s.why[host]
	}
	p := s.penalty[host]
	if p == 0 {
		p = s.opts.Rate
		if p <= 0 {
			p = time.Second
		}
	} else {
		p *= 2
	}
	if p > maxB {
		p = maxB
	}
	s.penalty[host], s.why[host] = p, reason
	return reason
}

// BackoffReason is what this host most recently earned, for the observation row. Empty means it
// has never pushed back, which is a different fact from "we did not look".
func (s *Scanner) BackoffReason(host string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.penalty[host]; p > 0 {
		return fmt.Sprintf("%s (+%s)", s.why[host], p.Round(time.Millisecond))
	}
	if w := s.why[host]; w != "" {
		// Pushed back, but backoff is off: name it and say the gap did not move, because
		// "429, unpaced" and "429, +2s" are different things to read off a row.
		return w + " (unpaced)"
	}
	return ""
}

// Sample sends the same exact bytes N times on N fresh connections and aggregates.
//
// Fresh connections, not one reused connection: reuse would let contamination from repeat
// i leak into repeat i+1 and turn the stability gate into a desync detector by accident.
// Each repeat has to be an independent draw.
//
// It takes raw bytes rather than a URL because most injection points are not in the URL.
// A header value, a header NAME, a cookie, a JSON string — none of those survive being
// modelled as a path, and several of them are not valid HTTP once the payload lands.
func (s *Scanner) SampleRaw(ctx context.Context, u unit.Exchange, raw []byte, label string) Sample {
	return s.SampleRawN(ctx, u, raw, label, s.Repeats())
}

// SampleRawN is SampleRaw with an explicit sample size. Screening a point wants the cheapest
// sample that still supports the contamination oracle; a point that reacted earns more.
func (s *Scanner) SampleRawN(ctx context.Context, u unit.Exchange, raw []byte, label string, repeats int) Sample {
	if repeats < 1 {
		repeats = 1
	}
	out := Sample{Path: label, N: repeats, RequestRaw: raw}
	pt := probeTargetOf(u)
	if pt.Host == "" {
		out.Errors = append(out.Errors, "unit has no resolvable host")
		return out
	}

	po := probe.DefaultOptions()
	po.ReadTimeout = s.opts.Timeout
	po.DialTimeout = s.opts.Timeout
	po.KeepRawResponses = true

	var bodies [][]byte
	for i := 0; i < repeats; i++ {
		s.pace(pt.Host)
		obs := probe.Run(ctx, pt, []probe.Step{{Label: "probe", Raw: raw}}, po)
		out.Requests += obs.RequestsSent
		// The splitting axis shares the pacer with the framing axis, so it must also feed it:
		// a host throttling one of them is throttling the origin, not an axis.
		s.observe(pt.Host, obs)
		if obs.DialErr != "" {
			out.Errors = append(out.Errors, "dial: "+obs.DialErr)
			continue
		}
		if obs.TLSErr != "" {
			out.Errors = append(out.Errors, "tls: "+obs.TLSErr)
			continue
		}
		st := obs.Steps[0]
		out.Statuses = append(out.Statuses, st.Status)
		out.ElapsedMsAvg += st.ElapsedMs
		bodies = append(bodies, st.BodyPreview)
		if st.ParseErr != "" {
			out.Errors = append(out.Errors, "parse: "+st.ParseErr)
		}
		if i == 0 {
			out.HeaderNames = st.HeaderNames
			out.BodyLen = st.BodyLen
			out.BodySHA = st.BodySHA
			out.BodyPreview = printable(st.BodyPreview, 240)
			out.ContentLen = declaredContentLength(st.RespRaw)
			out.ShapeSource = st.RespRaw
		}
	}
	if n := len(out.Statuses); n > 0 {
		out.ElapsedMsAvg /= float64(n)
		out.Status = modal(out.Statuses)
		out.Stable = allSame(out.Statuses) && allSameBytes(bodies)
	}
	return out
}

// ShapeSource carries the raw response of the first repeat so anomaly detection can run
// over real bytes. It is not serialised — the bytes are large and the derived signals
// are what the dataset needs.
func (s Sample) shape() []byte { return s.ShapeSource }

// Analyze compares two samples and reports which oracles fired.
func Analyze(base, mut Sample, v vectors.Vector) Signals {
	var sig Signals
	if base.Status != mut.Status && base.Status != 0 && mut.Status != 0 {
		sig.StatusChanged, sig.StatusFrom, sig.StatusTo = true, base.Status, mut.Status
	}
	if base.BodySHA != mut.BodySHA && base.BodySHA != "" && mut.BodySHA != "" {
		sig.BodyChanged = true
		sig.BodyLenDelta = mut.BodyLen - base.BodyLen
		if !bytes.Equal(redact(base.ShapeSource, v), redact(mut.ShapeSource, v)) {
			sig.BodyChangedNormalized = true
		}
	}
	sig.NewHeaders = added(base.HeaderNames, mut.HeaderNames)

	// Terminator's ContaminationScan: repeats that disagree are the signal, whatever
	// the responses happen to look like.
	if len(mut.Statuses) > 1 && !allSame(mut.Statuses) {
		sig.Contamination = true
	}
	sig.ShapeAnomaly = ShapeAnomalies(mut.shape(), mut.ContentLen, mut.BodyLen)

	for _, fired := range []bool{sig.StatusChanged, sig.BodyChangedNormalized, sig.Contamination,
		len(sig.NewHeaders) > 0, len(sig.ShapeAnomaly) > 0} {
		if fired {
			sig.Count++
		}
	}
	return sig
}

// redact removes every trace of the injected sequence from a response BODY so two
// responses can be compared on what the server did rather than on what we sent.
//
// Two properties matter and both were got wrong first time round:
//
//	SYMMETRY — the identical redaction set is applied to both arms. Redacting 0x0A
//	  from one arm and 0x0B from the other makes them differ structurally every time,
//	  which silently turns the oracle into a constant "true".
//	BODY ONLY — redacting the folded byte from the whole raw response would erase
//	  header line endings, destroying the framing the shape oracles rely on.
func redact(raw []byte, v vectors.Vector) []byte {
	if len(raw) == 0 {
		return nil
	}
	body := raw
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		body = raw[i+4:]
	}
	folded := byte('\n')
	if v.Class == vectors.ClassCR {
		folded = '\r'
	}

	out := body
	for _, enc := range []string{v.Encoded, v.Control} {
		if enc == "" {
			continue
		}
		for _, form := range []string{enc, strings.ToLower(enc), strings.ToUpper(enc)} {
			out = bytes.ReplaceAll(out, []byte(form), []byte("<V>"))
		}
	}
	// The once-decoded form. A double-encoded vector arrives as "%250A" and is echoed
	// back as the literal text "%0A", which neither the encoded form nor the folded
	// byte matches.
	for _, enc := range []string{v.Encoded, v.Control} {
		if d := decodeOnce(enc); len(d) > 0 && !bytes.Equal(d, []byte(enc)) {
			out = bytes.ReplaceAll(out, d, []byte("<V>"))
			out = bytes.ReplaceAll(out, bytes.ToLower(d), []byte("<V>"))
		}
	}
	// Wire bytes, for a server that decoded the escape but did not fold it.
	if wire := wireBytes(v.WireBytes); len(wire) > 0 {
		ctrl := append([]byte(nil), wire...)
		ctrl[len(ctrl)-1]++
		out = bytes.ReplaceAll(out, wire, []byte("<V>"))
		out = bytes.ReplaceAll(out, ctrl, []byte("<V>"))
	}
	// The folded byte and its control twin, for a server that did.
	out = bytes.ReplaceAll(out, []byte{folded}, []byte("<V>"))
	out = bytes.ReplaceAll(out, []byte{folded + 1}, []byte("<V>"))
	return out
}

// decodeOnce applies a single percent-decode pass, mirroring what a proxy does before
// handing the path to an application that will decode it again.
func decodeOnce(s string) []byte {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				out = append(out, byte(v))
				i += 2
				continue
			}
		}
		out = append(out, s[i])
	}
	return out
}

func wireBytes(s string) []byte {
	if s == "" {
		return nil
	}
	var out []byte
	for _, f := range strings.Fields(s) {
		n, err := strconv.ParseUint(f, 16, 8)
		if err != nil {
			return nil
		}
		out = append(out, byte(n))
	}
	return out
}

// ShapeAnomalies is a port of terminator's FindingType enum. Each entry is a way a
// response can betray a desync without the scanner knowing what it was looking for.
func ShapeAnomalies(raw []byte, declaredCL, bodyLen int) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	// A status LINE, not merely the string "HTTP/1." — bodies legitimately quote
	// request lines, and counting substrings makes every such response a finding.
	if statusLineCount(raw) > 1 {
		out = append(out, "dual_response")
	}
	if declaredCL > 0 && bodyLen > 0 && bodyLen < declaredCL {
		out = append(out, "truncated_body")
	}
	if declaredCL > 0 && bodyLen > declaredCL {
		out = append(out, "cl_mismatch")
	}
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		body := raw[i+4:]
		// A body that opens with something header-shaped is a response fragment that
		// escaped its own header block.
		if bytes.HasPrefix(body, []byte("HTTP/")) || looksLikeHeaderLine(body) {
			out = append(out, "leaked_headers")
		}
		if o, c := bytes.Count(body, []byte("<html")), bytes.Count(body, []byte("</html")); o != c {
			out = append(out, "unclosed_html")
		}
	}
	return out
}

// statusLineCount counts well-formed status lines that begin a line: HTTP/x.y SP ddd.
func statusLineCount(raw []byte) int {
	n := 0
	for i := 0; i < len(raw); i++ {
		if i != 0 && raw[i-1] != '\n' {
			continue
		}
		r := raw[i:]
		if len(r) < 12 || !bytes.HasPrefix(r, []byte("HTTP/")) {
			continue
		}
		if !isDigit(r[5]) || r[6] != '.' || !isDigit(r[7]) || r[8] != ' ' {
			continue
		}
		if isDigit(r[9]) && isDigit(r[10]) && isDigit(r[11]) {
			n++
		}
	}
	return n
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func looksLikeHeaderLine(b []byte) bool {
	line := b
	if i := bytes.IndexByte(b, '\n'); i > 0 {
		line = b[:i]
	}
	if len(line) == 0 || len(line) > 200 {
		return false
	}
	name, _, ok := bytes.Cut(line, []byte(":"))
	if !ok || len(name) == 0 || bytes.ContainsAny(name, " \t") {
		return false
	}
	for _, c := range name {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '-' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func declaredContentLength(raw []byte) int {
	for _, ln := range bytes.Split(headerPart(raw), []byte("\n")) {
		name, val, ok := bytes.Cut(ln, []byte(":"))
		if ok && strings.EqualFold(strings.TrimSpace(string(name)), "content-length") {
			n, _ := strconv.Atoi(strings.TrimSpace(string(val)))
			return n
		}
	}
	return 0
}

func headerPart(raw []byte) []byte {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[:i]
	}
	return raw
}

// ------------------------------------------------------------------- helpers

func modal(xs []int) int {
	c := map[int]int{}
	best, bestN := 0, 0
	for _, x := range xs {
		c[x]++
		if c[x] > bestN {
			best, bestN = x, c[x]
		}
	}
	return best
}

func allSame(xs []int) bool {
	for i := 1; i < len(xs); i++ {
		if xs[i] != xs[0] {
			return false
		}
	}
	return true
}

func allSameBytes(xs [][]byte) bool {
	for i := 1; i < len(xs); i++ {
		if !bytes.Equal(xs[i], xs[0]) {
			return false
		}
	}
	return true
}

func added(base, mut []string) []string {
	have := map[string]bool{}
	for _, b := range base {
		have[strings.ToLower(b)] = true
	}
	var out []string
	for _, m := range mut {
		if !have[strings.ToLower(m)] {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func printable(b []byte, n int) string {
	s := strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return '.'
		}
		return r
	}, string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		s = s[:n]
	}
	return strings.TrimSpace(s)
}

// Sha is exported for the CLI's summary output.
func Sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:8])
}

var _ = net.JoinHostPort
