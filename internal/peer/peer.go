// Package peer is the replica-to-replica hop of spec/relay.md §11.2 (D163). Only the replica holding a tunnel
// session can open a stream on it, and a plain MCP request has no Temporal activity to carry it there: a
// replica that receives a request for a tunnel session another replica holds forwards the one HTTP/1.1 exchange
// to that replica's peer listener (peer.listen, advertised in gate_replicas.peer_addr), which runs §6.1's check
// on its own registry and the exchange on its own tunnel and streams the response back unchanged.
//
// The receiving replica applies every other rule of §11 once, so the hop carries no credential: the client's
// Authorization is never forwarded. What the holding replica trusts is the Kontra-Peer header, an assertion of
// the principal, the tunnel session, the service, the preamble's MCP fields (spec/tunnel.md §8.1), the
// upstream's Mcp-Session-Id, the SHA-256 of the other forwarded headers (ForwardedHeaders), a nonce and the
// time, with an HMAC-SHA256 under the shared secret (peer.secret_env, at least 32 bytes) over the assertion, the
// method, the path and the body's SHA-256 (D179). One older than MaxAge, or whose nonce was seen within MaxAge,
// is refused, and so is a request whose headers do not hash to the assertion's. The holding replica builds the
// upstream request from signed data only: Mcp-Session-Id from UpstreamSID, and only the headers ForwardedHeaders
// keeps. The listener serves Path only, answers anything else 404, and is never the public listener; its
// address must be reachable only from the gate's replicas, or carry TLS, since the HMAC authenticates a request
// and does not hide it.
//
// mcp_sessions.replica_id names the replica holding the tunnel session (NULL for a direct backend), never the
// one the client reached, so the reaper of a dead replica closes only the sessions whose tunnels it held.
//
// Work package: M3 WP-04.
package peer

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Fixed values of the hop.
const (
	Header = "Kontra-Peer"
	// Path is the one route of the peer listener.
	Path = "/v1/peer/mcp"
	// MaxAge bounds an assertion's age; nonces are remembered that long.
	MaxAge = 30 * time.Second
	// MinSecret is the shortest shared secret accepted.
	MinSecret = 32
	// HeaderGone is the holding replica's answer when §6.1's check refuses the session (a 404 with no body):
	// GoneTunnelLost or GoneRevoked, the close_reason the receiving replica writes on the pinned row.
	HeaderGone     = "Kontra-Peer-Gone"
	GoneTunnelLost = "tunnel_lost"
	GoneRevoked    = "revoked"
	// MaxBody bounds a forwarded body when the server is given no limit (the receiving replica's edge has
	// already applied limits.body).
	MaxBody = 16 << 20
	// dialTimeout bounds the connection to another replica's peer listener.
	dialTimeout = 5 * time.Second
	// macDomain separates the hop's MACs from any other use of the secret.
	macDomain = "kontra-peer-v1"
)

// Errors of Verify. Each is a 401 at the peer listener, and no stream is opened.
var (
	ErrForged   = errors.New("peer: the Kontra-Peer assertion does not verify")
	ErrStale    = errors.New("peer: the Kontra-Peer assertion is older than 30 s, or dated in the future")
	ErrReplayed = errors.New("peer: the Kontra-Peer nonce was already seen")
	ErrHeaders  = errors.New("peer: the forwarded headers do not hash to the assertion's")
)

// Assertion is what the receiving replica vouches for.
type Assertion struct {
	Replica       string    `json:"replica"` // the receiving replica's id
	TenantID      uuid.UUID `json:"tenant"`
	Service       string    `json:"service"`
	TunnelSession uuid.UUID `json:"tunnel_session"`
	Principal     string    `json:"principal"`
	BuyerID       uuid.UUID `json:"buyer"`
	Via           string    `json:"via"`
	MCPSession    string    `json:"mcp_session,omitempty"`    // tunnel.md §8.1
	MCPRequestID  string    `json:"mcp_request_id,omitempty"` // tunnel.md §8.1
	UpstreamSID   string    `json:"upstream_sid,omitempty"`   // the upstream's Mcp-Session-Id the holder sends
	Headers       string    `json:"headers"`                  // hex SHA-256 of ForwardedHeaders' canonical form
	Nonce         string    `json:"nonce"`
	At            time.Time `json:"at"`
}

// Forwarded are the request headers the hop carries besides Mcp-Session-Id, which travels in the assertion;
// every Mcp-Param-* header is carried too (spec/relay.md §11.2, §11.12).
var Forwarded = []string{"Accept", "Content-Type", "MCP-Protocol-Version", "Last-Event-ID", "Mcp-Method", "Mcp-Name"}

// paramPrefix is the lowercase prefix of the Mcp-Param-* headers.
const paramPrefix = "mcp-param-"

// ForwardedHeaders returns the headers of h the hop carries (Forwarded and every Mcp-Param-*) and the SHA-256 of
// their canonical form: names lowercased and sorted, one "name: value\n" line per value, in order. The receiving
// replica puts the hash in the assertion; the holding replica recomputes it from what arrived and forwards only
// these headers.
func ForwardedHeaders(h http.Header) (http.Header, [32]byte) {
	keep := func(name string) bool {
		l := strings.ToLower(name)
		return strings.HasPrefix(l, paramPrefix) || slices.ContainsFunc(Forwarded, func(f string) bool { return strings.EqualFold(f, name) })
	}
	out := http.Header{}
	lower := map[string][]string{}
	for name, vals := range h {
		if !keep(name) || len(vals) == 0 {
			continue
		}
		l := strings.ToLower(name)
		lower[l] = append(lower[l], vals...)
		out[http.CanonicalHeaderKey(name)] = append(out[http.CanonicalHeaderKey(name)], vals...)
	}
	names := make([]string, 0, len(lower))
	for n := range lower {
		names = append(names, n)
	}
	slices.Sort(names)
	var b bytes.Buffer
	for _, n := range names {
		for _, v := range lower[n] {
			b.WriteString(n)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteByte('\n')
		}
	}
	return out, sha256.Sum256(b.Bytes())
}

// Signer signs and verifies assertions with the shared secret, and remembers the nonces it verified for MaxAge.
type Signer struct {
	secret []byte

	mu    sync.Mutex
	seen  map[string]time.Time // nonce → when it may be forgotten
	prune time.Time
}

// NewSigner refuses a secret shorter than MinSecret.
func NewSigner(secret []byte) (*Signer, error) {
	if len(secret) < MinSecret {
		return nil, fmt.Errorf("peer: the shared secret has %d bytes; at least %d are needed", len(secret), MinSecret)
	}
	return &Signer{secret: bytes.Clone(secret), seen: map[string]time.Time{}}, nil
}

// mac is the HMAC-SHA256 over the assertion's bytes, the method, the path and the body's SHA-256.
func (s *Signer) mac(payload []byte, method, path string, bodySHA256 [32]byte) []byte {
	m := hmac.New(sha256.New, s.secret)
	fmt.Fprintf(m, "%s\n%s\n%s\n%s\n", macDomain, method, path, hex.EncodeToString(bodySHA256[:]))
	m.Write(payload)
	return m.Sum(nil)
}

// Sign returns the Kontra-Peer header value for a, over method, path and the body's SHA-256: the assertion's
// JSON and its MAC, each base64url, joined by a dot. a must carry its nonce and time (Client.Forward sets them).
func (s *Signer) Sign(a Assertion, method, path string, bodySHA256 [32]byte) (string, error) {
	if a.Nonce == "" || a.At.IsZero() {
		return "", errors.New("peer: an assertion needs its nonce and time")
	}
	payload, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(s.mac(payload, method, path, bodySHA256)), nil
}

// Verify checks a header value against method, path and body hash at now, and the nonce against those seen. The
// MAC is checked first, so that unauthenticated bytes never reach the nonce cache.
func (s *Signer) Verify(header, method, path string, bodySHA256 [32]byte, now time.Time) (Assertion, error) {
	enc := base64.RawURLEncoding
	p64, m64, ok := strings.Cut(header, ".")
	if !ok || p64 == "" || m64 == "" || len(header) > 8<<10 {
		return Assertion{}, ErrForged
	}
	payload, err := enc.DecodeString(p64)
	if err != nil {
		return Assertion{}, ErrForged
	}
	mac, err := enc.DecodeString(m64)
	if err != nil || !hmac.Equal(mac, s.mac(payload, method, path, bodySHA256)) {
		return Assertion{}, ErrForged
	}
	var a Assertion
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil || a.Nonce == "" {
		return Assertion{}, ErrForged
	}
	if now.Sub(a.At) > MaxAge || a.At.Sub(now) > MaxAge {
		return Assertion{}, ErrStale
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.prune) > MaxAge {
		for n, until := range s.seen {
			if now.After(until) {
				delete(s.seen, n)
			}
		}
		s.prune = now
	}
	if _, dup := s.seen[a.Nonce]; dup {
		return Assertion{}, ErrReplayed
	}
	// Remembered until the assertion could no longer pass the age check, whichever clock runs ahead.
	s.seen[a.Nonce] = a.At.Add(MaxAge).Add(MaxAge)
	return a, nil
}

// Client forwards exchanges to other replicas.
type Client struct {
	signer *Signer
	hc     *http.Client
	now    func() time.Time
}

// NewClient returns a client whose transport dials only the address each Forward names, read by the caller from
// gate_replicas.peer_addr: never the environment's proxy, never a redirect, plaintext HTTP/1.1 (the peer network
// is private, or the operator puts TLS in front of it).
func NewClient(s *Signer) (*Client, error) {
	if s == nil {
		return nil, errors.New("peer: a client needs its signer")
	}
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   false,
		DisableCompression:  true,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{signer: s, now: time.Now, hc: &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Forward sends req (method, the MCP path, body, the headers to forward upstream) to the replica at addr and
// returns its streamed response. It fills the assertion's Headers, Nonce and At: the headers hashed and carried
// are ForwardedHeaders(req.Header), so the holding replica forwards exactly those.
func (c *Client) Forward(ctx context.Context, addr string, a Assertion, req *http.Request) (*http.Response, error) {
	if addr == "" {
		return nil, errors.New("peer: no address")
	}
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		var err error
		if body, err = io.ReadAll(req.Body); err != nil {
			return nil, fmt.Errorf("peer: read the body to forward: %w", err)
		}
	}
	fwd, hash := ForwardedHeaders(req.Header)
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	a.Headers = hex.EncodeToString(hash[:])
	a.Nonce = base64.RawURLEncoding.EncodeToString(nonce)
	a.At = c.now().UTC()
	sig, err := c.signer.Sign(a, req.Method, Path, sha256.Sum256(body))
	if err != nil {
		return nil, err
	}
	out, err := http.NewRequestWithContext(ctx, req.Method, "http://"+addr+Path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("peer: %w", err)
	}
	out.Header = fwd
	out.Header.Set(Header, sig)
	return c.hc.Do(out)
}

// Runner runs one verified exchange on this replica's tunnel session: §6.1's check on its own registry, then the
// exchange, the upstream request built from req (the forwarded headers only) with Mcp-Session-Id from
// a.UpstreamSID, and the response streamed back to w unchanged. A session §6.1's check refuses is a 404 carrying
// HeaderGone, and no stream is opened.
type Runner interface {
	RunExchange(ctx context.Context, a Assertion, req *http.Request, w http.ResponseWriter) error
}

// Server is the peer listener's handler.
type Server struct {
	signer  *Signer
	run     Runner
	maxBody int64
	log     *slog.Logger
	now     func() time.Time
}

// ServerOption configures a Server.
type ServerOption func(*Server)

// WithMaxBody bounds a forwarded body (limits.body of the gate).
func WithMaxBody(n int64) ServerOption { return func(s *Server) { s.maxBody = n } }

// WithLogger sets the server's logger.
func WithLogger(l *slog.Logger) ServerOption { return func(s *Server) { s.log = l } }

// withClock replaces the clock (tests).
func withClock(now func() time.Time) ServerOption { return func(s *Server) { s.now = now } }

// NewServer returns the handler for peer.listen.
func NewServer(s *Signer, run Runner, opts ...ServerOption) (*Server, error) {
	if s == nil || run == nil {
		return nil, errors.New("peer: a server needs its signer and its runner")
	}
	srv := &Server{signer: s, run: run, maxBody: MaxBody, log: slog.Default(), now: time.Now}
	for _, o := range opts {
		o(srv)
	}
	return srv, nil
}

// ServeHTTP implements http.Handler: Path only (anything else 404), the assertion verified (401 with no body
// otherwise), the forwarded headers checked against it, then Runner with a request carrying only those headers.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != Path || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPost, http.MethodGet, http.MethodDelete:
	default:
		http.NotFound(w, r)
		return
	}
	// No assertion at all is refused before the body is read; one that does not verify, after (it covers the body).
	vals := r.Header.Values(Header)
	if len(vals) != 1 || !strings.Contains(vals[0], ".") {
		s.refuse(w, r, ErrForged)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, s.maxBody+1))
	if err != nil || int64(len(body)) > s.maxBody {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	a, err := s.signer.Verify(vals[0], r.Method, Path, sha256.Sum256(body), s.now())
	if err != nil {
		s.refuse(w, r, err)
		return
	}
	fwd, hash := ForwardedHeaders(r.Header)
	if !hmac.Equal([]byte(hex.EncodeToString(hash[:])), []byte(a.Headers)) {
		s.refuse(w, r, ErrHeaders)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, "/", bytes.NewReader(body))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	req.Header = fwd
	if err := s.run.RunExchange(r.Context(), a, req, w); err != nil {
		s.log.Debug("peer: exchange ended with an error", "service", a.Service, "tunnel_session", a.TunnelSession, "error", err)
	}
}

func (s *Server) refuse(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Warn("peer: a request was refused", "remote", r.RemoteAddr, "error", err)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusUnauthorized)
}

// NewHTTPServer is the peer listener's own server (never the public listener's, spec §11.2): a header read
// timeout, no write timeout (SSE responses stream back through it), HTTP/1.1.
func NewHTTPServer(h http.Handler, log *slog.Logger) *http.Server {
	if log == nil {
		log = slog.Default()
	}
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
}
