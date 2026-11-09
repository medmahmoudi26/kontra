# Design

## Context

See proposal.md for why. The gate holds no price (PRD §13): the listing file is checked by `kontra publish` and
carried to the operator's control plane, which writes the gate's `services` row and its annotations. The gate learns
about money only through the Admission hook (`quote`) and operator annotations (`tool_meta`, `initialize_meta`,
`mcpMeta`), which it renders verbatim. So most of this change is spec, schema and CLI work; the gate code that changes
is `/mcp` (`describe`, `hire`, `search`) and the quote check at Admission.

No migration holds a price or amount column. The `calls` table is the metering record and holds no price;
`services.tasks_mode = 'required'` (0004) exists for D322.

## Goals / Non-Goals

**Goals:**
- No spec, schema, README or example mentions the input-schema extension, MCP listings or MCP prices.
- The gate refuses to start the paid path for an MCP task, whatever the operator's hook answers.

**Non-Goals:**
- Any change to MCP hosting: transport, sessions, policy, task modes, sealing, the stdio bridge and roots.
- Removing the generic annotation machinery (`tool_meta`, `initialize_meta`, `mcpMeta`) or the `sh.kontra/` scrub of
  upstream `_meta`. They stay; only the price semantics written into them go.
- Dropping `calls` metering. It is usage accounting, not pricing.
- kontra-cloud work. It is listed in the proposal's Impact for the Cloud agent to track.

## Decisions

**D1. A `quote` on an MCP task is a deny, enforced by the gate.** The quote check in the Admission hook client
already denies a quote for `session` and `call`. It is widened to a quote for operation `task` whose request names
`protocol: mcp`, with a new refusal text `MCP services are not priced` (`-32603`). The old text `priced tasks need
tasks: durable` is replaced by the same new text for `session` and `call`: it pointed at a remedy that no longer
exists. *Alternative:* leave it to the control plane to never quote MCP. Rejected: a gate that relays a quoted MCP task
would show an approve URL for a price nothing settles, and the check costs one condition.

**D2. `tasks: required` stays.** It was added so `per_task` could cover every call (D322), but it is also a hosting
mode an operator can want without a price: every call durable and resumable. Keeping it needs no migration and keeps
the issue's promise that hosting does not change. *Alternative:* remove it with its pricing reason. Rejected: it would
need a migration whose down already refuses while any service is `required`, for no gain.

**D3. `search` drops its `kind` input; results keep `kind`.** The input schema loses `kind` (a client that still sends
it gets a tool error, `isError`, naming the unknown argument, as for any argument a tool does not take, D172). Each result keeps `kind: "a2a"` so a client that
reads it does not break. *Alternative:* accept `kind: "a2a"` only. Rejected: a filter with one value is noise.

**D4. A seller card that still carries the input-schema entry is relayed unchanged.** It becomes an extension the gate
does not own (`spec/relay.md` §2.6, D326). Nothing reads it; `describe` no longer surfaces it. No refusal, so old
sellers keep working.

**D5. The listing file loses `kind`, `tools` and `tasks` as members.** `kontra publish` refuses them as unknown
members. Which services are MCP is known only to the gate and control plane (`services.kind`), so the control plane
refuses a listing whose service is an MCP service. The CLI cannot check that.

**D6. One decision entry, D350.** It records both removals as the owner's decision of 7 Nov and names what each
supersedes. Earlier decisions are not edited; D350 says which of their parts no longer hold.

## Risks / Trade-offs

- [An operator hook still quotes MCP tasks] → D1 makes it a deny with a clear message; the e2e and hook tests assert it.
- [Cloud still writes `sh.kontra/price` into annotations] → The gate keeps rendering operator annotations verbatim, so
  a stale price would show. Mitigation: the Cloud follow-up in the proposal; the gate does not police operator keys
  (a known limitation of `relay-mcp`).
- [A client sends `search {kind: …}`] → It gets a tool error naming `kind`. Acceptable before M5; nothing outside this repo calls it yet.
- [Tests and fixtures that cover MCP pricing are deleted, losing coverage of shared code] → Keep the `mcp_*` e2e tests
  minus their pricing cases; the `calls` row tests stay.

## Migration Plan

No data migration. Deploy is the normal gate release; rollback is the previous image. The decision entry and the spec
edits land in the same PR as the code.
