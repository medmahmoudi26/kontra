# 2. proto-envelope / Apicurio-JSON split with an opaque payload

## Status

Settled; **implemented** (Phase 1). Protos under `shared/contracts/kontra/v1/`
(`run.proto`, `step.proto`, `entry.proto`) with buf codegen wired and drift-checked in CI;
the cross-language boundary types are generated + committed. Per-actor I/O is validated as
JSON Schema (Draft 2020-12) on the execution path (ADR 0003) **and** compat-checked in
**Apicurio** (ADR 0008, `actorkit/schema_registry.py`).

One nuance the proto comments now carry: of the two envelopes, **`EntryInput` (entry.proto)
is the dispatch input actually in use** — the interpreter starts the Python `run` workflow
as a Temporal child workflow with it. **`RunEnvelope` (run.proto) is the FUTURE Nexus
envelope** for cross-namespace dispatch (namespace-per-author, ADR 0001), not yet wired.

## Context

The wire envelope is shared byte-for-byte by the TypeScript, Python, and (future) Go seams, so it must not churn whenever a single actor's shape changes. Per-actor input/output/params shapes change constantly. Modeling per-actor I/O as regenerated proto messages would force the shared envelope and every seam's generated code to move on each actor edit.

The earlier `options.proto` model encoded per-step options as a `MethodOptions` extension on a per-actor proto service — coupling per-actor surface back into proto. That file was removed.

## Decision

- buf/proto is the **internal control-plane envelope only**:
  - `RunEnvelope` (`run.proto`) carries identity/routing (`actor`), lineage (`run_id`, `node_id`), `tenant`, `idempotency_key`, an **opaque** `bytes payload`, `bytes params`, and `DispatchOptions`.
  - `StepOptions` / `RetryPolicy` (`step.proto`) carry per-step scheduling and retry.
- `options.proto` (the per-actor `MethodOptions`-extension model) is removed; `step.proto` records the removal.
- Per-actor input/output/params are **JSON Schema (Draft 2020-12) in Apicurio**, never modeled as per-actor proto messages.

## Consequences

- One generic Nexus wire type serves all actors; the shared envelope does not regenerate on per-actor shape changes.
- Because `payload` is opaque bytes, the claim-check codec can offload large payloads transparently below the envelope (see ADR 0005) without the envelope knowing the per-actor shape.
- Compatibility ownership necessarily splits across two surfaces (see ADR 0008): buf owns the envelope, Apicurio owns per-actor I/O.
- Apicurio is wired (`actorkit/schema_registry.py`, `make register`): per-actor schemas are registered per `(name, version)` and compat-checked (input BACKWARD, output FORWARD) at build time.
