package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
)

// gate_mcp_search (spec/relay.md §16; §12.2; DM4-4, DM4-26). Multi-tenant: the services are rows (D116), refs are
// <tenant>/<name>, and the caller is buyer A, an API key of the team tenant default (principal org:default). Seeded:
// acme/pub (a2a, another tenant's, listed, annotations.search {price, price_model per_call}, §13.4), default/team (a2a, allow org:default), default/open
// (a2a, allow anonymous only, not listed), default/fs (mcp, its tools cached on replica A by a client's tools/list
// through the relay), default/cold (mcp, nothing cached), 60 default/page-NN (mcp) for paging, an anonymous tenant's
// service (allow anonymous and its own org), and the harness's default/slow. Asserted:
//
//   - search with no arguments returns, in ref order and 50 a page, every service but open and the anonymous tenant's;
//     the cursor continues with no duplicate and no gap;
//   - query matches a case-insensitive substring of the ref, the name or the description; kind and price filter by
//     equality (price=free: no price_model); every answer is ordered by ref;
//   - describe of open and of the anonymous tenant's service is the unknown-service error; describe of an A2A ref
//     answers the served card and the URLs to call it directly;
//   - describe of fs answers the cached tools with no upstream request during the call; describe of cold answers
//     toolsCached false and, however many describes are sent, starts exactly one background drain on that replica.
func TestGateMCPSearch(t *testing.T) {
	sellers := map[string]*Seller{"pub": StartSeller(t), "team": StartSeller(t), "open": StartSeller(t), "anon": StartSeller(t)}
	fsUp, coldUp, pageUp := mcptest.New(), mcptest.New(), mcptest.New()
	fsUp.Tools = []json.RawMessage{mcptest.Tool("read_file", ""), mcptest.Tool("list_dir", "")}
	coldUp.Tools = []json.RawMessage{mcptest.Tool("freeze", "")}
	h := start(t, Options{Multi: true})
	ctx := ctxFor(t, 10*time.Minute)
	a, b := h.Gates[0], h.Gates[1]

	anonSlug := "anon-" + randomHex(8)
	if _, err := h.Pool.Exec(ctx, `insert into kontra.tenants (id, slug, kind, expires_at, created_from)
		values ($1, $2, 'anonymous', now() + interval '1 hour', '198.51.100.0/24')`, uuid.Must(uuid.NewV7()), anonSlug); err != nil {
		t.Fatal(err)
	}
	svcs := []SeededService{
		{Tenant: "acme", Name: "pub", URL: sellers["pub"].URL, Listed: true, Annotations: map[string]any{"search": map[string]any{"price": "0.05 USD a call", "price_model": "per_call"}}},
		{Tenant: TeamTenant, Name: "team", URL: sellers["team"].URL},
		{Tenant: TeamTenant, Name: "open", URL: sellers["open"].URL, Allow: []string{"anonymous"}},
		{Tenant: TeamTenant, Name: "fs", Kind: "mcp", URL: doubleURL(t, fsUp)},
		{Tenant: TeamTenant, Name: "cold", Kind: "mcp", URL: doubleURL(t, coldUp)},
		{Tenant: anonSlug, Name: "shown", URL: sellers["anon"].URL, Allow: []string{"anonymous", "org:" + anonSlug}},
	}
	pageURL := doubleURL(t, pageUp)
	for i := range 60 {
		svcs = append(svcs, SeededService{Tenant: TeamTenant, Name: fmt.Sprintf("page-%02d", i), Kind: "mcp", URL: pageURL})
	}
	if err := h.SeedServices(ctx, svcs...); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"acme/pub", "default/team"} {
		if err := h.WaitA2A(ctx, ref, h.APIKey, 60*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range []string{"default/open", anonSlug + "/shown"} {
		if err := h.WaitA2A(ctx, ref, "", 60*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.WaitMCP(ctx, "default/page-59", h.APIKey, 30*time.Second); err != nil {
		t.Fatal(err)
	}

	visible := []string{"acme/pub", "default/cold", "default/fs"}
	for i := range 60 {
		visible = append(visible, fmt.Sprintf("default/page-%02d", i))
	}
	visible = append(visible, "default/slow", "default/team")
	s := OpenGateSession(t, ctx, a, h.APIKey, `{}`)
	sb := s.On(b)

	type found struct {
		Ref, Kind, Name, Description, Price string
		Available                           bool
	}
	search := func(c *GateSession, args string) ([]found, string) {
		t.Helper()
		r := Tool(t, c.Tool("search", args))
		var sc struct {
			Services   []found `json:"services"`
			NextCursor string  `json:"nextCursor"`
		}
		if r.IsError || json.Unmarshal(r.StructuredRaw, &sc) != nil {
			t.Fatalf("search %s: %+v", args, r)
		}
		if !strings.Contains(r.Text(), `"ref"`) && len(sc.Services) > 0 {
			t.Errorf("search %s: the text block does not carry the result: %q", args, r.Text())
		}
		return sc.Services, sc.NextCursor
	}
	refs := func(fs []found) []string {
		var out []string
		for _, f := range fs {
			out = append(out, f.Ref)
		}
		return out
	}
	// all pages a search returns, alternating replicas, checking 50 a page and ordering.
	all := func(args string) []string {
		t.Helper()
		var out []string
		cursor := ""
		for page := 0; ; page++ {
			a := args
			if cursor != "" {
				a = strings.TrimSuffix(args, "}")
				if a != "{" {
					a += ","
				}
				a += `"cursor":"` + cursor + `"}`
			}
			c := s
			if page%2 == 1 {
				c = sb
			}
			got, next := search(c, a)
			if next != "" && len(got) != 50 {
				t.Errorf("search %s page %d: %d services with a next cursor, want 50", args, page, len(got))
			}
			out = append(out, refs(got)...)
			if next == "" {
				return out
			}
			cursor = next
			if page > 10 {
				t.Fatalf("search %s: more than 10 pages", args)
			}
		}
	}

	// No arguments: every service but open and the anonymous tenant's, by ref, 50 a page, no duplicate.
	first, next := search(s, `{}`)
	if len(first) != 50 || next == "" {
		t.Fatalf("search's first page: %d services, next %q", len(first), next)
	}
	if got := all(`{}`); !slices.Equal(got, visible) {
		t.Errorf("search's pages:\n got %v\nwant %v", got, visible)
	}
	for _, f := range first {
		if !f.Available {
			t.Errorf("search: %s is not available (a direct backend)", f.Ref)
		}
	}
	if first[0].Ref != "acme/pub" || first[0].Kind != "a2a" || first[0].Name != "pub" || first[0].Price != "0.05 USD a call" || first[0].Description == "" {
		t.Errorf("search's acme/pub: %+v", first[0])
	}
	// Filters.
	for _, c := range []struct {
		args string
		want []string
	}{
		{`{"query":"PUB"}`, []string{"acme/pub"}},
		{`{"query":"page-1"}`, visible[13:23]},
		{`{"query":"every kind"}`, []string{"acme/pub", "default/team"}}, // the seller card's description
		{`{"kind":"a2a"}`, []string{"acme/pub", "default/slow", "default/team"}},
		{`{"kind":"mcp"}`, visible[1:63]},
		{`{"price":"per_call"}`, []string{"acme/pub"}},
		{`{"price":"free"}`, visible[1:]},
		{`{"kind":"a2a","price":"free"}`, []string{"default/slow", "default/team"}},
	} {
		if got := all(c.args); !slices.Equal(got, c.want) {
			t.Errorf("search %s:\n got %v\nwant %v", c.args, got, c.want)
		}
	}
	for _, f := range append(all(`{}`), all(`{"query":"anon"}`)...) {
		if f == "default/open" || strings.HasPrefix(f, "anon-") {
			t.Errorf("search returned %s", f)
		}
	}

	// describe: open and the anonymous tenant's service are unknown; an A2A ref answers its card and URLs.
	for _, ref := range []string{"default/open", anonSlug + "/shown", "default/nope"} {
		if r := Tool(t, s.Tool("describe", `{"service":"`+ref+`"}`)); !r.IsError || r.Text() != "unknown service" {
			t.Errorf("describe %s: %+v, want the unknown-service error", ref, r)
		}
	}
	for _, ref := range []string{"acme/pub", "default/team"} {
		r := Tool(t, sb.Tool("describe", `{"service":"`+ref+`"}`))
		if r.IsError || !strings.Contains(string(r.StructuredRaw), `"name":"kinds"`) || !strings.Contains(string(r.StructuredRaw), h.Front.URL+"/agents/"+ref) {
			t.Errorf("describe %s: %s", ref, r.StructuredRaw)
		}
	}

	// fs: its tools cached on replica A by a client's tools/list through the relay; describe there asks nothing upstream.
	fc := NewMCPClient(a.URL+"/mcp/default/fs", h.APIKey)
	fc.Capabilities = `{}`
	if _, err := fc.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if r, err := fc.Request(ctx, 1, "tools/list", map[string]any{}); err != nil || !strings.Contains(r.Text(), "read_file") {
		t.Fatalf("tools/list of fs through the relay: %v %v", r, err)
	}
	_, _ = fc.Delete(ctx)
	upstream := len(fsUp.Requests())
	r := Tool(t, s.Tool("describe", `{"service":"default/fs"}`))
	if r.IsError || !strings.Contains(string(r.StructuredRaw), `"toolsCached":true`) || !strings.Contains(string(r.StructuredRaw), `"read_file"`) ||
		!strings.Contains(string(r.StructuredRaw), h.Front.URL+"/mcp/default/fs") {
		t.Errorf("describe of fs: %s", r.StructuredRaw)
	}
	if n := len(fsUp.Requests()); n != upstream {
		t.Errorf("describe of fs sent %d requests upstream, want none", n-upstream)
	}

	// cold: toolsCached false, and one background drain however many describes come.
	for i := range 5 {
		r := Tool(t, s.Tool("describe", `{"service":"default/cold"}`))
		if i == 0 && (r.IsError || !strings.Contains(string(r.StructuredRaw), `"toolsCached":false`)) {
			t.Errorf("describe of cold: %s", r.StructuredRaw)
		}
	}
	eventually(t, 20*time.Second, "the background drain of cold", func() bool { return coldUp.Count("tools/list") >= 1 })
	for range 5 {
		_ = s.Tool("describe", `{"service":"default/cold"}`)
	}
	time.Sleep(3 * time.Second)
	if n, m := coldUp.Count("initialize"), coldUp.Count("tools/list"); n != 1 || m != 1 {
		t.Errorf("ten describes of cold on one replica: %d initialize and %d tools/list upstream, want one drain", n, m)
	}
	if r := Tool(t, s.Tool("describe", `{"service":"default/cold"}`)); !strings.Contains(string(r.StructuredRaw), `"toolsCached":true`) ||
		!strings.Contains(string(r.StructuredRaw), `"freeze"`) {
		t.Errorf("describe of cold after its drain: %s", r.StructuredRaw)
	}
}
