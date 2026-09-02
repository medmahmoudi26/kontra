# Data Plane — codec & datasets

All bulk data lives in one S3-compatible object store (SeaweedFS locally, S3 in cloud; `KONTRA_S3_ENDPOINT`). Three live planes ride it: the claim-check codec (§1), per-unit blobs (§2), and datasets (§4). The durable state tiers (`unit_state` / `object_state` / `global_state`) live in Redis, not here (§3).

## 1. The claim-check codec (implicit, whole-payload)

Authors return **plain JSON** from steps. A Temporal `PayloadCodec` (`ClaimCheckCodec`) offloads any payload over a threshold (default 128 KB) to the store and replaces it in workflow history with a tiny reference; decode rehydrates it transparently for the next step. This is the idiomatic Temporal external-storage pattern — the blessed path, not a fallback.

- **Content-addressed**: objects live at `cas/<sha256[:2]>/<sha256>`; the ref carries `{sha256, size, meta}`. Identical bytes dedup; the digest is an integrity check on read.
- **Cross-run dedup is clean**: the per-step workflow payload carries no run-scoped field, so re-running the identical batch adds **0** new objects. Repeated test runs don't grow the store.
- **No store configured?** The codec is a pure passthrough — base Kontra needs no object store.
- The TS orchestrator's codec port is **byte-compatible** (marker `binary/claim-check-v1`), held there by the codec conformance corpus ([[Contracts]]).

A large step payload is offloaded transparently and a re-run adds 0 new objects. Browse objects at http://localhost:8888/buckets/kontra/.

## 2. Per-unit blobs (`units/` — the emit plane)

With `KONTRA_S3_ENDPOINT` set on the **actor process**, each emitted record is written to the object store **at emit time**, and the durable commit in Redis holds only `{"$ref": {key, size, sha256}}` — blob write **first**, then the ref commit. Redis never holds payloads, and a retry replays the refs verbatim with no S3 reads.

- **Key layout — hive-partitioned**: `units/run={run}/dt={date}/actor={actor}/shard={n}/unit={i}/{sha}.json`. Every segment is `key=value`, so DuckDB's `hive_partitioning=true` exposes run/dt/actor/shard/unit as typed **columns** and a `WHERE dt = …` prunes at the file level. `run=` leads for a measured reason, not a stylistic one: an object store prunes a LIST only by literal prefix, and with `dt=` first, locating one run means listing the whole bucket — **8.4s against 0.17s** on a 188k-object bucket, a gap that widens with every object ever written. `dt` is the RUN's date, threaded down from the handler rather than read from the worker's clock, so a run crossing midnight stays in one partition instead of splitting and disagreeing between hosts.
- **One golden fixture**: the two SDKs must produce the **same key for the same inputs** or a reader sees two layouts. They have drifted once already (isolation counters shipped Go-only), so both are pinned to `conformance/blobkey.json`, asserted by each SDK's own suite.
- **1 → N is just emitting twice**: a Unit that fans out commits one blob per `emit`, each named by the sha of its canonical record — so a re-emit on a resume is an idempotent **overwrite**, which is why records must be content-deterministic (no timestamps, no random ids). The Unit's done-marker (holding those refs) lands only after the Method finishes with that Unit, so a death mid-Unit re-runs it cleanly. See [[Durability-and-Failures]].
- **Producer**: Python `runtime/python/internals/unitstore.py` (boto3), Go `runtime/go/unitstore` (aws-sdk-go-v2, path-style). Both honor the same `KONTRA_S3_*` contract as the Go/TS codecs. The handler threads `run_id`/`node_id`/`run_date` in the `RunBatch` payload (`handler/activity.go` `RunBatchInput`).
- **Consumer**: the actor side resolves `$ref` entries on ingest (bounded at 16 concurrent fetches, `_resolve_refs` in `internals/engine.py`), so a Method's author never sees a ref and one Actor's output feeds the next directly. The orchestrator resolves them in `backend/src/data/parquet.ts` — sha256-verified, spliced back in input order — so materialization, the output endpoint and datasets all see plain units.
- **Inline fallback**: store unset ⇒ inline commits (dev/test mode). A Batch's records may **mix** inline units and `$ref` entries.
- **The consumer's cursor is a blob too** — `cursors/{run_id}/{node_id}.json`, holding `{lastSeq, consumed, pending}` for one streaming node. It lives here and not in the `pollParentUnits` activity input because Temporal keeps every activity input in workflow history **permanently**: the consumed set used to ride along, making history quadratic in blob count, and the history service terminated a real 6,190-seed crawl at ~2,500 blobs. The workflow now sends only the cursor key plus its own poll counter, and the counter is what distinguishes "the workflow got the last batch" from "that result was lost, hand it over again" — so a retry re-offers a chunk rather than dropping it.

## 3. The state tiers — live, in Redis (not S3)

Durable author state lives in **Redis**, not S3. The tiers all share **one keyed `get(key)`/`set(key, value)`/`delete(key)` shape** and differing only in scope; each has a frozen key scheme ([ADR 0015](../adr/legacy/0015-three-tier-state-and-naming.md)) that ADR 0018's runtime change deliberately left alone: same keys, same Redis, same TTLs. See [[Writing-Actors-Python]] / [[Writing-Actors-Go]] for the author surface.

| Tier | Scope | Redis key | Cleared on commit? | Backing |
|---|---|---|---|---|
| `unit_state` | one Unit | field `{slot}-ckpt` — **one blob holds all a Unit's keys** | **yes** (whole blob) | the per-actor hash |
| `object_state` | cross-Session, dispatch-**key**-scoped | the per-actor hash of the keyed actor id | no | the per-actor hash |
| `global_state` | cross-Session, actor-**name**-scoped | `kontra-global:{actor}:{key}` — one hash **per key** | no | `RedisEtagKV` / `rediskv.EtagKV` |

`self.*` is the fourth thing an author reaches for and is deliberately **not** in this table: it is in-memory, survives a re-entry, and dies with the Session ([ADR 0023](../adr/0023-v2-one-kind-sessions-caller-owned-loop.md) §7, §13). The cross-call durable tier that used to sit here, `session_state`, is retired — within a Session `self.*` already survives, and across Sessions nothing should (§19).

`unit_state` and `object_state` share **one Redis hash per actor id** — `kontra-actor:{actor_id}`, one field per state key, 24 h TTL (`internals/statekv.py`, `runtime/go/statekv`). A hash rather than a key per field because the TTL is what makes it safe to leave state behind and `EXPIRE` applies to the whole hash: one call slides an actor's entire state forward, instead of re-writing every live session key just to move a clock. It is also what `backend/src/state.ts` reads to project the operator-facing state view: one `HGETALL` per actor id.

- **`unit_state`** is per-Unit **resume scratch, not a result store** — keyed `get`/`set`/`delete`, so a Unit can hold several independent slots (a frontier, a cursor). All of a Unit's keys live in **one** blob at `{slot}-ckpt` beside the Unit's commit, and the framework **deletes the whole blob** when the Unit commits (so `delete()` is rarely needed by hand). Its only reader is a Unit that was in flight when the activity died and re-runs on the handler's retry — a committed Unit is skipped by the commit map, an isolated one is never resumed. Output rides the emit plane (§2), never here. `set()` is at-least-once, so snapshots must tolerate replay. It deliberately stays in Redis rather than riding Temporal heartbeat details: it is author-controlled opaque JSON, `RecordHeartbeat` *panics* rather than degrading when a payload will not encode, and one oversized frontier would kill the activity ([legacy ADR 0018](../adr/legacy/0018-temporal-native-actor-runtime.md) §4).
- **`object_state`** is what makes a **keyed** Actor a virtual object: the dispatch key becomes the actor id, so the same key meets the same hash next dispatch, next week. Because a keyed dispatch is exclusive per key (the backing workflow id carries the key), two Batches for one key cannot race. That longevity is a trap of its own — a genuine re-run whose Method opens by checking a keyed dedupe set will correctly do nothing ([legacy ADR 0022](../adr/legacy/0022-object-state-and-keyed-dispatch.md)).
- **`global_state`** is actor-**name**-scoped (a version bump shares it), keyed `kontra-global:{actor}:{key}` — one small hash per key, fields `data` and `ver`, with **no TTL**. Redis has no native ETag, so `ver` supplies one: every write bumps it, including the unconditional last-write-wins `set`, so a CAS holder that missed one correctly loses its race. The compare-and-set is a single Lua script (`internals/redis_kv.py`, `runtime/go/rediskv`) — byte-identical in both SDKs — because a read-then-write from the client is exactly the lost-update race the atomics exist to prevent. Its atomic ops (`add_to_set`/`incr`/`compare_and_set`) are optimistic-CAS on that etag, so concurrent sessions never lose an update; naive `get`/`set` is last-write-wins.

**Redis is required, and it is the Controller's.** `KONTRA_REDIS_HOST` (`host:port`) points every actor at the one shared store — the `redis` service in `docker-compose.yml`, `redis:6379` on the `kontra` network, and the Controller's address in a fleet Machine's `/etc/kontra/worker.env`. Workers used to bundle their own, which silently made the *cross-session, fleet-wide* tier per-container. There is no bundled fallback left to hide behind. Eviction is `volatile-lru` on purpose: the per-actor hash carries a TTL and may be evicted under pressure, but `global_state` has none — evicting a dedupe set would silently corrupt correctness.

## 4. Datasets (the SQL surface — what you load in, and what a run produced)

A node's output is **materialized into a DuckLake table** (DuckDB's lakehouse format), so results are queryable with SQL without touching Temporal history. **One table per ACTOR**, named after the actor, in the `output` schema, partitioned by `(version, dt)`. `DATA_PATH` is the **bucket root**, so DuckLake's own `<schema>/<table>/` layout *is* the object layout:

```
output/<actor>/version=<v>/dt=<YYYY-MM-DDTHH-MM-SS>/…parquet   # what an actor produced
standalone/<name>/…parquet                                     # a list you loaded (`kontra dataset create`)
```

`dt` is the **dispatch time to the second**, from the server-minted `run_started_at`, so one partition directory holds exactly one dispatch — and a dispatch sharded across `n1..nK` is many graph nodes but **ONE dataset**. That replaces `ds_<sha1(actor,version,node)>` table names, the `_kontra_dataset` registry whose only job was translating those hashes back, and the `datasets/` middle segment that held them: nothing parses a path and nothing needs a lookup table to answer "whose output is this?". DuckLake owns the catalog, per-table schema evolution and snapshots — replacing the earlier hand-rolled Hive-path scheme that faked all three by string-parsing S3 keys (ADR 0014). The catalog is a local DuckDB file by default (`KONTRA_DUCKLAKE_CATALOG`, or a `postgres:…` connstring to share one catalog across processes).

**Rows are typed.** Every unit is claim-checked regardless of size, so the old writer — which materialized the output *envelope* — produced tables with exactly two columns, `run_id` and `$ref`: `SELECT host, status_code` could not be written at all. The materializer now decodes those refs into the actor's real columns, nested output preserved as `STRUCT`/`LIST`, plus the identity columns `version`, `dt`, `node`, `run_id` (all `VARCHAR`) and a server-minted `run_started_at TIMESTAMPTZ` (ADR 0017). `version` and `dt` are the partition columns — filtering on them prunes directories rather than scanning rows. Pointer-layout output written before this (schema version 1) is not part of this layout and is never rewritten — there is no backfill.

The decode is bounded and verified (`backend/src/data/parquet.ts`): read the manifest and check its sha256 **in SQL**, page its unit refs into Node (refs only — ~110 bytes each, no payload), then per batch `read_blob` the objects and verify **every** carried sha256 before `read_json` parses them. A missing object reads as zero rows in `read_blob`, which is exactly the shape of "absent output looks empty" — so it is detected and raised, not skipped. One transaction per node: every batch lands or none does. `DATA_INLINING_ROW_LIMIT 0` keeps data in real parquet files rather than inlined into the catalog, so a presigned data file is a complete readable object on its own.

**Materialization is gated, not best-effort.** It runs as the `materializeNode` activity on its own Temporal task queue (`kontra-materializer`), served by an isolated off-controller worker ([[Deployment]]); the interpreter does not poll that queue, so the placement is enforced by routing. The old `linkDataset` activity swallowed every error in a bare `catch {}` with no log line, which made a failed materialization indistinguishable from an empty result. Failures are now recorded and surfaced.

That gives a run **two status dimensions**, never merged in storage:

| Dimension | Authority | Values |
|---|---|---|
| Execution — did the actor graph finish? | Temporal (`RunStatus`, unchanged) | `pending` · `running` · `completed` · `failed` · `cancelled` |
| Materialization — is the typed output queryable? | an application-owned `materialization_node` table, keyed `(run_id, actor, version, node, materialization_schema_version)` — Postgres in production, SQLite single-host (`KONTRA_MATERIALIZATION_DB`) | `pending` · `running` · `complete` · `failed` |

`GET /api/runs/:runId/lifecycle` returns both, plus a derived projection `executing | finalizing | completed | output_failed`. The projection is a separate field, **never** a new `RunStatus` member. A materialization failure still never fails the run (ADR 0005 — analytics off the critical path); it surfaces as `output_failed`. Row count zero with `complete` is a **successful empty result**, not a failure — see [[Query-Surface]] for why that distinction is the point.

**Querying it** — `kontra dataset query` (and the web **Query** workbench) run on the orchestrator's open connection; `kontra explore` runs DuckDB on the operator's workstation over presigned files. Full surface in [[Query-Surface]]:

```sh
kontra dataset list                                      # standalone (loaded) + output (produced)
kontra dataset query crawl4ai --dt 2026-08-03 \
  --sql "SELECT host, status_code FROM crawl4ai LIMIT 20"
kontra explore crawl4ai --dt 2026-08-03T17               # one dispatch, presigned URLs, a DuckDB shell
```

`kontra explore` addresses a dispatch by **actor + version + dt** (never a run UUID), creates one view **per actor**, and lands you on a `kontra_datasets` table that shows a failed dataset as a failure instead of as an empty one. `kontra dataset query` resolves a bare name to a view over the whole table and folds `--version`/`--dt` into its `WHERE` so they prune partitions. Datasets are also the **input** currency: the rows a query returns can be dispatched verbatim as units (`--input <dataset> --query "…"`, [[Query-Surface]] §4).

**In the UI** — the **Datasets** page (http://localhost:8088) lists every dataset from `GET /api/datasets`, actor output and standalone lists side by side. That listing reads **catalog metadata only**: row counts and byte sizes come from `ducklake_data_file` and freshness from the committing snapshot, so it costs a handful of Postgres rows no matter how much output exists — the same figures Grafana's `kontra_runs` view reports. It opens no data file, and it names no graph node: a five-shard dispatch of one actor is one dataset.

Selecting one calls `GET /api/datasets/:name/preview`, a bounded read (500 rows, clamped to 5000) through the orchestrator's own attached connection, returned as JSON. `version` and `dt` are partition columns, so a scoped preview prunes to one dispatch's directory. The name is checked against the catalog rather than escaped — an identifier cannot be parameterised, so the only safe one is a name the catalog already lists.

The browser used to do this itself: presign every parquet file and range-read them with DuckDB-WASM. It worked, but it shipped **76 MB of WebAssembly** (`duckdb-mvp.wasm` 40 MB + `duckdb-eh.wasm` 36 MB) uncompressed and re-fetched per visit, and it needed a presigning token baked into the bundle. Neither is worth it for a capped preview, and the deep path was always `kontra dataset query` anyway. Presigning still exists where it belongs — `kontra explore`, which hands an operator's own DuckDB short-lived URLs for one dispatch's files with no credential on the workstation.

**Cross-dispatch, from a shell** — the advanced path, when you want native DuckDB rather than the CLI. `kontra explore --catalog` generates this shape against the shared catalog from `KONTRA_CATALOG_PG`; the `ATTACH` is `READ_ONLY` and wants **separately provisioned read-only credentials**, never the orchestrator's writer DSN. `DATA_PATH` must match what the catalog recorded (the bucket root) or DuckLake refuses to attach:

```sql
INSTALL ducklake; LOAD ducklake; INSTALL postgres; LOAD postgres; INSTALL httpfs; LOAD httpfs;
SET s3_endpoint='localhost:8333'; SET s3_url_style='path'; SET s3_use_ssl=false;
SET s3_access_key_id='kontra'; SET s3_secret_access_key='kontra'; SET s3_region='us-east-1';

ATTACH 'ducklake:postgres:dbname=kontra_ducklake host=localhost user=kontra' AS lake
  (DATA_PATH 's3://kontra/', READ_ONLY);
SHOW ALL TABLES;                       -- schema `output` = actors, `standalone` = the lists you loaded
-- identity IS the layout: the table is the actor, version and dt are the partitions
SELECT * FROM lake.output."crawl4ai" WHERE version = '1.0.0' AND dt LIKE '2026-08-03T17%';
```

A `WHERE` here is a query predicate, not an authorization boundary — which is exactly why exact-dispatch explore, whose scope is physical (one `version=…/dt=…` directory, one set of presigned objects), is the default ([[Query-Surface]] §8).

## Configuration

| Env var | Purpose | Default |
|---|---|---|
| `KONTRA_REDIS_HOST` | `host:port` of the shared state store holding all three tiers (§3) — the Controller's | `localhost:6379` |
| `KONTRA_S3_ENDPOINT` | the object store; unset ⇒ codec passthrough | _(unset)_ |
| `KONTRA_S3_BUCKET` / `KONTRA_S3_PREFIX` | where objects land | `kontra` / — |
| `KONTRA_S3_THRESHOLD` | offload threshold (bytes) | `131072` |
| `KONTRA_S3_REGION` / `KONTRA_S3_ACCESS_KEY` / `KONTRA_S3_SECRET_KEY` | credentials | local dev values |
| `KONTRA_S3_PUBLIC_ENDPOINT` | browser-facing endpoint for presigned URLs | = endpoint |
| `KONTRA_DUCKLAKE_CATALOG` | DuckLake catalog; local file, or `postgres:…` to share one catalog | `orchestrator-datasets.ducklake` |
| `KONTRA_DUCKLAKE_DATA_PATH` | lake `DATA_PATH` — the **bucket root**, under which `output/` and `standalone/` are written; used when there is no S3 endpoint (tests/local) | `s3://<bucket>/<prefix>/` with a store, else `./` |
| `KONTRA_MATERIALIZATION_DB` | materialization status ledger + summary tables | `KONTRA_ORCHESTRATOR_DB`, else `orchestrator.db` |

The materializer's own knobs (queue, slots, DuckDB memory/threads/spill, batch size) are in [[Deployment]].

## Retention & garbage collection

The prefixes have **different** safe reclamation rules:

| Prefix | What it holds | How it is reclaimed |
|---|---|---|
| `cas/` | claim-check objects, content-addressed | mark-and-sweep by reachability — **never** by age |
| `units/` | raw per-unit output blobs + cursors, run-addressed | expiry by age, 90 days (`sweepUnits`) |
| `output/` | DuckLake data files for actor output | **untagged**: TTL sweep, 24h from last write (`sweepDatasets`, ADR 0029). **Tagged**: kept. Compaction/snapshot expiry still happen through the catalog |
| `standalone/` | DuckLake data files for the lists you loaded | operator-owned: `kontra dataset delete <name>`, never a lifecycle rule |
| `history/` | one run's reduced Temporal event log, kept after retention drops the execution (ADR 0025) | **kept** — nothing reclaims it, by decision |

### `output/` — the Dataset TTL, and the two things that can delete a Dataset

[ADR 0029](../adr/0029-live-datasets-name-tag-retention.md) §3. Untagged actor output expires on a
TTL **this repo owns** (default 24h), deliberately *not* read from the namespace config; tagged
output is kept. The clock starts at **last write**, so a run that appends for thirty hours does not
have its earliest chunks collected while it is still writing to them, and collection is a sweep with
a **grace window** rather than a delete at exactly T+TTL — tagging is frequently retroactive, and the
sweep must not race the tag.

**Two things can delete a Dataset, and they must agree.** The sweeper is one; the explicit
`deleteTemporaryDataset` verb (ADR 0028) is the other. A **temporary** Dataset is untagged by
construction, has an owner and a deletion verb, and is therefore *never* swept — `classifySweep`
keeps every temp whatever its age or tag. A tag on a temp keeps the same Run's **durable** output,
because the record is Run-grain; it does not extend the temp, make it durable, or block its delete.

The sweeper reads the **Dataset record**, never the `KontraTag` search attribute — one authority, so
the projection can lag without changing what is collected. See [[Query-Surface]] §2a for the operator
view and the `tag`/`rename` verbs.

**A tag is therefore a privilege, and the routes that write it answer to `KONTRA_RUN_TOKEN`** — the
same credential as `serve`/`start`. Untagging is not a metadata edit; it hands a Dataset to the next
sweep. Open when no token is configured, like the rest of the Run surface.

> **Armed at boot, previewing by default.** `orchestrator-infra` registers the Schedule
> (`kontra-dataset-retention`, hourly, overlap `SKIP`) on every start, idempotently by its id — that
> process and not the API, because it is the one hosting `sweepDatasetsWorkflow`. Every firing
> **previews**: it reports what it would collect and deletes nothing until
> `KONTRA_RETENTION_COLLECT=1` is set on the worker holding the lake (the materializer, which serves
> `DATASET_QUEUE`). Steer it with `kontra schedule`; see what a firing would do, in full, at
> `GET /api/datasets/retention/preview`. The scheduled tick itself returns **counts plus a bounded
> sample**, never the whole catalog — an activity result is written into workflow history, and this
> repo has measured that wall.

### `history/` — kept, and deliberately outside every sweep

`history/run=<id>/dt=<start>/log.json` is the payload-free event log the console renders, written by the orchestrator's sweep once a run closes, because Temporal's retention (**measured 24h** on the local `default` namespace) drops the execution long before the Datasets it wrote expire. It is capped by construction — 1,000 events, HEAD + TAIL, an explicit `elided` count — so a run's whole story is **~175 KB at worst** (measured: 53,011 bytes for the 305-event `nscheck-1786831339` run). A **Dataset** outlives its **Run**; this is what lets its story do the same. `sweepUnits` walks `units/` only, so nothing here is reclaimed by age; deleting the prefix is a deliberate act, not a lifecycle rule.

### `cas/` — mark-and-sweep only, NEVER lifecycle-by-age

CAS keys are pure content hashes (`cas/<sha[:2]>/<sha>`) with **no tenant or run component**, so dedup is cross-run and cross-tenant (ADR 0001, ADR 0007). That is the valuable property — but it means **age tells you nothing about liveness**: a blob written months ago can be the dedup target of a run started this second. An age-based lifecycle rule on `cas/` *will* delete objects that live runs still reference. **Never point a lifecycle rule at `cas/`.**

The safe reclaimer is mark-and-sweep (the `internals/gc.py` CLI was removed in the restructure; the design below stands, pending a reworked tool):

- **Mark** = the set of reachable CAS sha256s, gathered from the durable **run records** (the orchestrator's run store). The mark phase is an **input** to the sweeper — produce a newline-delimited list of live shas and feed it in. This keeps liveness where the truth lives and the sweeper store-agnostic.
- **Sweep** = enumerate `cas/`, delete every object whose sha is absent from the live set. **Dry-run by default** (reports what *would* go); a `--delete` flag actually removes.

> **Correctness note:** run the mark against a *quiesced* view (or subtract in-flight runs), so a run that just wrote a blob but hasn't recorded its ref yet isn't swept out from under itself.

### `units/` — age-safe, 90 days

Run-addressed, not content-shared: `units/run={run}/…` is a per-run emit output and `cursors/{run_id}/{node_id}.json` is a per-run streaming cursor. An object here is dead once its run is old enough, so **expiration by age is the right tool** — no reachability scan needed. Scope every rule to the prefix; do not use a bucket-wide rule (it would catch `cas/`). (`unit_state` resume scratch never lands on S3 — it lives in Redis, §3.)

`sweepUnits` (`backend/src/data/maintenance.ts`) is the in-repo sweeper: default retention **90 days**, `keepRuns` to pin runs that must survive, and **dry-run by default** — a sweep that deletes on its first invocation is one typo away from removing a run's raw evidence, and the report is what makes the blast radius inspectable first. An object whose backing store reports no mtime is **retained**, never guessed at. It walks `units/` only: `cas/` is never touched by age, whatever the retention is set to.

MinIO (`mc ilm rule add`, [MinIO docs](https://docs.min.io/aistor/reference/cli/mc-ilm-rule/mc-ilm-rule-add/)) is the equivalent if you would rather the object store do it:

```bash
# expire per-unit output blobs 90 days after creation
mc ilm rule add myminio/kontra --prefix "units/" --expire-days 90
```

### `output/` and `standalone/` — through the catalog, never by age

These hold DuckLake's parquet **data files**, and the catalog is what knows which are live. Deleting them out of band removes files the catalog still references, which turns a readable lake into one that errors on every query touching a missing file — so a lifecycle rule on these prefixes is **not** safe, even though the paths look dispatch-addressed. `backend/src/data/maintenance.ts` owns the three passes:

- **`compactTable`** — merges small files **within** each `version=…/dt=…` partition, target 128 MB. Bounded per pass (`maxFiles`, default 10) and it re-checks after every pass that every data file still carries both partition values. That check is load-bearing: a compaction that merged two dispatches into one file would silently turn every exact-dispatch presigned URL into a cross-dispatch disclosure ([[Query-Surface]] §8). **Never merge across dispatches.**
- **`expireSnapshots`** — `ducklake_expire_snapshots` then `ducklake_cleanup_old_files`, in that order, always. Expiry only *schedules* files for deletion; cleanup is what removes them, and cleanup without expiry deletes nothing.
- **`sweepUnits`** — the `units/` pass above.

Equivalent S3 lifecycle JSON (`aws s3api put-bucket-lifecycle-configuration --bucket kontra --lifecycle-configuration file://lifecycle.json`, [AWS PutBucketLifecycleConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketLifecycleConfiguration.html)):

```json
{
  "Rules": [
    {
      "ID": "expire-units",
      "Status": "Enabled",
      "Filter": { "Prefix": "units/" },
      "Expiration": { "Days": 90 }
    }
  ]
}
```

> If a `KONTRA_S3_PREFIX` is set, prepend it to the `Prefix` value above (e.g. `demo/checkpoints/`). There is deliberately **no rule for `cas/`** (swept by reachability, never by age) and **none for `output/` or `standalone/`** (reclaimed through the DuckLake catalog).
