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
  const namespace = process.env.KONTRA_NAMESPACE ?? 'default';
  const connection = await Connection.connect(temporalConnectOptions({ address }));
  try {
    return await fn(new Client({ connection, namespace, dataConverter }));
  } finally {
    await connection.close().catch(() => undefined);
  }
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

  return withClient(client, async (client) => {
    const handle = await client.workflow.signalWithStart(LEASE_WORKFLOW, {
      taskQueue: infraQueue(),
      workflowId: workflowId,
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
