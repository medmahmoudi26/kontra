# 8. Compat ownership split: buf owns the envelope, Apicurio owns author I/O

## Status

Settled as ownership; **implemented**. The buf side: `buf lint`, `buf breaking` (on PRs),
and `buf generate` drift are wired in CI, so the envelope's compat is enforced. The
Apicurio side is now wired too: `actorkit/schema_registry.py` (+ `scripts/actor_build.py`,
`make register`) publishes each actor's input schema under a **BACKWARD** rule and output
under **FORWARD**, and an incompatible change under the same `(name, version)` is rejected
(HTTP 409 -> `SchemaIncompatible`). Per-actor I/O is thus both data-validated inline
(ADR 0003) **and** compat-gated on evolution.

## Context

The envelope and per-actor I/O evolve on different cadences and under opposite variance rules. A consumer must accept old producers' outputs (forward compatibility), while a producer must accept new consumers' narrower inputs (backward compatibility). One tool pretending to own both surfaces would apply the wrong rule to one of them.

## Decision

- **buf owns** backward/forward compatibility of the proto **envelope** (`EntryInput` today; `StepOptions`; `RunEnvelope` when Nexus lands).
- **Apicurio owns** per-actor I/O compat: **BACKWARD on inputs**, **FORWARD on outputs** — registered per `(name, version)` as artifacts `{name}.run.input` / `{name}.run.output` in group `kontra`.
- The repo-level `kontra.yaml` records this per-surface in its `contracts` block; it **does not enforce** it.

## Consequences

- Each compat check lives in the registry that actually governs its surface, so the correct variance rule is applied to each.
- `kontra.yaml` documents ownership but is not the enforcement point — enforcement lives in buf and Apicurio.
- The local registry is the in-memory Apicurio image, so its compat history resets on restart (re-register after a fresh `make up`); the SQL/kafkasql variants persist it for durable, cross-restart compat history.
- This split is a direct consequence of the opaque-payload / proto-vs-Apicurio split in ADR 0002.
