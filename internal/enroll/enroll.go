// Package enroll binds node keys to services at one gate, and revokes and rotates them (spec/tunnel.md §5,
// §12; relay.md §2.3; D40, D61). Everything here is a row in the gate's Postgres (migrations/0001_init.sql:
// tunnel_keys, key_services, enrollment_tokens, audit) and every enrollment, revocation and rotation
// appends an audit row in the same transaction.
//
// Authorities (§5):
//
//   - Service token: `kontra-gate enroll <service>` prints one on a team gate (Cloud: its own API). Single
//     use, 15 minutes, stored as sha256, bound to one service of one tenant; `ket_` + 32 base62 characters.
//   - Tenant token: issued by the operator only (Cloud's "Add a node"; this package's MintTenantToken is
//     the code it and the tests call). Single use, 15 minutes, bound to the tenant and a list of service
//     names it may bind (D40); it creates the services that do not exist yet (backend tunnel, allow
//     [org:<tenant>]) and never adds a key to a service that already has one.
//   - API key (hello's auth) with scope enroll:<service> or enroll:*, only when tunnel.enroll_with_api_key
//     is true (default false on self-hosted gates).
//
// A token is consumed in the transaction that inserts key_services, after the proof verified. The
// handshake is all-or-nothing: a declared service the key was never enrolled for and is not enrolling now
// is service_denied and nothing is enrolled; a binding the operator revoked is left out of the served set.
// The key's tenant is fixed by its first enrollment.
//
// Work package: WP-02.
package enroll

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/unimpl"
)

// Tokens (§5).
const (
	TokenPrefix = "ket_"
	TokenTTL    = 15 * time.Minute
	// RotationOverlap is how long a rotated key keeps serving the tasks already on its session (§12).
	RotationOverlap = 24 * time.Hour
)

// Token scopes (enrollment_tokens.scope, D40).
const (
	ScopeService = "service"
	ScopeTenant  = "tenant"
)

// Revocation reasons (tunnel_keys.revoked_reason).
const (
	ReasonOwner    = "owner"
	ReasonOperator = "operator"
	ReasonRotated  = "rotated"
	ReasonExpired  = "expired" // anonymous expiry, M4
)

// Audit events.
const (
	EventEnrolled       = "tunnel.enrolled"
	EventTokenMinted    = "tunnel.token_minted"
	EventKeyRevoked     = "tunnel.key_revoked"
	EventBindingRevoked = "tunnel.binding_revoked"
	EventRotated        = "tunnel.rotated"
	EventOverlapEnded   = "tunnel.overlap_ended"
	EventServiceCreated = "tunnel.service_created"
)

// ErrNotTunnel is a token asked for a service whose backend is not tunnel.
var ErrNotTunnel = errors.New("enroll: the service's backend is not tunnel")

// Options configure a Store.
type Options struct {
	// PublicURL and Multi build the welcome's service URLs ({public_url}/agents/[{tenant}/]{name}).
	PublicURL string
	Multi     bool
	// EnrollWithAPIKey is tunnel.enroll_with_api_key: an API key's enroll: scopes enroll.
	EnrollWithAPIKey bool
	// Keys authenticates hello's auth (auth_invalid when it does not authenticate).
	Keys   *auth.APIKeys
	Logger *slog.Logger
}

// Store is the gate's enrollment tables.
type Store struct {
	db   *pgxpool.Pool
	opts Options
}

// NewStore returns a store over db.
func NewStore(db *pgxpool.Pool, o Options) *Store { return &Store{db: db, opts: o} }

// Key is one tunnel_keys row.
type Key struct {
	KeyID         string
	TenantID      uuid.UUID
	Tenant        string // slug
	PubKey        []byte // 32 bytes X25519
	NodeName      string
	RevokedAt     *time.Time
	RevokedReason string
	RotatedTo     string
	OverlapUntil  *time.Time
}

// MintServiceToken mints a service token for one service of a tenant and returns it (shown once). The
// service must exist, be enabled, and have backend tunnel (ErrNotTunnel).
func (s *Store) MintServiceToken(ctx context.Context, tenantID uuid.UUID, service, actor string) (string, error) {
	return "", unimpl.Error("enroll.MintServiceToken", "WP-02")
}

// MintTenantToken mints a tenant token bound to the service names it may bind (D40).
func (s *Store) MintTenantToken(ctx context.Context, tenantID uuid.UUID, services []string, actor string) (string, error) {
	return "", unimpl.Error("enroll.MintTenantToken", "WP-02")
}

// Request is a hello whose proof (and proof_old) verified, with what the gate decided around it.
type Request struct {
	Hello     *tunnel.Hello
	PubKey    []byte // the hello's recipient, decoded
	OldPubKey []byte // rotate_from's recipient, decoded; nil without rotate_from
	SessionID uuid.UUID
	ReplicaID string
}

// Binding names one key_services row.
type Binding struct {
	KeyID    string
	TenantID uuid.UUID
	Tenant   string
	Service  string
}

// Decision is an admitted handshake.
type Decision struct {
	Key Key
	// Served is what the session will receive requests for: the declared services the key is enrolled for
	// (after this handshake's enrollments), revoked bindings left out; the welcome's services and
	// tunnels.services.
	Served []tunnel.WelcomeService
	// HirePrincipal is tunnels.hire_principal: the auth API key's principal, else node:<key_id> (relay.md
	// §2.3). Hiring streams are M3.
	HirePrincipal string
	// Enrolled are the bindings this handshake added to services that already had a key: their other
	// nodes are told on their control streams (§5).
	Enrolled []Binding
	// Rotated is the old key when the handshake rotated (§12).
	Rotated *Key
}

// Admit decides a proven hello in one transaction (§4, §5, §12): the key's lookup (a known key's stored
// public key must equal the hello's), revocation (revoked), the API key (auth_invalid), the tokens and
// scopes (enroll_invalid, kind_mismatch, service_denied), the rotation (under a row lock on the old key),
// the inserts and the audit rows. A refusal is a *tunnel.HandshakeError; anything else is the gate's own
// failure.
func (s *Store) Admit(ctx context.Context, req Request) (*Decision, error) {
	return nil, unimpl.Error("enroll.Admit", "WP-02")
}

// RevokeKey sets tunnel_keys.revoked_at (reason owner or operator), appends the audit row and sends
// NOTIFY kontra_revoke 'key:<key_id>' in one transaction (§12, `kontra-gate enroll --revoke <key_id>`).
func (s *Store) RevokeKey(ctx context.Context, keyID, reason, actor string) error {
	return unimpl.Error("enroll.RevokeKey", "WP-02")
}

// RevokeBinding sets key_services.revoked_at for one service, appends the audit row and sends NOTIFY
// kontra_revoke 'binding:<key_id>/<service>' (§12).
func (s *Store) RevokeBinding(ctx context.Context, keyID, service, actor string) error {
	return unimpl.Error("enroll.RevokeBinding", "WP-02")
}

// EndOverlaps sets revoked_at on rotated keys whose overlap_until has passed, with their audit rows and a
// NOTIFY 'key:<key_id>' each (§12: "revoked_at is set when the overlap ends"), and returns their ids. A
// replica's sweep calls it.
func (s *Store) EndOverlaps(ctx context.Context) ([]string, error) {
	return nil, unimpl.Error("enroll.EndOverlaps", "WP-02")
}

// Recipients is a sealed service's recipients: the keys enrolled for it, neither revoked nor rotated,
// connected or not (§12, D61). Sealing (M3) publishes them; M2 builds the query.
func (s *Store) Recipients(ctx context.Context, tenantID uuid.UUID, service string) ([]string, error) {
	return nil, unimpl.Error("enroll.Recipients", "WP-02")
}
