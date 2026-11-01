package tenant

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/pgtest"
)

// EnsureTeam creates the team tenant once, returns the same id to every caller (concurrent ones too),
// and refuses a "default" tenant that is not a live team tenant.
func TestEnsureTeam(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	ids := make([]uuid.UUID, 8)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := EnsureTeam(ctx, pool)
			if err != nil {
				t.Error(err)
			}
			ids[i] = id
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] || id == uuid.Nil {
			t.Fatalf("ids %v", ids)
		}
	}
	if ids[0].Version() != 7 {
		t.Errorf("tenant id %s is not a UUIDv7", ids[0])
	}
	var n int
	var kind string
	if err := pool.QueryRow(ctx, `select count(*), min(kind) from kontra.tenants`).Scan(&n, &kind); err != nil || n != 1 || kind != KindTeam {
		t.Errorf("tenants: %d %q %v", n, kind, err)
	}
	if id, err := Lookup(ctx, pool, TeamSlug); err != nil || id != ids[0] {
		t.Errorf("Lookup: %s %v", id, err)
	}
	if _, err := Lookup(ctx, pool, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Lookup unknown: %v", err)
	}

	if _, err := pool.Exec(ctx, `update kontra.tenants set deleted_at = now()`); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureTeam(ctx, pool); err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Errorf("a deleted team tenant: %v", err)
	}
	if _, err := pool.Exec(ctx, `update kontra.tenants set deleted_at = null, kind = 'account'`); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureTeam(ctx, pool); err == nil || !strings.Contains(err.Error(), "kind account") {
		t.Errorf("a default tenant of another kind: %v", err)
	}
}

// D315: the tenant an OIDC token's tenant claim names is a live team or account tenant: never an anonymous one, a
// deleted one or an expired one. An answer is kept ResolveTTL (an unknown slug's too), never past the tenant's
// expires_at.
func TestResolver(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	team, err := EnsureTeam(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	add := func(slug, kind, extra string) uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		cols, vals := "id, slug, kind", "$1, $2, $3"
		if extra != "" {
			cols, vals = cols+", expires_at, created_from", vals+", "+extra+", '203.0.113.0/24'"
		}
		if _, err := pool.Exec(ctx, `insert into kontra.tenants (`+cols+`) values (`+vals+`)`, id, slug, kind); err != nil {
			t.Fatal(err)
		}
		return id
	}
	acme := add("acme", KindAccount, "")
	add("anon-00000000000000a1", KindAnonymous, "now() + interval '1 hour'")
	gone := add("gone", KindAccount, "")
	if _, err := pool.Exec(ctx, `update kontra.tenants set deleted_at = now() where id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	add("lapsed", KindAccount, "now() - interval '1 second'")
	soon := add("soon", KindAccount, "now() + interval '300 milliseconds'")

	r := NewResolver(pool)
	for slug, want := range map[string]uuid.UUID{"default": team, "acme": acme, "soon": soon,
		"anon-00000000000000a1": uuid.Nil, "gone": uuid.Nil, "lapsed": uuid.Nil, "nobody": uuid.Nil} {
		id, ok, err := r.Tenant(ctx, slug)
		if err != nil || ok != (want != uuid.Nil) || id != want {
			t.Errorf("Tenant(%q) = %s %v %v, want %s", slug, id, ok, err, want)
		}
	}

	// Cached: a deletion is seen once the answer is ResolveTTL old; an expiry at once.
	clock := time.Now()
	r.now = func() time.Time { return clock }
	r.cache = map[string]resolved{}
	if _, ok, _ := r.Tenant(ctx, "acme"); !ok {
		t.Fatal("acme")
	}
	if _, err := pool.Exec(ctx, `update kontra.tenants set deleted_at = now() where id = $1`, acme); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := r.Tenant(ctx, "acme"); !ok {
		t.Error("the cached answer was not used")
	}
	clock = clock.Add(ResolveTTL)
	if _, ok, _ := r.Tenant(ctx, "acme"); ok {
		t.Error("a deleted tenant still maps after ResolveTTL")
	}
	if _, ok, _ := r.Tenant(ctx, "soon"); !ok {
		t.Fatal("soon")
	}
	time.Sleep(400 * time.Millisecond)
	clock = time.Now()
	if _, ok, _ := r.Tenant(ctx, "soon"); ok {
		t.Error("an expired tenant's cached answer outlived its expires_at")
	}
}
