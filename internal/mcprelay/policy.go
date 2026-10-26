package mcprelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/unimpl"
)

// Policy is a service's tools: {allow: [glob…], deny: [glob…]} (spec/relay.md §11.3): globs use * only, deny
// wins, an absent allow (nil) allows all, and an empty one allows none. It is applied to the body's params.name,
// never to a header.
type Policy struct {
	Allow []string
	Deny  []string
}

// ErrBadGlob is a policy glob that is empty (the configuration refuses one, spec §11.3).
var ErrBadGlob = errors.New("mcprelay: an empty tool glob")

// Allows reports whether tool may be listed and called. A tool it refuses is answered exactly as one that does
// not exist: -32602 "Unknown tool: <name>" before dispatch (mcp_tool_denied).
func (p Policy) Allows(tool string) (bool, error) {
	for _, g := range p.Deny {
		if g == "" {
			return false, ErrBadGlob
		}
		if globMatch(g, tool) {
			return false, nil
		}
	}
	if p.Allow == nil {
		return true, nil
	}
	for _, g := range p.Allow {
		if g == "" {
			return false, ErrBadGlob
		}
		if globMatch(g, tool) {
			return true, nil
		}
	}
	return false, nil
}

// globMatch matches name against a glob in which * stands for any run of characters (none included) and every
// other character for itself, case-sensitively (tool names are, MCP server/tools).
func globMatch(glob, name string) bool {
	parts := strings.Split(glob, "*")
	if len(parts) == 1 {
		return glob == name
	}
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(name, mid)
		if i < 0 {
			return false
		}
		name = name[i+len(mid):]
	}
	return strings.HasSuffix(name, last) && len(name) >= len(last)
}

// Limits of a drained tools/list (spec/relay.md §6.6): 100 pages or 4 MiB in all; over either, the drain fails
// (upstream_error, a 502 to the client).
const (
	MaxToolPages = 100
	MaxToolBytes = 4 << 20
)

// ErrToolsLimit is a tools/list over §6.6's limits, or one that is not a tools/list result.
var ErrToolsLimit = errors.New("mcprelay: the upstream's tools/list is over its limits or malformed")

// ToolsKey names a service's tools in the cache.
type ToolsKey struct {
	TenantID uuid.UUID
	Service  string
}

// Pager fetches one page of the upstream's tools/list: the result of a tools/list with cursor ("" for the first
// page), as the upstream answered it (Parse accepted it). The relay sends it on the client's own upstream session.
type Pager func(ctx context.Context, cursor string) (json.RawMessage, error)

// ToolView is how a cached list is shown to one client: the service's policy, the operator's tool_meta annotation
// ("<tool name>" | "*" → sh.kontra/ keys, spec §13.4), and whether every tool's execution is removed (tasks: off,
// a 2025-06-18 session, §11.5).
type ToolView struct {
	Policy        Policy
	Meta          map[string]map[string]json.RawMessage
	DropExecution bool
}

// ToolsCache is each service's drained tools/list, per era (spec/relay.md §11.3, §11.12): every upstream page
// followed (within §6.6's limits), Kontra's keys removed; denied tools dropped and the operator's tool_meta applied
// when it is shown (ToolView), so a policy or annotation change applies at once; answered as one page with no
// cursor, so denied tools never shape page boundaries. Refreshed on an upstream notifications/tools/list_changed
// (Invalidate) and once RefreshTools old, at the next request; one drain per service and era at a time, which the
// other requests wait for. It is per replica; a services row that changes (NOTIFY kontra_services) invalidates it
// as it does the cards.
//
// The modern list carries ttlMs (at most RefreshTools) and cacheScope "private". It also keeps the upstream's
// server/discover capabilities, whose tasks extension decides gate-owned modern tasks (D159), and the service's era
// (D177). A server/discover result makes the service modern: the cached one, or the answer to the gate's own
// server/discover (no session headers, one per service at a time), which a modern request finding the era unknown
// waits for. Only an explicit legacy answer to the gate's own probe makes it legacy-only: a 400 with rule 3's
// -32000 body, a 404 or 405, or a 200 or 400 whose body is neither a server/discover result nor an error coded
// -32020, -32021 or -32022; any other outcome leaves the era unknown. The answer to a client's request never sets
// the era: a client's 400, 404 or 405 is relayed to that client and changes nothing for other principals. The era is
// kept RefreshTools and dropped when a node of the service connects or disconnects. While a service is legacy-only,
// modern requests to it are answered with §11.12 rule 3's 400 without being forwarded. (The era and the modern
// list: M3 WP-07.)
type ToolsCache struct {
	mu        sync.Mutex
	byService map[cacheKey]*tools
	gen       map[ToolsKey]uint64 // bumped by Invalidate: a drain begun before it installs nothing
	now       func() time.Time
}

type cacheKey struct {
	ToolsKey
	era mcpwire.Era
}

type tools struct {
	list    []json.RawMessage
	names   []string // list[i]'s name
	fetched time.Time
	filling chan struct{} // closed when the drain in flight ends
}

// RefreshTools is the cache's period.
const RefreshTools = 10 * time.Minute

// NewToolsCache returns an empty cache.
func NewToolsCache() *ToolsCache {
	return &ToolsCache{byService: map[cacheKey]*tools{}, gen: map[ToolsKey]uint64{}, now: time.Now}
}

// List returns the service's tools for era as v shows them, draining the upstream through fetch when the cache
// has no fresh list.
func (c *ToolsCache) List(ctx context.Context, key ToolsKey, era mcpwire.Era, v ToolView, fetch Pager) ([]json.RawMessage, error) {
	t, err := c.get(ctx, key, era, fetch)
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(t.list))
	for i, raw := range t.list {
		ok, err := v.Policy.Allows(t.names[i])
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		shown, err := present(raw, t.names[i], v)
		if err != nil {
			return nil, err
		}
		out = append(out, shown)
	}
	return out, nil
}

// Tool returns one tool's cached entry (§11.5 step 0: execution.taskSupport), ok false for an unknown or
// denied tool.
func (c *ToolsCache) Tool(ctx context.Context, key ToolsKey, era mcpwire.Era, p Policy, name string, fetch Pager) (json.RawMessage, bool, error) {
	allowed, err := p.Allows(name)
	if err != nil || !allowed {
		return nil, false, err
	}
	t, err := c.get(ctx, key, era, fetch)
	if err != nil {
		return nil, false, err
	}
	for i, n := range t.names {
		if n == name {
			return t.list[i], true, nil
		}
	}
	return nil, false, nil
}

// Invalidate drops a service's lists (list_changed, a configuration change).
func (c *ToolsCache) Invalidate(key ToolsKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen[key]++
	for k, t := range c.byService {
		if k.ToolsKey == key && t.filling == nil {
			delete(c.byService, k)
		}
	}
}

// Era returns the service's upstream era as last learned, ok false when it is not known (D177).
func (c *ToolsCache) Era(ctx context.Context, service string) (era mcpwire.Era, ok bool, err error) {
	return 0, false, unimpl.Error("mcprelay.ToolsCache.Era", "M3 WP-07")
}

// get returns a fresh list, draining it (or waiting for the drain in flight).
func (c *ToolsCache) get(ctx context.Context, key ToolsKey, era mcpwire.Era, fetch Pager) (*tools, error) {
	k := cacheKey{key, era}
	for {
		c.mu.Lock()
		t := c.byService[k]
		switch {
		case t != nil && t.filling != nil:
			ch := t.filling
			c.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case t != nil && c.now().Sub(t.fetched) < RefreshTools:
			c.mu.Unlock()
			return t, nil
		}
		if fetch == nil {
			c.mu.Unlock()
			return nil, errors.New("mcprelay: no tools/list cached and no way to fetch one")
		}
		ch := make(chan struct{})
		c.byService[k] = &tools{filling: ch}
		gen := c.gen[key]
		c.mu.Unlock()

		list, names, err := drain(ctx, fetch)
		c.mu.Lock()
		if err != nil || c.gen[key] != gen {
			delete(c.byService, k)
		} else {
			c.byService[k] = &tools{list: list, names: names, fetched: c.now()}
		}
		close(ch)
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return &tools{list: list, names: names, fetched: c.now()}, nil
	}
}

// drain follows every page of the upstream's tools/list within MaxToolPages and MaxToolBytes, removes Kontra's
// keys from each tool's _meta, and drops entries that are not objects with a string name (the policy could not be
// applied to them) and repeated names (the first stands).
func drain(ctx context.Context, fetch Pager) ([]json.RawMessage, []string, error) {
	var (
		list  []json.RawMessage
		names []string
		total int
		seen  = map[string]bool{}
		cur   string
	)
	for page := 0; ; page++ {
		if page == MaxToolPages {
			return nil, nil, fmt.Errorf("%w: more than %d pages", ErrToolsLimit, MaxToolPages)
		}
		res, err := fetch(ctx, cur)
		if err != nil {
			return nil, nil, err
		}
		if total += len(res); total > MaxToolBytes {
			return nil, nil, fmt.Errorf("%w: more than %d bytes", ErrToolsLimit, MaxToolBytes)
		}
		res, err = mcpwire.ScrubResult(mcpwire.FromUpstream, mcpwire.MethodToolsList, res, false)
		if err != nil {
			return nil, nil, err
		}
		m, err := strictjson.Members(res)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %w", ErrToolsLimit, err)
		}
		var page []json.RawMessage
		if raw, ok := m["tools"]; ok {
			if err := json.Unmarshal(raw, &page); err != nil {
				return nil, nil, fmt.Errorf("%w: tools is not an array", ErrToolsLimit)
			}
		}
		for _, tool := range page {
			name, ok := toolName(tool)
			if !ok || seen[name] {
				continue
			}
			seen[name] = true
			list = append(list, bytes.Clone(tool))
			names = append(names, name)
		}
		next, ok := m["nextCursor"]
		if !ok || bytes.Equal(bytes.TrimSpace(next), []byte("null")) {
			return list, names, nil
		}
		var s string
		if err := json.Unmarshal(next, &s); err != nil {
			return nil, nil, fmt.Errorf("%w: nextCursor is not a string", ErrToolsLimit)
		}
		if s == "" {
			return list, names, nil
		}
		cur = s
	}
}

// toolName is a tool entry's name, read with exact keys (Parse checked each of result.tools).
func toolName(tool json.RawMessage) (string, bool) {
	m, err := strictjson.Members(tool)
	if err != nil {
		return "", false
	}
	raw, ok := m["name"]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || s == "" {
		return "", false
	}
	return s, true
}

// present is one cached tool as v shows it: tool_meta's "*" keys and then the tool's own written into its _meta,
// each replacing a same-named key (§13.4), and execution removed when v says so.
func present(tool json.RawMessage, name string, v ToolView) (json.RawMessage, error) {
	out := []byte(tool)
	var err error
	if len(v.Meta) > 0 {
		add := map[string]json.RawMessage{}
		for k, val := range v.Meta["*"] {
			add[k] = val
		}
		for k, val := range v.Meta[name] {
			add[k] = val
		}
		if out, err = mcpwire.MergeMeta(out, add); err != nil {
			return nil, err
		}
	}
	if v.DropExecution {
		if out, err = mcpwire.DeleteMember(out, "execution"); err != nil {
			return nil, err
		}
	}
	return out, nil
}
