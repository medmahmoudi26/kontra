# escrow Specification

## Purpose
Escrow is the `per_task` price shape: a fixed quote a person accepts, held, released to the seller after a review
window, refunded on failure or dispute. The extension `https://kontra.sh/ext/escrow/v1` is implemented only by Kontra
Cloud; the open-source gate contains no money code and names no escrow key, and instead offers hook points
(Admission, quotes and approval, Lifecycle, annotations, the operator's reads) that an operator fills by configuration
without rebuilding the binary. This capability states both: what the gate guarantees at its hook points (OSS), and
what the escrow extension promises on top of them (Cloud). `spec/ext-escrow.md`, `spec/relay.md` §13,
`spec/schema/hooks.json` and the `ext-escrow.*` schemas are normative; PRD §13 draws the OSS/Cloud line. Prices and
metering are in `listings-and-pricing`; served cards and A2A follow-ups in `relay-a2a`; `/mcp` `hire` in `hiring` and
`gate-mcp-tools`.

## Known limitations
- The metric for the age of the oldest pending Lifecycle hook activity, which `spec/relay.md` §13.3 says the gate
  exports, is not built yet (D302).
- Nothing makes Admission atomic with the task's creation; an operator must treat a pre-minted `taskId` that does not
  appear in `kontra.tasks` within 10 minutes as abandoned (`spec/relay.md` §13.1; D45).
- MCP has no in-protocol dispute: a task hired through MCP is disputed on the operator's page (`spec/ext-escrow.md` §5).
- The escrow requirements marked (Cloud) below, and the named tests `escrow_e2e` and `escrow_dispute`, are Kontra
  Cloud's; this repository tests only the hook points (`spec/ext-escrow.md` §8).

## Requirements

### Requirement: No Money Code In The Gate
The open-source gate SHALL contain no money code and SHALL name no escrow key. Quotes, holds, ledgers, receipts,
disputes and payouts are the operator's (Kontra Cloud), which fills the gate's hooks by configuration (a remote
Admission URL and a Lifecycle task queue) on the same binary. (`spec/relay.md` §13 intro, §2.5; PRD §13; D26, D51)

#### Scenario: Built-in hooks only
- **WHEN** a gate runs with no `hooks:` configured
- **THEN** Admission allows every request that passed `allow:`, `ReviewWindow` answers `{open: false}`, and `OnTerminal` is the last hook that fires

### Requirement: Escrow Card Entry
An escrow operator SHALL advertise the `https://kontra.sh/ext/escrow/v1` entry, with `params` per
`spec/schema/ext-escrow.params.json`, through the service's `card_extensions` annotation; the gate SHALL append it to
the served card's `capabilities.extensions` without interpreting it. (Cloud for the entry; `spec/ext-escrow.md` §1;
`spec/relay.md` §13.4)

#### Scenario: Entry appended verbatim
- **WHEN** a service's `card_extensions` annotation holds the escrow entry with `quote: {amount: "15.00", currency: "EUR"}`
- **THEN** the served card's `capabilities.extensions` carries that entry unchanged

#### Scenario: Review window bound
- **WHEN** an escrow entry's `reviewWindowSeconds` is 700000
- **THEN** `spec/schema/ext-escrow.params.json` refuses it (at most 604800)

### Requirement: Required Escrow Entry Enforced
The gate SHALL enforce the escrow entry's `required: true` like any extension's: a `SendMessage` or
`SendStreamingMessage` that does not activate the URI MUST be refused with `ExtensionSupportRequiredError` before any
task exists, while a task's reads, cancel and subscription need no activation. (`spec/ext-escrow.md` §1;
`spec/relay.md` §2.4; D318)

#### Scenario: Message without activation
- **WHEN** a caller sends `SendMessage` without `A2A-Extensions` naming the escrow URI to a service whose card requires it
- **THEN** the gate refuses it with `ExtensionSupportRequiredError` and no task is created

#### Scenario: Read without activation
- **WHEN** the same caller calls `GetTask` on its existing task without activating the URI
- **THEN** the gate answers the task

### Requirement: Activation Acknowledges, Only A Person Approves
Activating the escrow URI SHALL acknowledge the quote shown in the fetched card and MUST NOT by itself approve payment:
a task from an API key or an MCP session is always `quoted` until a person accepts it on the operator's page. A `hire`
of the gate's own `/mcp` SHALL activate the operator's required entry for the buyer and send the service no
`A2A-Extensions`. (`spec/ext-escrow.md` §1; D38, D300; `gate_mcp_hire`)

#### Scenario: API key cannot approve
- **WHEN** an API-key principal activates the escrow URI and sends a message to a priced service
- **THEN** Admission quotes, the task is `quoted`, and nothing is dispatched

### Requirement: Admission Hook Call
The gate SHALL call Admission before any task, MCP session or plain call exists, with an `AdmitRequest` per
`spec/schema/hooks.json` (operation, a pre-minted `taskId` for `task`, service, principal, protocol, extensions,
`contextId`, message metadata and part sizes, tool) that never carries content. `allow:`, per-tool policy, Kontra's
keys and edge limits SHALL be enforced before any hook. (`spec/relay.md` §13 intro, §13.1)

#### Scenario: No content to the hook
- **WHEN** a caller's message holds a 10 KiB text part
- **THEN** the `AdmitRequest` carries that part's `mediaType` and `size`, and not its text

#### Scenario: Denied tool never reaches Admission
- **WHEN** a client calls a tool the service's policy denies
- **THEN** the gate answers `Unknown tool` and does not call Admission

### Requirement: Built-In And Remote Admission
The built-in Admission SHALL allow every request that passed `allow:`. A remote hook (`hooks.admission: {url,
secret_env, timeout, operations}`) SHALL be `POST`ed the `AdmitRequest` with `Authorization: Bearer <secret>`; `url`
MUST be `https://` unless its host is loopback, `timeout` defaults to 5 s and is at most 30 s, and the URL is the
operator's, never derived from a request. (`spec/relay.md` §13.1; D284)

#### Scenario: Remote hook asked
- **WHEN** `hooks.admission.url` is set and a caller starts an A2A task
- **THEN** the hook receives one `POST` with the bearer secret and `operation: "task"`

### Requirement: Admission Fails Closed
A remote Admission that times out, answers non-`2xx` (redirects not followed) or unparsable, answers other than
exactly a valid `decision` of `spec/schema/hooks.json`, breaks the key and size rules, or quotes an `approveUrl` that is
not absolute `https://` without userinfo or an `expiresAt` not a future RFC 3339 time, SHALL be a deny with `-32603`
`admission unavailable` (HTTP `503`). (`spec/relay.md` §13.1; D284; `gate_mcp_hire_quote`)

#### Scenario: Hook down
- **WHEN** the remote Admission URL does not answer within its timeout
- **THEN** the request is refused `-32603` `admission unavailable` (HTTP `503`) and no task exists

#### Scenario: Decision with an unknown member
- **WHEN** the hook answers `{"outcome": "allow", "extra": 1}`
- **THEN** the gate treats it as `admission unavailable`

#### Scenario: Expired quote
- **WHEN** the hook answers `quote` with an `expiresAt` in the past
- **THEN** the gate treats it as `admission unavailable` and creates no task

### Requirement: Admission Deny Codes
A `deny` SHALL refuse with `refusal.message` (`admission denied` when empty) and its `refusal.code`: an A2A code is that
A2A error; an operator code from `-32099` to `-32050` is that code on `/mcp` and `/mcp/{svc}`, and on the A2A routes
`PERMISSION_DENIED` (`-31403`, HTTP `403`) with `{"code": <code>}` in the details. Any other code is `PERMISSION_DENIED`
on A2A and `-32603` on `/mcp`; `/mcp/{svc}` answers every code as it is. (`spec/relay.md` §13.1; D325)

#### Scenario: Operator code on A2A
- **WHEN** the hook denies an A2A `SendMessage` with `refusal.code` `-32060`
- **THEN** the caller gets `PERMISSION_DENIED` (HTTP `403`) whose details carry `{"code": -32060}`

#### Scenario: Operator code on the MCP relay
- **WHEN** the hook denies a task-augmented call on `/mcp/{svc}` with `refusal.code` `-32060`
- **THEN** the client receives JSON-RPC error `-32060` with the refusal's message

### Requirement: Admission Of MCP Sessions And Calls
Only the operations in `hooks.admission.operations` (default `[task]`) SHALL consult the remote hook; others are
answered as the built-in answers them. `session` SHALL be asked for a legacy MCP `initialize` and once per principal
and modern service before its first modern request; `call` for a plain MCP `tools/call`. A passthrough
task-augmented call is asked as `task`. (`spec/relay.md` §13.1, §11.12; D284, D322)

#### Scenario: Call not consulted by default
- **WHEN** `hooks.admission.operations` is `[task]` and a client sends a plain `tools/call`
- **THEN** the remote hook is not called and the call is forwarded

#### Scenario: Session denied
- **WHEN** `operations` lists `session` and the hook denies a client's `initialize`
- **THEN** the client is refused and no `mcp_sessions` row is written

### Requirement: Quotes Only For Gate-Owned Tasks
A `quote` decision SHALL be valid only for operation `task` on a gate-owned task (an A2A task, a `hire`, or an MCP task
of a `durable` or `required` service). For `session`, `call` or a passthrough task-augmented call it MUST be treated as
a deny with `-32603` `priced tasks need tasks: durable`. (`spec/relay.md` §13.1; `internal/hooks/remote.go:233-236`,
`internal/mcprelay/legacy.go:930-931`)

#### Scenario: Quote on a plain call
- **WHEN** the hook answers `quote` for operation `call`
- **THEN** the client receives `-32603` `priced tasks need tasks: durable` and nothing is forwarded

#### Scenario: Quote on a passthrough task
- **WHEN** the hook answers `quote` for a task-augmented call on a `passthrough` service
- **THEN** the client receives `-32603` `priced tasks need tasks: durable` and nothing is forwarded

### Requirement: Admission Never Moves Money
Admission SHALL decide only; money MUST move only where a workflow exists, on approval or in the Lifecycle hooks, so
every paid task starts `quoted`. (`spec/relay.md` §13.1; `spec/ext-escrow.md` §2; D45)

#### Scenario: Paid task starts quoted
- **WHEN** Admission answers `quote` for an A2A task
- **THEN** the task is created `quoted`, nothing is dispatched, and the gate places no hold

### Requirement: Quoted Tasks
A `quote` decision SHALL create the task `quoted` with the decision's `metadata` and `mcpMeta` plus the approve URL
(A2A `kontra.approve.url`, shown as `TASK_STATE_INPUT_REQUIRED`), and dispatch nothing. A caller's message to a
`quoted` task SHALL be refused `UnsupportedOperationError`: approval happens on the operator's page, by a person.
(`spec/relay.md` §13.2, §7; `spec/ext-escrow.md` §2)

#### Scenario: Quoted A2A task
- **WHEN** Admission quotes an A2A task with `approveUrl` `https://app.kontra.sh/#/t/<taskId>`
- **THEN** `GetTask` shows `TASK_STATE_INPUT_REQUIRED` with `metadata["kontra.approve.url"]` equal to that URL

#### Scenario: Message to a quoted task
- **WHEN** a caller sends a message to its `quoted` task
- **THEN** the gate answers `UnsupportedOperationError` and the seller receives nothing

### Requirement: Quote Approval
`TaskLifecycle` SHALL wait on a `quoted` task for the `kontra.approval` Update: `accepted: true` applies its metadata as
one `metadata` event, moves the task to `submitted` (starting `task.timeout`) and dispatches; `accepted: false` ends it
`canceled` with `kontra.canceled = "declined"`; the quote's `expiresAt` ends it `canceled` with `quote_expired`. The
Update's validator MUST refuse once the task has left `quoted`. (`spec/relay.md` §13.2)

#### Scenario: Approval after expiry
- **WHEN** the operator sends `kontra.approval {accepted: true}` after the quote's `expiresAt`
- **THEN** the Update is refused and the task stays `canceled` with `kontra.canceled = "quote_expired"`

#### Scenario: Accepted quote dispatches
- **WHEN** the operator sends `kontra.approval {accepted: true, metadata: {"kontra.escrow.status": "held"}}`
- **THEN** the task moves to `submitted` with that metadata and its dispatch begins

### Requirement: Quoted MCP Tasks
A quoted task of `/mcp/{svc}` SHALL be shown as `working` with the approve URL in `statusMessage` and in
`_meta["sh.kontra/approve-url"]`; a modern call is not waited on. A declined or expired quote SHALL end the task
`canceled`, its `tasks/result` `-32603` `task cancelled` with `data["sh.kontra/canceled"]` `declined` or
`quote_expired`, and its `calls` row `is_error = true`. (`spec/relay.md` §11.5 step 1, §11.8 case 3, §11.12 step 4,
§13.2; D21)

#### Scenario: Quoted durable MCP call
- **WHEN** Admission quotes a task-augmented `tools/call` on a `required` service
- **THEN** the client's `CreateTaskResult` and `tasks/get` show `working` with the approve URL in `statusMessage` and `_meta["sh.kontra/approve-url"]`
- **AND** the upstream receives nothing until the approval

#### Scenario: Declined MCP quote
- **WHEN** the operator sends `kontra.approval {accepted: false}` for that task
- **THEN** `tasks/result` returns `-32603` `task cancelled` with `data["sh.kontra/canceled"] = "declined"`

### Requirement: Escrow On MCP Tasks
An operator that escrows an MCP service SHALL price it `per_task` with `tasks: required`, so every call is a gate task
Admission quotes, and SHALL render the escrow state through `mcpMeta` as `_meta["sh.kontra/escrow"] = {status, quote,
holdId, reviewEndsAt, receipts}`. The gate SHALL render that `mcpMeta` verbatim into `CreateTaskResult`, `tasks/get`
and `tasks/result`, and MUST NOT build it. (Cloud for the state; `spec/ext-escrow.md` §6; `spec/relay.md` §13.3; D51,
D322)

#### Scenario: Escrow state on tasks/get
- **WHEN** a Lifecycle patch sets `mcpMeta {"sh.kontra/escrow": {"status": "held"}}` on an MCP task
- **THEN** the client's next `tasks/get` carries that object at `_meta["sh.kontra/escrow"]`

### Requirement: Operator Metadata Rules
The gate SHALL accept operator-written `metadata` (Admission decisions, `kontra.approval`, `kontra.annotate`,
Lifecycle patches) only under `operator_metadata_prefixes` (Cloud: `["kontra.escrow."]`) or `kontra.keyRetainUntil`,
and `mcpMeta` only under `sh.kontra/`, each map at most 16 KiB. A violating Update SHALL be refused; a violating patch
fails its hook activity non-retryably and is dropped with a metric and an `audit` row. (`spec/relay.md` §13.2, §13.3;
D113)

#### Scenario: Patch outside the prefixes
- **WHEN** `OnTerminal` returns a patch with key `kontra.other`
- **THEN** the patch is dropped and an `audit` row records it

#### Scenario: Oversized annotation
- **WHEN** the operator sends `kontra.annotate` with a 20 KiB `metadata` map
- **THEN** the Update is refused and the task's metadata is unchanged

### Requirement: Caller And Service Kontra Keys
The gate SHALL remove every `kontra.` key from caller metadata except `kontra.sealed`, `kontra.sealed.replyTo` and the
keys listed in `caller_metadata` (Cloud: `kontra.escrow.dispute`), and every `kontra.` key from service output except
the sealed part marker, so that no caller or service can assert escrow state. (`spec/relay.md` §2.5;
`spec/ext-escrow.md` §3; D51)

#### Scenario: Caller forges escrow status
- **WHEN** a caller's message metadata carries `kontra.escrow.status: "released"`
- **THEN** the gate removes the key before logging or forwarding

#### Scenario: Service forges an approve URL
- **WHEN** an A2A upstream's status update carries `metadata["kontra.approve.url"]`
- **THEN** the caller never sees that key

### Requirement: Lifecycle Hooks
`TaskLifecycle` SHALL call `OnDispatched` on an attempt's first upstream task id, `OnTerminal` once after the terminal
event is logged, `ReviewWindow` after it (`seconds` ≤ 604800), `OnReviewMessage` inside each `kontra.review_message`
Update while the window is open, and `OnReviewClosed` when it ends or after a `dispute`, each with a `TaskRef` built
from Postgres right before the call (operator keys only in its `metadata`). Built in, they are no-ops.
(`spec/relay.md` §13.3; D114)

#### Scenario: Terminal hook once
- **WHEN** a task completes
- **THEN** `OnTerminal` is called exactly once, after the terminal event is in the log

### Requirement: Remote Lifecycle Hooks
With `hooks.lifecycle.task_queue` set, the gate SHALL schedule the five activities by type name on that queue with
unlimited retry (1 s doubling to 5 min), so a hook that is down delays settlement and never skips it, and SHALL
snapshot the hook configuration in force when a task started into its workflow input. (`spec/relay.md` §13.3; D26)

#### Scenario: Hook down delays, never skips
- **WHEN** the remote `OnTerminal` worker is down for an hour
- **THEN** the activity is retried until it returns, and the task's settlement runs then

### Requirement: Cancel Never Races Money
The gate MUST write `canceled` before calling `OnTerminal`, and MUST hold patches and annotations that arrive while a
dispatch, `abort` or `forward_cancel` runs, applying them when it returns. `kontra.annotate` MAY be sent at any time
while the workflow runs, and `kontra.close` ends a `holdOpenSeconds` hold. (`spec/relay.md` §13.3; D78)

#### Scenario: Terminal before the hook
- **WHEN** a quoted-and-held task is cancelled
- **THEN** `canceled` is in the task's log before `OnTerminal` is called

### Requirement: Escrow Dispute
An escrow buyer SHALL dispute an A2A task with a message inside the review window carrying
`metadata["kontra.escrow.dispute"] = {"reason": <at most 2000 chars>}`; the operator's hook answers `dispute: true`
and a patch setting `disputed`, and the release timer stops. A dispute on a task without escrow is refused like any
message to a terminal task. (Cloud; `spec/ext-escrow.md` §5; `spec/schema/ext-escrow.dispute.json`; `escrow_dispute`)

#### Scenario: Dispute inside the window
- **WHEN** a buyer sends a dispute message to its completed escrow task inside the review window
- **THEN** `kontra.escrow.status` becomes `disputed`, the message is in the task's history, and the seller never receives it

#### Scenario: Dispute after release
- **WHEN** the buyer sends a dispute message after the window closed
- **THEN** it is refused `UnsupportedOperationError` and nothing is logged

### Requirement: Escrow Task Metadata
An escrow operator SHALL keep a task's escrow state in `kontra.escrow.status` (`held`, `released`, `refunded`,
`disputed`, `resolved`), `kontra.escrow.quote` (fixed at admission), `holdId`, `reviewEndsAt` and append-only
`receipts` of `{event, rail, ref, at}`, whose `rail` is `stripe`, `ledger` or an operator's own; on completion it SHALL
set `kontra.keyRetainUntil` to `reviewEndsAt`. (Cloud; `spec/ext-escrow.md` §3;
`spec/schema/ext-escrow.metadata.json`; D320)

#### Scenario: Later price change
- **WHEN** a service's price changes while a task admitted at the old quote is running
- **THEN** the task's `kontra.escrow.quote` still holds the old quote

#### Scenario: Key retained for review
- **WHEN** an escrow task completes with a 1-day review window
- **THEN** its `kontra.keyRetainUntil` equals its `kontra.escrow.reviewEndsAt`

### Requirement: Escrow Lifecycle
An escrow operator SHALL place the hold before `kontra.approval` and refund it if the Update is refused; refund a
`failed`, `rejected` or `canceled` task only after its terminal event is logged; keep a `completed` task `held` until
`reviewEndsAt` (completion + `reviewWindowSeconds`), then `released` unless disputed; and decide a dispute `refunded`,
`released` or `resolved`, each with a receipt. (Cloud; `spec/ext-escrow.md` §2, §4; `escrow_e2e`)

#### Scenario: Happy path
- **WHEN** a person accepts a quote, the task completes and the review window passes without a dispute
- **THEN** the task's receipts hold `held` then `released`, and its status is `released`

#### Scenario: Failed task refunded
- **WHEN** a held task ends `failed`
- **THEN** its status becomes `refunded` with a `refunded` receipt

### Requirement: Operator Reads
The operator SHALL read the gate's tables in schema `kontra` (`tasks`, `task_events` without sealed content, `calls`,
`mcp_sessions`, `tunnels`) and MUST NOT read `push_configs.config` or `work_items.payload`. No protocol surface lists
tasks across buyers; a service owner's view of its tasks and calls is the operator's read, not a gate route.
(`spec/relay.md` §13.5; D37, D39, D319)

#### Scenario: Cross-buyer listing
- **WHEN** a buyer calls A2A `ListTasks` or MCP `tasks/list`
- **THEN** it sees only tasks of its own `buyer_id`

### Requirement: Operator Writes
Operator writes to `services`, `enrollment_tokens` and `api_keys` SHALL be followed by
`NOTIFY kontra_services, '<tenant>/<name>'`, which every replica applies without restart, and by an `audit` row
appended in the same transaction with `kontra.audit_append`. (`spec/relay.md` §13.5; D116, D317)

#### Scenario: Service change applied live
- **WHEN** the control plane updates a `services` row and sends the `NOTIFY`
- **THEN** every replica applies the change without restart

### Requirement: AP2 Evidence
The escrow quote object SHALL be shaped to embed in an AP2 `CartMandate`, and a `PaymentMandate` a client provides in
message metadata (`ap2.PaymentMandate`) SHALL stay in that message, in the task's history, as evidence; neither is
required in v4. (`spec/ext-escrow.md` §7)

#### Scenario: Mandate kept as evidence
- **WHEN** a caller's first message carries `ap2.PaymentMandate` in its metadata
- **THEN** the task's history holds that message with the mandate, and the gate neither requires nor interprets it
