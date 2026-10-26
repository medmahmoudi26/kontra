// Package auth turns credentials into principals and decides whether an allow: list admits them
// (spec/relay.md §2.3, PRD §12).
//
// M1 accepts API keys (Authorization: Bearer <key>, stored as sha256) and the anonymous principal on
// services whose allow: list contains `anonymous`. OIDC JWTs, MCP OAuth and node principals are later
// milestones; their kinds are declared here so allow: lists parse the same way from M1 on, and a bearer
// token that is not an API key of this gate is 401.
//
// Revocation is checked on every request against a per-replica RevocationCache, refreshed by NOTIFY
// kontra_revoke and re-read from Postgres at least every 5 s; APIKeys keeps verified keys in memory for a
// few minutes so a request does not need a query, and goes back to Postgres whenever that cache is not
// current.
package auth

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/config"
)

// Via is how the principal arrived (spec §2.3); the hooks receive it.
type Via string

// Via values (tasks.via, mcp_sessions.via).
const (
	ViaAPIKey    Via = "api_key"
	ViaOIDC      Via = "oidc"
	ViaMCP       Via = "mcp"
	ViaNode      Via = "node"
	ViaAnonymous Via = "anonymous"
)

// Kind is the principal kind, the prefix of its id.
type Kind string

// Principal kinds.
const (
	KindKey       Kind = "key"  // key:<prefix>
	KindOIDC      Kind = "oidc" // oidc:sub:<sub>
	KindNode      Kind = "node" // node:<key_id> (M2)
	KindAnonymous Kind = "anonymous"
)

// AnonymousID is the id of the anonymous principal.
const AnonymousID = "anonymous"

// Scope is an API key scope (spec §2.3). A key's scopes never grant an operator action.
type Scope string

// Scopes.
const (
	ScopeHire      Scope = "hire"     // the default: call services
	ScopeEnrollAll Scope = "enroll:*" // enroll node keys for any service (M2)
)

// EnrollScope is enroll:<service>: enroll node keys for that service (spec/tunnel.md §5, M2).
func EnrollScope(service string) Scope { return Scope("enroll:" + service) }

// ParseScope parses one scope: hire, enroll:* or enroll:<service name>.
func ParseScope(s string) (Scope, error) {
	switch Scope(s) {
	case ScopeHire, ScopeEnrollAll:
		return Scope(s), nil
	}
	if svc, ok := strings.CutPrefix(s, "enroll:"); ok {
		if !config.NameRE.MatchString(svc) {
			return "", fmt.Errorf("scope %q: enroll: needs * or a service name matching %s", s, config.NameRE)
		}
		return Scope(s), nil
	}
	return "", fmt.Errorf("scope %q: want hire, enroll:* or enroll:<service>", s)
}

// Principal is who a request is from, after authentication.
type Principal struct {
	Kind       Kind
	ID         string     // the full principal id, e.g. "key:kk_acme_ab12cd34"
	TenantID   *uuid.UUID // the key's tenant; nil for anonymous and unmapped OIDC subjects
	TenantSlug string     // that tenant's slug, which org:<tenant> entries name; "" without a tenant
	BuyerID    uuid.UUID  // stable; several keys may share one
	Via        Via
	Scopes     []Scope    // API keys only; other kinds carry none
	KeyID      *uuid.UUID // api_keys.id, for revocation
}

// Anonymous is the anonymous principal with the buyer its service gives it (ids.AnonymousBuyer).
func Anonymous(buyer uuid.UUID) Principal {
	return Principal{Kind: KindAnonymous, ID: AnonymousID, BuyerID: buyer, Via: ViaAnonymous}
}

// Authenticated reports whether p came from a credential rather than being the anonymous principal.
func (p Principal) Authenticated() bool { return p.Kind != KindAnonymous && p.Kind != "" }

// HasScope reports whether an API key principal holds s.
func (p Principal) HasScope(s Scope) bool { return slices.Contains(p.Scopes, s) }

// MayHire reports whether p may call services at all: an API key needs the hire scope (spec §2.3: a key
// with only enroll: scopes enrolls nodes and nothing else); the other kinds carry no scopes and may.
func (p Principal) MayHire() bool {
	if p.Kind == KindKey {
		return p.HasScope(ScopeHire)
	}
	return p.Kind != ""
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the principal the edge authenticated for this request.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
