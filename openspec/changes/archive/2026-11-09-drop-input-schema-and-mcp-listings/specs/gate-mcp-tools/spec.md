## MODIFIED Requirements

### Requirement: Search Filtering And Paging
`search` SHALL match `query` as a case-insensitive substring of the ref, name or description, filter `price` by
equality on the `price_model` annotation (`free` meaning none), order by ref, and answer 50 a page with an opaque
keyset `nextCursor`. It SHALL take no `kind` input. One request MUST read at most 1000 services, answering what it
found with `nextCursor` at the last one read (`spec/relay.md` §12.2; D271, D293, D350; test `gate_mcp_search`).

#### Scenario: Free filter
- **WHEN** a principal calls `search {price: "free"}`
- **THEN** only visible A2A services without a `price_model` annotation are returned

#### Scenario: Kind input refused
- **WHEN** a principal calls `search {kind: "a2a"}`
- **THEN** the gate answers a tool error (`isError`) naming the unknown argument `kind`, as for any argument the tool does not take

## ADDED Requirements

### Requirement: Search Returns A2A Agents Only
`search` SHALL return only `kind: a2a` services, each result still carrying `kind: "a2a"`, and MUST NOT return an MCP
service whatever its visibility; `describe` SHALL keep answering for an MCP service the principal can reach by ref
(`spec/relay.md` §12.2; D350; kontra#22).

#### Scenario: MCP service not searched
- **WHEN** a principal's tenant hosts an MCP service `files` and the principal calls `search {query: "files"}`
- **THEN** the result does not hold `files`

#### Scenario: MCP service still described
- **WHEN** the same principal calls `describe` with the ref of `files`
- **THEN** the gate answers with its tools, as before

### Requirement: First Message Suggestions From Skill Examples
`describe` SHALL be the way a client learns what to send first: it returns each skill's `examples` from the served
card, and neither `describe` nor `hire` SHALL document or expect a typed first message. An agent that needs structured
input asks for it in an `input-required` turn (A2UI or text) (`spec/relay.md` §12.6; D339, D350).

#### Scenario: Suggested prompts
- **WHEN** a principal describes an A2A service whose card has a skill with `examples: ["a table for two"]`
- **THEN** `capabilities.skills` lists that skill with its `examples`, and the result carries no `inputSchema`

## REMOVED Requirements

### Requirement: Describe Input Schema Capability
**Reason**: The input-schema extension is withdrawn (D350, kontra#22).
**Migration**: Clients read `capabilities.skills[].examples` for suggested first messages.

### Requirement: Hire Input Schema Data Part First Message
**Reason**: A first message is plain A2A; there is no typed first message (D350).
**Migration**: Send text (or any parts); an agent that needs structured input asks with `input-required`.

### Requirement: Search Lists MCP Services
**Reason**: MCP services are not listed (D350).
**Migration**: Reach an MCP service by its ref with `describe`, or call it at `/mcp/{svc}`.
