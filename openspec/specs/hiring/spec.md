# hiring Specification

## Purpose
The hiring side of `kontra connect`: how a node lets local clients hire services through the gate. The node serves a
local HTTP endpoint, checks each local request, forwards it on a stream it opens in its own tunnel, and reads the
gate's answers (cards, redirects, MCP sessions, blob parts, sealed parts) before its client does; the gate serves the
forwarded request as the session's principal. Tunnel framing, handshake and enrollment belong to `tunnel` and
`node-enrollment-and-keys`, sealing's cryptography to `sealing`, quotes and holds to `escrow`. `spec/tunnel.md` §2.3,
§8.3, §9, §13–§14 and `spec/relay.md` §2.3, §3, §7, §10.4 are normative.

## Known limitations
- On systems without the kernel socket table (not Linux) a loopback `--listen` without `--local-token` cannot tell other local users apart and serves them as the node's principal (`spec/tunnel.md` §9; D207, D229).
- Before the node binds its port (at start or restart), another local user can bind it and serve the node's clients; `--local-token` is the remedy on shared machines (`spec/tunnel.md` §9).

## Requirements

### Requirement: Local Hiring Endpoint
`kontra connect` SHALL serve local clients on `127.0.0.1:7777` (`--listen`) unless `--listen off`, which binds no local port and hires for no one; `--listen off` MUST be refused beside `--local-token` or `--allow-origin`, and without any `--serve`. A loopback `--listen` SHALL bind both `127.0.0.1` and `[::1]`, a port held by another process on either family stopping the start. The node serves no other local port (`spec/tunnel.md` §9; D164, D208, D262).

#### Scenario: Hosting-only node
- **WHEN** `kontra connect --serve a2a:hello=http://localhost:9999 --listen off` starts
- **THEN** it binds no local port and says hiring is off

#### Scenario: Off with nothing to serve
- **WHEN** `kontra connect --listen off` is given no `--serve`
- **THEN** it refuses to start

#### Scenario: Port held on IPv6 loopback
- **WHEN** another process holds `[::1]:7777`
- **THEN** the node does not start its hiring endpoint on `127.0.0.1:7777` alone

### Requirement: Local Request Checks
The node MUST answer `403` (an id-less JSON-RPC error on MCP paths), opening no stream, to a request whose `Host` is not `127.0.0.1:<port>`, `localhost:<port>` or `[::1]:<port>` of its `--listen` (or the specific non-loopback address it listens on), and to one carrying an `Origin` not listed by `--allow-origin` (default none). It SHALL answer `OPTIONS` with `405` and forward `Origin` only when allowed (`spec/tunnel.md` §9; D207; `local_rebinding`).

#### Scenario: DNS rebinding
- **WHEN** a browser page sends a request to the node with `Host: evil.example:7777`
- **THEN** the node answers `403` and opens no stream

#### Scenario: CORS preflight
- **WHEN** a client sends `OPTIONS /agents/hello`
- **THEN** the node answers `405`

### Requirement: Local Paths
The node SHALL serve only `/agents/…`, `/mcp/…`, `/mcp` and `/.well-known/…` on its local endpoint and MUST answer `404`, opening no stream, for any other path (`spec/tunnel.md` §9).

#### Scenario: Unknown path
- **WHEN** a local client requests `/admin`
- **THEN** the node answers `404` and opens no tunnel stream

### Requirement: Local Token Off Loopback
A `--listen` on a non-loopback address MUST require `--local-token <file>` (its first line, at least 16 characters without white space); every request MUST then carry `Authorization: Bearer <local token>`, which the node strips before forwarding (`spec/tunnel.md` §9; D207).

#### Scenario: Non-loopback without a token
- **WHEN** `kontra connect --listen 10.0.0.5:7777` is started without `--local-token`
- **THEN** it refuses to start

#### Scenario: Token never reaches the gate
- **WHEN** a local client sends a request with the local token
- **THEN** the forwarded request carries no `Authorization` header from the client

### Requirement: Same-User Connections On Loopback
On a loopback `--listen` without `--local-token`, the node on Linux SHALL read the owner of each connection's client socket from `/proc/self/net/tcp` and `tcp6`, whatever the source address, and MUST close a connection of another user or one it cannot attribute (inode 0, the overflow uid, an unreadable table, about which it warns at start) (`spec/tunnel.md` §9; D207, D229).

#### Scenario: Another account connects
- **WHEN** a process of another local user connects to `127.0.0.1:7777`
- **THEN** the node closes the connection without forwarding anything

### Requirement: Forwarding On Node-Opened Streams
The node SHALL forward each accepted local request on a stream it opens in its current tunnel session, with the preamble `{"v": 1, "op": "http"}` and the request path unchanged, for paths under `/agents/…`, `/mcp/…`, `/mcp` and `/.well-known/…`, and MUST answer any other path `404` itself. A request other than `GET` or `HEAD` to `/.well-known/…`, or to an `/agents/…` path that names no service, MUST be `404` (`spec/tunnel.md` §9; D34, D247).

#### Scenario: Unknown local path
- **WHEN** a local client requests `/admin`
- **THEN** the node answers `404` and opens no stream

#### Scenario: A2A request forwarded
- **WHEN** a local client sends `POST /agents/acme/hello` with a JSON-RPC `SendMessage`
- **THEN** the gate receives it on a node-opened stream with the same path

### Requirement: Hiring Principal Is The Session's
The gate SHALL serve a request that arrived on a node-opened stream with the same handler as a network request, with the session's principal: the API key from the handshake's `auth` when present, else `node:<key_id>` with the key's tenant and `buyer_id` UUIDv5 of the key id. A local client MUST NOT need to send a credential, and every local client of a node SHALL share that principal, including its MCP `max_sessions_per_principal` budget (`spec/tunnel.md` §9; `spec/relay.md` §2.3; D231, D319).

#### Scenario: Node without an API key
- **WHEN** a node that connected without `auth` hires `/agents/hello`
- **THEN** the gate task's principal is `node:<key_id>` and the service's `allow:` must admit that principal or the node's tenant

#### Scenario: Revoked hiring key
- **WHEN** the session's hiring API key is revoked
- **THEN** later hiring streams get `401` and the node's hosting continues

### Requirement: Hiring Edge Limits Per Session
The gate SHALL apply edge limits to forwarded requests with the session as the client: `limits.body` and the body timeout as on the network, `rps_per_ip` per session, and at most 32 node-opened streams in flight per session (8 for anonymous sessions), not shared with the same API key's network requests. Over the cap the gate answers `429`; the node waits for a free slot for a bounded time and then answers its client `429` (`spec/tunnel.md` §9, §14; D137, D182, D207).

#### Scenario: Burst over the stream cap
- **WHEN** local clients start more concurrent requests than the session's stream cap
- **THEN** the excess wait for a slot and, if none frees in time, get `429`

### Requirement: Local Card Verification And Rewrite
The node SHALL verify the gate's signature on every card it serves locally with a `jku` equal to `{the origin it dialed}/.well-known/jwks.json`, fetched through its tunnel, and MUST NOT serve a card that does not verify. It SHALL rewrite every `supportedInterfaces[].url` to `http://localhost:<port>{svc}` for the service the card was read for, keep only `url`, `protocolBinding` and `protocolVersion` per interface, and drop the 0.3 endpoint members (`spec/tunnel.md` §9; D223, D247, D259).

#### Scenario: Gate card points at another path
- **WHEN** the gate's card for `/agents/hello` names an interface at `/agents/other`
- **THEN** the node serves the card with the interface at `http://localhost:7777/agents/hello`

#### Scenario: Bad signature
- **WHEN** a card's gate signature does not verify with the dialed origin's key set
- **THEN** the node does not serve the card to its client

#### Scenario: Case variant of an endpoint member
- **WHEN** a card holds a lone `SupportedInterfaces` member
- **THEN** the node does not serve it

### Requirement: Gate Answers Read Before Clients
The node MUST NOT relay a `3xx` from the gate on any path: the client gets the node's own `502` and no `Location`. It MUST remove `Mcp-Session-Id` from every answer except an `initialize`, and use a session id only for the service it was initialized for: a request on it to another path, or on an unsealed session to a service whose pin says sealed, is `404` (`spec/tunnel.md` §9; D258, D260).

#### Scenario: Gate answers a redirect
- **WHEN** the gate answers a forwarded request with `307` and a `Location`
- **THEN** the local client receives `502` without `Location`

#### Scenario: Session reused for another service
- **WHEN** a local client sends a request with an MCP session id initialized for `/mcp/a` to `/mcp/b`
- **THEN** the node answers `404`

### Requirement: Hiring Streams Across Drain And Revocation
The node SHALL open no new hiring streams on a session the gate told to drain and SHALL put the next one on its new session, waiting for a session (up to a bound) while it has none; after `{"t": "auth_revoked"}` its hiring streams get `401` while hosting continues (`spec/tunnel.md` §13, §12).

#### Scenario: Gate replica drains
- **WHEN** the replica holding the node's session sends `{"t": "drain"}`
- **THEN** the node's next hiring request goes on the new session it dialed

### Requirement: Blob Parts Fetched Through The Tunnel
A node MUST NOT dial a URL taken from a part. A `kontra-blob:<ref>` part in an A2A answer SHALL be fetched on a blob stream of the node's own tunnel, the gate checking that the ref's task is served by the session or owned by its hiring principal's buyer, and the node refusing more than 64 MiB or more bytes than announced. `/mcp` answers carry presigned links, relayed as they are (`spec/tunnel.md` §8.3; `spec/relay.md` §10.4; D56, D181, D303).

#### Scenario: Large artifact on a hired task
- **WHEN** a hired A2A task's artifact holds a part over 256 KiB
- **THEN** the node fetches it through its tunnel and its local client receives the bytes without the node dialing any URL from the part

#### Scenario: Ref of another buyer's task
- **WHEN** a node asks for a blob ref of a task neither served by its session nor owned by its hiring principal's buyer
- **THEN** the gate answers `{"error": "forbidden"}`

### Requirement: Sealing Around Forwarding
The node SHALL seal requests to a sealed service and open sealed answers around the forwarding, whatever the client, as the `sealing` capability specifies, and MUST refuse every unsealed part of a sealed job except a terminal status's gate text (`spec/tunnel.md` §9; `spec/ext-sealed.md` rule 4a; D342).

#### Scenario: Sealed service hired locally
- **WHEN** a local client sends a plain A2A message to a sealed service through the node
- **THEN** the gate receives sealed parts only

### Requirement: Hires Wait For Approval Off Protocol
A hire whose Admission returns a quote SHALL create the task `quoted` (A2A `TASK_STATE_INPUT_REQUIRED` with `metadata["kontra.approve.url"]`) and dispatch nothing until the operator's control plane sends `kontra.approval`. A message to a `quoted` task MUST be refused with `UnsupportedOperationError`: a person approves on the operator's page, never in the protocol (`spec/relay.md` §5.1, §7, §13.2; D38, D45, D188).

#### Scenario: Buyer tries to approve by message
- **WHEN** a hiring client sends a message to its `quoted` task
- **THEN** it gets `UnsupportedOperationError` and nothing is dispatched

#### Scenario: Quote expires
- **WHEN** a quote's `expiresAt` passes without approval
- **THEN** the task ends `canceled` with `kontra.canceled = "quote_expired"` and nothing was held
