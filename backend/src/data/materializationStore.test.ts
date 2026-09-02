/**
 * The durable materialization-status store: transitions, idempotency and reconciliation.
 *
 * Runs against SQLite `:memory:` by default. Set `KONTRA_TEST_PG` to a Postgres URL and
 * the SAME suite runs again against Postgres — the two backends share one schema and one
 * set of assertions, so a dialect difference cannot hide behind "it passed locally".
 */

import { afterAll, beforeEach, describe, expect, it } from 'vitest';

import { MATERIALIZATION_SCHEMA_VERSION, type MaterializationKey } from './materialization';
import { MaterializationStore, defaultTable, isPostgresUrl, toPositional } from './materializationStore';

const KEY: MaterializationKey = {
  runId: 'run-1',
  actor: 'crawl4ai',
  version: '0.5.0',
  node: 'n1',
  schemaVersion: MATERIALIZATION_SCHEMA_VERSION,
};

const OTHER_NODE: MaterializationKey = { ...KEY, node: 'n2' };
const OTHER_RUN: MaterializationKey = { ...KEY, runId: 'run-2' };
const RUN_STARTED = 1_700_000_000_000;

describe('toPositional', () => {
  it('rewrites every bind for Postgres, in order', () => {
    expect(toPositional('SELECT ? WHERE a = ? AND b = ?')).toBe('SELECT $1 WHERE a = $2 AND b = $3');
  });

  it('leaves a bind-free statement alone', () => {
    expect(toPositional('SELECT 1')).toBe('SELECT 1');
  });
});

describe('backend resolution', () => {
  it('recognises both Postgres URL forms', () => {
    expect(isPostgresUrl('postgres://h/db')).toBe(true);
    expect(isPostgresUrl('postgresql://h/db')).toBe(true);
    expect(isPostgresUrl('/var/lib/orchestrator.db')).toBe(false);
  });

  it('keeps the Postgres table in its OWN schema, never beside the DuckLake catalog', () => {
    // The DuckLake catalog shares this database. An unqualified name would sit in `public`
    // among `ducklake_*`, where a catalog-maintenance script would happily drop it.
    expect(defaultTable('postgres://h/db')).toBe('kontra.materialization_node');
    expect(defaultTable('orchestrator.db')).toBe('materialization_node');
  });
});

// One suite body, run once per available backend.
const backends: Array<{ name: string; make: () => MaterializationStore }> = [
  { name: 'sqlite', make: () => new MaterializationStore({ url: ':memory:' }) },
];
if (process.env.KONTRA_TEST_PG) {
  backends.push({
    name: 'postgres',
    make: () =>
      new MaterializationStore({
        url: process.env.KONTRA_TEST_PG!,
        table: 'kontra_test.materialization_node',
      }),
  });
}

for (const backend of backends) {
  describe(`MaterializationStore (${backend.name})`, () => {
    let store: MaterializationStore;

    beforeEach(async () => {
      store = backend.make();
      await store.ensureSchema();
      // A Postgres table outlives the process, so every case must start from a known-empty
      // ledger or one test's committed record becomes the next one's phantom.
      await store.purgeRun(KEY.runId);
      await store.purgeRun(OTHER_RUN.runId);
    });

    afterAll(async () => {
      await store?.close().catch(() => undefined);
    });

    it('declares a pending record and is idempotent about it', async () => {
      await store.declare(KEY, RUN_STARTED);
      await store.declare(KEY, RUN_STARTED + 99_999);
      const rec = await store.get(KEY);
      expect(rec?.state).toBe('pending');
      expect(rec?.attempt).toBe(0);
      // run_started_at is minted once and preserved: a second declare must not move it,
      // or two attempts of the same node would stamp two different run timestamps.
      expect(rec?.runStartedAt).toBe(RUN_STARTED);
    });

    it('claims, then commits with a row count', async () => {
      const claimed = await store.claim(KEY, RUN_STARTED);
      expect(claimed?.state).toBe('running');
      expect(claimed?.attempt).toBe(1);

      await store.complete(KEY, { rows: 42, bytes: 4096, snapshotId: 7, tbl: 'ds_abc' });
      const rec = await store.get(KEY);
      expect(rec?.state).toBe('complete');
      expect(rec?.rows).toBe(42);
      expect(rec?.bytes).toBe(4096);
      expect(rec?.snapshotId).toBe(7);
      expect(rec?.tbl).toBe('ds_abc');
      expect(rec?.error).toBeNull();
    });

    it('stores a zero-row commit as a SUCCESS, not a failure', async () => {
      await store.claim(KEY, RUN_STARTED);
      await store.complete(KEY, { rows: 0, bytes: 0, snapshotId: 1, tbl: 'ds_abc' });
      const rec = await store.get(KEY);
      expect(rec?.state).toBe('complete');
      expect(rec?.rows).toBe(0);
    });

    it('short-circuits a retry that finds a committed key', async () => {
      await store.claim(KEY, RUN_STARTED);
      await store.complete(KEY, { rows: 42, bytes: 4096, snapshotId: 7, tbl: 'ds_abc' });

      // ADR 0017 §6: the retry must be told "already done" so it appends nothing.
      const second = await store.claim(KEY, RUN_STARTED);
      expect(second).toBeNull();

      const rec = await store.get(KEY);
      expect(rec?.state).toBe('complete');
      expect(rec?.rows).toBe(42); // untouched — no double-count
      expect(rec?.attempt).toBe(1); // the refused claim did not even bump the attempt
    });

    it('re-claims a running key so a cross-worker retry cannot deadlock', async () => {
      await store.claim(KEY, RUN_STARTED);
      const again = await store.claim(KEY, RUN_STARTED);
      expect(again?.state).toBe('running');
      expect(again?.attempt).toBe(2);
    });

    it('reconciles a lost status write: complete straight from pending', async () => {
      // The DuckLake commit landed; the status update did not. The next attempt must be
      // able to write `complete` by key alone, without replaying the write.
      await store.declare(KEY, RUN_STARTED);
      await store.complete(KEY, { rows: 5, bytes: 500, snapshotId: 2, tbl: 'ds_abc' });
      expect((await store.get(KEY))?.state).toBe('complete');
    });

    it('records an exhausted failure with a bounded, flattened message', async () => {
      await store.claim(KEY, RUN_STARTED);
      await store.fail(KEY, new Error('boom\n  at line 2'));
      const rec = await store.get(KEY);
      expect(rec?.state).toBe('failed');
      expect(rec?.error).toBe('boom at line 2');
    });

    it('lets a failed key be retried and then succeed', async () => {
      await store.claim(KEY, RUN_STARTED);
      await store.fail(KEY, 'transient');
      const retry = await store.claim(KEY, RUN_STARTED);
      expect(retry?.state).toBe('running');
      expect(retry?.attempt).toBe(2);
      expect(retry?.error).toBeNull(); // the stale error must not outlive the retry
      await store.complete(KEY, { rows: 1, bytes: 10, snapshotId: 3, tbl: 'ds_abc' });
      expect((await store.get(KEY))?.state).toBe('complete');
    });

    it('never un-commits: a late failure report cannot overwrite a landed write', async () => {
      await store.claim(KEY, RUN_STARTED);
      await store.complete(KEY, { rows: 9, bytes: 90, snapshotId: 4, tbl: 'ds_abc' });
      await store.fail(KEY, 'a straggler attempt reporting in');
      const rec = await store.get(KEY);
      expect(rec?.state).toBe('complete');
      expect(rec?.rows).toBe(9);
    });

    it('isolates runs: listForRun returns only that run', async () => {
      await store.claim(KEY, RUN_STARTED);
      await store.claim(OTHER_NODE, RUN_STARTED);
      await store.claim(OTHER_RUN, RUN_STARTED);
      const rows = await store.listForRun(KEY.runId);
      expect(rows.map((r) => r.node).sort()).toEqual(['n1', 'n2']);
      expect(rows.every((r) => r.runId === KEY.runId)).toBe(true);
    });

    it('keys on schema version, so two writer shapes never share a record', async () => {
      const v3: MaterializationKey = { ...KEY, schemaVersion: 3 };
      await store.claim(KEY, RUN_STARTED);
      await store.complete(KEY, { rows: 1, bytes: 1, snapshotId: 1, tbl: 'ds_v2' });
      await store.claim(v3, RUN_STARTED);
      expect((await store.get(KEY))?.state).toBe('complete');
      expect((await store.get(v3))?.state).toBe('running');
      expect((await store.listForRun(KEY.runId)).length).toBe(2);
    });

    it('surfaces records stuck non-terminal as stale', async () => {
      await store.claim(KEY, RUN_STARTED);
      await store.claim(OTHER_NODE, RUN_STARTED);
      await store.complete(OTHER_NODE, { rows: 1, bytes: 1, snapshotId: 1, tbl: 'ds_abc' });
      // Everything older than "now" — the claimed one is stuck, the committed one is not.
      const stale = await store.listStale(-1000);
      expect(stale.map((r) => r.node)).toEqual(['n1']);
    });

    it('reports health counts by state', async () => {
      await store.claim(KEY, RUN_STARTED);
      await store.claim(OTHER_NODE, RUN_STARTED);
      await store.complete(OTHER_NODE, { rows: 1, bytes: 1, snapshotId: 1, tbl: 'ds_abc' });
      const h = await store.health();
      expect(h.running).toBe(1);
      expect(h.complete).toBe(1);
    });

    it('returns null for an unknown key rather than inventing a record', async () => {
      expect(await store.get(KEY)).toBeNull();
    });

    it('purges a run and leaves every other run intact', async () => {
      await store.claim(KEY, RUN_STARTED);
      await store.claim(OTHER_RUN, RUN_STARTED);
      expect(await store.purgeRun(KEY.runId)).toBe(1);
      expect(await store.listForRun(KEY.runId)).toEqual([]);
      expect((await store.listForRun(OTHER_RUN.runId)).length).toBe(1);
    });
  });
}
