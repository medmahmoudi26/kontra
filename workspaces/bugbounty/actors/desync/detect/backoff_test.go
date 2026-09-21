package detect

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra-actors/go/desync/probe"
)

// hostPort splits an httptest URL into a probe.Target.
func targetFor(t *testing.T, srv *httptest.Server) probe.Target {
	t.Helper()
	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("addr: %v", err)
	}
	var p int
	fmt.Sscanf(port, "%d", &p)
	return probe.Target{Host: host, Port: p, Scheme: "http"}
}

func benignFor(host string) []byte {
	return []byte("GET / HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n\r\n")
}

// A 429 WIDENS THE GAP, AND SAYS SO. This is the whole reason `rate_ms: 100` is safe to ask for:
// the configured rate is what we are willing to send, and the host gets to disagree.
func TestA429WidensTheGapAndIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	tgt := targetFor(t, srv)

	o := DefaultOptions()
	o.Rate = 20 * time.Millisecond
	o.MaxBackoff = 200 * time.Millisecond
	s := New(o)

	if got := s.BackoffReason(tgt.Host); got != "" {
		t.Fatalf("a host nobody has probed has no reason yet, got %q", got)
	}

	steps := []probe.Step{{Label: "probe", Raw: benignFor(tgt.Host)}}
	s.Connection(context.Background(), tgt, steps, probe.ModeSequential)

	reason := s.BackoffReason(tgt.Host)
	if reason == "" {
		t.Fatal("a 429 earned no backoff — BackoffReason is still the empty column it was")
	}

	// The SECOND connection must actually wait longer than the configured gap. Measuring the
	// wall clock is the point: a reason string with no sleep behind it is a comment.
	start := time.Now()
	s.Connection(context.Background(), tgt, steps, probe.ModeSequential)
	waited := time.Since(start)
	if waited < o.Rate {
		t.Errorf("second probe waited %v, less than the base rate %v — the penalty did nothing",
			waited, o.Rate)
	}
}

// A 400 IS NOT BACKPRESSURE. The framing oracle's attack step is a deliberately malformed
// request, and a front-end rejecting it is correct behaviour. Treating that as "slow down" would
// make every well-behaved host throttle the scan against itself.
func TestAMalformedRequestRejectionIsNotBackpressure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	tgt := targetFor(t, srv)

	s := New(DefaultOptions())
	s.Connection(context.Background(), tgt, []probe.Step{{Label: "probe", Raw: benignFor(tgt.Host)}},
		probe.ModeSequential)

	if got := s.BackoffReason(tgt.Host); got != "" {
		t.Errorf("a 400 backed the scanner off with %q — the attack step is MEANT to be refused", got)
	}
}

// The penalty decays, so one bad minute does not cost the rest of the run. Without this a host
// that 503s during somebody's deploy stays slow forever and reports as covered.
func TestThePenaltyDecaysOnCleanTraffic(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tgt := targetFor(t, srv)

	o := DefaultOptions()
	o.Rate = time.Millisecond
	o.MaxBackoff = 8 * time.Millisecond
	s := New(o)
	steps := []probe.Step{{Label: "probe", Raw: benignFor(tgt.Host)}}

	for i := 0; i < 5; i++ {
		s.Connection(context.Background(), tgt, steps, probe.ModeSequential)
	}
	if s.BackoffReason(tgt.Host) == "" {
		t.Fatal("five 503s earned no penalty")
	}

	fail.Store(false)
	for i := 0; i < 12; i++ {
		s.Connection(context.Background(), tgt, steps, probe.ModeSequential)
	}
	if got := s.BackoffReason(tgt.Host); got != "" {
		t.Errorf("penalty survived a clean run: %q", got)
	}
}
