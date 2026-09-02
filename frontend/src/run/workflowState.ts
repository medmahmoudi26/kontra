/**
 * What state a workflow is in — the three the Workflows page draws, and the one it must not fake.
 *
 * THE STATES ARE NOT A SCALE. They come from two different authorities answering two different
 * questions, and reading either one alone gets a workflow wrong:
 *
 *   SERVING   a worker is POLLING this workflow's queue. Temporal's `DescribeTaskQueue` is the only
 *             thing that knows. Registration says a workflow exists; a poller says it can run.
 *   RUNNING   a Run of it is open. The run list knows. A workflow can be running while NOTHING is
 *             serving it — that is a run whose worker died, and it is the most important state on
 *             the page, because it looks exactly like a slow run.
 *   IDLE      neither. Not an error: most workflows most of the time.
 *   UNKNOWN   Temporal could not be asked. Distinct from IDLE on purpose — a cluster that is
 *             unreachable would otherwise draw every workflow as un-served, which is a page-wide
 *             lie told confidently.
 *
 * A workflow that is BOTH serving and running is `running`: the run is the more specific fact and
 * the more urgent one.
 */

import { POLL_FRESH_MS, pollIsFresh } from '@core/panels/pollers';
import type { RunRow } from './api';

export type WorkflowState = 'running' | 'serving' | 'idle' | 'unknown';

/**
 * What `/api/queues/:queue/pollers` answers with.
 *
 * `pollers: 0` WITH `error` set means the count is meaningless, not zero — the distinction the
 * route goes to some trouble to preserve, and the one this module exists to keep.
 */
export interface PollerReport {
  queue: string;
  pollers: number;
  identities: string[];
  /**
   * The same pollers WITH THEIR OWN timestamps — `backend/src/panels/pollers.ts:PollerPoll`.
   *
   * `lastPoll` below is the freshest across the queue, which answers "is anything serving this" and
   * cannot answer "is THIS worker serving". The two differ exactly where it costs: a queue with one
   * live worker and one killed three minutes ago has a fresh `lastPoll` and two identities, so a
   * reader choosing a target from `identities` chooses the dead one.
   */
  workers: PollerWorker[];
  /** Epoch ms of the freshest poll across identities; 0 when never. */
  lastPoll: number;
  error?: string;
}

/** One poller, kept whole. `lastPoll` is 0 when Temporal listed the identity without dating it —
 *  which is not evidence of anything current, and never rendered as if it were. */
export interface PollerWorker {
  identity: string;
  lastPoll: number;
}

/**
 * How stale a poll may be and still count as serving, and the window applied to one timestamp.
 *
 * MEASURED, AND NOW SHARED WITH THE SERVER. It used to be defined here, which was right while the
 * only readers were this module and `panels/actorWorkers.ts`. The probe's route needs the same
 * window — it refuses a dispatch aimed at a queue whose only pollers are dead
 * (`backend/src/probe.ts`), and there is no path from `src/` into this tree — so the constant
 * moved to `@core/panels/pollers` and is re-exported here rather than written a second time. The
 * failure a second spelling would produce is the one both sides exist to prevent: a page offering
 * a worker the server calls stale, or the reverse.
 */
export { POLL_FRESH_MS, pollIsFresh };

/** Is a worker actually polling — as opposed to having polled, once, before it died? */
export function isServing(report: PollerReport | null, now: number): boolean {
  if (!report || report.error !== undefined) return false;
  if (report.pollers <= 0) return false;
  // A poller with no timestamp at all is not evidence of anything current.
  return pollIsFresh(report.lastPoll, now);
}

/**
 * The state to draw for one workflow.
 *
 * `runs` is already this workflow's — filtering by type belongs to whoever knows the type, not
 * here, because a type read from a source file and a type Temporal reports are two strings this
 * module has no way to reconcile.
 */
export function workflowState(
  runs: readonly RunRow[],
  report: PollerReport | null,
  now: number
): WorkflowState {
  if (runs.some((r) => r.status === 'running')) return 'running';
  if (isServing(report, now)) return 'serving';
  // Only after both of the above: a workflow that is running is running whether or not Temporal
  // can be reached to ask about its queue.
  if (report === null || report.error !== undefined) return 'unknown';
  return 'idle';
}

/** How each state is said, and what it claims. The words are the contract with the operator. */
export interface StateWords {
  label: string;
  title: string;
  /** True for the states where something is happening, which is what the animation is FOR. */
  live: boolean;
}

export function stateWords(state: WorkflowState): StateWords {
  switch (state) {
    case 'running':
      return {
        label: 'running',
        title: 'a Run of this workflow is open. Open it to see what it is doing.',
        live: true,
      };
    case 'serving':
      return {
        label: 'serving',
        title:
          'a worker is polling this workflow’s queue and has nothing to do. Press Run to give it ' +
          'something.',
        live: true,
      };
    case 'unknown':
      return {
        label: 'unknown',
        title:
          'Temporal could not be asked whether anything is serving this. NOT the same as idle — ' +
          'the answer is missing, not negative.',
        live: false,
      };
    default:
      return {
        label: 'idle',
        title:
          'nothing is serving this workflow and no Run of it is open. Serve it before starting ' +
          'one, or the Run will sit on a queue nobody polls.',
        live: false,
      };
  }
}

/**
 * The warning worth interrupting somebody for: a Run is open and NOTHING is serving it.
 *
 * This is the state that looks exactly like a slow run and is not one — the caller's worker is gone
 * (a pause nobody resumed, a crashed process, a machine rebooted), so the workflow is making no
 * progress and will make none. Temporal reports it as `running` the whole time.
 */
export function strandedRun(
  runs: readonly RunRow[],
  report: PollerReport | null,
  now: number
): boolean {
  if (!runs.some((r) => r.status === 'running')) return false;
  // Only when the answer is a real negative. An unreachable Temporal must not raise this alarm.
  if (report === null || report.error !== undefined) return false;
  return !isServing(report, now);
}
