package traffic

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// stuckDB is a database that never answers until released, or that refuses every batch.
type stuckDB struct {
	release chan struct{}
	fail    bool
	mu      sync.Mutex
	rows    int
}

func (d *stuckDB) CopyFrom(ctx context.Context, _ pgx.Identifier, _ []string, src pgx.CopyFromSource) (int64, error) {
	if d.fail {
		return 0, errors.New("refused")
	}
	select {
	case <-d.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	n := 0
	for src.Next() {
		n++
	}
	d.mu.Lock()
	d.rows += n
	d.mu.Unlock()
	return int64(n), nil
}

// A database that blocks never blocks Record: with the writer stuck on its first batch, the queue fills and every
// exchange past it is dropped and counted, at once.
func TestRecordNeverBlocks(t *testing.T) {
	db := &stuckDB{release: make(chan struct{})}
	r := New(db, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()

	const n = QueueSize + BatchRows + 1000
	start := time.Now()
	for range n {
		r.Record(Exchange{At: time.Now(), Service: "hello", Method: "GetTask", HTTPStatus: 200})
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("%d Records took %s", n, d)
	}
	// The writer holds at most one batch; the queue at most QueueSize; the rest was dropped.
	if got := r.Dropped(); got < n-QueueSize-BatchRows || got > n-QueueSize {
		t.Fatalf("dropped %d of %d", got, n)
	}
	close(db.release)
	cancel()
	<-done
	db.mu.Lock()
	defer db.mu.Unlock()
	if int64(db.rows)+r.Dropped() != n {
		t.Fatalf("stored %d + dropped %d != recorded %d", db.rows, r.Dropped(), n)
	}
}

// A batch the database refuses is dropped and counted.
func TestRefusedBatchIsCounted(t *testing.T) {
	r := New(&stuckDB{fail: true}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	for range 3 {
		r.Record(Exchange{At: time.Now(), Service: "hello", Method: "card", HTTPStatus: 200})
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.Dropped() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if r.Dropped() != 3 {
		t.Fatalf("dropped %d, want 3", r.Dropped())
	}
}

// A nil recorder (traffic.record off) records nothing and counts nothing.
func TestNilRecorder(t *testing.T) {
	var r *Recorder
	r.Record(Exchange{})
	if r.Dropped() != 0 {
		t.Fatal("a nil recorder counted")
	}
}

// row keeps every value inside a2a_exchanges' CHECKs: a principal only with a known via, a status in range, clipped
// strings, a method.
func TestRowFitsTheTable(t *testing.T) {
	long := string(make([]byte, 300))
	row := Exchange{Service: long, Method: "", Binding: "grpc", Principal: "anonymous", Via: "bogus", ContextID: long, HTTPStatus: 0}.row()
	if s := row[3].(string); len(s) != 256 {
		t.Errorf("service %d bytes", len(s))
	}
	if row[4] != MethodUnknown || row[5] != BindingJSONRPC || row[6] != nil || row[7] != nil || row[13] != int16(200) {
		t.Errorf("row %v", row[4:14])
	}
	if clip("aé", 2) != "a" {
		t.Error("clip split a rune")
	}
}
