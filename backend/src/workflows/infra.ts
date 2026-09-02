/**
 * The `INFRA_QUEUE` workflow bundle — everything the infra worker may run (ADR 0019, ADR 0020).
 *
 * `infra.ts` used to resolve `./workflows/stack` directly, which registered exactly one workflow.
 * Session existence is now a Temporal operation on the SAME queue (only that process holds
 * `KONTRA_SSH_KEY`, and creating a session on a Machine is Fleet authority), so the worker needs a
 * module that exports both. This is that module and nothing else: no logic lives here, so the
 * existing stack path is byte-identical to what it was.
 *
 * Re-exporting is not free of hazards — two workflows in one bundle must not export the same name.
 * `stackWorkflow`'s query is `getProgress`; `tmuxSessionWorkflow`'s is `getSessionState`, named
 * apart for exactly this reason.
 */

export { stackWorkflow, getProgress } from './stack';
export type { StackOp, StackWorkflowInput } from './stack';

export { tmuxSessionWorkflow, recreate, kill, addWindow, getSessionState } from './tmuxSession';
export type { TmuxSessionInput, TmuxSessionResult } from './tmuxSession';

// The retention sweep (ADR 0029 §5) is hosted here for the same reason `tmuxSessionWorkflow` is: this
// is the controller-pinned workflow host, and the sweep must run on the controller where the lake and
// the stores live. It exports no query, so it cannot collide with `getProgress`/`getSessionState`. Its
// one activity is proxied onto `DATASET_QUEUE`, not run here.
export { sweepDatasetsWorkflow } from './retention';
export type { SweepDatasetsWorkflowInput } from './retention';

// The **Lease** **Lease** workflow (ADR 0037). Controller-pinned for the reason `infra/CONTEXT.md` gives —
// *"Cloud authority — creating and destroying **Machines** — belongs to the **Controller** alone"* —
// and this is the workflow that decides when a **Fleet** is destroyed. Its query is `getLeases`,
// named apart from `getProgress` and `getSessionState` because a bundle may not export a name twice.
export { fleetLeaseWorkflow, holdLease, dropLease, getLeases } from './lease';
export type { FleetLeaseInput, HoldSignal, DropSignal } from './lease';
