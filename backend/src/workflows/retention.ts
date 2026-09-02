/**
 * `sweepDatasetsWorkflow` — the workflow a retention Schedule starts, and the reason `SKIP` overlap
 * means anything (ADR 0029 §5).
 *
 * IT AWAITS THE SWEEP. A Temporal Schedule's overlap policy compares the LIFETIME of the workflow it
 * starts, so a workflow that dispatched a sweep and returned would let `SKIP` guard nothing — the sweep
 * would run uncovered, and two ticks could sweep at once. This repo has been bitten by exactly that
 * shape: `kontra schedule` once started a thin dispatcher that returned in a second and left three 24h
 * runs alive at once under `--overlap skip` (`schedule-overlap-guards-the-dispatcher`). So this
 * workflow's whole body is `await sweepDatasets(...)`: its lifetime IS the sweep's, and `SKIP` compares
 * the real thing.
 *
 * ONE HEARTBEATING ACTIVITY, PROXIED TO WHERE THE STORES ARE. The sweep reads the lake and three SQL
 * stores, which live on the dataset-queue worker (`materializer.ts`), not wherever this workflow is
 * hosted — so the activity is pinned to that queue. It heartbeats, so a long sweep is liveness, not a
 * timeout, and a worker bounce re-polls rather than failing the tick.
 *
 * THE QUEUE IS INPUT, AND THERE IS NO OTHER SOURCE FOR IT. This file used to pin the `DATASET_QUEUE`
 * CONSTANT, which meant `KONTRA_DATASET_QUEUE` could not route a sweep anywhere: every sweep landed on
 * the shared queue and whichever worker polled it swept whichever lake it held. That is not a
 * theoretical gap — it is how a smoke test with three isolated queue names reached the LIVE
 * materializer and deleted 223,378 rows (`.scratch/post-merge-review/INCIDENT-2026-08-26.md`).
 *
 * AND `datasetQueue()` IS NOT THE FIX — measured, not assumed. Workflow code runs in a `vm` context
 * created EMPTY; `@temporalio/worker`'s `injectGlobals` puts `URL`, `assert`, `TextEncoder`,
 * `TextDecoder`, `AbortController` and a `console` in it, and nothing else. So `process` in here is not
 * merely a determinism hazard, it is not DEFINED: a `datasetQueue()` call at this module's scope
 * throws `ReferenceError: process is not defined` while the bundle is being imported, which fails the
 * workflow task for EVERY type in the bundle — `stackWorkflow` and `tmuxSessionWorkflow` with it — and
 * a failed workflow task retries forever. The caller sees a run that stays RUNNING with no error
 * anywhere, which is the invisible hang `workflows/appliance.ts` exists to prevent.
 *
 * So the queue is resolved OUTSIDE the sandbox, by the schedule creator (`src/retention.ts`, ordinary
 * Node code that may read `KONTRA_DATASET_QUEUE`), and travels as workflow INPUT — which is written
 * into history, so a replay proxies onto the identical string. Nothing here imports `queues.ts` any
 * more, and `KONTRA_DATASET_QUEUE` appearing nowhere in the workflow bundle is what pins that.
 *
 * AN UNSTATED QUEUE IS A REFUSAL, NOT A DEFAULT. Falling back to the shared constant would be the
 * defect again, one `??` further down: a sweep nobody told where to run would reach whatever worker
 * polls `kontra-datasets`, which on a shared cluster is somebody else's lake. Retention that does not
 * run costs storage; a sweep on the wrong lake costs data — so an unrouted sweep fails immediately and
 * says which field to state.
 */

import { ApplicationFailure } from '@temporalio/common';
import type { ActivityOptions } from '@temporalio/common';
import * as wf from '@temporalio/workflow';

import type { RetentionActivities, SweepDatasetsInput, SweepSummary } from '../activities/retention';

/**
 * Everything about the proxy EXCEPT where it goes. Held apart from the workflow body so the timeouts
 * stay one declaration while the queue stays per-execution.
 */
const SWEEP_ACTIVITY_OPTIONS: Omit<ActivityOptions, 'taskQueue'> = {
  // A sweep is a bounded catalog scan plus, on collection, a DELETE per collected Run — minutes at
  // worst on a large lake. The heartbeat is the liveness check; StartToClose is the backstop.
  startToCloseTimeout: '30 minutes',
  heartbeatTimeout: '2 minutes',
  // One attempt per tick. A failed sweep waits for the NEXT scheduled firing rather than hammering the
  // stores inside one workflow — and because the workflow awaits this, a retry storm would also hold
  // the overlap window open. The schedule's cadence is the retry.
  retry: { maximumAttempts: 1 },
};

/** The `type` on the refusal, so a caller (or a test) branches on it without matching prose — the same
 *  arrangement as `workflows/appliance.ts`'s `NO_PROVISIONER`. */
export const NO_DATASET_QUEUE = 'NoDatasetQueue';

/** What an operator reads when a sweep was started without being told where to run. It names the
 *  field, the variable behind it, and why there is no default to fall back on. */
export const NO_DATASET_QUEUE_MESSAGE =
  'this sweep was not told which dataset queue to run on, and there is no default: state ' +
  '`datasetQueue` in the workflow input. The retention Schedule pins it from KONTRA_DATASET_QUEUE ' +
  'when it is armed (`src/retention.ts`), so a firing always carries it — a start that does not is ' +
  'either hand-made or from a Schedule armed before this was required. Defaulting to the shared ' +
  '`kontra-datasets` queue is what let one control plane sweep another one’s lake.';

/**
 * What a firing sweeps.
 *
 * THE MODE IS NOT DECIDED HERE, and that is the safety. An OMITTED `dryRun` means "whatever the
 * worker holding the lake is configured for" (`KONTRA_RETENTION_COLLECT`, resolved in
 * `activities/retention.ts`), which defaults to a dry run — so a firing with no arguments at all,
 * including one an operator triggers by hand from `kontra schedule trigger`, previews and deletes
 * nothing. A stated `dryRun` still wins in both directions.
 *
 * This replaced a default APPLIED HERE, which was dead code and a trap: `{ dryRun: input.dryRun ??
 * false, ...input }` spread the input AFTER the default, so a present-but-nullish `dryRun` passed
 * straight through and the safety depended on a second `??` two files away — one whose default was
 * the OPPOSITE value (TRIAGE-2026-08-25 §4). There is now one default, in one place, and it keeps
 * data.
 *
 * WHERE THE DEFAULT LIVES NOW, all three layers agreeing and all three failing towards keeping data:
 * this workflow states none, the activity resolves the deployment's posture (`input.dryRun ??
 * !retentionCollects()` — a dry run unless the deployment opted in), and `data/retention.ts`'s
 * `opts.dryRun ?? true` is the floor under both. The two that were once opposite now agree, so
 * neither is load-bearing against the other; what {@link stated} below removes is the last way an
 * input could reach them and mean something they cannot express.
 *
 * THE QUEUE IS THE ONE FIELD THIS WORKFLOW READS FOR ITSELF, and the only one it insists on. It is
 * WHERE the sweep runs rather than WHAT it sweeps, so it is stripped before the activity is called —
 * `SweepDatasetsInput` is unchanged and the activity is handed exactly what it was handed before.
 */
export interface SweepDatasetsWorkflowInput extends SweepDatasetsInput {
  /**
   * The task queue the sweep ACTIVITY runs on — the worker holding the lake and the SQL stores.
   *
   * REQUIRED IN EFFECT. There is no default: see the file header. `src/retention.ts` resolves it from
   * `KONTRA_DATASET_QUEUE` outside the sandbox and pins it into the Schedule's arguments, which is
   * what makes a second control plane on one cluster sweep its own lake and only its own.
   */
  datasetQueue?: string;
}

/**
 * The input the sweep is actually given: the keys the caller STATED, with the nullish ones dropped.
 *
 * A `null` SURVIVES THE WIRE AND THE TYPE SYSTEM DOES NOT KNOW. `dryRun?: boolean` says a value is
 * either a boolean or absent, and for `undefined` that is true — the JSON payload converter drops the
 * key, so `{ dryRun: undefined }` is received as `{}`. `null` is not dropped: it arrives, it is
 * neither `true` nor `false` nor absent, and forwarding it means every layer downstream is one
 * `??` away from treating "unstated" as a mode. That is the shape the dead default hid behind, and it
 * outlived the default.
 *
 * So a nullish key is made ABSENT here, at the boundary it entered through, rather than coerced to a
 * mode. Coercing would be the wrong repair in the dangerous direction: `dryRun: false` means COLLECT,
 * so resolving an unstated mode to a boolean HERE would hand a firing a deletion order that nobody
 * wrote. Absent is what the caller meant, absent is what the deployment's own posture answers, and
 * that answer is a dry run.
 *
 * A nullish `datasetQueue` takes the same treatment and lands in the same place — absent, which is a
 * refusal rather than a mode, for the reason the header gives.
 *
 * Nullish-tolerant on the whole input too — `args: [null]` reaches a workflow as `null`, which the
 * parameter default (only fired by `undefined`) does not catch.
 */
function stated(input: SweepDatasetsWorkflowInput | null | undefined): SweepDatasetsWorkflowInput {
  const out: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(input ?? {})) {
    if (value !== undefined && value !== null) out[key] = value;
  }
  return out as SweepDatasetsWorkflowInput;
}

export async function sweepDatasetsWorkflow(
  input: SweepDatasetsWorkflowInput = {}
): Promise<SweepSummary> {
  // WHERE, split from WHAT. The rest is the activity's input, unchanged from before this field
  // existed — the queue is routing and the activity never sees it.
  const { datasetQueue, ...sweep } = stated(input);

  if (typeof datasetQueue !== 'string' || datasetQueue.trim() === '') {
    // NON-RETRYABLE: "nobody said where" will not become true on the third attempt, and a retry here
    // would hold the Schedule's overlap window open against a firing that can never succeed.
    throw ApplicationFailure.nonRetryable(NO_DATASET_QUEUE_MESSAGE, NO_DATASET_QUEUE);
  }

  // PROXIED PER EXECUTION, from a value that came in over the wire. That is deterministic in the way
  // the sandbox demands: the input is in history, so a replay builds the identical proxy — which a
  // `process.env` read in here could not do, because there is no `process` in here at all.
  const { sweepDatasets } = wf.proxyActivities<RetentionActivities>({
    ...SWEEP_ACTIVITY_OPTIONS,
    taskQueue: datasetQueue,
  });

  // AWAIT — the workflow represents the work, so the Schedule's overlap policy has the sweep's real
  // lifetime to compare.
  //
  // AND IT RETURNS THE SUMMARY THE ACTIVITY RETURNED, unchanged and bounded: a workflow result is
  // written into history exactly like an activity result, so returning the full per-Dataset report
  // here would have paid the catalog's size a second time on every tick.
  return sweepDatasets(sweep);
}
