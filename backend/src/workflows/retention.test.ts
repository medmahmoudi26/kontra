/**
 * The overlap guard and the ROUTING, both proven rather than assumed (ADR 0029 §5).
 *
 * A Temporal Schedule's `SKIP` overlap policy compares the LIFETIME of the workflow it starts, so it
 * guards nothing against a workflow that dispatches work and returns — the shape that once ran three
 * 24h dispatcher runs at once under `--overlap skip` in this repo. `sweepDatasetsWorkflow` closes that
 * hole by AWAITING its sweep: its lifetime IS the sweep's. This file pins exactly that — while the
 * sweep activity is in flight, the workflow has NOT completed; it completes only once the sweep does.
 *
 * And it pins WHERE the sweep went, which is the other half and the one that cost data. The sweep
 * activity is proxied onto a queue the workflow is HANDED, so every test here runs a workflow worker
 * and one or more fake sweep workers, and asserts which of them was reached.
 */

import * as path from 'node:path';

import { afterEach, beforeAll, afterAll, describe, expect, it, vi } from 'vitest';
import { TestWorkflowEnvironment } from '@temporalio/testing';
import { Worker, bundleWorkflowCode, type WorkflowBundle } from '@temporalio/worker';
import type { ApplicationFailure } from '@temporalio/common';

import { DATASET_QUEUE } from '../queues';
import type { SweepSummary } from '../activities/retention';
import { RETENTION_SCHEDULE_ID, armRetentionSchedule, retentionScheduleOptions } from '../retention';
import { NO_DATASET_QUEUE } from './retention';

/** What the activity returns now: counts plus a bounded sample, never the per-Dataset list. The
 *  workflow hands this back UNCHANGED, which is what keeps a tick's history footprint constant. */
const SUMMARY: SweepSummary = {
  scanned: 3,
  counts: { collect: 0, 'kept-tagged': 3, 'kept-open': 0, 'kept-temporary': 0, 'kept-fresh': 0 },
  collectedRows: 0,
  dryRun: false,
  ttlMs: 1,
  graceMs: 1,
  purgedRuns: 0,
  purgedRows: 0,
  sample: { collected: [], kept: [] },
  sampleSize: 20,
  truncated: false,
};

let env: TestWorkflowEnvironment;
let bundle: WorkflowBundle;

beforeAll(async () => {
  env = await TestWorkflowEnvironment.createLocal();
  bundle = await bundleWorkflowCode({ workflowsPath: path.join(__dirname, 'retention.ts') });
}, 300_000);

afterAll(async () => {
  await env?.teardown();
});

afterEach(() => {
  vi.unstubAllEnvs();
});

/** Keep every worker polling for the duration of `body`, without nesting `runUntil` by hand. */
async function withWorkers<T>(workers: readonly Worker[], body: () => Promise<T>): Promise<T> {
  let run = body;
  for (const worker of [...workers].reverse()) {
    const inner = run;
    run = () => worker.runUntil(inner);
  }
  return run();
}

/** A fake sweep on `taskQueue` that records every input it is handed. The recorder IS the assertion:
 *  a queue nobody reached has an empty array. */
async function recordingSweepWorker(
  taskQueue: string,
  seen: Array<Record<string, unknown>>
): Promise<Worker> {
  return Worker.create({
    connection: env.nativeConnection,
    taskQueue,
    // ONE POLLER. Proving isolation needs several queues at once, and the default poller counts
    // times five workers is enough long-polling to push this box's dev server into transaction
    // timeouts — which presents as a hung suite, not a failing one.
    maxConcurrentActivityTaskPolls: 1,
    activities: {
      async sweepDatasets(input: Record<string, unknown> = {}): Promise<SweepSummary> {
        seen.push(input);
        return SUMMARY;
      },
    },
  });
}

/** A workflow worker for the given queue, on the one bundle this file builds. Cache off and one
 *  poller, for the reason above; these workflows are two events long, so replaying costs nothing. */
async function workflowWorker(taskQueue: string): Promise<Worker> {
  return Worker.create({
    connection: env.nativeConnection,
    taskQueue,
    workflowBundle: bundle,
    maxCachedWorkflows: 0,
    maxConcurrentWorkflowTaskPolls: 1,
  });
}

describe('sweepDatasetsWorkflow awaits its sweep — the overlap guard', () => {
  it('does not complete until the sweep activity completes', async () => {
    let started = false;
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });

    // The fake sweep BLOCKS on the gate — standing in for a sweep still running.
    const sweepWorker = await Worker.create({
      connection: env.nativeConnection,
      taskQueue: DATASET_QUEUE,
      activities: {
        async sweepDatasets(): Promise<SweepSummary> {
          started = true;
          await gate;
          return SUMMARY;
        },
      },
    });

    const wfQueue = 'retention-wf-test';
    const wfWorker = await workflowWorker(wfQueue);

    // Both workers alive for the duration of the body: the workflow worker until the outer runUntil's
    // promise settles, the sweep worker until the inner body returns.
    const result = await wfWorker.runUntil(
      sweepWorker.runUntil(async () => {
        const handle = await env.client.workflow.start('sweepDatasetsWorkflow', {
          taskQueue: wfQueue,
          workflowId: `retention-${Math.random().toString(36).slice(2, 8)}`,
          // The queue is STATED now, even when it is the default one — the workflow has no other
          // source for it and refuses to guess.
          args: [{ dryRun: false, datasetQueue: DATASET_QUEUE }],
        });

        // Let the activity actually start, or there is nothing to be blocked on.
        for (let i = 0; i < 100 && !started; i += 1) {
          await new Promise((r) => setTimeout(r, 50));
        }
        expect(started).toBe(true);

        // THE GUARD: with the sweep still in flight, the workflow has not finished. A workflow that
        // dispatched-and-returned would resolve here — and `SKIP` would guard nothing.
        const settled = await Promise.race([
          handle.result().then(() => 'done' as const),
          new Promise<'pending'>((r) => setTimeout(() => r('pending'), 400)),
        ]);
        expect(settled).toBe('pending');
        const desc = await handle.describe();
        expect(desc.status.name).toBe('RUNNING');

        // Release the sweep; NOW the workflow completes, returning the sweep's summary.
        release();
        return handle.result();
      })
    );

    expect(result).toMatchObject({ scanned: 3, dryRun: false });
  }, 60_000);
});

/**
 * TWO CONTROL PLANES ON ONE CLUSTER CANNOT RUN EACH OTHER'S SWEEPS — the property, asserted.
 *
 * This is the defect that cost 223,378 rows (`.scratch/post-merge-review/INCIDENT-2026-08-26.md`).
 * The workflow pinned its activity to the `DATASET_QUEUE` CONSTANT, so a second control plane with
 * `KONTRA_DATASET_QUEUE` set to an isolated name still sent its sweep to the shared queue, where the
 * LIVE materializer took it and swept the live lake.
 *
 * SO THE TEST IS BUILT OUT OF THE PRODUCTION PATH, not a hand-written input. Each plane's arguments
 * come from `retentionScheduleOptions()` reading that plane's environment — which is exactly what
 * `armRetentionSchedule` does at boot — and the workflow is started with them. Anything that breaks
 * the chain (creator resolves ⇒ Schedule carries ⇒ workflow is handed ⇒ activity is proxied) shows up
 * here as a sweep landing on the wrong recorder.
 *
 * THE TRIPWIRE IS A WORKER ON THE SHARED DEFAULT `kontra-datasets` — the live materializer's queue,
 * the one the incident reached. Every test in this block asserts it saw nothing at all, and one of
 * them puts that queue in `KONTRA_DATASET_QUEUE` while the sweeps run, so the tripwire is what the
 * sandbox would reach if it could read an environment.
 */
describe('two control planes on one cluster cannot run each other’s sweeps', () => {
  const A = { wf: 'plane-a-infra', datasets: 'plane-a-datasets' };
  const B = { wf: 'plane-b-infra', datasets: 'plane-b-datasets' };

  /** The arguments a control plane whose environment says `queues` would ARM — through the real
   *  schedule creator, so the env read under test is the one production performs. */
  function argsArmedBy(queues: { wf: string; datasets: string }): { taskQueue: string; args: unknown[] } {
    vi.stubEnv('KONTRA_INFRA_QUEUE', queues.wf);
    vi.stubEnv('KONTRA_DATASET_QUEUE', queues.datasets);
    try {
      const opts = retentionScheduleOptions();
      if (opts.action.type !== 'startWorkflow') throw new Error('unreachable');
      return { taskQueue: opts.action.taskQueue, args: opts.action.args as unknown[] };
    } finally {
      vi.unstubAllEnvs();
    }
  }

  it('each plane sweeps its own lake and only its own, whatever the host environment says', async () => {
    const seenA: Array<Record<string, unknown>> = [];
    const seenB: Array<Record<string, unknown>> = [];
    const seenShared: Array<Record<string, unknown>> = [];

    const workers = [
      await workflowWorker(A.wf),
      await workflowWorker(B.wf),
      await recordingSweepWorker(A.datasets, seenA),
      await recordingSweepWorker(B.datasets, seenB),
      // THE LIVE MATERIALIZER'S QUEUE. Reaching this is the incident.
      await recordingSweepWorker(DATASET_QUEUE, seenShared),
    ];

    const armedA = argsArmedBy(A);
    const armedB = argsArmedBy(B);

    // Each plane resolved its OWN queues, out here where an environment exists.
    expect(armedA.taskQueue).toBe(A.wf);
    expect(armedB.taskQueue).toBe(B.wf);
    expect(armedA.args).toEqual([{ datasetQueue: A.datasets }]);
    expect(armedB.args).toEqual([{ datasetQueue: B.datasets }]);

    // AND THE HOST PROCESS SAYS SOMETHING ELSE ENTIRELY while both sweeps run. If the sandbox could
    // read an environment — the "obvious fix" this change did not make — both would land on the
    // tripwire below instead of on their own lakes. It cannot, so the input is the only routing
    // there is.
    vi.stubEnv('KONTRA_DATASET_QUEUE', DATASET_QUEUE);

    await withWorkers(workers, async () => {
      for (const [plane, armed] of [
        ['a', armedA],
        ['b', armedB],
      ] as const) {
        await env.client.workflow.execute('sweepDatasetsWorkflow', {
          taskQueue: armed.taskQueue,
          workflowId: `retention-plane-${plane}-${Math.random().toString(36).slice(2, 8)}`,
          args: armed.args,
        });
      }
    });

    // ONE SWEEP EACH, ON ITS OWN LAKE. Before the fix both of these landed in `seenShared` — and so
    // would both of them now if the sandbox honoured the `KONTRA_DATASET_QUEUE` stubbed above.
    expect(seenA).toHaveLength(1);
    expect(seenB).toHaveLength(1);
    expect(seenShared).toHaveLength(0);

    // And the ROUTING FIELD IS NOT THE SWEEP'S INPUT: the activity is handed exactly what it was
    // handed before this field existed — no mode, so the worker holding the lake decides, and its
    // default is a dry run.
    expect(seenA[0]).toEqual({});
    expect(seenB[0]).toEqual({});
  }, 120_000);

  it('a sweep nobody routed reaches NOBODY — there is no default queue to fall back to', async () => {
    const seenA: Array<Record<string, unknown>> = [];
    const seenShared: Array<Record<string, unknown>> = [];

    const workers = [
      await workflowWorker(A.wf),
      await recordingSweepWorker(A.datasets, seenA),
      await recordingSweepWorker(DATASET_QUEUE, seenShared),
    ];

    // A control plane's environment is set — and it changes nothing, because the sandbox cannot read
    // it. Only what the caller STATED can route a sweep.
    vi.stubEnv('KONTRA_DATASET_QUEUE', A.datasets);

    const failures = await withWorkers(workers, async () => {
      const out: unknown[] = [];
      // Every shape that carries no queue: the empty input a Schedule armed before this was required
      // would fire with, an explicit nullish one, a stated MODE with no route, and `args: [null]`,
      // which reaches a workflow as `null` and does not fire the parameter default.
      for (const args of [[{}], [{ datasetQueue: null }], [{ dryRun: true }], [null], []]) {
        const err = await env.client.workflow
          .execute('sweepDatasetsWorkflow', {
            taskQueue: A.wf,
            workflowId: `retention-unrouted-${Math.random().toString(36).slice(2, 8)}`,
            args,
          })
          .then(() => undefined)
          .catch((e: unknown) => e);
        out.push(err);
      }
      return out;
    });

    // It FAILED, by TYPE, every time — and by the refusal's own type, not a `TypeError` out of
    // `Object.entries(null)`, which is what an untolerated `args: [null]` would give.
    for (const failure of failures) {
      expect(failure).toBeDefined();
      const cause = (failure as { cause?: ApplicationFailure }).cause;
      expect(cause?.type).toBe(NO_DATASET_QUEUE);
      // Non-retryable: "nobody said where" does not become true on the third attempt, and a retry
      // would hold the Schedule's `SKIP` window open against a firing that can never succeed.
      expect(cause?.nonRetryable).toBe(true);
      // It names the field to state, so the fix is in the error rather than in this file.
      expect(cause?.message).toContain('datasetQueue');
    }

    // AND NOTHING SWEPT. Not this plane's lake, and above all not the shared one.
    expect(seenA).toHaveLength(0);
    expect(seenShared).toHaveLength(0);
  }, 120_000);

  it('carries no kontra environment variable into the workflow bundle at all', () => {
    // THE STRUCTURAL HALF, and the reason the obvious one-line fix was not made. The workflow
    // sandbox is a `vm` context created EMPTY — `process` is not a determinism hazard in there, it
    // is UNDEFINED — so `taskQueue: datasetQueue()` would throw `ReferenceError: process is not
    // defined` while the bundle is imported, failing the workflow task for every type in it,
    // forever, with nothing surfacing to the caller.
    //
    // This module therefore imports nothing from `queues.ts`: the routing module is not in the
    // compiled bundle at all, and no kontra override is read anywhere in it. A future edit that
    // reaches for one fails here, in milliseconds, instead of hanging a controller.
    //
    // NEITHER ASSERTION IS VACUOUS — a one-line workflow that does `import { DATASET_QUEUE } from
    // './queues'` puts both strings in its bundle, which is how they were chosen.
    expect(bundle.code).not.toContain('src/queues.ts');
    expect(bundle.code).not.toContain('process.env.KONTRA');
  });
});

/**
 * ARMING IT, AGAINST A REAL TEMPORAL — the half no fake can prove.
 *
 * `createRetentionSchedule` swallows "already exists" so that boot is idempotent, and until now the
 * only evidence that it recognises the error was a unit test throwing an error this repo INVENTED.
 * The server's real ALREADY_EXISTS, through the real `ScheduleClient`, is a different object. This
 * also proves `client.schedule` structurally satisfies `ScheduleCreator` at RUNTIME, which is what
 * `infra.ts` hands it.
 *
 * The schedule targets a queue nothing polls, so nothing is ever swept here; it is deleted at the end
 * regardless, and the environment is ephemeral.
 */
describe('armRetentionSchedule against a real namespace', () => {
  const taskQueue = 'retention-arm-test-nobody-polls-this';
  const datasetQueue = 'retention-arm-test-nobody-polls-this-either';

  it('is idempotent — a second boot leaves the first Schedule alone', async () => {
    const lines: string[] = [];
    const first = await armRetentionSchedule(env.client.schedule, {
      taskQueue,
      datasetQueue,
      log: (l) => lines.push(l),
    });
    try {
      expect(first).toBe(RETENTION_SCHEDULE_ID);

      // What a boot actually registered: the sweep's queue PINNED, and no pinned mode — so every
      // firing routes where this control plane says and resolves the deployment's own posture, and
      // an unconfigured deployment previews.
      const desc = await env.client.schedule.getHandle(RETENTION_SCHEDULE_ID).describe();
      expect(desc.action.type).toBe('startWorkflow');
      if (desc.action.type !== 'startWorkflow') throw new Error('unreachable');
      expect(desc.action.workflowType).toBe('sweepDatasetsWorkflow');
      // Through the real payload converter and back — the queue survives the wire, the mode is absent.
      expect(desc.action.args).toEqual([{ datasetQueue }]);

      // The boot log names the lake a firing would reach, because the queue IS the blast radius.
      expect(lines.join('\n')).toContain(datasetQueue);

      // The server DOES reject a duplicate — otherwise "idempotent" would be proving nothing, and
      // this is the error object `isAlreadyExists` has to recognise.
      const dup = await env.client.schedule
        .create(retentionScheduleOptions({ taskQueue, datasetQueue }))
        .then(() => undefined)
        .catch((e: unknown) => e);
      expect(dup).toBeInstanceOf(Error);
      expect(`${(dup as Error).name} ${(dup as Error).message}`.toLowerCase()).toMatch(/already (exists|running)/);

      // The second boot swallows exactly that.
      const second = await armRetentionSchedule(env.client.schedule, {
        taskQueue,
        datasetQueue,
        log: (l) => lines.push(l),
      });
      expect(second).toBe(RETENTION_SCHEDULE_ID);
      expect(lines.some((l) => l.includes('could not arm'))).toBe(false);

      // And still exactly one schedule, with the id it was created under.
      const ids: string[] = [];
      for await (const sched of env.client.schedule.list()) ids.push(sched.scheduleId);
      expect(ids.filter((id) => id === RETENTION_SCHEDULE_ID)).toHaveLength(1);
    } finally {
      await env.client.schedule.getHandle(RETENTION_SCHEDULE_ID).delete().catch(() => undefined);
    }
  }, 60_000);
});

/**
 * THE NULLISH MODE, PINNED AT THE BOUNDARY IT ENTERS THROUGH.
 *
 * `dryRun?: boolean` is a promise the wire does not keep. The payload converter DROPS an `undefined`
 * key — `{ dryRun: undefined }` is received as `{}` — but a `null` survives it intact, so a firing can
 * hand this workflow a mode that is neither `true`, nor `false`, nor absent. The dead default this
 * replaced (`{ dryRun: input.dryRun ?? false, ...input }`) forwarded exactly that, and its safety was
 * a second `??` two files away in the activity (TRIAGE-2026-08-25 §4).
 *
 * So this asserts the normalisation HERE, not there: a nullish key leaves the workflow ABSENT, which
 * is what lets the deployment's own posture answer — and that answer is a dry run. It does NOT assert
 * a coercion to `false`, because `dryRun: false` means COLLECT: resolving an unstated mode to a
 * boolean in the workflow would be inventing a deletion order, the one thing ADR 0029 §5 keeps out of
 * this file.
 *
 * It runs through a REAL server and a REAL payload converter, because the converter is the entire
 * subject — a unit call on the exported function would prove nothing about what a wire `null` does.
 */
describe('a nullish dryRun is made absent at the workflow boundary', () => {
  it('drops nullish keys, passes a stated mode through, and strips the routing field', async () => {
    const seen: Array<Record<string, unknown>> = [];
    const sweepWorker = await recordingSweepWorker(DATASET_QUEUE, seen);

    const wfQueue = 'retention-nullish-test';
    const wfWorker = await workflowWorker(wfQueue);

    /** Start the workflow with `args` and report what the sweep activity was HANDED. */
    const handed = async (args: unknown[]): Promise<Record<string, unknown>> => {
      seen.length = 0;
      await env.client.workflow.execute('sweepDatasetsWorkflow', {
        taskQueue: wfQueue,
        workflowId: `retention-nullish-${Math.random().toString(36).slice(2, 8)}`,
        args,
      });
      expect(seen).toHaveLength(1);
      return seen[0]!;
    };

    /** The same input, plus the routing the workflow now insists on. The queue is stated in every
     *  case and expected in none of them — it is WHERE, not WHAT. */
    const routed = (input: Record<string, unknown>) => [{ datasetQueue: DATASET_QUEUE, ...input }];

    await withWorkers([wfWorker, sweepWorker], async () => {
      // THE FINDING. A `null` reaches the workflow — and does not leave it. The activity is handed
      // no `dryRun` at all, so `input.dryRun ?? !retentionCollects()` reads the deployment, which
      // is the one default there is.
      const nulled = await handed(routed({ dryRun: null }));
      expect(nulled).toEqual({});
      expect('dryRun' in nulled).toBe(false);
      // …and no `datasetQueue` either: the activity's input is unchanged by this field's existence.
      expect('datasetQueue' in nulled).toBe(false);

      // The wire fact the type system relies on, and the reason this was latent rather than live:
      // an `undefined` key never arrives, so a real start already landed on `{}`.
      expect(await handed(routed({ dryRun: undefined }))).toEqual({});
      expect(await handed(routed({}))).toEqual({});

      // A STATED mode still wins in both directions — unchanged, and the acceptance bar for this
      // change: nothing that can arrive over the wire behaves differently.
      expect(await handed(routed({ dryRun: false }))).toEqual({ dryRun: false });
      expect(await handed(routed({ dryRun: true }))).toEqual({ dryRun: true });

      // Every other field takes the same treatment, and a stated one is untouched. `ttlMs: null`
      // was harmless only because `data/retention.ts` happens to use `??` on it too — the same
      // rescue-from-two-files-away this change stops relying on.
      expect(await handed(routed({ dryRun: null, ttlMs: null, graceMs: null, now: null }))).toEqual({});
      expect(await handed(routed({ ttlMs: 5, dryRun: null }))).toEqual({ ttlMs: 5 });
    });
  }, 120_000);
});
