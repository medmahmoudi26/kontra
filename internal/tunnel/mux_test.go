package tunnel_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnel/tunneltest"
)

// §7's table, and yamux's own check of it.
func TestYamuxConfig(t *testing.T) {
	c := tunnel.YamuxConfig(nil)
	if err := yamux.VerifyConfig(c); err != nil {
		t.Fatalf("VerifyConfig: %v", err)
	}
	for _, f := range []struct {
		name      string
		got, want any
	}{
		{"AcceptBacklog", c.AcceptBacklog, 64},
		{"EnableKeepAlive", c.EnableKeepAlive, false},
		{"KeepAliveInterval", c.KeepAliveInterval, 20 * time.Second},
		{"ConnectionWriteTimeout", c.ConnectionWriteTimeout, 15 * time.Second},
		{"MaxStreamWindowSize", c.MaxStreamWindowSize, uint32(256 << 10)},
		{"StreamOpenTimeout", c.StreamOpenTimeout, 30 * time.Second},
		{"StreamCloseTimeout", c.StreamCloseTimeout, 5 * time.Minute},
	} {
		if f.got != f.want {
			t.Errorf("%s = %v, want %v", f.name, f.got, f.want)
		}
	}
	if c.LogOutput != nil || c.Logger == nil {
		t.Errorf("LogOutput %v, Logger %v: want the slog logger alone", c.LogOutput, c.Logger)
	}

	// yamux's lines reach the slog handler at Warn.
	var buf bytes.Buffer
	c = tunnel.YamuxConfig(slog.New(slog.NewTextHandler(&buf, nil)))
	c.Logger.Printf("[ERR] yamux: %s", "a line")
	if got := buf.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "component=yamux") ||
		!strings.Contains(got, "a line") {
		t.Errorf("yamux's line logged as %q", got)
	}
}

// yamux's keepalive closes on the first missed pong (D33); a session with it on is refused.
func TestSessionRefusesYamuxKeepAlive(t *testing.T) {
	p := tunneltest.NewPair(t)
	c := tunnel.YamuxConfig(nil)
	c.EnableKeepAlive = true
	if _, err := tunnel.ServerSessionWith(context.Background(), p.Gate, c); err == nil {
		t.Fatal("a server session with yamux keepalive on started")
	}
	if _, err := tunnel.ClientSessionWith(context.Background(), p.Node, c); err == nil {
		t.Fatal("a client session with yamux keepalive on started")
	}
}

// The node is yamux.Client (odd stream ids), the gate yamux.Server (even), and each side accepts what the
// other opens (§1).
func TestSessionRoles(t *testing.T) {
	p := tunneltest.NewPair(t)
	ctx := context.Background()
	node, err := tunnel.ClientSession(ctx, p.Node, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	gate, err := tunnel.ServerSession(ctx, p.Gate, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()

	echo := func(s *yamux.Session) {
		st, err := s.AcceptStream()
		if err != nil {
			return
		}
		defer st.Close()
		b := make([]byte, 4)
		if _, err := io.ReadFull(st, b); err == nil {
			_, _ = st.Write(b)
		}
	}
	for _, c := range []struct {
		name         string
		open, accept *yamux.Session
		odd          bool
	}{{"gate opens", gate, node, false}, {"node opens", node, gate, true}} {
		go echo(c.accept)
		st, err := c.open.OpenStream()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := st.StreamID()%2 == 1; got != c.odd {
			t.Errorf("%s: stream id %d, want odd=%v", c.name, st.StreamID(), c.odd)
		}
		if _, err := st.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 4)
		if _, err := io.ReadFull(st, b); err != nil || string(b) != "ping" {
			t.Fatalf("%s: echo %q, %v", c.name, b, err)
		}
		_ = st.Close()
	}
}

// SSE over a gate-opened stream through the node's http.Server reaches the gate event by event, and
// killing the node's connection mid-stream surfaces as io.ErrUnexpectedEOF on the gate's HTTP client
// with the gate's session closed (research §3.3, probe tunnel [basic]).
func TestSSEOverStreamAndTunnelLoss(t *testing.T) {
	p := tunneltest.NewPair(t)
	node, gate := p.Sessions(t, nil)

	next := make(chan struct{})
	written := make(chan int, 16)
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 1; ; i++ {
			if _, err := fmt.Fprintf(w, "id: %d\ndata: event %d\n\n", i, i); err != nil {
				return
			}
			fl.Flush()
			written <- i
			select {
			case <-next:
			case <-r.Context().Done():
				return
			}
		}
	})
	// The node's server for tunnel streams: a header read timeout only, never a write timeout (§8.2).
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(node) }()
	go func() { <-node.CloseChan(); _ = srv.Close() }() // yamux issue #56
	defer srv.Close()

	// The gate's client: one stream per exchange, never reused, no overall timeout (§8.2).
	client := &http.Client{Transport: &http.Transport{
		DialContext:       func(context.Context, string, string) (net.Conn, error) { return gate.Open() },
		DisableKeepAlives: true,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://node.invalid/sse", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q", ct)
	}
	r := bufio.NewReader(resp.Body)
	readEvent := func() string {
		t.Helper()
		var data string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("read event: %v", err)
			}
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			}
			if line == "\n" {
				return data
			}
		}
	}
	for i := 1; i <= 3; i++ {
		// The handler writes event i+1 only after the gate has read event i: an event that anything on
		// the way buffered would never arrive, and the read would fail at the context's deadline.
		if got, want := readEvent(), fmt.Sprintf("event %d", i); got != want {
			t.Fatalf("event %q, want %q", got, want)
		}
		if n := <-written; n != i {
			t.Fatalf("event %d read while the node had written %d", i, n)
		}
		if i < 3 {
			next <- struct{}{}
		}
	}

	// The node dies mid-stream (its process killed: the TCP connection closes).
	if err := p.NodeWire.Cut(); err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(r)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read after the node's connection was killed: %v, want io.ErrUnexpectedEOF", err)
	}
	eventually(t, 5*time.Second, "the gate's session reports IsClosed", gate.IsClosed)
}

// Closing a session sends the WebSocket close as StatusNormalClosure, which the other side reads as
// io.EOF: its yamux ends without "[ERR] yamux: Failed to read header" (research §3.5.8).
func TestNormalCloseLogsNoError(t *testing.T) {
	p := tunneltest.NewPair(t)
	var mu sync.Mutex
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, nil))
	ctx := context.Background()
	node, err := tunnel.ClientSession(ctx, p.Node, logger)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := tunnel.ServerSession(ctx, p.Gate, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	if _, err := gate.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the gate's session closes when the node's does", gate.IsClosed)
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(buf.String(), "Failed to read header") {
		t.Errorf("yamux logged a read error on a normal close:\n%s", buf.String())
	}
}

// CloseStatusOf reads the handshake code of a close status 4000 + i, and nothing else.
func TestCloseStatusOf(t *testing.T) {
	for _, c := range []struct {
		err  error
		code tunnel.Code
		ok   bool
	}{
		{websocket.CloseError{Code: 4001, Reason: "bad_origin"}, tunnel.CodeBadOrigin, true},
		{fmt.Errorf("read: %w", websocket.CloseError{Code: 4010}), tunnel.CodeDraining, true},
		{websocket.CloseError{Code: 4011}, tunnel.CodeAuthInvalid, true},
		{websocket.CloseError{Code: 4012}, "", false},
		{websocket.CloseError{Code: websocket.StatusNormalClosure}, "", false},
		{websocket.CloseError{Code: websocket.StatusInternalError}, "", false},
		{io.EOF, "", false},
		{nil, "", false},
	} {
		code, ok := tunnel.CloseStatusOf(c.err)
		if code != c.code || ok != c.ok {
			t.Errorf("CloseStatusOf(%v) = %q, %v; want %q, %v", c.err, code, ok, c.code, c.ok)
		}
	}

	// Over a real connection: the gate closes with draining's status, the node reads it.
	p := tunneltest.NewPair(t)
	status, _ := tunnel.CodeDraining.CloseStatus()
	go func() { _ = p.Gate.Close(websocket.StatusCode(status), string(tunnel.CodeDraining)) }()
	_, _, err := p.Node.Read(context.Background())
	if code, ok := tunnel.CloseStatusOf(err); !ok || code != tunnel.CodeDraining || !code.Retryable() {
		t.Fatalf("node read %v: code %q, %v", err, code, ok)
	}
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", within, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
