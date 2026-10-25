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
