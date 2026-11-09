# Tasks

## 1. Decision record

- [x] 1.1 Add D350 to `docs/decisions.md` under a 7 Nov owner-decision heading: plain A2A first message and no MCP listings or prices (kontra#22), naming what it supersedes (`spec/ext-input-schema.md`, D339's `inputSchema`, D27's price key, D160, D322's pricing reason) and that `tasks: required` and `calls` stay (design D2). Verify `grep -n '^### D350' docs/decisions.md` finds it and `make check` passes.

## 2. Withdraw the input-schema extension

- [x] 2.1 Delete `spec/ext-input-schema.md`, `spec/schema/ext-input-schema.params.json` and `spec/schema/examples/ext-input-schema.params.json`; drop their entries from `scripts/check_specs.py` and from the spec list in `docs/README.md`. Verify `make check` passes and `grep -rn 'ext-input-schema' spec docs scripts` finds only D350 and older decisions.
- [x] 2.2 In `internal/gatemcp/tools.go`, remove `InputSchema` / `InputSchemaURI` from `describe`'s capabilities, the `describe` description's input-schema sentence and the `hire` description's `{data: …}` first-message text; update `tools_test.go` and `gatemcp_test.go`. Verify `go test ./internal/gatemcp/...` passes and a `describe` of a card carrying the entry returns no `inputSchema` (new test).
- [x] 2.3 In `examples/sellers/a2ui`, replace the input-schema card entry with `examples` on its skill and update its README and `main_test.go`; keep booking from a data part if it still works. Verify `go test ./examples/sellers/a2ui/...` and the `a2ui_passthrough` test pass.
- [x] 2.4 Edit `spec/relay.md` §2.6 and §12.6, `README.md` and `docs/prd-v4.md` notes so nothing describes a typed first message; point at `skills[].examples` and `input-required`. Verify `grep -rniE 'input.?schema' spec README.md docs/prd-v4.md` finds nothing.

## 3. Listings are A2A only; free or per_task

- [x] 3.1 Edit `spec/schema/listing.json`: remove `kind`, `tools`, `tasks`, the `per_call` / `per_session` models and the rules that served them; delete the MCP and metered fixtures (`listing.mcp_*`, `listing.*per_call*`, `listing.*per_session*`, `listing.cap_per_session`, `listing.review_window_per_call`, `listing.per_task_mcp_*`, `listing.tools_on_a2a`, `listing.tasks_on_a2a`) and add invalid fixtures for `kind`, `tools` and `model: per_call`. Verify `make check` (schema fixtures) passes.
- [x] 3.2 Delete `spec/schema/price.json` and `spec/schema/examples/price.per_*.json`, and their entries in `scripts/check_specs.py`. Verify `make check` passes.
- [x] 3.3 Update `internal/publish/listing.go` and `command.go` to the new schema (drop the 64-glob `tools` rule and the metered models); update `publish_test.go` and `TestSchemaFixtures`. Verify `go test ./internal/publish/...` passes with every fixture agreeing.
- [x] 3.4 Edit `spec/tunnel.md` §6.4, `spec/ext-escrow.md` (remove §6 metered prices and the MCP escrow text), `spec/relay.md` §11.4 and §13.4–13.5 pricing parts, and the README so no text describes MCP listings or MCP prices. Verify `grep -rnE 'per_call|per_session|sh\.kontra/price|kind: mcp' spec README.md` finds nothing.

## 4. No quote on an MCP task

- [x] 4.1 In `internal/hooks/remote.go`, treat a `quote` for operation `task` with `protocol: mcp` as a deny, and replace `MsgPricedTasks` with `MCP services are not priced` for every MCP case (design D1); update `remote_test.go`. Verify `go test ./internal/hooks/...` passes, including a new case for a quoted MCP task.
- [x] 4.2 Remove the quoted-MCP-task path in `internal/mcprelay` (`legacy.go`, `modern.go`, `relay.go`: approve-URL status, `quote_expired` / `declined` outcomes for MCP) and its tests in `tasks_pg_test.go`, `modern_test.go` and `policy_pg_test.go`; keep a test that a hook's quote on a durable MCP task creates no task and reaches no upstream. Verify `go test ./internal/mcprelay/... ./internal/lifecycle/...` passes.
- [x] 4.3 Update `internal/gate/admission_it_test.go` and `mcp_tasks_it_test.go` to the new refusal text and the MCP-task deny. Verify those integration tests pass.
- [x] 4.4 Edit `spec/relay.md` §11.5, §11.8, §11.12 and §13.1–13.2 so a quote is valid only for A2A tasks and `hire`. Verify `make check` passes.

## 5. Keep annotation and scrub coverage without prices

- [x] 5.1 Where tests use `sh.kontra/price` only as a sample `tool_meta` / `initialize_meta` key (`internal/mcprelay`, `internal/mcpwire/scrub_test.go`, `internal/gate/*_it_test.go`, `internal/store/mcp_test.go`, `mcptest`), switch to a neutral `sh.kontra/` test key so rendering and the upstream scrub stay covered; update `internal/mcpwire/scrub.go`'s comment. Verify `go test ./...` passes and `grep -rn 'sh.kontra/price' internal` finds nothing.
- [x] 5.2 Remove price annotations from `deploy/compose/kontra*.yaml`, `examples/mcp/*/kontra.yaml` and `internal/gate/services.go` docs or tests that set them. Verify `make check` and `go test ./internal/gate/... ./internal/config/...` pass.

## 6. `/mcp` search is A2A only

- [x] 6.1 In `internal/gatemcp`, make `search` return `kind: a2a` services only and drop its `kind` input; update `spec/schema/mcp-tools.json` `$defs/search` and `spec/relay.md` §12.2; update `catalog_pg_test.go`. Verify `go test ./internal/gatemcp/...` and `gate_mcp_search` pass, with a case that an MCP service is not returned and `{kind: "a2a"}` is a tool error naming `kind`.

## 7. Integration

- [x] 7.1 Run `make check`, the e2e suite and the TCK. Verify all pass, the `mcp_*` e2e tests stay green minus their pricing cases, and hosting an MCP server through the tunnel behaves as before.
- [x] 7.2 Remove the input-schema bullet from `relay-a2a`'s Known limitations and the currency and `idleTimeoutSeconds` bullets from `listings-and-pricing`'s that no longer apply (`openspec/specs/*/spec.md`, after sync). Verify `openspec validate --specs --strict` passes.

## Workflow follow-up

- Open a kontra-cloud issue for its side: stop writing `sh.kontra/price` / `sh.kontra/escrow` for MCP services, refuse MCP listings, and drop per_call / per_session from Console v4 (kontra-cloud#59).
- Archive this change with `/opsx:archive` once the PR merges, and check the synced specs with `openspec validate --specs --strict`.
