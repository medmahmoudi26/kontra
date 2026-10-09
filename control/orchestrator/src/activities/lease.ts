/**
 * The three I/O halves of a **Lease** (ADR 0037), all of them on the CALLER's queue.
 *
 *   holdFleetLease   a **Run** claims a **Fleet** — signal-with-start onto the **Lease** workflow
 *   dropFleetLease   a **Run** lets go
 *   runningHolders   is a **Lease**'s holder still there? asked by the **Lease** workflow at an expiry
 *
 * WHY ACTIVITIES AT ALL, for the first two. A workflow can signal an EXISTING workflow
 * (`getExternalWorkflowHandle`) and can start a CHILD, and neither is what a **Lease** needs: the
 * **Lease** workflow must be started by whichever **Run** gets there first and must OUTLIVE all of them, which
 * is signal-with-start — a client call, and therefore an activity. Making it a child instead would
 * put the **Lease** workflow's lifetime back inside one **Run**, which is the coupling this slice removes.
 *
 * WHY NOT THE INFRA QUEUE, where the **Lease** workflow itself runs: `activities/fleet.ts` gives the
 * disqualifying reason and it applies unchanged here. `orchestrator-infra` runs
 * `maxConcurrentActivityTaskExecutions: 1` because a Pulumi engine forks a CLI child plus one per
 * provider on a memory-capped container. A hold posted there would sit behind a sixty-minute
 * converge — and a **Run** would block at the top of `fleet.up()` for an hour with no fleet of its
 * own in flight. These three are one gRPC call each and hold no credential.
 */

import { Client, Connection, WorkflowNotFoundError } from '@temporalio/client';
import { runLog } from './runLog';

import { dataConverter } from '../codec/dataConverter';
import {
  LEASE_DROP_SIGNAL,
  LEASE_HOLD_SIGNAL,
  LEASE_QUERY,
  LEASE_WORKFLOW,
  leaseWorkflowId,
  type FleetLeaseSet,
} from '../lease';
import { datasetQueue, infraQueue } from '../queues';
import { temporalConnectOptions } from '../temporalTls';
import { registerSearchAttributes, tenantAttributes } from '../visibility';
import { activityNamespace } from '../activityNamespace';

export interface HoldFleetLeaseInput {
  /** The stack being held — `kontra-fleet/<actor>-<version>`. The **Lease** workflow's id derives from it. */
  stackFqn: string;
  /** The **Lease** id, built by the caller: `<holder>#<nonce>`. */
  lease: string;
  /** The **Run**'s workflow id, or empty for an unattributed claim that only the clock ends. */
  holder?: string;
  /** Milliseconds. Absent takes the control plane's default (`lease.ts:LEASE_TTL_MS`). */
  ttlMs?: number;
  /** The NAME of the credential the eventual teardown uses. Never a value. */
  credential?: string;
}

export interface HoldFleetLeaseOutput {
  /** The **Lease** workflow's workflow id — worth returning because it is the handle for `kontra fleet leases`. */
  workflowId: string;
  /** The **Lease** that is now held, echoed back from the **Lease** workflow rather than from the request. */
  lease: string;
  /**
   * HOW MANY **LEASES** ARE ON THIS **FLEET**, INCLUDING THIS ONE. One means the caller is alone on
   * it; more means the **Fleet** is shared, and `kontra.fleet` uses exactly that to decide whether
   * its own failure may tear the **Fleet** down. A saga leg is only yours while the **Fleet** is.
   */
  leases: number;
  /** When this claim lapses if nothing renews it. Milliseconds since the epoch. */
  expiresAt: number;
}

export interface DropFleetLeaseInput {
  stackFqn: string;
  lease: string;
}

export interface DropFleetLeaseOutput {
  workflowId: string;
  /** False when there was no **Lease** workflow to tell — a **Fleet** with nothing held, which is what the
   *  caller wanted to be true. Not an error: see {@link dropFleetLease}. */
  delivered: boolean;
}

export interface HoldersAliveInput {
  /** Temporal workflow ids. Never empty — the **Lease** workflow does not ask about nothing. */
  holders: string[];
}

export interface HoldersAliveOutput {
  /** Still RUNNING. Their **Leases** are renewed. */
  alive: string[];
  /** Closed, or no such workflow. Their **Leases** drop. */
  gone: string[];
  /**
   * Could not be established. NOT the same as `gone`, and the **Lease** workflow treats it as neither — see
   * `lease.ts:LEASE_UNKNOWN_LIMIT`. `queuePollers` carries the same distinction for the same reason:
   * an answer nobody could get is not a measurement.
   */
  unknown: string[];
}

/**
 * A client, dialled per call and closed again.
 *
 * THE SAME SHAPE `infra.ts:armRetention` USES, and for the same reason: these run on a worker whose
 * own connection is a `NativeConnection` (the Rust core's), and signal-with-start lives on
 * `@temporalio/client`'s gRPC-JS `Connection`. There is no way to share one. Three calls a **Run**
 * makes at most a handful of times is not the cost worth pooling for, and a pooled client is a
 * connection this process holds open for a workflow that may never come back.
 *
 * The repo's `dataConverter`, so the **Lease** workflow's arguments are encoded by the codec its worker decodes
 * with — a signal payload that skipped it would be undecodable on the other side.
 */
async function withClient<T>(
  injected: Client | undefined,
  fn: (client: Client) => Promise<T>
): Promise<T> {
  // ONLY CLOSE WHAT WE OPENED — `queuePollers` carries the same line about its injected describer,
  // and it is the same arrangement: a client passed in belongs to the caller, and closing it would
  // take the test's own connection down mid-suite.
  if (injected) return fn(injected);
  const address = process.env.KONTRA_ADDRESS ?? 'localhost:7233';
  // The CALLER'S namespace, not the install's: see `activityNamespace.ts`.
  const namespace = activityNamespace();
  const connection = await Connection.connect(temporalConnectOptions({ address }));
  try {
    // REGISTERED BEFORE THE FIRST STAMPED START, AND THIS IS NOT BOOKKEEPING.
    //
    // `holdFleetLease` stamps `KontraTenant` at start. Temporal HARD-ERRORS on an attribute the
    // namespace has no mapping for — "Namespace default has no mapping defined for search attribute
    // KontraTenant" — so on a namespace where nothing had registered it, adding the stamp turned a
    // working Lease hold into a failed one. Caught by `lease.test.ts`, which drives a real ephemeral
    // server; it would otherwise have surfaced on a fresh install, at the moment a Run first claimed
    // a Fleet.
    //
    // THIS CLIENT IS THE REASON IT WAS MISSING. `temporalClient.ts:getClient` registers on connect,
    // and this activity deliberately does NOT use it (see the header: a NativeConnection cannot do
    // signal-with-start). A second client is a second place the namespace has to be prepared.
    //
    // Idempotent, self-healing and it never throws — see `registerSearchAttributes`. Memoised per
    // namespace so this costs one round trip per worker process, not one per hold.
    await ensureAttributes(connection, namespace);
    return await fn(new Client({ connection, namespace, dataConverter }));
  } finally {
    await connection.close().catch(() => undefined);
  }
}

/** Namespaces this process has already prepared. Per-process, which is the lifetime that matters:
 *  a worker restart re-registering four attributes is four no-ops. */
const prepared = new Set<string>();

async function ensureAttributes(connection: Connection, namespace: string): Promise<void> {
  if (prepared.has(namespace)) return;
  await registerSearchAttributes(connection, namespace);
  prepared.add(namespace);
}

/**
 * Claim a **Fleet**.
 *
 * SIGNAL-WITH-START, so the first **Run** to arrive creates the **Lease** workflow and every later one signals
 * the same **Lease** workflow with nothing to look up — the id is derived from the stack (`leaseWorkflowId`).
 *
 * AND THEN A READ, WHICH IS THE PART THAT IS NOT DECORATION. A hold that arrives after the **Lease** workflow
 * has committed to teardown is refused by silence (`workflows/lease.ts` explains the race), so the
 * only way to know a claim was actually taken is to ask the **Lease** workflow what it is holding. If this
 * **Lease** is not in the answer, the activity FAILS and Temporal retries it — by which time the old
 * **Lease** workflow has closed and the retry's signal-with-start opens a new one. Without this read a **Run**
 * would converge onto **Machines** that a **Lease** workflow was in the middle of deleting, and nothing would
 * report it.
 */
export async function holdFleetLease(
  input: HoldFleetLeaseInput,
  client?: Client
): Promise<HoldFleetLeaseOutput> {
  if (!input?.stackFqn) throw new Error('holdFleetLease: no stackFqn — a Lease is a claim on a Fleet');
  if (!input?.lease) throw new Error('holdFleetLease: no lease id');
  const workflowId = leaseWorkflowId(input.stackFqn);
  // THE RUN PAGE HAS NOTHING ELSE TO SHOW YET. This is the first activity of a Fleet run and it
  // held for 29 seconds on canary-1790684761 while the rail sat empty — see `runLog.ts`.
  runLog('fleet', `claiming capacity on ${input.stackFqn}`, { stack: input.stackFqn, lease: input.lease });

  return withClient(client, async (client) => {
    const start = (stamp: boolean) => client.workflow.signalWithStart(LEASE_WORKFLOW, {
      taskQueue: infraQueue(),
      workflowId: workflowId,
      // Whose Lease this is, stamped at start — see `tenantAttributes`. On a signalWithStart this
      // rides the START half only, so an existing Lease that is merely being signalled is not
      // re-stamped and writes no extra event, which is the behaviour this workflow's own
      // event-budget comment asks for.
      //
      // BEST-EFFORT, AND THAT IS THE WHOLE POINT — see `withoutAttributes` below.
      ...(stamp
        ? { typedSearchAttributes: tenantAttributes(client.options.namespace) }
        : {}),
      args: [
        {
          stackFqn: input.stackFqn,
          // WHERE THE LIVENESS CHECK RUNS, resolved HERE — ordinary Node code that may read the
          // environment — and carried into the **Lease** workflow's history as input. The **Lease** workflow cannot read it
          // for itself: `workflows/retention.ts` records what a queue constant baked into workflow
          // code cost when a second control plane needed its own.
          livenessQueue: datasetQueue(),
        },
      ],
      signal: LEASE_HOLD_SIGNAL,
      signalArgs: [
        {
          lease: input.lease,
          holder: input.holder ?? '',
          ...(input.ttlMs ? { ttlMs: input.ttlMs } : {}),
          ...(input.credential ? { credential: input.credential } : {}),
        },
      ],
    });

    /*
     * A LEASE HOLD MUST NEVER FAIL BECAUSE OF A VISIBILITY INDEX.
     *
     * Temporal HARD-ERRORS on an attribute the namespace has no mapping for — `3 INVALID_ARGUMENT:
     * Namespace default has no mapping defined for search attribute KontraTenant`. Adding the stamp
     * therefore turned a working hold into a failed one on any namespace where nothing had
     * registered it: a fresh install, a second control plane, or an ephemeral test server.
     *
     * AND A FAILED HOLD IS NOT A COSMETIC FAILURE. A Fleet nobody holds is a Fleet the sweep tears
     * down (ADR 0037), so this would have turned a missing INDEX into destroyed Machines and a Run
     * that cannot place work. `tenantOf` already derives the tenant from the namespace at READ time,
     * which is what makes the attribute a convenience rather than the authority — so the correct
     * behaviour when it cannot be written is to carry on without it.
     *
     * `registerSearchAttributes` in `withClient` prevents this for a client we dial ourselves. This
     * covers the one we do not: an INJECTED client belongs to its caller, and a caller that has not
     * prepared the namespace is not a caller whose Lease should break.
     */
    const handle = await start(true).catch(async (err: unknown) => {
      if (!isUnmappedSearchAttribute(err)) throw err;
      console.warn(
        `[lease] ${leaseWorkflowId(input.stackFqn)}: this namespace has no KontraTenant mapping, ` +
          `so the hold is recorded without it. The tenant is still derived at read time ` +
          `(temporalClient.ts:tenantOf); run the control plane once to register the attributes.`
      );
      return start(false);
    });

    const held = (await handle.query(LEASE_QUERY)) as FleetLeaseSet;
    const mine = (held?.leases ?? []).find((l) => l.lease === input.lease);
    if (!mine) {
      // RETRYABLE ON PURPOSE. The one way to reach this is the teardown race, and it resolves
      // itself: the **Lease** workflow closes, and the next attempt's signal-with-start opens a fresh one.
      throw new Error(
        `the Lease workflow for ${input.stackFqn} did not take ${input.lease} — it is tearing the ` +
          `Fleet down. Retrying will open a new one once it has.`
      );
    }
    return {
      workflowId,
      lease: mine.lease,
      leases: held.leases.length,
      expiresAt: mine.expiresAt,
    };
  });
}

/**
 * Let go of a **Fleet**.
 *
 * A DROP WITH NO LEDGER IS A SUCCESS, not an error, and the distinction is the difference between a
 * clean scope exit and a **Run** that fails at the end of otherwise-finished work. The **Lease** workflow may
 * legitimately be gone: another holder's expiry may have reaped this **Lease** already, or the
 * **Fleet** may have been torn down by hand. Both mean the same thing to a caller letting go —
 * nothing of mine is held — and both must not raise.
 *
 * DROPPING TWICE IS ALSO A SUCCESS. The **Lease** workflow deletes an absent key and the teardown is driven by
 * the SIZE of the **Lease** workflow rather than by a count of drops, so a repeated drop cannot destroy a
 * **Fleet** a second time.
 */
export async function dropFleetLease(
  input: DropFleetLeaseInput,
  client?: Client
): Promise<DropFleetLeaseOutput> {
  if (!input?.stackFqn) throw new Error('dropFleetLease: no stackFqn');
  if (!input?.lease) throw new Error('dropFleetLease: no lease id');
  const workflowId = leaseWorkflowId(input.stackFqn);
  return withClient(client, async (client) => {
    try {
      await client.workflow.getHandle(workflowId).signal(LEASE_DROP_SIGNAL, { lease: input.lease });
      return { workflowId, delivered: true };
    } catch (err) {
      if (err instanceof WorkflowNotFoundError) return { workflowId, delivered: false };
      throw err;
    }
  });
}

/**
 * Which of these holders are still RUNNING.
 *
 * ASKED OF TEMPORAL, NOT OF THE HOLDERS. A **Run** that died does not get to answer, which is the
 * whole reason a heartbeat cannot be the mechanism: the failure to detect is exactly the failure
 * mode. `describe` reports the LATEST execution of a workflow id, so a **Run** that was retried into
 * a new run id under the same workflow id is correctly alive.
 *
 * A PER-HOLDER FAILURE DOES NOT SINK THE OTHERS. One unreachable holder among four must not make the
 * other three unknown — that would extend three **Leases** on the strength of one bad answer, and
 * over a few expiries it is how a **Fleet** with one broken holder never dies.
 */
export async function runningHolders(
  input: HoldersAliveInput,
  client?: Client
): Promise<HoldersAliveOutput> {
  const holders = [...new Set((input?.holders ?? []).filter((h) => typeof h === 'string' && h !== ''))];
  const out: HoldersAliveOutput = { alive: [], gone: [], unknown: [] };
  if (holders.length === 0) return out;

  return withClient(client, async (client) => {
    for (const holder of holders) {
      try {
        const desc = await client.workflow.getHandle(holder).describe();
        (String(desc.status.name) === 'RUNNING' ? out.alive : out.gone).push(holder);
      } catch (err) {
        // NOT FOUND IS AN ANSWER — the workflow is gone, whether it never existed or its retention
        // elapsed. Anything else is a question nobody could get to.
        (err instanceof WorkflowNotFoundError ? out.gone : out.unknown).push(holder);
      }
    }
    return out;
  });
}

/**
 * Is this "the namespace has no mapping for that search attribute", and nothing else?
 *
 * MATCHED ON THE gRPC CODE **AND** THE PHRASE, not on the phrase alone. `INVALID_ARGUMENT` covers a
 * great many caller mistakes and swallowing all of them here would hide a real bug behind a retry
 * that quietly drops the attribute; matching the phrase alone would fire on an unrelated message
 * that happened to contain it. Both, or neither.
 */
function isUnmappedSearchAttribute(err: unknown): boolean {
  // WALKED, NOT READ OFF THE TOP. The SDK wraps the gRPC ServiceError, so `code` and the phrase can
  // sit one or two `cause` links down — reading only the outermost object silently answered `false`
  // and rethrew, which is exactly the failure this predicate exists to prevent.
  let code: number | undefined;
  let text = '';
  let cur: unknown = err;
  for (let depth = 0; cur && depth < 5; depth++) {
    const e = cur as { code?: unknown; message?: unknown; details?: unknown; cause?: unknown };
    if (typeof e.code === 'number' && code === undefined) code = e.code;
    text += ` ${String(e.message ?? '')} ${String(e.details ?? '')}`;
    cur = e.cause;
  }
  // grpc.status.INVALID_ARGUMENT, AND the phrase. Either alone is wrong: INVALID_ARGUMENT covers a
  // great many caller mistakes and swallowing all of them would hide a real bug behind a retry that
  // quietly drops the attribute; the phrase alone would fire on an unrelated message containing it.
  return code === 3 && /no mapping defined for search attribute/i.test(text);
}
