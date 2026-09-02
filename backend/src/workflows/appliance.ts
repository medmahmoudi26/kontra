/**
 * The APPLIANCE's `kontra-infra` bundle — the same queue, minus the provisioner (ADR 0031 §4).
 *
 * `workflows/infra.ts` is the compose controller's bundle and is unchanged. This is the one a
 * process with no Pulumi engine, no state directory, no passphrase and no cloud credential
 * registers, and the difference between them is one workflow — the same three TYPES, one of which
 * refuses.
 *
 * WHY THE TYPE IS STILL REGISTERED, which is the whole point of this file. Leaving `stackWorkflow`
 * out does not make `fleet.up()` fail, it makes it HANG: `actorkit.fleet.up()` starts the child on
 * this queue, this worker takes the workflow task, finds no such type, and FAILS THE TASK — which
 * Temporal retries, forever. The run shows a workflow that never progresses and no error
 * anywhere, which is exactly the invisible failure ADR 0031 §4 says must not be how the appliance
 * says "no provisioner here". So: register it, refuse in one second, name the limitation.
 *
 * THE TWO THAT STAY ARE NOT PROVISIONING (ADR 0034 §2, quoting ADR 0031 §4). `tmuxSessionWorkflow`
 * is session existence on a Machine — Fleet authority, and what the Monitor's panes are built on;
 * the appliance's case for it is the LOCAL pane, a served worker's `kontra-wf-<queue>` session on
 * the operator's own host, which needs neither the SSH key nor a cloud token.
 * `sweepDatasetsWorkflow` is the retention sweep, hosted here because this is the controller-pinned
 * workflow host, with its one activity proxied onto `DATASET_QUEUE`.
 *
 * KEEP THIS MODULE'S IMPORT GRAPH AS THIN AS ITS PEERS'. Everything reachable from here is bundled
 * into the workflow sandbox, where there is no filesystem, no client and no Node built-in — and a
 * non-bundleable import does not error, it makes bundling HANG. The `./stack` import below is
 * `import type`, erased at compile time, so no line of the provisioner reaches this bundle; that is
 * load-bearing, not tidiness.
 */

import { ApplicationFailure } from '@temporalio/common';
import type { StackWorkflowInput } from './stack';

export { tmuxSessionWorkflow, recreate, kill, addWindow, getSessionState } from './tmuxSession';
export type { TmuxSessionInput, TmuxSessionResult } from './tmuxSession';

export { sweepDatasetsWorkflow } from './retention';
export type { SweepDatasetsWorkflowInput } from './retention';

/**
 * THE **LEASE** LEDGER IS THE REAL ONE HERE, NOT A REFUSAL, and the asymmetry with `stackWorkflow`
 * above is the point. A **Lease** workflow holds no credential and converges nothing: it counts claims and, at
 * zero, asks `stackWorkflow` to destroy — which on this bundle refuses with the sentence above. So
 * an appliance can track who is holding self-enrolled capacity (ADR 0037's *"the appliance gains a
 * **Fleet** without gaining a cloud credential"*) and the one operation it cannot perform fails
 * where it was always going to fail, naming the reason. Registering a refusal HERE instead would
 * make `fleet.up()` fail at the FIRST line of the scope on an appliance that is perfectly capable of
 * running the rest of it.
 */
export { fleetLeaseWorkflow, holdLease, dropLease, getLeases } from './lease';
export type { FleetLeaseInput, HoldSignal, DropSignal } from './lease';

/** The stack workflow's input shape is unchanged — this bundle refuses it, it does not redefine it. */
export type { StackOp, StackWorkflowInput } from './stack';

/**
 * The `type` field on the failure, so a caller can branch on the refusal without matching prose.
 * `fleet.up()` surfaces it through `ChildWorkflowFailure.cause`.
 */
export const NO_PROVISIONER = 'NoProvisioner';

/**
 * What an operator reads when they ask an appliance for Machines.
 *
 * IT NAMES THE FIX, not just the fact. "This control plane has no provisioner" on its own reads as
 * a bug; the second sentence is what makes it a deployment choice somebody can act on. `kontra
 * fleet` is an HTTP client (`cli/fleet.go`), so pointing it at a compose controller is the whole
 * migration — one binary addresses either control plane (ADR 0034 §1).
 */
export const NO_PROVISIONER_MESSAGE =
  'this control plane has no provisioner: `kontra up` ships no Pulumi engine, no provider plugin ' +
  'and no cloud credential (ADR 0031 §4), so it cannot converge cloud Machines. Run the compose ' +
  'controller for a cloud fleet and point `kontra fleet` / KONTRA_ORCHESTRATOR_URL at it — the ' +
  'same command drives either control plane.';

/**
 * `stackWorkflow`, registered and refusing.
 *
 * NON-RETRYABLE AND IMMEDIATE. There is no retry policy on the child `fleet.up()` starts, but the
 * flag is set anyway: a caller who adds one must not turn a permanent deployment fact into a loop,
 * and "no provisioner" will not become true on the third attempt.
 *
 * IT REFUSES `destroy` TOO, and that is the right answer rather than a gap. An appliance never
 * built the fleet it is being asked to tear down — it could not — so there is nothing here to
 * compensate; the Machines, if any exist, belong to the compose controller that made them and are
 * torn down from there.
 */
export async function stackWorkflow(input: StackWorkflowInput): Promise<never> {
  throw ApplicationFailure.nonRetryable(
    `fleet ${input.op} ${input.stackFqn}: ${NO_PROVISIONER_MESSAGE}`,
    NO_PROVISIONER
  );
}
