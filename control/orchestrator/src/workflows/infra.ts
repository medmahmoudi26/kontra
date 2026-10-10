/**
 * The `INFRA_QUEUE` workflow bundle — everything the infra worker may run (ADR 0019, ADR 0020).
 *
 * `infra.ts` used to resolve `./workflows/stack` directly, which registered exactly one workflow.
 * It carried `tmuxSessionWorkflow` beside the stack — session existence on a Machine, on this queue
 * because only that process holds `KONTRA_SSH_KEY` — until the Monitor was deleted. No logic lives
 * here either way: this module only re-exports, so the stack path is byte-identical to what it was.
 *
 * Re-exporting is not free of hazards — two workflows in one bundle must not export the same name.
 * `stackWorkflow`'s query is `getProgress`; keep any new one distinct from it.
 */

export { stackWorkflow, getProgress } from './stack';
export type { StackOp, StackWorkflowInput } from './stack';

// The retention sweep (ADR 0029 §5) is hosted here because this is the controller-pinned workflow
// host, and the sweep must run on the controller where the lake and the stores live. It exports no
// query, so it cannot collide with `getProgress`. Its one activity is proxied onto `DATASET_QUEUE`,
// not run here.
export { serveDevWorkflow } from './serveDev';

// The BUILD channel (`activities/buildActor.ts`), on this queue for the reason serve-dev is: this
// is the process holding the Docker socket. It exports no query, so it cannot collide with
// `getProgress`.
export { buildActorWorkflow } from './buildActor';

export { sweepDatasetsWorkflow } from './retention';
export type { SweepDatasetsWorkflowInput } from './retention';

// The **Lease** **Lease** workflow (ADR 0037). Controller-pinned for the reason `infra/CONTEXT.md` gives —
// *"Cloud authority — creating and destroying **Machines** — belongs to the **Controller** alone"* —
// and this is the workflow that decides when a **Fleet** is destroyed. Its query is `getLeases`,
// named apart from `getProgress` and `getSessionState` because a bundle may not export a name twice.
export { fleetLeaseWorkflow, holdLease, dropLease, getLeases } from './lease';
// THE FLEET POOL (ADR 0066): one per profile, the single writer of a Kubernetes fleet.
export { kontraFleetPoolWorkflow } from './fleetPool';
export type { FleetLeaseInput, HoldSignal, DropSignal } from './lease';
