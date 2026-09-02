import * as path from 'node:path';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { TestWorkflowEnvironment } from '@temporalio/testing';
import { ApplicationFailure } from '@temporalio/common';
import { bundleWorkflowCode, Worker, type WorkflowBundle } from '@temporalio/worker';
import type { StackOpResult } from '../activities/infra';
import type { StackWorkflowInput } from './stack';
// The bundle `infra.ts` resolves, not this file — the same reason `tmuxSession.test.ts` uses it.
import * as bundleModule from './infra';
import { ResolutionLog } from '../secrets/audit';
import { FileSecretBackend } from '../secrets/fileBackend';
import { SecretStore } from '../secrets/store';
import { checkCredential, credentialFrom, resolveProviderEnv } from '../infra/credential';

/**
 * The stack workflow's SAGA LEG, against a real Temporal with faked Pulumi.
 *
 * WHAT THIS PINS, and why it is worth a workflow test rather than a unit one. A `fleet.up` that
 * FAILS partway leaves Droplets running: the caller's `async with` never opened, so its scope exit
 * never ran, and the only thing left that could tear them down is this workflow. The compensation
 * used to be guarded by `wf.isCancellation(err)` — the case somebody thought of — so the ordinary
 * failure path (a Pulumi error after three of four Droplets exist, a non-zero install script, a
 * converge that outran its timeout) rethrew with the machines alive and nobody tracking them.
 *
 * Cancellation and failure are different code paths in the Temporal SDK, so proving one says
 * nothing about the other. Both are exercised here.
 */

let queueSeq = 0;
const nextQueue = (): string => `stack-test-${(queueSeq += 1)}`;

/**
 * `retry: { maximumAttempts: 3 }` on the workflow's proxied activities — so a FAILING activity is
 * called three times, not once. Counting raw calls without accounting for that reads a retry as a
 * second operation, which is how "did the compensation run twice?" gets answered wrongly.
 */
const ATTEMPTS = 3;

/** Temporal wraps a workflow failure several layers deep (`WorkflowFailedError` → `ApplicationFailure`
 *  → …), and only the innermost layer carries what the activity actually said. */
function causeChain(err: unknown): string {
  const parts: string[] = [];
  let cur: unknown = err;
  for (let i = 0; i < 8 && cur; i += 1) {
    parts.push(String((cur as Error).message ?? cur));
    cur = (cur as { cause?: unknown }).cause;
  }
  return parts.join(' | ');
}

interface Recorded {
  ups: string[];
  destroys: string[];
  previews: string[];
  /** Every credential preflight, in order — see `refuses before anything is built`. */
  checks: string[];
}

/**
 * WAIT ON THE CONDITION, NEVER ON THE CLOCK.
 *
 * The cancellation test used to `setTimeout(600)` and hope the activity had started. That is a
 * race rather than a synchronisation: nothing about Temporal promises to dispatch an activity
 * inside 600 ms, so the test's verdict was a function of machine load. MEASURED 2026-08-28: pass
 * at 58 s on an idle box, FAIL at 171 s on the same tree under load, with `rec.ups` empty. A red
 * that means "the box was busy" teaches everyone to re-run rather than read, which is the exact
 * reflex that lets a real red through — and this repo has already paid for green suites hiding
 * real bugs.
 *
 * So the ceiling here is not a delay, it is a giving-up point: a slow machine is slow, not wrong.
 * (Temporal's time-skipping environment would remove the wall clock entirely, but the thing being
 * waited for is a real ACTIVITY on a real worker, which time-skipping does not accelerate — the
 * skip applies to workflow timers.)
 */
async function waitFor(what: string, ready: () => boolean, ceilingMs = 30_000): Promise<void> {
  const deadline = Date.now() + ceilingMs;
  while (!ready()) {
    if (Date.now() >= deadline) {
      throw new Error(`waited ${ceilingMs} ms for ${what} and it never happened`);
    }
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}

let env: TestWorkflowEnvironment;
let bundle: WorkflowBundle;

beforeAll(async () => {
  env = await TestWorkflowEnvironment.createLocal();
  bundle = await bundleWorkflowCode({ workflowsPath: path.join(__dirname, 'infra.ts') });
}, 300_000);

afterAll(async () => {
  await env?.teardown();
});

const OK: StackOpResult = { result: 'succeeded', changes: {} } as StackOpResult;

function activities(
  rec: Recorded,
  over: {
    upFails?: string;
    /** Holds `stackUp` open until the test resolves it — see `upHeldOpen` below. */
    upHeldOpen?: Promise<void>;
    destroyFails?: string;
    /** A credential that cannot be resolved. Non-retryable, the way the real activity throws it. */
    credentialFails?: string;
  } = {}
) {
  return {
    /**
     * The credential preflight (ADR 0034 §4). The real one reads METADATA — never a value — so a
     * missing or revoked secret fails at the start of `fleet.up()` naming it.
     *
     * Faked here for the same reason Pulumi is: what this file pins is the WORKFLOW's shape, and
     * the shape that matters is that this runs before the saga leg is armed.
     */
    async checkCloudCredential(call: {
      stackFqn: string;
      run?: string;
    }): Promise<{ credential: string; version: number }> {
      rec.checks.push(`${call.stackFqn}|${call.run ?? ''}`);
      if (over.credentialFails) {
        throw ApplicationFailure.nonRetryable(over.credentialFails, 'CloudCredentialUnavailable');
      }
      return { credential: 'do-token', version: 1 };
    },
    async stackUp(call: { stackFqn: string }): Promise<StackOpResult> {
      rec.ups.push(call.stackFqn);
      if (over.upHeldOpen) {
        // A converge in flight, so the test can cancel it mid-`up` — which is what an operator
        // pressing stop during a four-minute provision actually does.
        //
        // THE TEST DECIDES WHEN THIS RETURNS, and that is the point: a timer here would be a
        // second clock to be wrong about. `worker.runUntil` will not resolve while an activity is
        // still executing and this fake does not heartbeat, so nothing can tell it to stop — the
        // test holds it open exactly as long as it needs the converge to be in flight, then lets
        // go. A sleep long enough on an idle box is not long enough on a busy one.
        await over.upHeldOpen;
      }
      if (over.upFails) throw new Error(over.upFails);
      return OK;
    },
    async stackDestroy(call: { stackFqn: string }): Promise<StackOpResult> {
      rec.destroys.push(call.stackFqn);
      if (over.destroyFails) throw new Error(over.destroyFails);
      return OK;
    },
    async stackPreview(call: { stackFqn: string }): Promise<StackOpResult> {
      rec.previews.push(call.stackFqn);
      return OK;
    },
  };
}

const FQN = 'kontra-fleet/nscheck-0.1.0';

/** Start the workflow and return its handle plus the worker's `runUntil`, so a test can cancel a
 *  run that is still going. */
async function run(
  rec: Recorded,
  input: Partial<StackWorkflowInput> = {},
  over: Parameters<typeof activities>[1] = {},
  during?: (handle: { cancel(): Promise<void> }) => Promise<void>
): Promise<{ result?: StackOpResult; error?: Error }> {
  const queue = nextQueue();
  const worker = await Worker.create({
    connection: env.nativeConnection,
    taskQueue: queue,
    workflowBundle: bundle,
    activities: activities(rec, over),
  });
  return worker.runUntil(async () => {
    const handle = await env.client.workflow.start('stackWorkflow', {
      taskQueue: queue,
      workflowId: `${FQN}-${Math.random().toString(36).slice(2, 8)}`,
      args: [{ stackFqn: FQN, op: 'up', compensateOnCancel: true, ...input }],
    });
    if (during) await during(handle);
    try {
      return { result: (await handle.result()) as StackOpResult };
    } catch (err) {
      return { error: err as Error };
    }
  });
}

describe('a provision that does not finish tears down what it built', () => {
  it('destroys the stack when `up` FAILS', async () => {
    // THE BUG. Three of four Droplets exist and Pulumi gives up; without this the workflow
    // rethrows and the machines bill until somebody notices.
    const rec: Recorded = { ups: [], destroys: [], previews: [], checks: [] };
    const { error } = await run(rec, {}, { upFails: 'creating kf-dns-03: droplet limit reached' });

    expect(error).toBeDefined();
    expect(rec.destroys).toEqual([FQN]);
    // The provision's own error is what reaches the caller — a teardown must not rename the cause.
    expect(causeChain(error)).toContain('droplet limit reached');
  });

  it('destroys the stack when the provision is CANCELLED', async () => {
    // The case that always worked, kept so widening the guard cannot quietly drop it.
    const rec: Recorded = { ups: [], destroys: [], previews: [], checks: [] };

    // The converge runs until this test releases it. Every step below is gated on something that
    // OBSERVABLY happened rather than on an elapsed millisecond count, so the whole scenario —
    // "the cancel arrives while `up` is genuinely in flight" — is a fact the test establishes
    // instead of a window it bets on.
    let release!: () => void;
    const upHeldOpen = new Promise<void>((resolve) => {
      release = resolve;
    });

    const { error } = await run(rec, {}, { upHeldOpen }, async (handle) => {
      try {
        // 1. The activity has actually started, or there is nothing to compensate for. `rec.ups`
        //    is the activity's own observable effect — the same array the assertion reads.
        await waitFor('the converge to start', () => rec.ups.length > 0);
        // 2. The operator presses stop.
        await handle.cancel();
        // 3. The compensation fires. TRY_CANCEL means the WORKFLOW is told immediately and does
        //    not wait for the activity, so the teardown is scheduled while `up` is still running —
        //    which is why this can be awaited before releasing the converge, and why doing so
        //    proves the cancel landed mid-flight rather than after a converge that had finished.
        await waitFor('the compensating teardown', () => rec.destroys.length > 0);
      } finally {
        // Unconditional: a failed wait above must still let the worker drain, or the assertion
        // that would have explained it never gets to run.
        release();
      }
    });

    expect(error).toBeDefined();
    expect(rec.ups).toEqual([FQN]);
    expect(rec.destroys).toEqual([FQN]);
    // Two 30 s ceilings plus starting a worker, and it has to hold on a box under load — which is
    // the whole point. The old 30 s was fine for a 600 ms sleep and would have turned a slow
    // machine into a red anyway.
  }, 120_000);

  it('does not tear down a provision that SUCCEEDED', async () => {
    const rec: Recorded = { ups: [], destroys: [], previews: [], checks: [] };
    const { result } = await run(rec);
    expect(result?.result).toBe('succeeded');
    expect(rec.destroys).toEqual([]);
  });

  it('leaves a failed `destroy` alone rather than destroying twice', async () => {
    // `op: 'destroy'` failing is not an incomplete provision. Compensating it would mean running
    // the failing operation a second time, on a schedule nobody asked for.
    const rec: Recorded = { ups: [], destroys: [], previews: [], checks: [] };
    const { error } = await run(
      rec,
      { op: 'destroy' },
      { destroyFails: 'stack is locked by another update' }
    );
    expect(error).toBeDefined();
    // The operation's own retries and nothing more. Compensating would run it a second time on a
    // schedule nobody asked for, which would show up here as six.
    expect(rec.destroys).toHaveLength(ATTEMPTS);
  });

  it('does not tear down when the caller did not ask for compensation', async () => {
    // `kontra fleet up` from the CLI adopts a fleet deliberately and owns its lifetime; only a
    // caller that opened a SCOPE asks for the saga leg.
    const rec: Recorded = { ups: [], destroys: [], previews: [], checks: [] };
    const { error } = await run(rec, { compensateOnCancel: false }, { upFails: 'boom' });
    expect(error).toBeDefined();
    expect(rec.destroys).toEqual([]);
  });

  it('reports the provision error even when the teardown also fails', async () => {
    // Two things went wrong and only one of them is the cause. Reporting the cleanup failure
    // would send an operator to look at a destroy problem when their provision is what broke.
    const rec: Recorded = { ups: [], destroys: [], previews: [], checks: [] };
    const { error } = await run(
      rec,
      {},
      { upFails: 'cloud-init exited 1', destroyFails: 'DigitalOcean API 503' }
    );
    // The compensation's own retry budget — it tried, three times, and could not.
    expect(rec.destroys).toHaveLength(ATTEMPTS);
    expect(causeChain(error)).toContain('cloud-init exited 1');
    expect(causeChain(error)).not.toContain('503');
  });
});

describe('the bundle', () => {
  it('still exports the stack workflow from the path infra.ts resolves', () => {
    // A bundle that dropped `stackWorkflow` would break every `fleet` verb, and the failure would
    // be a start that hangs rather than an import error.
    expect(typeof (bundleModule as Record<string, unknown>).stackWorkflow).toBe('function');
  });
});

/**
 * THE CREDENTIAL FAILS AT THE START, AND IT FAILS BEFORE THE SAGA LEG IS ARMED (ADR 0034 §4).
 *
 * The failure this replaces is the one this repo has already paid for: a workflow retrying an
 * activity that cannot succeed, which looks like a hung run rather than a missing secret. What
 * matters at the WORKFLOW level is the ordering — a credential nobody can read must not fire a
 * compensating `destroy`, because that destroy needs the same credential, fails on it, and buries
 * the sentence that says what to fix under a cleanup error.
 */
describe('a credential that cannot be resolved', () => {
  it('refuses before anything is built, and compensates nothing', async () => {
    const rec: Recorded = { ups: [], destroys: [], previews: [], checks: [] };
    const { error } = await run(
      rec,
      { compensateOnCancel: true },
      { credentialFails: 'this fleet needs the cloud credential "do-prod" and there is no secret by that name' }
    );

    expect(causeChain(error)).toContain('do-prod');
    // NOTHING RAN. Not the converge, and — the part that matters — not the teardown.
    expect(rec.ups).toEqual([]);
    expect(rec.destroys).toEqual([]);
    // ONE attempt. A secret that does not exist does not start existing on the second try, and
    // three identical refusals only delay the answer an operator has to act on.
    expect(rec.checks).toHaveLength(1);
  });

  it('checks the credential on a DESTROY too', async () => {
    // A teardown makes provider calls exactly as a provision does. `kontra fleet down` against a
    // credential nobody can read must say so in a second — the alternative is a converge that
    // retries for an hour while the Droplets it cannot delete keep billing.
    const rec: Recorded = { ups: [], destroys: [], previews: [], checks: [] };
    const { error } = await run(rec, { op: 'destroy' }, { credentialFails: 'every version of "do-prod" has been revoked' });
    expect(causeChain(error)).toContain('revoked');
    expect(rec.destroys).toEqual([]);
  });

  it('runs the check before the converge on the happy path, and names the Run it serves', async () => {
    const rec: Recorded = { ups: [], destroys: [], previews: [], checks: [] };
    const { result } = await run(rec, {});
    expect(result?.result).toBe('succeeded');
    expect(rec.checks).toHaveLength(1);
    expect(rec.ups).toEqual([FQN]);
    // The Run is the CALLER's workflow id, and this workflow was started with no parent — so it is
    // empty rather than invented. A `kontra fleet` command is not a Run.
    expect(rec.checks[0]).toBe(`${FQN}|`);
  });
});

/**
 * NEGATIVES 1-3 OF ADR 0034 §4, IN THIS SLICE'S OWN SHAPE: the credential's value is never a
 * workflow argument, never an activity argument, never part of a Batch.
 *
 * `secrets/history.test.ts` is the general regression guard and this is the fleet's instance of it,
 * written the same way rather than a second way: run the REAL `stackWorkflow` against a real
 * Temporal with a real secret store behind the activities, then read the history back off the
 * server and sweep every byte of the `History` proto for a sentinel.
 *
 * WHAT MAKES IT NOT VACUOUS is that the credential is genuinely resolved during the run — the fake
 * `stackUp` calls the same `resolveProviderEnv` the real one does, against the same store, and the
 * test asserts the resolved value reached it. The value existed, inside one activity, and left no
 * trace above it. The NAME is asserted present in the same haystack, which is the design in a line.
 */
describe('the fleet workflow leaves no credential in its history', () => {
  const SENTINEL = 'dop_v1_SENTINEL_never_in_fleet_history_c17b';

  /** Walk to the leaves, decoding byte buffers: a payload body is a `Uint8Array` here, and a
   *  substring search over its JSON form searches a list of integers. */
  function flatten(node: unknown, out: string[] = []): string {
    if (node === null || node === undefined) return out.join('\n');
    if (typeof node === 'string') out.push(node);
    else if (node instanceof Uint8Array) out.push(Buffer.from(node).toString('utf8'));
    else if (Array.isArray(node)) for (const item of node) flatten(item, out);
    else if (typeof node === 'object') for (const v of Object.values(node)) flatten(v, out);
    else out.push(String(node));
    return out.join('\n');
  }

  it('resolves it inside the activity, and the run records only the name', async () => {
    const dir = mkdtempSync(path.join(tmpdir(), 'kontra-fleet-history-'));
    const store = new SecretStore(new FileSecretBackend({ dir }), { keyDir: dir });
    await store.put('do-prod', SENTINEL);
    /** What the last hop actually handed the engine — the proof the credential was really read. */
    let engineSaw = '';

    const queue = nextQueue();
    const worker = await Worker.create({
      connection: env.nativeConnection,
      taskQueue: queue,
      workflowBundle: bundle,
      activities: {
        // The REAL preflight, against the REAL store: metadata only, so its result is safe here.
        async checkCloudCredential(call: { args?: Record<string, unknown> }) {
          return checkCredential(credentialFrom(call.args, {}), { store });
        },
        // The REAL last hop, minus Pulumi. What comes back is a summary, exactly as `stackUp`'s is.
        async stackUp(call: { stackFqn: string; args?: Record<string, unknown>; run?: string }) {
          const providerEnv = await resolveProviderEnv(
            {
              credential: credentialFrom(call.args, {}),
              providerEnvVar: 'DIGITALOCEAN_TOKEN',
              actor: 'nscheck',
              version: '0.1.0',
              run: call.run ?? '',
            },
            { store, log: new ResolutionLog({ dir }) }
          );
          engineSaw = providerEnv.DIGITALOCEAN_TOKEN ?? '';
          return { fqn: call.stackFqn, result: 'succeeded', changes: { create: 2 }, outputs: {} };
        },
        async stackDestroy() {
          return OK;
        },
        async stackPreview() {
          return OK;
        },
      },
    });

    const workflowId = `${FQN}-history-${Math.random().toString(36).slice(2, 8)}`;
    const handle = await worker.runUntil(async () => {
      const h = await env.client.workflow.start('stackWorkflow', {
        taskQueue: queue,
        workflowId,
        args: [
          {
            stackFqn: FQN,
            op: 'up',
            args: { tag: 'dns', machines: 2, credential: 'do-prod' },
            compensateOnCancel: true,
          },
        ],
      });
      await h.result();
      return h;
    });

    // The credential really did reach the call it was for. Without this the sweep below would pass
    // by resolving nothing at all, which is the vacuous version of every assertion after it.
    expect(engineSaw).toBe(SENTINEL);

    const haystack = flatten(await handle.fetchHistory());
    // THE ASSERTION.
    expect(haystack).not.toContain(SENTINEL);
    expect(haystack).not.toContain(SENTINEL.slice(0, 20));
    // …in a history that demonstrably holds this run, and that carries the NAME the value did not.
    expect(haystack).toContain('do-prod');
    expect(haystack).toContain('stackWorkflow');
    expect(haystack).toContain(FQN);
    // The preflight's RESULT is in there too — a name and a version number, and nothing else.
    expect(haystack).toContain('"version":1');
  }, 120_000);
});
