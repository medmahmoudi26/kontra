# 5. Claim-check codec = execution; output query = read-only side-channel (browser DuckDB now, Iceberg later)

## Status

Settled. The codec is **built and conformance-pinned**. The output-query side-channel is
**built as browser DuckDB-WASM** (`frontend/src/run/dataset.ts`). A durable
server-side **Iceberg** sink is **future** (P4, not built) — the same side-channel at scale.

## Context

Temporal workflow history has hard payload limits, but step I/O between actors can be large. We need durable, cheap data movement on the execution path. Separately, we want to query actor outputs (datasets). Conflating the two would put an analytics store on the critical execution path.

## Decision

- The byte-compatible **claim-check codec** (`runtime/python/internals/codec.py` <-> `backend/src/codec/claimCheck.ts`) is the data-movement layer on the **execution path**: payloads larger than the threshold (128 KiB) are offloaded to one S3 store (SeaweedFS locally, real S3 in cloud), content-addressed by sha256 at `cas/<sha[:2]>/<sha>`, and rehydrated on decode. Offload is `iff len(data) > threshold` (`<=` stays inline).
- **Output query is a read-only side-channel over outputs only** — never on the execution path, never replacing the codec. It is one idea at two tiers:
  - **Built today — browser DuckDB-WASM.** A completed run's per-node outputs (`GET /api/runs/:id/output`) are registered one-table-per-node and queried with real DuckDB SQL (joins, aggregates) entirely **client-side**: no server query endpoint, no native dependency, the WASM bundle lazy-loaded on first query. This is the Dataset page (`frontend/src/run/dataset.ts`, `panels/DatasetPage.tsx`; pure helpers in `datasetSql.ts`).
  - **Future (P4) — Iceberg/DuckDB server-side.** When client-side-over-JSON outgrows the browser (large outputs, cross-run analytics), an async sink off run-completion writes Parquet on a real catalog (Postgres, never the SeaweedFS filesystem), queried by DuckDB. Same side-channel role, durable and scalable. **Not built.**

## Consequences

- Workflow history stays under payload limits while large step I/O flows cheaply between steps via references.
- Content-addressing gives free dedup (identical payloads collapse to one object) and an integrity check on read.
- Analytics never sit on the critical path; a stale or unavailable query store (browser **or** Iceberg) cannot block execution.
- Querying outputs **already works** for normal runs via browser DuckDB — Iceberg is a scale-up, not a prerequisite for the datasets feature.
- The codec is the one cross-language wire contract, so its bytes are pinned by the conformance corpus (see ADR 0010); the codec marker and CAS layout cannot change casually.

Superseded in part by ADR 0017 (the completion-reporting implication of the P4 "async sink off
run-completion"; the critical-path invariant above is retained verbatim).
