package tunnelgate

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnel/tunneltest"
	"kontra.sh/kontra/internal/upstream"
)

// lingerNode is the node's side of an in-process session that answers every exchange with a complete
// response and then, unless it closes its halves, sends 64 KiB more and keeps its half open.
type lingerNode struct {
	closeHalves bool
	answering   sync.WaitGroup
	mu          sync.Mutex
	open        []*yamux.Stream
}

func (n *lingerNode) serve(nodeMux *yamux.Session) {
	for {
		st, err := nodeMux.AcceptStream()
		if err != nil {
			return
		}
		n.answering.Add(1)
		go func() {
			defer n.answering.Done()
			br := bufio.NewReader(st)
			if _, err := tunnel.ReadPreamble(br); err != nil {
				_ = st.Close()
				return
			}
			req, err := http.ReadRequest(br)
			if err != nil {
				_ = st.Close()
				return
			}
			_, _ = io.Copy(io.Discard, req.Body)
			_, _ = io.WriteString(st, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
			if n.closeHalves {
				_ = st.Close()
				return
			}
			_, _ = st.Write(make([]byte, 64<<10))
			n.mu.Lock()
			n.open = append(n.open, st)
			n.mu.Unlock()
		}()
	}
}

// closeAll closes the halves the node kept open, once it has answered every stream.
func (n *lingerNode) closeAll() {
	n.answering.Wait()
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, st := range n.open {
		_ = st.Close()
	}
	n.open = nil
}

func lingerSession(t *testing.T, n *lingerNode) (*Session, upstream.Backend) {
	t.Helper()
	r := NewRegistry(RegistryConfig{})
	nodeMux, gateMux := tunneltest.NewPair(t).Sessions(t, nil)
	s := &Session{ID: uuid.Must(uuid.NewV7()), KeyID: "k1_" + strings.Repeat("c", 26), TenantID: testTenantID,
		mux: gateMux, served: map[string]string{"hello": tunnel.KindA2A}}
	go n.serve(nodeMux)
	hold(r, s)
	be, err := r.Backend(s.ID, store.ServiceRef{TenantID: testTenantID, Tenant: "default", Name: "hello", Kind: "a2a"})
	if err != nil {
		t.Fatal(err)
	}
	return s, be
}

func lingerGet(ctx context.Context, be upstream.Backend) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.Endpoint(be, upstream.Info{Path: "/x"}), nil)
	resp, err := be.HTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	return err
}

// A gate-opened stream the gate has closed stays counted until the node's half ends, and a node that keeps
// more than tunnel.GateMaxHalfClosed of them open loses its session: yamux v0.1.2 buffers up to a window
// of what the node sends on such a stream for StreamCloseTimeout (5 min), so a node that answered each
// exchange but never closed its half pinned 256 KiB of the gate's memory per stream, at the rate the gate
// opens streams to it (M2 review, security-3; D157). A node that closes its halves, even late, never comes
// near the cap.
func TestHalfClosedStreamsAreCapped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	t.Run("a node that never closes its halves", func(t *testing.T) {
		s, be := lingerSession(t, &lingerNode{})
		for i := 0; i < tunnel.GateMaxHalfClosed; i++ {
			if err := lingerGet(ctx, be); err != nil {
				t.Fatalf("exchange %d: %v", i, err)
			}
		}
		eventually(t, 5*time.Second, "every stream counted half-closed", func() bool { return s.HalfClosed() == tunnel.GateMaxHalfClosed })
		if s.Mux().IsClosed() {
			t.Fatal("the session closed at the cap")
		}
		_ = lingerGet(ctx, be) // one more
		eventually(t, 5*time.Second, "the session closed past the cap", s.Mux().IsClosed)
		// yamux ends the streams once the WebSocket's close is done, up to 5 s after the session's close.
		eventually(t, 30*time.Second, "the half-closed streams ended with the session", func() bool { return s.HalfClosed() == 0 })
	})

	t.Run("a node that closes its halves late", func(t *testing.T) {
		n := &lingerNode{}
		s, be := lingerSession(t, n)
		for range 2 {
			for i := 0; i < tunnel.GateMaxHalfClosed/2; i++ {
				if err := lingerGet(ctx, be); err != nil {
					t.Fatalf("exchange %d: %v", i, err)
				}
			}
			n.closeAll()
			eventually(t, 5*time.Second, "no stream half-closed once the node closed its halves", func() bool { return s.HalfClosed() == 0 })
		}
		if s.Mux().IsClosed() {
			t.Fatal("the session closed")
		}
	})

	t.Run("a node that closes each half after its response", func(t *testing.T) {
		s, be := lingerSession(t, &lingerNode{closeHalves: true})
		for i := 0; i < 2*tunnel.GateMaxHalfClosed; i++ {
			if err := lingerGet(ctx, be); err != nil {
				t.Fatalf("exchange %d: %v", i, err)
			}
		}
		eventually(t, 5*time.Second, "no stream half-closed", func() bool { return s.HalfClosed() == 0 })
		if s.Mux().IsClosed() || s.InFlight() != 0 {
			t.Fatalf("closed %v, in flight %d", s.Mux().IsClosed(), s.InFlight())
		}
	})
}
