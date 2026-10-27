package node

import (
	"bufio"
	"bytes"
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

	"kontra.sh/kontra/internal/a2awire"
	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/mcproots"
	"kontra.sh/kontra/internal/obs"
	"kontra.sh/kontra/internal/stdiobridge"
	"kontra.sh/kontra/internal/strictjson"
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
// MCP services (§10, M3 WP-05): a stdio service's exchanges go to its internal/stdiobridge Bridge, which runs the
// server, one child per MCP session, and dials nothing; a URL service's go to its target like an A2A service's, the
// gate's "/" to the target's endpoint path, through the roots filter of mcp.go (D173): the node reads an MCP body
// with exact keys, answers the server's roots/list itself and never relays the client's roots; nothing else in an
// MCP message is changed.
//
// A url part in a body is never dialed (D56). The node reads the JSON body of a request to an A2A service only to
// find the parts the gate wrote as kontra-blob:<ref> (relay.md §10.4: a part over 256 KiB reaches a node so), and
// replaces each with a raw part holding the object, fetched on a blob stream it opens in the same session
// (§8.3; at most 64 MiB each, spilled to a temporary file over 8 MiB), so that a stock agent never sees the
// scheme. A request whose part cannot be fetched is 400 and never reaches the service. Nothing else in a body is
// read or changed.
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
	// inline fetches the kontra-blob: parts of a request on the session's blob streams (setBlobOpener); nil
	// forwards bodies as they came.
	inline *a2awire.Inliner
	// bridges serve the stdio MCP services (internal/stdiobridge), mcp the URL MCP services' roots filter (mcp.go);
	// both per service, shared by forSession's proxies. A proxy is the owner of the stdio sessions opened through it
	// (its tunnel session's, release).
	bridges map[string]*stdiobridge.Bridge
	mcp     map[string]*mcpURL

	mu       sync.Mutex
	served   map[string]bool
	inflight atomic.Int64
}

// MCPOptions configure the MCP services a proxy hosts (spec/tunnel.md §10): the stdio children's --env names,
// --session-idle and --max-sessions, and each service's --roots.
type MCPOptions struct {
	Env         []string
	SessionIdle time.Duration
	MaxSessions int
	Roots       map[string]*mcproots.Roots
	// LookupEnv reads the node's environment for the children; nil is os.LookupEnv.
	LookupEnv func(string) (string, bool)
}

// NewProxy returns a proxy for the services the session serves, its MCP services with MCPOptions' defaults.
func NewProxy(serves []Serve, maxInflight int, logger *slog.Logger) *Proxy {
	p, err := NewMCPProxy(serves, maxInflight, MCPOptions{}, logger)
	if err != nil {
		p.logger.Error("a hosted MCP service cannot be served", "err", err)
	}
	return p
}

// NewMCPProxy returns a proxy for the services the session serves, its MCP services configured by opts: a bridge per
// stdio service (no child runs before an initialize) and a roots filter per URL service. On an error it still
// returns the proxy, without the service that failed.
func NewMCPProxy(serves []Serve, maxInflight int, opts MCPOptions, logger *slog.Logger) (*Proxy, error) {
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
		bridges:     map[string]*stdiobridge.Bridge{},
		mcp:         map[string]*mcpURL{},
	}
	var errs []error
	for _, s := range serves {
		p.serves[s.Name] = s
		p.served[s.Name] = true
		if s.Target != nil {
			p.transports[s.Name] = targetTransport(s.Target)
		}
		if s.Kind != tunnel.KindMCP {
			continue
		}
		if s.Command == nil {
			p.mcp[s.Name] = newMCPURL(s.Name, opts.Roots[s.Name], logger.With(obs.KeyService, s.Name))
			continue
		}
		b, err := stdiobridge.New(stdiobridge.Config{Service: s.Name, Command: s.Command, Env: opts.Env, LookupEnv: opts.LookupEnv,
			SessionIdle: opts.SessionIdle, MaxSessions: opts.MaxSessions, Roots: opts.Roots[s.Name]}, logger)
		if err != nil {
			errs = append(errs, fmt.Errorf("--serve mcp:%s: %w", s.Name, err))
			continue
		}
		p.bridges[s.Name] = b
	}
	return p, errors.Join(errs...)
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
		bridges:     p.bridges,
		mcp:         p.mcp,
	}
	q.SetServed(served)
	return q
}

// setBlobOpener makes the proxy inline the kontra-blob: parts of A2A requests, fetched on blob streams that open
// opens (the session's own, §8.3).
func (p *Proxy) setBlobOpener(open func(ctx context.Context) (net.Conn, error)) {
	p.inline = &a2awire.Inliner{Fetch: func(ctx context.Context, ref blob.Ref, w io.Writer) (int64, error) {
		st, err := open(ctx)
		if err != nil {
			return 0, err
		}
		return blob.Fetch(ctx, st, ref, w)
	}}
}

// closeIdle closes the targets' idle connections (the node is stopping).
func (p *Proxy) closeIdle() {
	for _, t := range p.transports {
		t.CloseIdleConnections()
	}
}

// close stops every stdio child and its process group (the node is stopping) and closes the targets' idle
// connections.
func (p *Proxy) close() {
	var wg sync.WaitGroup
	for _, b := range p.bridges {
		wg.Add(1)
		go func() { defer wg.Done(); _ = b.Close() }()
	}
	wg.Wait()
	p.closeIdle()
}

// release stops the stdio children of the MCP sessions opened through p, a session's proxy, whose tunnel session
// closed: the gate ends those MCP sessions with it (relay.md §11.2), so nothing could reach them again.
func (p *Proxy) release() {
	for _, b := range p.bridges {
		b.CloseOwner(p)
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
		if sv.Kind == tunnel.KindMCP && sv.Command != nil {
			// A stdio MCP service: the bridge is the server's endpoint, whatever the path (§10); nothing is dialed.
			b := p.bridges[sv.Name]
			if b == nil {
				nodeAnswer(w, http.StatusBadGateway, tunnel.NodeTargetUnreachable, "kontra connect: the stdio service is not running")
				return
			}
			sw := &statusWriter{ResponseWriter: w} // the bridge's own Kontra-Node answers (full, target_unreachable) are kept
			b.ServeHTTP(sw, r.WithContext(stdiobridge.WithOwner(r.Context(), p)))
			log.Debug("tunnel exchange (stdio)", "method", r.Method, "status", sw.status, "duration_ms", time.Since(start).Milliseconds())
			return
		}
		out, ok := targetPath(basePath(sv.Target), r.URL.Path)
		if sv.Kind == tunnel.KindMCP && (r.URL.Path == "/" || r.URL.Path == "") {
			// SPEC-CONFLICT (spec/tunnel.md §8.2 with relay.md §11, M3 WP-05): the gate sends every MCP exchange of a
			// tunnel service to "/" (upstream.Tunnel.Base), and §8.2 prefixes a path not under the base with it, which
			// sends "/" to "<base>/": a server whose one endpoint is /mcp (a Go ServeMux pattern "/mcp") would answer
			// 404 there. The closest faithful behaviour: the gate's root is the target itself, its endpoint exactly as
			// --serve names it (ParseServe); every other path follows §8.2's rule.
			out, ok = sv.Target.Path, true
		}
		if !ok {
			log.Warn("tunnel exchange refused: the request target is not a path under the target's base path")
			nodeAnswer(w, http.StatusBadRequest, "", "the request target is not a path under the service's base path")
			return
		}
		if sv.Kind == tunnel.KindA2A && p.inline != nil {
			done, ok := p.inlineBody(w, r, log)
			if !ok {
				return
			}
			defer done()
		}
		modify := stripResponse
		if f := p.mcp[sv.Name]; sv.Kind == tunnel.KindMCP && f != nil {
			x, ok := f.request(w, r, log, p.answerRoots(sv, out, log))
			if !ok {
				return
			}
			modify = func(res *http.Response) error {
				if err := stripResponse(res); err != nil {
					return err
				}
				return x.response(res)
			}
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
			ModifyResponse: modify,
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

// inlineBody replaces the request's body with one whose kontra-blob: parts are inlined (§8.3, relay.md §10.4),
// when it is JSON and holds the scheme; done releases what the inlining holds once the exchange is over. A
// request whose parts cannot be fetched never reaches the service, and ok is false:
//
//   - for good (inlineFinal: the gate refused a ref, the gate's answer breaks §8.3, the body is not an A2A body
//     with exact keys, too much in all): 400 without Kontra-Node (D151: the gate fails the task upstream_error);
//   - for now (a blob stream that found no slot of the session within its wait, could not be opened, failed in
//     transit or ended unanswered, as the gate ends one over its cap of node-opened streams): 503 with Kontra-Node:
//     full, which the gate retries (on another session before the first upstream task id, with backoff after it,
//     relay.md §6.4).
//
// SPEC-CONFLICT (spec/tunnel.md §8.2, §8.3, §14): §8.3 has the node refuse an announced size over 64 MiB and more
// bytes than announced, and §8.2 closes the list of reasons a node's own answer carries; neither says what the
// exchange whose part cannot be fetched answers. The closest faithful behaviour: a refusal that would come again is
// D151's 400 without Kontra-Node, final, like the node's other 400s; a fetch that may succeed if asked again is
// §8.2's full (503), the reason the gate already retries, since the node then had no room for the exchange's
// stream: the node keeps its node-opened streams under the gate's cap (§14) and waits for a slot first. full
// otherwise names the node's own in-flight cap (--max-inflight); the owner may want a reason of its own for it.
func (p *Proxy) inlineBody(w http.ResponseWriter, r *http.Request, log *slog.Logger) (done func(), ok bool) {
	none := func() {}
	if r.Body == nil || r.Body == http.NoBody {
		return none, true
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !a2awire.IsJSON(ct) {
		return none, true
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, a2awire.MaxBody+1))
	if err != nil {
		nodeAnswer(w, http.StatusBadRequest, "", "kontra connect: the request's body could not be read")
		return nil, false
	}
	if len(b) > a2awire.MaxBody {
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(b), r.Body), r.Body}
		return none, true
	}
	if !a2awire.HasRefs(b) {
		r.Body = io.NopCloser(bytes.NewReader(b))
		return none, true
	}
	body, err := p.inline.Inline(r.Context(), b)
	if err != nil {
		if !inlineFinal(err) {
			log.Warn("tunnel exchange refused for now: a kontra-blob: part could not be fetched; the gate retries it", "err", err)
			nodeAnswer(w, http.StatusServiceUnavailable, tunnel.NodeFull,
				"kontra connect: a part of the request could not be fetched from the gate now; try again")
			return nil, false
		}
		log.Warn("tunnel exchange refused: a kontra-blob: part could not be fetched", "err", err)
		nodeAnswer(w, http.StatusBadRequest, "", "kontra connect: a part of the request could not be fetched from the gate")
		return nil, false
	}
	pr, pw := io.Pipe()
	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		_, err := body.WriteTo(pw)
		_ = pw.CloseWithError(err)
	}()
	r.Body = pr
	r.ContentLength = body.Len()
	r.Header.Del("Content-Length")
	r.TransferEncoding = nil
	log.Debug("tunnel exchange: kontra-blob: parts inlined", "bytes", body.Len())
	return func() {
		_ = pr.Close()
		<-wrote
		_ = body.Close()
	}, true
}

// inlineFinal reports an inlining that would fail again if asked again: a ref the gate refused (*blob.FetchError:
// not_found, forbidden, too_large), an answer that breaks §8.3 (blob.ErrBadAnswer: a malformed size line, a size
// over 64 MiB, more bytes than announced), a body that is not an A2A body with exact keys (a2awire.ErrShape,
// *strictjson.KeyError), a kontra-blob: url that names no ref (a2awire.ErrReservedURL), more than the inlining's
// total (a2awire.ErrTooMuch). Anything else is the stream's: its slot, its open, its transit.
func inlineFinal(err error) bool {
	var (
		fe *blob.FetchError
		ke *strictjson.KeyError
	)
	return errors.As(err, &fe) || errors.As(err, &ke) || errors.Is(err, blob.ErrBadAnswer) || errors.Is(err, a2awire.ErrShape) ||
		errors.Is(err, a2awire.ErrReservedURL) || errors.Is(err, a2awire.ErrTooMuch) || errors.Is(err, strictjson.ErrNotObject)
}

// targetPath is the path a request goes to (§8.2): the request path, which net/http has percent-decoded into
// URL.Path once, cleaned (., .., //); a path already under the target's base path (without its trailing slash, ""
// for the root) is forwarded as it is, any other is prefixed with the base and cleaned again; ok is false when the
// result is not under the base. A trailing slash is kept, as the service's routes may tell "/a/" from "/a". The
// decoded path is what is sent: an encoded '/' (%2F) becomes a separator, so no segment can hide a '..' from the
// clean.
//
// The prefix comes before the second clean, so that a '..' climbing out of a non-root base is refused (400) and
// not silently folded into it; with the root base every cleaned path is under it and is confined to the target (M2
// plan audit). The "already under the base" rule is §8.2's as D123 amended it: the gate's A2A paths come from the
// upstream card's interface, which already holds the base, and an MCP URL target is its endpoint (exchange maps the
// gate's "/" to it).
func targetPath(base, reqPath string) (string, bool) {
	if reqPath == "" {
		reqPath = "/"
	}
	if reqPath[0] != '/' {
		return "", false // "*" (OPTIONS *) and other non-paths
	}
	under := func(p string) bool { return base == "" || p == base || strings.HasPrefix(p, base+"/") }
	keepSlash := func(c string) string {
		if strings.HasSuffix(reqPath, "/") && c != "/" {
			return c + "/"
		}
		return c
	}
	if c := keepSlash(path.Clean(reqPath)); base != "" && under(c) {
		return c, true
	}
	cleaned := keepSlash(path.Clean(base + reqPath))
	if under(cleaned) {
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

// statusWriter records the status of a response it passes on unchanged.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if code >= 200 && w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush sends what was written (the bridge's event streams are flushed per event).
func (w *statusWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

// Unwrap lets http.ResponseController reach the stream's writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

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
