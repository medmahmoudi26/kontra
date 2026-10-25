package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// ErrUnauthenticated is a missing, unknown, revoked or expired credential: HTTP 401 on every route.
var ErrUnauthenticated = errors.New("unauthenticated")

// APIKeys authenticates and mints API keys over kontra.api_keys (sha256 of the key, prefix shown).
type APIKeys struct {
	db      *pgxpool.Pool
	revoked *RevocationCache
}

// NewAPIKeys returns the API key authenticator. Revocation is checked on every request through revoked.
func NewAPIKeys(db *pgxpool.Pool, revoked *RevocationCache) *APIKeys {
	return &APIKeys{db: db, revoked: revoked}
}

// Authenticate maps a bearer key to its principal key:<prefix>, with the key's tenant, buyer and scopes.
// An unknown, revoked or expired key is ErrUnauthenticated. last_used_at is written at most once a minute.
func (k *APIKeys) Authenticate(ctx context.Context, bearer string) (Principal, error) {
	return Principal{}, unimpl.Error("auth.APIKeys.Authenticate", "WP-03")
}

// MintRequest is what `kontra-gate enroll --api-key` asks for.
type MintRequest struct {
	TenantID  uuid.UUID
	Name      string
	BuyerID   *uuid.UUID // nil mints a new buyer
	Scopes    []Scope    // default [hire]
	ExpiresAt *time.Time
	Prefix    string // optional operator-chosen part of the prefix, so key: globs can name a group of keys
}

// Minted is a new key. Secret is shown once and never stored; only its sha256 is.
type Minted struct {
	ID     uuid.UUID
	Prefix string
	Secret string
}

// Mint creates a key, minting again on a prefix collision (spec §2.3).
func (k *APIKeys) Mint(ctx context.Context, req MintRequest) (Minted, error) {
	return Minted{}, unimpl.Error("auth.APIKeys.Mint", "WP-03")
}

// Authenticator turns a request's credential into a principal: an API key, or none (the anonymous
// principal, admitted only by a service whose allow: list says so).
type Authenticator struct {
	keys *APIKeys
}

// NewAuthenticator returns the edge's authenticator.
func NewAuthenticator(keys *APIKeys) *Authenticator {
	return &Authenticator{keys: keys}
}

// Authenticate reads Authorization. No credential returns the anonymous principal with ok=false so the
// route can decide; a bad credential is ErrUnauthenticated even on an anonymous service.
func (a *Authenticator) Authenticate(r *http.Request, anonymousBuyer uuid.UUID) (p Principal, authenticated bool, err error) {
	return Principal{}, false, unimpl.Error("auth.Authenticator.Authenticate", "WP-03")
}
