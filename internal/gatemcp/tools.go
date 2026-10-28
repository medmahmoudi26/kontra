package gatemcp

import (
	"encoding/json"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/unimpl"
)

// The four tools (spec §12.2). Their input schemas are spec/schema/mcp-tools.json's $defs; every tool declares
// annotations.readOnlyHint and annotations.destructiveHint.
const (
	ToolSearch   = "search"
	ToolDescribe = "describe"
	ToolHire     = "hire"
	ToolReply    = "reply"
)

// Ref names a service in search results and in describe's and hire's arguments: <name> on a single-tenant gate,
// <tenant>/<name> on a multi-tenant one (§12.2, DM4-4).
type Ref struct {
	Tenant string // "" on a single-tenant gate
	Name   string
}

// ParseRef parses a ref as the gate's tenancy writes it; anything else is the unknown-service error.
func ParseRef(s string, multi bool) (Ref, error) {
	return Ref{}, unimpl.Error("gatemcp.ParseRef", "M4 WP-04")
}

// String is the ref as search returns it.
func (r Ref) String() string {
	if r.Tenant == "" {
		return r.Name
	}
	return r.Tenant + "/" + r.Name
}

// SearchArgs are search's arguments (mcp-tools.json#/$defs/search).
type SearchArgs struct {
	Query  string `json:"query,omitempty"`
	Kind   string `json:"kind,omitempty"`  // a2a | mcp
	Price  string `json:"price,omitempty"` // equality on the price_model annotation; "free": none (DM4-4)
	Cursor string `json:"cursor,omitempty"`
}

// ServiceEntry is one search result.
type ServiceEntry struct {
	Ref         string `json:"ref"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Available   bool   `json:"available"`       // a connected session of a live replica, or a direct backend
	Price       string `json:"price,omitempty"` // the operator's search.price annotation, verbatim (§13.4)
}

// SearchResult is search's structuredContent (also sent as a text block).
type SearchResult struct {
	Services   []ServiceEntry `json:"services"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

// DescribeArgs are describe's arguments.
type DescribeArgs struct {
	Service string `json:"service"`
}

// DescribeResult is describe's structuredContent: the served card (A2A) or the cached, filtered, annotated
// tools/list (MCP; ToolsCached false and no Tools when the cache is cold, DM4-4), and how to call the service.
type DescribeResult struct {
	Ref         string            `json:"ref"`
	Kind        string            `json:"kind"`
	Card        json.RawMessage   `json:"card,omitempty"`
	Tools       []json.RawMessage `json:"tools,omitempty"`
	ToolsCached *bool             `json:"toolsCached,omitempty"`
	Sealed      bool              `json:"sealed"`
	Available   bool              `json:"available"`
	URLs        []string          `json:"urls"` // {public_url}/agents/… or {public_url}/mcp/…
}

// HireArgs are hire's arguments: Message is a string or {parts: [Part]} (mcp-tools.json#/$defs/message).
type HireArgs struct {
	Service   string          `json:"service"`
	Message   json.RawMessage `json:"message"`
	ContextID string          `json:"contextId,omitempty"`
}

// ReplyArgs are reply's arguments.
type ReplyArgs struct {
	TaskID  uuid.UUID       `json:"taskId"`
	Message json.RawMessage `json:"message"`
}

// PlainState is the structuredContent of a plain hire or reply that did not end in time, or beside an ended
// task's rendered content (§12.2, DM4-5).
type PlainState struct {
	TaskID     string `json:"taskId"`
	State      string `json:"state"`
	ApproveURL string `json:"approveUrl,omitempty"`
}

// ToolsList is the tools/list result for a request of era: the four tools with their input schemas; hire
// declares execution.taskSupport "optional" in the legacy list of a 2025-11-25 session only (§12.5).
func ToolsList(era mcpwire.Era, protocolVersion string) (json.RawMessage, error) {
	return nil, unimpl.Error("gatemcp.ToolsList", "M4 WP-04")
}

// FirstRequest is a hire task's resend key: SHA-256(utf8(service ref) ‖ 0x00 ‖ JCS(arguments)) (§12.3 Retries),
// which replaces §15's request hash for these tasks.
func FirstRequest(ref Ref, arguments json.RawMessage) ([32]byte, error) {
	return [32]byte{}, unimpl.Error("gatemcp.FirstRequest", "M4 WP-04")
}
