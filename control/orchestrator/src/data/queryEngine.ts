/**
 * The query workbench's engine: arbitrary operator SQL over the lake, on a DELIBERATELY
 * CRIPPLED DuckDB.
 *
 * Everything else in this directory runs queries the server itself composed. This module runs
 * SQL a human typed into a browser, which is a different threat model entirely — so the
 * connection it runs on is built to be bad at everything except reading datasets. Each control
 * below was verified to actually hold, not assumed from documentation:
 *
 *   READ_ONLY on the ATTACH        DROP / DELETE / INSERT / CREATE against the lake are refused.
 *   disabled_filesystems           LocalFileSystem is off, so `read_text('/etc/passwd')`,
 *                                  `COPY … TO '/app/…'`, `INSTALL … FROM 'http://…'` and plain
 *                                  `http://` reads all fail. `s3://` keeps working, which is
 *                                  the only filesystem the lake needs.
 *   lock_configuration             the SQL cannot turn any of that back on, memory_limit
 *                                  included. This is what makes the rest load-bearing rather
 *                                  than advisory.
 *   its own DuckDB instance        the hardening is instance-global, so it must not be applied
 *                                  to the connection the rest of the server (and the tests,
 *                                  which use a LOCAL data path) share.
 *
 * NO SPILLING, ON PURPOSE. With LocalFileSystem disabled DuckDB cannot spill to a temp
 * directory, so `memory_limit` stops being advisory and becomes a real ceiling: a runaway join
 * fails with an error instead of growing until the cgroup OOM-kills the API. `friendlyError`
 * turns that failure into a sentence an operator can act on, because raw DuckDB reports it as
 * a filesystem permission error, which is a confusing way to say "too big".
 *
 * The deep path is still `kontra dataset query --local` / `kontra explore` on the operator's
 * own machine. This is for exploring, on a 512 MB controller shared with six other services.
 */

import { statSync } from 'node:fs';

import {
  DuckDBBitValue,
  DuckDBBlobValue,
  DuckDBDateValue,
  DuckDBDecimalValue,
  DuckDBInstance,
  DuckDBIntervalValue,
  DuckDBTimeNSValue,
  DuckDBTimeTZValue,
  DuckDBTimeValue,
  DuckDBTimestampMillisecondsValue,
  DuckDBTimestampNanosecondsValue,
  DuckDBTimestampSecondsValue,
  DuckDBTimestampTZValue,
  DuckDBTimestampValue,
  DuckDBUUIDValue,
  dateFromDateValue,
  dateFromTimestampMillisecondsValue,
  dateFromTimestampNanosecondsValue,
  dateFromTimestampSecondsValue,
  dateFromTimestampTZValue,
  dateFromTimestampValue,
  doubleFromDecimalValue,
  type DuckDBConnection,
} from '@duckdb/node-api';

import type { ObjectStore } from '../codec/objectStore';
import { assertSingleRead, type Verdict } from './readonlySql';
import { listDatasets, schemaOf, type DatasetColumn, type DatasetInfo } from './datasets';
import {
  LAKE,
  META,
  OUTPUT_SCHEMA,
  STANDALONE_SCHEMA,
  catalogFilePath,
  lakeEnabled,
  resolveLakeConfig,
  type LakeConfig,
} from './parquet';

/** Rows one request may return. The grid pages through blocks; this bounds ONE block. */
export const QUERY_MAX_ROWS = 5000;
export const QUERY_DEFAULT_ROWS = 500;

/** Formats `COPY … TO` can write. Excel is absent: it needs a community extension, and those
 *  are disabled — fetching code at query time is exactly what this connection must not do. */
export const EXPORT_FORMATS = ['csv', 'parquet', 'json', 'jsonl'] as const;
export type ExportFormat = (typeof EXPORT_FORMATS)[number];

export interface QueryResult {
  columns: DatasetColumn[];
  rows: unknown[][];
  /** Server-side execution time. Shown in the workbench so a slow query is visibly slow. */
  elapsedMs: number;
  /** True when the row cap cut the answer short — the grid asks for the next block. */
  truncated: boolean;
}

/** One dataset's queryable shape: what the editor autocompletes and the sidebar lists. */
export interface QuerySchemaEntry {
  kind: DatasetInfo['kind'];
  name: string;
  version?: string;
  dt?: string;
  columns: DatasetColumn[];
}

/**
 * Narrow ONE dataset's view to a version and/or dispatch before the query runs.
 *
 * `version` and `dt` are the partition columns, so folding them into the view PRUNES partitions
 * rather than filtering rows — `--dt 2026-08-03` reads that day's directories and nothing else.
 * They exist only on actor output; a standalone list has neither column, so asking is refused
 * rather than failing deep inside DuckDB with a missing-column error.
 */
export interface QueryScope {
  name: string;
  version?: string;
  dt?: string;
}

interface EngineOptions {
  lake?: Partial<LakeConfig>;
  /** Scope one dataset's view (the CLI's `--version` / `--dt`). */
  scope?: QueryScope;
  /**
   * Apply the hardening. Default true, and only ever false in tests: the hardening blocks
   * LocalFileSystem, and the test lake IS a local directory. The hardening itself is covered
   * by its own test, which asserts each control against a memory-only connection.
   */
  harden?: boolean;
}

function lit(v: string): string {
  return `'${v.replace(/'/g, "''")}'`;
}

/** `memory_limit` for the workbench connection, UNDER what the API's cgroup leaves spare. */
function queryMemoryLimit(): string {
  return process.env.KONTRA_QUERY_MEMORY_LIMIT ?? '192MB';
}

interface CachedEngine {
  /** What the catalog looked like when this connection attached — see {@link catalogGeneration}. */
  generation: string;
  conn: Promise<DuckDBConnection>;
}

const engines = new Map<string, CachedEngine>();

/**
 * How many hardened connections this process has OPENED. A counter, not a gauge: it is the
 * measure of how often the cache was missed, which is the only thing worth watching about it.
 *
 * IT EXISTS BECAUSE THE CACHE NOW HAS A CONDITION. "One connection per (catalog, dataPath)" was a
 * property a reader could check by reading the function; "…and per catalog generation" is one that
 * needs observing, and an engine rebuilt per QUERY rather than per COMMIT costs ~260 ms a page
 * while still answering correctly — a regression nothing else would catch.
 */
let opens = 0;
export function queryEngineOpens(): number {
  return opens;
}

/** Test seam — drop cached engines so a fresh config attaches cleanly. */
export function resetQueryEngines(): void {
  engines.clear();
}

/**
 * A token that changes whenever the lake commits — `''` for a catalog this cannot go stale
 * against.
 *
 * WHY A CACHED READ CONNECTION GOES STALE AT ALL, which is not obvious and cost a measurement to
 * find. A Postgres catalog is a SERVER: every statement asks it again, so a connection opened an
 * hour ago sees a Dataset written a second ago. A FILE catalog is a database this process opened,
 * and DuckDB does not re-read a file another handle has written — measured on DuckLake 1.5.4, one
 * process, writer and reader on separate instances: the writer inserts and commits, and the
 * reader's `SELECT count(*)` keeps answering the count it saw at ATTACH, forever. DETACH +
 * re-ATTACH answers the new count, so the staleness is bound to the attached handle and nothing
 * else.
 *
 * IT IS THE ROWS, NOT THE TABLE LIST, and that is what made it hard to see. A table created after
 * the ATTACH resolves fine — `runQuery`'s "sees a dataset created after the engine was opened" has
 * always passed, and still passes with this whole mechanism disabled. What freezes is the
 * SNAPSHOT: a Dataset that existed when the connection attached keeps the row count it had then,
 * so the workbench stays up, stays fast, answers every query, and quietly stops counting.
 *
 * AND THE RE-ATTACH IS THE ONE THING THIS CONNECTION CANNOT DO. `disabled_filesystems` is set
 * before the operator's SQL ever runs, and a local catalog file is LocalFileSystem: measured, a
 * re-ATTACH answers `Permission Error: File system LocalFileSystem has been disabled by
 * configuration`. Lifting that to let the reader refresh would hand every workbench query
 * `read_text('/etc/passwd')` back, which is a trade nobody should make for a cache. So the
 * connection is REBUILT instead, on a fresh instance, whenever this token moves.
 *
 * THE TOKEN IS THE CATALOG'S FILES, NOT THE CATALOG'S CONTENT, because content is exactly what
 * cannot be read without a fresh connection. `<catalog>` alone is not enough: DuckDB commits into
 * the write-ahead log and leaves the main file's mtime and size untouched until a checkpoint —
 * measured, three commits, `cat.ducklake` frozen at 12288 bytes while `cat.ducklake.wal` grew
 * 12342 → 14013 → 15684. Both files, size and nanosecond mtime, is what covers a commit, a
 * checkpoint, and the truncation between them.
 *
 * THE COST, MEASURED, so nobody discovers it as a mystery: a rebuild is ~260 ms (a DuckDB
 * instance plus three extension loads), and it happens once per COMMIT, not once per query. An
 * idle lake pays nothing; a workbench page read taken while a Run is materializing into the same
 * lake pays it once. If that ever needs to be zero, the way out is `enable_external_access=false`
 * with `allowed_paths`/`allowed_directories` naming the catalog and the data path, which permits
 * a re-ATTACH while still refusing `/etc/passwd` and `169.254.169.254` — verified to work, and
 * left undone here because it also refuses the `COPY … TO 's3://…'` that {@link exportQuery}
 * needs, and that is a bigger change than this slice.
 */
function catalogGeneration(catalog: string): string {
  const file = catalogFilePath(catalog);
  if (file === null) return '';
  return [file, `${file}.wal`]
    .map((p) => {
      try {
        const s = statSync(p, { bigint: true });
        return `${s.size}@${s.mtimeNs}`;
      } catch {
        return '-'; // absent is a state like any other: a catalog appearing IS a change
      }
    })
    .join(',');
}

/**
 * The hardened connection, one per (catalog, dataPath) — and per catalog GENERATION, because a
 * file catalog's reader freezes at ATTACH ({@link catalogGeneration}).
 *
 * ORDER MATTERS. Credentials and the ATTACH have to be established BEFORE the filesystem is
 * disabled and the configuration locked, because both of those need the very access being
 * revoked. Lock last, and nothing the operator types can reach back past it.
 */
async function engine(store: ObjectStore, cfg: LakeConfig, harden: boolean): Promise<DuckDBConnection> {
  const key = `${cfg.catalog}\0${cfg.dataPath}\0${harden}`;
  const generation = catalogGeneration(cfg.catalog);
  const cached = engines.get(key);
  // A SUPERSEDED CONNECTION IS DROPPED, NOT CLOSED. `closeSync` on a handle that another in-flight
  // query is still reading would take that query down with it, and dropping the reference is
  // enough: measured over 40 rebuilds, RSS oscillates and returns (119 MB before, 148 MB after a
  // collection), so the addon frees the native instance on GC. Same posture as
  // `resetQueryEngines`, which has always simply cleared the map.
  if (cached && cached.generation === generation) return cached.conn;

  opens += 1;
  const opening = (async () => {
    const c = await (await DuckDBInstance.create()).connect();
    await c.run(`SET memory_limit='${queryMemoryLimit()}'`);
    await c.run('SET threads=1');
    await c.run('INSTALL ducklake; LOAD ducklake;');
    await c.run('INSTALL json; LOAD json;');
    if (cfg.catalog.startsWith('postgres:')) await c.run('INSTALL postgres; LOAD postgres;');
    if (cfg.s3) {
      const endpoint = store.endpoint ?? '';
      await c.run('INSTALL httpfs; LOAD httpfs;');
      await c.run(`SET s3_endpoint=${lit(endpoint.replace(/^https?:\/\//, '').replace(/\/+$/, ''))}`);
      await c.run("SET s3_url_style='path'");
      await c.run(`SET s3_use_ssl=${endpoint.startsWith('https')}`);
      await c.run(`SET s3_access_key_id=${lit(store.accessKey)}`);
      await c.run(`SET s3_secret_access_key=${lit(store.secretKey)}`);
      await c.run(`SET s3_region=${lit(store.region)}`);
    }
    // READ_ONLY is the write boundary. It is enforced by DuckDB on statement type, so it holds
    // for statements this module never anticipated.
    await c.run(
      `ATTACH IF NOT EXISTS 'ducklake:${cfg.catalog}' AS ${LAKE} ` +
        `(DATA_PATH ${lit(cfg.dataPath)}, READ_ONLY)`
    );
    if (harden) {
      // HTTPFileSystem is disabled ALONGSIDE LocalFileSystem, and that pairing is the whole
      // SSRF defence. httpfs has to be loaded — S3 is built on it — and while it is loaded,
      // `read_csv('http://169.254.169.254/…')` is a fully functional outbound GET from this
      // container. Measured on the live service before this line existed: it came back HTTP
      // 404, meaning the request left the box. S3FileSystem is registered separately, so
      // banning HTTPFileSystem closes arbitrary http/https reads while `s3://` keeps working —
      // verified against the real lake, not assumed.
      await c.run("SET disabled_filesystems='LocalFileSystem,HTTPFileSystem'");
      await c.run('SET allow_community_extensions=false');
      await c.run('SET autoinstall_known_extensions=false');
      await c.run('SET autoload_known_extensions=false');
      await c.run('SET lock_configuration=true'); // must be last
    }
    return c;
  })();

  engines.set(key, { generation, conn: opening });
  try {
    return await opening;
  } catch (err) {
    engines.delete(key); // never cache a rejection — a transient catalog blip is not permanent
    throw err;
  }
}

/**
 * The bare-name views built on ONE connection, and the catalog state they were built from.
 *
 * A WeakMap KEYED ON THE CONNECTION, which is what makes the generation half of the invalidation
 * free rather than something this code has to remember. The views are objects inside that
 * connection's in-memory database; when {@link catalogGeneration} moves, {@link engine} builds a
 * new connection, and a new connection has no entry here and none of the views. So "rebuild the
 * layer whenever the engine is rebuilt" is not a rule enforced anywhere — it is the only thing
 * that can happen. Nothing to leak either: the entry dies with the connection it describes.
 */
interface ViewLayer {
  /** {@link lakeFingerprint} when these views were built. */
  fingerprint: string;
  /** What {@link listDatasets} answered then — reused, so a hit costs no metadata query. */
  datasets: DatasetInfo[];
  /** The view names created, so one that has since vanished can be DROPPED rather than left. */
  names: string[];
  /** {@link scopeKey} of the scope currently folded into one of those views; `''` for none. */
  scopeKey: string;
}

const viewLayers = new WeakMap<DuckDBConnection, ViewLayer>();

/**
 * How many times the bare-name layer has been BUILT, as opposed to reused.
 *
 * The companion to {@link queryEngineOpens}, and it exists for the same reason: the layer used to
 * be rebuilt unconditionally, so there was nothing to observe. Now there is a condition, and a
 * condition that silently stops holding costs `listDatasets` (~1 s on the live catalog) plus one
 * `CREATE OR REPLACE VIEW` per dataset (152 of them) on EVERY query, while still answering
 * correctly — a regression no assertion about results would catch.
 */
let viewBuilds = 0;
export function viewLayerBuilds(): number {
  return viewBuilds;
}

/**
 * What the lake looks like RIGHT NOW, cheaply enough to ask before every query.
 *
 * TWO PROBES, BECAUSE THE TWO CATALOG BACKENDS GO STALE IN OPPOSITE WAYS and neither probe alone
 * covers both. They are complementary rather than redundant:
 *
 *   the TABLE SET (`duckdb_tables()`) moves when a Dataset is created, dropped or renamed. On a
 *   POSTGRES catalog this is the whole mechanism — every statement asks the server again, so a
 *   table written a second ago is visible here immediately, and {@link catalogGeneration} answers
 *   `''` forever because there is no file to stat.
 *
 *   the SNAPSHOT ID moves on every commit, including one that only adds a partition to a table
 *   that already existed — which the table set cannot see. That is what keeps `querySchema`'s
 *   `version`/`dt` from going stale on a Postgres catalog while the sidebar is open.
 *
 * On a FILE catalog both probes are frozen at ATTACH, exactly as {@link catalogGeneration}
 * describes — and that is precisely the case where the generation has already moved and the
 * connection this is running on no longer exists. Each backend is covered by the mechanism that
 * can see it.
 *
 * BEST-EFFORT ON THE SNAPSHOT HALF, AND IT FAILS TOWARDS REBUILDING. If the metadata catalog is
 * not attached under the name this expects, the probe answers a token that cannot match a cached
 * one, so the layer is rebuilt — the old behaviour, which is slow and right, rather than an
 * exception thrown from the middle of a query route.
 */
async function lakeFingerprint(c: DuckDBConnection, cfg: LakeConfig): Promise<string> {
  const tables = await c.runAndReadAll(
    `SELECT schema_name, table_name FROM duckdb_tables() ` +
      `WHERE database_name = ${lit(LAKE)} ORDER BY schema_name, table_name`
  );
  const names = tables
    .getRows()
    .map((r) => `${String(r[0])}.${String(r[1])}`)
    .join(',');

  let snapshot: string;
  try {
    const res = await c.runAndReadAll(
      `SELECT max(snapshot_id) FROM ${META}.${cfg.metaSchema}.ducklake_snapshot`
    );
    snapshot = String(res.getRows()[0]?.[0] ?? '');
  } catch {
    // Unattached, renamed, or a backend that does not keep this table: answer something no cached
    // fingerprint can equal, so the caller rebuilds rather than trusting a probe that did not run.
    snapshot = `?${viewBuilds}`;
  }
  return `${snapshot}\0${names}`;
}

/** The identity of a scope, for comparing the one a layer carries against the one asked for. */
function scopeKey(scope?: QueryScope): string {
  if (!scope || (!scope.version && !scope.dt)) return '';
  return `${scope.name}\0${scope.version ?? ''}\0${scope.dt ?? ''}`;
}

/**
 * Expose every dataset as a BARE NAME in the default schema, so an operator writes
 * `FROM crawl4ai` rather than `FROM lake.output.crawl4ai`.
 *
 * This is the same name resolution `kontra dataset query` uses — standalone wins a collision,
 * because someone who just loaded a list means that list — so a query drafted in the workbench
 * runs unchanged in the CLI, and can be pasted into `kontra dispatch --query`.
 *
 * The views live in the writable in-memory database, NOT in the read-only lake.
 *
 * ── IT USED TO BE REBUILT PER QUERY, AND THE REASON GIVEN WAS GOOD ──────────────────────────────
 *
 * The comment this replaces said: *"Rebuilt per query rather than cached: views are catalog-only
 * (no file is opened), a dispatch that finishes mid-session shows up immediately, and there is no
 * invalidation to get wrong."* Every clause of that is true. What it did not price is the scale it
 * would meet — on the live install the rebuild is `listDatasets` (MEASURED around a second on its
 * own) plus 152 `CREATE OR REPLACE VIEW` statements, paid by `SELECT 1` as surely as by a scan,
 * which made the query surface feel like a slow protocol when the cost was ours.
 *
 * So the trade is taken the other way and the invalidation is the work: {@link lakeFingerprint}
 * for what changed inside the catalog, connection identity for what changed about the catalog
 * itself. A dispatch that finishes mid-session still shows up on the next query, which is the
 * property the rebuild existed to protect and the one the tests hold this to.
 */
async function refreshViews(
  c: DuckDBConnection,
  store: ObjectStore,
  lake: Partial<LakeConfig>,
  cfg: LakeConfig,
  scope?: QueryScope
): Promise<DatasetInfo[]> {
  const fingerprint = await lakeFingerprint(c, cfg);
  const cached = viewLayers.get(c);
  if (cached && cached.fingerprint === fingerprint) {
    await rescope(c, cached, scope);
    return cached.datasets;
  }

  viewBuilds += 1;
  const datasets = await listDatasets(store, {}, lake);
  await reattachIfStale(c, cfg, datasets);
  const seen = new Set<string>();
  // STANDALONE FIRST, so it wins a name collision — `cli/dataset_test.go` asserts
  // "resolution order must be [standalone, output]" and `cli/dataset.go` gives the reason:
  // an operator who just `dataset create`d a list means that list.
  //
  // THIS SORT USED TO SAY `a.kind.localeCompare(b.kind)` AND MEANT THE OPPOSITE. The kinds are
  // spelled `output` and `standalone`, `o` sorts before `s`, and the first `seen` wins — so the
  // console resolved a collision to output while the CLI resolved it to standalone, and a query
  // drafted in the workbench answered a different question when pasted into `kontra dataset
  // query`. Ranked explicitly rather than re-sorted, so renaming a kind cannot invert it again.
  const rank = (k: DatasetInfo['kind']): number => (k === 'standalone' ? 0 : 1);
  for (const d of [...datasets].sort((a, b) => rank(a.kind) - rank(b.kind))) {
    if (seen.has(d.name)) continue;
    seen.add(d.name);
    await c.run(
      `CREATE OR REPLACE VIEW "${d.name.replace(/"/g, '""')}" AS ` +
        `SELECT * FROM ${LAKE}.${schemaOf(d.kind)}."${d.name.replace(/"/g, '""')}"`
    );
  }

  // A DATASET THAT HAS GONE TAKES ITS VIEW WITH IT. `CREATE OR REPLACE` only ever adds, so a
  // dropped or renamed Dataset used to leave a view standing over a lake table that no longer
  // exists — and the operator got DuckDB's "table does not exist" naming the LAKE table, for a
  // bare name `kontra dataset list` had already stopped showing. Dropping it means the name stops
  // resolving, which is the honest answer.
  for (const name of cached?.names ?? []) {
    if (!seen.has(name)) await c.run(`DROP VIEW IF EXISTS "${name.replace(/"/g, '""')}"`);
  }

  const layer: ViewLayer = {
    // Re-probed rather than reused: `reattachIfStale` may have just DETACHed and re-ATTACHed, and
    // a layer stamped with the pre-reattach fingerprint would miss on the very next query.
    fingerprint: await lakeFingerprint(c, cfg),
    datasets,
    names: [...seen],
    scopeKey: '',
  };
  viewLayers.set(c, layer);
  await rescope(c, layer, scope);
  return datasets;
}

/**
 * Move a cached layer from the scope it carries to the one this query asked for.
 *
 * THE PART THAT WOULD HAVE BEEN A SILENT BUG. `applyScope` narrows ONE dataset's view with a
 * partition-pruning WHERE, and the per-query rebuild used to wash that out for free — the next
 * query recreated every view unscoped before doing anything else. A cached layer has no such
 * eraser, so a `--dt 2026-08-03` query followed by an unscoped one would have answered the
 * unscoped question with the scoped view still in place: fewer rows, no error, no indication.
 * Two statements at most, and only when the scope actually differs.
 */
async function rescope(c: DuckDBConnection, layer: ViewLayer, scope?: QueryScope): Promise<void> {
  const want = scopeKey(scope);
  if (want === layer.scopeKey) return;

  if (layer.scopeKey) {
    const prev = layer.scopeKey.split('\0')[0]!;
    const d = layer.datasets.find((x) => x.name === prev);
    if (d) {
      const q = d.name.replace(/"/g, '""');
      await c.run(
        `CREATE OR REPLACE VIEW "${q}" AS SELECT * FROM ${LAKE}.${schemaOf(d.kind)}."${q}"`
      );
    }
  }
  // Set before the apply, not after: `applyScope` throws for an unknown name or a standalone
  // list, and a layer that says it is unscoped while carrying a scope is the one state this
  // cache must never be in. Overstating the scope costs one redundant statement; understating it
  // costs a wrong answer.
  layer.scopeKey = want;
  if (scope && want) await applyScope(c, layer.datasets, scope);
}

/** Redefine one dataset's view with a partition-pruning WHERE. */
async function applyScope(
  c: DuckDBConnection,
  datasets: readonly DatasetInfo[],
  scope: QueryScope
): Promise<void> {
  const target = datasets.find((d) => d.name === scope.name);
  if (!target) throw new Error(`no dataset named "${scope.name}"`);
  if (target.kind !== 'output') {
    throw new Error(
      `--version/--dt apply to actor OUTPUT only; "${scope.name}" is a standalone list with neither`
    );
  }
  const where: string[] = [];
  if (scope.version) where.push(`version = ${lit(scope.version)}`);
  // PREFIX match: a date selects a whole day, a full stamp selects one dispatch.
  if (scope.dt) where.push(`dt LIKE ${lit(scope.dt + '%')}`);
  const q = target.name.replace(/"/g, '""');
  await c.run(
    `CREATE OR REPLACE VIEW "${q}" AS SELECT * FROM ${LAKE}.${schemaOf(target.kind)}."${q}" ` +
      `WHERE ${where.join(' AND ')}`
  );
}

/**
 * Re-ATTACH when this connection's view of the catalog has fallen behind the authority's.
 *
 * A long-lived attachment can hold a SNAPSHOT rather than track the catalog: measured, a
 * Postgres-backed catalog shows new tables to an existing READ_ONLY reader immediately, while a
 * file-backed one does not. Without this, whichever backend snapshots would leave a dispatch
 * that materialized mid-session invisible until the API restarted — and it would be invisible
 * silently, as "table does not exist" for a dataset `kontra dataset list` is happily showing.
 *
 * Cheap: one catalog query, and a reattach only when the two actually disagree. Best-effort by
 * design — if the reattach fails we still answer from the current attachment rather than
 * turning a staleness problem into an outage.
 */
async function reattachIfStale(
  c: DuckDBConnection,
  cfg: LakeConfig,
  datasets: readonly DatasetInfo[]
): Promise<void> {
  if (datasets.length === 0) return;
  const res = await c.runAndReadAll(
    `SELECT table_name FROM duckdb_tables() WHERE database_name = ${lit(LAKE)}`
  );
  const visible = new Set(res.getRows().map((r) => String(r[0])));
  if (datasets.every((d) => visible.has(d.name))) return;
  try {
    await c.run(`DETACH ${LAKE}`);
    await c.run(
      `ATTACH 'ducklake:${cfg.catalog}' AS ${LAKE} (DATA_PATH ${lit(cfg.dataPath)}, READ_ONLY)`
    );
  } catch {
    /* keep serving from the existing attachment */
  }
}

/**
 * Translate a DuckDB failure into something an operator can act on.
 *
 * The important one is the spill: with LocalFileSystem disabled, a query that outgrows
 * `memory_limit` surfaces as "File system LocalFileSystem has been disabled by configuration",
 * which reads like a misconfiguration rather than "your query is too big for this box".
 */
function friendlyError(err: unknown): Error {
  const msg = err instanceof Error ? err.message : String(err);
  // A file-backed catalog that was never written cannot be opened READ_ONLY. That is the state
  // of a fresh install, and "Cannot open database in read-only mode" reads like a broken
  // deployment rather than "nothing has run yet".
  if (/in read-only mode: database does not exist/i.test(msg)) {
    return new Error('no datasets yet — run a workflow, or load a list with `kontra dataset create`');
  }
  // One DuckDB error, two very different causes: the query tried to touch a blocked filesystem,
  // OR it outgrew `memory_limit` and tried to SPILL to one. They are indistinguishable from the
  // message, so say both — an earlier version reported a blocked `read_text('/etc/passwd')` as
  // "needs more working memory", which is a confident, wrong answer.
  if (/LocalFileSystem has been disabled/i.test(msg)) {
    return new Error(
      'blocked: this query either read a local file (not permitted — the workbench reads ' +
        `datasets only) or needed more working memory than it is allowed (${queryMemoryLimit()}), ` +
        'which it may not spill to disk. Narrow it with WHERE/LIMIT, or run it on your own ' +
        'machine: kontra dataset query <name> --local --sql "…"'
    );
  }
  if (/HTTPFileSystem has been disabled/i.test(msg)) {
    return new Error(
      'blocked: the workbench cannot fetch arbitrary URLs. Query a dataset by name — ' +
        '`kontra dataset list` shows what exists.'
    );
  }
  if (/Cannot execute statement of type/i.test(msg)) {
    return new Error(`${msg.split('\n')[0]} — the workbench catalog is attached READ_ONLY.`);
  }
  return new Error(msg);
}

/**
 * Run one block of operator SQL.
 *
 * `limit`/`offset` are applied by WRAPPING the query rather than by appending, so an SQL that
 * already ends in its own LIMIT still pages correctly. A query with no ORDER BY has no defined
 * row order, so paging it is only as stable as the query itself — the workbench says so.
 */
/**
 * The one place operator SQL is admitted — trim, then judge.
 *
 * EVERY SINK BELOW COMPOSES THIS TEXT WITH SQL OF ITS OWN (`SELECT * FROM (<text>) LIMIT n`), and
 * that composition is the vulnerability: the text can close the parenthesis and open a second
 * statement, which is how `COPY … TO 's3://elsewhere'` reached a connection holding the install's
 * credentials. `assertSingleRead` refuses a second statement outright, so the breakout has nowhere
 * to live regardless of what the wrapper happens to append today.
 *
 * `readonlySql` existed for a Postgres-wire front door that was never built, and until this function
 * its only importer was its own test.
 */
function readOnlyText(sql: string): string {
  const text = sql.trim().replace(/;\s*$/, '');
  if (!text) throw new Error('no SQL to run');
  const verdict = assertSingleRead(text);
  if (!verdict.allowed) throw new ReadOnlyRefusal(verdict);
  return text;
}

/** A refusal from the gate, distinguishable from a DuckDB error so a route can answer 400. */
export class ReadOnlyRefusal extends Error {
  constructor(readonly verdict: Verdict) {
    super(verdict.reason);
    this.name = 'ReadOnlyRefusal';
  }
}

export async function runQuery(
  store: ObjectStore,
  sql: string,
  opts: { limit?: number; offset?: number } & EngineOptions = {}
): Promise<QueryResult> {
  const text = readOnlyText(sql);
  if (!lakeEnabled(store, opts.lake ?? {})) throw new Error('no lake configured');

  const cfg = resolveLakeConfig(store, opts.lake ?? {});
  let c: DuckDBConnection;
  try {
    c = await engine(store, cfg, opts.harden ?? true);
    await refreshViews(c, store, opts.lake ?? {}, cfg, opts.scope);
  } catch (err) {
    throw friendlyError(err);
  }

  const limit = Math.min(Math.max(Math.trunc(opts.limit ?? QUERY_DEFAULT_ROWS), 1), QUERY_MAX_ROWS);
  const offset = Math.max(Math.trunc(opts.offset ?? 0), 0);
  const started = Date.now();
  try {
    // One row beyond the cap, so `truncated` is a fact rather than a guess from a full page.
    const res = await c.runAndReadAll(
      `SELECT * FROM (${text}) AS _q LIMIT ${limit + 1} OFFSET ${offset}`
    );
    const rows = res.getRows().map((r) => r.map(jsonSafe));
    const truncated = rows.length > limit;
    return {
      columns: res.columnNames().map((name, i) => ({ name, type: String(res.columnTypes()[i]) })),
      rows: truncated ? rows.slice(0, limit) : rows,
      elapsedMs: Date.now() - started,
      truncated,
    };
  } catch (err) {
    throw friendlyError(err);
  }
}

/** One chunk of a streamed result, plus the head that precedes it. */
export interface QueryStreamSink {
  head: (columns: DatasetColumn[]) => void | Promise<void>;
  rows: (rows: unknown[][]) => void | Promise<void>;
}

/**
 * Operator SQL with NO ROW CEILING, handed out as it arrives (issue 02).
 *
 * ── WHY THE CAP EXISTED AND WHY IT CAN GO ───────────────────────────────────────────────────────
 *
 * {@link runQuery} answers a whole JSON body, so every row it returns is alive in V8 at once, and
 * `QUERY_MAX_ROWS` is what stopped a scan of a real Dataset from becoming the API's heap. The
 * ceiling was never a statement about the lake — MEASURED against the same DuckLake through
 * Arrow Flight, the widest table on this install returns 29,825 rows in 316 ms — it was a
 * statement about buffering. So the fix is to stop buffering rather than to raise the number:
 * `conn.stream()` yields one chunk at a time and retains nothing, each chunk is written to the
 * socket and dropped, and the peak is one chunk rather than one result.
 *
 * This is the same `stream()`/`fetchChunk()` pair the materializer uses to page unit refs, and it
 * is here for the same reason it is there — not for speed, but so that the ceiling on what can be
 * answered stops being the size of a Node heap.
 *
 * ── WHAT DOES **NOT** MOVE, AND THE REASON IS THE WHOLE SLICE ───────────────────────────────────
 *
 * Execution stays on THIS connection — hardened, `READ_ONLY`, `disabled_filesystems`,
 * `lock_configuration` — and does not move to Porter, even though Porter is what made the
 * measurement above. `porter serve` takes twelve flags and not one of them is a sandbox control,
 * so a statement that is merely read-only is not thereby safe there: `read_text('/etc/passwd')`
 * and `read_csv('http://169.254.169.254/…')` are both SELECTs, and the header of this file records
 * that the second one was measured LEAVING THE BOX before `HTTPFileSystem` was banned. Delegating
 * operator-typed SQL would have traded every control in this module for a wire format.
 *
 * Porter still earns its place on SQL this codebase composed, where there is no statement from a
 * person to defend against. `docs/THREAT_MODEL.md` §4 carries the decision.
 *
 * ── OFFSET IS WRAPPED, LIMIT IS THE CALLER'S ────────────────────────────────────────────────────
 *
 * An `offset` still wraps the statement, so resuming a long read does not depend on the query
 * having its own. There is deliberately no `limit`: a caller that wants fewer rows writes one, and
 * a caller that wants all of them is the reason this function exists.
 */
export async function streamQuery(
  store: ObjectStore,
  sql: string,
  opts: { offset?: number } & EngineOptions,
  sink: QueryStreamSink
): Promise<{ rows: number; elapsedMs: number }> {
  const text = readOnlyText(sql);
  if (!lakeEnabled(store, opts.lake ?? {})) throw new Error('no lake configured');

  const cfg = resolveLakeConfig(store, opts.lake ?? {});
  let c: DuckDBConnection;
  try {
    c = await engine(store, cfg, opts.harden ?? true);
    await refreshViews(c, store, opts.lake ?? {}, cfg, opts.scope);
  } catch (err) {
    throw friendlyError(err);
  }

  const offset = Math.max(Math.trunc(opts.offset ?? 0), 0);
  const started = Date.now();
  let n = 0;
  try {
    const res = await c.stream(
      offset > 0 ? `SELECT * FROM (${text}) AS _q OFFSET ${offset}` : text
    );
    const types = res.columnTypes();
    await sink.head(res.columnNames().map((name, i) => ({ name, type: String(types[i]) })));
    for (;;) {
      const chunk = await res.fetchChunk();
      if (!chunk || chunk.rowCount === 0) break;
      // Column-wise out of the chunk and row-wise into the sink: `getColumnValues` is one call per
      // column rather than one per cell, and the row shape is what every consumer already reads.
      const cols = Array.from({ length: chunk.columnCount }, (_, i) => chunk.getColumnValues(i));
      const rows: unknown[][] = [];
      for (let i = 0; i < chunk.rowCount; i += 1) {
        rows.push(cols.map((v) => jsonSafe(v[i])));
      }
      n += rows.length;
      await sink.rows(rows);
    }
    return { rows: n, elapsedMs: Date.now() - started };
  } catch (err) {
    throw friendlyError(err);
  }
}

/**
 * Write the FULL result of a query to an object, and hand back its key.
 *
 * Export is deliberately not row-capped: "show me a page" and "give me the data" are different
 * questions, and the second is why an export button exists. It writes through `COPY … TO
 * 's3://…'` because that is the one filesystem the hardened connection still has — DuckDB
 * encodes the format, so Parquet costs no encoder in this codebase.
 *
 * The caller streams the object and deletes it; the key carries a caller-supplied token so two
 * concurrent exports cannot collide.
 */
export async function exportQuery(
  store: ObjectStore,
  sql: string,
  format: ExportFormat,
  token: string,
  opts: EngineOptions = {}
): Promise<{ key: string }> {
  const text = readOnlyText(sql);
  if (!EXPORT_FORMATS.includes(format)) throw new Error(`unsupported export format ${format}`);
  if (!lakeEnabled(store, opts.lake ?? {})) throw new Error('no lake configured');

  const cfg = resolveLakeConfig(store, opts.lake ?? {});
  const c = await engine(store, cfg, opts.harden ?? true);
  await refreshViews(c, store, opts.lake ?? {}, cfg, opts.scope);

  // `dataPath` is the bucket root the lake already writes under, so the export lands beside it
  // rather than needing a second bucket or credential.
  const key = `exports/${token}.${format}`;
  const target = `${cfg.dataPath}${key}`;
  // jsonl is DuckDB's newline-delimited JSON: the same writer, minus the array wrapper.
  const copyOpts =
    format === 'jsonl' ? "FORMAT json, ARRAY false" : format === 'json' ? 'FORMAT json, ARRAY true' : `FORMAT ${format}`;
  try {
    await c.run(`COPY (${text}) TO ${lit(target)} (${copyOpts})`);
  } catch (err) {
    throw friendlyError(err);
  }
  return { key };
}

/** One page of a Dataset, as a claim-check ref. Never rows. */
export interface DatasetPage {
  /** A BareRef the actor's `kontra.fetch_blob` can dereference — same bucket, same prefix. */
  ref: { sha256: string; size: number; meta: Record<string, string> };
  /** How many units the page holds. */
  n: number;
  /** True when this page was short, i.e. the dataset is exhausted. */
  done: boolean;
}

/**
 * Read one page of a Dataset and write it to the CAS, returning a REF.
 *
 * This is what makes `A Dataset yields Batches` true: the caller's workflow never sees a row,
 * so a 40k-unit dataset costs its history a handful of ~110-byte refs instead of the payload.
 * `runQuery` cannot serve this — it returns positional rows through an inline 5000-row cap
 * sized for a JSON HTTP body, and a page must be JSON OBJECTS because objects are what a unit
 * is everywhere else in the system.
 *
 * ORDER_BY IS REQUIRED, and that is a correctness decision rather than an ergonomic one. A
 * materialized dataset stamps no row id (`insertBatch` writes version/dt/node/run_id/
 * run_started_at and `hive_partitioning=false` strips the filename), so LIMIT/OFFSET over it
 * has no defined meaning: two pages could overlap, or skip units entirely, and nothing would
 * raise. Defaulting to `ORDER BY ALL` is worse than refusing — this engine runs `threads=1`
 * with a 192MB `memory_limit` and LocalFileSystem disabled, so a large sort cannot spill and
 * fails as "LocalFileSystem has been disabled", which names neither the cause nor the fix.
 *
 * The bytes go through `putContentAddressed`, NOT `exportQuery`'s `COPY … TO`: that writes an
 * uncontent-addressed object inside the lake prefix which only the export route's `finally`
 * cleans up, so a crash would leave strays sitting beside the datasets.
 */
export async function pageDataset(
  store: ObjectStore,
  sel: { sql: string; orderBy: string; limit: number; offset: number } & EngineOptions
): Promise<DatasetPage> {
  const text = sel.sql.trim().replace(/;\s*$/, '');
  if (!text) throw new Error('no SQL to page');
  if (!sel.orderBy || !sel.orderBy.trim()) {
    throw new Error(
      'paging a dataset needs an explicit order_by: a materialized dataset stamps no row id, ' +
        'so LIMIT/OFFSET over it has no defined row order and two pages may overlap or skip units'
    );
  }
  if (!lakeEnabled(store, sel.lake ?? {})) throw new Error('no lake configured');

  const cfg = resolveLakeConfig(store, sel.lake ?? {});
  let c: DuckDBConnection;
  try {
    c = await engine(store, cfg, sel.harden ?? true);
    await refreshViews(c, store, sel.lake ?? {}, cfg, sel.scope);
  } catch (err) {
    throw friendlyError(err);
  }

  const limit = Math.max(Math.trunc(sel.limit), 1);
  const offset = Math.max(Math.trunc(sel.offset ?? 0), 0);
  try {
    // One row beyond the page, so `done` is a fact rather than a guess from a full page —
    // the same trick runQuery uses for `truncated`, and the termination condition Temporal's
    // own Batch Iterator pattern relies on.
    const res = await c.runAndReadAll(
      `SELECT * FROM (${text}) AS _q ORDER BY ${sel.orderBy} LIMIT ${limit + 1} OFFSET ${offset}`
    );
    const names = res.columnNames();
    const raw = res.getRows();
    const done = raw.length <= limit;
    const rows = done ? raw : raw.slice(0, limit);
    // Objects, not positional arrays: a unit is an object everywhere else, and the CLI's
    // `duckdb -json` path already produces this shape.
    const units = rows.map((r) => {
      const o: Record<string, unknown> = {};
      names.forEach((name, i) => {
        o[name] = jsonSafe(r[i]);
      });
      return o;
    });
    const body = Buffer.from(JSON.stringify(units), 'utf8');
    const sha = await store.putContentAddressed(body);
    return {
      ref: {
        sha256: sha,
        size: body.length,
        // `kind: units` is the same discriminator the handler stamps, so a page and a Method's
        // output are indistinguishable downstream — which is the point.
        meta: { kind: 'units', n: String(units.length) },
      },
      n: units.length,
      done,
    };
  } catch (err) {
    throw friendlyError(err);
  }
}

/** Every dataset's columns — the editor's autocomplete source and the sidebar's tree. */
export async function querySchema(
  store: ObjectStore,
  opts: EngineOptions = {}
): Promise<QuerySchemaEntry[]> {
  if (!lakeEnabled(store, opts.lake ?? {})) return [];
  const cfg = resolveLakeConfig(store, opts.lake ?? {});
  const c = await engine(store, cfg, opts.harden ?? true);
  const datasets = await refreshViews(c, store, opts.lake ?? {}, cfg, opts.scope);

  const out: QuerySchemaEntry[] = [];
  const seen = new Set<string>();
  for (const d of datasets) {
    if (seen.has(d.name)) continue;
    seen.add(d.name);
    // DESCRIBE reads the catalog's column list; it opens no data file.
    const res = await c.runAndReadAll(
      `DESCRIBE SELECT * FROM ${LAKE}.${schemaOf(d.kind)}."${d.name.replace(/"/g, '""')}"`
    );
    out.push({
      kind: d.kind,
      name: d.name,
      version: d.version,
      dt: d.dt,
      columns: res.getRows().map((r) => ({ name: String(r[0]), type: String(r[1]) })),
    });
  }
  return out;
}

/**
 * ISO-8601 UTC, dropping a `.000` that carries no information: `2026-08-08T02:30:34Z`, and
 * `2026-08-08T02:30:34.250Z` when there really are sub-second digits.
 *
 * A naive DuckDB TIMESTAMP has no zone, and this stamps it `Z` on purpose: everything in kontra
 * is written UTC (actors use `time.gmtime`, the materializer stamps epoch micros), so UTC is the
 * true reading rather than an assumption. Verified against the library that the conversion does
 * not shift the wall clock — `TIMESTAMP '2026-08-08 02:30:34'` comes back `02:30:34Z`.
 */
function isoUtc(d: Date): string {
  const s = d.toISOString();
  return s.endsWith('.000Z') ? `${s.slice(0, -5)}Z` : s;
}

/**
 * Make a DuckDB cell JSON-serializable.
 *
 * Integers arrive as BigInt, and every non-primitive type arrives as a WRAPPER OBJECT — LIST and
 * STRUCT, but also TIMESTAMP, DATE, TIME, INTERVAL, DECIMAL and UUID. Only the first two were
 * handled, so the rest fell through to the generic branch and serialized their internals: a
 * timestamp column rendered as `{"micros":1786156234000000}` in the workbench, in
 * `kontra dataset query`, and in every API consumer. A UUID would have rendered `{"hugeint":…}`
 * and a DECIMAL `{"width":5,"scale":2,"value":125}` — same bug, not yet tripped over.
 *
 * ORDER MATTERS. The container checks (`items` / `entries`) come first because LIST and STRUCT
 * must be recursed into, not stringified; the scalar wrappers are all leaves. Recursive, because
 * actor output nests — a shallow pass leaves BigInts inside structs and the response fails to
 * serialize.
 */
function jsonSafe(v: unknown): unknown {
  if (typeof v === 'bigint') return Number(v);
  if (Array.isArray(v)) return v.map(jsonSafe);
  if (v && typeof v === 'object') {
    const items = (v as { items?: unknown[] }).items;
    if (Array.isArray(items)) return items.map(jsonSafe);
    const entries = (v as { entries?: unknown }).entries;
    if (entries && typeof entries === 'object') return jsonSafe(entries);

    // Temporal scalars -> ISO-8601 UTC. The library's own converters are used rather than
    // arithmetic on `micros`, so the epoch and the unit stay the library's problem.
    if (v instanceof DuckDBTimestampValue) return isoUtc(dateFromTimestampValue(v));
    if (v instanceof DuckDBTimestampTZValue) return isoUtc(dateFromTimestampTZValue(v));
    if (v instanceof DuckDBTimestampSecondsValue) return isoUtc(dateFromTimestampSecondsValue(v));
    if (v instanceof DuckDBTimestampMillisecondsValue) return isoUtc(dateFromTimestampMillisecondsValue(v));
    if (v instanceof DuckDBTimestampNanosecondsValue) return isoUtc(dateFromTimestampNanosecondsValue(v));
    if (v instanceof DuckDBDateValue) return isoUtc(dateFromDateValue(v)).slice(0, 10);

    // DECIMAL stays a NUMBER: JSON has no decimal type, and turning a numeric column into a
    // string would break every consumer that sorts or aggregates it. Lossy past 2^53, which is
    // the same trade the BigInt branch above already makes.
    if (v instanceof DuckDBDecimalValue) return doubleFromDecimalValue(v);

    // TIME, TIMETZ, INTERVAL, UUID, BLOB, BIT: each has a faithful toString, and none has a
    // sensible JSON shape beyond it.
    if (
      v instanceof DuckDBTimeValue ||
      v instanceof DuckDBTimeTZValue ||
      v instanceof DuckDBTimeNSValue ||
      v instanceof DuckDBIntervalValue ||
      v instanceof DuckDBUUIDValue ||
      v instanceof DuckDBBlobValue ||
      v instanceof DuckDBBitValue
    ) {
      return String(v);
    }

    const out: Record<string, unknown> = {};
    for (const [k, val] of Object.entries(v as Record<string, unknown>)) out[k] = jsonSafe(val);
    return out;
  }
  return v;
}

export { OUTPUT_SCHEMA, STANDALONE_SCHEMA };
