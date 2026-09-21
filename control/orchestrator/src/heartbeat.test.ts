import { describe, expect, it } from 'vitest';
import { heartbeatRow } from './heartbeat';

/**
 * Regression cover for the heartbeat row build.
 *
 * The bug this pins: `isolated` travelled the whole way from the handler
 * (runtime/handler/activity.go:135 puts it in the heartbeat payload) to the CLI
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

/**
 * The SAME bug, one field over, found by an operator asking a question the API could not answer:
 * "which program is this worker on, which host, and how much is left?"
 *
 * `@actor.healthcheck`'s map is the only part of a beat that describes the WORK rather than the
 * machine — for the desync scanner `{program, at, found, hosts, probes, withdrawn, erratic}`. The
 * actor beat it every two seconds, Temporal served it on the pending activity, and `heartbeatRow`
 * built its row out of `done/total/isolated` and dropped the rest on the floor. So a console could
 * render `0/6` and nothing else, and a 27-minute verify run against visa was indistinguishable
 * from a wedged worker.
 *
 * Pinned as PASS-THROUGH, not as a shape: the keys belong to the actor author. A typed subset here
 * is what would drop the next actor's fields exactly the way this dropped desync's.
 */
describe('heartbeatRow: the author healthcheck map', () => {
  it('passes the progress map through verbatim', () => {
    const progress = {
      program: 'visa',
      at: 'www.visamiddleeast.com/',
      found: 3,
      hosts: 394,
      probes: 1720,
      withdrawn: 1,
      egress: '172.20.0.17',
    };
    const row = heartbeatRow({ node: 'n1', done: 2, total: 6, isolated: 0, progress }, 'wf-x', 1, 9);
    expect(row.progress).toEqual(progress);
    // Untouched, not rebuilt: a copy that re-listed keys would be the same bug again.
    expect(row.progress).not.toBe(undefined);
    expect(Object.keys(row.progress ?? {})).toHaveLength(7);
  });

  it('omits the key entirely for an actor that defines no healthcheck', () => {
    // ABSENT and EMPTY are different facts. `{}` says the healthcheck ran and had nothing to say;
    // absence says this actor has none. A panel that conflates them shows a blank progress row
    // for a perfectly healthy worker.
    const row = heartbeatRow({ node: 'n1', done: 1, total: 1, isolated: 0 }, 'wf-x', 1, 9);
    expect('progress' in row).toBe(false);
  });
});
