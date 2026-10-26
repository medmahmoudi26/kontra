// Package tunnel is what the node (`kontra connect`) and the gate (`kontra-gate serve`) share over the one
// WebSocket the node opens (spec/tunnel.md): the four handshake messages and their error codes (§4), the
// HPKE proof of possession and its transcript (§4.1, D30), the key id (§2.2), the origin rules (§3), the
// stream preamble (§8.1), the control stream's JSON lines (§13), the yamux configuration both sides use
// (§7) and the ping loop both sides run (§11, D33).
//
// It is not a protocol a service or a client speaks. Nothing here is visible to the A2A agent on the
// node's localhost or to a caller of the gate: A2A 1.0 rides inside the streams unchanged, and the node
// is a fixed-target reverse proxy (§8.2). Nothing beyond what tunnel.md defines goes on the wire.
//
// Roles (§1): the node always dials and is the yamux client; the gate never opens a connection to a node
// and is the yamux server. The gate's side is internal/tunnelgate, the node's internal/node, the node's
// identity key file internal/nodekey, and enrollment, revocation and rotation (Postgres) internal/enroll.
//
// Work packages (M2 plan): WP-01 builds the handshake messages, the proof, the key id, the origin rules,
// the preamble and the control codec; WP-03 builds mux.go and liveness.go.
package tunnel

import "time"

// The connection (§3).
const (
	// Path is the gate route a node upgrades on (relay.md §2.1).
	Path = "/v1/tunnel"
	// Subprotocol is the one Sec-WebSocket-Protocol value; the gate refuses an upgrade without it (400)
	// and the node closes a connection on which the gate did not select it.
	Subprotocol = "kontra-tunnel.v1"
	// Version is the "v" of every handshake message and stream preamble (§4, §8.1). Control lines carry
	// none (§13).
	Version = 1
	// DefaultGate is `--gate`'s default (§3).
	DefaultGate = "https://gateway.kontra.sh"
)

// Limits (§14).
const (
	// MaxHandshakeMessage is the read limit of each handshake message, both sides (§4).
	MaxHandshakeMessage = 16 << 10
	// HandshakeTimeout is how long the gate gives the whole handshake from the upgrade (§4).
	HandshakeTimeout = 10 * time.Second
	// MaxPreamble is the longest stream preamble, its terminating '\n' included (§8.1).
	MaxPreamble = 4096
	// NodeMaxInflight is `--max-inflight`'s default: inbound streams in flight per session on the node;
	// over it the node answers 503 with Kontra-Node: full (§14).
	NodeMaxInflight = 64
	// GateMaxInflight and GateMaxInflightAnonymous bound node-opened (hiring) streams per session on the
	// gate; over them the gate answers 429 (§14, §6.2). Hiring is M3.
	GateMaxInflight          = 32
	GateMaxInflightAnonymous = 8
	// MaxBlob is the most a blob stream carries (§8.3, M3).
	MaxBlob = 64 << 20
)

// Liveness and the registry (§11, D33).
const (
	// PingEvery is each side's Session.Ping() interval; welcome.ping_s carries it in seconds.
	PingEvery = 20 * time.Second
	// PingFailures consecutive failed pings close the session (a silent peer is seen within about 75 s).
	PingFailures = 3
	// LastPingEvery is how often the gate writes tunnels.last_ping_at (not per ping).
	LastPingEvery = 60 * time.Second
)

// Responses the node writes itself carry HeaderNode with one of the NodeReason values, so the gate never
// mistakes them for the service's own (§8.2, relay.md §6.4). The node removes HeaderNode and
// HeaderSealedReject from every response of the service.
const (
	HeaderNode         = "Kontra-Node"
	HeaderSealedReject = "Kontra-Sealed-Reject"
)

// NodeReason is the value of a Kontra-Node header (§8.2).
type NodeReason string

// Node reasons and the status each goes with.
const (
	NodeNotHosted         NodeReason = "not_hosted"         // 404: a service the node does not host on this session
	NodeKindMismatch      NodeReason = "kind_mismatch"      // 404: a kind that does not match the hosted service
	NodeFull              NodeReason = "full"               // 503: the node's in-flight cap
	NodeTargetUnreachable NodeReason = "target_unreachable" // 502: the configured target did not answer
	NodeSealedRejected    NodeReason = "sealed_rejected"    // 400: ext-sealed.md §6 (M3)
)

// Service kinds a node declares and a preamble names (relay.md §1).
const (
	KindA2A = "a2a"
	KindMCP = "mcp" // hosting MCP is M3
)
