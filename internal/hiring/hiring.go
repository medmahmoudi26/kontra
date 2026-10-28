// Package hiring is the hiring side of kontra connect (spec/tunnel.md §9, D34): the node serves
// 127.0.0.1:7777 (--listen) to local clients and forwards each request on a stream it opens in its own tunnel,
// preamble {"v": 1, "op": "http"}, path unchanged: /agents/…, /mcp/…, /mcp and /.well-known/…; anything else is
// 404. A local client never sends a credential to the node, and the node never sends one the gate did not
// issue: the gate takes the session's principal (spec/relay.md §2.3).
//
// Local requests are checked first (DNS rebinding, other local users, MCP's Origin rule; local_rebinding):
// a Host that is not 127.0.0.1:<port>, localhost:<port> or [::1]:<port> of --listen, or an Origin not listed by
// --allow-origin (default none), is 403 (an id-less JSON-RPC error on MCP paths) and opens no stream; OPTIONS is
// 405; --listen off loopback requires --local-token, whose bearer the node strips. There is no other local port
// (no console, D164).
//
// Cards: the node verifies the gate's signature on every card it serves locally, with a jku equal to
// {the origin it dialed}/.well-known/jwks.json, and rewrites supportedInterfaces[].url to its own
// http://localhost:<port>/… URLs; a card whose signature does not verify is not served, whatever the answer's
// Content-Type and the case of its member names. Sealing and opening for sealed services happen around the
// forwarding (internal/sealing, M3 WP-08); blobs an A2A answer (/agents/…) names as kontra-blob:<ref> are fetched
// on blob streams and inlined (internal/blob). A node keeps at most the gate's cap of node-opened streams in
// flight per session (spec/tunnel.md §14): a request that finds none free in time is 429 (ErrBusy).
//
// Work package: M3 WP-03 (listener, checks, forwarding, cards), M3 WP-08 (sealing around it).
package hiring

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"kontra.sh/kontra/internal/a2awire"
	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/pins"
	"kontra.sh/kontra/internal/sealing"
	"kontra.sh/kontra/internal/tunnel"
)

// DefaultListen is --listen's default.
const DefaultListen = "127.0.0.1:7777"

// ListenOff is the --listen value that turns the hiring endpoint off, for a node that only hosts (D208): the node
// binds no local port and hires for no one.
const ListenOff = "off"

// Local listener timeouts: a header read timeout and an idle timeout, never a write timeout (SSE answers last
// as long as their task).
const (
	headerTimeout = 10 * time.Second
	idleTimeout   = 60 * time.Second
	// openTimeout bounds the wait for a session to open a stream on (a node between sessions).
	openTimeout = 15 * time.Second
)

// Config is the hiring side's part of the kontra connect command line.
type Config struct {
	Listen       string   // --listen
	LocalToken   string   // --local-token <file>, read; required when Listen is not loopback
	AllowOrigins []string // --allow-origin, repeatable
	Gate         *url.URL // the gate the node dialed: the jku origin and the card URLs' replacement base
}

// Opener opens one node-opened stream in the current session, preamble written; it fails while the node has no
// session or is draining (no new hiring streams on a draining session, spec/tunnel.md §13), and with an error
// wrapping ErrBusy while the session has as many node-opened streams in flight as the gate allows (§14).
type Opener interface {
	OpenHire(ctx context.Context) (net.Conn, error)
}

// ErrBusy is an Opener's refusal to open one more node-opened stream on a session that already has the gate's cap
// of them in flight (spec/tunnel.md §14: 32, 8 for an anonymous session) after a short wait: the local client gets
// 429 with Retry-After, and no stream is opened, so the gate never meets more streams than it allows.
var ErrBusy = errors.New("the tunnel session has as many node-opened streams in flight as the gate allows")

// BlobOpener opens a blob stream in the current session, its preamble not yet written (blob.Fetch writes it).
// An Opener that is one too lets the server inline the kontra-blob: parts of the gate's answers.
type BlobOpener interface {
	OpenBlob(ctx context.Context) (net.Conn, error)
}

// Server is the local endpoint.
type Server struct {
	cfg  Config
	open Opener
	log  *slog.Logger

	origins map[string]bool // --allow-origin, canonical
	proxy   *httputil.ReverseProxy
	tunnel  *http.Client // GET of the gate's key set through the tunnel
	inline  *a2awire.Inliner

	mu     sync.Mutex
	listen string   // the bound address (Serve), else cfg.Listen
	local  *url.URL // http://localhost:<port>
	// seal is the sealing around the forwarding (internal/sealing, ext-sealed.md §5 "Hiring node"); nil seals nothing.
	seal *sealing.Hiring
}

// jobKey carries a request's sealing job from Handler to modify.
type jobKey struct{}

// SetSealing puts the hiring node's sealing around the forwarding: a request to a sealed service is sealed before it
// is forwarded (its pin checked first, ext-sealed.md §7), the answer opened before the local client sees it, and a
// card served locally is recorded with its recipients and the pin checked. It is set before Serve.
func (s *Server) SetSealing(h *sealing.Hiring) {
	s.mu.Lock()
	s.seal = h
	s.mu.Unlock()
}

func (s *Server) sealing() *sealing.Hiring {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seal
}

// Card fetches an A2A service's served card through the tunnel and verifies the gate's signature (VerifyCard); when the
// gate says it verified the seller's signature, the upstream card is fetched too and its hash checked, and the seller
// key set the gate names (kontra.upstream.jwks, D223), for the seller keys the pins record (ext-sealed.md §7). svc is
// the service's path ({svc}).
func (s *Server) Card(ctx context.Context, svc string) (*sealing.Card, error) {
	body, err := s.get(ctx, svc+"/.well-known/agent-card.json")
	if err != nil {
		return nil, err
	}
	return s.verified(ctx, body)
}

// verified is a served card's sealing.Card: the gate's signature verified, and the seller's keys when it says it
// verified them.
func (s *Server) verified(ctx context.Context, body []byte) (*sealing.Card, error) {
	_, hdr, err := VerifyCard(ctx, body, s.cfg.Gate, s.tunnel)
	if err != nil {
		return nil, err
	}
	c := &sealing.Card{Body: body}
	if up := hdr.Upstream; up != nil && up.Verified {
		u, err := url.Parse(up.URL)
		if err != nil {
			return nil, fmt.Errorf("%w: kontra.upstream.url", ErrCardNotVerified)
		}
		o, err := tunnel.CanonicalOrigin(u)
		if g, gerr := tunnel.CanonicalOrigin(s.cfg.Gate); err != nil || gerr != nil || o != g {
			return nil, fmt.Errorf("%w: kontra.upstream.url is not on the gate's origin", ErrCardNotVerified)
		}
		ub, err := s.get(ctx, u.EscapedPath())
		if err != nil {
			return nil, err
		}
		g, _ := tunnel.CanonicalOrigin(s.cfg.Gate)
		set, err := sealing.SellerKeySet(ctx, up, g, func(ctx context.Context, raw string) ([]byte, error) {
			ju, err := url.Parse(raw)
			if err != nil {
				return nil, err
			}
			return s.get(ctx, ju.EscapedPath()) // through the tunnel, to the gate's origin only
		})
		if err != nil {
			return nil, err
		}
		if c.SellerKeys, err = sealing.SellerKeyIDs(ub, up.SHA256, set); err != nil {
			return nil, err
		}
		// Signed only when a seller signature verified, with its own key or the named set's (or names its key by kid
		// only and no set is named, D223): a card whose signatures all fail to verify is unsigned, a downgrade a pin
		// refuses (round 2 sealing-fixes-5).
		c.Signed = len(c.SellerKeys) > 0
	}
	return c, nil
}

// get fetches path from the gate through the tunnel (200 only, at most maxCard bytes).
func (s *Server) get(ctx context.Context, path string) ([]byte, error) {
	u := url.URL{Scheme: s.cfg.Gate.Scheme, Host: s.cfg.Gate.Host, Path: path}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := s.tunnel.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the gate answered HTTP %d for %s", res.StatusCode, clip(path))
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxCard+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxCard {
		return nil, fmt.Errorf("an answer over %d bytes for %s", maxCard, clip(path))
	}
	return b, nil
}

// ServicePath is the {svc} prefix of a hiring path (spec/relay.md §2.1): /agents/{name} or /agents/{tenant}/{name}
// with what follows it for HTTP+JSON (message:send, message:stream, tasks/…, .well-known/…), /mcp/{name} or
// /mcp/{tenant}/{name}; "" for any other path. A second segment is the service's name, unless it is one of A2A's
// HTTP+JSON routes (a multi-tenant gate's service named "tasks" is read as the single-tenant route).
func ServicePath(p string) string {
	for _, prefix := range []string{"/agents/", "/mcp/"} {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok || rest == "" {
			continue
		}
		segs := strings.Split(rest, "/")
		if prefix == "/mcp/" {
			if len(segs) > 2 || segs[0] == "" {
				return ""
			}
			return strings.TrimRight(p, "/")
		}
		if segs[0] == "" {
			return ""
		}
		if len(segs) >= 2 && segs[1] != "" && !strings.Contains(segs[1], ":") && segs[1] != "tasks" &&
			!strings.HasPrefix(segs[1], ".") && segs[1] != "extendedAgentCard" {
			return prefix + segs[0] + "/" + segs[1]
		}
		return prefix + segs[0]
	}
	return ""
}

// New checks cfg (a non-loopback Listen without LocalToken is refused, as is an Origin that is not one) and
// returns the server; Serve binds nothing, its listener is the caller's.
func New(cfg Config, open Opener, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if open == nil {
		return nil, errors.New("hiring: no opener")
	}
	if cfg.Gate == nil {
		return nil, errors.New("hiring: no gate")
	}
	if _, err := tunnel.CanonicalOrigin(cfg.Gate); err != nil {
		return nil, fmt.Errorf("hiring: the gate: %w", err)
	}
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	if err := CheckListen(cfg.Listen, cfg.LocalToken); err != nil {
		return nil, err
	}
	if cfg.Listen == ListenOff && len(cfg.AllowOrigins) > 0 {
		return nil, fmt.Errorf("--allow-origin names an Origin allowed to call --listen, which is off (D208)")
	}
	s := &Server{cfg: cfg, open: open, log: log.With("component", "hiring"), origins: map[string]bool{}}
	for _, o := range cfg.AllowOrigins {
		c, err := canonicalOrigin(o)
		if err != nil {
			return nil, fmt.Errorf("--allow-origin %q: %w", o, err)
		}
		s.origins[c] = true
	}
	transport := &streamTransport{open: open}
	s.tunnel = &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if bo, ok := open.(BlobOpener); ok {
		s.inline = &a2awire.Inliner{Fetch: func(ctx context.Context, ref blob.Ref, w io.Writer) (int64, error) {
			st, err := bo.OpenBlob(ctx)
			if err != nil {
				return 0, err
			}
			return blob.Fetch(ctx, st, ref, w)
		}}
	}
	if cfg.Listen != ListenOff {
		s.listen, s.local = cfg.Listen, localBase(cfg.Listen)
	}
	gate := cfg.Gate
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			in := pr.In
			pr.Out.URL = &url.URL{Scheme: gate.Scheme, Host: gate.Host, Path: in.URL.Path, RawPath: in.URL.RawPath, RawQuery: in.URL.RawQuery}
			pr.Out.Host = gate.Host
			pr.Out.Close = true
			h := pr.Out.Header
			// The local bearer and any credential a client sent stay here: the gate takes the session's
			// principal. Origin goes on only when it was allowed (§9); cookies never.
			h.Del("Authorization")
			h.Del("Proxy-Authorization")
			h.Del("Cookie")
			h.Del("Forwarded")
			// Answers are read (cards verified, parts inlined): identity encoding only.
			h.Del("Accept-Encoding")
			if o := in.Header.Get("Origin"); o == "" || !s.originAllowed(o) {
				h.Del("Origin")
			}
		},
		Transport:      transport,
		FlushInterval:  -1,
		ModifyResponse: s.modify,
		ErrorHandler:   s.proxyError,
		ErrorLog:       stdlog.New(io.Discard, "", 0),
	}
	return s, nil
}

// CheckListen refuses a --listen that is not host:port or off, one off loopback without a local token (§9), and a
// local token with --listen off (D208).
func CheckListen(listen, token string) error {
	if listen == ListenOff {
		if token != "" {
			return fmt.Errorf("--local-token guards --listen, which is off (D208)")
		}
		return nil
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("--listen %q: want host:port", listen)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("--listen %q: port", listen)
	}
	if !loopbackHost(host) && token == "" {
		return fmt.Errorf("--listen %s is not a loopback address: it needs --local-token <file> (spec/tunnel.md §9)", listen)
	}
	return nil
}

// loopbackHost reports localhost or a loopback IP literal.
func loopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.Unmap().IsLoopback()
}

// localBase is the base of the card URLs the node serves: http://localhost:<port> for a loopback or unspecified
// --listen, else its own address.
func localBase(listen string) *url.URL {
	host, port, _ := net.SplitHostPort(listen)
	a, err := netip.ParseAddr(host)
	if loopbackHost(host) || host == "" || (err == nil && a.IsUnspecified()) {
		host = "localhost"
	}
	return &url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}
}

// Handler is the local HTTP handler: the checks above, then the forwarding.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status, err := CheckLocal(r, s.config()); err != nil {
			s.log.Warn("local request refused", "status", status, "reason", err.Error(), "path", clip(r.URL.Path))
			refuse(w, r, status, err.Error())
			return
		}
		if !hiringPath(r.URL.Path) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "kontra connect forwards /agents/…, /mcp/…, /mcp and /.well-known/… only\n")
			return
		}
		if seal := s.sealing(); seal != nil {
			svc := ServicePath(r.URL.Path)
			// A card read ({svc}/.well-known/…, or a GET on {svc} itself) carries no part: its card is verified and its
			// pin checked when it is served (modify, CardSeen).
			cardRead := isA2APath(r.URL.Path) && (isCardPath(r.URL.Path) ||
				r.Method == http.MethodGet && strings.TrimRight(r.URL.Path, "/") == svc)
			if svc != "" && !cardRead {
				// ext-sealed.md §5: the pin is checked and the request sealed before anything is forwarded; a refusal
				// is answered here and opens no stream.
				r2, job, err := seal.Request(r.Context(), svc, r)
				if err != nil {
					s.sealRefused(w, r, err)
					return
				}
				r = r2.WithContext(context.WithValue(r2.Context(), jobKey{}, job))
			}
		}
		s.proxy.ServeHTTP(w, r)
	})
}

// config is cfg with the bound address.
func (s *Server) config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cfg
	c.Listen = s.listen
	return c
}

// Serve serves ln until ctx ends. The listener's address is --listen's (a port 0 bound to a free port): the Host
// check and the card URLs take its port.
//
// On a loopback address without --local-token only the node's own user is served: a connection of another account
// on the machine is closed (peerListener, spec/tunnel.md §9).
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	s.listen = ln.Addr().String()
	s.local = localBase(s.listen)
	token := s.cfg.LocalToken
	s.mu.Unlock()
	if host, _, err := net.SplitHostPort(ln.Addr().String()); err == nil && loopbackHost(host) && token == "" {
		if !peerCheckWorks() {
			s.log.Warn("the socket table (/proc/self/net/tcp) cannot be read: every local connection will be closed; " +
				"run with --local-token to serve this machine's users (spec/tunnel.md §9)")
		}
		if os.Getuid() == overflowUID {
			s.log.Warn("kontra connect runs as the overflow uid, which the node attributes to no one: every local "+
				"connection will be closed; run with --local-token (spec/tunnel.md §9)", "uid", overflowUID)
		}
		ln = &peerListener{Listener: ln, uid: os.Getuid(), log: s.log}
	}
	defer forgetKeySets(s.tunnel)
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: headerTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	stop := context.AfterFunc(ctx, func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		_ = srv.Close()
	})
	defer stop()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Local is the base of the URLs the node serves (http://localhost:<port>).
func (s *Server) Local() *url.URL {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := *s.local
	return &u
}

// CheckLocal applies the local request checks (Host, Origin, local token) to r; a refusal is the status to answer:
// 403 for a Host that is not one of --listen's loopback names with its port, or an Origin not listed by
// --allow-origin; 401 without the local token when one is set; 405 for OPTIONS (no CORS preflight is answered).
//
// SPEC-CONFLICT (D207; spec/tunnel.md §9): §9 accepts only 127.0.0.1:<port>, localhost:<port> and [::1]:<port> as
// Host, and also allows --listen on a non-loopback address with --local-token, whose clients reach it by that
// address. The closest faithful behaviour: with a non-loopback --listen of a specific address (never 0.0.0.0 or
// ::), that address with the port is accepted too; the local token, which a web page cannot know, guards it.
func CheckLocal(r *http.Request, cfg Config) (status int, err error) {
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return http.StatusInternalServerError, errors.New("no --listen address")
	}
	allowed := []string{net.JoinHostPort("127.0.0.1", port), net.JoinHostPort("localhost", port), net.JoinHostPort("::1", port)}
	if a, err := netip.ParseAddr(host); err == nil && !a.IsUnspecified() && !a.Unmap().IsLoopback() {
		allowed = append(allowed, net.JoinHostPort(a.String(), port))
	}
	got := strings.ToLower(r.Host)
	ok := false
	for _, h := range allowed {
		if got == h {
			ok = true
			break
		}
	}
	if !ok {
		return http.StatusForbidden, fmt.Errorf("Host %s is not this node's local endpoint (spec/tunnel.md §9)", clip(r.Host))
	}
	if origins := r.Header.Values("Origin"); len(origins) > 0 {
		if len(origins) != 1 || !originListed(origins[0], cfg.AllowOrigins) {
			return http.StatusForbidden, errors.New("this Origin is not allowed (--allow-origin)")
		}
	}
	if cfg.LocalToken != "" {
		want := "Bearer " + cfg.LocalToken
		vals := r.Header.Values("Authorization")
		if len(vals) != 1 || subtle.ConstantTimeCompare([]byte(vals[0]), []byte(want)) != 1 {
			return http.StatusUnauthorized, errors.New("the local token is required (Authorization: Bearer <--local-token>)")
		}
	}
	if r.Method == http.MethodOptions {
		return http.StatusMethodNotAllowed, errors.New("kontra connect answers no CORS preflight")
	}
	return 0, nil
}

// originAllowed reports an Origin --allow-origin lists.
func (s *Server) originAllowed(o string) bool {
	c, err := canonicalOrigin(o)
	return err == nil && s.origins[c]
}

func originListed(o string, list []string) bool {
	c, err := canonicalOrigin(o)
	if err != nil {
		return false
	}
	for _, l := range list {
		if lc, err := canonicalOrigin(l); err == nil && lc == c {
			return true
		}
	}
	return false
}

// canonicalOrigin is scheme://host[:port], lowercase, the default port elided; anything else (a path, "null")
// is refused.
func canonicalOrigin(o string) (string, error) {
	u, err := url.Parse(o)
	if err != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("not an origin (scheme://host[:port])")
	}
	return tunnel.CanonicalOrigin(u)
}

// hiringPath is §9's four prefixes.
func hiringPath(p string) bool {
	return strings.HasPrefix(p, "/agents/") || strings.HasPrefix(p, "/mcp/") || p == "/mcp" || strings.HasPrefix(p, "/.well-known/")
}

func isMCPPath(p string) bool { return p == "/mcp" || strings.HasPrefix(p, "/mcp/") }

// isA2APath reports a path of the gate's A2A routes, the only answers whose parts are walked.
func isA2APath(p string) bool { return strings.HasPrefix(p, "/agents/") }

// isCardPath reports a path under a service that reads a card ({svc}/.well-known/…).
func isCardPath(p string) bool { return strings.Contains(p, "/.well-known/") }

// sealRefused answers a request the sealing refused locally (a pin change, no key for the task, a part that cannot be
// sealed): the LocalError's status, else 502; the pins' refusal names the change and what accepts it (D164, D187).
func (s *Server) sealRefused(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadGateway
	var le *sealing.LocalError
	if errors.As(err, &le) {
		status = le.Status
		if le.Body != nil {
			// An answer the client reads as it is (relay.md §11.12 rule 3's legacy 400, a JSON-RPC error with its id).
			s.log.Info("a request to a sealed service is answered here", "status", status, "path", clip(r.URL.Path), "reason", tunnel.Printable(err.Error()))
			h := w.Header()
			h.Set("Content-Type", "application/json")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Content-Length", strconv.Itoa(len(le.Body)))
			w.WriteHeader(status)
			_, _ = w.Write(le.Body)
			return
		}
	}
	msg := err.Error()
	var ce *pins.ChangeError
	if errors.As(err, &ce) {
		msg = "kontra connect: " + ce.Detail(pins.Options{})
	}
	s.log.Warn("a request to a sealed service is refused here", "status", status, "path", clip(r.URL.Path), "reason", tunnel.Printable(msg))
	refuse(w, r, status, tunnel.Printable(msg))
}

// refuse answers a local refusal: on an MCP path a JSON-RPC error without id (MCP's rule for an Origin it
// refuses), else plain text. It opens no stream.
func refuse(w http.ResponseWriter, r *http.Request, status int, msg string) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	if status == http.StatusMethodNotAllowed {
		h.Set("Allow", "GET, POST, DELETE")
	}
	if status == http.StatusUnauthorized {
		h.Set("WWW-Authenticate", `Bearer realm="kontra connect"`)
	}
	if isMCPPath(r.URL.Path) {
		h.Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","error":{"code":-32600,"message":%s}}`, jsonString(msg))
		return
	}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}

// errNoSession is the forwarding's error while the node has no session to open a stream on.
var errNoSession = errors.New("hiring: no tunnel session to forward on")

// proxyError answers a forwarding that failed: 429 while the session has the gate's cap of node-opened streams in
// flight, 503 while there is no session, 502 otherwise (a card that does not verify included).
func (s *Server) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	if job, _ := r.Context().Value(jobKey{}).(*sealing.Job); job != nil {
		job.Release() // no answer: a new task's provisional key goes (ext-sealed.md §5; tunnel.md §2.3)
	}
	status, msg := http.StatusBadGateway, "kontra connect: the gate's answer could not be forwarded"
	switch {
	case errors.Is(err, ErrBusy):
		status, msg = http.StatusTooManyRequests, "kontra connect: as many requests are in flight on the tunnel as the gate allows; try again"
		w.Header().Set("Retry-After", "1")
	case errors.Is(err, errNoSession):
		status, msg = http.StatusServiceUnavailable, "kontra connect: no tunnel session to the gate right now; try again"
	case errors.Is(err, ErrCardNotVerified):
		msg = "kontra connect: the gate's card signature does not verify; the card is not served"
	case r.Context().Err() != nil:
		s.log.Debug("local client left before the answer", "err", err)
		return
	}
	s.log.Warn("hiring request failed", "status", status, "path", clip(r.URL.Path), "err", err)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}

// modify is the ReverseProxy's ModifyResponse: a card is verified and rewritten (relay.md §3, tunnel.md §9), and
// the kontra-blob: parts of an A2A answer are inlined (relay.md §10.4), JSON and SSE.
//
// Cards. Every GET answer in 2xx on a card route is read whatever its Content-Type: a2a-go's resolver decodes
// any body with encoding/json, which ignores the Content-Type and matches member names without regard to case.
// On a route that must answer a card ({svc}/.well-known/agent-card.json), the answer is a card; on one that may
// ({svc}), it is a card when any of its members is a card's member under case folding (isCard). A card is
// verified (RewriteCard) and is never served unverified; an answer on a card route that is not one JSON object
// with exact keys, an event stream included, is not served either (502).
//
// Parts. Only answers on A2A routes (/agents/…) are walked: MCP and the gate's own routes carry no kontra-blob:
// part (D167: the bucket carries A2A parts only), and an MCP answer is no A2A body (an MCP task's status is a
// string).
func (s *Server) modify(res *http.Response) error {
	req := res.Request
	ctx := req.Context()
	job, _ := ctx.Value(jobKey{}).(*sealing.Job)
	ct := res.Header.Get("Content-Type")
	must, may := false, false
	if req.Method == http.MethodGet && res.StatusCode/100 == 2 {
		must, may = isCardRoute(req.URL.Path)
	}
	cardRoute := must || may
	inline := s.inline != nil && isA2APath(req.URL.Path)
	switch {
	case cardRoute && a2awire.IsEventStream(ct):
		return fmt.Errorf("%w: an event stream on a card route", ErrCardNotVerified)
	case a2awire.IsEventStream(ct):
		if inline {
			pr, pw := io.Pipe()
			body := res.Body
			go func() {
				err := s.inline.InlineSSE(ctx, body, pw, nil)
				_ = body.Close()
				_ = pw.CloseWithError(err)
			}()
			res.Body = pr
			res.Header.Del("Content-Length")
			res.ContentLength = -1
		}
		if job != nil {
			return s.openAnswer(ctx, job, res)
		}
		return nil
	case cardRoute:
		// Read whatever its Content-Type (above).
	case !a2awire.IsJSON(ct) || !inline:
		if job != nil {
			return s.openAnswer(ctx, job, res)
		}
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, a2awire.MaxBody+1))
	if err != nil {
		return err
	}
	if len(b) > a2awire.MaxBody {
		if cardRoute {
			return fmt.Errorf("%w: an answer over %d bytes on a card route", ErrCardNotVerified, a2awire.MaxBody)
		}
		// Too long to read whole: passed as it came, unless it is a sealed job's, which the sealing refuses (it never
		// passes an answer it has not opened, ext-sealed.md §5 rule 4a).
		res.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(b), res.Body), res.Body}
		if job != nil {
			return s.openAnswer(ctx, job, res)
		}
		return nil
	}
	_ = res.Body.Close()
	card := must
	if may && !must {
		if card, err = isCard(b); err != nil {
			return fmt.Errorf("%w: %v", ErrCardNotVerified, err)
		}
	}
	if card {
		out, err := RewriteCard(ctx, b, s.cfg.Gate, s.Local(), s.tunnel)
		if err != nil {
			s.log.Error("a card whose gate signature does not verify is not served (spec/tunnel.md §9)", "path", clip(req.URL.Path), "err", err)
			return err
		}
		if seal := s.sealing(); seal != nil {
			// The card's sealing is recorded, and its pin checked, before it is served (ext-sealed.md §7, D187).
			c, err := s.verified(ctx, b)
			if err != nil {
				return err
			}
			if _, err := seal.CardSeen(ServicePath(req.URL.Path), c); err != nil {
				return fmt.Errorf("%w: %v", ErrCardNotVerified, err)
			}
		}
		setBody(res, out)
		res.Header.Del("ETag") // the hash of the gate's body, not of this one
		return nil
	}
	if inline && a2awire.HasRefs(b) {
		body, err := s.inline.Inline(ctx, b)
		if err != nil {
			return err
		}
		if job != nil {
			// A sealed answer is opened whole, once every blob it names was fetched (its age file authenticated).
			nb, err := body.Bytes()
			_ = body.Close()
			if err != nil {
				return err
			}
			setBody(res, nb)
			return s.openAnswer(ctx, job, res)
		}
		pr, pw := io.Pipe()
		go func() {
			_, err := body.WriteTo(pw)
			_ = body.Close()
			_ = pw.CloseWithError(err)
		}()
		res.Body = pr
		res.ContentLength = body.Len()
		res.Header.Set("Content-Length", strconv.FormatInt(body.Len(), 10))
		return nil
	}
	setBody(res, b)
	if job != nil {
		return s.openAnswer(ctx, job, res)
	}
	return nil
}

// openAnswer opens a sealed job's answer (internal/sealing) in place of the gate's.
func (s *Server) openAnswer(ctx context.Context, job *sealing.Job, res *http.Response) error {
	seal := s.sealing()
	if seal == nil {
		return nil
	}
	out, err := seal.Response(ctx, job, res)
	if err != nil {
		return err
	}
	*res = *out
	return nil
}

func setBody(res *http.Response, b []byte) {
	res.Body = io.NopCloser(bytes.NewReader(b))
	res.ContentLength = int64(len(b))
	res.Header.Set("Content-Length", strconv.Itoa(len(b)))
}

// streamTransport carries one HTTP/1.1 exchange per node-opened stream (§9): the request is written as it is
// (origin-form, its Host the gate's), the response read from the stream, which is closed with the body.
type streamTransport struct {
	open Opener
}

func (t *streamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	octx, cancel := context.WithTimeout(req.Context(), openTimeout)
	st, err := t.open.OpenHire(octx)
	cancel()
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		if errors.Is(err, ErrBusy) {
			return nil, fmt.Errorf("hiring: %w", err)
		}
		return nil, fmt.Errorf("%w: %v", errNoSession, err)
	}
	stop := context.AfterFunc(req.Context(), func() { _ = st.Close() })
	werr := make(chan error, 1)
	go func() { werr <- req.Write(st) }()
	res, err := http.ReadResponse(bufio.NewReader(st), req)
	if err != nil {
		stop()
		_ = st.Close()
		if we := <-werr; we != nil {
			return nil, fmt.Errorf("hiring: the request: %w", we)
		}
		return nil, fmt.Errorf("hiring: the gate's answer: %w", err)
	}
	res.Body = &streamBody{ReadCloser: res.Body, st: st, stop: stop}
	return res, nil
}

// streamBody closes its stream with the body.
type streamBody struct {
	io.ReadCloser
	st   net.Conn
	stop func() bool
}

func (b *streamBody) Close() error {
	err := b.ReadCloser.Close()
	b.stop()
	_ = b.st.Close()
	return err
}

// clip shortens what a client sent before it is logged.
func clip(s string) string {
	const max = 120
	if len(s) > max {
		return strconv.Quote(s[:max]) + "…"
	}
	return strconv.Quote(s)
}

// ReadLocalToken reads --local-token's file: its first line, at least 16 characters.
func ReadLocalToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("--local-token: %w", err)
	}
	tok, _, _ := strings.Cut(string(b), "\n")
	tok = strings.TrimSpace(tok)
	if len(tok) < 16 || strings.ContainsAny(tok, " \t\r") {
		return "", errors.New("--local-token: the file's first line must be a token of at least 16 characters without white space")
	}
	return tok, nil
}
