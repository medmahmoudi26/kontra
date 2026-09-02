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
import { materializationStore, type MaterializationStore } from '../data/materializationStore';
import type { LakeConfig } from '../data/parquet';
import { runWorkflowStore, type RunWorkflowStore } from '../data/runWorkflows';
import {
  summarizeSweep,
  sweepDatasets,
  type RetentionDeps,
  type SweepOptions,
  type SweepSummary,
} from '../data/retention';
import { summaryStore, type SummaryStore } from '../data/summaries';

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

  const retentionDeps: RetentionDeps = { store, lake, materialization, records, workflows, summaries };

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
  };
}
