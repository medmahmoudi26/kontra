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
	"kontra.sh/kontra/internal/config"
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

// okBucket is a bucket whose DeleteTask succeeds at once.
type okBucket struct{}

func (okBucket) DeleteTask(context.Context, uuid.UUID, uuid.UUID) error { return nil }

// spec/tunnel.md §6.1 (D304): a multi-tenant gate runs the anonymous sweeps whether or not it opens anonymous sessions
// now, so the tenants opened before tunnel.anonymous was turned off still end at their expires_at (services disabled,
// bindings revoked) and are purged purge_after later (only the tombstone left), with the block's defaults. A
// single-tenant gate, which has no anonymous tenant, runs none.
func TestAnonymousSweepsRunWithoutAnonymousSessions(t *testing.T) {
	if _, ok := anonSweepLimits(&config.Config{Tenancy: config.TenancySingle}); ok {
		t.Fatal("a single-tenant gate runs the anonymous sweeps")
	}
	cfg := &config.Config{Tenancy: config.TenancyMulti} // tunnel.anonymous off
	lim, ok := anonSweepLimits(cfg)
	if !ok || lim.PurgeAfter != anon.Defaults.PurgeAfter || lim.TTL != anon.Defaults.TTL {
		t.Fatalf("a multi-tenant gate without tunnel.anonymous: sweeps %v, limits %+v", ok, lim)
	}

	pool := pgtest.Pool(t, pgtest.Migrated(t))
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	old := openAnonymous(t, pool, anon.Defaults, 1)
	exec(`update kontra.tenants set expires_at = now() - interval '25 hours' where id = $1`, old.ID)
	live := openAnonymous(t, pool, anon.Defaults, 2)

	log := testLog(t, "sweeps")
	r := &Replica{log: log, pool: pool, storeLog: store.NewLog(pool),
		opts: options{orphanEvery: time.Hour, orphanAge: OrphanAge, retentionEvery: time.Hour,
			anonExpireEvery: 50 * time.Millisecond, anonPurgeEvery: 50 * time.Millisecond},
		anonSweeper: anon.NewSweeper(pool, lim, okBucket{}, log), anonKick: make(chan struct{}, 1)}
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
	eventually(t, 10*time.Second, "the tenant past purge_after purged", func() bool {
		return scalar[bool](t, pool, `select deleted_at is not null from kontra.tenants where id = $1`, old.ID) &&
			scalar[int](t, pool, `select count(*) from kontra.services where tenant_id = $1`, old.ID) == 0 &&
			scalar[int](t, pool, `select count(*) from kontra.tunnel_keys where tenant_id = $1`, old.ID) == 0
	})
	if scalar[int](t, pool, `select count(*) from kontra.services where tenant_id = $1 and disabled_at is null`, live.ID) != 1 {
		t.Fatal("a live tenant's service was disabled before its end")
	}
	exec(`update kontra.tenants set expires_at = now() - interval '1 second' where id = $1`, live.ID)
	eventually(t, 10*time.Second, "the tenant at its end has its services disabled and bindings revoked", func() bool {
		return scalar[int](t, pool, `select count(*) from kontra.services where tenant_id = $1 and disabled_at is null`, live.ID) == 0 &&
			scalar[int](t, pool, `select count(*) from kontra.key_services where tenant_id = $1 and revoked_at is null`, live.ID) == 0
	})
}
