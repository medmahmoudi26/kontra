/**
 * Operational summaries: the bounded scalars Grafana reads instead of scanning findings.
 *
 * Runs against SQLite by default; set `KONTRA_TEST_PG` and the same suite runs against
 * Postgres too, so a dialect difference cannot hide behind "it passed locally".
 */

import { afterAll, beforeEach, describe, expect, it } from 'vitest';

import { MATERIALIZATION_SCHEMA_VERSION, type MaterializationRecord, type MaterializationState } from './materialization';
import {
  ALLOWED_METRIC_LABELS,
  SummaryStore,
  hourStart,
  metricSafeLabels,
} from './summaries';

const RUN_STARTED = 1_700_000_000_000;

function rec(
  node: string,
  state: MaterializationState,
  rows: number,
  bytes = rows * 10,
  extra: Partial<MaterializationRecord> = {}
): MaterializationRecord {
  return {
    runId: 'run-1',
    actor: 'crawl4ai',
    version: '0.5.0',
    node,
    schemaVersion: MATERIALIZATION_SCHEMA_VERSION,
    state,
    attempt: 1,
    rows,
    bytes,
    snapshotId: 1,
    tbl: 'ds_abc',
    error: null,
    runStartedAt: RUN_STARTED,
    createdAt: RUN_STARTED,
    updatedAt: RUN_STARTED,
    ...extra,
  };
}

describe('hourStart', () => {
  it('buckets an instant to the top of its hour', () => {
    expect(hourStart(RUN_STARTED + 61_000)).toBe(hourStart(RUN_STARTED));
    expect(hourStart(RUN_STARTED) % 3_600_000).toBe(0);
  });
});

describe('metricSafeLabels', () => {
  it('drops the labels that would make the series count unbounded', () => {
    // run_id, url and instance id are one new time series per value. A run's worth of
    // runs would quietly multiply the series count until the TSDB fell over mid-scan.
    const out = metricSafeLabels({
      actor: 'crawl4ai',
      version: '0.5.0',
      run_id: 'a-uuid',
      url: 'https://target/path',
      actor_instance: 'pid-1234',
    });
    expect(out).toEqual({ actor: 'crawl4ai', version: '0.5.0' });
  });

  it('keeps every allowlisted dimension', () => {
    const all = Object.fromEntries(ALLOWED_METRIC_LABELS.map((k) => [k, 'v']));
    expect(metricSafeLabels(all)).toEqual(all);
  });
});

const backends: Array<{ name: string; make: () => SummaryStore }> = [
  { name: 'sqlite', make: () => new SummaryStore({ url: ':memory:' }) },
];
if (process.env.KONTRA_TEST_PG) {
  // Its OWN schema. A test run must never write into the schema the dashboards read.
  backends.push({
    name: 'postgres',
    make: () => new SummaryStore({ url: process.env.KONTRA_TEST_PG!, schema: 'kontra_test' }),
  });
}

for (const backend of backends) {
  describe(`SummaryStore (${backend.name})`, () => {
    let store: SummaryStore;

    beforeEach(async () => {
      store = backend.make();
      await store.ensureSchema();
      await store.purgeRun('run-1');
    });

    afterAll(async () => {
      await store?.close().catch(() => undefined);
    });

    it('summarizes a run across both status dimensions', async () => {
      await store.refreshRun({
        runId: 'run-1',
        execution: 'completed',
        lifecycle: 'completed',
        records: [rec('n1', 'complete', 10), rec('n2', 'complete', 5)],
      });
      const row = await store.getRun('run-1');
      expect(row).toMatchObject({
        run_id: 'run-1',
        execution: 'completed',
        lifecycle: 'completed',
        nodes_total: 2,
        nodes_complete: 2,
        nodes_failed: 0,
      });
      expect(Number(row!.rows_total)).toBe(15);
    });

    it('counts a failed node as failed, not as zero rows', async () => {
      // "output_failed" and "found nothing" must not land on the same dashboard row.
      await store.refreshRun({
        runId: 'run-1',
        execution: 'completed',
        lifecycle: 'output_failed',
        records: [rec('n1', 'complete', 10), rec('n2', 'failed', 0, 0, { error: 'sha mismatch' })],
      });
      const row = await store.getRun('run-1');
      expect(row).toMatchObject({ lifecycle: 'output_failed', nodes_failed: 1, nodes_complete: 1 });
      expect(Number(row!.rows_total)).toBe(10);
    });

    it('is idempotent — a repeated refresh does not double-count', async () => {
      // This runs after a commit whose activity may itself be retried; a summary that
      // double-counted on retry would drift away from the ledger it summarizes.
      const input = {
        runId: 'run-1',
        execution: 'completed',
        lifecycle: 'completed',
        records: [rec('n1', 'complete', 10)],
      };
      await store.refreshRun(input);
      await store.refreshRun(input);
      expect(Number((await store.getRun('run-1'))!.rows_total)).toBe(10);
      const hourly = await store.hourly(RUN_STARTED - 3_600_000);
      expect(hourly.filter((h) => h.actor === 'crawl4ai').map((h) => h.rows)).toEqual([10]);
    });

    it('buckets output volume by the run START, not by when it was materialized', async () => {
      // A run that materializes an hour after it executed must land in the hour it ran, or
      // the rate panel shows a spike that never happened.
      await store.refreshRun({
        runId: 'run-1',
        execution: 'completed',
        lifecycle: 'completed',
        records: [rec('n1', 'complete', 7)],
      });
      const hourly = await store.hourly(RUN_STARTED - 3_600_000);
      const mine = hourly.find((h) => h.actor === 'crawl4ai' && h.hour === hourStart(RUN_STARTED));
      expect(mine?.rows).toBe(7);
    });

    it('records fleet health including records nothing will finish', async () => {
      await store.refreshHealth({ pending: 1, running: 2, complete: 30, failed: 3 }, 4);
      const h = await store.getHealth();
      expect(h).toMatchObject({ pending: 1, running: 2, complete: 30, failed: 3, stale: 4 });
      expect(h.refreshedAt).toBeGreaterThan(0);
    });

    it('overwrites the single health row rather than appending', async () => {
      await store.refreshHealth({ pending: 1, running: 0, complete: 0, failed: 0 }, 0);
      await store.refreshHealth({ pending: 9, running: 0, complete: 0, failed: 0 }, 0);
      expect((await store.getHealth()).pending).toBe(9);
    });

    it('purges a run so retention can drop its rows with its files', async () => {
      await store.refreshRun({
        runId: 'run-1',
        execution: 'completed',
        lifecycle: 'completed',
        records: [rec('n1', 'complete', 1)],
      });
      expect(await store.purgeRun('run-1')).toBe(1);
      expect(await store.getRun('run-1')).toBeNull();
    });
  });
}
