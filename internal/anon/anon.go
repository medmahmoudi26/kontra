// Package anon is anonymous sessions on a multi-tenant gate with tunnel.anonymous set (spec/tunnel.md §6; Cloud's
// public gate, never a default; DM4-10).
//
// A hello with neither enroll nor auth whose key is unknown at the gate, or belongs to an anonymous tenant, opens
// one: internal/enroll's admission calls Open inside its transaction, which creates (or finds again, for the same
// key before expiry) a tenant anon-<16 lowercase hex> of kind anonymous with expires_at and created_from (the
// client's IPv4 /24 or IPv6 /48, never the address), and the declared services under it with allow
// [org:anon-…], plus anonymous for those the hello lists in public (kontra connect --public). They are never
// listed and never in search.
//
// Ten minutes before the end the gate sends {"t": "expiring"}; at the end Sweeper.Expire revokes the key's
// bindings, disables the services (404; tasks in flight fail service_unavailable), sends {"t": "revoked"} and closes
// the session, without revoking the key: its next hello opens a new tenant with a new slug. Sweeper.Purge deletes
// the tenant's services, enrollments, tasks, calls, MCP sessions and bucket objects purge_after later and keeps the
// tenant row as a tombstone (tenants.deleted_at), so a slug is never reused.
//
// The runtime limits (§6.2) are checked where the work is created: Caps.Task when a task is created (as host and as
// hirer, under a per-tenant lock), the hiring-stream cap in internal/tunnelgate (8 for an anonymous session, D182),
// the parts total with relay §6.6's byte accounting.
//
// kontra login's conversion (§6.3, DM4-11) is Convert, also inside the admission's transaction.
//
// Work packages: M4 WP-03 (sessions, limits, sweeps, --public), M4 WP-06 (Convert).
package anon

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kontra.sh/kontra/internal/unimpl"
)

// Limits are tunnel.anonymous's (spec/tunnel.md §6.2, relay.md §17).
type Limits struct {
	TTL               time.Duration
	TenantsPerNetwork int
	Services          int
	Tasks             int // as host and as hirer, each
	HiringStreams     int
	PartsBytes        int64
	PurgeAfter        time.Duration
}

// Defaults are §6.2's.
var Defaults = Limits{
	TTL: 2 * time.Hour, TenantsPerNetwork: 3, Services: 4, Tasks: 4, HiringStreams: 8, PartsBytes: 64 << 20,
	PurgeAfter: 24 * time.Hour,
}

// ExpiringNotice is how long before expires_at the gate sends {"t": "expiring"} (§6.1).
const ExpiringNotice = 10 * time.Minute

// SlugPrefix starts every anonymous tenant's slug: anon- and 16 lowercase hex digits (8 random bytes).
const SlugPrefix = "anon-"

// Refusals Open and Caps return; internal/enroll maps them to handshake codes (rate_limited, service_denied) and the
// relays to 429.
var (
	ErrTooManyTenants  = errors.New("anon: too many anonymous sessions from this network")
	ErrTooManyServices = errors.New("anon: too many services for an anonymous session")
	ErrNotDeclared     = errors.New("anon: a public service must be one of the declared services")
	ErrTooManyTasks    = errors.New("anon: too many tasks in flight for this anonymous session")
	ErrPartsFull       = errors.New("anon: this anonymous session stores no more parts")
)

// Network is the network an address is counted and stored by: its IPv4 /24 or its IPv6 /48 (an IPv4-mapped IPv6
// address is IPv4).
func Network(addr netip.Addr) netip.Prefix {
	panic(unimpl.Error("anon.Network", "M4 WP-03"))
}

// NewSlug mints anon-<16 lowercase hex>.
func NewSlug() (string, error) {
	return "", unimpl.Error("anon.NewSlug", "M4 WP-03")
}

// Service is one service a hello declares.
type Service struct {
	Kind   string // a2a | mcp
	Name   string
	Public bool // listed in the hello's public
}

// OpenRequest is what the admission knows when a hello opens or resumes an anonymous session.
type OpenRequest struct {
	KeyID    string
	Network  netip.Prefix
	Services []Service
	Now      time.Time
}

// Tenant is the anonymous tenant a session runs under.
type Tenant struct {
	ID        uuid.UUID
	Slug      string
	ExpiresAt time.Time
	Resumed   bool // the same key before expiry: the tenant it already had
}

// Open creates or resumes the key's anonymous tenant and its services, in tx (the admission's transaction, under
// its key lock): the per-network count under a lock on the network, the services limit, public names checked
// against the declared ones.
func Open(ctx context.Context, tx pgx.Tx, lim Limits, req OpenRequest) (*Tenant, error) {
	return nil, unimpl.Error("anon.Open", "M4 WP-03")
}

// Sweeper ends anonymous tenants: Expire at expires_at, Purge purge_after later. Every replica runs it; each row is
// claimed once (skip locked), and the replica holding a session closes it on the NOTIFY the expiry sends.
type Sweeper struct {
	lim Limits
}

// NewSweeper sweeps with lim.
func NewSweeper(lim Limits) *Sweeper { return &Sweeper{lim: lim} }

// Expiring returns the sessions due their {"t": "expiring"} notice (once each).
func (s *Sweeper) Expiring(ctx context.Context, tx pgx.Tx, now time.Time) ([]uuid.UUID, error) {
	return nil, unimpl.Error("anon.Sweeper.Expiring", "M4 WP-03")
}

// Expire ends the tenants whose expires_at has passed: bindings revoked, services disabled, NOTIFY kontra_revoke and
// kontra_services so that every replica answers 404 and the holder closes the session with {"t": "revoked"}.
func (s *Sweeper) Expire(ctx context.Context, tx pgx.Tx, now time.Time) (int, error) {
	return 0, unimpl.Error("anon.Sweeper.Expire", "M4 WP-03")
}

// Purge deletes what the tenants expired purge_after ago hold, bucket objects first, and sets tenants.deleted_at.
func (s *Sweeper) Purge(ctx context.Context, tx pgx.Tx, now time.Time) (int, error) {
	return 0, unimpl.Error("anon.Sweeper.Purge", "M4 WP-03")
}

// Role is how a new task counts against an anonymous tenant (§6.2).
type Role int

// Roles.
const (
	AsHost  Role = iota + 1 // the task's service is the tenant's
	AsHirer                 // the task's principal is a key of the tenant
)

// Caps checks the runtime limits of anonymous tenants.
type Caps struct {
	lim Limits
}

// NewCaps checks against lim.
func NewCaps(lim Limits) *Caps { return &Caps{lim: lim} }

// Task checks, in tx (the transaction that creates the task) and under a per-tenant lock, that the tenant has
// fewer than Tasks non-terminal tasks in role: ErrTooManyTasks otherwise. A tenant that is not anonymous passes.
func (c *Caps) Task(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, role Role) error {
	return unimpl.Error("anon.Caps.Task", "M4 WP-03")
}

// ConvertRequest is a hello that converts an anonymous key (§6.3): its key, the account the auth API key belongs
// to, and the services it declares.
type ConvertRequest struct {
	KeyID     string
	AccountID uuid.UUID
	Services  []Service
	Now       time.Time
}

// Convert moves an anonymous key to the account in tx (the admission's transaction): the declared services created
// in the account where missing (same names, allow [org:<account>]; another kind is kind_mismatch and nothing moves),
// the key moved with bindings to them, the anonymous tenant expired at once, and an audit row.
func Convert(ctx context.Context, tx pgx.Tx, req ConvertRequest) error {
	return unimpl.Error("anon.Convert", "M4 WP-06")
}
