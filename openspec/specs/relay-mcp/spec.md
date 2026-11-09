# relay-mcp Specification

## Purpose
The MCP relay is the gate's route `/mcp/{svc}`: it serves each `kind: mcp` service as a Streamable HTTP MCP
endpoint for protocol revisions 2025-11-25 and 2025-06-18 ("legacy") and 2026-07-28 ("modern") on the same
endpoint, adding authentication, `allow:`, per-tool policy, limits, session affinity and, for task-augmented calls,
a gate-owned task on the shared `TaskLifecycle`. It covers hosting only: listings, prices and the `calls` metering
record belong to `listings-and-pricing`; Admission, quotes and the Lifecycle hooks to `escrow`; the gate's own `/mcp`
endpoint to `gate-mcp-tools`; the node side of a hosted server to `tunnel`; sealed calls to `sealing`.
`spec/relay.md` §11 (with §2.5, §6.1, §13.4 and §16) is normative and wins over this file.

## Known limitations
- A client that learned "modern" from one service on the gate's origin and does not probe again fails on a
  legacy-only, sealed or stdio service of the same origin with the legacy `400`; `mcp_era_fallback` logs this
  residual and does not assert it (`spec/relay.md` §11.12 "One origin, several eras"; D177).
- Without `peer.listen`, a gate of more than one replica ends a session whenever its balancer sends a later request
  to a replica other than the one holding the tunnel (`spec/relay.md` §11.2; D163, D250).
- The gate does not check that the operator's `tool_meta` and `initialize_meta` annotations hold only `sh.kontra/`
  keys, as `spec/relay.md` §13.4 describes them; it merges whatever object the service row holds
  (`internal/gate/services.go:860-868`).

## Requirements

### Requirement: Raw JSON Relay
The relay SHALL work on raw JSON and SHALL NOT decode and re-encode a message through a typed MCP SDK. Unknown
fields and unknown methods MUST pass through unchanged, except Kontra's own `sh.kontra/` keys. (`spec/relay.md` §11
intro, §2.5; D19)

#### Scenario: Unknown field passes through
- **WHEN** an upstream `tools/list` result carries a tool with an `execution` member and an unknown member `x-foo`
- **THEN** the client receives both members unchanged

### Requirement: One Message Per POST
The gate SHALL refuse every POST body on `/mcp/{svc}` that is not a single JSON object, before any forwarding, with
HTTP `400` and an id-less JSON-RPC error `-32600` (`batch not supported`), whatever `MCP-Protocol-Version` says.
(`spec/relay.md` §11.1; `mcp_batch_refused`)

#### Scenario: Batch refused
- **WHEN** a client POSTs an array of two `tools/call` requests, one of them to a denied tool
- **THEN** the gate answers `400` with `-32600` and no `id`
- **AND** no upstream receives any traffic, and no Admission call or `calls` row is made

### Requirement: Strict Decoding Of Client Messages
The gate MUST decode every client message it acts on with exact, case-sensitive keys and MUST refuse one in which,
at a level `spec/relay.md` §11.1 lists, a key repeats, two keys are equal under Unicode case folding, or a lone case
variant stands for a member it reads. The refusal is HTTP `400` with `-32600`, before policy, metering, Admission or
forwarding. (`spec/relay.md` §11.1; D172, D191; `mcp_case_smuggling`)

#### Scenario: Case-variant method smuggling
- **WHEN** a client's `tools/call` carries both `"method"` and `"METHOD"`
- **THEN** the gate answers `400` with `-32600`
- **AND** nothing reaches the upstream, Admission or the `calls` table

#### Scenario: Case-variant tool name
- **WHEN** a client's `tools/call` carries `"name": "echo"` (denied) and `"NAME": "add"`, or repeats `name`
- **THEN** the gate answers `400` with `-32600` and nothing is forwarded

### Requirement: Strict Decoding Of Upstream Messages
The gate MUST decode upstream messages it acts on with the same exact-key rules and MUST NOT relay one that fails:
a JSON response becomes HTTP `502` with `-32603` `upstream error`, an SSE stream ends before that event, and a
dispatch fails `upstream_error`. It SHALL read an upstream answer as a client reads it: a leading BOM dropped, only
events typed absent or `message` relayed, ids matched as the TypeScript SDK matches them. (`spec/relay.md` §11.1;
D172, D197)

#### Scenario: Case-variant isError from upstream
- **WHEN** an upstream answers a call with `{"isError": true, "ISERROR": false}` in its result
- **THEN** the client receives `502` with `-32603` and the upstream result is not relayed

#### Scenario: Non-boolean isError
- **WHEN** an upstream result's top-level `isError` is the string `"false"`
- **THEN** the message is treated as malformed and not relayed

#### Scenario: Other event types dropped
- **WHEN** an upstream event stream holds an event of type `ping` before the response
- **THEN** the client's stream carries the response and not the `ping` event

### Requirement: Protocol Versions
The gate SHALL accept `MCP-Protocol-Version` `2025-11-25` and `2025-06-18` on a session, an absent header meaning
the session's negotiated version, and SHALL treat `2026-07-28` as a modern request. If an upstream's `initialize`
negotiates another version, the gate MUST answer the client `-32602` naming the supported versions and send the
upstream a best-effort `DELETE`. On `2025-06-18` sessions it SHALL advertise no tasks. (`spec/relay.md` §11.1
Versions)

#### Scenario: Upstream negotiates an unsupported version
- **WHEN** an upstream answers `initialize` with `protocolVersion: "2024-11-05"`
- **THEN** the client receives `-32602` naming the supported versions, and the upstream receives a `DELETE`

#### Scenario: 2025-06-18 session has no tasks
- **WHEN** a client initializes with `2025-06-18` on a `durable` service
- **THEN** the `initialize` result advertises no `tasks` capability and tools carry no `execution`

### Requirement: No Legacy HTTP+SSE Transport
The gate SHALL NOT serve the 2024-11-05 HTTP+SSE transport to clients nor speak it to upstreams; it SHALL answer a
client's legacy `GET` probe with `405`. Upstreams that only speak it can be bridged on the node. (`spec/relay.md`
§11.11; D23)

#### Scenario: Legacy SSE probe
- **WHEN** a client sends `GET /mcp/{svc}` with `Accept: text/event-stream` and no session
- **THEN** the gate answers `405`

### Requirement: Request Header Handling
The gate SHALL forward `Accept`, `Content-Type` and `MCP-Protocol-Version` from the client (plus, on modern requests,
`Mcp-Method`, `Mcp-Name` and `Mcp-Param-*`). It MUST validate the client's `Authorization` at the gate and MUST NOT
forward it, and MUST refuse with `403` an `Origin` that `mcp.allowed_origins` does not list. (`spec/relay.md` §11.1
Request headers)

#### Scenario: Client token never reaches the upstream
- **WHEN** a client calls a tool with `Authorization: Bearer <api key>`
- **THEN** the upstream request carries no `Authorization` header

#### Scenario: Origin not allowed
- **WHEN** a POST carries `Origin: https://evil.example` and `mcp.allowed_origins` does not list it
- **THEN** the gate answers `403` and forwards nothing

### Requirement: Response Header Handling
The gate SHALL drop every upstream response header except `Content-Type` and `Cache-Control`, SHALL replace
`Mcp-Session-Id` with its own, and MUST turn an upstream `401`, `403` or `407` into HTTP `502` with `-32603`
`upstream refused`, so that the only `WWW-Authenticate` a client sees is the gate's. (`spec/relay.md` §11.1 Response
headers)

#### Scenario: Upstream asks for authorization
- **WHEN** the upstream answers `401` with a `WWW-Authenticate` header
- **THEN** the client receives `502` with `-32603` `upstream refused` and no upstream `WWW-Authenticate`

#### Scenario: Upstream cookie dropped
- **WHEN** the upstream's response carries `Set-Cookie`
- **THEN** the client's response carries no `Set-Cookie`

### Requirement: Kontra Keys Removed From MCP Messages
The gate SHALL remove every `sh.kontra/` key, matched under Unicode case folding, from client `params._meta` and
`result._meta` and from an upstream's protocol-level `_meta` locations and capabilities, keeping only the exact
sealed envelope on a sealed service, before it writes its own keys and the operator's. It MUST NOT descend into
`content`, `structuredContent` or `arguments`. (`spec/relay.md` §2.5; D51, D174; `mcp_meta_spoof`)

#### Scenario: Upstream spoofs an approve URL
- **WHEN** an upstream result carries `_meta["sh.kontra/approve-url"]`
- **THEN** the client receives the result without that key

#### Scenario: Upstream claims to be sealed
- **WHEN** an upstream's `initialize` result carries `capabilities.experimental["sh.kontra/sealed"]`
- **THEN** the client's `initialize` result does not carry it

#### Scenario: Content is not scrubbed
- **WHEN** a tool result's `structuredContent` holds a member named `sh.kontra/x`
- **THEN** the client receives it unchanged

### Requirement: Session Initialization
The gate SHALL forward a legacy `initialize` to an upstream chosen as `spec/relay.md` §6.1 says, record the
upstream's `Mcp-Session-Id`, and mint its own (128 random bits, base64url, 22 characters), stored only as its sha256
in `mcp_sessions`, bound to the principal, with `replica_id` naming the replica that holds the tunnel. The result's
`_meta` SHALL carry the operator's `initialize_meta`, then `sh.kontra/session_idle_s`. (`spec/relay.md` §11.2; D179,
D224)

#### Scenario: Gate session id replaces the upstream's
- **WHEN** a client initializes and the upstream returns `Mcp-Session-Id: up-1`
- **THEN** the client receives a 22-character gate session id, never `up-1`
- **AND** the `initialize` result's `_meta["sh.kontra/session_idle_s"]` is the service's `session_idle_s` as an integer

#### Scenario: Operator cannot override the idle bound
- **WHEN** the operator's `initialize_meta` sets `sh.kontra/session_idle_s` to 60 on a service whose `session_idle_s` is 3600
- **THEN** the client's `initialize` result carries 3600

### Requirement: Session Affinity
The gate SHALL route every later request carrying the gate's session id to the same tunnel session, after the
enrollment check of `spec/relay.md` §6.1, with the upstream's id substituted. It SHALL relay a `POST`'s event stream
without the upstream's event ids and end a `GET` stream before any response. A client notification or response SHALL
be answered `202` with no body when the upstream answered `2xx`, its error status bare otherwise. (`spec/relay.md`
§11.2; D265, D307)

#### Scenario: Pinned request
- **WHEN** a client sends `tools/list` with its gate session id
- **THEN** the request reaches the same node session as its `initialize`, carrying the upstream's session id

#### Scenario: Notification answered 202
- **WHEN** a client POSTs `notifications/initialized` and the upstream answers `200` with an event stream
- **THEN** the client receives `202` with no body, and the upstream's stream is closed unread

#### Scenario: POST stream has no event ids
- **WHEN** an upstream answers a pinned request with an SSE stream whose events carry `id:` fields
- **THEN** the events the client receives carry no `id:` field

### Requirement: Session Not Found
The gate SHALL answer `404` with `-32001` to a session id that is unknown, closed, bound to another principal, or
presented on another service's path. A pinned tunnel session that is gone or revoked SHALL close the row
(`tunnel_lost` or `revoked`) and answer `404`; an upstream `404` on a live tunnel SHALL close it `upstream_closed`.
(`spec/relay.md` §11.2; D196; `mcp_session_cross_node`, `mcp_session_revoked`)

#### Scenario: Session presented on another service
- **WHEN** a client presents the session id it got on `/mcp/a` to `/mcp/b`
- **THEN** the gate answers `404` with `-32001` and opens no stream

#### Scenario: Binding revoked while pinned
- **WHEN** the node's binding for the service is revoked while a session is pinned, and the node ignores the control message
- **THEN** the next request on that session is `404`, opens no stream, and the row is closed `revoked`

### Requirement: Session Deletion And Idle Close
The gate SHALL forward a `DELETE` with the session id upstream and close the row `deleted`, answering `200` even when
the upstream's tunnel cannot be reached. It SHALL close a session idle for the service's `session_idle_s` (default
3600) as `idle`, with a best-effort upstream `DELETE`, and SHALL write `last_seen_at` at most once a minute.
(`spec/relay.md` §11.2; D196, D250)

#### Scenario: Delete after the node left
- **WHEN** a client sends `DELETE` on a session whose node has disconnected
- **THEN** the gate answers `200` and the row is closed `deleted`

#### Scenario: Idle session
- **WHEN** a session sees no request for longer than `session_idle_s`
- **THEN** the gate closes it `idle`, and its next request is `404`

### Requirement: Session Cap Per Principal
For a tunnel-backed service, the gate SHALL let a principal other than `anonymous` hold at most
`services.<name>.max_sessions_per_principal` (default 4) open legacy sessions. An `initialize` beyond the cap MUST
close only that principal's sessions idle at least 2 minutes, oldest first, else be refused `429` with `-32600`
`too many sessions`. Virtual sessions, a task's own upstream session, `anonymous` and direct backends are not
capped. (`spec/relay.md` §11.2; D213, D231, D249)

#### Scenario: Fifth session with none idle
- **WHEN** a principal holding 4 sessions of a tunnel service, each used within the last minute, sends a fifth `initialize`
- **THEN** the gate answers `429` with `-32600` `too many sessions`, `data.max_sessions_per_principal` and a `Retry-After`, and closes none

#### Scenario: Idle session evicted
- **WHEN** one of the 4 sessions has been unused for 3 minutes and the principal sends a fifth `initialize`
- **THEN** that session is closed `idle`, its upstream receives a `DELETE`, and the new session opens

#### Scenario: Session in use is never evicted
- **WHEN** a session has a `GET` stream open on another replica for 10 minutes
- **THEN** a new `initialize` of its principal does not close it

### Requirement: Peer Hop To The Holding Replica
When a request needs a tunnel session held by another replica and `peer.listen` is set, the gate SHALL forward it to
that replica's peer listener with a `Kontra-Peer` assertion signed by HMAC-SHA256 under `peer.secret_env`. The holder
MUST refuse an assertion older than 30 s, a seen nonce, or headers that do not hash to it, MUST build the upstream
request from signed data only, and its listener MUST answer any other route `404`. (`spec/relay.md` §11.2; D163,
D179; `mcp_peer_hop`)

#### Scenario: Pinned call through the peer hop
- **WHEN** the client reaches replica A and the node's tunnel is on replica B
- **THEN** `initialize`, a pinned `tools/call` and its SSE stream complete through B's peer listener

#### Scenario: Replayed or altered assertion
- **WHEN** a `Kontra-Peer` request is replayed, older than 30 s, or its upstream session id is altered after signing
- **THEN** the holding replica refuses it and opens no stream

#### Scenario: Rules applied once
- **WHEN** a request crosses the peer hop
- **THEN** authentication, `allow:`, per-tool policy and metering are applied by the receiving replica only

### Requirement: Single Replica Without Peer Listener
Without `peer.listen`, the gate SHALL consider only this replica's tunnel sessions as candidates, and a pinned
session held by another replica SHALL be answered `404` and its row closed `tunnel_lost`. A direct `mcp: {url}`
backend is reached from any replica and never hops. (`spec/relay.md` §11.2; D250)

#### Scenario: Pinned session on another replica
- **WHEN** a request for a session held by another replica reaches a replica without `peer.listen`
- **THEN** the gate answers `404` and closes the row `tunnel_lost`

### Requirement: Per-Tool Policy
The gate SHALL apply the service's `tools: {allow, deny}` globs (`*` only; deny wins; an absent `allow` allows all,
an empty one none) to the body's `params.name`, never to a header. It SHALL answer a `tools/call` of a denied or
unknown tool before dispatch with `-32602` `Unknown tool: <name>`, exactly as for a tool that does not exist.
(`spec/relay.md` §11.3; D198; `mcp_tool_denied`)

#### Scenario: Denied tool called
- **WHEN** a service sets `tools: {deny: ["rm*"]}` and a client calls `rm_rf`
- **THEN** the gate answers `-32602` `Unknown tool: rm_rf` and nothing reaches the upstream

#### Scenario: Empty allow list
- **WHEN** a service sets `tools: {allow: []}`
- **THEN** `tools/list` returns no tools and every `tools/call` is `-32602` `Unknown tool`

### Requirement: Drained Tools List
On `tools/list` the gate SHALL drain every upstream page within `spec/relay.md` §6.6's limits, drop denied tools,
remove Kontra's keys, apply the operator's `tool_meta` annotation to each tool's `_meta` (`"*"` first, then the
tool's own entry, each replacing a same-named key), and answer one page with no cursor. It MUST NOT forward a cursor
it did not mint. (`spec/relay.md` §11.3, §13.4)

#### Scenario: Paginated upstream list
- **WHEN** the upstream returns its tools in three pages
- **THEN** the client receives one page holding every allowed tool and no `nextCursor`

#### Scenario: Operator annotation on a tool
- **WHEN** the service's `tool_meta` is `{"*": {"sh.kontra/a": 1}, "echo": {"sh.kontra/a": 2}}`
- **THEN** `echo` carries `_meta["sh.kontra/a"] = 2` and every other tool `1`

### Requirement: Tools List Cache
The gate SHALL cache the drained list per service and refresh it on upstream `notifications/tools/list_changed`
(which is forwarded, and told to the other replicas with `NOTIFY kontra_tunnel, 'tools:<tenant_id>/<service>'`) and
every 10 minutes. A replica whose `LISTEN` connection comes back SHALL drop every cached list. (`spec/relay.md` §11.3;
D199, D230)

#### Scenario: List changed on another replica
- **WHEN** a node sends `notifications/tools/list_changed` through replica A
- **THEN** replica B's next `tools/list` for that service is drained afresh, not served from its old cache

### Requirement: Plain Tool Calls
The gate SHALL forward a plain `tools/call` on the pinned session after per-tool policy and, when `call` is among the
Admission operations, Admission (see `escrow`), and SHALL record it as `listings-and-pricing` describes. A hosting
node's refusal of a sealed call SHALL reach the client as HTTP `200` with `-32602` `sealed part rejected: <reason>`
under the request's id, and be audited. (`spec/relay.md` §11.4; D202)

#### Scenario: Plain call relayed
- **WHEN** a client calls an allowed tool without `params.task`
- **THEN** the upstream receives the call on the pinned session and the client receives its response

#### Scenario: Sealed call refused by the node
- **WHEN** the hosting node refuses a sealed call's envelope
- **THEN** the client receives HTTP `200` with JSON-RPC `-32602` `sealed part rejected: <reason>` and an `audit` row is written

### Requirement: Tasks Modes
The gate SHALL apply the service's `tasks:` mode: `durable` (default) advertises the upstream's `capabilities.tasks`
plus `list` and `cancel` and owns the task; `passthrough` forwards `params.task` and relays `tasks/*` on the pinned
session; `off` removes `capabilities.tasks` and every tool's `execution` and answers `params.task` with `-32601`. The
gate MUST NOT advertise tasks the upstream does not support, nor run the task shape over a plain upstream call.
(`spec/relay.md` §11.5)

#### Scenario: Off service refuses a task
- **WHEN** a client sends a `tools/call` with `params.task` to a `tasks: off` service
- **THEN** the gate answers `-32601`

#### Scenario: Passthrough task
- **WHEN** a client sends a task-augmented call to a `passthrough` service
- **THEN** the upstream's `CreateTaskResult` reaches the client, and its `tasks/get` is forwarded on the pinned session

### Requirement: Required Tasks Mode
On a `tasks: required` service the gate SHALL behave as `durable`, except that `tools/list` leaves out the tools whose
upstream `execution.taskSupport` is absent or `forbidden` and shows `taskSupport: "required"` on every other, a plain
`tools/call` is `-32601`, and a call of a left-out tool is `-32602` `Unknown tool`; neither reaches Admission, the
upstream or a `calls` row. (`spec/relay.md` §11.5; D322)

#### Scenario: Required service refuses a plain call
- **WHEN** a legacy client sends a plain `tools/call` to a `tasks: required` service
- **THEN** the gate answers `-32601`, nothing reaches Admission or the upstream, and no `calls` row is written

#### Scenario: Forbidden tool hidden
- **WHEN** the upstream declares a tool with `execution.taskSupport: "forbidden"`
- **THEN** the client's `tools/list` leaves it out, and a call of it is `-32602` `Unknown tool`

### Requirement: Gate-Owned MCP Task Creation
For a task-augmented `tools/call` on a `durable` or `required` service the gate SHALL resolve the tool against the
cached list (unknown or denied: `-32602`; `taskSupport` absent or `forbidden`: `-32601`; no task either way), run
Admission with operation `task`, create a gate task (`protocol = mcp`) on `TaskLifecycle`, and answer at once with a
`CreateTaskResult` carrying the gate's task id, `status: "working"` and `pollInterval: 1000`. (`spec/relay.md` §11.5
steps 0–2)

#### Scenario: Durable call answered with the gate's task
- **WHEN** a client sends a task-augmented `tools/call` to a `durable` service
- **THEN** it receives a `CreateTaskResult` whose `taskId` is the gate's, never the upstream's

#### Scenario: Tool that forbids tasks
- **WHEN** a client sends a task-augmented call of a tool whose upstream `taskSupport` is `forbidden`
- **THEN** the gate answers `-32601` and creates no task

### Requirement: Gate-Owned MCP Task Dispatch
The dispatch activity SHALL open its own upstream session (with the client's `elicitation` and `sampling`), send the call with `task.ttl` until the deadline and the client's JSON-RPC id, record the first
upstream task id, poll `tasks/get` at the upstream's `pollInterval` bounded to 0.5–30 s, and on a terminal upstream
status store `tasks/result` as the terminal event. An upstream `-32602` there after the first id SHALL fail the task
`upstream_error`. (`spec/relay.md` §11.5 steps 3–6; D22)

#### Scenario: Dispatch on its own session
- **WHEN** a gate-owned MCP task is dispatched
- **THEN** the upstream receives an `initialize` from the dispatch, and the task-augmented call on that session, not the client's

#### Scenario: Upstream forgets the task
- **WHEN** the upstream answers `tasks/get` with `-32602` after accepting the task
- **THEN** the gate task fails `upstream_error`, `upstream task expired`

### Requirement: Gate-Written Input Requests
The gate SHALL log each upstream `elicitation/create` or `sampling/createMessage` of a gate-owned task as an
`input_request` event, its id rewritten to the string `k.<taskId>.<seq>` and its related-task id to the gate's, and
deliver it only on the client's `tasks/result` stream. A client response to a `k.` id MUST be accepted once and only
from a session of the task's buyer (otherwise `202` and dropped), then written upstream under the original id.
(`spec/relay.md` §11.5 step 5, §11.7)

#### Scenario: Second answer dropped
- **WHEN** a client answers input request `k.T.4` twice
- **THEN** the first answer reaches the upstream under the original request id, and the second is `202` and dropped

#### Scenario: Foreign answer dropped
- **WHEN** another buyer's session sends a response with id `k.T.4`
- **THEN** the gate answers `202` and nothing reaches the upstream

### Requirement: Gate-Written Streams
The gate MUST NOT write into a relayed upstream stream and SHALL NOT send `notifications/tasks/status` on
`/mcp/{svc}`. Gate-served SSE events SHALL carry `id: k.<taskId>.<seq>`, and a `GET` whose `Last-Event-ID` starts
with `k.` SHALL be served by any replica from `task_events`, for the same principal only, never forwarded; it ends
when the task is terminal, without the answer to the `tasks/result` it resumed. (`spec/relay.md` §11.7; D203)

#### Scenario: Resume on any replica
- **WHEN** a client reconnects to replica B with `Last-Event-ID: k.T.3` after a stream served by replica A
- **THEN** replica B serves the task's pending input requests after seq 3 without forwarding the `GET` upstream

### Requirement: Client Side Of A Gate-Owned MCP Task
The gate SHALL answer `tasks/get`, `tasks/result` and `tasks/list` of gate-owned tasks from Postgres only, on any
replica: `tasks/get` the projection plus the operator's `mcpMeta`; `tasks/result` blocks until terminal, delivering
pending input requests first; `tasks/list` the principal's own MCP tasks of the service, newest first, refused for
`anonymous`. A call naming another buyer's task MUST be `-32602`, and upstream task ids MUST NOT reach the client.
(`spec/relay.md` §11.6)

#### Scenario: Another buyer's task
- **WHEN** buyer B calls `tasks/get` with buyer A's task id
- **THEN** the gate answers `-32602`, as for an unknown task

#### Scenario: Result after completion
- **WHEN** a client calls `tasks/result` on a running task that later completes
- **THEN** the call returns the stored result with `_meta["io.modelcontextprotocol/related-task"]` naming the gate's task id

### Requirement: MCP Task Cancel
On `tasks/cancel` of a non-terminal gate-owned task the gate SHALL run `abort` (no upstream task yet) or send upstream
`tasks/cancel`, wait at most 10 s, then write `canceled` whatever the upstream answered and return the task with
`status: "cancelled"`; later upstream events MUST be refused. A cancel of a terminal task SHALL be `-32602`.
(`spec/relay.md` §11.6; D57, D203)

#### Scenario: Cancel always cancels
- **WHEN** a client cancels a running task and the upstream never answers its `tasks/cancel`
- **THEN** within 10 s the client receives the task with `status: "cancelled"`, and a later upstream `completed` is not recorded

#### Scenario: Cancel of a finished task
- **WHEN** a client cancels a `completed` task
- **THEN** the gate answers `-32602`

### Requirement: Upstream Outcomes On MCP
A `tasks/result` on a terminal gate-owned task SHALL return exactly what the task-augmented call returned: the
upstream's own result, or its JSON-RPC error verbatim with ids rewritten (a sealed service's error with its code
only). An upstream JSON-RPC error SHALL end the task `failed`, with `_meta["sh.kontra/failure"] = "upstream_error"`
on `tasks/get`. (`spec/relay.md` §11.8 case 1; D195)

#### Scenario: Upstream error returned verbatim
- **WHEN** the upstream answers the task with JSON-RPC error `-32000` and `data: {"x": 1}`
- **THEN** `tasks/result` returns that code, message and data, and `tasks/get` shows `failed` with `sh.kontra/failure` `upstream_error`

### Requirement: Gate Failures And Cancels On MCP
For a failure the gate wrote, `tasks/get` SHALL show `status: "failed"`, the reason text as `statusMessage` and
`_meta["sh.kontra/failure"] = "<reason>"`, and `tasks/result` SHALL return `-32603` with the reason text and
`data["sh.kontra/failure"]`. A client cancel SHALL return `-32603` `task cancelled` with
`data["sh.kontra/canceled"] = "client"`, and an upstream's own cancel the same without `data`. (`spec/relay.md` §11.8
cases 2–3; D203)

#### Scenario: Gate-written failure
- **WHEN** a gate-owned task fails `tunnel_lost`
- **THEN** `tasks/get` shows `failed` with `_meta["sh.kontra/failure"] = "tunnel_lost"`
- **AND** `tasks/result` returns `{"code": -32603, "message": <reason text>, "data": {"sh.kontra/failure": "tunnel_lost"}}`

#### Scenario: Client cancel outcome
- **WHEN** a client cancels its task and then calls `tasks/result`
- **THEN** it receives `-32603` `task cancelled` with `data["sh.kontra/canceled"] = "client"`

### Requirement: MCP Events In The Log And Replica Loss
MCP task events SHALL use the shared `task_events` rows (`task`, `status`, `input_request`, `input_response`, the
terminal `result`), with retries, timeouts and cancel as for A2A. After the first upstream task id a lost tunnel SHALL
fail the task `tunnel_lost`, and the caller's `tasks/result` MUST still return on any replica and never hang.
(`spec/relay.md` §11.9; `mcp_task_resume`)

#### Scenario: Replica holding only the client connection dies
- **WHEN** the replica holding the client's connection is killed during a task-augmented call
- **THEN** `tasks/result` on the other replica returns the task's result

#### Scenario: Replica holding the tunnel dies after the upstream task exists
- **WHEN** the replica holding the tunnel is killed after the first upstream task id
- **THEN** `tasks/result` on the other replica returns the `failed: tunnel_lost` form, without hanging

### Requirement: Stdio And Roots At The Relay
The gate SHALL see a node's stdio MCP server as a Streamable HTTP target like any other and SHALL forward
`roots/list`, `notifications/roots/list_changed` and the `roots` capability like any other message. The node answers
`roots/list` from its own `--roots` and drops the rest, so a client's roots MUST NOT reach a hosted server.
(`spec/relay.md` §11.10; D173; node side in `tunnel`)

#### Scenario: Client roots stay out
- **WHEN** a client declares the `roots` capability and a hosted server asks `roots/list`
- **THEN** the server receives the node operator's configured roots, never the client's

### Requirement: Request Era Classification
After the one-message rule, strict decoding and the `Origin` check, the gate SHALL classify each request: `initialize`
is legacy (without an `id`, `400` `-32600`); `MCP-Protocol-Version: 2026-07-28` is modern, its session headers
ignored; `2025-11-25`, `2025-06-18` or absent is legacy and needs a session; any other value is `400` `-32022`; `GET`
and `DELETE` without a session or with `2026-07-28` are `405`. (`spec/relay.md` §11.12 Classification; D158, D201,
D312)

#### Scenario: Legacy request without a session
- **WHEN** a client POSTs `tools/list` with `MCP-Protocol-Version: 2025-11-25` and no `Mcp-Session-Id`
- **THEN** the gate answers `400` with `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Bad Request: No valid session ID provided"}}`

#### Scenario: Unsupported version header
- **WHEN** a client sends `MCP-Protocol-Version: 2024-11-05` on a POST
- **THEN** the gate answers `400` with `-32022` whose `data.supported` lists `2026-07-28`, `2025-11-25` and `2025-06-18`

### Requirement: Modern Request Checks
A modern request MUST carry `_meta` `io.modelcontextprotocol/protocolVersion` and `clientCapabilities` (else `400`
`-32602`), a `protocolVersion` equal to the header, and `Mcp-Method` and `Mcp-Name` equal to the body (else `400`
`-32020`), checked before policy. A modern JSON-RPC response SHALL be `400` `-32600`, and a modern notification
`202`, not forwarded. (`spec/relay.md` §11.12 Checks; D176)

#### Scenario: Header mismatch
- **WHEN** a modern request's `_meta` `protocolVersion` differs from its `MCP-Protocol-Version` header
- **THEN** the gate answers `400` with `-32020` and nothing reaches the upstream

#### Scenario: Mcp-Name mismatch
- **WHEN** a modern `tools/call` names `echo` in its body and `add` in `Mcp-Name`
- **THEN** the gate answers `400` with `-32020` before per-tool policy

### Requirement: Modern Request Routing
The gate SHALL pin nothing for a modern request: each goes to a session chosen as for an `initialize`, the peer hop,
or the direct backend, with the version and `Mcp-*` headers forwarded and the body changed only by Kontra-key removal
and the rewrites. The gate MUST NOT translate between eras: an upstream's `400`, `404` or `405` to a modern request is
relayed to that client. (`spec/relay.md` §11.12 Where a modern request goes; D204)

#### Scenario: Modern request not pinned
- **WHEN** a modern client sends two `tools/call` requests to a service with two connected nodes
- **THEN** each is routed by session choice, and no `Mcp-Session-Id` is minted for either

### Requirement: Service Era Learning
The gate SHALL learn whether a service speaks 2026-07-28 only from a `server/discover` result or its own probe (one per
service at a time), never from the answer to a client's request. While a service is legacy-only it SHALL answer every
modern request with the legacy `400` `-32000` without forwarding. The era is cached like the tools list and dropped
when a node of the service connects or disconnects. (`spec/relay.md` §11.12 The service's era; D177, D199, D204;
`mcp_era_fallback`)

#### Scenario: Legacy-only upstream is probed, not flooded
- **WHEN** three modern client requests to a legacy-only service arrive within a minute
- **THEN** the upstream receives the gate's own `server/discover` and none of the clients' modern requests
- **AND** each client gets the legacy `400`, and its call succeeds after `initialize`

#### Scenario: One principal's 404 does not demote the service
- **WHEN** a dual-era upstream answers one principal's modern request `404`
- **THEN** that client receives the `404`, and another principal's next modern request is still forwarded and answered modern

### Requirement: Modern Discover And Tools Rewrites
The gate SHALL forward `server/discover` and rewrite its result: `supportedVersions` the upstream's versions the gate
speaks plus `2025-11-25` and `2025-06-18`, the tasks extension kept only for a `durable` service, Kontra keys removed,
the operator's `initialize_meta` merged, `cacheScope: "private"`. A modern `tools/list` SHALL be the drained, filtered,
annotated list with `ttlMs` at most 600000 and `cacheScope: "private"`. (`spec/relay.md` §11.12 Rewrites)

#### Scenario: Discover advertises legacy versions
- **WHEN** a dual-era upstream's `server/discover` lists only `2026-07-28`
- **THEN** the client's result lists `2026-07-28`, `2025-11-25` and `2025-06-18` with `cacheScope: "private"`

### Requirement: Modern Task Field Rewrites
On modern requests the gate SHALL remove `params.task`, answer `tasks/result` and `tasks/list` `404` with `-32601`,
and remove the client's `io.modelcontextprotocol/tasks` capability from every request it does not turn into a
gate-owned task. A `subscriptions/listen` asking for `notifications.taskIds` without that extension SHALL be `400`
`-32021`; otherwise it is forwarded with `taskIds` removed. (`spec/relay.md` §11.12 Rewrites; D176)

#### Scenario: Modern tasks/result refused
- **WHEN** a modern client sends `tasks/result`
- **THEN** the gate answers `404` with `-32601`

### Requirement: Modern Gate-Owned Tasks
A modern `tools/call` declaring the tasks extension, on a `durable` or `required` service whose upstream declared it,
SHALL become a gate-owned task (Admission operation `task`); any other is a plain call, which `required` refuses
(`-32021` or `-32601`). The gate SHALL wait up to 2 s and answer in line, else with `CreateTaskResult`
(`pollIntervalMs: 1000`), only after the task row commits; an MCP task is never quoted (`spec/relay.md` §11.12;
D159, D322, D350; `mcp_modern_task`).

#### Scenario: Fast task answered in line
- **WHEN** a modern task-declaring call's upstream completes within 2 s
- **THEN** the client receives the `CallToolResult` with `resultType: "complete"` and never sees a task

#### Scenario: Client leaves during the wait
- **WHEN** the client closes its response stream during the 2 s wait
- **THEN** it is sent no answer, and the task is sent `kontra.cancel` and ends `canceled` within 10 s

### Requirement: Modern Task Dispatch
The dispatch SHALL speak to the upstream in the era of the request that created the task, sending stateless requests
with the stored client `_meta`; a `CreateTaskResult`'s `taskId`, or a first `InputRequiredResult`, is the first
upstream task id, after which the call MUST NOT be sent afresh. Answers go upstream as `tasks/update` under the
upstream keys, or as the call resent with `inputResponses` and the upstream's `requestState`. (`spec/relay.md` §11.12
Dispatch; D178, D205)

#### Scenario: Tunnel lost after an InputRequiredResult
- **WHEN** the tunnel is lost after the upstream answered the dispatch's `tools/call` with an `InputRequiredResult`
- **THEN** the task fails `tunnel_lost`, and the upstream never receives that `tools/call` again

### Requirement: Modern Client Side Of A Task
The gate SHALL answer modern `tasks/get` (`input_required` with `inputRequests` keyed by gate id), `tasks/update` (each
outstanding key logged once as `input_response`) and `tasks/cancel` (`kontra.cancel` sent, not awaited) from Postgres
on any replica. Each MUST declare the tasks extension (else `400` `-32021`), and another buyer's or an unknown task is
`-32602`. (`spec/relay.md` §11.12 The client side; D176)

#### Scenario: Modern task with one question
- **WHEN** a modern client's task asks one question
- **THEN** `tasks/get` is `input_required` with one gate-keyed request, `tasks/update` answers it, and a second answer to the same key is ignored
- **AND** `tasks/get` then shows `completed` with the result, the same from either replica

#### Scenario: Missing extension
- **WHEN** a modern client sends `tasks/get` without declaring the tasks extension
- **THEN** the gate answers `400` with `-32021` naming the extension

### Requirement: Virtual Sessions For Modern Clients
The gate SHALL keep one virtual session per principal and modern service: an `mcp_sessions` row with
`protocol_version = '2026-07-28'` and no upstream or tunnel ids, opened (after Admission operation `session` when
consulted) once the upstream answered modern, or the request was served from the modern cache or became a gate task;
never on a legacy `400`. It pins nothing, is closed `idle` after `session_idle_s`, and is its `calls` rows'
`mcp_session`. (`spec/relay.md` §11.12; D160, D177, D204)

#### Scenario: Concurrent first requests
- **WHEN** two first modern requests of one principal to a dual-era service are in flight at once
- **THEN** exactly one virtual session row is written, and both requests name it

#### Scenario: No session on a legacy answer
- **WHEN** a modern request's probe ends in the legacy `400`
- **THEN** no virtual session row is opened

### Requirement: Features Not Offered To Modern Clients
The gate SHALL NOT offer modern clients tasks of `passthrough` services, `notifications/tasks`, resumable streams,
`-32042`, sealed services, or stdio services. A modern request to a sealed or stdio service MUST be answered with the
legacy `400` so that a dual-era client falls back to `initialize`. (`spec/relay.md` §11.12; D161, D162)

#### Scenario: Sealed service to a modern client
- **WHEN** a dual-era client sends its first modern request to a sealed service
- **THEN** the gate answers the legacy `400` `-32000` without probing the service, and the client's call succeeds after `initialize`
