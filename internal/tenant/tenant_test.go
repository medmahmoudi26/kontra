package tenant

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/medmahmoudi26/kontra/internal/pgtest"
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
