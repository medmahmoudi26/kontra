# 3. Enforce I/O schema server-side, not advisory

## Status

**Implemented** (Phase 1). ajv (Draft 2020-12) validates on the execution path: server-side
at `POST /api/runs` (seed inputs vs the actor input schema → 400) and at runtime in
`mapAndRoute` (merged units vs the node's server-stamped input schema → non-retryable
failure). See `src/schema/validateRun.ts`. Schemas travel inline (catalog → graph) for this
runtime DATA validation; their evolution COMPAT is the separate concern now wired in
Apicurio (ADR 0008). The enforcement ADR 0003 demanded — schema validation on the
execution path — exists.

## Context

The defining theme of the arch review: planes that should be load-bearing contracts are currently advisory — enforced only by prose or by CI that does not exist. Concretely today:

- `validateGraph` is structural-only.
- `checkEdge` runs in the browser only.
- `mapAndRoute` stores merged units with zero schema validation.

Any non-browser client therefore feeds unvalidated garbage straight to actors.

## Decision

JSON Schema validation must run on the **execution path**, not only in the browser:

- Server-side at `POST /api/runs`.
- At runtime in `mapAndRoute`.

The contract is only real if code on the execution path enforces it.

## Consequences

- Non-browser clients (CLI, other services, future Go callers) can no longer bypass schema validation.
- Validation moves onto the critical path and must be implemented at both the API boundary and in `mapAndRoute`; the browser checks become a UX convenience, not the enforcement point.
- This is the precondition for treating per-actor I/O compat (ADR 0008) as enforceable rather than advisory.
- Implemented in Phase 1: the I/O contract is now enforced on the execution path, not advisory. The browser `checkEdge` remains a UX convenience.
