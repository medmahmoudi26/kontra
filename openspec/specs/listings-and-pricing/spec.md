# listings-and-pricing Specification

## Purpose
A listing says how one of a publisher's A2A services is sold: its visibility, sealing and price, `free` or `per_task`.
It is a file that `kontra publish` checks and sends to the operator's control plane (Kontra Cloud), which decides what
it accepts and writes the gate's `services` row and annotations; the gate itself holds no price. MCP services are not
listed or priced (D350). This capability covers the listing file, the price models and the metering of MCP calls.
`spec/schema/listing.json`, `spec/tunnel.md` §6.4, `spec/ext-escrow.md` §6 and `spec/relay.md` §11.4, §13.4 and §13.5
are normative. The sign-in a publish uses belongs to `node-enrollment-and-keys`; escrow of `per_task` prices to
`escrow`; `/mcp` `search` to `gate-mcp-tools`.

## Known limitations
- A listing prices in USD only (`currency` const `USD`, at most 2 decimals), while an Admission `quote` may name any
  ISO 4217 code (`EUR` in the examples of `spec/ext-escrow.md` §1); nothing ties a quote's currency to the listing's
  (`spec/schema/listing.json`; `spec/ext-escrow.md`).
- A listing carries no principal `allow:` list: who may call a service is the gate's `services.allow`, which the
  control plane writes, not a listing field (`spec/schema/listing.json`; `migrations/0001_init.sql:39`).

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
`examples/invalid/listing.*.json` refused. (`spec/tunnel.md` §6.4; D323, D350; `TestSchemaFixtures`, `check_specs.py`)

#### Scenario: Too many globs
- **WHEN** a listing's `tools.allow` and `tools.deny` hold 40 globs each
- **THEN** both the schema and `kontra publish` refuse it, because a listing has no `tools` member

#### Scenario: Fixture sets agree
- **WHEN** `make check` runs the listing fixtures through both the schema and `kontra publish --dry-run`
- **THEN** every valid fixture is accepted and every invalid fixture is refused by both

### Requirement: A2A Listings
A listing SHALL describe an A2A service and MUST NOT carry `kind`, `tools` or `tasks`; `kontra publish` refuses each as
an unknown member. (`spec/schema/listing.json`; D323, D350)

#### Scenario: Kind on a listing
- **WHEN** a listing carries `kind: mcp`
- **THEN** `kontra publish` refuses it, naming `kind`, and sends nothing

#### Scenario: Tools on an A2A listing
- **WHEN** a listing carries `tools: {deny: ["x"]}`
- **THEN** `kontra publish` refuses it, naming `tools`

#### Scenario: Free A2A listing
- **WHEN** a listing names only `service` and `price: {model: free}`
- **THEN** it is valid, with `visibility: public`, `sealed: off` and `published: true`

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
A listing's `price` SHALL be an object with a required `model` (`free` or `per_task`) and `currency` `USD` (the
default); any other currency or model MUST be refused. (`spec/schema/listing.json` `$defs/price`; D323, D350)

#### Scenario: Euro listing refused
- **WHEN** a listing's price has `currency: EUR`
- **THEN** `kontra publish` refuses it: listings are priced in USD

#### Scenario: Metered model refused
- **WHEN** a listing's price has `model: per_call` or `model: per_session`
- **THEN** `kontra publish` refuses it and sends nothing

### Requirement: Free Price Model
A `free` price SHALL carry no amount other than `0`, `0.0` or `0.00`, and no `reviewWindowSeconds` other than 0.
(`spec/schema/listing.json` `$defs/price`)

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

### Requirement: MCP Services Are Not Listed
The operator's control plane SHALL refuse a listing whose service is an MCP service, and an MCP service SHALL be
free and reachable only within its tenant or team gate, as an unlisted A2A service is. Hosting it through the tunnel
is unchanged. (Cloud; D350; kontra#22)

#### Scenario: Listing for an MCP service
- **WHEN** a person publishes a listing naming a service the gate hosts as `kind: mcp`
- **THEN** the control plane refuses it and the service stays unlisted and free

#### Scenario: MCP hosting unaffected
- **WHEN** a node serves `--serve mcp:files=stdio:…` and a principal of the same tenant calls `/mcp/files`
- **THEN** the call is relayed as before
