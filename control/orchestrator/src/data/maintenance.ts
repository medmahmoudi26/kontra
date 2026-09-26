/**
 * LAKE MAINTENANCE — the two thirds of it that expiry is not.
 *
 * `data/retention.ts` already owns EXPIRY: untagged Datasets past their TTL, swept on a schedule
 * armed at every boot, dry-run by default. It is careful, it has an incident behind it, and this
 * file does not touch it.
 *
 * What kontra has never had is the other two, and their absence is silent by construction:
 *
 *   COMPACTION       a Run that appends in small batches leaves a table made of small files, and
 *                    every later query pays for it — forever, because nothing ever merges them.
 *                    Nothing reports this; the table simply reads slower than it should.
 *   ORPHAN CLEANUP   a failed or superseded write leaves data files no snapshot references. No
 *                    query can reach them and no expiry removes them; storage grows and the bill
 *                    with it.
 *
 * ── THE ASYMMETRY THAT SHAPES THIS FILE ─────────────────────────────────────────────────────────
 *
 * DuckLake gives three of the four operations a `dry_run` parameter and **compaction none**:
 *
 *   ducklake_expire_snapshots(catalog, dry_run, versions, older_than)        dry_run ✓
 *   ducklake_delete_orphaned_files(catalog, dry_run, cleanup_all, older_than) dry_run ✓
 *   ducklake_cleanup_old_files(catalog, dry_run, cleanup_all, older_than)     dry_run ✓
 *   ducklake_merge_adjacent_files(catalog, …, min_file_size)                  dry_run ✗
 *
 * So a dry run cannot mean one thing across all four. For the three it means "tell DuckLake not to
 * act"; for compaction there is no such switch, and pretending otherwise would be a preview that
 * silently rewrote the lake. A compaction preview therefore COUNTS the small files it would merge
 * and calls nothing — which is honest, and is stated in the result rather than left to be assumed.
 *
 * ── A DELETE FREES NOTHING. THE FOUR-STEP CHAIN, AND THE TWO STEPS THAT WERE MISSING ────────────
 *
 * This file listed `ducklake_cleanup_old_files` in the block above and then did not implement it —
 * `orphans` calls `delete_orphaned_files`, which is a DIFFERENT function for a different problem
 * (files the catalog never knew about, not files it has retired). The gap meant `data/retention.ts`
 * could collect every expired Dataset on the box and free zero bytes, silently, forever.
 *
 * MEASURED ON A SCRATCH LAKE (3 runs × 20k rows, then `DELETE WHERE run_id = 'r1'`):
 *
 *     after 3 inserts        3 parquet, 236 KiB
 *     after DELETE r1        3 parquet, 236 KiB   ← the DELETE freed nothing
 *     after rewrite          3 parquet, 236 KiB
 *     after expire           3 parquet, 236 KiB   ← scheduled for deletion, still on disk
 *     after cleanup          2 parquet, 157 KiB   ← THIS is the step that frees disk
 *
 * And with the runs INTERLEAVED so every file is left partially dead, `cleanup` alone is not enough
 * either — a file two thirds alive is still referenced, so it stays whole:
 *
 *     DELETE + expire + cleanup   2 files, 315 KiB   ← a third of the rows dead, all still on disk
 *     + rewrite, then cleanup     1 file,  314 KiB   ← rewrite is what reclaims a PARTIAL delete
 *
 * So the chain is FOUR operations and the order is load-bearing:
 *
 *   1. DELETE                 (`data/retention.ts`) writes delete markers. Frees nothing.
 *   2. ducklake_rewrite_data_files   rewrites files whose deleted fraction crossed a threshold,
 *                             dropping the dead rows. Without it a partially-deleted file is kept
 *                             whole and the collected Run's bytes stay on disk forever.
 *   3. ducklake_expire_snapshots     retires the old versions still pointing at the pre-rewrite
 *                             files, which is what makes them unreferenced at all.
 *   4. ducklake_cleanup_old_files    unlinks them. The only step that changes a byte count.
 *
 * `runMaintenance` runs whatever ops it is given IN THE ORDER THE CHAIN NEEDS, not the order the
 * caller listed them, because a caller who asks for cleanup-then-expire has written a no-op and
 * would have no way to tell.
 */

import type { DuckDBConnection } from '@duckdb/node-api';

/**
 * The operations this module performs. The row DELETE itself is `data/retention.ts`'s and is not
 * here; everything that turns a DELETE into free space is.
 *
 * `rewrite` and `cleanup` are the two that were named in the header and never written — see the
 * measurements there for what their absence cost.
 */
export type MaintenanceOp = 'rewrite' | 'snapshots' | 'cleanup' | 'orphans' | 'compact';

/**
 * The order the chain must run in, whatever order a caller listed.
 *
 * REWRITE BEFORE EXPIRE BEFORE CLEANUP is not a style preference: each step only has work to do
 * because the one before it created it. Cleanup first unlinks nothing, because nothing has been
 * retired yet; expire before rewrite retires the snapshots that still hold the dead rows. A caller
 * who got the order wrong would see every operation "succeed" with a count of zero and conclude the
 * lake was already clean.
 *
 * Compaction runs LAST, on what is left after the dead rows are gone — merging first would carefully
 * pack data that the rewrite is about to throw away.
 */
const OP_ORDER: readonly MaintenanceOp[] = ['rewrite', 'snapshots', 'cleanup', 'orphans', 'compact'];

/**
 * How dead a data file must be before {@link rewrite} rewrites it, as a fraction of its rows.
 *
 * DuckLake's own default is 0.95 — near-total waste before anything is reclaimed. That is tuned for
 * a lake whose deletes are rare corrections; kontra's deletes are RETENTION, which removes whole
 * Runs from partitions several Runs share, so the steady state is files that are half dead and never
 * rewritten. A quarter is the point where rewriting costs less than carrying the corpse.
 */
const DEFAULT_DELETE_THRESHOLD = 0.25;

export interface MaintenanceOptions {
  /** Which operations to run. Empty runs nothing — a caller must choose. */
  ops: readonly MaintenanceOp[];
  /**
   * Report without changing anything. THE DEFAULT, and deliberately: every operation here deletes
   * or rewrites files, and this repo's retention sweep already defaults to a preview for the same
   * reason.
   */
  dryRun?: boolean;
  /**
   * Only touch things older than this. Guards against racing a live write — an orphan that is
   * "orphaned" because its commit has not landed yet is not an orphan.
   */
  olderThan?: Date;
  /** How dead a file must be before `rewrite` rewrites it. See {@link DEFAULT_DELETE_THRESHOLD}. */
  deleteThreshold?: number;
}

export interface MaintenanceResult {
  op: MaintenanceOp;
  dryRun: boolean;
  /** How many things the operation affected, or would affect. */
  count: number;
  /**
   * True when `dryRun` was asked for and the operation CANNOT simulate — compaction. The count is
   * then an estimate of what a real run would touch, and nothing was called.
   */
  estimated?: boolean;
  detail: string;
}

/** Anything older than this is fair game by default: a full day behind the newest write. */
const DEFAULT_OLDER_THAN_MS = 24 * 60 * 60 * 1000;

function olderThanSql(when: Date | undefined): string {
  const at = when ?? new Date(Date.now() - DEFAULT_OLDER_THAN_MS);
  return `TIMESTAMPTZ '${at.toISOString()}'`;
}

/**
 * Run the requested maintenance against an attached lake.
 *
 * `catalog` is the ATTACH alias, not a path — every DuckLake function here takes the alias.
 *
 * NOT AN AUTHORIZATION BOUNDARY. This deletes and rewrites files in whatever lake the connection
 * is attached to, on behalf of whoever asked. The caller MUST have established that they may — the
 * convention is stated here because a function that mutates on behalf of an identity it did not
 * check is safe exactly until somebody calls it from somewhere new.
 */
export async function runMaintenance(
  conn: DuckDBConnection,
  catalog: string,
  opts: MaintenanceOptions
): Promise<MaintenanceResult[]> {
  const dryRun = opts.dryRun ?? true;
  const out: MaintenanceResult[] = [];

  // SORTED INTO CHAIN ORDER, not run in the order asked — see {@link OP_ORDER}. Deduplicated too: a
  // caller who listed `cleanup` twice meant it once, and the second call would report zero and read
  // as "nothing left to free" rather than "already done".
  const asked = new Set(opts.ops);
  for (const op of OP_ORDER) {
    if (!asked.has(op)) continue;
    switch (op) {
      case 'rewrite':
        out.push(await rewrite(conn, catalog, dryRun, opts.deleteThreshold));
        break;
      case 'snapshots':
        out.push(await snapshots(conn, catalog, dryRun, opts.olderThan));
        break;
      case 'cleanup':
        out.push(await cleanup(conn, catalog, dryRun, opts.olderThan));
        break;
      case 'orphans':
        out.push(await orphans(conn, catalog, dryRun, opts.olderThan));
        break;
      case 'compact':
        out.push(await compact(conn, catalog, dryRun));
        break;
    }
  }
  return out;
}

async function rows(conn: DuckDBConnection, sql: string): Promise<Record<string, unknown>[]> {
  const reader = await conn.runAndReadAll(sql);
  return reader.getRowObjects() as Record<string, unknown>[];
}

/**
 * COMPACTION, and the one operation with no dry run.
 *
 * A preview counts the files a real call would merge and CALLS NOTHING. The count comes from ONE
 * query — never a count and a sum from two — because these tables are live: a lake being appended
 * to between two reads gives a number that was never true at any instant.
 */
async function compact(conn: DuckDBConnection, catalog: string, dryRun: boolean): Promise<MaintenanceResult> {
  if (dryRun) {
    // ONE QUERY, over the PUBLIC `ducklake_table_info` — not the internal
    // `__ducklake_metadata_<alias>` tables, which are an implementation detail and whose schema is
    // DuckLake's to change. A count and a sum taken separately would also be two reads of a lake
    // something may be appending to, giving a pair of numbers that was never true together.
    //
    // A table with MORE THAN ONE file is what `ducklake_merge_adjacent_files` can act on; a table
    // with one file has no adjacent files to merge, whatever their size.
    const [r] = await rows(
      conn,
      `SELECT count(*) FILTER (WHERE file_count > 1)::BIGINT AS tables,
              coalesce(sum(file_count) FILTER (WHERE file_count > 1), 0)::BIGINT AS files,
              coalesce(sum(file_size_bytes) FILTER (WHERE file_count > 1), 0)::BIGINT AS bytes
         FROM ducklake_table_info('${escapeLiteral(catalog)}')`
    );
    const files = Number(r?.files ?? 0);
    return {
      op: 'compact',
      dryRun: true,
      estimated: true,
      count: files,
      detail:
        `${files} file(s) across ${Number(r?.tables ?? 0)} table(s) are candidates ` +
        `(${Number(r?.bytes ?? 0)} bytes). DuckLake's merge has no dry run, so nothing was called.`,
    };
  }
  const merged = await rows(
    conn,
    `CALL ducklake_merge_adjacent_files('${escapeLiteral(catalog)}')`
  );
  return { op: 'compact', dryRun: false, count: merged.length, detail: `merged ${merged.length} group(s)` };
}

/**
 * REWRITE — the step that reclaims a PARTIAL delete, and the reason retention frees anything at all.
 *
 * A DuckLake `DELETE` does not touch the parquet; it records which rows are gone. A file whose rows
 * are half deleted is still a file the catalog references, so expiry cannot retire it and cleanup
 * cannot unlink it: the dead rows are carried forever. Rewriting drops them and leaves a smaller
 * file in place, which the following `snapshots` + `cleanup` then retire the original behind.
 *
 * THIS IS THE COMMON CASE HERE, not the exotic one. Retention deletes `WHERE run_id = …` from
 * partitions that several Runs share (`data/retention.ts` fix #5) — so a collected Run almost never
 * owns a whole file, and without this pass its bytes stay on disk after a sweep reports it collected.
 *
 * NO DRY RUN, LIKE COMPACTION. `ducklake_rewrite_data_files` takes no `dry_run`, so a preview COUNTS
 * what it would rewrite and calls nothing rather than quietly rewriting during a preview.
 */
async function rewrite(
  conn: DuckDBConnection,
  catalog: string,
  dryRun: boolean,
  threshold: number | undefined
): Promise<MaintenanceResult> {
  const t = threshold ?? DEFAULT_DELETE_THRESHOLD;
  if (dryRun) {
    // The PUBLIC view again, for the reason `compact` gives: the `__ducklake_metadata_*` tables are
    // DuckLake's to change. `ducklake_table_info` reports the deleted bytes a table is carrying, which
    // is what a rewrite would reclaim — it cannot report the per-FILE fraction the threshold applies
    // to, so this is stated as a candidate count, not a promise.
    const [r] = await rows(
      conn,
      `SELECT count(*) FILTER (WHERE delete_file_count > 0)::BIGINT AS tables,
              coalesce(sum(delete_file_count), 0)::BIGINT AS deletes
         FROM ducklake_table_info('${escapeLiteral(catalog)}')`
    );
    const tables = Number(r?.tables ?? 0);
    return {
      op: 'rewrite',
      dryRun: true,
      estimated: true,
      count: Number(r?.deletes ?? 0),
      detail:
        `${tables} table(s) carry deleted rows in ${Number(r?.deletes ?? 0)} delete file(s). ` +
        `DuckLake's rewrite has no dry run, so nothing was called; a real pass rewrites files at ` +
        `or past ${t} dead.`,
    };
  }
  const done = await rows(
    conn,
    `CALL ducklake_rewrite_data_files('${escapeLiteral(catalog)}', delete_threshold => ${Number(t)})`
  );
  return {
    op: 'rewrite',
    dryRun: false,
    count: done.length,
    detail: `rewrote ${done.length} file(s) at or past ${t} dead`,
  };
}

/**
 * CLEANUP — the only operation in this file that changes a byte count.
 *
 * `ducklake_expire_snapshots` does not delete: it SCHEDULES files for deletion. This is what unlinks
 * them. Expiry without cleanup frees nothing and reports a healthy-looking count of retired
 * snapshots, which is precisely how a lake grows while its maintenance says it is working.
 *
 * NOT `delete_orphaned_files`, WHICH IS THE OTHER FUNCTION AND THE ORIGINAL BUG. Orphans are files
 * the catalog never knew about (a crashed write); these are files the catalog knew about and has
 * retired. `orphans` below will not touch a single one of them, and this file used to call only that.
 *
 * `cleanup_all` is deliberately NOT passed, for the same reason as `orphans`: it ignores
 * `older_than`, which is the guard against unlinking a file a reader still has open.
 */
async function cleanup(
  conn: DuckDBConnection,
  catalog: string,
  dryRun: boolean,
  olderThan: Date | undefined
): Promise<MaintenanceResult> {
  const removed = await rows(
    conn,
    `CALL ducklake_cleanup_old_files('${escapeLiteral(catalog)}', dry_run => ${dryRun},` +
      ` older_than => ${olderThanSql(olderThan)})`
  );
  return {
    op: 'cleanup',
    dryRun,
    count: removed.length,
    detail: dryRun
      ? `${removed.length} retired file(s) would be unlinked`
      : `${removed.length} retired file(s) unlinked`,
  };
}

/** ORPHANED FILES — referenced by no snapshot, reachable by no query, removed by no expiry. */
async function orphans(
  conn: DuckDBConnection,
  catalog: string,
  dryRun: boolean,
  olderThan: Date | undefined
): Promise<MaintenanceResult> {
  // `cleanup_all` is deliberately NOT passed. It ignores `older_than`, which is the guard against
  // deleting a file whose commit simply has not landed yet.
  const found = await rows(
    conn,
    `CALL ducklake_delete_orphaned_files('${escapeLiteral(catalog)}', dry_run => ${dryRun},` +
      ` older_than => ${olderThanSql(olderThan)})`
  );
  return {
    op: 'orphans',
    dryRun,
    count: found.length,
    detail: dryRun
      ? `${found.length} orphaned file(s) would be deleted`
      : `${found.length} orphaned file(s) deleted`,
  };
}

/** OLD SNAPSHOTS — the metadata versions time travel can reach. Expiring them is what lets the
 *  orphan sweep above see their files as unreferenced at all. */
async function snapshots(
  conn: DuckDBConnection,
  catalog: string,
  dryRun: boolean,
  olderThan: Date | undefined
): Promise<MaintenanceResult> {
  const expired = await rows(
    conn,
    `CALL ducklake_expire_snapshots('${escapeLiteral(catalog)}', dry_run => ${dryRun},` +
      ` older_than => ${olderThanSql(olderThan)})`
  );
  return {
    op: 'snapshots',
    dryRun,
    count: expired.length,
    detail: dryRun
      ? `${expired.length} snapshot(s) would be expired`
      : `${expired.length} snapshot(s) expired`,
  };
}

/* ────────────────────────────────────────────────────────────────────────────────────────────────
 * `sweepUnits` — the `units/` pass, and the only unbounded thing in this system.
 *
 * MEASURED BEFORE IT WAS WRITTEN, on the dev box, 2026-09-24:
 *
 *     units/        254,801 objects   7.56 GiB   99.1% of the object store
 *     output/           747 objects   0.04 GiB   ← the queryable product
 *     cas/              893 objects   0.02 GiB
 *     history/          293 objects   0.00 GiB
 *
 * Six days of runs. Nothing had ever collected any of it, because this function was referenced by
 * four comments (`data/retention.ts` ×3, `historyArchive.ts`) and a wiki table as though it existed,
 * and did not. `retention.ts` cites its posture on missing mtimes as precedent. That posture is now
 * actually implemented here rather than merely cited.
 *
 * ── WHY AGE IS THE RIGHT TOOL HERE AND IS NOT FOR `cas/` ────────────────────────────────────────
 *
 * A `units/` key is RUN-ADDRESSED: `units/run={run}/dt=…/actor=…/shard=…/unit=…/{sha}.json`. The run
 * is the first segment, so liveness is a property of the run and age answers it. A `cas/` key is a
 * bare content hash shared across runs and tenants, where a blob written months ago can be this
 * second's dedup target — age there tells you nothing and an age rule WILL delete live data. So the
 * prefix below is a module constant, never a parameter: a caller cannot point this at `cas/`.
 *
 * ── THE FOUR THINGS THAT KEEP A RUN'S UNITS ─────────────────────────────────────────────────────
 *
 *   1. IT IS PINNED. `keepRuns` survives any age — a run under investigation, a run whose output
 *      somebody is still arguing about.
 *
 *   2. ITS MATERIALIZATION IS NOT COMPLETE. **This one is not in the wiki and is the reason a
 *      naive age sweep would have been data loss.** `units/` is not a byproduct of the lake, it is
 *      the SOURCE for it: `activities/datasets.ts:resolveBatch` GETs every unit blob to build the
 *      records a dispatch materializes, and fails the dispatch outright if one is missing — "this is
 *      data loss, not a shape problem". A materialization that is `pending`, `running` or `failed`
 *      is retryable, and its retry reads these objects. Deleting them turns a retryable failure into
 *      a permanent one. So a run is collectable only once the ledger says every dispatch is
 *      `complete`, or the ledger has never heard of it at all (nothing is waiting to retry).
 *
 *   3. IT IS NOT OLD ENOUGH — and the clock is the run's NEWEST object, never its oldest. Same
 *      reasoning as `data/retention.ts` fix #2: a run that writes for thirty hours must not have its
 *      first units collected while it is still pushing to the same prefix.
 *
 *   4. ITS AGE IS UNKNOWN. If any object in a run reports no mtime, the run's age cannot be
 *      established and the whole run is kept. Retained, never guessed at — the posture `retention.ts`
 *      already cites this function for.
 *
 * ── WHOLE RUNS, NEVER PART OF ONE ───────────────────────────────────────────────────────────────
 *
 * A run is collected entirely or not at all. A half-collected run is worse than either outcome: the
 * live row tail (`rowTail.ts`) reports a run's progress by LISTing exactly this prefix and counting
 * objects, so a partial sweep would not show an error, it would show a smaller number — a run that
 * produced plenty, reported as having produced less. Wrong answers that look like answers are the
 * failure mode this repo keeps paying for, so the decision grain is the run.
 * ──────────────────────────────────────────────────────────────────────────────────────────────── */

/** The ONE prefix this sweeps. A constant and not a parameter — see the header on `cas/`. */
export const UNITS_PREFIX = 'units/';

/**
 * A run's materialization state as this sweep needs it.
 *
 * STRUCTURALLY THE SAME UNION AS `MaterializationState` (`data/materialization.ts`, and the contract
 * copy in `shared/core`), restated rather than imported so this module stays free of the ledger — it
 * takes the states as data through {@link UnitsSweepDeps}, and importing the store to name a string
 * union would drag SQL into a file whose other half is DuckLake.
 *
 * ONLY `complete` MATTERS TO THE DECISION. The other three are spelled out because the report says
 * WHICH one kept a run, and "not complete" would make a run stuck `pending` read the same as one
 * actively retrying.
 */
export type MaterializationLike = 'pending' | 'running' | 'complete' | 'failed';

/** The documented default (`docs/wiki/Data-Plane.md`): 90 days. See {@link SweepUnitsOptions.retentionMs}. */
export const UNITS_RETENTION_DEFAULT_MS = 90 * 24 * 60 * 60 * 1000;

/** `units/run={runId}/…` — the run id is the first segment, which is what makes this run-grained. */
const UNITS_RUN_RE = /^units\/run=([^/]+)\//;

/** What the sweep needs from the outside world. Injected so the decision is testable without S3. */
export interface UnitsSweepDeps {
  /** Lists and deletes. `ObjectStore`, or anything with its two relevant methods. */
  store: {
    list(prefix: string): Promise<readonly { key: string; size?: number; lastModified?: Date }[]>;
    deleteMany(keys: readonly string[]): Promise<{ deleted: number; errors: string[] }>;
  };
  /**
   * Which runs have finished materializing — keep #2 above.
   *
   * A run ABSENT from this map has no ledger rows, which means nothing is waiting to retry against
   * its units, and the age check alone decides. A run PRESENT and not `complete` is kept.
   *
   * REQUIRED, not optional, and deliberately: a default of "assume complete" would make the most
   * dangerous configuration the one you get by forgetting to pass anything.
   */
  materializationState(): Promise<ReadonlyMap<string, MaterializationLike>>;
}

export interface SweepUnitsOptions {
  /**
   * How old a run's newest object must be before its units are collected. Default 90 days.
   *
   * NINETY DAYS IS THE DOCUMENTED DEFAULT AND IS NOT THE RIGHT NUMBER FOR A BUSY DEPLOYMENT. The dev
   * box writes ~1.25 GiB/day here, so a 90-day window is ~112 GiB of units before the first object is
   * ever eligible. The default stays at the documented value because that is the safe direction and
   * changing a documented contract silently is worse than a large number; the deployment sets a real
   * one (`KONTRA_UNITS_RETENTION_DAYS`) and the report says what a shorter window would free.
   */
  retentionMs?: number;
  /** Runs that survive any age. */
  keepRuns?: readonly string[];
  /** Report without deleting. THE DEFAULT, for the reason every sweep in this repo defaults to it. */
  dryRun?: boolean;
  /** Injected clock, for tests that age a run without waiting. */
  now?: number;
  /** Liveness for a sweep that may list a quarter of a million keys. */
  heartbeat?: (progress: string) => void;
}

/** Why one run's units were kept, or that they were not. */
export type UnitsDisposition =
  | 'collect'
  | 'kept-pinned'
  | 'kept-materializing'
  | 'kept-fresh'
  | 'kept-unknown-age';

/** One run as the sweep weighed it. */
export interface UnitsSweepRun {
  runId: string;
  objects: number;
  bytes: number;
  /** Epoch ms of the run's NEWEST object; `0` when the store reported no mtime for some object. */
  lastWriteAt: number;
  disposition: UnitsDisposition;
}

export interface UnitsSweepReport {
  dryRun: boolean;
  /** Every run found under the prefix, with its decision — the whole basis of the report. */
  runs: UnitsSweepRun[];
  /** Objects listed under `units/`. */
  scanned: number;
  /** Objects the store confirmed deleted. `0` on a dry run. */
  deleted: number;
  /** Bytes under the runs dispositioned `collect` — what a real sweep frees, or did. */
  bytes: number;
  /** Per-key failures the store reported. A partial sweep is reported, never silently rounded up. */
  errors: string[];
}

/**
 * Collect the unit blobs of runs that are old enough, complete, and not pinned.
 *
 * DRY RUN BY DEFAULT. A sweep that deletes on its first invocation is one typo away from removing a
 * run's raw evidence, and the report is what makes the blast radius inspectable first.
 */
export async function sweepUnits(
  deps: UnitsSweepDeps,
  opts: SweepUnitsOptions = {}
): Promise<UnitsSweepReport> {
  const dryRun = opts.dryRun ?? true;
  const now = opts.now ?? Date.now();
  const cutoff = now - (opts.retentionMs ?? UNITS_RETENTION_DEFAULT_MS);
  const pinned = new Set(opts.keepRuns ?? []);
  const beat = opts.heartbeat ?? (() => {});

  beat(`listing ${UNITS_PREFIX}`);
  const objects = await deps.store.list(UNITS_PREFIX);

  // GATHER PER RUN. Keys are grouped rather than decided one at a time, because the decision grain
  // is the run (see the header) and a run's age is a property of the whole group.
  const groups = new Map<string, { objects: number; bytes: number; newest: number; unknownAge: boolean }>();
  for (const o of objects) {
    const m = UNITS_RUN_RE.exec(o.key);
    // A key under `units/` that is not `run=`-shaped is not something this sweep understands, and it
    // is not deleted on a guess. It is counted as scanned and otherwise left entirely alone.
    if (!m) continue;
    const runId = m[1] as string;
    const g = groups.get(runId) ?? { objects: 0, bytes: 0, newest: 0, unknownAge: false };
    g.objects += 1;
    g.bytes += o.size ?? 0;
    const at = o.lastModified?.getTime();
    // NO MTIME POISONS THE WHOLE RUN'S AGE (keep #4). One unreadable object is enough: the run's
    // newest write is then not known, and a sweep that guessed would guess in the deleting direction.
    if (at === undefined || Number.isNaN(at)) g.unknownAge = true;
    else if (at > g.newest) g.newest = at;
    groups.set(runId, g);
  }

  beat(`${groups.size} run(s) under ${UNITS_PREFIX}, reading materialization state`);
  const materialization = await deps.materializationState();

  const runs: UnitsSweepRun[] = [];
  const doomed: string[] = [];
  let bytes = 0;
  for (const [runId, g] of groups) {
    const disposition = classifyUnits({
      pinned: pinned.has(runId),
      materialization: materialization.get(runId),
      unknownAge: g.unknownAge,
      lastWriteAt: g.newest,
      cutoff,
    });
    runs.push({ runId, objects: g.objects, bytes: g.bytes, lastWriteAt: g.unknownAge ? 0 : g.newest, disposition });
    if (disposition === 'collect') {
      bytes += g.bytes;
      doomed.push(runId);
    }
  }
  // Biggest first: the report's head is where the space is, and a capped render still shows what
  // matters. Ties broken by run id so the order is stable across two reads of the same store.
  runs.sort((a, b) => b.bytes - a.bytes || a.runId.localeCompare(b.runId));

  if (dryRun || doomed.length === 0) {
    return { dryRun, runs, scanned: objects.length, deleted: 0, bytes, errors: [] };
  }

  // DELETE BY RUN, and the keys come from the listing above rather than a second LIST. Re-listing
  // would race an actor that started pushing to a run between the decision and the delete — the keys
  // deleted are exactly the keys weighed.
  const condemned = new Set(doomed);
  const keys = objects
    .filter((o) => {
      const m = UNITS_RUN_RE.exec(o.key);
      return m !== null && condemned.has(m[1] as string);
    })
    .map((o) => o.key);

  beat(`deleting ${keys.length} object(s) across ${doomed.length} run(s)`);
  const { deleted, errors } = await deps.store.deleteMany(keys);
  return { dryRun: false, runs, scanned: objects.length, deleted, bytes, errors };
}

/**
 * The decision, with no store and no clock of its own — so every branch is testable directly.
 *
 * ORDER IS THE POLICY. Pinned beats everything; an incomplete materialization beats age, because a
 * retry needs these objects whatever their age; unknown age beats the cutoff, because an unknown age
 * must never compare as old. Only a run that survives all three reaches the age test.
 */
export function classifyUnits(c: {
  pinned: boolean;
  materialization: MaterializationLike | undefined;
  unknownAge: boolean;
  lastWriteAt: number;
  cutoff: number;
}): UnitsDisposition {
  if (c.pinned) return 'kept-pinned';
  // ABSENT is collectable, NOT-COMPLETE is not. Absent means no dispatch is waiting to retry against
  // these objects; `running` or `failed` means one is.
  if (c.materialization !== undefined && c.materialization !== 'complete') return 'kept-materializing';
  if (c.unknownAge || c.lastWriteAt === 0) return 'kept-unknown-age';
  if (c.lastWriteAt >= c.cutoff) return 'kept-fresh';
  return 'collect';
}

/** A catalog alias inside a quoted identifier. */
function quoteIdent(name: string): string {
  return `"${name.replace(/"/g, '""')}"`;
}

/** A catalog alias inside a single-quoted SQL literal. */
function escapeLiteral(name: string): string {
  return name.replace(/'/g, "''");
}
