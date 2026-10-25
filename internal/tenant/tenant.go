// Package tenant holds the gate's tenant rows (spec/relay.md §1; migrations/0001_init.sql tenants).
//
// A team gate has one tenant, "default" (kind team). Both paths that need it before anything else exists
// upsert it with EnsureTeam: `kontra-gate enroll --api-key`, which mints a key into it (the README order is
// migrate, enroll, serve), and the replica's service sync at start and on SIGHUP. Cloud's tenants are its
// own (one per account and per anonymous session) and are never created here.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TeamSlug is the slug of a team gate's one tenant.
const TeamSlug = "default"

// Kinds of tenant (tenants.kind).
const (
	KindTeam      = "team"
	KindAccount   = "account"
	KindAnonymous = "anonymous"
)

// Querier runs statements: a pool, a connection or a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// EnsureTeam returns the id of the team tenant, inserting it (a fresh UUIDv7, kind team) when it does not
// exist. Concurrent callers get the same id. A "default" tenant of another kind, or one marked deleted, is
// an error: it is not this gate's team tenant.
func EnsureTeam(ctx context.Context, q Querier) (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant: id: %w", err)
	}
	if _, err := q.Exec(ctx, `insert into kontra.tenants (id, slug, kind) values ($1, $2, $3) on conflict (slug) do nothing`,
		id, TeamSlug, KindTeam); err != nil {
		return uuid.Nil, fmt.Errorf("tenant: upsert %q: %w", TeamSlug, err)
	}
	var (
		got     uuid.UUID
		kind    string
		deleted *time.Time
	)
	err = q.QueryRow(ctx, `select id, kind, deleted_at from kontra.tenants where slug = $1`, TeamSlug).Scan(&got, &kind, &deleted)
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant: read %q: %w", TeamSlug, err)
	}
	switch {
	case kind != KindTeam:
		return uuid.Nil, fmt.Errorf("tenant: %q exists with kind %s, not %s: this database is not a team gate's", TeamSlug, kind, KindTeam)
	case deleted != nil:
		return uuid.Nil, fmt.Errorf("tenant: %q is marked deleted", TeamSlug)
	}
	return got, nil
}

// ErrNotFound is returned by Lookup for a slug with no live tenant.
var ErrNotFound = errors.New("tenant: not found")

// Lookup returns the id of the live tenant with this slug.
func Lookup(ctx context.Context, q Querier, slug string) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `select id from kontra.tenants where slug = $1 and deleted_at is null
		and (expires_at is null or expires_at > now())`, slug).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("%w: %q", ErrNotFound, slug)
	}
	return id, err
}
