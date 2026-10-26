// Package mcproots keeps a hosted MCP server's roots on the node (spec/tunnel.md §10, spec/relay.md §11.10,
// D173). server-filesystem@1.0.0 replaces the directories it was started with by the client's roots, so roots
// from the remote client, or forged by the gate, would decide what the node's filesystem exposes: the identity
// key, the job keys, and code execution through a write to ~/.bashrc. For every MCP server a node hosts, stdio
// (internal/stdiobridge) or mcp:<name>=<url> (internal/node's fixed-target proxy), the node is the server's only
// source of roots:
//
//   - kontra connect --roots <name>=<dir> names one root of a service: an absolute path to a directory that
//     exists at start. The flag is repeatable, one directory per flag (--roots fs=/a --roots fs=/b), so a
//     directory's name may hold a comma; a service's roots are those of every flag naming it, in order. None by
//     default.
//   - In the initialize it forwards, the client's capabilities.roots is replaced by the node's own: {} (no
//     listChanged) when roots are configured, absent otherwise.
//   - The server's roots/list is answered by the node with the configured directories as file:// URIs (an empty
//     list when there are none) and never relayed to the remote client: the stdio bridge writes the answer to the
//     child's stdin; the proxy of a URL service removes the event from the response stream and POSTs the answer
//     to the target on the same upstream session.
//   - The client's notifications/roots/list_changed, and any client response whose id is that of a roots/list
//     the node answered, are dropped with 202.
//   - On a 2026-07-28 exchange (URL services; the bridge is legacy only) roots is removed from the forwarded
//     _meta["io.modelcontextprotocol/clientCapabilities"], every params.inputResponses entry that carries roots is
//     removed, and every roots/list entry of an upstream inputRequests is removed. Configured roots are offered to
//     legacy sessions only.
//
// Messages are read with exact keys (internal/mcpwire.Parse, D172); one with a repeated or case-variant key is
// answered 400 with -32600 and not forwarded.
//
// Work package: M3 WP-05.
package mcproots

import (
	"encoding/json"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/unimpl"
)

// Roots is one hosted service's configured roots.
type Roots struct {
	dirs []string // absolute, existing, cleaned
}

// Parse reads every --roots value, each <name>=<dir> with one directory, and returns each service's roots with
// its directories in flag order, refusing a relative path or a directory that does not exist. kontra connect
// refuses a name that no --serve mcp: serves (a usage error).
func Parse(flags []string) (map[string]*Roots, error) {
	return nil, unimpl.Error("mcproots.Parse", "M3 WP-05")
}

// Action is what to do with one message the node sees.
type Action int

// Actions.
const (
	Forward Action = iota + 1 // forward Out (the message, possibly rewritten)
	Drop                      // answer the sender 202 (client side) or remove the event (server side); forward nothing
	Answer                    // send Reply to the server instead of relaying the message to the client
)

// Decision is FromClient's or FromServer's verdict.
type Decision struct {
	Action Action
	Out    *mcpwire.Message // Forward: the message to forward
	Reply  *mcpwire.Message // Answer: the node's response to the server's roots/list
}

// Session is the roots state of one MCP session (stdio child or upstream session): the ids of the roots/list
// requests the node answered, so that a client response carrying one is dropped.
type Session struct {
	roots    *Roots
	answered map[string]struct{}
}

// NewSession starts the roots state of one session; r nil means no roots configured.
func NewSession(r *Roots) *Session {
	return &Session{roots: r, answered: map[string]struct{}{}}
}

// FromClient filters one message from the remote client (what the gate forwards) before it reaches the server.
func (s *Session) FromClient(era mcpwire.Era, m *mcpwire.Message) (Decision, error) {
	return Decision{}, unimpl.Error("mcproots.Session.FromClient", "M3 WP-05")
}

// FromServer filters one message from the server before it reaches the remote client.
func (s *Session) FromServer(era mcpwire.Era, m *mcpwire.Message) (Decision, error) {
	return Decision{}, unimpl.Error("mcproots.Session.FromServer", "M3 WP-05")
}

// Capability is what the node declares as capabilities.roots in a forwarded initialize: {} when roots are
// configured, nil (the member removed) otherwise.
func (r *Roots) Capability() json.RawMessage {
	if r == nil || len(r.dirs) == 0 {
		return nil
	}
	return json.RawMessage(`{}`)
}
