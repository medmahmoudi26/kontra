/**
 * Visibility wiring for the execution monitor (roadmap platform-x100 #04). Kontra tags its
 * Temporal workflows with custom Search Attributes so `kontra monitor`'s EXECUTION views
 * (run list / detail / --state) read straight from Temporal Visibility instead of the
 * SQLite status cache: Temporal becomes the source of truth for run status, and the CLI
 * never needs cluster creds (the orchestrator proxies the visibility query, GET /api/executions).
 *
 * The SQLite `runs` table is NOT retired — it still owns idempotency-key -> runId and the
 * ADR-0006 persist-before-start record (visibility has no row until AFTER StartWorkflow) —
 * so this module is purely additive over that durable substrate. The DATA views
 * (--query/--export/--duckdb) are untouched; they read S3 blobs, never the run table.
 */

import type { Connection } from '@temporalio/client';
import {
  SearchAttributeType,
  TypedSearchAttributes,
  defineSearchAttributeKey,
} from '@temporalio/common';
import { temporal } from '@temporalio/proto';

import { SERVE_DEV_WORKFLOW } from './queues';

/** temporal.api.enums.v1.IndexedValueType.INDEXED_VALUE_TYPE_KEYWORD — all four SAs are
 *  small scalars, so Keyword (exact match / equality-filterable) is the right type. */
const KEYWORD = temporal.api.enums.v1.IndexedValueType.INDEXED_VALUE_TYPE_KEYWORD;

/** The custom Search Attributes kontra registers and tags. Types are immutable once
 *  registered, so all four are pinned Keyword (a future type change needs a NEW name).
 *
 *  `KontraGraph` was a fifth. It is not registered any more: there is no graph, so nothing
 *  could ever have set it — it was declared and left unwritten from the start. An attribute
 *  already registered on a namespace stays there harmlessly; nothing queries it.
 *
 *  `KontraTag` is a PROJECTION, not an authority (ADR 0029 §4). The Dataset record
 *  (`data/datasetRecords.ts`) owns a Dataset's tags; a live in-workflow `publish(..., tag=…)`
 *  writes that record FIRST and then MIRRORS the tag here, so `KontraTag` is a fast query path
 *  over the live window only. It CANNOT be written after the caller's execution closes
 *  (`UpsertSearchAttributes` is a workflow-internal command with no outside path — finding 6), so
 *  it is a frozen index that legitimately disagrees with the record for any Dataset tagged after
 *  its Run ended. Registering it is what makes the fast path exist; it is NEVER read as the
 *  retention source of truth — the sweeper reads the record (§5). */
export const KontraTenant = defineSearchAttributeKey('KontraTenant', SearchAttributeType.KEYWORD);
export const KontraRunId = defineSearchAttributeKey('KontraRunId', SearchAttributeType.KEYWORD);
export const KontraActor = defineSearchAttributeKey('KontraActor', SearchAttributeType.KEYWORD);
export const KontraTag = defineSearchAttributeKey('KontraTag', SearchAttributeType.KEYWORD);

const ALL = [KontraTenant, KontraRunId, KontraActor, KontraTag];

/**
 * The tenant stamp every start in this control plane carries — written once, here, because it was
 * previously written nowhere.
 *
 * WHAT THIS FIXES. `KontraTenant` was registered on the namespace and written by NOTHING. Two
 * readers took it, so every `tenant` this control plane reported was the empty string. `startRun`
 * was fixed first (ADR 0046's prerequisite); the INFRA side never goes through `startRun`, so
 * `leaseWorkflow`, `stackWorkflow` and the Probe were still unstamped — and
 * those are exactly the perpetual executions a meter has to attribute.
 *
 * AT START, NEVER BY UPSERT. Attributes on `start` ride inside `WorkflowExecutionStarted` and write
 * no extra event; an upsert inside the workflow is a command of its own, and adding one per Machine
 * per attach to a Warden's history is the precise cost the event-log audit exists to avoid.
 *
 * THE NAMESPACE IS THE TENANT (ADR 0036 §7, `CONTEXT.md`), so this records what a tenant IS rather
 * than inventing a second notion of one. It also means this stamp is a CONVENIENCE and not the
 * authority: `temporalClient.ts`'s `tenantOf` derives the same answer from the namespace it queried,
 * which is what makes the already-attached Wardens — the ones whose start site re-attaches by
 * `WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING` and which therefore can never acquire a stamp —
 * readable anyway.
 */
export function tenantAttributes(namespace: string): TypedSearchAttributes {
  return new TypedSearchAttributes([{ key: KontraTenant, value: namespace }]);
}

/**
 * Idempotently register the four Kontra Search Attributes on the namespace's visibility
 * store (Keyword). Registration MUST precede the first tagged start or visibility query,
 * or Temporal hard-errors ("column name is not a valid search attribute"). Self-healing:
 * each SA is added independently and an AlreadyExists is swallowed, so a partially-
 * registered namespace still converges. Never throws — a registration hiccup must not
 * break run reads (the SAs may already exist from a prior boot or the temporal CLI).
 *
 * `KontraTag` is registered here so an in-workflow `publish(..., tag=…)` can mirror to it over
 * the live window (ADR 0029 §4); it is a projection of the Dataset record, never the truth the
 * retention sweeper reads.
 */
export async function registerSearchAttributes(connection: Connection, namespace: string): Promise<void> {
  for (const key of ALL) {
    try {
      await connection.operatorService.addSearchAttributes({
        namespace,
        searchAttributes: { [key.name]: KEYWORD },
      });
    } catch (err) {
      if (!isAlreadyExists(err)) {
        console.warn(`[visibility] could not register search attribute ${key.name}: ${errMessage(err)}`);
      }
    }
  }
}

/**
 * Temporal's own system search attribute for "this execution belongs to a subsystem, not to a user".
 * The scheduler sets it to `TemporalScheduler` on every `temporal-sys-scheduler-workflow`; the batcher
 * sets its own. A run never has one, which is what makes `IS NULL` the right side of the test.
 */
export const NAMESPACE_DIVISION = 'TemporalNamespaceDivision';

/**
 * Every workflow type KONTRA ITSELF starts. Not runs — infrastructure.
 *
 * This is the whole list, and it is short because only two processes start workflows: the handler
 * starts one `ActorService.Run` per dispatch, and the infra worker hosts the two in
 * `workflows/infra.ts`. Everything else executing on this cluster is somebody's caller workflow.
 *
 * Temporal's OWN system executions are not here and must not be: they are excluded by namespace
 * division instead ({@link NAMESPACE_DIVISION}), which covers every subsystem at once rather than
 * needing a new literal each time Temporal grows one.
 *
 * KEEP IT IN SYNC WITH `workflows/infra.ts`. A new infra workflow that is not named here does not
 * break — it shows up on the Runs page as a run, which is wrong but visible and obviously so. That
 * is the correct direction for this list to fail in: a missing entry over-reports, and the
 * alternative arrangement (an allow-list of caller types) would silently under-report instead, which
 * is the bug this list was written to fix.
 */
export const KONTRA_INTERNAL_WORKFLOW_TYPES = [
  'kontra.v1.ActorService.Run',
  'stackWorkflow',
  /**
   * PRESSING SERVE — and the one entry here that now has a SHOWING half to agree with.
   *
   * Every other type on this list is hidden and then unlisted anywhere; this one is hidden HERE and
   * drawn on the Actors page, by `temporalClient.listServes`, as that folder's serve history. The
   * two read the same constant for the reason `queues.ts` sets out at {@link SERVE_DEV_WORKFLOW}:
   * a literal here and a literal there fail in opposite directions, and the history's direction —
   * "nothing has ever served this", about a folder served forty times — is the silent one.
   */
  SERVE_DEV_WORKFLOW,
  'sweepDatasetsWorkflow',
  /**
   * ONE PER **MACHINE**, AND IT NEVER CLOSES — the shape this list's failure direction was written
   * for, arriving at the worst possible scale. A **Warden**'s watcher (ADR 0037,
   * `cli/warden/warden_workflow.go`) is started by the Machine itself and blocks for that Machine's whole
   * life, so a ten-Machine Fleet is ten permanently-Running executions. Left out of this list they
   * would be ten rows on the Runs page that never finish, per Fleet, for ever.
   *
   * IT IS STILL READABLE, and that is why excluding it costs nothing: `GET /api/runs/:runId` fetches
   * a history by id and does not consult this list, so the Transcript renders a Machine's lifecycle
   * from the id `kontra warden status` prints. What this removes is a Fleet's infrastructure from
   * the list of things a CALLER ran, which is what the Runs page is.
   *
   * The fifth spelling of one Go constant (`wardenWorkflowType`), and `vocabulary.test.ts` pins it
   * against that file's bytes with the rest of the Warden's wire words.
   */
  'wardenWorkflow',
  /**
   * ONE PER **FLEET**, AND IT OUTLIVES EVERY **RUN** THAT HOLDS IT (ADR 0037) — the same shape as
   * the two above it, and it was the one omission in this list.
   *
   * OBSERVED, NOT REASONED ABOUT. `GET /api/runs` on the live cluster answered with two rows for one
   * `nscheck` run: the run, and `kontra-lease/kontra-fleet/dns` beside it. A **Lease** workflow is a
   * **Fleet**'s bookkeeping — started by an activity, never by a caller — so it is exactly "a Fleet's
   * infrastructure in the list of things a CALLER ran", which is what the entry above says this list
   * exists to remove.
   *
   * AND IT IS NOW LOAD-BEARING FOR THE ARCHIVE (kontra#10). The **Lease** workflow continues-as-new
   * when the server suggests it, so its workflow id resolves to a CHAIN. `historyArchive.ts` reads
   * by workflow id with no `execId`, and `fetchRunHistory` documents what that answers — "whichever
   * ran last" — so a swept chain would be archived as its final leg alone, with every earlier leg
   * silently absent. Excluding it means the sweep never sees a chain at all.
   *
   * THAT IS A FIX HERE AND NOT A FIX EVERYWHERE. The day a CALLER's workflow continues-as-new
   * (kontra#12 proposes exactly that) the sweep meets a chain it cannot exclude, and `read(runId)`
   * has to learn `firstExecutionRunId`. This list postpones that; it does not answer it.
   *
   * Still readable by id, on the same terms as the Warden: `GET /api/runs/:runId` does not consult
   * this list, so `kontra fleet leases` and the Transcript are unaffected.
   */
  'fleetLeaseWorkflow',
] as const;

/**
 * The visibility query that DISCOVERS runs.
 *
 * A **Run** is one execution of a caller's workflow (ADR 0023 §12) — the caller's own code on the
 * caller's own task queue. The orchestrator can START one (`POST /api/runs`, by registered folder
 * and type), but it does not own the set: `kontra workflow start` dials Temporal directly and a
 * caller may start their own workflow with no orchestrator in the picture at all. So it cannot
 * enumerate runs by type or by id prefix, and for a long time it did not try: it listed the
 * DISPATCHES instead
 * (`WorkflowType = 'kontra.v1.ActorService.Run'`, which carry the caller's id in `KontraRunId`) and
 * took the distinct ids.
 *
 * THAT DISCOVERED RUNS BACKWARDS, and the cost was not a cosmetic one. A run appeared only once it
 * had dispatched an Actor, so:
 *
 *   - a run spending its first ten minutes on setup showed nothing at all;
 *   - a run that FAILED before its first dispatch showed nothing — indistinguishable, on every
 *     surface, from a run that was never started;
 *   - a run that dispatches no Actors was permanently invisible;
 *   - the count in the nav was simply a different number from what Temporal held.
 *
 * Observed on this controller: `canary-1787009695` (4 events) and `sleeper-1786919716` (9 events)
 * were both Running, and `/api/runs` returned neither.
 *
 * So discovery is now by SUBTRACTION — every execution that is not one of kontra's own
 * (`KONTRA_INTERNAL_WORKFLOW_TYPES`) is a run, and it is discoverable from its first event. The
 * subtraction, rather than an allow-list of known caller types, because the orchestrator genuinely
 * does not know what a caller's workflows are called and must not need to.
 *
 * `NOT IN` IS SUPPORTED ON BOTH VISIBILITY BACKENDS — verified, not assumed, because the neighbour
 * rule here is a scar from exactly this: SQLite visibility (what `temporal start-dev` and CI run)
 * rejects `ORDER BY` outright. Probed against a live `start-dev` with one caller workflow and two
 * internal ones present: the query returned the caller and neither internal one, and the same
 * server rejected `ORDER BY` in the same session, which is what makes the pass meaningful.
 *
 * Still NO `ORDER BY` for that reason. Temporal returns executions newest-first by default and the
 * caller sorts defensively, so ordering survives on every backend.
 *
 * AND IT EXCLUDES TEMPORAL'S OWN SYSTEM EXECUTIONS BY NAMESPACE DIVISION, which is the clause
 * Temporal's CLI and Web UI use for the same purpose. Arming the retention sweep (ADR 0029 §5)
 * creates a Schedule, and a Schedule is backed by a permanently-Running
 * `temporal-sys-scheduler-workflow`: one per schedule, never closing, and a caller's workflow only in
 * the sense that it is not one of OURS either — the subtraction above would let it through, where it
 * would sit at the top of the Runs page forever and eat a slot of the listing's cap.
 *
 * MEASURED, and the measurement corrects the claim that sent me here (probe against this box's
 * Temporal 1.31.2, one live Schedule present — `docker-leaks-live`):
 *
 *   "WorkflowType NOT IN (…)"                                        -> 0 rows
 *   "WorkflowType = 'temporal-sys-scheduler-workflow'"               -> 0 rows
 *   "WorkflowType = '…' AND TemporalNamespaceDivision = 'TemporalScheduler'" -> 1 row
 *
 * So the SERVER already applies `TemporalNamespaceDivision is null` to any list query that does not
 * mention the attribute — the scheduler workflow was invisible to discovery before this clause
 * existed, and the leak this was written to prevent does not reproduce on 1.31.2. The clause stays
 * anyway, for the reason every other defence in this file stays: it is free, it is what Temporal's
 * own surfaces write, it makes the exclusion a property of THIS query rather than of an
 * implementation detail one server upgrade away, and the failure it guards against is silent.
 * `TemporalNamespaceDivision` is a SYSTEM search attribute, present on every backend (verified in the
 * same probe against this namespace's `listSearchAttributes`) — nothing needs registering.
 */
export function buildRunDiscoveryQuery(
  internalTypes: readonly string[],
  filter: { tenant?: string; status?: string } = {}
): string {
  const excluded = internalTypes.map((t) => `'${sqlQuote(t)}'`).join(', ');
  const clauses = [`WorkflowType NOT IN (${excluded})`, `${NAMESPACE_DIVISION} IS NULL`];
  if (filter.tenant) clauses.push(`KontraTenant = '${sqlQuote(filter.tenant)}'`);
  // The status filter can finally ride the QUERY. It used to be applied after describing each run,
  // and had to be: the discovery query matched dispatches, so a status clause there would have
  // filtered the dispatches rather than the run that made them. Now the query matches the caller's
  // own execution, which is the thing whose status is being asked about.
  const status = executionStatusFilter(filter.status);
  if (status) clauses.push(`ExecutionStatus = '${sqlQuote(status)}'`);
  return clauses.join(' AND ');
}

/** The dispatches ONE run made — the `dispatches` column, and the drill-down's entry point. Split
 *  out of discovery when discovery stopped depending on it. */
export function buildDispatchQuery(
  workflowType: string,
  filter: { tenant?: string } = {}
): string {
  const clauses = [`WorkflowType = '${sqlQuote(workflowType)}'`];
  if (filter.tenant) clauses.push(`KontraTenant = '${sqlQuote(filter.tenant)}'`);
  return clauses.join(' AND ');
}

/**
 * Every execution recorded under ONE workflow id — what a reused id's HISTORY is.
 *
 * THE THIRD QUERY IN THIS FILE, AND THE ONLY ONE THAT NARROWS TO A SINGLE SUBJECT. Discovery
 * subtracts kontra's own types; the dispatch query takes one type across the cluster; this takes
 * one type under one id, which is the shape you need the moment a workflow id is deliberately
 * REUSED. Two are: `kontra-fleet/<stack>` is every operation on that stack, and
 * `serve-dev/at:<folder>` is every press of Serve on that folder (`queues.ts:serveDevWorkflowId`).
 * Asking Temporal for that id alone answers with whichever ran LAST — the property `fetchRunHistory`
 * documents — so a history has to be a LIST query and cannot be a describe.
 *
 * `WorkflowId` IS A SYSTEM SEARCH ATTRIBUTE, present on every visibility backend with nothing to
 * register — the same standing as `ExecutionStatus`, which `buildRunDiscoveryQuery` already filters
 * on, and `TemporalNamespaceDivision`, which it already reads. Only equality is used, and there is
 * deliberately no `ORDER BY`, for the reason written at length above: SQLite visibility (what
 * `temporal start-dev` and CI run) rejects `ORDER BY` outright, so every caller in this module
 * sorts client-side.
 *
 * NO NAMESPACE-DIVISION CLAUSE, unlike discovery. That clause exists to keep Temporal's own
 * subsystem executions out of a list defined by SUBTRACTION; this query names one type and one id
 * that kontra itself started, so there is nothing for a subsystem to leak through.
 */
export function buildWorkflowIdQuery(workflowType: string, workflowId: string): string {
  return `${buildDispatchQuery(workflowType)} AND WorkflowId = '${sqlQuote(workflowId)}'`;
}

/** Narrow a run list by the caller workflow's own execution status. */
export function executionStatusFilter(status?: string): string | undefined {
  return executionStatusName(status);
}

/** Map a contract RunStatus (or a raw Temporal name) to the visibility ExecutionStatus
 *  enum name. An unrecognized value drops the filter rather than erroring the query. */
function executionStatusName(s?: string): string | undefined {
  if (!s) return undefined;
  switch (s.toLowerCase()) {
    case 'running':
      return 'Running';
    case 'completed':
      return 'Completed';
    case 'failed':
      return 'Failed';
    case 'cancelled':
    case 'canceled':
      return 'Canceled';
    case 'terminated':
      return 'Terminated';
    case 'timedout':
    case 'timed_out':
      return 'TimedOut';
    case 'continuedasnew':
    case 'continued_as_new':
      return 'ContinuedAsNew';
    default:
      return undefined;
  }
}

/** Escape single quotes for a visibility query string literal. */
function sqlQuote(s: string): string {
  return s.replace(/'/g, "''");
}

function isAlreadyExists(err: unknown): boolean {
  const code = (err as { code?: number } | null)?.code;
  // gRPC ALREADY_EXISTS is 6; also match the message defensively across server versions.
  return code === 6 || errMessage(err).toLowerCase().includes('already');
}

function errMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
