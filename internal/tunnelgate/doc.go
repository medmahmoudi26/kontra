// Package tunnelgate is the gate's side of the tunnel (spec/tunnel.md; relay.md §6.1–§6.4, §8, §14.2):
//
//   - Handler (WP-03) serves GET /v1/tunnel: the WebSocket upgrade with the kontra-tunnel.v1 subprotocol, the
//     four-message handshake within 10 s (§4), the proof (§4.1), the admission (internal/enroll), then the
//     yamux server, the ping loop and the control stream (§7, §11, §13). The gate never dials a node.
//   - Registry (WP-04) holds this replica's sessions: the tunnels row (inserted once welcome is sent and the
//     session's cards are fetched, last_ping_at every 60 s, deleted at close), one session per key per gate
//     (a newer handshake supersedes the older, in the order of the handshakes' session ids, under a row lock
//     on the key, except on a draining replica: make-before-break), revocations (close the key's sessions, drop a service from the served set and
//     abort its streams), "enrolled" notices, the drain message, and the key_scope check before every stream
//     (relay.md §6.1). Replicas learn of supersedes (sent by the superseding registration) and enrollments
//     (sent by the admission that committed them, enroll.EnrolledPayload) by a gate-internal NOTIFY
//     kontra_tunnel (pg.ChannelTunnel), with the 60 s row check as the supersede's fallback.
//   - Cards (WP-04): a tunnel service's upstream card is fetched through a session when one for the service
//     connects, before it becomes a candidate, and every 10 minutes while serving (the keeper in
//     internal/gate), and stored on the services row (relay.md §3); replicas without a session serve the
//     stored card.
//   - Picker (WP-05) is prepare's session choice (relay.md §6.1): the candidate query over tunnels,
//     tunnel_keys, key_services and gate_replicas, round-robin, the context's previous session first.
//   - Backend (WP-05) is an upstream.Backend over one session: a fresh stream per exchange, its preamble
//     naming the service (§8.1), one HTTP/1.1 exchange, never reused (§8.2).
//
// Node-opened streams (M3 WP-03, NodeStreams): hiring (§9), one HTTP/1.1 request served by the gate's own routes
// with the session's principal (auth.HireSource), and blob streams (§8.3, Blobs), one object of a task the session
// serves or hires. They are accepted promptly (§7), counted from Accept until both halves end, answered 429 over
// the cap, and a node that keeps more than the cap of refused streams open loses its session (§14, D137, D182).
// An API key revocation reaches the sessions hiring with it (Registry.APIKeyRevoked: 401 and auth_revoked, §12).
//
// MCP hosting (M3 WP-04; relay.md §11): Picker.PickMCP chooses the tunnel session of a new MCP session, this
// replica's own first and another replica's (through the peer hop, relay.md §11.2) only when this one runs a peer
// listener; Registry.MCPBackend is an exchange's backend on a session this replica holds, its preamble carrying
// mcp_session and mcp_request_id (§8.1), and its refusal saying whether the key or binding was revoked
// (ErrRevoked) or the session is gone. The mcp_sessions rows pinned to a session close revoked on a revocation
// (§12) and tunnel_lost when it closes for any other reason (Registry.remove, CloseAll). Anonymous sessions (§6)
// are M4.
package tunnelgate
