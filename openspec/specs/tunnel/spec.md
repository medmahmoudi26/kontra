# tunnel Specification

## Purpose
The tunnel connects a node (`kontra connect`) to a gate (`kontra-gate serve`). The node opens one outbound WebSocket, and yamux multiplexes it. Over it the gate sends HTTP/1.1 exchanges to services on the node's localhost (hosting), and the node sends its local clients' requests to the gate (hiring). This capability covers the connection, the handshake and proof, stream preambles, the node's fixed-target proxy, blob streams, MCP servers that the node bridges (stdio, roots), liveness and the registry, the control stream, reconnects, limits and security properties. The node's local hiring endpoint (`--listen`, local request checks) belongs to `hiring`. Identity keys, enrollment, anonymous sessions, `kontra login` and `kontra publish` belong to `node-enrollment-and-keys`. Relay semantics belong to `relay-a2a` and `relay-mcp`, and sealed-part details belong to `sealing`. `spec/tunnel.md` is normative and wins over this file, together with the decisions it cites in `docs/decisions.md`.

## Known limitations
- A hello of the wrong shape with no listed subject (for example a `name` over 64 characters) is refused `unsupported_version`, which the node retries with backoff. No dedicated non-retryable code exists, so `kontra connect` validates `--name` and `--serve` before it dials (`internal/tunnel/handshake.go:380` SPEC-CONFLICT note). (`spec/tunnel.md` §4; D134)

## Requirements

### Requirement: Node-Initiated Connection Only
The node SHALL open every tunnel connection, and the gate MUST NOT open a connection to a node. Cancels, blob fetches and requests to hosted services MUST travel inside the tunnel that the node opened. The node is the `yamux.Client` and the gate is the `yamux.Server`. (`spec/tunnel.md` §1, §15; PRD §12)

#### Scenario: Gate reaches a hosted service
- **WHEN** a caller's request for a service hosted on a node arrives at the gate
- **THEN** the gate opens a yamux stream on that node's existing session
- **AND** the gate makes no TCP connection to the node's address

### Requirement: Tunnel Upgrade Request
`kontra connect` SHALL dial `GET {gate}/v1/tunnel` as a WebSocket upgrade with subprotocol `kontra-tunnel.v1`, a `User-Agent` of `kontra/<version> (<os>/<arch>)`, and no credential in any header. The gate MUST refuse an upgrade that does not offer the subprotocol with `400`. The node MUST close a connection on which the gate did not select the subprotocol. Compression is off, and the connection carries binary messages only after the handshake. (`spec/tunnel.md` §3)

#### Scenario: Upgrade without the subprotocol
- **WHEN** a client sends an upgrade to `/v1/tunnel` without `Sec-WebSocket-Protocol: kontra-tunnel.v1`
- **THEN** the gate answers `400` and no handshake starts

#### Scenario: Credentials stay out of headers
- **WHEN** `kontra connect` dials with an API key or enrollment tokens
- **THEN** the upgrade request carries none of them, and they travel only in the handshake's `hello`

### Requirement: Gate URL Scheme
`kontra connect` MUST accept only an `https://` value for `--gate`. The one exception is `http://` on `localhost`, `127.0.0.1` or `[::1]`. The default value is `https://gateway.kontra.sh`. (`spec/tunnel.md` §3)

#### Scenario: Plain HTTP to a remote gate
- **WHEN** `kontra connect --gate http://gate.example.com` is run
- **THEN** it refuses to start with a usage error naming the `https://` rule

### Requirement: Channel Binding Refusals
A gate that binds `tls-exporter` MUST refuse an upgrade that it cannot channel-bind. It answers `400` when the client's TLS has no exporter, and `500` when its own listener has no TLS. It MUST NOT downgrade such a connection to `cb = none`. The node SHALL retry these refusals as failed dials. A node started with `--require-channel-binding` MUST refuse a challenge whose `cb` is `none`. (`spec/tunnel.md` §3, §4, §4.1; D135)

#### Scenario: TLS 1.2 without Extended Master Secret
- **WHEN** the gate binds `tls-exporter` and the client's TLS connection exposes no exporter
- **THEN** the gate answers the upgrade `400`, and the node backs off and dials again

#### Scenario: Node requires channel binding
- **WHEN** a node started with `--require-channel-binding` receives a `challenge` with `cb: none`
- **THEN** the node does not send a proof and closes the connection

### Requirement: Four-Message Handshake Bounds
The handshake SHALL be four WebSocket text messages (`hello`, `challenge`, `proof`, then `welcome` or `error`), sent before yamux starts. Each message is a JSON object of at most 16 KiB, and both sides MUST enforce that read limit. The gate MUST close a connection whose handshake has not completed within 10 s of the upgrade. (`spec/tunnel.md` §4, §14)

#### Scenario: Oversized hello
- **WHEN** a node sends a `hello` larger than 16 KiB
- **THEN** the gate refuses it and no session is admitted

#### Scenario: Stalled handshake
- **WHEN** a client completes the upgrade and sends no `proof` within 10 s
- **THEN** the gate closes the connection

### Requirement: Hello Origin Binding
The gate MUST require the `hello`'s `origin` to equal the origin of its own `public_url`, in canonical form (lowercase scheme and host, default port elided), and MUST refuse any other origin with `bad_origin`. The `kid` MUST equal the key-id derivation of `recipient`, or the hello is `bad_proof`. (`spec/tunnel.md` §4, §2.2)

#### Scenario: Hello names another gate's origin
- **WHEN** a hello's `origin` differs from the gate's `public_url` origin
- **THEN** the gate answers `error` with code `bad_origin` and closes

### Requirement: Proof of Possession
The gate SHALL admit a key only after the HPKE export-only proof of §4.1 shows that the node holds the private key of `recipient`. The proof's `info` binds the dialed origin, the channel binding, the hash of the exact `hello` bytes, the session id and the gate nonce. The gate MUST compare the proof in constant time and refuse a mismatch with `bad_proof`. With `rotate_from`, the node MUST also prove the old key (`proof_old`). (`spec/tunnel.md` §4.1; D30; vectors in `internal/tunnel/testdata`)

#### Scenario: Proof relayed from another gate
- **WHEN** a malicious gate relays another gate's `challenge` to a node, and the node answers it
- **THEN** the proof is bound to the origin that the node dialed, and the other gate rejects it with `bad_proof`

#### Scenario: Replayed proof
- **WHEN** a recorded `proof` is sent on a new connection
- **THEN** it fails, because `enc`, the session id and both nonces are fresh for each connection

#### Scenario: Low-order encapsulated key
- **WHEN** a `challenge` carries a low-order `enc`
- **THEN** the node treats decapsulation as an error and sends no proof

### Requirement: Handshake Error Codes and Close Status
On refusal the gate SHALL send `error` with one code from the closed, ordered list of §4, then close with WebSocket status 4000 plus the code's index. A malformed hello takes the code of its subject (`bad_origin`, `bad_proof`, `service_denied`). Any other shape fault, or a binary message during the handshake, is `unsupported_version`. A gate that fails internally during admission MUST close with `1011` and send no `error`. (`spec/tunnel.md` §4; D134, D136)

#### Scenario: Close status mirrors the code
- **WHEN** the gate refuses a hello with `revoked`
- **THEN** it closes the WebSocket with status 4004

#### Scenario: Database unreachable during admission
- **WHEN** Postgres is unreachable while the gate admits a handshake
- **THEN** the gate sends neither `welcome` nor `error` and closes with `1011`

### Requirement: Node Retry Policy on Refusal
`kontra connect` MUST NOT redial after `bad_origin`, `bad_proof`, `revoked`, `enroll_invalid`, `service_denied`, `kind_mismatch` or `auth_invalid` until its operator changes something. It SHALL retry every other code, and an unknown code, with backoff. After a `1011` close it SHALL probe first, as `node-enrollment-and-keys` defines. (`spec/tunnel.md` §4, §13; D136)

#### Scenario: Revoked key
- **WHEN** the gate answers the node's hello with `revoked`
- **THEN** `kontra connect` exits with that error and does not redial

#### Scenario: Gate draining during handshake
- **WHEN** the gate answers `draining`
- **THEN** the node redials with backoff

### Requirement: Welcome Lists the Served Services
A `welcome` SHALL list in `services` only the declared services that the key is enrolled for after this handshake's enrollments, with each one's public URL. It SHALL carry `session`, `replica` and `ping_s`, and it carries `anonymous.expires_at` only for an anonymous session. A declared service whose binding was revoked is left out, and the handshake still succeeds for the others. (`spec/tunnel.md` §4, §5; D130; `key_scope`)

#### Scenario: One binding revoked
- **WHEN** a node declares services `a` and `b`, and its binding to `b` was revoked
- **THEN** `welcome.services` lists only `a`, and the session receives no request for `b`

### Requirement: Multiplexer Configuration and Write Deadline
After `welcome`, both sides SHALL run yamux over `websocket.NetConn` in binary mode with the identical configuration of §7: `AcceptBacklog` 64, keepalive off, write timeout 15 s, window 256 KiB, open timeout 30 s, close timeout 5 min. Every WebSocket write MUST have the 15 s deadline. Both sides enforce in-flight caps after `Accept`. The gate's first stream is the control stream. (`spec/tunnel.md` §7; D33, D155)

#### Scenario: Peer stops reading
- **WHEN** a node stops reading from the WebSocket while the gate has data to write
- **THEN** the gate's write fails after 15 s and the session closes

### Requirement: Hosting Stream Preamble
Each gate-opened hosting stream SHALL begin with one JSON preamble line of at most 4096 bytes, followed by exactly one HTTP/1.1 exchange for one service. The node MUST refuse with `404` a preamble for a service it does not host on this session (`Kontra-Node: not_hosted`) or of the wrong kind (`kind_mismatch`). The node never forwards the preamble and MUST NOT rely on `mcp_session` for 2026-07-28 exchanges. (`spec/tunnel.md` §8.1; D162, D180; `key_scope`)

#### Scenario: Preamble for a service not hosted
- **WHEN** the gate opens a stream whose preamble names a service that this session does not host
- **THEN** the node answers `404` with `Kontra-Node: not_hosted` and contacts no target

### Requirement: Fixed-Target Reverse Proxy
The node SHALL forward each hosting exchange only to the `--serve` target of the named service, ignoring the request's `Host` and any absolute-form target. It MUST confine the cleaned path to the target's base path (else `400`), set `Host` to the target's, strip `Origin`, hop-by-hop and `Forwarded`/`X-Forwarded-*` headers, and refuse `CONNECT`, `Upgrade` and `TRACE`. (`spec/tunnel.md` §8.2; D123, D210; `sidecar_not_a_proxy`)

#### Scenario: Path traversal
- **WHEN** the gate sends a request whose path, after percent-decoding and cleaning, is outside the target's base path
- **THEN** the node answers `400` and forwards nothing

#### Scenario: Foreign Host header
- **WHEN** a hosting request carries `Host: 169.254.169.254`
- **THEN** the node still sends it to the configured target's host and port only

#### Scenario: A2A target with a path
- **WHEN** `kontra connect --serve a2a:hello=http://127.0.0.1:9000/sub` is run
- **THEN** it refuses the target; an MCP target may name its endpoint path, and the gate's `/` maps to it

#### Scenario: CONNECT through the node
- **WHEN** the gate sends `CONNECT` or a request with `Upgrade` on a hosting stream
- **THEN** the node answers `400` without `Kontra-Node` and dials nothing

### Requirement: Node-Written Responses Are Marked
Every response the node writes itself SHALL carry `Kontra-Node`: `not_hosted` or `kind_mismatch` (404), `full` (503), `target_unreachable` (502), `sealed_rejected` (400). The node MUST strip `Kontra-Node` and `Kontra-Sealed-Reject` from the service's own responses. Its `400`s for refused methods, paths and unreadable preambles carry no `Kontra-Node`, so the gate fails the task `upstream_error`. (`spec/tunnel.md` §8.2; D151)

#### Scenario: Service spoofs the node header
- **WHEN** a hosted service returns a response that carries `Kontra-Node: full`
- **THEN** the node strips that header before the response reaches the gate

#### Scenario: Target down
- **WHEN** the configured target refuses the connection
- **THEN** the node answers `502` with `Kontra-Node: target_unreachable`

### Requirement: Blob Streams
The node SHALL fetch a `kontra-blob:<ref>` part over a stream it opens with an `op: blob` preamble, and MUST NOT dial a URL taken from a part. The gate MUST serve a ref only to a session serving its task or whose hiring principal bought it, and refuse objects over 64 MiB. The node MUST refuse an announced size over 64 MiB or extra bytes. (`spec/tunnel.md` §8.3; D56, D181, D209; `sidecar_not_a_proxy`)

#### Scenario: Blob of another tenant's task
- **WHEN** a node requests a blob ref whose task this session neither serves nor bought
- **THEN** the gate answers `{"error":"forbidden"}` and closes the stream

#### Scenario: URL part pointing at a private address
- **WHEN** a hosted exchange carries a `url` part naming `http://10.0.0.1/x`
- **THEN** the node dials nothing for it

#### Scenario: Blob fetch fails permanently
- **WHEN** a hosting exchange's `kontra-blob:` part is refused or answered with more bytes than announced
- **THEN** the node answers the exchange `400` without `Kontra-Node`

#### Scenario: Blob fetch may succeed later
- **WHEN** a blob fetch fails transiently (over the session's cap, or the bucket stalls for 30 s)
- **THEN** the node answers the exchange `503` with `Kontra-Node: full`, which the gate retries

### Requirement: Hiring Streams Carry the Session Principal
The node SHALL forward each accepted local request on a stream it opens, with the preamble `{"v":1,"op":"http"}` and the path unchanged. The gate MUST serve the request with the session's principal: the `auth` API key when one is present, else `node:<key_id>` in the key's tenant. It applies the session's edge limits and in-flight cap, which are not shared with the same API key's network requests. A local client never sends a credential to the gate. (`spec/tunnel.md` §9; D34, D207, D231)

#### Scenario: Two local clients share one principal
- **WHEN** two local MCP clients use the same node to reach one service
- **THEN** the gate counts both against one principal's `max_sessions_per_principal`

### Requirement: Node Vets the Gate's Hiring Answers
The node MUST serve a gate card locally only after verifying its signature against `{dialed origin}/.well-known/jwks.json`, and MUST rewrite its interface URLs to the node's own `localhost` URL for that service. It MUST NOT relay a gate `3xx` (it answers `502` without `Location`), and MUST remove `Mcp-Session-Id` from every answer but an `initialize`'s. (`spec/tunnel.md` §9; D247, D258, D259, D260)

#### Scenario: Gate redirects a hiring request
- **WHEN** the gate answers a forwarded request with `307` and a `Location`
- **THEN** the local client receives the node's `502` with no `Location`

#### Scenario: Unsigned card
- **WHEN** the gate's card for a service fails signature verification
- **THEN** the node does not serve that card to the local client

#### Scenario: Card interface pointing elsewhere
- **WHEN** the gate's signed card for `svc` names an interface URL on another path
- **THEN** the card served locally names `http://localhost:<port>{svc}` and keeps only `url`, `protocolBinding` and `protocolVersion` per interface

#### Scenario: Session id reused across services
- **WHEN** a local client uses an MCP session id initialized for one service on another service's path
- **THEN** the node answers `404` and the client initializes again

### Requirement: Stdio MCP Bridge
For `--serve mcp:<name>=stdio:<cmd>` the node SHALL run the command without a shell, split by POSIX word rules, in a scrubbed environment, one child per MCP session in its own process group. Unquoted shell metacharacters are a usage error. Stopping a child closes stdin, then sends `SIGTERM` and `SIGKILL` to its group 5 s apart. The bridge answers `400` with the legacy body to a non-`initialize` request without a session. (`spec/tunnel.md` §10; D161, D184, D211)

#### Scenario: Shell metacharacter
- **WHEN** `--serve 'mcp:fs=stdio:npx srv | tee log'` is given with an unquoted `|`
- **THEN** `kontra connect` refuses it as a usage error

#### Scenario: Session deleted
- **WHEN** a client sends `DELETE` for a stdio session
- **THEN** the child's stdin closes, and its process group receives `SIGTERM` 5 s later and `SIGKILL` 5 s after that if it is still alive

#### Scenario: Modern request to the bridge
- **WHEN** a request without a session id that is not `initialize` reaches a stdio service
- **THEN** the bridge answers `400` with the legacy body from `spec/relay.md` §11.12 rule 3

### Requirement: Stdio Bridge Session Cap and Queue
The node SHALL run at most `--max-sessions` (default 8) children per stdio service, counting stopping ones, and stop a child idle for `--session-idle` (default 1 h). Over the cap it MUST stop the longest-idle child with no exchange for at least 5 min, or else answer `503` with `Kontra-Node: full`. Server-to-client messages with no open `GET` stream SHALL be queued, up to 256 messages or 1 MiB per session, dropping the oldest. (`spec/tunnel.md` §10; D211, D213)

#### Scenario: Bridge full with busy children
- **WHEN** an `initialize` arrives with 8 children running, all with exchanges in the last 5 minutes and none stopping
- **THEN** the node answers `503` with `Kontra-Node: full`

#### Scenario: Queue flushed on GET
- **WHEN** the server sends notifications while no `GET` stream is open, and the client then opens one
- **THEN** the queued messages are delivered in order on that stream

### Requirement: Roots Stay on the Node
For every MCP service it hosts, stdio or URL, the node MUST be the server's only source of roots: it replaces the client's `capabilities.roots` in the forwarded `initialize`, answers the server's `roots/list` itself from `--roots`, and never relays it. It drops client `notifications/roots/list_changed` and root-bearing responses with `202`, and strips roots from 2026-07-28 exchanges. (`spec/tunnel.md` §10; D173, D212; `stdio_roots`)

#### Scenario: Client declares root file:///
- **WHEN** a remote client that declares the root `file:///` uses `server-filesystem@1.0.0`, which the node serves with `--roots` naming one directory
- **THEN** the client never receives `roots/list`, its first `tools/call` is answered in under 2 s, and `list_allowed_directories` names only the node's configured root

#### Scenario: Read outside the configured root
- **WHEN** that client asks the server to read `~/.kontra/identity.key`
- **THEN** the server refuses the read

#### Scenario: Unknown roots service
- **WHEN** `--roots docs=/srv` is given and no `--serve mcp:docs=…` exists
- **THEN** `kontra connect` refuses it as a usage error

#### Scenario: Case-variant key
- **WHEN** a client message carries both `roots` and `Roots` keys
- **THEN** the node answers `400` with `-32600`

### Requirement: Liveness Ping Loop
Both sides SHALL call `Session.Ping()` every 20 s and close the session after three consecutive failures, so a silent peer is detected within about 75 s. The gate MUST close a session whose node sends ping SYNs faster than a burst of 16, refilled at one every 500 ms. (`spec/tunnel.md` §11, §14; D33, D155)

#### Scenario: Silent node
- **WHEN** a node stops answering pings
- **THEN** the gate closes its session within about 75 s

#### Scenario: Ping flood
- **WHEN** a node sends more ping SYNs than the burst-16, one-per-500 ms allowance
- **THEN** the gate closes the session

### Requirement: Session Registry and Supersession
On `welcome` the gate SHALL insert a `tunnels` row, refresh `last_ping_at` every 60 s and delete it on close. A key SHALL hold at most one non-draining session per gate: a completed handshake supersedes the key's older session (earlier UUIDv7 session id), and two coexist only during a drain or rotation. Replicas silent for more than 30 s are removed with their tunnels. (`spec/tunnel.md` §11; D138, D139, D179; `replica_kill`)

#### Scenario: Same key connects twice
- **WHEN** a node completes a handshake while an older non-draining session of the same key exists on another replica
- **THEN** that replica is told through `NOTIFY kontra_tunnel` and closes the older session, and only the newer one is registered

#### Scenario: Replica dies
- **WHEN** a replica stops updating `gate_replicas` for more than 30 s
- **THEN** another replica deletes its row and its `tunnels` rows, and the relay selects none of its sessions

#### Scenario: Late registration of an older session
- **WHEN** a registration finds a newer session of its key on a replica that is not draining
- **THEN** it inserts nothing and closes

### Requirement: Control Stream Messages
The gate SHALL send JSON lines on the control stream: `drain`, `revoked` (whole key or one `service`), `auth_revoked`, `enrolled` and `expiring`. The node MUST NOT write on the control stream, and it ignores unknown messages. On `drain` the node MUST dial a new session at once, open no new hiring streams on the old one, keep serving inbound streams on it, and close it when the gate closes it. (`spec/tunnel.md` §13; D35)

#### Scenario: Replica drains
- **WHEN** the node receives `{"t":"drain"}`
- **THEN** it completes a new session before the old one closes, and in-flight hosted exchanges on the old session finish

### Requirement: Reconnect with Backoff
After any close other than a revocation or a non-retryable refusal, `kontra connect` SHALL redial with the same key, backing off exponentially from 1 s to 60 s with full jitter, reset after a session that lived 60 s. The node holds no task state: a task whose session is lost after its first upstream task id waits `task.reattach_grace` (default 15 min) for the key, or its rotation successor, to return. (`spec/tunnel.md` §13; D340, D345, D349; `tunnel_kill`)

#### Scenario: Node killed mid-task, restarted after the grace
- **WHEN** the node is SIGKILLed during a task and restarts later than `task.reattach_grace`
- **THEN** the task fails `tunnel_lost` after the grace, the node re-registers within 60 s, and the next task succeeds

#### Scenario: Node restarted within the grace
- **WHEN** the node restarts before `task.reattach_grace` ends
- **THEN** the task resumes on the new session

#### Scenario: Reconnect reporting
- **WHEN** the node loses the gate and reconnects after several failed attempts
- **THEN** it prints the loss with the next wait, each failed attempt with its next wait, and how long it was gone

### Requirement: Stream In-Flight Limits
The node SHALL cap inbound streams per session at `--max-inflight` (default 64), answering `503` with `Kontra-Node: full` above it. The gate SHALL cap node-opened streams per session at 32 (8 when anonymous), answering `429` above it. The gate MUST close a session with more than 256 gate-opened streams half-closed by the gate only. A stream is in flight until both halves end. (`spec/tunnel.md` §14; D137, D157, D182)

#### Scenario: Node over its inbound cap
- **WHEN** a 65th concurrent gate-opened stream arrives at a node with the default `--max-inflight`
- **THEN** the node answers it `503` with `Kontra-Node: full`

#### Scenario: Hiring flood
- **WHEN** a non-anonymous session has 32 hiring streams in flight and opens a 33rd
- **THEN** the gate answers the 33rd `429`

#### Scenario: Half-closed stream pile-up
- **WHEN** a node leaves more than 256 gate-opened streams open after the gate closed its half
- **THEN** the gate closes the session

