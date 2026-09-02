/**
 * Session existence and the read probes, as Temporal activities (ADR 0020).
 *
 * These run on `INFRA_QUEUE` for two reasons, both of which are authority rather than convenience:
 * that worker is the only process holding `KONTRA_SSH_KEY`, and creating a session on a Machine is
 * FLEET authority — `CONTEXT-MAP.md` forbids a Run reaching a Machine. The same rule that put
 * `pulumi up` in an activity puts these here: they do blocking network I/O to a host that may be
 * rebooting, and durability, retries and cancellation are Temporal's properties, not SSH's.
 *
 * The converge replaces the `kontra-tmux.service` unit `machine.ts` used to install. That unit was
 * `ConditionPathExists=/usr/bin/tmux` beside an apt install the installer was allowed to fail, so a
 * Machine could deploy "successfully" and never be viewable — silently. A failed activity is not
 * silent.
 *
 * Every interpolated value passes a whitelist first (see `panels/ids.ts`). The remote command is
 * one argv element handed to `ssh`; no local shell is involved at any point.
 */

import { Context } from '@temporalio/activity';
import {
  convergeScript,
  killScript,
  parseConvergeReport,
  type SessionWindow,
} from '../panels/converge';
import { assertSafe } from '../panels/ids';
import { realSshRunner, type SshResult, type SshRunner } from '../panels/ssh';
import {
  batchSnapshotCommand,
  LIST_PANES_COMMAND,
  parseBatchSnapshot,
  parseListPanes,
  type PaneRow,
} from '../panels/tmux';

/**
 * The SSH seam.
 *
 * A mutable module-level runner, in the spirit of `cli/workers.go`'s `newDescriber`: no fleet
 * exists in this environment and no Machine can be SSHed to, so the only way these activities are
 * provable at all is that a test can put a fake here.
 */
let runner: SshRunner = realSshRunner;

export function useSshRunner(next: SshRunner): void {
  runner = next;
}

export function resetSshRunner(): void {
  runner = realSshRunner;
}

/** apt-installing tmux on a cold Machine, with cloud-init possibly still holding the dpkg lock, is
 * minutes rather than seconds. The workflow's StartToClose is the real bound; this stops one wedged
 * `ssh` from holding an activity slot forever. */
const CONVERGE_TIMEOUT_MS = 8 * 60_000;
const PROBE_TIMEOUT_MS = 15_000;

/** Heartbeat if we are inside an activity; do nothing if a test called the function directly.
 * Keeping these callable outside a worker is what makes them testable without Temporal. */
function beat(detail: Record<string, unknown>): void {
  try {
    Context.current().heartbeat(detail);
  } catch {
    /* not running as an activity */
  }
}

export interface TmuxTarget {
  /** `kf-<role>-NN`. Validated: it becomes part of a ControlPath as well as a log line. */
  machine: string;
  host: string;
  /**
   * The Machine's public address, when the caller has one. Optional and additive — a converge
   * started by an older caller still runs.
   *
   * It is here for exactly one reason: it is half of what keys the transport's known-hosts file
   * (`panels/ssh.ts`, {@link knownHostsPath}). Without it every run's `kf-dns-01` shares a file
   * with the last run's, at the VPC address the VPC just recycled, and the converge fails
   * `Host key verification failed` for the same reason the tiles did.
   */
  publicIp?: string;
}

export interface ConvergeSessionInput extends TmuxTarget {
  session: string;
  windows: SessionWindow[];
  cols?: number;
  rows?: number;
}

export interface ConvergeSessionResult {
  machine: string;
  session: string;
  /** The windows the Machine reports AFTER the converge — not the ones we asked for. */
  windows: string[];
  /** True when this converge created the session rather than finding it. */
  created: boolean;
}

function failed(what: string, target: TmuxTarget, res: SshResult): Error {
  const first = res.stderr.trim().split('\n').slice(-3).join(' | ');
  return new Error(
    `panels: ${what} on ${target.machine} (${target.host}) exited ${res.code}` +
      `${res.timedOut ? ' after timing out' : ''}: ${first}`
  );
}

/**
 * Ensure a session and its windows exist. Idempotent, one SSH round trip.
 *
 * A Machine deployed WITHOUT `--tmux` is converged by exactly this call, with no re-deploy — which
 * is the property that let `machine.ts` stop owning sessions at all.
 */
export async function convergeTmuxSession(
  input: ConvergeSessionInput
): Promise<ConvergeSessionResult> {
  const target = validateTarget(input);
  const script = convergeScript({
    session: input.session,
    windows: input.windows,
    ...(input.cols === undefined ? {} : { cols: input.cols }),
    ...(input.rows === undefined ? {} : { rows: input.rows }),
  });
  beat({ phase: 'converge', machine: input.machine, session: input.session });
  const res = await runner.run(target, script, { timeoutMs: CONVERGE_TIMEOUT_MS });
  if (res.code !== 0) throw failed('the session converge', target, res);
  const report = parseConvergeReport(res.stdout);
  beat({ phase: 'converged', created: report.created, windows: report.windows.length });
  return {
    machine: input.machine,
    session: input.session,
    windows: report.windows,
    created: report.created,
  };
}

/** `kill-session`. The desired state is "absent", so killing a session that is already gone is a
 * success — which is what makes `recreate` safe to signal twice. */
export async function killTmuxSession(
  input: TmuxTarget & { session: string }
): Promise<{ machine: string; session: string }> {
  const target = validateTarget(input);
  assertSafe('session', input.session);
  const res = await runner.run(target, killScript(input.session), { timeoutMs: PROBE_TIMEOUT_MS });
  if (res.code !== 0) throw failed('kill-session', target, res);
  return { machine: input.machine, session: input.session };
}

/** Every pane on the Machine — the discovery probe, and the drift detector for session existence
 * (there is deliberately no polling reconciler). */
export async function listTmuxPanes(input: TmuxTarget): Promise<PaneRow[]> {
  const target = validateTarget(input);
  const res = await runner.run(target, LIST_PANES_COMMAND, { timeoutMs: PROBE_TIMEOUT_MS });
  if (res.code !== 0) throw failed('list-panes', target, res);
  return parseListPanes(res.stdout);
}

export interface CaptureInput extends TmuxTarget {
  session: string;
  /** One or more windows, captured in ONE exec — twelve Machines cost twelve execs, not
   * twenty-four persistent PTYs (ADR 0020). */
  windows: string[];
  lines?: number;
}

/** A snapshot per window: a full screen, not a log. The caller prefixes each with home+clear
 * before it reaches a tile. */
export async function captureTmuxPanes(input: CaptureInput): Promise<Record<string, string>> {
  const target = validateTarget(input);
  const remote = batchSnapshotCommand(
    input.session,
    input.windows,
    input.lines === undefined ? undefined : input.lines
  );
  const res = await runner.run(target, remote, { timeoutMs: PROBE_TIMEOUT_MS });
  if (res.code !== 0) throw failed('capture-pane', target, res);
  const out: Record<string, string> = {};
  for (const [window, screen] of parseBatchSnapshot(res.stdout)) out[window] = screen;
  return out;
}

function validateTarget(input: TmuxTarget): TmuxTarget {
  return {
    machine: assertSafe('machine', input.machine),
    host: assertSafe('host', input.host),
    // Admitted on the same terms as the address it sits beside: it reaches a hashed filename rather
    // than a command, but an unvalidated value that crosses this function is one nobody checks later.
    ...(input.publicIp ? { publicIp: assertSafe('host', input.publicIp) } : {}),
  };
}
