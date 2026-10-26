package lifecycle

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/worker"
)

// stopRecorder is a worker that records when it is stopped.
type stopRecorder struct {
	worker.Worker
	name string
	mu   *sync.Mutex
	log  *[]string
}

func (r stopRecorder) Stop() {
	r.mu.Lock()
	*r.log = append(*r.log, r.name)
	r.mu.Unlock()
}

// Stop follows the drain order (spec §14.3, D76): kontra-direct stops first, waiting for the dispatches
// in flight while kontra-lifecycle, kontra-push, kontra-direct-ctl and kontra-ctl-<id> keep polling (on
// a gate of one replica, a cancel or follow-up Update is still accepted and a finished dispatch's result
// recorded), then the others, the control queue last.
func TestWorkersStopInTheDrainOrder(t *testing.T) {
	var (
		mu  sync.Mutex
		got []string
	)
	rec := func(name string) worker.Worker { return stopRecorder{name: name, mu: &mu, log: &got} }
	w := &Workers{lifecycle: rec(QueueLifecycle), direct: rec(QueueDirect), directCtl: rec(QueueDirectCtl), ctl: rec(QueueCtl("r1")), push: rec(QueuePush)}
	w.Stop()
	want := []string{QueueDirect, QueueLifecycle, QueuePush, QueueDirectCtl, QueueCtl("r1")}
	if !slices.Equal(got, want) {
		t.Fatalf("stop order %v, want %v", got, want)
	}
}

// Drain (spec §14.3, D76): kontra-direct stops first and Drain waits for it (its stop returns when the last
// dispatch has ended, or at drain_timeout); the other workers keep polling for the settle period, so the
// workflow tasks of the last results run, and stop after it, the control queue last. Drain reports a stop
// of kontra-direct that took drain_timeout.
func TestDrainSettlesThenStopsInTheDrainOrder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		directTook time.Duration
		wantErr    bool
	}{
		{"the last dispatch ended", 3 * time.Second, false},
		{"drain_timeout cut dispatches", 10 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu    sync.Mutex
				got   []string
				clock = time.Unix(0, 0)
			)
			rec := func(name string) worker.Worker { return stopRecorder{name: name, mu: &mu, log: &got} }
			direct := clockedStop{stopRecorder: stopRecorder{name: QueueDirect, mu: &mu, log: &got}, advance: func() { clock = clock.Add(tc.directTook) }}
			w := &Workers{lifecycle: rec(QueueLifecycle), direct: direct, directCtl: rec(QueueDirectCtl), ctl: rec(QueueCtl("r1")), push: rec(QueuePush),
				drainTimeout: 10 * time.Second, settle: 2 * time.Second, now: func() time.Time { return clock }}
			w.sleep = func(d time.Duration) {
				mu.Lock()
				got = append(got, "settle "+d.String())
				mu.Unlock()
			}
			err := w.Drain()
			if errors.Is(err, ErrDrainTimeout) != tc.wantErr {
				t.Fatalf("Drain = %v", err)
			}
			want := []string{QueueDirect, "settle 2s", QueueLifecycle, QueuePush, QueueDirectCtl, QueueCtl("r1")}
			if !slices.Equal(got, want) {
				t.Fatalf("drain order %v, want %v", got, want)
			}
		})
	}
}

// clockedStop is kontra-direct's stop, taking the time the dispatches in flight take.
type clockedStop struct {
	stopRecorder
	advance func()
}

func (c clockedStop) Stop() {
	c.advance()
	c.stopRecorder.Stop()
}

// kontra-tunnel-<replica> stops in the drain order (spec §14.3, D76): after kontra-direct and the
// lifecycle, push and direct-ctl workers, before the control queue.
func TestTunnelWorkerStopsInTheDrainOrder(t *testing.T) {
	var (
		mu  sync.Mutex
		got []string
	)
	rec := func(name string) worker.Worker { return stopRecorder{name: name, mu: &mu, log: &got} }
	w := &Workers{lifecycle: rec(QueueLifecycle), direct: rec(QueueDirect), directCtl: rec(QueueDirectCtl), ctl: rec(QueueCtl("r1")),
		push: rec(QueuePush), tunnel: rec(QueueTunnel("r1"))}
	w.Stop()
	want := []string{QueueDirect, QueueLifecycle, QueuePush, QueueDirectCtl, QueueTunnel("r1"), QueueCtl("r1")}
	if !slices.Equal(got, want) {
		t.Fatalf("stop order %v, want %v", got, want)
	}
}

// A drain keeps kontra-tunnel-<replica> polling until no dispatch runs on it (a follow-up of a task on this
// replica's sessions may still arrive), within drain_timeout counted from the drain's start, then settles
// and stops every worker; a tunnel dispatch still running at drain_timeout is reported as ErrDrainTimeout
// and cut by the worker's stop (a lost session, spec §14.3).
func TestDrainWaitsForTunnelDispatches(t *testing.T) {
	for _, tc := range []struct {
		name    string
		endsAt  time.Duration // when the last tunnel dispatch ends; 0: never within the drain
		wantErr bool
	}{
		{"the last tunnel dispatch ends", 3 * time.Second, false},
		{"drain_timeout cuts it", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu    sync.Mutex
				got   []string
				clock = time.Unix(0, 0)
			)
			rec := func(name string) worker.Worker { return stopRecorder{name: name, mu: &mu, log: &got} }
			direct := clockedStop{stopRecorder: stopRecorder{name: QueueDirect, mu: &mu, log: &got}, advance: func() { clock = clock.Add(time.Second) }}
			w := &Workers{lifecycle: rec(QueueLifecycle), direct: direct, directCtl: rec(QueueDirectCtl), ctl: rec(QueueCtl("r1")),
				push: rec(QueuePush), tunnel: rec(QueueTunnel("r1")), drainTimeout: 10 * time.Second, settle: 2 * time.Second,
				now: func() time.Time { return clock }}
			w.tunnelInflight.Store(1)
			polls := 0
			w.sleep = func(d time.Duration) {
				if d == drainPoll {
					polls++
					clock = clock.Add(d)
					if tc.endsAt > 0 && clock.Sub(time.Unix(0, 0)) >= tc.endsAt {
						w.tunnelInflight.Store(0)
					}
					return
				}
				mu.Lock()
				got = append(got, "settle "+d.String())
				mu.Unlock()
			}
			err := w.Drain()
			if errors.Is(err, ErrDrainTimeout) != tc.wantErr {
				t.Fatalf("Drain = %v", err)
			}
			want := []string{QueueDirect, "settle 2s", QueueLifecycle, QueuePush, QueueDirectCtl, QueueTunnel("r1"), QueueCtl("r1")}
			if !slices.Equal(got, want) {
				t.Fatalf("drain order %v, want %v", got, want)
			}
			waited := clock.Sub(time.Unix(0, 0))
			switch {
			case tc.endsAt > 0 && (waited < tc.endsAt || waited > tc.endsAt+drainPoll):
				t.Errorf("the drain waited %v for the tunnel dispatch that ended at %v", waited, tc.endsAt)
			case tc.endsAt == 0 && (waited < w.drainTimeout || waited > w.drainTimeout+drainPoll):
				t.Errorf("the drain waited %v, want drain_timeout %v", waited, w.drainTimeout)
			}
			if polls == 0 {
				t.Error("the drain did not wait for the tunnel dispatch")
			}
		})
	}
}
