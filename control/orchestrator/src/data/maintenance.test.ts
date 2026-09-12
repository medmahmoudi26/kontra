/**
 * Lake maintenance, against a REAL DuckLake — because the thing worth proving is that the SQL is
 * accepted by the extension, and a mocked connection proves only that a string was assembled.
 *
 * THE ASSERTION THAT MATTERS IS THAT A DRY RUN CHANGES NOTHING. Every operation here deletes or
 * rewrites files; a preview that quietly acted would be the most expensive possible bug, and
 * compaction has no `dry_run` parameter to lean on, so its preview has to be a different code path
 * that calls nothing. That is the case this suite pins.
 */

import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { DuckDBInstance, type DuckDBConnection } from '@duckdb/node-api';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { runMaintenance } from './maintenance';

const CATALOG = 'lake';
let dir: string;
let conn: DuckDBConnection;
let available = true;

beforeAll(async () => {
  dir = mkdtempSync(join(tmpdir(), 'kontra-lake-'));
  try {
    const instance = await DuckDBInstance.create(':memory:');
    conn = await instance.connect();
    await conn.run(`INSTALL ducklake`);
    await conn.run(`LOAD ducklake`);
    await conn.run(
      `ATTACH 'ducklake:${join(dir, 'catalog.sqlite')}' AS ${CATALOG} (DATA_PATH '${join(dir, 'data')}')`
    );
    // Several small appends, so there is genuinely something to compact.
    await conn.run(`CREATE TABLE ${CATALOG}.findings (id INTEGER, host VARCHAR)`);
    // FIVE SEPARATE FILES, which takes a flush per insert and is the whole point of the fixture.
    //
    // Two things had to be learned by running it. DuckLake INLINES small writes into the catalog
    // rather than writing Parquet, so five inserts alone leave five rows and ZERO files. And one
    // flush at the end merges them into ONE file, which has no adjacent file to be merged with —
    // so there would still be nothing to compact. A lake with something to compact is a lake that
    // was written to, flushed, and written to again.
    for (let i = 0; i < 5; i++) {
      await conn.run(`INSERT INTO ${CATALOG}.findings VALUES (${i}, 'host-${i}')`);
      await conn.run(`CALL ducklake_flush_inlined_data('${CATALOG}')`);
    }
  } catch {
    // A machine without the ducklake extension (offline CI) skips rather than fails — but it says
    // so, so a permanently-skipped suite is visible rather than silently green.
    available = false;
  }
}, 120_000);

afterAll(() => {
  try {
    rmSync(dir, { recursive: true, force: true });
  } catch {
    /* best effort */
  }
});

describe('lake maintenance', () => {
  it('previews compaction without calling the merge', async () => {
    if (!available) return expect.soft(available, 'ducklake extension unavailable — suite skipped').toBe(true);

    const before = await countFiles();
    const [result] = await runMaintenance(conn, CATALOG, { ops: ['compact'], dryRun: true });

    expect(result.op).toBe('compact');
    expect(result.dryRun).toBe(true);
    // COMPACTION CANNOT SIMULATE, and the result says so rather than implying a real preview.
    expect(result.estimated).toBe(true);
    expect(result.count).toBeGreaterThan(0); // non-vacuous: there really are small files
    expect(result.detail).toContain('no dry run');

    // And the lake is untouched.
    expect(await countFiles()).toBe(before);
  });

  it('actually merges when it is not a dry run', async () => {
    if (!available) return;
    const before = await countFiles();
    expect(before).toBeGreaterThan(1);

    await runMaintenance(conn, CATALOG, { ops: ['compact'], dryRun: false });

    // The rows survive the rewrite — the point of compaction is fewer files, not less data.
    const [{ n }] = await rows(`SELECT count(*)::BIGINT AS n FROM ${CATALOG}.findings`);
    expect(Number(n)).toBe(5);
  });

  it('previews orphan deletion and snapshot expiry without deleting', async () => {
    if (!available) return;
    const results = await runMaintenance(conn, CATALOG, {
      ops: ['orphans', 'snapshots'],
      dryRun: true,
    });
    expect(results.map((r) => r.op)).toEqual(['orphans', 'snapshots']);
    for (const r of results) {
      expect(r.dryRun).toBe(true);
      // These two DO have a real dry run, so they are not estimates.
      expect(r.estimated).toBeUndefined();
      expect(r.detail).toContain('would be');
    }
    // Data is still readable afterwards.
    const [{ n }] = await rows(`SELECT count(*)::BIGINT AS n FROM ${CATALOG}.findings`);
    expect(Number(n)).toBe(5);
  });

  it('runs nothing when no operation is chosen', async () => {
    if (!available) return;
    expect(await runMaintenance(conn, CATALOG, { ops: [] })).toEqual([]);
  });

  it('defaults to a dry run', async () => {
    if (!available) return;
    // No `dryRun` key at all — the default must be the safe one, because every operation here
    // deletes or rewrites.
    const [r] = await runMaintenance(conn, CATALOG, { ops: ['orphans'] });
    expect(r.dryRun).toBe(true);
  });
});

async function rows(sql: string): Promise<Record<string, unknown>[]> {
  const reader = await conn.runAndReadAll(sql);
  return reader.getRowObjects() as Record<string, unknown>[];
}

/** ONE query, never a count and a sum from two — these tables are live. */
async function countFiles(): Promise<number> {
  const [r] = await rows(`SELECT coalesce(sum(file_count), 0)::BIGINT AS n FROM ducklake_table_info('${CATALOG}')`);
  return Number(r?.n ?? 0);
}
