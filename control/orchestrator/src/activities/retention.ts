/**
 * The retention-sweep ACTIVITY — the I/O half of ADR 0029 §3/§5, behind `sweepDatasetsWorkflow`.
 *
 * WHY AN ACTIVITY THE WORKFLOW AWAITS, and not a workflow that kicks a sweep off and returns. A
 * Temporal Schedule's overlap policy guards the LIFETIME of the workflow it starts, so a workflow
 * that returns immediately makes `SKIP` a no-op — the exact way the scheduled dispatcher once ran
 * three 24h runs at once (`schedule-overlap-guards-the-dispatcher`). The sweep is the work, so the
 * workflow AWAITS this activity and its lifetime IS the sweep's; `SKIP` then has something real to
 * guard. This heartbeats as it gathers and as it collects, so a long sweep looks alive to Temporal
 * rather than timing out, and a worker bounce mid-sweep re-polls instead of failing the schedule tick.
 *
 * IT RUNS WHERE THE STORES AND THE LAKE LIVE — the DATASET_QUEUE worker in `materializer.ts`, which
 * already holds the DuckLake attach, the ObjectStore and the SQL config the record/ledger/summary
 * stores resolve. The workflow is hosted elsewhere and PROXIES this onto that queue.
 */

import { Context } from '@temporalio/activity';

import { ObjectStore } from '../codec/objectStore';
import { datasetRecordStore, type DatasetRecordStore } from '../data/datasetRecords';
import {
  UNITS_RETENTION_DEFAULT_MS,
  runMaintenance,
  sweepUnits,
  type MaintenanceOp,
  type MaterializationLike,
  type MaintenanceResult,
  type UnitsSweepReport,
} from '../data/maintenance';
import { materializationStore, type MaterializationStore } from '../data/materializationStore';
import { LAKE, lakeConnection, lakeEnabled, resolveLakeConfig, type LakeConfig } from '../data/parquet';
import { runWorkflowStore, type RunWorkflowStore } from '../data/runWorkflows';
import {
  summarizeSweep,
  sweepDatasets,
  type RetentionDeps,
  type SweepOptions,
  type SweepSummary,
} from '../data/retention';
import { summaryStore, type SummaryStore } from '../data/summaries';
import { reportStore, type ReportStore } from '../report/store';

export type { SweepReport, SweepSummary } from '../data/retention';

/**
 * The environment variable that turns COLLECTION on — and the whole of the dry-run safety.
 *
 * IT IS READ HERE, IN THE PROCESS THAT WOULD DO THE DELETING, and not baked into the Schedule's
 * arguments. Two reasons, both learned the hard way in this repo:
 *
 *   - THE SCHEDULE IS CREATED IDEMPOTENTLY AT BOOT (`src/retention.ts`), so a mode carried in its
 *     arguments would be frozen by the FIRST boot that ever ran: the second boot's `create` is an
 *     already-exists no-op and would silently keep the old mode. A posture that can only be changed
 *     by deleting the Schedule is a posture nobody changes.
 *   - The arming process and the sweeping process are different containers. The one whose
 *     configuration should decide whether rows get deleted is the one holding the lake.
 *
 * So the default is a DRY RUN everywhere — an unset variable, a hand-triggered firing, a manually
 * started workflow — and collection is a deliberate act of configuration on the worker serving
 * `DATASET_QUEUE` (the materializer). An explicit `dryRun` in the input still wins, which is what
 * lets the preview path and the tests state the mode outright.
 */
export const RETENTION_COLLECT_ENV = 'KONTRA_RETENTION_COLLECT';

/**
 * How long a run's unit blobs are kept, in days. Unset falls to the documented 90.
 *
 * A SEPARATE KNOB FROM THE DATASET TTL, because they are separate questions with separate costs.
 * `units/` is the raw per-record plane — 99.1% of the measured object store, ~1.25 GiB/day on the
 * dev box — and it is intermediate: once a run's dispatches have materialized, the rows are in the
 * lake and the blobs are evidence, not data. The lake's own TTL is about the ANSWER and is measured
 * in hours; this is about the WORKING, and 90 days of it is ~112 GiB before the first object expires.
 *
 * So the library default stays at what the wiki documents and the deployment states a real number.
 */
export const UNITS_RETENTION_DAYS_ENV = 'KONTRA_UNITS_RETENTION_DAYS';

/** The units window this deployment is configured for. A value that is not a positive number falls
 *  back to the documented default rather than to zero — the failure direction that keeps data. */
export function unitsRetentionMs(env: NodeJS.ProcessEnv = process.env): number {
  const days = Number((env[UNITS_RETENTION_DAYS_ENV] ?? '').trim());
  if (!Number.isFinite(days) || days <= 0) return UNITS_RETENTION_DEFAULT_MS;
  return days * 24 * 60 * 60 * 1000;
}

/** Whether this deployment has opted IN to collection. Anything but a recognised affirmative is a
 *  dry run, including a typo — the failure direction that keeps data. */
export function retentionCollects(env: NodeJS.ProcessEnv = process.env): boolean {
  const v = (env[RETENTION_COLLECT_ENV] ?? '').trim().toLowerCase();
  return v === '1' || v === 'true' || v === 'yes' || v === 'on';
}

/** What the workflow hands the sweep: the mode and, for a test, the injected clock and constants. The
 *  workflow never invents policy — omitted fields fall to the repo constants inside the sweep. */
export interface SweepDatasetsInput {
  /** UNSET means "whatever this deployment is configured for" — see {@link RETENTION_COLLECT_ENV},
   *  which defaults to a dry run. Set explicitly, it wins over the environment in both directions. */
  dryRun?: boolean;
  ttlMs?: number;
  graceMs?: number;
  /** Injected clock, for tests that age a Dataset without waiting. Absent means the activity's own
   *  `Date.now()`. */
  now?: number;
}

/**
 * What a tick runs by default: the three steps that turn a DELETE into free space, and nothing else.
 *
 * `compact` and `orphans` are deliberately absent — see {@link createRetentionActivities}'s
 * `maintainLake`. This is the reclamation chain, not every operation DuckLake exposes.
 */
export const DEFAULT_MAINTENANCE_OPS: readonly MaintenanceOp[] = ['rewrite', 'snapshots', 'cleanup'];

/** What a firing asks of the lake. An omitted `dryRun` is the deployment's posture, as everywhere. */
export interface MaintainLakeInput {
  dryRun?: boolean;
  /** Override the chain. Omitted runs {@link DEFAULT_MAINTENANCE_OPS}. */
  ops?: readonly MaintenanceOp[];
  /** Epoch ms — only touch things older than this. A number, not a `Date`: this crosses the wire. */
  olderThan?: number;
}

/** What a firing asks of the object store. */
export interface SweepUnitsActivityInput {
  dryRun?: boolean;
  /** Omitted is the deployment's window — {@link UNITS_RETENTION_DAYS_ENV}, then the documented 90 days. */
  retentionMs?: number;
  /** Runs that survive any age. */
  keepRuns?: readonly string[];
  /** Injected clock, for tests that age a run without waiting. */
  now?: number;
}

export interface RetentionActivityDeps {
  store?: ObjectStore;
  lake?: Partial<LakeConfig>;
  /** The retention arms. Default to the process-wide singletons — the same stores the API's routes
   *  and the materializer's own activities resolve, over the same DB. */
  materialization?: MaterializationStore;
  records?: DatasetRecordStore;
  /** The Runs' caller-workflow identities (ADR 0029 §2) — what the sweep's report CALLS each
   *  Dataset, and the fourth thing a collected Run leaves behind if it is not purged with the rest. */
  workflows?: RunWorkflowStore;
  summaries?: SummaryStore;
  /** The Runs' **Reports** (ADR 0055) — the fifth arm, and the one that is purged FIRST because it is
   *  the only one holding unredacted credential bytes. */
  reports?: ReportStore;
}

/** The activities this factory returns — the type `sweepDatasetsWorkflow` proxies. The activities are
 *  built by a factory (they close over injected stores), so the workflow cannot proxy the module's
 *  top-level exports the way a plain-function activity module is proxied; it proxies THIS shape. */
export type RetentionActivities = ReturnType<typeof createRetentionActivities>;

export function createRetentionActivities(deps: RetentionActivityDeps = {}) {
  const store = deps.store ?? new ObjectStore();
  const lake = deps.lake ?? {};
  const materialization = deps.materialization ?? materializationStore();
  const records = deps.records ?? datasetRecordStore();
  const workflows = deps.workflows ?? runWorkflowStore();
  const summaries = deps.summaries ?? summaryStore();
  const reports = deps.reports ?? reportStore();

  const retentionDeps: RetentionDeps = {
    store,
    lake,
    materialization,
    records,
    workflows,
    summaries,
    reports,
  };

  return {
    /**
     * Sweep untagged Datasets past their TTL — reads the record (never `KontraTag`), the clock is
     * last write, a grace window sits behind the TTL, an `open` or temporary Dataset is never
     * collected. See `data/retention.ts` for the four safeguards this is the durable arm of.
     *
     * IT RETURNS A SUMMARY, NOT THE REPORT. An activity's return value is written into workflow
     * history, and the full report holds one decision per **Run** in the lake — an hourly schedule
     * would write the whole catalog into history on every tick, twice (the workflow returns it on
     * too), against a measured ~8k-reference ceiling. Counts and a bounded sample go into history;
     * the full list lives on `GET /api/datasets/retention/preview`, which is read once and dropped.
     */
    async sweepDatasets(input: SweepDatasetsInput = {}): Promise<SweepSummary> {
      const opts: SweepOptions = {
        // The deployment's posture unless the caller stated one. Dry run by default — see
        // {@link RETENTION_COLLECT_ENV}.
        dryRun: input.dryRun ?? !retentionCollects(),
        ...(input.ttlMs !== undefined ? { ttlMs: input.ttlMs } : {}),
        ...(input.graceMs !== undefined ? { graceMs: input.graceMs } : {}),
        ...(input.now !== undefined ? { now: input.now } : {}),
        heartbeat: (progress) => {
          // Best-effort: `Context.current()` throws outside an activity (a direct unit call), and the
          // heartbeat is liveness, not correctness — never let it fail the sweep.
          try {
            Context.current().heartbeat(progress);
          } catch {
            /* not running as an activity, or no heartbeat available */
          }
        },
      };
      return summarizeSweep(await sweepDatasets(retentionDeps, opts));
    },

    /**
     * THE STEP THAT TURNS A DELETE INTO FREE SPACE, and the one nothing ever called.
     *
     * `sweepDatasets` above writes `DELETE … WHERE run_id = …`. On a DuckLake that frees nothing —
     * measured, in `data/maintenance.test.ts`: the parquet is immutable, so a DELETE records which
     * rows are gone and the bytes stay on disk until the files are rewritten, the snapshots holding
     * them retired, and the retired files unlinked. `data/maintenance.ts` has implemented those three
     * the whole time and had no caller anywhere in the repo — no route, no CLI, no schedule.
     *
     * IT RUNS AFTER THE SWEEP IN THE SAME TICK, which is the only ordering that makes sense: the
     * files this reclaims are the ones that sweep just orphaned, and a separate schedule could fire
     * between the two and find nothing to do.
     *
     * COMPACTION AND THE ORPHAN PASS ARE NOT IN THE DEFAULT SET. Compaction rewrites live data for a
     * read-speed win nobody has asked for yet, and the orphan pass deletes files the catalog does not
     * know about — which on a lake being written to concurrently is the one operation here that can
     * destroy a commit in flight. Both are reachable by stating `ops`; neither happens on a tick.
     */
    async maintainLake(input: MaintainLakeInput = {}): Promise<MaintenanceResult[]> {
      if (!lakeEnabled(store, lake)) return [];
      const dryRun = input.dryRun ?? !retentionCollects();
      // THE CONNECTION IS SHARED AND IS NOT THIS ACTIVITY'S TO CLOSE.
      //
      // `lakeConnection` is MEMOIZED on `(catalog, dataPath)` in a module-level map, so this is the
      // same handle `sweepDatasets` uses two lines earlier and the same one every `publishBatch` on
      // this worker uses. An earlier draft closed it in a `finally`, reasoning it had opened it.
      //
      // WHAT THAT COST, MEASURED, AND IT WAS NOT A LEAK — IT WAS A WEDGE. The 22:00 firing closed the
      // shared handle and the memo kept serving the corpse. The materializer runs
      // `maxConcurrentActivityTaskExecutions: slots()` and `DEFAULT_SLOTS` is **1**
      // (`materializer.ts`), so the first activity to touch the dead handle did not merely fail —
      // it occupied the worker's ONLY slot, and every later activity on `kontra-datasets` queued
      // behind it. The next run's `publishBatch` was dispatched to the poller (so the backlog read
      // ZERO) and sat at `ACTIVITY_TASK_SCHEDULED` with no `STARTED` event, against a queue whose
      // poller Temporal reported as healthy. A run that stopped, with nothing in any log to say so.
      //
      // `queryEngine.ts` already states the rule for its own pool: "A SUPERSEDED CONNECTION IS
      // DROPPED, NOT CLOSED. `closeSync` on a handle that another in-flight query is still reading
      // would take that query down with it" — and measured that the addon frees the native instance
      // on GC anyway. `data/retention.ts:728` takes the same handle and likewise never closes it.
      // This was the only `closeSync` on a lake connection in the tree, and it was wrong.
      const conn = await lakeConnection(store, resolveLakeConfig(store, lake));
      return runMaintenance(conn, LAKE, {
        ops: input.ops ?? DEFAULT_MAINTENANCE_OPS,
        dryRun,
        ...(input.olderThan !== undefined ? { olderThan: new Date(input.olderThan) } : {}),
      });
    },

    /**
     * Collect the unit blobs of runs that are old enough, complete and unpinned.
     *
     * THE ONLY UNBOUNDED THING IN THE SYSTEM — see the header over `sweepUnits` for the measurement
     * (254,801 objects, 7.56 GiB, 99.1% of the store, six days, nothing collecting it). This is the
     * function four comments and a wiki table referred to as though it existed.
     *
     * THE MATERIALIZATION LEDGER IS READ HERE AND NOT INSIDE THE SWEEP, because the sweep is pure and
     * this is where the stores are. A run whose dispatches have not all reached `complete` keeps its
     * units whatever their age: `resolveBatch` reads these objects on every retry and fails the
     * dispatch outright if one is missing, so collecting them early converts a retryable failure into
     * permanent data loss.
     */
    async sweepUnits(input: SweepUnitsActivityInput = {}): Promise<UnitsSweepReport> {
      return sweepUnits(
        {
          store,
          async materializationState() {
            const out = new Map<string, MaterializationLike>();
            for (const d of await materialization.listDispatches()) out.set(d.runId, d.state);
            return out;
          },
        },
        {
          dryRun: input.dryRun ?? !retentionCollects(),
          retentionMs: input.retentionMs ?? unitsRetentionMs(),
          ...(input.keepRuns !== undefined ? { keepRuns: input.keepRuns } : {}),
          ...(input.now !== undefined ? { now: input.now } : {}),
          heartbeat: (progress) => {
            try {
              Context.current().heartbeat(progress);
            } catch {
              /* not running as an activity */
            }
          },
        }
      );
    },
  };
}
