// Package tunnelgate is the gate's side of the tunnel (spec/tunnel.md; relay.md §6.1–§6.4, §8, §14.2):
//
//   - Handler (WP-03) serves GET /v1/tunnel: the WebSocket upgrade with the kontra-tunnel.v1 subprotocol, the
//     four-message handshake within 10 s (§4), the proof (§4.1), the admission (internal/enroll), then the
//     yamux server, the ping loop and the control stream (§7, §11, §13). The gate never dials a node.
//   - Registry (WP-04) holds this replica's sessions: the tunnels row (inserted at welcome, last_ping_at every
//     60 s, deleted at close), one session per key per gate (a newer handshake supersedes the older, except
//     during make-before-break), revocations (close the key's sessions, drop a service from the served set),
//     the drain message, and the key_scope check before every stream (relay.md §6.1).
//   - Cards (WP-04): a tunnel service's upstream card is fetched through a session when one for the service
//     connects and every 10 minutes while serving, and stored on the services row (relay.md §3); replicas
//     without a session serve the stored card.
//   - Picker (WP-05) is prepare's session choice (relay.md §6.1): the candidate query over tunnels,
//     tunnel_keys, key_services and gate_replicas, round-robin, the context's previous session first.
//   - Backend (WP-05) is an upstream.Backend over one session: a fresh stream per exchange, its preamble
//     naming the service (§8.1), one HTTP/1.1 exchange, never reused (§8.2).
//
// Node-opened streams (hiring §9, blobs §8.3) are M3: an M2 gate accepts them promptly (§7) and closes
// them, and closes the session of a node that keeps more than tunnel.GateMaxInflight of them open (§14).
// MCP hosting (§10, relay.md §11) and anonymous sessions (§6) are M3 and M4.
package tunnelgate
