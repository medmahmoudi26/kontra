/**
 * Moving a lake from one workspace address to another (issue 10), and — as the same act —
 * leaving the 2026-09-28 wipe's orphaned partitions behind (issue 09).
 *
 * ── WHY ONE MODULE AND NOT TWO ──────────────────────────────────────────────────────────────────
 *
 * Issue 09 was written as a PURGE: enumerate the catalog entries whose parquet the wipe deleted,
 * and drop them. Issue 10 was written as a MOVE. Doing 09 first means editing a catalog in place —
 * the highest-risk operation available on a DuckLake, and the one the memory note about never
 * hand-editing `__ducklake_metadata_*` exists to forbid.
 *
 * A COPY-FORWARD does both and edits nothing. The migration reads the source lake READ_ONLY and
 * writes only into the destination, so a partition whose data file is gone cannot be copied — it
 * fails its probe and is recorded as skipped. The orphans are purged by NOT BEING CARRIED, which
 * needs no DELETE, no catalog surgery, and no irreversible step: the source lake is untouched and
 * remains the rollback.
 *
 * ── THE PROBE IS `SELECT * … LIMIT 1`, NEVER `count(*)` ─────────────────────────────────────────
 *
 * DuckLake answers `count(*)` from file STATISTICS held in the catalog, so a partition whose
 * parquet has been deleted still reports its original row count, cheerfully, forever. Measured on
 * this install 2026-09-28: `canary_signals` answered 288, `observations` 833, `reach_verdicts` 319
 * — every one unreadable. Only a probe that must OPEN a data file tells the truth, which is why
 * the classification below runs one per partition and treats an error as `dead` rather than
 * letting it abort the migration.
 *
 * `empty` IS KEPT DISTINCT FROM `dead`. A partition that reads successfully and returns no rows is
 * a legitimately empty Dataset, not a missing file, and collapsing the two would delete a real
 * (if boring) result. An empty partition IS carried across.
 */

import type { DuckDBConnection } from '@duckdb/node-api';

import type { ObjectStore } from '../codec/objectStore';
import { listDatasets, type DatasetInfo } from './datasets';
import { MIGRATE_DST, MIGRATE_SRC, migrationConnection, safeName, type LakeConfig } from './parquet';

/** Which schema a dataset kind lives in. Mirrors `datasets.ts`'s mapping, by the same two names. */
function schemaOf(kind: DatasetInfo['kind']): string {
  return kind === 'standalone' ? 'standalone' : 'output';
}

/** What the probe decided about one catalog entry. */
export type PartitionState = 'live' | 'empty' | 'dead';

export interface PartitionResult {
  schema: string;
  table: string;
  kind: DatasetInfo['kind'];
  version?: string;
  dt?: string;
  /** Rows the SOURCE CATALOG claims — statistics, and therefore not to be trusted as truth. */
  claimedRows: number;
  state: PartitionState;
  /** Rows actually copied into the destination. Zero unless `state` is live or empty. */
  copiedRows: number;
  /** The probe's or the copy's error, when there was one. */
  error?: string;
}

export interface MigrationReport {
  source: { catalog: string; dataPath: string };
  destination: { catalog: string; dataPath: string };
  /** False for a dry run — nothing was written. */
  applied: boolean;
  probed: number;
  live: number;
  empty: number;
  dead: number;
  /** Partitions written into the destination (live + empty), when `applied`. */
  copied: number;
  rowsCopied: number;
  /** Rows the source catalog claims for the partitions left behind. */
  rowsAbandoned: number;
  entries: PartitionResult[];
}

/** A single-quoted SQL literal. Identifiers go through {@link safeName} instead. */
function lit(v: string): string {
  return `'${v.replace(/'/g, "''")}'`;
}

/**
 * The partition predicate for one entry.
 *
 * Standalone datasets have neither a version nor a dt — they are whole tables — so they get no
 * predicate and are copied entire. Output datasets are partitioned by (version, dt) and are copied
 * ONE PARTITION AT A TIME, which is what keeps a single dead partition from failing its whole
 * table: a table with 3 live and 1 dead partition migrates 3 and reports the fourth.
 */
function partitionWhere(e: DatasetInfo): string {
  const clauses: string[] = [];
  if (e.version !== undefined) clauses.push(`version = ${lit(e.version)}`);
  if (e.dt !== undefined) clauses.push(`dt = ${lit(e.dt)}`);
  return clauses.length > 0 ? ` WHERE ${clauses.join(' AND ')}` : '';
}

async function probe(conn: DuckDBConnection, from: string): Promise<PartitionState> {
  const res = await conn.runAndReadAll(`SELECT * FROM ${from} LIMIT 1`);
  return res.getRows().length > 0 ? 'live' : 'empty';
}

/**
 * Copy every readable partition of one lake into another, and report what was left behind.
 *
 * `apply: false` (the default) probes and reports WITHOUT WRITING ANYTHING, which is the form a
 * human reads before authorising the move. The probe itself is a SELECT against a READ_ONLY
 * attachment, so a dry run cannot change either lake.
 *
 * Idempotent by construction: the destination table is created `IF NOT EXISTS` and the copy is
 * skipped for any (table, version, dt) the destination already holds. Re-running after an
 * interruption therefore resumes rather than duplicating — which matters, because a partial
 * migration is the normal outcome of a lake big enough to need one.
 */
export async function migrateWorkspaceLake(
  store: ObjectStore,
  opts: {
    from?: Partial<LakeConfig>;
    to: Partial<LakeConfig>;
    apply?: boolean;
    /** Called after each partition so a long migration is legible while it runs. */
    onPartition?: (r: PartitionResult) => void;
  }
): Promise<MigrationReport> {
  const apply = opts.apply ?? false;
  const from = opts.from ?? {};
  const entries = await listDatasets(store, {}, from);
  const { conn, src, dst } = await migrationConnection(store, from, opts.to);

  const report: MigrationReport = {
    source: { catalog: src.catalog, dataPath: src.dataPath },
    destination: { catalog: dst.catalog, dataPath: dst.dataPath },
    applied: apply,
    probed: 0,
    live: 0,
    empty: 0,
    dead: 0,
    copied: 0,
    rowsCopied: 0,
    rowsAbandoned: 0,
    entries: [],
  };

  try {
    for (const e of entries) {
      const schema = schemaOf(e.kind);
      const table = safeName(e.name);
      const srcRef = `${MIGRATE_SRC}.${schema}."${table}"`;
      const dstRef = `${MIGRATE_DST}.${schema}."${table}"`;
      const where = partitionWhere(e);
      const out: PartitionResult = {
        schema,
        table,
        kind: e.kind,
        version: e.version,
        dt: e.dt,
        claimedRows: e.rows ?? 0,
        state: 'dead',
        copiedRows: 0,
      };

      try {
        out.state = await probe(conn, `${srcRef}${where}`);
      } catch (err) {
        // A missing data file, a permission error, a corrupt footer — all of them mean the same
        // thing for this partition's future: it cannot be read, so it cannot be moved. The message
        // is kept so a reader can tell those apart afterwards.
        out.state = 'dead';
        out.error = err instanceof Error ? err.message : String(err);
      }

      report.probed += 1;
      report[out.state] += 1;

      if (out.state === 'dead') {
        report.rowsAbandoned += out.claimedRows;
      } else if (apply) {
        try {
          // `LIMIT 0` gives the destination the source's exact column types without reading a
          // data file, so a table whose OTHER partitions are dead still gets created correctly.
          await conn.run(
            `CREATE TABLE IF NOT EXISTS ${dstRef} AS SELECT * FROM ${srcRef} LIMIT 0`
          );
          if (schema === 'output') {
            // Partitioning is not carried by CTAS, and an unpartitioned destination would write
            // every dt into one directory — losing the layout the object-store paths encode.
            // Already-partitioned is not an error worth failing a migration over.
            await conn
              .run(`ALTER TABLE ${dstRef} SET PARTITIONED BY (version, dt)`)
              .catch(() => undefined);
          }
          const already = await conn.runAndReadAll(`SELECT 1 FROM ${dstRef}${where} LIMIT 1`);
          if (already.getRows().length > 0) {
            out.error = 'already present in destination — not copied again';
          } else {
            // BY NAME, so a table whose columns were added over time still lands correctly when
            // the destination was created from a different partition's shape.
            await conn.run(`INSERT INTO ${dstRef} BY NAME SELECT * FROM ${srcRef}${where}`);
            const n = await conn.runAndReadAll(
              `SELECT count(*) AS n FROM ${dstRef}${where}`
            );
            out.copiedRows = Number(n.getRows()[0]?.[0] ?? 0);
            report.copied += 1;
            report.rowsCopied += out.copiedRows;
          }
        } catch (err) {
          out.error = err instanceof Error ? err.message : String(err);
          report.rowsAbandoned += out.claimedRows;
        }
      }

      report.entries.push(out);
      opts.onPartition?.(out);
    }
  } finally {
    conn.closeSync();
  }

  return report;
}
