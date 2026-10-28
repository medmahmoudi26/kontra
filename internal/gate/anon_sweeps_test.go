package gate

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/store"
)

// stuckBucket is a bucket whose DeleteTask waits until its context ends; asked is closed at the first call.
type stuckBucket struct{ asked chan struct{} }

func (b *stuckBucket) DeleteTask(ctx context.Context, _, _ uuid.UUID) error {
	select {
	case <-b.asked:
	default:
		close(b.asked)
	}
	<-ctx.Done()
	return ctx.Err()
}

// openAnonymous opens an anonymous tenant with one a2a service "a" for a new key, as the admission does.
func openAnonymous(t *testing.T, pool *pgxpool.Pool, lim anon.Limits, i int) *anon.Tenant {
	t.Helper()
	pub := make([]byte, 32)
	copy(pub, fmt.Sprintf("pub-%028d", i))
	key := anon.Key{ID: "k1_" + strings.Repeat(string(rune('a'+i)), 26), PubKey: pub, NodeName: "n"}
	var tn *anon.Tenant
	err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		tn, err = anon.Open(context.Background(), tx, lim, anon.OpenRequest{Key: key,
			Network: anon.Network(netip.MustParseAddr(fmt.Sprintf("203.0.%d.1", 100+i))), Services: []anon.Service{{Kind: "a2a", Name: "a"}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

// spec/tunnel.md §6.1 (M4 WP-03 review): the anonymous sweeps run on their own goroutines, the expiry apart from the
// purge: a purge held by a bucket that hangs neither holds the expiry (an expired tenant's services are still disabled
// within the expiry's interval) nor the replica's other sweeps.
func TestAnonymousSweepsRunApart(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.Migrated(t))
	ctx := context.Background()
	lim := anon.Defaults
	purged := openAnonymous(t, pool, lim, 1)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`insert into kontra.tasks (id, tenant_id, service, protocol, buyer_id, principal, via, context_id, state, version,
		snapshot, status_at, terminal_at) values ($1, $2, 'a', 'a2a', $3, 'anonymous', 'node', 'ctx', 'completed', 1, '{}', now(), now())`,
		uuid.Must(uuid.NewV7()), purged.ID, uuid.New())
	exec(`update kontra.tenants set expires_at = now() - interval '25 hours' where id = $1`, purged.ID)
	live := openAnonymous(t, pool, lim, 2)

	bucket := &stuckBucket{asked: make(chan struct{})}
	log := testLog(t, "sweeps")
	r := &Replica{log: log, pool: pool, storeLog: store.NewLog(pool),
		opts: options{orphanEvery: time.Hour, orphanAge: OrphanAge, retentionEvery: time.Hour,
			anonExpireEvery: 50 * time.Millisecond, anonPurgeEvery: 50 * time.Millisecond},
		anonSweeper: anon.NewSweeper(pool, lim, bucket, log), anonKick: make(chan struct{}, 1)}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.RunSweeps(sctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("RunSweeps did not return")
		}
	}()
	select {
	case <-bucket.asked:
	case <-time.After(20 * time.Second):
		t.Fatal("the purge never reached the bucket")
	}
	// The purge now waits on the bucket. Another tenant reaches its end: the expiry still runs.
	exec(`update kontra.tenants set expires_at = now() - interval '1 second' where id = $1`, live.ID)
	eventually(t, 10*time.Second, "the expired tenant's services disabled while the purge waits on the bucket", func() bool {
		return scalar[int](t, pool, `select count(*) from kontra.services where tenant_id = $1 and disabled_at is null`, live.ID) == 0
	})
}
