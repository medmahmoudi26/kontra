/**
 * Reading one workspace's shared Dataset from another (ADR 0053), against TWO REAL DUCKLAKES.
 *
 * NOT MOCKED, for `workspaceMigrate.test.ts`'s reason: every claim this feature makes is about what
 * DuckDB does to a statement — READ_ONLY refuses a write, a missing table is a Catalog Error, a
 * partition predicate prunes directories — and a fake connection would test the fake. So two
 * catalogs are stood up in a temp directory and rows are materialized into them through the real
 * claim-check writer.
 *
 * THE TEST THAT MATTERS MOST IS `the attach itself refuses a write`. ADR 0053's central claim is
 * that a cross-workspace read cannot become a cross-workspace write "at the DuckDB level, not
 * merely by convention". A reviewer can read `readOnly: true` in `sharedLakeConnection`; only an
 * INSERT that comes back refused proves the flag reached the ATTACH.
 */

import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterAll, beforeEach, describe, expect, it } from 'vitest';

import { MemoryStore, ObjectStore } from '../codec/objectStore';
import { NoSuchDatasetError } from './datasets';
import {
  SHARED_SRC,
  resetLakeConnections,
  sharedLakeConnection,
  writeDatasetParquet,
  type LakeConfig,
} from './parquet';
import { SharedDatasetStore } from './sharedDatasets';
import {
  NotSharedError,
  UnsupportedOrderByError,
  assertSimpleOrderBy,
  listSharedDatasets,
  pageSharedDataset,
  previewSharedDataset,
} from './sharedRead';

/**
 * TWO WORKSPACES, TWO LAKES, with the addresses passed EXPLICITLY.
 *
 * An explicit `catalog`/`dataPath` wins over the derived `ws-<name>` address (`resolveLakeConfig`),
 * which is how a suite with no S3 and no workspaces mount can still hold two separate lakes. The
 * DERIVATION itself is covered by `workspaces.test.ts`; what is covered here is everything that
 * happens once two addresses exist.
 */
interface Ctx {
  store: ObjectStore;
  dir: string;
  blobs: string;
  /** `bugbounty`'s lake — the OWNER, the one that shares. */
  owner: Partial<LakeConfig>;
  /** `scraping`'s lake — the READER's own, which must never be touched by a shared read. */
  reader: Partial<LakeConfig>;
}

const OWNER_WS = 'bugbounty';
const READER_WS = 'scraping';

function twoWorkspaces(): Ctx {
  const dir = mkdtempSync(join(tmpdir(), 'kontra-shared-read-'));
  const ownerData = join(dir, 'ws-bugbounty');
  const readerData = join(dir, 'ws-scraping');
  const blobs = join(dir, 'blobs');
  for (const d of [ownerData, readerData, blobs]) mkdirSync(d, { recursive: true });
  const store = new ObjectStore({
    backing: new MemoryStore(),
    prefix: '',
    endpoint: 'http://localhost:8333',
  });
  return {
    store,
    dir,
    blobs,
    owner: {
      catalog: join(dir, 'ws-bugbounty.ducklake'),
      dataPath: `${ownerData}/`,
      s3: false,
      blobBase: `${blobs}/`,
    },
    reader: {
      catalog: join(dir, 'ws-scraping.ducklake'),
      dataPath: `${readerData}/`,
      s3: false,
      blobBase: `${blobs}/`,
    },
  };
}

let seq = 0;

/** Materialize one dispatch into a lake through the real claim-check writer. */
async function write(
  ctx: Ctx,
  lake: Partial<LakeConfig>,
  sel: { actor: string; version: string; runId: string; node: string; runStartedAt: number },
  rows: unknown[]
): Promise<void> {
  const name = `u${(seq += 1)}.json`;
  const body = JSON.stringify(rows);
  writeFileSync(join(ctx.blobs, name), body);
  const ref = {
    $ref: { key: name, sha256: createHash('sha256').update(body).digest('hex'), size: body.length },
  };
  const manifest = JSON.stringify({ results: [ref], failures: [] });
  const src = join(mkdtempSync(join(tmpdir(), 'kontra-src-')), 'blob.json');
  writeFileSync(src, manifest);
  await writeDatasetParquet(
    ctx.store,
    { sha256: createHash('sha256').update(manifest).digest('hex'), ...sel },
    { ...lake, sourceUri: src }
  );
}

const T1 = Date.UTC(2026, 9, 1, 9, 0, 0);
const T2 = Date.UTC(2026, 9, 2, 9, 0, 0);
const DT1 = '2026-10-01T09-00-00';
const DT2 = '2026-10-02T09-00-00';

describe('assertSimpleOrderBy', () => {
  it('accepts a plain column sort, bare or quoted, with direction and nulls', () => {
    expect(assertSimpleOrderBy('run_id')).toBe('run_id');
    expect(assertSimpleOrderBy(' host desc ')).toBe('host desc');
    expect(assertSimpleOrderBy('"dt" ASC, run_id NULLS LAST')).toBe('"dt" ASC, run_id NULLS LAST');
  });

  it('REFUSES an expression, because an ORDER BY can read a table no grant named', () => {
    // The hole this grammar closes. Everything else in the read path is composed from a checked
    // name; `orderBy` is the one caller-supplied fragment, and a subquery here would reach the
    // whole of the owning workspace's attached catalog.
    expect(() =>
      assertSimpleOrderBy(`(SELECT count(*) FROM ${SHARED_SRC}.output.secrets)`)
    ).toThrow(UnsupportedOrderByError);
    expect(() => assertSimpleOrderBy('length(host)')).toThrow(UnsupportedOrderByError);
    expect(() => assertSimpleOrderBy('host; DROP TABLE x')).toThrow(UnsupportedOrderByError);
    expect(() => assertSimpleOrderBy('host -- comment')).toThrow(UnsupportedOrderByError);
    expect(() => assertSimpleOrderBy('t.host')).toThrow(UnsupportedOrderByError);
  });

  it('refuses an absent sort rather than defaulting one', () => {
    // `pageDataset`'s rule: a materialized Dataset stamps no row id, so LIMIT/OFFSET over it has no
    // defined order. Silently choosing one would make two pages overlap or skip rows with nothing
    // raising — the failure the requirement exists to prevent.
    expect(() => assertSimpleOrderBy('')).toThrow(UnsupportedOrderByError);
    expect(() => assertSimpleOrderBy('   ')).toThrow(UnsupportedOrderByError);
  });
});

describe('the cross-workspace read', () => {
  let ctx: Ctx;
  let shares: SharedDatasetStore;
  const dirs: string[] = [];

  beforeEach(async () => {
    resetLakeConnections();
    ctx = twoWorkspaces();
    dirs.push(ctx.dir);
    shares = new SharedDatasetStore({ url: join(ctx.dir, 'shares.db') });
    await shares.ensureSchema();
    // The owner's lake holds two dispatches of `lame`; the reader's holds a DIFFERENT table, so a
    // read that accidentally hit the reader's own lake would be visible as a missing table rather
    // than as plausible rows.
    await write(
      ctx,
      ctx.owner,
      { actor: 'lame', version: '0.1.0', runId: 'wf-a-1', node: 'kf-01', runStartedAt: T1 },
      [{ host: 'a.example', ok: true }, { host: 'b.example', ok: false }]
    );
    await write(
      ctx,
      ctx.owner,
      { actor: 'lame', version: '0.1.0', runId: 'wf-a-2', node: 'kf-02', runStartedAt: T2 },
      [{ host: 'c.example', ok: true }]
    );
    await write(
      ctx,
      ctx.reader,
      { actor: 'readers_own', version: '0.1.0', runId: 'wf-b-1', node: 'kf-03', runStartedAt: T1 },
      [{ host: 'z.example', ok: true }]
    );
  });

  afterAll(() => {
    resetLakeConnections();
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
  });

  it('REFUSES a Dataset that is not shared, before anything is attached', async () => {
    await expect(
      previewSharedDataset(ctx.store, shares, { workspace: OWNER_WS, name: 'lame', kind: 'output' }, ctx.owner)
    ).rejects.toThrow(NotSharedError);
  });

  it('refuses a read that does not NAME a workspace — there is no default', async () => {
    // A default would make a cross-workspace read expressible by leaving a field out.
    await expect(
      previewSharedDataset(ctx.store, shares, { workspace: '', name: 'lame', kind: 'output' }, ctx.owner)
    ).rejects.toThrow(NotSharedError);
  });

  it('reads the granted Dataset, and echoes the address that answered', async () => {
    await shares.share(OWNER_WS, 'lame', 'output');
    const got = await previewSharedDataset(
      ctx.store,
      shares,
      { workspace: OWNER_WS, name: 'lame', kind: 'output' },
      ctx.owner
    );
    expect(got.rows.length).toBe(3); // both dispatches, which is what the TABLE holds
    expect(got.columns.map((c) => c.name)).toContain('host');
    expect(got.address).toMatchObject({ workspace: OWNER_WS, name: 'lame', kind: 'output' });
    // The catalog is reported, not only that something answered — an operator debugging an empty
    // page needs to know WHICH lake was opened.
    expect(got.address.catalog).toBe(ctx.owner.catalog);
  });

  it('a grant on one KIND does not admit the other', async () => {
    await shares.share(OWNER_WS, 'lame', 'standalone');
    await expect(
      previewSharedDataset(ctx.store, shares, { workspace: OWNER_WS, name: 'lame', kind: 'output' }, ctx.owner)
    ).rejects.toThrow(NotSharedError);
  });

  it('a grant in one WORKSPACE does not admit a read naming another', async () => {
    await shares.share(OWNER_WS, 'lame', 'output');
    await expect(
      previewSharedDataset(ctx.store, shares, { workspace: READER_WS, name: 'lame', kind: 'output' }, ctx.owner)
    ).rejects.toThrow(NotSharedError);
  });

  it('a grant pointing at a Dataset the catalog does not hold fails LOUDLY', async () => {
    // Reachable and important: a grant is a record and a Dataset can be dropped or aged out from
    // under it. The exposure is open and points at nothing, and the read says exactly that rather
    // than answering zero rows — which would read as "the Dataset is empty".
    await shares.share(OWNER_WS, 'never_written', 'output');
    await expect(
      previewSharedDataset(
        ctx.store,
        shares,
        { workspace: OWNER_WS, name: 'never_written', kind: 'output' },
        ctx.owner
      )
    ).rejects.toThrow(NoSuchDatasetError);
  });

  it('clamps the limit and prunes by partition', async () => {
    await shares.share(OWNER_WS, 'lame', 'output');
    const one = await previewSharedDataset(
      ctx.store,
      shares,
      { workspace: OWNER_WS, name: 'lame', kind: 'output', limit: 1 },
      ctx.owner
    );
    expect(one.rows.length).toBe(1);
    // `dt` is a partition column, so this selects one dispatch's directory rather than filtering.
    const scoped = await previewSharedDataset(
      ctx.store,
      shares,
      { workspace: OWNER_WS, name: 'lame', kind: 'output', dt: DT2 },
      ctx.owner
    );
    expect(scoped.rows.length).toBe(1);
    const day = await previewSharedDataset(
      ctx.store,
      shares,
      { workspace: OWNER_WS, name: 'lame', kind: 'output', dt: '2026-10-01' },
      ctx.owner
    );
    expect(day.rows.length).toBe(2);
    expect(DT1).toBe('2026-10-01T09-00-00'); // the prefix the day above matches, spelled once
  });

  it('pages as a CLAIM-CHECKED REF into the READER’s store, with `done` read one row past', async () => {
    await shares.share(OWNER_WS, 'lame', 'output');
    const page = await pageSharedDataset(
      ctx.store,
      shares,
      { workspace: OWNER_WS, name: 'lame', kind: 'output', orderBy: 'host', limit: 2, offset: 0 },
      ctx.owner
    );
    expect(page.n).toBe(2);
    expect(page.done).toBe(false); // three rows exist, so a full page is NOT the end
    expect(page.ref.meta).toMatchObject({ kind: 'units', n: '2' });
    // THE BYTES ARE ON THE READER'S SIDE. This is the copy ADR 0053 hands to userland, begun: the
    // page object is in the calling process's own CAS, dereferenceable without touching the
    // owner's store again.
    const body = await ctx.store.get(ctx.store.casKey(page.ref.sha256));
    expect(body).toBeTruthy();
    const units = JSON.parse(Buffer.from(body!).toString('utf8')) as Array<Record<string, unknown>>;
    expect(units.map((u) => u.host)).toEqual(['a.example', 'b.example']);
    // Provenance rides the row, so a clone can keep it: `run_id` is the OWNER's Run, not the
    // reader's.
    expect(units[0]!.run_id).toBe('wf-a-1');

    const last = await pageSharedDataset(
      ctx.store,
      shares,
      { workspace: OWNER_WS, name: 'lame', kind: 'output', orderBy: 'host', limit: 2, offset: 2 },
      ctx.owner
    );
    expect(last.n).toBe(1);
    expect(last.done).toBe(true);
  });

  it('validates the sort BEFORE the attach, so a bad one costs no DuckDB instance', async () => {
    await shares.share(OWNER_WS, 'lame', 'output');
    await expect(
      pageSharedDataset(
        ctx.store,
        shares,
        {
          workspace: OWNER_WS,
          name: 'lame',
          kind: 'output',
          orderBy: `(SELECT 1 FROM ${SHARED_SRC}.output.lame)`,
          limit: 2,
        },
        ctx.owner
      )
    ).rejects.toThrow(UnsupportedOrderByError);
  });

  it('THE ATTACH ITSELF REFUSES A WRITE — READ_ONLY is DuckDB’s, not this module’s', async () => {
    // ADR 0053's central claim, and the only way to prove it is to try. A reviewer can read
    // `readOnly: true`; an INSERT that comes back refused is what shows the flag reached the ATTACH
    // — so a later function in this tree that composed a write would be stopped by the engine
    // rather than by a review.
    const { conn } = await sharedLakeConnection(ctx.store, OWNER_WS, ctx.owner);
    try {
      await expect(
        conn.run(`INSERT INTO ${SHARED_SRC}.output."lame" (host) VALUES ('evil.example')`)
      ).rejects.toThrow();
      await expect(conn.run(`DROP TABLE ${SHARED_SRC}.output."lame"`)).rejects.toThrow();
      await expect(
        conn.run(`DELETE FROM ${SHARED_SRC}.output."lame" WHERE host = 'a.example'`)
      ).rejects.toThrow();
      // And the rows are still there: the refusals were refusals, not silent no-ops.
      const res = await conn.runAndReadAll(`SELECT count(*) FROM ${SHARED_SRC}.output."lame"`);
      expect(Number(res.getRows()[0]![0])).toBe(3);
    } finally {
      conn.closeSync();
    }
  });

  it('attaches the OWNER’s lake and nothing else — the reader’s own lake is absent', async () => {
    // There is no cross-workspace WRITE in this design, and this is the structural reason: the
    // connection a shared read holds cannot name the reader's lake, so a one-statement copy is not
    // expressible on it whatever a future route intends.
    const { conn } = await sharedLakeConnection(ctx.store, OWNER_WS, ctx.owner);
    try {
      const dbs = await conn.runAndReadAll(
        `SELECT DISTINCT database FROM (SHOW ALL TABLES) ORDER BY database`
      );
      const names = dbs.getRows().map((r) => String(r[0]));
      expect(names).toContain(SHARED_SRC);
      expect(names).not.toContain('lake');
      // `readers_own` lives in the reader's lake; this connection cannot see it at all.
      await expect(
        conn.runAndReadAll(`SELECT count(*) FROM ${SHARED_SRC}.output."readers_own"`)
      ).rejects.toThrow();
    } finally {
      conn.closeSync();
    }
  });

  it('enumerates every exposure with the address it points at, derived not stored', async () => {
    await shares.share(OWNER_WS, 'lame', 'output');
    await shares.share(READER_WS, 'pages', 'output');
    const all = await listSharedDatasets(shares);
    expect(all.map((e) => `${e.workspace}/${e.name}`)).toEqual([
      'bugbounty/lame',
      'scraping/pages',
    ]);
    // The pointer's VALUE is the point: `workspaceAddress` is the same derivation the read path
    // uses, so the bucket and catalog printed here are the ones a reader will actually open.
    expect(all[0]!.address).toMatchObject({
      workspace: 'bugbounty',
      namespace: 'ws-bugbounty',
      bucket: 'ws-bugbounty',
    });
    expect((await listSharedDatasets(shares, { workspace: READER_WS })).length).toBe(1);
  });
});
