/**
 * The tile's status line — tmux's green bar, drawn by us, at the BOTTOM of EVERY tile.
 *
 * WHERE TMUX PUTS IT. "And put the green banner on the bottom, like tmux." It began as row 2 of the
 * tile header, which is the one place tmux never draws it, so the bar an operator was meant to
 * recognise did not read as one. It is now the last element in the tile, edge to edge, styled like
 * tmux's default `status-style bg=green,fg=black` — a solid band with dark text, in every theme,
 * because tmux's does not change with yours either.
 *
 * IT COST THE WALL NOTHING, and that was a constraint rather than a happy accident. `grid/wall.ts`'s
 * `WALL_ROW_PX` is MEASURED against the chrome a tile carries, so a bar ADDED at the bottom while the
 * header kept two rows would have taken a line of a Worker's output from every tile on the wall. The
 * header gave its second row back in the same change: this is one element moved between two siblings
 * of one flex column, so the chrome sums to what it summed to before and nothing was re-measured.
 *
 * GREEN IS NOT A HEALTH CLAIM, AND THIS IS WHERE THAT IS RESOLVED. tmux's bar is green whatever is
 * happening; ours sits on a tile whose chips are severity-ordered and can be red. A permanently green
 * band under a red `▲ poller: NONE` would be the wall contradicting itself in the loudest colour it
 * has. So the bar's tone is the worst of what it carries AND what the chips found: an exited process
 * or a dead stream is red, a stalled feed or ANY failing health signal is amber, and green means
 * "this is a current picture of a pane with nothing failing on it". The failing signal is named on
 * the bar in the chips' own words, because an amber band with no reason on it is the same complaint
 * in another colour — and it is `HealthChips.leadingFinding`, the literal first chip of the row, so
 * the two cannot pick different findings or different words for one.
 *
 * WHY IT IS OURS AND NOT TMUX'S. The bar an operator knows (`[session] 0:actor* 1:handler … "host"
 * 17:41`) is drawn by the tmux CLIENT, not stored in the pane — so `capture-pane`, which is what the
 * wall runs, can never contain it. Measured on this box: a snapshot carries pane content only, while
 * a live PTY attach carries the bar in its byte stream (`^[[30m^[[42m` — black on green). So the wall
 * had NO bar on most tiles and tmux's own on the few that were live, which is two different tiles
 * pretending to be one wall. The live attach now turns tmux's bar off for its own viewer session
 * (`panels/attach.ts`) and this component draws one for both, so a tile has exactly one status line
 * whichever way it is being fed.
 *
 * AND IT SAYS THINGS TMUX'S BAR CANNOT. tmux knows what session you are in; it does not know that
 * this picture is four seconds old, that the screen has not changed in five minutes, or that the
 * process the pane was running exited with 143. Those are the facts that separate "this Worker is
 * quiet" from "this tile stopped being fed" from "this Worker is over", and an operator acts
 * differently on each. Every cell here is something someone actually measured:
 *
 *   host          the inventory's, and which KIND of node it is — this box, a Machine, a container
 *   session:window the string an operator types into `tmux attach -t`
 *   command       `pane_current_command`, reported, never interpreted (see `health.process`)
 *   geometry      the PANE's own cols×rows, which since `window-size manual` is not the tile's
 *   feed + age    snapshot or live, and how old this picture is, measured in this browser
 *
 * NOTHING HERE IS INFERRED FROM WHAT IT WOULD BE NICE TO SAY. A field the streamer did not send
 * renders as `—` with a tooltip saying why, because a plausible-looking blank is how a wall starts
 * lying quietly.
 */

import { memo, useEffect, useState } from 'react';
import { ageWords } from './format';
import { modeStakes, type TileRef } from './tileRef';
import { leadingFinding, leadingUnmeasured, type Finding } from '../HealthChips';
import type { Terminal } from '../panelsClient';

/** How a tile is being fed — `{t:'state'}`'s `mode`. A different axis from the execution mode. */
export type Feed = 'snapshot' | 'live' | 'error';

/**
 * When a snapshot age stops being normal.
 *
 * The streamer's cadence is ~3 s, so 20 s is about six missed passes: long enough that a busy `ssh`
 * is not the explanation. Same number as the status bar's, and for the same reason — two thresholds
 * for one measurement is how a footer and a tile end up disagreeing about the same wall.
 */
export const STALE_FRAME_MS = 20_000;

/**
 * When an UNCHANGING screen is worth saying out loud.
 *
 * This is the "running but silent" fact, and it is a different one from a stale feed: frames keep
 * arriving on time, they are just identical. A journal follower on an idle Worker is legitimately
 * quiet, so this is stated and never coloured as a fault. A minute is above any normal log cadence
 * and below the point where an operator has already started wondering.
 */
export const QUIET_MS = 60_000;

/** What the tile knows about its own stream. A getter over refs, not props: frames arrive up to
 *  fifteen times a second and this component owns the once-a-second clock that reads them. */
export interface FrameClock {
  /** When a frame for this Terminal last ARRIVED, or null if none ever has. */
  lastFrameAt: number | null;
  /** When the painted screen last CHANGED, or null. Identical repaints do not move it — that is
   *  what makes "quiet" mean the Worker is silent rather than the feed being dead. */
  lastChangeAt: number | null;
}

export interface StatusLineProps {
  id: string;
  terminal: Terminal;
  ref_: TileRef;
  feed: Feed;
  /** Read once a second. See {@link FrameClock}. */
  frames(): FrameClock;
  /** What addon-fit measured for the TILE. Only ever shown as a contrast to the pane's own size. */
  tileCols: number;
  tileRows: number;
  /** The page's socket is down: the screen below is frozen and the age is not the pane's fault. */
  stale?: boolean;
  /** A tile too narrow for the whole line. Identity and liveness stay; the address, the actor and
   *  the geometry go — a bar that wraps costs the tile a row of output. */
  compact?: boolean;
  /** Injectable so a test does not have to wait a second. */
  tickMs?: number;
  /** Injectable clock, for the same reason. */
  now?(): number;
}

/** The bar's colour, from the worst thing it is currently carrying. Green is tmux's own default and
 * means "this is a working tile"; nothing else may be green. */
export type BarTone = 'ok' | 'warn' | 'bad' | 'unmeasured';

/**
 * THE WHOLE TILE'S WORST FACT, not just the feed's.
 *
 * `failing` is what stops the band contradicting the chips above it. tmux's own bar is green while
 * the Worker in the pane is on fire, because tmux has no idea; ours is on a tile that has already
 * decided, in severity order, that something is broken — so green here has to mean "and nothing on
 * this tile is failing" or it means nothing at all.
 *
 * A FAILING SIGNAL IS AMBER, NOT RED, and the split is deliberate. Red is reserved for the two facts
 * that say THIS SCREEN WILL NOT CHANGE AGAIN — the process exited, or the stream is dead — because
 * that is the one thing the bar knows and the chips do not. A failing `poller` is urgent and it is
 * still a live picture of a running pane, so it takes the same amber every other "what you are
 * looking at needs qualifying" state on this surface uses (the held-repaint badge, the stale badge,
 * the elided-bytes badge).
 */
export function barTone(input: {
  process: Terminal['health']['process'];
  feed: Feed;
  stale: boolean;
  frameAgeMs: number | null;
  /** Any applicable health signal reading `bad` — see `HealthChips.leadingFinding`. */
  failing?: boolean;
  /** Any applicable health signal that was never measured — `HealthChips.leadingUnmeasured`. */
  unmeasured?: boolean;
}): BarTone {
  // A finished process is the loudest fact a tile can carry, and it outranks a stale feed: the
  // screen is final either way, but only one of the two is about the Worker.
  if (input.process === 'exited') return 'bad';
  if (input.feed === 'error') return 'bad';
  if (input.stale) return 'warn';
  if (input.frameAgeMs !== null && input.frameAgeMs > STALE_FRAME_MS) return 'warn';
  if (input.failing === true) return 'warn';
  // NOT GREEN, and this is the whole reason the tone exists. ADR 0020 forbids `unknown` rendering
  // as healthy, and once the chip row left the tile this bar became the only health reading on the
  // wall — a pane with an uncollected `poller` or `loads` was showing solid tmux green, identical to
  // one where every signal had been checked. Ranked last because "not checked" is the mildest thing
  // that is still not "fine".
  if (input.unmeasured === true) return 'unmeasured';
  return 'ok';
}

/**
 * tmux's default `status-style bg=green,fg=black` — a solid band with DARK text on it.
 *
 * ONE PALETTE, NOT A THEMED PAIR. Every other coloured surface in this app has a `dark:` variant;
 * this one does not, because the thing being reproduced is a bar that looks the same on every box an
 * operator has ever attached to. A tmux status line does not follow the reader's app theme, and one
 * that faded in dark mode would stop being the object it is imitating.
 */
const TONE_CLASS: Record<BarTone, string> = {
  ok: 'bg-green-600 text-black',
  warn: 'bg-amber-500 text-black',
  bad: 'bg-red-500 text-black',
  // Slate, not a paler green and not amber. It has to be unmistakably NOT the healthy band — the
  // whole failure was an unmeasured pane passing for a checked one — while staying quieter than the
  // amber a real finding earns. tmux itself greys a window it has no current information about.
  unmeasured: 'bg-slate-500 text-white',
};

/** What the node IS, in one sentence — the first thing the operator asked for, and the one tmux's
 * own bar answers with a bare hostname. */
export function hostSentence(terminal: Terminal, ref: TileRef): string {
  const name = terminal.host || terminal.machine || ref.node;
  switch (ref.mode) {
    case 'local':
      return `${name} — THIS host: the box the streamer runs on, reached with no transport at all`;
    case 'docker':
      return `${name} — a worker container on this host, reached with \`docker exec\``;
    case 'fleet':
      return `${name} — a fleet Machine, reached over ssh${terminal.publicIp ? ` (${terminal.publicIp})` : ''}`;
    default:
      return `${name} — this build does not know the mode \`${ref.mode}\``;
  }
}

/**
 * What is running in the pane, in the bar's own words — and the sentence behind them.
 *
 * THE WORDS AND THE TOOLTIP DISAGREE ON PURPOSE about how much is known. `zsh` is what tmux
 * reported and is printed verbatim; the tooltip is where it says that a shell is not evidence
 * either way, because kontra's hold shell reports itself whether the Worker is running or finished.
 */
export function commandCell(terminal: Terminal): { text: string; title: string } {
  const command = terminal.command ?? '';
  const process = terminal.health.process;
  if (process === 'exited') {
    const status = terminal.exitStatus ?? '';
    return {
      text: status === '' ? 'exited' : `exited ${status}`,
      title:
        (status === ''
          ? 'the process in this pane has exited. '
          : `the command in this pane exited with status ${status}. `) +
        'The window is being held open so its last output stays readable, so the screen below is ' +
        'final — it is not going to change again. ' +
        (terminal.health.detail ?? ''),
    };
  }
  if (command === '') {
    return {
      text: '—',
      title:
        'this streamer did not report a pane command. It comes from the same `list-panes` probe the ' +
        'session check runs, so a blank here means the probe has not answered yet or predates the field.',
    };
  }
  if (process === 'running') {
    return {
      text: command,
      title:
        `\`${command}\` is in the foreground of this pane (\`pane_current_command\`), which is not a ` +
        'shell — so something is genuinely running here.',
    };
  }
  return {
    text: command,
    title:
      `\`${command}\` is what tmux reports for this pane (\`pane_current_command\`) — AND IT IS NOT A ` +
      'READING of whether the Worker is alive. kontra runs a Worker from a hold shell so that a crash ' +
      'leaves its exit status on screen; that shell keeps the pane’s foreground process group, so a ' +
      'running Go Worker and a finished one both report the shell. Measured on this host. The pane ' +
      'says which when it prints its exit banner; `kontra workers list` says now.',
  };
}

/**
 * How this tile is being fed, and how old what you are looking at is.
 *
 * `live` IS NOT A SYNONYM FOR FRESH and this is where that stops being implied. A live tile whose
 * last byte arrived four minutes ago is showing a four-minute-old screen, exactly like a snapshot
 * tile whose feed has stalled — the difference is only in what would fix it. So the mode and the age
 * are always both stated, and the age is measured in THIS browser (when a frame arrived) rather than
 * taken from an inventory that is re-read every thirty seconds.
 */
export function feedCell(input: {
  feed: Feed;
  stale: boolean;
  frameAgeMs: number | null;
  changeAgeMs: number | null;
}): { text: string; title: string } {
  const age = ageWords(input.frameAgeMs);
  // "quiet 6m", not "quiet 6m ago": this one is a DURATION of silence rather than a moment in the
  // past, and `ageWords` is shared with the status bar, where the moment reading is the right one.
  const quiet =
    input.changeAgeMs !== null && input.changeAgeMs > QUIET_MS
      ? ` · quiet ${ageWords(input.changeAgeMs).replace(/ ago$/, '')}`
      : '';

  if (input.stale) {
    return {
      text: `frozen · ${age}`,
      title:
        'the page’s socket to the streamer is down, so no frames are arriving and the screen below is ' +
        'frozen at its last one. The age is how long ago that frame came, not how long the pane has ' +
        'been quiet — the page is reconnecting on a backoff.',
    };
  }
  if (input.feed === 'error') {
    return {
      text: 'error',
      title:
        'the streamer refused or lost this Terminal’s stream. The notices below the wall carry the ' +
        'sentence; the screen below is whatever was last painted.',
    };
  }
  if (input.frameAgeMs === null) {
    return {
      text: `${input.feed} · no frame yet`,
      title:
        'nothing has arrived for this tile yet. A tile subscribes only once it has measured itself, ' +
        'so a tile that has not been laid out has not asked for a frame.',
    };
  }
  const stalled = input.feed !== 'live' && input.frameAgeMs > STALE_FRAME_MS;
  return {
    text: `${input.feed} · ${age}${quiet}`,
    title:
      (input.feed === 'live'
        ? 'a real PTY attach: bytes arrive as the pane prints them. '
        : 'a snapshot: the streamer runs one `capture-pane` per node every few seconds and this is the ' +
          'newest screen it sent. ') +
      `Last frame ${age}. ` +
      (stalled
        ? 'That is several missed passes — the feed has stalled, which is a different fault from a quiet ' +
          'Worker. '
        : '') +
      (quiet
        ? 'The screen itself has not CHANGED in that time: frames keep arriving and the pane is printing ' +
          'nothing, which is a running-but-silent Worker rather than a broken tile.'
        : ''),
  };
}

/** The two ages, in milliseconds — `null` for a measurement that has never happened. */
export interface Ages {
  frameAgeMs: number | null;
  changeAgeMs: number | null;
}

/** Pure, so the clock is a value and not a side effect. Clamped at zero: a browser clock that steps
 * backwards must not render a negative age. */
export function ages(clock: FrameClock, at: number): Ages {
  return {
    frameAgeMs: clock.lastFrameAt === null ? null : Math.max(0, at - clock.lastFrameAt),
    changeAgeMs: clock.lastChangeAt === null ? null : Math.max(0, at - clock.lastChangeAt),
  };
}

export default memo(function StatusLine({
  id,
  terminal,
  ref_,
  feed,
  frames,
  tileCols,
  tileRows,
  stale = false,
  compact = false,
  tickMs = 1000,
  now = Date.now,
}: StatusLineProps): JSX.Element {
  /**
   * The once-a-second clock, HERE rather than on the page: the age has to advance every second, and
   * a page-level clock would re-render the whole wall to move one number on every tile.
   *
   * READ ON THE FIRST RENDER, not only in the effect. An effect-only clock renders `no frame yet`
   * for up to a second on a tile that is already painting — and, less cosmetically, it makes the bar
   * untestable without a DOM, since `useEffect` never runs under `renderToStaticMarkup`.
   */
  const [clock, setClock] = useState<Ages>(() => ages(frames(), now()));

  useEffect(() => {
    const read = (): void => setClock(ages(frames(), now()));
    read();
    const timer = setInterval(read, tickMs);
    return () => clearInterval(timer);
  }, [frames, now, tickMs]);

  const host = terminal.host || terminal.machine || ref_.node;
  const command = commandCell(terminal);
  const fed = feedCell({ feed, stale, frameAgeMs: clock.frameAgeMs, changeAgeMs: clock.changeAgeMs });
  /** The chip row's OWN leading chip, when it is a failing one. Not a second reading of the health —
   *  the same call the row makes, so the bar cannot name a finding the chips do not. */
  const finding: Finding | undefined = leadingFinding(terminal.health, ref_.mode);
  /** Only asked when nothing is FAILING: a broken signal outranks an uncollected one, and the bar
   *  has room for one explanation. */
  const unmeasured: Finding | undefined =
    finding === undefined ? leadingUnmeasured(terminal.health, ref_.mode) : undefined;
  const tone = barTone({
    process: terminal.health.process,
    feed,
    stale,
    frameAgeMs: clock.frameAgeMs,
    failing: finding !== undefined,
    unmeasured: unmeasured !== undefined,
  });
  /** What the band's colour is ABOUT — a finding if there is one, else the unmeasured signal. */
  const explains: Finding | undefined = finding ?? unmeasured;
  const paneCols = terminal.paneCols ?? 0;
  const paneRows = terminal.paneRows ?? 0;
  const stakes = modeStakes(ref_.mode);

  return (
    <div
      data-testid={`tile-status-${id}`}
      data-tone={tone}
      data-feed={feed}
      data-process={terminal.health.process ?? 'unknown'}
      data-finding={finding?.signal}
      data-unmeasured={unmeasured?.signal}
      data-frame-age-ms={clock.frameAgeMs ?? ''}
      className={`flex min-w-0 shrink-0 items-center gap-1.5 px-1 font-mono text-[10px] leading-[15px] ${TONE_CLASS[tone]}`}
    >
      {/* tmux's `status-left`: the reverse-video block that names where you are. */}
      <span
        data-testid={`tile-host-${id}`}
        data-mode={ref_.mode}
        title={`${hostSentence(terminal, ref_)}. ${stakes.sentence}`}
        className="shrink-0 rounded-sm bg-black/25 px-1 font-semibold"
      >
        {host}
      </span>

      {!compact && (terminal.publicIp !== '' || ref_.mode === 'fleet') && (
        // The address `ssh` takes, and only when there is one to take. A `local` node has none and
        // the mode segment below already says so — printing the word `local` twice on a bar this
        // narrow spends a cell on nothing. A FLEET Machine with no address is different: that is a
        // missing inventory field, and it says so rather than disappearing.
        <span
          data-testid={`tile-ip-${id}`}
          className="shrink-0 tabular-nums opacity-90"
          title={
            terminal.publicIp
              ? `ssh root@${terminal.publicIp}`
              : 'the Fleet inventory carries no public address for this Machine'
          }
        >
          {terminal.publicIp || 'no address'}
        </span>
      )}

      {/* The string an operator types. It is the one thing on this bar that is an INSTRUCTION. */}
      <span
        data-testid={`tile-session-${id}`}
        className="shrink-0 font-semibold"
        title={`tmux attach -t ${ref_.session}   # then Ctrl-b then the window: ${ref_.window}`}
      >
        {ref_.session}:{ref_.window}
      </span>

      <span data-testid={`tile-command-${id}`} className="min-w-0 truncate" title={command.title}>
        {command.text}
      </span>

      {!compact && terminal.actor !== '' && (
        <span className="hidden shrink-0 opacity-90 sm:inline">
          {terminal.actor}
          {terminal.version ? `@${terminal.version}` : ''}
        </span>
      )}

      {!compact && ref_.mode !== 'fleet' && (
        // ADR 0020's asymmetry: on a local or container node these panes hold the REAL processes, so
        // crashing this tmux server costs a Worker and not just the view.
        <span
          data-testid={`tile-mode-${id}`}
          data-mode-level={stakes.level}
          title={stakes.sentence}
          className="shrink-0 rounded-sm bg-black/25 px-1"
        >
          {ref_.mode}
        </span>
      )}

      {!compact && (
        <span
          data-testid={`tile-geometry-${id}`}
          data-pane-cols={paneCols || ''}
          data-pane-rows={paneRows || ''}
          data-tile-cols={tileCols || ''}
          data-tile-rows={tileRows || ''}
          className="ml-auto shrink-0 tabular-nums"
          title={
            (paneCols > 0
              ? `the PANE is ${paneCols}×${paneRows} cells, from \`list-panes\`. kontra pins its sessions at ` +
                '200×50 with `window-size manual`, so a viewer cannot reshape the operator’s Worker by ' +
                'looking at it. '
              : 'the streamer has not reported this pane’s size yet. ') +
            (tileCols > 0
              ? `This tile measured ${tileCols}×${tileRows} for itself — a NARROWER tile scrolls rather than ` +
                'reflowing the pane, which is what read-only has to mean for geometry too.'
              : 'This tile has not measured itself yet, so it has not subscribed.')
          }
        >
          {paneCols > 0 ? `${paneCols}×${paneRows}` : '—'}
        </span>
      )}

      {explains && tone !== 'bad' && (
        /*
          WHY THIS BAND IS NOT GREEN, in the chips' own words.

          ONLY WHEN THE BAR WOULD OTHERWISE LOOK FINE. This cell exists to explain a colour, so a bar
          that is already RED does not carry it: red is the process having exited or the stream being
          dead, and both of those are already spelled out on this bar in their own cells — `exited
          143` in the command cell, `error` in the feed cell. Printing `▲ process: EXITED` beside
          `exited 143` would spend one of about eight cells saying the same thing twice, on the one
          tile state where nobody could mistake the bar for healthy.

          Rendered on EVERY width, including `compact`, and that is the one exception to this bar's
          "drop what you can afford to lose" rule: the only cell that can turn the band amber has to
          be the last cell to leave, or a narrow tile shows an unexplained colour. The command cell
          beside it is the `min-w-0 truncate` one, so the width comes out of a string that is already
          qualified as not-a-reading rather than out of a finding.

          `label`, not a sentence: `poller: NONE` is exactly what the leading chip says, and the
          clause that says what to DO about it is in the tooltip here as it is there.

          IT ALSO CARRIES THE UNMEASURED CASE, under a different mark. `▲` is a finding — something
          is wrong. `?` is an absence of information, which is why the band is slate rather than
          amber. Both have to be NAMED and not merely coloured: the tile's chip row used to be where
          an operator read which signal was uncertain, and a bar that went a different colour without
          saying which axis it was about would be a worse answer than the one it replaced.
        */
        <span
          data-testid={`tile-finding-${id}`}
          data-signal={explains.signal}
          data-kind={finding ? 'finding' : 'unmeasured'}
          className={`shrink-0 rounded-sm bg-black/25 px-1 font-semibold ${compact ? 'ml-auto' : ''}`}
          title={
            finding
              ? `${finding.text}\n\nThis bar is not green because of it. tmux's own status line is green whatever is happening in the pane; this one is the worst of what the tile knows.`
              : `${explains.text}\n\nThis bar is not green because this signal has never been measured — which is not the same as it being fine. Open the detail drawer for every signal in full.`
          }
        >
          {finding ? '▲' : '?'} {explains.label}
        </span>
      )}

      <span
        data-testid={`tile-feed-${id}`}
        className={`shrink-0 ${compact && !(explains && tone !== 'bad') ? 'ml-auto' : ''}`}
        title={fed.title}
      >
        {fed.text}
      </span>
    </div>
  );
});
