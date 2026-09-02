import { afterEach, describe, expect, it, vi } from 'vitest';
import { CANCEL_REPORT_MS, STOP_GRACE_MS, clampGrace, stopRun } from './workflowControl';

/**
 * Stopping a run, and the one thing that makes cancel and terminate different verbs.
 *
 * CANCEL is cooperative: Temporal delivers a cancellation, the workflow's `async with` scopes run
 * their exits, and `fleet.up`'s exit DESTROYS the Machines. TERMINATE is unilateral: the workflow
 * closes where it stands, no code in it runs again, and whatever it provisioned keeps billing.
 *
 * So the whole design is that the forceful-sounding verb has to be the patient one. `terminate`
 * cancels first and escalates only if that does not land — because otherwise the ordinary way to
 * stop a run would be the way that strands infrastructure, and an operator reaching for the
 * stronger word would be choosing the more expensive outcome without being told.
 *
 * Temporal is faked here through `getClient`. What is under test is the DECISION — which of the two
 * happened, after how long, and what the operator is told — and a decision proven against a live
 * cluster is one proven against whatever that cluster happened to be doing.
 */

interface FakeHandle {
  describe(): Promise<{ status: { name: string } }>;
  cancel(): Promise<void>;
  terminate(reason?: string): Promise<void>;
}

interface Recorded {
  cancels: number;
  terminates: string[];
  describes: number;
}

/**
 * A run whose status changes after N describes.
 *
 * `settleAfter: Infinity` is a workflow blocked in an activity with no heartbeat — it cannot act on
 * a cancellation until that activity returns, which is the case `terminate` exists for and the case
 * `cancel` has to report honestly rather than claim.
 */
function fakeRun(opts: { status?: string; settleAfter?: number; settledAs?: string } = {}) {
  const rec: Recorded = { cancels: 0, terminates: [], describes: 0 };
  let cancelled = false;
  const handle: FakeHandle = {
    async describe() {
      rec.describes += 1;
      if (opts.status && opts.status !== 'RUNNING') return { status: { name: opts.status } };
      const settleAfter = opts.settleAfter ?? Number.POSITIVE_INFINITY;
      const settled = cancelled && rec.describes > settleAfter;
      return { status: { name: settled ? (opts.settledAs ?? 'CANCELED') : 'RUNNING' } };
    },
    async cancel() {
      rec.cancels += 1;
      cancelled = true;
    },
    async terminate(reason?: string) {
      rec.terminates.push(reason ?? '');
    },
  };
  vi.doMock('./temporalClient', () => ({
    getClient: async () => ({ workflow: { getHandle: () => handle } }),
  }));
  return rec;
}

/** No real waiting: the grace period is a decision, not a duration to sit through. */
function clock() {
  let t = 0;
  return {
    now: () => t,
    sleep: async (ms: number) => {
      t += ms;
    },
  };
}

async function freshStopRun(): Promise<typeof stopRun> {
  vi.resetModules();
  return (await import('./workflowControl')).stopRun;
}

afterEach(() => {
  vi.doUnmock('./temporalClient');
  vi.resetModules();
});

describe('cancel is the verb that tears a fleet down', () => {
  it('cancels and reports the teardown when the run closes', async () => {
    const rec = fakeRun({ settleAfter: 2 });
    const run = await freshStopRun();
    const got = await run('nscheck-1', { escalate: false, graceMs: CANCEL_REPORT_MS, ...clock() });

    expect(rec.cancels).toBe(1);
    expect(rec.terminates).toEqual([]);
    expect(got.outcome).toBe('cancelled');
    // The sentence has to say what CHANGED in the world, not what API call was made.
    expect(got.detail).toMatch(/fleet it held has been destroyed/);
  });

  it('NEVER terminates, however long the cancel takes', async () => {
    // The property that makes `cancel` a verb an operator can reach for without reading the docs:
    // it cannot escalate behind their back.
    const rec = fakeRun({ settleAfter: Number.POSITIVE_INFINITY });
    const run = await freshStopRun();
    const got = await run('nscheck-1', { escalate: false, graceMs: CANCEL_REPORT_MS, ...clock() });

    expect(rec.terminates).toEqual([]);
    expect(got.outcome).toBe('cancelling');
  });

  it('reports a cancel still in flight as `cancelling`, not as a failure', async () => {
    // A workflow blocked in an activity cannot act on a cancellation until that activity returns —
    // an ordinary state, and the request is durable. Calling it `cancelled` would claim a teardown
    // that has not happened; calling it a failure would send somebody to terminate a run that is in
    // the middle of cleaning up properly.
    fakeRun({ settleAfter: Number.POSITIVE_INFINITY });
    const run = await freshStopRun();
    const got = await run('nscheck-1', { escalate: false, graceMs: CANCEL_REPORT_MS, ...clock() });

    expect(got.outcome).toBe('cancelling');
    expect(got.detail).toMatch(/durable/);
    expect(got.detail).toMatch(/terminate/);
  });
});

describe('terminate cancels first', () => {
  it('does not terminate a run that settles inside the grace period', async () => {
    // THE WHOLE POINT. Asking to terminate a run that can clean up after itself gets the cleanup.
    const rec = fakeRun({ settleAfter: 3 });
    const run = await freshStopRun();
    const got = await run('nscheck-1', { escalate: true, graceMs: STOP_GRACE_MS, ...clock() });

    expect(rec.cancels).toBe(1);
    expect(rec.terminates).toEqual([]);
    expect(got.outcome).toBe('cancelled');
  });

  it('escalates when the cancel does not land, and SAYS what that costs', async () => {
    const rec = fakeRun({ settleAfter: Number.POSITIVE_INFINITY });
    const run = await freshStopRun();
    const got = await run('nscheck-1', { escalate: true, graceMs: 10_000, ...clock() });

    expect(rec.cancels).toBe(1);
    expect(rec.terminates).toHaveLength(1);
    expect(got.outcome).toBe('terminated');
    expect(got.waitedMs).toBeGreaterThanOrEqual(10_000);
    // The consequence, in the words that matter: Machines, still up, and where to look.
    expect(got.detail).toMatch(/scope exits did NOT run/);
    expect(got.detail).toMatch(/Machines are still up/);
    expect(got.detail).toMatch(/kontra fleet status/);
  });

  it('--force skips the cancel entirely, and says so differently', async () => {
    // For a run whose worker is gone: it cannot process a cancellation at all, and waiting a minute
    // to learn that is a minute spent watching nothing happen.
    const rec = fakeRun({ settleAfter: Number.POSITIVE_INFINITY });
    const run = await freshStopRun();
    const got = await run('nscheck-1', { escalate: true, force: true, ...clock() });

    expect(rec.cancels).toBe(0);
    expect(rec.terminates).toHaveLength(1);
    expect(got.outcome).toBe('terminated');
    expect(got.detail).toMatch(/without asking it to cancel/);
  });
});

describe('a run that is not running', () => {
  it('says it was already closed rather than cancelling nothing', async () => {
    const rec = fakeRun({ status: 'COMPLETED' });
    const run = await freshStopRun();
    const got = await run('nscheck-1', { escalate: true, ...clock() });

    expect(got.outcome).toBe('already-closed');
    expect(rec.cancels).toBe(0);
    expect(rec.terminates).toEqual([]);
    expect(got.detail).toMatch(/already completed/);
  });

  it('refuses a run id that is not one, before dialling', async () => {
    fakeRun();
    const run = await freshStopRun();
    await expect(run('not a run id', clock())).rejects.toThrow(/is not a run id/);
  });
});

describe('the grace period is bounded at both ends', () => {
  it('refuses zero, which would be a terminate wearing the word terminate', () => {
    expect(clampGrace(0)).toBeGreaterThanOrEqual(5_000);
  });

  it('refuses unbounded, which would be an HTTP request that never returns', () => {
    expect(clampGrace(Number.POSITIVE_INFINITY)).toBe(STOP_GRACE_MS);
    expect(clampGrace(10_000_000)).toBeLessThanOrEqual(300_000);
  });

  it('defaults when the caller said nothing', () => {
    expect(clampGrace(undefined)).toBe(STOP_GRACE_MS);
    expect(clampGrace(Number.NaN)).toBe(STOP_GRACE_MS);
  });
});
