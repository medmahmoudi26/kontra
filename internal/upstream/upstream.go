// Package upstream is how dispatch reaches a service (spec/relay.md §6.1–§6.2, §6.6).
//
// A Backend is either Direct (a2a: {url}, M1) or a tunnel session (M2, spec/tunnel.md §8). Both give an
// *http.Client whose dialer goes where the backend says — the SSRF-safe dialer for direct backends, a
// fresh tunnel stream per exchange for tunnels — so the a2a-go client and the card fetch above them do
// not change between milestones. Every upstream body is read through a byte limit.
//
// A direct backend's configured URL is the base, not the A2A endpoint (D71): the card is fetched relative
// to its path, and every request goes to the configured scheme://host:port with only the path, binding and
// protocol version taken from the upstream card. The backend's dialer refuses any other host:port, so a
// seller-controlled card value never steers it.
package upstream

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"

	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/tunnel"
)

// Upstream limits (spec §6.6). Exceeding one fails the exchange upstream_error (service_unavailable for a
// card).
const (
	MaxBody     = 8 << 20   // a non-streaming upstream body
	MaxSSEEvent = 4 << 20   // one SSE event
	MaxCard     = 256 << 10 // a card
	// MaxPart is the largest part stored inline (spec §10.4), measured on the decoded bytes. Without a
	// bucket, an upstream part over it fails the task upstream_error.
	MaxPart = 256 << 10
)

// Errors.
var (
	// ErrLimit is an upstream body, SSE event or card over its limit (spec §6.6).
	ErrLimit = errors.New("upstream: limit exceeded")
	// ErrSteered is a connection the backend refused because it names another host:port than the
	// configured one (D71).
	ErrSteered = errors.New("upstream: a direct backend dials only its configured host:port")
	// ErrNoCard is a card fetch that found no card at either well-known path.
	ErrNoCard = errors.New("upstream: no agent card")
	// ErrBadCard is a card that is not a JSON object, or that names no interface the gate can speak.
	ErrBadCard = errors.New("upstream: unusable agent card")
)

// Kind is attempts.backend.
type Kind string

// Backend kinds.
const (
	KindDirect Kind = "direct"
	KindTunnel Kind = "tunnel" // M2
)

// Backend is one way to reach one service.
type Backend interface {
	Kind() Kind
	// Origin is the scheme://host:port every upstream request is sent to, whatever the upstream card's
	// interface URL says: a card never steers the dialer.
	Origin() *url.URL
	// Base is the configured URL, origin and path: the card is fetched relative to its path (D71).
	Base() *url.URL
	// HTTPClient sends upstream requests: HTTP/1.1, no client timeout, bodies through the byte limits.
	HTTPClient() *http.Client
}

// Direct is an a2a: {url} backend on the operator's network.
type Direct struct {
	URL          *url.URL // the configured base URL
	AllowPrivate bool
	origin       *url.URL
	hostPort     string
	client       *http.Client
}

var _ Backend = (*Direct)(nil)

// NewDirect returns the backend for a configured URL, with an ssrf.Dialer of its allow_private policy
// narrowed to the URL's own host:port.
func NewDirect(rawURL string, allowPrivate bool) (*Direct, error) {
	return newDirect(rawURL, ssrf.Policy{AllowPrivate: allowPrivate}, nil)
}

// newDirect is NewDirect with the dial below the host:port check replaceable (tests).
func newDirect(rawURL string, pol ssrf.Policy, dial func(ctx context.Context, network, address string) (net.Conn, error)) (*Direct, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("upstream: backend url: %w", err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if err := pol.CheckScheme(u); err != nil {
		return nil, fmt.Errorf("upstream: backend url %s: %w", rawURL, err)
	}
	if u.User != nil || u.Opaque != "" || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("upstream: backend url %s: want scheme://host[:port][/path], without userinfo, query or fragment", rawURL)
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("upstream: backend url %s: bad port", rawURL)
	}
	host := strings.ToLower(u.Hostname())
	d := &Direct{
		URL:          u,
		AllowPrivate: pol.AllowPrivate,
		origin:       &url.URL{Scheme: u.Scheme, Host: u.Host},
		hostPort:     net.JoinHostPort(host, port),
	}
	if dial == nil {
		dial = ssrf.NewDialer(pol).DialContext
	}
	pinned := func(ctx context.Context, network, address string) (net.Conn, error) {
		h, p, err := net.SplitHostPort(address)
		if err != nil || net.JoinHostPort(strings.ToLower(h), p) != d.hostPort {
			return nil, fmt.Errorf("%w: %s, not %s", ErrSteered, address, d.hostPort)
		}
		return dial(ctx, network, address)
	}
	d.client = &http.Client{
		Transport:     &limitTransport{next: ssrf.NewTransportWithDial(pol, pinned)},
		CheckRedirect: ssrf.NoRedirects,
	}
	return d, nil
}

// Kind implements Backend.
func (d *Direct) Kind() Kind { return KindDirect }

// Origin implements Backend: scheme://host[:port] of the configured URL.
func (d *Direct) Origin() *url.URL { u := *d.origin; return &u }

// Base implements Backend: the configured URL.
func (d *Direct) Base() *url.URL { u := *d.URL; return &u }

// HTTPClient implements Backend.
func (d *Direct) HTTPClient() *http.Client { return d.client }

// Exchange records what the last upstream HTTP response under a context said: its status and Retry-After.
// a2a-go's JSON-RPC transport turns a non-2xx status into an error that keeps only the status text, so
// the relay reads the status here to apply §6.4's table (502/503/504 unreachable, 408/429 retried
// honouring Retry-After, other 4xx an upstream error).
type Exchange struct {
	mu         sync.Mutex
	status     int
	retryAfter time.Duration
	limit      bool
	broken     bool
	// tunnelled is an exchange made through a tunnel backend, and node the Kontra-Node header of its last
	// response: a response the node wrote itself (tunnel.md §8.2). Only a tunnel's transport reads the
	// header, so a direct service cannot pass its own answer off as a node's.
	tunnelled bool
	node      tunnel.NodeReason
}

type exchangeKey struct{}

// WithExchange returns ctx carrying a fresh Exchange that every request made with it fills.
func WithExchange(ctx context.Context) (context.Context, *Exchange) {
	ex := &Exchange{}
	return context.WithValue(ctx, exchangeKey{}, ex), ex
}

func exchangeFrom(ctx context.Context) *Exchange {
	ex, _ := ctx.Value(exchangeKey{}).(*Exchange)
	return ex
}

// Status is the status of the last response (0 when none arrived) and its Retry-After.
func (e *Exchange) Status() (int, time.Duration) {
	if e == nil {
		return 0, 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status, e.retryAfter
}

// Reset forgets the last response, before the next call.
func (e *Exchange) Reset() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.status, e.retryAfter, e.limit, e.broken, e.node = 0, 0, false, false, ""
	e.mu.Unlock()
}

// Node is the Kontra-Node header of the last response through a tunnel backend: "" when the response is
// the service's own, or when none arrived (tunnel.md §8.2).
func (e *Exchange) Node() tunnel.NodeReason {
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.node
}

// Tunnelled reports whether the last call under this exchange went through a tunnel backend.
func (e *Exchange) Tunnelled() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tunnelled
}

func (e *Exchange) markTunnelled() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.tunnelled = true
	e.mu.Unlock()
}

func (e *Exchange) recordNode(n tunnel.NodeReason) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.node = n
	e.mu.Unlock()
}

// LimitExceeded reports whether a body read under this exchange ran over a limit of §6.6.
func (e *Exchange) LimitExceeded() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.limit
}

func (e *Exchange) record(status int, retryAfter time.Duration) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.status, e.retryAfter = status, retryAfter
	e.mu.Unlock()
}

// bodyBroken records a response body whose connection failed before its end (not a clean EOF).
func (e *Exchange) bodyBroken() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.broken = true
	e.mu.Unlock()
}

// Broken reports whether a response body under this exchange was cut by its connection.
func (e *Exchange) Broken() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.broken
}

func (e *Exchange) overLimit() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.limit = true
	e.mu.Unlock()
}

// RetryAfter parses a Retry-After value: delay-seconds or an HTTP date. Zero when absent or unusable.
func RetryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if n, err := strconv.ParseInt(h, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		return time.Duration(min(n, 24*3600)) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil && t.After(now) {
		return min(t.Sub(now), 24*time.Hour)
	}
	return 0
}

// limitTransport reads every upstream body through a limit: an SSE stream event by event (MaxSSEEvent),
// anything else as a whole (MaxBody). It also records the response in the request's Exchange; a tunnel's
// (tunnel set) marks the exchange tunnelled before the stream is opened, records the response's
// Kontra-Node header, and hands the request's context to the dial that opens the stream
// (withRequestContext), which bounds the open by the request.
type limitTransport struct {
	next   http.RoundTripper
	tunnel bool
}

func (t *limitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ex := exchangeFrom(r.Context())
	if t.tunnel {
		ex.markTunnelled()
		r = withRequestContext(r)
	}
	resp, err := t.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	ex.record(resp.StatusCode, RetryAfter(resp.Header.Get("Retry-After"), time.Now()))
	if t.tunnel {
		ex.recordNode(tunnel.NodeReason(strings.TrimSpace(resp.Header.Get(tunnel.HeaderNode))))
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt == "text/event-stream" {
		resp.Body = &sseLimitReader{rc: resp.Body, max: MaxSSEEvent, ex: ex, atLineStart: true}
	} else {
		resp.Body = &bodyLimitReader{rc: resp.Body, left: MaxBody, ex: ex}
	}
	return resp, nil
}

// bodyLimitReader fails a body longer than its limit with ErrLimit.
type bodyLimitReader struct {
	rc   io.ReadCloser
	left int64
	ex   *Exchange
}

func (b *bodyLimitReader) Read(p []byte) (int, error) {
	if b.left < 0 {
		return 0, fmt.Errorf("%w: body over %d bytes", ErrLimit, MaxBody)
	}
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.rc.Read(p)
	if err != nil && err != io.EOF {
		b.ex.bodyBroken()
	}
	b.left -= int64(n)
	if b.left < 0 {
		b.ex.overLimit()
		return n + int(b.left), fmt.Errorf("%w: body over %d bytes", ErrLimit, MaxBody)
	}
	return n, err
}

func (b *bodyLimitReader) Close() error { return b.rc.Close() }

// sseLimitReader fails a stream one of whose events (the bytes between two blank lines) is longer than
// its limit with ErrLimit. Lines end in LF, CR or CRLF (WHATWG SSE).
type sseLimitReader struct {
	rc          io.ReadCloser
	max         int
	cur         int
	atLineStart bool
	lastCR      bool
	ex          *Exchange
	err         error
}

func (s *sseLimitReader) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.rc.Read(p)
	if err != nil && err != io.EOF {
		s.ex.bodyBroken()
	}
	for i := range n {
		c := p[i]
		switch {
		case c == '\n' && s.lastCR:
			s.lastCR = false
			continue // the LF of a CRLF
		case c == '\n' || c == '\r':
			s.lastCR = c == '\r'
			if s.atLineStart {
				s.cur = 0 // a blank line ends the event
			}
			s.atLineStart = true
			continue
		}
		s.lastCR = false
		s.atLineStart = false
		s.cur++
		if s.cur > s.max {
			s.ex.overLimit()
			s.err = fmt.Errorf("%w: one SSE event over %d bytes", ErrLimit, s.max)
			return i, s.err
		}
	}
	return n, err
}

func (s *sseLimitReader) Close() error { return s.rc.Close() }

// Info is what dispatch needs to speak to an upstream, read from its card (for 0.x cards through
// a2a-go's a2av0 parser, which is never used to produce the served card, D12).
type Info struct {
	ProtocolVersion a2a.ProtocolVersion // "1.0", or "0.3" for the a2av0 transports
	Binding         a2a.TransportProtocol
	Path            string // the interface URL's path, joined to the backend's origin
	Tenant          string // the interface's A2A tenant, if the seller's card names one
	Streaming       bool
	Extensions      []string // URIs the upstream card declares
	PushToUpstream  bool     // always false: the gate is the agent's only client (spec §10.1)
}

// cardV1 is what ReadInfo reads of a 1.0 card.
type cardV1 struct {
	SupportedInterfaces []*a2a.AgentInterface `json:"supportedInterfaces"`
	Capabilities        struct {
		Streaming  bool `json:"streaming"`
		Extensions []struct {
			URI string `json:"uri"`
		} `json:"extensions"`
	} `json:"capabilities"`
}

// ReadInfo parses the upstream card bytes for Info. A 1.0 card (with supportedInterfaces) is read as is;
// a pre-1.0 card goes through a2av0's parser, with preferredTransport defaulting to JSONRPC as a 0.2.x
// card without it means (spec §3, D12). The interface chosen is the newest protocol version the gate
// speaks, JSONRPC before HTTP+JSON; only its path, binding, version and tenant are kept, never its host.
func ReadInfo(raw []byte) (Info, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return Info{}, fmt.Errorf("%w: not a JSON object", ErrBadCard)
	}
	var card cardV1
	if _, v1 := top["supportedInterfaces"]; v1 {
		if err := json.Unmarshal(raw, &card); err != nil {
			return Info{}, fmt.Errorf("%w: %v", ErrBadCard, err)
		}
	} else {
		if _, ok := top["preferredTransport"]; !ok {
			top["preferredTransport"] = json.RawMessage(`"JSONRPC"`)
		}
		fixed, err := json.Marshal(top)
		if err != nil {
			return Info{}, fmt.Errorf("%w: %v", ErrBadCard, err)
		}
		parsed, err := a2av0.NewAgentCardParser()(fixed)
		if err != nil {
			return Info{}, fmt.Errorf("%w: %v", ErrBadCard, err)
		}
		card.SupportedInterfaces = parsed.SupportedInterfaces
		for _, i := range card.SupportedInterfaces {
			i.ProtocolVersion = a2av0.Version
		}
		card.Capabilities.Streaming = parsed.Capabilities.Streaming
		for _, e := range parsed.Capabilities.Extensions {
			card.Capabilities.Extensions = append(card.Capabilities.Extensions, struct {
				URI string `json:"uri"`
			}{URI: e.URI})
		}
	}
	iface, err := pickInterface(card.SupportedInterfaces)
	if err != nil {
		return Info{}, err
	}
	u, err := url.Parse(iface.URL)
	if err != nil {
		return Info{}, fmt.Errorf("%w: interface url: %v", ErrBadCard, err)
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	info := Info{
		ProtocolVersion: iface.ProtocolVersion,
		Binding:         iface.ProtocolBinding,
		Path:            path,
		Tenant:          iface.Tenant,
		Streaming:       card.Capabilities.Streaming,
	}
	for _, e := range card.Capabilities.Extensions {
		if e.URI != "" && !slices.Contains(info.Extensions, e.URI) {
			info.Extensions = append(info.Extensions, e.URI)
		}
	}
	return info, nil
}

// pickInterface prefers protocol version 1 over 0, then JSONRPC over HTTP+JSON; gRPC is not spoken.
func pickInterface(ifaces []*a2a.AgentInterface) (*a2a.AgentInterface, error) {
	var best *a2a.AgentInterface
	rank := func(i *a2a.AgentInterface) int {
		r := 0
		v := string(i.ProtocolVersion)
		switch {
		case v == "" || v == "1" || v == "1.0" || strings.HasPrefix(v, "1."):
			r += 10
		case strings.HasPrefix(v, "0."):
		default:
			return -1
		}
		switch i.ProtocolBinding {
		case a2a.TransportProtocolJSONRPC:
			r += 2
		case a2a.TransportProtocolHTTPJSON:
			r++
		default:
			return -1
		}
		return r
	}
	for _, i := range ifaces {
		if i == nil || i.URL == "" || rank(i) < 0 {
			continue
		}
		if best == nil || rank(i) > rank(best) {
			best = i
		}
	}
	if best == nil {
		return nil, fmt.Errorf("%w: no JSONRPC or HTTP+JSON interface of protocol 1.0 or 0.x", ErrBadCard)
	}
	out := *best
	if out.ProtocolVersion == "" || out.ProtocolVersion == "1" {
		out.ProtocolVersion = a2a.Version
	}
	return &out, nil
}

// extensionsHeader sends the activated URIs the upstream declares in one comma-separated A2A-Extensions
// header (spec §2.4, §6.2).
type extensionsHeader struct {
	a2aclient.PassthroughInterceptor
	uris string
}

func (e extensionsHeader) Before(ctx context.Context, req *a2aclient.Request) (context.Context, any, error) {
	req.ServiceParams.Append("A2A-Extensions", e.uris)
	return ctx, nil, nil
}

// Endpoint is the URL dispatch calls: the backend's origin with the interface path of the card.
func Endpoint(b Backend, info Info) string {
	u := b.Origin()
	path := info.Path
	if path == "" {
		path = "/"
	}
	pu, err := url.Parse(path)
	if err == nil {
		u.Path, u.RawPath = pu.Path, pu.RawPath
	}
	return u.String()
}

// NewClient returns an a2a-go client for the upstream over the backend: JSON-RPC or HTTP+JSON transport
// (a2av0 transports for 0.3 upstreams), A2A-Version for the upstream's version, A2A-Extensions limited to
// the activated URIs the upstream declares, and never the caller's Authorization, cookies or other
// headers, nor a gate credential (spec §6.2): the client sends only what a2a-go sets for the call.
func NewClient(ctx context.Context, b Backend, info Info, activated []string) (*a2aclient.Client, error) {
	hc := b.HTTPClient()
	iface := &a2a.AgentInterface{
		URL: Endpoint(b, info), ProtocolBinding: info.Binding, ProtocolVersion: info.ProtocolVersion, Tenant: info.Tenant,
	}
	if iface.ProtocolVersion == "" {
		iface.ProtocolVersion = a2a.Version
	}
	opts := []a2aclient.FactoryOption{
		a2aclient.WithDefaultsDisabled(),
		a2aclient.WithConfig(a2aclient.Config{DisableTenantPropagation: true}),
		a2aclient.WithJSONRPCTransport(hc),
		a2aclient.WithRESTTransport(hc),
		a2av0.WithJSONRPCTransport(a2av0.JSONRPCTransportConfig{Client: hc}),
		a2av0.WithRESTTransport(a2av0.RESTTransportConfig{Client: hc}),
	}
	var send []string
	for _, uri := range activated {
		if slices.Contains(info.Extensions, uri) && !slices.Contains(send, uri) {
			send = append(send, uri)
		}
	}
	if len(send) > 0 {
		opts = append(opts, a2aclient.WithCallInterceptors(extensionsHeader{uris: strings.Join(send, ", ")}))
	}
	c, err := a2aclient.NewFromEndpoints(ctx, []*a2a.AgentInterface{iface}, opts...)
	if err != nil {
		return nil, fmt.Errorf("upstream: client for %s: %w", iface.URL, err)
	}
	return c, nil
}

// Fetched is an upstream card exactly as fetched.
type Fetched struct {
	Raw  []byte
	Path string // the path it was fetched from: …/.well-known/agent-card.json, or …/agent.json for 0.2.x sellers
}

// Well-known card paths, relative to the backend's base (D71).
const (
	CardPath       = ".well-known/agent-card.json"
	LegacyCardPath = ".well-known/agent.json"
)

// FetchCard GETs .well-known/agent-card.json relative to the backend's base, then .well-known/agent.json
// when the first answers a 4xx, reading at most MaxCard bytes (spec §3). A redirect is not followed.
func FetchCard(ctx context.Context, b Backend) (*Fetched, error) {
	var errs []error
	for _, p := range []string{CardPath, LegacyCardPath} {
		u := b.Base().JoinPath(p)
		f, status, err := fetchOne(ctx, b.HTTPClient(), u)
		if err == nil {
			return f, nil
		}
		errs = append(errs, err)
		if status < 400 || status >= 500 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
			break // not a "no card here" answer: the second path would not tell more
		}
	}
	return nil, errors.Join(errs...)
}

func fetchOne(ctx context.Context, hc *http.Client, u *url.URL) (*Fetched, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("upstream: card %s: %w", u.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, resp.StatusCode, fmt.Errorf("%w at %s: HTTP %d", ErrNoCard, u.Path, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxCard+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("upstream: card %s: %w", u.Path, err)
	}
	if len(raw) > MaxCard {
		return nil, resp.StatusCode, fmt.Errorf("%w: card at %s is over %d bytes", ErrLimit, u.Path, MaxCard)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, resp.StatusCode, fmt.Errorf("%w: card at %s is not a JSON object", ErrBadCard, u.Path)
	}
	return &Fetched{Raw: raw, Path: u.EscapedPath()}, resp.StatusCode, nil
}

// Class is how dispatch treats a failed upstream exchange (spec §6.4).
type Class int

// Classes of failure.
const (
	// Unreachable: connection refused, reset or closed, DNS, TLS, an address the dialer refused, or HTTP
	// 502/503/504. Retried before the first upstream task id; upstream_unreachable after it.
	Unreachable Class = iota + 1
	// Throttled: HTTP 408 or 429. Retried before the first upstream task id, honouring Retry-After;
	// upstream_error after it.
	Throttled
	// Protocol: a JSON-RPC error, another HTTP status, a malformed body. upstream_error with the
	// upstream's message, never retried.
	Protocol
	// Limit: an upstream limit of §6.6 exceeded. upstream_error.
	Limit
	// Canceled: the dispatch's own context ended (an abort, the activity's end).
	Canceled
)

// Failure is a classified upstream error.
type Failure struct {
	Class      Class
	RetryAfter time.Duration
	Message    string // the upstream's message (Protocol), capped; a short description otherwise
	Err        error
}

// MaxMessage caps an upstream error text the gate repeats in a status message or a Temporal result.
const MaxMessage = 1 << 10

// Classify maps an error from an upstream call made under ctx (and its Exchange) to §6.4's classes.
//
// Through a tunnel (relay.md §6.4, tunnel.md §8.2) the node is the only peer the gate talks to, so an
// exchange that got no complete HTTP response (the stream could not be opened, the session closed, the
// body was cut) is ClassTunnelLost, never Unreachable; a response the node wrote itself is told by its
// Kontra-Node header: full is ClassNodeFull, sealed_rejected ClassSealedRejected, and any other
// (not_hosted, kind_mismatch, target_unreachable) ClassTunnelLost. A response without the header is the
// service's own and is classified as a direct backend's: its own 503 is Unreachable.
func Classify(ctx context.Context, err error, ex *Exchange) Failure {
	f := Failure{Err: err}
	status, retryAfter := ex.Status()
	node, tunnelled := ex.Node(), ex.Tunnelled()
	var a2aErr *a2a.Error
	switch {
	case ctx.Err() != nil:
		f.Class, f.Message = Canceled, "the dispatch ended"
	case errors.Is(err, ErrLimit) || ex.LimitExceeded():
		f.Class, f.Message = Limit, "the service sent more than the gate accepts (spec §6.6)"
	case tunnelled && node == tunnel.NodeFull:
		f.Class, f.Message = ClassNodeFull, "the node hosting the service is at its limit of exchanges in flight"
	case tunnelled && node == tunnel.NodeSealedRejected:
		f.Class, f.Message = ClassSealedRejected, "the node hosting the service refused a sealed part"
	case tunnelled && node != "":
		f.Class, f.Message = ClassTunnelLost, nodeMessage(node)
	case tunnelled && (status == 0 || ex.Broken() || errors.Is(err, ErrTunnelLost)):
		f.Class, f.Message = ClassTunnelLost, "the tunnel session to the service was lost"
	case status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout:
		f.Class, f.Message = Unreachable, fmt.Sprintf("the service answered HTTP %d", status)
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests:
		f.Class, f.RetryAfter, f.Message = Throttled, retryAfter, fmt.Sprintf("the service answered HTTP %d", status)
	case errors.As(err, &a2aErr):
		f.Class, f.Message = Protocol, a2aErr.Error()
	case status >= 300:
		f.Class, f.Message = Protocol, fmt.Sprintf("the service answered HTTP %d", status)
	case status == 0 || ex.Broken() || unreachable(err):
		// No response at all (refused, reset, DNS, TLS, a refused address), or a body cut by its connection.
		f.Class, f.Message = Unreachable, "the service could not be reached"
	default:
		f.Class, f.Message = Protocol, "the service sent a malformed response: "+err.Error()
	}
	f.Message = Cap(f.Message)
	return f
}

// nodeMessage is the gate's words for a response a node wrote itself; a value outside tunnel.md §8.2's list
// is not repeated (the node's text never reaches the task's log).
func nodeMessage(n tunnel.NodeReason) string {
	switch n {
	case tunnel.NodeNotHosted:
		return "the node does not host the service on this session (Kontra-Node: not_hosted)"
	case tunnel.NodeKindMismatch:
		return "the node hosts the service with another kind (Kontra-Node: kind_mismatch)"
	case tunnel.NodeTargetUnreachable:
		return "the node could not reach the service (Kontra-Node: target_unreachable)"
	}
	return "the node hosting the service answered for it"
}

// Cap trims s to MaxMessage bytes on a rune boundary, as valid UTF-8.
func Cap(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= MaxMessage {
		return s
	}
	cut := MaxMessage
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// unreachable reports a transport failure: no connection, a dropped one, TLS, or a refused address.
func unreachable(err error) bool {
	var (
		opErr   *net.OpError
		dnsErr  *net.DNSError
		certErr x509.UnknownAuthorityError
		hostErr x509.HostnameError
		invErr  x509.CertificateInvalidError
	)
	switch {
	case errors.As(err, &opErr), errors.As(err, &dnsErr), errors.As(err, &certErr), errors.As(err, &hostErr),
		errors.As(err, &invErr):
		return true
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE),
		errors.Is(err, ssrf.ErrAddress), errors.Is(err, ErrSteered),
		errors.Is(err, net.ErrClosed):
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "tls: ") || strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "server closed")
}
