package gate

import (
	"context"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/a2ahttp"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/unimpl"
)

// TeamTenant is the slug of a team gate's one tenant.
const TeamTenant = tenant.TeamSlug

// Services is the replica's service registry. A team gate upserts its tenant (tenant.EnsureTeam, the code
// `kontra-gate enroll --api-key` runs too) and the services: block into tenants and services (source =
// config) at start and on SIGHUP, marks removed ones disabled_at
// (rows are not deleted), and fetches each direct backend's card; db-sourced services (Cloud) are read
// from Postgres and re-read on NOTIFY kontra_services.
type Services struct{}

var (
	_ a2ahttp.Services          = (*Services)(nil)
	_ lifecycle.BackendResolver = (*Services)(nil)
)

// Sync applies the services of cfg.
func (s *Services) Sync(ctx context.Context, cfg *config.Config) error {
	return unimpl.Error("gate.Services.Sync", "WP-08")
}

// Lookup implements a2ahttp.Services.
func (s *Services) Lookup(ctx context.Context, tenant, name string) (*a2ahttp.Service, error) {
	return nil, unimpl.Error("gate.Services.Lookup", "WP-08")
}

// BackendFor implements lifecycle.BackendResolver.
func (s *Services) BackendFor(ctx context.Context, tenantID uuid.UUID, service string) (store.Backend, error) {
	return "", unimpl.Error("gate.Services.BackendFor", "WP-08")
}
