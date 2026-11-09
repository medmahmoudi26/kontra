## MODIFIED Requirements

### Requirement: Quotes Only For Gate-Owned Tasks
A `quote` decision SHALL be valid only for operation `task` on a gate-owned A2A task or a `hire`. For an MCP task,
`session`, `call` or a passthrough task-augmented call the gate MUST treat it as a deny with `-32603` `MCP services
are not priced`, whatever the hook's quote holds, and forward nothing. (`spec/relay.md` §13.1; D350)

#### Scenario: Quote on a plain call
- **WHEN** the hook answers `quote` for operation `call`
- **THEN** the client receives `-32603` `MCP services are not priced` and nothing is forwarded

#### Scenario: Quote on a passthrough task
- **WHEN** the hook answers `quote` for a task-augmented call on a `passthrough` service
- **THEN** the client receives `-32603` `MCP services are not priced` and nothing is forwarded

#### Scenario: Quote on a durable MCP task
- **WHEN** the hook answers `quote` for operation `task` with `protocol: mcp` on a `durable` or `required` service
- **THEN** the client receives `-32603` `MCP services are not priced`, no task is created and the upstream receives nothing

#### Scenario: Quote on an A2A task
- **WHEN** the hook answers a valid `quote` for an A2A task
- **THEN** the task is held `quoted` with the approve URL, as before

## REMOVED Requirements

### Requirement: Quoted MCP Tasks
**Reason**: No MCP service is priced, so no MCP task is quoted (D350, kontra#22).
**Migration**: A hook that quoted MCP tasks gets the deny `MCP services are not priced`; price the service as an A2A agent instead.

### Requirement: Escrow On MCP Tasks
**Reason**: Escrow covers A2A tasks and `hire` only (D350).
**Migration**: The control plane stops writing `sh.kontra/escrow` into an MCP task's `mcpMeta`. The gate still renders operator `mcpMeta` verbatim (`relay-mcp`).
