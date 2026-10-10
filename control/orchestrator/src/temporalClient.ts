/**
 * Temporal reads for the backend. Nothing in THIS module starts a workflow — everything here
 * describes what Temporal already holds — but the client it memoizes is the one the starts go
 * through: `workflowControl.startRun` (`POST /api/runs`) and `probe.ts` (ADR 0033) both take their
 * handle from `getClient`.
 *
 * SO THE INVARIANT IS NARROWER THAN "THE ORCHESTRATOR NO LONGER STARTS ANYTHING", which this
 * header used to claim: **it starts workflows; it does not execute Batches.** What ADR 0023 §12
 * removed was the interpreter that took a user-composed GRAPH. A **Run** is one execution of a
 * caller's workflow, on a queue derived from that caller's own folder, and the Batch runs in a
 * worker this process does not host.
 *
 * The connection is lazy and memoized, so the HTTP server boots with no Temporal running —
 * only the run endpoints touch it (and surface a clear error if it's down).
 */

import { Client, Connection, defaultPayloadConverter } from '@temporalio/client';
import type { Payload, PayloadCodec } from '@temporalio/common';
import { OpenTelemetryWorkflowClientInterceptor } from '@temporalio/interceptors-opentelemetry';
import type { RunStatus } from '../contract/types';
import { dataConverter } from './codec/dataConverter';
import { type HeartbeatDetail, type NodeHeartbeat, heartbeatRow } from './heartbeat';
import { HistoryReducer, failureMessage, type RawHistoryEvent, type RunHistory } from './history';
import { inFlightOf, type InFlight } from './runActivity';
import { startTracing, tracingEnabled } from './otel';
import {
  KONTRA_INTERNAL_WORKFLOW_TYPES,
  KontraRunId,
  KontraTenant,
  buildDispatchQuery,
  buildRunDiscoveryQuery,
  buildWorkflowIdQuery,
  registerSearchAttributes,
} from './visibility';
import { SERVE_DEV_WORKFLOW, serveDevWorkflowId } from './queues';
import { temporalConnectOptions } from './temporalTls';
import { clientIdentity } from './workerIdentity';
import { ensureNamespace } from './namespaces';
import { activeNamespace, namespaceFor } from './workspaces';

/**
 * THE INSTALL'S LEGACY NAMESPACE: `KONTRA_NAMESPACE`, or `default`. Since ADR 0051 it is no longer
 * where every Run lives. It belongs to the workspace called `default` and to an install with no named
 * workspace, and it is where every Run from before isolation stays. Install-level work that belongs
 * to no workspace, such as the probe, keeps using it. Everything a workspace owns resolves its
 * namespace through {@link getClient} or {@link clientFor} instead.
 */
export const LEGACY_NAMESPACE = namespaceFor('');

/** The handler backing workflow's registered type name (handler main.go registers RunWorkflow
 *  under kontrav1.RunWorkflowName). Every dispatch a run makes starts one, tagged with the
 *  caller's workflow id — which is how a run is discovered at all (see buildRunDiscoveryQuery)
 *  and how its live progress is read back. */
const RUN_WORKFLOW_TYPE = 'kontra.v1.ActorService.Run';

/** The activity the ACTOR registers and the workflow schedules by name (ADR 0018). Byte-identical
 *  to `@activity.defn(name=...)` in runtime/python/internals/temporal/host.py and to the literal
 *  in runtime/handler/workflow.go — three independent spellings of one string, per the decoupling rule. */
const RUN_BATCH_ACTIVITY = 'RunBatch';

/** How many distinct runs one list may return. It used to bound a describe-per-run fan-out as
 *  well; listing no longer describes anything, so only the page size survives. */
/** EXPORTED so the history sweep can say whether its page came back FULL without restating the
 *  number — a second `200` written elsewhere is how a cap and the check for it drift apart. */
export const LIST_LIMIT = 200;

/** How many backing workflows the `dispatches` column may scan before giving up on being exact.
 *  One sweep on this controller produced 225 of them and a busy cluster produces far more; the
 *  column is a volume indicator, so a bounded undercount is the right failure. */
const DISPATCH_SCAN_LIMIT = 5000;

let connectionPromise: Promise<Connection> | null = null;
/** One Client per namespace, all on the one connection. */
const clients = new Map<string, Promise<Client>>();

/** The one connection every namespace's client shares, dialled on first use. */
async function connection(): Promise<Connection> {
  if (!connectionPromise) {
    connectionPromise = (async () => {
      startTracing();
      return Connection.connect(temporalConnectOptions());
    })();
    connectionPromise.catch(() => (connectionPromise = null));
  }
  return connectionPromise;
}

/**
 * The client for one namespace, created on first use: the namespace registered if it is new and
 * usable before this resolves, and the custom Search Attributes registered in it (they are
 * per-namespace, so every namespace needs its own registration before its first visibility query).
 */
export async function clientFor(namespace: string): Promise<Client> {
  let p = clients.get(namespace);
  if (!p) {
    p = (async () => {
      const conn = await connection();
      // NOT FOR THE LEGACY NAMESPACE, which the install's Temporal was set up with and which nothing
      // here ever registered: a workspace namespace is the only kind this process creates.
      if (namespace !== LEGACY_NAMESPACE) await ensureNamespace(conn, namespace);
      const client = new Client({
        connection: conn,
        namespace,
        dataConverter,
        // WHO STARTED THIS RUN, recorded by the server rather than inferred. Every console-driven
        // start, signal, terminate and reset goes through a client built here, so its identity is
        // what lands on `WorkflowExecutionStarted.identity` and on a `RequestCancel` event. Left at
        // the default that is `<pid>@<hostname>` — indistinguishable from the Workers sharing this
        // container, and useless for the question an audit asks of it.
        identity: clientIdentity(),
        interceptors: tracingEnabled
          ? { workflow: [new OpenTelemetryWorkflowClientInterceptor()] }
          : undefined,
      });
      await registerSearchAttributes(conn, namespace);
      return client;
    })();
    p.catch(() => clients.delete(namespace));
    clients.set(namespace, p);
  }
  return p;
}

/**
 * The client for the workspace this install is looking at NOW (ADR 0051 §4): `.current`, read per
 * call. Every read the console makes, of runs, histories, reports, fleets and pulse, goes through
 * here, so they are all scoped to that workspace's namespace by address. A Run in another
 * workspace's namespace is not filtered out; this client cannot see it.
 */
export async function getClient(): Promise<Client> {
  return clientFor(activeNamespace());
}

// The namespace scope lives beside the namespace rule (workspaces.ts) so that a store can address
// itself by namespace without importing a Temporal client. Re-exported: every reader here uses it.
export { activeNamespace, inNamespace } from './workspaces';

/**
 * The CONNECTION behind the client, for the RPCs that are not workflow calls.
 *
 * `Client` exposes `workflowService` and nothing else, but Nexus endpoints live on the OPERATOR
 * service — `createNexusEndpoint`, `listNexusEndpoints`, `deleteNexusEndpoint` — which hangs off
 * the connection. Reached through `getClient` rather than by dialling again so registration shares
 * the one connection, its tracing and its search-attribute registration, instead of opening a
 * second one per register call.
 */
export async function getConnection(): Promise<Connection> {
  const client = await getClient();
  return client.connection as Connection;
}

/** One caller workflow, as Temporal describes it. */
export interface RunDescription {
  /** The caller's workflow id — the run id, and the only identifier a run has. */
  runId: string;
  status: RunStatus;
  /** The caller's workflow type, e.g. the class name of their `@workflow.defn`. */
  type: string;
  tenant: string;
  startedAt: number;
  /** 0 while the run is open. */
  closedAt: number;
  /**
   * The run's MEMO, decoded — where a parked run publishes its asks (`hitl.ts`).
   *
   * IT RIDES ON THE DESCRIBE THIS FUNCTION ALREADY MAKES, which is the property that makes the ask
   * readable with no worker polling the queue and no second RPC. Absent on a description that came
   * from anywhere but Temporal.
   */
  memo?: Record<string, unknown>;
  /**
   * What Temporal says this run has in flight — the stall evidence (`runActivity.ts`).
   *
   * PENDING WORK IS METADATA, not payloads: attempt counters and timestamps off the describe
   * response, so reading it costs no blob GET (ADR 0007). Absent, rather than empty, when the
   * description did not come from a describe — "nothing is pending" and "nobody looked" are
   * different answers and the activity reading depends on which one it has.
   */
  inFlight?: InFlight[];
}

/** One row of the run list: a description plus what discovery knows about it. */
export interface RunRow extends RunDescription {
  /** How many Actor dispatches this run made — the evidence it was discovered by. */
  dispatches: number;
}

/**
 * Whose Run this is — read from the attribute, DERIVED when nothing wrote one.
 *
 * A TENANT IS A TEMPORAL NAMESPACE (ADR 0036 §7, and `CONTEXT.md` says it in as many words). It is
 * the only authorisation boundary Temporal has and the only one kontra leans on. Every read in this
 * module is namespace-scoped — `client.workflow.list` and `getHandle(id).describe()` both answer
 * within the namespace `getClient` connected to — so the tenant of any row this process can SEE is
 * `NAMESPACE`, whether or not an attribute was ever stamped.
 *
 * WHICH IS WHY THE FALLBACK IS THE NAMESPACE AND NOT THE EMPTY STRING. `startRun` stamps
 * `KontraTenant` at start (`workflowControl.ts`), but that reaches only executions begun after it
 * landed. The perpetual ones — `wardenWorkflow`, `leaseWorkflow` — were already running, never
 * close, and in the Warden's case cannot be made to restart into a stamp: its start site uses
 * `WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING` precisely so `systemctl restart kontra-warden`
 * RE-ATTACHES to the same execution. Reporting `''` for those was reporting "no tenant" about a
 * fact that is knowable without asking anyone.
 *
 * ABSENT IS STILL ABSENT FOR ANYTHING THE NAMESPACE CANNOT ANSWER — this is not a default, it is a
 * derivation, and it is only sound because the read that produced the row was namespace-scoped.
 */
function tenantOf(
  attrs: { get(key: typeof KontraTenant): string | undefined },
  namespace: string
): string {
  return attrs.get(KontraTenant) ?? namespace;
}

/**
 * Describe one run by the id the caller started it under. `undefined` when Temporal has no
 * such execution — which is an ordinary answer, not an error: retention drops closed
 * workflows long before the Datasets they wrote expire.
 */
export async function describeRun(runId: string): Promise<RunDescription | undefined> {
  const client = await getClient();
  try {
    const desc = await client.workflow.getHandle(runId).describe();
    return {
      runId,
      status: mapStatus(desc.status),
      type: desc.type ?? '',
      tenant: tenantOf(desc.typedSearchAttributes, client.options.namespace),
      startedAt: desc.startTime?.getTime() ?? 0,
      closedAt: desc.closeTime?.getTime() ?? 0,
      // Both come off the SAME response. A parked run's asks and the evidence that a running run
      // is going nowhere are two readings of one RPC, which is what keeps the third status
      // dimension from costing a second round trip per run.
      memo: desc.memo ?? {},
      inFlight: inFlightOf(desc.raw),
    };
  } catch (err) {
    if (isNotFound(err)) return undefined;
    throw err;
  }
}

/**
 * Send one signal to a run.
 *
 * THE ONE WRITE THIS MODULE MAKES. Everything else here describes what Temporal already holds
 * (ADR 0023 §12) — but answering a parked run's ask is not the orchestrator starting work, it is
 * an operator's reply reaching the workflow that asked for it, and a signal is the only durable
 * way to deliver one. Refusals happen BEFORE this is called (`hitl.ts`): a signal in a run's
 * history cannot be taken back.
 */
export async function signalRun(runId: string, name: string, payload: unknown): Promise<void> {
  const client = await getClient();
  await client.workflow.getHandle(runId).signal(name, payload);
}

/**
 * Every run kontra can see, newest first.
 *
 * TWO LISTS, AND THEY ANSWER DIFFERENT QUESTIONS. The first asks *which runs exist*, by listing the
 * caller workflow executions themselves (see `buildRunDiscoveryQuery` for why that is a subtraction
 * and what listing the dispatches instead used to cost). The second asks *how much work each one
 * dispatched*, which is still only knowable from the dispatches. Existence no longer depends on the
 * answer to the second question, which is the entire point: a run with zero dispatches is a row
 * with `dispatches: 0`, not an absence.
 *
 * Status comes straight off the listing, so it is still the CALLER'S status — a run whose own
 * workflow is still looping is `running` even when every dispatch it made has completed. It no
 * longer costs a describe per run to learn that.
 *
 * The dispatch scan is bounded by `DISPATCH_SCAN_LIMIT` because it is the one unbounded thing here:
 * a single sweep produced 225 backing workflows on this controller, and the count is a column, not
 * a correctness property. When the scan is cut short the counts it did gather are still reported —
 * an undercount on a busy cluster is a worse column, not a wrong list.
 */
export async function listRuns(
  filter: { tenant?: string; status?: string; limit?: number } = {}
): Promise<RunRow[]> {
  const client = await getClient();
  const cap = Math.min(Math.max(filter.limit ?? LIST_LIMIT, 1), LIST_LIMIT);

  // 1. THE RUNS. Keyed by workflow id, because ADR 0023 §12 says the Run IS the workflow id — so a
  //    reused id (the fleet reuses `kontra-fleet/<name>` for both bring-up and teardown) collapses
  //    to its newest execution here, matching what `describeRun` answers for the same id.
  const byId = new Map<string, RunRow>();
  for await (const info of client.workflow.list({
    query: buildRunDiscoveryQuery(KONTRA_INTERNAL_WORKFLOW_TYPES, filter),
  })) {
    if (byId.has(info.workflowId)) continue;
    byId.set(info.workflowId, {
      runId: info.workflowId,
      status: mapStatus(info.status),
      type: info.type ?? '',
      tenant: tenantOf(info.typedSearchAttributes, client.options.namespace),
      startedAt: info.startTime?.getTime() ?? 0,
      closedAt: info.closeTime?.getTime() ?? 0,
      dispatches: 0,
      // FREE, AND NOT AUTHORITATIVE FOR AN OPEN RUN. The visibility record carries the memo, so a
      // CLOSED run's asks — which can no longer change — arrive with the listing at no cost. An
      // open run's memo reaches visibility asynchronously, so `runs.ts` re-reads that one from a
      // describe rather than reporting a stale "nothing pending" about a run that just parked.
      memo: info.memo ?? {},
    });
    if (byId.size >= cap) break;
  }
  if (byId.size === 0) return [];

  // 2. THE DISPATCH COUNTS, for the runs we already have. A dispatch whose `KontraRunId` names no
  //    listed run is skipped rather than inventing a row: that is either a run outside this page or
  //    one whose caller workflow retention has already dropped, and neither is a run to show.
  let scanned = 0;
  try {
    for await (const info of client.workflow.list({
      query: buildDispatchQuery(RUN_WORKFLOW_TYPE, filter),
    })) {
      if (++scanned > DISPATCH_SCAN_LIMIT) break;
      const runId = info.typedSearchAttributes.get(KontraRunId);
      const row = runId === undefined ? undefined : byId.get(runId);
      if (row) row.dispatches += 1;
    }
  } catch {
    // A failed count must not empty the list — the runs are already known at this point.
  }

  // Sorted here, not with a visibility `ORDER BY`: SQLite visibility (what `temporal start-dev`
  // runs) rejects ORDER BY entirely, so the ordering guarantee has to live client-side.
  return [...byId.values()].sort((a, b) => b.startedAt - a.startedAt);
}

/**
 * How many runs Temporal currently holds — COUNTED, never listed.
 *
 * THIS IS THE WHOLE REASON THE CHROME CAN ASK "is anything running" ON EVERY SURFACE. `listRuns`
 * pages executions back and then reads a ledger record and a describe per row; the number in the
 * rail needs none of that, and paying for it on a Datasets or Monitor session was the run list
 * coming back through the chrome. `CountWorkflowExecutions` answers off the visibility INDEX in one
 * RPC: no page, no cap, no per-run read, and the cost does not move when a controller accumulates
 * ten thousand closed runs.
 *
 * MEASURED ON THIS BOX rather than assumed, because the neighbour rule in `visibility.ts` is a scar
 * from exactly this — SQLite visibility (what `temporal start-dev` and CI run) rejects `ORDER BY`
 * outright, so "the SDK exposes it" proves nothing about the backend under it. Probed against this
 * controller's Temporal 1.31.0 with three caller executions present, one of them carrying an ask
 * memo:
 *
 *   count "<discovery query>"                        -> 2, then 3 as the third started
 *   count "<discovery query> AND ExecutionStatus = 'Running'" -> same
 *   count "<discovery query> GROUP BY ExecutionStatus"        -> Group total: 2, values: Running
 *
 * so both the plain count and the grouped form are served by the SQLite backend. The count is
 * APPROXIMATE AND EVENTUALLY CONSISTENT, which the SDK says of `list` in the same words — a run
 * started half a second ago may not be in it yet. That is the right error for a chrome counter: it
 * arrives a poll late, it never invents one.
 */
export async function countRuns(filter: { tenant?: string; status?: string } = {}): Promise<number> {
  const client = await getClient();
  const { count } = await client.workflow.count(
    buildRunDiscoveryQuery(KONTRA_INTERNAL_WORKFLOW_TYPES, filter)
  );
  return count;
}

/** One OPEN run, as the visibility listing already holds it — no describe behind any field. */
export interface OpenRun {
  /** The caller's workflow id. The only identifier a run has (ADR 0023 §12). */
  runId: string;
  /** The caller's workflow type — what the thread it belongs to is called. */
  type: string;
  startedAt: number;
  /**
   * The run's memo, where a parked run publishes its asks (`hitl.ts`).
   *
   * IT RIDES ON THE LISTING, which is the property this whole read is built on: the visibility
   * record carries the memo, so knowing which open runs are parked costs ZERO extra RPCs — not the
   * describe-per-run that `runs.ts` pays to answer the same question about a page of rows.
   * Verified against this controller's SQLite visibility, not inferred: a `--memo
   * 'kontra.ask.q1={…}'` start came back through `workflow list -o json` with its `kontra.ask.q1`
   * field intact while its two memo-less neighbours came back `{}`.
   *
   * AND IT IS EVENTUALLY CONSISTENT FOR AN OPEN RUN, which `listRuns` already says out loud: a run
   * that parked a moment ago reaches this index a beat later. So an absent ask here means "not seen
   * yet", never "asked nothing" — the caller must not turn it into a green all-clear.
   */
  memo: Record<string, unknown>;
}

/**
 * The runs Temporal reports as RUNNING right now, capped, with nothing read per row.
 *
 * DELIBERATELY NOT `listRuns({ status: 'running' })`. That function is the run LIST and pays for
 * being one: a second visibility scan over every dispatch on the cluster to fill the `dispatches`
 * column (bounded at 5,000, which is a bound and not a small number). Nothing in the chrome shows
 * that column, so this is step one of that function and stops there.
 *
 * RUNNING ONLY, WHICH IS WHAT MAKES IT BOUNDED BY THE FLEET RATHER THAN BY HISTORY. Closed runs
 * never enter the query, so a controller with ten thousand finished runs scans exactly as much as a
 * fresh one; what moves this number is how much is executing, and that is bounded by the machines.
 * The cap is the second belt: a caller that fans out a thousand concurrent workflows gets a partial
 * scan and a caller that says so ({@link OpenRunScan.capped}), never an unbounded page.
 */
export interface OpenRunScan {
  runs: OpenRun[];
  /** The scan stopped at its cap — there are more running runs than were examined. */
  capped: boolean;
}

export async function listOpenRuns(cap: number): Promise<OpenRunScan> {
  const client = await getClient();
  const limit = Math.min(Math.max(cap, 1), LIST_LIMIT);
  const runs: OpenRun[] = [];
  const seen = new Set<string>();
  let capped = false;
  for await (const info of client.workflow.list({
    query: buildRunDiscoveryQuery(KONTRA_INTERNAL_WORKFLOW_TYPES, { status: 'running' }),
  })) {
    // Same collapse as `listRuns`: the Run IS the workflow id, so a reused id is its newest
    // execution here and cannot be counted twice.
    if (seen.has(info.workflowId)) continue;
    if (runs.length >= limit) {
      capped = true;
      break;
    }
    seen.add(info.workflowId);
    runs.push({
      runId: info.workflowId,
      type: info.type ?? '',
      startedAt: info.startTime?.getTime() ?? 0,
      memo: info.memo ?? {},
    });
  }
  return { runs, capped };
}

/** One open execution, with the queue it is waiting on. */
export interface OpenExecution {
  workflowId: string;
  execId: string;
  type: string;
  queue: string;
  startedAt: number;
}

/**
 * EVERY open execution, internal types INCLUDED — which is what makes it different from
 * {@link listOpenRuns} and is the whole reason it exists.
 *
 * `listRuns` and `listOpenRuns` both subtract `KONTRA_INTERNAL_WORKFLOW_TYPES`, correctly: the Runs
 * page is about a user's runs and a wall of `wardenWorkflow` rows would bury them. But an audit
 * found nine open executions wedged on this cluster — five Wardens up to 14 days old, a retention
 * workflow that never ran a task in 18 days — and EVERY ONE of them was an internal type. A health
 * check that inherited that exclusion would have been blind to all nine, which is how they sat
 * unnoticed for a month.
 *
 * THE QUEUE IS WHAT MAKES THE ANSWER USEFUL. "Running" is what Temporal says about all of these;
 * whether anything is POLLING the queue they are parked on is the difference between working and
 * wedged, and it is the one thing the visibility record carries that says so.
 *
 * NO DEDUPE BY WORKFLOW ID, unlike the two above. They collapse a reused id to its newest execution
 * because a Run IS its workflow id; here a reused id with two open executions is two things that
 * cannot move, and hiding one of them would be hiding exactly the case worth seeing.
 */
export async function listOpenExecutions(
  cap: number
): Promise<{ executions: OpenExecution[]; capped: boolean }> {
  const client = await getClient();
  const limit = Math.min(Math.max(cap, 1), LIST_LIMIT);
  const executions: OpenExecution[] = [];
  let capped = false;
  for await (const info of client.workflow.list({ query: `ExecutionStatus = "Running"` })) {
    if (executions.length >= limit) {
      capped = true;
      break;
    }
    executions.push({
      workflowId: info.workflowId,
      execId: info.runId ?? '',
      type: info.type ?? '',
      queue: info.taskQueue ?? '',
      startedAt: info.startTime?.getTime() ?? 0,
    });
  }
  return { executions, capped };
}

/** The workflow type every Fleet operation runs as (`workflows/stack.ts`). */
const STACK_WORKFLOW_TYPE = 'stackWorkflow';

/**
 * One Fleet operation — a bring-up, a preview or a teardown of one stack.
 *
 * Deliberately NOT a `RunRow`. A Fleet operation is not a Run: nobody dispatches Actors from it, it
 * has no Dataset and no tenant, and ADR 0017's rule against merging two authorities into one field
 * applies exactly here — folding these into the run list would mean `dispatches: 0` on a row where
 * the number is meaningless rather than merely zero.
 */
export interface FleetOperationRow {
  /** `kontra-fleet/<stack>` — the SAME id for every operation on that stack. */
  workflowId: string;
  /** Temporal's run id, which is the only thing that tells two operations on one stack apart.
   *  Never called a run id: a **Run** is its caller workflow's id (ADR 0023 §12). */
  execId: string;
  /** The stack this acted on, taken from the id — the readable half of `kontra-fleet/<stack>`. */
  stack: string;
  status: RunStatus;
  startedAt: number;
  /** 0 while the operation is open. */
  closedAt: number;
}

/**
 * Every Fleet operation this cluster still holds, newest first.
 *
 * This surface did not exist. `kontra-fleet/nscheck-0.1.0` ran four times on this controller and
 * appeared on no page — the only record was `temporal workflow list`, which drops at retention. A
 * fleet costs real money, so "which stacks did I bring up, and did any teardown fail" is not a
 * curiosity; a teardown that failed silently is a bill.
 *
 * NO `op` FIELD, and that is a limit not an omission: `up` / `preview` / `destroy` travels in the
 * child's INPUT, which is a payload the event log never decodes (ADR 0007) and which a visibility
 * listing does not carry at all. `fleetPhase.ts` gets it by QUERYING the workflow, which needs a
 * worker able to replay each one — far too expensive per row. The list says what it can read.
 */
export async function listFleetOperations(limit = 50): Promise<FleetOperationRow[]> {
  const client = await getClient();
  const cap = Math.min(Math.max(limit, 1), LIST_LIMIT);
  const rows: FleetOperationRow[] = [];
  for await (const info of client.workflow.list({
    query: buildDispatchQuery(STACK_WORKFLOW_TYPE),
  })) {
    rows.push({
      workflowId: info.workflowId,
      execId: info.runId ?? '',
      stack: info.workflowId.startsWith('kontra-fleet/')
        ? info.workflowId.slice('kontra-fleet/'.length)
        : info.workflowId,
      status: mapStatus(info.status),
      startedAt: info.startTime?.getTime() ?? 0,
      closedAt: info.closeTime?.getTime() ?? 0,
    });
    if (rows.length >= cap) break;
  }
  // Client-side for the same reason as `listRuns`: SQLite visibility rejects ORDER BY.
  return rows.sort((a, b) => b.startedAt - a.startedAt);
}

/**
 * ONE PRESS OF SERVE, as Temporal still holds it.
 *
 * Deliberately NOT a `RunRow`, for the reason {@link FleetOperationRow} is not one and states: a
 * serve is not a **Run**. It dispatches no Actor, writes no Dataset, and `visibility.ts` excludes
 * its type from the Runs page precisely because a wall of these would bury what a caller ran.
 * Folding it into that list with `dispatches: 0` would make the number meaningless rather than zero.
 */
export interface ServeRow {
  /**
   * Temporal's run id for this ONE execution — the only thing that tells two serves of one folder
   * apart, because the workflow id IS the folder (`queues.ts:serveDevWorkflowId`). Never called a
   * run id: a **Run** is its caller workflow's id (ADR 0023 §12).
   */
  execId: string;
  status: RunStatus;
  startedAt: number;
  /** 0 while the serve is still in flight. */
  closedAt: number;
  /**
   * WHY THIS SERVE DID NOT PRODUCE A WORKER, in Temporal's own words — and the whole reason a
   * history is worth keeping at all.
   *
   * `status` CANNOT CARRY IT. `mapStatus` collapses FAILED, TERMINATED and TIMED_OUT into the one
   * word `failed`, which is right for a run list and useless here: the three sentences behind those
   * are "the CLI exited non-zero and here is its stderr", "somebody killed it", and "two minutes
   * elapsed and the image was probably still pulling" (`workflows/serveDev.ts` sets that budget).
   * Those are three different afternoons.
   *
   * ABSENT ON A SERVE THAT WORKED, and absent — rather than a placeholder — when the close event
   * could not be read: a closed execution whose history retention has passed is an ordinary answer
   * on this path, and inventing a reason for it would be the confident wrong kind.
   */
  failure?: string;
}

/** A folder's serve history, and whether the scan saw all of it. */
export interface ServeScan {
  /** Newest first. */
  serves: ServeRow[];
  /**
   * The scan stopped at its cap — this folder has been served more times than are listed.
   *
   * REPORTED RATHER THAN IMPLIED, on the same terms as {@link OpenRunScan.capped}. A silent slice
   * of a history reads as the whole history, and "this actor has been served twice" is a different
   * claim from "here are the last two of many".
   */
  capped: boolean;
}

/** How many serves one history read may examine, whatever the caller asks for. The close-event read
 *  below is one RPC per non-completed row, so this is the bound on that fan-out and not merely a
 *  page size — see {@link listServes}. */
export const SERVE_SCAN_LIMIT = 25;

/** What a caller gets when it does not say. A compact panel beside an actor, not an archive. */
const SERVE_DEFAULT_LIMIT = 10;

/**
 * EVERY SERVE OF ONE FOLDER, newest first, with the failures explained.
 *
 * ── WHY THIS EXISTS ─────────────────────────────────────────────────────────────────────────────
 *
 * Pressing Serve starts `serveDevWorkflow`, and `visibility.ts` keeps that type off the Runs page —
 * correctly, because it is kontra's own infrastructure and not a caller's Run. But the hiding was
 * the whole of it: nowhere in this control plane could answer "when was this actor last served, and
 * did it work". A serve that failed on an import error left the operator with a Serve button that
 * looked pressed and an actor nothing was polling, with the sentence explaining why held only in a
 * Temporal execution nobody was listing.
 *
 * ── ONE QUERY, ONE ID ───────────────────────────────────────────────────────────────────────────
 *
 * The id is derived, not discovered: `serveDevWorkflowId(sourceId)` is the same function both start
 * sites call, so this cannot address a folder they did not write. It is REUSED across serves —
 * that is what makes one folder's serves mutually exclusive — so the history is a LIST of
 * executions under one id. A describe would answer with whichever ran last and call it the history.
 *
 * ── WHAT THE FAILURE COSTS ──────────────────────────────────────────────────────────────────────
 *
 * The visibility record carries status and both timestamps and NOT the failure message, so the
 * reason needs the close event. That is one `getWorkflowExecutionHistory` with
 * `historyEventFilterType: CLOSE_EVENT` — Temporal answering "the last event" without sending the
 * ones before it — which is the same targeted read `fetchRunIO` makes and for the same reason: no
 * page, and no payload decode, so no blob GET (ADR 0007).
 *
 * IT IS PAID ONLY FOR ROWS THAT NEED IT. A running serve has no close event and a completed one has
 * nothing to explain, so the fan-out is bounded by the number of FAILED serves in the page, itself
 * bounded by {@link SERVE_SCAN_LIMIT}. A folder that has always served cleanly costs exactly one RPC.
 *
 * Sorted here rather than with a visibility `ORDER BY`, like every other listing in this module:
 * SQLite visibility rejects `ORDER BY` outright. Temporal returns executions newest-first by
 * default, which is what makes the cap take the newest ones; the sort is the defensive half.
 */
export async function listServes(
  sourceId: string,
  limit = SERVE_DEFAULT_LIMIT
): Promise<ServeScan> {
  const client = await getClient();
  const cap = Math.min(Math.max(limit, 1), SERVE_SCAN_LIMIT);
  const workflowId = serveDevWorkflowId(sourceId);
  const serves: ServeRow[] = [];
  let capped = false;
  for await (const info of client.workflow.list({
    query: buildWorkflowIdQuery(SERVE_DEV_WORKFLOW, workflowId),
  })) {
    if (serves.length >= cap) {
      capped = true;
      break;
    }
    // NO DEDUPE BY WORKFLOW ID, unlike `listRuns` — the opposite rule, for the opposite reason.
    // There the id IS the Run, so a reused id collapses to its newest execution; here the id is the
    // FOLDER and every execution under it is a separate press of the button, which is the thing
    // being listed.
    serves.push({
      execId: info.runId ?? '',
      status: mapStatus(info.status),
      startedAt: info.startTime?.getTime() ?? 0,
      closedAt: info.closeTime?.getTime() ?? 0,
    });
  }
  serves.sort((a, b) => b.startedAt - a.startedAt);
  for (const row of serves) {
    if (row.status === 'running' || row.status === 'completed') continue;
    const why = await closeReason(client, workflowId, row.execId);
    if (why) row.failure = why;
  }
  return { serves, capped };
}

/**
 * The sentence on a closed execution's LAST event. One RPC, no page, no payload.
 *
 * THREE SOURCES, IN THE ORDER A READER WANTS THEM. The failure proto's message is the real answer
 * when there is one — `failureMessage` walks the nested `cause` chain and JOINS it, returning
 * `${own}: ${cause}`, so an activity that threw `serve-dev failed (exit 1): ModuleNotFoundError…`
 * reads as `Activity task failed: serve-dev failed (exit 1): ModuleNotFoundError…`. The SDK's
 * wrapper is KEPT as the prefix, not stripped: the innermost sentence is what a person needs and
 * the outer frames are what say where it came from, and `serveHistory.test.ts` asserts the joined
 * form. (This comment previously claimed the wrapper was discarded. It never was.) A termination
 * carries no failure at all,
 * only the operator's `reason`. And a workflow that ran out its two-minute budget carries neither,
 * where the WORD is the whole of the information — which is exactly the case `status` alone cannot
 * express, since `mapStatus` files a timeout under `failed` beside a crash.
 *
 * NEVER THROWS, AND ANSWERS `''` FOR "COULD NOT ASK". A closed execution whose history has aged out
 * of retention is ordinary, and a history read that failed must cost the caller a reason and not
 * the row — the status and the timestamps are already in hand and are the half that survives
 * retention in the visibility index longest.
 */
async function closeReason(
  client: Awaited<ReturnType<typeof getClient>>,
  workflowId: string,
  execId: string
): Promise<string> {
  const close = await runClose(client, workflowId, execId);
  // The ROW wants one sentence, and prefers the specific one: a failure message if there is one, else
  // the terminator's reason, else the bare kind. {@link fetchRunClose} keeps the two apart for callers
  // that need them apart; this flattens them exactly as it always did.
  if (!close) return '';
  return close.message || close.type;
}

/** `WORKFLOW_EXECUTION_STATUS_RUNNING` on the wire. Numeric, like the filter below, because the
 *  enum's export path has moved between SDK minors and the wire value has not. */
const STATUS_RUNNING = 1;
/** `HISTORY_EVENT_FILTER_TYPE_CLOSE_EVENT` on the wire. */
const CLOSE_EVENT = 2;

/** The two raw-service calls {@link closeEventOf} makes, so a test can hand it a fake. */
export interface CloseEventService {
  describeWorkflowExecution(req: {
    namespace: string;
    execution: { workflowId: string; runId?: string };
  }): Promise<{ workflowExecutionInfo?: { status?: number | null } | null }>;
  getWorkflowExecutionHistory(req: {
    namespace: string;
    execution: { workflowId: string; runId?: string };
    historyEventFilterType: number;
    waitNewEvent: boolean;
  }): Promise<{ history?: { events?: unknown[] | null } | null }>;
}

/**
 * An execution's close event, or `undefined` while it is still running — WITHOUT WAITING FOR ONE.
 *
 * THE CLOSE-EVENT FILTER LONG-POLLS AN OPEN RUN, AND `waitNewEvent: false` DOES NOT STOP IT. Asked
 * for the close event of a running workflow, Temporal holds the request until its long-poll expiry
 * and then answers with nothing. Measured inside a live install's API container against an open run:
 * 20,006 ms and zero events for this call, 30 ms for a first-event read of the same history.
 *
 * Two callers paid that on every open run. `/api/runs/:id/io` took 20 s, so the run page's output
 * region hung for as long. And a live report's first render read it TWICE — once through `fetchRunIO`
 * and once through `fetchRunClose` — before writing a byte. The stream's headers only leave on the
 * first write, so the browser sat on "No report yet" for 41 s of a 54 s canary, which is the
 * opposite of streaming (ADR 0062).
 *
 * So ask whether it is running first: one describe, a few milliseconds. A describe that FAILS says
 * nothing either way, and falls through to the read that always worked rather than guessing.
 */
export async function closeEventOf(
  service: CloseEventService,
  namespace: string,
  execution: { workflowId: string; runId?: string }
): Promise<Record<string, unknown> | undefined> {
  try {
    const described = await service.describeWorkflowExecution({ namespace, execution });
    if (described.workflowExecutionInfo?.status === STATUS_RUNNING) return undefined;
  } catch {
    // Could not tell. The history read below is the old behaviour: slow on an open run, but correct.
  }
  const res = await service.getWorkflowExecutionHistory({
    namespace,
    execution,
    historyEventFilterType: CLOSE_EVENT,
    waitNewEvent: false,
  });
  return (res.history?.events ?? []).at(-1) as Record<string, unknown> | undefined;
}

/**
 * How a Run ended, as a TYPE and a MESSAGE rather than one sentence.
 *
 * EXTRACTED FROM `closeReason`, WHICH NOW CALLS IT, so there is one reader of the close event and not
 * two that agree today. A report's context (§2.4) gives a template `run.error.type` and
 * `run.error.message` separately — a template that wants to say "TimeoutError" in a heading and the
 * message in a paragraph cannot do it from a pre-joined string, and joining them here and splitting
 * them there is how the two spellings would drift.
 *
 * `undefined` for a Run that COMPLETED, which is a different answer from a Run whose close event could
 * not be read: the second returns a type with an empty message, so a report never claims a successful
 * Run failed because an RPC was slow.
 */
async function runClose(
  client: Awaited<ReturnType<typeof getClient>>,
  workflowId: string,
  execId: string
): Promise<{ type: string; message: string } | undefined> {
  try {
    const last = await closeEventOf(client.workflowService, client.options.namespace, {
      workflowId,
      ...(execId ? { runId: execId } : {}),
    });
    if (!last) return undefined;
    const kind = closedAsOf(last);
    if (!kind) return undefined;
    const failed = last.workflowExecutionFailedEventAttributes as { failure?: unknown } | undefined;
    const message = failureMessage(failed?.failure);
    if (message) return { type: kind, message };
    const killed = last.workflowExecutionTerminatedEventAttributes as { reason?: unknown } | undefined;
    const reason = typeof killed?.reason === 'string' ? killed.reason.trim() : '';
    return { type: kind, message: reason };
  } catch {
    return undefined;
  }
}

/**
 * How one Run ended, for a caller that has only its id — the report renderer's single use.
 *
 * `undefined` means it completed, is still running, or Temporal could not answer. A caller that needs
 * to tell those apart has the status from the describe already.
 */
export async function fetchRunClose(
  runId: string,
  execId?: string
): Promise<{ type: string; message: string } | undefined> {
  const client = await getClient();
  return runClose(client, runId, execId ?? '');
}

export type { NodeHeartbeat } from './heartbeat';

/** How many events one page of history asks for, and how many pages we will walk. The product
 *  bounds the work a single browser poll can cost this process: a run that has been looping for a
 *  day has a history nobody wants rendered, and paging to the end of it to find that out is the
 *  unbounded read this cap exists to prevent. */
const HISTORY_PAGE = 1000;
const HISTORY_MAX_PAGES = 20;

/**
 * A run's Temporal event history, reduced to the log the Workflows surface renders.
 *
 * THE RAW SERVICE, NOT `handle.fetchHistory()`. The typed helper runs every payload through the
 * data converter, and on this deployment that converter is a claim-check codec (ADR 0007) — so
 * fetching a 623-unit sweep's history would fan out into thousands of blob GETs to render a list
 * of event names. Nothing here reads a payload; see history.ts.
 *
 * IT ALSO SERVES THE LEVELS BELOW A RUN. A child workflow and a dispatch's backing workflow are
 * just workflow ids, so drilling into one is this function again — same raw service, same reducer,
 * same cap, same zero blob reads. There is deliberately no second history path to keep honest.
 *
 * `execId` is Temporal's run id for one execution, and is passed ONLY because a workflow id can be
 * reused: `kontra-fleet/dns` is the id of both the bring-up and the teardown of every fleet, and
 * asking by id alone answers with whichever ran last.
 *
 * Returns `undefined` when Temporal has no such execution, which is an ordinary answer: closed
 * workflows are dropped for retention long before their Datasets expire.
 */
export async function fetchRunHistory(
  runId: string,
  execId?: string
): Promise<RunHistory | undefined> {
  const client = await getClient();
  /* REDUCED PER PAGE, SO NO PAGE OUTLIVES THE LOOP.
   *
   * This used to accumulate every raw event into one array — `HISTORY_PAGE × HISTORY_MAX_PAGES` =
   * 20,000 decoded `HistoryEvent`s, each with its whole attribute bag — and hand that to the
   * reducer, to produce at most `EVENT_CAP` = 1,000 rows. Peak heap scaled with the size of the RUN
   * on a path every open browser tab polls, for an output whose size is FIXED.
   *
   * The reducer keeps a head and a bounded tail, which is exactly what a streaming reduce needs; the
   * page is garbage the moment it has been pushed. `scanned`, `elided` and `truncated` are unchanged
   * — see `HistoryReducer` for why the link pass can still be correct without the array.
   */
  const reducer = new HistoryReducer();
  let nextPageToken: Uint8Array | undefined;
  let truncated = false;
  try {
    for (let page = 0; page < HISTORY_MAX_PAGES; page++) {
      const res = await client.workflowService.getWorkflowExecutionHistory({
        namespace: client.options.namespace,
        execution: { workflowId: runId, ...(execId ? { runId: execId } : {}) },
        maximumPageSize: HISTORY_PAGE,
        nextPageToken,
        // Long-poll off: this is a browser poll, and a request that parks for a minute waiting
        // for the next event is a request that holds a connection per open tab.
        waitNewEvent: false,
      });
      for (const ev of res.history?.events ?? []) reducer.push(ev as RawHistoryEvent);
      nextPageToken = res.nextPageToken?.length ? res.nextPageToken : undefined;
      if (!nextPageToken) break;
      if (page === HISTORY_MAX_PAGES - 1) truncated = true;
    }
  } catch (err) {
    if (isNotFound(err) || isNotFoundStatus(err)) return undefined;
    throw err;
  }
  // The namespace goes in so the reducer can refuse a link that points OUT of it — see mapHistory.
  // The LENGTH goes in because `scanned` is what this reader fetched, and past the cap above that
  // is not the same number — see `describeLength`.
  return reducer.finish(truncated, client.options.namespace, await describeLength(client, runId, execId));
}

/**
 * How long the history ACTUALLY is, from the server — one RPC, no pages, no payloads.
 *
 * THIS IS THE WHOLE OF THE FIX FOR A NUMBER THAT WAS WRONG BY UP TO 61%. The pager above stops at
 * `HISTORY_MAX_PAGES × HISTORY_PAGE` = 20,000 events and sets `truncated`, and `scanned` then
 * reports what it fetched — which past that point is THE CAP AND NOT THE COUNT. A run between
 * 20,001 and Temporal's 51,200 ceiling is legal, completes normally, and was recorded as exactly
 * 20,000. Nothing about that looks wrong on screen; it is only wrong for the largest runs, which
 * are the ones a meter would charge the most for.
 *
 * `DescribeWorkflowExecution` RETURNS IT WITHOUT READING AN EVENT, so this costs one round trip and
 * no blob GETs — the property that let it be added to a path every browser poll takes.
 * `historySizeBytes` comes back in the same response and rides along, because a byte-based meter
 * would otherwise need a second call to ask for it.
 *
 * IT NEVER THROWS. The events are already in hand; a describe that fails must cost the caller the
 * exact count and not the log. Absent then means NOT ESTABLISHED — `scanned` is still there as the
 * honest floor, and `RunHistory.historyLength` documents that absent is not zero.
 *
 * `execId` IS PASSED FOR THE REASON `fetchRunHistory` STATES: a workflow id can be reused, and
 * asking by id alone answers with whichever execution ran last. A length taken from a different
 * execution than the events would be worse than no length at all.
 */
/**
 * A count off the raw gRPC decode, as a number — `undefined` when the server did not send one.
 *
 * `Number(long)` IS `NaN`, AND A TEST CAUGHT THAT. The raw service returns 64-bit fields as
 * protobufjs Longs — an object carrying `low`/`high` — and `Number()` on one is not a number at all.
 * `shared/core/src/history.ts:num` already goes through `toString()` for exactly this reason, on
 * exactly this wire; this is the same rule, kept local because it answers `undefined` where that one
 * answers `0`, and here those must not be the same thing.
 *
 * ZERO IS A LEGAL COUNT AND `undefined` IS NOT A COUNT. A history really can have zero events —
 * `kontra-dataset-retention-workflow-…` sat for 18 days without running a task — so collapsing the
 * two would turn "the server did not say" into "the run did nothing".
 */
function longToNumber(raw: unknown): number | undefined {
  if (raw === undefined || raw === null) return undefined;
  if (typeof raw === 'number') return Number.isFinite(raw) ? raw : undefined;
  const n = Number((raw as { toString(): string }).toString());
  return Number.isFinite(n) ? n : undefined;
}

// EXPORTED FOR ITS TEST, and that is the whole reason. `fetchRunHistory` reaches Temporal through a
// memoized module-level client, so driving this through it means mocking `@temporalio/client` — and
// a factory that loads the real package to spread it pulls protobufjs into a worker that cannot
// resolve it. Measured: six cases green alone, six red the moment the file shared a worker. Taking
// the client as an argument was already true; naming it here is what lets a test hand one over.
export async function describeLength(
  client: Awaited<ReturnType<typeof getClient>>,
  runId: string,
  execId?: string
): Promise<{ historyLength?: number; historySizeBytes?: number }> {
  try {
    const desc = await client.workflowService.describeWorkflowExecution({
      namespace: client.options.namespace,
      execution: { workflowId: runId, ...(execId ? { runId: execId } : {}) },
    });
    const info = desc.workflowExecutionInfo;
    const len = longToNumber(info?.historyLength);
    const bytes = longToNumber(info?.historySizeBytes);
    return {
      ...(len === undefined ? {} : { historyLength: len }),
      ...(bytes === undefined ? {} : { historySizeBytes: bytes }),
    };
  } catch {
    return {};
  }
}

/** The raw service answers a missing workflow with a gRPC NOT_FOUND (code 5) rather than with the
 *  typed client's `WorkflowNotFoundError`, so `isNotFound` alone would turn a plain "no such run"
 *  into a 502 about an outage. */
function isNotFoundStatus(err: unknown): boolean {
  return (err as { code?: unknown } | null)?.code === 5;
}

/**
 * A run's live per-Method-call heartbeat progress. Discovers the run's handler backing
 * workflows via `KontraRunId`, describes each, and decodes the RunBatch pending activity's
 * {node,done,total,isolated} heartbeat detail.
 *
 * The activity name is a STRING MATCH against another process's registration, and a miss is
 * silent — it yields an empty map, which renders identically to a run that has not started. It
 * went stale exactly that way once: ADR 0018 renamed the activity to `RunBatch` when the actor
 * started serving it itself, and live progress quietly went blank.
 *
 * Empty when the run hasn't dispatched anything or has no live Batch. Never throws:
 * heartbeats are best-effort.
 */
export async function describeRunHeartbeats(runId: string): Promise<Record<string, NodeHeartbeat>> {
  const client = await getClient();
  const codec = dataConverter.payloadCodecs[0];
  const out: Record<string, NodeHeartbeat> = {};
  const query = `KontraRunId = '${runId.replace(/'/g, "''")}' AND WorkflowType = '${RUN_WORKFLOW_TYPE}'`;
  for await (const info of client.workflow.list({ query })) {
    let desc;
    try {
      desc = await client.workflowService.describeWorkflowExecution({
        namespace: client.options.namespace,
        execution: { workflowId: info.workflowId },
      });
    } catch {
      continue; // a workflow may close between list and describe — just skip it
    }
    for (const pa of desc.pendingActivities ?? []) {
      if (pa.activityType?.name !== RUN_BATCH_ACTIVITY) continue;
      const detail = await decodeHeartbeat(codec, pa.heartbeatDetails?.payloads);
      if (!detail) continue;
      const row = heartbeatRow(
        detail,
        info.workflowId,
        pa.attempt ?? 0,
        timestampToMs(pa.lastHeartbeatTime)
      );
      out[row.node] = row;
    }
  }
  return out;
}

/** Decode a heartbeat detail from the raw describe path. The codec is applied defensively
 *  (the tiny {node,done,total} stays below the claim-check threshold, so it is a passthrough)
 *  before the default payload converter deserializes it. */
async function decodeHeartbeat(
  codec: PayloadCodec | undefined,
  payloads: unknown[] | null | undefined
): Promise<HeartbeatDetail | undefined> {
  if (!payloads || payloads.length === 0) return undefined;
  try {
    const raw = payloads as unknown as Payload[];
    const decoded = codec ? await codec.decode(raw) : raw;
    const first = decoded[0];
    if (!first) return undefined;
    return defaultPayloadConverter.fromPayload<HeartbeatDetail>(first);
  } catch {
    return undefined; // an undecodable shape must not break the monitor
  }
}

/** Proto Timestamp ({seconds, nanos}) -> epoch ms. Handles both a numeric `seconds` and a
 *  protobufjs Long (via toString), so no `long` import is needed. */
function timestampToMs(ts?: { seconds?: unknown; nanos?: unknown } | null): number {
  if (!ts) return 0;
  const rawSeconds = ts.seconds as { toString(): string } | number | null | undefined;
  const seconds = typeof rawSeconds === 'number' ? rawSeconds : Number(rawSeconds?.toString() ?? 0);
  const nanos = Number(ts.nanos ?? 0);
  return Number.isFinite(seconds) ? seconds * 1000 + Math.floor(nanos / 1e6) : 0;
}

/** Is this "that workflow does not exist" rather than "Temporal is unwell"? Matched by name
 *  because the client's error class is not exported from every entry point. */
function isNotFound(err: unknown): boolean {
  return (err as { name?: string } | null)?.name === 'WorkflowNotFoundError';
}

/** Map Temporal's execution status to the contract's RunStatus. */
function mapStatus(s: unknown): RunStatus {
  const name = typeof s === 'string' ? s : ((s as { name?: string } | null)?.name ?? '');
  switch (name) {
    case 'RUNNING':
    case 'CONTINUED_AS_NEW':
      return 'running';
    case 'COMPLETED':
      return 'completed';
    case 'CANCELLED':
    case 'CANCELED':
      return 'cancelled';
    case 'FAILED':
    case 'TERMINATED':
    case 'TIMED_OUT':
      return 'failed';
    default:
      return 'pending';
  }
}

/**
 * WHAT A RUN WAS STARTED WITH AND WHAT IT RETURNED — the two payloads, and nothing else.
 *
 * ── WHY IT IS NOT `fetchRunHistory` ────────────────────────────────────────────────────────────
 *
 * That function is emphatic that it reads NO payload, and it is right to be: it walks up to 20,000
 * events, and on this deployment the converter is a claim-check codec (ADR 0007), so decoding them
 * would fan a browser poll out into thousands of blob GETs. The rule it is protecting is "a
 * history read costs no blob reads", not "no route may ever decode a payload".
 *
 * This decodes exactly TWO — the run's argument and its result — and fetches them with two
 * targeted RPCs rather than by paging anything:
 *
 *   - the FIRST event, `maximumPageSize: 1`, which is always `WorkflowExecutionStarted`;
 *   - the CLOSE event, via `historyEventFilterType: CLOSE_EVENT`, which is Temporal answering
 *     "the last event" without sending the ones before it.
 *
 * So the cost is bounded by the size of one workflow argument and one workflow result, whatever
 * the run did in between. A run that is still going has no close event and gets `output: undefined`
 * — which is the honest answer and not an error.
 *
 * ── WHY THE RUN PAGE NEEDED IT ─────────────────────────────────────────────────────────────────
 *
 * The page rendered the DECLARED SCHEMA in place of the run: the Input region printed each field's
 * default and footnoted "the run's recorded values land with the snapshot store", and the Output
 * region printed field names and descriptions with no values at all. So two runs of the same
 * workflow started with different arguments drew the identical page, and a reader had no way to
 * see what THIS one did. A run record that shows the defaults is a form, not a record.
 *
 * ── FAILURE IS NOT SILENCE ─────────────────────────────────────────────────────────────────────
 *
 * `undefined` on either half means NOT AVAILABLE, and the caller says so rather than drawing an
 * empty object: Temporal drops an execution at retention, a payload may be a claim-check whose
 * blob has expired, and a failed run has a failure where its result would be. Each of those is a
 * different fact from "this run was started with nothing".
 */
export interface RunIO {
  /** The single `@workflow.run` argument, decoded. `undefined` when there was none or it is gone. */
  input?: unknown;
  /** The workflow's return value, decoded. `undefined` while it is still running, or on failure. */
  output?: unknown;
  /** Why the close event carries no output — `failed`, `canceled`, `terminated`, `timed_out`. */
  closedAs?: string;
}

/** One payload list through the codec and the default converter. `undefined` on anything odd,
 *  because a run record must not 500 over an argument it cannot read. */
async function decodeFirst(
  codec: PayloadCodec | undefined,
  payloads: unknown[] | null | undefined
): Promise<unknown> {
  if (!payloads || payloads.length === 0) return undefined;
  try {
    const raw = payloads as unknown as Payload[];
    const decoded = codec ? await codec.decode(raw) : raw;
    const first = decoded[0];
    return first ? defaultPayloadConverter.fromPayload(first) : undefined;
  } catch {
    return undefined;
  }
}

/** Which terminal event this is, as the word the run page shows. */
function closedAsOf(ev: Record<string, unknown>): string | undefined {
  if (ev.workflowExecutionFailedEventAttributes) return 'failed';
  if (ev.workflowExecutionCanceledEventAttributes) return 'canceled';
  if (ev.workflowExecutionTerminatedEventAttributes) return 'terminated';
  if (ev.workflowExecutionTimedOutEventAttributes) return 'timed out';
  if (ev.workflowExecutionContinuedAsNewEventAttributes) return 'continued as new';
  return undefined;
}

export async function fetchRunIO(runId: string, execId?: string): Promise<RunIO | undefined> {
  const client = await getClient();
  const codec = dataConverter.payloadCodecs[0];
  const execution = { workflowId: runId, ...(execId ? { runId: execId } : {}) };
  const io: RunIO = {};

  try {
    const first = await client.workflowService.getWorkflowExecutionHistory({
      namespace: client.options.namespace,
      execution,
      maximumPageSize: 1,
      waitNewEvent: false,
    });
    const started = (first.history?.events ?? [])[0] as Record<string, unknown> | undefined;
    const attrs = started?.workflowExecutionStartedEventAttributes as
      | { input?: { payloads?: unknown[] } }
      | undefined;
    io.input = await decodeFirst(codec, attrs?.input?.payloads);
  } catch (err) {
    if (isNotFound(err) || isNotFoundStatus(err)) return undefined;
    throw err;
  }

  try {
    const last = await closeEventOf(client.workflowService, client.options.namespace, execution);
    if (last) {
      const done = last.workflowExecutionCompletedEventAttributes as
        | { result?: { payloads?: unknown[] } }
        | undefined;
      if (done) io.output = await decodeFirst(codec, done.result?.payloads);
      else {
        const why = closedAsOf(last);
        if (why) io.closedAs = why;
      }
    }
  } catch {
    // A RUNNING RUN HAS NO CLOSE EVENT, and asking for one is not an error worth propagating —
    // the input half is already in hand and is the half a live run has.
  }

  return io;
}

/** How long a live render waits for a workflow's `report` query before rendering without it. */
const REPORT_QUERY_TIMEOUT_MS = 3_000;

/**
 * An open run's own account of itself: its workflow's `report` query (ADR 0062 §2), or `undefined`.
 *
 * MOST WORKFLOWS DEFINE NO SUCH QUERY, and that is an answer, not a failure: the SDK raises for an
 * unknown query type and this returns `undefined`, so the template sees `result` as null exactly as it
 * did before. Same for a run whose worker is gone — Temporal cannot answer a query without one, and
 * would otherwise hold the request — so it is bounded by {@link REPORT_QUERY_TIMEOUT_MS}: a live
 * render renders without the query rather than waits for it.
 */
export async function queryRunReport(
  runId: string,
  timeoutMs = REPORT_QUERY_TIMEOUT_MS
): Promise<unknown | undefined> {
  let timer: NodeJS.Timeout | undefined;
  try {
    const client = await getClient();
    const answer = client.workflow.getHandle(runId).query<unknown>('report');
    const timeout = new Promise<undefined>((resolve) => {
      timer = setTimeout(() => resolve(undefined), timeoutMs);
      timer.unref?.();
    });
    return await Promise.race([answer, timeout]);
  } catch {
    return undefined;
  } finally {
    if (timer) clearTimeout(timer);
  }
}
