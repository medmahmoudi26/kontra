package traffic

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/pgtest"
)

// Exchanges land in a2a_exchanges through COPY; the sweep deletes those older than the retention, in batches, and
// nothing newer.
func TestWriteAndSweep(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.Migrated(t))
	ctx := context.Background()
	r := New(pool, nil)
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = r.Run(rctx); close(done) }()
	task, tenant, code := uuid.New(), uuid.New(), -32009
	now := time.Now()
	for i := range 7 {
		r.Record(Exchange{At: now.Add(-time.Duration(i) * 24 * time.Hour), TenantID: &tenant, Service: "hello", Method: "SendMessage",
			Binding: BindingJSONRPC, Principal: "anonymous", Via: "anonymous", TaskID: &task, ContextID: "c1", BytesIn: 10,
			BytesOut: 20, Duration: 3 * time.Millisecond, HTTPStatus: 200, RPCCode: &code, Reason: ReasonVersionUnsupported})
	}
	cancel()
	<-done
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from kontra.a2a_exchanges where task_id = $1 and rpc_code = -32009
		and principal = 'anonymous' and duration_ms = 3 and reason = 'version_unsupported'`, task).Scan(&n); err != nil || n != 7 {
		t.Fatalf("stored %d (%v), want 7", n, err)
	}
	deleted, err := Sweep(ctx, pool, now.Add(-3*24*time.Hour+time.Minute), 2)
	if err != nil || deleted != 4 {
		t.Fatalf("sweep deleted %d (%v), want the 4 older than 3 days", deleted, err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from kontra.a2a_exchanges`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("left %d", n)
	}
}

// With bodies on, an exchange's bodies and its stream's frames land in the same batch; the sweep deletes the frames
// with their exchange.
func TestFramesWrittenAndSwept(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.Migrated(t))
	ctx := context.Background()
	r := New(pool, nil).KeepBodies(1024)
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = r.Run(rctx); close(done) }()
	gate := uuid.New()
	old, recent := NewID(), NewID()
	now := time.Now()
	for _, x := range []struct {
		id uuid.UUID
		at time.Time
	}{{old, now.Add(-48 * time.Hour)}, {recent, now}} {
		r.Record(Exchange{ID: x.id, At: x.at, Leg: LegCallee, GateTaskID: &gate, Service: "hello", Method: "SendStreamingMessage",
			Binding: BindingJSONRPC, A2AVersion: "1.0", HTTPStatus: 200, Request: KeepBody([]byte(`{"a":1}`), 1024),
			Frames:        []Frame{{Offset: 0, Data: []byte(`{"task":{}}`), Size: 11}, {Offset: 5 * time.Millisecond, SSEID: "2", Data: []byte(`{}`), Size: 2}},
			FramesDropped: 1})
	}
	cancel()
	<-done
	var n, frames, dropped int
	var leg string
	var body []byte
	if err := pool.QueryRow(ctx, `select count(*), min(leg), (array_agg(request_body))[1], min(frames_dropped) from kontra.a2a_exchanges
		where gate_task_id = $1`, gate).Scan(&n, &leg, &body, &dropped); err != nil || n != 2 || leg != LegCallee || string(body) != `{"a":1}` || dropped != 1 {
		t.Fatalf("stored %d %s %q %d (%v)", n, leg, body, dropped, err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from kontra.a2a_exchange_frames`).Scan(&frames); err != nil || frames != 4 {
		t.Fatalf("%d frames (%v), want 4", frames, err)
	}
	if _, err := Sweep(ctx, pool, now.Add(-24*time.Hour), 10); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from kontra.a2a_exchange_frames where exchange_id = $1`, old).Scan(&frames); err != nil || frames != 0 {
		t.Errorf("the swept exchange's frames: %d (%v)", frames, err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from kontra.a2a_exchange_frames where exchange_id = $1`, recent).Scan(&frames); err != nil || frames != 2 {
		t.Errorf("the kept exchange's frames: %d (%v)", frames, err)
	}
}
