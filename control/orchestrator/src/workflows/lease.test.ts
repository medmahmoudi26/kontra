import * as path from 'node:path';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { TestWorkflowEnvironment } from '@temporalio/testing';
import { ApplicationFailure } from '@temporalio/common';
import { bundleWorkflowCode, Worker, type WorkflowBundle } from '@temporalio/worker';
import type { WorkflowHandle } from '@temporalio/client';

import {
  LEASE_DROP_SIGNAL,
  LEASE_HOLD_SIGNAL,
  LEASE_QUERY,
  LEASE_UNKNOWN_LIMIT,
  LEASE_WORKFLOW,
  type FleetLeaseSet,
} from '../lease';
import * as bundleModule from './infra';
import * as applianceModule from './appliance';
import type { HoldersAliveInput, HoldersAliveOutput } from '../activities/lease';

/**
 * THE LEASE LEDGER, against a real Temporal with faked Pulumi and a faked liveness check.
 *
 * WHAT THIS FILE IS FOR, in one sentence: the one failure this program must not introduce is the
 * first shared **Fleet** leaking **Machines**, and there are exactly three ways a **Lease** fails to
 * drop — dropped twice, never dropped because the holder died, never dropped because the control
 * plane restarted holding nothing. Each has a test below, and each has its OPPOSITE beside it,
 * because a **Lease** workflow that destroys everything passes all three and is worse than the leak.
 *
 * ── THE TTL IS A PARAMETER, WHICH IS WHY THIS RUNS IN SECONDS ────────────────────────────────────
 *
 * `LEASE_TTL_MS` is an hour and every test below states its own `ttlMs` in the low hundreds of
 * milliseconds. That is not a shortcut around the mechanism, it IS the mechanism: the deadline is
 * carried per **Lease** on the hold signal, so a 200 ms **Lease** and a one-hour **Lease** take the
 * same code path through the same timer. What a time-skipping environment would buy here is testing
 * the DEFAULT rather than the mechanism, at the price of a second server binary to download.
 *
 * ── AND THE WAITS ARE ON CONDITIONS, NOT ON THE CLOCK ────────────────────────────────────────────
 *
 * `stack.test.ts` records what the alternative cost — a 600 ms sleep that passed at 58 s on an idle
 * box and failed at 171 s on the same tree under load, teaching everyone to re-run rather than read.
 * Every step below waits for something OBSERVABLE with a generous giving-up point. The two places a
 * real elapsed interval is unavoidable are the "nothing happened" measurements, which are assertions
 * about a period rather than about an event, and both are stated as such.
 */

let env: TestWorkflowEnvironment;
let bundle: WorkflowBundle;

beforeAll(async () => {
  env = await TestWorkflowEnvironment.createLocal();
  bundle = await bundleWorkflowCode({ workflowsPath: path.join(__dirname, 'infra.ts') });
}, 300_000);

afterAll(async () => {
  await env?.teardown();
});

let seq = 0;
const nextFqn = (): string => `kontra-fleet/nscheck-0.${(seq += 1)}.0`;
const nextQueue = (): string => `lease-test-${seq}-${Math.random().toString(36).slice(2, 7)}`;

/** Everything the fakes saw, in order. */
interface Recorded {
  /** Every `stackDestroy` — the thing that must happen exactly once, and only at zero Leases. */
  destroys: string[];
  /** Every liveness check, so a renewal can be told from a check that never ran. */
  checks: HoldersAliveInput[];
}

interface Fakes {
  /** Holders the check reports as RUNNING. Everything else is `gone`. */
  alive?: string[];
  /** Holders the check cannot answer for. */
  unknown?: string[];
  /** The check throws — the **Lease** workflow must treat every holder it asked about as unknown. */
  checkFails?: string;
  /** Holds `stackDestroy` open until the test releases it. */
  destroyHeldOpen?: Promise<void>;
  /** The teardown refuses the way an appliance's `stackWorkflow` does. */
  destroyRefuses?: string;
}

function activities(rec: Recorded, over: Fakes = {}) {
  return {
    async runningHolders(input: HoldersAliveInput): Promise<HoldersAliveOutput> {
      rec.checks.push(input);
      if (over.checkFails) throw new Error(over.checkFails);
      const alive = new Set(over.alive ?? []);
      const unknown = new Set(over.unknown ?? []);
      const out: HoldersAliveOutput = { alive: [], gone: [], unknown: [] };
      for (const h of input.holders) {
        if (alive.has(h)) out.alive.push(h);
        else if (unknown.has(h)) out.unknown.push(h);
        else out.gone.push(h);
      }
      return out;
    },
    // `stackWorkflow`'s two activities, faked exactly as `stack.test.ts` fakes them: what this file
    // pins is WHEN a teardown is asked for, not what Pulumi does with it.
    async checkCloudCredential(): Promise<{ credential: string; version: number }> {
      return { credential: 'do-token', version: 1 };
    },
    async stackDestroy(call: { stackFqn: string; args?: Record<string, unknown> }) {
      rec.destroys.push(`${call.stackFqn}|${String(call.args?.credential ?? '')}`);
      if (over.destroyHeldOpen) await over.destroyHeldOpen;
      if (over.destroyRefuses) {
        throw ApplicationFailure.nonRetryable(over.destroyRefuses, 'NoProvisioner');
      }
      return { result: 'succeeded', changes: {} };
    },
    async stackUp() {
      return { result: 'succeeded', changes: {} };
    },
    async stackPreview() {
      return { result: 'succeeded', changes: {} };
    },
  };
}

/** Temporal wraps a workflow failure several layers deep (`WorkflowFailedError` → `ApplicationFailure`
 *  → …), and only the innermost layer carries what the workflow actually said. */
function causeChain(err: unknown): string {
  const parts: string[] = [];
  for (let cur: unknown = err, i = 0; cur && i < 8; i += 1) {
    parts.push(String((cur as Error).message ?? cur));
    cur = (cur as { cause?: unknown }).cause;
  }
  return parts.join(' | ');
}

/** Wait for something observable, with a giving-up point rather than a bet on a duration. */
async function waitFor(what: string, ready: () => boolean | Promise<boolean>, ceilingMs = 30_000) {
  const deadline = Date.now() + ceilingMs;
  for (;;) {
    if (await ready()) return;
    if (Date.now() >= deadline) throw new Error(`waited ${ceilingMs} ms for ${what}, never happened`);
    await new Promise((r) => setTimeout(r, 20));
  }
}

interface Held {
  handle: WorkflowHandle;
  hold(lease: string, opts?: { holder?: string; ttlMs?: number; credential?: string }): Promise<void>;
  drop(lease: string): Promise<void>;
  read(): Promise<FleetLeaseSet>;
  status(): Promise<string>;
  events(): Promise<number>;
}

/**
 * Open a **Lease** workflow the way `holdFleetLease` does — signal-with-start — and hand back the four verbs a
 * test needs. `queue` is per-test so one test's worker cannot serve another's **Lease** workflow.
 */
async function startLeases(
  fqn: string,
  queue: string,
  first: { lease: string; holder?: string; ttlMs?: number; credential?: string }
): Promise<Held> {
  const handle = await env.client.workflow.signalWithStart(LEASE_WORKFLOW, {
    taskQueue: queue,
    workflowId: `kontra-lease/${fqn}`,
    args: [{ stackFqn: fqn, livenessQueue: queue }],
    signal: LEASE_HOLD_SIGNAL,
    signalArgs: [{ holder: '', ...first }],
  });
  return {
    handle,
    hold: (lease, opts = {}) =>
      handle.signal(LEASE_HOLD_SIGNAL, { lease, holder: '', ...opts }),
    drop: (lease) => handle.signal(LEASE_DROP_SIGNAL, { lease }),
    read: () => handle.query(LEASE_QUERY) as Promise<FleetLeaseSet>,
    status: async () => String((await handle.describe()).status.name),
    events: async () => (await handle.fetchHistory()).events?.length ?? 0,
  };
}

async function worker(queue: string, rec: Recorded, over: Fakes = {}): Promise<Worker> {
  return Worker.create({
    connection: env.nativeConnection,
    taskQueue: queue,
    workflowBundle: bundle,
    activities: activities(rec, over),
  });
}

const fresh = (): Recorded => ({ destroys: [], checks: [] });

// ═════════════════════════════════════════════════════════════════════════════════════════════════
// LAST ONE OUT — the four "done when"s of this slice, and the controls that make them mean something
// ═════════════════════════════════════════════════════════════════════════════════════════════════

describe('several Runs hold one Fleet', () => {
  it('the FIRST to exit destroys nothing, and the LAST destroys once', async () => {
    // ADR 0037: "A Fleet is NOT owned by one Run — several Runs may hold it at once, and it outlives
    // any one of them." Before this slice the first scope exit tore the Fleet down unconditionally,
    // which is correct with exactly one owner and is the leak's mirror image with two.
    const rec = fresh();
    const fqn = nextFqn();
    const queue = nextQueue();
    const w = await worker(queue, rec);

    await w.runUntil(async () => {
      const led = await startLeases(fqn, queue, { lease: 'run-a#1', holder: 'run-a', ttlMs: 3_600_000 });
      await led.hold('run-b#1', { holder: 'run-b', ttlMs: 3_600_000 });
      await waitFor('both holds to land', async () => (await led.read()).leases.length === 2);

      const both = await led.read();
      expect(both.leases.map((l) => l.lease)).toEqual(['run-a#1', 'run-b#1']);
      expect(both.leases.map((l) => l.holder)).toEqual(['run-a', 'run-b']);

      // THE FIRST OUT.
      await led.drop('run-a#1');
      await waitFor('the first drop', async () => (await led.read()).leases.length === 1);
      expect(rec.destroys).toEqual([]);
      expect(await led.status()).toBe('RUNNING');
      expect((await led.read()).leases[0]?.lease).toBe('run-b#1');

      // THE LAST OUT.
      await led.drop('run-b#1');
      await led.handle.result();
      expect(rec.destroys).toHaveLength(1);
      expect(rec.destroys[0]).toBe(`${fqn}|`);
      expect((await led.read()).destroyed).toBe(true);
    });
  }, 120_000);

  it('a Lease dropped TWICE destroys once and takes nobody else with it', async () => {
    // The first of the three ways a Lease fails to drop cleanly. `Map.delete` of an absent key is
    // the whole mechanism, and the teardown is driven by the SIZE of the **Lease** workflow rather than by a
    // count of drops — which is what makes a repeated drop structurally unable to double-fire.
    const rec = fresh();
    const fqn = nextFqn();
    const queue = nextQueue();
    const w = await worker(queue, rec);

    await w.runUntil(async () => {
      const led = await startLeases(fqn, queue, { lease: 'run-a#1', holder: 'run-a', ttlMs: 3_600_000 });
      await led.hold('run-b#1', { holder: 'run-b', ttlMs: 3_600_000 });
      await waitFor('both holds', async () => (await led.read()).leases.length === 2);

      await led.drop('run-a#1');
      await led.drop('run-a#1');
      await led.drop('run-a#1');
      await waitFor('the drops to settle', async () => (await led.read()).leases.length === 1);

      // THREE DROPS OF ONE LEASE LEFT THE OTHER ONE ALONE, and destroyed nothing.
      expect((await led.read()).leases.map((l) => l.lease)).toEqual(['run-b#1']);
      expect(rec.destroys).toEqual([]);
      expect(await led.status()).toBe('RUNNING');

      await led.drop('run-b#1');
      await led.handle.result();
      expect(rec.destroys).toHaveLength(1);
    });
  }, 120_000);

  it('holding the SAME id twice is one Lease, refreshed', async () => {
    // A Run that retries its own hold — an activity retry, a replay — must not accumulate claims it
    // will only drop once. That would be a leak with no bad actor in it at all.
    const rec = fresh();
    const fqn = nextFqn();
    const queue = nextQueue();
    const w = await worker(queue, rec);

    await w.runUntil(async () => {
      const led = await startLeases(fqn, queue, { lease: 'run-a#1', holder: 'run-a', ttlMs: 3_600_000 });
      await led.hold('run-a#1', { holder: 'run-a', ttlMs: 3_600_000 });
      await led.hold('run-a#1', { holder: 'run-a', ttlMs: 7_200_000 });
      await waitFor('the refreshes', async () => (await led.read()).leases.length === 1);
      expect((await led.read()).leases).toHaveLength(1);

      await led.drop('run-a#1');
      await led.handle.result();
      expect(rec.destroys).toHaveLength(1);
    });
  }, 120_000);
});

// ═════════════════════════════════════════════════════════════════════════════════════════════════
// THE CLOCK — and the control that stops it being a way to destroy live work
// ═════════════════════════════════════════════════════════════════════════════════════════════════

describe('a Lease expires on a clock', () => {
  it('every Lease EXPIRED, with nothing running teardown, destroys the Machines', async () => {
    // The second and third ways a Lease fails to drop, which have one answer: the holder died
    // mid-run, or the control plane restarted holding nothing. NOTHING IN THIS TEST DROPS ANYTHING.
    const rec = fresh();
    const fqn = nextFqn();
    const queue = nextQueue();
    // `run-a` is not alive and not unknown, so the check answers `gone` — a Run Temporal has never
    // heard of, which is what a holder whose workflow was terminated looks like.
    const w = await worker(queue, rec, { alive: [] });

    await w.runUntil(async () => {
      const led = await startLeases(fqn, queue, { lease: 'run-a#1', holder: 'run-a', ttlMs: 250 });
      await led.handle.result();
      expect(rec.destroys).toHaveLength(1);
      // And it got there by ASKING, not by assuming — the deadline alone is never the verdict.
      expect(rec.checks.length).toBeGreaterThan(0);
      expect(rec.checks[0]?.holders).toEqual(['run-a']);
    });
  }, 120_000);

  it('a LIVE holder is renewed, not reaped — the control in the other direction', async () => {
    // WITHOUT THIS THE TEST ABOVE IS SATISFIED BY A LEDGER THAT DESTROYS EVERYTHING, which would be
    // a worse defect than the leak: Machines deleted under running work. The TTL is a check, not a
    // deadline, and this is the assertion that says so.
    const rec = fresh();
    const fqn = nextFqn();
    const queue = nextQueue();
    const w = await worker(queue, rec, { alive: ['run-a'] });

    await w.runUntil(async () => {
      const led = await startLeases(fqn, queue, { lease: 'run-a#1', holder: 'run-a', ttlMs: 200 });
      // Several deadlines' worth of a Lease nobody renewed by hand. The wait FAILS FAST on a
      // teardown rather than running out its ceiling, so the red says what went wrong instead of
      // saying that something did not happen for thirty seconds.
      await waitFor('three expiry checks with nothing destroyed', () => {
        if (rec.destroys.length > 0) {
          throw new Error(
            'the Lease workflow destroyed a Fleet whose holder Temporal reports as RUNNING — the TTL is ' +
              'acting as a deadline on the Run rather than as a check on the holder'
          );
        }
        return rec.checks.length >= 3;
      }, 30_000);
      expect(rec.destroys).toEqual([]);
      expect(await led.status()).toBe('RUNNING');
      expect((await led.read()).leases).toHaveLength(1);

      await led.drop('run-a#1');
      await led.handle.result();
      expect(rec.destroys).toHaveLength(1);
    });
  }, 120_000);

  it('an UNATTRIBUTED Lease is never renewed — the clock is the whole of its life', async () => {
    // `destroy_on_exit=False` takes one of these: a Fleet adopted for a handoff, held by nobody, so
    // there is nobody to ask about. The check is not even called, and the deadline is final.
    const rec = fresh();
    const fqn = nextFqn();
    const queue = nextQueue();
    // Every holder would be reported alive — so if an unattributed Lease were ever asked about, it
    // would be renewed for ever and this test would hang rather than pass for the wrong reason.
    const w = await worker(queue, rec, { alive: ['', 'run-a'] });

    await w.runUntil(async () => {
      const led = await startLeases(fqn, queue, { lease: 'adopted#1', holder: '', ttlMs: 250 });
      await led.handle.result();
      expect(rec.destroys).toHaveLength(1);
      expect(rec.checks).toEqual([]);
    });
  }, 120_000);

  it('a liveness answer nobody could get buys time, and only so much of it', async () => {
    /**
     * "0 because we could not ask is not 0 because nothing is polling" — `queuePollers`'s rule, and
     * the cost here is opposite in each direction: unknown-as-dead destroys a healthy Run's
     * Machines, unknown-as-alive for ever is the leak. So it extends, COUNTED, and then stops.
     *
     * THE COUNT IS EXACT, AND IT HAS TO BE. This assertion was written as
     * `toBeGreaterThanOrEqual(LEASE_UNKNOWN_LIMIT)` against a check that THREW, and it was vacuous:
     * the activity's own `maximumAttempts: 3` produced three records from ONE logical check, so a
     * **Lease** workflow that dropped the Lease at the first expiry passed. Proved by breaking it — the
     * unknown-is-dead mutation stayed green. The check now RETURNS `unknown` instead of throwing,
     * which is one record per expiry, and the count is pinned exactly.
     */
    const rec = fresh();
    const fqn = nextFqn();
    const queue = nextQueue();
    const w = await worker(queue, rec, { unknown: ['run-a'] });

    await w.runUntil(async () => {
      const led = await startLeases(fqn, queue, { lease: 'run-a#1', holder: 'run-a', ttlMs: 200 });
      await led.handle.result();
      // Extended at expiries 1 and 2, dropped at 3. One more would be a Fleet that outlives its
      // budget; one fewer is unknown read as dead.
      expect(rec.checks).toHaveLength(LEASE_UNKNOWN_LIMIT);
      expect(rec.destroys).toHaveLength(1);
    });
  }, 120_000);

  it('a liveness check that FAILS OUTRIGHT still ends the Fleet rather than hanging the Lease workflow', async () => {
    // The other half, and a different failure: the activity does not answer at all. Every holder it
    // was asked about becomes unknown, which is bounded exactly as above — but the thing that must
    // NOT happen is the failure propagating, because a **Lease** workflow that fails on a liveness error is the
    // one workflow that could still have torn this Fleet down, dead.
    const rec = fresh();
    const fqn = nextFqn();
    const queue = nextQueue();
    const w = await worker(queue, rec, { checkFails: 'temporal frontend: connection refused' });

    await w.runUntil(async () => {
      const led = await startLeases(fqn, queue, { lease: 'run-a#1', holder: 'run-a', ttlMs: 200 });
      // Resolves rather than rejects: the **Lease** workflow completed, having destroyed the Fleet.
      await led.handle.result();
      expect(rec.destroys).toHaveLength(1);
      // The check really was attempted — otherwise this passes on a **Lease** workflow that skipped it.
      expect(rec.checks.length).toBeGreaterThan(0);
    });
  }, 120_000);

  it('a Lease workflow that never held anything destroys NOTHING', async () => {
    // A **Lease** workflow is created by a hold, so "zero Leases" normally means "the last one dropped". A hold
    // the **Lease** workflow could not use — a malformed signal, an empty id — would otherwise reach the same
    // branch and tear down a Fleet nobody ever claimed. The teardown is gated on having HELD, not
    // merely on being empty.
    const rec = fresh();
    const fqn = nextFqn();
    const queue = nextQueue();
    const w = await worker(queue, rec);

    await w.runUntil(async () => {
      const led = await startLeases(fqn, queue, { lease: '' });
      const out = (await led.handle.result()) as { destroyed?: boolean };
      expect(rec.destroys).toEqual([]);
      expect(out.destroyed).toBe(false);
    });
  }, 120_000);
});

// ═════════════════════════════════════════════════════════════════════════════════════════════════
// KILLING THE CONTROL PLANE MID-HOLD
// ═════════════════════════════════════════════════════════════════════════════════════════════════

describe('a control plane that dies holding a Fleet', () => {
  it('still destroys it, from a worker that was not running when the deadline passed', async () => {
    /**
     * ADR 0037's whole claim for the TTL: "strictly stronger than scope-exit teardown, because scope
     * exit never covered the control plane dying."
     *
     * WHAT IS ACTUALLY KILLED HERE. There is no orchestrator process in this test — the control
     * plane's part in a teardown is a WORKER polling the infra queue, and this test stops it, lets
     * the deadline pass with nothing anywhere able to act on it, and starts a different worker.
     * Nothing signals anything. The `Fleet` is destroyed by a process that did not exist when its
     * last holder was alive, which is the property, and the second `rec` is what proves the first
     * worker did not quietly do it on the way out.
     */
    const before = fresh();
    const after = fresh();
    const fqn = nextFqn();
    const queue = nextQueue();

    const w1 = await worker(queue, before, { alive: ['run-a'] });
    const led = await w1.runUntil(async () => {
      const l = await startLeases(fqn, queue, { lease: 'run-a#1', holder: 'run-a', ttlMs: 1_500 });
      await waitFor('the hold to land', async () => (await l.read()).leases.length === 1);
      return l;
    });
    // The control plane is gone. `runUntil` has drained the worker; nothing polls this queue.
    expect(before.destroys).toEqual([]);

    // The deadline passes in the dark. A real elapsed interval, and it has to be: the assertion is
    // about a period in which no process was able to act, which no condition can express.
    await new Promise((r) => setTimeout(r, 3_000));
    expect(before.destroys).toEqual([]);
    expect(after.destroys).toEqual([]);
    expect(await led.status()).toBe('RUNNING');

    // A NEW worker — the control plane comes back, holding nothing and told nothing.
    const w2 = await worker(queue, after, { alive: [] });
    await w2.runUntil(async () => {
      await led.handle.result();
    });

    expect(before.destroys).toEqual([]);
    expect(after.destroys).toHaveLength(1);
    expect(after.destroys[0]).toBe(`${fqn}|`);
  }, 120_000);
});

// ═════════════════════════════════════════════════════════════════════════════════════════════════
// THE TEARDOWN ITSELF
// ═════════════════════════════════════════════════════════════════════════════════════════════════

describe('the teardown the last drop fires', () => {
  it('uses the credential NAME the most recent hold named', async () => {
    // The **Lease** workflow outlives the Run that provisioned, so the last holder out is very often not the one
    // that paid. A teardown against the control plane's default name when the Fleet was brought up
    // under `do-prod` fails to resolve and leaves Droplets billing — strictly worse than a failed
    // provision, which is why `kontra.fleet` already carried this on the destroy.
    const rec = fresh();
    const fqn = nextFqn();
    const queue = nextQueue();
    const w = await worker(queue, rec);

    await w.runUntil(async () => {
      const led = await startLeases(fqn, queue, {
        lease: 'run-a#1',
        holder: 'run-a',
        ttlMs: 3_600_000,
        credential: 'do-prod',
      });
      await waitFor('the hold', async () => (await led.read()).leases.length === 1);
      await led.drop('run-a#1');
      await led.handle.result();
      expect(rec.destroys).toEqual([`${fqn}|do-prod`]);
    });
  }, 120_000);

  it('a hold arriving while the Lease workflow is tearing down is REFUSED, not accepted', async () => {
    /**
     * THE ONE RACE IN THE DESIGN. Run A drops its last Lease, the **Lease** workflow commits to teardown, Run B
     * holds a microsecond later. Accepting B would leave B converged onto Machines this workflow is
     * deleting — a Fleet that reports as held and is being destroyed.
     *
     * The hold is dropped on the floor, which is not silent: `holdFleetLease` confirms its own hold
     * by reading the **Lease** workflow back, and `activities/lease.test.ts` is where that half is proven. This
     * test pins the **Lease** workflow's half — that the late claim does not survive into the answer.
     */
    const rec = fresh();
    const fqn = nextFqn();
    const queue = nextQueue();
    let release!: () => void;
    const destroyHeldOpen = new Promise<void>((r) => {
      release = r;
    });
    const w = await worker(queue, rec, { destroyHeldOpen });

    await w.runUntil(async () => {
      const led = await startLeases(fqn, queue, { lease: 'run-a#1', holder: 'run-a', ttlMs: 3_600_000 });
      try {
        await waitFor('the hold', async () => (await led.read()).leases.length === 1);
        await led.drop('run-a#1');
        // The teardown is genuinely in flight — its own observable effect, not an elapsed guess.
        await waitFor('the teardown to start', () => rec.destroys.length > 0);

        // Run B, too late.
        await led.hold('run-b#1', { holder: 'run-b', ttlMs: 3_600_000 });
        const during = await led.read();
        expect(during.leases).toEqual([]);
      } finally {
        // UNCONDITIONAL. A failing assertion above leaves `stackDestroy` blocked for ever, so the
        // worker never drains and the red arrives as a 120-second timeout instead of as the
        // sentence that says what went wrong. (Observed: the race-accepted mutation reported
        // "Test timed out" rather than "expected 1 lease to equal []".)
        release();
      }
      await led.handle.result();
      const done = await led.read();
      expect(done.leases).toEqual([]);
      expect(done.destroyed).toBe(true);
      // ONE teardown. A **Lease** workflow that had accepted B and then looped would have run a second.
      expect(rec.destroys).toHaveLength(1);
    });
  }, 120_000);

  it('fails LOUDLY, naming the fix, when the Machines cannot be destroyed', async () => {
    // A **Lease** workflow that could not tear down must not complete quietly — completing says "this Fleet is
    // gone". It also must not retry a permanent refusal five times: an appliance's `stackWorkflow`
    // refuses non-retryably and will refuse identically in thirty seconds.
    const rec = fresh();
    const fqn = nextFqn();
    const queue = nextQueue();
    const w = await worker(queue, rec, { destroyRefuses: 'this control plane has no provisioner' });

    await w.runUntil(async () => {
      const led = await startLeases(fqn, queue, { lease: 'run-a#1', holder: 'run-a', ttlMs: 3_600_000 });
      await waitFor('the hold', async () => (await led.read()).leases.length === 1);
      await led.drop('run-a#1');
      const err = await led.handle.result().then(
        () => undefined,
        (e: unknown) => e
      );
      expect(err).toBeDefined();
      // Temporal wraps a workflow failure several layers deep and only the innermost carries what
      // the workflow said, which is why `stack.test.ts` has the same helper.
      expect(causeChain(err)).toMatch(/still running and still billing/);
      expect(causeChain(err)).toMatch(/kontra fleet down/);
      // ONE attempt, not five: it stopped at the non-retryable refusal.
      expect(rec.destroys).toHaveLength(1);
    });
  }, 120_000);
});

// ═════════════════════════════════════════════════════════════════════════════════════════════════
// WHAT IT COSTS — measured, with controls in both directions
// ═════════════════════════════════════════════════════════════════════════════════════════════════

describe('what a Lease workflow costs', () => {
  it('writes NOTHING while it holds, and six events per hold and per drop', async () => {
    /**
     * The Warden's standard (`cli/warden/warden_workflow.go`): "0 events/hour at rest, 6 per real change",
     * with controls in both directions so that "it did not grow" is a statement about the design and
     * not about something that was never running.
     *
     * THE CONTROL IS THE SECOND LEDGER. Two **Lease** workflows are opened side by side on the same worker: one
     * with an hour-long TTL, which should sit blocked and silent, and one with a 200 ms TTL, which
     * must grow over the SAME wall-clock window. If the counter were broken, or the workflow were
     * not running at all, both would read zero and the "at rest" claim would be about nothing.
     */
    const rec = fresh();
    const queue = nextQueue();
    const quietFqn = nextFqn();
    const tickingFqn = nextFqn();
    const w = await worker(queue, rec, { alive: ['ticker'] });

    await w.runUntil(async () => {
      const quiet = await startLeases(quietFqn, queue, {
        lease: 'run-a#1',
        holder: 'run-a',
        ttlMs: 3_600_000,
      });
      const ticking = await startLeases(tickingFqn, queue, {
        lease: 'ticker#1',
        holder: 'ticker',
        ttlMs: 200,
      });
      await waitFor('both holds', async () => (await quiet.read()).leases.length === 1);
      await waitFor('the ticking one to check', () => rec.checks.length > 0);

      const quietBefore = await quiet.events();
      const tickingBefore = await ticking.events();
      const checksBefore = rec.checks.length;

      // A real elapsed window. There is no event to wait for — the claim is that none arrives.
      const WINDOW_MS = 4_000;
      await new Promise((r) => setTimeout(r, WINDOW_MS));

      const quietAfter = await quiet.events();
      const tickingAfter = await ticking.events();
      const checks = rec.checks.length - checksBefore;
      const perHour = (n: number) => Math.round((n / WINDOW_MS) * 3_600_000);

      // eslint-disable-next-line no-console
      console.log(
        `[lease cost] at rest: ${quietAfter - quietBefore} events in ${WINDOW_MS} ms ` +
          `(${perHour(quietAfter - quietBefore)}/hour). ` +
          `control, a 200 ms TTL over the same window: ${tickingAfter - tickingBefore} events ` +
          `(${perHour(tickingAfter - tickingBefore)}/hour) over ${checks} expiry checks = ` +
          `${((tickingAfter - tickingBefore) / Math.max(checks, 1)).toFixed(1)} events per check`
      );

      // THE MEASUREMENT: a held Fleet with nothing happening to it costs nothing.
      expect(quietAfter - quietBefore).toBe(0);
      // THE CONTROL: the same counter, on the same server, over the same window, does grow — and it
      // grows because expiry checks RAN, not because the counter drifts. Both halves asserted, so
      // "it did not grow" is a claim about the quiet **Lease** workflow's design rather than about a dead
      // counter or a workflow that was never running.
      expect(checks).toBeGreaterThan(0);
      expect(tickingAfter - tickingBefore).toBeGreaterThan(0);

      // AND SIX PER REAL CHANGE — Signal, WorkflowTaskScheduled/Started/Completed, TimerCanceled,
      // TimerStarted. The pair is what a blocked workflow re-arming its one timer costs.
      const beforeHold = await quiet.events();
      await quiet.hold('run-b#1', { holder: 'run-b', ttlMs: 3_600_000 });
      await waitFor('the second hold', async () => (await quiet.read()).leases.length === 2);
      const afterHold = await quiet.events();

      await quiet.drop('run-b#1');
      await waitFor('the drop', async () => (await quiet.read()).leases.length === 1);
      const afterDrop = await quiet.events();

      // eslint-disable-next-line no-console
      console.log(
        `[lease cost] one hold: ${afterHold - beforeHold} events; one drop: ${afterDrop - afterHold}`
      );
      expect(afterHold - beforeHold).toBe(6);
      expect(afterDrop - afterHold).toBe(6);

      // Both **Lease** workflows are drained rather than abandoned. The ticking one's holder is reported ALIVE,
      // so its clock alone will never end it — which is the point of the control and would be a
      // hang if this test walked away from it.
      await quiet.drop('run-a#1');
      await quiet.handle.result();
      await ticking.drop('ticker#1');
      await ticking.handle.result();
    });
  }, 180_000);
});

// ═════════════════════════════════════════════════════════════════════════════════════════════════
// THE BUNDLES
// ═════════════════════════════════════════════════════════════════════════════════════════════════

describe('the workflow bundles', () => {
  it('both export the Lease workflow, and the appliance exports the REAL one', () => {
    // A type nobody registered does not fail a `fleet.up()`, it HANGS one: the worker takes the
    // task, finds no such type, fails the task, and Temporal retries for ever. That is the whole
    // reason `workflows/appliance.ts` registers `stackWorkflow` as a refusal — and the reason the
    // **Lease** workflow must NOT be a refusal there: it holds no credential and converges nothing.
    expect(typeof (bundleModule as Record<string, unknown>).fleetLeaseWorkflow).toBe('function');
    expect(typeof (applianceModule as Record<string, unknown>).fleetLeaseWorkflow).toBe('function');
    expect(applianceModule.fleetLeaseWorkflow).toBe(bundleModule.fleetLeaseWorkflow);
    // …while the provisioner beside it is NOT the same function in the two bundles.
    expect(applianceModule.stackWorkflow).not.toBe(bundleModule.stackWorkflow);
  });

  it('the appliance bundle still BUNDLES', async () => {
    /**
     * IMPORTING A MODULE IN NODE IS NOT THE SAME AS BUNDLING IT FOR THE SANDBOX, and the difference
     * is the hazard `workflows/appliance.ts` names in its own header: "a non-bundleable import does
     * not error, it makes bundling HANG". `workflows/lease.ts` reaches `../activities/lease` — a
     * module that imports `@temporalio/client` and `queues.ts` — and it is safe only because that
     * import is `import type`, erased before the bundler sees it. The test above passes either way.
     * This one does not.
     */
    const applianceBundle = await bundleWorkflowCode({
      workflowsPath: path.join(__dirname, 'appliance.ts'),
    });
    expect(applianceBundle.code.length).toBeGreaterThan(1000);
    // The client half must not have been dragged in with the types. `Connection.connect` is the
    // call `activities/lease.ts` makes and there is no `process` in a workflow sandbox to make it
    // with; finding it here would mean the erasure did not happen.
    expect(applianceBundle.code).not.toContain('KONTRA_ADDRESS');
  }, 300_000);
});

/**
 * THE HANDOVER (kontra#10).
 *
 * The header of `workflows/lease.ts` used to end "…which is why there is no continue-as-new here".
 * That was right about the RATE and silent about the TOTAL: Temporal TERMINATES an execution at
 * 51,200 events, so a **Fleet** held across a quarter walks into a hard stop and what dies is the
 * thing that knows the **Machines** must be destroyed.
 *
 * THE THRESHOLD IS LOWERED RATHER THAN THE HISTORY INFLATED. Reaching the real 4,096 would take
 * ~680 hold/drop pairs at ~6 events each — minutes of test, for no extra confidence. Moving the
 * server's own `suggestContinueAsNew` limit exercises the SAME signal on the SAME path: the server
 * sets `continueAsNewSuggested` on a workflow task and the workflow reads it. What is asserted is
 * the workflow's behaviour, not the number.
 */
describe('continue-as-new', () => {
  let small: TestWorkflowEnvironment;

  beforeAll(async () => {
    small = await TestWorkflowEnvironment.createLocal({
      server: {
        extraArgs: [
          '--dynamic-config-value',
          'limit.historyCount.suggestContinueAsNew=40',
        ],
      },
    });
  }, 300_000);

  afterAll(async () => {
    await small?.teardown();
  });

  it('hands over when the server suggests it, and the Leases survive the boundary', async () => {
    const fqn = nextFqn();
    const queue = nextQueue();
    // THE FILE'S OWN ACTIVITY SET, against the lowered-threshold server. A hand-rolled one here
    // registered two activities and the teardown path calls `checkCloudCredential` as well — the
    // workflow failed on a missing activity rather than on anything this test is about.
    const rec = fresh();
    const w = await Worker.create({
      connection: small.nativeConnection,
      taskQueue: queue,
      workflowBundle: bundle,
      activities: activities(rec),
    });

    await w.runUntil(async () => {
      const handle = await small.client.workflow.signalWithStart(LEASE_WORKFLOW, {
        taskQueue: queue,
        workflowId: `kontra-lease/${fqn}`,
        args: [{ stackFqn: fqn, livenessQueue: queue }],
        signal: LEASE_HOLD_SIGNAL,
        signalArgs: [{ lease: 'run-a#1', holder: '', ttlMs: 60_000, credential: 'do-token' }],
      });
      const first = handle.firstExecutionRunId;

      // Signals are ~6 events each, so this crosses 40 several times over and the workflow has to
      // hand over more than once. A chain of TWO would pass a test that only looked at one.
      for (let i = 0; i < 24; i++) {
        await handle.signal(LEASE_HOLD_SIGNAL, { lease: `filler#${i}`, holder: '', ttlMs: 60_000 });
        await handle.signal(LEASE_DROP_SIGNAL, { lease: `filler#${i}` });
      }

      const desc = await handle.describe();
      // THE CHAIN MOVED. `runId` is the CURRENT execution; `firstExecutionRunId` is where the chain
      // began. They differ only if at least one continue-as-new happened.
      expect(desc.runId, 'the workflow never handed over').not.toBe(first);

      // THE LEDGER CROSSED THE BOUNDARY, which is the whole risk: a handover that lost its Leases
      // would destroy a Fleet under a live holder.
      const held = (await handle.query(LEASE_QUERY)) as FleetLeaseSet;
      expect(held.leases.map((l) => l.lease)).toEqual(['run-a#1']);

      // …AND SO DID `everHeld`. Without it the new leg would drop the last Lease and decline to
      // destroy, leaving Machines billing with nobody watching.
      await handle.signal(LEASE_DROP_SIGNAL, { lease: 'run-a#1' });
      await handle.result();
      // `${fqn}|${credential}` — so this asserts TWO carried fields at once: `everHeld` (without it
      // there is no destroy at all) and `credential` (without it the teardown runs against an empty
      // credential name, which on a real Fleet is Machines left billing and reaching hostile
      // infrastructure because the provider call had nothing to authenticate with).
      expect(rec.destroys, 'the continued leg lost everHeld or the credential').toEqual([
        `${fqn}|do-token`,
      ]);
    });
  }, 300_000);
});
