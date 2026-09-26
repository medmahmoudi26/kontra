/**
 * WHAT WILL NEVER MOVE — open executions whose task queue nobody is polling.
 *
 * ── WHY THIS SURFACE EXISTS ─────────────────────────────────────────────────────────────────────
 *
 * An event-log audit found NINE open executions on this cluster, every one of them ending on a
 * `WorkflowTaskScheduled` that nobody polls: five Wardens up to 14 days old, an idle StreamWF at 38
 * days, a dataset-retention workflow that had sat 18 days without ever running a task, and a
 * dispatch pair. Not one of them appeared on any surface kontra has.
 *
 * THEY ARE FREE, AND THAT IS NOT THE PROBLEM. Measured in the same audit: a warden history shows a
 * 14-day gap with no new events, because the server does not re-time-out an unpolled task. They cost
 * nothing and will cost nothing. What is wrong is that **nothing reaps them and nothing reports
 * them**, so the set only grows — and each one RESUMES the moment a worker returns to its queue,
 * which is a two-week-old Warden waking up with a decision it made a fortnight ago.
 *
 * ── IT REPORTS AND DOES NOT REAP, DELIBERATELY ──────────────────────────────────────────────────
 *
 * Terminating somebody's execution is not a thing a health check should decide. A wedged Warden may
 * be a Machine that is coming back; a wedged dispatch may be a worker somebody is about to restart.
 * The audit says the same: "the decision about what dealt-with means" is the work, and it is an
 * operator's. So this answers the question — WHICH ones, and for how long — and `kontra doctor`
 * prints it. What to do about one is typed by a person.
 *
 * ── INTERNAL TYPES ARE INCLUDED HERE, AND THAT IS THE POINT ─────────────────────────────────────
 *
 * `listRuns` subtracts `KONTRA_INTERNAL_WORKFLOW_TYPES` because the Runs page is about a user's
 * runs, and it is right to. Every one of the nine was an internal type. A surface that inherited
 * that exclusion would have been blind to all of them, which is how they went unnoticed for a month.
 *
 * ── "NO POLLER" IS NOT "NOTHING IS SERVING IT" ──────────────────────────────────────────────────
 *
 * `isServing` is the authority and the same one the Actors grid uses: a queue with a poller whose
 * last poll is older than `POLL_FRESH_MS` is NOT being served, and a queue Temporal could not be
 * asked about is UNKNOWN rather than dead. An unknown is reported as unknown — a health check that
 * called a cluster it could not reach "wedged" would be the loudest possible way to be wrong.
 */

import type { FastifyInstance } from 'fastify';

import { pollIsFresh, type QueueState } from '../pollers';
import { listOpenExecutions } from '../temporalClient';

/** One open execution and whether anything is polling the queue it is waiting on. */
export interface StuckExecution {
  workflowId: string;
  execId: string;
  type: string;
  queue: string;
  startedAt: number;
  /** Milliseconds since it started. The number an operator judges "is this normal" by. */
  ageMs: number;
  /** How many pollers Temporal reports on its queue. `null` when the cluster could not be asked. */
  pollers: number | null;
  /**
   * Nothing is polling its queue, so it cannot move.
   *
   * `false` FOR AN UNKNOWN, not `true`. The failure direction matters: reporting a cluster nobody
   * could reach as a wall of wedged workflows would send an operator to terminate things that are
   * fine. `pollers === null` beside it is what says the question went unanswered.
   */
  wedged: boolean;
}

export interface StuckReport {
  executions: StuckExecution[];
  /** How many of them are wedged — the number worth acting on. */
  wedged: number;
  /** The listing hit its cap, so there may be more. Said rather than inferred, for the reason
   *  `historyArchive`'s sweep now says the same thing about its own page. */
  capped: boolean;
}

/** How many open executions one report considers. Bounded like every other listing here. */
const SCAN_CAP = 200;

export interface StuckDeps {
  list?: typeof listOpenExecutions;
  describeQueue?: (queue: string) => Promise<QueueState>;
  now?: () => number;
}

export async function stuckExecutions(deps: StuckDeps = {}): Promise<StuckReport> {
  const list = deps.list ?? listOpenExecutions;
  const now = deps.now ?? Date.now;
  const { executions, capped } = await list(SCAN_CAP);

  // ONE DESCRIBE PER DISTINCT QUEUE, not per execution. Five Wardens on one queue is one question,
  // and asking it five times is five `DescribeTaskQueue` calls for one answer.
  const queues = [...new Set(executions.map((e) => e.queue).filter((q) => q !== ''))];
  const reports = new Map<string, QueueState>();
  if (deps.describeQueue) {
    await Promise.all(
      queues.map(async (q) => {
        try {
          reports.set(q, await deps.describeQueue!(q));
        } catch {
          // Left absent, which reads as `unknown` below — never as "nothing is serving it".
        }
      })
    );
  }

  const at = now();
  const rows: StuckExecution[] = executions.map((e) => {
    const report = reports.get(e.queue);
    const pollers =
      report === undefined || report.error !== undefined ? null : report.identities.length;
    return {
      workflowId: e.workflowId,
      execId: e.execId,
      type: e.type,
      queue: e.queue,
      startedAt: e.startedAt,
      ageMs: e.startedAt > 0 ? at - e.startedAt : 0,
      pollers,
      /* `isServing`'S RULE, RESTATED FROM ITS PARTS because that helper lives on the console side
         of the seam: a queue is being served when it has a poller AND that poller's last poll is
         inside `POLL_FRESH_MS`. Temporal keeps a poller listed for about five minutes after it was
         last seen, so identities alone would report a worker killed thirty seconds ago as serving.
         `report === undefined` is UNKNOWN and not wedged — see the field doc. */
      wedged:
        report !== undefined &&
        report.error === undefined &&
        !(report.identities.length > 0 && pollIsFresh(report.lastPoll, at)),
    };
  });

  return {
    // Oldest first: a fortnight-old Warden is the one to look at, and a listing sorted by id
    // buries it among things that started a minute ago.
    executions: rows.sort((a, b) => b.ageMs - a.ageMs),
    wedged: rows.filter((r) => r.wedged).length,
    capped,
  };
}

export function registerStuckRoutes(app: FastifyInstance, deps: StuckDeps = {}): void {
  /* NOT GATED, like the rest of the read-only health surface. It answers workflow ids, types and
     queue names — the same class of fact `/api/runs` and `/api/pulse` already answer — and no
     payload, no argument and no history. */
  app.get('/api/stuck', async (_req, reply) => {
    try {
      return await stuckExecutions(deps);
    } catch (err) {
      // A cluster that cannot be listed is not a wedged cluster. Answering `{executions: []}` here
      // would be a confident all-clear over a control plane nobody can see into — the same mistake
      // `pulse` keeps `pulseError` to avoid.
      return reply.code(502).send({
        error: `could not list open executions: ${err instanceof Error ? err.message : String(err)}`,
      });
    }
  });
}
