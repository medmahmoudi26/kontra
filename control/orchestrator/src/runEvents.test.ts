/**
 * The run event bus (ADR 0062).
 *
 * The assertion that matters most is the guarded dispatch. `emitData` is called from inside
 * `publishBatch` immediately after `COMMIT`, so a renderer that throws on a listener would propagate
 * into the publish path and fail an activity whose rows are already durable — turning a cosmetic
 * report bug into a retried batch and a double insert.
 */

import { describe, expect, it, vi } from 'vitest';

import { observesCommits, RunEventBus, type RunDataEvent } from './runEvents';

const EVENT: RunDataEvent = {
  runId: 'enrich-1791234567',
  runStartedAt: 1_791_234_567_000,
  dataset: 'products',
  rows: 4_120,
};

describe('a listener hears what was emitted', () => {
  it('delivers a data event whole, including the execution it belongs to', () => {
    const bus = new RunEventBus();
    const seen: RunDataEvent[] = [];
    bus.onData((e) => seen.push(e));
    bus.emitData(EVENT);
    expect(seen).toEqual([EVENT]);
  });

  it('delivers status and progress on their own channels, not to each other', () => {
    const bus = new RunEventBus();
    const data = vi.fn();
    const status = vi.fn();
    const progress = vi.fn();
    bus.onData(data);
    bus.onStatus(status);
    bus.onProgress(progress);

    bus.emitStatus({ runId: 'r', runStartedAt: 1, status: 'completed' });
    expect(status).toHaveBeenCalledTimes(1);
    expect(data).not.toHaveBeenCalled();
    expect(progress).not.toHaveBeenCalled();
  });

  it('fans one emit out to every listener, which is what makes N viewers cost one render', () => {
    const bus = new RunEventBus();
    const calls: number[] = [];
    for (let i = 0; i < 10; i++) bus.onData(() => calls.push(i));
    bus.emitData(EVENT);
    expect(calls).toHaveLength(10);
  });
});

describe('a throwing listener cannot reach the publish path', () => {
  /** THE ONE THAT PROTECTS A COMMIT. `emitData` runs on the far side of `conn.run('COMMIT')`. */
  it('swallows the throw and reports it instead of propagating', () => {
    const onError = vi.fn();
    const bus = new RunEventBus({ onError });
    bus.onData(() => {
      throw new Error('render blew up');
    });

    expect(() => bus.emitData(EVENT)).not.toThrow();
    expect(onError).toHaveBeenCalledTimes(1);
    expect((onError.mock.calls[0]![0] as Error).message).toBe('render blew up');
  });

  it('still delivers to the listeners after the one that threw', () => {
    const bus = new RunEventBus();
    const after = vi.fn();
    bus.onData(() => {
      throw new Error('first');
    });
    bus.onData(after);
    bus.emitData(EVENT);
    expect(after).toHaveBeenCalledTimes(1);
  });
});

describe('unsubscribing detaches', () => {
  /** A stream that forgets this leaks one listener per viewer that ever connected. */
  it('stops delivering, and the count returns to where it started', () => {
    const bus = new RunEventBus();
    expect(bus.listenerCount()).toBe(0);
    const seen = vi.fn();
    const off = bus.onData(seen);
    expect(bus.listenerCount()).toBe(1);

    bus.emitData(EVENT);
    off();
    bus.emitData(EVENT);

    expect(seen).toHaveBeenCalledTimes(1);
    expect(bus.listenerCount()).toBe(0);
  });

  it('is idempotent, so a double disconnect is not an error', () => {
    const bus = new RunEventBus();
    const off = bus.onData(vi.fn());
    off();
    expect(() => off()).not.toThrow();
    expect(bus.listenerCount()).toBe(0);
  });
});

describe('observesCommits says whether this process can see a commit at all', () => {
  /** The shipped default: `KONTRA_ROLES` unset means all three roles in one PID. */
  it('is true for the default role set', () => {
    expect(observesCommits(['api', 'materializer', 'worker'])).toBe(true);
  });

  /**
   * `KONTRA_ROLES=api` serves reports and never sees a commit, so no `data` event can arrive. The
   * live route reads this to answer DEGRADED rather than to stream a document frozen for no stated
   * reason.
   */
  it('is false for an api-only process, which is what makes the route honest', () => {
    expect(observesCommits(['api'])).toBe(false);
  });

  it('is true for a materializer-only process, which does see commits', () => {
    expect(observesCommits(['materializer'])).toBe(true);
  });
});
