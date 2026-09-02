# Platform x100 — lean on Temporal & Dapr instead of re-building them

**Status:** ready-for-agent
**Author:** offensive-security eng (via Claude Code)
**Date:** 2026-07-26

> Convention note: the repo's issue tracker normally lives in `.scratch/` (gitignored,
> local-only — see `docs/agents/issue-tracker.md`). This roadmap is deliberately placed under
> `docs/roadmap/` **because it must be reviewable as a PR**. The `issues/` here follow the same
> `NN-<slug>.md` + `Status:` convention; they double as the execution tracker.

## Thesis

Kontra's genuine, differentiated value is its **data plane**: actors produce recon data →
streamed as sub-unit blobs to S3 → queried with SQL (DuckDB-over-S3). That part is good and stays
bespoke.

But kontra has quietly grown a **parallel control/observability plane** that Temporal and Dapr
already provide — *while under-using or mis-configuring the platform primitives it depends on*. The
highest-leverage work is not new features; it is **deleting the parallel plane and leaning on the
sidecar + a codec server + Temporal visibility/tracing**, and fixing two latent correctness bugs in
how Dapr is wired. That concentrates our effort where we are actually differentiated.

## The parallel-systems problem

| kontra built… | …but the platform already gives us | verdict |
|---|---|---|
| `kontra monitor --state` + `repo.listRuns` over **SQLite** | Temporal **Visibility API** + Search Attributes + Web UI (:8233) | duplicated, drifts → [04](issues/04-visibility-backed-monitor.md) |
| custom progress plumbing (host→orchestrator→CLI, S3 blob-counts) | Temporal **activity heartbeats** (live progress in the UI, queryable) | reinvented → [06](issues/06-heartbeat-progress.md) |
| `parquet.ts` blob-threshold + raised Temporal blob limits + "offloaded-root-disables-streaming" | the **claim-check codec we already ship** | two offloaders fighting → [05](issues/05-unify-offload-claimcheck.md) |
| per-worker Redis, per-worker placement | Dapr **shared state store** + central/optional placement | mis-configured (correctness bug) → [02](issues/02-shared-state-store.md) |
| offloaded payloads are opaque in the Temporal UI | Temporal **remote codec server** (decode endpoint the UI calls) | missing 20% → [01](issues/01-codec-server.md) |
| creds in env; no cross-fleet debugging | Dapr **Secrets API**; Dapr+Temporal **OTel tracing** | missing → [03](issues/03-otel-tracing.md), [08](issues/08-secrets-and-placement.md) |
| dataset listing via single-process DuckDB catalog | a shared **Postgres** catalog (one is already running) | half-built → [07](issues/07-postgres-ducklake-catalog.md) |

## What is already solid (do not touch)

- **The claim-check codec is real and cross-SDK** — `runtime/handler/internal/codec/codec.go` +
  `control/orchestrator/src/codec/claimCheck.ts`, wired into the Temporal `DataConverter`
  (`runtime/handler/main.go:46`), with a conformance test. This is the correct pattern; items 01/05 build
  *on* it, they do not replace it.
- **Self-contained worker image**, **streaming sub-unit blobs with `$ref` commits**, and
  **DuckDB-over-S3** for data queries. Keep all of it.

## Two latent correctness bugs (not just cleanups)

1. **`global_state` is not global.** `infra/dapr/statestore.yaml` hard-codes
   `redisHost: localhost:6379` and every worker container starts its own `redis-server`
   (`infra/worker-entrypoint.sh:23`). So `kontra-global:<actor>:*` is **per-worker** — any actor
   using `global_state` for a dedup set, crawl cursor, or rate-limit budget is silently
   inconsistent across the fleet. → [02](issues/02-shared-state-store.md)
2. **No trace across the fleet.** A `500` from an actor (e.g. the crawl4ai `RunBatch` failures)
   surfaces as a stack-less log line you have to SSH-tail on the right droplet. There is zero
   distributed tracing (`config.yaml` only enables `ActorStateTTL`). → [03](issues/03-otel-tracing.md)

## Ranked items

**Tier 1 — config-level fixes to mis-used primitives, outsized payoff:**
- [01 — Temporal codec server](issues/01-codec-server.md) — free payload visibility in the Temporal UI.
- [02 — Shared Dapr state store](issues/02-shared-state-store.md) — fixes the `global_state` bug + central observability.
- [03 — OTel tracing](issues/03-otel-tracing.md) — one trace, dispatch → actor → S3.

**Tier 2 — structural, higher effort:**
- [04 — Visibility-backed monitor](issues/04-visibility-backed-monitor.md) — Temporal Visibility instead of a parallel run table.
- [05 — Unify offload onto the codec](issues/05-unify-offload-claimcheck.md) — one claim-check authority; remove the tuning cliffs.
- [06 — Heartbeat progress](issues/06-heartbeat-progress.md) — live progress from Temporal, not S3 globbing.

**Tier 3 — cheap wins / hygiene:**
- [07 — Postgres DuckLake catalog](issues/07-postgres-ducklake-catalog.md) — datasets finally list (reuse the running Postgres).
- [08 — Dapr Secrets + placement decision](issues/08-secrets-and-placement.md) — creds out of env; deliberate placement stance.

## Suggested sequencing

Tier 1 (01 → 02 → 03) is ~three days of mostly-config that together deliver the visibility we keep
asking for **and** fix a real correctness bug. Then 04 (visibility-backed monitor) and 05 (offload
unification) are the deeper cleanups; 06/07/08 slot in around them.

## Principle to hold going forward

**Whenever Temporal or Dapr already exposes a capability or endpoint — the sidecar, the codec
server, Visibility, Search Attributes, heartbeats, secrets, tracing — use it before building our
own.** Bespoke code is reserved for the recon *data* plane, where kontra is actually
differentiated.
