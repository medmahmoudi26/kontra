package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kontra.sh/kontra/internal/obs"
	"kontra.sh/kontra/internal/tunnel"
)

// StreamHeaderTimeout bounds the wait for a stream's preamble and then for its request's header. It is the
// only timeout of the node's server for tunnel streams (§8.2: a header read timeout, never a write timeout,
// so an SSE response lasts as long as the gate reads it).
const StreamHeaderTimeout = 30 * time.Second

// targetDialTimeout bounds one dial of a --serve target; a target that does not answer is 502
// target_unreachable.
const targetDialTimeout = 10 * time.Second

// errSteered is a dial to an address other than the target's own host:port.
var errSteered = errors.New("node: a dial to an address other than the --serve target")

// Proxy serves gate-opened streams (§8): it reads the preamble, refuses a service this session does not
// host (404, Kontra-Node: not_hosted) or a kind that does not match (404, kind_mismatch), and over
// MaxInflight streams answers 503 (full). It forwards the stream's one HTTP/1.1 request to the service's
// configured target and nowhere else (sidecar_not_a_proxy):
//
//   - Host and an absolute-form request target are ignored: scheme, host and port come from --serve;
//   - the path is percent-decoded and cleaned (., .., //), prefixed with the target's base path, and a
//     result outside that base is 400;
//   - Host is the target's host; Origin, hop-by-hop headers, Forwarded and X-Forwarded-* are removed, none
//     added; CONNECT, Upgrade and TRACE are 400;
//   - responses stream as they come, text/event-stream flushed per event, never compressed; the stream's
//     server sets a header read timeout only, never a write timeout;
//   - Kontra-Node and Kontra-Sealed-Reject are removed from the service's responses; a target that does not
//     answer is 502 with Kontra-Node: target_unreachable.
//
// A url part in a body is never dialed: the node does not read bodies (D56).
//
// The target's transport never uses a proxy from the environment (HTTP_PROXY would otherwise carry a
// non-loopback target's traffic to that proxy), dials only the target's own host:port whatever address it
// is asked for, and follows no redirect: a 3xx goes back to the gate as the service's answer (M2 plan
// audit).
//
// net/http's own answer to OPTIONS * (200, with no Kontra-Node) is off: OPTIONS * reaches the handler and,
// its request target being no path, is 400 like a path outside the base. Every answer the node writes
// itself has Connection: close.
//
// SPEC-CONFLICT (spec/tunnel.md §8.2 with relay.md §6.4; M2 WP-06 review): §8.2 says the responses the node
// writes itself carry a Kontra-Node header, so the gate never mistakes them for the service's own, and
// lists the reasons as a closed set (not_hosted, kind_mismatch, full, target_unreachable, sealed_rejected);
// the same section has the node answer 400 to CONNECT, an Upgrade, TRACE and a path outside the base, and a
// stream must open with a preamble the node can read (§8.1). No listed reason fits those answers. They
// carry no Kontra-Node header: the gate (upstream.Classify) then takes them as the service's own 400,
// failed upstream_error and never retried, which suits them, since the gate never sends such a request and
// another session would refuse it alike. A value outside §8.2's list would be a wire change, and the gate
// reads any Kontra-Node other than full and sealed_rejected as tunnel_lost, retried on another session.
// Proposal for the owner: a reason for them, such as `refused` (400, not retried), or the rule that a 400
// the node writes carries no Kontra-Node and is final.
//
// One Proxy serves one session: the in-flight count and the served set are the session's (§14: inbound
// streams in flight per session). The sessions of one node share the targets' transports (forSession).
type Proxy struct {
	serves      map[string]Serve
	maxInflight int
	logger      *slog.Logger

	transports map[string]*http.Transport // per service; shared by forSession's proxies
	errLog     *log.Logger                // net/http's own lines about a stream
	// onControl serves the session's control stream (op control, §13); nil refuses it.
	onControl func(stream net.Conn, br *bufio.Reader)

	mu       sync.Mutex
	served   map[string]bool
	inflight atomic.Int64
}

// NewProxy returns a proxy for the services the session serves.
func NewProxy(serves []Serve, maxInflight int, logger *slog.Logger) *Proxy {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if maxInflight <= 0 {
		maxInflight = tunnel.NodeMaxInflight
	}
	p := &Proxy{
		serves:      make(map[string]Serve, len(serves)),
		maxInflight: maxInflight,
		logger:      logger,
		transports:  make(map[string]*http.Transport, len(serves)),
		errLog:      slog.NewLogLogger(logger.With("component", "node-http").Handler(), slog.LevelWarn),
		served:      make(map[string]bool, len(serves)),
	}
	for _, s := range serves {
		p.serves[s.Name] = s
		p.served[s.Name] = true
		if s.Target != nil {
			p.transports[s.Name] = targetTransport(s.Target)
		}
	}
	return p
}

// forSession is a proxy for one session over the same targets and transports, serving served (the
// welcome's services), with its own in-flight count.
func (p *Proxy) forSession(served []string, logger *slog.Logger) *Proxy {
	if logger == nil {
		logger = p.logger
	}
	q := &Proxy{
		serves:      p.serves,
		maxInflight: p.maxInflight,
		logger:      logger,
		transports:  p.transports,
		errLog:      slog.NewLogLogger(logger.With("component", "node-http").Handler(), slog.LevelWarn),
		served:      map[string]bool{},
	}
	q.SetServed(served)
	return q
}

// closeIdle closes the targets' idle connections (the node is stopping).
func (p *Proxy) closeIdle() {
	for _, t := range p.transports {
		t.CloseIdleConnections()
	}
}

// SetServed narrows the hosted services to those the welcome lists, and later drops one the gate revoked
// ({"t": "revoked", "service"}): its preambles are then refused not_hosted. A name the node was not
// started with (--serve) is ignored.
func (p *Proxy) SetServed(names []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.served = make(map[string]bool, len(names))
	for _, n := range names {
		if _, ok := p.serves[n]; ok {
			p.served[n] = true
		}
	}
}

// Drop removes one service from the served set (a binding the gate revoked, §12).
func (p *Proxy) Drop(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.served, name)
}

// Served is the served set, for a log line.
func (p *Proxy) Served() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.served))
	for n := range p.served {
		out = append(out, n)
	}
	return out
}

// hosted is the Serve of a service this session serves.
func (p *Proxy) hosted(name string) (Serve, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.served[name] {
		return Serve{}, false
	}
	s, ok := p.serves[name]
	return s, ok
}

// Inflight is the number of exchanges in flight.
func (p *Proxy) Inflight() int { return int(p.inflight.Load()) }

// ServeStream serves one accepted stream; it returns when the exchange ends.
//
// It reads the preamble within StreamHeaderTimeout. op control goes to the session's control reader (the
// first gate-opened stream, §7, §13); op http is one HTTP/1.1 exchange, served by an http.Server of its
// own with ReadHeaderTimeout only and keep-alives off (one exchange per stream, §8.2); anything else, and a
// preamble that cannot be read (over 4096 bytes, another version, an unknown op), is 400 before any target
// is dialed. The node never forwards the preamble.
func (p *Proxy) ServeStream(ctx context.Context, stream net.Conn) {
	defer stream.Close()
	_ = stream.SetReadDeadline(time.Now().Add(StreamHeaderTimeout))
	br := bufio.NewReaderSize(stream, tunnel.MaxPreamble)
	pre, err := tunnel.ReadPreamble(br)
	if err != nil {
		p.logger.Debug("tunnel stream refused: no valid preamble", "err", err)
		writeRaw400(stream, "kontra connect: the stream has no valid preamble (spec/tunnel.md §8.1)")
		return
	}
	_ = stream.SetReadDeadline(time.Time{})
	switch pre.Op {
	case tunnel.OpControl:
		if p.onControl == nil {
			writeRaw400(stream, "kontra connect: no control stream here")
			return
		}
		p.onControl(stream, br)
	case tunnel.OpHTTP:
		p.serveExchange(ctx, &bufferedConn{Conn: stream, r: br}, pre)
	default:
		// Blob streams are node-opened (§8.3, M3): a gate never opens one.
		writeRaw400(stream, "kontra connect: a gate-opened stream carries op http or control")
	}
}

// serveExchange serves the stream's one HTTP/1.1 exchange with an http.Server of its own.
func (p *Proxy) serveExchange(ctx context.Context, c *bufferedConn, pre tunnel.Preamble) {
	srv := &http.Server{
		Handler:           p.exchange(pre),
		ReadHeaderTimeout: StreamHeaderTimeout, // and nothing else: never a write timeout (SSE)
		ErrorLog:          p.errLog,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		// OPTIONS * goes to the handler (400), not to net/http's own 200 without Kontra-Node.
		DisableGeneralOptionsHandler: true,
	}
	srv.SetKeepAlivesEnabled(false)
	l := newOneConnListener(c)
	stop := context.AfterFunc(ctx, func() { _ = srv.Close() })
	defer stop()
	_ = srv.Serve(l)
}

// exchange is the handler of one stream's request.
func (p *Proxy) exchange(pre tunnel.Preamble) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log := p.logger.With(obs.KeyService, pre.Service)
		if pre.Task != "" {
			log = log.With(obs.KeyTaskID, pre.Task)
		}
		sv, ok := p.hosted(pre.Service)
		switch {
		case !ok:
			log.Info("tunnel exchange refused: the service is not hosted on this session")
			nodeAnswer(w, http.StatusNotFound, tunnel.NodeNotHosted, "kontra connect does not host this service on this session")
			return
		case pre.Kind != sv.Kind:
			log.Info("tunnel exchange refused: kind mismatch", "kind", pre.Kind, "hosted_kind", sv.Kind)
			nodeAnswer(w, http.StatusNotFound, tunnel.NodeKindMismatch, "kontra connect hosts this service as another kind")
			return
		}
		n := p.inflight.Add(1)
		defer p.inflight.Add(-1)
		if n > int64(p.maxInflight) {
			log.Warn("tunnel exchange refused: in-flight cap", "max_inflight", p.maxInflight)
			nodeAnswer(w, http.StatusServiceUnavailable, tunnel.NodeFull, "kontra connect is at its in-flight cap (--max-inflight)")
			return
		}
		switch {
		case r.Method == http.MethodConnect:
			nodeAnswer(w, http.StatusBadRequest, "", "kontra connect is a fixed-target reverse proxy: no CONNECT")
			return
		case r.Method == http.MethodTrace:
			nodeAnswer(w, http.StatusBadRequest, "", "kontra connect refuses TRACE")
			return
		case wantsUpgrade(r.Header):
			nodeAnswer(w, http.StatusBadRequest, "", "kontra connect refuses protocol upgrades")
			return
		}
		out, ok := targetPath(basePath(sv.Target), r.URL.Path)
		if !ok {
			log.Warn("tunnel exchange refused: the request target is not a path under the target's base path")
			nodeAnswer(w, http.StatusBadRequest, "", "the request target is not a path under the service's base path")
			return
		}
		target := sv.Target
		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				// Scheme, host and port from --serve; the request's Host and an absolute-form target are
				// ignored. Rewrite mode has already removed Forwarded and X-Forwarded-{For,Host,Proto} and the
				// hop-by-hop headers, and adds none.
				pr.Out.URL = &url.URL{Scheme: target.Scheme, Host: target.Host, Path: out, RawQuery: pr.Out.URL.RawQuery}
				pr.Out.Host = target.Host
				h := pr.Out.Header
				h.Del("Origin")
				h.Del("Forwarded")
				for k := range h {
					if strings.HasPrefix(k, "X-Forwarded-") {
						delete(h, k)
					}
				}
			},
			Transport:      p.transports[sv.Name],
			ModifyResponse: stripResponse,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				if sw, ok := w.(*stripWriter); ok {
					w = sw.ResponseWriter // the node's own answer keeps its Kontra-Node header
				}
				if r.Context().Err() != nil {
					log.Debug("tunnel exchange ended by the gate before the service answered", "err", err)
				} else {
					log.Warn("tunnel exchange: the service did not answer", "target", origin(target), "err", err)
				}
				nodeAnswer(w, http.StatusBadGateway, tunnel.NodeTargetUnreachable, "kontra connect: the service did not answer")
			},
			ErrorLog: p.errLog,
		}
		sw := &stripWriter{ResponseWriter: w}
		rp.ServeHTTP(sw, r)
		log.Debug("tunnel exchange", "method", r.Method, "path", out, "status", sw.status,
			"duration_ms", time.Since(start).Milliseconds())
	}
}

// targetPath is the path a request goes to (§8.2): the request path, which net/http has percent-decoded
// into URL.Path once, prefixed with the target's base path (without its trailing slash, "" for the root),
// then cleaned (., .., //); ok is false when the result is not under the base. A trailing slash is kept, as
// the service's routes may tell "/a/" from "/a". The decoded path is what is sent: an encoded '/' (%2F)
// becomes a separator, so no segment can hide a '..' from the clean.
//
// The prefix comes before the clean, so that a '..' climbing out of a non-root base is refused (400) and
// not silently folded into it; with the root base every cleaned path is under it and is confined to the
// target (M2 plan audit).
func targetPath(base, reqPath string) (string, bool) {
	if reqPath == "" {
		reqPath = "/"
	}
	if reqPath[0] != '/' {
		return "", false // "*" (OPTIONS *) and other non-paths
	}
	cleaned := path.Clean(base + reqPath)
	if strings.HasSuffix(reqPath, "/") && cleaned != "/" {
		cleaned += "/"
	}
	if base == "" || cleaned == base || strings.HasPrefix(cleaned, base+"/") {
		return cleaned, true
	}
	return "", false
}

// wantsUpgrade reports a request that asks to switch protocols: an Upgrade header, or Connection naming
// upgrade.
func wantsUpgrade(h http.Header) bool {
	if len(h.Values("Upgrade")) > 0 {
		return true
	}
	for _, v := range h.Values("Connection") {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), "upgrade") {
				return true
			}
		}
	}
	return false
}

// nodeAnswer writes a response of the node's own: with a Kontra-Node reason when it is one of §8.2's.
func nodeAnswer(w http.ResponseWriter, status int, reason tunnel.NodeReason, msg string) {
	h := w.Header()
	for k := range h {
		delete(h, k)
	}
	if reason != "" {
		h.Set(tunnel.HeaderNode, string(reason))
	}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Connection", "close")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}

// writeRaw400 answers a stream whose request was never read (no valid preamble) with a 400 and closes it.
func writeRaw400(c net.Conn, msg string) {
	body := msg + "\n"
	_ = c.SetWriteDeadline(time.Now().Add(tunnel.ConnectionWriteTimeout))
	_, _ = fmt.Fprintf(c, "HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain; charset=utf-8\r\n"+
		"X-Content-Type-Options: nosniff\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
}

// stripNodeHeaders removes what only the node may say (§8.2).
func stripNodeHeaders(h http.Header) {
	if h == nil {
		return
	}
	h.Del(tunnel.HeaderNode)
	h.Del(tunnel.HeaderSealedReject)
}

// stripResponse is the ReverseProxy's ModifyResponse: the service's headers and trailers lose Kontra-Node
// and Kontra-Sealed-Reject (§8.2), and a switch of protocols, which a tunnel stream does not carry, is the
// node's 502.
func stripResponse(res *http.Response) error {
	if res.StatusCode == http.StatusSwitchingProtocols {
		return errors.New("the service switched protocols, which a tunnel stream does not carry")
	}
	stripNodeHeaders(res.Header)
	stripNodeHeaders(res.Trailer) // declared trailers: not announced
	res.Body = &trailerStripper{ReadCloser: res.Body, res: res}
	return nil
}

// trailerStripper removes the node's headers from the trailers the transport fills in at the body's end,
// before the ReverseProxy copies them.
type trailerStripper struct {
	io.ReadCloser
	res *http.Response
}

func (t *trailerStripper) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if err != nil {
		stripNodeHeaders(t.res.Trailer)
	}
	return n, err
}

func (t *trailerStripper) Close() error {
	err := t.ReadCloser.Close()
	stripNodeHeaders(t.res.Trailer)
	return err
}

// stripWriter removes the node's headers from every header block the ReverseProxy writes for the service,
// 1xx informational responses included (they are copied from the service as they come).
type stripWriter struct {
	http.ResponseWriter
	status int
}

func (w *stripWriter) WriteHeader(code int) {
	stripNodeHeaders(w.Header())
	if code >= 200 && w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *stripWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush sends what was written (text/event-stream is flushed per event).
func (w *stripWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

// Unwrap lets http.ResponseController reach the stream's writer.
func (w *stripWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// targetTransport is the transport of one --serve target: no proxy from the environment, every dial to the
// target's own host:port (an address other than it is refused), no compression added, no redirect followed
// (a Transport never follows one, and the ReverseProxy hands a 3xx back as the service's answer).
func targetTransport(target *url.URL) *http.Transport {
	want := hostPort(target)
	d := &net.Dialer{Timeout: targetDialTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h, port, err := net.SplitHostPort(addr)
			if err != nil || net.JoinHostPort(strings.ToLower(h), port) != want {
				return nil, fmt.Errorf("%w: %s, not %s", errSteered, addr, want)
			}
			return d.DialContext(ctx, network, want)
		},
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        tunnel.NodeMaxInflight,
		MaxIdleConnsPerHost: tunnel.NodeMaxInflight,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
}

// bufferedConn is a stream whose first bytes (the preamble, and what the reader buffered after it) were
// read through r: the exchange is read from r.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// oneConnListener hands its one connection to http.Server.Serve, then blocks until that connection is
// closed (the exchange ended), so that Serve returns when the stream's exchange is over.
type oneConnListener struct {
	c      net.Conn
	taken  atomic.Bool
	closed chan struct{} // the connection was closed
	done   chan struct{} // the listener was closed
	once   sync.Once
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{closed: make(chan struct{}), done: make(chan struct{})}
	l.c = &notifyConn{Conn: c, closed: l.closed}
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.taken.CompareAndSwap(false, true) {
		return l.c, nil
	}
	select {
	case <-l.closed:
	case <-l.done:
	}
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.c.LocalAddr() }

// notifyConn closes its channel when it is closed.
type notifyConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *notifyConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}
