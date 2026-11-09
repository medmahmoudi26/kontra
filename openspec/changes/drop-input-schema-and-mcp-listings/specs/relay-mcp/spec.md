## MODIFIED Requirements

### Requirement: Modern Gate-Owned Tasks
A modern `tools/call` declaring the tasks extension, on a `durable` or `required` service whose upstream declared it,
SHALL become a gate-owned task (Admission operation `task`); any other is a plain call, which `required` refuses
(`-32021` or `-32601`). The gate SHALL wait up to 2 s and answer in line, else with `CreateTaskResult`
(`pollIntervalMs: 1000`), only after the task row commits; an MCP task is never quoted (`escrow`). (`spec/relay.md`
§11.12 Gate-owned tasks; D159, D322, D350; `mcp_modern_task`)

#### Scenario: Fast task answered in line
- **WHEN** a modern task-declaring call's upstream completes within 2 s
- **THEN** the client receives the `CallToolResult` with `resultType: "complete"` and never sees a task

#### Scenario: Client leaves during the wait
- **WHEN** the client closes its response stream during the 2 s wait
- **THEN** it is sent no answer, and the task is sent `kontra.cancel` and ends `canceled` within 10 s
