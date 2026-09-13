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
 */

import type { DuckDBConnection } from '@duckdb/node-api';

/** The three operations this module performs. Expiry is `data/retention.ts`'s and is not here. */
export type MaintenanceOp = 'compact' | 'orphans' | 'snapshots';

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

  for (const op of opts.ops) {
    switch (op) {
      case 'compact':
        out.push(await compact(conn, catalog, dryRun));
        break;
      case 'orphans':
        out.push(await orphans(conn, catalog, dryRun, opts.olderThan));
        break;
      case 'snapshots':
        out.push(await snapshots(conn, catalog, dryRun, opts.olderThan));
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

/** A catalog alias inside a quoted identifier. */
function quoteIdent(name: string): string {
  return `"${name.replace(/"/g, '""')}"`;
}

/** A catalog alias inside a single-quoted SQL literal. */
function escapeLiteral(name: string): string {
  return name.replace(/'/g, "''");
}
