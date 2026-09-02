/**
 * WHO CAN ACTUALLY SERVE THIS ACTOR — registered, serving and stale as three different facts.
 *
 * THE THREE ARE NOT A SCALE, and collapsing any two of them is the bug this module exists for:
 *
 *   REGISTERED  the Actor exists. A folder on this disk, or a descriptor a worker pushed. It says
 *               nothing whatever about whether anything can run it — a registration outlives the
 *               process that made it, which is the whole trap `cli/workers.go` was written to
 *               surface: *registration says an actor exists; only a poller says it can run.*
 *   SERVING     a worker is polling the Actor's shared queue NOW. `DescribeTaskQueue` is the only
 *               thing that knows, and the freshness window below is what makes "now" mean now.
 *   STALE       Temporal still lists a poller that has stopped polling. NOT a smaller kind of
 *               serving: a dispatch aimed at it sits on a queue nobody drains and reports as slow,
 *               which is the failure this repo has documented as indistinguishable from the
 *               outside from a run that is merely taking its time.
 *
 * And UNKNOWN, which is mandatory rather than a fourth nicety: Temporal could not be asked, so
 * "nothing is serving" is not a finding — it is a missing answer, and a page that drew it as a
 * negative would report every Actor as un-served for as long as the cluster was unreachable.
 *
 * THE WINDOW IS MEASURED AND IT IS NOT THIS MODULE'S. `run/workflowState.ts:POLL_FRESH_MS` — two
 * minutes, one comfortable long poll of headroom — exists because Temporal keeps a poller listed for
 * about five minutes after it was last seen. Read raw, an identity reports a worker killed thirty
 * seconds ago as serving, for five minutes. Everything here goes through `pollIsFresh` so the
 * Workflows page and this one cannot disagree about whether a worker is alive.
 *
 * PER WORKER, NOT PER QUEUE, and that is the reason the route grew `workers`. The queue-level
 * `lastPoll` is the freshest poll across every identity: on a queue with one live worker and one
 * killed three minutes ago it is fresh, and a reader choosing a target from `identities` chooses the
 * dead one. Mixed is the normal state of a fleet losing a Machine, not an edge case.
 */

import { identityHost } from '@core/panels/pollers';
import { pollIsFresh, type PollerReport } from '../run/workflowState';
import { terminalHealthReading, tileRefFor } from './chrome/tileRef';
import { paneAddress } from './paneFilter';
import type { Terminal } from './panelsClient';

/** One poller's state. Two words, because a listed poller either is polling now or is not — the
 *  third fact, `registered`, is about the ACTOR and cannot be a property of a worker. */
export type WorkerState = 'serving' | 'stale';

/** One row of "who can serve this", as the card draws it. */
export interface WorkerReading {
  /** The poller identity Temporal reported. Both SDK defaults carry the host — Go
   *  `<pid>@<hostname>@<queue>`, Python `<pid>@<hostname>` — but a custom one is legal. */
  identity: string;
  /** The host the identity names, or undefined when it is not in a shape we understand.
   *  `@core/panels/pollers:identityHost` owns that parse; a second one here is how the Monitor and
   *  this page would start attributing the same worker to different Machines. */
  host?: string;
  /** Epoch ms of this identity's freshest poll; 0 when Temporal listed it without dating it. */
  lastPoll: number;
  /** How long ago that was, or `null` when it was never dated. NOT `0` — see {@link WorkerReading.lastPoll}. */
  ageMs: number | null;
  state: WorkerState;
}

/** What the Actor's queue reads as, all four answers. `registered` is the one that means "it exists
 *  and nothing is polling it" — the classic trap, and a real, calm, common state. */
export type ActorServeState = 'serving' | 'stale' | 'registered' | 'unknown';

/**
 * Every listed poller, dated.
 *
 * An UNDATED poller — Temporal listed the identity with no last-access time — is `stale`, and that
 * is the fail-safe direction on purpose: it is not evidence of anything current (`isServing` holds
 * the same line for the queue), so it must not be offered as a target. The row says `never` rather
 * than an age, so nothing claims to know when it last polled.
 *
 * `report.workers` is defended rather than trusted: the field is required by the type, but the body
 * is `as PollerReport` over whatever the route sent, and an orchestrator older than the field sends
 * identities alone. Reading those as undated keeps the failure on the safe side — a worker the page
 * cannot date is never offered — instead of throwing inside a render.
 */
export function workerReadings(report: PollerReport | null, now: number): WorkerReading[] {
  if (report === null || report.error !== undefined) return [];
  const dated =
    report.workers ?? report.identities.map((identity) => ({ identity, lastPoll: 0 }));
  return dated.map((w) => {
    const host = identityHost(w.identity);
    const reading: WorkerReading = {
      identity: w.identity,
      lastPoll: w.lastPoll,
      ageMs: w.lastPoll > 0 ? Math.max(0, now - w.lastPoll) : null,
      state: pollIsFresh(w.lastPoll, now) ? 'serving' : 'stale',
    };
    if (host !== undefined) reading.host = host;
    return reading;
  });
}

/**
 * The workers a dispatch may be aimed at — the serving ones, and only those.
 *
 * THE POINT OF THE WHOLE SLICE, and the reason it is a function rather than a filter written at each
 * call site: issue 15's probe consumes this to decide what it may offer, and a second `filter` over
 * `identities` somewhere else is exactly how a dead worker gets offered again.
 */
export function dispatchTargets(readings: readonly WorkerReading[]): WorkerReading[] {
  return readings.filter((w) => w.state === 'serving');
}

/**
 * The Actor's own state, from its queue's report.
 *
 * ORDER IS THE CONTRACT. Serving wins over stale — one live worker means the Actor can run,
 * whatever else Temporal is still remembering. Stale wins over registered, because "a worker was
 * here and stopped" is a finding and "nothing has ever polled this" is a different one: the first
 * asks what happened to it, the second asks whether anything was ever served.
 */
export function actorServeState(report: PollerReport | null, now: number): ActorServeState {
  if (report === null || report.error !== undefined) return 'unknown';
  const readings = workerReadings(report, now);
  if (readings.some((w) => w.state === 'serving')) return 'serving';
  if (readings.length > 0) return 'stale';
  return 'registered';
}

/** How each state is said, and what it claims. The words are the contract with the operator —
 *  `run/workflowState.ts:stateWords` is the same idea for a workflow, and reads as its sibling. */
export interface ServeWords {
  label: string;
  title: string;
}

export function serveWords(state: ActorServeState, queue: string): ServeWords {
  switch (state) {
    case 'serving':
      return {
        label: 'serving',
        title: `a worker is polling ${queue} now. This Actor can run.`,
      };
    case 'stale':
      return {
        label: 'stale',
        title:
          `Temporal still lists a poller on ${queue}, but none has polled within the freshness ` +
          'window — a worker that was killed is listed for about five more minutes. Nothing here ' +
          'can run: a dispatch would sit on this queue and look like a slow run.',
      };
    case 'unknown':
      return {
        label: 'unknown',
        title:
          `Temporal could not be asked who is polling ${queue}. NOT the same as nothing serving — ` +
          'the answer is missing, not negative.',
      };
    default:
      return {
        label: 'registered',
        title:
          `this Actor is registered and NOTHING is polling ${queue}. Serve it before dispatching, ` +
          'or the dispatch will sit on a queue nobody polls.',
      };
  }
}

/**
 * The Actor's health across every Machine running it — TRI-STATE, and `null` is not a shrug.
 *
 * `true` every Machine's applicable signals were measured and are fine; `false` at least one has a
 * failing signal; `null` NOBODY LOOKED — no Machine, or none whose axes were ever measured. The
 * whole reason this is not a boolean: `isHealthy` folds unknown into false, which is right for a
 * COUNT ("how many can I vouch for") and wrong for a LABEL, because "we never probed this" and "the
 * handler is down" send an operator to two different places.
 *
 * SEVERITY DECIDES, the chips' order: one `false` outranks every unknown, and an unknown outranks
 * the greens. So a fleet with one dead handler and two unprobed Machines reads `bad`, never
 * `unknown` — "we cannot see" must never swallow "it is broken".
 *
 * NO MACHINES IS `null`, NEVER `true`. An Actor whose Monitor inventory is empty has not been
 * found healthy; nothing has looked at it at all, and a green tick there is precisely the confident
 * lie this page keeps being fixed for.
 */
export function actorHealthReading(machines: readonly Terminal[]): boolean | null {
  if (machines.length === 0) return null;
  const readings = machines.map(terminalHealthReading);
  if (readings.some((r) => r === false)) return false;
  if (readings.every((r) => r === true)) return true;
  return null;
}

/**
 * THIS ACTOR'S LIVE SESSIONS — where its worker is actually running, one row per tmux session.
 *
 * A SESSION IS NOT A PANE AND NOT A POLLER, and the page needs all three for "it is not responding"
 * to be diagnosable here rather than in a terminal. A poller says something is draining the queue
 * from a host; a session is the process that host is running it in, and it is the thing an operator
 * attaches to. They fail independently: a session present with nothing polling is a worker that
 * booted and died in its import, and a poller with no session is a Machine the Monitor cannot see.
 *
 * GROUPED BY SESSION, NOT BY PANE, because a session can hold several windows (`cli/tmux.go` puts
 * the actor and its handler in one) and a card that listed four rows for one worker would be
 * counting windows while saying Machines. The mode and node come out of the Terminal id
 * (`chrome/tileRef` owns that parse — `Terminal.mode` is not on the wire), and the address is
 * `paneFilter.paneAddress`, so an empty `publicIp` reads as `local` here exactly as it does on the
 * Monitor.
 */
export function actorSessions(machines: readonly Terminal[]): ActorSession[] {
  const by = new Map<string, { session: ActorSession; panes: Terminal[] }>();
  for (const m of machines) {
    const ref = tileRefFor(m);
    const key = `${ref.mode}:${ref.node}/${ref.session}`;
    const found = by.get(key);
    if (found) {
      found.session.windows += 1;
      found.panes.push(m);
      continue;
    }
    by.set(key, {
      session: {
        key,
        mode: ref.mode,
        node: ref.node,
        session: ref.session,
        address: paneAddress(m),
        windows: 1,
        health: null,
      },
      panes: [m],
    });
  }
  return [...by.values()]
    .map(({ session, panes }) => ({ ...session, health: actorHealthReading(panes) }))
    .sort((a, b) => a.key.localeCompare(b.key));
}

/** One live session of this Actor — a tmux session on a node, running its worker. */
export interface ActorSession {
  /** `<mode>:<node>/<session>` — unique per session, and what a React key needs. */
  key: string;
  mode: string;
  node: string;
  session: string;
  /** The address to reach it at: the Machine's public IP, or `local` for a session on this host. */
  address: string;
  /** How many of this Actor's panes belong to the session. */
  windows: number;
  /** The session's rolled-up health, tri-state — {@link actorHealthReading} over its own panes. */
  health: boolean | null;
}
