package e2e

import (
	"bufio"
	"context"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/tunnel"
)

// FakeNode is a node in the test process (key_scope): it completes the handshake with a real identity key
// (internal/node's dial and handshake, internal/nodekey's key), runs the yamux client, and serves every
// gate-opened stream itself. It reads the control stream and ignores every line on it, {"t": "revoked"}
// included, so a test proves that the gate never relies on the node honouring a revocation (spec/tunnel.md
// §12, relay.md §6.1). It keeps serving the services it declared whatever the gate says, proxying each
// exchange to one target (a slowagent), and records every preamble the gate sends.
type FakeNode struct {
	Session string // the welcome's session id
	Replica string // the welcome's replica id
	Served  []string

	mux     *yamux.Session
	target  *url.URL
	proxies map[string]*httputil.ReverseProxy // by service

	mu        sync.Mutex
	preambles []tunnel.Preamble // gate-opened streams other than the control stream, in arrival order
	controls  []tunnel.Control  // every control line received (and ignored)
}

// DialFakeNode dials gate (public_url), proves key (the identity file of a node, so the FakeNode holds that
// node's key), declares services as a2a, sends tokens as --enroll would, and serves every exchange by
// proxying it to target.
func DialFakeNode(ctx context.Context, gate string, key *nodekey.Identity, services []string, tokens []string, target string) (*FakeNode, error) {
	m := map[string]FakeService{}
	for _, s := range services {
		m[s] = FakeService{Kind: tunnel.KindA2A, Target: target}
	}
	return DialFakeNodeServices(ctx, gate, key, m, tokens)
}

// FakeService is one service a FakeNode declares: its kind (a2a or mcp) and the target its exchanges are proxied
// to (an MCP target's path is its endpoint: the gate's "/" goes to it, as kontra connect does).
type FakeService struct {
	Kind   string
	Target string
}

// DialFakeNodeServices is DialFakeNode with services of either kind, each proxied to its own target (M3:
// mcp_session_revoked, sealed_unsealed_part).
func DialFakeNodeServices(ctx context.Context, gate string, key *nodekey.Identity, services map[string]FakeService, tokens []string) (*FakeNode, error) {
	gu, err := url.Parse(gate)
	if err != nil {
		return nil, err
	}
	proxies := map[string]*httputil.ReverseProxy{}
	var first *url.URL
	for _, name := range slices.Sorted(maps.Keys(services)) {
		tu, err := url.Parse(services[name].Target)
		if err != nil {
			return nil, err
		}
		if first == nil {
			first = tu
		}
		mcp := services[name].Kind == tunnel.KindMCP
		proxies[name] = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(&url.URL{Scheme: tu.Scheme, Host: tu.Host})
				if mcp && (pr.In.URL.Path == "/" || pr.In.URL.Path == "") {
					pr.Out.URL.Path = tu.Path
				} else {
					pr.Out.URL.Path = strings.TrimSuffix(tu.Path, "/") + pr.In.URL.Path
				}
				pr.Out.Host = tu.Host
			},
			FlushInterval: -1,
			Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		}
	}
	d, err := node.Dialer{UserAgent: node.UserAgent("e2e-fakenode")}.Dial(ctx, gu)
	if err != nil {
		return nil, err
	}
	h := &tunnel.Hello{Name: "fakenode", Agent: node.UserAgent("e2e-fakenode"), Enroll: tokens}
	for _, s := range slices.Sorted(maps.Keys(services)) {
		h.Services = append(h.Services, tunnel.Service{Kind: services[s].Kind, Name: s})
	}
	w, err := node.Handshake(ctx, d, key, h, false)
	if err != nil {
		return nil, err
	}
	m, err := tunnel.ClientSession(context.Background(), d.WS, slog.New(slog.DiscardHandler))
	if err != nil {
		_ = d.WS.CloseNow()
		return nil, err
	}
	f := &FakeNode{Session: w.Session, Replica: w.Replica, mux: m, target: first, proxies: proxies}
	for _, s := range w.Services {
		f.Served = append(f.Served, s.Name)
	}
	go f.accept()
	return f, nil
}

func (f *FakeNode) accept() {
	first := true
	for {
		st, err := f.mux.AcceptStream()
		if err != nil {
			return
		}
		br := bufio.NewReaderSize(st, tunnel.MaxPreamble)
		if first {
			// §7, §13: the first gate-opened stream is the control stream. Its lines are read and ignored.
			first = false
			go func() {
				if p, err := tunnel.ReadPreamble(br); err != nil || p.Op != tunnel.OpControl {
					_ = st.Close()
					return
				}
				for {
					c, err := tunnel.ReadControl(br)
					if err != nil {
						return
					}
					f.mu.Lock()
					f.controls = append(f.controls, c)
					f.mu.Unlock()
				}
			}()
			continue
		}
		go func() {
			defer st.Close()
			p, err := tunnel.ReadPreamble(br)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.preambles = append(f.preambles, p)
			f.mu.Unlock()
			proxy, ok := f.proxies[p.Service]
			if !ok {
				// Not one it declared: refused as a node refuses it (tunnel.md §8.1).
				serveOne(&bufferedStream{Conn: st, r: br}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set(tunnel.HeaderNode, string(tunnel.NodeNotHosted))
					w.WriteHeader(http.StatusNotFound)
				}))
				return
			}
			serveOne(&bufferedStream{Conn: st, r: br}, proxy)
		}()
	}
}

// Preambles returns every preamble the gate has sent on an exchange stream so far.
func (f *FakeNode) Preambles() []tunnel.Preamble {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tunnel.Preamble(nil), f.preambles...)
}

// Controls returns every control line received so far (all of them ignored).
func (f *FakeNode) Controls() []tunnel.Control {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tunnel.Control(nil), f.controls...)
}

// Closed reports whether the session has ended.
func (f *FakeNode) Closed() bool { return f.mux.IsClosed() }

// Close ends the session.
func (f *FakeNode) Close() error { return f.mux.Close() }

// bufferedStream is a stream whose first bytes (after the preamble) are in r's buffer.
type bufferedStream struct {
	net.Conn
	r *bufio.Reader
}

func (s *bufferedStream) Read(p []byte) (int, error) { return s.r.Read(p) }

// serveOne serves the one HTTP/1.1 exchange on c with h (spec/tunnel.md §8.2: one exchange per stream).
func serveOne(c net.Conn, h http.Handler) {
	l := &oneConn{ch: make(chan net.Conn, 1), done: make(chan struct{})}
	l.ch <- &closingConn{Conn: c, l: l}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	srv.SetKeepAlivesEnabled(false)
	_ = srv.Serve(l)
}

type oneConn struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func (l *oneConn) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *oneConn) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *oneConn) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

type closingConn struct {
	net.Conn
	l *oneConn
}

func (c *closingConn) Close() error { _ = c.l.Close(); return c.Conn.Close() }
