package lifecycle

import (
	"slices"
	"sync"
	"testing"

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
