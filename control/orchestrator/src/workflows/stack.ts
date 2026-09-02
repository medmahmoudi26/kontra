/**
 * The stack workflow — one per stack, and its id IS the stack fqn (ADR 0019).
 *
 * That identity is the whole point. Pulumi's DIY lock has no compare-and-swap and no TTL, so
 * it cannot be relied on to serialise writers: a killed process wedges the stack permanently
 * and a live one has no way to tell a stale lock from a healthy concurrent update. Temporal's
 * workflow-id uniqueness supplies what the lock cannot — with one workflow per fqn, a second
 * concurrent writer is structurally impossible, and the lock file becomes a backstop rather
 * than the mechanism.
 *
 * It also gives fleet provisioning what every other durable operation in kontra already has:
 * a server-minted id, a queryable status, retries, and cancellation that compensates.
 */

import * as wf from '@temporalio/workflow';
import type * as activities from '../activities/infra';
import type { StackOpResult } from '../activities/infra';

/** A ten-machine `up` takes minutes, and SSH readiness alone has historically taken up to
 * 7.5. The heartbeat is the liveness check; StartToClose is only a backstop. */
const { stackUp, stackPreview, stackDestroy } = wf.proxyActivities<typeof activities>({
  startToCloseTimeout: '60 minutes',
  heartbeatTimeout: '2 minutes',
  retry: { maximumAttempts: 3 },
});

/**
 * The credential preflight, on its own proxy because its budget is a file read and not a converge.
 *
 * NO HEARTBEAT and a short StartToClose: a check that needs sixty minutes is not a check. One
 * attempt, because the failures it reports — no such secret, every version revoked, a bad pin —
 * are not transient, and three identical refusals only delay the answer.
 */
const { checkCloudCredential } = wf.proxyActivities<typeof activities>({
  startToCloseTimeout: '30 seconds',
  retry: { maximumAttempts: 1 },
});

export type StackOp = 'up' | 'preview' | 'destroy';

export interface StackWorkflowInput {
  stackFqn: string;
  op: StackOp;
  /**
   * The stack program's arguments — and, since ADR 0034 §4, the NAME of the cloud credential this
   * converge uses (`args.credential`, a string or `{ name, version }`).
   *
   * A NAME, NEVER A VALUE. This field is an ordinary `Record<string, unknown>` that crosses into
   * workflow history, and the payload codec is a claim-check rather than encryption: anything
   * under 128 KiB rides inline in the clear for the namespace's whole retention, and a cloud token
   * is about seventy bytes. There is no size at which a credential is safely a workflow argument.
   * `secrets/history.test.ts` is the regression guard, and `infra/stacks.ts` keeps the name out of
   * `FleetArgs` so it cannot reach a Machine either.
   *
   * Absent means this control plane's default name (`infra/credential.ts:defaultCloudCredential`),
   * which is what lets `kontra fleet down` — a CLI with no `--credential` flag — tear down a fleet
   * it did not start.
   */
  args?: Record<string, unknown>;
  /**
   * Tear down what was built if this `up` does not finish.
   *
   * A provision that leaves machines running is both a billing event and an OPSEC one — fleet
   * machines reach hostile infrastructure, so an orphan nobody is tracking is worse than a
   * failure.
   *
   * NAMED FOR CANCELLATION AND NOT ONLY ABOUT IT. It was written when cancellation was the only
   * incomplete ending anybody had in mind, and it covers FAILURE too — see the catch below for
   * the measured reason. The name is kept because it is on the wire in every caller's history.
   */
  compensateOnCancel?: boolean;
}

/** Progress for the HTTP surface: the last heartbeat the activity emitted. */
export const getProgress = wf.defineQuery<Record<string, unknown>>('getProgress');

export async function stackWorkflow(input: StackWorkflowInput): Promise<StackOpResult> {
  let progress: Record<string, unknown> = { phase: 'starting', op: input.op };
  wf.setHandler(getProgress, () => progress);

  // The Run this converge serves. `stackWorkflow`'s own id is the STACK, so the caller's workflow
  // id is the only thing that says which run asked — and it is what the resolution ledger files
  // the credential read under (issue 20's `(actor, version, slot, run)`). Absent for a
  // `kontra fleet` command, which has no parent and is not a Run.
  const run = wf.workflowInfo().parent?.workflowId ?? '';
  const call = { stackFqn: input.stackFqn, args: input.args, run };

  /**
   * THE CREDENTIAL IS CHECKED BEFORE ANYTHING ELSE, AND OUTSIDE THE TRY (ADR 0034 §4).
   *
   * A missing or revoked cloud credential must fail at the START of `fleet.up()`, naming the
   * secret — not thirty lines into an install script on a Droplet, and not as a provider 401 four
   * minutes into a converge. Outside the `try` on purpose: nothing has been built, so there is
   * nothing to compensate, and a `destroy` fired here would fail on the SAME credential and bury
   * the message that says what to fix under a cleanup error.
   *
   * IT RUNS FOR `destroy` TOO. A teardown needs the credential exactly as a provision does, and a
   * revocation that leaves a fleet standing is the worse of the two failures — ADR 0034 records
   * that as open, and the answer this code gives is "say so immediately, by name". `kontra fleet
   * down` on a credential nobody can read fails in a second with a sentence, instead of retrying
   * a converge that cannot succeed.
   */
  // No new phase word: this IS `starting`, and the surface's vocabulary is exactly what the
  // workflow reports (`fleetPhase.ts`). A check that takes a second does not need one.
  await checkCloudCredential(call);

  try {
    progress = { phase: 'running', op: input.op };
    const res =
      input.op === 'preview'
        ? await stackPreview(call)
        : input.op === 'destroy'
          ? await stackDestroy(call)
          : await stackUp(call);
    progress = { phase: 'done', op: input.op, result: res.result, changes: res.changes };
    return res;
  } catch (err) {
    /**
     * THE SAGA LEG, and it runs on FAILURE as well as on cancellation.
     *
     * It used to be `isCancellation(err) && …`, which is the case somebody thought of rather
     * than the case that happens. MEASURED: a `fleet.up` that fails partway — a Pulumi error
     * after three of four Droplets exist, an install script that exits non-zero, a converge
     * that outruns its own timeout — landed here, matched nothing, and rethrew with the
     * Droplets still running. The caller's `async with` never opened, so ITS scope exit never
     * ran either: the one path where the machines outlive everything that knows about them was
     * the ordinary failure path, not the exotic one.
     *
     * `stackDestroy` is idempotent (Pulumi destroys the recorded state, and an `up` that
     * created nothing has none), so compensating a failure that happened before anything was
     * built costs one no-op activity — against Droplets nobody is tracking.
     *
     * NON-CANCELLABLE EITHER WAY. A cancelled workflow cannot schedule ordinary activities, so
     * the teardown has to run in a disconnected context or the compensation is cancelled along
     * with the thing it exists to compensate for. That is harmless on the failure path and
     * mandatory on the cancellation one.
     */
    if (input.op === 'up' && input.compensateOnCancel) {
      progress = {
        phase: 'compensating',
        op: 'destroy',
        // WHY it is tearing down, because the two are read very differently: an operator who
        // cancelled expects this, and one whose provision failed is finding out that it did.
        because: wf.isCancellation(err) ? 'cancelled' : 'failed',
      };
      await wf.CancellationScope.nonCancellable(async () => {
        // The provision's own error is the one worth raising. A teardown that also fails must
        // not replace it — that would report a destroy problem to somebody whose `up` broke,
        // and hide the cause. It is left in `progress` instead, where the run detail shows it.
        try {
          await stackDestroy(call);
        } catch (cleanup) {
          progress = { ...progress, phase: 'compensating', cleanupFailed: String(cleanup) };
        }
      });
    }
    throw err;
  }
}
