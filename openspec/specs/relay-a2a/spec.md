# relay-a2a Specification

## Purpose
The gate's A2A relay: how `kontra-gate` accepts A2A 1.0 requests from callers, authenticates them, serves a signed
card per service, keeps each task's state and event log in Postgres, dispatches it to the service through a tunnel
or a direct backend under the `TaskLifecycle` workflow, and streams, resumes, cancels and pushes it. It ends at the
tunnel's framing (capability `tunnel`), the MCP relay (`relay-mcp`), the gate's own `/mcp` (`gate-mcp-tools`), the
operator's hooks (`escrow`) and sealing (`sealing`). `spec/relay.md` §0–10 and §14 are normative; decisions are cited as Dn
from `docs/decisions.md`.

## Known limitations
- Built-in ACME serves one replica only; several replicas need provided certificates or a front proxy until a shared certificate cache exists (`spec/relay.md` §2.2; D70).
- Anonymous callers of one tenant share one `buyer_id`, so any of them who holds a task id can read and cancel that task (`spec/relay.md` §2.3; D20).

## Requirements

### Requirement: Public Routes
The gate's public listener SHALL serve only the routes of `spec/relay.md` §2.1 and MUST keep metrics on a separate listener. `{svc}` is `/agents/{name}` on a single-tenant gate and `/agents/{tenant}/{name}` on a multi-tenant one; a JSON-RPC request with a non-empty `tenant` is answered as an unknown service (`spec/relay.md` §2.1; D10, D18).

#### Scenario: Unlisted path
- **WHEN** a client requests a path that is not one of the §2.1 routes
- **THEN** no handler runs and the closed default route answers it

#### Scenario: Card at the service URL
- **WHEN** a client sends `GET /agents/hello`
- **THEN** the gate answers the served card, as for `GET /agents/hello/.well-known/agent-card.json`

#### Scenario: Health while draining
- **WHEN** a replica is draining
- **THEN** `GET /healthz` answers `503`

### Requirement: Unknown Service Answer
The gate MUST give the same `404`, with a body that fits the route, for a path naming no service, a service of the other kind, a disabled service, or one whose `allow:` does not admit the principal, on every route of the service including its cards, unless the service admits `anonymous` or is `listed`. The credential SHALL be checked before the service is looked up (`spec/relay.md` §2.1; D50, D87).

#### Scenario: Not admitted looks like absent
- **WHEN** an authenticated caller that `allow:` does not admit sends `SendMessage` to an existing service
- **THEN** the answer is HTTP `404` with JSON-RPC error `-32601` "unknown service", identical to a nonexistent service

#### Scenario: Bad credential
- **WHEN** a request carries an unknown API key for a path naming no service
- **THEN** the answer is `401`

### Requirement: Edge Limits
The gate SHALL apply body, rate, in-flight, header and connection limits per replica before authentication and before any task row exists: `413` for the body, `429` with `Retry-After` for rates and in-flight caps (also JSON-RPC `-32600` on JSON-RPC routes), `408` for a body not arrived within 30 s, and connection caps per client address and in all (`spec/relay.md` §2.2; D73, D84, D85).

#### Scenario: Body over the limit
- **WHEN** a JSON-RPC `SendMessage` body exceeds `limits.body` (default 4 MiB)
- **THEN** the gate answers `413` with JSON-RPC error `-32600` "request too large" and creates no task

#### Scenario: Streams are not idle
- **WHEN** an SSE response has no event for 15 s
- **THEN** the gate writes a `: keep-alive` comment and the idle timeout does not close it

### Requirement: Client Address
The gate SHALL count `rps_per_ip` and `conns_per_ip` by the client address that `edge.client_ip` names: the remote address by default, the rightmost untrusted `X-Forwarded-For` hop or the PROXY protocol source only on a connection from a `trusted` CIDR, an IPv6 address by its /64. Trusted proxy connections MUST be exempt from `conns_per_ip` but not from `max_conns` (`spec/relay.md` §2.2; D69, D73, D83).

#### Scenario: Forwarded-for from an untrusted peer
- **WHEN** `edge.client_ip.from` is `x_forwarded_for` and a connection from outside `trusted` sends `X-Forwarded-For`
- **THEN** the gate counts the request by the connection's remote address

### Requirement: TLS Termination
The gate SHALL terminate TLS itself, with ACME or a provided certificate, and MUST refuse plaintext unless `public_url` is a localhost origin or `tls.terminated_by_proxy` is true, in which case it logs a warning at start and its tunnel channel binding is `none` (`spec/relay.md` §2.2; D69, D70, D82).

#### Scenario: Plaintext off localhost
- **WHEN** the gate has an `https` non-localhost `public_url`, no certificate source and no `terminated_by_proxy`
- **THEN** it refuses to serve plaintext

### Requirement: Principals
The gate SHALL map each credential to a principal, a tenant (possibly none), a stable `buyer_id` and a `via` as `spec/relay.md` §2.3's table says: API key, OIDC JWT on A2A routes, MCP OAuth JWT, a node-opened stream, or nothing on an `anonymous` service. Derived buyer ids MUST be UUIDv5 of netstring names in the gate namespace, so they never change (`spec/relay.md` §2.3; D37, D68, D319).

#### Scenario: Derived node buyer id
- **WHEN** a node with key `k1_aaaaaaaaaaaaaaaaaaaaaaaaaa` hires without an API key
- **THEN** its `buyer_id` is `70c9ed92-910e-5b48-b506-040034a1d192`

### Requirement: Allow Lists
`allow:` SHALL deny by default and admit by `key:<glob>`, `org:<tenant>`, `oidc:sub:<subject>`, `oidc:*`, `node:<key_id>` and `anonymous`. `oidc:*` admits only OIDC subjects, `oidc:sub:*` MUST be refused at load, and `anonymous` is never implied; an authenticated caller of an `anonymous` service keeps its own principal (`spec/relay.md` §2.3; D301).

#### Scenario: oidc:* does not admit API keys
- **WHEN** a service's `allow:` is `[oidc:*]` and a caller presents an API key
- **THEN** the caller gets the unknown-service answer

#### Scenario: oidc:sub:* refused
- **WHEN** a service's `allow:` contains `oidc:sub:*`
- **THEN** the gate refuses the configuration at load

### Requirement: OIDC Principals On A2A Routes
With `auth.oidc`, the gate SHALL verify a bearer of JWT shape on an A2A route as that issuer's token with `aud` containing `auth.oidc.audience`, never as an API key. Audiences MUST NOT cross between A2A and MCP routes, the gate MUST refuse to start when that audience names one of its own resources, and an issuer, tenant or feed it cannot read MUST be its own failure, never `401` (`spec/relay.md` §2.3; D314, D315).

#### Scenario: MCP token on an A2A route
- **WHEN** a JWT whose `aud` is `{public_url}/mcp` is presented on `/agents/hello`
- **THEN** the answer is `401`

#### Scenario: Tenant claim names no tenant
- **WHEN** a token's `tenant_claim` value names no live `team` or `account` tenant
- **THEN** the answer is `401`

### Requirement: OIDC Revocation Feed
With `revocations` configured, the gate SHALL read the issuer's feed every `poll`, refuse with `401` a token whose sha256 of `sid` is listed or whose lifetime exceeds the window minus 120 s, and MUST fail closed: once no read has succeeded for `max_stale`, or before the first, no token of the issuer is accepted (`spec/relay.md` §2.3; D316).

#### Scenario: Revoked session
- **WHEN** the feed lists sha256 of a token's `sid`
- **THEN** the token is `401` on its next request

#### Scenario: Feed stale
- **WHEN** no feed read has succeeded for `max_stale`
- **THEN** every token of that issuer gets an internal error (`-32603`) on A2A routes

### Requirement: Anonymous Callers Share A Buyer
For the `anonymous` principal the gate SHALL refuse `ListTasks` (`UnsupportedOperationError`) and push configs, and SHALL let whoever holds a task id read and cancel that task, all anonymous callers of a team gate, or of one tenant on a multi-tenant gate, being one `buyer_id` (`spec/relay.md` §2.3; D20).

#### Scenario: Anonymous ListTasks
- **WHEN** an unauthenticated caller of an `anonymous` service sends `ListTasks`
- **THEN** the gate answers `UnsupportedOperationError`

### Requirement: Revocation Checked On Every Request
The gate SHALL check API keys, node keys and key–service bindings against a revocation cache refreshed by `NOTIFY kontra_revoke` and re-read from Postgres at least every 5 s, so that a revoked or expired API key MUST be `401` on its next request, including every later stream a node opens with it, while the node's hosting continues (`spec/relay.md` §2.3).

#### Scenario: Revoked key
- **WHEN** an API key is revoked and 5 s pass
- **THEN** its next request on any replica is `401`

### Requirement: A2A Protocol Version
The gate SHALL serve A2A 1.0 only: `A2A-Version` (header, else query parameter) `1.0` or `1.0.<patch>` is accepted as `1.0`, and an absent or empty version MUST be refused with `VersionNotSupportedError` (`-32009`) naming the header to send (`spec/relay.md` §2.4; D9).

#### Scenario: Missing version
- **WHEN** a `SendMessage` carries no `A2A-Version`
- **THEN** the gate answers `-32009` and creates no task

### Requirement: A2A Extension Activation
The gate SHALL split, trim and deduplicate `A2A-Extensions` (and its alias `X-A2A-Extensions`), ignore URIs the served card does not declare, and echo the activated URIs on every method. A message missing a URI the served card marks `required: true` MUST be refused with `-32008` before any task row; methods on an existing task and `GetExtendedAgentCard` MUST NOT be refused for it (`spec/relay.md` §2.4; D318).

#### Scenario: Required extension missing on a message
- **WHEN** a served card requires an extension and a `SendMessage` does not activate it
- **THEN** the gate answers `-32008` and creates no task row

#### Scenario: Required extension missing on GetTask
- **WHEN** the task's owner sends `GetTask` without activating the required extension
- **THEN** the gate answers the task

### Requirement: Strict JSON-RPC Decoding
The gate MUST refuse with `InvalidRequestError` (`-32600`, HTTP `400`) a JSON-RPC body in which a key repeats, or two keys are equal under Unicode case folding, at any level the gate or a2a-go reads, after the credential and service checks and before the version and extension checks, Admission and any task row (`spec/relay.md` §2.4; D172; test `a2a_case_smuggling`).

#### Scenario: Case-variant method
- **WHEN** a JSON-RPC body carries both `"method"` and `"METHOD"`
- **THEN** the gate answers HTTP `400` with `-32600` and creates no task row

### Requirement: Kontra Metadata Keys Are The Gate's
The gate MUST remove every `kontra.` key from caller metadata (except `kontra.sealed`, `kontra.sealed.replyTo` and the operator's `caller_metadata`) and from every upstream task, status, artifact, message and part (except `kontra.sealed` on a sealed-envelope part) before logging or forwarding, then write its own and the operator's keys (`spec/relay.md` §2.5; D51, D113, D174, D224; test `mcp_meta_spoof`).

#### Scenario: Upstream spoofs an approve URL
- **WHEN** an A2A upstream sends `metadata["kontra.approve.url"]` on a status
- **THEN** the caller never receives that key from the upstream

### Requirement: Unowned Extensions Pass Through
The gate SHALL relay extensions it does not own, A2UI included, without interpreting them: the seller's card entry served as written, a declared URI activated, echoed and sent upstream when the upstream card declares it, and data parts with their `metadata` and `mediaType` relayed equal under RFC 8785 JCS on both bindings (`spec/relay.md` §2.6; D14, D311, D326, D328; test `a2ui_passthrough`).

#### Scenario: A2UI activation
- **WHEN** a caller activates `https://a2ui.org/a2a-extension/a2ui/v0.9.1` on a service whose served card declares it
- **THEN** the response's `A2A-Extensions` names the URI and the upstream request carries it

#### Scenario: A2UI part over 256 KiB
- **WHEN** an agent sends an A2UI data part over 256 KiB and the gate has a bucket
- **THEN** the caller receives a `url` part with a presigned URL, keeping the part's `metadata` and `mediaType`

### Requirement: Data Parts To A2A 0.3 Agents
The gate SHALL send a data part whose value is not a JSON object to an A2A 0.3 agent as `{"value": <the value>}` with `metadata.data_part_compat: true`, and SHALL deliver such a part from an agent to the caller as the bare value without the flag; a data part's `mediaType` does not reach a 0.3 agent (`spec/relay.md` §2.6; D327).

#### Scenario: List of A2UI messages to a 0.3 agent
- **WHEN** a caller sends a data part holding a JSON array to a 0.3 agent
- **THEN** the agent receives `{"value": [...]}` with `metadata.data_part_compat: true`

### Requirement: Served Card Fetch
The gate SHALL fetch a service's card through its backend when a session connects or a direct backend is configured and every 10 minutes, at most 256 KiB, keep the upstream card verbatim, and store the served card as `json` produced from raw JSON, never through a typed struct. A tunnel service's card is the last good fetch through any of its sessions (`spec/relay.md` §3; D11, D49, D142, D143).

#### Scenario: Agent starts after its node
- **WHEN** a session's card fetch fails because its agent is not up yet
- **THEN** the session still becomes a candidate and the card is fetched again with backoff from 1 s to 1 min

### Requirement: Served Card Rewrite
The gate SHALL normalize a pre-1.0 card, or a 1.0 card with 0.x members, to the 1.0 shape and keep every seller field byte for byte except those it sets: `supportedInterfaces`, the security members, `capabilities.pushNotifications`, `streaming`, `extendedAgentCard`, gate- or operator-asserted extension entries, and `signatures`. A card with a repeated key or case variant of a member the gate reads or writes MUST NOT be installed (`spec/relay.md` §3; D12, D52, D88, D311; test `card_compat`).

#### Scenario: 0.2.5 upstream
- **WHEN** a service's upstream card is a 0.2.5 card
- **THEN** the served card is A2A 1.0 with the gate's JSONRPC and HTTP+JSON interfaces at `{public_url}{svc}` and every unknown seller field kept

#### Scenario: Case-variant Extensions
- **WHEN** an upstream card's `capabilities` holds both `extensions` and `Extensions`
- **THEN** the card is not installed

### Requirement: Served Card Signature
The gate SHALL sign every served card with exactly one ES256 JWS over JCS of the card without `signatures`, whose protected header carries `jku` and `kontra.upstream` (the upstream card's hash and URL, `verified`, and `jwks` when verified). An upstream card that is not valid UTF-8 MUST NOT be installed. Card responses SHALL carry `Cache-Control: max-age=60`, `ETag` and `Last-Modified` (`spec/relay.md` §3; D13, D193, D223; test `card_passthrough`).

#### Scenario: Stock verifier
- **WHEN** a2a-go's verifier checks a served card against `{public_url}/.well-known/jwks.json`
- **THEN** the signature verifies and `kontra.upstream.sha256` matches the card served at `agent-card.upstream.json`

### Requirement: Seller Signature Verification
The gate MUST verify seller card signatures only against the operator's configured `services.<name>.seller_keys` and never fetch a `jku` taken from a card. With keys configured and no verifying signature the service SHALL be unavailable: card routes answer `503`, and a new task fails `service_unavailable` at its first attempt; a failed fetch keeps the last verified card for 30 minutes (`spec/relay.md` §3; D13, D89).

#### Scenario: Seller signature fails
- **WHEN** `seller_keys` are configured and no signature of the upstream card verifies
- **THEN** the card routes answer `503` and a new task ends `failed: service_unavailable`

### Requirement: Task And Context Identifiers
The gate SHALL mint task ids as UUIDv7 and never accept a caller's; a message naming an unknown, unseen or unparseable `taskId` MUST be `TaskNotFoundError` (`-32001`). A caller's `contextId` SHALL be kept verbatim and scoped by `(tenant, service, buyer_id, context_id)`, the gate minting a UUIDv7 when there is none (`spec/relay.md` §4; D3; test `cross_buyer`).

#### Scenario: Another buyer's task
- **WHEN** buyer B sends `GetTask` for buyer A's task id
- **THEN** the answer is `TaskNotFoundError`

### Requirement: Upstream Identifiers
The gate MUST NOT send a caller-supplied or gate-minted context id upstream: a later task of a context carries the recorded upstream context id only on the same session or direct backend, and `referenceTaskIds` become the caller's own upstream ids or are dropped. Every upstream id in an event SHALL be rewritten to the gate's before it is logged; an event with another upstream task id fails the attempt `upstream_error` (`spec/relay.md` §4; D53, D92; test `cross_buyer`).

#### Scenario: Same context string, two buyers
- **WHEN** two buyers send the same `contextId` to the same service
- **THEN** their tasks reach two distinct upstream contexts

### Requirement: Task States And Terminal Rule
The gate SHALL keep one state vocabulary (`quoted`, `submitted`, `working`, `auth-required`, `input-required`, `completed`, `failed`, `canceled`, `rejected`) rendered in the reading endpoint's wire vocabulary, `quoted` being A2A `TASK_STATE_INPUT_REQUIRED` with `kontra.approve.url`. After a terminal event the store MUST refuse every write except `metadata` rows and an accepted review-window message (`spec/relay.md` §5.1; D21, D54).

#### Scenario: Late upstream event after cancel
- **WHEN** an upstream `completed` status arrives after the task was logged `canceled`
- **THEN** the store refuses it and the task stays `canceled`

### Requirement: One Transaction Per Event
Every change to a task SHALL be one Postgres transaction that updates the projection by version, never merging a stale one and refusing `PrevVersion == 0`, inserts the event at `seq` = the new version and notifies `kontra_task_events`. A dispatch's append MUST check its fence in the same transaction, and writes MUST run detached from the request's cancellation (`spec/relay.md` §5.2; D8, D49, D77; test `version_conflict`).

#### Scenario: Two replicas, same version
- **WHEN** two replicas call `Store.Update` with the same `PrevVersion`
- **THEN** exactly one commits, the other gets `ErrConcurrentModification`, and seqs have no gap

### Requirement: Event Log Contents And Retention
The event log MUST hold only what a caller may read and never credentials or request headers; `task_events` is append-only. Every terminal task, with its events, attempts, work items, push configs and bucket objects, SHALL be deleted at `retention_until` (default 7 days after the terminal event) by a sweep every 10 minutes, and closed `mcp_sessions` after the same retention (`spec/relay.md` §5.2–§5.4; D6, D95).

#### Scenario: Retention passes
- **WHEN** a task's `retention_until` has passed
- **THEN** within 10 minutes its row, events, attempts, work items, push configs and blobs are deleted

### Requirement: Session Choice
A new task's first attempt SHALL use the configured direct URL, or a connected session whose key is enrolled, unrevoked and bound to the service, on a live non-draining replica, chosen round-robin per replica and preferring the context's previous session. The service MUST come from the enrollment tables, never from the request; with no candidate the attempt fails `no_session`, retryably (`spec/relay.md` §6.1; D145).

#### Scenario: No candidate
- **WHEN** no session is a candidate for the service
- **THEN** the attempt fails `no_session` before any upstream task exists and is retried

### Requirement: Enrollment Check Before Every Stream
Before opening any stream to a node (dispatch, follow-up, poll, `abort`, `forward_cancel`, card fetch, pinned MCP request), the replica holding the session MUST check that the session is open, its key unrevoked and within any rotation overlap, and the service still in its served set; otherwise it opens no stream and handles the request as a lost session (`spec/relay.md` §6.1; test `key_scope`).

#### Scenario: Binding revoked
- **WHEN** a key's binding to `hello` is revoked while its session is connected
- **THEN** from the next request no stream for `hello` is opened on that session

### Requirement: Upstream Call
The dispatch SHALL call a direct backend at its configured `scheme://host:port` whatever the card's interface names, sending `A2A-Version`, the activated extensions the upstream card declares and the binding's content headers, and MUST NOT forward the caller's `Authorization`, cookies or other headers, or any gate credential (`spec/relay.md` §6.2; D71).

#### Scenario: Card points elsewhere
- **WHEN** a direct backend's card names an interface on another host
- **THEN** the gate still calls the configured host and port, and its dialer refuses any other

#### Scenario: Caller credential not forwarded
- **WHEN** a caller's request carries `Authorization` and cookies
- **THEN** the upstream request carries neither

### Requirement: Streaming And Polling Upstream
The dispatch SHALL stream when the upstream card says so and fall back to polling in the same attempt on `UnsupportedOperationError` before any event; polling sends `returnImmediately: true`, then `GetTask` every 1 s backing off to 5 s. It SHALL follow the upstream through `auth-required` and end a turn only on a terminal or `input-required` state (`spec/relay.md` §6.2; D15, D54, D96).

#### Scenario: Streaming refused
- **WHEN** an upstream advertising streaming refuses `SendStreamingMessage` with `UnsupportedOperationError` before any event
- **THEN** the same attempt continues on the polling path

### Requirement: Upstream Event Translation
The gate SHALL log upstream results as `status` and `artifact` events, never a second `Task` event, and only the difference from its projection on later snapshots. An upstream `Message` without a task SHALL become a terminal `completed` status carrying it, and an upstream terminal status without a message gets a gate-written one. Every terminal outcome MUST be an explicit event (`spec/relay.md` §6.3; D5, D8, D93).

#### Scenario: Message-only agent
- **WHEN** an upstream answers `SendMessage` with a bare `Message`
- **THEN** the caller's task ends `completed` with that message as `status.message`

### Requirement: Retries Before The First Upstream Task Id
The gate SHALL make a new attempt, a fresh upstream `SendMessage` of the caller's original message counted against `task.retries` (default 2), only before the current attempt's first upstream task id exists, following `spec/relay.md` §6.4's failure table, and MUST NOT send that message again after it. Retries are the workflow's, never Temporal's (`spec/relay.md` §6.4; D99, D100, D146, D147).

#### Scenario: Retries spent with no session
- **WHEN** no session becomes a candidate through every retry
- **THEN** the task ends `failed: no_session` and the gate task id never changes

#### Scenario: 429 after the upstream task exists
- **WHEN** a direct upstream answers HTTP 429 after it returned a task id
- **THEN** the task fails `upstream_error` without a retry

### Requirement: Re-attach
After the first upstream task id, a lost direct dispatch SHALL be resumed by a new dispatch of the same attempt that calls `GetTask` on the recorded upstream id, logs the difference and follows the task, failing `upstream_unreachable` only after 30 s of failing `GetTask`. Resumes back off from 1 s to 30 s, and an 11th MUST fail the task `upstream_error` "re-attach limit reached" (`spec/relay.md` §6.4; D42, D97; test `replica_kill`).

#### Scenario: Dispatching replica killed, direct backend
- **WHEN** the replica running a direct task's dispatch is killed mid-task
- **THEN** the task completes by re-attaching on another replica and the caller's SSE resumes with no gap or duplicate

### Requirement: Follow-Up Delivery On Resume
A follow-up turn's dispatch SHALL record `delivered_at` when the upstream first answers it, before logging anything from that answer. A resume of a follow-up turn without `delivered_at` MUST send the follow-up again with the same `messageId`, then re-attach; with it set, it re-attaches without sending. A task's first turn is never re-sent this way (`spec/relay.md` §6.4; D81, D98).

#### Scenario: Replica dies before the upstream answered
- **WHEN** a follow-up's dispatch is lost before the upstream answered it
- **THEN** the resume sends the follow-up again with the same `messageId`

### Requirement: Dispatch Fencing
Every dispatch (a new attempt, a follow-up turn, a resume) MUST write a fresh `attempts.dispatch_id` when it starts, and every append it makes MUST commit only while its attempt is the task's latest, not ended, and holds that id; a fenced write aborts the dispatch (`spec/relay.md` §6.4; D77).

#### Scenario: Stale dispatch fenced
- **WHEN** a dispatch that Temporal timed out but whose process is still alive appends after a newer dispatch started
- **THEN** its write is refused and it aborts

### Requirement: Tunnel Reconnect Wait
A tunnel attempt of an A2A task whose session is lost after its first upstream task id MUST NOT fail at once: the workflow SHALL wait durably, up to `task.reattach_grace` (default 15 minutes) and never past the deadline, for a candidate session of the same node's key or a key it was rotated to, woken by `kontra.reconnected` and by reading Postgres, then re-attach there. MCP tasks keep `failed: tunnel_lost` (`spec/relay.md` §6.4; D340, D342, D344, D347; test `tunnel_reattach`).

#### Scenario: Node restarts within the grace
- **WHEN** the hosting node is killed mid-task and starts again within the grace
- **THEN** the task resumes on the node's new session and completes with every artifact

#### Scenario: Replica holding the session killed
- **WHEN** the replica holding the node's session is killed mid-task
- **THEN** the node redials the other replica and the task completes

### Requirement: Reconnect Wait Announced In Band
Entering a reconnect wait, the gate SHALL log a `working` status with the text "The agent's node disconnected. Waiting up to 15 minutes for it to reconnect." and `kontra.reconnect` `waiting` (metadata only for a sealed service; nothing at `input-required` or `auth-required`). The re-attach SHALL log `resumed`, and a terminal status written during the wait MUST carry `ended` (`spec/relay.md` §6.4; D341, D342; test `tunnel_reattach`).

#### Scenario: Caller sees the wait
- **WHEN** a task's node disconnects and returns within the grace
- **THEN** the caller's stream shows the `waiting` status, then the `resumed` one, with no artifact between them

### Requirement: Reconnect Wait Gives Up
A reconnect wait SHALL end the task `failed: tunnel_lost` when the grace ends, when the attempt's key chain or its binding to the service is revoked, when the agent answers the re-attach's `GetTask` with `TaskNotFoundError`, or when an 11th wait or resume would be needed, each with a text saying why (`spec/relay.md` §6.4; D342, D343; test `tunnel_reattach`).

#### Scenario: Node never returns
- **WHEN** the hosting node does not reconnect within the grace
- **THEN** the task ends `failed: tunnel_lost` saying the node did not reconnect, with `kontra.reconnect` `ended`

#### Scenario: Seller restarted too
- **WHEN** the node returns but the agent no longer has the task
- **THEN** the task fails `tunnel_lost` saying the agent no longer has the task

### Requirement: Failure Reasons
A gate-written failure SHALL be a terminal `failed` status with one agent text part and `metadata["kontra.failure"]` one of `no_session`, `tunnel_lost`, `upstream_unreachable`, `upstream_error`, `service_unavailable`, `timeout`, `sealed_rejected`, `lifecycle_unavailable`; for a sealed service the text MUST be the gate's own. A declined or expired quote ends `canceled`, not failed (`spec/relay.md` §6.4; D195).

#### Scenario: Sealed upstream error text
- **WHEN** a sealed service's upstream fails with an error message quoting its input
- **THEN** the task's failure text is the gate's and does not contain the upstream's message

### Requirement: Task Timeout
`task.timeout` (default 1 h) SHALL be wall clock from the transition into `submitted`, regardless of state, with `tasks.deadline` fixed then and never moved; when it passes the lifecycle MUST send `forward_cancel` upstream when an upstream task exists and its session is up, and write `failed: timeout` without waiting for the answer (`spec/relay.md` §6.5).

#### Scenario: Timeout at input-required
- **WHEN** a task waits at `input-required` past its deadline
- **THEN** it ends `failed: timeout` and its attempt is recorded `interrupted`

### Requirement: Upstream Limits
The gate MUST read every upstream body through a byte limit and fail the exchange `upstream_error` (`service_unavailable` for a card) beyond it: 8 MiB for a non-streaming body, 4 MiB per SSE event, 256 KiB per card, and `limits.task_bytes` (default 256 MiB) stored per task. For a sealed service events SHALL NOT be coalesced (`spec/relay.md` §6.6; D103, D194).

#### Scenario: Oversized SSE event
- **WHEN** an upstream stream sends one event over 4 MiB
- **THEN** the task fails `upstream_error`

### Requirement: Follow-Ups And Input Required
A message with a task's `taskId` SHALL be accepted only while the task is `input-required`: it is appended to history and dispatched on the same attempt's session to the same upstream task, or waits for the node by the reconnect wait when the session is gone. A message to a task in any other state, outside a review window, MUST be refused with `UnsupportedOperationError` and not forwarded (`spec/relay.md` §7; D54, D81; test `input_required_sticky`).

#### Scenario: Sticky follow-up
- **WHEN** a caller answers an `input-required` task
- **THEN** the answer reaches the same upstream task on the same session

#### Scenario: Message to a working task
- **WHEN** a caller sends a message to a `working` task
- **THEN** it gets `UnsupportedOperationError` and nothing is forwarded

### Requirement: Sequential Turns
Every follow-up SHALL be a `kontra.message` Update whose validator applies the follow-up rules on workflow state, so turns are strictly sequential and two racing follow-ups MUST NOT both be accepted. A refused follow-up's work item is deleted; one not accepted within 10 s gets `-32603` and its work item is kept. A `taskId` and `contextId` that disagree are `InvalidParams` (`spec/relay.md` §7; D41, D66, D105).

#### Scenario: Racing follow-ups
- **WHEN** two follow-ups to the same `input-required` turn arrive together
- **THEN** exactly one is accepted and the other gets `UnsupportedOperationError`

### Requirement: Review Window Messages
A message to a `completed` task with an open review window SHALL be stored as a work item and sent as the `kontra.review_message` Update, where the operator's `OnReviewMessage` decides: accepted, it is appended as a `message` event; refused, nothing is logged, the work item is deleted and the caller gets `UnsupportedOperationError`. It MUST never be forwarded upstream (`spec/relay.md` §7, §14.1; D51, D66, D106, D109, D110).

#### Scenario: Hook declines
- **WHEN** `OnReviewMessage` declines a review-window message
- **THEN** the caller gets `UnsupportedOperationError` and the task's log is unchanged

#### Scenario: Message after the window
- **WHEN** a caller sends a message to a completed task after its review window closed
- **THEN** it is refused `UnsupportedOperationError` and nothing is logged

### Requirement: Cancel
`CancelTask` SHALL be handled before a2a-go's executor as the `kontra.cancel` Update, awaited up to 60 s and completing only once `canceled` is logged. A terminal task MUST be `TaskNotCancelableError`; a `quoted` task is `canceled` with `kontra.canceled = "client"`; an active task without an upstream task has its dispatch aborted in process before `canceled` is written. Concurrent cancels share one result (`spec/relay.md` §8; D4, D44, D79).

#### Scenario: Cancel of an ended task
- **WHEN** a caller cancels a `completed` task
- **THEN** it gets `TaskNotCancelableError`

#### Scenario: Cancel before the upstream task exists
- **WHEN** a caller cancels a task whose dispatch has no upstream task id yet
- **THEN** the dispatch is aborted, the attempt is recorded `aborted`, and the task ends `canceled`

### Requirement: Cancel Forwarded Upstream
For a task with an upstream task, the gate SHALL send `CancelTask` upstream inside the tunnel the node opened (or to the direct URL) and answer what the upstream answers with gate ids: the task continues on an upstream error, a still-running upstream task or a node-written answer. With the session gone it MUST write `canceled` at once and forward the cancel if the node returns within the grace (`spec/relay.md` §8; D107, D148, D346; test `cancel_forwarded`).

#### Scenario: Cancel of an input-required task
- **WHEN** a caller cancels an `input-required` task whose session is up
- **THEN** the upstream receives `CancelTask`, the task ends `canceled`, and no `completed` follows

#### Scenario: Upstream refuses the cancel
- **WHEN** the upstream answers the forwarded cancel with `TaskNotCancelableError`
- **THEN** the caller gets that error and the task continues

### Requirement: Gate-Written Streams
The gate SHALL serve `SendStreamingMessage` and `SubscribeToTask` on both bindings from its own event log, never the upstream connection: one SSE event per carried `task`, `status` or `artifact` row with `id: <seq>` and one `data:` line, a leading snapshot without `id:`, errors returned before SSE headers, and the stream closed after the first row whose state is terminal, `input-required` or `quoted` (`spec/relay.md` §9; D2, D108, D109, D341).

#### Scenario: Reconnect status does not end a stream
- **WHEN** a task enters the reconnect wait during a `SendStreamingMessage`
- **THEN** the stream carries the `working` status with `kontra.reconnect` and stays open

#### Scenario: Subscribe to an ended task
- **WHEN** a caller sends `SubscribeToTask` without `Last-Event-ID` to a terminal task
- **THEN** the gate answers `UnsupportedOperationError` before any SSE header

### Requirement: Stream Resume With Last-Event-ID
`SubscribeToTask` with `Last-Event-ID: K`, K a seq of the task, SHALL send the task snapshot as of seq K without an `id:` line, then every carried row with seq > K exactly once in seq order, then the live tail. A K that is not a seq of the task MUST be `InvalidParams`, and a K at or past a terminal task's terminal seq `UnsupportedOperationError` (`spec/relay.md` §9; D2, D75; test `relay_resume`).

#### Scenario: Resume after a drop
- **WHEN** a caller's stream drops after seq 5 and it resubscribes with `Last-Event-ID: 5`
- **THEN** it receives the snapshot as of 5 and every carried row after 5 once, in order

### Requirement: Push Configs
The gate SHALL store push configs per gate task, saving an inline config before the work item so a refused URL is a synchronous `InvalidParams`, idempotently keyed by (task, URL, token hash), at most 10 per task and 256 delivering per buyer. A config set on an ended task MUST be stored but receive nothing, and the upstream's own push configs are never used (`spec/relay.md` §10.1; D110, D111).

#### Scenario: Config on an ended task
- **WHEN** a push config is set on a task that has already ended
- **THEN** it is stored and listed but receives nothing

### Requirement: Push Delivery
Push SHALL be delivered by the lifecycle, never by the relaying replica: one `push_webhook` activity per config on queue `kontra-push` posts the task snapshot at each seq in order, at least once, with a 10 s timeout and no redirect followed, retrying from 1 s to 5 min for at most 24 h, at most 10 a second per destination host across the gate. The workflow MUST NOT complete while a push it started is pending (`spec/relay.md` §10.2; D16, D55, D72, D78, D112; test `push_delivery`).

#### Scenario: Webhook down for two minutes
- **WHEN** the webhook is down for 2 minutes and then returns
- **THEN** every status change and the terminal snapshot are delivered, in seq order

### Requirement: SSRF-Safe Dialer
Every connection the gate opens to a URL it was given (push URLs, direct backends, `seller_keys` URLs) SHALL resolve the host, check every address against the IANA special-purpose registries after unmapping IPv4-mapped, NAT64, 6to4 and Teredo forms, and pin the address it uses; only `https://` unless `allow_private`. It MUST never follow redirects or use the environment's proxy (`spec/relay.md` §10.3; D17, D60, D67; test `ssrf_webhook`).

#### Scenario: Private push URL
- **WHEN** a caller sets a push URL at `10.0.0.1`, `[::ffff:10.0.0.1]`, `64:ff9b::a00:1`, `169.254.169.254` or a name resolving to a private address
- **THEN** the gate refuses it with `InvalidParams` and opens no connection

#### Scenario: Redirecting push URL
- **WHEN** a push URL answers `302`
- **THEN** the delivery counts as failed and the redirect target receives no connection

### Requirement: Operator-Configured Endpoints
The Admission hook, the OIDC issuers with their metadata and key sets, revocation feeds and the bucket SHALL skip only the private-address check: `https://` only (or `http://` to a loopback host, connected to loopback only), redirects never followed, the environment's proxy ignored, and answers bounded and timed out as `spec/relay.md` §10.3 lists (`spec/relay.md` §10.3; D192, D284).

#### Scenario: IdP on a private network
- **WHEN** `auth.oidc.issuer` is an `https` URL resolving to a private address
- **THEN** the gate reads its metadata and key set

### Requirement: Large Parts And Blob Refs
Parts up to 256 KiB SHALL be stored inline; larger ones go to the bucket as `kontra-blob:<ref>` parts, delivered to callers as presigned URLs valid 15 minutes and to nodes as refs fetched through their own tunnel. Without a bucket a caller part over 256 KiB MUST be `InvalidParams` ("part too large: configure blobs"); a `kontra-blob:` part from a caller MUST be `InvalidRequest` (`spec/relay.md` §10.4; D56, D167, D181).

#### Scenario: Caller forges a blob ref
- **WHEN** a caller sends a `url` part `kontra-blob:abc`
- **THEN** the gate refuses the request as `InvalidRequest`

#### Scenario: Service sends a blob ref
- **WHEN** an upstream sends a part whose URL starts with `kontra-blob:`
- **THEN** the task fails `upstream_error`

### Requirement: TaskLifecycle Workflow
Each gate task SHALL have one `TaskLifecycle` workflow, id `task-<task_id>`, on queue `kontra-lifecycle` in the gate's own namespace, branching only on its start input and activity results. Its history MUST NOT carry relayed events or caller or service content: only ids, states, small outcomes and patches of at most 16 KiB. A task takes at most 1,000 turns (`spec/relay.md` §14, §14.1; D59, D115).

#### Scenario: Config reload
- **WHEN** the gate's configuration is reloaded while a task runs
- **THEN** the running task keeps the timeout and retries of its start input

### Requirement: Lifecycle Updates And Work Items
Interactions refused synchronously SHALL be Updates with read-only validators (`kontra.message`, `kontra.cancel`, `kontra.approval`, `kontra.review_message`, `kontra.annotate`), the rest signals. The work queue MUST store a sanitized payload in `work_items`, keeping no header but `A2A-Version` and `A2A-Extensions`, and pass Temporal only its id. An Update not accepted within 10 s maps to `-32603` (`spec/relay.md` §8.1, §14.1; D6, D41, D63, D66, D94, D101).

#### Scenario: No Authorization in Temporal
- **WHEN** a `SendMessage` carrying `Authorization` and cookies starts a task
- **THEN** the workflow's history holds only the work item id, and the work item holds no `Authorization` or cookie

#### Scenario: Lifecycle worker absent
- **WHEN** no `kontra-lifecycle` worker accepts a follow-up's Update within 10 s
- **THEN** the caller gets `-32603` and the work item is kept

### Requirement: Lifecycle Activities And Per-Replica Queues
The workflow SHALL run `dispatch` on `kontra-tunnel-<replica>` or `kontra-direct` (schedule-to-start 15 s, heartbeat 30 s, one attempt), `abort` and `forward_cancel` on per-replica control queues with `kontra-direct-ctl` as fallback, `push_webhook` on `kontra-push`, and the rest on `kontra-lifecycle`. Activities on a per-replica queue MUST NOT rely on Temporal's retries (`spec/relay.md` §14.2; D79, D102, D120, D145, D149).

#### Scenario: Replica dies
- **WHEN** the replica owning a dispatch's queue is killed
- **THEN** the dispatch times out within a minute and the workflow applies the retry table from what Postgres recorded

### Requirement: Replica Draining
On `SIGTERM` a replica SHALL mark itself draining, fail `/healthz`, stop accepting tunnels, tell its nodes to reconnect elsewhere and stop taking new attempts, while it keeps polling its lifecycle, push, control and per-replica queues until its last dispatch ends or `drain_timeout` (default 10 min) passes, then settles 2 s and exits; open streams get 5 s (`spec/relay.md` §14.3; D76, D118, D119, D141, D150, D348).

#### Scenario: Rolling deploy
- **WHEN** a replica holding a tunnel task's session drains past `drain_timeout`
- **THEN** the task resumes on the node's new session on another replica and does not fail

### Requirement: Workflow Versioning And Namespace Access
Every change to `TaskLifecycle`'s behaviour SHALL go behind `workflow.GetVersion`, with CI replaying recorded histories; gate workers are unversioned. No node or caller MUST ever reach Temporal: the namespace credential is held only by the operator's workers and control-plane backend, and a self-hosted frontend's port is never published (`spec/relay.md` §14.4; D46).

#### Scenario: Compose deployment
- **WHEN** the gate runs from the provided compose file
- **THEN** Temporal's port 7233 is reachable only on the internal network

### Requirement: Orphaned Tasks
A sweep on any replica SHALL, once a minute, end tasks in `submitted` or `quoted` older than 2 minutes whose workflow does not exist with `failed: lifecycle_unavailable`, written through the store. A resend that matches an existing task MUST re-issue `UpdateWithStart` with the original work item id, so a lost start gets its workflow (`spec/relay.md` §14.5; D66, D290).

#### Scenario: Lost start
- **WHEN** a task row exists in `submitted` for over 2 minutes without a workflow
- **THEN** it ends `failed: lifecycle_unavailable`
