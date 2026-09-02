/**
 * Typed materialization of a node's output into **DuckLake** (ADR 0014, ADR 0017).
 *
 * WHAT CHANGED, AND WHY IT MATTERS
 *
 * The previous writer materialized the node's output ENVELOPE. Because every unit is
 * claim-checked regardless of size, that envelope is a list of `$ref` pointers, so every
 * table held exactly two columns — `run_id` and `$ref` — and `SELECT host, status_code`
 * could not be written at all. The table existed, the run said `completed`, and the
 * output was not there. This module now decodes those refs into TYPED ROWS.
 *
 * THE PIPELINE, and where the bytes are at each step:
 *
 *   1. Read the outer manifest by ref and VERIFY its sha256 — in SQL, so the bytes never
 *      enter Node. A manifest that is missing reads as zero rows in `read_blob`, which is
 *      indistinguishable from an empty one; that case is detected and raised, not skipped.
 *   2. Page the manifest's unit refs (`{key, sha256}`) into Node. Refs only — a ref is
 *      ~110 bytes and carries no payload, which is what keeps this bounded.
 *   3. For each bounded batch of refs: `read_blob` the objects and verify EVERY carried
 *      sha256 in SQL. A mismatch or a missing object fails the batch by name.
 *   4. Only then `read_json` the same verified objects for schema inference, and insert
 *      the typed rows.
 *   5. One transaction per node: either every batch lands or none does.
 *
 * Step 3 and step 4 each fetch the object, so a unit costs TWO object GETs. That is
 * deliberate: DuckDB's `read_json` never exposes the bytes it parsed, so the only way to
 * prove the parsed object is the object the manifest committed to is to read and hash it.
 * The objects are content-addressed and immutable, so the two reads cannot disagree. The
 * count is returned in {@link MaterializeResult.objectGets} so the canary gate measures it
 * rather than assuming it.
 *
 * NODE HOLDS NO PAYLOAD. Refs, counts and scalar status cross the process boundary;
 * payload bytes flow S3 → embedded DuckDB → parquet inside the materializer process.
 */

import { mkdirSync } from 'node:fs';
import path from 'node:path';

import { DuckDBInstance, type DuckDBConnection } from '@duckdb/node-api';

import type { ObjectStore } from '../codec/objectStore';
import { applianceDataDir } from './dataDir';
import { MATERIALIZATION_SCHEMA_VERSION } from './materialization';

/** Where a DuckLake lives: its catalog metadata store and the data-file storage root. */
export interface LakeConfig {
  /**
   * DuckLake catalog metadata store — a local DuckDB file in the appliance's data directory
   * ({@link defaultCatalogPath}), interpolated straight into `ATTACH 'ducklake:<catalog>'`.
   *
   * `KONTRA_DUCKLAKE_CATALOG` IS ALIVE AND UNSET, AND THAT IS THE WHOLE STATEMENT (ADR 0031 §1b).
   * It sets a `postgres:…` connstring so that MORE THAN ONE PROCESS can share one catalog, and
   * that is the only thing it was ever for: the API read the catalog the materializer wrote, and
   * they were two containers. They are one process now (`roles.ts`), so the file is enough — this
   * is not a migration to a different lake, it is the removal of the override that made a server
   * necessary. Anyone who re-splits the API and the materializer must set it again, because a
   * file catalog is single-writer and the second process refuses to boot (`catalogLock.ts`).
   */
  catalog: string;
  /** DATA_PATH: where DuckLake writes parquet data files. `s3://…/` in cloud, a dir locally. */
  dataPath: string;
  /** True when {@link dataPath} is on S3, so the connection needs httpfs credentials. */
  s3: boolean;
  /**
   * Schema DuckLake keeps its `ducklake_*` metadata tables under: `public` for a Postgres
   * catalog, `main` for a local file. Derived from {@link catalog} so every process agrees.
   * Data tables always live under `<lake>.main` regardless of backend.
   */
  metaSchema: string;
  /**
   * Source URI of the node's output manifest. Defaults to the CAS object on S3
   * (`s3://<bucket>/<casKey>`). Tests pass a local JSON file to exercise the mechanics
   * without a live S3.
   */
  sourceUri?: string;
  /**
   * URI prefix a per-unit blob KEY is resolved against. Defaults to `s3://<bucket>/`.
   * Tests point it at a local directory so the ref-expansion path runs for real offline.
   */
  blobBase?: string;
  /**
   * How many unit objects one verify+insert batch covers. Bounded on purpose: this is the
   * knob that trades object-GET round trips against the materializer's peak RSS, and the
   * canary gate tunes it per workload rather than raising the worker class first.
   */
  batchSize?: number;
  /** DuckDB `memory_limit` for this connection. NOT an RSS cap — the cgroup is. */
  memoryLimit?: string;
  /** DuckDB worker threads. One by default: the materializer is a single-slot worker. */
  threads?: number;
  /** Spill directory. Quota-limited on the materializer host. */
  tempDirectory?: string;
  /** Cap on spill so a runaway sort fills a quota instead of the host's disk. */
  maxTempSize?: string;
}

/** The lake alias every connection ATTACHes under; its metadata catalog is derived from it. */
const LAKE = 'lake';
const META = `__ducklake_metadata_${LAKE}`;

/**
 * Actor output. One table per ACTOR, named after the actor, partitioned by version and
 * dispatch time — so the object store reads `output/crawl4ai/version=1.0.0/dt=…/`.
 *
 * This replaces `ds_<sha1(actor\0version\0node\0schema)[:16]>` and the `_kontra_dataset`
 * registry that existed only to translate that hash back. The hash was collision-safe and
 * completely unreadable; an operator had to join a registry table to learn which actor a
 * directory belonged to. Actor names are already contract-validated identifiers, so the name
 * itself is a safe, and legible, table name.
 */
const OUTPUT_SCHEMA = 'output';

/**
 * Operator-loaded lists (scope, seeds) — `kontra db`. A SEPARATE top-level directory from
 * actor output, because they are separate things: one is what you feed a run, the other is
 * what a run produced. Mixing them is what made "which of these is my scope?" a question.
 */
const STANDALONE_SCHEMA = 'standalone';

/** Default units per verify+insert batch. Override with `KONTRA_MATERIALIZE_BATCH`. */
export const DEFAULT_BATCH_SIZE = 256;

/**
 * The dispatch-time partition value: `2026-08-03T19-42-07`.
 *
 * SECOND precision, and derived from the server-minted `run_started_at` — not from when
 * materialization happened, which drifts from the run whenever decoding lags. Hour precision
 * was not enough: two dispatches of the same actor in one hour is the normal case, and
 * merging them into one directory is exactly the "which run is this?" problem again.
 *
 * Colons are replaced because they are hostile in object keys and on Windows paths.
 */
export function dtPartition(runStartedAt: number): string {
  const ms = Number.isFinite(runStartedAt) && runStartedAt > 0 ? runStartedAt : 0;
  return new Date(ms).toISOString().slice(0, 19).replace(/:/g, '-');
}

/**
 * The inverse of {@link dtPartition}: the epoch ms a `dt=` partition value denotes.
 *
 * IT LIVES HERE, TOUCHING ITS FORWARD FORM, because ADR 0029 §2's standing warning is that "a
 * second datetime format would be a second thing to get wrong" — and an inverse spelled in some
 * other file IS a second spelling of the format, free to drift the moment either side is edited.
 * `parquet.test.ts` pins the round trip in both directions, so the pair is one fact.
 *
 * WHO NEEDS IT. A partition value is the ONLY record of a **Run**'s start that survives for a
 * **Dataset** the materialization ledger never saw — every Run the actorkit path produces, whose
 * rows are written by `publishBatch` and leave no ledger record at all. `withDatasetNames` renders
 * the derived name's datetime from the row's own `dt` for that reason; feeding it back through
 * `dtPartition` returns the identical string, which is what makes the rendering one rule rather
 * than a ledger branch and a lake branch.
 *
 * A value this cannot parse — a malformed or absent partition — is `0`, which {@link dtPartition}
 * renders as the epoch. Not thrown: a listing must not fail on one unreadable row.
 */
export function parseDtPartition(dt: string): number {
  const m = /^(\d{4}-\d{2}-\d{2})T(\d{2})-(\d{2})-(\d{2})$/.exec(String(dt ?? '').trim());
  if (!m) return 0;
  const ms = Date.parse(`${m[1]}T${m[2]}:${m[3]}:${m[4]}Z`);
  return Number.isFinite(ms) ? ms : 0;
}

/**
 * An actor (or dataset) name as a SQL identifier and a path segment. Actor names are already
 * contract-validated, so this is identity on every real name; it exists so a name arriving
 * from a catalog row can never carry a quote into generated SQL or a slash into a key.
 */
export function safeName(name: string): string {
  const out = name.replace(/[^A-Za-z0-9_.-]/g, '_');
  return /^[0-9]/.test(out) ? `a${out}` : out || 'unnamed';
}

/**
 * Schema DuckLake stores its `ducklake_*` metadata tables in, by catalog backend: a
 * Postgres catalog puts them in `public`; a local DuckDB/SQLite catalog file keeps them
 * in `main`. (Data tables always live under `<lake>.main` regardless.)
 *
 * MEASURED, both ways, against DuckLake 1.5.4: on a file catalog
 * `__ducklake_metadata_lake.public.ducklake_table` answers `Catalog Error: … schema "public" does
 * not exist`, and `main` answers rows. There is no schema both backends share, so every read of a
 * `ducklake_*` table takes this value — a hardcoded `public` is a query that worked for exactly
 * as long as the catalog was Postgres.
 */
function metaSchemaFor(catalog: string): string {
  return catalog.startsWith('postgres:') ? 'public' : 'main';
}

/**
 * The catalog as a FILESYSTEM PATH, or null when it names a server.
 *
 * A catalog string carrying a `<scheme>:` prefix is handed to the matching DuckDB extension
 * (`postgres:`, `sqlite:`, `mysql:`); a bare string is a path, which is what the default is. The
 * distinction is what decides three things that must agree — whether a parent directory gets
 * created, whether the boot takes the single-writer lock, and whether a query connection can go
 * stale — so it is one function rather than three `startsWith` calls.
 */
export function catalogFilePath(catalog: string): string | null {
  return /^[a-z][a-z0-9+.-]*:/i.test(catalog) ? null : catalog;
}

/**
 * The default catalog: ONE FILE, in the appliance's data directory (ADR 0031 §1b).
 *
 * The name is `datasets.ducklake` and the directory is the one `kontra up` owns, because the
 * previous default — the bare relative name `orchestrator-datasets.ducklake` — resolved against
 * the PROCESS'S CWD. That was harmless while a connstring overrode it and is not harmless now: in
 * the container cwd is `/app`, so the catalog would land outside every mounted volume and a
 * recreate would silently start a new, empty lake beside a bucket full of parquet.
 */
export function defaultCatalogPath(): string {
  return path.join(applianceDataDir(), 'datasets.ducklake');
}

/**
 * `KONTRA_DUCKLAKE_CATALOG`, when it names anything.
 *
 * EMPTY IS UNSET, and that distinction is the difference between a working control plane and one
 * that attaches `ducklake:` — compose substitutes the empty string for a variable nobody exported,
 * so `KONTRA_DUCKLAKE_CATALOG: "${KONTRA_DUCKLAKE_CATALOG:-}"` in a YAML file hands this process
 * `''` rather than nothing at all. The setting is meant to be present and unset (ADR 0031 §1b);
 * present-and-empty has to mean the same thing.
 */
function envCatalog(): string | undefined {
  const v = process.env.KONTRA_DUCKLAKE_CATALOG;
  return v && v.trim() !== '' ? v.trim() : undefined;
}

/**
 * The catalog this process will attach, WITHOUT needing a store to ask.
 *
 * It exists so `main.ts` can take the single-writer lock on the catalog it is about to use
 * (`catalogLock.ts`) at a point in boot where no {@link ObjectStore} has been built yet. One
 * function, so the lock and the connection can never disagree about which file is at stake —
 * a guard that resolved the name itself would agree until one of the two was edited.
 */
export function resolveCatalog(override?: string): string {
  return override ?? envCatalog() ?? defaultCatalogPath();
}

/**
 * Make a file catalog attachable: the directory it lives in exists.
 *
 * A DIRECTORY, NOT THE FILE. DuckDB creates a database it is pointed at; it does not create the
 * path to it, and measured on DuckLake 1.5.4 the failure is an `IO Error: … Cannot open file
 * "<data-dir>/datasets.ducklake": No such file or directory` from inside ATTACH — which names the
 * catalog and says nothing about the missing parent. That was invisible while the catalog was a
 * connstring, and it is the very first thing a fresh data directory hits now.
 *
 * THE WRITER AND THE BOOT LOCK CALL THIS; THE READER DOES NOT. `catalogLock.ts` needs it for the
 * same reason and one step earlier — on a FRESH install nothing has created the data directory yet,
 * and a boot guard that died with `ENOENT: … datasets.ducklake.lock` would make the first start of
 * a new appliance fail on the very mechanism meant to protect the second one. The workbench's
 * READ_ONLY connection is the exception: it must keep failing on a catalog nobody has written —
 * `queryEngine.ts:friendlyError` turns that into "no datasets yet", which is the truth, and
 * creating a lake as a side effect of reading one would replace a correct answer with an empty
 * table.
 */
export function ensureCatalogDir(catalog: string): void {
  const file = catalogFilePath(catalog);
  if (file === null) return; // a server catalog: it is the server's job to exist
  mkdirSync(path.dirname(path.resolve(file)), { recursive: true });
}

function intFromEnv(name: string, fallback: number): number {
  const v = process.env[name];
  if (v === undefined || v === '') return fallback;
  const n = Number.parseInt(v, 10);
  return Number.isFinite(n) && n > 0 ? n : fallback;
}

/**
 * Resolve the {@link LakeConfig} for a store. On S3 the data path is `datasets/` under the
 * store's bucket/prefix; locally it falls back to `KONTRA_DUCKLAKE_DATA_PATH` (tests). The
 * catalog is the local file {@link defaultCatalogPath} names, and `KONTRA_DUCKLAKE_CATALOG` can
 * still point it at a shared Postgres for a deployment that runs more than one process
 * ({@link LakeConfig.catalog}); {@link LakeConfig.metaSchema} then follows the backend.
 */
export function resolveLakeConfig(store: ObjectStore, override: Partial<LakeConfig> = {}): LakeConfig {
  const s3 = Boolean(store.endpoint);
  // DATA_PATH is the BUCKET ROOT, so DuckLake's own `<schema>/<table>/` layout produces the
  // directory structure directly:
  //     output/<actor>/version=<v>/dt=<dispatch>/…parquet
  //     standalone/<dataset>/…parquet
  // Nothing derives a path by string-building, and there is no `datasets/` middle segment
  // whose only job was to hold hashed table names.
  const dataPath = override.dataPath ?? (s3
    ? `s3://${store.bucket}/${store.prefix ? `${store.prefix}/` : ''}`
    : process.env.KONTRA_DUCKLAKE_DATA_PATH ?? './');
  const catalog = resolveCatalog(override.catalog);
  return {
    catalog,
    dataPath,
    s3: override.s3 ?? s3,
    metaSchema: override.metaSchema ?? metaSchemaFor(catalog),
    sourceUri: override.sourceUri,
    blobBase: override.blobBase ?? `s3://${store.bucket}/`,
    batchSize: override.batchSize ?? intFromEnv('KONTRA_MATERIALIZE_BATCH', DEFAULT_BATCH_SIZE),
    // 256 MB inside a 512 MB cgroup (plan §1). DuckDB's own setting bounds its buffer
    // manager, not the process — the cgroup is the hard boundary, and this sits under it
    // so DuckDB spills to the temp volume before the kernel reaches for the OOM killer.
    memoryLimit: override.memoryLimit ?? process.env.KONTRA_DUCKDB_MEMORY_LIMIT ?? '256MB',
    threads: override.threads ?? intFromEnv('KONTRA_DUCKDB_THREADS', 1),
    tempDirectory: override.tempDirectory ?? process.env.KONTRA_DUCKDB_TEMP_DIR,
    maxTempSize: override.maxTempSize ?? process.env.KONTRA_DUCKDB_MAX_TEMP_SIZE ?? '2GB',
  };
}

/**
 * One attached-lake connection per (catalog, dataPath), each on its own DuckDB instance:
 * `ATTACH … AS lake` is instance-scoped, so a shared instance would alias-collide.
 *
 * THE CACHE CLEARS ON FAILURE. The previous version memoized the promise unconditionally,
 * so a single transient catalog error was cached as a REJECTED promise and every later
 * materialization in that process failed instantly — writes disabled for the process
 * lifetime, with the run still reporting success. Clearing on failure is what lets the
 * activity's retry policy actually retry.
 */
const connections = new Map<string, Promise<DuckDBConnection>>();

/** Test seam: drop cached connections so a fresh config attaches cleanly. */
export function resetLakeConnections(): void {
  connections.clear();
}

/** The `SET s3_…` block pointing DuckDB's httpfs at the same store the codec uses. */
function s3Setup(store: ObjectStore): string {
  const endpoint = store.endpoint ?? '';
  const useSsl = endpoint.startsWith('https');
  const host = endpoint.replace(/^https?:\/\//, '').replace(/\/+$/, '');
  return [
    'INSTALL httpfs; LOAD httpfs;',
    `SET s3_endpoint='${host}';`,
    "SET s3_url_style='path';", // SeaweedFS (and our cloud layout) address path-style
    `SET s3_use_ssl=${useSsl};`,
    `SET s3_access_key_id='${sqlLiteral(store.accessKey)}';`,
    `SET s3_secret_access_key='${sqlLiteral(store.secretKey)}';`,
    `SET s3_region='${store.region}';`,
  ].join('\n');
}

/** A connection with DuckLake attached (and S3 configured when the data path is on S3). */
export async function lakeConnection(store: ObjectStore, cfg: LakeConfig): Promise<DuckDBConnection> {
  const cacheKey = `${cfg.catalog}\0${cfg.dataPath}`;
  const cached = connections.get(cacheKey);
  if (cached) return cached;

  const opening = (async () => {
    // The catalog's directory before the connection, because a missing parent is an `IO Error`
    // from inside ATTACH and not a missing-directory message.
    ensureCatalogDir(cfg.catalog);
    const c = await (await DuckDBInstance.create()).connect();
    // Resource posture FIRST, before anything can allocate: a bounded buffer manager, one
    // thread, and a spill directory with a ceiling. Community extensions and autoinstall
    // stay off — a materializer must never fetch code at runtime.
    await c.run(`SET memory_limit='${cfg.memoryLimit}'`);
    await c.run(`SET threads=${cfg.threads}`);
    if (cfg.tempDirectory) await c.run(`SET temp_directory='${sqlLiteral(cfg.tempDirectory)}'`);
    await c.run(`SET max_temp_directory_size='${cfg.maxTempSize}'`);
    if (cfg.s3) await c.run(s3Setup(store));
    await c.run('INSTALL ducklake; LOAD ducklake;');
    await c.run('INSTALL json; LOAD json;');
    // A Postgres-backed catalog needs the postgres extension. DuckLake auto-loads it on
    // ATTACH, but load it explicitly so a shared catalog attaches deterministically.
    if (cfg.catalog.startsWith('postgres:')) await c.run('INSTALL postgres; LOAD postgres;');
    // DATA_INLINING_ROW_LIMIT 0: keep data OUT of the catalog and IN parquet data files,
    // so a presigned data file is a complete, readable object on its own.
    await c.run(
      `ATTACH IF NOT EXISTS 'ducklake:${cfg.catalog}' AS ${LAKE} ` +
        `(DATA_PATH '${cfg.dataPath}', DATA_INLINING_ROW_LIMIT 0)`
    );
    await c.run(`CREATE SCHEMA IF NOT EXISTS ${LAKE}.${OUTPUT_SCHEMA}`);
    await c.run(`CREATE SCHEMA IF NOT EXISTS ${LAKE}.${STANDALONE_SCHEMA}`);
    return c;
  })();

  connections.set(cacheKey, opening);
  try {
    return await opening;
  } catch (err) {
    connections.delete(cacheKey); // never cache a rejection — see the comment above
    throw err;
  }
}

/** Escape a single-quoted SQL literal. Every interpolation below goes through this. */
function sqlLiteral(v: string): string {
  return v.replace(/'/g, "''");
}

/**
 * A DETERMINISTIC materialization failure: the bytes are missing, or they are not the
 * bytes the manifest committed to.
 *
 * Distinguished from transient I/O on purpose. Retrying a sha256 mismatch four times
 * produces four identical mismatches and four rounds of object GETs; the answer will not
 * change until someone fixes the data. The activity marks these non-retryable so the run
 * reaches `output_failed` immediately, with the offending key named, instead of spending
 * the retry budget re-proving the same fact.
 */
export class MaterializationIntegrityError extends Error {
  readonly keys: string[];

  constructor(message: string, keys: string[] = []) {
    super(message);
    this.name = 'MaterializationIntegrityError';
    this.keys = keys;
  }
}

/** One per-unit blob reference carried by the manifest. */
interface UnitRef {
  key: string;
  sha256: string;
}

/** What one node's materialization produced — the numbers the status record stores. */
export interface MaterializeResult {
  /** Physical DuckLake table, or null when there was nothing to create one from. */
  tbl: string | null;
  /** Typed rows committed. Zero is a successful empty result, never a failure. */
  rows: number;
  /** Parquet bytes DuckLake reports for this run partition after the commit. */
  bytes: number;
  /** DuckLake snapshot the commit landed in. */
  snapshotId: number | null;
  /** Unit objects read via `$ref` (0 for a fully inline envelope). */
  refObjects: number;
  /** Object GETs this materialization issued — measured, not assumed. */
  objectGets: number;
  /** Inline result entries taken straight from the (already verified) manifest. */
  inlineUnits: number;
}

export interface MaterializeSelector {
  /** The manifest's CAS content address (the node output BareRef sha256). */
  sha256: string;
  actor: string;
  /**
   * Which Actor version produced these rows. A PARTITION column, so it is also a directory.
   * `null` is UNRECORDED and is stored as SQL NULL — see {@link node} for why nothing plausible
   * may stand in for it. DuckLake partitions on a NULL value and reads it back as NULL.
   */
  version: string | null;
  runId: string;
  /**
   * WHICH MACHINE ran the Method that produced these rows — the actor host's own hostname
   * (`kf-dns-01`), carried from the Batch ref's meta through `publishBatch`.
   *
   * THE COLUMN IS CALLED `node` AND USED TO MEAN SOMETHING ELSE. In the v1 wire contract
   * `node_id` is documented as "which graph node produced this dispatch" — a position in the
   * graph interpreter (`handler/_gen/kontra/v1/run.pb.go`). v2 deleted graphs (ADR 0023 §12
   * retires **Node** outright), so the dimension stopped existing and every row landed the
   * literal `'w'` the publish activity substituted for the missing value. Nor is the column fed
   * from that field any more: `node_id` survives on the wire as a per-DISPATCH id that keys the
   * actor instance and the backing workflow (`catalog.py` mints `{actor}-{uuid8}`), which is a
   * third meaning again and not one a published row wants. The column is
   * REPURPOSED here, deliberately, to the provenance an operator actually needs — a column that
   * silently changed meaning between versions is worse than one that was always empty, so it is
   * said here, where the column is defined. The name itself stays: the lake table is created
   * from its first batch's schema, so renaming it is a migration.
   *
   * One caller still passes the OLD meaning: `activities/materialize.ts`, the v1 interpreter's
   * per-node materializer. It is registered on the materializer worker and no v2 workflow
   * schedules it, so nothing writes a graph node id into a live table — but if that path is ever
   * revived, it and this column now mean two different things and one of them has to give.
   *
   * THREE VALUES A READER MUST TELL APART:
   *   - a hostname — the Machine that ran the Method, measured;
   *   - SQL NULL — UNRECORDED: nothing knew the Machine (a Batch paged out of a Dataset was
   *     produced by the lake, not by a Machine; an actor host older than the meta contract
   *     reports none). No producer can emit NULL, which is what makes it unambiguous;
   *   - the literal `'w'` — a LEGACY row, written before this change by the substitution above.
   *     It reads back untouched and is NOT reinterpreted as unrecorded, because "nothing wrote
   *     this" and "the old placeholder was written here" are different facts.
   */
  node: string | null;
  /** Server-minted run start (ms); stamped on every row as `run_started_at`. */
  runStartedAt: number;
  /** Identity version. Defaults to the current writer's. */
  schemaVersion?: number;
}

/**
 * Materialize one node's output into its DuckLake table, in the `run_id=<runId>` partition.
 *
 * THROWS on failure. The previous version returned `null` from a bare `catch {}` with no
 * log line, so "materialization failed" and "the node found nothing" were the same value
 * to every caller — the exact ambiguity ADR 0017 exists to remove. A failure now reaches
 * the activity, which records it as `failed` and surfaces it as public `output_failed`.
 *
 * A genuinely empty node returns `rows: 0` (with `tbl: null` when no table existed yet):
 * a SUCCESS with nothing in it, which is a different fact from a failure.
 *
 * ponytail: the SQL interpolates identifiers/paths (COPY/read_json/ATTACH cannot bind a
 * path param) — every interpolated part is a server-minted or contract-validated
 * identifier (sha hex, run-id uuid, actor/version/node ids, store-owned keys) and goes
 * through {@link sqlLiteral}, never free user text.
 */
export async function writeDatasetParquet(
  store: ObjectStore,
  sel: MaterializeSelector,
  override: Partial<LakeConfig> = {}
): Promise<MaterializeResult> {
  const cfg = resolveLakeConfig(store, override);
  const conn = await lakeConnection(store, cfg);
  const tbl = safeName(sel.actor);
  const dt = dtPartition(sel.runStartedAt);
  const src = cfg.sourceUri ?? `s3://${store.bucket}/${store.casKey(sel.sha256)}`;
  const batchSize = cfg.batchSize ?? DEFAULT_BATCH_SIZE;

  let objectGets = 0;

  // --- 1. the manifest, verified in SQL -------------------------------------------------
  // `read_blob` on a missing object returns ZERO ROWS, not an error. Left unchecked that
  // reads as "the node produced nothing" — a missing manifest wearing an empty result's
  // clothes. Count first, then compare the digest.
  const manifest = await conn.runAndReadAll(
    `SELECT sha256(content) AS digest, json_type(decode(content)) AS shape,
            CASE WHEN json_type(decode(content)) = 'ARRAY' THEN decode(content)
                 ELSE json_extract(decode(content), '$.results')::VARCHAR END AS results
       FROM read_blob('${sqlLiteral(src)}')`
  );
  objectGets += 1;
  const mrows = manifest.getRows();
  if (mrows.length === 0) {
    throw new MaterializationIntegrityError(`output manifest object missing: ${src}`, [src]);
  }
  const digest = String(mrows[0]![0]);
  // `sourceUri` is a test seam pointing at a file whose sha is not the selector's; skip the
  // comparison only in that case, never for a real CAS read.
  if (!cfg.sourceUri && digest !== sel.sha256) {
    throw new MaterializationIntegrityError(
      `output manifest integrity check failed for ${src}: got ${digest}, want ${sel.sha256}`,
      [src]
    );
  }
  const shape = String(mrows[0]![1]);
  const resultsJson = mrows[0]![2];
  if (resultsJson === null || resultsJson === undefined) {
    throw new MaterializationIntegrityError(`output manifest has no results array: ${src}`, [src]);
  }

  // --- 2. page the refs (refs only — no payload crosses into Node) ----------------------
  const entries = await conn.runAndReadAll(
    `WITH d AS (SELECT ${quoteJson(String(resultsJson))} AS s)
     SELECT json_extract_string(e, '$."$ref".key')    AS key,
            json_extract_string(e, '$."$ref".sha256') AS sha
       FROM d, unnest(CAST(d.s AS JSON[])) AS t(e)`
  );
  const refs: UnitRef[] = [];
  let inlineUnits = 0;
  for (const row of entries.getRows()) {
    const key = row[0];
    const sha = row[1];
    if (typeof key === 'string' && typeof sha === 'string') refs.push({ key, sha256: sha });
    else inlineUnits += 1;
  }
  // Deterministic order: a batch's contents must not depend on DuckDB's unnest order, or
  // two attempts of the same node would read different objects into different batches.
  refs.sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));

  if (refs.length === 0 && inlineUnits === 0) {
    // A genuinely empty node. Nothing to infer a schema from, so no table is created —
    // but this is a SUCCESS, and the caller records it as `complete` with zero rows.
    return { tbl: null, rows: 0, bytes: 0, snapshotId: null, refObjects: 0, objectGets, inlineUnits: 0 };
  }

  // --- 3/4/5. verify, type, insert — one transaction for the whole node -----------------
  const base = cfg.blobBase ?? `s3://${store.bucket}/`;
  let rows = 0;
  let created = false;
  await conn.run('BEGIN TRANSACTION');
  try {
    if (inlineUnits > 0) {
      // Inline entries live inside the manifest, whose sha256 is already verified above —
      // they need no second integrity check.
      rows += await insertBatch(conn, {
        tbl,
        runId: sel.runId,
        version: sel.version,
        dt,
        node: sel.node,
        runStartedAt: sel.runStartedAt,
        source: inlineSelect(src, shape),
        ensureCreated: !created,
        onCreate: () => {
          created = true;
        },
      });
      objectGets += 1;
    }

    for (let i = 0; i < refs.length; i += batchSize) {
      const batch = refs.slice(i, i + batchSize);
      objectGets += await verifyBatch(conn, base, batch);
      rows += await insertBatch(conn, {
        tbl,
        runId: sel.runId,
        version: sel.version,
        dt,
        node: sel.node,
        runStartedAt: sel.runStartedAt,
        source: refSelect(base, batch),
        ensureCreated: !created,
        onCreate: () => {
          created = true;
        },
      });
      objectGets += batch.length;
    }

    await conn.run('COMMIT');
  } catch (err) {
    await conn.run('ROLLBACK').catch(() => undefined);
    throw err;
  }

  const { bytes, snapshotId } = await partitionStats(conn, cfg.metaSchema, tbl, dt);
  return { tbl, rows, bytes, snapshotId, refObjects: refs.length, objectGets, inlineUnits };
}

/** What a promotion asks for: fill a durable Dataset from a query over another one. */
export interface PromoteSelector {
  /**
   * The durable **Dataset** the rows land in. Created partitioned by (version, dt) when it does
   * not exist yet — exactly as a first publish creates it — so a promoted-into target and a
   * published-into one are the same physical shape.
   */
  target: string;
  /** The **Dataset** the rows come from, by bare name — normally a temporary one. */
  source: string;
  /**
   * The SELECT that decides which rows promote, referencing the source by its bare name. The
   * rows it returns ARE the rows promoted — the `--query` rule the pager already follows. The
   * default and the `where=` shorthand are `SELECT *`, which is what carries provenance through:
   * `node`, `version`, `run_id` and `run_started_at` ride the row from the producing **Run** into
   * the target UNTOUCHED. A custom query that projects them away drops provenance, and that is the
   * author's to choose — the same freedom `--query` gives everywhere else.
   */
  sql: string;
}

/**
 * PROMOTE rows out of one **Dataset** into a durable one — the second act the temp-datasets
 * feature exists to separate from production (temp-datasets slice 02). Producing rows (a **Method**
 * pushing into a temp) and ACCEPTING them (this) are two acts with a gap between them, and this is
 * the accepting side.
 *
 * A COPY, NOT A RE-REGISTRATION, AND THE COST IS MEASURED. `INSERT INTO … SELECT` is a real scan
 * and a real write of new parquet — measured at ~170 ms for 40k rows (single thread, 256 MB limit),
 * filtered or full, so "staging is cheap" holds and the number is in the PRD. Re-registering the
 * temp's underlying files into the target (`ducklake_add_data_files`) was rejected, not overlooked:
 * a promotion is a FILTERED query (`WHERE NOT ok` promoted 26,666 of 40,000), and file
 * re-registration has no row predicate; and even a promote-everything would SHARE the temp's
 * physical parquet with the target, so slice 03's deletion of the temp would take the target's rows
 * with it. A copy severs that link — after promotion the target owns its own files.
 *
 * PROVENANCE IS THE SOURCE'S, NEVER THIS CALL'S. Deliberately NOT routed through `publishBatch`:
 * that stamps the CALLING workflow's `run_id` and the Batch's Machine, which over a promotion would
 * write the promoting **Run** over the producing one — the exact shape of the four-Machine run that
 * landed 1,246 rows all reading `node='w'`. This carries `node`/`version`/`run_id`/`run_started_at`
 * straight out of the source row, so a promoted row still names the Machine and Actor version that
 * PRODUCED it.
 *
 * NOT IDEMPOTENT, BY DESIGN. A second identical promotion APPENDS the same rows again. Unlike
 * `publishBatch`, which dedups by the Batch's content address, a filtered `INSERT … SELECT` has no
 * content address and the lake stamps no row id to dedup on — and refusing a re-promote would break
 * the legitimate case of promoting different subsets over time (`WHERE NOT ok`, then later a wider
 * cut). So promotion is an explicit, deliberate act a caller runs once; running it twice doubles.
 *
 * ponytail: `sel.sql` is caller-authored SQL by the `--query` contract, the same trust model as
 * the pager's `where`/`query`; identifiers (`target`, `source`) go through {@link safeName}.
 */
export async function promoteInto(
  store: ObjectStore,
  sel: PromoteSelector,
  override: Partial<LakeConfig> = {}
): Promise<{ rows: number }> {
  const cfg = resolveLakeConfig(store, override);
  const conn = await lakeConnection(store, cfg);
  const target = safeName(sel.target);
  const source = safeName(sel.source);

  // The source addressed by its bare name so the caller's SQL (and the default `SELECT *`) reads
  // it unqualified — a TRANSACTION-SCOPED CTE, not a catalog view: nothing lingers on the shared
  // materializer connection after the promote, so a later temp deletion (slice 03) cannot leave a
  // view dangling at a dropped table. The outer subquery makes `LIMIT 0` and `DESCRIBE` apply
  // whatever shape the caller's SELECT has (a trailing ORDER BY / LIMIT would otherwise bind wrong).
  // `source`/`target` are already {@link safeName}'d to `[A-Za-z0-9_.-]`, so they carry no quote
  // to escape inside these identifiers; only `sel.sql` is free-form (the `--query` contract).
  const scoped =
    `WITH "${source}" AS (SELECT * FROM ${LAKE}.${OUTPUT_SCHEMA}."${source}") ` +
    `SELECT * FROM (${sel.sql}) AS _promote`;

  // A SOURCE THAT NEVER MATERIALIZED IS ZERO ROWS, NOT AN ERROR. A temp Dataset is created
  // lazily by its first published Batch, so a Run whose Methods all failed — or that legitimately
  // found nothing — reaches its promotion with no table behind the name. DuckDB answers that with
  // `Catalog Error: Table with name tmp_… does not exist!`, which this activity retries on the
  // default policy of UNLIMITED attempts: measured on `redditscrape-1787581348`, attempt 22 and
  // climbing 25 minutes after the fleet was already torn down, with the Run stuck `running`
  // forever. Nothing about that is transient — the table cannot appear, because the producer is
  // gone — so it must not be raised as a retryable failure.
  //
  // The target is deliberately NOT created here. There is no source table to read a shape from,
  // and a Dataset with no columns is worse than an absent one: `dataset list` would show a name
  // that no query can select from. Zero rows promoted, said plainly, lets the CALLER decide
  // whether an empty harvest is success or failure — which is a judgement the lake cannot make.
  if (!(await tableExists(conn, source))) return { rows: 0 };

  await conn.run('BEGIN TRANSACTION');
  try {
    if (!(await tableExists(conn, target))) {
      // Create empty (LIMIT 0) so partitioning is set BEFORE any row lands — a
      // CREATE … AS <select> would write the first rows unpartitioned.
      await conn.run(`CREATE TABLE ${LAKE}.${OUTPUT_SCHEMA}."${target}" AS ${scoped} LIMIT 0`);
      // Partition on (version, dt) ONLY when the promoted projection still carries them: the
      // default and `where=` do (they are `SELECT *`), while a custom query that dropped
      // provenance does not, and partitioning by an absent column would fail the create.
      const cols = new Set(
        (await conn.runAndReadAll(`DESCRIBE ${scoped}`)).getRows().map((r) => String(r[0]))
      );
      if (cols.has('version') && cols.has('dt')) {
        await conn.run(
          `ALTER TABLE ${LAKE}.${OUTPUT_SCHEMA}."${target}" SET PARTITIONED BY (version, dt)`
        );
      }
    }
    await evolveSchema(conn, target, scoped);
    const res = await conn.runAndReadAll(
      `INSERT INTO ${LAKE}.${OUTPUT_SCHEMA}."${target}" BY NAME ${scoped}`
    );
    await conn.run('COMMIT');
    return { rows: Number(res.getRows()[0]?.[0] ?? 0) };
  } catch (err) {
    await conn.run('ROLLBACK').catch(() => undefined);
    throw err;
  }
}

/**
 * A provenance column's SQL: the value as a VARCHAR literal, or a typed NULL when it is
 * unrecorded. Empty string is folded into NULL because it reaches here the same way absence
 * does — a Batch whose ref meta named no Machine arrives as `''` after one `||` too many — and
 * an empty Machine name is not a thing that exists.
 */
function varcharOrNull(v: string | null | undefined): string {
  return v ? `CAST('${sqlLiteral(v)}' AS VARCHAR)` : 'CAST(NULL AS VARCHAR)';
}

/** A JSON string as a SQL literal, cast to JSON so `unnest(CAST(… AS JSON[]))` applies. */
function quoteJson(s: string): string {
  return `'${sqlLiteral(s)}'::JSON`;
}

/**
 * Typed rows from the manifest's INLINE entries (already covered by the manifest sha).
 *
 * Two manifest shapes reach here and they need different SQL. An ENVELOPE
 * (`{done, results, failures, opens}`) is one row whose `results` column must be unnested.
 * A BARE ARRAY is what the handler stores since a Method's result ref started addressing a
 * bare list of units — `read_json` expands a top-level JSON array to one row per element
 * already, so unnesting a `results` column that does not exist would be a binder error.
 *
 * The manifest read above (`json_type(...) = 'ARRAY'`) has always branched on this; only the
 * inline path did not, which made it a latent break that went live the moment bare arrays
 * became real.
 */
function inlineSelect(src: string, shape: string): string {
  if (shape === 'ARRAY') {
    return `SELECT * FROM read_json('${sqlLiteral(src)}')`;
  }
  return (
    `SELECT r.* FROM (SELECT unnest(results) AS r FROM read_json('${sqlLiteral(src)}')) ` +
    `WHERE r IS NOT NULL`
  );
}

/**
 * Typed rows from a verified batch of per-unit objects.
 *
 * `hive_partitioning=false` is load-bearing. Unit blobs live at
 * `units/run=…/dt=…/actor=…/shard=…/unit=…/<sha>.json`, and `read_json` auto-detects that
 * `k=v` path shape and silently ADDS `run`, `dt`, `actor`, `shard` and `unit` as columns.
 * Those are storage-layout artefacts, not actor output: `run` duplicates the `run_id` this
 * writer stamps, and an actor whose own output has a field called `actor` or `unit` would
 * collide with one. Verified live — the first end-to-end run materialized all five.
 */
function refSelect(base: string, batch: UnitRef[]): string {
  const paths = batch.map((r) => `'${sqlLiteral(base + r.key)}'`).join(', ');
  return `SELECT * FROM read_json([${paths}], union_by_name=true, hive_partitioning=false)`;
}

/**
 * Read every object in the batch and prove it is the object the manifest committed to.
 *
 * Two distinct failures, both fatal, both named:
 *   - MISSING — `read_blob` silently omits absent objects, so the row count is compared
 *     against the batch size. This is the exact shape of "absent output looks empty".
 *   - CORRUPT — the object exists but its sha256 does not match the carried one.
 *
 * Returns the number of object GETs issued.
 */
async function verifyBatch(conn: DuckDBConnection, base: string, batch: UnitRef[]): Promise<number> {
  const paths = batch.map((r) => `'${sqlLiteral(base + r.key)}'`).join(', ');
  const expected = batch
    .map((r) => `('${sqlLiteral(base + r.key)}', '${sqlLiteral(r.sha256)}')`)
    .join(', ');
  const res = await conn.runAndReadAll(
    `WITH want(path, sha) AS (VALUES ${expected}),
          got AS (SELECT filename, sha256(content) AS digest FROM read_blob([${paths}]))
     SELECT w.path, g.digest IS NULL AS missing
       FROM want w LEFT JOIN got g ON g.filename = w.path
      WHERE g.digest IS NULL OR g.digest <> w.sha
      LIMIT 5`
  );
  const bad = res.getRows();
  if (bad.length > 0) {
    const missing = bad.filter((r) => r[1] === true).map((r) => String(r[0]));
    const corrupt = bad.filter((r) => r[1] !== true).map((r) => String(r[0]));
    const parts: string[] = [];
    if (missing.length) parts.push(`missing: ${missing.join(', ')}`);
    if (corrupt.length) parts.push(`sha256 mismatch: ${corrupt.join(', ')}`);
    throw new MaterializationIntegrityError(
      `unit object verification failed (${parts.join('; ')})`,
      bad.map((r) => String(r[0]))
    );
  }
  return batch.length;
}

interface InsertBatch {
  tbl: string;
  runId: string;
  /** `null` is unrecorded — see {@link MaterializeSelector.version}. */
  version: string | null;
  dt: string;
  /** The Machine that ran the Method; `null` is unrecorded — see {@link MaterializeSelector.node}. */
  node: string | null;
  runStartedAt: number;
  /** A SELECT producing the batch's typed unit rows. */
  source: string;
  ensureCreated: boolean;
  onCreate: () => void;
}

/**
 * Insert one batch's typed rows, evolving the table's schema first when the batch carries
 * a column the table has not seen. `INSERT … BY NAME` tolerates reordering and missing
 * columns but REJECTS unknown ones, so heterogeneous actor output needs the explicit
 * `ADD COLUMN` — otherwise the first batch's shape would silently become the node's shape
 * and later fields would fail the whole node.
 */
async function insertBatch(conn: DuckDBConnection, b: InsertBatch): Promise<number> {
  // `run_started_at` is server-minted per run and preserved across retries, so two attempts
  // of the same node stamp the same instant. `to_timestamp` yields TIMESTAMPTZ.
  //
  // Coerced, not trusted: a non-finite value would interpolate the literal text `NaN`,
  // which DuckDB parses as a COLUMN REFERENCE and reports as "Referenced column NaN not
  // found" — a confusing binder error standing in for a missing timestamp. A run older
  // than the field simply gets the epoch.
  const startedMs = Number.isFinite(b.runStartedAt) ? Math.floor(b.runStartedAt) : 0;
  // Identity travels IN the rows as well as in the path: `version` and `dt` are the partition
  // columns (so they become directories), `node` and `run_id` are ordinary columns kept for
  // diagnosis and for exact-run scoping. A reader who only has the parquet file still knows
  // exactly which dispatch produced it — and, since `node` became the Machine, which Machine.
  //
  // UNRECORDED IS SQL NULL, NOT THE EMPTY STRING. Both columns are VARCHAR and a producer can
  // legitimately emit any string, so `''` would be a value competing with real ones; NULL is
  // outside the domain entirely, which is what lets a reader tell "nothing wrote this" from
  // "this is the value". It also survives the CREATE: the table's shape comes from this SELECT,
  // and `CAST(NULL AS VARCHAR)` types the column exactly as the literal branch does.
  const select =
    `SELECT u.*, ` +
    `${varcharOrNull(b.version)} AS version, ` +
    `CAST('${sqlLiteral(b.dt)}' AS VARCHAR) AS dt, ` +
    `${varcharOrNull(b.node)} AS node, ` +
    `CAST('${sqlLiteral(b.runId)}' AS VARCHAR) AS run_id, ` +
    `to_timestamp(${startedMs} / 1000.0) AS run_started_at ` +
    `FROM (${b.source}) u`;

  if (b.ensureCreated) {
    const exists = await tableExists(conn, b.tbl);
    if (!exists) {
      // Create empty (LIMIT 0) so partitioning is set BEFORE any row lands; a
      // CREATE … AS <select> would write the first batch unpartitioned.
      await conn.run(`CREATE TABLE ${LAKE}.${OUTPUT_SCHEMA}."${b.tbl}" AS ${select} LIMIT 0`);
      // version + dt ARE the directory structure the operator asked for. run_id stays a
      // column: a dispatch is unique to the second, so a partition still holds one run.
      await conn.run(
        `ALTER TABLE ${LAKE}.${OUTPUT_SCHEMA}."${b.tbl}" SET PARTITIONED BY (version, dt)`
      );
    }
    b.onCreate();
  }

  await evolveSchema(conn, b.tbl, select);
  const res = await conn.runAndReadAll(`INSERT INTO ${LAKE}.${OUTPUT_SCHEMA}."${b.tbl}" BY NAME ${select}`);
  return Number(res.getRows()[0]?.[0] ?? 0);
}

/** Add any column the batch has and the table lacks, using the batch's inferred type. */
async function evolveSchema(conn: DuckDBConnection, tbl: string, select: string): Promise<void> {
  const have = new Set(
    (await conn.runAndReadAll(`DESCRIBE SELECT * FROM ${LAKE}.${OUTPUT_SCHEMA}."${tbl}"`))
      .getRows()
      .map((r) => String(r[0]))
  );
  const want = (await conn.runAndReadAll(`DESCRIBE ${select}`)).getRows();
  for (const row of want) {
    const name = String(row[0]);
    const type = String(row[1]);
    if (have.has(name)) continue;
    await conn.run(`ALTER TABLE ${LAKE}.${OUTPUT_SCHEMA}."${tbl}" ADD COLUMN "${sqlLiteral(name)}" ${type}`);
  }
}

/** Bytes and snapshot for one run partition, read from the catalog (no data-file scan). */
async function partitionStats(
  conn: DuckDBConnection,
  metaSchema: string,
  tbl: string,
  dt: string
): Promise<{ bytes: number; snapshotId: number | null }> {
  const res = await conn.runAndReadAll(
    `SELECT coalesce(sum(df.file_size_bytes), 0) AS bytes, max(df.begin_snapshot) AS snap
       FROM ${META}.${metaSchema}.ducklake_data_file df
       JOIN ${META}.${metaSchema}.ducklake_table t ON t.table_id = df.table_id
       LEFT JOIN ${META}.${metaSchema}.ducklake_file_partition_value pv
         ON pv.data_file_id = df.data_file_id
      WHERE t.table_name = '${sqlLiteral(tbl)}' AND t.end_snapshot IS NULL
        AND df.end_snapshot IS NULL AND pv.partition_value = '${sqlLiteral(dt)}'`
  );
  const row = res.getRows()[0];
  return {
    bytes: Number(row?.[0] ?? 0),
    snapshotId: row?.[1] === null || row?.[1] === undefined ? null : Number(row[1]),
  };
}

async function tableExists(conn: DuckDBConnection, tbl: string): Promise<boolean> {
  const r = await conn.runAndReadAll(
    `SELECT count(*) FROM (SHOW ALL TABLES) WHERE database='${LAKE}' ` +
      `AND schema='${OUTPUT_SCHEMA}' AND name='${sqlLiteral(tbl)}'`
  );
  return Number(r.getRows()[0]?.[0] ?? 0) > 0;
}

/**
 * Whether a DuckLake is available: a real S3 endpoint, or an explicit local data path
 * (tests / a local-only setup). When neither holds there is no store to materialize into.
 */
export function lakeEnabled(store: ObjectStore, override: Partial<LakeConfig> = {}): boolean {
  return Boolean(store.endpoint) || Boolean(override.dataPath) || Boolean(process.env.KONTRA_DUCKLAKE_DATA_PATH);
}

/**
 * The DATA_PATH DuckLake stored at creation — the root every data-file path sits under.
 * `metaSchema` selects the metadata schema for the catalog backend.
 */
export async function lakeDataPath(conn: DuckDBConnection, metaSchema = 'main'): Promise<string> {
  const r = await conn.runAndReadAll(`SELECT value FROM ${META}.${metaSchema}.ducklake_metadata WHERE key='data_path'`);
  return String(r.getRows()[0]?.[0] ?? '');
}

export { LAKE, META, OUTPUT_SCHEMA, STANDALONE_SCHEMA };
