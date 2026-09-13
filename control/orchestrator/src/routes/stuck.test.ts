/**
 * issue F7 — what will never move, and what must not be called that.
 *
 * The sharp assertions are the two that keep this from being alarming noise: a cluster that could
 * not be ASKED is not a wedged cluster, and a poller Temporal still lists from five minutes ago is
 * not a live one. Getting either backwards produces a health check that tells an operator to
 * terminate things that are fine — which is worse than the silence it replaces.
 */

import { describe, expect, it } from 'vitest';

import { stuckExecutions } from './stuck';
import type { QueueState } from '../panels/pollers';

const NOW = 1_789_000_000_000;

function execution(over: Partial<{ workflowId: string; type: string; queue: string; startedAt: number }> = {}) {
  return {
    workflowId: 'kontra-warden/wdn-live1',
    execId: 'exec-1',
    type: 'wardenWorkflow',
    queue: 'kontra-warden',
    startedAt: NOW - 14 * 86_400_000,
    ...over,
  };
}

function queue(over: Partial<QueueState> = {}): QueueState {
  return { queue: 'kontra-warden', identities: [], workers: [], lastPoll: 0, ...over };
}

async function report(
  executions: ReturnType<typeof execution>[],
  queues: Record<string, QueueState | Error>
) {
  return stuckExecutions({
    list: async () => ({ executions, capped: false }),
    describeQueue: async (q) => {
      const got = queues[q];
      if (got instanceof Error) throw got;
      if (got === undefined) throw new Error(`no stub for ${q}`);
      return got;
    },
    now: () => NOW,
  });
}

describe('an execution nothing is polling', () => {
  it('is wedged, and says how old it is', async () => {
    const got = await report([execution()], { 'kontra-warden': queue() });
    expect(got.wedged).toBe(1);
    expect(got.executions[0]!.wedged).toBe(true);
    expect(got.executions[0]!.pollers).toBe(0);
    expect(got.executions[0]!.ageMs).toBe(14 * 86_400_000);
  });

  it('is NOT wedged when a live worker is polling', async () => {
    // Non-vacuous partner: a check that reported everything as wedged would pass the case above.
    const got = await report([execution()], {
      'kontra-warden': queue({ identities: ['w1'], lastPoll: NOW }),
    });
    expect(got.wedged).toBe(0);
    expect(got.executions[0]!.wedged).toBe(false);
  });

  it('IS wedged when the only poller last polled long ago', async () => {
    // Temporal keeps a poller listed for about five minutes after it was last seen, so a count
    // alone reports a worker killed thirty seconds ago as serving. The freshness window is the
    // difference between "a worker is there" and "a worker was there".
    const got = await report([execution()], {
      'kontra-warden': queue({ identities: ['w1'], lastPoll: NOW - 3_600_000 }),
    });
    expect(got.executions[0]!.wedged).toBe(true);
  });
});

describe('a cluster that could not be asked is not a wedged cluster', () => {
  it('reports unknown rather than wedged when the describe FAILS', async () => {
    // The failure direction that matters. A health check that called an unreachable cluster a wall
    // of wedged workflows would send somebody to terminate things that are fine.
    const got = await report([execution()], { 'kontra-warden': new Error('temporal unreachable') });
    expect(got.wedged).toBe(0);
    expect(got.executions[0]!.wedged).toBe(false);
    expect(got.executions[0]!.pollers).toBeNull();
  });

  it('reports unknown when the describe ERRORS in its own answer', async () => {
    const got = await report([execution()], {
      'kontra-warden': queue({ error: 'DescribeTaskQueue: unavailable' }),
    });
    expect(got.executions[0]!.wedged).toBe(false);
    expect(got.executions[0]!.pollers).toBeNull();
  });
});

describe('the report is ordered and counted for a reader', () => {
  it('puts the oldest first, because that is the one to look at', async () => {
    const got = await report(
      [
        execution({ workflowId: 'young', startedAt: NOW - 60_000 }),
        execution({ workflowId: 'ancient', startedAt: NOW - 38 * 86_400_000 }),
        execution({ workflowId: 'middling', startedAt: NOW - 86_400_000 }),
      ],
      { 'kontra-warden': queue() }
    );
    expect(got.executions.map((e) => e.workflowId)).toEqual(['ancient', 'middling', 'young']);
  });

  it('asks each distinct queue once, not once per execution', async () => {
    // Five Wardens on one queue is ONE question. Asking it five times is five DescribeTaskQueue
    // calls for one answer, on a path a health check takes.
    const asked: string[] = [];
    const got = await stuckExecutions({
      list: async () => ({
        executions: [execution(), execution({ workflowId: 'b' }), execution({ workflowId: 'c' })],
        capped: false,
      }),
      describeQueue: async (q) => {
        asked.push(q);
        return queue();
      },
      now: () => NOW,
    });
    expect(asked).toEqual(['kontra-warden']);
    expect(got.wedged).toBe(3);
  });

  it('carries a capped listing through, because a full page is not a complete one', async () => {
    const got = await stuckExecutions({
      list: async () => ({ executions: [execution()], capped: true }),
      describeQueue: async () => queue(),
      now: () => NOW,
    });
    expect(got.capped).toBe(true);
  });
});
