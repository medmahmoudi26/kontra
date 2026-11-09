# Proposal: plain A2A first message, no MCP listings

Tracks medmahmoudi26/kontra#22. Owner decision of 7 Nov 2026.

## Why

Two features are too early to maintain. The input-schema extension invents a typed first message that A2A does not
have, and no client implements it: `kontra connect` never renders or checks a schema. MCP listings and their metered
prices (`per_call`, `per_session`) pull Stripe Billing, meters and per-buyer caps into M5 (kontra-cloud#60 item 6)
before anyone sells an MCP server. Dropping both now shrinks the M5 surface and the public spec.

## What Changes

- **BREAKING** The input-schema extension `https://kontra.sh/ext/input-schema/v1` is withdrawn. Authors suggest
  first messages with the card's own `skills[].examples`; an agent that needs structured input asks for it with an A2UI
  surface (`input-required`) and validates its own input. A seller card that still carries the entry is relayed as any
  extension the gate does not own.
- **BREAKING** `/mcp` `describe` no longer returns `capabilities.inputSchema`, and `hire` no longer documents a
  `{data: …}` first message.
- **BREAKING** Listings are A2A only. The listing file loses `kind`, `tools` and `tasks`; the control plane refuses a
  listing for an MCP service.
- **BREAKING** The `per_call` and `per_session` price models are removed. A price is `free` or `per_task`.
- **BREAKING** No MCP service is priced or escrowed. The `sh.kontra/price` object is no longer defined for MCP
  `tools/list` or `initialize`. An Admission `quote` for an MCP task is treated as a deny, as it already is for MCP
  sessions and plain calls.
- **BREAKING** `/mcp` `search` returns A2A agents only and no longer takes a `kind` filter.
- Unchanged: hosting MCP servers through the tunnel (`--serve mcp:…`, stdio, roots, `/mcp/{svc}`, sessions, task
  modes including `required`, sealing), the `calls` metering record, and `describe` on an MCP service the caller can
  reach. An MCP service is free and private to its tenant or team gate, like an unlisted A2A service.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `relay-a2a`: the input-schema requirements are removed.
- `gate-mcp-tools`: `describe` loses the input schema, `hire` loses the data-part first message, `search` is A2A only.
- `listings-and-pricing`: listings are A2A only; `per_call`, `per_session` and the MCP price metadata are removed.
- `escrow`: quoted and escrowed MCP tasks are removed; quotes are valid only for A2A tasks and `hire`.
- `relay-mcp`: a gate-owned MCP task is never quoted.

## Impact

- **Spec** (`spec/`): delete `ext-input-schema.md` and its schemas; edit `listing.json`, `price.json`,
  `mcp-tools.json`, their examples and invalid fixtures; edit `relay.md` §11.4, §11.5, §11.12, §12.2, §12.6, §13.1,
  §13.2, `ext-escrow.md` §6, `tunnel.md` §6.4, `README.md`, `docs/prd-v4.md` notes.
- **Code**: `internal/gatemcp` (`describe`, `hire` text, `search`), `internal/publish` (listing checks),
  `internal/hooks` and `internal/mcprelay` (quote on an MCP task becomes a deny), `examples/sellers/a2ui`.
- **Database**: none. No migration holds a price or amount column (`calls` holds no price); `services.tasks_mode`
  `required` stays.
- **Decisions**: one new entry in `docs/decisions.md` (D350) superseding the input-schema extension
  (`spec/ext-input-schema.md`, D339's `inputSchema`) and MCP pricing (D27's price key, D160, the pricing reason of
  D322).
- **Downstream**: kontra-cloud must stop writing `sh.kontra/price` into `tool_meta` / `initialize_meta`, refuse MCP
  listings, and drop the per_call / per_session parts of Console v4 (kontra-cloud#59).
