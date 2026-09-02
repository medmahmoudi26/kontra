/**
 * Is anything happening at all, anywhere — the one answer the chrome carries on every surface.
 *
 * WHY THIS IS A ROUTE OF ITS OWN AND NOT A FIELD ON THE RUN LIST. Retiring the global Runs surface
 * traded a table for a question: an operator on the Datasets page, or the Monitor, or nothing at
 * all, still needs to know whether the fleet is grinding, waiting on them, or resting. The rail
 * used to answer it by polling `/api/runs` every five seconds and filtering the array — which is
 * the run list coming back through the chrome, at the run list's price: a page of up to 200
 * executions, a ledger read per row, a describe per open row, and a second visibility scan over
 * every dispatch on the cluster to fill a column nothing in the rail draws. That number grows with
 * how many runs this controller has EVER held. The thing it is being asked is about right now.
 *
 * SO IT IS TWO BOUNDED READS AND NOTHING ELSE:
 *
 *   RUNNING is COUNTED, off the visibility index (`countRuns`) — one RPC, no page, no per-run read,
 *     and identical in cost on a fresh controller and on one with ten thousand closed runs.
 *   PARKED is read from the OPEN runs' memos, which ride on the listing for free (`listOpenRuns`).
 *     Closed runs never enter that query, so what bounds the scan is how much is EXECUTING — which
 *     is bounded by the machines — and {@link OPEN_SCAN_CAP} is the belt on top of that.
 *
 * IT DOES NOT REPORT `stalled`, AND THAT IS A BOUNDARY RATHER THAN AN OVERSIGHT. `runActivity.ts`
 * makes three readings out of `running`, and the third one — a dispatch onto a queue nobody polls,
 * an activity in retry backoff — is derived from `DescribeWorkflowExecution`'s pending-work view.
 * That is one RPC PER RUN, which is exactly the cost this route exists not to pay on every surface
 * every few seconds. A stall is a fact about one run and it is answered where that run is open (the
 * run list and `/api/runs/:runId` both carry `activity`). This answers a fact about the cluster.
 *
 * A PARK THIS HAS NOT SEEN IS NOT A RUN DECLARED UN-PARKED. An open run's memo reaches visibility
 * asynchronously (`listRuns` says so in the same words), so a run that parked a second ago arrives
 * here a beat later, and a scan that hit its cap has looked at some of the fleet rather than all of
 * it. Both are reported — {@link Pulse.scanned} and {@link Pulse.capped} — so the surface can say
 * "at least this many" instead of turning a partial look into an all-clear.
 *
 * PURE, THEN WIRED. {@link readPulse} is a total function over plain values, so idle, running,
 * parked, mixed and capped are all assertable with no cluster and no clock. {@link clusterPulse} is
 * the four lines that hand it two reads.
 */

import { pendingAsks, readAsks } from './hitl';

/**
 * How many RUNNING runs one pulse examines for parked questions.
 *
 * NOT A PAGE SIZE — a ceiling on a scan whose natural size is the fleet. Sixty-four concurrent
 * caller workflows is already a busy controller, and past it the parked count becomes a floor that
 * says so rather than a page-two link. The alternative failure is the one this route was written to
 * end: an unbounded scan on a timer behind every surface in the app.
 */
export const OPEN_SCAN_CAP = 64;

/**
 * How many parked runs the pulse NAMES.
 *
 * THE COUNT IS THE ANSWER; THESE ARE THE WAY IN. A chrome mark that says "3 waiting on you" needs
 * somewhere to send the person who reads it, and that is a run id — but a response that named every
 * open run would be the retired Runs list wearing a different route, which is precisely the thing
 * this whole change was paid for. So identities are carried only for the dimension that is
 * ACTIONABLE (a question addressed to a human), and only a handful: past eight, the answer to "what
 * is waiting on me" is the Workflows surface, not a longer tooltip.
 */
export const NAMED_PARKED_CAP = 8;

/** One run known to be waiting on a human — the pulse's only named runs. */
export interface PulseParked {
  /** The caller's workflow id. There is no second identifier (ADR 0023 §12). */
  runId: string;
  /** The caller's workflow TYPE — which thread this conversation belongs to, so the mark has
   *  somewhere to send you. Empty when the listing did not carry one. */
  workflow: string;
  /** How many of its asks are still waiting. */
  pending: number;
  /** Epoch ms the OLDEST waiting ask was asked, or 0 when no entry carried a readable instant.
   *  Never defaulted to `now`: "waiting since just now" about a question that has been open for an
   *  hour is the one thing this field must not say. */
  since: number;
}

/**
 * What is happening across everything, right now.
 *
 * `running` COUNTS PARKED RUNS TOO, because Temporal does: a workflow blocked on a human is
 * Running, and a count that quietly subtracted them would disagree with `temporal workflow list`
 * and with the run list on the same box. The two numbers are reported side by side and the
 * subtraction is left to the surface, which knows whether it may make it — see {@link capped}.
 */
export interface Pulse {
  /** Runs Temporal reports as Running, cluster-wide. Exact, and the parked ones are in it. */
  running: number;
  /**
   * How many of the SCANNED running runs are waiting on a human.
   *
   * A FLOOR WHENEVER {@link capped} IS TRUE, and equal to the whole truth otherwise. It is a
   * separate number from `named.length` because naming is capped harder than counting is.
   */
  parked: number;
  /** The parked runs, longest-waiting first, capped at {@link NAMED_PARKED_CAP}. */
  named: PulseParked[];
  /** How many running runs were examined for questions. Equal to `running` on any ordinary
   *  controller; below it when the scan was capped or when the index has not caught up. */
  scanned: number;
  /** The scan stopped at {@link OPEN_SCAN_CAP}. `parked` is then a floor, not a total. */
  capped: boolean;
  /** When this reading was taken, by the server's clock. The browser measures "how long has it been
   *  waiting" against this rather than against its own poll. */
  at: number;
}

/** One open run as this module needs it: an id, a type, and the memo bag its asks live in. Kept
 *  structural so the reduction never imports a Temporal type and can be driven from a literal. */
export interface OpenRunEvidence {
  runId: string;
  type: string;
  memo: unknown;
}

export interface PulseEvidence {
  /** Temporal's count of running runs. */
  running: number;
  /** The running runs that were examined, with their memos. */
  open: readonly OpenRunEvidence[];
  /** The scan hit its cap. */
  capped: boolean;
  now: number;
  /** How many parked runs to name. Injected so the cap is a decision a test can state. */
  nameCap?: number;
}

/**
 * The one reading, from the evidence.
 *
 * NOTHING HERE INVENTS AN IDLE. `running: 0` with nothing scanned is idle because Temporal said
 * zero, and a caller that could not reach Temporal must not call this with a zero — the route
 * answers 502 and the chrome says "not known", because "we could not look" and "nothing is
 * happening" are the two readings this codebase keeps furthest apart.
 */
export function readPulse(evidence: PulseEvidence): Pulse {
  const parked: PulseParked[] = [];
  for (const run of evidence.open) {
    const waiting = pendingAsks(readAsks(run.memo, evidence.now));
    if (waiting.length === 0) continue;
    const asked = waiting.map((a) => a.askedAt).filter((t) => Number.isFinite(t) && t > 0);
    parked.push({
      runId: run.runId,
      workflow: run.type,
      pending: waiting.length,
      since: asked.length > 0 ? Math.min(...asked) : 0,
    });
  }
  // LONGEST-WAITING FIRST, and a park with no readable instant sorts LAST rather than to 1970.
  // The list is the way in, so its order is a priority: the question that has been open longest is
  // the one an operator should be handed.
  const named = [...parked].sort((a, b) => rank(a.since) - rank(b.since)).slice(0, evidence.nameCap ?? NAMED_PARKED_CAP);
  return {
    running: Math.max(0, evidence.running),
    parked: parked.length,
    named,
    scanned: evidence.open.length,
    capped: evidence.capped,
    at: evidence.now,
  };
}

/** Sort key for "waiting since". `0` means the entry carried no readable instant, which is not the
 *  oldest park on the cluster — it is an unknown, and it goes to the back. */
function rank(since: number): number {
  return since > 0 ? since : Number.POSITIVE_INFINITY;
}

export interface PulseDeps {
  /** Runs Temporal reports in one status. Counted off the visibility index, never listed. */
  count(filter: { status?: string }): Promise<number>;
  /** The open runs and their memos, capped. */
  open(cap: number): Promise<{ runs: OpenRunEvidence[]; capped: boolean }>;
  now(): number;
}

/**
 * The pulse, from two bounded reads.
 *
 * THE COUNT IS TAKEN FIRST AND IS NOT DERIVED FROM THE SCAN. A count taken by counting the scan
 * would silently cap the headline number at {@link OPEN_SCAN_CAP} — a controller with 200 runs in
 * flight would read "64 running" forever, which is the kind of quietly-wrong number that gets
 * believed. They are two questions with two authorities' worth of confidence, and the response
 * carries both so a reader can tell when the second one fell short of the first.
 *
 * THE TWO READS ARE CONCURRENT because neither depends on the other, and this sits behind a poll
 * every surface pays for.
 */
export async function clusterPulse(deps: PulseDeps, cap = OPEN_SCAN_CAP): Promise<Pulse> {
  const [running, scan] = await Promise.all([deps.count({ status: 'running' }), deps.open(cap)]);
  return readPulse({ running, open: scan.runs, capped: scan.capped, now: deps.now() });
}
