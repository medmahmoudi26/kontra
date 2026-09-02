import { describe, expect, it } from 'vitest';
import { heartbeatRow } from './heartbeat';

/**
 * Regression cover for the heartbeat row build.
 *
 * The bug this pins: `isolated` travelled the whole way from the handler
 * (handler/activity.go:135 puts it in the heartbeat payload) to the CLI
 * (cli/monitor.go:57 declares it, isolatedCol renders "N !" for it) — and the orchestrator
 * dropped it in the middle, because HeartbeatDetail simply did not declare the field. Every
 * live node therefore reported ZERO isolated units no matter how many it had actually thrown
 * away, and the "!" that marks a run as having lost work could never fire.
 *
 * That is the precise failure mode the counter exists to prevent: a run that discarded its
 * whole batch and a run that discarded nothing looked identical. A silently-zeroed loss
 * counter is worse than an absent one — it reads as a clean bill of health.
 */
describe('heartbeatRow', () => {
  it('carries isolated through from the wire', () => {
    const row = heartbeatRow({ node: 'n1', done: 5, total: 20, isolated: 7 }, 'wf-x', 2, 1_700_000);
    expect(row).toEqual({
      node: 'n1',
      done: 5,
      total: 20,
      isolated: 7,
      attempt: 2,
      lastBeat: 1_700_000,
    });
  });

  it('reports a genuine zero as zero', () => {
    expect(heartbeatRow({ node: 'n1', done: 3, total: 3, isolated: 0 }, 'wf-x', 1, 5).isolated).toBe(
      0
    );
  });

  it('defaults a wire without the field to zero rather than undefined', () => {
    // An older handler predating the field. The caller-side discriminator for "no data" is the
    // ABSENCE of a heartbeat entry for the node (cli/monitor.go isolatedCol's `ok`), not a
    // sentinel inside a present one — so a present row must always carry a number.
    expect(heartbeatRow({ node: 'n1', done: 1, total: 2 }, 'wf-x', 1, 5).isolated).toBe(0);
  });

  it('falls back to the workflow id when the detail carries no node', () => {
    expect(heartbeatRow({ done: 1, total: 2 }, 'wf-fallback', 0, 0).node).toBe('wf-fallback');
  });
});
