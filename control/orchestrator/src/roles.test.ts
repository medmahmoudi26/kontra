/**
 * THE MERGE'S TWO PROMISES, pinned (ADR 0031 §1, §4).
 *
 * One: the processes merge and the QUEUES DO NOT. A queue is how work is routed; a process is only
 * where it runs — so a boot that hands two roles one queue must refuse, not start. It is worth a
 * suite because the failure it prevents is silent: two pollers on one queue each win some tasks,
 * the one that cannot run them fails them forever, and every surface reports a healthy process.
 *
 * Two: an appliance with no provisioner REGISTERS `stackWorkflow` and refuses inside it. Leaving
 * the type out is the tempting shape and it is the wrong one — a workflow task for an unregistered
 * type is FAILED and retried forever, so `fleet.up()` would hang instead of failing, which is the
 * invisible-failure mode this product exists to remove.
 *
 * DELIBERATELY OUTSIDE `src/workflows/`, and cheap. That directory's three suites each boot a
 * Temporal test server AND bundle; on a two-core box a fourth of those is how the previous attempt
 * at this slice turned 21 passing tests into timeouts. NOTHING HERE BOOTS A SERVER — the refusal is
 * a function that throws and the queue guard is a string comparison. One test does run the
 * bundler, because an unbundleable import is the one failure nothing else can catch, and webpack
 * on its own is a few seconds and no cluster.
 */

import path from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApplicationFailure } from '@temporalio/common';

import { assertDistinctQueues, queueAssignments, resolveRoles, ROLES } from './roles';
import * as appliance from './workflows/appliance';
import * as compose from './workflows/infra';

afterEach(() => {
  vi.unstubAllEnvs();
});

describe('which roles this process serves', () => {
  it('defaults to all three — the appliance is one process and a whole control plane', () => {
    expect(resolveRoles(undefined)).toEqual(['api', 'materializer', 'infra']);
    expect(resolveRoles('')).toEqual([...ROLES]);
  });

  it('takes a named subset, which is how compose keeps its own provisioner', () => {
    // ADR 0034 §1: the compose controller keeps `orchestrator-infra`, so the merged process there
    // must NOT poll the infra queue — two pollers and one of them refuses every `fleet.up()` it
    // happens to win.
    expect(resolveRoles('api,materializer')).toEqual(['api', 'materializer']);
  });

  it('is order- and duplicate-insensitive, so the boot log reads the same however it was typed', () => {
    expect(resolveRoles(' infra , api ,api ')).toEqual(['api', 'infra']);
  });

  it('REFUSES an unknown role rather than quietly serving fewer', () => {
    // The whole class of "the process came up and silently does less than you asked" is what this
    // slice must not create. A typo is the likeliest way in.
    expect(() => resolveRoles('api,materialiser')).toThrow(/no such role "materialiser"/);
    expect(() => resolveRoles('api,materialiser')).toThrow(/api, materializer, infra/);
  });
});

describe('the queues do not merge when the processes do', () => {
  it('lists the queues each role will poll, and no queue for the API', () => {
    // The API is a Temporal CLIENT: it starts workflows onto queues other roles serve, and polls
    // nothing. A row for it would be a queue nobody is on.
    expect(queueAssignments(['api'])).toEqual([]);
    // TWO QUEUES, NOT THREE. `kontra-materializer` and its three activities were removed as
    // uncalled on 2026-09-26 (`queues.ts` carries the measurement); the materializer role now
    // serves `kontra-datasets` alone, which carries the typed-output write as well as the reads.
    expect(queueAssignments([...ROLES]).map((q) => q.queue)).toEqual([
      'kontra-datasets',
      'kontra-infra',
    ]);
  });

  it('resolves them the way the workers do, through the environment', () => {
    vi.stubEnv('KONTRA_DATASET_QUEUE', 'other-datasets');
    vi.stubEnv('KONTRA_INFRA_QUEUE', 'other-infra');
    expect(queueAssignments([...ROLES]).map((q) => q.queue)).toEqual([
      'other-datasets',
      'other-infra',
    ]);
  });

  it('boots the default topology', () => {
    expect(() => assertDistinctQueues([...ROLES])).not.toThrow();
  });

  it('REFUSES TO BOOT when two roles were handed one queue, naming both variables', () => {
    // The collision that remains after the materializer queue went: dataset vs infra. It is the
    // dangerous one — the infra worker is deliberately serialised to one Pulumi update at a time,
    // so a dataset read that landed there would queue behind a sixty-minute converge.
    vi.stubEnv('KONTRA_INFRA_QUEUE', 'kontra-datasets');
    let err: unknown;
    try {
      assertDistinctQueues([...ROLES]);
    } catch (e) {
      err = e;
    }
    const msg = err instanceof Error ? err.message : String(err);
    // The only actionable half of a collision message is which two variables to edit.
    expect(msg).toMatch(/KONTRA_DATASET_QUEUE/);
    expect(msg).toMatch(/KONTRA_INFRA_QUEUE/);
    expect(msg).toMatch(/kontra-datasets/);
  });

  it('does not fire when the roles that would collide are not both served', () => {
    // Same misconfiguration, one role short: this process polls one of the two names, so nothing
    // is stolen and there is nothing to refuse. A guard that failed here would stop a deployment
    // that works.
    vi.stubEnv('KONTRA_INFRA_QUEUE', 'kontra-datasets');
    expect(() => assertDistinctQueues(['api', 'infra'])).not.toThrow();
  });

  it('refuses to take the actor probe’s queue, which is a different process entirely', () => {
    // ADR 0033: `kontra-probe` serves exactly one workflow type, from a Python worker. A role
    // pointed at it would take `ActorProbe` tasks it cannot run and fail them forever, while the
    // Actors page reports nothing.
    vi.stubEnv('KONTRA_INFRA_QUEUE', 'kontra-probe');
    expect(() => assertDistinctQueues([...ROLES])).toThrow(/actor probe/);
  });
});

describe('the appliance bundle: the same two types, one of which refuses', () => {
  it('registers both workflow types, exactly as the compose bundle does', () => {
    // If it ever exported fewer, the missing type would not fail its caller — it would hang it.
    for (const type of ['stackWorkflow', 'sweepDatasetsWorkflow'] as const) {
      expect(typeof (appliance as unknown as Record<string, unknown>)[type]).toBe('function');
      expect(typeof (compose as unknown as Record<string, unknown>)[type]).toBe('function');
    }
  });

  it('is NOT the provisioner — the two bundles export different implementations', () => {
    expect(appliance.stackWorkflow).not.toBe(compose.stackWorkflow);
    // …and the sweep IS the same one, because nothing about it changed.
    expect(appliance.sweepDatasetsWorkflow).toBe(compose.sweepDatasetsWorkflow);
  });

  it('refuses immediately, non-retryably, naming the limitation and what to do instead', async () => {
    const err = await appliance
      .stackWorkflow({ stackFqn: 'kontra/fleet/nmap-1', op: 'up' })
      .then(() => undefined)
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ApplicationFailure);
    const failure = err as ApplicationFailure;
    // Non-retryable: "no provisioner" will not become true on the third attempt.
    expect(failure.nonRetryable).toBe(true);
    expect(failure.type).toBe(appliance.NO_PROVISIONER);
    // Names the operation, the limitation, and the deployment that has one.
    expect(failure.message).toMatch(/fleet up kontra\/fleet\/nmap-1/);
    expect(failure.message).toMatch(/no provisioner/);
    expect(failure.message).toMatch(/compose controller/);
  });

  it('refuses a teardown too, because it never built the fleet it is being asked to destroy', async () => {
    const err = await appliance
      .stackWorkflow({ stackFqn: 'kontra/fleet/nmap-1', op: 'destroy' })
      .catch((e: unknown) => e);
    expect((err as ApplicationFailure).message).toMatch(/fleet destroy/);
  });

  it('BUNDLES, and carries no line of the provisioner into the sandbox', async () => {
    // THE ONE FAILURE THIS SUITE CANNOT CATCH ANY OTHER WAY. Workflow code is bundled for a
    // sandbox with no filesystem, no client and no Node built-in, and an import that cannot be
    // bundled does not error — it makes bundling HANG, which surfaces as an appliance that starts
    // and never polls. So the guard has to actually run the bundler.
    //
    // NO TEMPORAL SERVER, deliberately: `bundleWorkflowCode` is webpack and nothing else. The
    // three suites in `src/workflows/` each boot one, and a fourth on a two-core box is how this
    // slice's previous attempt turned 21 passing tests into timeouts.
    const { bundleWorkflowCode } = await import('@temporalio/worker');
    const bundle = await bundleWorkflowCode({
      // The SOURCE path, the way the three suites in `src/workflows/` spell it — `require.resolve`
      // is what the worker uses at runtime against `dist/`, and it does not resolve a `.ts` here.
      workflowsPath: path.join(__dirname, 'workflows', 'appliance.ts'),
    });
    for (const type of ['stackWorkflow', 'sweepDatasetsWorkflow']) {
      expect(bundle.code).toContain(type);
    }
    // `./stack` is imported for its TYPES only, so the preflight that reads a cloud credential is
    // not in here. If this ever fails, the appliance bundle grew a provisioner.
    expect(bundle.code).not.toContain('checkCloudCredential');
  }, 60_000);
});
