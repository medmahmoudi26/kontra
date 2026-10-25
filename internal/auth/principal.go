// Package auth turns credentials into principals and decides whether an allow: list admits them
// (spec/relay.md §2.3, PRD §12).
//
// M1 accepts API keys (Authorization: Bearer <key>, stored as sha256) and the anonymous principal on
// services whose allow: list contains `anonymous`. OIDC JWTs, MCP OAuth and node principals are later
// milestones; their kinds are declared here so allow: lists parse the same way from M1 on.
package auth

import (
	"context"

	"github.com/google/uuid"
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

// Scope is an API key scope (spec §2.3). A key's scopes never grant an operator action.
type Scope string

// Scopes.
const (
	ScopeHire      Scope = "hire"     // the default: call services
	ScopeEnrollAll Scope = "enroll:*" // enroll node keys for any service (M2)
)

// Principal is who a request is from, after authentication.
type Principal struct {
	Kind     Kind
	ID       string     // the full principal id, e.g. "key:kk_ab12cd34"
	TenantID *uuid.UUID // the key's tenant; nil for anonymous and unmapped OIDC subjects
	BuyerID  uuid.UUID  // stable; several keys may share one
	Via      Via
	Scopes   []Scope
	KeyID    *uuid.UUID // api_keys.id, for revocation
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
