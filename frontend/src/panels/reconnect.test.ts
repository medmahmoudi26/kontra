import { describe, expect, it } from 'vitest';

import {
  disconnectedNotice,
  RECONNECT_BASE_MS,
  RECONNECT_MAX_MS,
  reconnectDelay,
  resubscribePlan,
} from './reconnect';

/**
 * Slice 05, acceptance criterion 3: "Reconnecting after a dropped socket does not silently lose or
 * duplicate output." These are the pure halves that make that true — the backoff schedule and the
 * re-subscribe delta. The socket wiring that calls them lives in `DashboardPage` and `useSinglePane`,
 * where it is proven by the Playwright suite; a `subscribe` that arrives once here is what keeps that
 * true across a reconnect.
 */

describe('the reconnect backoff', () => {
  it('starts at the base delay and doubles', () => {
    expect(reconnectDelay(0)).toBe(RECONNECT_BASE_MS);
    expect(reconnectDelay(1)).toBe(RECONNECT_BASE_MS * 2);
    expect(reconnectDelay(2)).toBe(RECONNECT_BASE_MS * 4);
    expect(reconnectDelay(3)).toBe(RECONNECT_BASE_MS * 8);
  });

  it('caps, so a streamer that is down for an hour is retried, not spun on', () => {
    expect(reconnectDelay(50)).toBe(RECONNECT_MAX_MS);
    // A large attempt count must not overflow past the cap.
    expect(reconnectDelay(1000)).toBe(RECONNECT_MAX_MS);
    expect(reconnectDelay(Number.MAX_SAFE_INTEGER)).toBe(RECONNECT_MAX_MS);
  });

  it('treats a negative or fractional attempt as the first', () => {
    expect(reconnectDelay(-5)).toBe(RECONNECT_BASE_MS);
    expect(reconnectDelay(0.9)).toBe(RECONNECT_BASE_MS);
  });
});

describe('the re-subscribe delta does not lose or duplicate', () => {
  it('subscribes every wanted Terminal exactly once on a fresh socket', () => {
    // A reconnect: the new socket has told the streamer about nothing yet.
    const wanted = ['local:h/s1/w', 'local:h/s2/w', 'fleet:kf-a/kontra-x/actor'];
    const plan = resubscribePlan(wanted, new Set());
    // NO LOSS — every wanted id is asked for.
    expect(plan.subscribe.sort()).toEqual([...wanted].sort());
    // NO DUPLICATION — each appears once.
    expect(new Set(plan.subscribe).size).toBe(plan.subscribe.length);
    expect(plan.unsubscribe).toEqual([]);
  });

  it('is a no-op when the socket already holds exactly the wanted set', () => {
    const wanted = ['a', 'b', 'c'];
    const plan = resubscribePlan(wanted, new Set(wanted));
    expect(plan).toEqual({ subscribe: [], unsubscribe: [] });
  });

  it('subscribes only the newcomers on a live socket', () => {
    const plan = resubscribePlan(['a', 'b', 'c'], new Set(['a', 'b']));
    expect(plan.subscribe).toEqual(['c']);
    expect(plan.unsubscribe).toEqual([]);
  });

  it('unsubscribes the Terminals that left the wall, and nothing wanted', () => {
    const plan = resubscribePlan(['a', 'c'], new Set(['a', 'b', 'c', 'd']));
    expect(plan.subscribe).toEqual([]);
    expect(plan.unsubscribe.sort()).toEqual(['b', 'd']);
  });

  it('folds a duplicate in the wanted list to a single subscribe', () => {
    // The inventory should not carry duplicate ids, but a plan that would subscribe twice is a
    // re-`stty`'d PTY attach, so the guard is load-bearing rather than paranoid.
    const plan = resubscribePlan(['a', 'a', 'b'], new Set());
    expect(plan.subscribe).toEqual(['a', 'b']);
  });

  it('never produces a third, byte-bearing kind — a reconnect is read-only', () => {
    // Criterion 5 across a reconnect: the plan's whole vocabulary is subscribe/unsubscribe. There is
    // no field a keystroke could ride in.
    const plan = resubscribePlan(['a', 'b'], new Set(['b', 'c']));
    expect(Object.keys(plan).sort()).toEqual(['subscribe', 'unsubscribe']);
  });
});

describe('a dropped socket says so', () => {
  it('names the wait and warns the screen is frozen', () => {
    const notice = disconnectedNotice(0, 1000);
    expect(notice).toContain('reconnecting');
    expect(notice).toContain('1s');
    expect(notice).toContain('frozen');
  });

  it('counts attempts from one for a human', () => {
    expect(disconnectedNotice(2, 4000)).toContain('attempt 3');
    expect(disconnectedNotice(2, 4000)).toContain('4s');
  });
});
