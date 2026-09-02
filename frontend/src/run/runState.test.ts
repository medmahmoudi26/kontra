/**
 * The two dimensions, as the Runs list and the run detail print them.
 *
 * Every case here is one an operator has actually been misled by: a green label over an unknown,
 * a previous run's verdict under a new run's id, and a run that is running but not yet listed.
 */

import { describe, expect, it } from 'vitest';

import {
  datasetsOf,
  executionOf,
  isUnlisted,
  materializationOf,
  newestFirst,
  runDatasetCounts,
  streakOf,
  watchedRunState,
} from './runState';
import { share } from '../datasets/scope';
import type { DatasetInfo, MaterializationSummary, RunDetail, RunRow } from './api';

function run(over: Partial<RunRow> = {}): RunRow {
  return {
    runId: 'nightly-sweep-2026-08-14',
    type: 'NsCheck',
    status: 'running',
    tenant: '',
    startedAt: 1_700_000_000_000,
    closedAt: 0,
    dispatches: 2,
    ...over,
  };
}

function summary(over: Partial<MaterializationSummary> = {}): MaterializationSummary {
  return { total: 0, pending: 0, running: 0, complete: 0, failed: 0, rows: 0, bytes: 0, ...over };
}

describe('the two dimensions are never one', () => {
  it('reports a completed run whose output failed as BOTH, in different words', () => {
    // The pairing ADR 0017 exists for. Temporal says the workflow finished; the ledger says its
    // output is not queryable. One merged verdict would have to pick one of them and be wrong
    // about the other.
    const row = run({ status: 'completed', materialization: summary({ total: 2, complete: 1, failed: 1 }) });
    expect(executionOf(row).label).toBe('completed');
    expect(materializationOf(row).label).toBe('failed');
  });

  it('does not let the materialization dimension touch the execution word', () => {
    const row = run({ status: 'failed', materialization: summary({ total: 1, complete: 1, rows: 623 }) });
    expect(executionOf(row).label).toBe('failed');
    expect(materializationOf(row).label).toBe('complete');
  });
});

describe('materializationOf tells three silences apart', () => {
  it('says UNKNOWN when the ledger could not be read', () => {
    // Not "wrote nothing". A store outage reporting an empty result about every run on the list
    // is the confident-success-over-lost-work failure, inverted.
    const got = materializationOf(run({ materialization: null }));
    expect(got.label).toBe('unknown');
    expect(got.title).toMatch(/NOT a claim/);
  });

  it('says UNKNOWN when the field is absent altogether', () => {
    expect(materializationOf(run()).label).toBe('unknown');
  });

  it('says UNRECORDED when the ledger has no record for this run', () => {
    expect(materializationOf(run({ materialization: summary() })).label).toBe('unrecorded');
  });

  it('calls a committed zero-row Dataset COMPLETE, because it is', () => {
    // Row count zero is a successful empty result (ADR 0017 §3). Reporting it as a failure would
    // invent one; the count itself is what says the run found nothing.
    const got = materializationOf(run({ materialization: summary({ total: 1, complete: 1, rows: 0 }) }));
    expect(got.label).toBe('complete');
    expect(got.title).toContain('0 rows');
  });

  it('says WRITING while any Dataset is still outstanding', () => {
    const got = materializationOf(
      run({ materialization: summary({ total: 3, complete: 1, running: 1, pending: 1 }) })
    );
    expect(got.label).toBe('writing');
    expect(got.title).toContain('2 still being written');
  });

  it('lets a failure outrank an outstanding one', () => {
    // "Some output is missing and will not arrive" is the fact to act on; "some is still coming"
    // is not. Same order the server's projection uses.
    const got = materializationOf(
      run({ materialization: summary({ total: 3, complete: 1, running: 1, failed: 1 }) })
    );
    expect(got.label).toBe('failed');
  });
});

describe('watchedRunState never shows a previous run under a new id', () => {
  const previous = run({ runId: 'old', status: 'completed', startedAt: 5 });

  it('says STARTING for a run nothing has reported on yet', () => {
    // The measured trap: for the ~2s before the first `/api/runs/:id` answer there is no detail,
    // and the newest KNOWN run was `completed` — so the header printed `completed` under an id
    // that had existed for one second.
    expect(watchedRunState('brand-new', null, [previous])).toBe('starting');
  });

  it('uses the list row when it is a row for THIS run', () => {
    expect(watchedRunState('old', null, [previous])).toBe('completed');
  });

  it('ignores a detail that belongs to another run', () => {
    // `useRunTelemetry` clears on id change, so this is belt and braces — and it is the one
    // failure mode that would put a settled verdict under a live run with no way to notice.
    const stale: Pick<RunDetail, 'runId' | 'settled' | 'lifecycle'> = {
      runId: 'old',
      settled: true,
      lifecycle: 'completed',
    };
    expect(watchedRunState('brand-new', stale, [])).toBe('starting');
  });

  it('shows the projection once the run has settled, and `running` until then', () => {
    const open: Pick<RunDetail, 'runId' | 'settled' | 'lifecycle'> = {
      runId: 'r1',
      settled: false,
      lifecycle: 'executing',
    };
    expect(watchedRunState('r1', open, [])).toBe('running');
    expect(watchedRunState('r1', { ...open, settled: true, lifecycle: 'output_failed' }, [])).toBe(
      'output_failed'
    );
  });

  it('says NEVER RUN only when there is no run to show', () => {
    expect(watchedRunState(null, null, [previous])).toBe('never run');
  });
});

describe('the list', () => {
  it('is newest first, whatever order it arrived in', () => {
    const rows = [run({ runId: 'a', startedAt: 10 }), run({ runId: 'c', startedAt: 30 }), run({ runId: 'b', startedAt: 20 })];
    expect(newestFirst(rows).map((r) => r.runId)).toEqual(['c', 'b', 'a']);
  });

  it('does not mutate what it was handed', () => {
    const rows = [run({ runId: 'a', startedAt: 10 }), run({ runId: 'b', startedAt: 20 })];
    newestFirst(rows);
    expect(rows.map((r) => r.runId)).toEqual(['a', 'b']);
  });

  it('knows when the run being watched is not in it yet', () => {
    // A run in its provisioning window has dispatched nothing, so `/api/runs` cannot see it —
    // measured at 156 of 294 seconds on a four-Machine sweep.
    expect(isUnlisted([run({ runId: 'a' })], 'b')).toBe(true);
    expect(isUnlisted([run({ runId: 'a' })], 'a')).toBe(false);
    expect(isUnlisted([], null)).toBe(false);
  });
});

describe('datasetsOf — what the ledger recorded FOR THIS RUN', () => {
  const lame: DatasetInfo = { kind: 'output', name: 'lame', rows: 1_246, bytes: 4_096 };

  it('names the Dataset by the actor that produced it, and sums this run rows', () => {
    // Two records of one Dataset — two Methods, or one retried across nodes — are one row on
    // screen. The rows are the ones THIS run committed; the lake's total is shown beside them and
    // is a different number the moment a second run writes (1,246 over two dispatches of 623).
    const [got] = datasetsOf(
      [
        { actor: 'lame', node: 'w1', state: 'complete', rows: 400 },
        { actor: 'lame', node: 'w2', state: 'complete', rows: 223 },
      ],
      [lame]
    );
    expect(got?.rows).toBe(623);
    expect(got?.info?.rows).toBe(1_246);
    expect(got?.state).toBe('complete');
  });

  it('ranks the worst record first, so a failed shard is not hidden by a complete one', () => {
    const [got] = datasetsOf(
      [
        { actor: 'lame', node: 'w1', state: 'complete', rows: 400 },
        { actor: 'lame', node: 'w2', state: 'failed', rows: 0 },
      ],
      [lame]
    );
    expect(got?.state).toBe('failed');
  });

  it('draws a Dataset the lake does not hold as absent, not as zero rows', () => {
    const [got] = datasetsOf([{ actor: 'ghost', state: 'failed', rows: 0 }], [lame]);
    expect(got?.info).toBeNull();
  });

  it('skips a record with no actor rather than inventing a name for it', () => {
    expect(datasetsOf([{ state: 'complete', rows: 5 }], [lame])).toEqual([]);
  });

  it('dates the count by the newest ledger row, not by when the page asked', () => {
    // The summed count is only as old as its last addend, and the ledger is the only thing that
    // knows when the rows landed. A fetch timestamp would date the request instead.
    const [got] = datasetsOf(
      [
        { actor: 'lame', node: 'w1', state: 'complete', rows: 400, updatedAt: 1_700_000_010_000 },
        { actor: 'lame', node: 'w2', state: 'complete', rows: 223, updatedAt: 1_700_000_020_000 },
      ],
      [lame]
    );
    expect(got?.committedAt).toBe(1_700_000_020_000);
  });

  it('records no moment when the ledger carried none, rather than guessing one', () => {
    const [got] = datasetsOf([{ actor: 'lame', state: 'complete', rows: 5 }], [lame]);
    expect(got?.committedAt).toBe(0);
  });
});

/**
 * The two counts a run's Dataset row shows.
 *
 * THE MISLABELLING THIS REPLACES WAS MEASURED. The row printed `<info.rows> rows in the Dataset,
 * all runs` where `info` was the FIRST listing row of the name — one dispatch — so one run's
 * number was captioned "all runs".
 */
describe('runDatasetCounts — the run’s share and the Dataset’s total, never divided', () => {
  const AT = 1_700_000_030_000;
  /** `lame` as the catalog lists it: two Runs of one workflow, one listing row each. */
  const catalog: DatasetInfo[] = [
    { kind: 'output', name: 'lame', version: '0.1.0', dt: '2026-08-15T01-00-00', rows: 623, bytes: 4_096 },
    { kind: 'output', name: 'lame', version: '0.1.0', dt: '2026-08-14T01-00-00', rows: 623, bytes: 4_000 },
  ];
  const mine = { name: 'lame', rows: 623, committedAt: 1_700_000_020_000 };

  it('counts this run from the ledger and the Dataset from every listing row', () => {
    const { committed, lake } = runDatasetCounts(mine, catalog, AT);
    expect(committed.rows).toBe(623);
    expect(committed.scope).toBe('run');
    expect(lake?.total.rows).toBe(1_246);
    expect(lake?.total.scope).toBe('dataset');
    expect(lake?.dispatches).toBe(2);
  });

  it('refuses to divide them — two authorities, two polls, two moments', () => {
    // "623 of 1,246" formed from these two is a ratio across two moments of a Dataset that may
    // have grown in between: the live-table snapshot bug, wearing a label. The surface shows both
    // numbers with their scopes and draws no bar between them.
    const { committed, lake } = runDatasetCounts(mine, catalog, AT);
    expect(share(committed, lake!.total)).toBeNull();
  });

  it('says each number as of the moment its own authority measured it', () => {
    const { committed, lake } = runDatasetCounts(mine, catalog, AT);
    expect(committed.measuredAt).toBe(1_700_000_020_000); // the ledger's own stamp
    expect(lake?.total.measuredAt).toBe(AT); // the listing poll's
  });

  it('reports nothing under the name as absent rather than as a zero-row Dataset', () => {
    const { lake } = runDatasetCounts({ name: 'ghost', rows: 0, committedAt: 0 }, catalog, AT);
    expect(lake).toBeNull();
  });
});

describe('streakOf', () => {
  it('draws oldest first, so the newest bar is on the right', () => {
    const now = 1_700_000_100_000;
    const bars = streakOf(
      [
        run({ runId: 'new', status: 'failed', closedAt: now }),
        run({ runId: 'old', status: 'completed', closedAt: now - 50_000 }),
      ],
      now
    );
    expect(bars.map((b) => b.status)).toEqual(['completed', 'failed']);
    expect(bars[1]?.label).toContain('new');
  });
});
