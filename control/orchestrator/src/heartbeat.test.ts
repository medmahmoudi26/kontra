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

/**
 * THE CHECKPOINT IS THE DURABLE HALF OF A BEAT, and these are the rows built from it.
 *
 * `done: 2` does not say WHICH two, so nothing can resume from the counters. The checkpoint says
 * which, in the encoding every SDK shares (`shared/conformance/checkpoint.json`), and it rides in
 * the activity's own history rather than in a cache that can evict it (ADR 0059).
 */
describe('a row built from a checkpoint', () => {
  const beat = (checkpoint: unknown, rest: Record<string, unknown> = {}) =>
    heartbeatRow({ node: 'n7', total: 10, ...rest, checkpoint }, 'fallback', 1, 1234);

  it('counts the set rather than the counter the actor sent', () => {
    // The counters are derived twice — once by the actor for display, once from the set a retry
    // resumes from. Where they disagree it is the counters that are wrong.
    const row = beat(
      { v: 1, batch_id: 'b1', done: [[0, 6]], failed: [7, 8] },
      { done: 999, isolated: 999 }
    );
    expect(row.done).toBe(7);
    expect(row.isolated).toBe(2);
  });

  it('falls back to the counters when there is no checkpoint', () => {
    // An older SDK beats without one, and that is a normal actor rather than a broken one.
    const row = heartbeatRow({ node: 'n7', done: 4, total: 10, isolated: 1 }, 'fallback', 1, 1234);
    expect(row.done).toBe(4);
    expect(row.isolated).toBe(1);
  });

  it('falls back when the checkpoint is a version it cannot read', () => {
    // Refused rather than partly believed — and a refusal must not zero the row, or an operator
    // sees a node that has done nothing when it has done everything.
    const row = beat({ v: 2, batch_id: 'b1', done: [[0, 6]], failed: [] }, { done: 7, isolated: 0 });
    expect(row.done).toBe(7);
  });

  it('reports a node that isolated everything as isolated, not as idle', () => {
    // A node that dropped every Unit and a node that legitimately found nothing render identically
    // without this — which is the reason `isolated` exists at all.
    const row = beat({ v: 1, batch_id: 'b1', done: [], failed: [0, 1, 2] }, { total: 3 });
    expect(row.done).toBe(0);
    expect(row.isolated).toBe(3);
    expect(row.total).toBe(3);
  });

  it('keeps the total from the beat, because a checkpoint does not carry one', () => {
    // A checkpoint says what finished, never how many there were. Deriving the total from it would
    // make every node look complete the moment it committed its last known Unit.
    const row = beat({ v: 1, batch_id: 'b1', done: [[0, 1]], failed: [] }, { total: 50 });
    expect(row.total).toBe(50);
    expect(row.done).toBe(2);
  });
});
