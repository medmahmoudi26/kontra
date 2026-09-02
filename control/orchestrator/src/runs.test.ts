/**
 * The run read surface after the interpreter (ADR 0023 §12).
 *
 * A **Run** is one execution of a caller's workflow, identified by that workflow's id. The
 * orchestrator still STARTS runs — `POST /api/runs` starts one execution of a REGISTERED caller's
 * workflow, by folder and type — but it stopped OWNING their execution, and that is what these
 * reads inherit: no server-minted run id beside the workflow id, no durable pre-start record to
 * recover them from, and no second identifier to translate. What was started is what every read
 * here is keyed by.
 */

import { describe, expect, it, vi } from 'vitest';

import { RunLifecycle } from './runs';
import type { MaterializationRecord } from './data/materialization';
import type { RunRow } from './temporalClient';

vi.mock('./temporalClient', () => ({
  describeRun: vi.fn(),
  listRuns: vi.fn(),
  describeRunHeartbeats: vi.fn(),
}));

function record(over: Partial<MaterializationRecord> = {}): MaterializationRecord {
  return {
    runId: 'wf-1',
    actor: 'crawler',
    version: '0.1.0',
    node: 'pages',
    schemaVersion: 2,
    state: 'complete',
    attempt: 1,
    rows: 3,
    bytes: 10,
    snapshotId: 1,
    tbl: 'ds_pages',
    error: null,
    runStartedAt: 1,
    createdAt: 1,
    updatedAt: 2,
    ...over,
  };
}

const store = (records: MaterializationRecord[]) =>
  ({ listForRun: async () => records }) as never;

describe('a Run is the caller workflow', () => {
  it('reads a run by the id the caller started it under', async () => {
    const { describeRun } = await import('./temporalClient');
    vi.mocked(describeRun).mockResolvedValueOnce({
      runId: 'nightly-sweep-2026-08-14',
      status: 'running',
      startedAt: 5,
      closedAt: 0,
      tenant: 'acme',
      type: 'Sweep',
    });

    const view = await new RunLifecycle().read('nightly-sweep-2026-08-14');

    expect(view?.runId).toBe('nightly-sweep-2026-08-14');
    expect(view?.execution).toBe('running');
  });

  it('has no second identifier to translate', () => {
    // The `orch-` prefixed workflow id existed because the SERVER minted the run id and then
    // derived a workflow id from it. Both ends of that mapping are gone; a run id that has to
    // be un-prefixed before it can be looked up is the old model surviving as a string.
    expect(Object.keys(new RunLifecycle())).not.toContain('workflowIdFor');
    expect(new RunLifecycle()).not.toHaveProperty('start');
  });

  it('reports both status dimensions, with the materialization one keyed by the run', async () => {
    const { describeRun } = await import('./temporalClient');
    vi.mocked(describeRun).mockResolvedValueOnce({
      runId: 'wf-1',
      status: 'completed',
      startedAt: 1,
      closedAt: 9,
      tenant: '',
      type: 'Sweep',
    });

    const view = await new RunLifecycle(store([record()])).read('wf-1');

    expect(view?.execution).toBe('completed');
    expect(view?.materialization.total).toBe(1);
    expect(view?.lifecycle).toBe('completed');
    expect(view?.settled).toBe(true);
  });

  it('is still a run when its workflow is gone but its output is not', async () => {
    // Retention drops a closed workflow from Temporal long before its Datasets expire. A 404
    // there would hide output that exists and is readable — the same class of mistake as
    // reporting an empty result for a failed decode.
    const { describeRun } = await import('./temporalClient');
    vi.mocked(describeRun).mockResolvedValueOnce(undefined);

    const view = await new RunLifecycle(store([record()])).read('wf-1');

    expect(view).toBeDefined();
    expect(view?.materialization.total).toBe(1);
  });

  it('says Temporal is unwell rather than reporting an unknown run', async () => {
    // A 404 and a 502 mean different things to whoever is holding the pager: one is a typo,
    // the other is an outage. Collapsing them was the defect the run routes were fixed for.
    const { describeRun } = await import('./temporalClient');
    vi.mocked(describeRun).mockRejectedValueOnce(new Error('connection refused'));

    await expect(new RunLifecycle(store([])).read('wf-1')).rejects.toThrow('connection refused');
  });

  it('still describes a run from the lake when Temporal is unreachable', async () => {
    const { describeRun } = await import('./temporalClient');
    vi.mocked(describeRun).mockRejectedValueOnce(new Error('connection refused'));

    const view = await new RunLifecycle(store([record()])).read('wf-1');

    expect(view?.execution).toBe('pending');
    expect(view?.materialization.total).toBe(1);
  });

  it('is not a run at all when neither authority has heard of it', async () => {
    const { describeRun } = await import('./temporalClient');
    vi.mocked(describeRun).mockResolvedValueOnce(undefined);

    expect(await new RunLifecycle(store([])).read('never-ran')).toBeUndefined();
  });

  it('reports the execution dimension when the materialization store is unreachable', async () => {
    const { describeRun } = await import('./temporalClient');
    vi.mocked(describeRun).mockResolvedValueOnce({
      runId: 'wf-1',
      status: 'running',
      startedAt: 1,
      closedAt: 0,
      tenant: '',
      type: 'Sweep',
    });
    const broken = {
      listForRun: async () => {
        throw new Error('postgres is down');
      },
    } as never;

    const view = await new RunLifecycle(broken).read('wf-1');

    expect(view?.execution).toBe('running');
    expect(view?.materialization.total).toBe(0);
  });
});

describe('the run LIST carries both dimensions too', () => {
  /** One Temporal row, as `listRuns` hands it over. */
  function listed(over: Partial<RunRow> = {}): RunRow {
    return {
      runId: 'wf-1',
      status: 'completed' as const,
      type: 'Sweep',
      tenant: 'acme',
      startedAt: 2,
      closedAt: 9,
      dispatches: 3,
      ...over,
    };
  }

  it('does not merge them: a completed run whose output failed says BOTH', async () => {
    // The four measured Machines all finished; one Dataset never became queryable. Temporal's
    // verdict and the ledger's are both true and they are different words, so a row prints two
    // columns — the moment they become one, three outcomes share a label again (ADR 0017).
    const { listRuns } = await import('./temporalClient');
    vi.mocked(listRuns).mockResolvedValueOnce([listed()]);

    const rows = await new RunLifecycle(store([record({ state: 'failed' })])).list();

    expect(rows[0]?.status).toBe('completed');
    expect(rows[0]?.materialization?.failed).toBe(1);
    expect(rows[0]?.lifecycle).toBe('output_failed');
  });

  it('records a run nothing wrote for as a ledger with no records, not as a gap', async () => {
    const { listRuns } = await import('./temporalClient');
    vi.mocked(listRuns).mockResolvedValueOnce([listed()]);

    const rows = await new RunLifecycle(store([])).list();

    expect(rows[0]?.materialization?.total).toBe(0);
    expect(rows[0]?.lifecycle).toBe('completed');
  });

  it('says UNKNOWN rather than "no output" when the ledger cannot be read', async () => {
    // The distinction the whole dimension is for: a store outage must not report that every run
    // on the list wrote nothing. `null` is the row admitting it does not know.
    const { listRuns } = await import('./temporalClient');
    vi.mocked(listRuns).mockResolvedValueOnce([listed()]);
    const broken = {
      listForRun: async () => {
        throw new Error('postgres is down');
      },
    } as never;

    const rows = await new RunLifecycle(broken).list();

    expect(rows[0]?.status).toBe('completed');
    expect(rows[0]?.materialization).toBeNull();
    expect(rows[0]?.lifecycle).toBeNull();
  });

  it('keys the ledger read by the run, so no row inherits another run output', async () => {
    const { listRuns } = await import('./temporalClient');
    vi.mocked(listRuns).mockResolvedValueOnce([
      listed({ runId: 'wf-2', startedAt: 20 }),
      listed({ runId: 'wf-1' }),
    ]);
    const byRun = {
      listForRun: async (runId: string) =>
        runId === 'wf-2' ? [record({ runId: 'wf-2', state: 'running' })] : [],
    } as never;

    const rows = await new RunLifecycle(byRun).list();

    expect(rows.map((r) => r.runId)).toEqual(['wf-2', 'wf-1']);
    expect(rows[0]?.lifecycle).toBe('finalizing');
    expect(rows[1]?.materialization?.total).toBe(0);
  });

  it('passes the filter through untouched', async () => {
    const { listRuns } = await import('./temporalClient');
    vi.mocked(listRuns).mockResolvedValueOnce([]);

    await new RunLifecycle(store([])).list({ tenant: 'acme', status: 'running', limit: 10 });

    expect(vi.mocked(listRuns)).toHaveBeenCalledWith({
      tenant: 'acme',
      status: 'running',
      limit: 10,
    });
  });
});
