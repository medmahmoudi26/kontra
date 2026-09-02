/**
 * The retention Schedule's shape (ADR 0029 §5). The overlap policy, the awaited-workflow target and
 * the two QUEUES are what make the schedule sound, so they are pinned here — no live cluster needed,
 * because the options object is what `client.schedule.create` receives.
 *
 * THE QUEUES ARE RESOLVED IN THIS MODULE AND NOWHERE ELSE, which is what `KONTRA_DATASET_QUEUE`
 * being able to route a sweep now rests on: the workflow's sandbox has no `process` in it, so an
 * override read anywhere but out here reaches nothing. `workflows/retention.test.ts` proves the other
 * end — two control planes, two lakes, neither reaching the other's.
 */

import { afterEach, describe, expect, it, vi } from 'vitest';
import { ScheduleOverlapPolicy, type ScheduleOptions } from '@temporalio/client';

import {
  DEFAULT_RETENTION_INTERVAL,
  RETENTION_SCHEDULE_ID,
  armRetentionSchedule,
  createRetentionSchedule,
  retentionScheduleOptions,
} from './retention';
import { DATASET_QUEUE, INFRA_QUEUE } from './queues';

/** The suite reads the environment through `queues.ts`, so it must not inherit one. Every test that
 *  cares states what it wants; the rest run against the repo defaults. */
afterEach(() => {
  vi.unstubAllEnvs();
});

describe('retentionScheduleOptions', () => {
  it('uses SKIP overlap — the guard the awaited workflow makes meaningful', () => {
    expect(retentionScheduleOptions().policies?.overlap).toBe(ScheduleOverlapPolicy.SKIP);
  });

  /**
   * THE SAFETY, at the one place it is decidable without a cluster: the arguments a boot registers.
   *
   * NO MODE IN THE INPUT, not `{ dryRun: false }`. The mode a firing runs in is resolved by the worker
   * holding the lake (`KONTRA_RETENTION_COLLECT`, `activities/retention.ts`), which defaults to a dry
   * run — so the first deployment of this Schedule previews and deletes nothing, and turning
   * collection on is a configuration change on the process that would do the deleting rather than a
   * Schedule that must be destroyed to change its mind. Pinning `false` here would have frozen the
   * mode at whatever the FIRST boot ever registered, because creation is idempotent.
   */
  it('starts sweepDatasetsWorkflow with NO pinned mode — the deployment decides, and defaults to dry', () => {
    vi.stubEnv('KONTRA_DATASET_QUEUE', '');
    const opts = retentionScheduleOptions();
    expect(opts.action.type).toBe('startWorkflow');
    if (opts.action.type !== 'startWorkflow') throw new Error('unreachable');
    expect(opts.action.workflowType).toBe('sweepDatasetsWorkflow');
    // The QUEUE is pinned and the MODE is not — the two halves of the safety, each settled by the
    // only process that can settle it. See `RetentionScheduleOptions.datasetQueue`.
    expect(opts.action.args).toEqual([{ datasetQueue: DATASET_QUEUE }]);
  });

  /**
   * THE ROUTING, at the one place it is decidable: the arguments a boot registers.
   *
   * `workflows/retention.ts` pinned the `DATASET_QUEUE` CONSTANT until 2026-08-26, so
   * `KONTRA_DATASET_QUEUE` could not route a sweep and a second control plane's isolated stack still
   * swept the live lake. The override is read HERE now — outside the sandbox, where an environment
   * exists — and travels as workflow input.
   */
  it('pins the sweep’s queue from KONTRA_DATASET_QUEUE, which is what a second control plane isolates with', () => {
    vi.stubEnv('KONTRA_DATASET_QUEUE', 'plane-b-datasets');
    const opts = retentionScheduleOptions();
    if (opts.action.type !== 'startWorkflow') throw new Error('unreachable');
    expect(opts.action.args).toEqual([{ datasetQueue: 'plane-b-datasets' }]);
  });

  it('takes an explicit datasetQueue over the environment, for a caller that states one', () => {
    vi.stubEnv('KONTRA_DATASET_QUEUE', 'plane-b-datasets');
    const opts = retentionScheduleOptions({ datasetQueue: 'stated-outright' });
    if (opts.action.type !== 'startWorkflow') throw new Error('unreachable');
    expect(opts.action.args).toEqual([{ datasetQueue: 'stated-outright' }]);
  });

  /** The WORKFLOW's queue comes from the same module, and is not re-derived here — the rule
   *  `roles.ts` states, applied to the file that used to keep its own copy of it. */
  it('hosts the workflow on the infra queue in effect', () => {
    vi.stubEnv('KONTRA_INFRA_QUEUE', '');
    expect(retentionScheduleOptions().action.taskQueue).toBe(INFRA_QUEUE);
    vi.stubEnv('KONTRA_INFRA_QUEUE', 'plane-b-infra');
    expect(retentionScheduleOptions().action.taskQueue).toBe('plane-b-infra');
  });

  it('fires on the default interval under a stable id', () => {
    const opts = retentionScheduleOptions();
    expect(opts.scheduleId).toBe(RETENTION_SCHEDULE_ID);
    expect(opts.spec?.intervals).toEqual([{ every: DEFAULT_RETENTION_INTERVAL }]);
  });

  it('can be pointed at a preview-only firing that deletes nothing', () => {
    const opts = retentionScheduleOptions({ dryRun: true, interval: '30m', datasetQueue: 'q' });
    if (opts.action.type !== 'startWorkflow') throw new Error('unreachable');
    expect(opts.action.args).toEqual([{ datasetQueue: 'q', dryRun: true }]);
    expect(opts.spec?.intervals).toEqual([{ every: '30m' }]);
  });

  it('can pin collection explicitly, for a deployment that states it in the Schedule', () => {
    const opts = retentionScheduleOptions({ dryRun: false, datasetQueue: 'q' });
    if (opts.action.type !== 'startWorkflow') throw new Error('unreachable');
    expect(opts.action.args).toEqual([{ datasetQueue: 'q', dryRun: false }]);
  });
});

describe('armRetentionSchedule — what boot calls', () => {
  it('arms the schedule and says so — naming the lake a firing would reach', async () => {
    const lines: string[] = [];
    vi.stubEnv('KONTRA_DATASET_QUEUE', 'plane-b-datasets');
    const create = vi.fn(async (o: ScheduleOptions) => ({ scheduleId: o.scheduleId }));
    const id = await armRetentionSchedule({ create }, { log: (l) => lines.push(l) });
    expect(id).toBe(RETENTION_SCHEDULE_ID);
    expect(lines.join('\n')).toContain('KONTRA_RETENTION_COLLECT');
    // The QUEUE IS THE BLAST RADIUS — a sweep deletes from exactly one worker's lake, and the
    // 2026-08-26 incident was a sweep whose reach nobody could see until after it ran. So the boot
    // line says which worker, every time.
    expect(lines.join('\n')).toContain('plane-b-datasets');
  });

  /** A restart must not duplicate it, reset its next firing, or undo a `kontra schedule pause`. */
  it('is a no-op on a second boot', async () => {
    const create = vi.fn(async () => {
      const err = new Error('schedule already exists');
      (err as { name: string }).name = 'ScheduleAlreadyRunning';
      throw err;
    });
    const schedules = { create };
    await expect(armRetentionSchedule(schedules)).resolves.toBe(RETENTION_SCHEDULE_ID);
    await expect(armRetentionSchedule(schedules)).resolves.toBe(RETENTION_SCHEDULE_ID);
    expect(create).toHaveBeenCalledTimes(2);
  });

  /**
   * The opposite rule from the sweep's. A sweep that cannot read its stores MUST abort — sweeping
   * blind deletes kept output. A control plane that cannot register an hourly housekeeping schedule
   * must still come up: the same process converges the Fleet and supervises the Dashboard streamer.
   */
  it('never throws — a boot is not failed over housekeeping, and the log says what did not happen', async () => {
    const lines: string[] = [];
    const create = vi.fn(async () => {
      throw new Error('namespace unreachable');
    });
    await expect(armRetentionSchedule({ create }, { log: (l) => lines.push(l) })).resolves.toBeUndefined();
    expect(lines.join('\n')).toContain('namespace unreachable');
    expect(lines.join('\n')).toContain('will not expire');
  });
});

describe('createRetentionSchedule', () => {
  it('creates the schedule and returns its id', async () => {
    const create = vi.fn(async (o: ScheduleOptions) => ({ scheduleId: o.scheduleId }));
    const id = await createRetentionSchedule({ create });
    expect(id).toBe(RETENTION_SCHEDULE_ID);
    expect(create).toHaveBeenCalledOnce();
    expect(create.mock.calls[0]![0].policies?.overlap).toBe(ScheduleOverlapPolicy.SKIP);
  });

  it('is idempotent — an already-existing schedule is success, not an error', async () => {
    const create = vi.fn(async () => {
      const err = new Error('Schedule with this ID is already registered');
      (err as { name: string }).name = 'ScheduleAlreadyRunning';
      throw err;
    });
    await expect(createRetentionSchedule({ create })).resolves.toBe(RETENTION_SCHEDULE_ID);
  });

  it('propagates a real failure rather than swallowing it', async () => {
    const create = vi.fn(async () => {
      throw new Error('namespace unreachable');
    });
    await expect(createRetentionSchedule({ create })).rejects.toThrow('namespace unreachable');
  });
});
