/**
 * A **Lease** — one **Run**'s claim on a **Fleet** — as names and grammar, with no I/O in it.
 *
 * ADR 0037: *"A **Lease** is one **Run**'s claim on a **Fleet**. **Machines** are destroyed when the
 * last **Lease** drops. Every **Lease** also expires on a clock, so a **Fleet** outlives neither its
 * holders nor a control plane that died holding it — which is strictly stronger than scope-exit
 * teardown, because scope exit never covered the control plane dying."*
 *
 * ── WHY THIS FILE HAS NO IMPORTS ─────────────────────────────────────────────────────────────────
 *
 * It is imported by WORKFLOW code (`workflows/lease.ts`), by ordinary Node code (`activities/lease.ts`,
 * `infraRoutes.ts`), and by a conformance driver. Workflow code runs in a `vm` context created EMPTY:
 * a module-scope `process.env` read anywhere in a workflow bundle's import graph throws
 * `ReferenceError: process is not defined` WHILE THE BUNDLE IS BEING IMPORTED, which fails the
 * workflow task for every type in that bundle and retries forever — a caller sees a run that never
 * progresses and no error anywhere. `queues.ts` carries the same warning and is the file that taught
 * it. So: constants and pure functions, and nothing that could grow an environment read.
 *
 * ── THE ONE FAILURE THIS WHOLE SLICE EXISTS TO PREVENT ────────────────────────────────────────────
 *
 * A **Lease** held under one spelling and dropped under another is a **Lease** that is never dropped,
 * and a **Lease** that is never dropped is **Machines** that bill until a human notices. There is no
 * loud failure: the hold succeeds, the drop succeeds, and the **Lease** workflow keeps a holder nobody can find.
 * `shared/conformance/lease.json` is what stops the two spellings drifting, and {@link parseLeaseId} exists
 * so that this side READS the grammar the caller SDK writes rather than merely storing it.
 */

/**
 * The workflow type of the **Lease** workflow. One per **Fleet**, long-lived, and BLOCKED between signals — the
 * same shape ADR 0037 gives the **Warden**, for the same reason: a workflow that waits on a condition
 * writes nothing while it waits.
 */
export const LEASE_WORKFLOW = 'fleetLeaseWorkflow';

/**
 * A **Lease** workflow's workflow id is `kontra-lease/` + the stack fqn it guards.
 *
 * DERIVED, LIKE THE FLEET'S OWN NAME. Nothing allocates it, so a hold from a **Run** that has never
 * heard of this **Fleet** reaches the same **Lease** workflow as the hold from the **Run** that created it, with
 * nothing to look up. It is NOT the stack fqn itself, because that id already belongs to
 * `stackWorkflow` (ADR 0019: the workflow id IS the stack, which is what serialises Pulumi writers)
 * and a **Lease** workflow that collided with a converge would be a hold that fails while a fleet is coming up.
 */
export const LEASE_WORKFLOW_ID_PREFIX = 'kontra-lease/';

/** Signals. Dotted and namespaced, as `kontra.warden.retire` is. */
export const LEASE_HOLD_SIGNAL = 'kontra.lease.hold';
export const LEASE_DROP_SIGNAL = 'kontra.lease.drop';

/**
 * The query. Named apart from `getProgress` (`stackWorkflow`) and `getSessionState`
 * (`tmuxSessionWorkflow`) because all three are registered into ONE workflow bundle, where two
 * workflows exporting the same name is a bundling error rather than a routing one.
 */
export const LEASE_QUERY = 'getLeases';

/** The two activity names the caller SDK schedules by literal. Their peers are in `kontra.fleet`. */
export const HOLD_ACTIVITY = 'holdFleetLease';
export const DROP_ACTIVITY = 'dropFleetLease';

/**
 * How long a **Lease** lives without being renewed, when the holder does not say.
 *
 * ONE HOUR, AND THE NUMBER IS A TRADE STATED RATHER THAN TUNED. It bounds a leak: a control plane
 * that dies holding a **Fleet** costs at most an hour of **Machines** past the moment a worker polls
 * the infra queue again. It is also the window a `destroy_on_exit=False` handoff has to be picked up
 * in — see `sdk/python/kontra/fleet.py`, where adopting a **Fleet** now means "for one TTL" rather
 * than "for ever", which is what ADR 0037's *"expires on a clock"* costs and buys.
 *
 * A LIVE HOLDER IS NEVER REAPED BY IT. Expiry is a CHECK, not a deadline: when it fires, the **Lease** workflow
 * asks Temporal whether each expired **Lease**'s holder is still running and renews the ones that
 * are. So this is not "the longest a Run may take" — it is "how long a Fleet survives a holder
 * nobody can account for". A holder-side heartbeat would have made it the former, and would have put
 * a timer loop in every caller's history for the privilege.
 */
export const LEASE_TTL_MS = 60 * 60 * 1000;

/**
 * How many consecutive UNKNOWN liveness answers a **Lease** survives before it is dropped anyway.
 *
 * UNKNOWN IS NOT DEAD, AND IT IS NOT ALIVE EITHER. `queuePollers` records the same distinction from
 * the other side — "0 because we could not ask is not 0 because nothing is polling" — and the
 * consequence here is opposite in cost: reading unknown as DEAD destroys a healthy **Run**'s
 * **Machines**, and reading it as ALIVE for ever is the leak this program must not introduce. So
 * unknown BUYS TIME and is counted, and a **Lease** workflow that has been unable to answer three expiries
 * running stops paying for a holder it cannot find. Three expiries is three TTLs, not three seconds.
 */
export const LEASE_UNKNOWN_LIMIT = 3;

/** The **Lease** workflow's workflow id for one stack. See {@link LEASE_WORKFLOW_ID_PREFIX}. */
export function leaseWorkflowId(stackFqn: string): string {
  return `${LEASE_WORKFLOW_ID_PREFIX}${stackFqn}`;
}

/**
 * A **Lease** id: the holder, a `#`, and a nonce.
 *
 * THE HOLDER IS IN THE ID ON PURPOSE. A **Lease** is the only thing standing between a shared
 * **Fleet** and its teardown, so the first question about a **Fleet** that will not die is *who is
 * holding it* — and an opaque uuid answers that with a second lookup that a leaked **Lease** workflow may not
 * survive to serve. `kontra fleet leases` prints these verbatim.
 *
 * THE NONCE IS WHAT MAKES IT A LEASE AND NOT A HOLDER. One **Run** may open two scopes on one
 * **Fleet** (a retry, a nested `async with`), and if both claimed the id `<run>` the inner scope's
 * exit would drop the outer scope's claim and destroy **Machines** the outer scope is still using.
 * It comes from the caller's `workflow.uuid4()`, which is deterministic under replay.
 */
export function leaseId(holder: string, nonce: string): string {
  return `${holder}#${nonce}`;
}

/**
 * Read a **Lease** id back into its holder and nonce — the READER half of the grammar above.
 *
 * SPLIT ON THE LAST `#`, NOT THE FIRST. A Temporal workflow id may contain almost anything, `#`
 * included; the nonce is generated here-shaped and never contains one, so the last separator is the
 * only one that is structurally the separator. Splitting on the first would report a holder called
 * `run` for a **Run** genuinely named `run#7`.
 *
 * An id with no `#` at all is a holder with no nonce rather than an error: this parses what a **Lease** workflow
 * is HOLDING, and a **Lease** workflow that refuses to describe an entry it cannot parse is one that goes blank
 * exactly when somebody needs to know who to call.
 */
export function parseLeaseId(id: string): { holder: string; nonce: string } {
  const at = id.lastIndexOf('#');
  return at < 0 ? { holder: id, nonce: '' } : { holder: id.slice(0, at), nonce: id.slice(at + 1) };
}

/** One entry in the **Lease** workflow, as the query and the HTTP surface render it. */
export interface LeaseView {
  /** The full id, exactly as it was held and exactly as it must be dropped. */
  lease: string;
  /**
   * The Run holding it — its Temporal workflow id — or empty for a **Lease** nobody can be asked
   * about. An unattributed **Lease** is never renewed by the liveness check, so it is precisely a
   * claim with a deadline and no appeal: `destroy_on_exit=False` takes one.
   */
  holder: string;
  /** When it lapses if nothing renews it. Milliseconds since the epoch, so it survives JSON. */
  expiresAt: number;
}

/** What the **Lease** workflow answers, and what `GET /api/infra/stacks/:fqn/leases` returns. */
export interface FleetLeaseSet {
  /** The stack these **Leases** are on — `kontra-fleet/<actor>-<version>`. */
  fleet: string;
  leases: LeaseView[];
  /**
   * Set once the **Lease** workflow has run the teardown. A **Lease** workflow only reaches this at zero **Leases**, so it
   * is also the answer to "was this **Fleet** destroyed by the last one out, or by a hand?".
   */
  destroyed?: boolean;
}
