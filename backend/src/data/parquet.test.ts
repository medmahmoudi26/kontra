/**
 * The typed materializer: integrity, typing, transactionality and the empty/failed split.
 *
 * These run against a REAL DuckLake whose DATA_PATH is a local directory standing in for
 * S3, and real per-unit blob files standing in for object-store keys. Everything the
 * writer does — sha256 verification, ref paging, bounded batching, schema evolution,
 * transaction rollback — executes for real; only the storage backend URL differs.
 */

import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, isAbsolute, join } from 'node:path';

import { afterAll, beforeEach, describe, expect, it } from 'vitest';

import { MemoryStore, ObjectStore } from '../codec/objectStore';
import { MATERIALIZATION_SCHEMA_VERSION } from './materialization';
import { MaterializationStore } from './materializationStore';
import {
  catalogFilePath,
  resetLakeConnections,
  writeDatasetParquet,
  lakeConnection,
  resolveLakeConfig,
  dtPartition,
  parseDtPartition,
  safeName,
  LAKE,
  OUTPUT_SCHEMA,
  type LakeConfig,
  type MaterializeSelector,
} from './parquet';

interface Ctx {
  store: ObjectStore;
  cfg: Partial<LakeConfig>;
  /** Directory the per-unit blob keys resolve under. */
  blobs: string;
  /** Write a per-unit blob and return the `$ref` entry that points at it. */
  unit: (name: string, units: unknown[]) => { $ref: { key: string; sha256: string; size: number } };
  /** Write a manifest file and return its path. */
  manifest: (results: unknown[]) => string;
  /**
   * Write a BARE-ARRAY manifest — what the handler stores since a Method's result ref started
   * addressing a bare list of units rather than the {done, results, failures, opens} envelope.
   */
  bareManifest: (results: unknown[]) => string;
}

function lake(overrides: Partial<LakeConfig> = {}): Ctx {
  const dir = mkdtempSync(join(tmpdir(), 'kontra-mat-'));
  const dataPath = join(dir, 'data');
  const blobs = join(dir, 'blobs');
  mkdirSync(dataPath, { recursive: true });
  mkdirSync(blobs, { recursive: true });
  const store = new ObjectStore({ backing: new MemoryStore(), prefix: '', endpoint: 'http://localhost:8333' });
  return {
    store,
    blobs,
    cfg: {
      catalog: join(dir, 'cat.ducklake'),
      dataPath: `${dataPath}/`,
      s3: false,
      blobBase: `${blobs}/`,
      ...overrides,
    },
    unit(name, units) {
      const body = JSON.stringify(units);
      writeFileSync(join(blobs, name), body);
      return { $ref: { key: name, sha256: createHash('sha256').update(body).digest('hex'), size: body.length } };
    },
    manifest(results) {
      const src = join(mkdtempSync(join(tmpdir(), 'kontra-man-')), 'blob.json');
      writeFileSync(src, JSON.stringify({ results, failures: [] }));
      return src;
    },
    bareManifest(results) {
      const src = join(mkdtempSync(join(tmpdir(), 'kontra-man-')), 'blob.json');
      writeFileSync(src, JSON.stringify(results));
      return src;
    },
  };
}

const SEL: MaterializeSelector = {
  sha256: 'unused-when-sourceUri-is-set',
  actor: 'crawl4ai',
  version: '0.5.0',
  runId: 'run-1',
  node: 'n1',
  runStartedAt: 1_700_000_000_000,
};

/** Where the actor's output lives now: `lake.output."crawl4ai"` — one table per ACTOR. */
function T(tbl: string | null): string {
  return `${LAKE}.${OUTPUT_SCHEMA}."${tbl}"`;
}

function materialize(ctx: Ctx, src: string, sel: Partial<MaterializeSelector> = {}) {
  return writeDatasetParquet(ctx.store, { ...SEL, ...sel }, { ...ctx.cfg, sourceUri: src });
}

/** Read the materialized rows back out of the lake, the way an operator would. */
async function query(ctx: Ctx, sql: string): Promise<unknown[][]> {
  const cfg = resolveLakeConfig(ctx.store, ctx.cfg);
  const conn = await lakeConnection(ctx.store, cfg);
  const res = await conn.runAndReadAll(sql);
  return res.getRows() as unknown[][];
}

describe('writeDatasetParquet — typed output', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });

  it('materializes ACTOR COLUMNS, not $ref pointers', async () => {
    // The defect this whole change exists to fix: the table used to hold (run_id, $ref)
    // and `SELECT host, status_code` could not be written at all.
    const src = ctx.manifest([
      ctx.unit('u0.json', [{ host: 'a.example', status_code: 200 }]),
      ctx.unit('u1.json', [{ host: 'b.example', status_code: 404 }]),
    ]);
    const out = await materialize(ctx, src);

    expect(out.rows).toBe(2);
    expect(out.refObjects).toBe(2);
    // The table is the ACTOR, not `ds_<sha1>` — an operator reads the name off the path.
    expect(out.tbl).toBe(safeName(SEL.actor));
    expect(out.tbl).toBe('crawl4ai');

    const cols = (await query(ctx, `DESCRIBE SELECT * FROM ${T(out.tbl)}`)).map((r) => String(r[0]));
    expect(cols).toContain('host');
    expect(cols).toContain('status_code');
    expect(cols).not.toContain('$ref');

    const rows = await query(ctx, `SELECT host, status_code FROM ${T(out.tbl)} ORDER BY host`);
    expect(rows).toEqual([
      ['a.example', 200n],
      ['b.example', 404n],
    ]);
  });

  it('stamps version, dt, node, run_id and a preserved run_started_at on every row', async () => {
    // Identity travels IN the rows as well as in the path: a reader holding one presigned
    // parquet file still knows exactly which dispatch of which version produced it.
    const src = ctx.manifest([ctx.unit('u0.json', [{ host: 'a' }])]);
    const out = await materialize(ctx, src);
    const rows = await query(
      ctx,
      `SELECT version, dt, node, run_id, epoch_ms(run_started_at) FROM ${T(out.tbl)}`
    );
    expect(rows[0]![0]).toBe(SEL.version);
    // dt is derived from the run's own start, not from when materialization happened.
    expect(rows[0]![1]).toBe(dtPartition(SEL.runStartedAt));
    expect(rows[0]![1]).toBe('2023-11-14T22-13-20');
    expect(rows[0]![2]).toBe('n1');
    expect(rows[0]![3]).toBe('run-1');
    expect(Number(rows[0]![4])).toBe(SEL.runStartedAt);
  });

  it('stores unrecorded provenance as NULL, distinguishable from every real value', async () => {
    // "Unrecorded is recorded as unrecorded". `node` is the Machine that ran the Method and
    // `version` is the Actor version that ran it; both are VARCHAR, so any string a producer
    // could emit is a value competing with the real ones. NULL is outside the domain, which is
    // the whole reason it is the representation.
    const src = ctx.manifest([ctx.unit('u0.json', [{ host: 'a' }])]);
    const out = await materialize(ctx, src, { node: null, version: null });
    const rows = await query(
      ctx,
      `SELECT node IS NULL, version IS NULL, node, version FROM ${T(out.tbl)}`
    );
    expect(rows[0]!.slice(0, 2)).toEqual([true, true]);
    expect(rows[0]![2]).toBeNull();
    expect(rows[0]![3]).toBeNull();
  });

  it('lets a reader tell a recorded Machine from unrecorded from the legacy placeholder', async () => {
    // THE THREE-WAY DISTINCTION issue 02 renders in the console, in one table, because that is
    // how it will actually arrive: rows written before this change carry the literal `'w'` and
    // `'0'` the publish activity used to substitute, and they must keep reading as themselves.
    // Reinterpreting them as unrecorded would erase the difference between "nothing wrote this"
    // and "the old placeholder was written here".
    await materialize(ctx, ctx.manifest([ctx.unit('w.json', [{ host: 'legacy' }])]), {
      node: 'w',
      version: '0',
    });
    await materialize(ctx, ctx.manifest([ctx.unit('m.json', [{ host: 'measured' }])]), {
      node: 'kf-dns-01',
      version: '0.1.0',
    });
    const out = await materialize(ctx, ctx.manifest([ctx.unit('n.json', [{ host: 'unknown' }])]), {
      node: null,
      version: null,
    });

    const rows = await query(ctx, `SELECT host, node, version FROM ${T(out.tbl)} ORDER BY host`);
    expect(rows).toEqual([
      ['legacy', 'w', '0'],
      ['measured', 'kf-dns-01', '0.1.0'],
      ['unknown', null, null],
    ]);
    // And the group-by that failed on the `nscheck` run now separates them.
    const grouped = await query(
      ctx,
      `SELECT node, count(*) FROM ${T(out.tbl)} GROUP BY 1 ORDER BY 1 NULLS LAST`
    );
    expect(grouped).toEqual([
      ['kf-dns-01', 1n],
      ['w', 1n],
      [null, 1n],
    ]);
  });

  it('expands a 1→N unit blob into N rows (arun is flatMap)', async () => {
    const src = ctx.manifest([ctx.unit('u0.json', [{ host: 'a' }, { host: 'b' }, { host: 'c' }])]);
    const out = await materialize(ctx, src);
    expect(out.rows).toBe(3);
    expect(out.refObjects).toBe(1);
  });

  it('materializes inline (non-ref) entries from the manifest itself', async () => {
    const src = ctx.manifest([{ host: 'inline.example', status_code: 500 }]);
    const out = await materialize(ctx, src);
    expect(out.rows).toBe(1);
    expect(out.inlineUnits).toBe(1);
    expect(out.refObjects).toBe(0);
    const rows = await query(ctx, `SELECT host FROM ${T(out.tbl)}`);
    expect(rows).toEqual([['inline.example']]);
  });

  it('materializes inline entries from a BARE-ARRAY manifest', async () => {
    // The shape the handler stores now that a Method's result ref addresses a bare list of
    // units. The manifest read has always branched on `json_type = 'ARRAY'`; the inline path
    // did not, so it tried to unnest a `results` column that is not there.
    const src = ctx.bareManifest([
      { host: 'inline.example', status_code: 500 },
      { host: 'two.example', status_code: 200 },
    ]);
    const out = await materialize(ctx, src);
    expect(out.rows).toBe(2);
    expect(out.inlineUnits).toBe(2);
    expect(out.refObjects).toBe(0);
    const rows = await query(ctx, `SELECT host FROM ${T(out.tbl)} ORDER BY host`);
    expect(rows).toEqual([['inline.example'], ['two.example']]);
  });

  it('materializes REF entries from a BARE-ARRAY manifest', async () => {
    const src = ctx.bareManifest([ctx.unit('u0.json', [{ host: 'a' }, { host: 'b' }])]);
    const out = await materialize(ctx, src);
    expect(out.rows).toBe(2);
    expect(out.refObjects).toBe(1);
    expect(out.inlineUnits).toBe(0);
  });

  it('keeps nested output nested (STRUCT / LIST), not flattened into strings', async () => {
    const src = ctx.manifest([
      ctx.unit('u0.json', [{ host: 'a', response: { headers: { ct: 'text/html' } }, tags: ['x', 'y'] }]),
    ]);
    const out = await materialize(ctx, src);
    const types = new Map(
      (await query(ctx, `DESCRIBE SELECT * FROM ${T(out.tbl)}`)).map((r) => [String(r[0]), String(r[1])])
    );
    expect(types.get('response')).toContain('STRUCT');
    expect(types.get('tags')).toContain('[]');
    // and the nested value is reachable with plain SQL — the whole point
    const rows = await query(ctx, `SELECT response.headers.ct FROM ${T(out.tbl)}`);
    expect(rows).toEqual([['text/html']]);
  });
});

describe('writeDatasetParquet — integrity', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });

  it('fails by name when a unit object is MISSING', async () => {
    // read_blob silently omits absent objects; unchecked, that reads as an empty result.
    const src = ctx.manifest([
      ctx.unit('u0.json', [{ host: 'a' }]),
      { $ref: { key: 'gone.json', sha256: 'a'.repeat(64), size: 3 } },
    ]);
    await expect(materialize(ctx, src)).rejects.toThrow(/missing.*gone\.json/s);
  });

  it('fails by name when a unit object is CORRUPT', async () => {
    const ref = ctx.unit('u0.json', [{ host: 'a' }]);
    ref.$ref.sha256 = 'b'.repeat(64); // manifest claims a digest the bytes do not have
    const src = ctx.manifest([ref]);
    await expect(materialize(ctx, src)).rejects.toThrow(/sha256 mismatch.*u0\.json/s);
  });

  it('fails when the manifest object itself is missing', async () => {
    await expect(materialize(ctx, join(ctx.blobs, 'no-such-manifest.json'))).rejects.toThrow(
      /manifest object missing/
    );
  });

  it('verifies the manifest sha256 against the selector on the real CAS path', async () => {
    // No sourceUri override => the writer derives the CAS key and MUST check the digest.
    const dataDir = mkdtempSync(join(tmpdir(), 'kontra-cas-'));
    const store = new ObjectStore({ backing: new MemoryStore(), prefix: '', endpoint: 'http://localhost:8333' });
    const body = JSON.stringify({ results: [{ host: 'a' }], failures: [] });
    const realSha = createHash('sha256').update(body).digest('hex');
    const casDir = join(dataDir, 'cas', realSha.slice(0, 2));
    mkdirSync(casDir, { recursive: true });
    writeFileSync(join(casDir, realSha), body);

    // Point the writer at the on-disk CAS by overriding sourceUri with the true path but
    // asserting a DIFFERENT sha — the check must reject it.
    await expect(
      writeDatasetParquet(
        store,
        { ...SEL, sha256: 'c'.repeat(64) },
        { ...lake().cfg, sourceUri: undefined, dataPath: `${dataDir}/lake/`, s3: false }
      )
    ).rejects.toThrow();
  });

  it('leaves NO partial rows behind when a later batch fails', async () => {
    // All-or-nothing per node. A first batch that committed while a second failed would be
    // a `failed` status over rows that are actually queryable — the worst of both.
    const good = Array.from({ length: 3 }, (_, i) => ctx.unit(`u${i}.json`, [{ host: `h${i}` }]));
    const src = ctx.manifest([...good, { $ref: { key: 'zz-gone.json', sha256: 'a'.repeat(64), size: 1 } }]);
    await expect(
      writeDatasetParquet(ctx.store, SEL, { ...ctx.cfg, batchSize: 2, sourceUri: src })
    ).rejects.toThrow(/missing/);

    const tbl = safeName(SEL.actor);
    const exists = await query(
      ctx,
      `SELECT count(*) FROM (SHOW ALL TABLES) WHERE database='${LAKE}' ` +
        `AND schema='${OUTPUT_SCHEMA}' AND name='${tbl}'`
    );
    // Either the table was never created, or it exists and holds nothing for this run.
    if (Number(exists[0]![0]) > 0) {
      const rows = await query(ctx, `SELECT count(*) FROM ${T(tbl)}`);
      expect(Number(rows[0]![0])).toBe(0);
    }
  });
});

describe('writeDatasetParquet — empty, batching and evolution', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });

  it('reports an empty node as a SUCCESS with zero rows, not a failure', async () => {
    const out = await materialize(ctx, ctx.manifest([]));
    expect(out.rows).toBe(0);
    expect(out.tbl).toBeNull();
    expect(out.refObjects).toBe(0);
  });

  it('commits every batch of a multi-batch node', async () => {
    const refs = Array.from({ length: 7 }, (_, i) => ctx.unit(`u${i}.json`, [{ host: `h${i}` }]));
    const out = await writeDatasetParquet(ctx.store, SEL, {
      ...ctx.cfg,
      batchSize: 2,
      sourceUri: ctx.manifest(refs),
    });
    expect(out.rows).toBe(7);
    const rows = await query(ctx, `SELECT count(*) FROM ${T(out.tbl)}`);
    expect(Number(rows[0]![0])).toBe(7);
  });

  it('evolves the schema when a later batch carries a new field', async () => {
    // Heterogeneous actor output is normal. Without ADD COLUMN, `INSERT BY NAME` would
    // reject the unknown column and fail the whole node.
    const src = ctx.manifest([
      ctx.unit('u0.json', [{ host: 'a' }]),
      ctx.unit('u1.json', [{ host: 'b', discovered_at: '2026-08-03' }]),
    ]);
    const out = await writeDatasetParquet(ctx.store, SEL, { ...ctx.cfg, batchSize: 1, sourceUri: src });
    expect(out.rows).toBe(2);
    // Cast on read: DuckDB infers `discovered_at` as a real DATE, which is the point —
    // the column is typed, not a string the operator has to parse in every query.
    const rows = await query(
      ctx,
      `SELECT host, CAST(discovered_at AS VARCHAR) FROM ${T(out.tbl)} ORDER BY host`
    );
    expect(rows).toEqual([
      ['a', null],
      ['b', '2026-08-03'],
    ]);
    const types = new Map(
      (await query(ctx, `DESCRIBE SELECT * FROM ${T(out.tbl)}`)).map((r) => [String(r[0]), String(r[1])])
    );
    expect(types.get('discovered_at')).toBe('DATE');
  });

  it('reports the object GETs it issued rather than leaving them unmeasured', async () => {
    const src = ctx.manifest([ctx.unit('u0.json', [{ host: 'a' }]), ctx.unit('u1.json', [{ host: 'b' }])]);
    const out = await materialize(ctx, src);
    // 1 manifest + (verify + read_json) per unit object.
    expect(out.objectGets).toBe(1 + 2 * 2);
  });

  it('does not let the STORAGE PATH leak columns into the typed table', async () => {
    // Unit blobs live under `units/run=…/dt=…/actor=…/shard=…/unit=…/`, and read_json will
    // happily read that `k=v` path shape as hive partitions and add five columns that are
    // storage layout, not actor output. A live run materialized exactly that before this
    // was pinned; `run` even duplicated `run_id`.
    const hive = lake();
    const name = 'run=r1/dt=2026-08-03/actor=echo/shard=0001/unit=0/u.json';
    mkdirSync(join(hive.blobs, 'run=r1/dt=2026-08-03/actor=echo/shard=0001/unit=0'), { recursive: true });
    const body = JSON.stringify([{ host: 'a' }]);
    writeFileSync(join(hive.blobs, name), body);
    const ref = {
      $ref: { key: name, sha256: createHash('sha256').update(body).digest('hex'), size: body.length },
    };
    const out = await writeDatasetParquet(hive.store, SEL, {
      ...hive.cfg,
      sourceUri: hive.manifest([ref]),
    });
    const cols = (await query(hive, `DESCRIBE SELECT * FROM ${T(out.tbl)}`)).map((r) => String(r[0]));
    // The writer's own five stamps, and the actor's one field. Nothing from the blob path —
    // note the blob's own `dt=2026-08-03` did NOT overwrite the dispatch's dt.
    expect(cols.sort()).toEqual(['dt', 'host', 'node', 'run_id', 'run_started_at', 'version']);
    const dt = await query(hive, `SELECT DISTINCT dt FROM ${T(out.tbl)}`);
    expect(dt).toEqual([[dtPartition(SEL.runStartedAt)]]);
  });

  it('partitions by (version, dt) so a dispatch never shares a data file with another', async () => {
    // Exact-dispatch presigned files are an authorization boundary: one presigned object must
    // contain one dispatch only. Partitioning is what makes that physically true.
    const first = Date.UTC(2026, 7, 3, 19, 42, 7);
    const second = Date.UTC(2026, 7, 3, 20, 5, 0); // same hour bucket would have merged these
    await materialize(ctx, ctx.manifest([ctx.unit('a.json', [{ host: 'a' }])]), {
      runId: 'run-A',
      runStartedAt: first,
    });
    const out = await materialize(ctx, ctx.manifest([ctx.unit('b.json', [{ host: 'b' }])]), {
      runId: 'run-B',
      runStartedAt: second,
    });
    expect(out.bytes).toBeGreaterThan(0);
    expect(out.snapshotId).not.toBeNull();

    const rows = await query(ctx, `SELECT dt, count(*) FROM ${T(out.tbl)} GROUP BY 1 ORDER BY 1`);
    expect(rows.map((r) => [String(r[0]), Number(r[1])])).toEqual([
      ['2026-08-03T19-42-07', 1],
      ['2026-08-03T20-05-00', 1],
    ]);

    // …and physically: every data file sits under exactly one dispatch's directory.
    const files = (
      await query(ctx, `SELECT data_file FROM ducklake_list_files('${LAKE}', '${out.tbl}', schema := '${OUTPUT_SCHEMA}')`)
    ).map((r) => String(r[0]));
    expect(files.length).toBeGreaterThan(1);
    for (const f of files) {
      expect(f).toContain('/version=0.5.0/');
      const dts = ['2026-08-03T19-42-07', '2026-08-03T20-05-00'].filter((d) => f.includes(`/dt=${d}/`));
      expect(dts).toHaveLength(1);
    }
  });

  it('keeps each ACTOR in its own table — a new version is a partition, not a new table', async () => {
    const a = await materialize(ctx, ctx.manifest([ctx.unit('a.json', [{ host: 'a' }])]));
    const other = await materialize(ctx, ctx.manifest([ctx.unit('b.json', [{ z: 1 }])]), { actor: 'other' });
    // Two actors, two tables, each named after its actor.
    expect(a.tbl).toBe('crawl4ai');
    expect(other.tbl).toBe('other');

    // A second version of the SAME actor stays in the same table: version is a partition
    // column, so `SELECT … WHERE version = …` works without knowing another table name.
    const v2 = await materialize(ctx, ctx.manifest([ctx.unit('c.json', [{ host: 'c' }])]), {
      version: '0.6.0',
    });
    expect(v2.tbl).toBe(a.tbl);
    const rows = await query(ctx, `SELECT version, count(*) FROM ${T(a.tbl)} GROUP BY 1 ORDER BY 1`);
    expect(rows.map((r) => [String(r[0]), Number(r[1])])).toEqual([
      ['0.5.0', 1],
      ['0.6.0', 1],
    ]);
  });
});

/**
 * The partition value and its inverse are ONE fact, so they are pinned together (ADR 0029 §2's
 * warning: a second datetime format is a second thing to get wrong). `withDatasetNames` renders a
 * derived name's datetime by feeding a row's own `dt` back through this pair, so a drift between
 * them would rename every Dataset the ledger never saw.
 */
describe('dtPartition / parseDtPartition — one format, both directions', () => {
  it('round-trips a partition value back to the second it denotes', () => {
    const t = Date.UTC(2026, 7, 19, 14, 32, 7);
    expect(parseDtPartition(dtPartition(t))).toBe(t);
    // And the other way: rendering what was parsed returns the identical string, which is the
    // property the name renderer actually leans on.
    expect(dtPartition(parseDtPartition('2026-08-19T14-32-07'))).toBe('2026-08-19T14-32-07');
  });

  it('drops sub-second precision, because the partition has none', () => {
    const t = Date.UTC(2026, 7, 19, 14, 32, 7) + 456;
    expect(parseDtPartition(dtPartition(t))).toBe(t - 456);
  });

  it('reads a malformed or absent partition as the epoch rather than throwing', () => {
    // A listing must not fail on one unreadable row — the same best-effort rule the lifecycle and
    // owner reads follow.
    expect(parseDtPartition('')).toBe(0);
    expect(parseDtPartition('2026-08-19 14:32:07')).toBe(0);
    expect(parseDtPartition(undefined as unknown as string)).toBe(0);
    expect(dtPartition(parseDtPartition('nonsense'))).toBe(dtPartition(0));
  });
});

/**
 * WHERE THE CATALOG IS, AND WHAT SCHEMA ITS METADATA LIVES IN (ADR 0031 §1b).
 *
 * `ducklake-postgres` is gone and the connstring is an override nobody sets, so these two facts
 * moved from "the thing compose configures" to "the thing the code decides" — which is exactly
 * when they need pinning.
 */
describe('catalog resolution', () => {
  const env = { ...process.env };
  beforeEach(() => {
    process.env = { ...env };
    delete process.env.KONTRA_DUCKLAKE_CATALOG;
    delete process.env.KONTRA_DATA_DIR;
    delete process.env.KONTRA_DUCKLAKE_DATA_PATH;
  });
  afterAll(() => {
    process.env = env;
  });

  const store = () => new ObjectStore({ backing: new MemoryStore(), prefix: '', endpoint: 'http://s3' });

  /**
   * ONE FILE, IN THE DATA DIRECTORY. The previous default was the bare relative name
   * `orchestrator-datasets.ducklake`, resolved against the PROCESS'S CWD — harmless while a
   * connstring overrode it, and a lake outside every mounted volume once it does not.
   */
  it('defaults to a file in the data directory, not to a cwd-relative name', () => {
    process.env.KONTRA_DATA_DIR = '/srv/kontra-data';
    const cfg = resolveLakeConfig(store());
    expect(cfg.catalog).toBe(join('/srv/kontra-data', 'datasets.ducklake'));
    expect(isAbsolute(cfg.catalog)).toBe(true);
  });

  it('falls back to $KONTRA_HOME/data when no data directory is named', () => {
    process.env.KONTRA_HOME = '/home/op/.kontra';
    expect(resolveLakeConfig(store()).catalog).toBe(join('/home/op/.kontra', 'data', 'datasets.ducklake'));
  });

  /**
   * THE SETTING IS ALIVE AND UNSET. A deployment that re-splits the API and the materializer sets
   * it again and gets the shared catalog back, unchanged.
   */
  it('still honours KONTRA_DUCKLAKE_CATALOG, which is what a re-split deployment sets', () => {
    process.env.KONTRA_DUCKLAKE_CATALOG = 'postgres:dbname=kontra_ducklake host=db user=kontra';
    const cfg = resolveLakeConfig(store());
    expect(cfg.catalog).toBe('postgres:dbname=kontra_ducklake host=db user=kontra');
    expect(cfg.metaSchema).toBe('public');
  });

  /**
   * EMPTY IS UNSET. Compose substitutes `''` for a variable nobody exported, so
   * `KONTRA_DUCKLAKE_CATALOG: "${KONTRA_DUCKLAKE_CATALOG:-}"` hands this process an empty string —
   * and attaching `ducklake:` is not what "leave it unset" is supposed to do.
   */
  it('treats an EMPTY KONTRA_DUCKLAKE_CATALOG as unset, because compose writes one', () => {
    process.env.KONTRA_DUCKLAKE_CATALOG = '';
    process.env.KONTRA_DATA_DIR = '/srv/kontra-data';
    expect(resolveLakeConfig(store()).catalog).toBe(join('/srv/kontra-data', 'datasets.ducklake'));
  });

  /**
   * THE SCHEMA DIFFERS BY BACKEND, AND THERE IS NO VALUE THAT WORKS FOR BOTH. Measured below
   * against a real file catalog: `public` answers `Catalog Error`. Anything reading a `ducklake_*`
   * table takes this value rather than a constant.
   */
  it('derives the metadata schema from the backend: main for a file, public for Postgres', () => {
    process.env.KONTRA_DATA_DIR = '/srv/kontra-data';
    expect(resolveLakeConfig(store()).metaSchema).toBe('main');
    expect(resolveLakeConfig(store(), { catalog: 'postgres:host=db' }).metaSchema).toBe('public');
  });

  it('tells a file catalog from a server one, which is what decides the lock and the mkdir', () => {
    expect(catalogFilePath('/srv/kontra-data/datasets.ducklake')).toBe('/srv/kontra-data/datasets.ducklake');
    expect(catalogFilePath('datasets.ducklake')).toBe('datasets.ducklake');
    expect(catalogFilePath('postgres:host=db')).toBeNull();
    expect(catalogFilePath('postgresql://u@h/db')).toBeNull();
  });
});

/**
 * A FILE CATALOG UNDER THE CONCURRENCY THE MATERIALIZER ACTUALLY CREATES (issue 10).
 *
 * The question the slice had to answer is not "does DuckLake work on a file" — it always has —
 * but whether ONE PROCESS writing it is safe while the same process reads it, and what happens
 * when a SECOND process tries. Both are measured here rather than asserted.
 */
describe('the file catalog under concurrency', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });

  /**
   * THE ACCEPTANCE CRITERION: a materializing run and a concurrent query against the same Dataset
   * both succeed. `writeDatasetParquet` holds an OPEN TRANSACTION on the shared connection for the
   * whole node, and with the roles merged the API's reads run on that same connection — so this is
   * the case where a page read could have found a half-written table or a `BEGIN` inside a `BEGIN`.
   */
  it('serves a query issued while a materialization is in flight', async () => {
    const first = ctx.manifest([ctx.unit('c0.json', [{ host: 'a.example', ok: true }])]);
    await materialize(ctx, first);

    const second = ctx.manifest([
      ctx.unit('c1.json', Array.from({ length: 200 }, (_, i) => ({ host: `h${i}.example`, ok: false }))),
    ]);
    // Both in flight at once, on the one connection the materializer and the readers share.
    const [out, during] = await Promise.all([
      materialize(ctx, second, { runId: 'run-2' }),
      query(ctx, `SELECT count(*) FROM ${T('crawl4ai')}`),
    ]);
    expect(out.rows).toBe(200);
    // The read saw a consistent table — the first run's row, or both runs' — never a partial batch
    // and never an error.
    expect([1n, 201n]).toContain(during[0]![0] as bigint);

    const after = await query(ctx, `SELECT count(*) FROM ${T('crawl4ai')}`);
    expect(after[0]![0]).toBe(201n);
  });

  /**
   * THE METADATA SCHEMA, PROVEN AGAINST A REAL FILE CATALOG rather than read off a doc page. This
   * is the measurement behind `metaSchemaFor`, and behind the two Go queries that hardcoded
   * `public` (cli/dataset.go, cli/explore.go).
   */
  it('keeps ducklake_* under `main`, and answers a Catalog Error for `public`', async () => {
    await materialize(ctx, ctx.manifest([ctx.unit('m0.json', [{ host: 'a.example' }])]));
    const rows = await query(ctx, `SELECT count(*) FROM __ducklake_metadata_lake.main.ducklake_table`);
    expect(Number(rows[0]![0])).toBeGreaterThan(0);
    await expect(
      query(ctx, `SELECT count(*) FROM __ducklake_metadata_lake.public.ducklake_table`)
    ).rejects.toThrow(/public/);
  });

  /**
   * AND THE SECOND PROCESS IS REFUSED BY DUCKDB ITSELF. This is the fact `catalogLock.ts` exists
   * to move to boot time: the collision is real, it is total, and on its own it surfaces from
   * inside a retrying activity rather than from a start-up that failed.
   *
   * A SEPARATE `DuckDBInstance` IS NOT A SEPARATE PROCESS, so this test spawns one. POSIX record
   * locks are per-process — two attaches in ONE process do not conflict at all, measured — which
   * is precisely why an in-process assertion here would have proven nothing.
   */
  it('refuses a SECOND PROCESS attaching the same catalog, read-only included', async () => {
    await materialize(ctx, ctx.manifest([ctx.unit('p0.json', [{ host: 'a.example' }])]));
    const cfg = resolveLakeConfig(ctx.store, ctx.cfg);
    await lakeConnection(ctx.store, cfg); // held read-write by THIS process for the whole test

    const probe = `
      const { DuckDBInstance } = require(${JSON.stringify(require.resolve('@duckdb/node-api'))});
      (async () => {
        const c = await (await DuckDBInstance.create()).connect();
        await c.run('INSTALL ducklake; LOAD ducklake;');
        await c.run("ATTACH 'ducklake:" + process.argv[1] + "' AS lake (DATA_PATH '" + process.argv[2] + "', READ_ONLY)");
        console.log('ATTACHED');
      })().catch((e) => { console.log('REFUSED: ' + String(e.message).split('\\n')[0]); });
    `;
    const res = spawnSync(process.execPath, ['-e', probe, cfg.catalog, cfg.dataPath], {
      encoding: 'utf8',
      timeout: 60_000,
    });
    expect(res.stdout).toContain('REFUSED');
    expect(res.stdout).toMatch(/lock/i);
  }, 90_000);

  /**
   * THE LEDGER IS THE OTHER THING THAT LIVED IN THAT POSTGRES (ADR 0017's second authority), and
   * it lands in the SAME directory as the catalog — two files, one data directory. It kept its own
   * `kontra` SCHEMA on Postgres precisely so catalog maintenance could not drop it; on SQLite the
   * separation is stronger still, because it is a different FILE.
   */
  it('reads and writes the materialization ledger beside the file catalog', async () => {
    const cfg = resolveLakeConfig(ctx.store, ctx.cfg);
    const ledger = join(dirname(cfg.catalog), 'orchestrator.db');
    const store = new MaterializationStore({ url: ledger });

    const out = await materialize(ctx, ctx.manifest([ctx.unit('l0.json', [{ host: 'a.example' }])]));
    const key = {
      runId: SEL.runId,
      actor: SEL.actor,
      version: SEL.version!,
      node: SEL.node!,
      schemaVersion: MATERIALIZATION_SCHEMA_VERSION,
    };
    await store.claim(key, SEL.runStartedAt);
    await store.complete(key, { tbl: out.tbl!, rows: out.rows, bytes: out.bytes, snapshotId: out.snapshotId });

    const back = await store.get(key);
    expect(back?.state).toBe('complete');
    expect(back?.rows).toBe(out.rows);
    expect(back?.tbl).toBe(out.tbl);
    // Two files, both real, neither inside the other.
    expect(existsSync(ledger)).toBe(true);
    expect(existsSync(cfg.catalog)).toBe(true);
    await store.close();
  });
});
