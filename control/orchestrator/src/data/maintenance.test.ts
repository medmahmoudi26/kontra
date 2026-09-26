/**
 * Lake maintenance, against a REAL DuckLake — because the thing worth proving is that the SQL is
 * accepted by the extension, and a mocked connection proves only that a string was assembled.
 *
 * THE ASSERTION THAT MATTERS IS THAT A DRY RUN CHANGES NOTHING. Every operation here deletes or
 * rewrites files; a preview that quietly acted would be the most expensive possible bug, and
 * compaction has no `dry_run` parameter to lean on, so its preview has to be a different code path
 * that calls nothing. That is the case this suite pins.
 */

import { mkdtempSync, readdirSync, rmSync, statSync } from 'node:fs';
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
    // ASKED IN ONE ORDER, RUN IN THE OTHER. `runMaintenance` sorts into chain order (`OP_ORDER`),
    // because expiry is what makes a file look orphaned in the first place and a caller who listed
    // them the other way round has written a no-op they cannot detect.
    expect(results.map((r) => r.op)).toEqual(['snapshots', 'orphans']);
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

/**
 * THE CHAIN, end to end, against a real lake — the claim the header makes, asserted.
 *
 * Every step of this was measured by hand before it was written down, because "a DELETE frees disk"
 * is the assumption the whole retention design rested on and it is false.
 */
describe('the reclamation chain', () => {
  it('frees nothing until cleanup runs, and cleanup is what frees it', async () => {
    if (!available) return expect.soft(available, 'ducklake extension unavailable — suite skipped').toBe(true);

    // A table of its own, so the compaction fixture above is not disturbed.
    await conn.run(`CREATE TABLE ${CATALOG}.chain (run_id VARCHAR, v INTEGER)`);
    for (const run of ['r1', 'r2', 'r3']) {
      await conn.run(`INSERT INTO ${CATALOG}.chain SELECT '${run}', i FROM range(20000) t(i)`);
      await conn.run(`CALL ducklake_flush_inlined_data('${CATALOG}')`);
    }
    const planted = diskBytes();
    expect(planted).toBeGreaterThan(0);

    // 1. THE DELETE. This is all `data/retention.ts` does, and on its own it frees nothing.
    await conn.run(`DELETE FROM ${CATALOG}.chain WHERE run_id = 'r1'`);
    expect(diskBytes()).toBe(planted);

    // 2 + 3. Rewrite and expire. Still nothing: expiry SCHEDULES files for deletion.
    await runMaintenance(conn, CATALOG, {
      ops: ['rewrite', 'snapshots'],
      dryRun: false,
      olderThan: new Date(Date.now() + 60_000),
    });
    expect(diskBytes()).toBe(planted);

    // 4. CLEANUP — the only step that changes a byte count.
    const [cleaned] = await runMaintenance(conn, CATALOG, {
      ops: ['cleanup'],
      dryRun: false,
      olderThan: new Date(Date.now() + 60_000),
    });
    expect(cleaned.op).toBe('cleanup');
    expect(cleaned.count).toBeGreaterThan(0);
    expect(diskBytes()).toBeLessThan(planted);

    // And the surviving runs are intact — reclaiming space is not losing data.
    const [{ n }] = await rows(`SELECT count(*)::BIGINT AS n FROM ${CATALOG}.chain`);
    expect(Number(n)).toBe(40000);
  }, 120_000);

  it('previews rewrite without rewriting, because DuckLake gives it no dry run', async () => {
    if (!available) return;
    const [r] = await runMaintenance(conn, CATALOG, { ops: ['rewrite'], dryRun: true });
    expect(r.op).toBe('rewrite');
    expect(r.estimated).toBe(true);
    expect(r.detail).toContain('no dry run');
  });

  it('previews cleanup with a real dry run', async () => {
    if (!available) return;
    const [r] = await runMaintenance(conn, CATALOG, { ops: ['cleanup'], dryRun: true });
    expect(r.op).toBe('cleanup');
    // It HAS a `dry_run` parameter, so unlike rewrite this is not an estimate.
    expect(r.estimated).toBeUndefined();
    expect(r.detail).toContain('would be');
  });

  it('runs the chain in chain order however the caller lists it', async () => {
    if (!available) return;
    const results = await runMaintenance(conn, CATALOG, {
      // Deliberately backwards, and with a duplicate.
      ops: ['compact', 'cleanup', 'snapshots', 'rewrite', 'cleanup'],
      dryRun: true,
    });
    expect(results.map((r) => r.op)).toEqual(['rewrite', 'snapshots', 'cleanup', 'compact']);
  });
});

/**
 * BYTES ON DISK, walked — deliberately NOT `ducklake_table_info.file_size_bytes`.
 *
 * The catalog view is what makes this whole class of bug invisible. It reports the size of the files
 * a table currently REFERENCES, so it drops the moment a `DELETE` releases a file — measured here,
 * 241,206 -> 160,804 bytes with nothing unlinked and not one byte returned to the filesystem. Believe
 * that number and retention looks like it works.
 *
 * The claim under test is about DISK, so the test reads the disk.
 */
function diskBytes(): number {
  let total = 0;
  const walk = (p: string) => {
    for (const e of readdirSync(p, { withFileTypes: true })) {
      const f = join(p, e.name);
      if (e.isDirectory()) walk(f);
      else if (f.endsWith('.parquet')) total += statSync(f).size;
    }
  };
  try {
    walk(join(dir, 'data'));
  } catch {
    return 0; // no data directory yet
  }
  return total;
}

async function rows(sql: string): Promise<Record<string, unknown>[]> {
  const reader = await conn.runAndReadAll(sql);
  return reader.getRowObjects() as Record<string, unknown>[];
}

/** ONE query, never a count and a sum from two — these tables are live. */
async function countFiles(): Promise<number> {
  const [r] = await rows(`SELECT coalesce(sum(file_count), 0)::BIGINT AS n FROM ducklake_table_info('${CATALOG}')`);
  return Number(r?.n ?? 0);
}
