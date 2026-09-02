/**
 * Mode `fleet`: the SSH transport (ADR 0020, slice 1; one of three since slice 6).
 *
 * This file is no longer "the transport" — it is the implementation of {@link Transport} that
 * reaches a fleet MACHINE, beside `docker.ts` and `local.ts`. What it still exclusively owns is
 * everything SSH: the flag set, the `ControlMaster` socket, and the key. The interface it satisfies,
 * and the child-process accounting every mode shares, are in `transport.ts`.
 *
 * Two things this file exists to guarantee.
 *
 * **The command line is built here, entirely server-side.** ADR 0020's finding (3) measured that
 * `tmux attach -r` is not a security boundary — through a read-only client both `run-shell` and
 * `send-keys` executed, as root, with no error. So "read-only" cannot be a property of tmux; it
 * has to be a property of our own surface. It is: `spawn` is given an argv ARRAY with no shell,
 * the remote string is assembled from whitelisted parts, and {@link SshRunner} has no way to write
 * to a running command's stdin. There is no channel for anything to land in.
 *
 * **It is an interface, so tests never dial.** No fleet exists in this environment and no Machine
 * can be SSHed to; every path above this file is proven against a fake runner, the same way
 * `cli/workers.go` proves its Temporal join against a `queueDescriber`.
 *
 * `ControlMaster` is what makes a snapshot cheap: the first exec pays the handshake, the next
 * sixty reuse the connection, and `ControlPersist=60` drops it when nobody is watching.
 */

import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdir } from 'node:fs/promises';
import * as path from 'node:path';
import { assertSafe } from './ids';
import {
  captureChild,
  DEFAULT_TIMEOUT_MS,
  MAX_OUTPUT_BYTES,
  type CommandRunner,
  type ExecResult,
  type ExecTarget,
} from './transport';

/** Where the multiplexing sockets live. One per Machine, named after it. */
export function controlDir(): string {
  return process.env.KONTRA_PANEL_CONTROL_DIR ?? '/run/kontra-panels';
}

/** The read-only fleet key. Already mounted on `orchestrator-infra`, and inherited by the forked
 * child — same container, same key, different PID (ADR 0020). */
export function keyPath(): string {
  return process.env.KONTRA_SSH_KEY ?? '/run/secrets/fleet_key';
}

/**
 * The flags every panels SSH carries.
 *
 * `BatchMode=yes` because a password prompt in a background probe is a hang, not a prompt.
 * `accept-new` rather than `no` so a rebuilt Machine's new host key is learned once instead of
 * failing every tile forever — it still refuses a CHANGED key, which is the property worth having
 * and the reason the known-hosts file it learns into is scoped rather than shared (see
 * {@link knownHostsPath}). `ServerAlive*` bounds a dead connection at ~45 s, which is what a
 * Machine that vanished mid-run looks like.
 */
export const SSH_FLAGS: readonly string[] = [
  '-o',
  'BatchMode=yes',
  '-o',
  'StrictHostKeyChecking=accept-new',
  '-o',
  'ConnectTimeout=10',
  '-o',
  'ServerAliveInterval=15',
  '-o',
  'ServerAliveCountMax=3',
] as const;

/**
 * Where this Machine's learned host key lives — and why it is not the shared `~/.ssh/known_hosts`.
 *
 * MEASURED, ON THIS DEPLOYMENT. The streamer's known-hosts file held keys for `10.124.0.3` through
 * `10.124.0.12`: the exact addresses DigitalOcean's VPC hands out, one fleet after another. A
 * run's Machines are destroyed when it ends and the next run's Machines are NEW hosts at
 * those SAME addresses with new host keys — so `accept-new` did what it promises and refused every
 * one of them:
 *
 *     @@@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @@@
 *     Host key verification failed.                                          (exit 255)
 *
 * Every fleet tile on the Monitor therefore showed an SSH error, on every fleet after the first,
 * for as long as the file survived. The flag was not wrong; the file it wrote into was, because a
 * shared known-hosts file assumes an address identifies a host over time and on ephemeral
 * infrastructure it does not.
 *
 * SO THE FILE IS KEYED BY THE MACHINE'S IDENTITY, not by the address it answers on. The name alone
 * is not that identity — the fleet mints `kf-dns-01` every run — so the key is the name
 * together with the addresses the inventory reports for it. A new run's `kf-dns-01` gets a
 * fresh public address and therefore a fresh file, while every poll of the SAME Machine reuses one.
 *
 * WHAT THAT KEEPS, and it is the reason this is not simply `UserKnownHostsFile=/dev/null`: a key
 * that changes UNDER a Machine we are already watching is still refused, which is the case where
 * TOFU is worth anything. What it gives up is detection across runs, which was never real —
 * the host genuinely changed, and reporting that as an attack is how a true positive becomes noise
 * that gets turned off.
 *
 * Hashed rather than concatenated because the parts reach a filesystem path. `machine` is already
 * whitelisted; an address from a stack output is not.
 */
export function knownHostsPath(target: SshTarget): string {
  assertSafe('machine', target.machine);
  const identity = createHash('sha256')
    .update(`${target.machine}\0${target.host}\0${target.publicIp ?? ''}`)
    .digest('hex')
    .slice(0, 12);
  return path.join(controlDir(), `kh-${target.machine}-${identity}`);
}

/**
 * A fleet target: the Machine (for the `ControlPath` name — validated, because it becomes a
 * filesystem path) and the address to dial.
 *
 * An alias of {@link ExecTarget} since slice 6, kept under this name because slice 1's and slice 2's
 * tests construct `SshTarget` literals and this refactor must not change what they prove.
 */
export type SshTarget = ExecTarget;

/**
 * Build the argv for one remote exec.
 *
 * Returns an ARRAY on purpose. There is no interpolation of the remote command into a local shell
 * anywhere in the panels path: the remote string is one argv element, and `sh` on the Machine is
 * the only shell involved.
 */
export function sshArgs(target: SshTarget, remote: string, opts?: { controlPath?: string }): string[] {
  assertSafe('machine', target.machine);
  assertSafe('host', target.host);
  const control = opts?.controlPath ?? path.join(controlDir(), `cm-${target.machine}`);
  return [
    ...SSH_FLAGS,
    // Scoped to THIS Machine, so a recycled VPC address cannot make one run's learned key
    // refuse the next run's Machine. `GlobalKnownHostsFile=/dev/null` because `/etc/ssh/` is
    // the other file `accept-new` would consult, and a key in it would defeat the scoping.
    '-o',
    `UserKnownHostsFile=${knownHostsPath(target)}`,
    '-o',
    'GlobalKnownHostsFile=/dev/null',
    // One master per Machine. Sixty seconds of persistence covers a 3-second snapshot cadence
    // with room to spare and costs nothing once nobody is subscribed.
    '-o',
    'ControlMaster=auto',
    '-o',
    `ControlPath=${control}`,
    '-o',
    'ControlPersist=60',
    '-i',
    keyPath(),
    `root@${target.host}`,
    remote,
  ];
}

/** An alias since slice 6 — see {@link SshTarget}. */
export type SshResult = ExecResult;

/** The seam. One method, no stdin, no streaming — the live attach adds its own (`PtyOpener`).
 *
 * An alias of {@link CommandRunner} since slice 6: `probe.ts`, `attach.ts` and
 * `activities/panels.ts` all name this type, and every one of them works against any mode's
 * transport, which is the entire point of the extraction. */
export type SshRunner = CommandRunner;

export { MAX_OUTPUT_BYTES, DEFAULT_TIMEOUT_MS };

/**
 * The real runner.
 *
 * The child's descriptors and its argv are decided HERE, and its accounting — the timeout, the
 * output cap, the `'error'` listener on every stream — is `transport.ts`'s {@link captureChild},
 * shared with the other two modes. Both halves matter: an unheard `'error'` is the shape ADR 0020's
 * finding (6) measured failing an in-flight `pulumi up`, and a first descriptor that was not
 * `'ignore'` would be a channel a byte could reach a Machine through.
 */
export const realSshRunner: SshRunner = {
  async run(target, remote, opts): Promise<SshResult> {
    // ControlMaster refuses to create a socket in a directory that does not exist, and the error
    // ("Control socket connect: No such file or directory") reads like a dead Machine.
    await mkdir(controlDir(), { recursive: true }).catch(() => {
      /* a read-only /run is survivable: ssh just stops multiplexing */
    });
    const args = sshArgs(target, remote);
    const child = spawn('ssh', args, { stdio: ['ignore', 'pipe', 'pipe'] });
    const capture: { binary: string; timeoutMs?: number } = { binary: 'ssh' };
    if (opts?.timeoutMs !== undefined) capture.timeoutMs = opts.timeoutMs;
    return await captureChild(child, capture);
  },
};
