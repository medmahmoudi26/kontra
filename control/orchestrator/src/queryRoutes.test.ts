/**
 * The workbench's HTTP surface: run SQL, read the schema, download a result.
 *
 * These exercise the routes against a REAL DuckLake on a local data path. The engine runs
 * unhardened here for the same reason its own suite does — the hardening disables
 * LocalFileSystem and the test lake is local files — and the hardening itself is asserted
 * directly in data/queryEngine.test.ts.
 *
 * What these cover that the engine's tests cannot: the auth gate, the status codes, and the
 * export's Content-Disposition + cleanup.
 */

import { mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';

import { buildServer } from './server';
import { Repo } from './db/repo';
import { ObjectStore } from './codec/objectStore';
import {
  LAKE,
  OUTPUT_SCHEMA,
  STANDALONE_SCHEMA,
  lakeConnection,
  resetLakeConnections,
  resolveLakeConfig,
  type LakeConfig,
} from './data/parquet';
import { datasetOwnerKey } from './data/datasets';
import { resetQueryEngines } from './data/queryEngine';

const TOKEN = 'a'.repeat(64);

interface Ctx {
  app: FastifyInstance;
  store: ObjectStore;
  lake: Partial<LakeConfig>;
}

/**
 * A BackingStore over a real directory.
 *
 * In production the object store and the lake's DATA_PATH are THE SAME BUCKET: DuckDB writes the
 * export with `COPY … TO 's3://…'` and the route reads it back with `store.get`. A MemoryStore
 * paired with a local data path breaks that identity — DuckDB writes to disk, the store reads
 * from a map, and the export silently 404s. This keeps the test honest about the one property
 * the export depends on.
 */
class DiskStore {
  constructor(private readonly root: string) {}
  private p(key: string): string {
    return join(this.root, key);
  }
  async get(key: string): Promise<Uint8Array | null> {
    try {
      return readFileSync(this.p(key));
    } catch {
      return null;
    }
  }
  async put(key: string, data: Uint8Array): Promise<void> {
    mkdirSync(join(this.p(key), '..'), { recursive: true });
    writeFileSync(this.p(key), data);
  }
  async exists(key: string): Promise<boolean> {
    return (await this.get(key)) !== null;
  }
  async list(prefix: string): Promise<string[]> {
    const dir = join(this.root, prefix);
    try {
      return readdirSync(dir).map((f) => `${prefix}${f}`);
    } catch {
      return [];
    }
  }
  async remove(key: string): Promise<void> {
    rmSync(this.p(key), { force: true });
  }
}

function build(): Ctx {
  const dir = mkdtempSync(join(tmpdir(), 'kontra-qr-'));
  const dataPath = join(dir, 'data');
  mkdirSync(join(dataPath, 'exports'), { recursive: true }); // S3 needs no directories; a local path does
  const store = new ObjectStore({ backing: new DiskStore(dataPath), prefix: '', endpoint: 'http://localhost:8333' });
  const lake: Partial<LakeConfig> = {
    catalog: join(dir, 'cat.ducklake'),
    dataPath: `${dataPath}/`,
    s3: false,
  };
  return {
    app: buildServer({ repo: new Repo(':memory:'), store, lake, webRoot: '', unsafeQueryNoSandbox: true }),
    store,
    lake,
  };
}

describe('the query workbench routes', () => {
  let ctx: Ctx;

  beforeEach(async () => {
    resetLakeConnections();
    resetQueryEngines();
    // Cleared first: checkBearer falls back from KONTRA_EXPLORE_TOKEN to KONTRA_STATE_TOKEN, so
    // an ambient token in the developer's shell would satisfy the fail-closed assertions and
    // turn them green without testing anything.
    delete process.env.KONTRA_EXPLORE_TOKEN;
    delete process.env.KONTRA_STATE_TOKEN;
    process.env.KONTRA_EXPLORE_TOKEN = TOKEN;
    ctx = build();
    const c = await lakeConnection(ctx.store, resolveLakeConfig(ctx.store, ctx.lake));
    await c.run(`CREATE SCHEMA IF NOT EXISTS ${LAKE}.${STANDALONE_SCHEMA}`);
    await c.run(
      `CREATE OR REPLACE TABLE ${LAKE}.${STANDALONE_SCHEMA}."scope_paid" AS ` +
        `SELECT 'a.com' AS target, 'h1' AS platform UNION ALL SELECT 'b.com', 'h1'`
    );
  });

  afterEach(async () => {
    delete process.env.KONTRA_EXPLORE_TOKEN;
    delete process.env.KONTRA_STATE_TOKEN;
    await ctx.app.close();
  });

  const post = (sql: string, token: string | null = TOKEN, body: Record<string, unknown> = {}) =>
    ctx.app.inject({
      method: 'POST',
      url: '/api/datasets/query',
      headers: token ? { authorization: `Bearer ${token}` } : {},
      payload: { sql, ...body },
    });

  it('runs SQL against a dataset by its bare name', async () => {
    const res = await post('SELECT count(*) AS n FROM scope_paid');
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({ rows: [[2]], truncated: false });
    expect(res.json().columns[0].name).toBe('n');
    expect(res.json().elapsedMs).toBeGreaterThanOrEqual(0);
  });

  /**
   * A privileged surface: this reads every dataset, so it fails closed exactly like
   * /api/state/* and /api/runs/:id/explore.
   */
  it('is bearer-gated on all three routes', async () => {
    expect((await post('SELECT 1', null)).statusCode).toBe(401);
    expect((await post('SELECT 1', 'b'.repeat(64))).statusCode).toBe(401);
    expect(
      (await ctx.app.inject({ method: 'GET', url: '/api/datasets/schema' })).statusCode
    ).toBe(401);
    expect(
      (await ctx.app.inject({ method: 'GET', url: '/api/datasets/export?sql=SELECT%201' })).statusCode
    ).toBe(401);
  });

  /**
   * A broken query is the OPERATOR'S input. 400 with the engine's own message, so the workbench
   * prints it under the editor — a 500 would read as "the server is down" and send them to the
   * wrong place entirely.
   */
  it('reports a bad query as 400 with the database message', async () => {
    const res = await post('SELECT * FROM no_such_dataset');
    expect(res.statusCode).toBe(400);
    expect(res.json().error).toMatch(/no_such_dataset/);
    expect((await post('   ')).statusCode).toBe(400);
  });

  it('pages with limit and offset', async () => {
    const res = await post('SELECT * FROM scope_paid ORDER BY target', TOKEN, { limit: 1, offset: 1 });
    expect(res.json().rows).toEqual([['b.com', 'h1']]);
    expect(res.json().truncated).toBe(false);
  });

  it('serves the schema for autocomplete', async () => {
    const res = await ctx.app.inject({
      method: 'GET',
      url: '/api/datasets/schema',
      headers: { authorization: `Bearer ${TOKEN}` },
    });
    expect(res.statusCode).toBe(200);
    const entry = res.json().datasets.find((d: { name: string }) => d.name === 'scope_paid');
    expect(entry.kind).toBe('standalone');
    expect(entry.columns.map((c: { name: string }) => c.name)).toEqual(['target', 'platform']);
  });

  const download = (qs: string) =>
    ctx.app.inject({
      method: 'GET',
      url: `/api/datasets/export?${qs}`,
      headers: { authorization: `Bearer ${TOKEN}` },
    });

  it('downloads a result in each format, named and typed for saving', async () => {
    const sql = encodeURIComponent('SELECT * FROM scope_paid');
    for (const [format, type] of [
      ['csv', 'text/csv'],
      ['parquet', 'application/vnd.apache.parquet'],
      ['json', 'application/json'],
      ['jsonl', 'application/x-ndjson'],
    ]) {
      const res = await download(`sql=${sql}&format=${format}&filename=scope_paid`);
      expect(res.statusCode).toBe(200);
      expect(res.headers['content-type']).toContain(type);
      expect(res.headers['content-disposition']).toBe(`attachment; filename="scope_paid.${format}"`);
      expect(res.rawPayload.length).toBeGreaterThan(0);
    }
    const csv = await download(`sql=${sql}&format=csv`);
    expect(csv.body).toContain('a.com');
  });

  /**
   * The filename reaches the header from the CLIENT. Unsanitised it could inject header syntax
   * or a path — so it is reduced to a safe basename rather than trusted.
   */
  it('sanitises a hostile download filename', async () => {
    const res = await download(
      `sql=${encodeURIComponent('SELECT 1 AS a')}&format=csv&filename=${encodeURIComponent('../../etc/p"; x=y')}`
    );
    expect(res.statusCode).toBe(200);
    const cd = res.headers['content-disposition'] as string;
    expect(cd).not.toContain('..');
    expect(cd).toBe('attachment; filename="______etc_p___x_y.csv"');
  });

  it('rejects an unknown export format', async () => {
    const res = await download(`sql=${encodeURIComponent('SELECT 1')}&format=exe`);
    expect(res.statusCode).toBe(400);
    expect(res.json().error).toMatch(/format must be one of/);
  });

  /**
   * The export object is scratch. Leaving one behind would drop a stray object beside the
   * datasets, which is exactly the "no leftovers" property the storage layout exists to have.
   */
  it('deletes the temp export object after streaming it', async () => {
    const res = await download(`sql=${encodeURIComponent('SELECT * FROM scope_paid')}&format=csv`);
    // Assert the download WORKED first. Without this the test passes when the route 401s —
    // nothing was written, so nothing is left over, and the cleanup is never exercised.
    expect(res.statusCode).toBe(200);
    expect(res.body).toContain('a.com');
    expect(await ctx.store.list('exports/')).toEqual([]);
  });

  /**
   * The sandbox is ON unless a test opts out — and this proves it, rather than trusting the
   * default. A server built WITHOUT `unsafeQueryNoSandbox` cannot read this local test lake at
   * all: LocalFileSystem is disabled, so the parquet behind the dataset is unreachable and the
   * engine says so in the terms an operator needs. If someone ever flipped the default, the
   * rest of this file would keep passing and only this test would go red.
   */
  it('sandboxes the engine by default', async () => {
    const guarded = buildServer({ repo: new Repo(':memory:'), store: ctx.store, lake: ctx.lake, webRoot: '' });
    try {
      const res = await guarded.inject({
        method: 'POST',
        url: '/api/datasets/query',
        headers: { authorization: `Bearer ${TOKEN}` },
        payload: { sql: 'SELECT * FROM scope_paid ORDER BY target' },
      });
      expect(res.statusCode).toBe(400);
      expect(res.json().error).toMatch(/more working memory|LocalFileSystem/);
    } finally {
      await guarded.close();
    }
  });

  /**
   * With no token configured at all these fail CLOSED with 503, the same posture as
   * /api/state/* — "no token set" must never mean "no authentication required".
   */
  it('serves nothing when no token is configured', async () => {
    delete process.env.KONTRA_EXPLORE_TOKEN;
    delete process.env.KONTRA_STATE_TOKEN;
    expect((await post('SELECT 1', null)).statusCode).toBe(503);
    expect((await post('SELECT 1', TOKEN)).statusCode).toBe(503);
  });

  /**
   * THE ROW CEILING IS GONE ON THE STREAMING ROUTE (issue 02).
   *
   * `QUERY_MAX_ROWS` is 5,000 and it bounded the API's heap, not the lake — every scan of a real
   * Dataset on this install came back truncated. These assert the property that replaces it: a
   * result larger than the old ceiling arrives in full, in frames, with nothing called truncated.
   */
  describe('the streaming query route', () => {
    const stream = (sql: string, token: string | null = TOKEN, body: Record<string, unknown> = {}) =>
      ctx.app.inject({
        method: 'POST',
        url: '/api/datasets/query/stream',
        headers: token ? { authorization: `Bearer ${token}` } : {},
        payload: { sql, ...body },
      });

    /** One NDJSON frame per line. */
    const frames = (payload: string): Array<Record<string, any>> =>
      payload
        .split('\n')
        .filter((l) => l.trim() !== '')
        .map((l) => JSON.parse(l));

    it('returns every row of a result four times the old ceiling, with no truncation', async () => {
      const res = await stream('SELECT i FROM range(20000) AS t(i)');
      expect(res.statusCode).toBe(200);
      expect(res.headers['content-type']).toMatch(/x-ndjson/);

      const f = frames(res.payload);
      const rows = f.flatMap((x) => x.rows ?? []);
      expect(rows.length).toBe(20000);
      expect(rows[0]).toEqual([0]);
      expect(rows[19999]).toEqual([19999]);
      // The word does not appear at all — there is nothing left to truncate.
      expect(res.payload).not.toContain('truncated');
    });

    /**
     * The frames are ordered and disjoint: a reader switches on which key is present, and the
     * head arrives before any row so a table can paint its columns before it has data.
     */
    it('frames the result as columns, then rows, then done', async () => {
      const f = frames((await stream('SELECT i FROM range(5) AS t(i)')).payload);
      expect(f[0].columns).toEqual([{ name: 'i', type: 'BIGINT' }]);
      expect(f[f.length - 1].done.rows).toBe(5);
      expect(f[f.length - 1].done.elapsedMs).toBeGreaterThanOrEqual(0);
      // No frame carries two of the three shapes.
      for (const x of f) {
        expect(Object.keys(x).length).toBe(1);
      }
    });

    it('arrives in more than one frame, so a table can paint before the end', async () => {
      const f = frames((await stream('SELECT i FROM range(20000) AS t(i)')).payload);
      expect(f.filter((x) => x.rows).length).toBeGreaterThan(1);
    });

    it('resolves a bare dataset name, exactly as the buffered route does', async () => {
      const f = frames((await stream('SELECT target FROM scope_paid ORDER BY target')).payload);
      expect(f.flatMap((x) => x.rows ?? [])).toEqual([['a.com'], ['b.com']]);
    });

    it('skips with offset', async () => {
      const f = frames(
        (await stream('SELECT target FROM scope_paid ORDER BY target', TOKEN, { offset: 1 })).payload
      );
      expect(f.flatMap((x) => x.rows ?? [])).toEqual([['b.com']]);
    });

    /** Privileged like the other three — it reads every dataset. */
    it('is bearer-gated', async () => {
      expect((await stream('SELECT 1', null)).statusCode).toBe(401);
      expect((await stream('SELECT 1', 'b'.repeat(64))).statusCode).toBe(401);
    });

    /** Nothing has been committed to yet, so a bad query is still a plain 400. */
    it('answers 400 for a query that fails BEFORE the first frame', async () => {
      const res = await stream('SELECT * FROM no_such_dataset');
      expect(res.statusCode).toBe(400);
      expect(res.json().error).toMatch(/no_such_dataset/);
      expect((await stream('   ')).statusCode).toBe(400);
    });

    /**
     * WHICH FAILURES ARE PRE-HEAD, MEASURED RATHER THAN ASSUMED — because it decides whether the
     * caller sees a status code or an error frame, and the answer was not what it looked like.
     *
     * A row that cannot be evaluated does NOT surface part way through. MEASURED on
     * `@duckdb/node-api` 1.5.4: a cast failing at row 150,000 of 200,000 throws from `stream()`
     * itself, before a single chunk is fetched — DuckDB validates the pipeline up front. So a bad
     * projection is a 400 like any other bad query, and this pins that.
     *
     * The in-band `{"error"}` frame is therefore NOT for malformed SQL. It is for a read that
     * fails after rows have already gone out, which on this install is a real and recent shape:
     * the 2026-09-28 wipe left 94 catalog partitions pointing at parquet objects that no longer
     * exist, and those fail when the missing object is OPENED — chunks in. That case cannot be a
     * 400, because the 200 and ten thousand rows are already on the wire.
     */
    it('answers 400 for a row error, which DuckDB raises before the first chunk', async () => {
      const res = await stream(
        `SELECT CAST(CASE WHEN i < 15000 THEN '1' ELSE 'not-a-number' END AS INTEGER) AS q
           FROM range(20000) AS t(i)`
      );
      expect(res.statusCode).toBe(400);
      expect(res.json().error).toMatch(/Conversion/i);
    });
  });
});

/**
 * The DELETE route — the API half of temp-datasets slice 03, the surface slice 04's UI button calls.
 *
 * Two properties at the HTTP boundary the data-layer tests cannot show: a temp deletion answers with
 * what it freed, and a durable Dataset is REFUSED here (409) rather than destroyed. Ungated, like the
 * list/preview/provenance routes — no operator SQL crosses the wire.
 */
describe('DELETE /api/datasets/:name', () => {
  let ctx: Ctx;

  beforeEach(async () => {
    resetLakeConnections();
    resetQueryEngines();
    ctx = build();
    const c = await lakeConnection(ctx.store, resolveLakeConfig(ctx.store, ctx.lake));
    // A durable output Dataset (no owner marker) and a temp output Dataset (owner marker written the
    // way slice 01's openTempDataset does).
    await c.run(`CREATE SCHEMA IF NOT EXISTS ${LAKE}.${OUTPUT_SCHEMA}`);
    await c.run(`CREATE OR REPLACE TABLE ${LAKE}.${OUTPUT_SCHEMA}."kept" AS SELECT 'x' AS d`);
    await c.run(`CREATE OR REPLACE TABLE ${LAKE}.${OUTPUT_SCHEMA}."tmp_route" AS SELECT 'a' AS d UNION ALL SELECT 'b'`);
    await ctx.store.put(
      datasetOwnerKey('tmp_route'),
      Buffer.from(JSON.stringify({ owner: 'NsCheck-7', createdAt: 1_700_000_000_000 }), 'utf8')
    );
  });

  afterEach(async () => {
    await ctx.app.close();
  });

  const del = (name: string) =>
    ctx.app.inject({ method: 'DELETE', url: `/api/datasets/${encodeURIComponent(name)}` });

  it('deletes a temp and reports what it freed', async () => {
    const res = await del('tmp_route');
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({ deleted: true, name: 'tmp_route', rows: 2, owner: 'NsCheck-7' });
    expect(res.json().bytes).toBeGreaterThan(0);

    // Gone from the listing, and the owner marker cleaned so it does not resurrect.
    const list = (await ctx.app.inject({ method: 'GET', url: '/api/datasets' })).json() as Array<{
      name: string;
    }>;
    expect(list.some((d) => d.name === 'tmp_route')).toBe(false);
    expect(await ctx.store.get(datasetOwnerKey('tmp_route'))).toBeFalsy();
  });

  it('REFUSES a durable Dataset through this route — 409, not a destroyed record', async () => {
    const res = await del('kept');
    expect(res.statusCode).toBe(409);
    expect(res.json().error).toMatch(/not a temporary Dataset/);

    // The durable Dataset is untouched — still listed with its row.
    const list = (await ctx.app.inject({ method: 'GET', url: '/api/datasets' })).json() as Array<{
      name: string;
      rows: number;
    }>;
    expect(list.find((d) => d.name === 'kept')?.rows).toBe(1);
  });
});
