/**
 * What an OPEN run is doing: running, parked on a human, or stalled.
 *
 * A THIRD DIMENSION, NOT A FOURTH `RunStatus`. `data/materialization.ts` states the rule and the
 * reason: adding a non-terminal member to `RunStatus` would hang the terminal set in `runs.ts` and
 * the CLI's dispatch poll, and Temporal has no such state to reconcile from. Execution is
 * Temporal's dimension and materialization is the ledger's; this is a third with its own authority
 * — the run's own memo for `parked`, and Temporal's pending-work view for `stalled` — reported
 * BESIDE them and never merged into either.
 *
 * WHY IT HAS TO EXIST AT ALL. `running` covers three situations an operator has to tell apart, and
 * `history.ts` concedes as much in its own header: a run grinding through Batches, a run whose
 * every dispatch is sitting in a retry backoff, and a run whose work went onto a queue nobody
 * polls. Since HITL, there is a fourth — a run waiting for a person. All four report `running`,
 * and a run waiting on a human that reads identically to a run making progress is the
 * failure this whole surface exists to end.
 *
 * `null` IS AN ANSWER AND IT IS NOT `running`. A closed run is not running, parked or stalled —
 * saying one of those about a finished run would be a wrong answer, not a rough one. And a run
 * Temporal could not be asked about gets `null` too rather than an optimistic `running`, because
 * "we could not look" and "it is fine" are the two readings this codebase keeps furthest apart.
 *
 * PURE. Every rule below is a total function over plain values, so all four readings are assertable
 * without a cluster, a worker or a clock this module was not handed.
 */

import type { RunStatus } from '../contract/types';

export type RunActivity = 'running' | 'parked' | 'stalled';

export const RUN_ACTIVITIES: readonly RunActivity[] = ['running', 'parked', 'stalled'];

/**
 * How long work may sit unstarted before it counts as stalled.
 *
 * ABOUT A QUEUE NOBODY POLLS, not about a busy one. A worker under load takes seconds; a minute of
 * nothing means the queue has no poller, or every poller on it is wedged. Deliberately generous:
 * calling a briefly-queued dispatch stalled would train an operator to ignore the word.
 */
export const UNPOLLED_AFTER_MS = 60_000;

/**
 * One thing Temporal says this run has in flight — a pending activity, a pending Nexus operation,
 * or the run's own pending workflow task.
 *
 * ALL FROM `DescribeWorkflowExecution`, which is metadata and costs no payload decode (ADR 0007):
 * `attempt`, `scheduledTime`, `lastStartedTime`, `lastHeartbeatTime`. The three are read into one
 * shape on purpose — a run blocked on an unstarted Nexus dispatch and one blocked on an unstarted
 * activity are the same fact to the person watching it.
 */
export interface InFlight {
  /** Temporal's attempt counter. `> 1` means it has already failed at least once and is in a
   *  retry backoff — the second of the three failures that look like `running`. */
  attempt: number;
  /** Epoch ms it was scheduled. */
  scheduledAt: number;
  /** Epoch ms a worker last STARTED it. 0 when no worker has ever taken it — the first of the
   *  three failures, and the one that reads identically to a slow run. */
  startedAt: number;
  /** Epoch ms of its last heartbeat, for work that heartbeats. 0 otherwise. A heartbeating
   *  activity is the one case where a long-started item is provably still alive. */
  heartbeatAt: number;
}

export interface ActivityEvidence {
  /** EXECUTION dimension — Temporal's. Only `running` is open; everything else yields `null`. */
  execution: RunStatus;
  /** How many asks are still waiting on a human. Authority: the run's own memo (`hitl.ts`). */
  pendingAsks: number;
  /** What Temporal says is in flight. An EMPTY array means "nothing in flight", which is a real
   *  answer; a run between two steps has nothing pending and is running normally. */
  inFlight: readonly InFlight[];
  now: number;
  /** Milliseconds unstarted work may wait before it reads as stalled. Injected so the threshold
   *  is a decision a test can state rather than a constant it has to sleep through. */
  unpolledAfterMs?: number;
}

/**
 * The one reading, from the evidence.
 *
 * PARKED BEATS STALLED, and that ordering is a decision rather than a convenience. A run waiting
 * for a human IS blocked, so a stall rule would fire on it — and it would send an operator hunting
 * a dead worker when the bottleneck is the operator. A parked run says so; nothing else it is
 * doing changes that sentence.
 *
 * STALLED IS ABOUT WHAT IS IN FLIGHT, not about a wall clock. "Nothing has happened for N minutes"
 * would report a run legitimately waiting on a long timer or a slow Method as broken, and a word
 * that fires on healthy runs stops being read. So a run is stalled when it HAS work in flight and
 * every piece of it is either retrying after a failure or has never been picked up — which is
 * exactly the pair of failures that report `running` with no new rows. A run with nothing pending
 * is between steps, and that is running.
 */
export function readRunActivity(evidence: ActivityEvidence): RunActivity | null {
  if (evidence.execution !== 'running') return null;
  if (evidence.pendingAsks > 0) return 'parked';
  const work = evidence.inFlight;
  if (work.length === 0) return 'running';
  const unpolledAfter = evidence.unpolledAfterMs ?? UNPOLLED_AFTER_MS;
  const stuck = work.every((w) => isStuck(w, evidence.now, unpolledAfter));
  return stuck ? 'stalled' : 'running';
}

/**
 * Is this one piece of work going nowhere?
 *
 * Two ways, both of which report as `running` from outside:
 *
 *   RETRYING — `attempt > 1` and no worker holds it right now. A failed activity in a backoff has
 *     an attempt counter above 1 and no live start; one that failed once and is running again does
 *     not, and calling THAT stalled would flag every run that survived a blip.
 *   UNPOLLED — scheduled, never started by anyone, and past the threshold. This is the dispatch
 *     onto a queue with no poller, which is indistinguishable from a slow run until you ask how
 *     long it has been waiting for a worker that never came.
 *
 * A HEARTBEAT ALWAYS WINS. Work that beat recently is alive whatever its attempt counter says.
 */
function isStuck(work: InFlight, now: number, unpolledAfter: number): boolean {
  if (work.heartbeatAt > 0 && now - work.heartbeatAt < unpolledAfter) return false;
  const running = work.startedAt > 0 && work.startedAt >= work.scheduledAt;
  if (work.attempt > 1 && !running) return true;
  return !running && work.scheduledAt > 0 && now - work.scheduledAt >= unpolledAfter;
}

/**
 * Read the pending-work view off a raw `DescribeWorkflowExecutionResponse`.
 *
 * STRUCTURAL AND DEFENSIVE, because it reads a proto bag rather than a typed SDK object: the three
 * pending collections are optional, their timestamps are proto Timestamps whose `seconds` arrives
 * as a number or a protobufjs Long, and `pendingNexusOperations` only exists on servers new enough
 * to have it. Anything missing yields nothing rather than a zero that would read as "scheduled at
 * the epoch, therefore stalled forever".
 */
export function inFlightOf(raw: unknown): InFlight[] {
  const bag = (raw && typeof raw === 'object' ? raw : {}) as Record<string, unknown>;
  const out: InFlight[] = [];
  for (const field of ['pendingActivities', 'pendingNexusOperations'] as const) {
    for (const item of asArray(bag[field])) out.push(oneInFlight(item));
  }
  // The run's OWN workflow task. A caller's worker that stopped answering leaves this pending and
  // its attempt counter climbing — the third of the three failures, and the one where nothing the
  // run dispatched is wrong at all.
  const task = bag.pendingWorkflowTask;
  if (task && typeof task === 'object') out.push(oneInFlight(task));
  return out;
}

function oneInFlight(item: unknown): InFlight {
  const bag = (item && typeof item === 'object' ? item : {}) as Record<string, unknown>;
  return {
    attempt: Math.max(1, num(bag.attempt) || 1),
    scheduledAt: tsToMs(bag.scheduledTime),
    startedAt: tsToMs(bag.lastStartedTime ?? bag.startedTime ?? bag.lastAttemptCompleteTime),
    heartbeatAt: tsToMs(bag.lastHeartbeatTime),
  };
}

function asArray(raw: unknown): unknown[] {
  return Array.isArray(raw) ? raw : [];
}

/** Proto Timestamp → epoch ms. `seconds` arrives as a number or a protobufjs Long — the same
 *  reading `history.ts` does, written again here rather than imported, because that module's copy
 *  is private to its own reduction. */
function tsToMs(ts: unknown): number {
  if (!ts || typeof ts !== 'object') return 0;
  const bag = ts as { seconds?: unknown; nanos?: unknown };
  const ms = num(bag.seconds) * 1000 + Math.floor(num(bag.nanos) / 1e6);
  return ms > 0 ? ms : 0;
}

function num(raw: unknown): number {
  if (typeof raw === 'number') return Number.isFinite(raw) ? raw : 0;
  if (raw == null) return 0;
  const n = Number((raw as { toString(): string }).toString());
  return Number.isFinite(n) ? n : 0;
}
