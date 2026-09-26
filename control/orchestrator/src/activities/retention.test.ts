/**
 * THE DRY-RUN DEFAULT — the safety requirement of arming the sweep at all (issue 18), pinned at the
 * one place that decides it.
 *
 * The Schedule carries no mode (`src/retention.ts`), the workflow invents none
 * (`workflows/retention.ts`), so the mode a firing runs in is resolved HERE, in the process holding
 * the lake, from `KONTRA_RETENTION_COLLECT`. Unset means a DRY RUN — a first deployment previews and
 * deletes nothing — and collection is a deliberate act of configuration on the worker that would do
 * the deleting.
 *
 * The sweep itself is mocked out: what is under test is which `dryRun` reaches it, and that the
 * activity returns the BOUNDED summary rather than the per-Dataset report.
 */

import { beforeEach, describe, expect, it, vi } from 'vitest';

import type { SweepOptions, SweepReport } from '../data/retention';

const calls: SweepOptions[] = [];

vi.mock('../data/retention', async (importActual) => {
  const actual = await importActual<typeof import('../data/retention')>();
  return {
    ...actual,
    sweepDatasets: async (_deps: unknown, opts: SweepOptions = {}): Promise<SweepReport> => {
      calls.push(opts);
      return {
        scanned: 1,
        collected: [],
        kept: [
          {
            runId: 'r1',
            name: 'wf-nscheck-0.1.0--2026-08-18T00-00-00Z--abc123',
            disposition: 'kept-tagged',
            ageMs: 1,
            lastWriteAt: 1,
            rows: 3,
          },
        ],
        dryRun: opts.dryRun ?? true,
        ttlMs: 1,
        graceMs: 1,
        purgedRuns: 0,
        purgedRows: 0,
      };
    },
  };
});

// The stores are never touched — every one is injected, so no singleton opens a database here.
const DEPS = {
  store: {} as never,
  materialization: {} as never,
  records: {} as never,
  workflows: {} as never,
  summaries: {} as never,
};

async function sweep(input: Record<string, unknown> = {}) {
  const { createRetentionActivities } = await import('./retention');
  return createRetentionActivities(DEPS).sweepDatasets(input);
}

beforeEach(() => {
  calls.length = 0;
  delete process.env.KONTRA_RETENTION_COLLECT;
});

describe('retentionCollects — the deployment switch', () => {
  it('is OFF unless the deployment says otherwise', async () => {
    const { retentionCollects } = await import('./retention');
    expect(retentionCollects({})).toBe(false);
    expect(retentionCollects({ KONTRA_RETENTION_COLLECT: '' })).toBe(false);
    // A typo is a dry run, not a collection — the failure direction that keeps data.
    expect(retentionCollects({ KONTRA_RETENTION_COLLECT: 'ture' })).toBe(false);
    expect(retentionCollects({ KONTRA_RETENTION_COLLECT: 'false' })).toBe(false);
    expect(retentionCollects({ KONTRA_RETENTION_COLLECT: '0' })).toBe(false);
  });

  it('is ON for the affirmatives an operator would actually type', async () => {
    const { retentionCollects } = await import('./retention');
    for (const v of ['1', 'true', 'TRUE', 'yes', 'on', ' true ']) {
      expect(retentionCollects({ KONTRA_RETENTION_COLLECT: v })).toBe(true);
    }
  });
});

describe('the sweep activity resolves the mode', () => {
  it('DRY RUNS a firing with no arguments on an unconfigured deployment', async () => {
    await sweep();
    expect(calls[0]?.dryRun).toBe(true);
  });

  it('collects only once the deployment opts in', async () => {
    process.env.KONTRA_RETENTION_COLLECT = '1';
    await sweep();
    expect(calls[0]?.dryRun).toBe(false);
  });

  it('lets an explicit input win in both directions', async () => {
    process.env.KONTRA_RETENTION_COLLECT = '1';
    await sweep({ dryRun: true });
    expect(calls[0]?.dryRun).toBe(true);

    delete process.env.KONTRA_RETENTION_COLLECT;
    await sweep({ dryRun: false });
    expect(calls[1]?.dryRun).toBe(false);
  });

  /** A `null` that survives the wire — the shape TRIAGE-2026-08-25 §4 measured passing through the
   *  workflow's dead default — must still land on a dry run, not on `null`. */
  it('treats a nullish dryRun as unset, and unset is a dry run', async () => {
    await sweep({ dryRun: null });
    expect(calls[0]?.dryRun).toBe(true);
  });

  it('returns the bounded summary, never the per-Dataset report', async () => {
    const out = await sweep();
    expect(out).toMatchObject({ scanned: 1, truncated: false, dryRun: true });
    expect(out.counts['kept-tagged']).toBe(1);
    expect(out.sample.kept).toHaveLength(1);
    expect(out).not.toHaveProperty('kept.0.disposition.length');
    // The report's own arrays are gone from the return value — that is the history footprint fix.
    expect((out as Record<string, unknown>).collected).toBeUndefined();
  });
});

/**
 * THE SHARED LAKE CONNECTION, AND THE ONE THING `maintainLake` MUST NOT DO TO IT.
 *
 * `lakeConnection` is memoized on `(catalog, dataPath)`, so the handle this activity is given is the
 * same one `sweepDatasets` uses and the same one every `publishBatch` on this worker uses. A draft
 * of `maintainLake` closed it in a `finally`, on the reasonable-sounding grounds that a per-tick
 * connection would leak.
 *
 * IT COST A SILENT HANG. The 22:00 firing closed the shared handle, the memo kept handing out the
 * closed one, and the next run's `publishBatch` sat at `ACTIVITY_TASK_SCHEDULED` against a queue
 * with a live poller and an empty backlog — no error, no log line, a run that simply stopped.
 *
 * So the assertion is not "it works", it is "it did not close it". Nothing else in the tree closes a
 * lake connection (`queryEngine.ts` states the rule and drops rather than closes; `retention.ts`
 * takes the same handle and leaves it), and this test is what keeps that true.
 */
describe('maintainLake leaves the shared connection open', () => {
  it('never closes the handle it was given', async () => {
    let closed = 0;
    const ops: unknown[] = [];
    const conn = {
      closeSync: () => {
        closed += 1;
      },
    };

    vi.resetModules();
    vi.doMock('../data/parquet', async (importActual) => ({
      ...(await importActual<typeof import('../data/parquet')>()),
      lakeEnabled: () => true,
      lakeConnection: async () => conn,
      resolveLakeConfig: () => ({}),
    }));
    vi.doMock('../data/maintenance', async (importActual) => ({
      ...(await importActual<typeof import('../data/maintenance')>()),
      runMaintenance: async (_c: unknown, _cat: string, o: unknown) => {
        ops.push(o);
        return [];
      },
    }));

    const { createRetentionActivities } = await import('./retention');
    await createRetentionActivities(DEPS).maintainLake({});

    expect(closed).toBe(0);
    // Non-vacuous: the maintenance really did run against that connection.
    expect(ops).toHaveLength(1);
    vi.doUnmock('../data/parquet');
    vi.doUnmock('../data/maintenance');
    vi.resetModules();
  });

  it('runs the reclamation chain and not the destructive extras by default', async () => {
    // `compact` rewrites live data for a read-speed win nobody asked for; `orphans` can delete a
    // commit that is still in flight. Neither belongs on an hourly tick.
    const { DEFAULT_MAINTENANCE_OPS } = await import('./retention');
    expect([...DEFAULT_MAINTENANCE_OPS]).toEqual(['rewrite', 'snapshots', 'cleanup']);
  });
});
