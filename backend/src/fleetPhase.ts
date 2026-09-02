/**
 * The phase a Fleet child reports about ITSELF, read for the Run that started it.
 *
 * WHY THE RUN SURFACE ASKS AT ALL. MEASURED on run `nscheck-1786831339` (294 s): for 156 of those
 * seconds — 53% of the run, and the entire period before any work could exist — the console said
 * "nothing dispatched yet", while `kontra-fleet/dns` sat there answering `{phase, op}` to anyone who
 * asked it. Nobody asked. This is the asking.
 *
 * IT IS THE CHILD'S OWN REPORT AND NOTHING ELSE. Fleet supplies the machines Execution dispatches
 * to and the handoff is one-way and narrow (CONTEXT-MAP.md): nothing here lets Execution infer a
 * **Machine's** health from a **Run**, or Fleet judge a **Machine** from one. What crosses is a word
 * the Fleet workflow said about its own operation, shown inside a **Run** because it happened
 * DURING that Run — a phase of the run, not a verdict about hardware.
 *
 * TWO FIELDS LEAVE THIS MODULE AND NO MORE. `stackWorkflow`'s progress also carries Pulumi's
 * `result` and its per-operation `changes` counts once it is `done`. This route is open like the
 * rest of the run surface, while `/api/infra/*` has been token-gated since its first commit
 * precisely because an inventory of what a fleet operation created is what an attacker wants first.
 * A phase and an operation name are what the run detail needs; the counts stay behind that token.
 *
 * NOTHING HERE READS A PAYLOAD OF THE RUN'S HISTORY. The window's duration and the child's id come
 * from the reduced event log the console already fetched (`history.ts`, `EventLink` and `dur`) —
 * this adds one query, not a second history read.
 */

import { WorkflowNotFoundError } from '@temporalio/client';
import { getClient } from './temporalClient';

/**
 * The query `stackWorkflow` answers, spelled independently of it.
 *
 * A THIRD SPELLING OF ONE STRING, per the decoupling rule that already governs `RunBatch` in
 * temporalClient.ts — and with the same failure mode: a rename over there does not break a build
 * over here, it just makes every window read "no phase reported". `fleetPhase.test.ts` asserts this
 * constant against `workflows/stack.ts` itself so the miss cannot be silent.
 */
export const PHASE_QUERY = 'getProgress';

/**
 * How long the child gets to answer.
 *
 * A query needs a **Worker** polling the infra queue to serve it — and for a CLOSED child, one that
 * will replay its history first. With no worker there, Temporal holds the call until its own query
 * timeout, which is far longer than a 2-second browser poll should ever wait. Timing out here turns
 * a hung request into a sentence the surface can print.
 */
export const PHASE_TIMEOUT_MS = 2_500;

/** What one Fleet child says about itself. Every field optional but the id: this type is also the
 *  shape of "it said nothing", which is an answer the run detail must be able to render. */
export interface FleetPhase {
  /** The child's workflow id — `kontra-fleet/dns`. */
  workflowId: string;
  /** Temporal's run id for the execution asked. NEVER called a run id: a **Run** is identified by
   *  its caller workflow's id (ADR 0023 §12), and this is the other thing Temporal calls one. */
  execId?: string;
  /** The phase the child reported. Today `stackWorkflow` reports one of `starting`, `running`,
   *  `done` or `compensating` — whatever it says is what appears, because the surface must not
   *  outgrow what the workflow actually reports. */
  phase?: string;
  /** The operation it reported being on: `up`, `preview` or `destroy`. This is the ONE fact the
   *  run's history cannot supply — a `StackOp` travels in the child's input, which is a payload the
   *  event log never decodes (ADR 0007). */
  op?: string;
  /** Why there is no phase, when there is none. A reason, never a phase word. */
  unavailable?: string;
}

/**
 * The two fields, out of whatever the child answered with.
 *
 * Pure, and strict about types: a `phase` that is not a string is not a phase, and printing
 * `[object Object]` under a run would be exactly the plausible-looking default this plane exists to
 * refuse. An empty answer yields an empty object rather than an invented `unknown`.
 */
export function phaseOf(progress: unknown): { phase?: string; op?: string } {
  const bag = (progress ?? {}) as Record<string, unknown>;
  const out: { phase?: string; op?: string } = {};
  if (typeof bag.phase === 'string' && bag.phase) out.phase = bag.phase;
  if (typeof bag.op === 'string' && bag.op) out.op = bag.op;
  return out;
}

/**
 * Ask one Fleet child what it is doing.
 *
 * `execId` PINS THE EXECUTION and the caller is expected to supply it: `kontra-fleet/dns` is the
 * workflow id of every bring-up AND every teardown, of this run and of the last one, so a query by
 * bare id answers with whichever ran most recently — a plausible wrong answer, which is worse than
 * no answer. `frontend/src/run/api.ts:fetchFleetPhase` says the same thing on the browser's side;
 * the parameter stays optional only because the underlying handle's is.
 *
 * NEVER THROWS. "The child could not be asked" is a thing this surface must PRINT beside a window
 * whose duration the run's history already knows — an exception here would blank all of it.
 */
export async function readFleetPhase(workflowId: string, execId?: string): Promise<FleetPhase> {
  const base: FleetPhase = { workflowId, ...(execId ? { execId } : {}) };
  try {
    const client = await getClient();
    const handle = client.workflow.getHandle(workflowId, execId);
    const progress = await withTimeout(
      handle.query<Record<string, unknown>, []>(PHASE_QUERY),
      PHASE_TIMEOUT_MS
    );
    return { ...base, ...phaseOf(progress) };
  } catch (err) {
    return { ...base, unavailable: whyNot(err) };
  }
}

/** The marker a timed-out query rejects with, so {@link whyNot} can tell it from a real error. */
class PhaseTimeout extends Error {}

/**
 * Why the child said nothing, in words an operator can act on.
 *
 * The three cases are genuinely different and are never collapsed: retention dropped the execution
 * (nothing to do, history still has the window), nobody is polling the queue (start the infra
 * worker), or something else went wrong (read the message).
 */
function whyNot(err: unknown): string {
  if (err instanceof PhaseTimeout) {
    return `no answer in ${PHASE_TIMEOUT_MS / 1000}s — no Worker is polling the infra queue, or it is busy replaying`;
  }
  if (err instanceof WorkflowNotFoundError || (err as { code?: unknown } | null)?.code === 5) {
    return 'Temporal has no such execution — a child is dropped for retention on its own schedule';
  }
  return err instanceof Error ? err.message : String(err);
}

/** Race a promise against a clock. The loser gets a no-op handler so a late rejection is not an
 *  unhandled one, and the timer is always cleared — a poll every two seconds must not accumulate
 *  either. */
async function withTimeout<T>(promise: Promise<T>, ms: number): Promise<T> {
  void promise.catch(() => {
    /* the race has already reported; this only stops an unhandled rejection */
  });
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      promise,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new PhaseTimeout()), ms);
      }),
    ]);
  } finally {
    if (timer) clearTimeout(timer);
  }
}
