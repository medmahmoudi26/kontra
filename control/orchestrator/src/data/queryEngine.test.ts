/**
 * The workbench engine: operator-typed SQL over the lake.
 *
 * Two halves, deliberately. The FUNCTIONAL tests run against a real DuckLake on a local data
 * path with `harden: false`, because the hardening disables LocalFileSystem and the test lake
 * IS local files. The SANDBOX tests run with the hardening on and assert each control directly.
 * Splitting them is what lets both be real: a functional suite that silently ran unhardened
 * while claiming otherwise would be worse than no test at all.
 */

import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, mkdtempSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { beforeEach, describe, expect, it } from 'vitest';

import { MemoryStore, ObjectStore } from '../codec/objectStore';
import { listDatasets } from './datasets';
import {
  EXPORT_FORMATS,
  QUERY_MAX_ROWS,
  exportQuery,
  pageDataset,
  querySchema,
  queryEngineOpens,
  resetQueryEngines,
  runQuery,
} from './queryEngine';
import { LAKE, STANDALONE_SCHEMA, lakeConnection, resetLakeConnections, resolveLakeConfig, writeDatasetParquet, type LakeConfig } from './parquet';

interface Ctx {
  store: ObjectStore;
  cfg: Partial<LakeConfig>;
  blobs: string;
}

function lake(): Ctx {
  const dir = mkdtempSync(join(tmpdir(), 'kontra-q-'));
  const dataPath = join(dir, 'data');
  const blobs = join(dir, 'blobs');
  mkdirSync(dataPath, { recursive: true });
  mkdirSync(blobs, { recursive: true });
  // S3 has no directories, so production needs no equivalent — but a LOCAL data path does, and
  // DuckDB's COPY will not create it.
  mkdirSync(join(dataPath, 'exports'), { recursive: true });
  const store = new ObjectStore({ backing: new MemoryStore(), prefix: '', endpoint: 'http://localhost:8333' });
  return {
    store,
    blobs,
    cfg: { catalog: join(dir, 'cat.ducklake'), dataPath: `${dataPath}/`, s3: false, blobBase: `${blobs}/` },
  };
}

let seq = 0;

async function writeOutput(ctx: Ctx, results: unknown[]): Promise<void> {
  const name = `u${(seq += 1)}.json`;
  const body = JSON.stringify(results);
  writeFileSync(join(ctx.blobs, name), body);
  const ref = {
    $ref: { key: name, sha256: createHash('sha256').update(body).digest('hex'), size: body.length },
  };
  const src = join(mkdtempSync(join(tmpdir(), 'kontra-qsrc-')), 'blob.json');
  writeFileSync(src, JSON.stringify({ results: [ref], failures: [] }));
  await writeDatasetParquet(
    ctx.store,
    {
      sha256: 'unused',
      actor: 'crawl4ai',
      version: '1.0.0',
      runId: 'r1',
      node: 'n1',
      runStartedAt: Date.UTC(2026, 7, 3, 17, 50, 50),
    },
    { ...ctx.cfg, sourceUri: src }
  );
}

async function writeList(ctx: Ctx, name: string, sql: string): Promise<void> {
  const c = await lakeConnection(ctx.store, resolveLakeConfig(ctx.store, ctx.cfg));
  await c.run(`CREATE SCHEMA IF NOT EXISTS ${LAKE}.${STANDALONE_SCHEMA}`);
  await c.run(`CREATE OR REPLACE TABLE ${LAKE}.${STANDALONE_SCHEMA}."${name}" AS ${sql}`);
}

const OPEN = { harden: false } as const;

describe('runQuery', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    resetQueryEngines();
    ctx = lake();
  });

  /**
   * The point of the whole surface: an operator types the name they see in `dataset list`, not
   * `lake.output.crawl4ai`, and not a table hash. This is also what makes a query drafted here
   * paste verbatim into `kontra dataset query` and `kontra dispatch --query`.
   */
  it('resolves a dataset by its BARE NAME, for both kinds', async () => {
    await writeOutput(ctx, [{ url: 'a', status: 200 }, { url: 'b', status: 403 }]);
    await writeList(ctx, 'scope_paid', `SELECT 'example.com' AS host UNION ALL SELECT 'b.com'`);

    const out = await runQuery(ctx.store, 'SELECT count(*) AS n FROM crawl4ai', { lake: ctx.cfg, ...OPEN });
    expect(out.rows).toEqual([[2]]);

    const list = await runQuery(ctx.store, 'SELECT count(*) AS n FROM scope_paid', { lake: ctx.cfg, ...OPEN });
    expect(list.rows).toEqual([[2]]);
  });

  it('joins across datasets — the reason a workbench beats a per-dataset preview', async () => {
    await writeOutput(ctx, [{ url: 'https://a.com/x', host: 'a.com' }]);
    await writeList(ctx, 'scope_paid', `SELECT 'a.com' AS target UNION ALL SELECT 'z.com'`);

    const out = await runQuery(
      ctx.store,
      'SELECT s.target FROM scope_paid s JOIN crawl4ai c ON c.host = s.target',
      { lake: ctx.cfg, ...OPEN }
    );
    expect(out.rows).toEqual([['a.com']]);
  });

  it('reports rows, columns and elapsed time', async () => {
    await writeList(ctx, 'nums', 'SELECT 1 AS a, 2 AS b');
    const out = await runQuery(ctx.store, 'SELECT * FROM nums', { lake: ctx.cfg, ...OPEN });
    expect(out.columns.map((c) => c.name)).toEqual(['a', 'b']);
    expect(out.elapsedMs).toBeGreaterThanOrEqual(0);
    expect(out.truncated).toBe(false);
  });

  /**
   * `truncated` drives the grid's "there is more" affordance, so it must be a fact rather than
   * "the page came back full" — a result of exactly `limit` rows is NOT truncated.
   */
  it('reports truncation exactly, without an off-by-one at the boundary', async () => {
    await writeList(ctx, 'nums', 'SELECT * FROM range(10) t(i)');
    const three = await runQuery(ctx.store, 'SELECT * FROM nums ORDER BY i', { limit: 3, lake: ctx.cfg, ...OPEN });
    expect(three.rows).toHaveLength(3);
    expect(three.truncated).toBe(true);

    const exact = await runQuery(ctx.store, 'SELECT * FROM nums ORDER BY i', { limit: 10, lake: ctx.cfg, ...OPEN });
    expect(exact.rows).toHaveLength(10);
    expect(exact.truncated).toBe(false);
  });

  it('pages with offset, so the grid can fetch blocks', async () => {
    await writeList(ctx, 'nums', 'SELECT * FROM range(10) t(i)');
    const page = await runQuery(ctx.store, 'SELECT * FROM nums ORDER BY i', {
      limit: 3,
      offset: 6,
      lake: ctx.cfg,
      ...OPEN,
    });
    expect(page.rows).toEqual([[6], [7], [8]]);
  });

  /**
   * The wrap is `SELECT * FROM (<sql>) LIMIT n`, not `<sql> LIMIT n`. Appending would produce
   * `… LIMIT 5 LIMIT 3`, a syntax error on every query an operator wrote a LIMIT into — which
   * is most of them.
   */
  it('pages a query that already ends in its own LIMIT', async () => {
    await writeList(ctx, 'nums', 'SELECT * FROM range(100) t(i)');
    const out = await runQuery(ctx.store, 'SELECT * FROM nums ORDER BY i LIMIT 5', {
      limit: 3,
      lake: ctx.cfg,
      ...OPEN,
    });
    expect(out.rows).toEqual([[0], [1], [2]]);
  });

  it('tolerates a trailing semicolon', async () => {
    await writeList(ctx, 'nums', 'SELECT 1 AS a');
    expect((await runQuery(ctx.store, 'SELECT * FROM nums;  ', { lake: ctx.cfg, ...OPEN })).rows).toEqual([[1]]);
  });

  it('clamps limit to the ceiling and rejects empty SQL', async () => {
    await writeList(ctx, 'nums', 'SELECT * FROM range(3) t(i)');
    const huge = await runQuery(ctx.store, 'SELECT * FROM nums', { limit: 1e9, lake: ctx.cfg, ...OPEN });
    expect(huge.rows.length).toBeLessThanOrEqual(QUERY_MAX_ROWS);
    await expect(runQuery(ctx.store, '   ', { lake: ctx.cfg, ...OPEN })).rejects.toThrow(/no SQL/);
  });

  it('surfaces a SQL error as its message, not as an empty result', async () => {
    await writeList(ctx, 'nums', 'SELECT 1 AS a');
    await expect(runQuery(ctx.store, 'SELECT * FROM does_not_exist', { lake: ctx.cfg, ...OPEN })).rejects.toThrow(
      /does_not_exist/
    );
  });

  /**
   * A fresh install has no catalog at all. DuckDB reports that as "Cannot open database in
   * read-only mode", which reads like a broken deployment rather than "nothing has run yet" —
   * and it is the FIRST thing a new operator would see.
   */
  it('says "no datasets yet" on an empty install, not a read-only open failure', async () => {
    await expect(runQuery(ctx.store, 'SELECT 1', { lake: ctx.cfg, ...OPEN })).rejects.toThrow(/no datasets yet/);
  });

  // A dataset created after the connection was opened must be queryable without a restart —
  // views are rebuilt per query precisely so a dispatch finishing mid-session shows up.
  it('sees a dataset created after the engine was opened', async () => {
    await writeList(ctx, 'first', 'SELECT 1 AS a');
    await runQuery(ctx.store, 'SELECT * FROM first', { lake: ctx.cfg, ...OPEN });
    await writeList(ctx, 'second', 'SELECT 2 AS b');
    expect((await runQuery(ctx.store, 'SELECT * FROM second', { lake: ctx.cfg, ...OPEN })).rows).toEqual([[2]]);
  });

  it('returns JSON-safe cells for nested actor output', async () => {
    await writeOutput(ctx, [{ url: 'a', meta: { depth: 2, links: [1, 2] } }]);
    const out = await runQuery(ctx.store, 'SELECT * FROM crawl4ai', { lake: ctx.cfg, ...OPEN });
    expect(() => JSON.stringify(out)).not.toThrow();
  });
});

describe('querySchema', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    resetQueryEngines();
    ctx = lake();
  });

  it('lists every dataset with its columns, for autocomplete and the sidebar', async () => {
    await writeList(ctx, 'scope_paid', `SELECT 'a.com' AS target, 'h1' AS platform`);
    const schema = await querySchema(ctx.store, { lake: ctx.cfg, ...OPEN });
    const s = schema.find((e) => e.name === 'scope_paid')!;
    expect(s.kind).toBe('standalone');
    expect(s.columns.map((c) => c.name)).toEqual(['target', 'platform']);
    expect(s.columns[0]!.type).toBeTruthy();
  });
});

/**
 * THE SANDBOX. These run hardened, and each asserts a control that the workbench's safety
 * actually rests on. They use a memory-only lake because the point is what the connection
 * REFUSES, not what it can read.
 */
describe('the hardened connection', () => {
  let ctx: Ctx;
  beforeEach(async () => {
    resetLakeConnections();
    resetQueryEngines();
    ctx = lake();
    // A catalog has to EXIST for the engine to attach one — the ATTACH happens before the
    // sandbox closes, so this is about opening, not about reading. These tests never read data.
    await writeList(ctx, 'seed', 'SELECT 1 AS a');
  });

  const hardened = (sql: string) => runQuery(ctx.store, sql, { lake: ctx.cfg, harden: true });

  it('refuses to read a local file', async () => {
    await expect(hardened("SELECT * FROM read_text('/etc/hostname')")).rejects.toThrow(/blocked/);
  });

  /**
   * SSRF. httpfs MUST be loaded — S3 is built on it — and while it is, `read_csv('http://…')` is
   * a working outbound GET from the API container. Measured against the live service before
   * HTTPFileSystem was added to the ban list: it returned an HTTP 404, i.e. the request left the
   * box and reached something. S3FileSystem is registered separately, which is what lets the
   * lake keep working with plain HTTP banned.
   */
  it('refuses to fetch an arbitrary URL', async () => {
    // s3 ON, deliberately: that is what LOADS httpfs, and without httpfs loaded the URL would
    // fail as a missing extension — an accident of the test config that would pass whether or
    // not HTTPFileSystem is banned. Production always has httpfs, so the test must too.
    await expect(
      runQuery(ctx.store, "SELECT * FROM read_csv('http://169.254.169.254/latest/meta-data/')", {
        lake: { ...ctx.cfg, s3: true },
        harden: true,
      })
    ).rejects.toThrow(/blocked|HTTPFileSystem/);
  });

  /**
   * Every submission is wrapped as `SELECT * FROM (<sql>)`, so a non-SELECT statement is a
   * PARSE error before the sandbox is consulted. That is a real control and it is what these
   * assertions exercise — worth stating plainly, because a test named "READ_ONLY refuses DROP"
   * would look like it proved the attach flag when it only proved the wrapper.
   *
   * READ_ONLY on the ATTACH is the second layer, for any path that ever reaches the connection
   * unwrapped. It was verified directly against the live Postgres catalog: DROP, DELETE, INSERT
   * and CREATE each fail with `Cannot execute statement of type "…" on database "lake" which is
   * attached in read-only mode`.
   */
  it('rejects a non-SELECT statement before it reaches the connection', async () => {
    for (const write of [
      `DROP TABLE ${LAKE}.${STANDALONE_SCHEMA}.anything`,
      `CREATE TABLE ${LAKE}.${STANDALONE_SCHEMA}.pwn AS SELECT 1`,
      `DELETE FROM ${LAKE}.${STANDALONE_SCHEMA}.anything`,
      "INSTALL evil FROM 'http://example.invalid'",
      "SET disabled_filesystems=''",
      "SET memory_limit='8GB'",
    ]) {
      await expect(hardened(write)).rejects.toThrow();
    }
  });

  /**
   * …and the sandbox holds for the statements that ARE expressions, which is where the wrapper
   * offers nothing. A local write smuggled through a scalar subquery is the interesting case.
   */
  it('refuses a local write even in expression position', async () => {
    await expect(
      hardened("SELECT (SELECT count(*) FROM read_text('/etc/hostname')) AS leaked")
    ).rejects.toThrow(/blocked/);
  });
});

describe('exportQuery', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    resetQueryEngines();
    ctx = lake();
  });

  /**
   * Export is deliberately NOT row-capped. "Show me a page" and "give me the data" are different
   * questions, and the second is the entire reason the button exists — capping it would make the
   * export a bigger preview rather than an answer.
   */
  it('writes every row, past the preview cap, in each format', async () => {
    await writeList(ctx, 'nums', 'SELECT * FROM range(9000) t(i)');
    for (const format of EXPORT_FORMATS) {
      const { key } = await exportQuery(ctx.store, 'SELECT * FROM nums', format, `t-${format}`, {
        lake: ctx.cfg,
        ...OPEN,
      });
      expect(key).toBe(`exports/t-${format}.${format}`);
      const path = join(String(ctx.cfg.dataPath), key);
      expect(existsSync(path)).toBe(true);
      expect(statSync(path).size).toBeGreaterThan(0);
    }
    // 9000 rows is well past QUERY_MAX_ROWS — the export must not have inherited that ceiling.
    const back = await runQuery(
      ctx.store,
      `SELECT count(*) AS n FROM read_parquet('${join(String(ctx.cfg.dataPath), 'exports/t-parquet.parquet')}')`,
      { lake: ctx.cfg, ...OPEN }
    );
    expect(back.rows).toEqual([[9000]]);
  });

  // The token is what keeps two concurrent exports off each other's object.
  it('keys the object by the caller-supplied token', async () => {
    await writeList(ctx, 'nums', 'SELECT 1 AS a');
    const a = await exportQuery(ctx.store, 'SELECT * FROM nums', 'csv', 'aaa', { lake: ctx.cfg, ...OPEN });
    const b = await exportQuery(ctx.store, 'SELECT * FROM nums', 'csv', 'bbb', { lake: ctx.cfg, ...OPEN });
    expect(a.key).not.toBe(b.key);
  });

  it('refuses an unknown format rather than passing it to COPY', async () => {
    await writeList(ctx, 'nums', 'SELECT 1 AS a');
    await expect(
      exportQuery(ctx.store, 'SELECT * FROM nums', 'exe' as never, 'x', { lake: ctx.cfg, ...OPEN })
    ).rejects.toThrow(/unsupported export format/);
  });
});

describe('pageDataset', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    resetQueryEngines();
    ctx = lake();
  });

  it('returns a CAS REF, never rows — a page is a Batch', async () => {
    await writeList(ctx, 'hosts', `SELECT * FROM (VALUES ('a'),('b'),('c')) AS t(host)`);

    const page = await pageDataset(ctx.store, {
      sql: 'SELECT * FROM hosts',
      orderBy: 'host',
      limit: 10,
      offset: 0,
      lake: ctx.cfg,
      ...OPEN,
    });

    expect(page.n).toBe(3);
    expect(page.done).toBe(true);
    expect(page.ref.meta.kind).toBe('units');
    expect(page.ref.sha256).toMatch(/^[0-9a-f]{64}$/);

    // The bytes are readable from the SAME store the actor's fetch_blob reads — the invariant
    // the whole chain rests on. Units are OBJECTS, not positional arrays.
    const body = await ctx.store.get(ctx.store.casKey(page.ref.sha256));
    expect(JSON.parse(Buffer.from(body!).toString('utf8'))).toEqual([
      { host: 'a' },
      { host: 'b' },
      { host: 'c' },
    ]);
  });

  it('pages without overlapping or skipping, and reports the last page as done', async () => {
    await writeList(ctx, 'nums', `SELECT * FROM range(0, 5) AS t(i)`);

    const seen: unknown[] = [];
    for (let offset = 0, guard = 0; guard < 10; guard += 1) {
      const page = await pageDataset(ctx.store, {
        sql: 'SELECT * FROM nums',
        orderBy: 'i',
        limit: 2,
        offset,
        lake: ctx.cfg,
        ...OPEN,
      });
      const body = await ctx.store.get(ctx.store.casKey(page.ref.sha256));
      seen.push(...JSON.parse(Buffer.from(body!).toString('utf8')));
      if (page.done) break;
      offset += 2;
    }
    expect(seen).toEqual([{ i: 0 }, { i: 1 }, { i: 2 }, { i: 3 }, { i: 4 }]);
  });

  it('REFUSES to page without an explicit order_by', async () => {
    // A materialized dataset stamps no row id, so LIMIT/OFFSET over it has no defined order:
    // two pages may overlap or skip units, and nothing would raise. Defaulting to ORDER BY ALL
    // would fail as "LocalFileSystem has been disabled" on the hardened engine, which names
    // neither the cause nor the fix.
    await writeList(ctx, 'hosts', `SELECT 'a' AS host`);
    await expect(
      pageDataset(ctx.store, {
        sql: 'SELECT * FROM hosts',
        orderBy: '',
        limit: 10,
        offset: 0,
        lake: ctx.cfg,
        ...OPEN,
      })
    ).rejects.toThrow(/needs an explicit order_by/);
  });

  it('reports an exhausted dataset as a done page with zero units', async () => {
    await writeList(ctx, 'hosts', `SELECT 'a' AS host`);
    const page = await pageDataset(ctx.store, {
      sql: 'SELECT * FROM hosts',
      orderBy: 'host',
      limit: 10,
      offset: 99,
      lake: ctx.cfg,
      ...OPEN,
    });
    expect(page.n).toBe(0);
    expect(page.done).toBe(true);
  });
});

/**
 * THE CACHED READER GOES STALE AGAINST A FILE CATALOG, AND THAT IS WHY THE ENGINE IS REBUILT.
 *
 * Every other test in this file resets the engine cache in `beforeEach` and then writes before it
 * reads, so none of them could ever have seen this. It is the failure the catalog moving off
 * Postgres introduced (ADR 0031 §1b) and it is silent in the worst way: the workbench keeps
 * answering, keeps returning rows, and simply never shows a Dataset written after the first query.
 *
 * MEASURED, not theorised. With a Postgres catalog every statement re-asks the server, so a
 * connection opened an hour ago is current. With a FILE catalog DuckDB does not re-read a database
 * another handle has written: the reader's count froze at whatever it saw when it ATTACHed. The
 * engine cache therefore keys on the catalog's generation, and a commit rebuilds it.
 */
describe('freshness against a file catalog', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    resetQueryEngines();
    ctx = lake();
  });

  it('sees rows materialized AFTER the workbench connection was opened', async () => {
    await writeOutput(ctx, [{ url: 'a', status: 200 }]);
    const first = await runQuery(ctx.store, 'SELECT count(*) AS n FROM crawl4ai', { lake: ctx.cfg, ...OPEN });
    expect(first.rows).toEqual([[1]]);

    // A Run materializes into the same lake while the workbench connection is cached and open.
    await writeOutput(ctx, [{ url: 'b', status: 403 }, { url: 'c', status: 500 }]);

    const second = await runQuery(ctx.store, 'SELECT count(*) AS n FROM crawl4ai', { lake: ctx.cfg, ...OPEN });
    expect(second.rows).toEqual([[3]]);
  });

  /**
   * The same fact for the PAGING path, which is the one a long-lived caller leans on: a parent
   * workflow paging a temp Dataset a Run is still writing would otherwise page forever over the
   * rows that existed when it started — a hang wearing a completed run's clothes.
   */
  it('pages rows that appeared after the first page', async () => {
    await writeOutput(ctx, [{ url: 'a', status: 200 }]);
    const p1 = await pageDataset(ctx.store, {
      sql: 'SELECT url FROM crawl4ai',
      orderBy: 'url',
      limit: 10,
      offset: 0,
      lake: ctx.cfg,
      ...OPEN,
    });
    expect(p1.n).toBe(1);

    await writeOutput(ctx, [{ url: 'b', status: 403 }]);

    const p2 = await pageDataset(ctx.store, {
      sql: 'SELECT url FROM crawl4ai',
      orderBy: 'url',
      limit: 10,
      offset: 0,
      lake: ctx.cfg,
      ...OPEN,
    });
    expect(p2.n).toBe(2);
  });

  /**
   * THIS ONE PASSES WITH THE MECHANISM DISABLED, and it is kept for exactly that reason: it marks
   * the boundary of the bug. A table CREATED after the ATTACH resolves fine — the views are
   * rebuilt per query from a fresh listing — so a reader watching only the sidebar would have
   * concluded the connection was current. What freezes is the row snapshot, above.
   */
  it('shows a Dataset created after the connection attached', async () => {
    await writeOutput(ctx, [{ url: 'a' }]);
    expect((await querySchema(ctx.store, { lake: ctx.cfg, ...OPEN })).map((d) => d.name)).toEqual(['crawl4ai']);

    await writeList(ctx, 'scope_paid', `SELECT 'a.com' AS host`);

    const names = (await querySchema(ctx.store, { lake: ctx.cfg, ...OPEN })).map((d) => d.name).sort();
    expect(names).toEqual(['crawl4ai', 'scope_paid']);
  });

  /**
   * AND AN IDLE LAKE PAYS NOTHING. The rebuild is worth ~260 ms, so it must happen on a COMMIT and
   * not on a query: two reads with no write between them must be served by the same connection.
   * Asserted through the connection's own identity, because that is the thing being cached.
   */
  it('opens ONE connection for many reads, and one more per commit', async () => {
    await writeOutput(ctx, [{ url: 'a' }]);
    const before = queryEngineOpens();

    const q = () => runQuery(ctx.store, 'SELECT count(*) FROM crawl4ai', { lake: ctx.cfg, ...OPEN });
    await q();
    await q();
    await q();
    expect(queryEngineOpens() - before).toBe(1); // three reads, one connection

    await writeOutput(ctx, [{ url: 'b' }]);
    await q();
    await q();
    expect(queryEngineOpens() - before).toBe(2); // one commit, one rebuild — not one per query
  });
});

/**
 * `kontra explore --catalog` AND THE DATASETS CONSOLE OVER THE SAME FILE CATALOG (issue 10).
 *
 * Two surfaces read the same `ducklake_*` metadata through two different code bases — the Go CLI
 * builds the `kontra_datasets` view (cli/explore.go) and the server builds `listDatasets`
 * (data/datasets.ts) — and both took the metadata schema as the constant `public`. That was
 * correct for exactly as long as the catalog was Postgres. MEASURED on both backends: against the
 * live Postgres catalog `public` answers 29 tables and `main` answers `Catalog Error: … schema
 * "main" does not exist`; against a file catalog it is the other way round. There is no value that
 * works for both, so the schema is derived and this test runs the CLI's own SQL, verbatim, against
 * the backend that is now the default.
 */
describe('CLI/console parity over a file catalog', () => {
  let ctx: Ctx;
  beforeEach(() => {
    resetLakeConnections();
    resetQueryEngines();
    ctx = lake();
  });

  it('gives `kontra explore` the same dispatches the Datasets page lists', async () => {
    await writeOutput(ctx, [{ url: 'a', status: 200 }, { url: 'b', status: 403 }]);
    await writeList(ctx, 'scope_paid', `SELECT 'a.com' AS host`);

    const cfg = resolveLakeConfig(ctx.store, ctx.cfg);
    expect(cfg.metaSchema).toBe('main');
    const conn = await lakeConnection(ctx.store, cfg);
    const meta = (t: string) => `__ducklake_metadata_lake.${cfg.metaSchema}.${t}`;

    // cli/explore.go:catalogInitSQL's `kontra_datasets`, with the schema derived rather than
    // hardcoded. Kept byte-for-byte in shape so a drift on either side shows up here.
    const cli = (
      await conn.runAndReadAll(`
        WITH pf AS (
          SELECT df.data_file_id, t.table_name AS actor, df.record_count AS rows,
                 max(CASE WHEN pv.partition_key_index=0 THEN pv.partition_value END) AS version,
                 max(CASE WHEN pv.partition_key_index=1 THEN pv.partition_value END) AS dt
          FROM ${meta('ducklake_data_file')} df
          JOIN ${meta('ducklake_table')} t ON t.table_id=df.table_id AND t.end_snapshot IS NULL
          JOIN ${meta('ducklake_schema')} sc ON sc.schema_id=t.schema_id AND sc.schema_name='output'
          JOIN ${meta('ducklake_file_partition_value')} pv ON pv.data_file_id=df.data_file_id
          WHERE df.end_snapshot IS NULL
          GROUP BY df.data_file_id, t.table_name, df.record_count
        )
        SELECT actor, version, dt, sum(rows) AS rows FROM pf GROUP BY actor, version, dt`)
    )
      .getRows()
      .map((r) => ({ name: String(r[0]), version: String(r[1]), dt: String(r[2]), rows: Number(r[3]) }));

    const page = (await listDatasets(ctx.store, { kind: 'output' }, ctx.cfg)).map((d) => ({
      name: d.name,
      version: String(d.version),
      dt: String(d.dt),
      rows: d.rows,
    }));

    expect(cli.length).toBeGreaterThan(0);
    expect(cli).toEqual(page);
  });
});
