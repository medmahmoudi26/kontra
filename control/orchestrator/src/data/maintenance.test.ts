/**
 * Storage maintenance: compaction, expiry and retention.
 *
 * The load-bearing test here is `NEVER merges files across dispatches`. Exact-dispatch
 * presigned URLs are object-level authorization, so a compaction pass that mixed two
 * dispatches into one Parquet file would turn every "scoped to dispatch A" URL into a
 * cross-dispatch disclosure — silently. That property is asserted against a REAL DuckLake,
 * not reasoned about.
 *
 * The exclusivity unit is `version=<v>/dt=<dispatch>` now, not `run_id=<uuid>`: output is one
 * table per ACTOR partitioned by (version, dt), and `dt` is the dispatch time to the second.
 */

import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { beforeEach, describe, expect, it } from 'vitest';

import { MemoryStore, ObjectStore } from '../codec/objectStore';
import {
  LARGE_FILE_BYTES,
  compactTable,
  dispatchOfPath,
  expireSnapshots,
  partitionSizes,
  runIdOfUnitKey,
  summarizeSizes,
  sweepUnits,
} from './maintenance';
import {
  LAKE,
  OUTPUT_SCHEMA,
  dtPartition,
  lakeConnection,
  resetLakeConnections,
  resolveLakeConfig,
  writeDatasetParquet,
  type LakeConfig,
} from './parquet';

/** Two dispatches of ONE actor, a second apart — the only thing that separates them is `dt`. */
const DISPATCH_A = 1_700_000_000_000;
const DISPATCH_B = 1_700_000_001_000;
const VERSION = '0.1.0';

/** `version=…/dt=…` — the selector `partitionSizes` and `PartitionReport.dispatch` speak. */
const dispatchOf = (runStartedAt: number) => `version=${VERSION}/dt=${dtPartition(runStartedAt)}`;

interface Ctx {
  store: ObjectStore;
  cfg: Partial<LakeConfig>;
  blobs: string;
  write: (runStartedAt: number, units: unknown[][]) => Promise<string>;
}

function lake(): Ctx {
  const dir = mkdtempSync(join(tmpdir(), 'kontra-maint-'));
  const dataPath = join(dir, 'data');
  const blobs = join(dir, 'blobs');
  mkdirSync(dataPath, { recursive: true });
  mkdirSync(blobs, { recursive: true });
  const store = new ObjectStore({ backing: new MemoryStore(), prefix: '', endpoint: 'http://localhost:8333' });
  const cfg: Partial<LakeConfig> = {
    catalog: join(dir, 'cat.ducklake'),
    dataPath: `${dataPath}/`,
    s3: false,
    blobBase: `${blobs}/`,
  };
  let seq = 0;
  return {
    store,
    cfg,
    blobs,
    /**
     * Materialize one dispatch of `echo`, one unit blob per entry (one data file each).
     * `runStartedAt` IS the dispatch: it is what `dt=` is derived from.
     */
    async write(runStartedAt, units) {
      const results = units.map((u) => {
        const name = `u${seq++}.json`;
        const body = JSON.stringify(u);
        writeFileSync(join(blobs, name), body);
        return { $ref: { key: name, sha256: createHash('sha256').update(body).digest('hex'), size: body.length } };
      });
      const src = join(mkdtempSync(join(tmpdir(), 'kontra-maint-src-')), 'blob.json');
      writeFileSync(src, JSON.stringify({ results, failures: [] }));
      const out = await writeDatasetParquet(
        store,
        {
          sha256: 'x',
          actor: 'echo',
          version: VERSION,
          runId: `run-${runStartedAt}`,
          node: 'n1',
          runStartedAt,
        },
        { ...cfg, sourceUri: src, batchSize: 1 } // one batch per unit => one file per unit
      );
      return out.tbl!;
    },
  };
}

async function query(ctx: Ctx, sql: string): Promise<unknown[][]> {
  const conn = await lakeConnection(ctx.store, resolveLakeConfig(ctx.store, ctx.cfg));
  return (await conn.runAndReadAll(sql)).getRows() as unknown[][];
}

/** Live data-file paths for `tbl`, filtered by RAW string so nothing under test is trusted. */
async function pathsUnderDt(ctx: Ctx, tbl: string, dt: string): Promise<string[]> {
  const rows = await query(
    ctx,
    `SELECT DISTINCT df.path FROM __ducklake_metadata_lake.main.ducklake_data_file df
       JOIN __ducklake_metadata_lake.main.ducklake_table t ON t.table_id = df.table_id
      WHERE t.table_name = '${tbl}' AND t.end_snapshot IS NULL AND df.end_snapshot IS NULL`
  );
  return rows.map((r) => String(r[0])).filter((p) => p.includes(`/dt=${dt}/`));
}

describe('compactTable', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });

  it('NEVER merges files across dispatches', async () => {
    // The whole safety argument for exact-dispatch presigning rests on this.
    const tbl = await ctx.write(DISPATCH_A, [[{ host: 'a1' }], [{ host: 'a2' }], [{ host: 'a3' }]]);
    await ctx.write(DISPATCH_B, [[{ host: 'b1' }], [{ host: 'b2' }], [{ host: 'b3' }]]);
    const dtA = dtPartition(DISPATCH_A);
    const dtB = dtPartition(DISPATCH_B);

    const bBefore = await pathsUnderDt(ctx, tbl, dtB);
    const report = await compactTable(ctx.store, { table: tbl }, ctx.cfg);
    const aAfter = await pathsUnderDt(ctx, tbl, dtA);
    const bAfter = await pathsUnderDt(ctx, tbl, dtB);

    // 1. No physical file is shared between the two dispatches. This is what makes a
    //    presigned URL scoped to dispatch A incapable of returning dispatch B — the
    //    partition is a directory, so exclusivity is a property of the layout.
    expect(aAfter.length).toBeGreaterThan(0);
    expect(aAfter.filter((p) => bAfter.includes(p))).toEqual([]);
    for (const p of aAfter) expect(p).toContain(`version=${VERSION}/dt=${dtA}/`);
    for (const p of bAfter) expect(p).toContain(`version=${VERSION}/dt=${dtB}/`);

    // 2. Both partitions were compacted — DuckLake's merge is table-wide — but each
    //    partition's files stayed inside its own directory. The report says so per
    //    DISPATCH, rather than collapsing two dispatches into one number.
    expect(report.partitions.map((p) => p.dispatch).sort()).toEqual(
      [dispatchOf(DISPATCH_A), dispatchOf(DISPATCH_B)].sort()
    );
    expect(bBefore.length).toBeGreaterThan(bAfter.length);

    // 3. …and no rows were lost or duplicated in the process.
    const counts = await query(
      ctx,
      `SELECT dt, count(*) FROM ${LAKE}.${OUTPUT_SCHEMA}."${tbl}" GROUP BY 1 ORDER BY 1`
    );
    expect(counts.map((r) => [String(r[0]), Number(r[1])])).toEqual([
      [dtA, 3],
      [dtB, 3],
    ]);
  });

  it('reduces the file count for a fragmented dispatch partition', async () => {
    const tbl = await ctx.write(DISPATCH_A, [[{ host: 'a1' }], [{ host: 'a2' }], [{ host: 'a3' }], [{ host: 'a4' }]]);
    const before = await partitionSizes(ctx.store, { table: tbl, dispatch: dispatchOf(DISPATCH_A) }, ctx.cfg);
    expect(before.files).toBeGreaterThan(1);

    const report = await compactTable(ctx.store, { table: tbl }, ctx.cfg);
    expect(report.filesAfter).toBeLessThan(report.filesBefore);
    expect(report.skipped).toBeUndefined();
    expect(report.partitions).toEqual([
      expect.objectContaining({ dispatch: dispatchOf(DISPATCH_A), filesBefore: before.files }),
    ]);
  });

  it('says WHY it did nothing rather than reporting a silent success', async () => {
    // A no-op that looks identical to a successful pass is how "maintenance ran" becomes
    // an unfalsifiable claim.
    const tbl = await ctx.write(DISPATCH_A, [[{ host: 'a1' }]]);
    const report = await compactTable(ctx.store, { table: tbl }, ctx.cfg);
    expect(report.skipped).toMatch(/two or more files below the target size/);
    expect(report.filesAfter).toBe(report.filesBefore);
  });

  it('refuses an unbounded pass rather than silently rewriting a huge partition', async () => {
    // Bounding by REFUSING is honest; bounding by doing part of the work and reporting
    // success is how "maintenance ran" stops meaning anything.
    const tbl = await ctx.write(DISPATCH_A, [[{ h: 1 }], [{ h: 2 }], [{ h: 3 }], [{ h: 4 }]]);
    const report = await compactTable(ctx.store, { table: tbl, maxFiles: 2 }, ctx.cfg);
    expect(report.skipped).toMatch(/more than maxFiles=2/);
    expect(report.filesAfter).toBe(report.filesBefore);
  });

  it('scopes partitionSizes to ONE dispatch, not the whole actor table', async () => {
    // The measurement has to answer "is THIS dispatch fragmented?" — an actor table holds
    // every dispatch it ever ran, so a table-wide number would never fall.
    const tbl = await ctx.write(DISPATCH_A, [[{ host: 'a1' }], [{ host: 'a2' }]]);
    await ctx.write(DISPATCH_B, [[{ host: 'b1' }]]);
    const a = await partitionSizes(ctx.store, { table: tbl, dispatch: dispatchOf(DISPATCH_A) }, ctx.cfg);
    const b = await partitionSizes(ctx.store, { table: tbl, dispatch: dispatchOf(DISPATCH_B) }, ctx.cfg);
    expect(a.files).toBe(2);
    expect(b.files).toBe(1);
  });
});

describe('dispatchOfPath', () => {
  it('reads BOTH partition segments off the path, which is the only place they appear together', () => {
    // DuckLake stores one `ducklake_file_partition_value` row per partition COLUMN, so
    // reassembling (version, dt) from the catalog would need a pivot; the path has it whole.
    expect(dispatchOfPath('output/crawl4ai/version=1.0.0/dt=2026-08-03T19-42-07/data_0.parquet')).toBe(
      'version=1.0.0/dt=2026-08-03T19-42-07'
    );
  });

  it('reads it from an absolute store path too — presigning re-roots the same string', () => {
    expect(
      dispatchOfPath('s3://kontra/output/echo/version=0.1.0/dt=2023-11-14T22-13-20/x.parquet')
    ).toBe('version=0.1.0/dt=2023-11-14T22-13-20');
  });

  it('returns null for a file that carries no dispatch — the orphan compaction refuses to trust', () => {
    // `assertDispatchExclusive` turns exactly this into a throw: a live data file with no
    // `version=/dt=` directory means the layout assumption stopped holding, and every
    // exact-dispatch presigned URL issued afterwards would be unsound.
    expect(dispatchOfPath('output/echo/data_0.parquet')).toBeNull();
    expect(dispatchOfPath('datasets/main/ds_cc1dcc89c996f795/run_id=r1/x.parquet')).toBeNull();
  });
});

describe('summarizeSizes', () => {
  it('measures the acceptance criterion: share of bytes in files >= 32 MB', () => {
    const s = summarizeSizes([
      { bytes: LARGE_FILE_BYTES * 2 },
      { bytes: LARGE_FILE_BYTES },
      { bytes: 1024 },
    ]);
    expect(s.files).toBe(3);
    expect(s.largeFiles).toBe(2);
    expect(s.largeByteFraction).toBeGreaterThan(0.99);
  });

  it('calls an empty partition fully compliant rather than dividing by zero', () => {
    expect(summarizeSizes([]).largeByteFraction).toBe(1);
  });
});

describe('expireSnapshots', () => {
  beforeEach(() => resetLakeConnections());

  it('expires old snapshots and deletes the files that expiry orphaned', async () => {
    const ctx = lake();
    const tbl = await ctx.write(DISPATCH_A, [[{ host: 'a1' }], [{ host: 'a2' }]]);
    await compactTable(ctx.store, { table: tbl }, ctx.cfg);

    // Everything is older than "now", so this expires the whole history but the CURRENT
    // snapshot — the live data must survive.
    const report = await expireSnapshots(ctx.store, 0, ctx.cfg);
    expect(report.snapshotsExpired).toBeGreaterThan(0);

    const rows = await query(ctx, `SELECT count(*) FROM ${LAKE}.${OUTPUT_SCHEMA}."${tbl}"`);
    expect(Number(rows[0]![0])).toBe(2); // still readable — expiry removed history, not data
  });
});

describe('sweepUnits', () => {
  function storeWith(objects: Array<{ key: string; ageDays: number }>): ObjectStore {
    const backing = new MemoryStore();
    const store = new ObjectStore({ backing, prefix: '' });
    for (const o of objects) backing.map.set(o.key, new TextEncoder().encode('[]'));
    // MemoryStore's list() carries no mtime, so drive age through a stub list().
    (store as unknown as { list: (p: string) => Promise<unknown> }).list = async (prefix: string) =>
      objects
        .filter((o) => o.key.startsWith(prefix))
        .map((o) => ({
          key: o.key,
          lastModified: new Date(Date.now() - o.ageDays * 24 * 3_600_000),
        }));
    return store;
  }

  it('defaults to a dry run and reports the blast radius without deleting', async () => {
    const store = storeWith([{ key: 'units/run=old/dt=1/a.json', ageDays: 200 }]);
    const report = await sweepUnits(store);
    expect(report.dryRun).toBe(true);
    expect(report.deleted).toBe(1);
    expect(await store.exists('units/run=old/dt=1/a.json')).toBe(true); // nothing removed
  });

  it('deletes only objects past the retention window', async () => {
    const store = storeWith([
      { key: 'units/run=old/dt=1/a.json', ageDays: 200 },
      { key: 'units/run=new/dt=1/b.json', ageDays: 1 },
    ]);
    const report = await sweepUnits(store, { dryRun: false, retentionDays: 90 });
    expect(report.deleted).toBe(1);
    expect(report.retained).toBe(1);
    expect(await store.exists('units/run=old/dt=1/a.json')).toBe(false);
    expect(await store.exists('units/run=new/dt=1/b.json')).toBe(true);
  });

  it('keeps objects belonging to a run the caller pinned', async () => {
    const store = storeWith([{ key: 'units/run=keepme/dt=1/a.json', ageDays: 200 }]);
    const report = await sweepUnits(store, { dryRun: false, keepRuns: new Set(['keepme']) });
    expect(report.deleted).toBe(0);
    expect(report.retained).toBe(1);
  });

  it('refuses to delete an object whose age is unknown', async () => {
    // No mtime means the backing store did not report one. Guessing here would delete raw
    // evidence on the strength of an assumption.
    const backing = new MemoryStore();
    const store = new ObjectStore({ backing, prefix: '' });
    backing.map.set('units/run=x/dt=1/a.json', new TextEncoder().encode('[]'));
    const report = await sweepUnits(store, { dryRun: false, retentionDays: 0 });
    expect(report.deleted).toBe(0);
    expect(report.retained).toBe(1);
  });

  it('never touches cas/ — content-addressed blobs are shared across runs', async () => {
    const store = storeWith([
      { key: 'cas/ab/abcdef', ageDays: 999 },
      { key: 'units/run=old/dt=1/a.json', ageDays: 999 },
    ]);
    const report = await sweepUnits(store, { dryRun: false });
    expect(report.deleted).toBe(1); // the unit blob only
    expect(await store.exists('cas/ab/abcdef')).toBe(true);
  });
});

describe('runIdOfUnitKey', () => {
  // Raw units still live under `units/run=<id>/` — the re-layout moved OUTPUT, not evidence.
  it('reads the run id from the hive layout', () => {
    expect(runIdOfUnitKey('units/run=abc/dt=2026-08-03/actor=x/shard=0001/unit=1/aa.json')).toBe('abc');
  });

  it('reads the run id from the legacy positional layout', () => {
    expect(runIdOfUnitKey('units/abc/n1/u0.json')).toBe('abc');
  });

  it('returns null for a key it does not recognise, so nothing is swept by guess', () => {
    expect(runIdOfUnitKey('cas/ab/abcdef')).toBeNull();
    expect(runIdOfUnitKey('output/echo/version=0.1.0/dt=2023-11-14T22-13-20/x.parquet')).toBeNull();
  });
});
