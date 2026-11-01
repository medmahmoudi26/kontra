package tenant

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Resolver answers an OIDC token's tenant claim (spec/relay.md §2.3, D315): the live tenant with a slug, of kind team
// or account, that an issuer's principal may belong to. Anonymous tenants are never one: they belong to an anonymous
// session's node key, not to a signed-in principal (spec/tunnel.md §6). Answers are kept ResolveTTL, so a tenant
// deleted or expired stops mapping within that time, as an API key's tenant stops authenticating within the revocation
// cache's re-read (§2.3); an unknown slug is kept as long, so a token naming one costs one query per ResolveTTL.
type Resolver struct {
	db  Querier
	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	cache map[string]resolved
}

type resolved struct {
	id  uuid.UUID
	ok  bool
	at  time.Time
	exp *time.Time // the tenant's expires_at, so an answer never outlives it
}

// ResolveTTL is how long an answer is kept.
const ResolveTTL = 5 * time.Second

// maxResolved bounds the answers kept; past it the cache starts again empty.
const maxResolved = 4096

// NewResolver returns a resolver over db.
func NewResolver(db Querier) *Resolver {
	return &Resolver{db: db, ttl: ResolveTTL, now: time.Now, cache: map[string]resolved{}}
}

// Tenant returns the id of the live team or account tenant with this slug; ok is false when there is none.
func (r *Resolver) Tenant(ctx context.Context, slug string) (uuid.UUID, bool, error) {
	now := r.now()
	r.mu.Lock()
	if c, hit := r.cache[slug]; hit && now.Sub(c.at) < r.ttl && (c.exp == nil || now.Before(*c.exp)) {
		r.mu.Unlock()
		return c.id, c.ok, nil
	}
	r.mu.Unlock()

	var (
		id  uuid.UUID
		exp *time.Time
	)
	err := r.db.QueryRow(ctx, `select id, expires_at from kontra.tenants where slug = $1 and kind in ('team', 'account')
		and deleted_at is null and (expires_at is null or expires_at > now())`, slug).Scan(&id, &exp)
	ok := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, err
	}
	r.mu.Lock()
	if len(r.cache) >= maxResolved {
		clear(r.cache)
	}
	r.cache[slug] = resolved{id: id, ok: ok, at: now, exp: exp}
	r.mu.Unlock()
	return id, ok, nil
}
