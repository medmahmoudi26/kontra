package tunnelgate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnel/tunneltest"
)

// A card fetch abandoned while its stream's open waits (yamux's open blocks behind AcceptBacklog
// unacknowledged opens) returns at its own deadline, and opens no stray stream with a preamble once the
// node acknowledges the opens: the open is bound to the fetch's request, as a dispatch's is (upstream.Tunnel;
// M2 review, robustness-5). net/http hands a dial the request's values but not its cancellation, and the
// card fetch's own transport opened the stream whenever yamux let it, then wrote the preamble on it.
func TestCardFetchOpenEndsWithItsRequest(t *testing.T) {
	r := NewRegistry(RegistryConfig{})
	nodeMux, gateMux := tunneltest.NewPair(t).Sessions(t, nil)
	s := &Session{ID: uuid.Must(uuid.NewV7()), KeyID: "k1_" + strings.Repeat("d", 26), TenantID: testTenantID,
		mux: gateMux, served: map[string]string{"hello": tunnel.KindA2A}}
	hold(r, s)
	// The node accepts nothing yet: AcceptBacklog opens fill yamux's backlog, and the next open waits.
	for range tunnel.AcceptBacklog {
		if _, err := gateMux.OpenStream(); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.FetchCard(ctx, testTenantID, "hello")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the card fetch behind a full backlog: %v, want its deadline", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the card fetch returned %v after it started, want at its 300 ms deadline", took)
	}

	// The node accepts every open now: what reaches it on each stream.
	var (
		mu   sync.Mutex
		seen []string
	)
	accepted := make(chan struct{}, 2*tunnel.AcceptBacklog)
	go func() {
		for {
			st, err := nodeMux.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				_ = st.SetReadDeadline(time.Now().Add(time.Second))
				b := make([]byte, 4096)
				n, _ := st.Read(b)
				mu.Lock()
				seen = append(seen, string(b[:n]))
				mu.Unlock()
				accepted <- struct{}{}
			}()
		}
	}()
	deadline := time.After(5 * time.Second)
	for n := 0; n < tunnel.AcceptBacklog; n++ {
		select {
		case <-accepted:
		case <-deadline:
			t.Fatalf("the node read %d of the %d streams", n, tunnel.AcceptBacklog)
		}
	}
	select {
	case <-accepted:
	case <-time.After(1500 * time.Millisecond):
	}
	mu.Lock()
	defer mu.Unlock()
	for _, got := range seen {
		if strings.Contains(got, `"op":"http"`) || strings.Contains(got, "agent-card.json") {
			t.Fatalf("the abandoned card fetch opened a stream with its preamble once the node acknowledged it: %q", got)
		}
	}
}
