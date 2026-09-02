/**
 * Creating the retention Schedule — the control-plane half of ADR 0029 §5.
 *
 * This lives OUTSIDE the workflow bundle on purpose: it speaks the `@temporalio/client` Schedule API,
 * which cannot run in the workflow sandbox, and it names the workflow by STRING so importing it drags
 * no workflow code into a client module.
 *
 * WHO CALLS IT: the `orchestrator-infra` worker, at boot, once its Temporal connection is up
 * ({@link armRetentionSchedule} ← `infra.ts`). That process and not the API, because it is the one
 * HOSTING `sweepDatasetsWorkflow` — arming a Schedule whose task queue nobody polls would start a
 * workflow per hour that never runs, and `SKIP` would then skip every firing behind the first stuck
 * one, which reads as "retention is armed" while nothing is ever swept.
 *
 * AND IT RESOLVES BOTH OF THE SWEEP'S QUEUES, which is not incidental. Being outside the sandbox is
 * the only place `KONTRA_DATASET_QUEUE` can be READ — workflow code runs in a `vm` context with no
 * `process` in it at all — so the queue the sweep ACTIVITY runs on is resolved here and pinned into
 * the Schedule's workflow arguments. That is what lets a second control plane on one cluster sweep
 * its own lake and only its own (`.scratch/post-merge-review/INCIDENT-2026-08-26.md`).
 *
 * `SKIP` IS THE WHOLE POINT, and it is only sound because `sweepDatasetsWorkflow` AWAITS its sweep
 * (see `workflows/retention.ts`). A Schedule's overlap policy compares the started workflow's lifetime,
 * and this workflow's lifetime IS the sweep's — so `SKIP` genuinely prevents two sweeps running at
 * once, rather than being the no-op it is against a workflow that returns immediately (the bug that once
 * ran three 24h dispatcher runs at once in this repo).
 */

import type { ScheduleOptions } from '@temporalio/client';
import { ScheduleOverlapPolicy } from '@temporalio/client';
import type { Duration } from '@temporalio/common';

import { datasetQueue, infraQueue } from './queues';
import type { SweepDatasetsWorkflowInput } from './workflows/retention';

/** The schedule's stable id — one retention sweep per namespace. */
export const RETENTION_SCHEDULE_ID = 'kontra-dataset-retention';

/** How often the sweep fires. Hourly: the grace window is hours and the TTL a day, so an hourly sweep
 *  collects an expired Dataset within an hour of the grace closing, at a cost of one bounded catalog
 *  scan. */
export const DEFAULT_RETENTION_INTERVAL: Duration = '1h';

/** The one method this helper needs — so a test hands a spy instead of a live `ScheduleClient`. It is
 *  `Client['schedule']`. */
export interface ScheduleCreator {
  create(options: ScheduleOptions): Promise<{ scheduleId: string }>;
}

export interface RetentionScheduleOptions {
  /** Firing cadence, a Temporal interval string (`'1h'`, `'30m'`). Defaults to hourly. */
  interval?: Duration;
  /** The queue the workflow is hosted on. Defaults to the infra worker's queue. */
  taskQueue?: string;
  /**
   * The queue the sweep ACTIVITY runs on — the worker holding the lake. Defaults to
   * {@link datasetQueue}, i.e. `KONTRA_DATASET_QUEUE` or `kontra-datasets`.
   *
   * PINNED INTO THE ARGUMENTS ALWAYS, unlike {@link dryRun}, and the two are opposite on purpose.
   * WHERE a sweep runs can only be decided by whoever arms the Schedule, because the workflow's
   * sandbox cannot read an environment; WHAT MODE it runs in can only be decided by the worker that
   * would do the deleting. Each is settled by the process that is able to settle it.
   */
  datasetQueue?: string;
  /**
   * PIN the mode into the Schedule's arguments, instead of leaving each firing to resolve the
   * deployment's own posture.
   *
   * OMITTED IS THE PRODUCTION SHAPE, and the boot caller omits it. A firing with no `dryRun` asks the
   * worker holding the lake what this deployment is configured for
   * (`KONTRA_RETENTION_COLLECT`, `activities/retention.ts`), which defaults to a DRY RUN — so the
   * first deployment of this schedule previews and deletes nothing until somebody says otherwise, and
   * saying otherwise is a configuration change on the process that would do the deleting rather than
   * a Schedule that has to be destroyed and rebuilt to change its mind.
   *
   * That last part is the reason this is not simply defaulted to `false` here: {@link
   * createRetentionSchedule} is idempotent by design, so whatever a FIRST boot pinned into the
   * arguments would outlive every later change to them.
   */
  dryRun?: boolean;
}

/**
 * Build the `ScheduleOptions` for the retention sweep — overlap `SKIP`, hourly, starting
 * `sweepDatasetsWorkflow`. Exported so its shape is testable without a live cluster: the overlap
 * policy, the awaited-workflow target and the two queues are what ADR 0029 §5 turns on.
 *
 * THE TWO QUEUES A FIRING NEEDS ARE BOTH RESOLVED HERE, both through `queues.ts`.
 *
 *   - WHERE THE WORKFLOW IS HOSTED: {@link infraQueue}, the controller's `orchestrator-infra`
 *     worker, which already hosts the other controller-pinned workflows (ADR 0019, ADR 0020). It
 *     rides in `action.taskQueue`.
 *   - WHERE THE SWEEP ACTIVITY RUNS: {@link datasetQueue}, the worker holding the DuckLake attach
 *     and the SQL stores. It rides in the workflow ARGUMENTS, because the workflow cannot resolve it
 *     — its sandbox has no `process` (see `workflows/retention.ts` for what happens to a bundle that
 *     tries), so `KONTRA_DATASET_QUEUE` can only be read out here.
 *
 * NEITHER IS RE-DERIVED, which is the rule `roles.ts` states for the same reason: a second
 * derivation agrees with the worker until one of them changes. A local
 * `process.env.KONTRA_INFRA_QUEUE ?? 'kontra-infra'` used to sit in this file to keep
 * `@temporalio/worker` out of a client module; the constant moved into `queues.ts` when the roles
 * merged (ADR 0031 §1), and that module imports nothing at all, so the copy bought nothing and could
 * only drift.
 */
export function retentionScheduleOptions(opts: RetentionScheduleOptions = {}): ScheduleOptions {
  // THE QUEUE IS ALWAYS STATED, even when it is the default one. An argument that says
  // `kontra-datasets` outright is a routing decision an operator can read out of `kontra schedule
  // describe`; an argument that omits it is a sweep that would have to guess, and the workflow
  // refuses to guess — guessing is what reached another control plane's lake.
  //
  // THE MODE IS STILL UNSTATED unless pinned — see `RetentionScheduleOptions.dryRun`. The workflow
  // passes it through and the activity resolves it, so a firing with no mode is a dry run.
  const args: [SweepDatasetsWorkflowInput] = [
    {
      datasetQueue: opts.datasetQueue ?? datasetQueue(),
      ...(opts.dryRun === undefined ? {} : { dryRun: opts.dryRun }),
    },
  ];
  return {
    scheduleId: RETENTION_SCHEDULE_ID,
    spec: { intervals: [{ every: opts.interval ?? DEFAULT_RETENTION_INTERVAL }] },
    action: {
      type: 'startWorkflow',
      workflowType: 'sweepDatasetsWorkflow',
      taskQueue: opts.taskQueue ?? infraQueue(),
      args,
    },
    policies: {
      // The guard. Meaningful ONLY because the workflow awaits the sweep — a firing is skipped while
      // the previous sweep is still running, so two sweeps never delete at once.
      overlap: ScheduleOverlapPolicy.SKIP,
    },
  };
}

/**
 * Create (or leave in place) the retention Schedule. Idempotent by the stable id: a second call
 * against a namespace that already has it is a no-op, so this is safe to run at every boot. Returns
 * the schedule id in both cases.
 */
export async function createRetentionSchedule(
  schedules: ScheduleCreator,
  opts: RetentionScheduleOptions = {}
): Promise<string> {
  try {
    const handle = await schedules.create(retentionScheduleOptions(opts));
    return handle.scheduleId;
  } catch (err) {
    // Already exists is success, not failure — the schedule is a namespace singleton and boot must be
    // idempotent. Any other error is real and propagates.
    if (isAlreadyExists(err)) return RETENTION_SCHEDULE_ID;
    throw err;
  }
}

/** Whether an error is Temporal's "this schedule id already exists". Matched loosely because the
 *  client wraps it (`ScheduleAlreadyRunning`) and the code is what is stable across wrappings. */
function isAlreadyExists(err: unknown): boolean {
  const name = (err as { name?: unknown })?.name;
  if (name === 'ScheduleAlreadyRunning') return true;
  const message = String((err as { message?: unknown })?.message ?? '');
  return /already (exists|running)/i.test(message);
}

/**
 * ARM the sweep at boot: create the Schedule if it is not there, and never stop a process over it.
 *
 * IDEMPOTENT BY THE STABLE ID — {@link createRetentionSchedule} treats "already exists" as success —
 * so this runs on every boot and a restart does not duplicate it, does not reset its next firing, and
 * does not undo a `kontra schedule pause`.
 *
 * IT SWALLOWS ITS FAILURES ON PURPOSE, which is the opposite of the rule the SWEEP follows. A sweep
 * that cannot read the record store must abort, because sweeping blind deletes kept output. A CONTROL
 * PLANE that cannot reach Temporal for one boot must still come up: this process also converges the
 * Fleet and supervises the Dashboard streamer, and taking `kontra fleet up` down because an hourly
 * housekeeping schedule could not be registered is a worse trade in every direction. The next boot
 * arms it; the log line says it did not.
 *
 * Returns the schedule id, or `undefined` when it could not be armed.
 */
export async function armRetentionSchedule(
  schedules: ScheduleCreator,
  opts: RetentionScheduleOptions & { log?: (line: string) => void } = {}
): Promise<string | undefined> {
  const { log, ...rest } = opts;
  // RESOLVED ONCE, so the queue this line NAMES is the queue the Schedule was PINNED to. Reading the
  // environment again for the log would be a second derivation of the thing this change exists to
  // stop deriving twice.
  const scheduleOpts = { ...rest, datasetQueue: rest.datasetQueue ?? datasetQueue() };
  try {
    const id = await createRetentionSchedule(schedules, scheduleOpts);
    log?.(
      `retention schedule '${id}' armed — every ${scheduleOpts.interval ?? DEFAULT_RETENTION_INTERVAL}, ` +
        // WHICH LAKE, in the line an operator reads at boot. A sweep reaches exactly one worker and
        // deletes from exactly that worker's lake, so the queue is the blast radius — and the
        // 2026-08-26 incident was a sweep whose blast radius nobody could see until after it ran.
        `sweeping the lake held by the '${scheduleOpts.datasetQueue}' worker, ` +
        (scheduleOpts.dryRun === undefined
          ? "mode per firing from KONTRA_RETENTION_COLLECT on the worker holding the lake (unset = dry run, deletes nothing)"
          : `mode pinned dryRun=${scheduleOpts.dryRun}`)
    );
    return id;
  } catch (err) {
    log?.(
      `could not arm the retention schedule: ${err instanceof Error ? err.message : String(err)} ` +
        '— untagged Datasets will not expire until a boot succeeds'
    );
    return undefined;
  }
}
