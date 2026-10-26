package tunnel

import (
	"bufio"
	"io"

	"kontra.sh/kontra/internal/unimpl"
)

// Op is a preamble's operation (§8.1, §8.3, §13).
type Op string

// Operations.
const (
	OpHTTP    Op = "http"    // one HTTP/1.1 exchange for one service (gate-opened: hosting; node-opened: hiring, M3)
	OpControl Op = "control" // the control stream, once per session, the first stream the gate opens (§13)
	OpBlob    Op = "blob"    // a node-opened blob fetch (§8.3, M3)
)

// Preamble is the one JSON line, at most MaxPreamble bytes with its '\n', that starts every stream
// (§8.1). Unknown fields are ignored; the node never forwards it.
type Preamble struct {
	V       int    `json:"v"`
	Op      Op     `json:"op"`
	Service string `json:"service,omitempty"` // hosting: the hosted service the exchange is for
	Kind    string `json:"kind,omitempty"`    // hosting: its kind
	Task    string `json:"task,omitempty"`    // the gate task the exchange belongs to (sealed headers, ext-sealed.md)
	Context string `json:"context,omitempty"` // the gate context of that task
	// MCPSession and MCPRequestID are MCP's (M3): the hex SHA-256 of the client's gate Mcp-Session-Id, and
	// a tools/call's original JSON-RPC id as a string.
	MCPSession   string `json:"mcp_session,omitempty"`
	MCPRequestID string `json:"mcp_request_id,omitempty"`
	Ref          string `json:"ref,omitempty"` // blob streams (M3)
}

// WritePreamble writes p as one JSON line, refusing one over MaxPreamble.
func WritePreamble(w io.Writer, p Preamble) error {
	return unimpl.Error("tunnel.WritePreamble", "WP-01")
}

// ReadPreamble reads one line of at most MaxPreamble bytes and decodes it; a longer line, a v other than
// Version or an unknown op is an error (the node answers it 400 before any target is dialed).
func ReadPreamble(r *bufio.Reader) (Preamble, error) {
	return Preamble{}, unimpl.Error("tunnel.ReadPreamble", "WP-01")
}
