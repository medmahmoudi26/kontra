package tunnel_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnel/tunneltest"
)

// scripted is a Pinger whose pings succeed or fail as scripted, then succeed.
type scripted struct {
	mu     sync.Mutex
	script []error
	pings  int
	closed int
}

func (s *scripted) Ping() (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pings++
	if len(s.script) == 0 {
		return time.Millisecond, nil
	}
	err := s.script[0]
	s.script = s.script[1:]
	return time.Millisecond, err
}

func (s *scripted) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
	return nil
}

// Three consecutive failures close the session; a success in between resets the count (§11, D33).
func TestKeepAliveCountsConsecutiveFailures(t *testing.T) {
	fail := yamux.ErrTimeout
	p := &scripted{script: []error{nil, fail, fail, nil, fail, fail, nil, fail, fail, fail, nil}}
	err := tunnel.KeepAlive(context.Background(), p, time.Millisecond, 3)
	if !errors.Is(err, tunnel.ErrPeerSilent) || !errors.Is(err, yamux.ErrTimeout) {
		t.Fatalf("KeepAlive = %v, want ErrPeerSilent wrapping the last ping's error", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pings != 10 || p.closed != 1 {
		t.Fatalf("%d pings and %d closes, want the session closed once, at the 10th ping (the third failure in a row)", p.pings, p.closed)
	}
}

// The loop ends without closing anything when its context ends, and when the session closed under it.
func TestKeepAliveEnds(t *testing.T) {
	t.Run("context", func(t *testing.T) {
		p := &scripted{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- tunnel.KeepAlive(ctx, p, time.Millisecond, 3) }()
		time.Sleep(20 * time.Millisecond)
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.pings == 0 || p.closed != 0 {
			t.Fatalf("%d pings, %d closes", p.pings, p.closed)
		}
	})
	t.Run("session closed", func(t *testing.T) {
		p := &scripted{script: []error{yamux.ErrSessionShutdown}}
		if err := tunnel.KeepAlive(context.Background(), p, time.Millisecond, 3); err != nil {
			t.Fatal(err)
		}
		if p.closed != 0 {
			t.Fatal("KeepAlive closed a session that was already shut down")
		}
	})
	t.Run("CloseChan", func(t *testing.T) {
		pair := tunneltest.NewPair(t)
		node, _ := pair.Sessions(t, nil)
		done := make(chan error, 1)
		go func() { done <- tunnel.KeepAlive(context.Background(), node, time.Hour, 3) }()
		_ = node.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("KeepAlive did not end with its session")
		}
	})
}

// counting wraps a session to count the pings and the failures the loop sees, and when the last failed.
type counting struct {
	*yamux.Session
	pings, failures atomic.Int32
	lastFailure     atomic.Int64 // unix nanoseconds
}

func (c *counting) Ping() (time.Duration, error) {
	c.pings.Add(1)
	d, err := c.Session.Ping()
	if err != nil {
		c.failures.Add(1)
		c.lastFailure.Store(time.Now().UnixNano())
	}
	return d, err
}

// A frozen peer (a dead path: nothing arrives, nothing leaves, nothing closes; probe tunnel [keepalive]) is
// closed after three failed pings by each side's own loop, independently: with yamux's keepalive off, the
// side that runs no loop never notices. PingEvery is shortened to 200 ms and, since a ping fails only at
// its pong deadline, ConnectionWriteTimeout to 300 ms.
func TestKeepAliveFrozenPeer(t *testing.T) {
	const every, pong = 200 * time.Millisecond, 300 * time.Millisecond
	for _, side := range []string{"gate", "node"} {
		t.Run(side+" detects", func(t *testing.T) {
			t.Parallel()
			p := tunneltest.NewPair(t)
			cfg := tunneltest.ShortConfig(pong)
			if cfg.EnableKeepAlive {
				t.Fatal("yamux keepalive is on")
			}
			node, gate := p.Sessions(t, cfg)
			if _, err := gate.Ping(); err != nil { // a live path first
				t.Fatal(err)
			}
			watched, other := gate, node
			if side == "node" {
				watched, other = node, gate
			}
			c := &counting{Session: watched}
			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- tunnel.KeepAlive(context.Background(), c, every, tunnel.PingFailures) }()

			time.Sleep(3 * every) // healthy pings go by
			if c.failures.Load() != 0 || c.pings.Load() == 0 {
				t.Fatalf("before the freeze: %d pings, %d failures", c.pings.Load(), c.failures.Load())
			}
			p.NodeWire.Freeze()
			frozen := time.Now()

			// Closing waits up to 5 s for the WebSocket close the dead peer never answers.
			var err error
			select {
			case err = <-done:
			case <-time.After(20 * time.Second):
				t.Fatalf("the %s did not close its session within 20 s of the freeze", side)
			}
			took := time.Unix(0, c.lastFailure.Load()).Sub(frozen)
			if !errors.Is(err, tunnel.ErrPeerSilent) {
				t.Fatalf("KeepAlive = %v", err)
			}
			if !watched.IsClosed() {
				t.Fatalf("the %s's session is open after its loop gave up", side)
			}
			if f := c.failures.Load(); f != tunnel.PingFailures {
				t.Fatalf("%d failed pings, want %d", f, tunnel.PingFailures)
			}
			// The third failure comes three pong deadlines at least, and about three intervals more,
			// after the freeze.
			if min, max := 3*pong, 3*(every+pong)+2*time.Second; took < min || took > max {
				t.Fatalf("third failure %v after the freeze, want between %v and %v", took, min, max)
			}
			// The other side ran no loop: yamux's keepalive is off, so it has not noticed anything.
			if other.IsClosed() {
				t.Fatalf("the side without a ping loop closed too (%v after the start): yamux keepalive must be off", time.Since(start))
			}
		})
	}
}
