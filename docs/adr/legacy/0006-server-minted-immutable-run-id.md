# 6. Server-minted immutable runId, distinct from the user graph label

## Status

**Implemented** (Phase 1). `POST /api/runs` mints the runId (`crypto.randomUUID`), rejects a
client-supplied one (400), and persists a durable run record (`repo.createRun`) before
starting the interpreter; the web stopped supplying a runId. The Temporal workflow id is
derived deterministically (`orch-${runId}`). The run store (`src/db/repo.ts`) now backs onto
**SQLite** (`node:sqlite`, per-mutation transactions) rather than a whole-file-rewrite JSON
blob, so a crash mid-write can no longer corrupt the record — the "durable" claim is now
literally true. See `src/server.ts`, `src/db/repo.ts`, `src/temporalClient.ts`.

## Context

The completeness pass caught that `runId` was a reused, human-typed graph label (e.g. `"graph-1"`). A reused id breaks all lineage and idempotency: under at-least-once retries, side effects double-fire because there is no unique key per execution. Losing the browser tab also lost the run, because no durable record persisted the id.

## Decision

- `run_id` is **server-minted** at `POST /api/runs`, **immutable**, and **unique per execution**.
- It is **never accepted from the client** and is **distinct** from the user-facing graph label.
- It is the lineage key threaded into child workflow ids, codec/idempotency provenance, and future Iceberg partition columns.
- A durable run record must persist it (losing the tab must not lose the run).
- The rule is recorded in the `run_id` comment in `run.proto`.

## Consequences

- A minted unique id is the precondition for the idempotency-key tuple `(run_id, node_id, generation, unit-index)` in `run.proto` field 5 — without it, at-least-once retries cannot dedup.
- The server must mint and durably persist the id, and the client must stop supplying one.
- The user-facing graph label remains a separate, mutable, human concept.
- Implemented in Phase 1. The lineage key (`run_id`/`tenant`/`idempotency_key`) is also threaded to the actor (see entry.proto and the SDK's `self.run_id` / `self.idempotency_key`); the full idempotency-key tuple's dedup of side effects is a later step.
