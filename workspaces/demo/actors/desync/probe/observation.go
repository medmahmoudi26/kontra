// Package probe implements the connection-level primitive for HTTP desync and
// header-injection detection.
//
// THE UNIT OF OBSERVATION IS A CONNECTION, NOT A REQUEST. A desync is a property of
// the socket: request N's bytes change how the server interprets request N+1. A tool
// that models "send request, get response" cannot see it. So this package dials its
// own socket, writes exact bytes, and records everything that comes back — including
// which step the peer hung up on, and how many bytes it managed first.
//
// IT EMITS OBSERVATIONS, NOT VERDICTS. Nothing here decides "this is CL.0" or "this
// is request splitting". Classification happens later, in SQL, against the dataset.
// That split is the whole point: re-scanning 37k hosts costs hours and goodwill,
// re-querying a dataset costs nothing. Every time detection logic improves, it should
// be re-run over data already on disk — which is only possible if the scanner never
// threw the raw bytes away.
//
// WHY net/http.Transport IS NOT USED. Transport owns a connection pool. It decides
// which socket a request lands on, when to retry on a new connection, and when to
// close one. Every one of those decisions destroys the signal being measured. httptrace
// reports what the pool did only after the fact. So the socket is dialed here and the
// fork is used purely for serialization and best-effort parsing.
//
// ON THE FORK (github.com/sw33tLie/http). Its value is entirely on the WRITE path:
// ValidHeaderFieldName and ValidHeaderFieldValue are patched to return true, so
// malformed header names and values survive. Its READ path is stock — ReadResponse
// still uses stdlib net/textproto and still rejects malformed status lines and header
// blocks. That is exactly why StepObs.RespRaw exists and is populated before any parse
// is attempted: when a desync fires, the interesting responses are the unparseable ones.
package probe

import (
	"net"
	"strconv"
	"time"
)

// Mode controls how steps are ordered on the wire.
type Mode int

const (
	// ModeSequential writes a step, reads its response, then moves to the next.
	// Use for differential probes, where each response must be unambiguously
	// attributable to one request.
	ModeSequential Mode = iota

	// ModePipelined writes every step back-to-back, then reads. Required for CL.0
	// and every "send the group on one connection" case in the corpus: the smuggled
	// prefix has to already be sitting in the server's socket buffer when the victim
	// request arrives. Reading between writes gives the server time to resynchronise
	// and the bug disappears.
	ModePipelined
)

func (m Mode) String() string {
	if m == ModePipelined {
		return "pipelined"
	}
	return "sequential"
}

// Target is one endpoint. Port is explicit because several findings in the corpus
// existed on :80 but not :443, and one existed only on backend IPs behind the edge —
// so Host may legitimately be an IP with SNI/Host-header set to something else.
type Target struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Scheme string `json:"scheme"` // "http" | "https"

	// SNI overrides the TLS server name. Empty means use Host (and skip SNI entirely
	// when Host is an IP literal, which is what a browser does).
	SNI string `json:"sni,omitempty"`

	// ALPN is what we OFFER. Default is http/1.1 only. Offering h2 and then writing
	// HTTP/1.1 bytes is a category error, so the caller must opt in deliberately.
	ALPN []string `json:"alpn,omitempty"`
}

func (t Target) addr() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

// Step is one request, as exact bytes. There is deliberately no Method/Header/Body
// struct here: the moment a request is modelled as fields, something downstream
// normalises it. The corpus payloads depend on header order, on a space before the
// colon, on a tab, on no colon at all, and on a bare LF where a CRLF belongs.
type Step struct {
	Label string `json:"label"` // "baseline" | "mutated" | "canary" | free-form
	Raw   []byte `json:"-"`
}

// StepObs is what was observed for one step. Sizes and hashes are always populated;
// RespRaw is populated when Options.KeepRawResponses is set.
type StepObs struct {
	Label string `json:"label"`

	// Request side.
	SentBytes int    `json:"sent_bytes"`
	SentSHA   string `json:"sent_sha"`
	WriteErr  string `json:"write_err,omitempty"`

	// Timing. TTFB is measured from the end of this step's write.
	TTFBMs    float64 `json:"ttfb_ms"`
	ElapsedMs float64 `json:"elapsed_ms"`

	// Response side. RespRaw is the exact byte range the parser consumed for this
	// step — the ground truth. Everything below it is derived and may be absent.
	RespRaw   []byte `json:"resp_raw,omitempty"`
	RespBytes int    `json:"resp_bytes"`
	RespSHA   string `json:"resp_sha,omitempty"`

	Status      int      `json:"status,omitempty"`
	Proto       string   `json:"proto,omitempty"`
	HeaderNames []string `json:"header_names,omitempty"` // in wire order, case preserved
	HeaderSHA   string   `json:"header_sha,omitempty"`
	BodyLen     int      `json:"body_len"`
	BodySHA     string   `json:"body_sha,omitempty"`
	BodyPreview []byte   `json:"body_preview,omitempty"`

	// Failure modes. A parse error with a non-empty RespRaw is a signal, not noise:
	// it is what a stacked or truncated response looks like.
	ParseErr string `json:"parse_err,omitempty"`
	ReadErr  string `json:"read_err,omitempty"`

	// Socket-level events, attributed to the step they occurred on.
	PeerClosed bool `json:"peer_closed,omitempty"`
	PeerReset  bool `json:"peer_reset,omitempty"`
	Timeout    bool `json:"timeout,omitempty"`
}

// ConnObs is one connection's worth of observation — the row that lands in the dataset.
type ConnObs struct {
	Target Target `json:"target"`
	Mode   string `json:"mode"`

	// Connection establishment.
	DialMs  float64 `json:"dial_ms"`
	DialErr string  `json:"dial_err,omitempty"`
	TLSMs   float64 `json:"tls_ms,omitempty"`
	TLSErr  string  `json:"tls_err,omitempty"`

	// Negotiated TLS facts. ALPN matters: if it came back "h2" the HTTP/1.1 bytes
	// written below were nonsense, and any finding derived from them is invalid.
	ALPN        string `json:"alpn,omitempty"`
	TLSVersion  string `json:"tls_version,omitempty"`
	CipherSuite string `json:"cipher_suite,omitempty"`

	Steps []StepObs `json:"steps"`

	// StreamBytes is the total read off the socket across all steps. It will exceed
	// the sum of per-step RespBytes when the server sent trailing data nobody asked
	// for — which is itself one of the more interesting things that can happen.
	StreamBytes  int     `json:"stream_bytes"`
	StreamSHA    string  `json:"stream_sha,omitempty"`
	TrailingRaw  []byte  `json:"trailing_raw,omitempty"`
	ClosedAfter  int     `json:"closed_after_step"` // -1 if the peer never closed
	StartedAtUTC string  `json:"started_at_utc"`
	TotalMs      float64 `json:"total_ms"`

	// RequestsSent is how many of this connection's steps actually reached the wire.
	//
	// IT IS NOT len(Steps), AND THAT IS THE ENTIRE REASON IT EXISTS. A connection that fails to
	// dial sends nothing and still carries three planned steps; a connection whose peer hangs up
	// after the first sends one. Counting rows and multiplying by the step count over-reports the
	// first case and under-reports every escalation, and a scan that cannot say how much traffic
	// it generated is one nobody can hold to a rate limit or an authorisation.
	RequestsSent int `json:"requests_sent"`
}

// Options are the dials. Defaults are deliberately conservative: this runs against
// third-party production infrastructure under bug bounty scope.
type Options struct {
	Mode Mode

	DialTimeout  time.Duration
	WriteTimeout time.Duration
	ReadTimeout  time.Duration // per read, reset before each

	// MaxBodyBytes caps how much of each response body is drained. The body still has
	// to be consumed to position the reader at the next response, so a cap that is too
	// low will desynchronise the PARSER (not the server) on large bodies.
	MaxBodyBytes int64

	// MaxStreamBytes caps total retained bytes per connection, so one chatty target
	// cannot blow up a batch.
	MaxStreamBytes int

	// BodyPreviewBytes is how much body prefix to retain for querying even when
	// KeepRawResponses is off.
	BodyPreviewBytes int

	KeepRawResponses bool

	// InsecureSkipVerify is the norm here: probing by backend IP with a mismatched
	// SNI is a documented technique in this corpus, and a cert error would mask it.
	InsecureSkipVerify bool
}

// DefaultOptions is what the actor should use unless it has a reason not to.
func DefaultOptions() Options {
	return Options{
		Mode:               ModeSequential,
		DialTimeout:        6 * time.Second,
		WriteTimeout:       6 * time.Second,
		ReadTimeout:        8 * time.Second,
		MaxBodyBytes:       256 << 10,
		MaxStreamBytes:     1 << 20,
		BodyPreviewBytes:   512,
		KeepRawResponses:   true,
		InsecureSkipVerify: true,
	}
}
