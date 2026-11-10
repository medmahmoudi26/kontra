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
