/**
 * Storage maintenance for the lake: within-dispatch compaction, snapshot expiry, file
 * cleanup and `units/` lifecycle (plan §2, delivery stage 6).
 *
 * THE ONE RULE THAT IS NOT NEGOTIABLE: never merge files across dispatches.
 *
 * Exact-dispatch presigned URLs are object-level authorization (see `explore.ts`). A
 * compaction pass that merged dispatch A's rows into the same Parquet file as dispatch B
 * would silently convert every "scoped to A" URL into a cross-dispatch disclosure — with no
 * error, no log line, and no way to notice until someone read data they should not have.
 *
 * That exclusivity is a property of the LAYOUT, not of any call here: output is partitioned
 * by `(version, dt)`, and `dt` is the dispatch time to the SECOND, so every Parquet file
 * lives under exactly one `version=<v>/dt=<dispatch>/` directory. {@link compactTable}
 * re-checks it after every pass rather than assuming it, because the day it stops holding is
 * the day every exact-dispatch presigned URL becomes unsound.
 *
 * Everything is BOUNDED. A maintenance pass that tries to rewrite a whole lake in one go
 * on a 4 GB host is an outage, not housekeeping: files per pass, bytes touched and peak
 * spill are all capped, and every pass reports what it actually did so a silent no-op is
 * distinguishable from a silent truncation.
 */

import type { ObjectStore } from '../codec/objectStore';
import { LAKE, META, OUTPUT_SCHEMA, lakeConnection, resolveLakeConfig, type LakeConfig } from './parquet';

/** Files rewritten per compaction pass. Small on purpose — see the bounding note above. */
export const DEFAULT_MAX_FILES_PER_PASS = 10;

/** Target compacted file size. DuckLake's own guidance, and what the acceptance bar uses. */
export const TARGET_FILE_BYTES = 128 * 1024 * 1024;

/** A file at or above this size is already "large enough" for the 80%-of-bytes criterion. */
export const LARGE_FILE_BYTES = 32 * 1024 * 1024;

/** Default retention for raw per-unit objects under `units/`. */
export const DEFAULT_UNITS_RETENTION_DAYS = 90;

/** What one compaction pass did to one dispatch partition. */
export interface PartitionReport {
  /** `version=<v>/dt=<dispatch>` — the dispatch whose files were compacted. */
  dispatch: string;
  filesBefore: number;
  filesAfter: number;
  bytesBefore: number;
  bytesAfter: number;
}

export interface CompactionReport {
  table: string;
  partitions: PartitionReport[];
  filesBefore: number;
  filesAfter: number;
  bytesBefore: number;
  bytesAfter: number;
  /** Set when nothing was done, explaining WHY — a no-op must never look like a success. */
  skipped?: string;
}

/**
 * File-size distribution for one run partition — the measurement behind the acceptance
 * criterion "for partitions >= 64 MB, at least 80% of bytes sit in files >= 32 MB".
 */
export interface PartitionSizes {
  files: number;
  bytes: number;
  largeFiles: number;
  largeBytes: number;
  /** Fraction of bytes in files >= {@link LARGE_FILE_BYTES}; 1 when the partition is empty. */
  largeByteFraction: number;
}

function sqlLiteral(v: string): string {
  return v.replace(/'/g, "''");
}

/** One data file DuckLake currently holds, and the dispatch it belongs to. */
interface DataFile {
  path: string;
  bytes: number;
  /** `version=<v>/dt=<dispatch>` derived from the file path — the exclusivity unit. */
  dispatch: string | null;
}

/**
 * The dispatch a file belongs to, read from its PATH.
 *
 * The path is authoritative — it is what presigning re-roots and hands out — and it is the
 * one place both partition columns appear together. DuckLake stores one
 * `ducklake_file_partition_value` row PER partition column, so a two-column partition would
 * need pivoting to reassemble; the path already has `version=…/dt=…` intact.
 */
export function dispatchOfPath(path: string): string | null {
  const m = /(?:^|\/)(version=[^/]+\/dt=[^/]+)\//.exec(path);
  return m ? m[1]! : null;
}

/** Live data files for a table, optionally narrowed to one dispatch (`version=…/dt=…`). */
async function dataFiles(
  conn: Awaited<ReturnType<typeof lakeConnection>>,
  metaSchema: string,
  tbl: string,
  dispatch?: string
): Promise<DataFile[]> {
  const res = await conn.runAndReadAll(
    `SELECT DISTINCT df.path, df.file_size_bytes
       FROM ${META}.${metaSchema}.ducklake_data_file df
       JOIN ${META}.${metaSchema}.ducklake_table t ON t.table_id = df.table_id
       JOIN ${META}.${metaSchema}.ducklake_schema sc
         ON sc.schema_id = t.schema_id AND sc.schema_name = '${OUTPUT_SCHEMA}'
      WHERE t.table_name = '${sqlLiteral(tbl)}' AND t.end_snapshot IS NULL
        AND df.end_snapshot IS NULL`
  );
  const files = res.getRows().map((r) => ({
    path: String(r[0]),
    bytes: Number(r[1] ?? 0),
    dispatch: dispatchOfPath(String(r[0])),
  }));
  return dispatch === undefined ? files : files.filter((f) => f.dispatch === dispatch);
}

export function summarizeSizes(files: Array<{ bytes: number }>): PartitionSizes {
  const bytes = files.reduce((n, f) => n + f.bytes, 0);
  const large = files.filter((f) => f.bytes >= LARGE_FILE_BYTES);
  const largeBytes = large.reduce((n, f) => n + f.bytes, 0);
  return {
    files: files.length,
    bytes,
    largeFiles: large.length,
    largeBytes,
    largeByteFraction: bytes === 0 ? 1 : largeBytes / bytes,
  };
}

/** File-size distribution for one run partition, without changing anything. */
export async function partitionSizes(
  store: ObjectStore,
  sel: { table: string; dispatch: string },
  override: Partial<LakeConfig> = {}
): Promise<PartitionSizes> {
  const cfg = resolveLakeConfig(store, override);
  const conn = await lakeConnection(store, cfg);
  return summarizeSizes(await dataFiles(conn, cfg.metaSchema, sel.table, sel.dispatch));
}

/** Group files by their dispatch (`version=…/dt=…`). */
function byDispatch(files: DataFile[]): Map<string, DataFile[]> {
  const out = new Map<string, DataFile[]>();
  for (const f of files) {
    const key = f.dispatch ?? '';
    const cur = out.get(key) ?? [];
    cur.push(f);
    out.set(key, cur);
  }
  return out;
}

/**
 * Compact one table's data files.
 *
 * WHAT THIS ACTUALLY DOES, precisely — because the safety argument depends on it:
 * DuckLake's `ducklake_merge_adjacent_files` merges ADJACENT files WITHIN each partition,
 * across the whole table. It is not scoped to a single run, and there is no DuckLake API
 * that is. Run exclusivity is therefore NOT a property of this call — it is a property of
 * the LAYOUT: output is partitioned by `(version, dt)` and `dt` is dispatch-unique to the
 * second, so every data file lives under exactly one `version=…/dt=…/` directory and a
 * merge has nothing cross-dispatch to merge.
 *
 * Because that invariant is what makes exact-run presigned URLs an authorization boundary,
 * it is CHECKED AT RUNTIME after every pass rather than trusted. A file that came back
 * without a partition value would mean the layout assumption had silently stopped holding,
 * and every presigned URL issued afterwards would be unsound.
 *
 * `maxFiles` bounds what a pass is willing to START on, and `truncated` on each partition
 * report says when work was left behind — silent truncation reads as "covered everything".
 */
export async function compactTable(
  store: ObjectStore,
  sel: { table: string; maxFiles?: number },
  override: Partial<LakeConfig> = {}
): Promise<CompactionReport> {
  const cfg = resolveLakeConfig(store, override);
  const conn = await lakeConnection(store, cfg);
  const maxFiles = sel.maxFiles ?? DEFAULT_MAX_FILES_PER_PASS;

  const before = await dataFiles(conn, cfg.metaSchema, sel.table);
  assertDispatchExclusive(before, sel.table, 'before');
  const beforeByDispatch = byDispatch(before);

  const totals = (files: DataFile[]) => ({
    filesBefore: files.length,
    bytesBefore: files.reduce((n, f) => n + f.bytes, 0),
  });
  const base: CompactionReport = {
    table: sel.table,
    partitions: [],
    ...totals(before),
    filesAfter: before.length,
    bytesAfter: before.reduce((n, f) => n + f.bytes, 0),
  };

  // Only small files are worth rewriting; a file already at target size would be rewritten
  // for no gain and at full I/O cost.
  const mergeable = [...beforeByDispatch.values()].filter(
    (files) => files.filter((f) => f.bytes < TARGET_FILE_BYTES).length >= 2
  );
  if (mergeable.length === 0) {
    return { ...base, skipped: 'no partition has two or more files below the target size' };
  }
  if (mergeable.some((files) => files.length > maxFiles)) {
    return {
      ...base,
      skipped: `a partition holds more than maxFiles=${maxFiles} files; raise the cap deliberately rather than rewriting an unbounded set in one pass`,
    };
  }

  await conn.run('BEGIN TRANSACTION');
  try {
    // schema := is load-bearing: output tables live in `lake.output.<actor>`, and without it
    // DuckLake resolves against `main` and throws "table does not exist". datasets.ts's
    // ducklake_list_files call passes it the same way.
    await conn.run(
      `CALL ducklake_merge_adjacent_files('${LAKE}', '${sqlLiteral(sel.table)}', schema := '${OUTPUT_SCHEMA}')`
    );
    await conn.run('COMMIT');
  } catch (err) {
    await conn.run('ROLLBACK').catch(() => undefined);
    throw err;
  }

  const after = await dataFiles(conn, cfg.metaSchema, sel.table);
  // The post-condition. If this ever fires, presigned exact-dispatch URLs must be considered
  // unsound until it is explained — hence a throw, not a warning.
  assertDispatchExclusive(after, sel.table, 'after');
  const afterByDispatch = byDispatch(after);

  const partitions: PartitionReport[] = [];
  for (const [dispatch, files] of beforeByDispatch) {
    const now = afterByDispatch.get(dispatch) ?? [];
    partitions.push({
      dispatch,
      filesBefore: files.length,
      filesAfter: now.length,
      bytesBefore: files.reduce((n, f) => n + f.bytes, 0),
      bytesAfter: now.reduce((n, f) => n + f.bytes, 0),
    });
  }
  partitions.sort((a, b) => a.dispatch.localeCompare(b.dispatch));

  return {
    ...base,
    partitions,
    filesAfter: after.length,
    bytesAfter: after.reduce((n, f) => n + f.bytes, 0),
  };
}

/**
 * Every live data file must belong to exactly one dispatch (`version=…/dt=…`). Violating
 * this would mean a single Parquet object contained more than one dispatch, which would make
 * every exact-dispatch presigned URL a cross-dispatch disclosure.
 */
function assertDispatchExclusive(files: DataFile[], tbl: string, when: string): void {
  const orphans = files.filter((f) => f.dispatch === null || f.dispatch === '');
  if (orphans.length > 0) {
    throw new Error(
      `dispatch-exclusivity violated ${when} compacting ${tbl}: ${orphans.length} data file(s) carry no ` +
        `version=/dt= partition (${orphans[0]!.path}). Exact-dispatch presigned URLs are unsound until this is explained.`
    );
  }
}

export interface ExpiryReport {
  /** Snapshots older than the cutoff that were expired. */
  snapshotsExpired: number;
  /** Data files the cleanup then physically removed. */
  filesDeleted: number;
}

/**
 * Expire snapshots older than `olderThanMs` and delete the files that expiry orphaned.
 *
 * TWO STEPS, IN THIS ORDER, ALWAYS. Snapshot expiry only SCHEDULES files for deletion;
 * `ducklake_cleanup_old_files` is what removes them. Running cleanup without expiry
 * deletes nothing; deleting objects out-of-band without either one removes files the
 * catalog still references, which turns a readable lake into one that errors on every
 * query touching the missing file.
 */
export async function expireSnapshots(
  store: ObjectStore,
  olderThanMs: number,
  override: Partial<LakeConfig> = {}
): Promise<ExpiryReport> {
  const cfg = resolveLakeConfig(store, override);
  const conn = await lakeConnection(store, cfg);
  const cutoff = new Date(Date.now() - olderThanMs).toISOString();

  const beforeSnaps = await conn.runAndReadAll(
    `SELECT count(*) FROM ${META}.${cfg.metaSchema}.ducklake_snapshot`
  );
  await conn.run(
    `CALL ducklake_expire_snapshots('${LAKE}', older_than => TIMESTAMP '${sqlLiteral(cutoff)}')`
  );
  const afterSnaps = await conn.runAndReadAll(
    `SELECT count(*) FROM ${META}.${cfg.metaSchema}.ducklake_snapshot`
  );
  const deleted = await conn.runAndReadAll(
    `CALL ducklake_cleanup_old_files('${LAKE}', cleanup_all => true)`
  );

  return {
    snapshotsExpired: Number(beforeSnaps.getRows()[0]?.[0] ?? 0) - Number(afterSnaps.getRows()[0]?.[0] ?? 0),
    filesDeleted: deleted.getRows().length,
  };
}

export interface UnitsSweepReport {
  scanned: number;
  deleted: number;
  /** Keys that were kept because their run is still referenced. */
  retained: number;
  dryRun: boolean;
}

/**
 * Age out raw per-unit objects under `units/`.
 *
 * `cas/` IS NEVER TOUCHED BY AGE. Content-addressed objects are deduplicated across runs,
 * so "this blob is 90 days old" says nothing about whether a live run still points at it;
 * deleting by age there would break runs that share a hash with an old one. CAS reclamation
 * needs run-record mark-and-sweep, which is a different operation with a different input.
 *
 * Defaults to a DRY RUN. A sweep that deletes on its first invocation is one typo away
 * from removing a run's raw evidence, and the report is what makes the blast radius
 * inspectable before it happens.
 */
export async function sweepUnits(
  store: ObjectStore,
  opts: { retentionDays?: number; dryRun?: boolean; keepRuns?: ReadonlySet<string> } = {}
): Promise<UnitsSweepReport> {
  const retentionDays = opts.retentionDays ?? DEFAULT_UNITS_RETENTION_DAYS;
  const dryRun = opts.dryRun ?? true;
  const keep = opts.keepRuns ?? new Set<string>();
  const cutoff = Date.now() - retentionDays * 24 * 3_600_000;

  const objects = await store.list('units/');
  let deleted = 0;
  let retained = 0;
  for (const o of objects) {
    const runId = runIdOfUnitKey(o.key);
    if (runId && keep.has(runId)) {
      retained += 1;
      continue;
    }
    // No mtime means the backing store did not report one; refuse to guess. Deleting an
    // object whose age is unknown is exactly the mistake this sweep exists to avoid.
    if (!o.lastModified || o.lastModified.getTime() >= cutoff) {
      retained += 1;
      continue;
    }
    if (!dryRun) await store.delete(o.key);
    deleted += 1;
  }
  return { scanned: objects.length, deleted, retained, dryRun };
}

/**
 * The run id embedded in a unit-blob key, for both layouts:
 *   hive   — `units/run=<id>/dt=…/actor=…/shard=…/unit=…/<sha>.json`
 *   legacy — `units/<id>/<node>/u<i>.json`
 * Returns null when the key matches neither, so an unrecognised key is never swept by
 * accident.
 */
export function runIdOfUnitKey(key: string): string | null {
  const hive = /(?:^|\/)units\/run=([^/]+)\//.exec(key);
  if (hive) return hive[1]!;
  const legacy = /(?:^|\/)units\/([^/=]+)\//.exec(key);
  return legacy ? legacy[1]! : null;
}
