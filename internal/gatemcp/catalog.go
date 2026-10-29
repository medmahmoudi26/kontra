package gatemcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/mcprelay"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/tenant"
)

// PageSize is search's page (D271).
const PageSize = 50

// DrainEvery bounds describe's background drain of a cold MCP tools cache: at most one per service per period,
// never on the request's time (§12.2, §11.3, D271).
const DrainEvery = 10 * time.Minute

// Catalog answers search and describe for a principal from the services table, the tunnels registry and the
// caches the relays keep (the served card, the MCP tools cache). It never makes an upstream request on a request's
// time (§12.2).
//
// Visibility (§12.2): a service whose allow: admits the principal by an entry other than anonymous, or one the
// operator marks listed; never a service of an anonymous tenant (spec/tunnel.md §6.2), never a disabled one, nor one
// of a tenant that has ended. hire reaches a service by ref when its allow: admits the principal at all (any entry,
// anonymous included), as the A2A route does.
type Catalog struct {
	db        *pgxpool.Pool
	multi     bool
	publicURL string
	mcp       mcprelay.Services    // the MCP relay's view of a service: its tools policy and tool_meta (§11.3, §13.4)
	tools     *mcprelay.ToolsCache // the drained tools/list of each MCP service (§11.3), read without draining
	warm      ToolsWarmer          // starts a background drain of a cold cache; nil starts none
	log       *slog.Logger

	mu     sync.Mutex
	warmed map[mcprelay.ToolsKey]time.Time // the last drain describe started per service (DrainEvery)
	infos  map[mcprelay.ToolsKey]string    // an MCP upstream's serverInfo title or name, as its last drain read it
	now    func() time.Time

	batch   int // rows one query of a search reads (searchBatch)
	examine int // rows one search request reads at most (SearchExamine)
}

// ToolsWarmer drains an MCP service's tools/list into the relay's cache in the background, on its own context and
// upstream session, for a principal the service admits (§11.3; *mcprelay.Handler); info gets the upstream's
// serverInfo when its initialize answers one. It returns at once.
type ToolsWarmer interface {
	WarmTools(svc *mcprelay.Service, p auth.Principal, info func(serverInfo json.RawMessage))
}

// CatalogConfig configures a Catalog.
type CatalogConfig struct {
	DB        *pgxpool.Pool
	Multi     bool   // tenancy: multi: refs are <tenant>/<name>
	PublicURL string // the URLs describe gives
	MCP       mcprelay.Services
	Tools     *mcprelay.ToolsCache
	Warm      ToolsWarmer
	Logger    *slog.Logger
}

// NewCatalog reads from cfg.DB.
func NewCatalog(cfg CatalogConfig) *Catalog {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Catalog{db: cfg.DB, multi: cfg.Multi, publicURL: strings.TrimRight(cfg.PublicURL, "/"), mcp: cfg.MCP,
		tools: cfg.Tools, warm: cfg.Warm, log: cfg.Logger, warmed: map[mcprelay.ToolsKey]time.Time{},
		infos: map[mcprelay.ToolsKey]string{}, now: time.Now, batch: searchBatch, examine: SearchExamine}
}

// service is one services row as the catalog reads it.
type service struct {
	ref         store.ServiceRef
	allow       auth.AllowList
	listed      bool
	anonymous   bool // of an anonymous tenant (spec/tunnel.md §6): never in search or describe
	tunnelled   bool
	available   bool
	card        json.RawMessage // the served card (A2A), nil when none is stored
	description string          // search.description annotation
	price       string          // search.price annotation
	priceModel  string          // search.price_model annotation
}

// refString is the service's ref on this gate.
func (c *Catalog) refString(s *service) string {
	if c.multi {
		return s.ref.Tenant + "/" + s.ref.Name
	}
	return s.ref.Name
}

// visible reports whether search shows s to p (§12.2): never a service of an anonymous tenant.
func visible(s *service, p auth.Principal) bool {
	return !s.anonymous && (s.listed || AdmitsNamed(s.allow, p))
}

// AdmitsNamed reports whether allow admits p by an entry other than anonymous: the allow list without its anonymous
// entries (§12.2's search visibility).
func AdmitsNamed(allow auth.AllowList, p auth.Principal) bool {
	return allow.WithoutAnonymous().Admits(p)
}

// serviceColumns read a services row with its tenant, its search annotations, its stored card and whether a session
// of a live replica serves it (tunnelgate's candidate query) or it has a direct backend.
const serviceColumns = `t.id, t.slug, s.name, s.kind, s.allow, s.listed, t.kind = 'anonymous', s.backend ? 'tunnel', s.timeout_s,
	s.retries, s.retention_s,
	s.sealed, s.card::text, coalesce(s.annotations -> 'search' ->> 'description', ''),
	coalesce(s.annotations -> 'search' ->> 'price', ''), coalesce(s.annotations -> 'search' ->> 'price_model', ''),
	not (s.backend ? 'tunnel') or exists (select 1 from kontra.tunnels tu
		join kontra.tunnel_keys k on k.key_id = tu.key_id and k.revoked_at is null and k.revoked_reason is distinct from 'rotated'
		join kontra.key_services ks on ks.key_id = tu.key_id and ks.service = s.name and ks.revoked_at is null
		join kontra.gate_replicas r on r.replica_id = tu.replica_id and r.last_seen_at > now() - interval '30 seconds' and r.draining_at is null
		where tu.tenant_id = s.tenant_id and s.name = any (tu.services))`

// serviceFrom: the enabled services of live tenants; on a single-tenant gate, the team tenant's only.
const serviceFrom = ` from kontra.services s join kontra.tenants t on t.id = s.tenant_id
	where s.disabled_at is null and t.deleted_at is null and (t.expires_at is null or t.expires_at > now())`

func scanService(row pgx.Row) (*service, error) {
	var (
		s                    service
		allow                []string
		timeoutS, retentionS int64
		sealed               string
		card                 *string
	)
	err := row.Scan(&s.ref.TenantID, &s.ref.Tenant, &s.ref.Name, &s.ref.Kind, &allow, &s.listed, &s.anonymous, &s.tunnelled, &timeoutS,
		&s.ref.Retries, &retentionS, &sealed, &card, &s.description, &s.price, &s.priceModel, &s.available)
	if err != nil {
		return nil, err
	}
	s.ref.Timeout, s.ref.Retention = time.Duration(timeoutS)*time.Second, time.Duration(retentionS)*time.Second
	s.ref.Sealed = store.SealedRef(sealed)
	if s.allow, err = auth.ParseAllowList(allow); err != nil {
		return nil, fmt.Errorf("gatemcp: service %s/%s: %w", s.ref.Tenant, s.ref.Name, err)
	}
	if card != nil && json.Valid([]byte(*card)) && *card != "null" {
		s.card = json.RawMessage(*card)
	}
	return &s, nil
}

// lookup is the service a ref names, nil when none: an enabled service of a live tenant (a single-tenant gate's
// team tenant). Visibility is the caller's to decide.
func (c *Catalog) lookup(ctx context.Context, ref Ref) (*service, error) {
	slug := ref.Tenant
	if !c.multi {
		slug = tenant.TeamSlug
	}
	s, err := scanService(c.db.QueryRow(ctx, `select `+serviceColumns+serviceFrom+` and t.slug = $1 and s.name = $2`, slug, ref.Name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gatemcp: look up service %s: %w", ref, err)
	}
	return s, nil
}

// Hireable is the service a hire's ref names when p may hire it (its allow: admits p, by any entry), nil otherwise:
// the unknown-service answer (§12.2).
func (c *Catalog) Hireable(ctx context.Context, p auth.Principal, ref Ref) (*service, error) {
	s, err := c.lookup(ctx, ref)
	if err != nil || s == nil || !s.allow.Admits(p) {
		return nil, err
	}
	return s, nil
}

// errBadCursor is a search cursor the gate did not mint.
var errBadCursor = badArgs("cursor is not one this gate gave")

// encodeCursor is search's keyset position after ref: opaque, granting nothing (every page is filtered again).
func encodeCursor(ref string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("s1:" + ref))
}

func decodeCursor(s string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || !bytes.HasPrefix(b, []byte("s1:")) {
		return "", errBadCursor
	}
	return string(b[3:]), nil
}

// searchBatch is how many rows one query of a search reads; a page filters them for the principal.
const searchBatch = 200

// SearchExamine bounds the rows one search request reads (five queries of searchBatch): a page is filtered for the
// principal after the read, so a principal who sees few of many services would otherwise make each request read the
// whole table. A request that reaches it answers what it found, fewer than PageSize services or none, with nextCursor
// at the last row it read (D293).
const SearchExamine = 1000

// Search answers search for p (D271): the services visible to p whose ref, name or description contains query
// (case-insensitively), of kind, whose price_model annotation equals price ("free": none); ordered by ref (byte
// order), PageSize a page, with an opaque keyset cursor; anonymous tenants never appear. One request reads at most
// SearchExamine rows: past them the page ends early, its nextCursor at the last row read.
func (c *Catalog) Search(ctx context.Context, p auth.Principal, args SearchArgs) (SearchResult, error) {
	after := ""
	if args.Cursor != "" {
		var err error
		if after, err = decodeCursor(args.Cursor); err != nil {
			return SearchResult{}, err
		}
	}
	refExpr := `s.name`
	scope := ` and t.slug = '` + tenant.TeamSlug + `'`
	if c.multi {
		refExpr, scope = `t.slug || '/' || s.name`, ` and t.kind <> 'anonymous'`
	}
	var kind any
	if args.Kind != "" {
		kind = args.Kind
	}
	query := strings.ToLower(args.Query)
	out := SearchResult{Services: []ServiceEntry{}}
	examined := 0
	for {
		limit := min(c.batch, c.examine-examined)
		rows, err := c.db.Query(ctx, `select `+serviceColumns+`, `+refExpr+serviceFrom+scope+`
			and ($1::text is null or s.kind = $1) and (`+refExpr+`) collate "C" > $2::text collate "C"
			order by (`+refExpr+`) collate "C" limit $3`, kind, after, limit)
		if err != nil {
			return SearchResult{}, fmt.Errorf("gatemcp: search: %w", err)
		}
		var batch []*service
		var refs []string
		for rows.Next() {
			var (
				s   service
				ref string
			)
			sp, err := scanService(scanTail{rows, &ref})
			if err != nil {
				rows.Close()
				return SearchResult{}, fmt.Errorf("gatemcp: search: %w", err)
			}
			s = *sp
			batch, refs = append(batch, &s), append(refs, ref)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return SearchResult{}, fmt.Errorf("gatemcp: search: %w", err)
		}
		for i, s := range batch {
			after = refs[i]
			if !visible(s, p) || !c.matches(s, query, args.Price) {
				continue
			}
			if len(out.Services) == PageSize {
				// One more visible service: the page ends at the last one it holds.
				out.NextCursor = encodeCursor(out.Services[PageSize-1].Ref)
				return out, nil
			}
			out.Services = append(out.Services, c.entry(s))
		}
		if len(batch) < limit {
			return out, nil
		}
		if examined += len(batch); examined >= c.examine {
			// The request's bound: the next page starts after the last row read (a cursor grants nothing).
			out.NextCursor = encodeCursor(after)
			return out, nil
		}
	}
}

// scanTail scans a row whose last column is extra, after serviceColumns.
type scanTail struct {
	rows  pgx.Rows
	extra *string
}

func (s scanTail) Scan(dest ...any) error { return s.rows.Scan(append(dest, s.extra)...) }

// matches applies search's query and price filters (kind is the query's).
func (c *Catalog) matches(s *service, query, price string) bool {
	switch {
	case price == "free" && s.priceModel != "":
		return false
	case price != "" && price != "free" && s.priceModel != price:
		return false
	}
	if query == "" {
		return true
	}
	for _, f := range []string{c.refString(s), s.ref.Name, c.describe(s)} {
		if strings.Contains(strings.ToLower(f), query) {
			return true
		}
	}
	return false
}

// describe is a service's description (D271): the served card's (A2A), else the operator's search.description
// annotation, else the MCP upstream's serverInfo title or name as this replica's last background drain read it
// (describe starts one; none is read on a request's time).
func (c *Catalog) describe(s *service) string {
	if len(s.card) > 0 {
		if d, ok := mcpwire.Member(s.card, "description"); ok {
			var str string
			if json.Unmarshal(d, &str) == nil && str != "" {
				return str
			}
		}
	}
	if s.description != "" || s.ref.Kind != config.KindMCP {
		return s.description
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.infos[mcprelay.ToolsKey{TenantID: s.ref.TenantID, Service: s.ref.Name}]
}

// learnInfo keeps an MCP upstream's serverInfo title, else its name.
func (c *Catalog) learnInfo(key mcprelay.ToolsKey, serverInfo json.RawMessage) {
	name := ""
	for _, k := range []string{"title", "name"} {
		if v, ok := mcpwire.Member(serverInfo, k); ok {
			var str string
			if json.Unmarshal(v, &str) == nil && str != "" {
				name = str
				break
			}
		}
	}
	if name == "" {
		return
	}
	c.mu.Lock()
	c.infos[key] = name
	c.mu.Unlock()
}

func (c *Catalog) entry(s *service) ServiceEntry {
	return ServiceEntry{Ref: c.refString(s), Kind: s.ref.Kind, Name: s.ref.Name, Description: c.describe(s),
		Available: s.available, Price: s.price}
}

// Describe answers describe for p: ErrUnknownService for a ref Search would not return to p; the served card (A2A)
// or the cached, filtered, annotated tools/list (MCP; without tools and toolsCached false when the cache is cold,
// starting one background drain per service per DrainEvery), sealed, available, and the URLs to call it directly.
// It never makes an upstream request on the request's time.
func (c *Catalog) Describe(ctx context.Context, p auth.Principal, ref Ref) (DescribeResult, error) {
	s, err := c.lookup(ctx, ref)
	if err != nil {
		return DescribeResult{}, err
	}
	if s == nil || !visible(s, p) {
		return DescribeResult{}, ErrUnknownService
	}
	out := DescribeResult{Ref: c.refString(s), Kind: s.ref.Kind, Sealed: s.ref.Sealed != "", Available: s.available}
	switch s.ref.Kind {
	case config.KindA2A:
		out.Card = s.card
		out.URLs = []string{c.publicURL + "/agents/" + c.refString(s)}
	case config.KindMCP:
		out.URLs = []string{c.publicURL + "/mcp/" + c.refString(s)}
		tools, cached := c.cachedTools(ctx, s, p)
		out.ToolsCached = &cached
		out.Tools = tools
	}
	return out, nil
}

// cachedTools is an MCP service's cached tools/list as the relay would show it to p's sessions (the service's
// policy and tool_meta, §11.3, §13.4), read without draining; a cold cache starts a background drain (at most one per
// service per DrainEvery) and answers cached false.
func (c *Catalog) cachedTools(ctx context.Context, s *service, p auth.Principal) ([]json.RawMessage, bool) {
	if c.tools == nil || c.mcp == nil {
		return nil, false
	}
	svc, err := c.mcp.MCPServiceByID(ctx, s.ref.TenantID, s.ref.Name)
	if err != nil || svc == nil || svc.Disabled {
		return nil, false
	}
	key := mcprelay.ToolsKey{TenantID: s.ref.TenantID, Service: s.ref.Name}
	rctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	// A nil Pager reads only what is cached: a cold cache is an error, and a drain in flight is waited for briefly.
	v := mcprelay.ToolView{Policy: svc.Tools, Meta: svc.ToolMeta}
	if list, err := c.tools.List(rctx, key, mcpwire.EraLegacy, v, nil); err == nil {
		return list, true
	}
	if list, _, err := c.tools.ListModern(rctx, key, v, nil); err == nil {
		return list, true
	}
	c.startDrain(key, svc, p)
	return nil, false
}

// startDrain asks the warmer for a background drain, at most once per service per DrainEvery.
func (c *Catalog) startDrain(key mcprelay.ToolsKey, svc *mcprelay.Service, p auth.Principal) {
	if c.warm == nil {
		return
	}
	c.mu.Lock()
	now := c.now()
	if last, ok := c.warmed[key]; ok && now.Sub(last) < DrainEvery {
		c.mu.Unlock()
		return
	}
	c.warmed[key] = now
	c.mu.Unlock()
	c.warm.WarmTools(svc, p, func(si json.RawMessage) { c.learnInfo(key, si) })
}

// tenantOf is the tenant id an mcp_sessions row of /mcp names for p: its tenant, uuid.Nil (NULL) without one.
func tenantOf(p auth.Principal) uuid.UUID {
	if p.TenantID == nil {
		return uuid.Nil
	}
	return *p.TenantID
}
