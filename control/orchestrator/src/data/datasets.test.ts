/**
 * The read side: which actors have output, which dispatches of theirs exist, and which
 * parquet files belong to ONE dispatch.
 *
 * Identity is the path now — `output/<actor>/version=<v>/dt=<dispatch>/…parquet` — so these
 * run against a REAL DuckLake whose DATA_PATH is a local dir standing in for S3, with real
 * per-unit blob files behind `$ref` entries. Catalog listing, partition pruning and
 * per-dispatch data-file resolution all execute for real; only the storage backend differs.
 */

import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { beforeEach, describe, expect, it } from 'vitest';

import { MemoryStore, ObjectStore } from '../codec/objectStore';
import {
  datasetOwnerKey,
  datasetProvenance,
  datasetStateKey,
  deleteTemporaryDataset,
  listDatasets,
  listOutputActors,
  NoSuchDatasetError,
  NotTemporaryDatasetError,
  previewDataset,
  runFiles,
  withSharedDatasets,
  type DatasetInfo,
} from './datasets';
import { MATERIALIZATION_SCHEMA_VERSION, type MaterializationRecord } from './materialization';
import {
  LAKE,
  STANDALONE_SCHEMA,
  dtPartition,
  lakeConnection,
  promoteInto,
  resetLakeConnections,
  resolveLakeConfig,
  safeName,
  writeDatasetParquet,
  type LakeConfig,
} from './parquet';

interface Ctx {
  store: ObjectStore;
  cfg: Partial<LakeConfig>;
  /** Directory the per-unit blob keys resolve under. */
  blobs: string;
}

function lake(): Ctx {
  const dir = mkdtempSync(join(tmpdir(), 'kontra-lake-'));
  const dataPath = join(dir, 'data');
  const blobs = join(dir, 'blobs');
  mkdirSync(dataPath, { recursive: true });
  mkdirSync(blobs, { recursive: true });
  // endpoint set so presignGet can sign (a purely local HMAC — no network).
  const store = new ObjectStore({ backing: new MemoryStore(), prefix: '', endpoint: 'http://localhost:8333' });
  return {
    store,
    blobs,
    cfg: { catalog: join(dir, 'cat.ducklake'), dataPath: `${dataPath}/`, s3: false, blobBase: `${blobs}/` },
  };
}

/** One node's dispatch identity — the grain both the writer and the ledger are keyed on. */
interface Sel {
  actor: string;
  version: string;
  runId: string;
  node: string;
  runStartedAt: number;
}

/**
 * The same dispatch with its provenance UNRECORDED, which {@link Sel} cannot express: `node` is
 * the Machine that ran the Method and `version` the Actor version, and "nothing recorded one" is
 * SQL NULL rather than any string. The ledger's `Sel` stays non-null because a ledger row always
 * names both.
 */
type ProvenanceSel = Omit<Sel, 'node' | 'version'> & { node: string | null; version: string | null };

let seq = 0;

/**
 * Materialize one node's output through the real claim-check path: a per-unit blob on disk
 * and a manifest that points at it by `$ref` + sha256 — the shape every live run produces.
 */
async function write(ctx: Ctx, sel: ProvenanceSel, results: unknown[]): Promise<void> {
  const name = `u${(seq += 1)}.json`;
  const body = JSON.stringify(results);
  writeFileSync(join(ctx.blobs, name), body);
  const ref = {
    $ref: { key: name, sha256: createHash('sha256').update(body).digest('hex'), size: body.length },
  };
  const src = join(mkdtempSync(join(tmpdir(), 'kontra-src-')), 'blob.json');
  writeFileSync(src, JSON.stringify({ results: [ref], failures: [] }));
  await writeDatasetParquet(
    ctx.store,
    { sha256: 'unused-when-sourceUri-is-set', ...sel },
    { ...ctx.cfg, sourceUri: src }
  );
}

/** The ledger row a completed node leaves behind — the only input `runFiles` trusts. */
function record(sel: Sel, rows = 1): MaterializationRecord {
  return {
    runId: sel.runId,
    actor: sel.actor,
    version: sel.version,
    node: sel.node,
    schemaVersion: MATERIALIZATION_SCHEMA_VERSION,
    state: 'complete',
    attempt: 1,
    rows,
    bytes: 0,
    snapshotId: null,
    tbl: safeName(sel.actor),
    error: null,
    runStartedAt: sel.runStartedAt,
    createdAt: sel.runStartedAt,
    updatedAt: sel.runStartedAt,
  };
}

// Two dispatches on one day and one on the next. Second precision matters: the middle pair
// share an hour, which an hour-grained partition would have merged into one directory.
const D1 = Date.UTC(2026, 7, 3, 19, 42, 7);
const D2 = Date.UTC(2026, 7, 3, 19, 58, 30);
const D3 = Date.UTC(2026, 7, 4, 6, 0, 0);

/**
 * issue #3 — a table whose rows are all INLINED is still a dataset.
 *
 * DUCKLAKE INLINES A SMALL WRITE. Below its threshold there is no Parquet file at all, only a
 * catalog row — so a table created by `kontra dataset create` with two rows had ZERO entries in
 * `ducklake_data_file`, and a listing driven by that table could not see it. The reporter watched
 * `kontra dataset list --local` show their dataset and `/api/datasets` not have it, with
 * `SELECT count(*)` answering 2 the whole time.
 *
 * MEASURED ON A REAL INSTALLATION before the fix: nine of twenty-one live tables had no data file
 * and every one was missing from the API. Not a standalone quirk — `output/desync` was among them.
 *
 * THE TEST WRITES DIRECTLY THROUGH THE LAKE CONNECTION rather than through the materializer,
 * because the materializer's job is to produce files and this is about what happens when nothing
 * does. That is exactly the shape `kontra dataset create` has: a `CREATE TABLE AS SELECT` against
 * the attached catalog, no writer involved.
 */
describe('a dataset with no data file', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });

  /** A table created straight in the catalog — small enough that DuckLake inlines it. */
  async function inline(name: string, rows: number): Promise<void> {
    const conn = await lakeConnection(ctx.store, resolveLakeConfig(ctx.store, ctx.cfg));
    await conn.run(`CREATE SCHEMA IF NOT EXISTS ${LAKE}.${STANDALONE_SCHEMA}`);
    // ZERO ROWS IS ITS OWN STATEMENT. `VALUES` with nothing in it is a parse error, so an empty
    // table is a typed `SELECT … LIMIT 0` — which is also how `kontra db anew` creates one.
    const body =
      rows === 0
        ? `SELECT * FROM (VALUES (0, 'x')) AS t(n, host) LIMIT 0`
        : `SELECT * FROM (VALUES ${Array.from({ length: rows }, (_, i) => `(${i}, 'host${i}')`).join(
            ', '
          )}) AS t(n, host)`;
    await conn.run(`CREATE OR REPLACE TABLE ${LAKE}.${STANDALONE_SCHEMA}."${name}" AS ${body}`);
  }

  it('is listed, which it was not', async () => {
    await inline('repro_ds', 2);
    const got = await listDatasets(ctx.store, {}, ctx.cfg);
    const hit = got.find((d) => d.name === 'repro_ds');
    expect(hit, `not listed — got ${got.map((d) => d.name).join(', ')}`).toBeDefined();
    expect(hit!.kind).toBe('standalone');
  });

  it('reports its real row count, not zero', async () => {
    // A dataset that is visibly there and visibly empty is a worse lie than one that is absent:
    // the operator stops looking for the rows instead of looking for the dataset.
    await inline('repro_ds', 2);
    const hit = (await listDatasets(ctx.store, {}, ctx.cfg)).find((d) => d.name === 'repro_ds');
    expect(hit?.rows).toBe(2);
  });

  it('is findable by name and by kind, like any other', async () => {
    await inline('repro_ds', 2);
    expect((await listDatasets(ctx.store, { name: 'repro_ds' }, ctx.cfg)).map((d) => d.name)).toEqual([
      'repro_ds',
    ]);
    expect(
      (await listDatasets(ctx.store, { kind: 'standalone' }, ctx.cfg)).map((d) => d.name)
    ).toContain('repro_ds');
  });

  it('reports a genuinely empty table as zero rather than hiding it', async () => {
    // The other half of the same change: a table created and never written is REAL, and `0` is the
    // honest answer for it. Collapsing the two — absent for empty, absent for inlined — is what
    // made the bug invisible.
    await inline('empty_ds', 0);
    const hit = (await listDatasets(ctx.store, {}, ctx.cfg)).find((d) => d.name === 'empty_ds');
    expect(hit).toBeDefined();
    expect(hit?.rows).toBe(0);
  });
});

describe('listDatasets', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });

  it('groups by (actor, version, dispatch) with the newest dispatch first', async () => {
    // A sharded dispatch is ONE dataset: its nodes fold together, and their rows sum.
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 }, [
      { url: 'a', n: 1 },
      { url: 'b', n: 2 },
    ]);
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n2', runStartedAt: D1 }, [
      { url: 'c', n: 3 },
    ]);
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r2', node: 'n1', runStartedAt: D3 }, [
      { url: 'z', n: 9 },
    ]);

    const ds = await listDatasets(ctx.store, {}, ctx.cfg);
    expect(ds.map((d) => d.dt)).toEqual([dtPartition(D3), dtPartition(D1)]);

    expect(ds[0]).toMatchObject({ kind: 'output', name: 'echo', version: '0.1.0', rows: 1 });
    // Byte size is a real catalog figure now, not the hardcoded 0 the old listing returned.
    expect(ds[0]!.bytes).toBeGreaterThan(0);

    expect(ds[1]).toMatchObject({ kind: 'output', name: 'echo', version: '0.1.0', rows: 3 });
  });

  // The point of the rewrite. A graph node id is a scheduling detail: the two nodes above fold
  // into one dataset, and nothing in the listing names them. Reading them back cost a full
  // column scan of every file in the lake (`list(DISTINCT node)`), which is what made the
  // Datasets page slow in proportion to how much output existed.
  it('never surfaces a graph node id', async () => {
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 }, [{ a: 1 }]);
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n2', runStartedAt: D1 }, [{ a: 2 }]);

    const ds = await listDatasets(ctx.store, {}, ctx.cfg);
    expect(ds).toHaveLength(1);
    expect(JSON.stringify(ds)).not.toMatch(/\bn[12]\b/);
  });

  it('narrows to one dataset by name, and to one version of it', async () => {
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 }, [{ a: 1 }]);
    await write(ctx, { actor: 'echo', version: '0.2.0', runId: 'r2', node: 'n1', runStartedAt: D3 }, [{ a: 2 }]);
    await write(ctx, { actor: 'other', version: '0.1.0', runId: 'r3', node: 'n1', runStartedAt: D2 }, [{ z: 9 }]);

    const echo = await listDatasets(ctx.store, { name: 'echo' }, ctx.cfg);
    expect(echo.map((d) => `${d.name}@${d.version}`)).toEqual(['echo@0.2.0', 'echo@0.1.0']);

    const v1 = await listDatasets(ctx.store, { name: 'echo', version: '0.1.0' }, ctx.cfg);
    expect(v1).toHaveLength(1);
    expect(v1[0]).toMatchObject({ name: 'echo', version: '0.1.0' });

    // A dataset with no output is not an error, it is an empty answer.
    expect(await listDatasets(ctx.store, { name: 'never-ran' }, ctx.cfg)).toEqual([]);
  });

  it('treats dt as a PREFIX, so a date selects every dispatch of that day', async () => {
    // Operators think in days ("what did we run on the 3rd?") but address one dispatch when
    // they need exactly one. A prefix match is what serves both off the same field.
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 }, [{ a: 1 }]);
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r2', node: 'n1', runStartedAt: D2 }, [{ a: 2 }]);
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r3', node: 'n1', runStartedAt: D3 }, [{ a: 3 }]);

    const day = await listDatasets(ctx.store, { dt: '2026-08-03' }, ctx.cfg);
    expect(day.map((d) => d.dt)).toEqual([dtPartition(D2), dtPartition(D1)]);

    const one = await listDatasets(ctx.store, { dt: dtPartition(D1) }, ctx.cfg);
    expect(one.map((d) => d.dt)).toEqual([dtPartition(D1)]);

    // A selector that matches nothing must return NOTHING, not everything — the failure mode
    // where an unmatched filter is silently dropped reads as "this dataset exists on that day".
    expect(await listDatasets(ctx.store, { dt: '1999-01-01' }, ctx.cfg)).toEqual([]);
    expect(await listDatasets(ctx.store, { version: '9.9.9' }, ctx.cfg)).toEqual([]);
  });

  // ADR 0023 §11 reaches the operator or it does nothing. The lifecycle is written as an object
  // beside the data (the catalog's row count is a live SUM and cannot express "still
  // appending"), so the listing has to go and read it — otherwise every Dataset arrives at the
  // UI stateless and a half-written one renders exactly like a finished one.
  it('carries the recorded lifecycle, keyed by name across every dispatch', async () => {
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 }, [{ a: 1 }]);
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r2', node: 'n1', runStartedAt: D3 }, [{ a: 2 }]);
    await write(ctx, { actor: 'probe', version: '0.1.0', runId: 'r3', node: 'n1', runStartedAt: D1 }, [{ a: 3 }]);

    await ctx.store.put(
      datasetStateKey('echo'),
      Buffer.from(JSON.stringify({ state: 'sealed' }), 'utf8')
    );

    const ds = await listDatasets(ctx.store, {}, ctx.cfg);
    for (const d of ds.filter((x) => x.name === 'echo')) expect(d.state).toBe('sealed');
    // A Dataset nobody ever wrote a lifecycle for reports NONE, not `open`: nothing is
    // appending to it and nothing ever will.
    expect(ds.find((d) => d.name === 'probe')?.state).toBeUndefined();
  });

  // temp-datasets slice 01: a temporary Dataset must be VISIBLY temporary and name its owning Run,
  // so the listing reads the ownership marker beside the data — the same way it reads the state.
  it('marks a temporary Dataset temporary and carries its owning Run', async () => {
    await write(ctx, { actor: 'tmp_a7f3', version: '0.1.0', runId: 'NsCheck-42', node: 'n1', runStartedAt: D1 }, [{ a: 1 }]);
    await write(ctx, { actor: 'lame', version: '0.1.0', runId: 'r2', node: 'n1', runStartedAt: D1 }, [{ a: 2 }]);

    await ctx.store.put(
      datasetOwnerKey('tmp_a7f3'),
      Buffer.from(JSON.stringify({ owner: 'NsCheck-42', createdAt: 1_700_000_000_000 }), 'utf8')
    );

    const ds = await listDatasets(ctx.store, {}, ctx.cfg);
    const tmp = ds.find((d) => d.name === 'tmp_a7f3')!;
    expect(tmp.temporary).toBe(true);
    expect(tmp.owner).toBe('NsCheck-42');
    // A durable Dataset has NEITHER — temporary is the marker's presence, never the name prefix.
    const lame = ds.find((d) => d.name === 'lame')!;
    expect(lame.temporary).toBeUndefined();
    expect(lame.owner).toBeUndefined();
  });

  // Which Run wrote a row is a fact the ROWS have always carried, and the catalog keeps per-file
  // min/max for `run_id` — so the listing can say it without opening a data file. This is the only
  // authority that answers for a v2 Run at all: the materialization ledger holds no dispatch for
  // one (measured on the local controller: 50 dispatches, none from the SDK path).
  it('names every Run that contributed rows to a partition, from catalog statistics alone', async () => {
    // One name, one partition, two Runs — the shape a durable Dataset reaches by being promoted
    // into twice. `dt` is the partition, so both writes are aimed at the same one.
    await write(ctx, { actor: 'lame', version: '0.1.0', runId: 'nscheck-1', node: 'n1', runStartedAt: D1 }, [{ a: 1 }]);
    await write(ctx, { actor: 'lame', version: '0.1.0', runId: 'nscheck-2', node: 'n1', runStartedAt: D1 }, [{ a: 2 }]);
    await write(ctx, { actor: 'solo', version: '0.1.0', runId: 'nscheck-3', node: 'n1', runStartedAt: D1 }, [{ a: 3 }]);

    const ds = await listDatasets(ctx.store, {}, ctx.cfg);
    const lame = ds.find((d) => d.name === 'lame')!;
    expect(lame.contributingRuns).toEqual(['nscheck-1', 'nscheck-2']);
    // Every file was written by one Run, so the set is exact rather than a bound.
    expect(lame.contributingRunsPartial).toBeUndefined();
    expect(ds.find((d) => d.name === 'solo')!.contributingRuns).toEqual(['nscheck-3']);
  });

  it('says nothing about Runs for a table that has no run_id column at all', async () => {
    const conn = await lakeConnection(ctx.store, resolveLakeConfig(ctx.store, ctx.cfg));
    await conn.run(`CREATE SCHEMA IF NOT EXISTS ${LAKE}.${STANDALONE_SCHEMA}`);
    await conn.run(`CREATE TABLE ${LAKE}.${STANDALONE_SCHEMA}.seeds AS SELECT 'acme.com' AS host`);

    const ds = await listDatasets(ctx.store, {}, ctx.cfg);
    const seeds = ds.find((d) => d.name === 'seeds')!;
    // ABSENT, not empty: "this Dataset cannot say" is a different fact from "no Run wrote it".
    expect(seeds.contributingRuns).toBeUndefined();
    expect(seeds.contributingRunsPartial).toBeUndefined();
  });

  it('lists a Dataset whose lifecycle object is unreadable rather than failing the page', async () => {
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 }, [{ a: 1 }]);
    await ctx.store.put(datasetStateKey('echo'), Buffer.from('{not json', 'utf8'));

    const ds = await listDatasets(ctx.store, {}, ctx.cfg);
    expect(ds).toHaveLength(1);
    expect(ds[0]!.state).toBeUndefined();
  });

  it('is empty when the catalog holds no output', async () => {
    expect(await listDatasets(ctx.store, {}, ctx.cfg)).toEqual([]);
  });

  it('is empty when no object store / data path is configured', async () => {
    const store = new ObjectStore({}); // disabled: no endpoint, no backing
    expect(await listDatasets(store)).toEqual([]);
  });
});

describe('listOutputActors', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });

  it('names every actor with output — the name IS the table, no registry lookup', async () => {
    await write(ctx, { actor: 'other', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 }, [{ z: 9 }]);
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 }, [{ a: 1 }]);
    await write(ctx, { actor: 'echo', version: '0.2.0', runId: 'r2', node: 'n1', runStartedAt: D3 }, [{ a: 2 }]);
    // Two versions of one actor are one table — the version is a partition, not a name.
    expect(await listOutputActors(ctx.store, ctx.cfg)).toEqual(['echo', 'other']);
  });

  it('is empty before anything has been materialized', async () => {
    expect(await listOutputActors(ctx.store, ctx.cfg)).toEqual([]);
  });
});

describe('runFiles', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });

  it('scopes a dispatch to its own version=…/dt=… files, never another dispatch’s', async () => {
    // Presigned files are an authorization boundary: handing out a URL for one dispatch must
    // not hand out another's rows. Matching BOTH partition segments is what enforces that.
    const first: Sel = { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 };
    await write(ctx, first, [{ url: 'a' }]);
    await write(ctx, { ...first, node: 'n2' }, [{ url: 'b' }]);
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r9', node: 'n1', runStartedAt: D3 }, [
      { url: 'z' },
    ]);

    // Both nodes of the dispatch fold into ONE entry — a sharded dispatch is one dataset.
    const files = await runFiles(ctx.store, [record(first), record({ ...first, node: 'n2' })], ctx.cfg);
    expect(files).toHaveLength(1);
    expect(files[0]).toMatchObject({ actor: 'echo', version: '0.1.0', dt: dtPartition(D1), table: 'echo' });
    expect(files[0]!.keys.length).toBeGreaterThan(0);
    for (const key of files[0]!.keys) {
      expect(key).toContain(`output/echo/version=0.1.0/dt=${dtPartition(D1)}/`);
      expect(key).not.toContain(dtPartition(D3));
      expect(key.endsWith('.parquet')).toBe(true);
    }
    // The columns an operator queries, straight off the table.
    const cols = files[0]!.columns.map((c) => c.name);
    expect(cols).toContain('url');
    expect(cols).toEqual(expect.arrayContaining(['version', 'dt', 'node', 'run_id', 'run_started_at']));

    // …and the keys are presignable as-is, which is the only thing the caller does with them.
    const url = await ctx.store.presignGet(files[0]!.keys[0]!);
    expect(url).toContain('http://localhost:8333/kontra/output/echo/');
    expect(url).toContain(`dt%3D${dtPartition(D1)}`); // dt=… partition dir, URL-encoded
    expect(url).toContain('X-Amz-Signature=');
  });

  it('separates two dispatches of the same actor into disjoint key sets', async () => {
    const a: Sel = { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 };
    const b: Sel = { actor: 'echo', version: '0.1.0', runId: 'r2', node: 'n1', runStartedAt: D2 };
    await write(ctx, a, [{ url: 'a' }]);
    await write(ctx, b, [{ url: 'b' }]);

    const fa = await runFiles(ctx.store, [record(a)], ctx.cfg);
    const fb = await runFiles(ctx.store, [record(b)], ctx.cfg);
    expect(fa[0]!.dt).toBe(dtPartition(D1));
    expect(fb[0]!.dt).toBe(dtPartition(D2));
    expect(fa[0]!.keys.some((k) => fb[0]!.keys.includes(k))).toBe(false);
  });

  it('returns nothing for a record whose actor never materialized', async () => {
    // A `complete` record with no files is an EMPTY SUCCESS; inventing a path for it would
    // turn "found nothing" back into a broken URL.
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 }, [{ a: 1 }]);
    const ghost = { actor: 'ghost', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 };
    expect(await runFiles(ctx.store, [record(ghost)], ctx.cfg)).toEqual([]);
  });

  it('returns nothing when the run has no records at all', async () => {
    expect(await runFiles(ctx.store, [], ctx.cfg)).toEqual([]);
  });
});

/**
 * An operator-loaded list, written the way `kontra dataset create` writes one: a plain table in
 * the `standalone` schema, no partitions.
 */
async function standalone(ctx: Ctx, name: string, values: string): Promise<void> {
  const conn = await lakeConnection(ctx.store, resolveLakeConfig(ctx.store, ctx.cfg));
  await conn.run(`CREATE SCHEMA IF NOT EXISTS ${LAKE}.${STANDALONE_SCHEMA}`);
  await conn.run(`CREATE OR REPLACE TABLE ${LAKE}.${STANDALONE_SCHEMA}."${name}" AS ${values}`);
}

describe('listDatasets — standalone lists', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });

  /**
   * The bug this pins: the Datasets page showed actor output only, so a scope list loaded with
   * `kontra dataset create` was invisible in the UI while `kontra dataset list` showed it. One
   * noun, two answers.
   */
  it('lists an operator list beside actor output', async () => {
    await standalone(ctx, 'scope_paid', `SELECT 'example.com' AS host UNION ALL SELECT 'b.com'`);
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 }, [{ a: 1 }]);

    const ds = await listDatasets(ctx.store, {}, ctx.cfg);
    expect(ds.map((d) => `${d.kind}:${d.name}`).sort()).toEqual(['output:echo', 'standalone:scope_paid']);

    const list = ds.find((d) => d.kind === 'standalone')!;
    expect(list.rows).toBe(2);
    // A standalone list has no dispatch coordinates — undefined, so the UI can say so rather
    // than rendering "v" and an empty date.
    expect(list.version).toBeUndefined();
    expect(list.dt).toBeUndefined();
  });

  it('filters to one kind', async () => {
    await standalone(ctx, 'scope_paid', `SELECT 'example.com' AS host`);
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 }, [{ a: 1 }]);

    expect((await listDatasets(ctx.store, { kind: 'standalone' }, ctx.cfg)).map((d) => d.name)).toEqual(['scope_paid']);
    expect((await listDatasets(ctx.store, { kind: 'output' }, ctx.cfg)).map((d) => d.name)).toEqual(['echo']);
  });
});

describe('previewDataset', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });

  it('returns typed columns and rows for an output dataset', async () => {
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 }, [
      { url: 'a', n: 1 },
      { url: 'b', n: 2 },
    ]);

    const p = await previewDataset(ctx.store, { kind: 'output', name: 'echo' }, ctx.cfg);
    expect(p.columns.map((c) => c.name)).toContain('url');
    expect(p.rows).toHaveLength(2);
    // The response crosses the wire as JSON: DuckDB hands integers back as BigInt, which
    // JSON.stringify throws on. jsonSafe has to have run.
    expect(() => JSON.stringify(p)).not.toThrow();
  });

  it('previews a standalone list too — the page has one code path for both', async () => {
    await standalone(ctx, 'scope_paid', `SELECT 'example.com' AS host`);
    const p = await previewDataset(ctx.store, { kind: 'standalone', name: 'scope_paid' }, ctx.cfg);
    expect(p.columns.map((c) => c.name)).toEqual(['host']);
    expect(p.rows).toEqual([['example.com']]);
  });

  it('prunes to one dispatch when scoped by version and dt', async () => {
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 }, [{ a: 1 }]);
    await write(ctx, { actor: 'echo', version: '0.2.0', runId: 'r2', node: 'n1', runStartedAt: D3 }, [{ a: 2 }]);

    const one = await previewDataset(
      ctx.store,
      { kind: 'output', name: 'echo', version: '0.2.0', dt: dtPartition(D3) },
      ctx.cfg
    );
    expect(one.rows).toHaveLength(1);
  });

  it('clamps the row count to the ceiling, however many are asked for', async () => {
    await write(
      ctx,
      { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 },
      Array.from({ length: 20 }, (_, i) => ({ i }))
    );
    expect((await previewDataset(ctx.store, { kind: 'output', name: 'echo', limit: 5 }, ctx.cfg)).rows).toHaveLength(5);
    // A limit below 1 or above the ceiling must not become "unbounded" — this route is the one
    // place a browser can ask the controller to read rows.
    expect(
      (await previewDataset(ctx.store, { kind: 'output', name: 'echo', limit: 0 }, ctx.cfg)).rows
    ).toHaveLength(1);
    expect(
      (await previewDataset(ctx.store, { kind: 'output', name: 'echo', limit: 1e9 }, ctx.cfg)).rows
    ).toHaveLength(20);
  });

  /**
   * A table name cannot be parameterised, and this one arrives from a URL path segment. The
   * only safe identifier is one the catalog already lists, so an unknown name must be rejected
   * BEFORE it is interpolated — not escaped and run.
   */
  it('rejects a name the catalog does not list, rather than interpolating it', async () => {
    await write(ctx, { actor: 'echo', version: '0.1.0', runId: 'r1', node: 'n1', runStartedAt: D1 }, [{ a: 1 }]);

    await expect(previewDataset(ctx.store, { kind: 'output', name: 'nope' }, ctx.cfg)).rejects.toThrow(
      /no output dataset named/
    );
    await expect(
      previewDataset(ctx.store, { kind: 'output', name: 'echo" UNION SELECT 1 --' }, ctx.cfg)
    ).rejects.toThrow(/no output dataset named/);
    // `echo` exists as OUTPUT; asking for it as a standalone list must not find it.
    await expect(previewDataset(ctx.store, { kind: 'standalone', name: 'echo' }, ctx.cfg)).rejects.toThrow(
      /no standalone dataset named/
    );
    // AND THE REFUSAL IS A TYPE, not a sentence. The route turns this into a 404 and everything
    // else into a 502; it used to make that call with `msg.startsWith('no ')`, so the difference
    // between "that dataset is gone" and "kontra is broken" was the first word of a string the
    // console also RENDERS. Both facts are pinned: the class, and the wording that is displayed.
    await expect(previewDataset(ctx.store, { kind: 'output', name: 'nope' }, ctx.cfg)).rejects.toThrow(
      NoSuchDatasetError
    );
  });
});

/**
 * Which MACHINES wrote a Dataset.
 *
 * The measured failure: a four-Machine `nscheck` run put 1,246 rows into `lame`, and
 * `SELECT node, version, count(*) … GROUP BY 1,2` returned exactly one row — `['w','0',1246]`.
 * One distinct node across four Machines, so "four were asked, three produced rows" was a
 * sentence nothing could form. These run against a REAL DuckLake through the real writer, so the
 * three values a reader must tell apart are the ones the lake actually hands back.
 */
describe('datasetProvenance', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });

  /** One Machine's contribution to the `lame` Dataset, with its Actor version. */
  const from = (node: string | null, version: string | null, runId: string): ProvenanceSel => ({
    actor: 'lame',
    version,
    runId,
    node,
    runStartedAt: D1,
  });

  it('names every Machine that wrote the Dataset and what each contributed', async () => {
    // Three Machines, three sizes — the shape the run status cannot report. A fourth Machine that
    // produced nothing is ABSENT here, and that absence is the finding: a Dataset says who wrote
    // it, never who was asked.
    await write(ctx, from('kf-dns-01', '0.1.0', 'r1'), [{ d: 'a' }, { d: 'b' }, { d: 'c' }]);
    await write(ctx, from('kf-dns-02', '0.1.0', 'r1'), [{ d: 'd' }, { d: 'e' }]);
    await write(ctx, from('kf-dns-03', '0.1.0', 'r1'), [{ d: 'f' }]);

    const p = await datasetProvenance(ctx.store, { kind: 'output', name: 'lame' }, ctx.cfg);
    expect(p.carriesProvenance).toBe(true);
    expect(p.rows).toBe(6);
    expect(p.groups).toEqual([
      { machine: 'kf-dns-01', version: '0.1.0', run: 'r1', rows: 3 },
      { machine: 'kf-dns-02', version: '0.1.0', run: 'r1', rows: 2 },
      { machine: 'kf-dns-03', version: '0.1.0', run: 'r1', rows: 1 },
    ]);
  });

  it('keeps unrecorded, recorded and the legacy placeholder as three separate buckets', async () => {
    // `null` is "nothing knew the Machine"; `'w'` is the placeholder publish used to substitute,
    // read back UNTOUCHED. Folding either into the other loses a fact, and the console is the
    // only layer allowed to say which is which.
    await write(ctx, from('kf-dns-01', '0.1.0', 'r1'), [{ d: 'a' }, { d: 'b' }]);
    await write(ctx, from('w', '0', 'r2'), [{ d: 'c' }]);
    await write(ctx, from(null, null, 'r3'), [{ d: 'd' }]);

    const p = await datasetProvenance(ctx.store, { kind: 'output', name: 'lame' }, ctx.cfg);
    expect(p.rows).toBe(4);
    expect(p.groups).toEqual([
      { machine: 'kf-dns-01', version: '0.1.0', run: 'r1', rows: 2 },
      { machine: 'w', version: '0', run: 'r2', rows: 1 },
      { machine: null, version: null, run: 'r3', rows: 1 },
    ]);
  });

  it('reads a Dataset written entirely before the wire change back as the placeholder it holds', async () => {
    // `lame`'s real shape today: every row `('w','0')`. It arrives as RECORDED buckets, not as
    // unrecorded — those are different facts, and only one of them is a bug in the writer.
    //
    // Two Runs of one workflow, which is exactly `lame`'s 1,246-vs-623 shape: the Machine is the
    // same dead placeholder in both, and the RUN is what tells them apart. That is why the buckets
    // carry all three and the console folds per dimension rather than the query doing it.
    await write(ctx, from('w', '0', 'r1'), [{ d: 'a' }, { d: 'b' }, { d: 'c' }]);
    await write(ctx, from('w', '0', 'r2'), [{ d: 'd' }]);

    const p = await datasetProvenance(ctx.store, { kind: 'output', name: 'lame' }, ctx.cfg);
    expect(p.groups).toEqual([
      { machine: 'w', version: '0', run: 'r1', rows: 3 },
      { machine: 'w', version: '0', run: 'r2', rows: 1 },
    ]);
    expect(p.rows).toBe(4);
    expect(p.carriesRun).toBe(true);
  });

  it('reads a Dataset nothing recorded a Machine for as one unrecorded bucket', async () => {
    await write(ctx, from(null, null, 'r1'), [{ d: 'a' }, { d: 'b' }]);

    const p = await datasetProvenance(ctx.store, { kind: 'output', name: 'lame' }, ctx.cfg);
    expect(p.carriesProvenance).toBe(true);
    // The Machine is unrecorded and the Run is not: `run_id` has been written since the first
    // typed materialization, so a Dataset can be silent about who ran it and still say which Run
    // it belongs to. Two dimensions, gated separately.
    expect(p.groups).toEqual([{ machine: null, version: null, run: 'r1', rows: 2 }]);
  });

  it('counts every dispatch of the Dataset — the rows the console’s editor reads', async () => {
    // Opening a Dataset runs `SELECT * FROM <name>` with no partition filter, so the panel beside
    // that editor describes the same rows. Two dispatches, two Actor versions — and the second
    // version is exactly the fact an operator needs before comparing across them.
    await write(ctx, { ...from('kf-dns-01', '0.1.0', 'r1'), runStartedAt: D1 }, [{ d: 'a' }]);
    await write(ctx, { ...from('kf-dns-01', '0.2.0', 'r2'), runStartedAt: D3 }, [{ d: 'b' }]);

    // Two rows in the listing — one per (version, dt) partition — and ONE provenance answer.
    expect(await listDatasets(ctx.store, { name: 'lame' }, ctx.cfg)).toHaveLength(2);

    const p = await datasetProvenance(ctx.store, { kind: 'output', name: 'lame' }, ctx.cfg);
    expect(p.rows).toBe(2);
    expect(p.groups.map((g) => g.version).sort()).toEqual(['0.1.0', '0.2.0']);
    expect(new Set(p.groups.map((g) => g.machine))).toEqual(new Set(['kf-dns-01']));
  });

  it('totals in the SAME statement as the buckets, never a second count', async () => {
    // A Dataset a Run is still appending to grows between two queries, so a share divided out of
    // two separately-issued counts is wrong by whatever landed in between. `rows` is DuckDB's own
    // `sum(count(*)) OVER ()` from the one scan, so the identity holds by construction.
    await write(ctx, from('kf-dns-01', '0.1.0', 'r1'), [{ d: 'a' }, { d: 'b' }]);
    await write(ctx, from('kf-dns-02', '0.1.0', 'r1'), [{ d: 'c' }]);
    await write(ctx, from(null, null, 'r1'), [{ d: 'e' }]);

    const p = await datasetProvenance(ctx.store, { kind: 'output', name: 'lame' }, ctx.cfg);
    expect(p.groups.reduce((n, g) => n + g.rows, 0)).toBe(p.rows);
  });

  it('reports an operator-loaded list as carrying no provenance, without scanning it', async () => {
    // A standalone list has none of the three columns: nothing ever claimed to know a Machine for
    // it, and no Run wrote it. That is a different fact from "every row is unrecorded", and the
    // console says so differently.
    await standalone(ctx, 'scope_paid', `SELECT * FROM (VALUES ('a.example'), ('b.example')) t(host)`);

    const p = await datasetProvenance(ctx.store, { kind: 'standalone', name: 'scope_paid' }, ctx.cfg);
    expect(p).toEqual({
      name: 'scope_paid',
      kind: 'standalone',
      rows: 0,
      groups: [],
      carriesProvenance: false,
      carriesRun: false,
      measuredAt: expect.any(Number),
    });
  });

  it('answers about the dimension a table carries, and stays NULL about the one it does not', async () => {
    // A table with `run_id` and no Machine columns. It is answerable about Runs and silent about
    // Machines — and the missing columns come back as the NULL the lake already spells
    // "unrecorded" with, so a bucket has one shape whatever the Dataset happens to carry.
    await standalone(
      ctx,
      'seeded',
      `SELECT * FROM (VALUES ('a.example', 'r1'), ('b.example', 'r1'), ('c.example', 'r2')) t(host, run_id)`
    );

    const p = await datasetProvenance(ctx.store, { kind: 'standalone', name: 'seeded' }, ctx.cfg);
    expect(p.carriesProvenance).toBe(false);
    expect(p.carriesRun).toBe(true);
    expect(p.rows).toBe(3);
    expect(p.groups).toEqual([
      { machine: null, version: null, run: 'r1', rows: 2 },
      { machine: null, version: null, run: 'r2', rows: 1 },
    ]);
  });

  it('rejects a name the catalog does not list, rather than interpolating it', async () => {
    await write(ctx, from('kf-dns-01', '0.1.0', 'r1'), [{ d: 'a' }]);

    await expect(
      datasetProvenance(ctx.store, { kind: 'output', name: 'nope' }, ctx.cfg)
    ).rejects.toThrow(/no output dataset named/);
    await expect(
      datasetProvenance(ctx.store, { kind: 'output', name: 'lame" UNION SELECT 1 --' }, ctx.cfg)
    ).rejects.toThrow(/no output dataset named/);
    // Typed, for the reason spelled out on the preview's copy of this test: the route's 404 is a
    // classification, and a classification must not be a substring of the operator-facing text.
    await expect(
      datasetProvenance(ctx.store, { kind: 'output', name: 'nope' }, ctx.cfg)
    ).rejects.toThrow(NoSuchDatasetError);
  });

  it('is empty when no object store / data path is configured', async () => {
    const store = new ObjectStore({}); // disabled: no endpoint, no backing
    expect(await datasetProvenance(store, { kind: 'output', name: 'lame' })).toEqual({
      name: 'lame',
      kind: 'output',
      rows: 0,
      groups: [],
      carriesProvenance: false,
      carriesRun: false,
      measuredAt: expect.any(Number),
    });
  });

  // --- what each count counted --------------------------------------------------------------

  /**
   * Two Runs, one Dataset name — `lame`'s measured shape, and the whole reason this slice exists.
   *
   * `SELECT count(*) FROM lame` returned 1,246 and the listing showed 623 against the same name.
   * Both correct; neither labelled. Below, the two numbers come out of ONE statement, which is
   * what makes "623 of 1,246 rows, from this Run" a fact about a single moment.
   */
  it('separates two Runs of one workflow that appended to the same name', async () => {
    await write(ctx, from('kf-dns-01', '0.1.0', 'r1'), [{ d: 'a' }, { d: 'b' }, { d: 'c' }]);
    await write(ctx, from('kf-dns-02', '0.1.0', 'r1'), [{ d: 'd' }]);
    await write(ctx, from('kf-dns-01', '0.1.0', 'r2'), [{ d: 'e' }, { d: 'f' }]);

    const p = await datasetProvenance(ctx.store, { kind: 'output', name: 'lame' }, ctx.cfg);
    expect(p.rows).toBe(6);
    expect(p.carriesRun).toBe(true);

    // Folded per Run — the same fold the console does for the Machine dimension. One Machine
    // wrote under both Runs, so no bucket answers this on its own.
    const perRun = new Map<string | null, number>();
    for (const g of p.groups) perRun.set(g.run, (perRun.get(g.run) ?? 0) + g.rows);
    expect([...perRun]).toEqual([
      ['r1', 4],
      ['r2', 2],
    ]);
  });

  /**
   * A Dataset ONE Run wrote still names that Run.
   *
   * The bucket is not omitted for being unambiguous today: the second Run of the same workflow is
   * what turns yesterday's obvious number into today's unlabelled one, and "unlabelled because it
   * happens to be unambiguous" is how the 1,246-vs-623 confusion arrived in the first place.
   */
  it('names the Run even when exactly one Run wrote the Dataset', async () => {
    await write(ctx, from('kf-dns-01', '0.1.0', 'only-run'), [{ d: 'a' }, { d: 'b' }]);

    const p = await datasetProvenance(ctx.store, { kind: 'output', name: 'lame' }, ctx.cfg);
    expect(p.groups).toEqual([
      { machine: 'kf-dns-01', version: '0.1.0', run: 'only-run', rows: 2 },
    ]);
    expect(p.rows).toBe(2);
  });

  it('measures the buckets and the total at one moment, and says which moment', async () => {
    // The count is true of a MOMENT, not of the Dataset — a Run may still be appending. The stamp
    // is taken after the statement answered, so it dates the numbers rather than the request.
    const before = Date.now();
    await write(ctx, from('kf-dns-01', '0.1.0', 'r1'), [{ d: 'a' }]);

    const p = await datasetProvenance(ctx.store, { kind: 'output', name: 'lame' }, ctx.cfg);
    expect(p.measuredAt).toBeGreaterThanOrEqual(before);
    expect(p.measuredAt).toBeLessThanOrEqual(Date.now());
  });
});

/**
 * Deleting a temporary Dataset — the explicit half of the orphan policy (temp-datasets slice 03).
 *
 * The policy is EXPLICIT-ONLY: a temp is never swept on a clock or dropped at its Run's close, so
 * the only way one goes is a person (or slice 04's button) asking. These run against a REAL DuckLake
 * because what has to hold is what the catalog and the object-store markers do after the drop — the
 * rows gone, the markers cleaned, and crucially a Dataset promoted FROM the temp untouched.
 */
describe('deleteTemporaryDataset', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    ctx = lake();
  });

  /** Mark a Dataset temporary the way slice 01's openTempDataset does — the owner marker, apart. */
  async function own(name: string, owner: string): Promise<void> {
    await ctx.store.put(
      datasetOwnerKey(name),
      Buffer.from(JSON.stringify({ owner, createdAt: 1_700_000_000_000 }), 'utf8')
    );
  }

  it('drops a temp — its rows, its catalog entry and its markers — and reports what it freed', async () => {
    await write(ctx, { actor: 'tmp_run', version: '0.1.0', runId: 'NsCheck-1', node: 'n1', runStartedAt: D1 }, [
      { d: 'a' },
      { d: 'b' },
      { d: 'c' },
    ]);
    await own('tmp_run', 'NsCheck-1');
    // A finished Run leaves its temp `sealed`; deletion is a LATER, separate act.
    await ctx.store.put(datasetStateKey('tmp_run'), Buffer.from(JSON.stringify({ state: 'sealed' }), 'utf8'));

    const freed = await deleteTemporaryDataset(ctx.store, 'tmp_run', ctx.cfg);
    expect(freed).toMatchObject({ name: 'tmp_run', rows: 3, owner: 'NsCheck-1' });
    expect(freed.bytes).toBeGreaterThan(0);

    // Gone from the catalog, and BOTH markers cleaned so the next listing resurrects no phantom.
    expect(await listDatasets(ctx.store, {}, ctx.cfg)).toEqual([]);
    expect(await ctx.store.get(datasetOwnerKey('tmp_run'))).toBeFalsy();
    expect(await ctx.store.get(datasetStateKey('tmp_run'))).toBeFalsy();
  });

  it('REFUSES a durable Dataset — only a Run’s temporary Dataset is deleted by name', async () => {
    // The guardrail the API route rests on: without an owner marker the name is durable (or a
    // promoted target), and destroying it would leave a reader on an empty table. Refuse, untouched.
    await write(ctx, { actor: 'lame', version: '0.1.0', runId: 'r2', node: 'n1', runStartedAt: D1 }, [{ d: 'x' }]);

    await expect(deleteTemporaryDataset(ctx.store, 'lame', ctx.cfg)).rejects.toBeInstanceOf(
      NotTemporaryDatasetError
    );
    expect((await listDatasets(ctx.store, { name: 'lame' }, ctx.cfg))[0]).toMatchObject({ rows: 1 });
  });

  it('leaves a Dataset promoted FROM the temp fully intact', async () => {
    // The hard constraint slice 03 had to settle. Slice 02 chose a COPY, not a re-registration, so
    // a promoted row lives in the target's OWN files — dropping the source cannot reach it. Pinned
    // end to end: promote, delete the source, and the target still holds the row AND its producing
    // provenance (never an empty table, never a re-stamp).
    await write(
      ctx,
      { actor: 'tmp_run', version: '0.1.0', runId: 'NsCheck-1', node: 'kf-01', runStartedAt: D1 },
      [
        { domain: 'a', ok: false },
        { domain: 'b', ok: true },
      ]
    );
    await own('tmp_run', 'NsCheck-1');
    await promoteInto(
      ctx.store,
      { target: 'lame', source: 'tmp_run', sql: 'SELECT * FROM "tmp_run" WHERE NOT ok' },
      ctx.cfg
    );
    expect((await datasetProvenance(ctx.store, { kind: 'output', name: 'lame' }, ctx.cfg)).rows).toBe(1);

    await deleteTemporaryDataset(ctx.store, 'tmp_run', ctx.cfg);

    expect(await listDatasets(ctx.store, { name: 'tmp_run' }, ctx.cfg)).toEqual([]);
    const after = await datasetProvenance(ctx.store, { kind: 'output', name: 'lame' }, ctx.cfg);
    expect(after.rows).toBe(1);
    expect(after.groups).toEqual([{ machine: 'kf-01', version: '0.1.0', run: 'NsCheck-1', rows: 1 }]);
  });

  it('cleans the marker of a temp opened but never written to, freeing zero rows', async () => {
    // openTempDataset writes the owner before any Batch; a Run that died before producing leaves an
    // owned name with no table. Deletion frees zero rows and removes the marker, rather than
    // erroring on a table that never existed.
    await own('tmp_empty', 'NsCheck-9');

    const freed = await deleteTemporaryDataset(ctx.store, 'tmp_empty', ctx.cfg);
    expect(freed).toMatchObject({ name: 'tmp_empty', rows: 0, bytes: 0, owner: 'NsCheck-9' });
    expect(await ctx.store.get(datasetOwnerKey('tmp_empty'))).toBeFalsy();
  });
});

/**
 * The SHARE join (ADR 0053) — the third pure join over the listing, beside `withDatasetNames` (the
 * ledger and the lake) and `withDatasetDeviations` (the Dataset record).
 *
 * Pure and total over two plain arrays, so it needs no store and no lake. Tested here rather than
 * in `sharedDatasets.test.ts` because the function lives beside its two siblings, and the thing
 * worth pinning is the same thing they pin: WHICH authority is allowed to mark WHICH row.
 */
describe('withSharedDatasets', () => {
  const info = (o: Partial<DatasetInfo> = {}): DatasetInfo => ({
    kind: 'output',
    name: 'lame',
    version: '0.1.0',
    dt: '2026-10-01T09-00-00',
    rows: 1,
    bytes: 0,
    ...o,
  });

  it('marks a granted Dataset and leaves the field ABSENT on an ungranted one', () => {
    // Deviation-only, the rule the tags follow: not shared is no row and therefore no field, never
    // `shared: false` — so a console that renders the key at all is rendering a real grant.
    const out = withSharedDatasets(
      [info(), info({ name: 'other' })],
      [{ workspace: 'bugbounty', name: 'lame', kind: 'output' }],
      'bugbounty'
    );
    expect(out[0]!.shared).toBe(true);
    expect('shared' in out[1]!).toBe(false);
  });

  it('REFUSES a grant from another workspace — the one mistake this join can make', () => {
    // Two workspaces hold a Dataset called `lame`. Joining on the name alone would badge
    // `bugbounty`'s row because `scraping` shared its own, which is an exposure claimed over data
    // nobody opened up.
    const out = withSharedDatasets(
      [info()],
      [{ workspace: 'scraping', name: 'lame', kind: 'output' }],
      'bugbounty'
    );
    expect('shared' in out[0]!).toBe(false);
  });

  it('REFUSES a grant on the other KIND of the same name', () => {
    // An output table and a loaded list live in different schemas and can share a name; the grant's
    // key carries the kind for exactly this reason.
    const out = withSharedDatasets(
      [info({ kind: 'output' }), info({ kind: 'standalone', version: undefined, dt: undefined })],
      [{ workspace: 'bugbounty', name: 'lame', kind: 'standalone' }],
      'bugbounty'
    );
    expect('shared' in out[0]!).toBe(false);
    expect(out[1]!.shared).toBe(true);
  });

  it('marks EVERY partition of a granted name, because a grant is table-grain', () => {
    // The listing's grain is one `(name, version, dt)` partition; a grant licenses
    // `SELECT … FROM <schema>.<table>`, which reads all of them. Marking only one row would draw a
    // badge finer than the access it describes.
    const out = withSharedDatasets(
      [info({ dt: '2026-10-01T09-00-00' }), info({ dt: '2026-10-02T09-00-00' })],
      [{ workspace: 'bugbounty', name: 'lame', kind: 'output' }],
      'bugbounty'
    );
    expect(out.map((i) => i.shared)).toEqual([true, true]);
  });

  it('attaches nothing for an UNNAMED workspace — the legacy address has no grants', () => {
    // `activeLakeWorkspace()` answers `''` for an install with no `.current` and no
    // KONTRA_LAKE_WORKSPACE. There is no workspace a grant could be keyed under, and an install
    // that cannot be addressed cannot share.
    const out = withSharedDatasets(
      [info()],
      [{ workspace: 'bugbounty', name: 'lame', kind: 'output' }],
      ''
    );
    expect('shared' in out[0]!).toBe(false);
  });

  it('is total: no grants returns the rows unchanged', () => {
    const rows = [info()];
    expect(withSharedDatasets(rows, [], 'bugbounty')).toEqual(rows);
  });
});
