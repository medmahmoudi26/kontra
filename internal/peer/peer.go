// Package peer is the replica-to-replica hop of spec/relay.md §11.2 (D163). Only the replica holding a tunnel
// session can open a stream on it, and a plain MCP request has no Temporal activity to carry it there: a
// replica that receives a request for a tunnel session another replica holds forwards the one HTTP/1.1 exchange
// to that replica's peer listener (peer.listen, advertised in gate_replicas.peer_addr), which runs §6.1's check
// on its own registry and the exchange on its own tunnel and streams the response back unchanged.
//
// The receiving replica applies every other rule of §11 once, so the hop carries no credential: the client's
// Authorization is never forwarded. What the holding replica trusts is the Kontra-Peer header, an assertion of
// the principal, the tunnel session, the service, the preamble's MCP fields (spec/tunnel.md §8.1), the
// upstream's Mcp-Session-Id, the SHA-256 of the other forwarded headers (ForwardedHeaders), a nonce and the
// time, with an HMAC-SHA256 under the shared secret (peer.secret_env, at least 32 bytes) over the assertion, the
// method, the path and the body's SHA-256 (D179). One older than MaxAge, or whose nonce was seen within MaxAge,
// is refused, and so is a request whose headers do not hash to the assertion's. The holding replica builds the
// upstream request from signed data only: Mcp-Session-Id from UpstreamSID, and only the headers ForwardedHeaders
// keeps. The listener serves Path only, answers anything else 404, and is never the public listener; its
// address must be reachable only from the gate's replicas, or carry TLS, since the HMAC authenticates a request
// and does not hide it.
//
// mcp_sessions.replica_id names the replica holding the tunnel session (NULL for a direct backend), never the
// one the client reached, so the reaper of a dead replica closes only the sessions whose tunnels it held.
//
// Work package: M3 WP-04.
package peer

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/unimpl"
)

// Fixed values of the hop.
const (
	Header = "Kontra-Peer"
	// Path is the one route of the peer listener.
	Path = "/v1/peer/mcp"
	// MaxAge bounds an assertion's age; nonces are remembered that long.
	MaxAge = 30 * time.Second
	// MinSecret is the shortest shared secret accepted.
	MinSecret = 32
)

// Assertion is what the receiving replica vouches for.
type Assertion struct {
	Replica       string    `json:"replica"` // the receiving replica's id
	TenantID      uuid.UUID `json:"tenant"`
	Service       string    `json:"service"`
	TunnelSession uuid.UUID `json:"tunnel_session"`
	Principal     string    `json:"principal"`
	BuyerID       uuid.UUID `json:"buyer"`
	Via           string    `json:"via"`
	MCPSession    string    `json:"mcp_session,omitempty"`    // tunnel.md §8.1
	MCPRequestID  string    `json:"mcp_request_id,omitempty"` // tunnel.md §8.1
	UpstreamSID   string    `json:"upstream_sid,omitempty"`   // the upstream's Mcp-Session-Id the holder sends
	Headers       string    `json:"headers"`                  // hex SHA-256 of ForwardedHeaders' canonical form
	Nonce         string    `json:"nonce"`
	At            time.Time `json:"at"`
}

// Forwarded are the request headers the hop carries besides Mcp-Session-Id, which travels in the assertion;
// every Mcp-Param-* header is carried too (spec/relay.md §11.2, §11.12).
var Forwarded = []string{"Accept", "Content-Type", "MCP-Protocol-Version", "Last-Event-ID", "Mcp-Method", "Mcp-Name"}

// ForwardedHeaders returns the headers of h the hop carries (Forwarded and every Mcp-Param-*) and the SHA-256 of
// their canonical form: names lowercased and sorted, one "name: value\n" line per value, in order. The receiving
// replica puts the hash in the assertion; the holding replica recomputes it from what arrived and forwards only
// these headers.
func ForwardedHeaders(h http.Header) (http.Header, [32]byte) {
	panic(unimpl.Error("peer.ForwardedHeaders", "M3 WP-04"))
}

// Signer signs and verifies assertions with the shared secret.
type Signer struct {
	secret []byte
}

// NewSigner refuses a secret shorter than MinSecret.
func NewSigner(secret []byte) (*Signer, error) {
	return nil, unimpl.Error("peer.NewSigner", "M3 WP-04")
}

// Sign returns the Kontra-Peer header value for a, over method, path and the body's SHA-256.
func (s *Signer) Sign(a Assertion, method, path string, bodySHA256 [32]byte) (string, error) {
	return "", unimpl.Error("peer.Signer.Sign", "M3 WP-04")
}

// Verify checks a header value against method, path and body hash at now, and the nonce against those seen.
func (s *Signer) Verify(header, method, path string, bodySHA256 [32]byte, now time.Time) (Assertion, error) {
	return Assertion{}, unimpl.Error("peer.Signer.Verify", "M3 WP-04")
}

// Client forwards exchanges to other replicas.
type Client struct {
	signer *Signer
	hc     *http.Client
}

// NewClient returns a client whose transport dials only addresses read from gate_replicas.peer_addr.
func NewClient(s *Signer) (*Client, error) {
	return nil, unimpl.Error("peer.NewClient", "M3 WP-04")
}

// Forward sends req (method, the MCP path, body, the headers to forward upstream) to the replica at addr and
// returns its streamed response.
func (c *Client) Forward(ctx context.Context, addr string, a Assertion, req *http.Request) (*http.Response, error) {
	return nil, unimpl.Error("peer.Client.Forward", "M3 WP-04")
}

// Runner runs one verified exchange on this replica's tunnel session.
type Runner interface {
	RunExchange(ctx context.Context, a Assertion, req *http.Request, w http.ResponseWriter) error
}

// Server is the peer listener's handler.
type Server struct {
	signer *Signer
	run    Runner
}

// NewServer returns the handler for peer.listen.
func NewServer(s *Signer, run Runner) (*Server, error) {
	return nil, unimpl.Error("peer.NewServer", "M3 WP-04")
}

// ServeHTTP implements http.Handler: Path only, the assertion verified, then Runner.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}
