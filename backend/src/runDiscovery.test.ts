/**
 * How `listRuns` FINDS runs — the question every other run test mocks away.
 *
 * `runs.test.ts` and friends `vi.mock('./temporalClient')`, so discovery itself had no coverage at
 * all, and that is precisely where it was wrong: a Run was discovered by listing the Actor dispatch
 * workflows it had made and grouping them by `KontraRunId`, which means a run appeared only after it
 * dispatched something. On this controller `canary-1787009695` and `sleeper-1786919716` were both
 * Running with no dispatches, and `/api/runs` returned neither — a run that had failed before its
 * first dispatch was, on every surface, indistinguishable from a run that never started.
 *
 * These tests drive the real `listRuns` against a fake Temporal client and pin the two halves that
 * are now separate: WHICH RUNS EXIST comes from the caller workflow executions, HOW MUCH EACH
 * DISPATCHED comes from the dispatches, and the second can be empty without affecting the first.
 */

import { beforeEach, describe, expect, it, vi } from 'vitest';

import { KONTRA_INTERNAL_WORKFLOW_TYPES } from './visibility';

/** Executions the fake cluster holds. `type` is what a visibility query filters on. */
interface FakeExecution {
  workflowId: string;
  type: string;
  status: string;
  startTime: Date;
  /** Present only on dispatch workflows — the caller's id, as the handler stamps it. */
  runId?: string;
  /** Temporal's own `TemporalNamespaceDivision`, set only on the server's SUBSYSTEM executions — a
   *  Schedule's `temporal-sys-scheduler-workflow` carries `TemporalScheduler`. A caller's workflow
   *  never has one. */
  division?: string;
}

const cluster: FakeExecution[] = [];
/** Every query string `listRuns` sent, so a test can assert on the query and not just its result. */
const queries: string[] = [];

/**
 * The fake visibility filter. It understands exactly the clause forms this codebase builds —
 * `WorkflowType NOT IN (...)`, `WorkflowType = '...'`, `ExecutionStatus = '...'` and
 * `TemporalNamespaceDivision IS NULL` — and it deliberately does NOT understand `ORDER BY`, because
 * SQLite visibility does not either. A query that grows a clause this cannot parse fails loudly here
 * rather than quietly matching everything.
 *
 * NOTE what this fake does NOT do, on purpose: the real server auto-applies the division filter to a
 * query that never mentions it (measured on 1.31.2 — see `visibility.ts`). Reproducing that here
 * would make the explicit clause untestable, so this fake filters only on what the query SAYS. The
 * clause therefore has to be in the query for the exclusion test below to pass, which is the property
 * worth pinning: our query states its own exclusion rather than inheriting one.
 */
function evaluate(query: string): FakeExecution[] {
  return cluster.filter((e) =>
    query.split(' AND ').every((clause) => {
      const c = clause.trim();
      let m = /^WorkflowType NOT IN \((.*)\)$/.exec(c);
      if (m) return !unquoteList(m[1] ?? '').includes(e.type);
      m = /^WorkflowType = '(.*)'$/.exec(c);
      if (m) return e.type === m[1];
      m = /^ExecutionStatus = '(.*)'$/.exec(c);
      if (m) return e.status.toLowerCase() === (m[1] ?? '').toLowerCase();
      m = /^KontraTenant = '(.*)'$/.exec(c);
      if (m) return true;
      if (c === 'TemporalNamespaceDivision IS NULL') return e.division === undefined;
      throw new Error(`fake visibility cannot parse clause: ${c}`);
    })
  );
}

function unquoteList(s: string): string[] {
  return s.split(',').map((t) => t.trim().replace(/^'|'$/g, ''));
}

vi.mock('@temporalio/client', () => {
  class Client {
    workflow = {
      // eslint-disable-next-line require-yield
      async *list({ query }: { query: string }) {
        queries.push(query);
        // Newest first, as Temporal returns by default.
        const hits = [...evaluate(query)].sort(
          (a, b) => b.startTime.getTime() - a.startTime.getTime()
        );
        for (const e of hits) {
          yield {
            workflowId: e.workflowId,
            type: e.type,
            status: { name: e.status },
            startTime: e.startTime,
            closeTime: undefined,
            typedSearchAttributes: {
              get: (key: { name: string }) => (key.name === 'KontraRunId' ? e.runId : undefined),
            },
          };
        }
      },
      getHandle() {
        throw new Error('listRuns must not describe each run to list it');
      },
    };
    connection = {};
  }
  return {
    Client,
    Connection: { connect: async () => ({ operatorService: { addSearchAttributes: async () => {} } }) },
    defaultPayloadConverter: {},
  };
});

vi.mock('./otel', () => ({ startTracing: () => {}, tracingEnabled: false }));
vi.mock('./codec/dataConverter', () => ({ dataConverter: {} }));

const at = (s: number) => new Date(s * 1000);

function caller(workflowId: string, type: string, status: string, t: number): FakeExecution {
  return { workflowId, type, status, startTime: at(t) };
}
function dispatch(runId: string, n: number, t: number): FakeExecution[] {
  return Array.from({ length: n }, (_, i) => ({
    workflowId: `actor-${runId}-${i}`,
    type: 'kontra.v1.ActorService.Run',
    status: 'COMPLETED',
    startTime: at(t),
    runId,
  }));
}

beforeEach(() => {
  cluster.length = 0;
  queries.length = 0;
});

describe('listRuns discovers runs from the caller, not from what the caller dispatched', () => {
  it('lists a run that has dispatched NOTHING — the regression this replaced', async () => {
    const { listRuns } = await import('./temporalClient');
    cluster.push(
      caller('canary-1787009695', 'Canary', 'RUNNING', 300),
      caller('sleeper-1786919716', 'Sleeper', 'RUNNING', 200)
    );

    const rows = await listRuns();

    // Both were Running on the real controller and both were absent from /api/runs.
    expect(rows.map((r) => r.runId)).toEqual(['canary-1787009695', 'sleeper-1786919716']);
    // Zero dispatches is a COLUMN VALUE, not an absence.
    expect(rows.every((r) => r.dispatches === 0)).toBe(true);
    expect(rows[0]?.status).toBe('running');
    expect(rows[0]?.type).toBe('Canary');
  });

  it('shows a run that FAILED before dispatching, instead of rendering it as never started', async () => {
    const { listRuns } = await import('./temporalClient');
    cluster.push(caller('nightly-1787000000', 'Sweep', 'FAILED', 100));

    const rows = await listRuns();

    expect(rows).toHaveLength(1);
    expect(rows[0]?.status).toBe('failed');
  });

  it('still counts the dispatches a run did make', async () => {
    const { listRuns } = await import('./temporalClient');
    cluster.push(caller('nscheck-1787150959', 'NsCheck', 'COMPLETED', 400));
    cluster.push(...dispatch('nscheck-1787150959', 5, 401));

    const rows = await listRuns();

    expect(rows).toHaveLength(1);
    expect(rows[0]?.dispatches).toBe(5);
  });

  it('never lists kontra’s own infrastructure workflows as runs', async () => {
    const { listRuns } = await import('./temporalClient');
    cluster.push(caller('mine-1', 'MyWorkflow', 'RUNNING', 500));
    for (const t of KONTRA_INTERNAL_WORKFLOW_TYPES) {
      cluster.push(caller(`internal-${t}`, t, 'RUNNING', 499));
    }

    const rows = await listRuns();

    expect(rows.map((r) => r.runId)).toEqual(['mine-1']);
  });

  /**
   * ARMING THE RETENTION SWEEP CREATES ONE OF THESE (ADR 0029 §5), and it never closes: a Schedule is
   * backed by a permanently-Running `temporal-sys-scheduler-workflow`. It is not one of kontra's own
   * workflow types, so the `NOT IN` subtraction lets it through — the division clause is what keeps
   * it off the Runs page, out of `kontra runs list`, and out of the listing's cap.
   */
  it('never lists Temporal’s own scheduler workflow — the one arming retention creates', async () => {
    const { listRuns } = await import('./temporalClient');
    cluster.push(caller('mine-1', 'MyWorkflow', 'RUNNING', 500));
    cluster.push({
      workflowId: 'temporal-sys-scheduler:kontra-dataset-retention',
      type: 'temporal-sys-scheduler-workflow',
      status: 'RUNNING',
      startTime: at(499),
      division: 'TemporalScheduler',
    });

    const rows = await listRuns();

    expect(rows.map((r) => r.runId)).toEqual(['mine-1']);
    expect(queries[0]).toContain('TemporalNamespaceDivision IS NULL');
  });

  it('ignores a dispatch whose caller is not on this page, rather than inventing a row for it', async () => {
    const { listRuns } = await import('./temporalClient');
    cluster.push(caller('kept-1', 'Kept', 'COMPLETED', 600));
    cluster.push(...dispatch('kept-1', 2, 601));
    // A dispatch left behind by a caller Temporal retention has already dropped.
    cluster.push(...dispatch('evicted-caller', 9, 100));

    const rows = await listRuns();

    expect(rows.map((r) => r.runId)).toEqual(['kept-1']);
    expect(rows[0]?.dispatches).toBe(2);
  });

  it('pushes the status filter into the query, where it now narrows the CALLER', async () => {
    const { listRuns } = await import('./temporalClient');
    cluster.push(
      caller('running-1', 'A', 'RUNNING', 700),
      caller('done-1', 'B', 'COMPLETED', 701)
    );

    const rows = await listRuns({ status: 'running' });

    expect(rows.map((r) => r.runId)).toEqual(['running-1']);
    expect(queries[0]).toContain("ExecutionStatus = 'Running'");
  });

  it('collapses a reused workflow id to its newest execution, per ADR 0023 §12', async () => {
    const { listRuns } = await import('./temporalClient');
    // The fleet reuses one id for both bring-up and teardown.
    cluster.push(
      caller('kontra-run/dns', 'Run', 'COMPLETED', 800),
      caller('kontra-run/dns', 'Run', 'FAILED', 700)
    );

    const rows = await listRuns();

    expect(rows).toHaveLength(1);
    expect(rows[0]?.status).toBe('completed'); // the newest, not the older failure
  });

  it('never asks visibility to ORDER BY, which SQLite rejects outright', async () => {
    const { listRuns } = await import('./temporalClient');
    cluster.push(caller('a-1', 'A', 'RUNNING', 900));

    await listRuns();

    expect(queries.length).toBeGreaterThan(0);
    for (const q of queries) expect(q).not.toContain('ORDER BY');
  });

  it('sorts newest first client-side, since the ordering cannot ride the query', async () => {
    const { listRuns } = await import('./temporalClient');
    cluster.push(
      caller('old', 'A', 'COMPLETED', 100),
      caller('new', 'A', 'COMPLETED', 300),
      caller('mid', 'A', 'COMPLETED', 200)
    );

    const rows = await listRuns();

    expect(rows.map((r) => r.runId)).toEqual(['new', 'mid', 'old']);
  });
});
