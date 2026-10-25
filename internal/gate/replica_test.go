package gate

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/lifecycle"
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

// The refused-patch metric of spec §13.3 in the Prometheus text format: a counter per source and refusal.
func TestPatchRefusalsMetric(t *testing.T) {
	var b strings.Builder
	writePatchRefusals(&b, "r1", nil)
	if got := b.String(); !strings.Contains(got, "# TYPE kontra_gate_operator_patches_refused_total counter") || strings.Contains(got, "{") {
		t.Errorf("no refusal yet: %q", got)
	}
	var p lifecycle.PatchRefusals
	p.Add(lifecycle.HookOnTerminal, "too_large")
	p.Add(lifecycle.UpdateAnnotate, "forbidden_key")
	p.Add(lifecycle.HookOnTerminal, "too_large")
	b.Reset()
	writePatchRefusals(&b, "r1", &p)
	want := "kontra_gate_operator_patches_refused_total{replica=\"r1\",source=\"kontra.annotate\",refusal=\"forbidden_key\"} 1\n" +
		"kontra_gate_operator_patches_refused_total{replica=\"r1\",source=\"kontra.lifecycle.OnTerminal\",refusal=\"too_large\"} 2\n"
	if got := b.String(); !strings.HasSuffix(got, want) {
		t.Errorf("metric:\n%s\nwant it to end with\n%s", got, want)
	}
}
