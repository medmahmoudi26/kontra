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
import { mapHistory, type RawHistoryEvent, type RunHistory } from './history';
import { inFlightOf, type InFlight } from './runActivity';
import { startTracing, tracingEnabled } from './otel';
import {
  KONTRA_INTERNAL_WORKFLOW_TYPES,
  KontraRunId,
  KontraTenant,
  buildDispatchQuery,
  buildRunDiscoveryQuery,
  registerSearchAttributes,
} from './visibility';
import { temporalConnectOptions } from './temporalTls';

const NAMESPACE = process.env.KONTRA_NAMESPACE ?? 'default';

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
const LIST_LIMIT = 200;

/** How many backing workflows the `dispatches` column may scan before giving up on being exact.
 *  One sweep on this controller produced 225 of them and a busy cluster produces far more; the
 *  column is a volume indicator, so a bounded undercount is the right failure. */
const DISPATCH_SCAN_LIMIT = 5000;

let clientPromise: Promise<Client> | null = null;

/** Exported so the infra surface can start workflows on its own task queue (ADR 0019)
 * without opening a second connection to Temporal. */
export async function getClient(): Promise<Client> {
  if (!clientPromise) {
    clientPromise = (async () => {
      startTracing();
      const connection = await Connection.connect(temporalConnectOptions());
      const client = new Client({
        connection,
        namespace: NAMESPACE,
        dataConverter,
        interceptors: tracingEnabled
          ? { workflow: [new OpenTelemetryWorkflowClientInterceptor()] }
          : undefined,
      });
      // Register the custom Search Attributes exactly once, the first time we reach Temporal.
      // Doing it here (not at HTTP boot) preserves the "server boots with no cluster" property,
      // yet still guarantees registration BEFORE the first visibility query. Idempotent.
      await registerSearchAttributes(connection, NAMESPACE);
      return client;
    })();
  }
  return clientPromise;
}

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
      tenant: desc.typedSearchAttributes.get(KontraTenant) ?? '',
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
      tenant: info.typedSearchAttributes.get(KontraTenant) ?? '',
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
  const events: RawHistoryEvent[] = [];
  let nextPageToken: Uint8Array | undefined;
  let truncated = false;
  try {
    for (let page = 0; page < HISTORY_MAX_PAGES; page++) {
      const res = await client.workflowService.getWorkflowExecutionHistory({
        namespace: NAMESPACE,
        execution: { workflowId: runId, ...(execId ? { runId: execId } : {}) },
        maximumPageSize: HISTORY_PAGE,
        nextPageToken,
        // Long-poll off: this is a browser poll, and a request that parks for a minute waiting
        // for the next event is a request that holds a connection per open tab.
        waitNewEvent: false,
      });
      for (const ev of res.history?.events ?? []) events.push(ev as RawHistoryEvent);
      nextPageToken = res.nextPageToken?.length ? res.nextPageToken : undefined;
      if (!nextPageToken) break;
      if (page === HISTORY_MAX_PAGES - 1) truncated = true;
    }
  } catch (err) {
    if (isNotFound(err) || isNotFoundStatus(err)) return undefined;
    throw err;
  }
  // The namespace goes in so the reducer can refuse a link that points OUT of it — see mapHistory.
  return mapHistory(events, truncated, NAMESPACE);
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
        namespace: NAMESPACE,
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
