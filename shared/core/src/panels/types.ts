/**
 * The Dashboard's wire contract — what the browser is told, and what it may say (ADR 0020).
 *
 * Two rules run through every type here.
 *
 * **`unknown` is a value, never a shrug.** Each of the four health signals is independent and
 * carries its own words; none of them collapses into one light. The case this exists for is the
 * round-3 incident — 81 of 82 resource loads failing on one Machine while the run reported
 * `completed` — and `heartbeat.ts` already establishes the rule it follows: "unknown" and "zero"
 * must stay distinguishable. A tile must never render `unknown` as healthy.
 *
 * **Nothing a client can say carries bytes.** `subscribe`, `unsubscribe`, `focus`, `blur`,
 * `converge`, `ping`. There is no message type with a payload bound for a session, because
 * ADR 0020's finding (3) measured that `tmux attach -r` does not stop `run-shell` — so read-only
 * has to be a property of this list, not of tmux.
 */

import type { ExecutionMode } from './ids';

export interface TerminalHealth {
  /** Did the last `list-panes` probe reach the Machine over SSH? */
  reachable: 'ok' | 'fail' | 'unknown';
  /** Does the session exist on it — and does the Machine even have tmux? */
  session: 'present' | 'absent' | 'no-tmux' | 'unknown';
  /**
   * Is anything still RUNNING in the pane? The fifth signal, and it is not the second one again.
   *
   * A session can be perfectly present while the Worker inside it has exited — `cli/internal/tmux/tmux.go`'s hold
   * keeps the window open on purpose so the exit status stays readable, so "finished" looks exactly
   * like "running" from every other angle: the session is there, the screen is full of output, the
   * tile paints. That is the case this signal exists for, and it is the one that bit us.
   *
   * `unknown` IS COMMON HERE AND IT IS HONEST. `pane_current_command` reports the hold SHELL whether
   * the Worker is running or finished (measured — see `tmux.ts:paneProcess`), so a shell is not a
   * reading. `exited` is only ever claimed from `pane_dead`, from the `@kontra_exit` the hold shell
   * writes, or from the exit banner it prints on screen.
   */
  process: 'running' | 'exited' | 'unknown';
  /** Temporal pollers on the actor's shared queue. `none` = registered, nothing polling. */
  poller: 'live' | 'none' | 'unknown';
  /**
   * Is this Worker running and not working?
   *
   * TWO SOURCES, ONE CHIP. Where the **Machine's Warden** reports (ADR 0037), this is its verdict:
   * the ratio of failed to attempted `@actor.load`s over a window the Warden owns, from counters
   * both actor hosts increment (`cli/warden/sickworker.go`). Otherwise it is the VictoriaMetrics reading
   * `metrics.ts` computes — which that file records at length as unable to reach `ok` today, since
   * `kontra_batches_total` is incremented nowhere.
   *
   * `unknown` NEVER MEANS `ok` FROM EITHER SOURCE. The Warden has six separate reasons for not
   * knowing and every one of them lands here as `unknown` with its sentence, because a green chip
   * derived from a measurement nobody took is the same lie one layer down as a Run reporting
   * `completed` while it lost an eighth of a sweep.
   */
  loads: 'ok' | 'failing' | 'unknown';
  /** One human sentence per failing signal. Never a collapsed summary. */
  detail?: string;
}

export interface Terminal {
  /** `<mode>:<node>/<session>/<window>`. Opaque to the frontend; a Dashboard slot pins it. */
  id: string;
  /**
   * The EXECUTION MODE: where this Worker runs, and therefore what a crashed tmux server costs.
   *
   * Redundant with the id's first segment on purpose — a client must be able to select and label by
   * mode without parsing ids, and `kontra panels list` decodes an older streamer's payload by
   * falling back to the prefix. Added, never renamed: slice 5's Go struct decodes the shape slice 1
   * shipped, and a rename would break it silently.
   *
   * IT IS A DIFFERENT AXIS FROM `{t:'state'}`'s `mode`, which is `snapshot|live|error` — the wire
   * carries both words, on different messages. This one is where a Worker runs; that one is how a
   * tile is being fed.
   *
   * THE STAKES DIFFER BY VALUE, and the UI is expected to say so (ADR 0020, finding 2): a `fleet`
   * pane holds `journalctl -fu`, so crashing that tmux server costs the view; a `local` pane holds
   * the REAL actor and handler processes, so there it costs a running Worker.
   */
  mode: ExecutionMode;
  /** The node: a Machine (`fleet`), a container (`docker`), this host (`local`). */
  machine: string;
  host: string;
  publicIp: string;
  /** The fleet's label. A DigitalOcean tag and an inventory group; nothing dispatches on it. */
  tag: string;
  /** Which fleet — the stack name, `<actor>-<version>`. Was `run`, which named a bounded
   *  period of work that nothing in kontra ever measured. */
  fleet: string;
  /** '' when the stack carries no placement — a Machine with no actor is still a visible tile. */
  actor: string;
  version: string;
  window: string;
  /**
   * `pane_current_command` — what tmux says is in the pane's foreground.
   *
   * REPORTED, NOT INTERPRETED. It is the honest answer to "what is in this pane" and it is NOT the
   * answer to "is the Worker alive": kontra's hold shell keeps the pane's foreground process group
   * as the shell, so a running Go Worker reports `zsh`. `health.process` is the interpretation, and
   * it says `unknown` exactly when this field cannot settle the question.
   */
  command: string;
  /**
   * The PANE's own geometry, from `list-panes`.
   *
   * Not the browser tile's measurement — those are two different numbers, and since sessions are
   * pinned with `window-size manual` (at 120x40; see `paneCols`/`paneRows` in `cli/internal/tmux/tmux.go`, which
   * is the authority for the value) they no longer track each other at all. A tile that showed one
   * labelled as the other would be claiming the operator's pane is whatever shape the browser
   * happens to be.
   */
  paneCols: number;
  paneRows: number;
  /** The exit status of the command that WAS in this pane, when the pane reported one ('' otherwise).
   *  From `@kontra_exit`, or from the banner the hold shell prints on screen. */
  exitStatus: string;
  health: TerminalHealth;
  lastSnapshotAt?: number;
  /**
   * THE MACHINE'S OWN NUMBERS, when a **Warden** is reporting them (ADR 0037).
   *
   * THIS FIELD IS WHY TELEMETRY DOES NOT RIDE WORKFLOW HISTORY. 0037: "CPU, memory, load ratios and
   * tmux frames go over the existing pane-snapshot path. A 200×50 terminal frame through Temporal
   * history is the shape this repo already measured as 86% of a workflow's events, for a value that
   * is stale a second later." So it is here — on a payload that is re-fetched, overwritten and
   * thrown away — and not in a durable event log.
   *
   * NOT IN `health.detail`, and that is the contract above being kept rather than a preference:
   * `detail` is one sentence per FAILING signal, and CPU at 4% is not a signal. It is context for
   * the ones that are, so it is a field a tile can render as a number.
   *
   * ABSENT MEANS UNMEASURED, at every level. No Warden, an older Warden, a platform with no /proc,
   * or one reading that failed — all of them omit the field rather than sending a zero, because a
   * Machine nobody measured is not a Machine that is idle.
   */
  telemetry?: MachineTelemetry;
}

/** See {@link Terminal.telemetry}. Every field optional; absent means unmeasured, never zero. */
export interface MachineTelemetry {
  /** Fraction of the last interval not idle, across all cores. 0..1. iowait counts as idle. */
  cpu?: number;
  /** used/total, from MemAvailable rather than MemFree — see `cli/warden/panereport.go` for why. 0..1. */
  memory?: number;
  /** The kernel's own one-minute run-queue average. Beside `cpu`, never instead of it: a Machine at
   *  100% CPU with a load of 1 is working, and the same CPU at a load of 40 is thrashing. */
  load1?: number;
}

/** `GET /api/panels/health`. Ungated when the streamer is configured; 503 when it is not. */
export interface PanelsHealth {
  ok: boolean;
  terminals: number;
  /** Terminals currently subscribed by at least one client. */
  live: number;
  machines: number;
}

// --- client -> server ------------------------------------------------------------------------

export type ClientMessage =
  | { t: 'subscribe'; id: string; cols?: number; rows?: number }
  | { t: 'unsubscribe'; id: string }
  | { t: 'focus'; id: string; cols?: number; rows?: number }
  | { t: 'blur'; id: string }
  | { t: 'converge'; id: string }
  | { t: 'ping' };

// --- server -> client (text) -----------------------------------------------------------------

export type ServerMessage =
  | { t: 'hello'; terminals: number }
  | { t: 'state'; id: string; mode: 'snapshot' | 'live' | 'error'; health: TerminalHealth }
  | { t: 'error'; id?: string; message: string }
  | { t: 'elided'; id: string; bytes: number }
  | { t: 'pong' };

/** Health with nothing measured yet. Not "healthy until proven otherwise" — the tile renders it
 * as unknown, which is a visible state with its own words. */
export const UNKNOWN_HEALTH: TerminalHealth = {
  reachable: 'unknown',
  session: 'unknown',
  process: 'unknown',
  poller: 'unknown',
  loads: 'unknown',
};
