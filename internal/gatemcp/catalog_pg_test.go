package gatemcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/mcprelay"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/tenant"
)

type catalogEnv struct {
	t    *testing.T
	pool *pgxpool.Pool
	team uuid.UUID
	acme uuid.UUID
	anon uuid.UUID
}

func newCatalogEnv(t *testing.T) *catalogEnv {
	t.Helper()
	e := &catalogEnv{t: t, pool: pgtest.New(t)}
	ctx := context.Background()
	var err error
	if e.team, err = tenant.EnsureTeam(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	e.acme, e.anon = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	e.exec(`insert into kontra.tenants (id, slug, kind) values ($1, 'acme', 'account')`, e.acme)
	e.exec(`insert into kontra.tenants (id, slug, kind, expires_at, created_from)
		values ($1, 'anon-0123456789abcdef', 'anonymous', now() + interval '1 hour', '192.0.2.0/24')`, e.anon)
	return e
}

func (e *catalogEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// service inserts a services row: kind, backend JSON, allow, listed, annotations, card, disabled.
func (e *catalogEnv) service(tenantID uuid.UUID, name, kind, backend string, allow []string, listed bool, annotations, card string, disabled bool) {
	e.t.Helper()
	if annotations == "" {
		annotations = "{}"
	}
	var c any
	if card != "" {
		c = card
	}
	e.exec(`insert into kontra.services (tenant_id, name, kind, backend, allow, listed, annotations, card, source, disabled_at)
		values ($1, $2, $3, $4::jsonb, $5, $6, $7::jsonb, $8::json, 'db', case when $9 then now() end)`,
		tenantID, name, kind, backend, allow, listed, annotations, c, disabled)
}

const (
	directA2A = `{"a2a": {"url": "http://127.0.0.1:1"}}`
	directMCP = `{"mcp": {"url": "http://127.0.0.1:1/mcp"}}`
	tunnelled = `{"tunnel": {}}`
)

func keyPrincipal(tenantID uuid.UUID, slug, prefix string) auth.Principal {
	return auth.Principal{Kind: auth.KindKey, ID: "key:" + prefix, TenantID: &tenantID, TenantSlug: slug,
		BuyerID: uuid.Must(uuid.NewV7()), Via: auth.ViaAPIKey, Scopes: []auth.Scope{auth.ScopeHire}}
}

// mcpServices is the MCP relay's view of the services the catalog's tests describe.
type mcpServices map[string]*mcprelay.Service

func (m mcpServices) MCPService(_ context.Context, _, name string) (*mcprelay.Service, error) {
	return m[name], nil
}
func (m mcpServices) MCPServiceByID(_ context.Context, _ uuid.UUID, name string) (*mcprelay.Service, error) {
	return m[name], nil
}

type countingWarmer struct{ n atomic.Int32 }

func (w *countingWarmer) WarmTools(_ *mcprelay.Service, _ auth.Principal, info func(json.RawMessage)) {
	w.n.Add(1)
	info(json.RawMessage(`{"name":"web-server","title":"The web server","version":"1"}`))
}

// search and describe on a single-tenant gate (§12.2, D271): only the services whose allow: admits the principal by
// an entry other than anonymous, plus listed ones, never a disabled one; ordered by ref, 50 a page, the cursor
// granting nothing; query matches the ref, the name or the description (the served card's, else search.description)
// case-insensitively; kind and price filter (free: no price_model); available is a direct backend or a session of a
// live replica. describe of a ref search would not return is the unknown-service error; an MCP service's cold tools
// cache answers toolsCached false and starts one drain per service per 10 minutes, a warm one its filtered tools.
func TestCatalogSingleTenant(t *testing.T) {
	e := newCatalogEnv(t)
	ctx := context.Background()
	org := []string{"org:default"}
	e.service(e.team, "hello", "a2a", directA2A, org, false, `{"search":{"price":"1 EUR a task","price_model":"per_task"}}`,
		`{"name":"hello","description":"Says hello"}`, false)
	e.service(e.team, "pub", "a2a", directA2A, []string{"anonymous"}, false, "", "", false)
	e.service(e.team, "listedpub", "a2a", directA2A, []string{"anonymous"}, true, "", "", false)
	e.service(e.team, "web", "mcp", directMCP, org, false, `{"search":{"description":"Web tools"}}`, "", false)
	e.service(e.team, "nodeonly", "a2a", tunnelled, org, false, "", "", false)
	e.service(e.team, "off", "a2a", directA2A, org, false, "", "", true)
	e.service(e.team, "other", "a2a", directA2A, []string{"key:kk_zz_*"}, false, "", "", false)
	for i := range 55 {
		e.service(e.team, fmt.Sprintf("s%03d", i), "a2a", directA2A, org, false, "", "", false)
	}
	// Another tenant's services never show on a single-tenant gate.
	e.service(e.acme, "hello", "a2a", directA2A, []string{"anonymous"}, true, "", "", false)

	tools := mcprelay.NewToolsCache()
	warm := &countingWarmer{}
	webSvc := &mcprelay.Service{}
	webSvc.Tools = mcprelay.Policy{Deny: []string{"secret"}}
	webSvc.ToolMeta = map[string]map[string]json.RawMessage{"*": {"sh.kontra/price": json.RawMessage(`"1 EUR"`)}}
	c := NewCatalog(CatalogConfig{DB: e.pool, PublicURL: "https://gate.example/", MCP: mcpServices{"web": webSvc}, Tools: tools, Warm: warm})
	p := keyPrincipal(e.team, tenant.TeamSlug, "kk_default_ab12cd34")

	var all []ServiceEntry
	res, err := c.Search(ctx, p, SearchArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Services) != PageSize || res.NextCursor == "" {
		t.Fatalf("page 1: %d services, cursor %q", len(res.Services), res.NextCursor)
	}
	all = append(all, res.Services...)
	res2, err := c.Search(ctx, p, SearchArgs{Cursor: res.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if res2.NextCursor != "" {
		t.Errorf("page 2 has a cursor %q", res2.NextCursor)
	}
	all = append(all, res2.Services...)
	var refs []string
	for _, s := range all {
		refs = append(refs, s.Ref)
	}
	want := []string{"hello", "listedpub", "nodeonly"}
	for i := range 55 {
		want = append(want, fmt.Sprintf("s%03d", i))
	}
	want = append(want, "web")
	if fmt.Sprint(refs) != fmt.Sprint(want) {
		t.Fatalf("search returned %v\nwant %v", refs, want)
	}
	byRef := map[string]ServiceEntry{}
	for _, s := range all {
		byRef[s.Ref] = s
	}
	if h := byRef["hello"]; h.Kind != "a2a" || h.Description != "Says hello" || h.Price != "1 EUR a task" || !h.Available || h.Name != "hello" {
		t.Errorf("hello: %+v", h)
	}
	if w := byRef["web"]; w.Kind != "mcp" || w.Description != "Web tools" || !w.Available {
		t.Errorf("web: %+v", w)
	}
	if n := byRef["nodeonly"]; n.Available {
		t.Errorf("a tunnel service no session serves is available: %+v", n)
	}
	if _, err := c.Search(ctx, p, SearchArgs{Cursor: "not-a-cursor"}); err == nil {
		t.Error("a cursor the gate did not mint was accepted")
	}

	for _, tc := range []struct {
		args SearchArgs
		want string
	}{
		{SearchArgs{Kind: "mcp"}, "[web]"},
		{SearchArgs{Query: "HELLO"}, "[hello]"},
		{SearchArgs{Query: "says"}, "[hello]"},
		{SearchArgs{Query: "web t"}, "[web]"},
		{SearchArgs{Price: "per_task"}, "[hello]"},
		{SearchArgs{Price: "per_call"}, "[]"},
		{SearchArgs{Query: "s05", Price: "free"}, "[s050 s051 s052 s053 s054]"},
	} {
		r, err := c.Search(ctx, p, tc.args)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, s := range r.Services {
			got = append(got, s.Ref)
		}
		if fmt.Sprint(got) != tc.want && !(tc.want == "[]" && len(got) == 0) {
			t.Errorf("search %+v = %v, want %s", tc.args, got, tc.want)
		}
	}

	// describe.
	d, err := c.Describe(ctx, p, Ref{Name: "hello"})
	if err != nil || d.Ref != "hello" || d.Kind != "a2a" || string(d.Card) != `{"name":"hello","description":"Says hello"}` ||
		d.Sealed || !d.Available || len(d.URLs) != 1 || d.URLs[0] != "https://gate.example/agents/hello" || d.ToolsCached != nil {
		t.Errorf("describe hello: %+v %v", d, err)
	}
	for _, ref := range []string{"pub", "off", "other", "nope"} {
		if _, err := c.Describe(ctx, p, Ref{Name: ref}); !errors.Is(err, ErrUnknownService) {
			t.Errorf("describe %s: %v, want the unknown-service error", ref, err)
		}
	}
	if _, err := c.Describe(ctx, p, Ref{Name: "listedpub"}); err != nil {
		t.Errorf("describe a listed service: %v", err)
	}
	d, err = c.Describe(ctx, p, Ref{Name: "web"})
	if err != nil || d.ToolsCached == nil || *d.ToolsCached || d.Tools != nil || d.URLs[0] != "https://gate.example/mcp/web" {
		t.Fatalf("describe web, cold: %+v %v", d, err)
	}
	if _, err := c.Describe(ctx, p, Ref{Name: "web"}); err != nil || warm.n.Load() != 1 {
		t.Fatalf("a second describe within 10 minutes started %d drains, want 1 (%v)", warm.n.Load(), err)
	}
	// The drain's serverInfo describes an MCP service without a search.description annotation.
	e.exec(`update kontra.services set annotations = '{}' where tenant_id = $1 and name = 'web'`, e.team)
	if r, _ := c.Search(ctx, p, SearchArgs{Kind: "mcp"}); len(r.Services) != 1 || r.Services[0].Description != "The web server" {
		t.Errorf("an MCP service described by its serverInfo: %+v", r.Services)
	}
	// Warm the cache as a client's session would: describe then shows the relay's view (denied tools dropped, tool_meta).
	key := mcprelay.ToolsKey{TenantID: e.team, Service: "web"}
	if _, err := tools.List(ctx, key, mcpwire.EraLegacy, mcprelay.ToolView{}, func(context.Context, string) (json.RawMessage, error) {
		return json.RawMessage(`{"tools":[{"name":"fetch","inputSchema":{"type":"object"}},{"name":"secret","inputSchema":{"type":"object"}}]}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	d, err = c.Describe(ctx, p, Ref{Name: "web"})
	if err != nil || d.ToolsCached == nil || !*d.ToolsCached || len(d.Tools) != 1 {
		t.Fatalf("describe web, warm: %+v %v", d, err)
	}
	if got := string(d.Tools[0]); got != `{"name":"fetch","inputSchema":{"type":"object"},"_meta":{"sh.kontra/price":"1 EUR"}}` {
		t.Errorf("the described tool: %s", got)
	}

	// hire reaches a service the allow: list admits by any entry (anonymous included), not one it does not admit.
	for ref, ok := range map[string]bool{"pub": true, "hello": true, "other": false, "off": false} {
		s, err := c.Hireable(ctx, p, Ref{Name: ref})
		if err != nil || (s != nil) != ok {
			t.Errorf("Hireable(%s) = %v, %v; want %v", ref, s, err, ok)
		}
	}
}

// search on a multi-tenant gate: refs are <tenant>/<name>; an org: entry admits its tenant's keys; an anonymous
// tenant's services never appear and cannot be described, listed or not (spec/tunnel.md §6.2), though a public one
// is still hired by ref.
func TestCatalogMultiTenant(t *testing.T) {
	e := newCatalogEnv(t)
	ctx := context.Background()
	e.service(e.acme, "agent", "a2a", directA2A, []string{"org:acme"}, false, "", "", false)
	e.service(e.acme, "public", "a2a", directA2A, []string{"anonymous"}, true, "", "", false)
	e.service(e.anon, "laptop", "a2a", tunnelled, []string{"org:anon-0123456789abcdef", "anonymous"}, true, "", "", false)
	e.service(e.team, "teamonly", "a2a", directA2A, []string{"org:default"}, false, "", "", false)
	c := NewCatalog(CatalogConfig{DB: e.pool, Multi: true, PublicURL: "https://gate.example"})
	acme := keyPrincipal(e.acme, "acme", "kk_acme_ab12cd34")
	r, err := c.Search(ctx, acme, SearchArgs{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range r.Services {
		got = append(got, s.Ref)
	}
	if fmt.Sprint(got) != "[acme/agent acme/public]" {
		t.Fatalf("search as acme: %v", got)
	}
	if _, err := c.Describe(ctx, acme, Ref{Tenant: "anon-0123456789abcdef", Name: "laptop"}); !errors.Is(err, ErrUnknownService) {
		t.Errorf("describe an anonymous tenant's service: %v", err)
	}
	if s, err := c.Hireable(ctx, acme, Ref{Tenant: "anon-0123456789abcdef", Name: "laptop"}); err != nil || s == nil {
		t.Errorf("hire a public anonymous service by ref: %v %v", s, err)
	}
	if d, err := c.Describe(ctx, acme, Ref{Tenant: "acme", Name: "agent"}); err != nil || d.URLs[0] != "https://gate.example/agents/acme/agent" {
		t.Errorf("describe acme/agent: %+v %v", d, err)
	}
	other := keyPrincipal(e.team, tenant.TeamSlug, "kk_default_zz")
	if r, _ := c.Search(ctx, other, SearchArgs{}); len(r.Services) != 2 || r.Services[0].Ref != "acme/public" || r.Services[1].Ref != "default/teamonly" {
		t.Errorf("search as the team: %+v", r.Services)
	}
}

// oidc:* on a multi-tenant gate (D301): a listing whose allow: is [key:*, oidc:*] is searched and hired by every
// signed-in user of the configured issuer (an MCP OAuth principal: no tenant, via mcp) and every API key, and by no
// one else; [oidc:*] alone is not an API key's; neither is the anonymous principal's. An anonymous entry still admits
// everyone to hire, without showing the service.
func TestCatalogOIDCAny(t *testing.T) {
	e := newCatalogEnv(t)
	ctx := context.Background()
	e.service(e.acme, "listing", "a2a", directA2A, []string{"key:*", "oidc:*"}, false, "", "", false)
	e.service(e.acme, "signedin", "a2a", directA2A, []string{"oidc:*"}, false, "", "", false)
	e.service(e.acme, "keys", "a2a", directA2A, []string{"key:*"}, false, "", "", false)
	e.service(e.acme, "open", "a2a", directA2A, []string{"anonymous"}, false, "", "", false)
	c := NewCatalog(CatalogConfig{DB: e.pool, Multi: true, PublicURL: "https://gate.example"})
	user := auth.Principal{Kind: auth.KindOIDC, ID: "oidc:sub:user-7", BuyerID: uuid.Must(uuid.NewV7()), Via: auth.ViaMCP}
	key := keyPrincipal(e.team, tenant.TeamSlug, "kk_default_ab12cd34")
	for _, tc := range []struct {
		who    string
		p      auth.Principal
		search string
		hire   map[string]bool
	}{
		{"a signed-in user", user, "[acme/listing acme/signedin]", map[string]bool{"listing": true, "signedin": true, "keys": false, "open": true}},
		{"an API key", key, "[acme/keys acme/listing]", map[string]bool{"listing": true, "signedin": false, "keys": true, "open": true}},
		{"the anonymous principal", auth.Anonymous(uuid.Must(uuid.NewV7())), "[]", map[string]bool{"listing": false, "signedin": false, "keys": false, "open": true}},
	} {
		r, err := c.Search(ctx, tc.p, SearchArgs{})
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, s := range r.Services {
			got = append(got, s.Ref)
		}
		if fmt.Sprint(got) != tc.search {
			t.Errorf("search as %s: %v, want %s", tc.who, got, tc.search)
		}
		for name, ok := range tc.hire {
			s, err := c.Hireable(ctx, tc.p, Ref{Tenant: "acme", Name: name})
			if err != nil || (s != nil) != ok {
				t.Errorf("Hireable(acme/%s) as %s = %v, %v; want %v", name, tc.who, s != nil, err, ok)
			}
		}
	}
}

// One search request reads at most SearchExamine rows (M4 WP-04 review): a principal who sees none of more services
// than that gets an early page, empty here, whose nextCursor is the last row read, never a scan of the whole table;
// following the cursors reaches what it may see. Shown with a bound of 10 rows read 4 at a time.
func TestSearchBoundsTheRowsOneRequestReads(t *testing.T) {
	e := newCatalogEnv(t)
	ctx := context.Background()
	for i := range 25 {
		e.service(e.team, fmt.Sprintf("hidden%02d", i), "a2a", directA2A, []string{"key:kk_zz_*"}, false, "", "", false)
	}
	e.service(e.team, "visible", "a2a", directA2A, []string{"org:default"}, false, "", "", false)
	c := NewCatalog(CatalogConfig{DB: e.pool, PublicURL: "https://gate.example"})
	c.batch, c.examine = 4, 10
	p := keyPrincipal(e.team, tenant.TeamSlug, "kk_default_ab12cd34")

	var pages []string
	cursor := ""
	for range 5 {
		res, err := c.Search(ctx, p, SearchArgs{Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		var refs []string
		for _, s := range res.Services {
			refs = append(refs, s.Ref)
		}
		at := ""
		if res.NextCursor != "" {
			if at, err = decodeCursor(res.NextCursor); err != nil {
				t.Fatal(err)
			}
		}
		pages = append(pages, fmt.Sprintf("%v@%s", refs, at))
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	// Each request reads 10 rows; the third reads hidden20..24 and visible, and ends the table.
	if want := "[[]@hidden09 []@hidden19 [visible]@]"; fmt.Sprint(pages) != want {
		t.Fatalf("search pages %v\nwant %s", pages, want)
	}
}

// annotations.search.hidden (§12.2, D321; issue #7): Cloud's unlisted listing, [key:*, oidc:*, anonymous] with listed
// false, is left out of every principal's search once hidden, an API key's and a signed-in user's included, while
// describe and hire by ref still answer as allow: decides; a listed service hidden is left out too, and a hidden
// member that is not the boolean true (false, the string "true") hides nothing.
func TestSearchHidden(t *testing.T) {
	e := newCatalogEnv(t)
	ctx := context.Background()
	unlisted := []string{"key:*", "oidc:*", "anonymous"}
	e.service(e.acme, "link", "a2a", directA2A, unlisted, false, `{"search": {"hidden": true}}`, "", false)
	e.service(e.acme, "shown", "a2a", directA2A, unlisted, false, `{"search": {"hidden": false}}`, "", false)
	e.service(e.acme, "loose", "a2a", directA2A, unlisted, false, `{"search": {"hidden": "true"}}`, "", false)
	e.service(e.acme, "listedhidden", "a2a", directA2A, []string{"anonymous"}, true, `{"search": {"hidden": true, "price_model": "per_task"}}`, "", false)
	c := NewCatalog(CatalogConfig{DB: e.pool, Multi: true, PublicURL: "https://gate.example"})
	user := auth.Principal{Kind: auth.KindOIDC, ID: "oidc:sub:user-7", BuyerID: uuid.Must(uuid.NewV7()), Via: auth.ViaOIDC}
	key := keyPrincipal(e.team, tenant.TeamSlug, "kk_default_ab12cd34")
	for _, tc := range []struct {
		who      string
		p        auth.Principal
		search   string
		describe bool
	}{
		{"a signed-in user", user, "[acme/loose acme/shown]", true},
		{"an API key", key, "[acme/loose acme/shown]", true},
		{"the anonymous principal", auth.Anonymous(uuid.Must(uuid.NewV7())), "[]", false},
	} {
		r, err := c.Search(ctx, tc.p, SearchArgs{})
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, s := range r.Services {
			got = append(got, s.Ref)
		}
		if fmt.Sprint(got) != tc.search {
			t.Errorf("search as %s: %v, want %s", tc.who, got, tc.search)
		}
		if _, err := c.Describe(ctx, tc.p, Ref{Tenant: "acme", Name: "link"}); (err == nil) != tc.describe {
			t.Errorf("describe acme/link as %s: %v, want described %v", tc.who, err, tc.describe)
		}
		if s, err := c.Hireable(ctx, tc.p, Ref{Tenant: "acme", Name: "link"}); err != nil || s == nil {
			t.Errorf("hire acme/link as %s: %v %v", tc.who, s, err)
		}
	}
	// The listed one hidden: described by ref as a listed service is, never searched.
	if _, err := c.Describe(ctx, auth.Anonymous(uuid.Must(uuid.NewV7())), Ref{Tenant: "acme", Name: "listedhidden"}); err != nil {
		t.Errorf("describe a listed hidden service: %v", err)
	}
}
