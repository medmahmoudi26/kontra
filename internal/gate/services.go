package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/relay"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/tenant"
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
// tenant, read from Postgres, with a card.Keeper for each enabled direct a2a service.
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
// fetch. Replicas that fetched the same bytes serve the same body and ETag. card_hash (sha256 over the JCS
// of the upstream card) stays null until the gate has a JCS library (M3 signing).
type Services struct {
	db        *pgxpool.Pool
	logger    *slog.Logger
	publicURL string
	multi     bool
	keeperOpt []card.KeeperOption
	onChange  func(store.ServiceRef)

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
	keeper    *card.Keeper
	keeperKey string             // what the keeper was built from: rebuilt when it changes
	stop      context.CancelFunc // the keeper's Run, nil when not started
}

// backendSpec is services.backend: {"tunnel": {}} | {"a2a": {"url", "allow_private"}} | {"mcp": {…}}.
type backendSpec struct {
	Kind         string // tunnel, a2a or mcp
	URL          string
	AllowPrivate bool
}

func (b backendSpec) direct() bool { return b.Kind == config.KindA2A || b.Kind == config.KindMCP }

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
		case "tunnel":
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
}

// NewServices returns an empty registry over db; Reload or Sync fills it.
func NewServices(db *pgxpool.Pool, o ServicesOptions) *Services {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Services{
		db: db, logger: o.Logger, publicURL: o.PublicURL, multi: o.Multi, keeperOpt: o.KeeperOptions,
		onChange: o.OnChange, byName: map[string]*entry{}, byID: map[svcID]*entry{},
	}
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
}

func desiredOf(s config.Service) (desired, error) {
	d := desired{
		kind: s.Kind, allow: slices.Clone(s.Allow), sealed: string(s.Sealed.Mode), retries: s.Task.RetryCount(),
		timeoutS: int64(s.Task.Timeout.D() / time.Second), retentionS: int64(s.Task.Retention.D() / time.Second),
		annotations: "{}", sessionIdle: int(s.SessionIdle), listed: s.Listed,
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
			t := map[string]any{"allow": nonNil(s.Tools.Allow), "deny": nonNil(s.Tools.Deny)}
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
	"annotations", "seller_keys", "session_idle_s", "listed"}

const diffSQL = `select source, disabled_at is not null,
	kind is distinct from $3, backend is distinct from $4::jsonb, allow is distinct from $5::text[],
	tools is distinct from $6::jsonb, tasks_mode is distinct from $7::text, sealed is distinct from $8,
	retries is distinct from $9, timeout_s is distinct from $10, retention_s is distinct from $11,
	annotations is distinct from $12::jsonb, seller_keys is distinct from $13::jsonb,
	session_idle_s is distinct from $14, listed is distinct from $15
  from kontra.services where tenant_id = $1 and name = $2 for update`

// upsertSQL writes a config service. A changed backend drops the stored cards: they are another seller's.
const upsertSQL = `insert into kontra.services (tenant_id, name, kind, backend, allow, tools, tasks_mode, sealed,
	retries, timeout_s, retention_s, annotations, seller_keys, session_idle_s, listed, source)
values ($1, $2, $3, $4::jsonb, $5::text[], $6::jsonb, $7::text, $8, $9, $10, $11, $12::jsonb, $13::jsonb, $14, $15, 'config')
on conflict (tenant_id, name) do update set
	kind = excluded.kind, backend = excluded.backend, allow = excluded.allow, tools = excluded.tools,
	tasks_mode = excluded.tasks_mode, sealed = excluded.sealed, retries = excluded.retries,
	timeout_s = excluded.timeout_s, retention_s = excluded.retention_s, annotations = excluded.annotations,
	seller_keys = excluded.seller_keys, session_idle_s = excluded.session_idle_s, listed = excluded.listed,
	source = 'config', disabled_at = null, updated_at = now(),
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
		d.timeoutS, d.retentionS, d.annotations, d.sellerKeys, d.sessionIdle, d.listed}
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
	sellerKeys  bool   // seller_keys set (not SQL NULL, not JSON null)
	upstream    []byte
	fetchedAt   *time.Time
}

const selectServices = `select s.tenant_id, t.slug, s.name, s.kind, s.backend::text, s.allow, s.listed,
	s.disabled_at is not null, s.timeout_s, s.retries, s.retention_s, s.annotations::text, s.sealed,
	coalesce(s.seller_keys <> 'null'::jsonb, false), s.upstream_card, s.card_fetched_at
  from kontra.services s join kontra.tenants t on t.id = s.tenant_id
 where t.deleted_at is null and (t.expires_at is null or t.expires_at > now())`

func scanRow(r pgx.CollectableRow) (row, error) {
	var x row
	var backend, annots string
	err := r.Scan(&x.tenantID, &x.tenant, &x.name, &x.kind, &backend, &x.allow, &x.listed, &x.disabled,
		&x.timeoutS, &x.retries, &x.retentionS, &annots, &x.sealed, &x.sellerKeys, &x.upstream, &x.fetchedAt)
	x.backend, x.annotations = []byte(backend), []byte(annots)
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
		a.keeperKey == b.keeperKey && slices.Equal(a.allowRaw, b.allowRaw)
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
		if err := refusable(x, b); err != nil {
			return nil, err
		}
	}
	e := &entry{
		ref: store.ServiceRef{TenantID: x.tenantID, Tenant: x.tenant, Name: x.name, Kind: x.kind,
			Timeout: time.Duration(x.timeoutS) * time.Second, Retries: x.retries, Retention: time.Duration(x.retentionS) * time.Second},
		allow: allow, allowRaw: slices.Clone(x.allow), listed: x.listed, disabled: x.disabled, backend: b,
		backendJS: string(x.backend),
	}
	// Only an enabled a2a service with a direct backend has a card the gate fetches in M1: a tunnel
	// service's card comes through its sessions (M2), an MCP service has none (spec §3).
	if !x.disabled && x.kind == config.KindA2A && b.Kind == config.KindA2A {
		exts, err := cardExtensions(x.annotations)
		if err != nil {
			return nil, err
		}
		in := card.Input{PublicURL: s.publicURL, ServicePath: ServicePath(x.tenant, x.name, s.multi),
			OnlyAnonymous: allow.OnlyAnonymous(), CardExtensions: exts}
		e.keeperKey = keeperKey(e.backendJS, in)
		if old != nil && old.keeper != nil && old.keeperKey == e.keeperKey {
			e.keeper, e.stop = old.keeper, old.stop
			old.stop = nil // handed over: dropping old must not stop it
		} else {
			if old != nil && old.stop != nil {
				old.stop()
				old.stop = nil
			}
			k, err := s.newKeeper(e, b, in, x)
			if err != nil {
				return nil, err
			}
			e.keeper = k
			if s.running != nil {
				e.stop = s.start(k)
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
	e.served = &a2ahttp.Service{Ref: e.ref, Allow: allow, Listed: x.listed, Disabled: x.disabled, Cards: cards}
	return e, nil
}

// refusable applies to every row the refusals config.Validate makes for a services: entry, so a row the
// operator's control plane writes (source db, spec §13.5) cannot bypass them and fail open.
//
// SPEC-CONFLICT (the configuration's resolutions, config/validate.go): sealing needs backend tunnel (the
// recipients are the service's enrolled node keys and the hosting node unseals, ext-sealed.md §1), and this
// build, which does not seal, would relay a sealed direct service in plaintext; seller card signatures are
// not verified until M3, so a direct a2a service with seller_keys would serve its card unverified, where
// spec §3 makes it unavailable. Such a row is left out rather than served.
func refusable(x row, b backendSpec) error {
	if b.direct() && x.sealed != string(config.SealedOff) {
		return fmt.Errorf("sealed %q on a direct backend: sealing needs backend tunnel (ext-sealed.md §1), and this build would relay the service in plaintext", x.sealed)
	}
	if x.sellerKeys && b.direct() && x.kind == config.KindA2A {
		return errors.New("seller_keys on a direct a2a service: seller card signatures are not verified in this release (M3), and the card would be served unverified (spec §3)")
	}
	return nil
}

func keeperKey(backendJS string, in card.Input) string {
	b, _ := json.Marshal(struct {
		Backend json.RawMessage
		In      card.Input
	}{json.RawMessage(backendJS), in})
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

// newKeeper builds the card keeper of a direct a2a service, seeded from its stored upstream card.
func (s *Services) newKeeper(e *entry, b backendSpec, in card.Input, x row) (*card.Keeper, error) {
	be, err := upstream.NewDirect(b.URL, b.AllowPrivate)
	if err != nil {
		return nil, err
	}
	ref, backendJS := e.ref, e.backendJS
	opts := []card.KeeperOption{
		card.WithPersist(func(ctx context.Context, sv *card.Served, _ time.Time) error {
			return s.persistCard(ctx, ref, backendJS, sv)
		}),
		card.WithFetch(func(ctx context.Context, b upstream.Backend) (*upstream.Fetched, error) {
			f, err := upstream.FetchCard(ctx, b)
			if err == nil {
				s.touchCard(ctx, ref, backendJS, f.Raw)
			}
			return f, err
		}),
	}
	k := card.NewKeeper(be, in, append(opts, s.keeperOpt...)...)
	if len(x.upstream) > 0 && x.fetchedAt != nil {
		if err := k.Seed(x.upstream, *x.fetchedAt); err != nil {
			// A stored card this build cannot serve: the keeper fetches a fresh one.
			s.logger.Warn("services: the stored card cannot be served; fetching it again", "service", ref.Tenant+"/"+ref.Name, "error", err)
		}
	}
	return k, nil
}

// persistCard records the card a keeper serves (its persist hook, called after a good fetch until it
// succeeds): first the upstream bytes, verbatim, and the fetch time, which are what a restarted replica
// seeds from; then the served card, a best-effort copy (json, D49): a body Postgres refuses as json (a
// data exception: text that is not UTF-8, a NUL character) leaves services.card NULL, as the audit
// proposed, and is not an error. Any other failure is returned, and the keeper calls again at its next good
// fetch. The backend guard keeps a keeper of a URL the configuration has just replaced from writing its
// seller's card under the new backend; the upstream_card guard keeps a served card next to the bytes it was
// produced from when another replica records newer ones meanwhile.
func (s *Services) persistCard(ctx context.Context, ref store.ServiceRef, backendJS string, sv *card.Served) error {
	tag, err := s.db.Exec(ctx, `update kontra.services set upstream_card = $3, card_fetched_at = now(),
			updated_at = case when upstream_card is distinct from $3 then now() else updated_at end
		 where tenant_id = $1 and name = $2 and backend = $4::jsonb`, ref.TenantID, ref.Name, sv.Upstream, backendJS)
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

// start runs k under the registry's run context; s.mu is held and s.running set.
func (s *Services) start(k *card.Keeper) context.CancelFunc {
	ctx, cancel := context.WithCancel(s.running)
	go k.Run(ctx)
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
			e.stop = s.start(e.keeper)
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
// its way.
func (s *Services) WaitCards(ctx context.Context) []string {
	for {
		var missing []string
		s.mu.RLock()
		for key, e := range s.byName {
			if e.keeper != nil {
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

// Backend implements relay.Backends: the direct backend and the upstream card Info a dispatch or a
// forward_cancel uses, from the service's keeper (card.Keeper.Dispatch). No usable card is an error, and
// the dispatch fails service_unavailable (spec §6.4).
func (s *Services) Backend(_ context.Context, svc store.ServiceRef) (upstream.Backend, upstream.Info, error) {
	e, err := s.live(svc.TenantID, svc.Name)
	if err != nil {
		return nil, upstream.Info{}, err
	}
	if e.keeper == nil {
		return nil, upstream.Info{}, fmt.Errorf("gate: service %s/%s has no card this gate fetches (backend %s)", e.ref.Tenant, e.ref.Name, e.backend.Kind)
	}
	return e.keeper.Dispatch()
}
