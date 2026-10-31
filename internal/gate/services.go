package gate

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/a2ahttp"
	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/card"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcprelay"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/relay"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/upstream"
)

// TeamTenant is the slug of a team gate's one tenant.
const TeamTenant = tenant.TeamSlug

// Audit events of the service registry (the audit gap's WP-08 half: "key and service changes",
// migrations/0001_init.sql). A row names the tenant and the service and what changed; never a secret.
const (
	EventServiceUpserted = "service.upserted"
	EventServiceDisabled = "service.disabled"
)

// syncLockID serializes Sync across replicas that start, or are sent SIGHUP, together: "kontra" in ASCII,
// then 0x0101, next to the audit chain's 0x0100 (internal/audit). Sync takes it before the audit lock.
const syncLockID int64 = 0x6b6f6e7472610101

// RefreshEvery is how often a replica re-reads every service row besides NOTIFY kontra_services and the
// LISTEN connection's reconnects: a safety net for a missed notification (the table is the truth).
const RefreshEvery = time.Minute

// Services is the replica's service registry (spec §1, §2.1, §13.5): every service row of every live
// tenant, read from Postgres, with a card.Keeper for each enabled a2a service, direct or tunnel.
//
// A team gate upserts its tenant (tenant.EnsureTeam, the code `kontra-gate enroll --api-key` runs too) and
// the services: block into tenants and services (source = config) at start and on SIGHUP (Sync), marks the
// config services no longer in the file disabled_at (rows are not deleted: tasks reference them), appends an
// audit row for every service it creates, changes or disables, and sends NOTIFY kontra_services
// '<tenant>/<name>' for each. Every replica re-reads a service on that NOTIFY, every service when its
// LISTEN connection reconnects and every RefreshEvery, so db-sourced services (Cloud, spec §13.5) and other
// replicas' syncs apply without restart.
//
// Cards (spec §3, the audit's card lifecycle): each replica runs its own keeper per enabled direct a2a
// service, seeded at start from services.upstream_card so a restarted replica serves the last good card at
// once. A keeper fetches when the service is configured, retries a failed fetch with backoff, refetches
// every 10 minutes, keeps the last good card when a fetch fails, and marks the service unavailable after
// 30 minutes without a successful fetch: card routes answer 503 and dispatch fails service_unavailable.
// A keeper records the card it serves after a good fetch while that card is unrecorded (its first good
// fetch, new upstream bytes or a new served body, and after a failed write; persistCard): the upstream
// bytes to services.upstream_card with card_fetched_at, then the served card to services.card (json, D49),
// a best-effort copy left NULL when Postgres refuses it. Every successful fetch of the stored bytes moves
// card_fetched_at, so the seed of a restarted replica counts the 30 minutes from the gate's last good
// fetch. Replicas that fetched the same bytes serve the same body and ETag (the gate's signature is
// deterministic). card_hash is the SHA-256 over the JCS of the upstream card (D166).
//
// Signing and seller keys (spec §3, D13, D166; M3 WP-02): every served card is signed with the gate's key
// (ServicesOptions.Signer), its protected header binding the upstream card. A gate without a signer serves an
// a2a row only when its public_url is a loopback origin (a test registry); off loopback the row is left out, as
// the configuration refuses such a file (D166). seller_keys on an a2a service are verified on every fetch: a
// card no configured key verifies makes the service unavailable, and is never recorded (card.Keeper).
//
// Tunnel cards (spec §3, spec/tunnel.md; M2): every replica keeps a keeper per enabled tunnel a2a service,
// seeded from services.upstream_card. Its card is fetched through one of the replica's own sessions serving
// the service (TunnelCards.FetchCard): when a session for it connects, before that session becomes a
// candidate (TunnelCards.OnConnect), and every 10 minutes while the replica serves it. A replica without a
// session serves the stored card: a changed card is recorded with NOTIFY kontra_services, on which every
// replica re-reads the row and seeds its keeper with it, and every re-read seeds a newer card_fetched_at, so
// the 30 minutes without a good fetch count from the gate's last fetch through any session (D89). A
// service none of whose nodes has connected for 30 minutes is unavailable like a direct service whose
// fetches fail. A tunnel attempt's dispatch and forward_cancel run over its session on the replica that
// holds it (Backend, TunnelSessions), with the Info of this one card.
type Services struct {
	db        *pgxpool.Pool
	logger    *slog.Logger
	publicURL string
	multi     bool
	keeperOpt []card.KeeperOption
	onChange  func(store.ServiceRef)
	tunnels   TunnelCards
	sessions  TunnelSessions
	cardEvery time.Duration // a tunnel card's refetch while serving (card.Refresh)
	signer    card.Signer   // nil: cards are not signed (loopback test registries only)
	loopback  bool          // public_url is a loopback origin
	keyClient *http.Client  // fetches seller_keys {url} sets: the SSRF-safe client (spec §10.3)

	// readMu makes each re-read (its query and its apply) one step, so a read that began before a change
	// committed is never applied after one that began after it.
	readMu sync.Mutex

	mu      sync.RWMutex
	byName  map[string]*entry // "<tenant slug>/<name>"
	byID    map[svcID]*entry
	running context.Context // set while Run runs: keepers are started under it
}

var (
	_ a2ahttp.Services          = (*Services)(nil)
	_ mcprelay.Services         = (*Services)(nil)
	_ lifecycle.BackendResolver = (*Services)(nil)
	_ relay.Backends            = (*Services)(nil)
)

type svcID struct {
	tenant uuid.UUID
	name   string
}

// entry is one service as the replica serves it.
type entry struct {
	ref       store.ServiceRef
	allow     auth.AllowList
	allowRaw  []string // services.allow as read
	listed    bool
	disabled  bool
	backend   backendSpec
	backendJS string // services.backend as read: the persist and touch guards compare it
	served    *a2ahttp.Service
	mcp       *mcprelay.Service // a kind: mcp service as the MCP relay serves it
	mcpKey    string            // what mcp was built from: a change is announced (OnChange)
	keeper    *card.Keeper
	keeperKey string             // what the keeper was built from: rebuilt when it changes
	stop      context.CancelFunc // the keeper's Run, nil when not started
	tunnel    *tunnelCard        // a tunnel service's keeper state, nil for a direct one
}

// tunnelCard is what the registry keeps beside a tunnel service's keeper.
type tunnelCard struct {
	mu     sync.Mutex
	lastAt time.Time     // the newest card this replica fetched or seeded
	kick   chan struct{} // a fetch at a session's connect failed: retry on the backoff schedule
}

func newTunnelCard() *tunnelCard { return &tunnelCard{kick: make(chan struct{}, 1)} }

// newer records at as the newest card's time when it is newer than the last one, and reports whether it was.
func (c *tunnelCard) newer(at time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !at.After(c.lastAt) {
		return false
	}
	c.lastAt = at
	return true
}

// TunnelCards is how the card keepers of tunnel services reach the nodes: through this replica's own
// sessions (*tunnelgate.Registry).
type TunnelCards interface {
	// FetchCard fetches the service's upstream card through one of this replica's sessions serving it; an
	// error wrapping tunnelgate.ErrSessionGone when it holds none.
	FetchCard(ctx context.Context, tenantID uuid.UUID, service string) (*upstream.Fetched, error)
	// OnConnect registers fn for each a2a service of a session that connects, before the session becomes
	// a candidate for dispatch.
	OnConnect(fn func(ctx context.Context, tenantID uuid.UUID, service string))
	// Serves reports whether this replica holds a session serving the a2a service.
	Serves(tenantID uuid.UUID, service string) bool
}

// TunnelSessions is how a tunnel attempt reaches its node: the backend over the attempt's session on this
// replica (*tunnelgate.Registry), an error wrapping upstream.ErrTunnelLost when this replica does not hold
// it or it no longer serves the service (spec §6.1, §14.2).
type TunnelSessions interface {
	Backend(session uuid.UUID, svc store.ServiceRef) (upstream.Backend, error)
}

// backendSpec is services.backend: {"tunnel": {}} | {"a2a": {"url", "allow_private"}} | {"mcp": {…}}.
type backendSpec struct {
	Kind         string // tunnel, a2a or mcp
	URL          string
	AllowPrivate bool
}

func (b backendSpec) direct() bool { return b.Kind == config.KindA2A || b.Kind == config.KindMCP }

// backendTunnel is services.backend's key of a tunnel service.
const backendTunnel = "tunnel"

func parseBackend(js []byte) (backendSpec, error) {
	var m map[string]struct {
		URL          string `json:"url"`
		AllowPrivate bool   `json:"allow_private"`
	}
	if err := json.Unmarshal(js, &m); err != nil || len(m) != 1 {
		return backendSpec{}, fmt.Errorf("services.backend %s: want one of tunnel, a2a or mcp", js)
	}
	for k, v := range m {
		switch k {
		case backendTunnel:
			return backendSpec{Kind: k}, nil
		case config.KindA2A, config.KindMCP:
			if v.URL == "" {
				return backendSpec{}, fmt.Errorf("services.backend %s: no url", k)
			}
			return backendSpec{Kind: k, URL: v.URL, AllowPrivate: v.AllowPrivate}, nil
		}
		return backendSpec{}, fmt.Errorf("services.backend: unknown kind %q", k)
	}
	return backendSpec{}, errors.New("services.backend: empty")
}

// ServicesOptions configure a registry.
type ServicesOptions struct {
	PublicURL string // {public_url} of the served cards
	Multi     bool   // /agents/{tenant}/{name} paths
	Logger    *slog.Logger
	// OnChange is called, outside the registry's lock, for a service whose policy, allow: list, card source
	// or state changed, and for one that went away: the relay's handler cache drops it (relay.Handlers.
	// Invalidate), so the next request builds it again with the service's current policy.
	OnChange func(store.ServiceRef)
	// KeeperOptions are added to every card keeper (tests: the clock, the fetch timeout).
	KeeperOptions []card.KeeperOption
	// Tunnels serves the cards of tunnel services (*tunnelgate.Registry). Without it a tunnel service has
	// no card keeper, as in M1.
	Tunnels TunnelCards
	// Sessions gives a tunnel attempt its backend (*tunnelgate.Registry). Without it a tunnel attempt's
	// dispatch fails service_unavailable.
	Sessions TunnelSessions
	// TunnelCardRefresh replaces card.Refresh, a tunnel card's refetch interval while serving (tests).
	TunnelCardRefresh time.Duration
	// Signer signs every served card (card.JWSSigner over the gate's signing key, D166). Nil only in tests: a
	// registry without one leaves every a2a row out unless PublicURL is a loopback origin.
	Signer card.Signer
	// SellerKeysClient fetches seller_keys {url} sets; nil takes the SSRF-safe client with the address checks on
	// (spec §3, §10.3: never allow_private).
	SellerKeysClient *http.Client
}

// NewServices returns an empty registry over db; Reload or Sync fills it.
func NewServices(db *pgxpool.Pool, o ServicesOptions) *Services {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	s := &Services{
		db: db, logger: o.Logger, publicURL: o.PublicURL, multi: o.Multi, keeperOpt: o.KeeperOptions,
		onChange: o.OnChange, byName: map[string]*entry{}, byID: map[svcID]*entry{}, tunnels: o.Tunnels,
		sessions: o.Sessions, cardEvery: o.TunnelCardRefresh, signer: o.Signer, keyClient: o.SellerKeysClient,
	}
	if u, err := url.Parse(o.PublicURL); err == nil {
		s.loopback = config.IsLocalhost(u.Hostname())
	}
	if s.keyClient == nil {
		s.keyClient = ssrf.NewClient(ssrf.Policy{})
	}
	if s.cardEvery <= 0 {
		s.cardEvery = card.Refresh
	}
	if s.tunnels != nil {
		s.tunnels.OnConnect(s.tunnelConnected)
	}
	return s
}

// ServicePath is {svc} for a service: /agents/{name}, or /agents/{tenant}/{name} on a multi-tenant gate
// (spec §2.1).
func ServicePath(tenantSlug, name string, multi bool) string {
	if multi {
		return "/agents/" + tenantSlug + "/" + name
	}
	return "/agents/" + name
}

// desired is one services row as the configuration says it should be.
type desired struct {
	kind        string
	backend     string // jsonb text
	allow       []string
	tools       any // jsonb text or nil
	tasksMode   any // text or nil
	sealed      string
	retries     int
	timeoutS    int64
	retentionS  int64
	annotations string
	sellerKeys  any // jsonb text or nil
	sessionIdle int
	listed      bool
	maxSessions int // max_sessions_per_principal (D213)
}

func desiredOf(s config.Service) (desired, error) {
	d := desired{
		kind: s.Kind, allow: slices.Clone(s.Allow), sealed: string(s.Sealed.Mode), retries: s.Task.RetryCount(),
		timeoutS: int64(s.Task.Timeout.D() / time.Second), retentionS: int64(s.Task.Retention.D() / time.Second),
		annotations: "{}", sessionIdle: int(s.SessionIdle), listed: s.Listed, maxSessions: s.SessionsPerPrincipal(),
	}
	if d.allow == nil {
		d.allow = []string{}
	}
	var backend any
	switch {
	case s.Backend.Tunnel != nil:
		backend = map[string]any{"tunnel": map[string]any{}}
	case s.Backend.A2A != nil:
		backend = map[string]any{config.KindA2A: map[string]any{"url": s.Backend.A2A.URL, "allow_private": s.Backend.A2A.AllowPrivate}}
	case s.Backend.MCP != nil:
		backend = map[string]any{config.KindMCP: map[string]any{"url": s.Backend.MCP.URL, "allow_private": s.Backend.MCP.AllowPrivate}}
	default:
		return desired{}, errors.New("no backend")
	}
	b, err := json.Marshal(backend)
	if err != nil {
		return desired{}, err
	}
	d.backend = string(b)
	if s.Kind == config.KindMCP {
		d.tasksMode = s.Tasks
		if s.Tools != nil {
			// An absent allow allows every tool (spec §11.3), an empty one none: allow is written only when the file
			// names it, so the relay can tell the two apart (mcprelay.Policy).
			t := map[string]any{"deny": nonNil(s.Tools.Deny)}
			if s.Tools.Allow != nil {
				t["allow"] = s.Tools.Allow
			}
			tb, err := json.Marshal(t)
			if err != nil {
				return desired{}, err
			}
			d.tools = string(tb)
		}
	}
	if !s.Annotations.IsZero() {
		d.annotations = string(s.Annotations)
	}
	if !s.SellerKeys.IsZero() {
		d.sellerKeys = string(s.SellerKeys)
	}
	return d, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// The columns Sync compares, in the order of diffSQL's flags; jsonb columns compare as jsonb (semantic
// equality), so a reformatted file is no change.
var syncFields = []string{"kind", "backend", "allow", "tools", "tasks", "sealed", "retries", "timeout", "retention",
	"annotations", "seller_keys", "session_idle_s", "listed", "max_sessions_per_principal"}

const diffSQL = `select source, disabled_at is not null,
	kind is distinct from $3, backend is distinct from $4::jsonb, allow is distinct from $5::text[],
	tools is distinct from $6::jsonb, tasks_mode is distinct from $7::text, sealed is distinct from $8,
	retries is distinct from $9, timeout_s is distinct from $10, retention_s is distinct from $11,
	annotations is distinct from $12::jsonb, seller_keys is distinct from $13::jsonb,
	session_idle_s is distinct from $14, listed is distinct from $15, max_sessions_per_principal is distinct from $16
  from kontra.services where tenant_id = $1 and name = $2 for update`

// upsertSQL writes a config service. A changed backend drops the stored cards: they are another seller's.
const upsertSQL = `insert into kontra.services (tenant_id, name, kind, backend, allow, tools, tasks_mode, sealed,
	retries, timeout_s, retention_s, annotations, seller_keys, session_idle_s, listed, max_sessions_per_principal, source)
values ($1, $2, $3, $4::jsonb, $5::text[], $6::jsonb, $7::text, $8, $9, $10, $11, $12::jsonb, $13::jsonb, $14, $15, $16, 'config')
on conflict (tenant_id, name) do update set
	kind = excluded.kind, backend = excluded.backend, allow = excluded.allow, tools = excluded.tools,
	tasks_mode = excluded.tasks_mode, sealed = excluded.sealed, retries = excluded.retries,
	timeout_s = excluded.timeout_s, retention_s = excluded.retention_s, annotations = excluded.annotations,
	seller_keys = excluded.seller_keys, session_idle_s = excluded.session_idle_s, listed = excluded.listed,
	max_sessions_per_principal = excluded.max_sessions_per_principal, source = 'config', disabled_at = null, updated_at = now(),
	upstream_card   = case when services.backend is distinct from excluded.backend then null else services.upstream_card end,
	card            = case when services.backend is distinct from excluded.backend then null else services.card end,
	card_hash       = case when services.backend is distinct from excluded.backend then null else services.card_hash end,
	card_fetched_at = case when services.backend is distinct from excluded.backend then null else services.card_fetched_at end`

// ErrSourceConflict is Sync's error for a services: entry whose row Postgres holds as source db: a service
// the operator's control plane writes is never overwritten from a file.
var ErrSourceConflict = errors.New("gate: a service in kontra.yaml is a db-sourced service")

// Sync applies the services: block of cfg (spec §13.5: team gates load it at start and on SIGHUP). In one
// transaction, serialized across replicas: the team tenant is upserted, every service of the block is
// written (source config, disabled_at cleared) when it differs from its row, every config service missing
// from the block gets disabled_at, each such change appends its audit row and queues NOTIFY
// kontra_services '<tenant>/<name>'. Unchanged services write nothing, so a restart or a SIGHUP with the
// same file appends no audit row. Then the registry is read again. A multi-tenant gate's services are the
// operator's rows (Cloud): its block must be empty, and Sync only reads them.
func (s *Services) Sync(ctx context.Context, cfg *config.Config) error {
	if cfg.Tenancy == config.TenancyMulti {
		if len(cfg.Services) > 0 {
			return errors.New("gate: services: in kontra.yaml are a team gate's; a multi-tenant gate's services are rows the operator writes (spec §13.5)")
		}
		return s.Reload(ctx)
	}
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, syncLockID); err != nil {
			return fmt.Errorf("gate: services lock: %w", err)
		}
		tenantID, err := tenant.EnsureTeam(ctx, tx)
		if err != nil {
			return err
		}
		var notify []string
		names := make([]string, 0, len(cfg.Services))
		for n := range cfg.Services {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			changed, err := s.syncOne(ctx, tx, tenantID, name, cfg.Services[name])
			if err != nil {
				return err
			}
			if changed {
				notify = append(notify, TeamTenant+"/"+name)
			}
		}
		rows, err := tx.Query(ctx, `update kontra.services set disabled_at = now(), updated_at = now()
			 where tenant_id = $1 and source = 'config' and disabled_at is null and not (name = any($2::text[]))
			 returning name`, tenantID, names)
		if err != nil {
			return fmt.Errorf("gate: disable removed services: %w", err)
		}
		disabled, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("gate: disable removed services: %w", err)
		}
		sort.Strings(disabled)
		for _, name := range disabled {
			if _, err := audit.Append(ctx, tx, EventServiceDisabled, audit.Fields{
				"tenant": TeamTenant, "tenant_id": tenantID, "service": name, "source": "config",
				"actor": "kontra-gate serve",
			}); err != nil {
				return err
			}
			notify = append(notify, TeamTenant+"/"+name)
		}
		for _, p := range notify {
			if _, err := tx.Exec(ctx, `select pg_notify($1, $2)`, "kontra_services", p); err != nil {
				return fmt.Errorf("gate: notify kontra_services: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.Reload(ctx)
}

// syncOne writes one config service when it differs from its row, with its audit row.
func (s *Services) syncOne(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, name string, svc config.Service) (bool, error) {
	d, err := desiredOf(svc)
	if err != nil {
		return false, fmt.Errorf("gate: services.%s: %w", name, err)
	}
	args := []any{tenantID, name, d.kind, d.backend, d.allow, d.tools, d.tasksMode, d.sealed, d.retries,
		d.timeoutS, d.retentionS, d.annotations, d.sellerKeys, d.sessionIdle, d.listed, d.maxSessions}
	var (
		source   string
		disabled bool
		flags    = make([]bool, len(syncFields))
	)
	dest := []any{&source, &disabled}
	for i := range flags {
		dest = append(dest, &flags[i])
	}
	var changes []string
	err = tx.QueryRow(ctx, diffSQL, args...).Scan(dest...)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		changes = []string{"created"}
	case err != nil:
		return false, fmt.Errorf("gate: services.%s: %w", name, err)
	case source != "config":
		return false, fmt.Errorf("%w: %s/%s", ErrSourceConflict, TeamTenant, name)
	default:
		if disabled {
			changes = append(changes, "enabled")
		}
		for i, f := range flags {
			if f {
				changes = append(changes, syncFields[i])
			}
		}
	}
	if len(changes) == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, upsertSQL, args...); err != nil {
		return false, fmt.Errorf("gate: services.%s: %w", name, err)
	}
	f := audit.Fields{
		"tenant": TeamTenant, "tenant_id": tenantID, "service": name, "source": "config", "kind": d.kind,
		"backend": backendKind(svc), "allow": d.allow, "listed": d.listed, "changes": changes,
		"actor": "kontra-gate serve",
	}
	if u := svc.Backend.Direct(); u != nil {
		f["url"] = u.URL // validated: no userinfo, query or fragment (config.CheckBackendURL)
	}
	if _, err := audit.Append(ctx, tx, EventServiceUpserted, f); err != nil {
		return false, err
	}
	return true, nil
}

func backendKind(s config.Service) string {
	switch {
	case s.Backend.Tunnel != nil:
		return "tunnel"
	case s.Backend.A2A != nil:
		return config.KindA2A
	}
	return config.KindMCP
}

// row is one services row as the registry reads it.
type row struct {
	tenantID    uuid.UUID
	tenant      string
	name        string
	kind        string
	backend     []byte
	allow       []string
	listed      bool
	disabled    bool
	timeoutS    int64
	retries     int
	retentionS  int64
	annotations []byte
	sealed      string // off, optional or required
	sellerKeys  []byte // seller_keys as text, nil when SQL NULL or JSON null
	upstream    []byte
	fetchedAt   *time.Time
	tools       []byte // services.tools as text (mcp only), nil when NULL
	tasksMode   string // services.tasks_mode (mcp only), "" when NULL
	sessionIdle int    // services.session_idle_s
	maxSessions int    // services.max_sessions_per_principal (D213)
	// recipients are a sealed service's recipients (ext-sealed.md §1, D61): the keys enrolled for it, neither
	// revoked nor rotated, connected or not, as age1… strings in enrollment order; nil for a service that is not
	// sealed.
	recipients []string
}

const selectServices = `select s.tenant_id, t.slug, s.name, s.kind, s.backend::text, s.allow, s.listed,
	s.disabled_at is not null, s.timeout_s, s.retries, s.retention_s, s.annotations::text, s.sealed,
	case when s.seller_keys is null or s.seller_keys = 'null'::jsonb then null else s.seller_keys::text end,
	s.upstream_card, s.card_fetched_at, s.tools::text, coalesce(s.tasks_mode, ''), s.session_idle_s,
	s.max_sessions_per_principal,
	case when s.sealed <> 'off' then array(
		select k.pubkey from kontra.key_services ks join kontra.tunnel_keys k on k.key_id = ks.key_id
		 where ks.tenant_id = s.tenant_id and ks.service = s.name and ks.revoked_at is null
		   and k.revoked_at is null and k.revoked_reason is distinct from 'rotated'
		 order by ks.enrolled_at, k.key_id) end
  from kontra.services s join kontra.tenants t on t.id = s.tenant_id
 where t.deleted_at is null and (t.expires_at is null or t.expires_at > now())`

func scanRow(r pgx.CollectableRow) (row, error) {
	var x row
	var backend, annots string
	var keys, tools *string
	var pubkeys [][]byte
	err := r.Scan(&x.tenantID, &x.tenant, &x.name, &x.kind, &backend, &x.allow, &x.listed, &x.disabled,
		&x.timeoutS, &x.retries, &x.retentionS, &annots, &x.sealed, &keys, &x.upstream, &x.fetchedAt,
		&tools, &x.tasksMode, &x.sessionIdle, &x.maxSessions, &pubkeys)
	if err != nil {
		return x, err
	}
	x.backend, x.annotations = []byte(backend), []byte(annots)
	if x.sealed != string(config.SealedOff) {
		x.recipients = []string{}
		for _, b := range pubkeys {
			pub, err := ecdh.X25519().NewPublicKey(b)
			if err != nil {
				return x, fmt.Errorf("services: a recipient of %s/%s: %w", x.tenant, x.name, err)
			}
			rcpt, err := tunnel.FormatRecipient(pub)
			if err != nil {
				return x, err
			}
			x.recipients = append(x.recipients, rcpt)
		}
	}
	if keys != nil {
		x.sellerKeys = []byte(*keys)
	}
	if tools != nil {
		x.tools = []byte(*tools)
	}
	return x, err
}

// Reload reads every service row and applies it: new services appear, changed ones are rebuilt, and
// services whose rows are gone (their tenant deleted or expired) are dropped.
func (s *Services) Reload(ctx context.Context) error {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	rows, err := s.db.Query(ctx, selectServices)
	if err != nil {
		return fmt.Errorf("gate: read services: %w", err)
	}
	all, err := pgx.CollectRows(rows, scanRow)
	if err != nil {
		return fmt.Errorf("gate: read services: %w", err)
	}
	s.apply(all, true)
	return nil
}

// reloadID reads one service by its tenant's id.
func (s *Services) reloadID(ctx context.Context, tenantID uuid.UUID, name string) error {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	rows, err := s.db.Query(ctx, selectServices+` and s.tenant_id = $1 and s.name = $2`, tenantID, name)
	if err != nil {
		return fmt.Errorf("gate: read service %s/%s: %w", tenantID, name, err)
	}
	got, err := pgx.CollectRows(rows, scanRow)
	if err != nil {
		return fmt.Errorf("gate: read service %s/%s: %w", tenantID, name, err)
	}
	if len(got) > 0 {
		s.apply(got, false)
	}
	return nil
}

// reloadOne reads one service, named "<tenant slug>/<name>" as NOTIFY kontra_services carries it.
func (s *Services) reloadOne(ctx context.Context, payload string) error {
	slug, name, ok := strings.Cut(payload, "/")
	if !ok || !config.NameRE.MatchString(slug) || !config.NameRE.MatchString(name) {
		return s.Reload(ctx) // a payload of another shape: read everything rather than miss a change
	}
	s.readMu.Lock()
	defer s.readMu.Unlock()
	rows, err := s.db.Query(ctx, selectServices+` and t.slug = $1 and s.name = $2`, slug, name)
	if err != nil {
		return fmt.Errorf("gate: read service %s: %w", payload, err)
	}
	got, err := pgx.CollectRows(rows, scanRow)
	if err != nil {
		return fmt.Errorf("gate: read service %s: %w", payload, err)
	}
	if len(got) == 0 {
		s.remove(slug + "/" + name)
		return nil
	}
	s.apply(got, false)
	return nil
}

// Notify applies one NOTIFY kontra_services payload (pg.Listener.OnChannel); errors are logged, and the
// periodic refresh repairs a read that failed.
func (s *Services) Notify(payload string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.reloadOne(ctx, payload); err != nil {
		s.logger.Warn("services: re-read after NOTIFY kontra_services failed", "service", payload, "error", err)
	}
}

// apply installs rows; with full, services missing from rows are dropped.
func (s *Services) apply(rows []row, full bool) {
	var changed []store.ServiceRef
	s.mu.Lock()
	seen := map[string]bool{}
	for _, x := range rows {
		key := x.tenant + "/" + x.name
		seen[key] = true
		old := s.byName[key]
		e, err := s.build(x, old)
		if err != nil {
			s.logger.Error("services: a service row cannot be served; it is left out", "service", key, "error", err)
			if old != nil {
				s.drop(key, old)
				changed = append(changed, old.ref)
			}
			continue
		}
		if old == nil || !sameEntry(old, e) {
			changed = append(changed, e.ref)
		}
		s.byName[key] = e
		s.byID[svcID{e.ref.TenantID, e.ref.Name}] = e
	}
	if full {
		for key, e := range s.byName {
			if !seen[key] {
				s.drop(key, e)
				changed = append(changed, e.ref)
			}
		}
	}
	s.mu.Unlock()
	if s.onChange != nil {
		for _, ref := range changed {
			s.onChange(ref)
		}
	}
}

func (s *Services) remove(key string) {
	s.mu.Lock()
	e := s.byName[key]
	if e != nil {
		s.drop(key, e)
	}
	s.mu.Unlock()
	if e != nil && s.onChange != nil {
		s.onChange(e.ref)
	}
}

// drop removes an entry and stops its keeper; s.mu is held.
func (s *Services) drop(key string, e *entry) {
	delete(s.byName, key)
	if cur := s.byID[svcID{e.ref.TenantID, e.ref.Name}]; cur == e {
		delete(s.byID, svcID{e.ref.TenantID, e.ref.Name})
	}
	if e.stop != nil {
		e.stop()
		e.stop = nil
	}
}

func sameEntry(a, b *entry) bool {
	return a.ref == b.ref && a.listed == b.listed && a.disabled == b.disabled && a.backendJS == b.backendJS &&
		a.keeperKey == b.keeperKey && a.mcpKey == b.mcpKey && slices.Equal(a.allowRaw, b.allowRaw)
}

// build returns the entry for x, reusing old's keeper when what it was built from did not change; s.mu is
// held.
func (s *Services) build(x row, old *entry) (*entry, error) {
	allow, err := auth.ParseAllowList(x.allow)
	if err != nil {
		return nil, err
	}
	b, err := parseBackend(x.backend)
	if err != nil {
		return nil, err
	}
	if !x.disabled {
		if err := s.refusable(x, b); err != nil {
			return nil, err
		}
	}
	e := &entry{
		ref: store.ServiceRef{TenantID: x.tenantID, Tenant: x.tenant, Name: x.name, Kind: x.kind,
			Timeout: time.Duration(x.timeoutS) * time.Second, Retries: x.retries, Retention: time.Duration(x.retentionS) * time.Second,
			Sealed: store.SealedRef(x.sealed)},
		allow: allow, allowRaw: slices.Clone(x.allow), listed: x.listed, disabled: x.disabled, backend: b,
		backendJS: string(x.backend),
	}
	// An enabled a2a service has a card: fetched from a direct backend, or through the replica's sessions
	// for a tunnel one (when the registry has them). An MCP service has none (spec §3).
	tunnelled := b.Kind == backendTunnel && s.tunnels != nil
	var operatorURIs []string
	if !x.disabled && x.kind == config.KindA2A && (b.Kind == config.KindA2A || tunnelled) {
		exts, err := cardExtensions(x.annotations)
		if err != nil {
			return nil, err
		}
		operatorURIs = extensionURIs(exts)
		in := card.Input{PublicURL: s.publicURL, ServicePath: ServicePath(x.tenant, x.name, s.multi),
			OnlyAnonymous: allow.OnlyAnonymous(), CardExtensions: exts}
		if x.sealed != string(config.SealedOff) {
			// ext-sealed.md §1: the recipients are part of the card, so a change of them (an enrollment, a
			// revocation, a rotation) changes the keeper's key, and the card is rewritten and signed again.
			in.Sealed = sealedEntry(x)
		}
		e.keeperKey = keeperKey(e.backendJS, in, x.sellerKeys)
		if old != nil && old.keeper != nil && old.keeperKey == e.keeperKey {
			e.keeper, e.stop, e.tunnel = old.keeper, old.stop, old.tunnel
			old.stop = nil // handed over: dropping old must not stop it
			if e.tunnel != nil {
				s.seedTunnel(e, x)
			}
		} else {
			if old != nil && old.stop != nil {
				old.stop()
				old.stop = nil
			}
			k, tc, err := s.newKeeper(e, b, in, x)
			if err != nil {
				return nil, err
			}
			e.keeper, e.tunnel = k, tc
			if s.running != nil {
				e.stop = s.start(e)
			}
		}
	} else if old != nil && old.stop != nil {
		old.stop()
		old.stop = nil
	}
	cards := func() (*card.Served, error) { return nil, card.ErrUnavailable }
	if e.keeper != nil {
		cards = e.keeper.Current
	}
	e.served = &a2ahttp.Service{Ref: e.ref, Allow: allow, Listed: x.listed, Disabled: x.disabled, Cards: cards,
		OperatorExtensions: operatorURIs}
	if x.kind == config.KindMCP {
		m, err := s.mcpService(e, x, b)
		if err != nil {
			return nil, err
		}
		e.mcp = m
		e.mcpKey = string(x.tools) + "|" + x.tasksMode + "|" + x.sealed + "|" + string(x.annotations) + "|" + fmt.Sprint(x.sessionIdle) +
			"|" + fmt.Sprint(x.maxSessions) + "|" + strings.Join(x.recipients, ",")
	}
	return e, nil
}

// mcpService is a kind: mcp row as the MCP relay serves it (spec §11): its policy (services.tools), its tasks:
// mode, its sealed flag, its session_idle_s and max_sessions_per_principal, its tool_meta and initialize_meta annotations (§13.4), and for a
// direct mcp: {url} backend the SSRF-safe backend pinned to that URL's host:port (D71), which is the endpoint
// itself. A value that cannot be read leaves the row out (its policy must never be applied half).
func (s *Services) mcpService(e *entry, x row, b backendSpec) (*mcprelay.Service, error) {
	m := &mcprelay.Service{Ref: e.ref, Allow: e.allow, Disabled: x.disabled, Tasks: x.tasksMode,
		Sealed: x.sealed != string(config.SealedOff), SessionIdle: time.Duration(x.sessionIdle) * time.Second,
		MaxSessionsPerPrincipal: x.maxSessions}
	if m.Sealed {
		m.SealedRequired = x.sealed == string(config.SealedRequired)
		m.SealedCapability = sealedEntry(x)
	}
	if m.Tasks == "" {
		m.Tasks = config.TasksDurable
	}
	if len(x.tools) > 0 && string(x.tools) != "null" {
		var t struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		}
		if err := json.Unmarshal(x.tools, &t); err != nil {
			return nil, fmt.Errorf("services.tools: %w", err)
		}
		for _, g := range slices.Concat(t.Allow, t.Deny) {
			if g == "" {
				return nil, errors.New("services.tools: an empty glob")
			}
		}
		m.Tools = mcprelay.Policy{Allow: t.Allow, Deny: t.Deny}
	}
	if len(bytes.TrimSpace(x.annotations)) > 0 {
		var a struct {
			ToolMeta       map[string]map[string]json.RawMessage `json:"tool_meta"`
			InitializeMeta map[string]json.RawMessage            `json:"initialize_meta"`
		}
		if err := json.Unmarshal(x.annotations, &a); err != nil {
			return nil, fmt.Errorf("services.annotations: tool_meta and initialize_meta must be objects of objects: %w", err)
		}
		m.ToolMeta, m.InitializeMeta = a.ToolMeta, a.InitializeMeta
	}
	if b.Kind == config.KindMCP && !x.disabled {
		be, err := upstream.NewDirect(b.URL, b.AllowPrivate)
		if err != nil {
			return nil, err
		}
		m.Direct = be
	}
	return m, nil
}

// MCPService implements mcprelay.Services: the tenant's MCP service of that name, disabled ones included (the
// route answers them as unknown), nil when there is none or it is an a2a service.
func (s *Services) MCPService(_ context.Context, tenant, name string) (*mcprelay.Service, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e := s.byName[tenant+"/"+name]; e != nil {
		return e.mcp, nil
	}
	return nil, nil
}

// MCPServiceByID implements mcprelay.Services by the tenant's id.
func (s *Services) MCPServiceByID(_ context.Context, tenantID uuid.UUID, name string) (*mcprelay.Service, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e := s.byID[svcID{tenantID, name}]; e != nil {
		return e.mcp, nil
	}
	return nil, nil
}

// refusable applies to every row the refusals config.Validate makes for a services: entry, so a row the
// operator's control plane writes (source db, spec §13.5) cannot bypass them and fail open.
//
// SPEC-CONFLICT (the configuration's resolutions, config/validate.go; D90, D91): sealing needs backend tunnel (the
// recipients are the service's enrolled node keys and the hosting node unseals, ext-sealed.md §1), and a direct
// backend has neither, so a sealed direct service would be relayed in plaintext. Such a row is left out rather
// than served. A sealed tunnel row is served since M3 WP-08, which lifts D144's refusal: its card or initialize
// result publishes the recipients (sealedEntry), and the gate refuses unsealed parts to a required one.
//
// Seller card signatures are verified since M3 WP-02, which lifts D90 and the seller_keys half of D144: a row
// with seller_keys is served, its keeper verifying the card, unless the keys cannot be read (left out: a card
// that cannot be verified must not be served unverified). An a2a row on a gate that has no signing key off a
// loopback public_url is left out too (D166: the configuration refuses such a file; a row written later is
// treated as D90 treated a refused key).
func (s *Services) refusable(x row, b backendSpec) error {
	if b.direct() && x.sealed != string(config.SealedOff) {
		return fmt.Errorf("sealed %q on a direct backend: sealing needs backend tunnel (ext-sealed.md §1), and this build would relay the service in plaintext", x.sealed)
	}
	if x.kind == config.KindA2A && s.signer == nil && !s.loopback {
		return errors.New("an a2a service on a gate without signing_key: its card would be served unsigned (D166); set signing_key")
	}
	return nil
}

// sealedDescription is the sealed extension entry's description (ext-sealed.md §1).
const sealedDescription = "Parts are end-to-end sealed to the service's recipients; the gate relays ciphertext."

// sealedEntry is a sealed service's extension object (ext-sealed.md §1): in the served card's
// capabilities.extensions for A2A, under capabilities.experimental["sh.kontra/sealed"] of a legacy initialize result
// for MCP (D27). Its recipients are the row's (the keys enrolled for the service, neither revoked nor rotated,
// D61), params.required is sealed: required, and the entry itself is required: false, so a client that ignores it
// can still call an optional service unsealed.
func sealedEntry(x row) json.RawMessage {
	recipients := x.recipients
	if recipients == nil {
		recipients = []string{}
	}
	b, err := json.Marshal(map[string]any{
		"uri":         sealed.URI,
		"description": sealedDescription,
		"required":    false,
		"params": map[string]any{
			"recipients": recipients,
			"envelope":   sealed.MediaType,
			"required":   x.sealed == string(config.SealedRequired),
		},
	})
	if err != nil {
		panic(err) // strings and booleans only
	}
	return b
}

// RecipientsChanged applies a NOTIFY that may change a sealed service's recipients (ext-sealed.md §1, tunnel.md
// §5, §12): kontra_tunnel's 'enrolled:<key_id>/<tenant_id>/<service>' re-reads that service; kontra_revoke's
// 'key:<key_id>' and 'binding:<key_id>/<service>' (a revocation, the end of a rotation's overlap) re-read every
// service, whose recipients the query reads again. A rotation and a first enrollment send NOTIFY kontra_services
// for the service (enroll.Admit), which Notify applies. Other payloads are ignored. The periodic refresh repairs a
// lost notification.
func (s *Services) RecipientsChanged(payload string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var err error
	switch {
	case strings.HasPrefix(payload, "enrolled:"):
		b, ok := enroll.ParseEnrolledPayload(payload)
		if !ok {
			return
		}
		err = s.reloadID(ctx, b.TenantID, b.Service)
	case strings.HasPrefix(payload, "key:"), strings.HasPrefix(payload, "binding:"):
		err = s.Reload(ctx)
	default:
		return
	}
	if err != nil {
		s.logger.Warn("services: re-read after a recipients change failed", "notify", payload, "error", err)
	}
}

func keeperKey(backendJS string, in card.Input, sellerKeys []byte) string {
	b, _ := json.Marshal(struct {
		Backend    json.RawMessage
		In         card.Input
		SellerKeys string
	}{json.RawMessage(backendJS), in, string(sellerKeys)})
	return string(b)
}

// cardExtensions reads services.annotations.card_extensions (spec §13.4): extension entries appended to
// the served card's capabilities.extensions.
func cardExtensions(annotations []byte) ([]json.RawMessage, error) {
	if len(bytes.TrimSpace(annotations)) == 0 {
		return nil, nil
	}
	var a struct {
		CardExtensions []json.RawMessage `json:"card_extensions"`
	}
	if err := json.Unmarshal(annotations, &a); err != nil {
		return nil, fmt.Errorf("services.annotations.card_extensions: %w", err)
	}
	return a.CardExtensions, nil
}

// extensionURIs are the uri members of card_extensions entries, in order; an entry without one is skipped (the card
// keeper refuses it).
func extensionURIs(exts []json.RawMessage) []string {
	var out []string
	for _, e := range exts {
		var ext struct {
			URI string `json:"uri"`
		}
		if json.Unmarshal(e, &ext) == nil && ext.URI != "" && !slices.Contains(out, ext.URI) {
			out = append(out, ext.URI)
		}
	}
	return out
}

// newKeeper builds the card keeper of an a2a service, seeded from its stored upstream card: a direct
// backend's (fetched from its URL), or a tunnel service's (fetched through this replica's sessions; its
// state is the returned tunnelCard).
func (s *Services) newKeeper(e *entry, b backendSpec, in card.Input, x row) (*card.Keeper, *tunnelCard, error) {
	common, err := s.cardOptions(e, x)
	if err != nil {
		return nil, nil, err
	}
	if b.Kind == backendTunnel {
		k := s.newTunnelKeeper(e, in, common)
		tc := newTunnelCard()
		if len(x.upstream) > 0 && x.fetchedAt != nil {
			if err := k.Seed(x.upstream, *x.fetchedAt); err != nil {
				s.logger.Warn("services: the stored card of a tunnel service cannot be served; it is fetched when a session for it connects",
					"service", e.ref.Tenant+"/"+e.ref.Name, "error", err)
			} else {
				tc.newer(*x.fetchedAt)
			}
		}
		return k, tc, nil
	}
	be, err := upstream.NewDirect(b.URL, b.AllowPrivate)
	if err != nil {
		return nil, nil, err
	}
	ref, backendJS := e.ref, e.backendJS
	opts := append(common,
		card.WithPersist(func(ctx context.Context, sv *card.Served, _ time.Time) error {
			return s.persistCard(ctx, ref, backendJS, sv, false)
		}),
	)
	k := card.NewKeeper(be, in, append(opts, s.keeperOpt...)...)
	if len(x.upstream) > 0 && x.fetchedAt != nil {
		if err := k.Seed(x.upstream, *x.fetchedAt); err != nil {
			// A stored card this build cannot serve: the keeper fetches a fresh one.
			s.logger.Warn("services: the stored card cannot be served; fetching it again", "service", ref.Tenant+"/"+ref.Name, "error", err)
		}
	}
	return k, nil, nil
}

// newTunnelKeeper builds the keeper of a tunnel service: no backend of its own (the dispatch's is the
// attempt's session's, tunnelBackend), a fetch through this replica's sessions (TunnelCards.FetchCard), and the persist hook
// with NOTIFY kontra_services, so the replicas without a session seed the new card at once.
//
// SPEC-CONFLICT (M2 plan, confirmed conflict on relay.md §3 with §6.2 and tunnel.md §11): §3 keeps one upstream
// card per service (services.upstream_card), while several nodes, each with its own target, may serve one
// service, and the dispatch reads its interface from that one card whichever session runs it. The closest
// faithful behaviour: the nodes of one service are expected to serve the same card; the last good fetch
// through any session is the service's card, and a fetch that differs from the card served is logged as a
// warning (it is also how a node's new card version arrives). A card per session would need a migration.
func (s *Services) newTunnelKeeper(e *entry, in card.Input, common []card.KeeperOption) *card.Keeper {
	ref, backendJS := e.ref, e.backendJS
	var k *card.Keeper
	k = card.NewKeeper(nil, in, append(append(common,
		card.WithPersist(func(ctx context.Context, sv *card.Served, _ time.Time) error {
			return s.persistCard(ctx, ref, backendJS, sv, true)
		}),
		card.WithFetch(func(ctx context.Context, _ upstream.Backend) (*upstream.Fetched, error) {
			f, err := s.tunnels.FetchCard(ctx, ref.TenantID, ref.Name)
			if err != nil {
				return nil, err
			}
			if cur, _ := k.Current(); cur != nil && !bytes.Equal(cur.Upstream, f.Raw) {
				s.logger.Warn("services: the card fetched through a tunnel session differs from the one served; the last good fetch is served from now on (a service's nodes should serve one card, relay.md §3)",
					"service", ref.Tenant+"/"+ref.Name)
			}
			return f, nil
		}),
	), s.keeperOpt...)...)
	return k
}

// cardOptions are the keeper options every a2a service's keeper gets: the gate's signer, the service's seller
// keys (a value that cannot be read leaves the row out: its card must not be served unverified), and the touch
// hook that moves card_fetched_at on every good fetch of the recorded card (D89; never for a card no seller key
// verifies).
func (s *Services) cardOptions(e *entry, x row) ([]card.KeeperOption, error) {
	ref, backendJS := e.ref, e.backendJS
	opts := []card.KeeperOption{
		card.WithTouch(func(ctx context.Context, sv *card.Served) { s.touchCard(ctx, ref, backendJS, sv.Upstream) }),
	}
	if s.signer != nil {
		opts = append(opts, card.WithSigner(s.signer))
	}
	if x.sellerKeys != nil {
		keys, err := card.NewSellerKeys(x.sellerKeys, s.keyClient)
		if err != nil {
			return nil, err
		}
		opts = append(opts, card.WithSellerKeys(keys))
	}
	return opts, nil
}

// seedTunnel installs a tunnel service's stored card when it is newer than the one its keeper has: another
// replica fetched it through its own session (or this one did, and the row carries the database's time). s.mu
// is held.
func (s *Services) seedTunnel(e *entry, x row) {
	if len(x.upstream) == 0 || x.fetchedAt == nil || !e.tunnel.newer(*x.fetchedAt) {
		return
	}
	if err := e.keeper.Seed(x.upstream, *x.fetchedAt); err != nil {
		s.logger.Warn("services: the stored card of a tunnel service cannot be served", "service", e.ref.Tenant+"/"+e.ref.Name, "error", err)
	}
}

// tunnelConnected is the registry's OnConnect: a session serving the tunnel service has connected and is
// not yet a candidate; its card is fetched through it now (spec §3; M2 plan audit, gap 3), within ctx. A
// service this replica has not read yet (a tenant token created it in the same handshake, and its NOTIFY
// kontra_services is on its way) is read first. A fetch that fails is retried on the keeper loop's backoff.
//
// A failed fetch does not keep the session from becoming a candidate (the registry inserts its row
// anyway): a node whose agent is not listening yet (a compose or systemd sidecar started before its agent:
// 502 target_unreachable) may serve other services on the session, and the handshake is all-or-nothing.
// Of the audit's two options for gap 3 the build therefore takes the second (M2 review of WP-04): a
// dispatch for a tunnel service whose keeper has no card yet (Dispatch's ErrUnavailable before any good
// fetch or seed) must be a retryable outcome, like no_session, not failed: service_unavailable. That is
// Backend's relay.ErrCardPending for tunnel services (tunnelBackend); a keeper that has a card, fetched or
// seeded from services.upstream_card, dispatches with it whatever this fetch did.
func (s *Services) tunnelConnected(ctx context.Context, tenantID uuid.UUID, service string) {
	s.mu.RLock()
	e := s.byID[svcID{tenantID, service}]
	s.mu.RUnlock()
	if e == nil {
		if err := s.reloadID(ctx, tenantID, service); err != nil {
			s.logger.Warn("services: a tunnel service whose session connected could not be read", "service", service, "error", err)
			return
		}
		s.mu.RLock()
		e = s.byID[svcID{tenantID, service}]
		s.mu.RUnlock()
	}
	if e == nil || e.tunnel == nil {
		return
	}
	if err := e.keeper.Refresh(ctx); err != nil {
		s.logger.Warn("services: the card of a tunnel service was not fetched when its session connected; retrying",
			"service", e.ref.Tenant+"/"+e.ref.Name, "error", err)
		select {
		case e.tunnel.kick <- struct{}{}:
		default:
		}
	}
}

// tunnelCardKick is the least time between two fetches of a tunnel card that kicks start (runTunnelCard).
const tunnelCardKick = time.Second

// runTunnelCard refetches a tunnel service's card every cardEvery while this replica holds a session
// serving it (spec §3: "again every 10 minutes while serving"), on a backoff of 1 s doubling to 1 min after
// a failure; a keeper without a card fetches at once. Without a session there is nothing to fetch through:
// the stored card is served (seedTunnel).
//
// A kick (a session whose fetch failed when it connected, a dispatch that found no card: relay.ErrCardPending)
// fetches at once, or tunnelCardKick after the loop's previous fetch when that is later, and never later than the
// fetch already due. Restarting the backoff instead (the WP-04 loop) left a node's agent that came up after
// it to the next refetch, up to a minute away, while each dispatch's kick postponed it again: a task placed
// meanwhile spent its retries and failed service_unavailable (M2 review of WP-08, TestTunnelCardPending).
func (s *Services) runTunnelCard(ctx context.Context, ref store.ServiceRef, k *card.Keeper, tc *tunnelCard) {
	wait, backoff := s.cardEvery, time.Second
	if _, err := k.Current(); err != nil {
		wait = 0
	}
	var last time.Time // the previous fetch
	for {
		due := time.Now().Add(wait)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-tc.kick:
			t.Stop()
			wait = max(0, min(time.Until(due), tunnelCardKick-time.Since(last)))
			continue
		case <-t.C:
		}
		if !s.tunnels.Serves(ref.TenantID, ref.Name) {
			// No session to fetch the card through: the stored card is served (seedTunnel), and a {url} seller
			// key set is fetched here, so a seeded card that waits for it is verified.
			if err := k.RefreshSellerKeys(ctx); err != nil && ctx.Err() == nil {
				s.logger.Warn("services: a tunnel service's seller keys were not fetched", "service", ref.Tenant+"/"+ref.Name, "error", err)
			}
			wait, backoff = s.cardEvery, time.Second
			continue
		}
		last = time.Now()
		if err := k.Refresh(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Warn("services: a tunnel service's card was not refetched; the last good card is kept",
				"service", ref.Tenant+"/"+ref.Name, "error", err, "retry_in", backoff)
			wait, backoff = backoff, min(backoff*2, time.Minute)
			continue
		}
		wait, backoff = s.cardEvery, time.Second
	}
}

// persistCard records the card a keeper serves (its persist hook, called after a good fetch until it
// succeeds): first the upstream bytes, verbatim, their JCS hash (card_hash, D166) and the fetch time, which
// are what a restarted replica seeds from; then the served card, a best-effort copy (json, D49): a body Postgres refuses as json (a
// data exception: text that is not UTF-8, a NUL character) leaves services.card NULL, as the audit
// proposed, and is not an error. Any other failure is returned, and the keeper calls again at its next good
// fetch. The backend guard keeps a keeper of a URL the configuration has just replaced from writing its
// seller's card under the new backend; the upstream_card guard keeps a served card next to the bytes it was
// produced from when another replica records newer ones meanwhile. With notify (a tunnel service's card,
// which the replicas without a session do not fetch themselves) a recorded card is announced with NOTIFY
// kontra_services, on which every replica seeds its keeper with it.
func (s *Services) persistCard(ctx context.Context, ref store.ServiceRef, backendJS string, sv *card.Served, notify bool) error {
	tag, err := s.db.Exec(ctx, `update kontra.services set upstream_card = $3, card_fetched_at = now(), card_hash = $5,
			updated_at = case when upstream_card is distinct from $3 then now() else updated_at end
		 where tenant_id = $1 and name = $2 and backend = $4::jsonb`, ref.TenantID, ref.Name, sv.Upstream, backendJS, sv.Hash[:])
	if err != nil {
		return fmt.Errorf("upstream_card: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // the backend changed (this keeper is being replaced) or the row is gone
	}
	_, err = s.db.Exec(ctx, `update kontra.services set card = $3::text::json, updated_at = now()
		 where tenant_id = $1 and name = $2 and backend = $4::jsonb and upstream_card = $5
		   and card::text is distinct from $3::text`, ref.TenantID, ref.Name, string(sv.Body), backendJS, sv.Upstream)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "22") {
		s.logger.Warn("services: Postgres refuses the served card as json; services.card is left NULL (upstream_card is stored)",
			"service", ref.Tenant+"/"+ref.Name, "error", err)
		_, err = s.db.Exec(ctx, `update kontra.services set card = null, updated_at = now()
			 where tenant_id = $1 and name = $2 and backend = $3::jsonb and upstream_card = $4 and card is not null`,
			ref.TenantID, ref.Name, backendJS, sv.Upstream)
	}
	if err != nil {
		return fmt.Errorf("card: %w", err)
	}
	if notify {
		if _, err := s.db.Exec(ctx, `select pg_notify($1, $2)`, string(pg.ChannelServices), ref.Tenant+"/"+ref.Name); err != nil {
			// The others re-read every service within RefreshEvery anyway.
			s.logger.Debug("services: NOTIFY kontra_services after a tunnel card was recorded failed", "service", ref.Tenant+"/"+ref.Name, "error", err)
		}
	}
	return nil
}

// touchCard moves card_fetched_at after a successful fetch of the bytes already stored, so a replica that
// starts later seeds its keeper with the gate's last good fetch. A fetch of other bytes touches nothing:
// the keeper persists them once it has installed them.
func (s *Services) touchCard(ctx context.Context, ref store.ServiceRef, backendJS string, raw []byte) {
	if _, err := s.db.Exec(ctx, `update kontra.services set card_fetched_at = now()
		 where tenant_id = $1 and name = $2 and backend = $3::jsonb and upstream_card = $4`,
		ref.TenantID, ref.Name, backendJS, raw); err != nil {
		s.logger.Debug("services: card_fetched_at not moved", "service", ref.Tenant+"/"+ref.Name, "error", err)
	}
}

// start runs e's keeper under the registry's run context (a tunnel service's through runTunnelCard); s.mu
// is held and s.running set.
func (s *Services) start(e *entry) context.CancelFunc {
	ctx, cancel := context.WithCancel(s.running)
	if e.tunnel != nil {
		go s.runTunnelCard(ctx, e.ref, e.keeper, e.tunnel)
	} else {
		go e.keeper.Run(ctx)
	}
	return cancel
}

// Run starts every card keeper and keeps the registry current until ctx ends: a full re-read every
// RefreshEvery (NOTIFY kontra_services and the LISTEN reconnects are wired by the replica). When ctx ends the
// keepers stop.
func (s *Services) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = RefreshEvery
	}
	s.mu.Lock()
	s.running = ctx
	for _, e := range s.byName {
		if e.keeper != nil && e.stop == nil {
			e.stop = s.start(e)
		}
	}
	s.mu.Unlock()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			for _, e := range s.byName {
				if e.stop != nil {
					e.stop()
					e.stop = nil
				}
			}
			s.running = nil
			s.mu.Unlock()
			return
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := s.Reload(rctx); err != nil && ctx.Err() == nil {
				s.logger.Warn("services: periodic re-read failed", "error", err)
			}
			cancel()
		}
	}
}

// WaitCards waits until every enabled direct a2a service has a card, or until ctx ends, and returns the
// services still without one. A replica waits for this, briefly, before its workers poll, so that a
// dispatch picked up at start does not fail service_unavailable only because the first fetch is still on
// its way. A tunnel service's card waits for its sessions, so it is not waited for here.
func (s *Services) WaitCards(ctx context.Context) []string {
	for {
		var missing []string
		s.mu.RLock()
		for key, e := range s.byName {
			if e.keeper != nil && e.tunnel == nil {
				if _, err := e.keeper.Current(); err != nil {
					missing = append(missing, key)
				}
			}
		}
		s.mu.RUnlock()
		if len(missing) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			sort.Strings(missing)
			return missing
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Count is the number of services the registry serves (enabled ones).
func (s *Services) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, e := range s.byName {
		if !e.disabled {
			n++
		}
	}
	return n
}

// Lookup implements a2ahttp.Services: the service, disabled ones included (the route answers them as
// unknown), or nil when the tenant has no service of that name.
func (s *Services) Lookup(_ context.Context, tenant, name string) (*a2ahttp.Service, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e := s.byName[tenant+"/"+name]
	if e == nil {
		return nil, nil
	}
	return e.served, nil
}

// LookupByID is Lookup by the tenant's id: the service as this replica serves it, disabled ones included, or nil when
// it serves none of that name (a row it has not loaded yet, or refused to build: apply leaves such a row out). The
// gate's own /mcp reads it to list, hire and follow exactly what the A2A route serves (relay.md §12.2, D303).
func (s *Services) LookupByID(tenantID uuid.UUID, name string) *a2ahttp.Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e := s.byID[svcID{tenantID, name}]; e != nil {
		return e.served
	}
	return nil
}

// errNoService is the registry's answer for a service it does not serve.
var errNoService = errors.New("gate: no such service")

func (s *Services) live(tenantID uuid.UUID, name string) (*entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e := s.byID[svcID{tenantID, name}]
	switch {
	case e == nil:
		return nil, fmt.Errorf("%w: %s/%s", errNoService, tenantID, name)
	case e.disabled:
		return nil, fmt.Errorf("%w: %s/%s is disabled", errNoService, e.ref.Tenant, name)
	}
	return e, nil
}

// BackendFor implements lifecycle.BackendResolver: how prepare reaches the service. A service the registry
// does not serve (unknown, disabled) is an error, which prepare turns into service_unavailable (spec §6.4).
func (s *Services) BackendFor(_ context.Context, tenantID uuid.UUID, service string) (store.Backend, error) {
	e, err := s.live(tenantID, service)
	if err != nil {
		return "", err
	}
	if e.backend.direct() {
		return store.BackendDirect, nil
	}
	return store.BackendTunnel, nil
}

// Backend implements relay.Backends: the backend and the upstream card Info a dispatch or a forward_cancel
// of the attempt uses (spec §6.2). A direct service's come from its keeper (card.Keeper.Dispatch). A tunnel
// service's backend is the attempt's session on this replica (TunnelSessions.Backend: an error wrapping
// upstream.ErrTunnelLost when this replica does not hold it, or it no longer serves the service), with the
// Info of the service's one card (spec §3). No usable card is an error, and the dispatch fails
// service_unavailable (spec §6.4), except a tunnel service's card that no fetch has produced yet, or whose
// last good fetch is older than card.UnavailableAfter (its session has just connected and the fetch failed):
// relay.ErrCardPending, which is retried, and the card is fetched again at once.
func (s *Services) Backend(_ context.Context, svc store.ServiceRef, attempt *store.Attempt) (upstream.Backend, upstream.Info, error) {
	e, err := s.live(svc.TenantID, svc.Name)
	if err != nil {
		return nil, upstream.Info{}, err
	}
	if e.backend.Kind == backendTunnel {
		return s.tunnelBackend(e, svc, attempt)
	}
	if attempt != nil && attempt.Backend == store.BackendTunnel {
		// The service moved from a tunnel to a direct backend while the task ran: the attempt's upstream
		// task lives on a node, never at this URL (spec §4).
		return nil, upstream.Info{}, fmt.Errorf("gate: service %s/%s is no longer a tunnel service; its attempt was placed on a tunnel session", e.ref.Tenant, e.ref.Name)
	}
	if e.keeper == nil {
		return nil, upstream.Info{}, fmt.Errorf("gate: service %s/%s has no card this gate fetches (backend %s)", e.ref.Tenant, e.ref.Name, e.backend.Kind)
	}
	return e.keeper.Dispatch()
}

// tunnelBackend is Backend for a tunnel service. The session is checked first: a dispatch whose session is
// gone opens no stream and is a lost session whatever the card (spec §14.2).
func (s *Services) tunnelBackend(e *entry, svc store.ServiceRef, attempt *store.Attempt) (upstream.Backend, upstream.Info, error) {
	name := e.ref.Tenant + "/" + e.ref.Name
	switch {
	case s.sessions == nil:
		return nil, upstream.Info{}, fmt.Errorf("gate: service %s is a tunnel service and this replica holds no tunnel sessions", name)
	case attempt == nil || attempt.Backend != store.BackendTunnel || attempt.TunnelSession == nil:
		return nil, upstream.Info{}, fmt.Errorf("%w: the attempt of tunnel service %s was placed on no session", upstream.ErrTunnelLost, name)
	}
	be, err := s.sessions.Backend(*attempt.TunnelSession, svc)
	if err != nil {
		return nil, upstream.Info{}, err
	}
	if e.keeper == nil {
		return nil, upstream.Info{}, fmt.Errorf("gate: tunnel service %s has no card keeper", name)
	}
	_, info, err := e.keeper.Dispatch()
	if err != nil {
		if errors.Is(err, card.ErrNotVerified) {
			// The card was fetched and no configured seller key verifies it: the service is unavailable
			// (spec §3), and fetching again changes nothing before the seller publishes another card.
			return nil, upstream.Info{}, err
		}
		if cur, _ := e.keeper.Current(); (cur == nil || cur.Unavailable) && e.tunnel != nil {
			// No usable card: none fetched or stored yet, or only one past card.UnavailableAfter (the node was
			// away for more than 30 minutes, and its agent is not up yet now that it is back). The attempt's
			// session is held here and live, so a fetch through it can still produce the card: fetch again
			// now, on the keeper's loop, and retry the dispatch (D143; M2 review, relay-tunnel-3).
			select {
			case e.tunnel.kick <- struct{}{}:
			default:
			}
			return nil, upstream.Info{}, fmt.Errorf("%w: %s: %w", relay.ErrCardPending, name, err)
		}
		return nil, upstream.Info{}, err
	}
	return be, info, nil
}
