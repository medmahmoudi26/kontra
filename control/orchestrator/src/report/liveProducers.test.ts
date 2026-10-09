import { describe, expect, it } from 'vitest';

import { buildContext } from './context';
import { IN_FLIGHT, inFlightSummary, progressFromHeartbeats } from './liveProducers';

/**
 * The producers ADR 0062 shipped without. Each half of live mode was tested against a fixture of the
 * other, and nothing tested the seam, so a context that could not change rendered green. The last
 * test here goes through `buildContext`, which is the seam.
 */

const beat = (node: string, done: number, total: number, isolated: number, lastBeat: number) => ({
  node,
  done,
  total,
  isolated,
  attempt: 1,
  lastBeat,
});

describe('progressFromHeartbeats', () => {
  it('sums the running batches, and counts an isolated unit as finished', () => {
    const p = progressFromHeartbeats({
      a: beat('a', 3, 10, 1, 1_000),
      b: beat('b', 2, 5, 0, 2_000),
    });
    expect(p).toEqual({
      units_done: 6,
      units_total: 15,
      isolated: 1,
      phase: '2 batches running',
      updated_at: new Date(2_000).toISOString(),
    });
  });

  it('is undefined when nothing is running, so a template sees run.progress as null', () => {
    expect(progressFromHeartbeats({})).toBeUndefined();
  });
});

describe('inFlightSummary', () => {
  it('unwraps the array a unit blob stores, so a table gets the record\'s own columns', () => {
    const s = inFlightSummary({
      rows: 2,
      lastChunkAt: 5_000,
      recent: [
        [{ target: 'alpha', step: 1, phase: 'resolve' }],
        [{ target: 'alpha', step: 2, phase: 'connect' }],
      ],
    });
    expect(s.rows).toBe(2);
    expect(s.batches).toBe(0);
    expect(s.last_commit_at).toBe(new Date(5_000).toISOString());
    expect(s.head).toEqual([
      { target: 'alpha', step: 1, phase: 'resolve' },
      { target: 'alpha', step: 2, phase: 'connect' },
    ]);
  });
});

describe('the seam: produced values reach the context a template reads', () => {
  it('an open run carries progress, the in-flight rows with their columns, and a moving clock', () => {
    const ctx = buildContext({
      runId: 'canary-1',
      status: 'running',
      startedAt: 10_000,
      closedAt: 0,
      version: 1,
      now: 25_000,
      progress: progressFromHeartbeats({ a: beat('a', 1, 2, 0, 24_000) }),
      datasets: {
        [IN_FLIGHT]: inFlightSummary({
          rows: 3,
          lastChunkAt: 24_500,
          recent: [[{ target: 'alpha', step: 3, phase: 'handshake' }]],
        }),
      },
    } as never);
    const run = ctx.run as { progress: { units_done: number; units_total: number } | null; duration_s: number };
    expect(run.progress).toMatchObject({ units_done: 1, units_total: 2 });
    expect(run.duration_s).toBeGreaterThan(0);
    expect(ctx.datasets[IN_FLIGHT]).toMatchObject({ rows: 3, columns: ['target', 'step', 'phase'] });
  });
});
