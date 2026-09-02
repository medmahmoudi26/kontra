/**
 * The three modes, wired: which transport reaches each, and which are switched on here (slice 6).
 *
 * `transport.ts` is the seam and holds no implementation; `ssh.ts`, `docker.ts` and `local.ts` are
 * the implementations and know nothing of each other. This file is the ONLY place that maps a mode to
 * a transport, which is what makes "nothing may assume `fleet:`" checkable: a code path that reaches
 * a tmux server without coming through here does not exist.
 *
 * WHY A DISPATCHING RUNNER RATHER THAN THREE STREAMERS. A wall is mixed. One Dashboard shows a
 * run's Machines beside the worker you are running on your laptop, and a Terminal id carries its
 * own mode, so the mode is resolved PER COMMAND from the target — the same way `probe`, `snapshot`,
 * the backlog read and the viewer kill all take a target and a command string and nothing else. That
 * is the whole reason the seam is `run(target, command)` and not a per-mode server.
 */

import { isExecutionMode, MODES, type ExecutionMode } from './ids';
import {
  modeOf,
  parseModes,
  type CommandRunner,
  type ExecResult,
  type ExecTarget,
} from './transport';
import { realSshRunner, sshArgs } from './ssh';
import { dockerExecArgs, dockerRunner } from './docker';
import { localRunner, localShArgs } from './local';

/** The binary each mode spawns, for an error sentence that names what is missing. */
export const MODE_BINARY: Record<ExecutionMode, string> = {
  fleet: 'ssh',
  docker: 'docker',
  local: 'sh',
} as const;

/**
 * The argv that carries one command to one node.
 *
 * The three shapes, in one function, so they can be read side by side and diffed by a test:
 *
 *   fleet   ssh    -o … root@<host> '<command>'
 *   docker  docker exec <container> sh -c '<command>'
 *   local   sh     -c '<command>'
 *
 * In every one of them the command is a SINGLE argv element handed to a shell on the far side. No
 * local shell is involved in any mode, and no `spawn` in this directory is given the option that
 * would make one (`readonly.test.ts` greps for it, which is why this comment does not spell it) — the
 * remote string is data here and a program only over there.
 */
export function execArgv(target: ExecTarget, command: string): { file: string; args: string[] } {
  const mode = modeOf(target);
  switch (mode) {
    case 'docker':
      return { file: MODE_BINARY.docker, args: dockerExecArgs(target, command) };
    case 'local':
      return { file: MODE_BINARY.local, args: localShArgs(command) };
    default:
      return { file: MODE_BINARY.fleet, args: sshArgs(target, command) };
  }
}

/** The one-shot runner for one mode. */
export function runnerFor(mode: ExecutionMode): CommandRunner {
  switch (mode) {
    case 'docker':
      return dockerRunner;
    case 'local':
      return localRunner;
    default:
      return realSshRunner;
  }
}

/**
 * One runner that reaches all three, chosen by the target's own mode.
 *
 * This is what the streamer and the attacher hold: `probe.ts` and `attach.ts` were written against
 * `run(target, command)` and neither had to learn that there is more than one place a command can
 * go — which is the property that keeps the probe, the snapshot and the attach IDENTICAL across
 * modes rather than three times reimplemented.
 */
export const panelRunner: CommandRunner = {
  run(target: ExecTarget, command: string, opts?: { timeoutMs?: number }): Promise<ExecResult> {
    return runnerFor(modeOf(target)).run(target, command, opts);
  },
};

/**
 * Modes discovery will look in, from `KONTRA_PANEL_MODES` (comma-separated).
 *
 * ALL THREE BY DEFAULT, and that is a considered default rather than an eager one. Each mode's
 * discovery is independent and fails to an empty list with a logged sentence: no stack outputs means
 * no Machines, no `docker` binary means no containers, no local tmux server means no local sessions.
 * So the default costs a streamer nothing it does not have, and it is what makes the feature
 * exercisable on a laptop with no fleet — the point of the mode existing. An operator who wants the
 * streamer to look ONLY at the fleet says `KONTRA_PANEL_MODES=fleet`.
 *
 * An unrecognised word is dropped and reported, never treated as "all": a typo must not be how the
 * docker socket and the host's tmux server end up in front of a Dashboard.
 */
export function enabledModes(raw = process.env.KONTRA_PANEL_MODES): {
  modes: ExecutionMode[];
  unknown: string[];
} {
  return parseModes(raw, MODES, MODES);
}

export { isExecutionMode, MODES, modeOf };
export type { ExecutionMode };
