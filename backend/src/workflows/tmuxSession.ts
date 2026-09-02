/**
 * The session converge workflow — one per Machine, and its id IS the Machine (ADR 0020).
 *
 * `workflowId = tmux-<machine>` with `workflowIdConflictPolicy: 'FAIL'`, so there is exactly one
 * writer per Machine, structurally, for the same reason `stackWorkflow`'s id is the stack fqn: two
 * concurrent converges on one session would race on `has-session` and produce duplicate windows.
 *
 * ONE-SHOT, NOT A RECONCILER. It converges, drains any signals that arrived, and exits. There is
 * deliberately no polling loop: the READ path is the drift detector — a Terminal that cannot attach
 * reports "no session" and offers the converge, which costs nothing when nobody is looking and
 * duplicates neither systemd nor the stack converge. A `kontra-tmux.service` unit is what this
 * replaces, and its failure mode (a `ConditionPathExists` that silently skipped) is what a workflow
 * cannot do.
 *
 * Because it exits, a signal that arrives later belongs to a NEW run: callers use
 * signal-with-start, or start it again. The short grace window below only exists so a signal racing
 * the initial converge is not lost between the activity completing and the workflow returning.
 */

import * as wf from '@temporalio/workflow';
import { ApplicationFailure } from '@temporalio/common';
import type * as activities from '../activities/panels';
import { DEFAULT_WINDOWS, type SessionWindow } from '../panels/converge';
import { SAFE } from '../panels/ids';

/**
 * A converge is one SSH round trip that may apt-install tmux while cloud-init still holds the dpkg
 * lock, so minutes, not seconds. No heartbeatTimeout: the activity is a single blocking `ssh` and a
 * heartbeat it cannot send would fail a converge that is working.
 */
const { convergeTmuxSession, killTmuxSession } = wf.proxyActivities<typeof activities>({
  startToCloseTimeout: '10 minutes',
  retry: { maximumAttempts: 3 },
});

export interface TmuxSessionInput {
  machine: string;
  host: string;
  session: string;
  /** Empty means the defaults: `actor` and `handler` following their journals. */
  windows: SessionWindow[];
}

export interface TmuxSessionResult {
  machine: string;
  session: string;
  windows: string[];
  created: boolean;
}

/** Tear the session down and build it again — for a session whose windows drifted, or whose
 * geometry predates the fixed-size converge. */
export const recreate = wf.defineSignal('recreate');

/** Remove the session. The desired state becomes "absent"; the tile then says "no session". */
export const kill = wf.defineSignal('kill');

/** Add one window to the session, converging it in place. */
export const addWindow = wf.defineSignal<[SessionWindow]>('addWindow');

/** Progress for a caller watching the converge. Named `getSessionState` rather than `getProgress`
 * because this workflow and `stackWorkflow` share one bundle, and two queries with one name would
 * collide at registration. */
export const getSessionState = wf.defineQuery<Record<string, unknown>>('getSessionState');

type Pending = { t: 'recreate' } | { t: 'kill' } | { t: 'addWindow'; window: SessionWindow };

/** How long the workflow waits for a racing signal after its initial converge. Deliberately short:
 * a one-shot workflow that lingers is a reconciler nobody asked for. */
const SIGNAL_GRACE = '5 seconds';

export async function tmuxSessionWorkflow(input: TmuxSessionInput): Promise<TmuxSessionResult> {
  validate(input);

  const windows: SessionWindow[] = input.windows?.length ? [...input.windows] : [...DEFAULT_WINDOWS];
  const pending: Pending[] = [];
  let state: Record<string, unknown> = { phase: 'starting', machine: input.machine };
  let result: TmuxSessionResult = {
    machine: input.machine,
    session: input.session,
    windows: [],
    created: false,
  };

  wf.setHandler(getSessionState, () => state);
  wf.setHandler(recreate, () => {
    pending.push({ t: 'recreate' });
  });
  wf.setHandler(kill, () => {
    pending.push({ t: 'kill' });
  });
  wf.setHandler(addWindow, (window: SessionWindow) => {
    pending.push({ t: 'addWindow', window });
  });

  const converge = async (): Promise<void> => {
    state = { phase: 'converging', machine: input.machine, windows: windows.map((w) => w.name) };
    result = await convergeTmuxSession({
      machine: input.machine,
      host: input.host,
      session: input.session,
      windows,
    });
    state = { phase: 'converged', machine: input.machine, created: result.created };
  };

  await converge();

  // Drain signals. Each one is applied in order, and the loop exits when a grace window passes with
  // nothing queued — signals after that are a new run's problem, which is what one-shot means.
  for (;;) {
    if (!pending.length) {
      const arrived = await wf.condition(() => pending.length > 0, SIGNAL_GRACE);
      if (!arrived) break;
    }
    const op = pending.shift();
    if (!op) break;
    if (op.t === 'kill') {
      state = { phase: 'killing', machine: input.machine };
      await killTmuxSession({ machine: input.machine, host: input.host, session: input.session });
      state = { phase: 'killed', machine: input.machine };
      return { machine: input.machine, session: input.session, windows: [], created: false };
    }
    if (op.t === 'recreate') {
      await killTmuxSession({ machine: input.machine, host: input.host, session: input.session });
      await converge();
      continue;
    }
    // addWindow: idempotent by name, because the converge itself is.
    if (!windows.some((w) => w.name === op.window.name)) windows.push(op.window);
    await converge();
  }

  return result;
}

/**
 * Reject unsafe input HERE as well as in the activity.
 *
 * Non-retryable on purpose: a hostile or malformed machine name is not a transient failure, and
 * retrying it three times only puts the same string in front of the same shell twice more. The
 * activity validates again — that is the boundary that actually matters — but a workflow that fails
 * fast puts the reason in the history where an operator reads it.
 */
function validate(input: TmuxSessionInput): void {
  const bad = (field: string, value: unknown): never => {
    throw ApplicationFailure.nonRetryable(
      `tmuxSessionWorkflow: ${field}=${JSON.stringify(value)} is not safe to place on a Machine`,
      'BadTmuxSessionInput'
    );
  };
  if (!input || typeof input !== 'object') bad('input', input);
  if (!SAFE.machine.test(input.machine ?? '')) bad('machine', input.machine);
  if (!SAFE.host.test(input.host ?? '')) bad('host', input.host);
  if (!SAFE.session.test(input.session ?? '')) bad('session', input.session);
  for (const w of input.windows ?? []) {
    if (!SAFE.window.test(w?.name ?? '')) bad('window', w?.name);
    if (!SAFE.command.test(w?.command ?? '')) bad('command', w?.command);
  }
}
