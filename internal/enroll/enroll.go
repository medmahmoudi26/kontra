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
// is service_denied and nothing is enrolled; a binding the operator revoked is left out of the served set
// (a rotation carries it over revoked), and only a one-time token in a hello enrolls it again, never the
// auth API key. The key's tenant is fixed by its first enrollment.
//
// Every query that recognizes a node key is keyed by its full public key (§2.2): a key id alone never
// authenticates anything. Operator commands (revocation) name a key by its id, which is how operators see
// it.
//
// Work package: WP-02.
package enroll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/tunnel"
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

// Errors of the operator's calls (minting, revoking). A handshake's refusals are *tunnel.HandshakeError.
var (
	// ErrNotTunnel is a token asked for a service whose backend is not tunnel.
	ErrNotTunnel = errors.New("enroll: the service's backend is not tunnel")
	// ErrUnknownService is a token asked for a service the tenant does not have.
	ErrUnknownService = errors.New("enroll: no such service")
	// ErrServiceDisabled is a token asked for a disabled service (removed from the configuration, or
	// unpublished).
	ErrServiceDisabled = errors.New("enroll: the service is disabled")
	// ErrUnknownTenant is a token asked for a tenant that does not exist or has ended.
	ErrUnknownTenant = errors.New("enroll: no such tenant")
	// ErrKeyNotFound is a revocation of a key id this gate does not have.
	ErrKeyNotFound = errors.New("enroll: no such node key")
	// ErrBindingNotFound is a binding revocation for a service the key was never enrolled for.
	ErrBindingNotFound = errors.New("enroll: the key is not enrolled for the service")
)

// Options configure a Store.
type Options struct {
	// PublicURL and Multi build the welcome's service URLs ({public_url}/agents/[{tenant}/]{name}).
	PublicURL string
	Multi     bool
	// EnrollWithAPIKey is tunnel.enroll_with_api_key: an API key's enroll: scopes enroll.
	EnrollWithAPIKey bool
	// Keys authenticates hello's auth (auth_invalid when it does not authenticate). Nil (auth.api_keys
	// false) refuses every auth with auth_invalid.
	Keys   *auth.APIKeys
	Logger *slog.Logger
}

// Store is the gate's enrollment tables.
type Store struct {
	db   *pgxpool.Pool
	opts Options
	// lockTimeout bounds Admit's lock waits (admitLockTimeout; tests that hold a lock on purpose raise it).
	lockTimeout time.Duration
}

// NewStore returns a store over db.
func NewStore(db *pgxpool.Pool, o Options) *Store {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	o.PublicURL = strings.TrimRight(o.PublicURL, "/")
	return &Store{db: db, opts: o, lockTimeout: admitLockTimeout}
}

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
	// §2.3), the principal of the streams the node opens (spec/tunnel.md §9).
	HirePrincipal string
	// Hire is the auth API key's principal as the admission authenticated it, nil without auth: the session
	// re-validates it on every stream it opens (spec/tunnel.md §12, relay.md §2.3).
	Hire *auth.Principal
	// Enrolled are the bindings this handshake added to services that already had a key: their other
	// nodes are told on their control streams (§5). Admit announces each with NOTIFY kontra_tunnel
	// (EnrolledPayload) in its own transaction, so the notice rides the binding's commit.
	Enrolled []Binding
	// Rotated is the old key when the handshake rotated (§12).
	Rotated *Key
}

// enrolledPrefix starts the gate-internal NOTIFY kontra_tunnel payload of an enrollment (EnrolledPayload).
const enrolledPrefix = "enrolled:"

// EnrolledPayload is NOTIFY kontra_tunnel's payload (pg.ChannelTunnel) for a key enrolled for a service that
// already had another key, "enrolled:<key_id>/<tenant_id>/<service>": every replica, the one that admitted
// it included, tells its sessions hosting the service (§5; tunnelgate.Registry.Notify). Admit sends it in
// the transaction that inserts the binding, so the notice is never lost to what becomes of the new key's
// session (a welcome lost, a connection cut while the gate fetches its card: M2 review of WP-04).
//
// SPEC-CONFLICT (M2 plan, confirmed conflict on tunnel.md §11 and §5): the spec names no way for one
// replica to reach the sessions another one holds; the channel is gate-internal, Postgres between
// replicas, not tunnel wire protocol (see tunnelgate.Registry).
func EnrolledPayload(b Binding) string {
	return enrolledPrefix + b.KeyID + "/" + b.TenantID.String() + "/" + b.Service
}

// ParseEnrolledPayload reads an EnrolledPayload; ok is false for anything else, a malformed one included.
// The binding's Tenant (its slug) is not carried.
func ParseEnrolledPayload(payload string) (b Binding, ok bool) {
	rest, ok := strings.CutPrefix(payload, enrolledPrefix)
	if !ok {
		return Binding{}, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || !config.NodeKeyIDRE.MatchString(parts[0]) || !config.NameRE.MatchString(parts[2]) {
		return Binding{}, false
	}
	tid, err := uuid.Parse(parts[1])
	if err != nil {
		return Binding{}, false
	}
	return Binding{KeyID: parts[0], TenantID: tid, Service: parts[2]}, true
}

// Payload prefixes of the tunnel's NOTIFY kontra_revoke (§12). API keys' ("apikey:<id>") are auth's.
const (
	revokeKeyPrefix     = "key:"
	revokeBindingPrefix = "binding:"
)

// KeyRevokePayload is NOTIFY kontra_revoke's payload for a node key, "key:<key_id>".
func KeyRevokePayload(keyID string) string { return revokeKeyPrefix + keyID }

// BindingRevokePayload is NOTIFY kontra_revoke's payload for one binding, "binding:<key_id>/<service>".
func BindingRevokePayload(keyID, service string) string {
	return revokeBindingPrefix + keyID + "/" + service
}

// Timeouts of the operator's writes (mint, revoke, the overlap sweep): on the server (statement_timeout)
// and on the client, detached from the caller's cancellation so that a write that started is not cut half
// way (as auth's writes, D8).
const writeTimeout = 10 * time.Second

// write runs fn in one transaction bounded by writeTimeout.
func (s *Store) write(ctx context.Context, fn func(pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout+2*time.Second)
	defer cancel()
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select set_config('statement_timeout', $1, true)`,
			fmt.Sprint(writeTimeout.Milliseconds())); err != nil {
			return err
		}
		return fn(tx)
	})
}

// serviceURL is a served service's gate URL (relay.md §2.1): {public_url}/agents/[{tenant}/]{name} for A2A
// and {public_url}/mcp/[{tenant}/]{name} for MCP.
func (s *Store) serviceURL(kind, tenantSlug, name string) string {
	root := "/agents/"
	if kind == tunnel.KindMCP {
		root = "/mcp/"
	}
	if s.opts.Multi {
		return s.opts.PublicURL + root + tenantSlug + "/" + name
	}
	return s.opts.PublicURL + root + name
}

// pgCode is the SQLSTATE of a Postgres error, "" for any other error.
func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}
