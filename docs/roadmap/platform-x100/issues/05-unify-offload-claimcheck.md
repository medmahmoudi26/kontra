# 05 — Unify offload onto the claim-check codec

Status: needs-triage
**Tier:** 2 | **Effort:** L | **Depends on:** 01 (shared codec extraction helps)

## Problem
There are **two** overlapping offload mechanisms that fight each other:
1. the Temporal claim-check **codec** (`handler/internal/codec`, `backend/src/codec/claimCheck.ts`)
   at the Temporal-payload layer, and
2. the orchestrator's own blob-threshold offload in `backend/src/data/parquet.ts`
   (`DEFAULT_BLOB_THRESHOLD`, `KONTRA_S3_THRESHOLD`) plus the raised Temporal blob limits in
   `docker-compose.yml` and the `!rootOffloaded` gate in `interpreter.ts` that **disables streaming
   when the root is offloaded**.

This is the source of the tuning cliffs we fought (blob-size limits, `MAX_INFLIGHT_CHUNKS`,
offloaded-root disabling distribution).

## Native capability to use
The Temporal DataConverter/codec is the sanctioned single place to transform payloads. Make it the
**one** claim-check authority and teach streaming to operate over a ref without inflating it.

## Approach
- Audit the two paths; define one claim-check contract (the codec's `$ref`) used end-to-end.
- Make the interpreter's streaming **claim-check-aware**: chunk over an offloaded root by reading
  the ref's manifest from S3 (already anonymous-readable) instead of pulling the whole blob into a
  Temporal payload — remove the `!rootOffloaded` "disable streaming" gate.
- Once the codec owns offload, drop the raised Temporal blob-size limits and the parallel
  `parquet.ts` threshold (or reduce it to the codec's threshold).

## Files
- `backend/src/workflows/interpreter.ts`, `backend/src/data/parquet.ts`,
  `backend/src/codec/claimCheck.ts`, `handler/internal/codec/codec.go`, `docker-compose.yml`

## Verify (local, no fleet)
- A run with a >128KB root input **streams** (chunks distribute) instead of falling back.
- Remove the 16MB Temporal blob-limit overrides and confirm large runs still succeed via refs.
- Codec conformance test still passes; Go↔TS ref shape stays congruent.

## Risks
- **Highest-risk item** — touches the working streaming/distribution path. Land behind a flag; keep
  the old path until the new one is proven on a real crawl→bust run.

## Comments
Left as `needs-triage`: sequence this **after** Tier 1 lands and is verified, so we refactor the
offload path with tracing + UI payload visibility already in place to debug it.
