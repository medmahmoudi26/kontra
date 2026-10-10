/**
 * The fleet pool against a real Temporal test server, with its activities faked: what it converges,
 * applies, deletes and destroys, and when. These are the counting rules ADR 0066 decision 7 rests on.
 */
import * as path from 'node:path';

import { TestWorkflowEnvironment } from '@temporalio/testing';
import { bundleWorkflowCode, Worker, type WorkflowBundle } from '@temporalio/worker';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import {
  POOL_DROP_SIGNAL,
  POOL_HOLD_UPDATE,
  POOL_PLACE_UPDATE,
  POOL_QUERY,
  POOL_TOUCH_SIGNAL,
  POOL_WORKFLOW,
  type PoolState,
} from '../fleetPool';

let env: TestWorkflowEnvironment;
let bundle: WorkflowBundle;

beforeAll(async () => {
  env = await TestWorkflowEnvironment.createTimeSkipping();
  bundle = await bundleWorkflowCode({ workflowsPath: path.join(__dirname, 'infra.ts') });
}, 300_000);

afterAll(async () => {
  await env?.teardown();
});

let seq = 0;

function fakes() {
  const calls: string[] = [];
  return {
    calls,
    activities: {
      convergePool: async ({ profile }: { profile: string }) => {
        calls.push(`converge ${profile}`);
        return { nodes: 1, idleMinutes: 15 };
      },
      bootstrapPoolTenant: async ({ namespace }: { namespace: string }) => {
        calls.push(`tenant ${namespace}`);
      },
      applyPlacement: async (i: { namespace: string; actor: string; version: string; replicas: number }) => {
        calls.push(`apply ${i.namespace}/${i.actor}@${i.version} x${i.replicas}`);
        return { deployment: `${i.actor}-dep`, image: `r/${i.actor}@sha256:${'a'.repeat(64)}` };
      },
      deletePlacement: async (i: { namespace: string; actor: string; version: string }) => {
        calls.push(`delete ${i.namespace}/${i.actor}@${i.version}`);
      },
      destroyPool: async ({ profile }: { profile: string }) => {
        calls.push(`destroy ${profile}`);
      },
      poolHoldersAlive: async (i: { holders: Array<{ workflowId: string; namespace: string }> }) => ({ alive: i.holders }),
    },
  };
}

async function withPool<T>(f: ReturnType<typeof fakes>, body: (h: Awaited<ReturnType<typeof start>>) => Promise<T>): Promise<T> {
  const taskQueue = `pool-test-${(seq += 1)}`;
  const worker = await Worker.create({ connection: env.nativeConnection, taskQueue, workflowBundle: bundle, activities: f.activities });
  return worker.runUntil(async () => body(await start(taskQueue)));
}

async function start(taskQueue: string) {
  const profile = `p${seq}`;
  const handle = await env.client.workflow.signalWithStart(POOL_WORKFLOW, {
    taskQueue,
    workflowId: `kontra-pool/${profile}`,
    args: [{ profile }],
    signal: POOL_TOUCH_SIGNAL,
    signalArgs: [],
  });
  const hold = (lease: string, ns = 'ws-a') =>
    handle.executeUpdate(POOL_HOLD_UPDATE, { args: [{ lease, holder: `run-${lease}`, holderNamespace: ns }] });
  const place = (lease: string, actor: string, replicas = 1, ns = 'ws-a') =>
    handle.executeUpdate(POOL_PLACE_UPDATE, { args: [{ lease, namespace: ns, actor, version: '1.0.0', replicas }] });
  const drop = (lease: string) => handle.signal(POOL_DROP_SIGNAL, { lease });
  const state = () => handle.query<PoolState>(POOL_QUERY);
  return { handle, profile, hold, place, drop, state };
}

describe('the fleet pool', () => {
  it('converges once, however many holds arrive', async () => {
    const f = fakes();
    await withPool(f, async ({ hold, drop, handle }) => {
      await hold('a#1');
      await hold('b#1', 'ws-b');
      expect(f.calls.filter((c) => c.startsWith('converge'))).toHaveLength(1);
      expect(f.calls).toContain('tenant ws-a');
      expect(f.calls).toContain('tenant ws-b');
      await drop('a#1');
      await drop('b#1');
      await env.sleep('20 minutes');
      await handle.result();
    });
  });

  it('applies a placement once for two holders, keeps it while one remains, deletes it with the last', async () => {
    const f = fakes();
    await withPool(f, async ({ hold, place, drop, state, handle }) => {
      await hold('a#1');
      await hold('b#1');
      await place('a#1', 'enrich', 2);
      await place('b#1', 'enrich', 2);
      expect(f.calls.filter((c) => c.startsWith('apply'))).toEqual(['apply ws-a/enrich@1.0.0 x2']);
      await drop('a#1');
      await env.sleep('1 second');
      expect(f.calls.filter((c) => c.startsWith('delete'))).toEqual([]);
      expect((await state()).placements[0]!.holders).toEqual(['b#1']);
      await drop('b#1');
      await env.sleep('1 second');
      expect(f.calls.filter((c) => c.startsWith('delete'))).toEqual(['delete ws-a/enrich@1.0.0']);
      await env.sleep('20 minutes');
      await handle.result();
    });
  });

  it('scales to the largest ask, and back down when that holder leaves', async () => {
    const f = fakes();
    await withPool(f, async ({ hold, place, drop, handle }) => {
      await hold('a#1');
      await hold('b#1');
      await place('a#1', 'crawl', 2);
      await place('b#1', 'crawl', 8);
      await drop('b#1');
      await env.sleep('1 second');
      expect(f.calls.filter((c) => c.startsWith('apply'))).toEqual([
        'apply ws-a/crawl@1.0.0 x2',
        'apply ws-a/crawl@1.0.0 x8',
        'apply ws-a/crawl@1.0.0 x2',
      ]);
      await drop('a#1');
      await env.sleep('20 minutes');
      await handle.result();
    });
  });

  it('keeps the nodes through the idle grace, and a hold in it cancels the teardown', async () => {
    const f = fakes();
    await withPool(f, async ({ hold, drop, handle }) => {
      await hold('a#1');
      await drop('a#1');
      await env.sleep('10 minutes');
      expect(f.calls).not.toContain(`destroy p${seq}`);
      await hold('b#1');
      expect(f.calls.filter((c) => c.startsWith('converge'))).toHaveLength(1);
      await drop('b#1');
      await env.sleep('16 minutes');
      const final = await handle.result();
      expect(final.destroyed).toBe(true);
      expect(f.calls.filter((c) => c.startsWith('destroy'))).toHaveLength(1);
    });
  });

  it('refuses a placement from a lease that does not hold the fleet', async () => {
    const f = fakes();
    await withPool(f, async ({ hold, place, drop, handle }) => {
      await hold('a#1');
      await expect(place('nobody#1', 'enrich')).rejects.toThrow();
      await drop('a#1');
      await env.sleep('20 minutes');
      await handle.result();
    });
  });

  it('drops a hold whose run has ended, at its deadline', async () => {
    const f = fakes();
    f.activities.poolHoldersAlive = async () => ({ alive: [] });
    await withPool(f, async ({ handle }) => {
      await handle.executeUpdate(POOL_HOLD_UPDATE, {
        args: [{ lease: 'gone#1', holder: 'run-gone', holderNamespace: 'ws-a', ttlMs: 60_000 }],
      });
      await env.sleep('2 minutes');
      await env.sleep('16 minutes');
      const final = await handle.result();
      expect(final.leases).toEqual([]);
      expect(final.destroyed).toBe(true);
    });
  });
});
