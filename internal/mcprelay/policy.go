package mcprelay

import (
	"context"
	"encoding/json"
	"time"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/unimpl"
)

// Policy is a service's tools: {allow: [glob…], deny: [glob…]} (spec/relay.md §11.3): globs use * only, deny
// wins, an absent allow allows all. It is applied to the body's params.name, never to a header.
type Policy struct {
	Allow []string
	Deny  []string
}

// Allows reports whether tool may be listed and called. A tool it refuses is answered exactly as one that does
// not exist: -32602 "Unknown tool: <name>" before dispatch (mcp_tool_denied).
func (p Policy) Allows(tool string) (bool, error) {
	return false, unimpl.Error("mcprelay.Policy.Allows", "M3 WP-04")
}

// ToolsCache is each service's drained tools/list, per era (spec/relay.md §11.3, §11.12): every upstream page
// followed (within §6.6's limits), denied tools dropped, Kontra's keys removed, the operator's tool_meta
// applied; answered as one page with no cursor, so denied tools never shape page boundaries. Refreshed on an
// upstream notifications/tools/list_changed and every RefreshTools. The modern list carries ttlMs (at most
// RefreshTools) and cacheScope "private". It also keeps the upstream's server/discover capabilities, whose
// tasks extension decides gate-owned modern tasks (D159), and the service's era (D177). A server/discover result
// makes the service modern: the cached one, or the answer to the gate's own server/discover (no session headers,
// one per service at a time), which a modern request finding the era unknown waits for. Only an explicit legacy
// answer to the gate's own probe makes it legacy-only: a 400 with rule 3's -32000 body, a 404 or 405, or a 200 or
// 400 whose body is neither a server/discover result nor an error coded -32020, -32021 or -32022; any other
// outcome leaves the era unknown. The answer to a client's request never sets the era: a client's 400, 404 or 405
// is relayed to that client and changes nothing for other principals. The era is kept RefreshTools and dropped
// when a node of the service connects or disconnects. While a service is legacy-only, modern requests to it are
// answered with §11.12 rule 3's 400 without being forwarded.
type ToolsCache struct {
	byService map[string]*tools
}

type tools struct {
	list    []json.RawMessage
	fetched time.Time
}

// RefreshTools is the cache's period.
const RefreshTools = 10 * time.Minute

// List returns the service's filtered, annotated tools for era and policy.
func (c *ToolsCache) List(ctx context.Context, service string, era mcpwire.Era, p Policy) ([]json.RawMessage, error) {
	return nil, unimpl.Error("mcprelay.ToolsCache.List", "M3 WP-04")
}

// Tool returns one tool's cached entry (§11.5 step 0: execution.taskSupport), ok false for an unknown or
// denied tool.
func (c *ToolsCache) Tool(ctx context.Context, service string, era mcpwire.Era, p Policy, name string) (json.RawMessage, bool, error) {
	return nil, false, unimpl.Error("mcprelay.ToolsCache.Tool", "M3 WP-04")
}

// Invalidate drops a service's lists (list_changed, a configuration change).
func (c *ToolsCache) Invalidate(service string) {}

// Era returns the service's upstream era as last learned, ok false when it is not known (D177).
func (c *ToolsCache) Era(ctx context.Context, service string) (era mcpwire.Era, ok bool, err error) {
	return 0, false, unimpl.Error("mcprelay.ToolsCache.Era", "M3 WP-07")
}
