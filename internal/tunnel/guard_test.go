package tunnel_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnel/tunneltest"
)

// frames encodes n yamux frame headers (no bodies): version 0, typ, flags, the stream id of i, length.
func frames(n int, typ byte, flags uint16, stream func(i int) uint32, length uint32) []byte {
	b := make([]byte, 12*n)
	for i := range n {
		h := b[i*12 : i*12+12]
		h[1] = typ
		binary.BigEndian.PutUint16(h[2:4], flags)
		binary.BigEndian.PutUint32(h[4:8], stream(i))
		binary.BigEndian.PutUint32(h[8:12], length+uint32(i))
	}
	return b
}

// A node that floods ping SYNs while it reads nothing loses its session, and the gate's goroutines stay
// bounded: yamux v0.1.2 answers each ping on a goroutine that waits up to ConnectionWriteTimeout for the
// send queue, so without the gate's ping budget 12 bytes of input pinned a goroutine for 15 s (M2 review,
// security-1). A node that pings within the budget keeps its session.
func TestGatePingBudget(t *testing.T) {
	t.Run("a flood from a node that does not read", func(t *testing.T) {
		p := tunneltest.NewPair(t)
		gate, err := tunnel.ServerSession(context.Background(), p.Gate, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer gate.Close()
		base := runtime.NumGoroutine()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// The node: raw frames on the WebSocket, and never a read.
		ping := frames(1024, 2, 1, func(int) uint32 { return 0 }, 0)
		peak := 0
		for range 64 {
			if err := p.Node.Write(ctx, websocket.MessageBinary, ping); err != nil {
				break // the gate closed the connection
			}
			peak = max(peak, runtime.NumGoroutine()-base)
		}
		eventually(t, 5*time.Second, "the gate's session closed after the flood", gate.IsClosed)
		if bound := tunnel.PingBurst + 100; peak > bound {
			t.Fatalf("the flood raised the gate's goroutines by %d, want at most %d", peak, bound)
		}
	})
	t.Run("pings within the budget", func(t *testing.T) {
		p := tunneltest.NewPair(t)
		node, gate := p.Sessions(t, nil)
		for range tunnel.PingBurst {
			if _, err := node.Ping(); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(2 * tunnel.PingRefill)
		for range 4 {
			if _, err := node.Ping(); err != nil {
				t.Fatal(err)
			}
			time.Sleep(tunnel.PingRefill)
		}
		if gate.IsClosed() || node.IsClosed() {
			t.Fatal("a session closed on pings within the budget")
		}
	})
}

// A node that stops reading fails the gate's writes within ConnectionWriteTimeout, which closes the session,
// instead of holding yamux's send loop, and every frame queued behind it, until the session ends some other
// way (M2 review, security-1).
func TestStalledPeerClosesTheSession(t *testing.T) {
	p := tunneltest.NewPair(t)
	cfg := tunneltest.ShortConfig(300 * time.Millisecond)
	gate, err := tunnel.ServerSessionWith(context.Background(), p.Gate, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	// The node reads nothing; the gate writes more than the loopback's socket buffers hold: 64 streams (the
	// open backlog), each a full initial window (256 KiB).
	chunk := make([]byte, 256<<10)
	var wg sync.WaitGroup
	for range 64 {
		st, err := gate.OpenStream()
		if err != nil {
			break
		}
		wg.Go(func() {
			_ = st.SetWriteDeadline(time.Now().Add(20 * time.Second))
			_, _ = st.Write(chunk)
		})
	}
	eventually(t, 10*time.Second, "the gate's session closed on a peer that stopped reading", gate.IsClosed)
	wg.Wait()
}

// yamux logs a line for every frame of a stream it does not hold; a session's yamux lines are rate-limited,
// with a line saying that lines are suppressed and how many were, so that a node cannot make the gate write
// some 25 bytes of log for each byte it sends (M2 review, security-2).
func TestYamuxLinesAreRateLimited(t *testing.T) {
	p := tunneltest.NewPair(t)
	var mu sync.Mutex
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, nil))
	gate, err := tunnel.ServerSession(context.Background(), p.Gate, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Window updates for odd stream ids the node never opened: one "frame for missing stream" line each.
	const sent = 4096
	updates := frames(sent, 1, 0, func(i int) uint32 { return uint32(1001 + 2*i) }, 1)
	if err := p.Node.Write(ctx, websocket.MessageBinary, updates); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // yamux reads them all

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	missing := strings.Count(out, "frame for missing stream")
	if missing == 0 || missing > tunnel.LogBurst+2 {
		t.Fatalf("%d frames for missing streams logged %d lines, want at most %d", sent, missing, tunnel.LogBurst+2)
	}
	if !strings.Contains(out, "suppressing them") {
		t.Fatalf("no line says yamux lines are suppressed:\n%s", out)
	}
	if gate.IsClosed() {
		t.Fatal("the session closed: frames for missing streams are logged, not refused")
	}

	// The next line let through, a second later, reports how many were suppressed.
	time.Sleep(tunnel.LogRefill + 100*time.Millisecond)
	if err := p.Node.Write(ctx, websocket.MessageBinary, frames(1, 1, 0, func(int) uint32 { return 99999 }, 1)); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the count of suppressed lines", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(buf.String(), "lines were suppressed") && strings.Contains(buf.String(), "suppressed="+strconv.Itoa(sent-missing))
	})
}
