/**
 * kontra.v1 — the shared contract the orchestrator's API speaks.
 *
 * It used to be the GRAPH contract: nodes, edges, field mappings, merges and per-node failure
 * policy, all of it the argument to one interpreter workflow. There is no graph (ADR 0023 §12).
 * A **Run** is one execution of a caller's workflow, and what it dispatches is a **Method** call
 * on an **Actor** — expressed in the caller's own code, not in a document this server executes.
 * So what is left here is what the API still hands to a client: how a run's execution reads, and
 * the shape of a schema the catalog carries.
 */

/**
 * A JSON Schema (Draft 2020-12) document as authored, carried by the catalog so a client can
 * render an Actor's Method input (ADR 0003). Opaque here — any schema object.
 */
export type JsonSchemaDoc = Record<string, unknown>;

/**
 * Lifecycle status of a whole run's EXECUTION — "did the caller's workflow finish?".
 *
 * Temporal is its authority. Output readiness is a SECOND dimension: see `PublicLifecycle` in
 * src/data/materialization.ts, which the API exposes as a separate field alongside this one. Do
 * not add a member here for "output is still being written" — a non-terminal member would hang
 * every existing poller, and Temporal has no such state to reconcile from.
 */
export type RunStatus = 'pending' | 'running' | 'completed' | 'failed' | 'cancelled';

/**
 * One input Unit an Actor isolated INSTEAD of failing the Batch (ADR 0023 §13): the Unit
 * exhausted its retries or was non-retryable. It rides in the Method call's result envelope
 * alongside the committed results, which is what stops a Batch that dropped everything from
 * rendering identically to one that legitimately found nothing. Shape mirrors the Python SDK's
 * PerUnitFailure.
 */
export interface PerUnitFailure {
  /** The full offending input unit — the correlation key (no positional index, which
   * cannot be globally meaningful across a re-invoked Method and a reopened Session). */
  unit: unknown;
  error: { type: string; message: string };
  category: 'terminal' | 'exhausted';
}
