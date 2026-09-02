/**
 * The session converge script — the Fleet's claim on session existence (ADR 0020).
 *
 * `machine.ts` used to install a `kontra-tmux.service` unit with
 * `ConditionPathExists=/usr/bin/tmux`, next to an apt install the installer was allowed to fail.
 * A Machine could therefore deploy "successfully" and never be viewable, silently. This replaces
 * it: one idempotent script, one SSH round trip, run by a Temporal workflow whose id is the
 * Machine — so a Machine deployed WITHOUT `--tmux` can be given a Terminal on demand with no
 * re-deploy, and a failure is a failed workflow rather than a skipped unit.
 *
 * Three of the tmux options here are load-bearing rather than tidy:
 *
 *   - `history-limit 20000` — the default 2000 is too short to seed a Terminal with useful
 *     backlog when it is opened, and scrollback cannot be raised retroactively.
 *   - `window-size manual` — with tmux's default (`latest`), an operator who runs `tmux attach`
 *     on the box reflows every browser tile mid-stream. Uniform read-only tiles want a fixed
 *     geometry that no attach can move.
 *   - `resize-window -x 200 -y 50` — the fixed geometry itself. It is what makes a snapshot a
 *     rectangle of known size instead of whatever the last client happened to be.
 *
 * The script is a pure function of its input so it can be diffed, pinned by a test, and read by
 * whoever has to debug a Machine at 3am. Nothing here interpolates an unvalidated value: every
 * one passes {@link assertSafe} first, then lands inside single quotes.
 */

import { assertSafe } from './ids';

export interface SessionWindow {
  /** tmux window name — also the last segment of the Terminal id. */
  name: string;
  /** What the window runs. A journal follower; never the Worker itself (see below). */
  command: string;
}

/**
 * The default windows for a placed Machine.
 *
 * `journalctl -fu`, NOT the actor and handler processes themselves, and that is an invariant now
 * rather than a preference: ADR 0020's finding (2) measured a stalled viewer segfaulting a tmux
 * server, which destroys every session on the socket. Because fleet panes hold only journals, a
 * viewer that crashes tmux costs the view and nothing else — systemd still supervises the Worker,
 * and the **Warden** that judges it sick but not dead still restarts it (ADR 0037; the on-machine
 * watchdog this sentence used to name retired into it, and `cli/sickworker.go` records why it had
 * never actually counted the 81-of-82 ratio it was written for).
 */
export const DEFAULT_WINDOWS: readonly SessionWindow[] = [
  { name: 'actor', command: 'journalctl -fu kontra-actor.service' },
  { name: 'handler', command: 'journalctl -fu kontra-handler.service' },
] as const;

/** Scrollback per pane. Above tmux's 2000 default so a Terminal opens with real backlog. */
export const HISTORY_LIMIT = 20000;

/** The fixed geometry every fleet session is pinned to. Wide enough for a journal line. */
export const GEOMETRY = { cols: 200, rows: 50 } as const;

export interface ConvergeSpec {
  session: string;
  windows: readonly SessionWindow[];
  /** Overridable so a future Dashboard can pin a different tile size; the default is what every
   * tile is drawn at today. */
  cols?: number;
  rows?: number;
  historyLimit?: number;
}

/** The sentinel the script prints last. Machine-readable so the activity learns whether it
 * CREATED the session without a second round trip. */
export const REPORT_PREFIX = 'KONTRA_TMUX';

export function validateConvergeSpec(spec: ConvergeSpec): void {
  assertSafe('session', spec.session);
  if (!spec.windows.length) throw new Error('panels: a session needs at least one window');
  const seen = new Set<string>();
  for (const w of spec.windows) {
    assertSafe('window', w.name);
    assertSafe('command', w.command);
    if (seen.has(w.name)) throw new Error(`panels: duplicate window ${JSON.stringify(w.name)}`);
    seen.add(w.name);
  }
  for (const [k, v] of [
    ['cols', spec.cols],
    ['rows', spec.rows],
    ['historyLimit', spec.historyLimit],
  ] as const) {
    if (v === undefined) continue;
    if (!Number.isInteger(v) || v < 1 || v > 100_000) {
      throw new Error(`panels: ${k}=${String(v)} is out of range`);
    }
  }
}

/**
 * Render the converge. Idempotent throughout — it is run again on every converge of a Machine
 * that already has the session, and a half-created session is worse than no session.
 *
 * One script, one `ssh`: the whole point of doing this as a single activity is that session
 * existence costs one round trip and nothing polls.
 */
export function convergeScript(spec: ConvergeSpec): string {
  validateConvergeSpec(spec);
  const s = spec.session;
  const cols = spec.cols ?? GEOMETRY.cols;
  const rows = spec.rows ?? GEOMETRY.rows;
  const limit = spec.historyLimit ?? HISTORY_LIMIT;
  const [first, ...rest] = spec.windows as SessionWindow[];
  // validateConvergeSpec proved the list is non-empty.
  const w0 = first as SessionWindow;

  const extraWindows = rest
    .map(
      (w) =>
        `tmux list-windows -t '${s}' -F '#{window_name}' | grep -qx '${w.name}' || \\
  tmux new-window -d -t '${s}': -n '${w.name}' '${w.command}'`
    )
    .join('\n');

  const resizes = spec.windows
    .map((w) => `tmux resize-window -t '${s}:${w.name}' -x ${cols} -y ${rows}`)
    .join('\n');

  return `set -eu
# tmux is not part of the Machine image and machine.ts no longer apt-installs it (ADR 0020) —
# the session's owner installs what the session needs. DEBIAN_FRONTEND because an apt prompt
# inside a Temporal activity is not a prompt, it is a hang.
command -v tmux >/dev/null 2>&1 || {
  export DEBIAN_FRONTEND=noninteractive
  apt-get -o DPkg::Lock::Timeout=600 install -y --no-install-recommends tmux
}

# The session, then each remaining window, each guarded by its own existence check: a re-converge
# must not add a second 'actor' window, and an operator's extra window must survive.
created=0
tmux has-session -t '${s}' 2>/dev/null || {
  tmux new-session -d -s '${s}' -n '${w0.name}' '${w0.command}'
  created=1
}
${extraWindows ? `${extraWindows}\n` : ''}
# Scrollback first: it applies to panes created after it is set as well as before, and a Terminal
# opened later is seeded from it.
tmux set-option -t '${s}' history-limit ${limit}
# Fixed geometry, so an operator running \`tmux attach\` on the box cannot reflow every tile.
tmux set-option -t '${s}' window-size manual
${resizes}

printf '${REPORT_PREFIX} created=%s windows=%s\\n' "$created" \\
  "$(tmux list-windows -t '${s}' -F '#{window_name}' | tr '\\n' ',')"
`;
}

export interface ConvergeReport {
  /** True when this converge created the session, false when it was already there. */
  created: boolean;
  windows: string[];
}

/**
 * Read the sentinel line back.
 *
 * Deliberately tolerant of everything else on stdout: apt is chatty, tmux warns, and an
 * operator's `.bashrc` prints banners. The report is the LAST matching line, and its absence is
 * an error the activity surfaces rather than a silently empty result.
 */
export function parseConvergeReport(stdout: string): ConvergeReport {
  const line = stdout
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l.startsWith(`${REPORT_PREFIX} `))
    .pop();
  if (!line) {
    throw new Error(
      `panels: the converge printed no ${REPORT_PREFIX} report — it did not run to completion`
    );
  }
  const created = /\bcreated=1\b/.test(line);
  const match = /\bwindows=([^\s]*)/.exec(line);
  const windows = (match?.[1] ?? '')
    .split(',')
    .map((w) => w.trim())
    .filter(Boolean);
  return { created, windows };
}

/** `kill-session`, the `kill` signal's body. Bounded and idempotent: killing a session that is
 * already gone is a success, because the desired state is "absent". */
export function killScript(session: string): string {
  assertSafe('session', session);
  return `tmux kill-session -t '${session}' 2>/dev/null || true`;
}
