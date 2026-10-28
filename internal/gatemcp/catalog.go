package gatemcp

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/unimpl"
)

// PageSize is search's page (DM4-4).
const PageSize = 50

// DrainEvery bounds describe's background drain of a cold MCP tools cache: at most one per service per period,
// never on the request's time (§12.2, §11.3, DM4-4).
const DrainEvery = 10 * time.Minute

// Catalog answers search and describe for a principal from the services table, the tunnels registry and the
// caches the relays keep (the served card, the MCP tools cache). It never makes an upstream request on a request's
// time (§12.2).
//
// Visibility (§12.2): a service whose allow: admits the principal by an entry other than anonymous, or one the
// operator marks listed; never a service of an anonymous tenant (spec/tunnel.md §6.2), never a disabled one.
type Catalog struct {
	db *pgxpool.Pool
}

// NewCatalog reads from db.
func NewCatalog(db *pgxpool.Pool) *Catalog { return &Catalog{db: db} }

// Search answers search for p: matching, filters, ordering by ref and the keyset cursor of DM4-4.
func (c *Catalog) Search(ctx context.Context, p auth.Principal, args SearchArgs) (SearchResult, error) {
	return SearchResult{}, unimpl.Error("gatemcp.Catalog.Search", "M4 WP-04")
}

// Describe answers describe for p: the unknown-service error for a ref Search would not return to p.
func (c *Catalog) Describe(ctx context.Context, p auth.Principal, ref Ref) (DescribeResult, error) {
	return DescribeResult{}, unimpl.Error("gatemcp.Catalog.Describe", "M4 WP-04")
}
