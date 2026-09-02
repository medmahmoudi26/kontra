# 14. Dataset query engine: parquet datasets + direct browser range-reads

## Status

**Accepted — implemented.** Every completed node materializes a parquet dataset the design
UI's Dataset page and any native DuckDB (CLI / Local UI) can query. The original hand-rolled
Hive-path scheme is now backed by **DuckLake** (DuckDB's lakehouse format) — see the
"DuckLake refinement" note below — which owns the catalog, schema evolution, and snapshots
that the path-parsing scheme faked. The two load-bearing properties are unchanged: **S3→S3
materialization** (bytes never through Node) and **direct browser range-reads of plain
parquet** (DuckLake's data files ARE ordinary parquet, so DuckLake is not in the read path).
Proven live before the DuckLake refinement: a crawl4ai run's parquet range-read directly from
SeaweedFS by DuckDB-WASM (`206 Partial Content`, footer then row-group). The DuckLake path is
unit-verified locally (catalog, listing, per-run resolution, snapshots, schema isolation); the
httpfs S3→S3 read/write and browser range-read against live SeaweedFS are unchanged SQL/paths,
verified against a running stack as a follow-up. One deferred item — see Consequences (parquet
extension CDN fetch).

## Context

An actor's output lands in the content-addressed store (CAS) as a `{results, failures}`
JSON envelope at `cas/<sha>` — **un-queryable by `(actor, version, run)`**: the key is a
content hash, so nothing maps a run to its bytes except Temporal history. The goal was a
real query engine over run outputs, assuming **large** datasets (crawl outputs, etc.).

Two forces shaped the design:

- **Content-addressed ≠ query-addressable.** DuckDB needs a *predictable, globbable* path;
  a sha256 is the opposite. So a "dataset" is a new **run-addressed** artifact, not a view
  over the CAS blob.
- **Bulk data must not flow through the API.** An early version proxied JSON
  (`/api/datasets/output` → whole file → DuckDB-WASM). That transfers the entire output per
  query — fine for tiny runs, hopeless at scale.

## Decision

1. **Materialize each node's output as run-addressed parquet**, Hive-partitioned so the
   identity fields are free partition columns:

   ```
   datasets/actor=<name>/version=<version>/run_id=<runId>/<node>.parquet
   ```

   The interpreter's `linkDataset` activity runs **DuckDB embedded in the worker**
   (`@duckdb/node-api`) to convert the CAS JSON envelope → parquet **S3 → S3** — the bytes
   never pass through Node, so it scales with output size. `unnest(results)` gives one row
   per unit; nested fields stay STRUCT (parquet stores them natively). Best-effort: a
   conversion failure is logged, never fails the run.

2. **Discovery via the API; data via direct browser range-reads.**
   - `GET /api/datasets` lists datasets; `GET /api/datasets/urls` returns **presigned GET
     URLs** per node parquet (the server holds the creds).
   - The browser's DuckDB-WASM reads those URLs with `read_parquet` over the **HTTP range**
     protocol — it fetches only the parquet footer + the row-groups/columns a query touches,
     so a `LIMIT` over a huge parquet transfers a few KB. The API never sees the bulk.

3. **Split-horizon presigning + bucket CORS.** The API reaches SeaweedFS at `seaweed:8333`
   but the browser reaches `localhost:8333`; a SigV4 URL must be signed for the host the
   browser calls, so presigning uses `KONTRA_S3_PUBLIC_ENDPOINT`. A permissive bucket CORS
   rule (GET/HEAD, `Range` in, `Content-Range` exposed) is applied automatically so the
   browser can cross-origin range-read.

4. **Grain = `(actor, version, run_id)`.** Within an actor+version the output schema is
   stable; **across actors it differs**. Under the original scheme a glob spanning actors
   needed `union_by_name=true`; under DuckLake (below) each `(actor, version, node)` is its
   own table, so per-actor schema isolation is structural and the `union_by_name` hack is gone.

## DuckLake refinement (supersedes the path/glob mechanics of the Decision)

The Decision's *mechanism* — a hand-rolled `datasets/actor=…/version=…/run_id=…/<node>.parquet`
Hive layout, discovered by globbing S3 and string-parsing keys (`parseDatasetKey`), unified with
`union_by_name` — was replaced by **DuckLake** on the same embedded DuckDB. What DuckLake changes,
and what it deliberately does not:

- **Catalog, not path-parsing.** One DuckLake table per `(actor, version, node)`, partitioned by
  `run_id`; a small `_kontra_dataset` registry table holds identity. Listing QUERIES the catalog
  (registry + DuckLake partition/snapshot metadata) instead of globbing + parsing Hive keys, so the
  drift-prone `parseDatasetKey` and the cross-actor `union_by_name` glob are both deleted.
- **Schema evolution + snapshots for free** — DuckLake tracks the (possibly evolving) per-table
  schema across runs and versions snapshots, which the path scheme couldn't.
- **Both load-bearing properties preserved by construction.** The INSERT reads the CAS blob and
  writes data files through DuckDB's httpfs (**S3→S3**); `DATA_INLINING_ROW_LIMIT 0` keeps data in
  real parquet **data files** (not inlined into the catalog), so the browser still range-reads a
  presigned plain-parquet file — DuckLake is not in that path.
- **Local-first catalog.** The catalog metadata is a local DuckDB file (`KONTRA_DUCKLAKE_CATALOG`);
  data files live under `datasets/` on S3 (or a local `KONTRA_DUCKLAKE_DATA_PATH` for tests). See
  `control/orchestrator/src/data/parquet.ts` + `datasets.ts` and https://ducklake.select/docs.

## Consequences

- **Scales to large datasets** — only the touched bytes ever transfer, straight from object
  storage; the orchestrator API stays lightweight (lists + presigns, never proxies data).
- **Two readers, one artifact.** The browser (WASM, presigned range reads) and native DuckDB
  (CLI / Local UI, S3 secret) read the *same* parquet. Cross-run analytics that would presign
  thousands of URLs belong on the native side, globbing the Hive tree directly.
- **DuckDB-WASM fetches its parquet extension from `extensions.duckdb.org` at runtime**
  (the WASM *bundles* are self-hosted, but the extension is not). It works and caches, but is
  not offline-safe — self-hosting the extension is the follow-up for air-gapped use.
- **Integers display safely.** DuckDB returns 64-bit ints to JS as `BigInt`, which
  `JSON.stringify` can't serialize; the result grid deep-sanitizes `BigInt → Number` (nested
  in structs/lists too), so no actor's output crashes the grid.

## Alternatives considered

- **JSON via API proxy** (the interim implementation): simplest, no CORS, but pulls the whole
  file through the API on every query — rejected for scale.
- **Parquet written by a Node parquet library** (`parquet-wasm` / `@dsnp/parquetjs`): would
  avoid a DuckDB dep, but hand-building Arrow schemas for heterogeneous nested JSON is
  brittle; DuckDB infers + compresses + writes S3→S3 in one statement.
- **Browser reads S3 with embedded credentials**: rejected — presigned URLs keep creds out of
  the browser and are the cloud-correct path (short-TTL, scoped to one object).

Superseded in part by ADR 0017 (the "Best-effort: a conversion failure is logged, never fails the
run" clause of Decision §1).
