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
// http://localhost:<port>/… URLs; a card whose signature does not verify is not served. Sealing and opening for
// sealed services happen around the forwarding (internal/sealing, M3 WP-08); blobs a reply names as
// kontra-blob:<ref> are fetched on blob streams and inlined (internal/blob).
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
	"kontra.sh/kontra/internal/tunnel"
)

// DefaultListen is --listen's default.
const DefaultListen = "127.0.0.1:7777"

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
// session or is draining (no new hiring streams on a draining session, spec/tunnel.md §13).
type Opener interface {
	OpenHire(ctx context.Context) (net.Conn, error)
}

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
	s.listen, s.local = cfg.Listen, localBase(cfg.Listen)
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

// CheckListen refuses a --listen that is not host:port, and one off loopback without a local token (§9).
func CheckListen(listen, token string) error {
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
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	s.listen = ln.Addr().String()
	s.local = localBase(s.listen)
	s.mu.Unlock()
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
// SPEC-CONFLICT (spec/tunnel.md §9): §9 accepts only 127.0.0.1:<port>, localhost:<port> and [::1]:<port> as Host,
// and also allows --listen on a non-loopback address with --local-token, whose clients reach it by that address.
// The closest faithful behaviour: with a non-loopback --listen of a specific address (never 0.0.0.0 or ::), that
// address with the port is accepted too; the local token, which a web page cannot know, guards it.
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

// proxyError answers a forwarding that failed: 503 while there is no session, 502 otherwise (a card that does
// not verify included).
func (s *Server) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := http.StatusBadGateway, "kontra connect: the gate's answer could not be forwarded"
	switch {
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
// the kontra-blob: parts of an answer are inlined (relay.md §10.4), JSON and SSE.
func (s *Server) modify(res *http.Response) error {
	req := res.Request
	ctx := req.Context()
	ct := res.Header.Get("Content-Type")
	must, may := false, false
	if req.Method == http.MethodGet && res.StatusCode/100 == 2 {
		must, may = isCardRoute(req.URL.Path)
	}
	switch {
	case a2awire.IsEventStream(ct):
		if s.inline == nil {
			return nil
		}
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
		return nil
	case !a2awire.IsJSON(ct) && !must:
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, a2awire.MaxBody+1))
	if err != nil {
		return err
	}
	if len(b) > a2awire.MaxBody {
		if must || may {
			return fmt.Errorf("%w: an answer over %d bytes on a card route", ErrCardNotVerified, a2awire.MaxBody)
		}
		// Too long to read whole: passed as it came.
		res.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(b), res.Body), res.Body}
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
		setBody(res, out)
		res.Header.Del("ETag") // the hash of the gate's body, not of this one
		return nil
	}
	if s.inline != nil && a2awire.HasRefs(b) {
		body, err := s.inline.Inline(ctx, b)
		if err != nil {
			return err
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
