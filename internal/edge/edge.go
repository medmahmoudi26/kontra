// Package edge is the gate's public listener: a complete edge on its own, meant to be exposed directly
// (PRD §12 "Front proxies"; spec/relay.md §2.1–§2.2).
//
// It terminates TLS (ACME or a provided certificate), sends HSTS, bounds headers, bodies and idle
// connections, caps connections, rates and in-flight requests before authentication, and serves a closed
// default route: a request that is not one of the gate's routes is an empty 404. Behind a TLS-terminating
// front proxy (tls.terminated_by_proxy) it listens in plaintext and leaves HSTS to the proxy, and the
// client address comes from edge.client_ip (D69). Metrics live on their own listener (Metrics).
//
// Timeouts: ReadHeaderTimeout is limits.header_timeout (10 s, which also bounds the TLS handshake and a
// PROXY header), IdleTimeout is limits.idle (60 s, between requests on a kept-alive connection only),
// WriteTimeout is never set: an SSE or long-poll response may last as long as its task (it sends
// `: keep-alive` every KeepAlive), and a write deadline would cut it. ReadTimeout is not set either:
// request bodies are bounded per request instead, the edge reading the body itself within BodyTimeout
// and limits.body before the route runs, so a slow or oversized body never reaches a handler, and the
// body's deadline is cleared once it is read.
//
// Dependencies: golang.org/x/crypto/acme/autocert (ACME) and golang.org/x/time/rate (token buckets),
// versions in go.mod; spec/relay.md §0 pins neither.
package edge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"kontra.sh/kontra/internal/config"
)

// Limits are the per-replica edge limits (spec §2.2, D73). Zero values are replaced by the defaults.
type Limits struct {
	Body           int64         // 413 above it; default 4 MiB
	InflightPerKey int           // 429 + Retry-After above it; default 32
	RPSPerIP       float64       // 429 + Retry-After above it (burst the same); default 50
	HeaderTimeout  time.Duration // http.Server.ReadHeaderTimeout; default 10 s
	Idle           time.Duration // http.Server.IdleTimeout; default 60 s
	ConnsPerIP     int           // open connections per client address; default 256
	MaxConns       int           // open connections in all; default 10000
	// BodyTimeout is how long a request body may take to arrive (slow-loris, PRD §12); default 30 s. It is
	// not a configuration key.
	BodyTimeout time.Duration
}

// DefaultBodyTimeout bounds the arrival of one request body: 4 MiB at about 140 KiB/s.
const DefaultBodyTimeout = 30 * time.Second

func (l Limits) withDefaults() Limits {
	if l.Body <= 0 {
		l.Body = config.DefaultBody
	}
	if l.InflightPerKey <= 0 {
		l.InflightPerKey = config.DefaultInflightPerKey
	}
	if !(l.RPSPerIP > 0) {
		l.RPSPerIP = config.DefaultRPSPerIP
	}
	if l.HeaderTimeout <= 0 {
		l.HeaderTimeout = config.DefaultHeaderTimeout
	}
	if l.Idle <= 0 {
		l.Idle = config.DefaultIdle
	}
	if l.ConnsPerIP <= 0 {
		l.ConnsPerIP = config.DefaultConnsPerIP
	}
	if l.MaxConns <= 0 {
		l.MaxConns = config.DefaultMaxConns
	}
	if l.BodyTimeout <= 0 {
		l.BodyTimeout = DefaultBodyTimeout
	}
	return l
}

// LimitsFrom returns the edge limits of a loaded configuration.
func LimitsFrom(c config.Limits) Limits {
	return Limits{
		Body:           c.Body.Int64(),
		InflightPerKey: c.InflightPerKey,
		RPSPerIP:       c.RPSPerIP,
		HeaderTimeout:  c.HeaderTimeout.D(),
		Idle:           c.Idle.D(),
		ConnsPerIP:     c.ConnsPerIP,
		MaxConns:       c.MaxConns,
	}.withDefaults()
}

// KeepAlive is the SSE and long-poll comment interval (`: keep-alive`, spec §2.2).
const KeepAlive = 15 * time.Second

// HSTS is the Strict-Transport-Security value sent on every response over TLS the gate terminates.
const HSTS = "max-age=31536000"

// Config configures one public listener.
type Config struct {
	Listen    string
	PublicURL string
	// TLS is the gate's own TLS (NewStatic, NewACME); nil is plaintext, allowed when PublicURL is a
	// localhost origin or TerminatedByProxy is set.
	TLS *tls.Config
	// ACME, when TLS comes from it, is warmed when Serve starts: the certificates are obtained (or read
	// from the cache) before the first caller's handshake needs them.
	ACME *ACME
	// TerminatedByProxy: a front proxy terminates TLS (D69); the gate listens in plaintext with an https
	// public_url and sends no HSTS.
	TerminatedByProxy bool
	Limits            Limits
	ClientIP          ClientIP
	Log               *slog.Logger
}

// FromConfig returns the listener configuration of a loaded kontra.yaml, with its TLS: a provided
// certificate (NewStatic), built-in ACME with the directory cache (NewACME; one replica, D70), or none.
func FromConfig(c *config.Config, log *slog.Logger) (Config, error) {
	tlsCfg, acme, err := TLSFromConfig(c.TLS)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Listen:            c.Listen,
		PublicURL:         c.PublicURL,
		TLS:               tlsCfg,
		ACME:              acme,
		TerminatedByProxy: c.TLS.TerminatedByProxy,
		Limits:            LimitsFrom(c.Limits),
		ClientIP:          ClientIP{From: c.Edge.ClientIP.From, Trusted: c.Edge.ClientIP.TrustedPrefixes},
		Log:               log,
	}, nil
}

// Server is the public listener. WriteTimeout is never set (SSE); slow bodies are bounded per request.
type Server struct {
	cfg Config
	srv *http.Server

	mu       sync.Mutex
	ln       *capListener
	serving  bool
	shutdown bool
}

// ErrPlaintext is New's error for a plaintext listener whose public_url is not a localhost origin and that
// does not declare a TLS-terminating front proxy (PRD §11, D69).
var ErrPlaintext = errors.New("edge: plaintext is refused unless public_url is a localhost origin or a front proxy terminates TLS")

// New returns a server for routes behind the edge middleware.
func New(cfg Config, routes *Router) (*Server, error) {
	if routes == nil {
		return nil, errors.New("edge: no routes")
	}
	cfg.Limits = cfg.Limits.withDefaults()
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.ClientIP.From == "" {
		cfg.ClientIP.From = config.ClientIPRemote
	}
	if cfg.TLS == nil && !cfg.TerminatedByProxy {
		u, err := url.Parse(cfg.PublicURL)
		if err != nil || u.Hostname() == "" || !config.IsLocalhost(u.Hostname()) {
			return nil, fmt.Errorf("%w (public_url %q)", ErrPlaintext, cfg.PublicURL)
		}
	}
	if cfg.TLS != nil && cfg.TerminatedByProxy {
		return nil, errors.New("edge: TLS and terminated_by_proxy are exclusive")
	}
	var tlsCfg *tls.Config
	if cfg.TLS != nil {
		tlsCfg = cfg.TLS.Clone()
		if tlsCfg.MinVersion < tls.VersionTLS12 {
			tlsCfg.MinVersion = tls.VersionTLS12
		}
		for _, p := range []string{"h2", "http/1.1"} {
			if !slices.Contains(tlsCfg.NextProtos, p) {
				tlsCfg.NextProtos = append(tlsCfg.NextProtos, p)
			}
		}
		cfg.TLS = tlsCfg
	}
	h := Middleware(MiddlewareConfig{Limits: cfg.Limits, HSTS: cfg.TLS != nil, ClientIP: cfg.ClientIP}, routes)
	s := &Server{cfg: cfg}
	s.srv = &http.Server{
		Handler:           h,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: cfg.Limits.HeaderTimeout,
		IdleTimeout:       cfg.Limits.Idle,
		ReadTimeout:       0, // bodies are bounded per request (readBody)
		WriteTimeout:      0, // SSE
		MaxHeaderBytes:    http.DefaultMaxHeaderBytes,
		// Handshake failures and the like are routine on a public listener (scanners): debug level.
		ErrorLog: slog.NewLogLogger(cfg.Log.Handler(), slog.LevelDebug),
	}
	return s, nil
}

// HTTPServer is the underlying server (tests read its timeouts).
func (s *Server) HTTPServer() *http.Server { return s.srv }

// Listen binds cfg.Listen (once) and returns the bound address; Serve calls it when it has not been.
func (s *Server) Listen() (net.Addr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shutdown {
		return nil, http.ErrServerClosed
	}
	if s.ln != nil {
		return s.ln.Addr(), nil
	}
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("edge: listen %s: %w", s.cfg.Listen, err)
	}
	s.ln = newCapListener(ln, s.cfg.Limits, s.cfg.ClientIP, s.cfg.Log)
	return ln.Addr(), nil
}

// Serve accepts until Shutdown (or Close through ctx: when ctx ends the listener and every connection
// are closed at once) and then returns nil. Request contexts carry ctx's values, not its cancellation:
// the drain decides when streams end.
func (s *Server) Serve(ctx context.Context) error {
	if _, err := s.Listen(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.serving {
		s.mu.Unlock()
		return errors.New("edge: Serve called twice")
	}
	s.serving = true
	var ln net.Listener = s.ln
	s.mu.Unlock()
	if s.cfg.TLS != nil {
		ln = tls.NewListener(ln, s.cfg.TLS)
	}
	base := context.WithoutCancel(ctx)
	s.srv.BaseContext = func(net.Listener) context.Context { return base }
	if s.cfg.ACME != nil {
		go func() {
			if err := s.cfg.ACME.Warm(ctx); err != nil && ctx.Err() == nil {
				s.cfg.Log.Warn("edge: acme: certificate not obtained yet; the first handshake for it will try again", "error", err)
			}
		}()
	}
	stop := context.AfterFunc(ctx, func() { _ = s.srv.Close() })
	defer stop()
	err := s.srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting and waits for in-flight requests until ctx ends; then it closes every
// connection still open and returns ctx's error.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shutdown = true
	ln, serving := s.ln, s.serving
	s.mu.Unlock()
	if !serving && ln != nil {
		_ = ln.Close()
	}
	if err := s.srv.Shutdown(ctx); err != nil {
		_ = s.srv.Close()
		return err
	}
	return nil
}

// Router is the closed route table: only the routes registered on it answer, everything else (an
// unknown path, a known path with another method, a path ServeMux would redirect) is an empty 404.
type Router struct {
	mux *http.ServeMux
}

// route is a registered handler and what the edge needs to know about it.
type route struct {
	h       http.Handler
	jsonrpc bool
}

func (rt *route) ServeHTTP(w http.ResponseWriter, r *http.Request) { rt.h.ServeHTTP(w, r) }

// NewRouter returns an empty closed router.
func NewRouter() *Router {
	return &Router{mux: http.NewServeMux()}
}

// Handle registers h for a Go 1.22 ServeMux pattern ("GET /healthz", "/agents/{name}/{rest...}"). The
// handler reads the pattern's wildcards with r.PathValue and the pattern with r.Pattern, behind the edge
// (Middleware, New) as on the bare router. Routes are registered before the router serves.
func (r *Router) Handle(pattern string, h http.Handler) {
	r.mux.Handle(pattern, &route{h: h})
}

// HandleJSONRPC registers h for a JSON-RPC 2.0 pattern ("POST /agents/{name}"): the edge's refusals on
// it (413, 429) carry a JSON-RPC error object (-32600) as their body.
func (r *Router) HandleJSONRPC(pattern string, h http.Handler) {
	r.mux.Handle(pattern, &route{h: h, jsonrpc: true})
}

// match returns the route r would be served by; ok is false for anything ServeMux would answer itself
// (not found, method not allowed, a redirect to a cleaned or slash-terminated path). It only looks:
// ServeMux.Handler leaves r.Pattern and the wildcards unset, so a matched request is served through
// dispatch.
func (r *Router) match(req *http.Request) (*route, bool) {
	h, _ := r.mux.Handler(req)
	rt, ok := h.(*route)
	return rt, ok
}

// ServeHTTP implements http.Handler: the closed default route alone, without the edge's limits.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if _, ok := r.match(req); !ok {
		emptyNotFound(w)
		return
	}
	r.dispatch(w, req)
}

// dispatch serves a request match accepted. ServeMux.ServeHTTP is the only place r.Pattern and the
// wildcards (r.PathValue) are set, so the request goes through it; its method, host and path are those
// match saw, so it reaches the same route.
func (r *Router) dispatch(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func emptyNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNotFound)
}

// Healthz is GET /healthz: 200 when this replica's Postgres and Temporal connections are up and it is not
// draining (ready reports it), else 503 (spec §2.1). Never cached.
func Healthz(ready func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if ready != nil && ready() {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "unavailable\n")
	})
}

// MiddlewareConfig configures the edge chain.
type MiddlewareConfig struct {
	Limits   Limits
	HSTS     bool // the gate terminates TLS: send HSTS on requests that came over it
	ClientIP ClientIP
	now      func() time.Time // tests
}

// Middleware is the edge chain in front of routes, outermost first: HSTS (TLS only, on every response, the closed
// route's included), the closed default route (an empty 404 for anything routes does not register), the per-IP rate, the body cap (the body is
// read here, within BodyTimeout, so a handler only ever sees a complete body within the limit), then the
// per-key in-flight cap, counted per presented Authorization value before it is verified (spec §2.2: the
// limits come before authentication; rps_per_ip bounds credential spraying). Refusals happen before any
// task row exists; on JSON-RPC routes their body is also a JSON-RPC error (-32600, "request too large" or
// "rate limited"), with the request's id when it was read, else null.
func Middleware(cfg MiddlewareConfig, routes *Router) http.Handler {
	cfg.Limits = cfg.Limits.withDefaults()
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.ClientIP.From == "" {
		cfg.ClientIP.From = config.ClientIPRemote
	}
	return &chain{
		cfg:      cfg,
		routes:   routes,
		rates:    newRateTable(cfg.Limits.RPSPerIP, cfg.now),
		inflight: &inflightTable{max: cfg.Limits.InflightPerKey, n: map[[32]byte]int{}},
	}
}

type chain struct {
	cfg      MiddlewareConfig
	routes   *Router
	rates    *rateTable
	inflight *inflightTable
}

// Refusal messages (spec §2.2).
const (
	msgTooLarge    = "request too large"
	msgRateLimited = "rate limited"
	msgBodyTimeout = "request body timeout"
)

func (c *chain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// HSTS first: the closed default route's 404 is a response over TLS too (M3 WP-10 pentest finding F-2).
	if c.cfg.HSTS && r.TLS != nil {
		w.Header().Set("Strict-Transport-Security", HSTS)
	}
	rt, ok := c.routes.match(r)
	if !ok {
		emptyNotFound(w)
		return
	}
	addr := c.cfg.ClientIP.Addr(r)
	r = r.WithContext(withClientAddr(r.Context(), addr))

	if ok, wait := c.rates.allow(addr); !ok {
		refuse(w, rt, http.StatusTooManyRequests, msgRateLimited, retryAfter(wait), nil)
		return
	}

	var body []byte
	if r.ContentLength > c.cfg.Limits.Body {
		closeAfter(w, r)
		refuse(w, rt, http.StatusRequestEntityTooLarge, msgTooLarge, "", nil)
		return
	}
	if r.Body != nil && r.Body != http.NoBody {
		b, err := c.readBody(w, r)
		switch {
		case errors.Is(err, errTooLarge):
			closeAfter(w, r)
			refuse(w, rt, http.StatusRequestEntityTooLarge, msgTooLarge, "", nil)
			return
		case errors.Is(err, os.ErrDeadlineExceeded):
			closeAfter(w, r)
			refuse(w, rt, http.StatusRequestTimeout, msgBodyTimeout, "", nil)
			return
		case err != nil:
			closeAfter(w, r)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body = b
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		r.TransferEncoding = nil
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	}

	if vals := r.Header.Values("Authorization"); len(vals) > 0 {
		key := credentialKey(vals)
		if !c.inflight.acquire(key) {
			refuse(w, rt, http.StatusTooManyRequests, msgRateLimited, "1", body)
			return
		}
		defer c.inflight.release(key)
	}
	c.routes.dispatch(w, r)
}

var errTooLarge = errors.New("edge: body over the limit")

// closeAfter ends an HTTP/1 connection after a refusal that leaves its body unread or half read, rather
// than letting the server drain it. On HTTP/2 the stream alone ends (a Connection header there would
// shut the whole connection down, every other stream on it included).
func closeAfter(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor < 2 {
		w.Header().Set("Connection", "close")
	}
}

// readBody reads the whole body within BodyTimeout and the body limit. The read deadline is the
// connection's (http.ResponseController), cleared afterwards so a long response is never cut by it.
func (c *chain) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	rc := http.NewResponseController(w)
	// A ResponseWriter without deadlines (a recorder in tests) reads without one; the body limit still holds.
	deadlineSet := rc.SetReadDeadline(time.Now().Add(c.cfg.Limits.BodyTimeout)) == nil
	b, err := readUpTo(r.Body, r.ContentLength, c.cfg.Limits.Body)
	if deadlineSet {
		_ = rc.SetReadDeadline(time.Time{})
	}
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, os.ErrDeadlineExceeded
		}
		return nil, err
	}
	if int64(len(b)) > c.cfg.Limits.Body {
		return nil, errTooLarge
	}
	return b, nil
}

// bodyPrealloc is the most readUpTo reserves before a body's bytes arrive, about what the server already
// spends on a request's headers. A declared Content-Length costs a client nothing to send (a header alone
// can declare limits.body, before authentication, on as many HTTP/2 streams as rps_per_ip lets through
// within BodyTimeout), so the buffer follows the bytes that do arrive.
const bodyPrealloc = 4 << 10

// readUpTo reads r to EOF, or until it holds limit+1 bytes (a body over the limit). The buffer starts at
// bodyPrealloc at most and doubles as bytes arrive, so it is never more than about twice what was received;
// declared (the Content-Length, -1 when unknown) only caps its growth, so a body of the declared length
// ends in a buffer of that length and one spare byte (which sees EOF), and a longer one is still read up to
// limit+1.
func readUpTo(r io.Reader, declared, limit int64) ([]byte, error) {
	limit = min(limit, math.MaxInt64/4) // limits.body may be configured absurdly large: keep the sums in range
	hint := limit + 1
	if declared >= 0 && declared < limit {
		hint = declared + 1
	}
	b := make([]byte, 0, min(hint, bodyPrealloc))
	for {
		if len(b) == cap(b) {
			if int64(len(b)) > limit {
				return b, nil
			}
			ceil := limit + 1
			if int64(len(b)) < hint {
				ceil = hint
			}
			// Double, or go straight to the ceiling when doubling again would pass it (so a body of the
			// declared length is not copied once more for its last byte); next > cap(b) because ceil is.
			next := 2 * int64(cap(b))
			if 2*next > ceil {
				next = ceil
			}
			b = append(make([]byte, 0, next), b...)
		}
		n, err := r.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		if err == io.EOF {
			return b, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// credentialKey identifies a presented credential for the in-flight cap without keeping it. A single
// `Bearer <token>` counts by its token, whatever the scheme's case and the spaces before the token (else
// spelling one key several ways would multiply its slots); anything else counts by its exact bytes.
func credentialKey(vals []string) [32]byte {
	h := sha256.New()
	if len(vals) == 1 {
		if scheme, tok, ok := strings.Cut(vals[0], " "); ok && strings.EqualFold(scheme, "bearer") {
			h.Write([]byte("bearer\x00"))
			h.Write([]byte(strings.TrimLeft(tok, " ")))
			var k [32]byte
			copy(k[:], h.Sum(nil))
			return k
		}
	}
	h.Write([]byte("raw\x00"))
	for _, v := range vals {
		h.Write([]byte(v))
		h.Write([]byte{0})
	}
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

// retryAfter is a Retry-After value in whole seconds, at least 1.
func retryAfter(wait time.Duration) string {
	s := int64(math.Ceil(wait.Seconds()))
	if s < 1 {
		s = 1
	}
	return strconv.FormatInt(s, 10)
}

// refuse writes an edge refusal: on a JSON-RPC route a JSON-RPC error object (-32600) carrying the
// request's id when body holds a readable request, else plain text.
func refuse(w http.ResponseWriter, rt *route, status int, msg, retry string, body []byte) {
	if retry != "" {
		w.Header().Set("Retry-After", retry)
	}
	if rt != nil && rt.jsonrpc {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(jsonRPCError(body, msg))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}

// JSONRPCInvalidRequest is the JSON-RPC error code of every edge refusal (spec §2.2).
const JSONRPCInvalidRequest = -32600

func jsonRPCError(body []byte, msg string) []byte {
	id := json.RawMessage("null")
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	if len(body) > 0 && json.Unmarshal(body, &req) == nil && validID(req.ID) {
		id = req.ID
	}
	out, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id, Error: struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{JSONRPCInvalidRequest, msg}})
	return out
}

// validID accepts a JSON-RPC id: a string, a number or null.
func validID(id json.RawMessage) bool {
	if len(id) == 0 {
		return false
	}
	switch id[0] {
	case '"':
		var s string
		return json.Unmarshal(id, &s) == nil
	case 'n':
		return string(id) == "null"
	default:
		var n json.Number
		return json.Unmarshal(id, &n) == nil
	}
}

// rateTable holds one token bucket per client address (an IPv6 client's /64, countKey): rps_per_ip a
// second, the same burst. A bucket idle long enough to be full again is dropped, so the table only holds
// recent clients.
type rateTable struct {
	mu        sync.Mutex
	limit     rate.Limit
	burst     int
	idle      time.Duration
	now       func() time.Time
	m         map[netip.Addr]*rateEntry
	lastSweep time.Time
}

type rateEntry struct {
	lim  *rate.Limiter
	seen time.Time
}

func newRateTable(rps float64, now func() time.Time) *rateTable {
	burst := int(math.Ceil(rps))
	if burst < 1 {
		burst = 1
	}
	idle := time.Duration(float64(burst)/rps*float64(time.Second)) + time.Second
	if idle < time.Minute {
		idle = time.Minute
	}
	return &rateTable{limit: rate.Limit(rps), burst: burst, idle: idle, now: now, m: map[netip.Addr]*rateEntry{}}
}

// allow takes a token for a's bucket, or reports how long until one is available.
func (t *rateTable) allow(a netip.Addr) (bool, time.Duration) {
	a = countKey(a)
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if now.Sub(t.lastSweep) >= t.idle {
		for k, e := range t.m {
			if now.Sub(e.seen) >= t.idle {
				delete(t.m, k)
			}
		}
		t.lastSweep = now
	}
	e := t.m[a]
	if e == nil {
		e = &rateEntry{lim: rate.NewLimiter(t.limit, t.burst)}
		t.m[a] = e
	}
	e.seen = now
	res := e.lim.ReserveN(now, 1)
	if !res.OK() {
		return false, time.Second
	}
	if d := res.DelayFrom(now); d > 0 {
		res.CancelAt(now)
		return false, d
	}
	return true, 0
}

// inflightTable counts the requests in flight per presented credential.
type inflightTable struct {
	mu  sync.Mutex
	max int
	n   map[[32]byte]int
}

func (t *inflightTable) acquire(k [32]byte) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.n[k] >= t.max {
		return false
	}
	t.n[k]++
	return true
}

func (t *inflightTable) release(k [32]byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.n[k] <= 1 {
		delete(t.n, k)
		return
	}
	t.n[k]--
}
