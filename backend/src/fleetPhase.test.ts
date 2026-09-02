/**
 * The query path behind the provisioning window.
 *
 * Hermetic: `getClient` is stubbed, so nothing here needs a cluster. The three ways a child can
 * fail to answer — dropped for retention, nobody polling its queue, and an error nobody predicted —
 * are asserted separately, because the whole point of this path is that "no phase" is an answer
 * with a REASON rather than a blank.
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { WorkflowNotFoundError } from '@temporalio/client';

import { PHASE_QUERY, PHASE_TIMEOUT_MS, phaseOf, readFleetPhase } from './fleetPhase';
import { buildServer } from './server';
import { Repo } from './db/repo';

// Only `getClient` — this module reaches Temporal for exactly one thing. The other exports the
// server imports are never touched by the route under test.
vi.mock('./temporalClient', () => ({ getClient: vi.fn() }));

/** The fleet child of the recorded run, and the execution of its BRING-UP (see history.test.ts:
 *  the teardown shares the workflow id and differs only here). */
const FLEET = 'kontra-fleet/dns';
const UP_EXEC = '01a00772-8296-7c0e-ae18-7aaca6721266';

/** A client whose one workflow handle answers `getProgress` however the test says. */
function stubClient(answer: () => Promise<unknown>): { getHandle: ReturnType<typeof vi.fn> } {
  const getHandle = vi.fn(() => ({ query: vi.fn(answer) }));
  return { getHandle };
}

async function withClient(answer: () => Promise<unknown>): Promise<{ getHandle: ReturnType<typeof vi.fn> }> {
  const { getClient } = await import('./temporalClient');
  const workflow = stubClient(answer);
  vi.mocked(getClient).mockResolvedValue({ workflow } as never);
  return workflow;
}

afterEach(() => {
  vi.useRealTimers();
  vi.clearAllMocks();
});

describe('the name of the query', () => {
  // A THIRD SPELLING of a string that lives in another module and, at runtime, in another process.
  // A rename in stack.ts breaks no build — it just makes every window report no phase, which is
  // exactly how live progress went silently blank when `RunBatch` was renamed (ADR 0018).
  it('is the query stack.ts actually defines', () => {
    const source = readFileSync(join(__dirname, 'workflows', 'stack.ts'), 'utf8');
    expect(source).toContain(`defineQuery`);
    expect(source).toContain(`'${PHASE_QUERY}'`);
  });

  // The OTHER independent spelling of the same workflow. `transcript.ts:FLEET_WORKFLOW_TYPE`
  // decides a child is a fleet operation by `workflowType.name === 'stackWorkflow'` — recorded
  // verbatim on events 11, 12 and 16 of run `nscheck-1786831339`. Rename the function and no fleet
  // turn is drawn at all: the run reads exactly as it did before this feature existed, which is the
  // silent failure to catch. (The browser once held a third spelling, in `run/fleetWindow.ts`; it
  // went with the page that was its only caller, so this is now the only peer to keep in step.)
  it('names the workflow the console keys the provisioning window off', () => {
    const source = readFileSync(join(__dirname, 'workflows', 'stack.ts'), 'utf8');
    expect(source).toContain('export async function stackWorkflow(');
  });
});

describe('what the child is allowed to say', () => {
  it('takes the phase and the operation, and nothing else it carries', () => {
    // `result` and `changes` are real fields of a finished `stackWorkflow`'s progress. They are an
    // account of what a fleet operation created, and that lives behind the infra token.
    expect(
      phaseOf({ phase: 'done', op: 'up', result: 'succeeded', changes: { create: 10 } })
    ).toEqual({ phase: 'done', op: 'up' });
  });

  it('reports every phase the workflow has, without a vocabulary of its own', () => {
    // starting | running | done | compensating, verbatim — the surface must not outgrow what the
    // workflow reports, and `compensating` (a cancelled `up` tearing itself down) is the one an
    // operator most needs to see.
    for (const phase of ['starting', 'running', 'done', 'compensating']) {
      expect(phaseOf({ phase, op: 'up' }).phase).toBe(phase);
    }
  });

  it('says nothing rather than something, for an empty or malformed answer', () => {
    expect(phaseOf({})).toEqual({});
    expect(phaseOf(undefined)).toEqual({});
    // A non-string phase is not a phase. `[object Object]` under a run is the plausible-looking
    // default this plane exists to delete.
    expect(phaseOf({ phase: { deep: 1 }, op: 3 })).toEqual({});
    expect(phaseOf({ phase: '' })).toEqual({});
  });
});

describe('asking one fleet child', () => {
  it('asks the pinned execution, and reports what it said', async () => {
    const workflow = await withClient(async () => ({ phase: 'running', op: 'up' }));

    expect(await readFleetPhase(FLEET, UP_EXEC)).toEqual({
      workflowId: FLEET,
      execId: UP_EXEC,
      phase: 'running',
      op: 'up',
    });
    // THE EXECUTION TRAVELS. `kontra-fleet/dns` is the id of the bring-up AND the teardown, so a
    // handle taken by bare id answers about whichever ran last.
    expect(workflow.getHandle).toHaveBeenCalledWith(FLEET, UP_EXEC);
  });

  it('degrades to a reason when Temporal has dropped the execution', async () => {
    await withClient(async () => {
      throw new WorkflowNotFoundError('not found', FLEET, UP_EXEC);
    });

    const got = await readFleetPhase(FLEET, UP_EXEC);
    expect(got.phase).toBeUndefined();
    expect(got.op).toBeUndefined();
    expect(got.unavailable).toContain('no such execution');
  });

  it('degrades on a raw gRPC NOT_FOUND too, which is not the typed error', async () => {
    await withClient(async () => {
      throw Object.assign(new Error('workflow not found'), { code: 5 });
    });
    expect((await readFleetPhase(FLEET, UP_EXEC)).unavailable).toContain('no such execution');
  });

  it('stops waiting when nobody is polling the queue, and says so', async () => {
    vi.useFakeTimers();
    // A query needs a Worker to serve it; with none, Temporal holds the call far longer than a
    // two-second browser poll should wait.
    await withClient(() => new Promise<never>(() => {}));

    const pending = readFleetPhase(FLEET, UP_EXEC);
    await vi.advanceTimersByTimeAsync(PHASE_TIMEOUT_MS);
    const got = await pending;

    expect(got.phase).toBeUndefined();
    expect(got.unavailable).toContain('no Worker is polling');
  });

  it('never throws — an unexpected failure is still an answer to render', async () => {
    await withClient(async () => {
      throw new Error('connection refused');
    });
    expect(await readFleetPhase(FLEET, UP_EXEC)).toEqual({
      workflowId: FLEET,
      execId: UP_EXEC,
      unavailable: 'connection refused',
    });
  });

  it('carries no exec id when it was given none, rather than an empty one', async () => {
    await withClient(async () => ({ phase: 'starting', op: 'up' }));
    expect(await readFleetPhase(FLEET)).toEqual({ workflowId: FLEET, phase: 'starting', op: 'up' });
  });
});

describe('GET /api/runs/:runId/phase', () => {
  let app: FastifyInstance;

  beforeEach(() => {
    app = buildServer({ repo: new Repo(':memory:'), webRoot: '' });
  });
  afterEach(async () => {
    await app.close();
  });

  it('answers with the child\'s own phase, for the execution asked for', async () => {
    const workflow = await withClient(async () => ({ phase: 'running', op: 'up' }));

    const res = await app.inject({
      method: 'GET',
      url: `/api/runs/${encodeURIComponent(FLEET)}/phase?exec=${UP_EXEC}`,
    });

    expect(res.statusCode).toBe(200);
    expect(res.json()).toEqual({ workflowId: FLEET, execId: UP_EXEC, phase: 'running', op: 'up' });
    expect(workflow.getHandle).toHaveBeenCalledWith(FLEET, UP_EXEC);
  });

  // The run detail keeps its window either way: the duration comes from the run's own history, and
  // only the phase word is missing. A 502 here would blank a panel that still has something true
  // to say.
  it('is 200 with a reason when the child cannot be asked at all', async () => {
    const { getClient } = await import('./temporalClient');
    vi.mocked(getClient).mockRejectedValue(new Error('Temporal is unreachable'));

    const res = await app.inject({ method: 'GET', url: `/api/runs/x/phase` });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toEqual({ workflowId: 'x', unavailable: 'Temporal is unreachable' });
  });
});
