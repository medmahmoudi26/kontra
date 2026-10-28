// Package anon is anonymous sessions on a multi-tenant gate with tunnel.anonymous set (spec/tunnel.md §6; Cloud's
// public gate, never a default; DM4-10, DM4-18, DM4-19, DM4-20).
//
// A hello that carries anonymous: true and neither enroll nor auth, from a key that is unknown at the gate or belongs
// to an anonymous tenant, opens or resumes one (DM4-18): internal/enroll's admission calls Open inside its
// transaction, under its row lock on the key. Open creates (or finds again, for the same key before expiry) a tenant
// anon-<16 lowercase hex> of kind anonymous with expires_at and created_from (the client's IPv4 /24 or IPv6 /48,
// never the address), and the declared services under it with allow [org:anon-…], plus anonymous for those the hello
// lists in public (kontra connect --public). They are never listed and never in search. A key of an ended anonymous
// tenant opens a new tenant with a new slug: the key moves (MoveKey), it is never revoked by expiry.
//
// Ten minutes before the end the gate sends {"t": "expiring"} (internal/tunnelgate's per-session timer); at the end
// the session's replica sends {"t": "revoked"} and closes it, and Sweeper.Expire (End) revokes the key's bindings and
// disables the services (404; tasks in flight fail service_unavailable at their next dispatch, D117) without revoking
// the key. Sweeper.Purge deletes what the tenant still holds purge_after later, bucket objects first, and keeps the
// tenant row as a tombstone (tenants.deleted_at), so a slug is never reused.
//
// The runtime limits (§6.2) are checked where the work is created: Caps.Task when a task is created (as host and as
// hirer, under a per-tenant lock), Caps.Call around a plain MCP call (gate-wide, in anon_calls), the hiring-stream cap
// in internal/tunnelgate, the parts total with relay §6.6's byte accounting (Caps.Message before a task row,
// Caps.Budget at each dispatch).
//
// kontra login's conversion (§6.3, DM4-11) and enroll tokens from an anonymous key (DM4-19) move the key in the
// admission's transaction (MoveKey, then §5's binding in internal/enroll) and end the tenant there (Convert).
//
// Work packages: M4 WP-03 (sessions, limits, sweeps, --public), M4 WP-06 (Convert).
package anon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/pg"
)

// Limits are tunnel.anonymous's (spec/tunnel.md §6.2, relay.md §17; DM4-10, DM4-20).
type Limits struct {
	TTL               time.Duration
	TenantsPerNetwork int
	Services          int
	Tasks             int // as host and as hirer, each
	Calls             int // concurrent plain MCP calls to the tenant's services
	HiringStreams     int
	PartsBytes        int64
	PurgeAfter        time.Duration
	MaxTenants        int // live anonymous tenants, gate-wide
}

// Defaults are §6.2's.
var Defaults = Limits{
	TTL: config.DefaultAnonTTL, TenantsPerNetwork: config.DefaultAnonTenantsPerNetwork, Services: config.DefaultAnonServices,
	Tasks: config.DefaultAnonTasks, Calls: config.DefaultAnonCalls, HiringStreams: config.DefaultAnonHiringStreams,
	PartsBytes: config.DefaultAnonPartsBytes, PurgeAfter: config.DefaultAnonPurgeAfter, MaxTenants: config.DefaultAnonMaxTenants,
}

// LimitsFrom maps tunnel.anonymous to Limits; a zero field (a disabled block, or a test's value left out) takes the
// default.
func LimitsFrom(a config.AnonymousSessions) Limits {
	l := Limits{
		TTL: a.TTL.D(), TenantsPerNetwork: a.TenantsPerNetwork, Services: a.Services, Tasks: a.Tasks, Calls: a.Calls,
		HiringStreams: a.HiringStreams, PartsBytes: a.PartsBytes.Int64(), PurgeAfter: a.PurgeAfter.D(), MaxTenants: a.MaxTenants,
	}
	return l.withDefaults()
}

func (l Limits) withDefaults() Limits {
	d := Defaults
	set := func(dst *int, def int) {
		if *dst <= 0 {
			*dst = def
		}
	}
	if l.TTL <= 0 {
		l.TTL = d.TTL
	}
	if l.PurgeAfter <= 0 {
		l.PurgeAfter = d.PurgeAfter
	}
	if l.PartsBytes <= 0 {
		l.PartsBytes = d.PartsBytes
	}
	set(&l.TenantsPerNetwork, d.TenantsPerNetwork)
	set(&l.Services, d.Services)
	set(&l.Tasks, d.Tasks)
	set(&l.Calls, d.Calls)
	set(&l.HiringStreams, d.HiringStreams)
	set(&l.MaxTenants, d.MaxTenants)
	return l
}

// ExpiringNotice is how long before expires_at the gate sends {"t": "expiring"} (§6.1).
const ExpiringNotice = 10 * time.Minute

// SlugPrefix starts every anonymous tenant's slug: anon- and 16 lowercase hex digits (8 random bytes).
const SlugPrefix = "anon-"

// slugRE is migration 0003's CHECK anonymous_slug: a tenant is anonymous if and only if its slug matches it.
var slugRE = regexp.MustCompile(`^anon-[0-9a-f]{16}$`)

// IsSlug reports whether slug is an anonymous tenant's. The CHECK makes it exact: a tenant whose slug matches is
// anonymous, and every anonymous tenant's does, so the relays decide by the slug they already hold, without a read.
func IsSlug(slug string) bool { return slugRE.MatchString(slug) }

// ExtensionURI is the gate-asserted card extension every served card of an anonymous service carries (the M4
// corrections' recommendation): an ephemeral session's card, signed by the gate, does not lend it Kontra's identity.
const ExtensionURI = "https://kontra.sh/ext/anonymous/v1"

// ErrRateLimited is wrapped by every runtime refusal the relays answer 429 with -32600 "rate limited" (§6.2).
var ErrRateLimited = errors.New("rate limited")

// Refusals Open and Caps return; internal/enroll maps Open's to handshake codes, the relays Caps' to their answers.
var (
	// ErrUnavailable: the gate's live anonymous tenants number max_tenants (anonymous_unavailable).
	ErrUnavailable = errors.New("anon: this gate opens no more anonymous sessions now")
	// ErrTooManyTenants: the client's network has tenants_per_network live anonymous tenants (rate_limited).
	ErrTooManyTenants = errors.New("anon: too many anonymous sessions from this network")
	// ErrTooManyServices: the hello declares more than services services (service_denied, nothing enrolled).
	ErrTooManyServices = errors.New("anon: too many services for an anonymous session")
	// ErrNotDeclared: a public name the hello does not declare (service_denied).
	ErrNotDeclared = errors.New("anon: a public service must be one of the declared services")
	// ErrKindMismatch: a declared service the tenant has with the other kind (kind_mismatch).
	ErrKindMismatch = errors.New("anon: the anonymous session has this service with another kind")
	// ErrTooManyTasks: the tenant has tasks non-terminal tasks in the role (429).
	ErrTooManyTasks = fmt.Errorf("%w: too many tasks in flight for this anonymous session", ErrRateLimited)
	// ErrTooManyCalls: calls plain MCP calls to the tenant's services are in flight (429).
	ErrTooManyCalls = fmt.Errorf("%w: too many MCP calls in flight to this anonymous session's services", ErrRateLimited)
	// ErrStorageFull: a caller's message would take the tenant's stored bytes past parts_bytes (InvalidParams
	// "anonymous storage limit", before any task row).
	ErrStorageFull = errors.New("anonymous storage limit")
)

// RetryAfter is the Retry-After of a runtime refusal (§6.2, relay.md §2.2): in-flight caps free as work ends.
const RetryAfter = 5 * time.Second

// Network is the network an address is counted and stored by: its IPv4 /24 or its IPv6 /48 (an IPv4-mapped IPv6
// address is IPv4). An invalid address has no network (the zero Prefix).
func Network(addr netip.Addr) netip.Prefix {
	if !addr.IsValid() {
		return netip.Prefix{}
	}
	addr = addr.Unmap().WithZone("")
	bits := 48
	if addr.Is4() {
		bits = 24
	}
	p, err := addr.Prefix(bits)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

// NewSlug mints anon-<16 lowercase hex>.
func NewSlug() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("anon: slug: %w", err)
	}
	return SlugPrefix + hex.EncodeToString(b[:]), nil
}

// newSlug is NewSlug; tests replace it to force a collision.
var newSlug = NewSlug

// slugAttempts is how often Open draws a slug that does not collide (a tombstone included) before it gives up.
const slugAttempts = 8

// Advisory lock keys: "ktan" in the high half, far from the gate's other keys ("kontra" + two bytes: audit.LockID, the
// service sync, migrate) and from the enrollments' "ktsv". openLock serializes the opening of new tenants gate-wide
// (max_tenants and tenants_per_network are counted under it).
const (
	lockSpace int64 = 0x6b74616e << 32
	openLock        = lockSpace | 1
)

// Service is one service a hello declares.
type Service struct {
	Kind   string // a2a | mcp
	Name   string
	Public bool // listed in the hello's public
}

// Key is the hello's key as the admission locked it.
type Key struct {
	ID       string
	PubKey   []byte
	NodeName string
	// TenantID is the key's tenant when the key is known (an anonymous tenant, live or ended); uuid.Nil for a new key.
	TenantID uuid.UUID
}

// OpenRequest is what the admission knows when a hello opens or resumes an anonymous session.
type OpenRequest struct {
	Key Key
	// Network is the client's (Network of the edge's client address, relay.md §2.2).
	Network  netip.Prefix
	Services []Service
}

// Tenant is the anonymous tenant a session runs under.
type Tenant struct {
	ID        uuid.UUID
	Slug      string
	ExpiresAt time.Time
	Resumed   bool // the same key before expiry: the tenant it already had
	// Enabled are the services this hello created, enabled again or bound again; Disabled those it no longer
	// declares (disabled, their bindings revoked). Both sorted.
	Enabled, Disabled []string
}

// Open creates or resumes the key's anonymous tenant and its services, in tx (the admission's transaction, under its
// row lock on the key), and queues the NOTIFYs that apply them on every replica (kontra_services, kontra_revoke):
//
//   - The key's tenant, when it is a live anonymous tenant, is resumed (Resumed; its expires_at is kept).
//   - Otherwise a new tenant is opened under openLock: ErrUnavailable when the live anonymous tenants number
//     MaxTenants, ErrTooManyTenants when the network's number TenantsPerNetwork; a slug that exists (a tombstone
//     included) is drawn again. A new key is inserted in it; a known one is moved there (MoveKey).
//   - The declared services, at most Services (ErrTooManyServices), and the public names among them
//     (ErrNotDeclared): each is created, enabled again and bound to the key, with allow [org:<slug>] plus anonymous
//     when public, and the anonymous card extension in its annotations; one the tenant has with the other kind is
//     ErrKindMismatch. The tenant's other services are disabled and their bindings revoked, so a tenant's services
//     that are not disabled are always the last hello's (§6.2: reconnecting with new names never grows it).
//
// A refusal leaves nothing written when tx rolls back (the handshake is all-or-nothing).
func Open(ctx context.Context, tx pgx.Tx, lim Limits, req OpenRequest) (*Tenant, error) {
	lim = lim.withDefaults()
	if len(req.Services) > lim.Services {
		return nil, fmt.Errorf("%w: %d declared, at most %d", ErrTooManyServices, len(req.Services), lim.Services)
	}
	seen := map[string]bool{}
	for _, s := range req.Services {
		if seen[s.Name] {
			return nil, fmt.Errorf("anon: service %q declared twice", s.Name)
		}
		seen[s.Name] = true
	}
	var t *Tenant
	if req.Key.TenantID != uuid.Nil {
		var err error
		if t, err = resume(ctx, tx, req.Key.TenantID); err != nil {
			return nil, err
		}
	}
	if t == nil {
		var err error
		if t, err = open(ctx, tx, lim, req); err != nil {
			return nil, err
		}
	}
	if err := applyServices(ctx, tx, t, req); err != nil {
		return nil, err
	}
	return t, nil
}

// resume returns the key's tenant when it is a live anonymous tenant, locked (FOR NO KEY UPDATE, which keeps Expire's
// FOR UPDATE SKIP LOCKED off it until the admission commits), and nil otherwise. A tenant that has ended is read, never
// locked: the admission already holds the key's row, and a purge holds the ended tenant's row before it takes the
// tenant's keys, so locking it here would close a cycle (the M4 WP-03 review; internal/enroll locks a key's anonymous
// tenant right after the key, before anything else, and the purge takes the keys with NOWAIT).
func resume(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (*Tenant, error) {
	live := func(sql string) (*Tenant, error) {
		t := &Tenant{ID: tenantID, Resumed: true}
		var ok bool
		err := tx.QueryRow(ctx, sql, tenantID).Scan(&t.Slug, &t.ExpiresAt, &ok)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil, nil
		case err != nil:
			return nil, err
		case !ok:
			return nil, nil
		}
		return t, nil
	}
	const read = `select slug, expires_at, kind = 'anonymous' and deleted_at is null and expires_at > now()
		from kontra.tenants where id = $1`
	if t, err := live(read); t == nil || err != nil {
		return nil, err
	}
	// Live: locked, and read again under the lock (an expiry committed meanwhile is seen).
	return live(read + ` for no key update`)
}

func open(ctx context.Context, tx pgx.Tx, lim Limits, req OpenRequest) (*Tenant, error) {
	if !req.Network.IsValid() {
		return nil, errors.New("anon: no client network to count the session by")
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, openLock); err != nil {
		return nil, err
	}
	var all, network int
	err := tx.QueryRow(ctx, `select count(*), count(*) filter (where created_from = $1)
		from kontra.tenants where kind = 'anonymous' and deleted_at is null and expires_at > now()`, req.Network).Scan(&all, &network)
	if err != nil {
		return nil, err
	}
	if all >= lim.MaxTenants {
		return nil, fmt.Errorf("%w (%d live, max_tenants %d)", ErrUnavailable, all, lim.MaxTenants)
	}
	if network >= lim.TenantsPerNetwork {
		return nil, fmt.Errorf("%w (%d live from %s, tenants_per_network %d)", ErrTooManyTenants, network, req.Network, lim.TenantsPerNetwork)
	}
	t := &Tenant{}
	for range slugAttempts {
		id, err := uuid.NewV7()
		if err != nil {
			return nil, err
		}
		slug, err := newSlug()
		if err != nil {
			return nil, err
		}
		err = tx.QueryRow(ctx, `insert into kontra.tenants (id, slug, kind, expires_at, created_from)
			values ($1, $2, 'anonymous', now() + make_interval(secs => $3), $4)
			on conflict (slug) do nothing returning expires_at`, id, slug, lim.TTL.Seconds(), req.Network).Scan(&t.ExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // the slug exists, a tombstone included (§6.1): draw again
		}
		if err != nil {
			return nil, err
		}
		t.ID, t.Slug = id, slug
		break
	}
	if t.ID == uuid.Nil {
		return nil, fmt.Errorf("anon: no free slug after %d draws", slugAttempts)
	}
	if req.Key.TenantID == uuid.Nil {
		if _, err := tx.Exec(ctx, `insert into kontra.tunnel_keys (key_id, tenant_id, pubkey, node_name) values ($1, $2, $3, $4)`,
			req.Key.ID, t.ID, req.Key.PubKey, req.Key.NodeName); err != nil {
			return nil, err // 23505: a concurrent handshake inserted the key; the admission runs again
		}
	} else if err := MoveKey(ctx, tx, req.Key.ID, t.ID); err != nil {
		return nil, err
	}
	return t, nil
}

// MoveKey moves a key to another tenant in tx, which holds the key's row lock (§6.1, DM4-19): the key's key_services
// rows are deleted first, since their foreign key (key_id, tenant_id) has no ON UPDATE, then tunnel_keys.tenant_id
// is set. It is every key move's: a new anonymous tenant after expiry, enroll from an anonymous key, §6.3's
// conversion.
func MoveKey(ctx context.Context, tx pgx.Tx, keyID string, to uuid.UUID) error {
	if _, err := tx.Exec(ctx, `delete from kontra.key_services where key_id = $1`, keyID); err != nil {
		return fmt.Errorf("anon: move key %s: %w", keyID, err)
	}
	tag, err := tx.Exec(ctx, `update kontra.tunnel_keys set tenant_id = $2 where key_id = $1`, keyID, to)
	if err != nil {
		return fmt.Errorf("anon: move key %s: %w", keyID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("anon: move key %s: no such key", keyID)
	}
	return nil
}

// CardAnnotations is an anonymous service's services.annotations: the gate-asserted card extension (ExtensionURI)
// naming the session's end, appended to its served card's capabilities.extensions (relay.md §3, §13.4), so that a
// card the gate signs says the service is an ephemeral anonymous session's.
func CardAnnotations(expires time.Time) ([]byte, error) {
	return json.Marshal(map[string]any{"card_extensions": []any{map[string]any{
		"uri":         ExtensionURI,
		"description": "An anonymous session's service: ephemeral, unverified, and not operated by the gate's operator. It ends at expiresAt.",
		"required":    false,
		"params":      map[string]any{"expiresAt": expires.UTC().Format(time.RFC3339)},
	}}})
}

func applyServices(ctx context.Context, tx pgx.Tx, t *Tenant, req OpenRequest) error {
	names := make([]string, 0, len(req.Services))
	for _, s := range req.Services {
		names = append(names, s.Name)
	}
	rows, err := tx.Query(ctx, `select name, kind from kontra.services where tenant_id = $1 and name = any($2::text[])`, t.ID, names)
	if err != nil {
		return err
	}
	have := map[string]string{}
	for rows.Next() {
		var n, k string
		if err := rows.Scan(&n, &k); err != nil {
			rows.Close()
			return err
		}
		have[n] = k
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, s := range req.Services {
		if k, ok := have[s.Name]; ok && k != s.Kind {
			return fmt.Errorf("%w: %s is %s, declared %s", ErrKindMismatch, s.Name, k, s.Kind)
		}
	}
	ann, err := CardAnnotations(t.ExpiresAt)
	if err != nil {
		return err
	}
	var changed []string
	for _, s := range req.Services {
		allow := []string{"org:" + t.Slug}
		if s.Public {
			allow = append(allow, "anonymous")
		}
		// Created, or enabled again with this hello's allow (public applied anew, §6.1); a row this hello leaves as it
		// was is not announced.
		var touched bool
		err := tx.QueryRow(ctx, `
			insert into kontra.services as s (tenant_id, name, kind, backend, allow, source, annotations)
			values ($1, $2, $3, '{"tunnel": {}}', $4, 'db', $5)
			on conflict (tenant_id, name) do update set disabled_at = null, allow = excluded.allow,
			       annotations = excluded.annotations, updated_at = now()
			 where s.disabled_at is not null or s.allow is distinct from excluded.allow or s.annotations is distinct from excluded.annotations
			returning true`, t.ID, s.Name, s.Kind, allow, ann).Scan(&touched)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		tag, err := tx.Exec(ctx, `
			insert into kontra.key_services (key_id, tenant_id, service) values ($1, $2, $3)
			on conflict (key_id, service) do update set revoked_at = null, enrolled_at = now()
			 where key_services.revoked_at is not null`, req.Key.ID, t.ID, s.Name)
		if err != nil {
			return err
		}
		if touched || tag.RowsAffected() > 0 {
			changed = append(changed, s.Name)
		}
	}
	disabled, err := disable(ctx, tx, t.ID, t.Slug, names)
	if err != nil {
		return err
	}
	sort.Strings(changed)
	t.Enabled, t.Disabled = changed, disabled
	for _, n := range changed {
		if err := notify(ctx, tx, pg.ChannelServices, t.Slug+"/"+n); err != nil {
			return err
		}
	}
	return nil
}

// disable disables the tenant's services not in keep and revokes their bindings, with NOTIFY kontra_services and
// kontra_revoke 'binding:…' (every replica answers 404 and stops streams for them). It returns the services it
// disabled, sorted.
func disable(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string, keep []string) ([]string, error) {
	if keep == nil {
		keep = []string{}
	}
	rows, err := tx.Query(ctx, `update kontra.services set disabled_at = now(), updated_at = now()
		where tenant_id = $1 and disabled_at is null and not (name = any($2::text[])) returning name`, tenantID, keep)
	if err != nil {
		return nil, err
	}
	disabled, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `update kontra.key_services set revoked_at = now()
		where tenant_id = $1 and revoked_at is null and not (service = any($2::text[])) returning key_id, service`, tenantID, keep)
	if err != nil {
		return nil, err
	}
	type binding struct{ key, service string }
	revoked, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (binding, error) {
		var b binding
		return b, r.Scan(&b.key, &b.service)
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(disabled)
	for _, n := range disabled {
		if err := notify(ctx, tx, pg.ChannelServices, slug+"/"+n); err != nil {
			return nil, err
		}
	}
	for _, b := range revoked {
		if err := notify(ctx, tx, pg.ChannelRevoke, BindingRevokePayload(b.key, b.service)); err != nil {
			return nil, err
		}
	}
	return disabled, nil
}

func notify(ctx context.Context, tx pgx.Tx, ch pg.Channel, payload string) error {
	_, err := tx.Exec(ctx, `select pg_notify($1, $2)`, string(ch), payload)
	return err
}

// BindingRevokePayload is NOTIFY kontra_revoke's payload for one binding, "binding:<key_id>/<service>"
// (enroll.BindingRevokePayload's; internal/enroll imports this package, so the format is repeated here and a test
// keeps the two equal).
func BindingRevokePayload(keyID, service string) string { return "binding:" + keyID + "/" + service }

// expiredPrefix starts NOTIFY kontra_tunnel's gate-internal payload for an anonymous tenant that ended.
const expiredPrefix = "expired:"

// ExpiredPayload is NOTIFY kontra_tunnel's payload (pg.ChannelTunnel) for an anonymous tenant that ended, at its
// expiry or by a conversion: "expired:<tenant_id>". The replica holding a session of the tenant sends it {"t":
// "revoked"} and closes it (tunnelgate.Registry.Notify). Like the channel's other payloads it is Postgres between
// replicas, not tunnel wire protocol.
func ExpiredPayload(tenantID uuid.UUID) string { return expiredPrefix + tenantID.String() }

// ParseExpiredPayload reads an ExpiredPayload; ok is false for anything else.
func ParseExpiredPayload(payload string) (uuid.UUID, bool) {
	rest, ok := strings.CutPrefix(payload, expiredPrefix)
	if !ok {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(rest)
	if err != nil || len(rest) != 36 {
		return uuid.Nil, false
	}
	return id, true
}

// Audit events.
const (
	EventExpired = "tunnel.anonymous_expired"
	EventPurged  = "tunnel.anonymous_purged"
)

// End ends an anonymous tenant in tx (§6.1): its services disabled, its bindings revoked (NOTIFY kontra_services and
// kontra_revoke), expires_at brought to now when it is later (a conversion ends it at once), and NOTIFY kontra_tunnel
// ExpiredPayload so that the replica holding its session sends {"t": "revoked"} and closes it. The keys are not
// revoked: a key's next hello opens a new tenant. why is the audit row's reason; it returns the services disabled.
func End(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, why string) ([]string, error) {
	var slug string
	if err := tx.QueryRow(ctx, `update kontra.tenants set expires_at = least(expires_at, now())
		where id = $1 and kind = 'anonymous' returning slug`, tenantID).Scan(&slug); err != nil {
		return nil, fmt.Errorf("anon: end tenant %s: %w", tenantID, err)
	}
	// The session's end first: NOTIFY delivers a transaction's payloads in the order sent, so the replica holding the
	// session ends it before the bindings' revocations reach it (one {"t": "revoked"}, not one per service).
	if err := notify(ctx, tx, pg.ChannelTunnel, ExpiredPayload(tenantID)); err != nil {
		return nil, err
	}
	disabled, err := disable(ctx, tx, tenantID, slug, nil)
	if err != nil {
		return nil, err
	}
	services := make([]any, len(disabled))
	for i, n := range disabled {
		services[i] = n
	}
	if _, err := audit.Append(ctx, tx, EventExpired, audit.Fields{
		"actor": "gate", "tenant": slug, "tenant_id": tenantID, "services": services, "reason": why,
	}); err != nil {
		return nil, err
	}
	return disabled, nil
}

// ObjectDeleter removes every bucket object of a task (blob.Store.DeleteTask, store.ObjectDeleter).
type ObjectDeleter interface {
	DeleteTask(ctx context.Context, tenant, task uuid.UUID) error
}

// Sweeper ends anonymous tenants: Expire at expires_at, Purge purge_after later. Every replica runs it; each tenant is
// taken by one pass at a time (Expire: FOR UPDATE SKIP LOCKED; Purge: a claim, then the same).
//
// Lock order. A transaction that takes a key and its anonymous tenant takes the key's row first, then the tenant's,
// before anything else (internal/enroll's admission). The sweeps take a tenant's row with SKIP LOCKED, and Purge takes
// the tenant's keys with NOWAIT, leaving a tenant whose key an admission holds to its next pass: so an admission and a
// sweep never wait on each other in a cycle (the M4 WP-03 review's 40P01s).
type Sweeper struct {
	db      *pgxpool.Pool
	lim     Limits
	objects ObjectDeleter // nil without a bucket
	log     *slog.Logger
}

// NewSweeper sweeps db with lim; objects deletes a purged tenant's bucket objects (nil without a bucket).
func NewSweeper(db *pgxpool.Pool, lim Limits, objects ObjectDeleter, log *slog.Logger) *Sweeper {
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{db: db, lim: lim.withDefaults(), objects: objects, log: log}
}

// sweepBatch bounds the tenants one Expire or Purge transaction takes.
const sweepBatch = 100

// sweepTimeout bounds one pass's statements.
const sweepTimeout = 30 * time.Second

// objectDeleteTimeout bounds the deletion of one task's objects (store.ObjectDeleteTimeout's); tests shorten it.
var objectDeleteTimeout = time.Minute

// purgeDeleters is how many tasks' objects a purge deletes at once (the retention sweep's objectDeleters).
const purgeDeleters = 8

// purgeBudget bounds one Purge pass: the tenants left when it is spent are the next pass's (tests shorten it).
var purgeBudget = 5 * time.Minute

// errObjects is a purge that the bucket stopped: the tenant keeps its rows, and the pass stops (the bucket is likely
// down; the next pass tries again).
var errObjects = errors.New("the bucket would not delete the objects")

// purgeLockSpace is the high half of a purge's claim on a tenant, a session advisory lock: "ktap", then a 32-bit
// FNV-1a hash of the tenant id (two tenants with one hash are only purged one after the other). It is far from openLock
// ("ktan"), the enrollments' "ktsv" and the gate's "kontra" keys.
const purgeLockSpace int64 = 0x6b746170 << 32

func purgeLock(tenantID uuid.UUID) int64 {
	h := fnv.New32a()
	h.Write(tenantID[:])
	return purgeLockSpace | int64(h.Sum32())
}

// Expire ends the tenants whose expires_at has passed and that still have a service not disabled or a binding not
// revoked (End, with reason "expired"), and returns how many it ended.
func (s *Sweeper) Expire(ctx context.Context) (int, error) {
	total := 0
	for {
		n := 0
		err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `select set_config('statement_timeout', $1, true)`, fmt.Sprint(sweepTimeout.Milliseconds())); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `select t.id from kontra.tenants t
				where t.kind = 'anonymous' and t.deleted_at is null and t.expires_at <= now()
				  and (exists (select 1 from kontra.services s where s.tenant_id = t.id and s.disabled_at is null)
				       or exists (select 1 from kontra.key_services ks where ks.tenant_id = t.id and ks.revoked_at is null))
				order by t.expires_at limit $1 for update of t skip locked`, sweepBatch)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			if err != nil {
				return err
			}
			for _, id := range ids {
				if _, err := End(ctx, tx, id, "expired"); err != nil {
					return err
				}
			}
			n = len(ids)
			return nil
		})
		if err != nil {
			return total, fmt.Errorf("anon: expire: %w", err)
		}
		total += n
		if n < sweepBatch {
			return total, nil
		}
	}
}

// Purge deletes what the tenants expired purge_after ago still hold (§6.1, DM4-20): for each, the bucket objects of
// every one of its tasks first, by prefix, whether or not the task has a blobs row (D167: an object stored for an event
// that was then refused has none), then in one transaction the keys still in it (their bindings and tunnels go with
// them), its MCP sessions (the gate's own /mcp sessions, service NULL, included), its calls, tasks, services, enrollment
// tokens and in-flight call rows, and sets tenants.deleted_at: the row stays as the tombstone, with created_from. It
// returns how many tenants it purged.
//
// A pass is bounded: it takes up to sweepBatch tenants, claims each before touching it (a session advisory lock, so
// replicas never purge one tenant together; one another replica holds is skipped), deletes a tenant's objects
// purgeDeleters tasks at once, each within objectDeleteTimeout, stops at the first bucket failure (that tenant keeps
// everything) and ends when purgeBudget is spent; what is left is the next pass's. A tenant one of whose keys an
// admission holds is left to the next pass too (NOWAIT, see Sweeper).
func (s *Sweeper) Purge(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, purgeBudget)
	defer cancel()
	rows, err := s.db.Query(ctx, `select id from kontra.tenants
		where kind = 'anonymous' and deleted_at is null and expires_at <= now() - make_interval(secs => $1)
		order by expires_at limit $2`, s.lim.PurgeAfter.Seconds(), sweepBatch)
	if err != nil {
		return 0, fmt.Errorf("anon: purge: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, fmt.Errorf("anon: purge: %w", err)
	}
	purged := 0
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			errs = append(errs, fmt.Errorf("anon: purge: the pass ended at its budget (%s); the next pass goes on", purgeBudget))
			break
		}
		ok, err := s.purge(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("anon: purge tenant %s: %w", id, err))
			if errors.Is(err, errObjects) {
				break
			}
			continue
		}
		if ok {
			purged++
		}
	}
	return purged, errors.Join(errs...)
}

// purge purges one tenant under its claim, and reports whether it did (false: another pass holds it, an admission
// holds one of its keys, or it is no longer due).
func (s *Sweeper) purge(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	claim := purgeLock(tenantID)
	var claimed bool
	if err := conn.QueryRow(ctx, `select pg_try_advisory_lock($1)`, claim).Scan(&claimed); err != nil {
		return false, err
	}
	if !claimed {
		return false, nil // another replica's pass is purging it
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
		defer cancel()
		var released bool
		if err := conn.QueryRow(uctx, `select pg_advisory_unlock($1)`, claim).Scan(&released); err != nil || !released {
			// A claim that could not be released goes with its connection (Release then discards it).
			_ = conn.Conn().Close(uctx)
		}
	}()
	due := `select slug, exists (select 1 from kontra.services where tenant_id = t.id and disabled_at is null)
		from kontra.tenants t where id = $1 and kind = 'anonymous' and deleted_at is null
		  and expires_at <= now() - make_interval(secs => $2)`
	var (
		slug    string
		running bool
	)
	if err := conn.QueryRow(ctx, due, tenantID, s.lim.PurgeAfter.Seconds()).Scan(&slug, &running); errors.Is(err, pgx.ErrNoRows) {
		return false, nil // purged by the pass that held the claim before
	} else if err != nil {
		return false, err
	}
	// The objects first (spec §5.4, D167: objects go before their rows; a tenant whose objects remain keeps its rows).
	if s.objects != nil {
		if err := s.deleteObjects(ctx, conn, tenantID); err != nil {
			return false, err
		}
	}
	purged := false
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select set_config('statement_timeout', $1, true)`, fmt.Sprint(sweepTimeout.Milliseconds())); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, due+` for update skip locked`, tenantID, s.lim.PurgeAfter.Seconds()).Scan(&slug, &running)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // an admission of one of its keys holds it, or it is done
		}
		if err != nil {
			return err
		}
		// Its keys next, without waiting: an admission holding one took it before the tenant (the lock order above).
		if _, err := tx.Exec(ctx, `select 1 from kontra.tunnel_keys where tenant_id = $1 for update nowait`, tenantID); err != nil {
			return err
		}
		if running {
			// Expire has not run on it (a gate that was down at its expiry): end it now, so its sessions close.
			if _, err := End(ctx, tx, tenantID, "expired"); err != nil {
				return err
			}
		}
		var keys int64
		for _, q := range []struct {
			sql string
			n   *int64
		}{
			{`delete from kontra.tunnel_keys where tenant_id = $1`, &keys}, // key_services and their tunnels cascade
			{`delete from kontra.tunnels where tenant_id = $1`, nil},       // a moved key's session row of this tenant
			{`delete from kontra.mcp_sessions where tenant_id = $1`, nil},  // service NULL included (no foreign key)
			{`delete from kontra.calls where tenant_id = $1`, nil},         // relay.md §13.5's exception (DM4-20)
			{`delete from kontra.tasks where tenant_id = $1`, nil},         // events, attempts, work items, push configs, blobs
			{`delete from kontra.anon_calls where tenant_id = $1`, nil},
			{`delete from kontra.enrollment_tokens where tenant_id = $1`, nil},
			{`delete from kontra.services where tenant_id = $1`, nil},
			{`delete from kontra.api_keys where tenant_id = $1`, nil},
		} {
			tag, err := tx.Exec(ctx, q.sql, tenantID)
			if err != nil {
				return fmt.Errorf("%s: %w", q.sql, err)
			}
			if q.n != nil {
				*q.n = tag.RowsAffected()
			}
		}
		if _, err := tx.Exec(ctx, `update kontra.tenants set deleted_at = now() where id = $1`, tenantID); err != nil {
			return err
		}
		if _, err := audit.Append(ctx, tx, EventPurged, audit.Fields{
			"actor": "gate", "tenant": slug, "tenant_id": tenantID, "keys": keys,
		}); err != nil {
			return err
		}
		purged = true
		return nil
	})
	if isLockNotAvailable(err) {
		s.log.Debug("anonymous purge: an admission holds a key of the tenant; the next pass purges it", "tenant_id", tenantID)
		return false, nil
	}
	return purged, err
}

// deleteObjects deletes the bucket objects of every task of the tenant (DeleteTask, by prefix), purgeDeleters at once,
// each within objectDeleteTimeout. The first failure stops the others and is returned wrapping errObjects.
func (s *Sweeper) deleteObjects(ctx context.Context, conn *pgxpool.Conn, tenantID uuid.UUID) error {
	rows, err := conn.Query(ctx, `select id from kontra.tasks where tenant_id = $1`, tenantID)
	if err != nil {
		return err
	}
	tasks, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	dctx, stop := context.WithCancel(ctx)
	defer stop()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	fail := func(err error) {
		mu.Lock()
		if first == nil {
			first = err
		}
		mu.Unlock()
		stop()
	}
	sem := make(chan struct{}, purgeDeleters)
launch:
	for _, task := range tasks {
		select {
		case sem <- struct{}{}:
		case <-dctx.Done():
			break launch
		}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			tctx, cancel := context.WithTimeout(dctx, objectDeleteTimeout)
			defer cancel()
			if err := s.objects.DeleteTask(tctx, tenantID, task); err != nil {
				fail(fmt.Errorf("%w of task %s: %w", errObjects, task, err))
			}
		}()
	}
	wg.Wait()
	if first == nil && dctx.Err() != nil {
		first = fmt.Errorf("%w: %w", errObjects, context.Cause(dctx))
	}
	return first
}

// isLockNotAvailable reports Postgres's lock_not_available (SQLSTATE 55P03: a NOWAIT lock, or lock_timeout).
func isLockNotAvailable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "55P03"
}

// Role is how a new task counts against an anonymous tenant (§6.2).
type Role int

// Roles.
const (
	AsHost  Role = iota + 1 // the task's service is the tenant's
	AsHirer                 // the task's principal is a key of the tenant
)

// Caps checks the runtime limits of anonymous tenants (§6.2). A nil *Caps checks nothing (a gate without
// tunnel.anonymous).
type Caps struct {
	db      *pgxpool.Pool
	lim     Limits
	replica string
	log     *slog.Logger

	mu     sync.Mutex
	leaked []uuid.UUID // in-flight call rows whose delete failed, deleted again at the next Call
}

// NewCaps checks against lim; replica is this replica's id (anon_calls.replica_id: the rows of a replica that dies
// go with its gate_replicas row).
func NewCaps(db *pgxpool.Pool, lim Limits, replica string, log *slog.Logger) *Caps {
	if log == nil {
		log = slog.Default()
	}
	return &Caps{db: db, lim: lim.withDefaults(), replica: replica, log: log}
}

// Limits are the limits c checks.
func (c *Caps) Limits() Limits { return c.lim }

// Task checks, in tx (the transaction that creates the task, before its row) and under a lock on each anonymous
// tenant involved, that the tenant has fewer than Tasks non-terminal tasks in each role it has here: as host when the
// service's tenant (svcTenant, its slug svcSlug) is anonymous, as hirer when principal is node:<key_id> of a key of an
// anonymous tenant. ErrTooManyTasks otherwise. Two creators of one tenant serialize on its row (FOR NO KEY UPDATE, so
// the inserts of its services, which take KEY SHARE, never wait on it), so they never both pass at Tasks-1.
func (c *Caps) Task(ctx context.Context, tx pgx.Tx, svcTenant uuid.UUID, svcSlug, principal string) error {
	if c == nil {
		return nil
	}
	type check struct {
		tenant uuid.UUID
		role   Role
	}
	var checks []check
	if IsSlug(svcSlug) {
		checks = append(checks, check{svcTenant, AsHost})
	}
	if keyID, ok := strings.CutPrefix(principal, "node:"); ok {
		var hirer uuid.UUID
		err := tx.QueryRow(ctx, `select k.tenant_id from kontra.tunnel_keys k join kontra.tenants t on t.id = k.tenant_id
			where k.key_id = $1 and t.kind = 'anonymous'`, keyID).Scan(&hirer)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			checks = append(checks, check{hirer, AsHirer})
		}
	}
	if len(checks) == 0 {
		return nil
	}
	// Locks in id order: a creator hiring one anonymous tenant's service from another's key never deadlocks with one
	// going the other way.
	tenants := make([]uuid.UUID, 0, len(checks))
	for _, ch := range checks {
		if !slices.Contains(tenants, ch.tenant) {
			tenants = append(tenants, ch.tenant)
		}
	}
	slices.SortFunc(tenants, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	for _, t := range tenants {
		if _, err := tx.Exec(ctx, `select 1 from kontra.tenants where id = $1 for no key update`, t); err != nil {
			return err
		}
	}
	for _, ch := range checks {
		var n int
		var err error
		if ch.role == AsHost {
			err = tx.QueryRow(ctx, `select count(*) from kontra.tasks where tenant_id = $1 and terminal_at is null`, ch.tenant).Scan(&n)
		} else {
			err = tx.QueryRow(ctx, `select count(*) from kontra.tasks where terminal_at is null
				and principal = any(array(select 'node:' || key_id from kontra.tunnel_keys where tenant_id = $1))`, ch.tenant).Scan(&n)
		}
		if err != nil {
			return err
		}
		if n >= c.lim.Tasks {
			role := "host"
			if ch.role == AsHirer {
				role = "hirer"
			}
			return fmt.Errorf("%w (%d as %s, tasks %d)", ErrTooManyTasks, n, role, c.lim.Tasks)
		}
	}
	return nil
}

// callTimeout bounds Call's statements, and the delete that releases a slot.
const callTimeout = 5 * time.Second

// Call takes one of the Calls slots of an anonymous service's tenant for a plain MCP call (§6.2, DM4-20), counted
// gate-wide in anon_calls under a lock on the tenant's row, and returns its release, which the caller runs when the
// call's answer is relayed. A service of a tenant that is not anonymous takes no slot. ErrTooManyCalls when Calls are
// in flight.
func (c *Caps) Call(ctx context.Context, svcTenant uuid.UUID, svcSlug string) (release func(), err error) {
	if c == nil || !IsSlug(svcSlug) {
		return func() {}, nil
	}
	c.retryLeaked(ctx)
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var taken bool
	err = pgx.BeginFunc(cctx, c.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(cctx, `select 1 from kontra.tenants where id = $1 for no key update`, svcTenant); err != nil {
			return err
		}
		tag, err := tx.Exec(cctx, `insert into kontra.anon_calls (id, tenant_id, replica_id)
			select $1, $2, $3 where (select count(*) from kontra.anon_calls where tenant_id = $2) < $4`,
			id, svcTenant, c.replica, c.lim.Calls)
		taken = err == nil && tag.RowsAffected() == 1
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("anon: take a call slot: %w", err)
	}
	if !taken {
		return nil, fmt.Errorf("%w (calls %d)", ErrTooManyCalls, c.lim.Calls)
	}
	var once sync.Once
	return func() { once.Do(func() { c.release(ctx, id) }) }, nil
}

func (c *Caps) release(ctx context.Context, id uuid.UUID) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
	defer cancel()
	if _, err := c.db.Exec(dctx, `delete from kontra.anon_calls where id = $1`, id); err != nil {
		c.log.Warn("anonymous call slot not released; retried at the next call", "error", err)
		c.mu.Lock()
		c.leaked = append(c.leaked, id)
		c.mu.Unlock()
	}
}

func (c *Caps) retryLeaked(ctx context.Context) {
	c.mu.Lock()
	ids := c.leaked
	c.leaked = nil
	c.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
	defer cancel()
	if _, err := c.db.Exec(dctx, `delete from kontra.anon_calls where id = any($1::uuid[])`, ids); err != nil {
		c.mu.Lock()
		c.leaked = append(c.leaked, ids...)
		c.mu.Unlock()
	}
}

// Stored is what the tenant's tasks store (relay.md §6.6's accounting, D103: each event's payload encoding, plus the
// tasks' bucket objects), less the task except (uuid.Nil: none).
func (c *Caps) Stored(ctx context.Context, tenantID, except uuid.UUID) (int64, error) {
	var n int64
	err := c.db.QueryRow(ctx, `select
		coalesce((select sum(octet_length(e.payload::text)) from kontra.task_events e join kontra.tasks t on t.id = e.task_id
		          where t.tenant_id = $1 and t.id <> $2), 0)
		+ coalesce((select sum(size) from kontra.blobs where tenant_id = $1 and task_id <> $2), 0)`, tenantID, except).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("anon: the stored bytes of tenant %s: %w", tenantID, err)
	}
	return n, nil
}

// Message checks a caller's message of n bytes to a service of tenant svcTenant (slug svcSlug) against parts_bytes
// before any task row (§6.2, DM4-20): ErrStorageFull when it would take what the tenant stores past it. A service of a
// tenant that is not anonymous passes.
func (c *Caps) Message(ctx context.Context, svcTenant uuid.UUID, svcSlug string, n int64) error {
	if c == nil || !IsSlug(svcSlug) {
		return nil
	}
	used, err := c.Stored(ctx, svcTenant, uuid.Nil)
	if err != nil {
		return err
	}
	if used+n > c.lim.PartsBytes {
		return fmt.Errorf("%w (%d bytes stored, %d more, parts_bytes %d)", ErrStorageFull, used, n, c.lim.PartsBytes)
	}
	return nil
}

// Budget is what a dispatch of task (a task of tenant svcTenant, slug svcSlug, read when "") counts against relay.md
// §6.6's limits:
// others, the bytes the tenant's other tasks store, and limit, the most the dispatch may count once others are added
// to its own (min(taskBytes + others, parts_bytes)). applies is false for a service of a tenant that is not anonymous,
// whose dispatch counts its own bytes against taskBytes alone. Upstream bytes past the limit fail the task
// upstream_error (§6.6).
//
// SPEC-CONFLICT (spec/tunnel.md §6.2, relay.md §6.6: parts_bytes "in all", DM4-20): an exact total across the
// tenant's tasks would read the other tasks' bytes, under a lock, at every event each dispatch logs. The closest
// faithful behaviour: a dispatch counts the others' bytes as they stood when it started, so the tasks of one tenant
// dispatched together may store up to tasks × the room left between them (with §6.2's defaults, 4 × 64 MiB, the
// default task_bytes); each event of each is still bounded, a caller's message is checked against the total before
// any task row (Message), and every later dispatch starts from the total. Proposal (DM4-25): keep it so, or re-read
// the total at each externalized part only.
func (c *Caps) Budget(ctx context.Context, svcTenant uuid.UUID, svcSlug string, task uuid.UUID, taskBytes int64) (others, limit int64, applies bool, err error) {
	if c == nil {
		return 0, taskBytes, false, nil
	}
	if svcSlug == "" {
		// A dispatch that does not hold the slug (an MCP task's row names the tenant by id).
		if err := c.db.QueryRow(ctx, `select slug from kontra.tenants where id = $1`, svcTenant).Scan(&svcSlug); err != nil {
			return 0, 0, false, fmt.Errorf("anon: the tenant of a task: %w", err)
		}
	}
	if !IsSlug(svcSlug) {
		return 0, taskBytes, false, nil
	}
	others, err = c.Stored(ctx, svcTenant, task)
	if err != nil {
		return 0, 0, false, err
	}
	return others, min(taskBytes+others, c.lim.PartsBytes), true, nil
}

// EventConverted is the audit row of a conversion: a key of a live anonymous tenant moved into an account (or the
// enrollment tokens' tenant), and the anonymous tenant ended at once (spec/tunnel.md §6.3, DM4-11, DM4-19).
const EventConverted = "tunnel.anonymous_converted"

// Conversion authorities (ConvertRequest.Via).
const (
	ViaAPIKey = "api_key"          // §6.3: the hello's auth, an API key with enroll:* (or one admitted hire-only)
	ViaTokens = "enrollment_token" // DM4-19: the hello's enroll tokens
)

// ConvertRequest is a handshake that converted a key of a live anonymous tenant (§6.3, DM4-19): the key, the
// anonymous tenant it left, the tenant it moved into, by which authority, and the services it is bound to there.
type ConvertRequest struct {
	KeyID    string
	From     uuid.UUID // the live anonymous tenant; the admission holds its row (FOR NO KEY UPDATE)
	To       uuid.UUID
	ToSlug   string
	Via      string   // ViaAPIKey or ViaTokens
	APIKey   string   // the auth API key's principal (key:<prefix>), when the hello carried one
	Services []string // the services the key serves in To after the handshake, sorted
}

// Convert ends the anonymous tenant a converted key left and records the conversion, in tx (the admission's
// transaction, which holds the key's row and then the anonymous tenant's, in that order, and has already moved the key
// with MoveKey and bound it in To as §5 binds a key: the services the anonymous session served created in the account
// where missing, with allow [org:<account>], another kind kind_mismatch with nothing moved; internal/enroll). The tenant ends at once (End,
// reason "converted": its services disabled and 404, their tasks in flight failing service_unavailable, its session
// told revoked), and an EventConverted row names both tenants. Nothing of another anonymous tenant is locked.
func Convert(ctx context.Context, tx pgx.Tx, req ConvertRequest) error {
	var in uuid.UUID
	if err := tx.QueryRow(ctx, `select tenant_id from kontra.tunnel_keys where key_id = $1`, req.KeyID).Scan(&in); err != nil {
		return fmt.Errorf("anon: convert %s: %w", req.KeyID, err)
	}
	if in != req.To || req.To == req.From {
		return fmt.Errorf("anon: convert %s: the key is not in the tenant it converts into", req.KeyID)
	}
	var (
		from string
		kind string
	)
	if err := tx.QueryRow(ctx, `select slug, kind from kontra.tenants where id = $1`, req.From).Scan(&from, &kind); err != nil {
		return fmt.Errorf("anon: convert %s: %w", req.KeyID, err)
	}
	if kind != "anonymous" {
		return fmt.Errorf("anon: convert %s: tenant %s is not anonymous", req.KeyID, from)
	}
	disabled, err := End(ctx, tx, req.From, "converted")
	if err != nil {
		return err
	}
	services := make([]any, len(req.Services))
	for i, n := range req.Services {
		services[i] = n
	}
	f := audit.Fields{
		"actor": "tunnel handshake", "key_id": req.KeyID, "from": from, "from_id": req.From, "to": req.ToSlug,
		"to_id": req.To, "via": req.Via, "services": services, "disabled": anyOf(disabled),
	}
	if req.APIKey != "" {
		f["api_key"] = req.APIKey
	}
	_, err = audit.Append(ctx, tx, EventConverted, f)
	return err
}

func anyOf(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// IsUniqueViolation reports a Postgres unique violation (SQLSTATE 23505).
func IsUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}
