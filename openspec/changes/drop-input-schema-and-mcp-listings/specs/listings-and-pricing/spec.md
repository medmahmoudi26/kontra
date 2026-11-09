## MODIFIED Requirements

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

## ADDED Requirements

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

## REMOVED Requirements

### Requirement: MCP Listings
**Reason**: Listings are A2A only (D350, kontra#22).
**Migration**: Host the MCP server through the tunnel unlisted; it is free and private to its tenant or team gate.

### Requirement: Per-Task Price On MCP Services
**Reason**: No MCP service is priced (D350).
**Migration**: None. `tasks: required` stays as a hosting mode without a price.

### Requirement: Per-Call Price Model
**Reason**: Metered MCP prices are removed (D350).
**Migration**: None; list the service as an A2A agent with a `per_task` price if it must be sold.

### Requirement: Per-Session Price Model
**Reason**: Metered MCP prices are removed (D350).
**Migration**: None; list the service as an A2A agent with a `per_task` price if it must be sold.

### Requirement: Price Metadata On MCP Tools
**Reason**: With no MCP prices there is no price to show on `tools/list` (D350).
**Migration**: The control plane stops writing `sh.kontra/price` into `tool_meta`. The gate's generic rendering of
operator annotations and its scrub of upstream `sh.kontra/` keys are unchanged (`relay-mcp`).

### Requirement: Price Metadata On MCP Initialize
**Reason**: With no MCP prices there is no price to show on `initialize` (D350).
**Migration**: The control plane stops writing `sh.kontra/price` into `initialize_meta`.

### Requirement: Metered Settlement Of MCP Calls
**Reason**: Nothing about MCP calls or sessions is billed (D350).
**Migration**: `calls` and `mcp_sessions` rows stay as usage records (`Calls Rows For MCP Calls`).
