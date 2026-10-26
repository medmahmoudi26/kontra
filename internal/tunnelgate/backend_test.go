package tunnelgate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/yamux"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnel/tunneltest"
	"kontra.sh/kontra/internal/upstream"
)

// httpNode is the node's side of an in-process session whose streams each carry one HTTP/1.1 exchange
// served by h; it records every preamble.
type httpNode struct {
	mu        sync.Mutex
	preambles []tunnel.Preamble
}

func (n *httpNode) seen() []tunnel.Preamble {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]tunnel.Preamble(nil), n.preambles...)
}

// httpSession is a registered Session over an in-process yamux pair (no database) serving the a2a services,
// its node answering each exchange with h.
func httpSession(t *testing.T, r *Registry, h http.Handler, services ...string) (*Session, *httpNode, *yamux.Session) {
	t.Helper()
	nodeMux, gateMux := tunneltest.NewPair(t).Sessions(t, nil)
	s := &Session{ID: uuid.Must(uuid.NewV7()), KeyID: "k1_" + strings.Repeat("b", 26), TenantID: testTenantID,
		mux: gateMux, served: map[string]string{}}
	for _, name := range services {
		s.served[name] = tunnel.KindA2A
	}
	n := &httpNode{}
	go func() {
		for {
			st, err := nodeMux.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				br := bufio.NewReader(st)
				p, err := tunnel.ReadPreamble(br)
				if err != nil {
					_ = st.Close()
					return
				}
				n.mu.Lock()
				n.preambles = append(n.preambles, p)
				n.mu.Unlock()
				serveOne(&prefixed{Conn: st, r: br}, h)
			}()
		}
	}()
	hold(r, s)
	return s, n, nodeMux
}

// Registry.Backend is the attempt's session as an upstream.Tunnel (relay.md §6.2; tunnel.md §8.1): every
// exchange is a fresh stream whose preamble names the service, its kind and the exchange's gate task and
// context; the stream leaves the session's in-flight set when the exchange ends. A session this replica does
// not hold, another tenant's, or one that no longer serves the service is a lost session (ErrSessionGone and
// upstream.ErrTunnelLost) and opens no stream (relay.md §14.2, key_scope §6.1).
func TestBackendOverSession(t *testing.T) {
	r := NewRegistry(RegistryConfig{})
	s, node, _ := httpSession(t, r, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.WriteString(w, "hello from the node")
	}), "hello", "recon")
	svc := store.ServiceRef{TenantID: testTenantID, Tenant: "default", Name: "hello", Kind: "a2a"}
	be, err := r.Backend(s.ID, svc)
	if err != nil {
		t.Fatal(err)
	}
	tb, ok := be.(*upstream.Tunnel)
	if !ok || tb.Service != "hello" || tb.ServiceKind != tunnel.KindA2A {
		t.Fatalf("backend %T %+v", be, be)
	}
	ctx := upstream.WithTask(context.Background(), upstream.ExchangeTask{TaskID: "task-1", ContextID: "ctx-1"})
	get := func(ctx context.Context) (string, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.Endpoint(be, upstream.Info{Path: "/x"}), nil)
		resp, err := be.HTTPClient().Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return string(b), err
	}
	for range 2 {
		if body, err := get(ctx); err != nil || body != "hello from the node" {
			t.Fatalf("exchange: %q %v", body, err)
		}
	}
	want := tunnel.Preamble{V: 1, Op: tunnel.OpHTTP, Service: "hello", Kind: tunnel.KindA2A, Task: "task-1", Context: "ctx-1"}
	if got := node.seen(); len(got) != 2 || got[0] != want || got[1] != want {
		t.Fatalf("preambles %+v", got)
	}
	// A card fetch belongs to no task: its preamble names none.
	if _, err := get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := node.seen(); len(got) != 3 || got[2].Task != "" || got[2].Context != "" || got[2].Service != "hello" {
		t.Fatalf("a preamble without a task %+v", got)
	}
	eventually(t, 5*time.Second, "the exchanges' streams leave the session", func() bool { return s.InFlight() == 0 })

	gone := func(what string, id uuid.UUID, svc store.ServiceRef) {
		t.Helper()
		if _, err := r.Backend(id, svc); !errors.Is(err, ErrSessionGone) || !errors.Is(err, upstream.ErrTunnelLost) {
			t.Errorf("%s: %v", what, err)
		}
	}
	gone("a session not held here", uuid.Must(uuid.NewV7()), svc)
	other := svc
	other.TenantID = uuid.Must(uuid.NewV7())
	gone("another tenant's session", s.ID, other)
	unserved := svc
	unserved.Name = "elsewhere"
	gone("a service the session does not serve", s.ID, unserved)

	// A binding revoked while a backend is held: its next exchange opens no stream (key_scope).
	be2, err := r.Backend(s.ID, svc)
	if err != nil {
		t.Fatal(err)
	}
	s.dropService("hello", ErrSessionGone)
	gone("the binding revoked", s.ID, svc)
	cctx, ex := upstream.WithExchange(ctx)
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, upstream.Endpoint(be2, upstream.Info{Path: "/x"}), nil)
	_, err = be2.HTTPClient().Do(req)
	if f := upstream.Classify(cctx, err, ex); err == nil || f.Class != upstream.ClassTunnelLost || !errors.Is(err, upstream.ErrTunnelLost) {
		t.Fatalf("an exchange after the binding's revocation: %v (%+v)", err, f)
	}
	if n := len(node.seen()); n != 3 {
		t.Fatalf("%d streams reached the node; the revoked binding's exchange opened one", n)
	}
	// The session closed: every backend of it is gone.
	_ = s.Close("test")
	gone("a closed session", s.ID, store.ServiceRef{TenantID: testTenantID, Name: "recon", Kind: "a2a"})
}

// A response delimited by its stream's close that the session's close cuts is a lost session (tunnel.md
// §8.2: "a stream that ends … while the session is closing is tunnel_lost"): yamux ends the streams of a
// closed session with a clean EOF, which the exchange's reader must not take for the body's end.
func TestSessionCloseCutsACloseDelimitedBody(t *testing.T) {
	r := NewRegistry(RegistryConfig{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{}, 1)
	s, _, nodeMux := httpSession(t, r, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		// HTTP/1.1 without Content-Length or chunking: the body ends when the stream does.
		_, _ = brw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n{\"partial\":")
		_ = brw.Flush()
		started <- struct{}{}
		<-release
	}), "hello")
	be, err := r.Backend(s.ID, store.ServiceRef{TenantID: testTenantID, Name: "hello", Kind: "a2a"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, ex := upstream.WithExchange(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.Endpoint(be, upstream.Info{Path: "/x"}), nil)
	resp, err := be.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(resp.Body)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	// The node's end goes away: the gate's yamux session closes under the exchange, before the registry
	// marks the session closed (no abort): the stream reads a clean EOF.
	_ = nodeMux.Close()
	select {
	case err := <-done:
		if err == nil || !ex.Broken() || upstream.Classify(ctx, err, ex).Class != upstream.ClassTunnelLost {
			t.Fatalf("the cut body read %v (broken %v)", err, ex.Broken())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the body read did not end when the session closed")
	}
}

// silentPostgres is a Postgres that accepts connections and never answers: every read through pool waits
// for its context.
func silentPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	pool, err := pgxpool.New(context.Background(), fmt.Sprintf("postgres://kontra@%s/kontra?sslmode=disable&connect_timeout=0", l.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return pool
}

// The key and binding read an exchange's stream makes while the revocation view is not fresh is bounded by
// allowsReadTimeout whatever the exchange's own context (M2 WP-05 review; WP-04's fail-closed bound): with
// Postgres not answering, an exchange with no deadline of its own fails closed as a lost session within that
// bound, and no stream reaches the node.
func TestExchangeKeyReadFailsClosed(t *testing.T) {
	rev := enroll.NewRevocations(silentPostgres(t)) // never read: not fresh
	r := NewRegistry(RegistryConfig{Revocations: rev})
	s, node, _ := httpSession(t, r, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.WriteString(w, "hello from the node")
	}), "hello")
	be, err := r.Backend(s.ID, store.ServiceRef{TenantID: testTenantID, Name: "hello", Kind: "a2a"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, ex := upstream.WithExchange(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.Endpoint(be, upstream.Info{Path: "/x"}), nil)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		resp, err := be.HTTPClient().Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if took := time.Since(start); took < allowsReadTimeout-200*time.Millisecond || took > allowsReadTimeout+2*time.Second {
			t.Errorf("the exchange failed after %v, want about %v", took, allowsReadTimeout)
		}
		if f := upstream.Classify(ctx, err, ex); err == nil || f.Class != upstream.ClassTunnelLost || !errors.Is(err, upstream.ErrTunnelLost) {
			t.Fatalf("an exchange whose key cannot be read: %v (%+v)", err, f)
		}
	case <-time.After(allowsReadTimeout + 5*time.Second):
		t.Fatalf("the exchange waited on the key's read for over %v", allowsReadTimeout+5*time.Second)
	}
	if n := len(node.seen()); n != 0 {
		t.Fatalf("%d streams reached the node", n)
	}
	if n := s.InFlight(); n != 0 {
		t.Fatalf("%d exchanges in flight", n)
	}
}
