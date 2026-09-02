/**
 * REGISTERED, SERVING AND STALE — the three facts, as values.
 *
 * WHAT THIS SUITE IS DEFENDING. Every case here is one where the WRONG answer is the plausible one:
 * a stale poller looks like a worker, an unreachable Temporal looks like an empty queue, an
 * unprobed Machine looks fine, and a queue whose freshest poll is recent looks entirely healthy
 * while one of its two workers is a corpse. Each of those reads as "everything is fine" to code
 * that asks the obvious question, and each one ends the same way: a dispatch onto a queue nobody
 * polls, reported by Temporal as a run that is merely slow.
 *
 * THE WINDOW IS IMPORTED, NEVER RETYPED. `POLL_FRESH_MS` is measured — Temporal lists a poller for
 * about five minutes after it stops — and a test that hardcoded `120_000` would keep passing while
 * the constant moved, which is the one failure that would make every assertion here meaningless.
 */

import { describe, expect, it } from 'vitest';

import { POLL_FRESH_MS, type PollerReport } from '../run/workflowState';
import type { Terminal } from './panelsClient';
import {
  actorHealthReading,
  actorServeState,
  actorSessions,
  dispatchTargets,
  serveWords,
  workerReadings,
} from './actorWorkers';

const NOW = 1_800_000_000_000;
const QUEUE = 'probe-0.1.0';

/** A poller identity in the shape both SDK defaults mint: Go `<pid>@<host>@<queue>`. */
const identity = (pid: number, host: string): string => `${pid}@${host}@${QUEUE}`;

function report(
  workers: readonly { identity: string; lastPoll: number }[],
  over: Partial<PollerReport> = {}
): PollerReport {
  const lastPoll = workers.reduce((max, w) => (w.lastPoll > max ? w.lastPoll : max), 0);
  return {
    queue: QUEUE,
    pollers: workers.length,
    identities: workers.map((w) => w.identity),
    workers: [...workers],
    lastPoll,
    ...over,
  };
}

const LIVE = { identity: identity(101, 'kf-actor-01'), lastPoll: NOW - 10_000 };
/** Killed, and still listed: Temporal keeps a poller for about five minutes after it was seen. */
const DEAD = { identity: identity(202, 'kf-actor-02'), lastPoll: NOW - 3 * 60_000 };

describe('the three facts', () => {
  it('calls a queue with a fresh poller SERVING', () => {
    expect(actorServeState(report([LIVE]), NOW)).toBe('serving');
  });

  it('calls a registered queue nobody polls REGISTERED, not stale and not unknown', () => {
    // The classic trap, and the calmest state on the page: the Actor exists, nothing has served it.
    // Reading it as `stale` would invent a worker that was never there.
    expect(actorServeState(report([]), NOW)).toBe('registered');
  });

  it('calls a queue whose every poller went quiet STALE', () => {
    expect(actorServeState(report([DEAD]), NOW)).toBe('stale');
  });

  it('calls an unreachable Temporal UNKNOWN, not registered', () => {
    // `pollers: 0` with an error is a MISSING answer. Drawing it as "nothing is serving" reports
    // every Actor on the installation as un-served for as long as the cluster is unreachable.
    expect(actorServeState(report([], { error: 'Connection refused: localhost:7233' }), NOW)).toBe(
      'unknown'
    );
  });

  it('calls a report that has not arrived UNKNOWN', () => {
    // The state every card is in before the first tick. Not asked is not answered.
    expect(actorServeState(null, NOW)).toBe('unknown');
  });

  it('is SERVING when one worker is live and another is dead', () => {
    // The Actor CAN run — the mixed case is about which worker to aim at, not about whether the
    // queue is servable.
    expect(actorServeState(report([LIVE, DEAD]), NOW)).toBe('serving');
  });
});

describe('the freshness window, which is the whole signal', () => {
  it('reads a worker JUST INSIDE the window as serving', () => {
    const w = { identity: identity(1, 'a'), lastPoll: NOW - POLL_FRESH_MS };
    expect(workerReadings(report([w]), NOW)[0]?.state).toBe('serving');
  });

  it('reads a worker JUST OUTSIDE the window as STALE, though Temporal still lists it', () => {
    // ONE MILLISECOND is the whole test. Temporal's list is identical either side of this line —
    // the identity is there, the count is 1 — and without the window the page offers a dead worker
    // with total confidence for five minutes.
    const w = { identity: identity(1, 'a'), lastPoll: NOW - POLL_FRESH_MS - 1 };
    const read = workerReadings(report([w]), NOW);
    expect(read[0]?.state).toBe('stale');
    expect(dispatchTargets(read)).toEqual([]);
  });

  it('reads an UNDATED poller as stale rather than as serving', () => {
    // Temporal listed the identity without a last-access time. That is not evidence of anything
    // current, and the fail-safe direction is the only honest one: never offered.
    const read = workerReadings(report([{ identity: identity(1, 'a'), lastPoll: 0 }]), NOW);
    expect(read[0]?.state).toBe('stale');
    expect(read[0]?.ageMs).toBeNull();
  });

  it('degrades an answer with no per-worker timestamps to stale, never to serving', () => {
    // An orchestrator older than the `workers` field sends identities alone. The body is cast, so
    // the field can be missing at runtime whatever the type says; reading those as undated keeps a
    // worker the page cannot date out of the dispatch list instead of throwing inside a render.
    const { workers: _dropped, ...legacy } = report([LIVE]);
    const read = workerReadings(legacy as PollerReport, NOW);
    expect(read).toHaveLength(1);
    expect(read[0]?.state).toBe('stale');
  });
});

describe('who a dispatch may be aimed at', () => {
  it('offers the live worker and NOT the dead one', () => {
    const read = workerReadings(report([LIVE, DEAD]), NOW);
    expect(read.map((w) => w.state)).toEqual(['serving', 'stale']);
    expect(dispatchTargets(read).map((w) => w.identity)).toEqual([LIVE.identity]);
  });

  it('offers nobody when Temporal could not be asked', () => {
    // An unknown answer is not a list of targets. Aiming at an identity from a report that carries
    // an error is aiming at a memory.
    expect(dispatchTargets(workerReadings(report([LIVE], { error: 'deadline exceeded' }), NOW))).toEqual(
      []
    );
  });

  it('names the host from the identity rather than parsing it a second way', () => {
    const read = workerReadings(report([LIVE]), NOW);
    expect(read[0]?.host).toBe('kf-actor-01');
  });

  it('leaves the host undefined for an identity that names none', () => {
    // A custom `Identity` is legal. Undefined is a real answer — the row prints the identity
    // itself — and guessing a host from it would attribute a worker to a Machine at random.
    expect(workerReadings(report([{ identity: 'custom-worker', lastPoll: NOW }]), NOW)[0]?.host)
      .toBeUndefined();
  });
});

describe('the words each state says', () => {
  it('names the queue in every one of them, including unknown', () => {
    for (const state of ['serving', 'stale', 'registered', 'unknown'] as const) {
      expect(serveWords(state, QUEUE).title).toContain(QUEUE);
    }
  });

  it('never says unknown is fine, and never says it is nothing', () => {
    const words = serveWords('unknown', QUEUE);
    expect(words.label).toBe('unknown');
    expect(words.title).toContain('missing, not negative');
  });
});

function pane(over: Partial<Terminal> & { id: string }): Terminal {
  return {
    machine: 'kf-actor-01',
    host: 'kf-actor-01.kontra.internal',
    publicIp: '10.124.0.5',
    tag: 'worker',
    fleet: 'apex',
    actor: 'probe',
    version: '0.1.0',
    window: '0',
    health: { reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' },
    ...over,
  };
}

describe('the actor’s health, which is tri-state', () => {
  it('is UNKNOWN when no Machine is running it — never ok', () => {
    // Nothing has looked. A green tick over an empty inventory is the confident lie this page is
    // repeatedly fixed for: "we have no evidence" rendered as "we have good evidence".
    expect(actorHealthReading([])).toBeNull();
  });

  it('is ok when every Machine’s applicable signals were measured and are fine', () => {
    expect(actorHealthReading([pane({ id: 'fleet:kf-actor-01/kontra-probe/0' })])).toBe(true);
  });

  it('is UNKNOWN, not ok, when a signal was never measured', () => {
    const unprobed = pane({
      id: 'fleet:kf-actor-01/kontra-probe/0',
      health: { reachable: 'ok', session: 'present', poller: 'unknown', loads: 'ok' },
    });
    expect(actorHealthReading([unprobed])).toBeNull();
  });

  it('is BAD when one Machine is failing, however many unknowns are beside it', () => {
    // Severity leads: "we cannot see" must never swallow "it is broken".
    const down = pane({
      id: 'fleet:kf-actor-02/kontra-probe/0',
      health: { reachable: 'ok', session: 'present', poller: 'none', loads: 'ok' },
    });
    const unprobed = pane({
      id: 'fleet:kf-actor-03/kontra-probe/0',
      health: { reachable: 'unknown', session: 'unknown', poller: 'unknown', loads: 'unknown' },
    });
    expect(actorHealthReading([down, unprobed])).toBe(false);
  });

  it('reads a LOCAL pane in its own mode, so `loads` does not fail it', () => {
    // `loads` comes from vmagent and vmagent runs only on the fleet. Reading a local Worker in the
    // fleet's mode is the bug that reported a perfectly healthy local actor as 0 serving.
    const local = pane({
      id: 'local:localhost/probe-0_1_0/0',
      publicIp: '',
      health: { reachable: 'unknown', session: 'present', poller: 'live', loads: 'unknown' },
    });
    expect(actorHealthReading([local])).toBe(true);
  });
});

describe('the actor’s live sessions', () => {
  it('groups the panes of one session into one row', () => {
    // A session holds the actor and its handler. Four rows for one worker would be counting windows
    // while saying Machines.
    const sessions = actorSessions([
      pane({ id: 'fleet:kf-actor-01/kontra-probe/0' }),
      pane({ id: 'fleet:kf-actor-01/kontra-probe/1', window: '1' }),
    ]);
    expect(sessions).toHaveLength(1);
    expect(sessions[0]?.session).toBe('kontra-probe');
    expect(sessions[0]?.windows).toBe(2);
  });

  it('keeps two nodes apart even under the same session name', () => {
    const sessions = actorSessions([
      pane({ id: 'fleet:kf-actor-01/kontra-probe/0' }),
      pane({ id: 'fleet:kf-actor-02/kontra-probe/0', machine: 'kf-actor-02', publicIp: '10.124.0.6' }),
    ]);
    expect(sessions.map((s) => s.node)).toEqual(['kf-actor-01', 'kf-actor-02']);
    expect(sessions.map((s) => s.address)).toEqual(['10.124.0.5', '10.124.0.6']);
  });

  it('calls an addressless session local rather than blank', () => {
    const sessions = actorSessions([pane({ id: 'local:localhost/probe-0_1_0/0', publicIp: '' })]);
    expect(sessions[0]?.address).toBe('local');
    expect(sessions[0]?.mode).toBe('local');
  });

  it('carries each session’s own tri-state health', () => {
    const sessions = actorSessions([
      pane({
        id: 'fleet:kf-actor-01/kontra-probe/0',
        health: { reachable: 'ok', session: 'present', poller: 'none', loads: 'ok' },
      }),
    ]);
    expect(sessions[0]?.health).toBe(false);
  });
});
