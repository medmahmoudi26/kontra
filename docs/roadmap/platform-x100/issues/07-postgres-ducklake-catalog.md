# 07 — DuckLake catalog on shared Postgres

Status: ready-for-agent
**Tier:** 3 | **Effort:** M | **Depends on:** —
Supersedes: (old task) "FOLLOWUP: Postgres DuckLake catalog for dataset listing"

## Problem
DuckLake datasets materialize fine (data is on S3, `kontra monitor --query` proves it) but they
**don't list**: materialization (`linkDataset`→`writeDatasetParquet`) runs in the orchestrator
*worker* while listing (`listDatasets`) runs in the orchestrator *API*, and a single-process DuckDB
catalog can't be shared across processes. So `GET /api/datasets` is empty and the web UI Datasets
tab shows nothing.

## Native capability to use
DuckLake supports an external catalog database. A **Postgres is already running** on the control
plane (`safedeps-postgres`), so no new infra — point the DuckLake catalog at it and both processes
share one catalog.

## Approach
- Stand up (or reuse) a Postgres schema for the DuckLake catalog; configure DuckDB's DuckLake/`ATTACH`
  to use the Postgres catalog + S3 data path in both the orchestrator worker and API.
- Ensure the S3 data-path config matches what `writeDatasetParquet` writes.
- Backfill/verify existing materialized datasets appear in `listDatasets`.

## Files
- `control/orchestrator/src/data/parquet.ts` (+ wherever `listDatasets`/`linkDataset` live), orchestrator
  config/env, `docker-compose.yml` (Postgres reuse/creds via secrets — see issue 08)

## Verify (local, no fleet)
- Materialize a dataset from a completed run.
- `GET /api/datasets` returns it; the web UI **Datasets** tab lists it; `kontra monitor list` shows
  the stage.
- Restart the API process → dataset still lists (catalog is shared, not in-process).

## Risks
- DuckLake↔Postgres catalog version/compat; connection pooling from two processes.
- Keep the anonymous S3 read path intact so `kontra monitor` data queries are unaffected.

## Comments

### Implemented — live-integration prerequisites (2026-07-26)

Implemented on `feat/platform-x100-pg-catalog` (`067842b`), merged into
`feat/platform-x100-tier1`. Cross-process sharing is proven: a writer process registered 3
datasets through the Postgres catalog and a **separate** reader process listed all 3, while a
per-process local-file catalog over the same data dir listed 0 — i.e. the exact bug reproduced
and fixed. `tsc` + 30/30 data tests pass.

Three things must be done before the **live** end-to-end check (dispatch → `GET /api/datasets`
→ restart the API and confirm it still lists). Recording them here because they are not
derivable from the diff:

1. **Network bridge.** `safedeps-postgres` sits on the default `bridge` network bound to
   `127.0.0.1:5432`, while the orchestrators run on the `kontra` network — so
   `host=safedeps-postgres` does not resolve for them. Either
   `docker network connect kontra safedeps-postgres`, or add a dedicated catalog Postgres
   service on the `kontra` network.
2. **Password must come from the secret store, not compose.** Compose currently references
   `${KONTRA_DUCKLAKE_PG_PASSWORD}`; a plaintext value there is visible via `docker inspect`.
   Route it through the Dapr secret store added in
   [issue 08](08-secrets-and-placement.md) and `ALTER ROLE kontra PASSWORD …` to match. The
   role/db (`kontra` / `kontra_ducklake`) are provisioned but currently carry a **dev-only**
   password used for the local proof.
3. **DATA_PATH freezes at first ATTACH.** DuckLake pins the data path when the catalog is first
   attached, and `kontra_ducklake` was deliberately reset to pristine-empty after the proof. The
   first attach in the live control plane MUST therefore set the `s3://` DATA_PATH — do not let a
   stray local-path attach create it first, or datasets will resolve to the wrong location.

Also note: datasets materialized under the OLD per-process catalog are not registered in
Postgres (their parquet is still on S3, unreferenced). Either accept list-from-cutover or re-run
`linkDataset` for recent completed runs.
