/**
 * The two bounds on `POST /api/login`, and the one assertion that matters most: a refusal does no
 * scrypt.
 *
 * A limiter that rejects AFTER running the expensive thing protects the password and nothing else —
 * the threadpool is already spent. Several tests below drive the guard with a thunk that records
 * whether it was called, because "did the work happen" is the property, not "what was returned".
 */

import { describe, expect, it } from 'vitest';

import { DEFAULTS, LoginGuard } from './loginGuard';

/** A clock a test can move. The guard's windows are in minutes; a test cannot wait one. */
function clock(start = 1_000_000) {
  let t = start;
  return { now: () => t, advance: (ms: number) => (t += ms) };
}

/** A thunk that records its calls, so "no scrypt ran" is assertable. */
function spy<T>(value: T) {
  let calls = 0;
  return {
    get calls() {
      return calls;
    },
    work: async () => {
      calls += 1;
      return value;
    },
  };
}

describe('the attempt budget', () => {
  it('allows exactly the configured number, then refuses', async () => {
    const c = clock();
    const guard = new LoginGuard({ attempts: 3, now: c.now });
    const s = spy('ok');

    for (let i = 0; i < 3; i++) {
      const out = await guard.attempt('1.2.3.4', s.work);
      expect(out, `attempt ${i + 1} should be allowed`).toEqual({ value: 'ok' });
    }
    const refused = await guard.attempt('1.2.3.4', s.work);
    expect(refused).toMatchObject({ reason: 'too-many-attempts' });

    // THE POINT: the fourth attempt did not run the verification.
    expect(s.calls).toBe(3);
  });

  it('names a retry-after that is within the window and never zero', async () => {
    const c = clock();
    const guard = new LoginGuard({ attempts: 1, windowMs: 60_000, now: c.now });
    await guard.attempt('1.2.3.4', async () => 'ok');

    const refused = await guard.attempt('1.2.3.4', async () => 'ok');
    if (!('reason' in refused)) throw new Error('expected a refusal');
    expect(refused.retryAfterSeconds).toBeGreaterThan(0);
    expect(refused.retryAfterSeconds).toBeLessThanOrEqual(60);

    // A retry-after of 0 would tell a client to come back immediately, which is a busy loop rather
    // than a limit. Taken at the very end of the window, where the rounding is most likely to
    // produce one.
    c.advance(59_999);
    const late = await guard.attempt('1.2.3.4', async () => 'ok');
    if (!('reason' in late)) throw new Error('expected a refusal at the window edge');
    expect(late.retryAfterSeconds).toBeGreaterThanOrEqual(1);
  });

  it('forgets the window once it expires', async () => {
    const c = clock();
    const guard = new LoginGuard({ attempts: 1, windowMs: 60_000, now: c.now });
    await guard.attempt('1.2.3.4', async () => 'ok');
    expect(await guard.attempt('1.2.3.4', async () => 'ok')).toMatchObject({ reason: 'too-many-attempts' });

    c.advance(60_000);
    expect(await guard.attempt('1.2.3.4', async () => 'ok')).toEqual({ value: 'ok' });
  });

  it('budgets each source separately, so one address cannot lock out another', async () => {
    const c = clock();
    const guard = new LoginGuard({ attempts: 1, now: c.now });
    await guard.attempt('1.2.3.4', async () => 'ok');
    expect(await guard.attempt('1.2.3.4', async () => 'ok')).toMatchObject({ reason: 'too-many-attempts' });

    // The operator, on a different address, is unaffected.
    expect(await guard.attempt('127.0.0.1', async () => 'ok')).toEqual({ value: 'ok' });
  });

  it('forgives a source on success', async () => {
    const c = clock();
    const guard = new LoginGuard({ attempts: 2, now: c.now });
    await guard.attempt('1.2.3.4', async () => 'ok');
    await guard.attempt('1.2.3.4', async () => 'ok');
    expect(await guard.attempt('1.2.3.4', async () => 'ok')).toMatchObject({ reason: 'too-many-attempts' });

    guard.forgive('1.2.3.4');
    expect(await guard.attempt('1.2.3.4', async () => 'ok')).toEqual({ value: 'ok' });
  });

  it('counts attempts on the way in, not only failures', async () => {
    // An attacker's every guess is wrong, so a budget spent only on failure is a budget spent only
    // when the attacker chooses. Successes count too — which is why `forgive` exists.
    const c = clock();
    const guard = new LoginGuard({ attempts: 2, now: c.now });
    expect(await guard.attempt('1.2.3.4', async () => 'a-success')).toEqual({ value: 'a-success' });
    expect(await guard.attempt('1.2.3.4', async () => 'a-success')).toEqual({ value: 'a-success' });
    expect(await guard.attempt('1.2.3.4', async () => 'a-success')).toMatchObject({
      reason: 'too-many-attempts',
    });
  });
});

describe('the concurrency gate', () => {
  it('never runs more verifications at once than it is configured for', async () => {
    const guard = new LoginGuard({ concurrency: 2, queueDepth: 100, attempts: 1000 });
    let live = 0;
    let peak = 0;
    let release: () => void = () => {};
    const held = new Promise<void>((r) => (release = r));

    const runs = Array.from({ length: 8 }, () =>
      guard.attempt('1.2.3.4', async () => {
        live += 1;
        peak = Math.max(peak, live);
        await held;
        live -= 1;
        return 'ok';
      })
    );

    // Let every request reach the gate before anything is allowed to finish.
    await new Promise((r) => setTimeout(r, 50));
    expect(peak).toBe(2);
    release();
    await Promise.all(runs);
    expect(peak).toBe(2);
  });

  it('sheds rather than queues once the wait is full, and runs nothing when it does', async () => {
    const guard = new LoginGuard({ concurrency: 1, queueDepth: 2, attempts: 1000 });
    let release: () => void = () => {};
    const held = new Promise<void>((r) => (release = r));
    const s = spy('ok');

    // One runs, two wait, and the fourth has nowhere to go.
    const busy = Array.from({ length: 3 }, () =>
      guard.attempt('1.2.3.4', async () => {
        await held;
        return 'ok';
      })
    );
    await new Promise((r) => setTimeout(r, 30));

    const shed = await guard.attempt('1.2.3.4', s.work);
    expect(shed).toMatchObject({ reason: 'busy' });
    expect(s.calls).toBe(0);

    release();
    await Promise.all(busy);
  });

  it('returns a slot when the verification throws', async () => {
    // `verifyPassword` throws on a hash somebody hand-edited into nonsense. Without the `finally`
    // the gate closes permanently after `concurrency` of them — a self-inflicted lockout that no
    // restart-free path recovers from.
    const guard = new LoginGuard({ concurrency: 1, attempts: 1000 });
    await expect(
      guard.attempt('1.2.3.4', async () => {
        throw new Error('malformed hash');
      })
    ).rejects.toThrow('malformed hash');

    expect(guard.snapshot().inFlight).toBe(0);
    expect(await guard.attempt('1.2.3.4', async () => 'ok')).toEqual({ value: 'ok' });
  });

  it('holds no slots once everything has drained', async () => {
    const guard = new LoginGuard({ concurrency: 2, attempts: 1000 });
    await Promise.all(Array.from({ length: 6 }, () => guard.attempt('1.2.3.4', async () => 'ok')));
    expect(guard.snapshot()).toMatchObject({ inFlight: 0, waiting: 0 });
  });
});

describe('bucket bookkeeping', () => {
  it('drops expired sources rather than holding one per address forever', async () => {
    const c = clock();
    const guard = new LoginGuard({ windowMs: 60_000, attempts: 1000, now: c.now });
    for (let i = 0; i < 50; i++) await guard.attempt(`10.0.0.${i}`, async () => 'ok');
    expect(guard.snapshot().sources).toBe(50);

    c.advance(60_000);
    expect(guard.sweep()).toBe(50);
    expect(guard.snapshot().sources).toBe(0);
  });

  it('keeps live sources when it sweeps', async () => {
    const c = clock();
    const guard = new LoginGuard({ windowMs: 60_000, attempts: 1000, now: c.now });
    await guard.attempt('10.0.0.1', async () => 'ok');
    c.advance(60_000);
    await guard.attempt('10.0.0.2', async () => 'ok');

    expect(guard.sweep()).toBe(1);
    expect(guard.snapshot().sources).toBe(1);
  });
});

describe('the defaults are the ones that were argued for', () => {
  /**
   * These are not arbitrary, and a change to them should be a deliberate edit to a test rather than
   * a number nudged in passing.
   *
   * `concurrency` is the load-bearing one: Node's libuv threadpool defaults to FOUR threads and is
   * shared with every `fs` and `dns` call the orchestrator makes. Two leaves two. Raising it to
   * four would make a login flood a storage outage.
   */
  it('leaves at least half the libuv threadpool for the rest of the process', () => {
    const LIBUV_DEFAULT_THREADS = 4;
    expect(DEFAULTS.concurrency).toBeLessThanOrEqual(LIBUV_DEFAULT_THREADS / 2);
    expect(DEFAULTS.concurrency).toBeGreaterThan(0);
  });

  it('allows far more attempts than a person types and far fewer than guessing needs', () => {
    expect(DEFAULTS.attempts).toBeGreaterThanOrEqual(5);
    expect(DEFAULTS.attempts).toBeLessThanOrEqual(20);
    expect(DEFAULTS.windowMs).toBeGreaterThanOrEqual(30_000);
  });
});
