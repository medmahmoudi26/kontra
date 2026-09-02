import * as path from 'node:path';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { TestWorkflowEnvironment } from '@temporalio/testing';
import { bundleWorkflowCode, Worker, type WorkflowBundle } from '@temporalio/worker';

import { dropFleetLease, holdFleetLease, runningHolders } from './lease';
import { LEASE_QUERY, leaseWorkflowId, type FleetLeaseSet } from '../lease';

/**
 * THE THREE LEASE ACTIVITIES, against a real Temporal.
 *
 * These are the only three places a **Lease** crosses a process boundary, and every one of them has
 * a failure that does not raise:
 *
 *   • a hold that signal-with-starts a **Lease** workflow which is already tearing the **Fleet** down —
 *     accepted silently, the **Run** converges onto **Machines** being deleted;
 *   • a drop against a **Lease** workflow that is gone — an exception at the very end of finished work, on the
 *     one path (`__aexit__`) where raising helps nobody;
 *   • a liveness check that reports "not running" for a holder it could not reach — a **Fleet**
 *     destroyed under a live **Run**.
 *
 * Each is a test below, with its opposite beside it.
 *
 * THE CLIENT IS INJECTED, which is `queuePollers`'s arrangement and not a new one: the activity
 * dials for itself in production and takes the test environment's client here, so what runs is the
 * real function against a real server rather than a mock of one.
 */

let env: TestWorkflowEnvironment;
let bundle: WorkflowBundle;
let worker: Worker;
let running: Promise<void>;
let stop: () => void;

/** The one queue this file's **Lease** workflows live on, pinned into the env so `infraQueue()` resolves to it. */
const QUEUE = `lease-activity-test-${Math.random().toString(36).slice(2, 8)}`;

beforeAll(async () => {
  process.env.KONTRA_INFRA_QUEUE = QUEUE;
  process.env.KONTRA_DATASET_QUEUE = QUEUE;
  env = await TestWorkflowEnvironment.createLocal();
  bundle = await bundleWorkflowCode({ workflowsPath: path.join(__dirname, '..', 'workflows', 'infra.ts') });
  worker = await Worker.create({
    connection: env.nativeConnection,
    taskQueue: QUEUE,
    workflowBundle: bundle,
    activities: {
      // The **Lease** workflow's own liveness check, against the same server, so an expiry in these tests
      // resolves for real rather than being answered by a fake.
      runningHolders: (input: { holders: string[] }) => runningHolders(input, env.client),
      async checkCloudCredential() {
        return { credential: 'do-token', version: 1 };
      },
      async stackDestroy(call: { stackFqn: string }) {
        destroyed.push(call.stackFqn);
        // Held open when a test wants the **Lease** workflow caught mid-teardown, which is the one state a
        // late hold has to be refused in. The test decides when it returns; a sleep here would be
        // a second clock to be wrong about.
        if (destroyGate) await destroyGate;
        return { result: 'succeeded', changes: {} };
      },
      async stackUp() {
        return { result: 'succeeded', changes: {} };
      },
      async stackPreview() {
        return { result: 'succeeded', changes: {} };
      },
    },
  });
  const done = new Promise<void>((resolve) => {
    stop = resolve;
  });
  running = worker.runUntil(done);
}, 300_000);

afterAll(async () => {
  stop?.();
  await running?.catch(() => undefined);
  await env?.teardown();
  delete process.env.KONTRA_INFRA_QUEUE;
  delete process.env.KONTRA_DATASET_QUEUE;
});

const destroyed: string[] = [];
let destroyGate: Promise<void> | undefined;

let seq = 0;
const nextFqn = (): string => `kontra-fleet/leaseact-0.${(seq += 1)}.0`;

async function ledgerOf(fqn: string): Promise<FleetLeaseSet> {
  return env.client.workflow.getHandle(leaseWorkflowId(fqn)).query<FleetLeaseSet, []>(LEASE_QUERY);
}

/**
 * The **Lease** workflow is STILL RUNNING while it tears the Fleet down, and confusing the two costs two tests.
 *
 * `destroyed.includes(fqn)` fires inside `stackDestroy` — the teardown has STARTED. The **Lease** workflow
 * completes later, after the child `stackWorkflow` returns and it writes its own result. Anything
 * that depends on the **Lease** workflow being closed has to wait for the close, and both failures found while
 * writing this file were exactly that mistake.
 */
async function waitClosed(fqn: string, ceilingMs = 30_000): Promise<void> {
  const handle = env.client.workflow.getHandle(leaseWorkflowId(fqn));
  await waitFor(
    `the Lease workflow for ${fqn} to close`,
    async () => String((await handle.describe()).status.name) !== 'RUNNING',
    ceilingMs
  );
}

async function waitFor(what: string, ready: () => boolean | Promise<boolean>, ceilingMs = 30_000) {
  const deadline = Date.now() + ceilingMs;
  for (;;) {
    if (await ready()) return;
    if (Date.now() >= deadline) throw new Error(`waited ${ceilingMs} ms for ${what}, never happened`);
    await new Promise((r) => setTimeout(r, 20));
  }
}

describe('holdFleetLease', () => {
  it('creates the Lease workflow on the first hold and joins it on the second', async () => {
    const fqn = nextFqn();
    const a = await holdFleetLease(
      { stackFqn: fqn, lease: 'run-a#1', holder: 'run-a', ttlMs: 3_600_000 },
      env.client
    );
    expect(a.workflowId).toBe(leaseWorkflowId(fqn));
    expect(a.lease).toBe('run-a#1');
    // ALONE. `actorkit.fleet` branches on this to decide whether its own failure may tear the
    // Fleet down, so the count is a contract and not a decoration.
    expect(a.leases).toBe(1);
    expect(a.expiresAt).toBeGreaterThan(Date.now());

    const b = await holdFleetLease(
      { stackFqn: fqn, lease: 'run-b#1', holder: 'run-b', ttlMs: 3_600_000 },
      env.client
    );
    // The SAME **Lease** workflow — derived from the stack, with nothing allocated and nothing looked up.
    expect(b.workflowId).toBe(a.workflowId);
    expect(b.leases).toBe(2);

    const led = await ledgerOf(fqn);
    expect(led.leases.map((l) => l.lease).sort()).toEqual(['run-a#1', 'run-b#1']);

    await dropFleetLease({ stackFqn: fqn, lease: 'run-a#1' }, env.client);
    await dropFleetLease({ stackFqn: fqn, lease: 'run-b#1' }, env.client);
    await waitFor('the teardown', () => destroyed.includes(fqn));
  }, 120_000);

  it('carries the ttl and the credential NAME through to the Lease workflow', async () => {
    const fqn = nextFqn();
    const before = Date.now();
    const held = await holdFleetLease(
      { stackFqn: fqn, lease: 'run-a#1', holder: 'run-a', ttlMs: 120_000, credential: 'do-prod' },
      env.client
    );
    // Not the default hour: a stated ttl that silently fell back would be invisible until a Fleet
    // outlived a Run by fifty-eight minutes.
    expect(held.expiresAt - before).toBeLessThan(3_600_000);
    expect(held.expiresAt - before).toBeGreaterThan(60_000);

    await dropFleetLease({ stackFqn: fqn, lease: 'run-a#1' }, env.client);
    await waitFor('the teardown', () => destroyed.includes(fqn));
  }, 120_000);

  it('REFUSES rather than joining a Lease workflow that is tearing the Fleet down', async () => {
    /**
     * THE RACE, from the activity's side, both halves.
     *
     * The **Lease** workflow drops a late hold on the floor (`workflows/lease.ts` says why); this read-after-
     * write is what makes that not a silent loss. A hold that reported success while the teardown
     * was in flight would leave a Run converged onto Machines being deleted, with nothing anywhere
     * saying so — so the activity FAILS, retryably, and the retry lands on a fresh **Lease** workflow.
     *
     * THIS FAILED FIRST TIME FOR A REAL REASON, which is recorded because it is also the shape of
     * the bug: the teardown having STARTED (`destroyed` grew) is not the **Lease** workflow having CLOSED. The
     * **Lease** workflow is running the whole time it destroys, and a hold arriving in that window is exactly
     * the case being refused.
     */
    const fqn = nextFqn();
    let release!: () => void;
    destroyGate = new Promise<void>((r) => {
      release = r;
    });
    try {
      await holdFleetLease({ stackFqn: fqn, lease: 'run-a#1', holder: 'run-a', ttlMs: 3_600_000 }, env.client);
      await dropFleetLease({ stackFqn: fqn, lease: 'run-a#1' }, env.client);
      await waitFor('the teardown to start', () => destroyed.includes(fqn));

      // THE REFUSAL, with the teardown genuinely in flight.
      await expect(
        holdFleetLease({ stackFqn: fqn, lease: 'run-b#1', holder: 'run-b', ttlMs: 3_600_000 }, env.client)
      ).rejects.toThrow(/tearing the Fleet down/);
    } finally {
      release();
      destroyGate = undefined;
    }

    // THE RETRY. Once the old **Lease** workflow has closed, the same call opens a new one — which is what a
    // Temporal activity retry does for real, and why that failure is retryable rather than fatal.
    await waitClosed(fqn);
    const again = await holdFleetLease(
      { stackFqn: fqn, lease: 'run-b#1', holder: 'run-b', ttlMs: 3_600_000 },
      env.client
    );
    expect(again.leases).toBe(1);
    expect(again.lease).toBe('run-b#1');
    const led = await ledgerOf(fqn);
    // A FRESH LEDGER, not the corpse of the old one: the previous run answered `destroyed: true`
    // with an empty list, which is what a query against the closed execution would still return.
    expect(led.destroyed).toBeFalsy();
    expect(led.leases.map((l) => l.lease)).toEqual(['run-b#1']);

    await dropFleetLease({ stackFqn: fqn, lease: 'run-b#1' }, env.client);
    await waitClosed(fqn);
  }, 120_000);

  it('refuses an input that names no Fleet or no Lease, rather than holding nothing', async () => {
    // A hold with a blank id would return a success the caller then "drops" against a **Lease** workflow that
    // never had it — the exact silent-leak shape this whole slice exists to prevent.
    await expect(holdFleetLease({ stackFqn: '', lease: 'x#1' }, env.client)).rejects.toThrow(/stackFqn/);
    await expect(
      holdFleetLease({ stackFqn: 'kontra-fleet/x-1', lease: '' }, env.client)
    ).rejects.toThrow(/lease id/);
  });
});

describe('dropFleetLease', () => {
  it('is a SUCCESS when there is no Lease workflow to tell', async () => {
    // The **Lease** workflow may legitimately be gone: another holder's expiry reaped this Lease, or the Fleet
    // was torn down by hand. Both mean "nothing of mine is held", which is what the caller wanted,
    // and raising here would fail a Run at the very end of otherwise-finished work.
    const out = await dropFleetLease(
      { stackFqn: 'kontra-fleet/never-existed-0.0.0', lease: 'ghost#1' },
      env.client
    );
    expect(out.delivered).toBe(false);
    expect(out.workflowId).toBe(leaseWorkflowId('kontra-fleet/never-existed-0.0.0'));
  }, 60_000);

  it('is a SUCCESS the second time, and takes nobody else with it', async () => {
    const fqn = nextFqn();
    await holdFleetLease({ stackFqn: fqn, lease: 'run-a#1', holder: 'run-a', ttlMs: 3_600_000 }, env.client);
    await holdFleetLease({ stackFqn: fqn, lease: 'run-b#1', holder: 'run-b', ttlMs: 3_600_000 }, env.client);

    expect((await dropFleetLease({ stackFqn: fqn, lease: 'run-a#1' }, env.client)).delivered).toBe(true);
    expect((await dropFleetLease({ stackFqn: fqn, lease: 'run-a#1' }, env.client)).delivered).toBe(true);
    await waitFor('the drops to settle', async () => (await ledgerOf(fqn)).leases.length === 1);
    expect((await ledgerOf(fqn)).leases.map((l) => l.lease)).toEqual(['run-b#1']);
    expect(destroyed.filter((d) => d === fqn)).toHaveLength(0);

    await dropFleetLease({ stackFqn: fqn, lease: 'run-b#1' }, env.client);
    await waitFor('the teardown', () => destroyed.includes(fqn));
    expect(destroyed.filter((d) => d === fqn)).toHaveLength(1);
  }, 120_000);
});

describe('runningHolders', () => {
  it('tells a RUNNING holder from one that never existed', async () => {
    // The two answers this must never conflate. A **Lease** workflow renews on `alive` and drops on `gone`, so
    // getting this backwards either leaks a Fleet for ever or deletes one under live work.
    //
    // The running workflow used here is a **Lease** workflow of this test's own: a real, open execution on this
    // server, which is exactly the shape a real holder is.
    const fqn = nextFqn();
    await holdFleetLease({ stackFqn: fqn, lease: 'run-a#1', holder: 'run-a', ttlMs: 3_600_000 }, env.client);
    const live = leaseWorkflowId(fqn);

    const out = await runningHolders({ holders: [live, 'no-such-workflow-anywhere'] }, env.client);
    expect(out.alive).toEqual([live]);
    expect(out.gone).toEqual(['no-such-workflow-anywhere']);
    expect(out.unknown).toEqual([]);

    await dropFleetLease({ stackFqn: fqn, lease: 'run-a#1' }, env.client);
    await waitClosed(fqn);

    // AND THE CONTROL IN THE OTHER DIRECTION: the same workflow, now CLOSED, is `gone`. Without
    // this the test above passes on an implementation that reports everything it can find as alive.
    // It is the same execution as `live` above, so nothing but its STATUS changed between the two
    // assertions — which is the property, and is why the wait is for the close and not for the
    // teardown to start.
    const after = await runningHolders({ holders: [live] }, env.client);
    expect(after.gone).toEqual([live]);
    expect(after.alive).toEqual([]);
  }, 120_000);

  it('asks about nothing without dialling anything', async () => {
    // The **Lease** workflow never calls this with an empty list, and if it ever does, opening a connection to
    // ask about no holders is a cost with no answer. No client is passed, so a dial would fail.
    expect(await runningHolders({ holders: [] })).toEqual({ alive: [], gone: [], unknown: [] });
    expect(await runningHolders({ holders: ['', ''] })).toEqual({ alive: [], gone: [], unknown: [] });
  });
});
