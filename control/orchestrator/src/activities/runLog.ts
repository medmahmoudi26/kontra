/**
 * A log line an operator can find by the Run it belongs to.
 *
 * ── THE GAP THIS CLOSES ─────────────────────────────────────────────────────────────────────────
 *
 * Every line the run page's rail draws is selected with `run_id:"<id>"`. An actor's lines carry
 * that field (the Python side stamps it from its Temporal context), so the sweep is readable. The
 * CONTROL PLANE's lines do not: the orchestrator logs through Fastify's pino, whose records are
 * shipped and queryable but carry no run — they are HTTP request noise, true of every run at once
 * and therefore attributable to none.
 *
 * MEASURED on canary-1790684761, which is what put this file here. The run took 110 seconds: 29 in
 * `holdFleetLease`, 33 in `resolveBundle`, 42 sweeping, 5 publishing. For the first 62 of those —
 * more than half the run — the page could show nothing at all, because the actor did not exist yet
 * and nothing else was writing a line the rail could select. The step said "Hold the Fleet lease,
 * in flight" and the two counters said zero, which is a true and useless answer to "what is it
 * doing". A Fleet phase that says nothing is indistinguishable from a Fleet phase that is stuck.
 *
 * ── WHY A PLAIN JSON LINE ON STDOUT ─────────────────────────────────────────────────────────────
 *
 * `control/images/logline.py` already parses three shapes, and its last branch keeps EVERY field of
 * any other JSON object it is handed ("it is already structured, and dropping it to text would lose
 * more than guessing at its schema would gain"). So a stamped object needs no shipper change, no
 * new transport and no logger plumbed through the activity seam — the worker's stdout is already
 * followed for `kontra-api` and `kontra-infra`, which is where these activities run.
 *
 * ── IT NEVER THROWS, AND IT NEVER INVENTS A RUN ─────────────────────────────────────────────────
 *
 * `Context.current()` throws outside an activity, which is an ordinary situation here: these
 * functions are called directly by unit tests. That is caught, and the line is still written —
 * without `run_id`, because a line attributed to a run it did not come from is worse than one that
 * is merely unattributed. A logging call must not be able to fail the work it is describing.
 */

import { Context } from '@temporalio/activity';

/** The Run this activity belongs to, or `''` when there is no activity context. */
function currentRunId(): string {
  try {
    // The workflow id IS the Run id everywhere in this system — `runs.ts` and the console both
    // address a Run by it, and `queues.ts` derives workflow ids from it.
    return Context.current().info.workflowExecution?.workflowId ?? '';
  } catch {
    return '';
  }
}

export type RunLogLevel = 'debug' | 'info' | 'warn' | 'error';

/**
 * Write one run-attributed line.
 *
 * `phase` is what the rail groups on and what a reader scans for — keep it a short, stable noun
 * (`fleet`, `bundle`, `lease`), not a sentence. The message is the sentence.
 */
export function runLog(
  phase: string,
  msg: string,
  fields: Record<string, unknown> = {},
  level: RunLogLevel = 'info'
): void {
  try {
    const runId = currentRunId();
    const line: Record<string, unknown> = {
      ...fields,
      level,
      phase,
      msg,
      // `role` is what distinguishes these from an actor's lines on a shared rail. The actor's
      // carry `role: 'actor'` from its own identity filter.
      role: 'control',
      ...(runId ? { run_id: runId } : {}),
    };
    // eslint-disable-next-line no-console -- stdout IS the transport; see the module header.
    console.log(JSON.stringify(line));
  } catch {
    /* A line that cannot be written must never fail the activity it describes. */
  }
}
