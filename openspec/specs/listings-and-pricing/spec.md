# listings-and-pricing Specification

## Purpose
A listing says how one of a publisher's services is sold: its kind, visibility, MCP policy, sealing and price. It
is a file that `kontra publish` checks and sends to the operator's control plane (Kontra Cloud), which decides what it
accepts and writes the gate's `services` row and annotations; the gate itself holds no price. This capability covers
the listing file, the price models, the price metadata the operator has the gate show on MCP, and the metering of
MCP calls. `spec/schema/listing.json`, `spec/schema/price.json`, `spec/tunnel.md` §6.4, `spec/ext-escrow.md` §6 and
`spec/relay.md` §11.4, §13.4 and §13.5 are normative. The sign-in a publish uses belongs to
`node-enrollment-and-keys`; escrow of `per_task` prices to `escrow`; `/mcp` `search` to `gate-mcp-tools`.

## Known limitations
- The listing schema prices in USD only (`currency` const `USD`, metered amounts at most 6 decimals), while
  `spec/schema/price.json` and the examples of `spec/ext-escrow.md` §6 allow any ISO 4217 code (`EUR` in the
  examples) and 8 decimals (`spec/schema/listing.json:69`, `:83`; `spec/schema/price.json:9-10`).
- A listing carries no principal `allow:` list: who may call a service is the gate's `services.allow`, which the
  control plane writes, not a listing field (`spec/schema/listing.json`; `migrations/0001_init.sql:39`).
- That a `per_session` price's `idleTimeoutSeconds` equals the service's `session_idle_s` is checked neither by the
  CLI nor by the gate; it is the control plane's to enforce (`spec/ext-escrow.md` §6; `internal/publish/listing.go`).

## Requirements

### Requirement: Listing File Validation
`kontra publish` SHALL read a listing file of at most 64 KiB as one YAML 1.2 or JSON document (JSON first when it
starts with `{`) and SHALL refuse a repeated key, an unknown member, a YAML integer with leading zeros, underscores or
a base prefix, and anything `spec/schema/listing.json` refuses, naming where. It MUST send nothing for a file it
refuses. (`spec/tunnel.md` §6.4; D323)

#### Scenario: Octal-looking integer refused
- **WHEN** a YAML listing writes `taskTimeoutSeconds: 0600`
- **THEN** `kontra publish` refuses the file naming the line, and sends nothing

#### Scenario: Unknown member refused
- **WHEN** a listing has a top-level member `owner`
- **THEN** `kontra publish` refuses it, naming `owner`

### Requirement: Listing Schema Fixtures
The CLI's checks and `spec/schema/listing.json` SHALL agree on every fixture: `examples/listing.*.json` accepted and
`examples/invalid/listing.*.json` refused, except the 64-glob sum of `tools.allow` and `tools.deny`, which the schema
cannot state and the CLI MUST refuse. (`spec/tunnel.md` §6.4; D323; `TestSchemaFixtures`, `check_specs.py`)

#### Scenario: Too many globs
- **WHEN** a listing's `tools.allow` and `tools.deny` hold 40 globs each
- **THEN** the schema accepts it and `kontra publish` refuses it for that rule alone

### Requirement: A2A Listings
A listing SHALL default to `kind: a2a`, and an A2A listing MUST NOT carry `tools` or `tasks`, which are an MCP
service's. (`spec/schema/listing.json` `kind`, `allOf[1]`; D323)

#### Scenario: Tools on an A2A listing
- **WHEN** a listing without `kind` carries `tools: {deny: ["x"]}`
- **THEN** the listing is refused (`examples/invalid/listing.tools_on_a2a.json`)

#### Scenario: Free A2A listing
- **WHEN** a listing names only `service` and `price: {model: free}`
- **THEN** it is valid, with `kind: a2a`, `visibility: public`, `sealed: off` and `published: true`

### Requirement: MCP Listings
A listing MAY set `kind: mcp`, and then MAY carry the MCP service's `tools: {allow, deny}` policy (each list at most
64 tool globs) and its `tasks` mode (`durable`, `required`, `passthrough` or `off`). A priced MCP listing MUST NOT be
`passthrough`, because the gate does not observe those calls. (`spec/schema/listing.json` `kind`, `allOf[1..2]`;
`spec/relay.md` §11.5; D323)

#### Scenario: Priced passthrough refused
- **WHEN** a `kind: mcp` listing has a price other than `free` and `tasks: passthrough`
- **THEN** the listing is refused

#### Scenario: MCP listing accepted
- **WHEN** a `kind: mcp` listing has `price: {model: free}` and `tools: {allow: ["read_*"]}`
- **THEN** the listing is valid and its body carries `kind`, `tools` and the price

### Requirement: Listing Visibility
A listing SHALL have a `visibility` of `public` (default: listed and searched), `unlisted` (reachable by its URL,
never searched) or `private` (the publisher's own tenant only), which the control plane carries onto the gate's
`services.listed` and `annotations.search.hidden`. (`spec/schema/listing.json` `visibility`; `spec/relay.md` §12.2,
§13.4; D321)

#### Scenario: Unlisted service
- **WHEN** a service is published `unlisted`
- **THEN** `/mcp` `search` never returns it, while a client holding its ref can still reach it

### Requirement: Listing Publication State
A listing SHALL carry `published` (default `true`); `published: false` MUST unpublish the service, which the gate then
answers as unknown, and keeps the listing as a new version. (`spec/schema/listing.json` `published`;
`spec/relay.md` §13.5; D117, D323)

#### Scenario: Unpublish
- **WHEN** a person publishes a listing with `published: false`
- **THEN** `kontra publish` reports the service unpublished at the new version, and the gate answers the service as unknown

### Requirement: Listing Versioning
`kontra publish` SHALL NOT replace a listing unseen. Without `--version` or `--replace` it reads the current listing
and publishes only when there is none (version 0), otherwise prints its version, price, visibility and state and
publishes nothing. `--version N` sends the version the person read; `--replace` sends the one just read and prints
it. A `409 stale_version` MUST be reported with the current version. (`spec/tunnel.md` §6.4; D323)

#### Scenario: Existing listing without a version
- **WHEN** a person runs `kontra publish listing.yaml` for a service whose listing is at version 3
- **THEN** the CLI prints version 3's price, visibility and state, names `--version 3`, exits non-zero and publishes nothing

#### Scenario: Publish raced
- **WHEN** `kontra publish --version 3` runs after another publish moved the listing to version 4
- **THEN** the CLI reports `409 stale_version` at version 4 and nothing is published

### Requirement: Publish Sends The Listing To The Control Plane
`kontra publish` SHALL send the checked listing as `PUT /v1/services/{name}` to the operator's control plane
(`--control-plane`, or the issuer of the gate's authorization server) with a bearer token from a person's sign-in
for that publish only. It MUST NOT follow a redirect nor send the token anywhere else. A gate with no sign-in has no
control plane, and the publish is refused. (`spec/tunnel.md` §6.4; D323; sign-in in `node-enrollment-and-keys`)

#### Scenario: Redirect refused
- **WHEN** the control plane answers the `PUT` with a `302`
- **THEN** the CLI reports an error and the token is sent to no other host

#### Scenario: Gate without sign-in
- **WHEN** a person publishes to a team gate that serves no authorization server
- **THEN** the CLI refuses, saying that the gate's operator lists its services in its configuration

### Requirement: Publish Dry Run
With `--dry-run`, `kontra publish` SHALL check the file and print the request it would send, and MUST NOT sign in or
send anything. (`spec/tunnel.md` §6.4; D323)

#### Scenario: Dry run
- **WHEN** a person runs `kontra publish --dry-run listing.yaml` with a valid file
- **THEN** the CLI prints `PUT /v1/services/<name>` and the body, and makes no network request

### Requirement: No Money Logic In The Gate Or CLI
The gate SHALL hold no price and the CLI SHALL compute, convert, round and compare no amount: the CLI checks shapes and
carries the listing, and the control plane decides what it accepts. (`spec/relay.md` §11.4, §13; PRD §13; D323;
`internal/publish/listing.go:1-7`)

#### Scenario: No price column
- **WHEN** an operator inspects the gate's schema `kontra`
- **THEN** no table holds a price or amount column

### Requirement: Listing Prices In USD
A listing's `price` SHALL be an object with a required `model` (`free`, `per_task`, `per_call` or `per_session`) and
`currency` `USD` (the default); any other currency MUST be refused. (`spec/schema/listing.json` `$defs/price`; D323)

#### Scenario: Euro listing refused
- **WHEN** a listing's price has `currency: EUR`
- **THEN** `kontra publish` refuses it: listings are priced in USD

### Requirement: Free Price Model
A `free` price SHALL carry no amount other than `0`, `0.0` or `0.00`, no `reviewWindowSeconds` other than 0, and MUST
NOT carry `tools`, `freePerMonth` or `capPerMonth`. (`spec/schema/listing.json` `$defs/price`)

#### Scenario: Free with an amount
- **WHEN** a listing has `price: {model: free, amount: "1.00"}`
- **THEN** the listing is refused

### Requirement: Per-Task Price Model
A `per_task` price SHALL carry an `amount` in USD above 0 with at most 2 decimals, and MAY carry `reviewWindowSeconds`
(0 to 604800) and `taskTimeoutSeconds` (1 to 604800, default 3600). It is the escrow price shape.
(`spec/schema/listing.json` `$defs/price`; `spec/ext-escrow.md` intro; escrow in `escrow`)

#### Scenario: Sub-cent task price
- **WHEN** a `per_task` price has `amount: "1.005"`
- **THEN** the listing is refused (`examples/invalid/listing.three_decimals_per_task.json`)

#### Scenario: Zero task price
- **WHEN** a `per_task` price has `amount: "0.00"`
- **THEN** the listing is refused (`examples/invalid/listing.zero_per_task.json`)

### Requirement: Per-Task Price On MCP Services
A `per_task` price on a `kind: mcp` listing MUST set `tasks: required`, so that every call is a gate task its quote
covers and no plain call escapes Admission. (`spec/schema/listing.json` `allOf[3]`; `spec/ext-escrow.md` §6;
`spec/relay.md` §11.5; D322)

#### Scenario: Per-task MCP on durable
- **WHEN** a `kind: mcp` listing has a `per_task` price and `tasks: durable`
- **THEN** the listing is refused (`examples/invalid/listing.per_task_mcp_durable.json`)

#### Scenario: Per-task MCP without tasks
- **WHEN** a `kind: mcp` listing has a `per_task` price and no `tasks`
- **THEN** the listing is refused (`examples/invalid/listing.per_task_mcp_no_tasks.json`)

### Requirement: Per-Call Price Model
A `per_call` price SHALL price an MCP service's calls: it MUST be on a `kind: mcp` listing and carry an `amount` in USD
above 0 with at most 6 decimals, and MAY carry per-tool amounts in `tools` (at most 100, never `*`), `freePerMonth` and
`capPerMonth`, which no other model may carry. Nothing is held. (`spec/schema/listing.json` `allOf[0]`, `$defs/price`;
`spec/schema/price.json`; `spec/ext-escrow.md` §6)

#### Scenario: Per-call on an A2A listing
- **WHEN** a listing without `kind: mcp` has a `per_call` price
- **THEN** the listing is refused (`examples/invalid/listing.per_call_on_a2a.json`)

#### Scenario: Seven decimals
- **WHEN** a `per_call` price has `amount: "0.0000001"`
- **THEN** the listing is refused (`examples/invalid/listing.seven_decimals_per_call.json`)

### Requirement: Per-Session Price Model
A `per_session` price SHALL price an MCP service's sessions: it MUST be on a `kind: mcp` listing, carry an `amount` in
USD above 0 with at most 6 decimals and `idleTimeoutSeconds` (1 to 604800), and MUST NOT carry `tools`,
`freePerMonth` or `capPerMonth`. One billable session is one `mcp_sessions` row, legacy or virtual, from `created_at`
to `closed_at`. (`spec/schema/listing.json`; `spec/schema/price.json`; `spec/ext-escrow.md` §6; `spec/relay.md` §11.12;
D160)

#### Scenario: Cap on a session price
- **WHEN** a `per_session` price carries `capPerMonth`
- **THEN** the listing is refused (`examples/invalid/listing.cap_per_session.json`)

#### Scenario: Per-session without kind
- **WHEN** a listing with a `per_session` price names no `kind`
- **THEN** the listing is refused (`examples/invalid/listing.per_session_kind_absent.json`)

### Requirement: Price Metadata On MCP Tools
For a `per_call` service, the operator's control plane SHALL write the `spec/schema/price.json` object under
`sh.kontra/price` in the service's `tool_meta` annotation (`"*"` or a tool's name), and the gate SHALL render it
verbatim into the `_meta` of each matching `tools/list` entry, legacy and modern, and in `/mcp` `describe`. An
upstream's own `sh.kontra/price` MUST NOT reach a client. (`spec/ext-escrow.md` §6; `spec/relay.md` §2.5, §11.3,
§13.4; D27)

#### Scenario: Operator price shown on tools
- **WHEN** a service's annotations hold `tool_meta: {"*": {"sh.kontra/price": {"model": "per_call", "amount": "0.002", "currency": "USD"}}}`
- **THEN** every tool in the client's `tools/list` carries that object at `_meta["sh.kontra/price"]`

#### Scenario: Upstream price spoof removed
- **WHEN** an upstream's `tools/list` entry carries `_meta["sh.kontra/price"]` and the operator wrote none
- **THEN** the client's entry carries no `sh.kontra/price` (`mcp_meta_spoof`)

### Requirement: Price Metadata On MCP Initialize
For a `per_session` service, the operator's control plane SHALL write the price object (`model: per_session`,
`idleTimeoutSeconds` equal to the service's `session_idle_s`) under `sh.kontra/price` in the `initialize_meta`
annotation, and the gate SHALL merge it into the `_meta` of the `initialize` and `server/discover` results. An
upstream's own `sh.kontra/price` there MUST NOT reach a client. (`spec/ext-escrow.md` §6; `spec/relay.md` §2.5, §11.2,
§11.12, §13.4)

#### Scenario: Session price on initialize
- **WHEN** a service's `initialize_meta` holds `{"sh.kontra/price": {"model": "per_session", "amount": "0.50", "currency": "USD", "idleTimeoutSeconds": 900}}`
- **THEN** the client's `initialize` result carries it at `_meta["sh.kontra/price"]`

### Requirement: Calls Rows For MCP Calls
The gate SHALL write one `calls` row per MCP `tools/call` it relays when the response completes, reading only the tool
name, byte counts, duration and top-level outcome, never `arguments`, `content` or `structuredContent`. A call that
got no answer at all has no row; `/mcp` `hire` writes none. The gate never deletes `calls` rows, except an anonymous
tenant's on purge. (`spec/relay.md` §11.4, §13.5; D196, D287, D289; `migrations/0001_init.sql:291-309`)

#### Scenario: Plain call metered
- **WHEN** a plain `tools/call` completes
- **THEN** one `calls` row holds its tenant, service, tool, buyer, principal, session hash, byte counts and duration, with `task_id` NULL

#### Scenario: Refused before forwarding
- **WHEN** a call is refused by the enrollment check before any upstream exchange
- **THEN** no `calls` row is written

### Requirement: Calls Row Outcome
A `calls` row's `is_error` SHALL be true for a JSON-RPC error, a top-level `isError: true`, an exchange answered whose
response never arrived, a sealed call the node refused, or a gate-owned task ending in a gate-written failure or a
cancel. It SHALL be NULL for a passthrough task-augmented call (written when the `CreateTaskResult` returns) and for a
plain call answered by an `InputRequiredResult`. (`spec/relay.md` §11.4, §11.5, §11.8, §11.12; D202, D204)

#### Scenario: Cut stream
- **WHEN** the upstream's event stream for a plain call is cut before its JSON-RPC response
- **THEN** the client's call fails and its `calls` row has `is_error = true`

#### Scenario: Passthrough outcome not observed
- **WHEN** a passthrough service answers a task-augmented call with a `CreateTaskResult`
- **THEN** one `calls` row is written with `is_error` NULL and `task_id` NULL

#### Scenario: Gate task row
- **WHEN** a gate-owned MCP task ends
- **THEN** its `calls` row is written with its terminal event and has `task_id` set

### Requirement: Metered Settlement Of MCP Calls
An operator that meters MCP prices SHALL bill from the gate's `calls` and `mcp_sessions` tables only: a `per_call`
call is billable when its row has `is_error = false`, a row with `is_error` NULL MUST NEVER be billable, and a
`per_session` session is one `mcp_sessions` row. Settlement keys on gate state and top-level `isError`, never content;
nothing is held, the operator invoices. (`spec/ext-escrow.md` §6; `spec/relay.md` §11.4, §11.5, §11.8, §13.5)

#### Scenario: Passthrough row not billable
- **WHEN** a `per_call` service's passthrough task-augmented call writes a `calls` row with `is_error` NULL
- **THEN** the operator does not bill that row

#### Scenario: Failure reported as success
- **WHEN** a `per_call` service returns failures without a top-level `isError`
- **THEN** each such row is billable, and the caller's exposure is bounded by `capPerMonth`
