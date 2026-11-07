# gate-mcp-tools Specification

## Purpose
The gate's own first-party MCP server at `{public_url}/mcp`: its authentication, its tools (`search`, `describe`,
`hire`, `reply`, `status`, `tasks`, `cancel`, `artifact`, `push_set`, `push_list`, `push_delete`), the MCP Tasks it
serves for `hire` tasks, quotes and approval, questions, and both MCP eras (2025-11-25/2025-06-18 and 2026-07-28).
A `hire` task is an ordinary A2A gate task (`relay-a2a`) followed over MCP; `/mcp/{svc}` is `relay-mcp`, quotes and
holds are `escrow`, listings and their annotations are `listings-and-pricing`. `spec/relay.md` §12 and
`spec/schema/mcp-tools.json` are normative.

## Known limitations
- `hire` and `reply` cannot activate an optional extension (A2UI included) nor send message metadata, and the hire result keeps a data part's value without its `mediaType` or `metadata`, so A2UI a seller sends reaches the client as unmarked JSON (`spec/relay.md` §12.2; D328).
- No tool relays a `tools/call` to an MCP service through `/mcp`; MCP services are called at `/mcp/{svc}` (`spec/relay.md` §12.6; D339).
- `GET` streams and `tasks/result` response streams of `/mcp` carry no event ids and are not resumed: a client that missed `notifications/elicitation/complete` retries `hire`, and one whose stream was cut asks `tasks/result` again (`spec/relay.md` §12.3; D288, D303).

## Requirements

### Requirement: Gate MCP Endpoint Sessions
The gate SHALL serve `/mcp` as an MCP server of both eras with no upstream, under `spec/relay.md` §11.1's transport rules and §11.12's era classification. A legacy session SHALL be an `mcp_sessions` row with `service` NULL that any replica serves, closed when idle but never while one of its streams lasts. A request naming another `MCP-Protocol-Version` than the session negotiated MUST be `400` with `-32022` (`spec/relay.md` §12; D24, D269, D293, D303).

#### Scenario: Version changes mid-session
- **WHEN** a legacy session negotiated `2025-11-25` and a later request names `MCP-Protocol-Version: 2025-06-18`
- **THEN** the gate answers `400` with `-32022`

#### Scenario: Any replica
- **WHEN** a legacy session's requests arrive on different replicas
- **THEN** each replica serves them without a hop

### Requirement: Gate MCP Requires A Hiring Principal
`/mcp` MUST NOT have an anonymous principal: a request without a credential is `401`; through a node, the session's principal applies. A principal that may not call services (an API key with only `enroll:` scopes) MUST be `403` on every request, before any session (`spec/relay.md` §12; D270, D293; test `gate_mcp_hire`).

#### Scenario: Enroll-only key
- **WHEN** an API key whose only scope is `enroll:*` sends `initialize` to `/mcp`
- **THEN** the gate answers `403`

#### Scenario: No credential
- **WHEN** a request without a credential reaches `/mcp`
- **THEN** the gate answers `401`

### Requirement: MCP OAuth Challenge
With `auth.mcp_oauth.issuer` configured, the gate SHALL be an OAuth 2.1 resource server for `/mcp` and every `/mcp/{svc}`: a request without a credential gets `401` with `WWW-Authenticate: Bearer resource_metadata=…, scope="mcp"` naming the route's RFC 9728 document, served for every well-formed name, whose `resource` is the route's canonical URI. The gate MUST refuse to start on a `public_url` with a path (`spec/relay.md` §12.1; D25, D296; test `gate_mcp_oauth`).

#### Scenario: Unknown service without credential
- **WHEN** a request without a credential reaches `/mcp/nosuch`
- **THEN** the gate answers the same `401` challenge as for an existing service

#### Scenario: Anonymous MCP service
- **WHEN** a request without a credential reaches an `/mcp/{svc}` whose `allow:` contains `anonymous`
- **THEN** the service serves it

### Requirement: MCP OAuth Token Verification
A JWT on an MCP route MUST be accepted only when its `aud` contains the route's canonical URI, `iss` equals the issuer, `exp` and `nbf` hold within 60 s, and `alg` is ES256, RS256 or EdDSA. A refused credential SHALL be `401` with `error="invalid_token"`; an unreadable issuer, stale revocation feed or unreadable tenant `503` with `-32603`. The principal is `oidc:sub:<sub>` with `via = mcp` (`spec/relay.md` §12.1; D276, D301, D315, D316; test `gate_mcp_oauth`).

#### Scenario: Token for /mcp used on a service route
- **WHEN** a token whose `aud` is `{public_url}/mcp` is presented on `/mcp/hello`
- **THEN** the gate answers `401` with `error="invalid_token"`

#### Scenario: Rotated signing key
- **WHEN** a token carries a `kid` not in the cached key set
- **THEN** the gate refetches the set, at most once a minute

#### Scenario: HMAC token
- **WHEN** a token is signed with `HS256` or `none`
- **THEN** the gate answers `401`

### Requirement: Tool Declarations
Every tool SHALL declare `annotations.readOnlyHint` and `annotations.destructiveHint` and the JSON Schema 2020-12 input schema published in `spec/schema/mcp-tools.json`. `hire`, `reply` and `cancel` SHALL declare `hireResult` as `outputSchema`, and `status`, `tasks`, `artifact` and the push tools their own strict result schemas (`spec/relay.md` §12.2; D324, D339; tests `gate_mcp_stock_client`, `gate_mcp_tools`).

#### Scenario: Stock client validation
- **WHEN** the stock MCP TypeScript SDK validates a `hire` result's `structuredContent` against the tool's `outputSchema`
- **THEN** every result the gate produces validates

#### Scenario: Legacy task support
- **WHEN** a 2025-11-25 session lists tools
- **THEN** `hire` declares `execution.taskSupport: "optional"`, and a 2025-06-18 session's list has no `execution`

### Requirement: Search Visibility
`search` SHALL return only services whose `allow:` admits the principal by an entry other than `anonymous`, plus services the operator marks `listed`, and MUST never return a service of an anonymous tenant or one with `annotations.search.hidden: true`, for any principal; a row the registry cannot build is left out and logged (`spec/relay.md` §12.2; D271, D301, D303, D321; test `gate_mcp_search`).

#### Scenario: Only-anonymous service
- **WHEN** a service's only admitting entry for the principal is `anonymous`
- **THEN** `search` does not return it

#### Scenario: Hidden service
- **WHEN** a service has `annotations.search.hidden: true`
- **THEN** no principal's `search` returns it, and `describe` and `hire` still reach it by ref when `allow:` admits the principal

### Requirement: Search Filtering And Paging
`search` SHALL match `query` as a case-insensitive substring of the ref, name or description, filter `kind` and `price` by equality (`price` on the `price_model` annotation, `free` meaning none), order by ref, and answer 50 a page with an opaque keyset `nextCursor`. One request MUST read at most 1000 services, answering what it found with `nextCursor` at the last one read (`spec/relay.md` §12.2; D271, D293; test `gate_mcp_search`).

#### Scenario: Free filter
- **WHEN** a principal calls `search {price: "free"}`
- **THEN** only visible services without a `price_model` annotation are returned

### Requirement: Search Lists MCP Services
`search` SHALL return MCP services (`kind: "mcp"`) beside A2A agents under the same visibility rules, accept `kind: "mcp"` as a filter, and describe an MCP service by the operator's `search.description` annotation or else the upstream's cached `serverInfo.title` or `name`, with its `price` annotation verbatim (`spec/relay.md` §12.2; `spec/schema/mcp-tools.json#/$defs/search`; D271).

#### Scenario: Filter by MCP kind
- **WHEN** a principal calls `search {kind: "mcp"}`
- **THEN** the result holds only the MCP services visible to it, each with `kind: "mcp"`

### Requirement: Describe A Service
`describe` SHALL answer only for a service `search` would return to the principal were it not hidden, else the unknown-service result. It answers the served card (A2A) or the cached, filtered, annotated `tools/list` (MCP), `sealed`, `available` and the URLs to call it directly, and MUST NOT make an upstream request per call; a cold MCP cache answers `toolsCached: false` and starts at most one drain per service per 10 minutes (`spec/relay.md` §12.2; D271; test `gate_mcp_search`).

#### Scenario: Cold tools cache
- **WHEN** a principal describes an MCP service whose `tools/list` is not cached
- **THEN** the answer has `toolsCached: false` and no `tools`, and one drain starts

### Requirement: Describe Capabilities
`describe` SHALL add normalized `capabilities`: for an A2A service, from its served card, `streaming`, `pushNotifications`, input and output modes, skills (`id`, `name`, `description`, `tags`, `examples`, modes) and extensions with `required`; for an MCP service, the tool count only when the list is cached, and `toolsCached`. The full card stays beside it (`spec/relay.md` §12.6; D339; test `gate_mcp_tools`).

#### Scenario: Seller skills
- **WHEN** a principal describes an A2A service whose card has a skill with examples
- **THEN** `capabilities.skills` lists it with its `examples`

### Requirement: Describe Input Schema Capability
For an A2A service whose served card carries an `https://kontra.sh/ext/input-schema/v1` entry with an object `params.schema`, `describe` SHALL include the first such schema as `capabilities.inputSchema`, and the `describe` tool's description SHALL say it shows the input schema when the service has one (`spec/relay.md` §12.6; `spec/ext-input-schema.md`; D339).

#### Scenario: Seller with an input schema
- **WHEN** a principal describes an A2A service whose card declares an input schema
- **THEN** `capabilities.inputSchema` equals the entry's `params.schema`

### Requirement: Hire Accepts A2A Services Only
`hire` SHALL create an ordinary gate task (`protocol = a2a`, marked `tasks.hired`) only for `kind: a2a` services. A ref of an MCP service, of a service whose sealing is required, or of one whose served card requires an extension of the seller's own (or the sealed extension) MUST be answered with a `CallToolResult` `isError` naming the route to use, and no task (`spec/relay.md` §12.2; D272, D294; test `gate_mcp_hire`).

#### Scenario: MCP ref
- **WHEN** a principal calls `hire` with the ref of an MCP service
- **THEN** the result is `isError` naming `/mcp/<ref>`, and no task exists

#### Scenario: Seller-required extension
- **WHEN** a service's served card requires an extension of the seller's own
- **THEN** the result is `isError` naming the A2A URL and the URIs to activate, and no task exists

### Requirement: Hire Activates Operator Extensions
A required extension of the service's `card_extensions` annotation SHALL be activated by `hire` on the buyer's behalf and given to Admission in `extensions`, and MUST NOT be sent to the service. `hire` and `reply` MUST NOT activate an optional extension (`spec/relay.md` §12.2; D300, D328; test `gate_mcp_hire`).

#### Scenario: Operator-required extension
- **WHEN** a service's only required extension is the operator's `card_extensions` entry
- **THEN** the hire creates a task and the seller receives no `A2A-Extensions`

### Requirement: Hire Message Arguments
`hire`'s and `reply`'s `message` SHALL be a non-empty string (one text part) or `{parts: [Part, …]}`, each part with exactly one of `text`, `raw`, `url`, `data` and optional `filename`, `mediaType`, `metadata`, read with exact keys; a part keeps its own `mediaType` and `metadata`. Arguments that break the input schema, or a `data` that is `null`, MUST be a `CallToolResult` `isError` and create no task (`spec/relay.md` §12.2; D272, D293, D328).

#### Scenario: Null data part
- **WHEN** `hire` is called with `message: {parts: [{data: null}]}`
- **THEN** the result is `isError` and no task is created

#### Scenario: A2UI action reaches the seller marked
- **WHEN** `reply` sends a part marked `mediaType: "application/a2ui+json"`
- **THEN** the seller receives the part with that `mediaType`

### Requirement: Hire Unknown Service Result
A ref that is malformed, unknown, disabled, not admitted by the principal's `allow:`, or not served by this replica's registry MUST be one `CallToolResult` with `isError` and the text `unknown service`, never a `404`, which would end the client's session (`spec/relay.md` §12.2; D293, D303).

#### Scenario: Not-admitted ref
- **WHEN** `hire` names a service whose `allow:` does not admit the principal
- **THEN** the result is `isError` with the text `unknown service`, as for a service that does not exist

### Requirement: Hire Input Schema Data Part First Message
For a service whose `describe` shows an `inputSchema`, the documented first message of a `hire` SHALL be `{parts: [{data: {…}}]}`, one data part holding an object of that schema, which the gate MUST relay without validating it against the schema (`spec/relay.md` §12.6; `spec/ext-input-schema.md` §2; `internal/gatemcp/tools.go:117-121`; test `a2ui_passthrough`).

#### Scenario: Booking through an input schema
- **WHEN** a principal hires the A2UI example seller with `{parts: [{data: <booking>}]}` matching its input schema
- **THEN** the seller receives the data part and books from it

### Requirement: Plain Calls Wait
A plain `hire` (no `params.task`, or a modern request without the tasks extension) and every `reply` SHALL wait up to 25 s for the task to end, reach `input-required` or be `quoted` (a quoted task answered at once); a task still running is answered with text naming the `status` call `{taskId, after: 0, wait: 25}`. The wait MUST be cut so the answer is written within 50 s of the request's arrival (`spec/relay.md` §12.2; D272, D295; test `gate_mcp_hire`).

#### Scenario: Slow task
- **WHEN** a plain `hire`'s task is still working after 25 s
- **THEN** the answer is the hire result with the running state and the text naming `status`

### Requirement: Hire Result
A plain `hire`, every `reply`, `cancel`, `status` and `tasks/result` of a `hire` task SHALL carry one `structuredContent` valid against `hireResult`: `taskId` and A2A `state` always, `contextId`, `service`, `url` (the approve URL byte for byte whenever the task has one; the gate never builds it), `quote` while `quoted`, `text`, and `data`/`files` when present, with matching content blocks (`spec/relay.md` §12.2; D188, D289, D324; test `gate_mcp_stock_client`).

#### Scenario: Quoted hire from structuredContent alone
- **WHEN** a plain `hire` is quoted
- **THEN** `structuredContent.url` and `structuredContent.quote.approveUrl` hold the approve URL

### Requirement: Hire Outcome Rendering
An ended `hire` task SHALL answer a `CallToolResult`, never a JSON-RPC error; one that ended `failed`, `rejected` or `canceled` MUST have `isError: true`, its final status message as text, and `sh.kontra/failure` or `sh.kontra/canceled` in `_meta` (`rejected` for a rejected task). The tools and MCP Tasks of `/mcp` MUST NOT write `calls` rows (`spec/relay.md` §12.2; D289, D293; test `gate_mcp_hire`).

#### Scenario: Failed upstream
- **WHEN** a `hire` task's upstream fails
- **THEN** its result is `isError: true` with `_meta["sh.kontra/failure"]` and the status message as text

#### Scenario: No metering row
- **WHEN** a principal completes a `hire`
- **THEN** no `calls` row is written for it

### Requirement: Hire Quote And Approval
`hire` SHALL run Admission with operation `task` and `via`. On a quote the task is created `quoted` and nothing is dispatched: a legacy client that declared `elicitation.url` gets `-32042` with one URL elicitation of the approve URL; any other client, and every modern request, gets the URL at once. Nothing a client sends MUST approve a quote: only `kontra.approval` from the operator's control plane does (`spec/relay.md` §12.3, §13.2; D21, D38, D45, D188, D275; test `gate_mcp_hire_quote`).

#### Scenario: Client with URL elicitation
- **WHEN** a quoted `hire` comes from a session that declared `elicitation.url`
- **THEN** the answer is `-32042` whose `url` is byte-equal to Admission's `approveUrl`

#### Scenario: Reply cannot approve
- **WHEN** a client calls `reply` on a `quoted` task
- **THEN** the result is `isError`, nothing is forwarded, and the task stays `quoted`

#### Scenario: Admission down
- **WHEN** the Admission server is unreachable
- **THEN** `hire` answers `-32603` "admission unavailable" and no task exists

### Requirement: Elicitation Complete Notification
When a task whose `hire` got `-32042` leaves `quoted`, the gate SHALL send `notifications/elicitation/complete {elicitationId}` once, on exactly one `GET` stream of the session that received the error, claimed by a conditional update so that no other stream or session MUST receive it; a stream opened within 10 minutes after gets it only if none claimed it (`spec/relay.md` §12.3; D273, D288; test `gate_mcp_hire_quote`).

#### Scenario: Approval notifies one stream
- **WHEN** the quote is approved while two `GET` streams of the session are open
- **THEN** exactly one stream receives `notifications/elicitation/complete`, once, and other sessions receive none

### Requirement: Hire Retries
A `hire` matching an existing task on `(tenant, service, buyer_id, principal, first_request)` while it is `quoted`, or within 10 minutes after it left `quoted`, SHALL answer that task in its current state, with the same `elicitationId` when still quoted, and MUST re-issue its start first while it is `quoted`. A declined or expired quote is answered `isError` (`spec/relay.md` §12.3; D290; test `gate_mcp_hire_quote`).

#### Scenario: Retry after approval
- **WHEN** the same `hire` is retried after its quote was accepted
- **THEN** it answers the task's result, not a new task

### Requirement: Hire Idempotency Key
A `hire` with `idempotencyKey` (1–128 of `[A-Za-z0-9._:-]`) SHALL answer the task that the same principal and key created within 24 h, whatever its state, and MUST never create a second task, even for two such `hire`s arriving together. The same key with other arguments is `isError` "idempotencyKey already used for a different hire" and creates nothing (`spec/relay.md` §12.3; D338; test `gate_mcp_tools`).

#### Scenario: Keyed hire twice
- **WHEN** a principal calls `hire` twice with the same `idempotencyKey` and arguments
- **THEN** both answer the same task

#### Scenario: Key reused with another message
- **WHEN** the same key is sent with a different `message`
- **THEN** the result is `isError` and no task is created

### Requirement: Hire Task Asking A Question
For a text-only question and a reader that declared form elicitation, the gate SHALL show `input_required` with one form `elicitation/create` (`reply: string`), on `tasks/result` (legacy) or in `inputRequests` keyed `k.<taskId>.<seq>` (modern); `accept` becomes the follow-up once, `decline` or `cancel` leaves the task waiting. Every other case MUST show `working` with the reply-tool hint and `sh.kontra/input-required` (`spec/relay.md` §12.4; D58, D274; test `gate_mcp_reply`).

#### Scenario: Client without form elicitation
- **WHEN** a `hire` task asks a question and the reader declared no form elicitation
- **THEN** `tasks/get` shows `working` with the reply-tool hint and `sh.kontra/input-required`

#### Scenario: Declined elicitation
- **WHEN** a legacy client answers the form elicitation with `decline`
- **THEN** the task stays at `input-required`

### Requirement: Reply And Continuation
`reply` SHALL send a follow-up to a task at `input-required` and answer its new state as a plain `hire` does; to a task that is not asking it MUST send nothing and be `isError`, a running task's text naming `status` and an ended task's naming `hire` with its `contextId`. A multi-turn conversation SHALL be `hire` with the earlier result's `contextId` (`spec/relay.md` §12.2, §12.6; D336; tests `gate_mcp_reply`, `gate_mcp_tools`).

#### Scenario: Reply to an ended task
- **WHEN** a client calls `reply` on a completed task
- **THEN** the result is `isError` naming `hire` with the task's `contextId`, and a `hire` with that `contextId` creates a new task in the same context

### Requirement: Hire Tasks Over MCP Tasks
`tasks/get`, `tasks/result`, `tasks/list` and `tasks/cancel` on `/mcp` SHALL see only `hire` tasks of the principal's `buyer_id` whose service this replica serves to the principal; any other task id MUST be `-32602`. `tasks/list` lists across services newest first, and `tasks/cancel` ends the task `canceled` within 10 s, the upstream receiving `CancelTask` (`spec/relay.md` §12.2, §11.6; D57, D293, D303; test `gate_mcp_hire`).

#### Scenario: Another buyer's task
- **WHEN** buyer B sends `tasks/get` for buyer A's `hire` task
- **THEN** the gate answers `-32602`

#### Scenario: Cancel through MCP Tasks
- **WHEN** a client sends `tasks/cancel` for a running `hire` task
- **THEN** the task is `cancelled` within 10 s and the upstream receives `CancelTask`

#### Scenario: Same task over A2A
- **WHEN** the same task is followed with A2A `SubscribeToTask`
- **THEN** it shows the same events in the same order

### Requirement: Modern Discovery And Methods
The gate SHALL answer a 2026-07-28 `server/discover` itself, with `supportedVersions` `["2026-07-28", "2025-11-25", "2025-06-18"]`, `tools` and the tasks extension, `ttlMs` 600000 and `cacheScope: "private"`, and every modern result SHALL carry `resultType`. On a modern request, `tasks/result`, `tasks/list`, `ping` and any method `/mcp` does not serve MUST be `404` with `-32601` (`spec/relay.md` §12.5; D161, D176, D269, D295; test `gate_mcp_modern`).

#### Scenario: Modern tasks/result
- **WHEN** a 2026-07-28 client sends `tasks/result`
- **THEN** the gate answers `404` with `-32601`

### Requirement: Modern Hire
A modern `hire` declaring the tasks extension SHALL be answered in line when its task ends within 2 s, else with `CreateTaskResult` (a quoted task `working` with the approve URL), and never `-32042`. A client that closes its stream before its `hire` is answered MUST have a non-quoted task it created sent `kontra.cancel` in MCP mode, unless a retry found the task or the `hire` carried an `idempotencyKey` (`spec/relay.md` §12.5; D21, D57, D178, D295, D338; test `gate_mcp_modern`).

#### Scenario: Client leaves during the wait
- **WHEN** a modern client closes its stream before its `hire` is answered
- **THEN** it receives no answer and the task ends `canceled` within 10 s

#### Scenario: Quoted task survives the client leaving
- **WHEN** a modern client closes its stream before a quoted `hire` is answered
- **THEN** the task stays `quoted` and the re-issued `hire` answers it

### Requirement: Modern Task Reads And Answers
For a modern reader, `tasks/get` of a `hire` task SHALL show `quoted`, `submitted` and `working` as `working`, a question per the question rules, and an ended task with its rendered `result` (`failed` or `rejected` as `completed` with `isError`). `tasks/update` SHALL act only on the outstanding gate key with an `accept` whose `content.reply` is a string; every other answer gets the empty acknowledgement and MUST forward nothing (`spec/relay.md` §12.5; D274, D295; test `gate_mcp_modern`).

#### Scenario: Update on a quoted task
- **WHEN** a client sends `tasks/update` with `accept` for a `quoted` task
- **THEN** the result is the empty acknowledgement, nothing reaches the seller and the task stays `quoted`

#### Scenario: Answer once
- **WHEN** a client answers the same gate key twice
- **THEN** only the first answer becomes a follow-up

### Requirement: Status Tool
`status {taskId}` SHALL answer the hire result as a plain `hire` does. With `after` it SHALL also answer the `task`, `status`, `artifact` and `message` rows with seq > `after` (at most 50 rows or about 256 KiB, `more` when cut) and `cursor`, an `after` beyond the tail read as the tail. With `wait` (0–25 s), a task that has neither ended nor asked SHALL first wait for a new row, under the 50 s rule (`spec/relay.md` §12.6; D295, D332; test `gate_mcp_tools`).

#### Scenario: Resubscribe loop
- **WHEN** a client loops `status {taskId, after: <cursor>, wait: 25}` on a slow task
- **THEN** each answer carries the events after its cursor, none skipped or repeated, until the task ends

#### Scenario: Whole history
- **WHEN** a client calls `status {taskId, after: 0}`
- **THEN** the answer carries the task's events from the first

### Requirement: Follow Tools Scoping
`status`, `tasks`, `cancel`, `artifact` and the push tools SHALL serve only `hire` tasks of the principal's `buyer_id` whose service this replica serves to the principal. Another buyer's task, an unhired one, an unknown id and one whose service is no longer served MUST be one answer, a `CallToolResult` `isError` with the text `unknown task`; none writes a `calls` row (`spec/relay.md` §12.6; D303, D332–D337).

#### Scenario: Unhired task id
- **WHEN** a principal calls `status` with the id of a task created over A2A
- **THEN** the result is `isError` `unknown task`

### Requirement: Tasks Tool
`tasks` SHALL list the principal's `hire` tasks newest first by `status_at`, 50 a page with an opaque keyset cursor and at most 1000 rows read per request, filtered by `service`, `contextId` and `state` (an A2A state, `active` or `ended`), each entry `{taskId, service, contextId, state, statusAt, text}` with `text` cut to 200 characters (`spec/relay.md` §12.6; D333; test `gate_mcp_tools`).

#### Scenario: List by context
- **WHEN** a client calls `tasks {contextId: c}`
- **THEN** only its `hire` tasks in context `c` are listed, newest first

### Requirement: Cancel Tool
`cancel` SHALL have `tasks/cancel`'s legacy semantics (`kontra.cancel` in MCP mode, the task `canceled` within 10 s), awaited, and answer the canceled task's hire result without `isError`. A task that has ended MUST be `isError` "the task already ended (<state>)" with nothing sent; a lifecycle that does not accept the cancel in time is `503` with `-32603` (`spec/relay.md` §12.6; D57, D334; test `gate_mcp_tools`).

#### Scenario: Cancel an ended task
- **WHEN** a client calls `cancel` on a completed task
- **THEN** the result is `isError` "the task already ended (TASK_STATE_COMPLETED)" and nothing is sent upstream

### Requirement: Artifact Tool
`artifact` SHALL answer one artifact of the task's snapshot in full as `{taskId, artifactId, name?, description?, text?, data?, files?}`, text parts as text, data parts as JSON and in `data`, raw parts as embedded resources and URL parts (blob parts presigned now) as resource links, each bounded as parts are. An id the task does not hold MUST be `isError` "unknown artifact" (`spec/relay.md` §12.6; D335; test `gate_mcp_tools`).

#### Scenario: Unknown artifact id
- **WHEN** a client asks `artifact` for an id the task does not hold
- **THEN** the result is `isError` "unknown artifact"

### Requirement: Push Tools
`push_set`, `push_list` and `push_delete` SHALL use the A2A push config store and rules for the task (limits, idempotent save, the SSRF check at set time, a config on an ended task receiving nothing) and answer `{taskId, configs: [{configId, url, token, scheme?}]}` with `token` a boolean; the token and credentials MUST never be shown, and delivery stays the lifecycle's (`spec/relay.md` §12.6; D111, D337; test `gate_mcp_tools`).

#### Scenario: Loopback webhook
- **WHEN** a client calls `push_set` with `http://127.0.0.1/hook`
- **THEN** the result is `isError` with the refusal and nothing is stored

#### Scenario: Token hidden
- **WHEN** a client lists a config set with a token
- **THEN** the entry shows `token: true` and not the token

### Requirement: Progress Notifications
A `tools/call` of `hire`, `reply` or `status` with `wait` that carries a string or integer `params._meta.progressToken` and accepts `text/event-stream` SHALL be answered on an SSE stream: one `notifications/progress` per `task`, `status` and `artifact` row of the call's span (`progress` increasing from 1, a repeated message not sent again), then the result, without event ids. Without a progress token nothing changes (`spec/relay.md` §12.6; D339; test `gate_mcp_tools`).

#### Scenario: Hire with a progress token
- **WHEN** a client calls `hire` with a progress token on a task that streams three statuses
- **THEN** it receives a progress notification for each new row before the result
