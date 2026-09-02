/**
 * The remote READ commands — discovery and snapshots (ADR 0020).
 *
 * Two shapes, and both are exactly as narrow as the read path needs:
 *
 *   discover  `tmux list-panes -a` — every pane on the Machine in one exec, which is where
 *             `health.reachable` and `health.session` come from. A Machine that answers with an
 *             empty list is reachable with no sessions; a Machine whose tmux is missing says so
 *             in its exit code, and those are three different tiles, never one grey one.
 *
 *   snapshot  `tmux capture-pane -p -e -S -50` — a full screen, per window, batched into ONE exec
 *             per Machine. Twelve Machines cost twelve periodic execs rather than twenty-four
 *             persistent PTYs and twenty-four `sshd` sessions on 2 GB boxes (ADR 0020), and two
 *             browsers watching the same Machine still cost one.
 *
 * WHY CAPTURE AND NOT CONTROL MODE. Measured, not assumed: a stalled control-mode client made a
 * Machine's tmux server grow 10 MB → 80 MB in five seconds and then segfaulted it, taking every
 * session on the socket with it. `capture-pane` retains nothing on the Machine — it prints the
 * screen and exits.
 *
 * A snapshot is a SCREEN, not a log. It is preceded by {@link CLEAR_HOME} so a repaint replaces
 * the tile instead of appending to it, and the Manifest, the journal and the lake remain the
 * record.
 */

import { assertSafe } from './ids';

/** Lines of scrollback a snapshot carries above the visible screen. */
export const SNAPSHOT_LINES = 50;

/** Home + clear. A snapshot frame MUST be prefixed with this, or a 3-second repaint turns a
 * 50-line screen into an ever-growing tile. */
export const CLEAR_HOME = '\x1b[H\x1b[2J';

/**
 * The tmux USER OPTION that marks a session as kontra's, and says what kind it is.
 *
 * IT REPLACED A NAME PREFIX, and that is the point. Every session kontra created used to be called
 * `kontra-<something>` so that `local.ts` could find it with `startsWith`. The prefix was therefore
 * doing two jobs: namespacing, and discovery. It did the first badly — it is on every name an
 * operator types at a `tmux attach` — and the second only barely, because a NAME cannot say whether
 * a session holds an Actor's Worker or a caller's workflow, so `kontra-wf-` had to be invented to
 * keep the two from colliding.
 *
 * A user option carries both facts and costs nothing in the name. Values kontra writes:
 *
 *   actor:<name>:<version>     an Actor's Worker — `kontra serve --tmux`, or a fleet Machine
 *   workflow:<name>            a caller's workflow — `kontra workflow serve --tmux`
 *
 * Verified on tmux 3.3a: `tmux set-option -t <session> @kontra '…'` and `#{@kontra}` in a format
 * string. User options (`@name`) have been supported since tmux 3.0.
 *
 * READ, NEVER INTERPOLATED. The value reaches a `startsWith` and a label, never a command — so a
 * session an operator tagged by hand cannot become one.
 */
export const KONTRA_SESSION_OPTION = '@kontra';

/**
 * The tmux PANE option `cli/tmux.go`'s hold writes the wrapped command's exit status into.
 *
 * IT EXISTS BECAUSE `pane_current_command` CANNOT ANSWER THE QUESTION, and that was measured rather
 * than assumed — see {@link paneProcess}. A pane option is per pane (verified on tmux 3.3a: setting
 * it on one pane leaves its sibling's `#{@kontra_exit}` empty and puts nothing on the session), it
 * costs one field in the probe that already runs, and it is readable without a subscription, a
 * capture or a second exec.
 *
 * WRITTEN BY THE SHELL THAT SAW THE EXIT, so it carries the STATUS and not merely the fact. `cli/
 * tmux.go:tmuxHold` is the peer, and it writes this immediately before the banner it prints.
 */
export const KONTRA_EXIT_OPTION = '@kontra_exit';

/**
 * Every pane on the Machine, one per line:
 * `<session> <window> <pane_id> <cols>x<rows> <pane_dead> c:<command> x:<@kontra_exit> <@kontra>`.
 *
 * The tag is LAST because it is the one field kontra does not control the shape of — an operator
 * may have set `@kontra` on a session of their own, with anything in it. Last means a value with a
 * space in it can only corrupt itself, never shift the fields that address a pane or describe it.
 *
 * THE TWO NEW FIELDS CARRY A LITERAL PREFIX, and that is not decoration — it cost a real bug to
 * learn. `@kontra_exit` is empty on every pane whose command has not exited, so `… #{pane_dead}
 * #{pane_current_command} #{@kontra_exit} #{@kontra}` rendered TWO ADJACENT SPACES, `split(/\s+/)`
 * collapsed them, and the tag arrived one field to the left — which took every tagged session off
 * the wall. Caught by `local.tmux.test.ts` against a real tmux, which is exactly what that file is
 * for. `c:`/`x:` make both fields non-empty by construction, so a positional parse cannot slip.
 *
 * `pane_start_command` IS DELIBERATELY NOT HERE. It holds the whole hold wrapper, quotes, spaces and
 * all (`trap ':' INT; '/root/.venv/bin/python' '…'; kontra_status=$?; …`), so one field would shred
 * every field after it. `pane_current_command` is a process name.
 */
export const COMMAND_FIELD_PREFIX = 'c:';
export const EXIT_FIELD_PREFIX = 'x:';

/**
 * WHICH BOX ANSWERED, printed before the panes.
 *
 * THE BUG THIS EXISTS FOR WAS OBSERVED LIVE, and it is worse than a stale tile. A pane for
 * `kf-nscheck-01` — a **Machine** from a Fleet that no longer existed — was on the wall with a
 * recent `lastSnapshotAt` and a health block claiming `session: present`. The cause is two facts
 * meeting:
 *
 *   1. `discovery.ts:sshAddress` dials the PRIVATE VPC address by default, and DigitalOcean reuses
 *      `10.124.0.3`-`.12` for every Fleet in a VPC. So the streamer dialled a live Machine belonging
 *      to a DIFFERENT, current Fleet.
 *   2. A Machine's session name is `<actor>-<version>` (`discovery.ts:sessionNameFor`). Two Fleets
 *      running the same Artifact therefore have IDENTICALLY NAMED sessions, so the probe found the
 *      session it was looking for.
 *
 * The result is not a tile that is out of date. It is one Machine's screen, live, drawn under
 * another Machine's name, offering a converge that would run against the wrong box. Nothing in the
 * probe could catch it, because every field it read was consistent — an address is not an identity.
 *
 * So the probe prints one. `hostname` because that is what a **Machine** is named after: the Fleet
 * names droplets `kf-<tag>-NN` explicitly and DigitalOcean sets a droplet's hostname from its name,
 * which `metrics.ts:instanceKeyFor` already relies on for the same reason.
 *
 * IT IS PREFIXED AND IT IS FIRST. Prefixed so `parseListPanes` cannot mistake it for a pane (it has
 * two fields, and a pane row needs four with a `<n>x<m>` in the fourth — but a marker is a decision
 * rather than a coincidence). First because the EXIT CODE of this command must stay tmux's:
 * `looksLikeNoTmux` reads it, and a shell reports the LAST command's status.
 *
 * A BOX THAT PRINTS NO MARKER IS NOT A MISMATCH. `docker exec` into a scratch-based worker image has
 * no `hostname` binary, and a Machine placed by an older streamer answers the older command. Both
 * are "unattributed", which `interpretProbe` treats as unchecked rather than as wrong — the only
 * safe reading, since the alternative empties the wall on an upgrade.
 */
export const HOST_MARKER = 'KONTRA_HOST';

export const LIST_PANES_COMMAND =
  `hostname 2>/dev/null | sed 's/^/${HOST_MARKER} /' || true; ` +
  "tmux list-panes -a -F '#{session_name} #{window_name} #{pane_id} #{pane_width}x#{pane_height} " +
  `#{pane_dead} ${COMMAND_FIELD_PREFIX}#{pane_current_command} ` +
  `${EXIT_FIELD_PREFIX}#{${KONTRA_EXIT_OPTION}} #{${KONTRA_SESSION_OPTION}}'`;

/**
 * The name the box that answered calls itself, or `undefined` when it did not say.
 *
 * The LAST marker wins, for the same reason `parseConvergeReport` takes the last sentinel: a shell
 * profile that prints a banner is normal, and so is one that prints something marker-shaped.
 */
export function parseProbeHost(stdout: string): string | undefined {
  let found: string | undefined;
  for (const line of stdout.split('\n')) {
    const parts = line.trim().split(/\s+/);
    if (parts.length === 2 && parts[0] === HOST_MARKER && parts[1]) found = parts[1];
  }
  return found;
}

export interface PaneRow {
  session: string;
  window: string;
  paneId: string;
  cols: number;
  rows: number;
  /** `#{pane_dead}` — the pane's process exited and tmux is holding the window open
   *  (`remain-on-exit`). False for every pane kontra creates: `cli/tmux.go` holds with a SHELL
   *  instead, so that the exit status is printed rather than only flagged. */
  dead: boolean;
  /** `#{pane_current_command}`. What tmux believes is in the foreground of the pane's tty — read
   *  {@link paneProcess} before drawing any conclusion from it. */
  command: string;
  /** `@kontra_exit`, or '' — see {@link KONTRA_EXIT_OPTION}. */
  exitStatus: string;
  /** `@kontra`, or '' — see {@link KONTRA_SESSION_OPTION}. Empty for every session tmux was not
   *  told about, and for every pane listed by a kontra older than this field. */
  kontra: string;
}

/** Is this field `pane_dead`, or is it the first word of an older streamer's `@kontra` tag? A tag
 * is `actor:…`/`workflow:…`/whatever an operator typed; `pane_dead` is exactly `0` or `1`. So the
 * two layouts are told apart by the value, and a row from either shape parses correctly. */
function looksLikeDeadFlag(field: string | undefined): boolean {
  return field === '0' || field === '1';
}

/**
 * Parse `list-panes -a` output.
 *
 * Skips lines it cannot parse rather than throwing: this is a probe on a box we do not control,
 * and one odd line from an operator's hand-made session must not blank the whole Fleet's health.
 */
export function parseListPanes(stdout: string): PaneRow[] {
  const out: PaneRow[] = [];
  for (const line of stdout.split('\n')) {
    const parts = line.trim().split(/\s+/);
    if (parts.length < 4) continue;
    const [session, window, paneId, size] = parts as [string, string, string, string];
    const dims = /^(\d+)x(\d+)$/.exec(size);
    if (!dims) continue;
    // The pre-`pane_dead` layout put the tag straight after the geometry. Both are read, because a
    // mis-read here does not fail loudly — it puts a process name into the field discovery matches
    // sessions on, which would take a whole host off the wall. The prefixes are checked too, so a
    // line only takes the extended path if it really has the extended shape.
    const extended =
      looksLikeDeadFlag(parts[4]) &&
      (parts[5] ?? '').startsWith(COMMAND_FIELD_PREFIX) &&
      (parts[6] ?? '').startsWith(EXIT_FIELD_PREFIX);
    const tagFrom = extended ? 7 : 4;
    const exit = extended ? (parts[6] as string).slice(EXIT_FIELD_PREFIX.length) : '';
    out.push({
      session,
      window,
      paneId,
      cols: Number(dims[1]),
      rows: Number(dims[2]),
      dead: extended && parts[4] === '1',
      command: extended ? (parts[5] as string).slice(COMMAND_FIELD_PREFIX.length) : '',
      // Only digits: this reaches a tile as "exited 143", and it is written by a shell on a box we
      // do not control. Anything else is dropped rather than shown.
      exitStatus: /^\d{1,3}$/.test(exit) ? exit : '',
      // Everything after the fixed fields, rejoined: a hand-set value may contain spaces, and
      // reading only the first word of it would silently truncate what an operator wrote.
      kontra: parts.slice(tagFrom).join(' '),
    });
  }
  return out;
}

/**
 * The shells a pane sits at when nothing is running in it — and the reason this list is not the
 * detector it looks like.
 *
 * `login -sh` forms are included (`-zsh`, `-bash`): a login shell's `comm` keeps the leading dash.
 */
export const SHELL_COMMANDS = new Set([
  'sh',
  '-sh',
  'bash',
  '-bash',
  'zsh',
  '-zsh',
  'dash',
  '-dash',
  'ash',
  '-ash',
  'ksh',
  '-ksh',
  'fish',
  '-fish',
]);

/** What one pane's process is doing, as far as ONE `list-panes` can honestly say. */
export type PaneProcess = 'running' | 'exited' | 'unknown';

export interface PaneVerdict {
  process: PaneProcess;
  /** '' unless the pane reported one. */
  exitStatus: string;
  /** One sentence, for `health.detail`. Absent when the pane is plainly running. */
  detail?: string;
}

/**
 * Is anything running in this pane?
 *
 * **`pane_current_command == <a shell>` IS NOT EVIDENCE THAT THE WORKER DIED.** Measured on this
 * box, tmux 3.3a, and it is the whole reason this function is careful:
 *
 *     $ tmux list-panes -a -F '#{session_name}:#{window_name} #{pane_pid} #{pane_current_command}'
 *     nscheck-0_1_0:actor 3535711 zsh          # ← and yet:
 *     $ ps -o pid,stat,comm --ppid 3535711
 *     3535713 Sl+  nscheck                      # the Worker, running
 *
 * `cli/tmux.go:tmuxHold` runs the Worker from a shell (`<cmd>; kontra_status=$?; … read -r _`) so
 * that a crash leaves its output and its exit code ON SCREEN. That shell does not put the child in
 * its own process group, so the pane's foreground pgid stays the SHELL's — and tmux reports the
 * shell whether the Worker is running or finished. Every kontra session on this host reported `zsh`
 * while its Worker was up. A tile that read that as "the process exited" would call every healthy
 * local Worker dead, which is a louder lie than the silence it was meant to fix.
 *
 * So the three sound readings, and nothing else:
 *
 *   `dead`                 tmux held the window on exit (`remain-on-exit`) — it is over.
 *   `@kontra_exit` set     the hold shell recorded the status — it is over, and that is the code.
 *   a NON-shell command    something is genuinely in the foreground — it is running.
 *
 * A shell with neither flag is `unknown`, WITH THE REASON, and the screen is what settles it —
 * `paneExitFromScreen` reads the banner the same shell prints. `unknown` is a value here for
 * exactly the reason `heartbeat.ts` gives: it must stay distinguishable from both `ok` and `zero`.
 */
export function paneProcess(row: Pick<PaneRow, 'dead' | 'command' | 'exitStatus'>): PaneVerdict {
  const command = row.command ?? '';
  if (row.dead) {
    return {
      process: 'exited',
      exitStatus: row.exitStatus ?? '',
      detail:
        'the process in this pane has exited and tmux is holding the window open (`pane_dead`) — ' +
        'the last screen below is its final output, not a live one',
    };
  }
  if (row.exitStatus) {
    return {
      process: 'exited',
      exitStatus: row.exitStatus,
      detail:
        `the command in this pane exited with status ${row.exitStatus}; the window is being held ` +
        'open by the shell so its output stays readable (`cli/tmux.go`) — nothing is running here now',
    };
  }
  if (command !== '' && !SHELL_COMMANDS.has(command)) {
    return { process: 'running', exitStatus: '' };
  }
  return {
    process: 'unknown',
    exitStatus: '',
    detail:
      command === ''
        ? 'tmux reported no command for this pane, so whether anything is running here is unmeasured'
        : `this pane's foreground command is \`${command}\`, which is what kontra's hold shell ` +
          'reports whether the Worker is running or finished — so this is not a reading either way. ' +
          'The pane says which when it prints its exit banner; `kontra workers list` says now',
  };
}

/**
 * The exit status the hold shell PRINTS, read back off a screen — '' when it is not there.
 *
 * The second half of the answer {@link paneProcess} cannot give, and the half that works on a Worker
 * that was started before `@kontra_exit` existed. `cli/tmux.go` prints exactly this line and then
 * blocks on `read`, so once it is on screen nothing else can be printed after it — which is why only
 * the LAST non-empty line is looked at, and why a pane that prints this line and keeps going is not
 * mistaken for a finished one for longer than one snapshot.
 */
export const HOLD_BANNER = /^\[exited (\d{1,3})\] press any key to close this window$/;

export function paneExitFromScreen(screen: string): string {
  const lines = screen.split('\n');
  for (let i = lines.length - 1; i >= 0; i -= 1) {
    // `capture-pane` right-trims nothing but the escape sequences it was asked for, and a held pane
    // ends with blank rows plus whatever SGR reset the previous line left behind.
    const line = stripAnsi(lines[i] ?? '').trimEnd();
    if (line === '') continue;
    return HOLD_BANNER.exec(line)?.[1] ?? '';
  }
  return '';
}

/** Escape sequences out, text left. A snapshot is captured with `-e`, so every line may carry SGR
 * and cursor codes around the words. */
function stripAnsi(line: string): string {
  // eslint-disable-next-line no-control-regex
  return line.replace(/\x1b\[[0-9;?]*[ -/]*[@-~]/g, '');
}

/** What kind of session a tag names — the part before the first `:`. '' for an untagged session. */
export function kontraSessionKind(tag: string): string {
  return tag.split(':')[0] ?? '';
}

/** The tag kontra writes for one Actor's Worker. */
export function actorSessionTag(actor: string, version: string): string {
  return `actor:${actor}:${version}`;
}

/** The tag kontra writes for one caller workflow being served. */
export function workflowSessionTag(name: string): string {
  return `workflow:${name}`;
}

/**
 * A session name as tmux will actually store it.
 *
 * MEASURED, and it is the reason this function exists rather than a comment. tmux's
 * `session_check_name()` rewrites every `.` and `:` in a session name to `_`, silently, at creation:
 *
 *     $ tmux new-session -d -s 'nscheck-0.1.0' ; tmux list-sessions -F '#{session_name}'
 *     nscheck-0_1_0
 *
 * Which matters the moment a session is named `<actor>-<version>`, because every version has dots
 * in it. Two things break, and both are silent. `tmux attach -t nscheck-0.1.0` fails against a
 * session that is right there. And the probe compares the name it expects against the name
 * `list-panes` reports (`probe.ts`), finds no match, and renders a Machine whose Worker is running
 * perfectly as one with NO SESSION — which is the exact thing ADR 0020 says a tile may never say.
 *
 * So the name is sanitised where it is MINTED, not where it is read: what kontra prints, what it
 * looks for, and what tmux holds are then the same string. `cli/tmux.go:tmuxSafeName` is the peer.
 */
export function tmuxSafeName(name: string): string {
  return name.replace(/[.:]/g, '_');
}

/**
 * The tmux session an Actor's Worker runs in: `<actor>-<version>`, sanitised.
 *
 * THE ONLY TYPESCRIPT COPY, and it is here because it was two. `control/orchestrator/src/actorControl.ts`
 * minted the name when the Serve button started a worker and `frontend/src/panels/actorSession.ts`
 * derived it again in the browser — byte-identical, fallback and all, one calling
 * {@link tmuxSafeName} and the other re-inlining `.replace(/[.:]/g, '_')`. Two writers on the same
 * side of a language boundary is not a contract, it is a copy: the browser imports this through
 * `@core/panels/tmux`, the same alias it already uses for `@core/panels/pollers` and
 * `@core/panels/ids`. What crosses a real boundary — this and `cli/tmux.go:tmuxSession` — is held
 * by `shared/conformance/queues.json` §tmux_session, which every side executes.
 *
 * WHY THE BROWSER DERIVES IT AT ALL rather than being told. The serve call answers with the
 * session it just created, but only for a serve THIS visit performed. An Actor served an hour ago,
 * or from a terminal with `kontra serve --actor … --tmux`, has a worker and a pane too, and a
 * workbench that could only name sessions it had created itself would show an empty rectangle
 * beside a worker that is polling perfectly well.
 *
 * THE VERSION IS THE LOAD-BEARING HALF: `probe` alone names the ACTOR and not the BUILD, so two
 * versions served side by side would collide on one name and the pane shown beside `0.2.0`'s code
 * would be `0.1.0`'s worker.
 *
 * THE FALLBACK IS `actor`, and it differs from `sessionNameFor`'s `fleet` on purpose — that one
 * names a MACHINE, which still has Terminals when no Actor is placed on it. A session called `''`
 * or `'_'` cannot be attached to, and a pane looked up by it would match whatever else in the
 * inventory happens to have no name.
 */
export function actorSession(actor: string, version: string): string {
  const name = tmuxSafeName(version ? `${actor}-${version}` : actor);
  return name === '' || name === '_' ? 'actor' : name;
}

/** One window's snapshot, the shape ADR 0020 pins. */
export function capturePaneCommand(session: string, window: string, lines = SNAPSHOT_LINES): string {
  assertSafe('session', session);
  assertSafe('window', window);
  if (!Number.isInteger(lines) || lines < 0 || lines > 10_000) {
    throw new Error(`panels: snapshot lines=${String(lines)} is out of range`);
  }
  return `tmux capture-pane -p -e -S -${lines} -t '${session}:${window}'`;
}

/** Record separator between windows in a batched snapshot, and the separator between a window's
 * name and its bytes. ASCII RS/US: control characters tmux will not emit in a `-e` capture, so no
 * pane content can forge a record boundary. */
export const SNAPSHOT_RS = '\x1e';
export const SNAPSHOT_US = '\x1f';

/**
 * Every subscribed window of one session in ONE remote exec.
 *
 * This is the batching ADR 0020 asks for ("one exec per Machine paints every Terminal on that
 * Machine at once"); the per-window command inside it is unchanged. `|| true` per window so one
 * killed window does not cost the whole Machine its snapshot — the missing record is what makes
 * that window's tile report "no session" instead of going quiet.
 */
export function batchSnapshotCommand(
  session: string,
  windows: readonly string[],
  lines = SNAPSHOT_LINES
): string {
  assertSafe('session', session);
  if (!windows.length) throw new Error('panels: a snapshot needs at least one window');
  return windows
    .map((w) => {
      assertSafe('window', w);
      // \036 / \037 are RS / US as `printf` writes them; the name is already whitelisted.
      return `printf '\\036%s\\037' '${w}'; ${capturePaneCommand(session, w, lines)} || true`;
    })
    .join('; ');
}

/** Split a batched snapshot back into `window -> screen bytes`. */
export function parseBatchSnapshot(stdout: string): Map<string, string> {
  const out = new Map<string, string>();
  for (const record of stdout.split(SNAPSHOT_RS)) {
    if (!record) continue;
    const sep = record.indexOf(SNAPSHOT_US);
    if (sep < 0) continue;
    out.set(record.slice(0, sep), record.slice(sep + 1));
  }
  return out;
}

/** Does this stderr/exit pair mean the Machine has no tmux at all? That is a different tile from
 * "reachable, no session" — and a different action. */
export function looksLikeNoTmux(code: number, stderr: string): boolean {
  return code === 127 || /tmux: (command )?not found|command not found/i.test(stderr);
}

/**
 * Did SSH refuse because the host key at this address is not the one it learned?
 *
 * A SEPARATE TILE BECAUSE IT IS A SEPARATE CAUSE. Every other exit-255 means the Machine is gone,
 * rebooting, or unreachable — an operator's answer is to wait or to check the fleet. This one means
 * the Machine ANSWERED and we hung up on it, and no amount of waiting fixes it.
 *
 * It is worth naming because it is the one this deployment actually hit: `ssh.ts` explains the
 * mechanism (a VPC address recycled onto a new run's Machine) and now keys its known-hosts file
 * so it cannot recur. A key that changes after that scoping is the case TOFU exists for, and it
 * deserves a sentence rather than a raw exit code.
 */
export function looksLikeHostKeyChanged(code: number, stderr: string): boolean {
  if (code === 0) return false;
  return /REMOTE HOST IDENTIFICATION HAS CHANGED|Host key verification failed|host key for .* has changed/i.test(
    stderr
  );
}
