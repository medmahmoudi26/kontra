/**
 * The transport seam — how a command REACHES a tmux server (ADR 0020, slice 6).
 *
 * Slice 1 had one place a Worker could run, so `ssh.ts` was both "the transport" and "the only
 * transport". `kontra serve --mode fleet|docker|local` makes that three places, and the Dashboard has
 * to treat all three with the SAME LOGIC: the converge script, the `list-panes` probe, the
 * `capture-pane` snapshot and the PTY attach are unchanged — every one of them emits shell, and
 * every mode runs shell. What differs is only the argv that carries the string to a shell:
 *
 *   fleet    `ssh -o ControlMaster=auto … root@<host> '<command>'`   (`ssh.ts`)
 *   docker   `docker exec <container> sh -c '<command>'`             (`docker.ts`)
 *   local    `sh -c '<command>'`                                     (`local.ts`)
 *
 * So this file is the interface and the SHARED ACCOUNTING, and nothing else. The three
 * implementations each own their own `spawn` — deliberately, because the descriptors a child is
 * given are the read-only boundary (`readonly.test.ts` pins them per file, and a shared spawn would
 * put that invariant one indirection away from the code that has to hold it) — but the timeout, the
 * output cap and the `'error'` listeners are written ONCE. A transport that forgot the last of those
 * is the shape ADR 0020's finding (6) measured failing an in-flight `pulumi up`.
 *
 * WHAT A TRANSPORT CANNOT DO, in any mode: write to a running command's stdin. There is no method
 * for it here, `stdio[0]` is `'ignore'` in every implementation, and finding (3) is why — through a
 * read-only tmux client both `run-shell` and a keystroke injection executed as root. Read-only is a
 * property of this seam, not of tmux.
 */

import type { ChildProcess } from 'node:child_process';
import type { ExecutionMode } from './ids';

/**
 * Where one command runs.
 *
 * `machine` is the node the tmux server lives on and the SECOND SEGMENT of every Terminal id on it:
 * a Machine for `fleet`, a container for `docker`, this host for `local`. It keeps slice 1's field
 * name because a Terminal's wire shape keeps `machine` too (additively — slice 5's Go struct decodes
 * the current shape and renaming a field would break it), and because the fleet is still the mode
 * where the word is literal.
 *
 * `mode` is OPTIONAL and absent means `fleet`. Not laziness: it is what lets slice 1's and slice 2's
 * tests — which construct `{machine, host}` because fleet was the only mode — keep passing verbatim,
 * so this refactor cannot quietly change the transport they prove.
 */
export interface ExecTarget {
  mode?: ExecutionMode;
  machine: string;
  /** The address a DIALLING transport needs. Unused by the two that do not dial. */
  host: string;
  /**
   * A second address for the same node, when the inventory has one — the Machine's PUBLIC address
   * beside the VPC one it is dialled on.
   *
   * Not an alternative route. It is here because a Machine's NAME is not unique over time: the
   * fleet mints `kf-dns-01` for every run, and the VPC hands the new one an address the last
   * one had. Together the two addresses distinguish one fleet's Machine from the next one's, which
   * is the whole of what `ssh.ts`'s known-hosts file is keyed on. See {@link knownHostsPath}.
   */
  publicIp?: string;
}

/** The mode a target is addressed in. One place decides the default, so a new mode cannot be
 * silently mistaken for the fleet at one call site and not another. */
export function modeOf(target: { mode?: ExecutionMode }): ExecutionMode {
  return target.mode ?? 'fleet';
}

export interface ExecResult {
  code: number;
  stdout: string;
  stderr: string;
  /** True when the runner killed the command for exceeding its timeout. */
  timedOut: boolean;
}

/** One-shot, buffered, no stdin: the probe, the snapshot, the backlog read and the viewer kill. */
export interface CommandRunner {
  run(target: ExecTarget, command: string, opts?: { timeoutMs?: number }): Promise<ExecResult>;
}

export interface PtyEvents {
  onData(chunk: Buffer): void;
  /** The local child could not be started at all — a missing binary, not a missing node. */
  onSpawnError(err: Error): void;
  onExit(code: number | null, signal: NodeJS.Signals | null, stderr: string): void;
}

export interface PtyChannel {
  kill(): void;
  /** True when the child has no writable first descriptor. Asserted, not assumed: it is the whole
   * read-only boundary. */
  readonly noWritableInput: boolean;
}

/** The streaming half. Separate from {@link CommandRunner} because a live attach neither buffers
 * nor terminates, not because it is a different mode. */
export interface PtyOpener {
  open(target: ExecTarget, command: string, events: PtyEvents): PtyChannel;
}

/** One mode's whole transport. `modes.ts` holds the three and picks between them. */
export interface Transport extends CommandRunner, PtyOpener {
  readonly mode: ExecutionMode;
  /** The binary this transport spawns, for an error sentence that names what is missing. */
  readonly binary: string;
}

/** Output cap per exec. A snapshot of a 200x50 screen with escapes is a few tens of KiB; a runaway
 * `capture-pane` must not be allowed to become the streamer's memory profile. */
export const MAX_OUTPUT_BYTES = 2 * 1024 * 1024;

export const DEFAULT_TIMEOUT_MS = 20_000;

/**
 * How long a child that has EXITED gets to let its pipes reach EOF.
 *
 * MEASURED, on node 22 with `/bin/sh` → dash, and this bound is a bug fix rather than a preference.
 * `'close'` fires when the child has exited AND our copies of its stdio have ended; a SIGKILLed shell
 * can leave a GRANDCHILD holding them. `sh -c 'sleep 30'` killed after 150 ms emitted `'exit'`
 * (SIGKILL) at 389 ms and **never emitted `'close'`** — the orphaned `sleep` still owned the write end
 * of our pipe. Awaiting `'close'` alone therefore hangs a probe forever, which for the streamer means
 * one wedged local exec pins the discovery loop's `discovering` flag and the wall stops refreshing.
 *
 * This never bit the fleet, which is why slice 1 did not find it: there the direct child is the `ssh`
 * client and it owns both pipes itself. It bites every transport that spawns a LOCAL shell. Same
 * reasoning as `PanelServer.close()`'s `CLOSE_DRAIN_MS`: a drain has a deadline.
 */
export const EXIT_DRAIN_MS = 250;

/**
 * Everything a one-shot child needs to become an {@link ExecResult}, written once.
 *
 * Every listener a child process can emit is attached, including `'error'` — ADR 0020's finding (6)
 * measured an `EventEmitter` `'error'` with no listener failing an in-flight `pulumi up` from a
 * different module in the same process. The streamer is a separate PID for that reason, and it still
 * does not get to be sloppy: the child that dies unheard here is the one that reintroduces a silent
 * fleet-deploy failure if anyone ever folds these processes back together.
 *
 * Never rejects. A transport that throws is a tile that disappears, and every caller above this
 * treats a non-zero `code` as the thing to explain to an operator.
 */
export function captureChild(
  child: ChildProcess,
  opts: {
    binary: string;
    timeoutMs?: number;
    maxBytes?: number;
    /** Signal the child's whole PROCESS GROUP, not just the child. For a transport that spawns a
     * local shell (`local`, `docker`) that is the difference between killing a wedged command and
     * leaving its grandchild running on the operator's own box; it requires `detached: true` at
     * spawn, so the transport asks for it rather than this function assuming it. */
    killGroup?: boolean;
  }
): Promise<ExecResult> {
  const timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const maxBytes = opts.maxBytes ?? MAX_OUTPUT_BYTES;

  return new Promise<ExecResult>((resolve) => {
    let stdout = '';
    let stderr = '';
    let bytes = 0;
    let timedOut = false;
    let done = false;
    let drain: NodeJS.Timeout | undefined;

    const finish = (code: number): void => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      if (drain) clearTimeout(drain);
      // Release the pipes. After a group kill they are usually already gone; after an exit-drain they
      // are held by an orphan, and keeping them would keep appending to a promise nobody awaits.
      child.stdout?.destroy();
      child.stderr?.destroy();
      resolve({ code, stdout, stderr, timedOut });
    };

    /** SIGKILL the child, or its group when the transport spawned one. */
    const kill = (): void => {
      if (opts.killGroup && typeof child.pid === 'number') {
        try {
          process.kill(-child.pid, 'SIGKILL');
          return;
        } catch {
          /* the group is already gone; fall through to the child itself */
        }
      }
      child.kill('SIGKILL');
    };

    const timer = setTimeout(() => {
      timedOut = true;
      kill();
    }, timeoutMs);

    child.stdout?.setEncoding('utf8');
    child.stderr?.setEncoding('utf8');
    child.stdout?.on('data', (chunk: string) => {
      bytes += Buffer.byteLength(chunk);
      if (bytes > maxBytes) {
        stderr += `\npanels: output exceeded ${maxBytes} bytes; killed`;
        kill();
        return;
      }
      stdout += chunk;
    });
    child.stderr?.on('data', (chunk: string) => {
      if (stderr.length < 64 * 1024) stderr += chunk;
    });
    // Streams get their own 'error' listeners: an EPIPE on a killed child is otherwise an
    // unhandled 'error' event, which is a process-level throw.
    child.stdout?.on('error', () => {
      /* the close handler below reports what we got */
    });
    child.stderr?.on('error', () => {
      /* idem */
    });
    child.on('error', (err: Error) => {
      stderr += `\npanels: ${opts.binary} could not be spawned: ${err.message}`;
      finish(127);
    });
    // `'close'` is the good path: the child is gone AND its output is complete.
    child.on('close', (code) => finish(code ?? (timedOut ? 124 : 1)));
    // `'exit'` is the bound. See EXIT_DRAIN_MS: a killed shell can leave a grandchild holding our
    // pipes, and then `'close'` never comes. The drain is what stops that from wedging a probe.
    child.on('exit', (code) => {
      if (done || drain) return;
      drain = setTimeout(() => finish(code ?? (timedOut ? 124 : 1)), EXIT_DRAIN_MS);
      drain.unref();
    });
  });
}

/** How a mode's failure to reach its node reads in a health sentence. `fleet`'s wording is
 * unchanged from slice 1, because `probe.test.ts` reads those sentences and an operator has learnt
 * them. */
export function reachWord(mode: ExecutionMode, node: string): string {
  switch (mode) {
    case 'docker':
      return `docker exec on ${node}`;
    case 'local':
      return `the local tmux server`;
    default:
      return `ssh to ${node}`;
  }
}

/** The three modes, as a comma-separated env value: `KONTRA_PANEL_MODES=fleet,local`.
 *
 * Unknown words are DROPPED with the caller logging them rather than silently enabling everything:
 * a typo'd mode that quietly turned into "all of them" would put the docker socket and the local
 * tmux server in front of the Dashboard on a controller where nobody asked for either. */
export function parseModes(
  raw: string | undefined,
  all: readonly ExecutionMode[],
  fallback: readonly ExecutionMode[]
): { modes: ExecutionMode[]; unknown: string[] } {
  if (raw === undefined || raw.trim() === '') return { modes: [...fallback], unknown: [] };
  const wanted = raw
    .split(',')
    .map((m) => m.trim().toLowerCase())
    .filter(Boolean);
  const modes: ExecutionMode[] = [];
  const unknown: string[] = [];
  for (const w of wanted) {
    const found = all.find((m) => m === w);
    if (found === undefined) unknown.push(w);
    else if (!modes.includes(found)) modes.push(found);
  }
  return { modes, unknown };
}
