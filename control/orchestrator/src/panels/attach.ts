/**
 * The live attach — a PTY, a per-viewer grouped session, and the only path bytes take off a
 * Machine in real time (ADR 0020, slice 2).
 *
 * The wall is snapshots; this file is what a FOCUSED Terminal gets instead. One `capture-pane`
 * exec per Machine paints twenty tiles cheaply, but it repaints on a cadence — a focused tile has
 * to scroll. So focus promotes a Terminal to a real `tmux attach` and blur demotes it again, which
 * keeps the number of persistent `sshd` sessions and tmux clients on a 2 GB Machine equal to the
 * number of tiles somebody is actually watching.
 *
 * WHY A PTY AND NOT CONTROL MODE, measured rather than assumed. With a stalled reader, a
 * control-mode client made a Machine's tmux server grow 10 MB → 80 MB in five seconds and then
 * segfaulted it, destroying every session on the socket, because `%output` is a log and a lagging
 * client forces retention of every byte. An attached client is screen-oriented: tmux owes it only
 * enough escape sequences to make a cols×rows screen correct and can always satisfy a laggard with
 * a full redraw, so the ceiling moves to OUR side of the wire — {@link Ring} and
 * {@link FrameBudget}, reused from the snapshot path rather than reimplemented here.
 *
 * WHY NOTHING CAN BE TYPED INTO IT. ADR 0020's finding (3): through a read-only (`-r`) client both
 * `run-shell` and a keystroke injection EXECUTED as the Machine's root, and tmux reported no error.
 * `-r` gates a client's keystroke handling, not tmux commands. So read-only cannot be a property of
 * tmux, and it is not claimed as one: the child is spawned with its first file descriptor
 * `'ignore'`, so `child.stdin` is `null` and there is no writable descriptor to put a byte into,
 * anywhere, ever. `-r` is kept as defence in depth. Measured locally on tmux 3.3a: a closed
 * descriptor does NOT make `script` exit, so this costs nothing.
 *
 * WHY THE GROUPED SESSION IS PER VIEWER, and why it is killed. `new-session -A -d -s kp-<nonce> -t
 * <owner>` gives each viewer its own selected window and size while sharing the owner's windows, so
 * two browsers on one Machine do not fight over which window is current. Measured on tmux 3.3a
 * (`tmux -L kontratest`), twice, because both facts are load-bearing:
 *
 *   - a viewer session SURVIVES its client dying, so it leaks unless explicitly killed — and a
 *     leaked one keeps the owner's windows alive after the owner session is killed, which turns a
 *     stale browser tab into a Machine that cannot be quiesced;
 *   - killing a viewer session leaves the owner's windows and their running panes untouched, so
 *     the cleanup below is safe. {@link killViewerCommand} additionally refuses any name that is
 *     not `kp-…`, so "close a tile" can never become "kill the operator's session".
 *
 * Every value interpolated into the remote command passes `ids.ts`'s whitelist first, and the two
 * sizes pass an integer range check — they come from a browser reporting what `addon-fit` measured.
 */

import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { killScript } from './converge';
import { assertSafe } from './ids';
import { execArgv, MODE_BINARY, panelRunner } from './modes';
import type { SshRunner, SshTarget } from './ssh';
import { FrameBudget, Ring, type Admission } from './stream';
import { capturePaneCommand, CLEAR_HOME } from './tmux';
import { modeOf, type PtyChannel, type PtyEvents, type PtyOpener } from './transport';

/** Every per-viewer session is named `kp-<nonce>`. The prefix is a claim of ownership: this
 * process created it, this process kills it, and nothing else on the Machine is touched. webmux
 * prefixes for the same reason — several backends on one tmux server must not kill each other's
 * sessions. */
export const VIEWER_PREFIX = 'kp-';

/**
 * Lines of backlog a newly-live Terminal is seeded with.
 *
 * A tile that goes live and then waits for the next line is a tile that looks broken; a journal
 * follower on a quiet Worker can be silent for minutes. 2000 lines is inside the `history-limit
 * 20000` the converge sets, and far above the snapshot path's 50 — a snapshot is a screen, a focus
 * is someone reading.
 */
export const BACKLOG_LINES = 2000;

/** Coalescing tick: ~15 frames/s, the cap `stream.ts` accounts for. A PTY delivers whatever the
 * kernel had ready, which for a chatty journal is dozens of small chunks a second; without a
 * release tick the last bytes of a burst would wait for the next byte to arrive. */
export const FLUSH_MS = 67;

/** The backlog read is also the reachability test, so it is the one that must not hang. `ssh`'s own
 * `ConnectTimeout=10` bounds a dead Machine; this bounds a live one that never answers. */
export const BACKLOG_TIMEOUT_MS = 12_000;

/** Cleanup gets its own, shorter bound: it runs on a socket close, and a Machine that has gone away
 * must not hold the shutdown path open. */
export const KILL_TIMEOUT_MS = 10_000;

/**
 * Terminal geometry a browser may ask for. Not a style rule — these numbers reach `stty`, and
 * through `stty` they reach the operator's real tmux window.
 *
 * THE FLOOR IS THE POINT, and it was one. This was `MIN_DIMENSION = 1`, so a tile that measured
 * itself before layout — collapsed, or zero-width, or mid font-swap — could ask for `12x6` and get
 * it. Measured on this box: one such attach resized a live 200x50 worker to 12x5 and left it there
 * (tmux's default `window-size latest` means the newest client wins AND sticks), so every log line
 * in that session wrapped at twelve characters for a day. The apparent symptom was "the panes are
 * laggy" — at twelve columns one 200-character log line is seventeen rows, so the client repaints a
 * huge cell grid to show almost nothing.
 *
 * kontra's own sessions are additionally pinned with `window-size manual` when they are created
 * (`cli/internal/tmux/tmux.go`), which makes them immune. This floor is what protects a session kontra did not
 * create — an operator's own tmux, attached to the wall.
 *
 * 40x10 is not a guess at a nice size; it is the point below which a terminal stops being one.
 */
export const MIN_COLS = 40;
export const MIN_ROWS = 10;
export const MAX_DIMENSION = 1000;

function minFor(kind: 'cols' | 'rows'): number {
  return kind === 'cols' ? MIN_COLS : MIN_ROWS;
}

function assertDimension(kind: 'cols' | 'rows', value: number): number {
  const min = minFor(kind);
  if (!Number.isInteger(value) || value < min || value > MAX_DIMENSION) {
    throw new Error(
      `panels: ${kind}=${String(value)} is not a terminal dimension (${min}..${MAX_DIMENSION})`
    );
  }
  return value;
}

/** Bound a browser-supplied dimension instead of refusing it: a tile that reported nonsense should
 * render at a sane size, not fail to go live. The strict check above is still what guards the
 * command — and the floor it clamps UP to is what stops a nonsense measurement reaching `stty`. */
export function clampDimension(kind: 'cols' | 'rows', value: unknown, fallback: number): number {
  const n = typeof value === 'number' ? Math.floor(value) : Number.NaN;
  if (!Number.isFinite(n)) return fallback;
  return Math.min(MAX_DIMENSION, Math.max(minFor(kind), n));
}

/** A fresh viewer-session name. 32 bits of randomness, so two tabs opened in the same millisecond
 * cannot collide onto one grouped session and kill each other's view. */
export function newViewerSession(rand: () => string = () => randomBytes(4).toString('hex')): string {
  return viewerSession(rand());
}

export function viewerSession(nonce: string): string {
  const name = `${VIEWER_PREFIX}${nonce}`;
  return assertViewerSession(name);
}

/**
 * Throw unless this is one of OUR viewer sessions.
 *
 * The guard exists because the catastrophic bug in this file would be killing the wrong session:
 * the owner session holds the journals every tile on the Machine reads, and an operator may be
 * attached to it. A name that is not `kp-…` cannot reach {@link killViewerCommand}.
 */
export function assertViewerSession(name: string): string {
  assertSafe('session', name);
  if (!name.startsWith(VIEWER_PREFIX) || name.length <= VIEWER_PREFIX.length) {
    throw new Error(
      `panels: ${JSON.stringify(name)} is not a per-viewer session — only ${VIEWER_PREFIX}* sessions ` +
        'belong to the streamer, and only those may be killed by it'
    );
  }
  return name;
}

/** What a newly-live Terminal is seeded with. Reuses the snapshot path's builder: the difference
 * between a snapshot and a seed is how many lines, not a second implementation. */
export function backlogCommand(session: string, window: string, lines = BACKLOG_LINES): string {
  return capturePaneCommand(session, window, lines);
}

/** `tmux kill-session`, and only ever on a `kp-…` session. */
export function killViewerCommand(viewer: string): string {
  return killScript(assertViewerSession(viewer));
}

export interface AttachTarget {
  target: SshTarget;
  /** The owner session — the one the converge created, which the viewer session is grouped with. */
  session: string;
  window: string;
  cols: number;
  rows: number;
}

/**
 * The remote command, in full.
 *
 * `script -q -c … /dev/null` is the PTY: `tmux attach` refuses to run without a terminal, and an
 * `ssh -t` pseudo-terminal would put OUR stdin on the far end of it. `stty` then sets the size the
 * tile measured, before `exec` replaces the shell with the tmux client so no shell survives to be
 * anything's parent.
 *
 * TWO QUOTING RULES, both load-bearing. The whole inner command is single-quoted, so `\;` reaches
 * the Machine's `sh` as an escaped semicolon and becomes tmux's own command separator rather than
 * the shell's. Inside it, tmux targets are DOUBLE-quoted, because a single quote cannot appear
 * inside a single-quoted string — and that is safe only because every interpolated value has
 * already passed `ids.ts`'s whitelist, which admits no `"`, `$`, backslash or backtick.
 *
 * `select-window` targets `kp-<nonce>:<window>` and NOT `<owner>:<window>`, which is a correction
 * to the shape CONTRACT.md originally pinned. Measured on tmux 3.3a: targeting the owner session
 * moved the OWNER's current window (the operator attached on the box gets yanked to another window)
 * and left the viewer session on whatever window the group started on — so the tile streamed the
 * wrong window's bytes. Targeting the viewer session leaves the owner untouched and selects
 * correctly, which is the entire point of a grouped session.
 */
/**
 * The `TERM` the attach runs under, and why it is SET rather than inherited.
 *
 * `tmux attach` refuses a terminal that cannot clear: with `TERM` unset or `dumb` it exits with
 * `open terminal failed: terminal does not support clear` and the tile streams that sentence instead
 * of a pane. Nothing inherits a usable value here — the streamer is a forked child of a container
 * process with no `TERM`, and for `fleet` the command crosses `ssh` WITHOUT `-t`, which sets no
 * `TERM` on the far side either. CI found this before production did: the real-tmux specs pass on a
 * developer box, which has a `TERM`, and failed on a runner, which does not.
 *
 * `xterm` because it is in `ncurses-base`, so it exists wherever tmux was installed — unlike
 * `xterm-256color`, which lives in the optional `ncurses-term`. This value only has to satisfy the
 * CLIENT's capability check; the colours a pane renders come from tmux's own `default-terminal`,
 * not from here. Overridable, and whitelisted because it is interpolated into a shell command.
 */
export function panelTerm(): string {
  const raw = process.env.KONTRA_PANEL_TERM;
  return raw && /^[A-Za-z0-9._-]{1,32}$/.test(raw) ? raw : 'xterm';
}

export function attachCommand(spec: AttachTarget & { viewer: string }): string {
  assertSafe('session', spec.session);
  assertSafe('window', spec.window);
  const viewer = assertViewerSession(spec.viewer);
  const rows = assertDimension('rows', spec.rows);
  const cols = assertDimension('cols', spec.cols);
  const inner =
    `export TERM=${panelTerm()}; ` +
    `stty rows ${rows} cols ${cols}; ` +
    `exec tmux new-session -A -d -s "${viewer}" -t "${spec.session}" ` +
    // ONE STATUS LINE PER TILE, AND IT IS OURS. Measured on this box: an attached client draws
    // tmux's own green bar into the byte stream (`^[[30m^[[42m [kp-…:actor* 2:handler# … "main-
    // droplet" 17:41`), and a snapshot NEVER carries it — the status line belongs to the client, and
    // `capture-pane` prints the pane. So a live tile and a snapshot tile disagreed about what chrome
    // they had, and the wall's own bar would have sat under a second, differently-worded one on
    // whichever tiles happened to be live.
    //
    // Off on the VIEWER SESSION ONLY, which is this streamer's own (`kp-…`) and nobody else's:
    // verified on tmux 3.3a that `set-option -t <viewer> status off` leaves the owner session's
    // `status` unset and an operator attached on the box keeps their bar. It also hands the tile back
    // the row the bar was using. The browser's bar says more than tmux's can — how old the frame is,
    // and whether the process in the pane is still running.
    `\\; set-option -t "${viewer}" status off ` +
    `\\; select-window -t "${viewer}:${spec.window}" ` +
    `\\; attach-session -r -t "${viewer}"`;
  return `script -q -c '${inner}' /dev/null`;
}

// --- the streaming seam ------------------------------------------------------------------------

/**
 * The seam, so no test dials a Machine.
 *
 * A {@link SshRunner} is one-shot and buffers; a live attach is neither, which is why this is a
 * second interface and not a second implementation. Both live in `transport.ts` since slice 6 — one
 * mode's transport is exactly this pair — and both are re-exported here under the names slice 2 gave
 * them, because the tests that prove the read-only boundary import them from this file.
 */
export type PtyRunner = PtyOpener;
export type { PtyChannel, PtyEvents };

/** Stderr kept from a failing attach. Enough for the first useful line, bounded because it is
 * attacker-adjacent output held in memory per tile. */
const MAX_STDERR_BYTES = 8 * 1024;

/** How long a `SIGTERM`'d ssh gets before it is killed outright. */
const TERM_GRACE_MS = 2000;

/**
 * The real one — for all three modes, from ONE spawn.
 *
 * The mode decides only the argv: `ssh … '<cmd>'`, `docker exec <c> sh -c '<cmd>'`, or `sh -c
 * '<cmd>'` (`modes.ts`'s `execArgv`). Everything else about a live attach is mode-independent,
 * including the two properties this function exists to hold, so there is deliberately one place that
 * creates a PTY child rather than three:
 *
 *  - **the first descriptor is `'ignore'`**, so the child gets `/dev/null` and `child.stdin` is
 *    `null`. Not `'pipe'` and not inherited: there must be no descriptor a byte could reach a session
 *    through, in any mode, even by mistake. This is asserted against this file's SOURCE by
 *    `attach.test.ts`, and one spawn site is what keeps that assertion meaningful;
 *  - **every stream gets an `'error'` listener.** ADR 0020's finding (6) measured an `EventEmitter`
 *    `'error'` with no listener failing an in-flight `pulumi up` from elsewhere in the same process;
 *    that is why the streamer is a separate PID, and it is still not licence to drop one here.
 *
 * For `fleet` it reuses `sshArgs()`, so the attach rides the SAME `ControlMaster` the snapshot and
 * probe execs use — one TCP connection and one `sshd` session per Machine rather than one per tile.
 * The master already exists by the time this runs, because {@link Attachment} reads the backlog
 * through the runner first, and that is also what creates the socket directory.
 */
export const realPtyRunner: PtyRunner = {
  open(target, remote, events): PtyChannel {
    const { file, args } = execArgv(target, remote);
    const child = spawn(file, args, { stdio: ['ignore', 'pipe', 'pipe'] });
    let stderr = '';
    let grace: NodeJS.Timeout | undefined;

    child.stdout.on('data', (chunk: Buffer) => events.onData(chunk));
    child.stderr.on('data', (chunk: Buffer) => {
      if (stderr.length < MAX_STDERR_BYTES) stderr += chunk.toString('utf8');
    });
    // An EPIPE on a killed child arrives as a stream 'error'; unheard, that is a process-level
    // throw. The exit handler below is what reports the outcome.
    child.stdout.on('error', () => undefined);
    child.stderr.on('error', () => undefined);
    child.on('error', (err: Error) => events.onSpawnError(err));
    child.on('close', (code, signal) => {
      if (grace) clearTimeout(grace);
      events.onExit(code, signal, stderr);
    });

    return {
      noWritableInput: child.stdin === null,
      kill(): void {
        if (child.exitCode !== null || child.signalCode !== null) return;
        child.kill('SIGTERM');
        // A wedged `ssh` (a Machine that stopped answering keepalives) would otherwise hold a tmux
        // client and an sshd session open until ServerAlive gives up.
        grace = setTimeout(() => child.kill('SIGKILL'), TERM_GRACE_MS);
        grace.unref();
      },
    };
  },
};

// --- one attachment ---------------------------------------------------------------------------

/** Where an attachment's bytes and its transitions go. The streamer implements this against one
 * client's subscription; a test implements it against an array. */
export interface AttachSink {
  /** Terminal bytes for `xterm.write()`. A payload prefixed with `\x1b[H\x1b[2J` is a full screen
   * and repaints; everything else appends. */
  bytes(payload: Buffer): void;
  /** Output dropped by the byte cap. Never silent: a tile that skips output is a tile that lies. */
  elided(bytes: number): void;
  /** The PTY is up — `mode:'live'`. */
  live(): void;
  /** It could not come up, and this is the sentence to say — `mode:'error'`. */
  failed(message: string): void;
  /** It was up and has ended — back to `mode:'snapshot'`. */
  ended(message: string): void;
}

export interface AttachDeps {
  /** For the backlog read and the cleanup kill. Reused, so the ControlMaster is shared. */
  ssh: SshRunner;
  pty: PtyRunner;
  now?(): number;
  log?(line: string, extra?: Record<string, unknown>): void;
}

export type AttachState = 'starting' | 'live' | 'error' | 'closed';

/**
 * One live attach, as the streamer sees it.
 *
 * An interface rather than the class, for the same reason every other seam in this directory is one:
 * `server.ts`'s focus/blur bookkeeping has to be provable without a child process. {@link Attachment}
 * is the implementation, and it is what the tests point at their own fake `ssh`/PTY.
 */
export interface LiveAttach {
  /** The per-viewer session on the Machine, so a log line can name what leaked. */
  readonly viewer: string;
  readonly mode: AttachState;
  start(): Promise<void>;
  stop(): Promise<void>;
  /** What the tile currently shows, as a repaint. */
  replay(): Buffer;
}

function firstLine(s: string): string {
  return s.trim().split('\n')[0]?.trim() ?? '';
}

/** A full screen: the clear-home prefix is what makes a payload a repaint rather than 50 more
 * lines appended to the tile forever. */
function repaint(payload: Buffer): Buffer {
  return Buffer.concat([Buffer.from(CLEAR_HOME, 'utf8'), payload]);
}

/**
 * One focused Terminal's live attach: one PTY, one grouped session, one client.
 *
 * Per viewer, deliberately — two browsers focusing the same Terminal get two grouped sessions and
 * two independent selected windows, which is ADR 0020's acceptance criterion 7 and the reason
 * grouped sessions were chosen over a shared client.
 */
export class Attachment implements LiveAttach {
  /** The per-viewer session this attachment owns on the Machine, and is responsible for killing. */
  readonly viewer: string;

  private state: AttachState = 'starting';
  private readonly ring = new Ring();
  private readonly budget: FrameBudget;
  private channel: PtyChannel | undefined;
  private flushTimer: NodeJS.Timeout | undefined;
  /** True once the attach command has been launched, i.e. from the moment a grouped session might
   * exist on the Machine. Set BEFORE the spawn: a session we are unsure about must still be killed. */
  private launched = false;
  /** The one shutdown, shared by every caller. Truthy from the first `stop()`, which is also what
   * tells a `start()` still in flight to give up. */
  private stopping: Promise<void> | undefined;

  constructor(
    private readonly deps: AttachDeps,
    readonly spec: AttachTarget,
    private readonly sink: AttachSink,
    viewer?: string
  ) {
    this.viewer = viewer ? assertViewerSession(viewer) : newViewerSession();
    this.budget = new FrameBudget(deps.now);
  }

  get mode(): AttachState {
    return this.state;
  }

  /** What this tile currently shows, as a repaint. Bounded by the Ring at 1 MiB — a tile that
   * re-focuses is re-seeded from this instead of churning a second grouped session on the Machine. */
  replay(): Buffer {
    return repaint(this.ring.concat());
  }

  /**
   * Seed, then go live. Never rejects and never hangs: every failure becomes
   * {@link AttachSink.failed} with a sentence, because a tile stuck on "connecting" is the one
   * outcome an operator cannot act on.
   */
  async start(): Promise<void> {
    try {
      if (!(await this.seed())) return;
      if (this.stopping) return;
      this.launch();
    } catch (err) {
      // Includes a validation throw: a hostile window name reaches here, not a shell.
      this.fail(`the live attach to ${this.spec.target.machine} could not start: ${String(err)}`);
    }
  }

  /**
   * Read the backlog over the shared connection.
   *
   * It doubles as the reachability test, which is why it runs BEFORE the PTY: a Machine that cannot
   * answer a `capture-pane` has nothing to attach to, and finding that out with a bounded exec is
   * how a focus on an unreachable Machine becomes `mode:'error'` in ten seconds instead of a
   * spinner. It is also what establishes the ControlMaster the attach then rides.
   */
  private async seed(): Promise<boolean> {
    const { target, session, window } = this.spec;
    const res = await this.deps.ssh.run(target, backlogCommand(session, window), {
      timeoutMs: BACKLOG_TIMEOUT_MS,
    });
    if (this.stopping) return false;
    if (res.timedOut) {
      this.fail(
        `${target.machine} did not answer within ${BACKLOG_TIMEOUT_MS} ms — it is unreachable or ` +
          'wedged, so there is nothing to attach to'
      );
      return false;
    }
    if (res.code !== 0) {
      const why = firstLine(res.stderr);
      this.fail(
        `could not read ${session}:${window} on ${target.machine} (ssh exited ${res.code})` +
          `${why ? `: ${why}` : ''} — converge the session, or check that the window still exists`
      );
      return false;
    }
    const backlog = Buffer.from(res.stdout, 'utf8');
    // Through the Ring on the way out: a `history-limit` an operator raised by hand must not be
    // able to hand a tile more than the per-Terminal cap.
    this.ring.reset(backlog);
    this.tell(() => this.sink.bytes(this.replay()));
    return true;
  }

  private launch(): void {
    const remote = attachCommand({ ...this.spec, viewer: this.viewer });
    this.launched = true;
    this.channel = this.deps.pty.open(this.spec.target, remote, {
      onData: (chunk) => this.onData(chunk),
      onSpawnError: (err) =>
        this.fail(
          `${MODE_BINARY[modeOf(this.spec.target)]} could not be spawned for the live attach: ` +
            err.message
        ),
      onExit: (code, signal, stderr) => this.onExit(code, signal, stderr),
    });
    this.state = 'live';
    this.flushTimer = setInterval(() => this.onFlush(), FLUSH_MS);
    // Unref'd: a forgotten interval must not be the reason this process cannot exit.
    this.flushTimer.unref();
    this.tell(() => this.sink.live());
  }

  private onData(chunk: Buffer): void {
    if (this.state !== 'live') return;
    this.ring.push(chunk);
    this.emit(this.budget.offer(chunk));
  }

  /** Release what the budget held. Without this, the tail of a burst waits for the next byte. */
  private onFlush(): void {
    if (this.state !== 'live') return;
    this.emit(this.budget.drain());
  }

  private emit(admission: Admission): void {
    this.tell(() => {
      if (admission.send) this.sink.bytes(admission.send);
      const elided = this.budget.takeElided();
      if (elided > 0) this.sink.elided(elided);
    });
  }

  private onExit(code: number | null, signal: NodeJS.Signals | null, stderr: string): void {
    if (this.state === 'closed' || this.state === 'error') return;
    const wasLive = this.state === 'live';
    this.state = 'closed';
    this.clearFlush();
    // The last screen of a Worker that just died is the most interesting one, so what the budget
    // still holds is released before the transition is announced.
    this.emit(this.budget.drain());
    const why = signal ? `signal ${signal}` : `exit ${code ?? 'unknown'}`;
    const detail = firstLine(stderr);
    const message =
      `the live attach to ${this.spec.target.machine} ended (${why})` +
      `${detail ? `: ${detail}` : ''}`;
    this.tell(() =>
      wasLive ? this.sink.ended(`${message} — back to snapshots`) : this.sink.failed(message)
    );
    this.cleanUp('the attach exited');
  }

  private fail(message: string): void {
    if (this.state === 'error' || this.state === 'closed') return;
    this.state = 'error';
    this.clearFlush();
    this.tell(() => this.sink.failed(message));
    this.cleanUp('the attach failed');
  }

  /**
   * Kill the PTY and the grouped session it created. Idempotent, and never rejects.
   *
   * A leaked `kp-*` session is a leak of the operator's own making: measured on tmux 3.3a, a viewer
   * session outlives its client, and while it exists the owner's windows survive `kill-session` on
   * the owner. So this runs on blur, on disconnect, on the attach exiting, and on shutdown.
   */
  async stop(): Promise<void> {
    // Every caller awaits the SAME shutdown rather than the second one returning early: a blur
    // followed immediately by a SIGTERM would otherwise let the process exit while the kill that the
    // blur started was still in flight, which is exactly the leak this method exists to prevent.
    this.stopping ??= this.shutdown();
    return this.stopping;
  }

  private async shutdown(): Promise<void> {
    this.clearFlush();
    if (this.state === 'live' || this.state === 'starting') this.state = 'closed';
    this.channel?.kill();
    this.channel = undefined;
    if (!this.launched) return; // no attach was ever launched, so no session can exist
    this.launched = false;
    await this.killViewer();
  }

  private cleanUp(why: string): void {
    // Not awaited — this runs from an event handler — but never floating: `stop()` swallows its own
    // failures and this catch is the belt to that braces.
    void this.stop().catch((err) => this.log('cleanup failed', { why, err: String(err) }));
  }

  private async killViewer(): Promise<void> {
    try {
      const res = await this.deps.ssh.run(this.spec.target, killViewerCommand(this.viewer), {
        timeoutMs: KILL_TIMEOUT_MS,
      });
      if (res.timedOut || res.code !== 0) {
        // Worth a line each time: this is the sentence that explains a `kp-*` session an operator
        // later finds on a Machine.
        this.log('a per-viewer tmux session may have leaked', {
          machine: this.spec.target.machine,
          viewer: this.viewer,
          code: res.code,
          timedOut: res.timedOut,
          stderr: firstLine(res.stderr),
        });
      }
    } catch (err) {
      this.log('a per-viewer tmux session may have leaked', {
        machine: this.spec.target.machine,
        viewer: this.viewer,
        err: String(err),
      });
    }
  }

  private clearFlush(): void {
    if (this.flushTimer) clearInterval(this.flushTimer);
    this.flushTimer = undefined;
  }

  /** Call the sink without letting it take the process down: a socket that fails mid-write must
   * cost one tile, not the streamer. */
  private tell(what: () => void): void {
    try {
      what();
    } catch (err) {
      this.log('a Dashboard client rejected a live frame', {
        machine: this.spec.target.machine,
        err: String(err),
      });
    }
  }

  private log(line: string, extra?: Record<string, unknown>): void {
    this.deps.log?.(line, extra);
  }
}

/** The factory the streamer holds. An interface so `server.ts` can be tested without a Machine and
 * without a child process. */
export interface Attacher {
  open(spec: AttachTarget, sink: AttachSink, viewer?: string): LiveAttach;
}

export function attacher(deps: AttachDeps): Attacher {
  return {
    open: (spec, sink, viewer) => new Attachment(deps, spec, sink, viewer),
  };
}

/** The production wiring: the mode-dispatching runner for the backlog and the cleanup, a real PTY
 * for the stream. Both resolve the transport from the target, so ONE attacher serves all three modes
 * and `server.ts` never learns that there is more than one. */
export function realAttacher(log?: (line: string, extra?: Record<string, unknown>) => void): Attacher {
  const deps: AttachDeps = { ssh: panelRunner, pty: realPtyRunner };
  if (log) deps.log = log;
  return attacher(deps);
}
