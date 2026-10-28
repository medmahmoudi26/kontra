package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/tenant"
)

// M4 (WP-07): a multi-tenant gate's services are the operator's rows, never a services: block (spec/relay.md §13.5,
// D116: such a gate refuses to start with one), and its API keys the operator's too (`kontra-gate enroll --api-key`
// mints only a team gate's, D37). With Options.Multi the harness writes them as Cloud would: tenants, services
// (source db) and API keys as rows of the gate's database, each service change followed by NOTIFY kontra_services
// '<tenant>/<name>', which every replica applies (and its periodic re-read would). A service's ref there is
// <tenant>/<name>. The team tenant is "default" (tenant.TeamTenant): buyer A's and B's keys, and the direct services of
// Options.Upstream and Options.Services, are its.

// TeamTenant is the team tenant's slug.
const TeamTenant = "default"

// SeededService is one service row a test writes on a multi-tenant gate.
type SeededService struct {
	Tenant      string   // the tenant's slug (created as an account when it does not exist)
	Name        string   // the service's name
	Kind        string   // "a2a" (default) or "mcp"
	URL         string   // a direct backend's URL (allow_private); "" is a tunnel service
	Allow       []string // allow: (default org:<tenant>)
	Listed      bool
	Annotations map[string]any // services.annotations (spec §13.4)
}

// SeedTenant writes a tenant of kind (team or account) with slug, and returns its id; an existing one is returned
// as it is.
func (h *Harness) SeedTenant(ctx context.Context, slug, kind string) (uuid.UUID, error) {
	if slug == TeamTenant {
		return tenant.EnsureTeam(ctx, h.Pool)
	}
	if id := h.TenantOf(ctx, slug); id != uuid.Nil {
		return id, nil
	}
	id := uuid.Must(uuid.NewV7())
	var got uuid.UUID
	err := h.Pool.QueryRow(ctx, `insert into kontra.tenants (id, slug, kind) values ($1, $2, $3)
		on conflict (slug) do update set slug = excluded.slug returning id`, id, slug, kind).Scan(&got)
	if err != nil {
		return uuid.Nil, fmt.Errorf("e2e: tenant %s: %w", slug, err)
	}
	return got, nil
}

// SeedServices writes services as rows (source db), each created or replaced, then NOTIFY kontra_services for each,
// in one transaction.
func (h *Harness) SeedServices(ctx context.Context, svcs ...SeededService) error {
	ids := map[string]uuid.UUID{}
	for _, s := range svcs {
		if _, ok := ids[s.Tenant]; !ok {
			kind := "account"
			if s.Tenant == TeamTenant {
				kind = "team"
			}
			id, err := h.SeedTenant(ctx, s.Tenant, kind)
			if err != nil {
				return err
			}
			ids[s.Tenant] = id
		}
	}
	return pgx.BeginFunc(ctx, h.Pool, func(tx pgx.Tx) error {
		for _, s := range svcs {
			kind := s.Kind
			if kind == "" {
				kind = "a2a"
			}
			backend := `{"tunnel": {}}`
			if s.URL != "" {
				b, _ := json.Marshal(map[string]any{kind: map[string]any{"url": s.URL, "allow_private": true}})
				backend = string(b)
			}
			allow := s.Allow
			if allow == nil {
				allow = []string{"org:" + s.Tenant}
			}
			ann := s.Annotations
			if ann == nil {
				ann = map[string]any{}
			}
			annJSON, _ := json.Marshal(ann)
			if _, err := tx.Exec(ctx, `insert into kontra.services (tenant_id, name, kind, backend, allow, listed, annotations, source, retries, timeout_s)
				values ($1, $2, $3, $4::jsonb, $5, $6, $7::jsonb, 'db', 1, 600)
				on conflict (tenant_id, name) do update set kind = excluded.kind, backend = excluded.backend, allow = excluded.allow,
					listed = excluded.listed, annotations = excluded.annotations, disabled_at = null, updated_at = now(),
					card = null, upstream_card = null, card_hash = null, card_fetched_at = null`,
				ids[s.Tenant], s.Name, kind, backend, allow, s.Listed, string(annJSON)); err != nil {
				return fmt.Errorf("e2e: service %s/%s: %w", s.Tenant, s.Name, err)
			}
			if _, err := tx.Exec(ctx, `select pg_notify('kontra_services', $1)`, s.Tenant+"/"+s.Name); err != nil {
				return err
			}
		}
		return nil
	})
}

// MintKey writes an API key of the tenant slug (hire by default), as an operator's control plane does, and returns it.
func (h *Harness) MintKey(ctx context.Context, slug, name string, scopes ...auth.Scope) (auth.Minted, error) {
	var id uuid.UUID
	if err := h.Pool.QueryRow(ctx, `select id from kontra.tenants where slug = $1`, slug).Scan(&id); err != nil {
		return auth.Minted{}, fmt.Errorf("e2e: tenant %s: %w", slug, err)
	}
	return auth.NewAPIKeys(h.Pool, nil).Mint(ctx, auth.MintRequest{TenantID: id, Name: name, Scopes: scopes, Actor: "e2e"})
}

// WaitA2A waits until every running replica serves the A2A service ref's card to key (its keeper fetched it).
func (h *Harness) WaitA2A(ctx context.Context, ref, key string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		ok := true
		for _, g := range h.Gates {
			if g != nil && !g.Exited() {
				ok = ok && status(ctx, http.MethodGet, g.URL+"/agents/"+ref+"/.well-known/agent-card.json", key) == http.StatusOK
			}
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("e2e: %s's card is not served by every replica within %v", ref, within)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// WaitMCP waits until every running replica serves the MCP service ref to key: an initialize answered 200 (the session
// is then deleted).
func (h *Harness) WaitMCP(ctx context.Context, ref, key string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		ok := true
		for _, g := range h.Gates {
			if g == nil || g.Exited() {
				continue
			}
			c := NewMCPClient(g.URL+"/mcp/"+ref, key)
			c.Capabilities = `{}`
			if _, err := c.Initialize(ctx); err != nil {
				ok = false
				break
			}
			_, _ = c.Delete(ctx)
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("e2e: %s is not served by every replica within %v", ref, within)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// TenantOf is the id of the tenant slug, uuid.Nil when there is none.
func (h *Harness) TenantOf(ctx context.Context, slug string) uuid.UUID {
	var id uuid.UUID
	if err := h.Pool.QueryRow(ctx, `select id from kontra.tenants where slug = $1`, slug).Scan(&id); err != nil {
		return uuid.Nil
	}
	return id
}

// seedTeam writes, at Start of a multi-tenant harness, the team tenant, buyer A's and B's keys, and the direct services
// of Options.Upstream and Options.Services as rows of the team tenant (the replicas read them when they start).
func (h *Harness) seedTeam(ctx context.Context) error {
	if _, err := tenant.EnsureTeam(ctx, h.Pool); err != nil {
		return err
	}
	a, err := h.MintKey(ctx, TeamTenant, "e2e-a")
	if err != nil {
		return err
	}
	b, err := h.MintKey(ctx, TeamTenant, "e2e-b")
	if err != nil {
		return err
	}
	h.APIKey, h.APIKeyB = a.Secret, b.Secret
	var svcs []SeededService
	for name, up := range h.Upstreams {
		if h.opts.tunnelled(name) {
			continue
		}
		svcs = append(svcs, SeededService{Tenant: TeamTenant, Name: name, URL: up.URL()})
	}
	for name, u := range h.opts.Direct {
		svcs = append(svcs, SeededService{Tenant: TeamTenant, Name: name, URL: u})
	}
	if len(svcs) == 0 {
		return nil
	}
	if err := h.SeedServices(ctx, svcs...); err != nil {
		return err
	}
	return nil
}

// refOf is a direct service's ref as the harness's waits name it: <name> on a team gate, default/<name> on a
// multi-tenant one.
func (h *Harness) refOf(name string) string {
	if h.opts.Multi && !strings.Contains(name, "/") {
		return TeamTenant + "/" + name
	}
	return name
}
