package gate

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"kontra.sh/kontra/internal/pgtest"
)

// The drain deletes the replica's gate_replicas row last, while the heartbeat loop still runs (it beats
// through the drain, which outlasts ReplicaSilence). No heartbeat may bring the row back afterwards: a dead
// replica's row would stay until another replica reaps it, or until the next start on a gate of one, and
// count in D72's N meanwhile.
func TestNoHeartbeatAfterDeregister(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.Migrated(t))
	ctx := context.Background()
	r := &Replica{pool: pool, id: "gate-test-0001", log: slog.Default(),
		opts: options{beatEvery: 25 * time.Millisecond, silence: ReplicaSilence}}
	if err := r.register(ctx); err != nil {
		t.Fatal(err)
	}
	rows := func() int {
		t.Helper()
		return scalar[int](t, pool, `select count(*) from kontra.gate_replicas where replica_id = $1`, r.id)
	}
	loop, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = r.replicaLoop(loop) }()
	defer func() { stop(); <-done }()

	time.Sleep(60 * time.Millisecond) // a few beats
	r.draining.Store(true)
	if err := r.markDraining(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.deregister(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // the loop keeps ticking
	if n := rows(); n != 0 {
		t.Fatalf("a heartbeat after deregister brought the replica's row back (%d rows)", n)
	}
	// Nor the drain's own beat (markDraining), nor a beat in flight.
	if err := r.beat(ctx); err != nil {
		t.Fatal(err)
	}
	if n := rows(); n != 0 {
		t.Fatalf("a beat after deregister: %d rows", n)
	}
}
