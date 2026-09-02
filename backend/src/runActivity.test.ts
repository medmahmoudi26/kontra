/**
 * Running, parked and stalled — three readings of the one word Temporal has for all of them.
 *
 * `history.ts` states the problem in its own header: a run grinding through Batches, a run whose
 * work is sitting in a retry backoff, and a run whose work went onto a queue nobody polls all
 * report `running` with no new rows. HITL adds a fourth — a run waiting for a person. Every case
 * below is one of those four, asserted against literal evidence with no cluster and no clock.
 */

import { describe, expect, it } from 'vitest';

import {
  UNPOLLED_AFTER_MS,
  inFlightOf,
  readRunActivity,
  type ActivityEvidence,
  type InFlight,
} from './runActivity';

const NOW = 1_786_831_339_000;

function evidence(over: Partial<ActivityEvidence> = {}): ActivityEvidence {
  return { execution: 'running', pendingAsks: 0, inFlight: [], now: NOW, ...over };
}

/** One piece of in-flight work: scheduled, and picked up a second later. */
function healthy(over: Partial<InFlight> = {}): InFlight {
  return {
    attempt: 1,
    scheduledAt: NOW - 5_000,
    startedAt: NOW - 4_000,
    heartbeatAt: 0,
    ...over,
  };
}

describe('a run that is not open has no activity', () => {
  it('reads null on every closed outcome rather than a fourth word for each', () => {
    // A finished run is not running, parked or stalled. Saying one of those about it would be a
    // wrong answer, not a rough one.
    for (const execution of ['completed', 'failed', 'cancelled'] as const) {
      expect(readRunActivity(evidence({ execution }))).toBeNull();
    }
  });

  it('reads null on a run Temporal could not be asked about', () => {
    // `pending` is what `runs.ts` reports when the describe found nothing — retention dropped it,
    // or the cluster could not answer. "We could not look" and "it is fine" are the two readings
    // this codebase keeps furthest apart.
    expect(readRunActivity(evidence({ execution: 'pending' }))).toBeNull();
  });
});

describe('parked', () => {
  it('is what a run with a pending ask reads as', () => {
    expect(readRunActivity(evidence({ pendingAsks: 1 }))).toBe('parked');
  });

  it('beats stalled, so an operator is not sent hunting a dead worker', () => {
    // A parked run IS blocked, so the stall rule would fire on it. The bottleneck is the human,
    // and reporting that as a stall points at the wrong thing entirely.
    const stuck = healthy({ attempt: 4, startedAt: 0 });
    expect(readRunActivity(evidence({ pendingAsks: 2, inFlight: [stuck] }))).toBe('parked');
  });

  it('stops being parked the moment the last ask is answered', () => {
    expect(readRunActivity(evidence({ pendingAsks: 0, inFlight: [healthy()] }))).toBe('running');
  });
});

describe('stalled', () => {
  it('is a run whose every dispatch is sitting in a retry backoff', () => {
    const retrying = healthy({ attempt: 3, startedAt: 0 });
    expect(readRunActivity(evidence({ inFlight: [retrying, retrying] }))).toBe('stalled');
  });

  it('is a run whose work went onto a queue nobody polls', () => {
    // Scheduled, never started by anyone, and past the threshold — the failure that reads
    // identically to a slow run until you ask how long it waited for a worker that never came.
    const unpolled = healthy({ scheduledAt: NOW - UNPOLLED_AFTER_MS - 1, startedAt: 0 });
    expect(readRunActivity(evidence({ inFlight: [unpolled] }))).toBe('stalled');
  });

  it('is a run whose own workflow task went unanswered', () => {
    // The third of the three, and the one where nothing the run dispatched is wrong at all:
    // `inFlightOf` folds the pending workflow task in beside the activities for exactly this.
    const task = healthy({ attempt: 5, startedAt: 0, scheduledAt: NOW - 600_000 });
    expect(readRunActivity(evidence({ inFlight: [task] }))).toBe('stalled');
  });

  it('is not a dispatch that has only just been queued', () => {
    // A worker under load takes seconds. Calling a briefly-queued dispatch stalled would train an
    // operator to ignore the word.
    const fresh = healthy({ scheduledAt: NOW - 1_000, startedAt: 0 });
    expect(readRunActivity(evidence({ inFlight: [fresh] }))).toBe('running');
  });

  it('is not a run where one thing is stuck and the rest are working', () => {
    // A sweep where unit 37 of 200 is retrying is a working sweep. Reporting it as stalled is the
    // same over-claim as shattering a transcript over one bad Unit.
    const retrying = healthy({ attempt: 3, startedAt: 0 });
    expect(readRunActivity(evidence({ inFlight: [retrying, healthy()] }))).toBe('running');
  });

  it('is not work that is heartbeating, whatever its attempt counter says', () => {
    // A heartbeat is the one case where a long-started item is PROVABLY alive. A Method on its
    // third attempt that is beating right now is working, not stuck.
    const beating = healthy({ attempt: 3, startedAt: 0, heartbeatAt: NOW - 2_000 });
    expect(readRunActivity(evidence({ inFlight: [beating] }))).toBe('running');
  });

  it('is not a run with nothing in flight at all', () => {
    // Between two steps. "Nothing has happened for N minutes" as a stall rule would report a run
    // legitimately waiting on a long timer as broken, and a word that fires on healthy runs stops
    // being read.
    expect(readRunActivity(evidence({ inFlight: [] }))).toBe('running');
  });

  it('takes its threshold from the caller, so the rule is stated rather than slept through', () => {
    const waiting = healthy({ scheduledAt: NOW - 30_000, startedAt: 0 });
    expect(readRunActivity(evidence({ inFlight: [waiting] }))).toBe('running');
    expect(
      readRunActivity(evidence({ inFlight: [waiting], unpolledAfterMs: 10_000 }))
    ).toBe('stalled');
  });
});

describe('reading the pending-work view off a describe', () => {
  it('folds activities, Nexus operations and the run\'s own workflow task into one shape', () => {
    // A run blocked on an unstarted Nexus dispatch and one blocked on an unstarted activity are
    // the same fact to the person watching it.
    const raw = {
      pendingActivities: [{ attempt: 2, scheduledTime: { seconds: 100 } }],
      pendingNexusOperations: [{ attempt: 1, scheduledTime: { seconds: 200 } }],
      pendingWorkflowTask: { attempt: 3, scheduledTime: { seconds: 300 } },
    };
    expect(inFlightOf(raw).map((w) => w.attempt)).toEqual([2, 1, 3]);
    expect(inFlightOf(raw).map((w) => w.scheduledAt)).toEqual([100_000, 200_000, 300_000]);
  });

  it('reads a protobufjs Long the same as a number', () => {
    const long = { toString: () => '1786831339' };
    const [work] = inFlightOf({ pendingActivities: [{ scheduledTime: { seconds: long } }] });
    expect(work!.scheduledAt).toBe(1_786_831_339_000);
  });

  it('yields nothing rather than an epoch timestamp for a field that is missing', () => {
    // A `scheduledAt` of 0 would read as "scheduled in 1970, therefore stalled forever" — the
    // plausible wrong answer, from a server too old to send the field.
    const [work] = inFlightOf({ pendingActivities: [{}] });
    expect(work).toEqual({ attempt: 1, scheduledAt: 0, startedAt: 0, heartbeatAt: 0 });
    expect(readRunActivity(evidence({ inFlight: [work!] }))).toBe('running');
  });

  it('reads a response with no pending work at all as no pending work', () => {
    expect(inFlightOf({})).toEqual([]);
    expect(inFlightOf(undefined)).toEqual([]);
  });
});
