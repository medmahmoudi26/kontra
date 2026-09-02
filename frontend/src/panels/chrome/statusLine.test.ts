/**
 * The tile's status line — the bar that replaced the one tmux draws and a snapshot cannot carry.
 *
 * `renderToStaticMarkup` rather than a DOM, for the reason `HealthChips.test.ts` states: vitest runs
 * with `environment: 'node'` and there is no jsdom in this package. Everything asserted here is
 * attributes and text, which SSR produces — and the clock is read on the FIRST render precisely so
 * that it can be.
 *
 * WHAT THIS FILE IS DEFENDING. Every cell on the bar is a fact somebody measured, and the failure
 * mode is not a crash: it is a plausible sentence that is not true. A tile that says `live` over a
 * forty-second-old frame, or `zsh` in a way that reads as "the Worker died", is worse than a blank
 * tile, because an operator acts on it.
 */

import { describe, expect, it } from 'vitest';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import StatusLine, {
  ages,
  barTone,
  commandCell,
  feedCell,
  hostSentence,
  QUIET_MS,
  STALE_FRAME_MS,
  type Feed,
} from './StatusLine';
import { parseTileRef } from './tileRef';
import { leadingFinding } from '../HealthChips';
import type { Terminal } from '../panelsClient';

const ID = 'local:main-droplet/nscheck-0_1_0/actor';

/** A FLEET tile's id. `mode` is not a prop and not a field on `Terminal` — `parseTileRef` reads it
 *  off the id's first segment — so a test that needs the axes only a Machine can answer (`poller`,
 *  `loads`) has to ask for them by addressing a fleet pane. */
const FLEET_ID = 'fleet:kf-crawl-01/nscheck-0_1_0/actor';

function terminal(over: Partial<Terminal> = {}): Terminal {
  return {
    id: ID,
    machine: 'main-droplet',
    host: 'main-droplet',
    publicIp: '',
    tag: '',
    fleet: '',
    actor: 'nscheck',
    version: '0.1.0',
    window: 'actor',
    command: 'zsh',
    paneCols: 200,
    paneRows: 50,
    exitStatus: '',
    health: {
      reachable: 'ok',
      session: 'present',
      process: 'unknown',
      poller: 'live',
      loads: 'unknown',
    },
    ...over,
  };
}

function render(
  over: Partial<Terminal> = {},
  opts: { feed?: Feed; frameAgeMs?: number | null; changeAgeMs?: number | null; stale?: boolean; compact?: boolean } = {}
): string {
  const now = 1_700_000_000_000;
  const t = terminal(over);
  return renderToStaticMarkup(
    createElement(StatusLine, {
      id: t.id,
      terminal: t,
      ref_: parseTileRef(t.id),
      feed: opts.feed ?? 'snapshot',
      frames: () => ({
        lastFrameAt: opts.frameAgeMs === undefined ? now - 2000 : opts.frameAgeMs === null ? null : now - opts.frameAgeMs,
        lastChangeAt: opts.changeAgeMs === undefined ? now - 2000 : opts.changeAgeMs === null ? null : now - opts.changeAgeMs,
      }),
      tileCols: 96,
      tileRows: 18,
      ...(opts.stale === undefined ? {} : { stale: opts.stale }),
      ...(opts.compact === undefined ? {} : { compact: opts.compact }),
      now: () => now,
    })
  );
}

/** One cell's text and attributes, by its test hook. */
function cell(html: string, testid: string): { text: string; tag: string } {
  const re = new RegExp(`<span[^>]*data-testid="${testid}"[^>]*>(.*?)</span>`, 's');
  const m = re.exec(html);
  if (!m) throw new Error(`no ${testid} in:\n${html}`);
  return { text: (m[1] ?? '').replace(/<[^>]*>/g, ''), tag: /<span[^>]*>/.exec(m[0])?.[0] ?? '' };
}

/**
 * THE BAR AS AN OPERATOR SEES IT — the tone, then every cell in render order, separated the way tmux
 * separates its own.
 *
 * A STRING, NOT A SET OF ATTRIBUTES, for the reason `HealthChips.test.ts`'s `row()` is one: the
 * complaint that produced this pass was "put the green banner on the bottom, like tmux", which is
 * entirely about what the thing LOOKS like. No assertion on `data-feed` can fail because the band is
 * the wrong colour beside a red chip, or because a cell an operator needs fell off a narrow tile.
 * This one can.
 */
function bar(html: string): string {
  const tone = /data-tone="([^"]*)"/.exec(html)?.[1] ?? '(no tone)';
  const cells = [...html.matchAll(/<span[^>]*>(.*?)<\/span>/gs)].map((m) =>
    (m[1] ?? '')
      .replace(/<[^>]*>/g, '')
      .replace(/&#x27;/g, "'")
      .replace(/&quot;/g, '"')
      .replace(/&amp;/g, '&')
      .replace(/\s+/g, ' ')
      .trim()
  );
  return `[${tone}] ${cells.join(' | ')}`;
}

describe('the bar an operator reads', () => {
  it('names the host, the session:window to attach to, and what is in the pane', () => {
    // The three things the user asked for, in the order they asked for them. The session cell is the
    // only INSTRUCTION on the bar, so it has to be the string `tmux attach -t` takes — which is the
    // tmux-sanitised name (`nscheck-0_1_0`), not the version with dots that nothing will match.
    const html = render();
    expect(cell(html, `tile-host-${ID}`).text).toBe('main-droplet');
    expect(cell(html, `tile-session-${ID}`).text).toBe('nscheck-0_1_0:actor');
    expect(html).toContain('tmux attach -t nscheck-0_1_0');
    expect(cell(html, `tile-command-${ID}`).text).toBe('zsh');
  });

  it('shows the PANE’s geometry, not the tile’s, and says which is which', () => {
    // Since `window-size manual` the two do not track each other: the pane is pinned at 200×50 and
    // the tile is whatever the browser is. A bar that showed the tile's number as "the pane" would
    // be claiming the operator's Worker is 96 columns wide.
    const html = render();
    const geometry = cell(html, `tile-geometry-${ID}`);
    expect(geometry.text).toBe('200×50');
    expect(geometry.tag).toContain('data-pane-cols="200"');
    expect(geometry.tag).toContain('data-tile-cols="96"');
    expect(html).toContain('This tile measured 96×18');
  });

  it('renders an unreported pane size as — rather than as zero', () => {
    const html = render({ paneCols: 0, paneRows: 0 });
    expect(cell(html, `tile-geometry-${ID}`).text).toBe('—');
  });
});

describe('liveness, and its age', () => {
  it('never says live without saying how old the last frame is', () => {
    // A live tile whose last byte arrived four minutes ago is showing a four-minute-old screen. The
    // mode is not the freshness, and the bar states both.
    const html = render({}, { feed: 'live', frameAgeMs: 240_000, changeAgeMs: 240_000 });
    const feed = cell(html, `tile-feed-${ID}`);
    expect(feed.text).toContain('live');
    expect(feed.text).toContain('4m ago');
  });

  it('distinguishes a stalled FEED from a quiet WORKER', () => {
    // Two different faults with two different fixes: frames not arriving, versus frames arriving and
    // being identical. A snapshot arrives whether or not the pane printed anything, so these can
    // only be told apart by tracking them separately.
    const stalled = feedCell({ feed: 'snapshot', stale: false, frameAgeMs: STALE_FRAME_MS + 1000, changeAgeMs: 0 });
    expect(stalled.title).toContain('the feed has stalled');

    const quiet = feedCell({ feed: 'snapshot', stale: false, frameAgeMs: 2000, changeAgeMs: QUIET_MS + 60_000 });
    expect(quiet.text).toContain('quiet');
    expect(quiet.title).toContain('has not CHANGED');
    expect(quiet.title).not.toContain('the feed has stalled');
  });

  it('says the screen is frozen when the socket is down, rather than ageing quietly', () => {
    const html = render({}, { stale: true, frameAgeMs: 30_000 });
    expect(cell(html, `tile-feed-${ID}`).text).toContain('frozen');
  });

  it('says "no frame yet" rather than inventing a zero', () => {
    const html = render({}, { frameAgeMs: null, changeAgeMs: null });
    expect(cell(html, `tile-feed-${ID}`).text).toContain('no frame yet');
  });

  it('clamps a backwards clock instead of rendering a negative age', () => {
    expect(ages({ lastFrameAt: 100, lastChangeAt: null }, 50)).toEqual({
      frameAgeMs: 0,
      changeAgeMs: null,
    });
  });
});

describe('what the bar refuses to imply', () => {
  /**
   * THE LIE THIS WHOLE PASS EXISTS TO AVOID.
   *
   * MEASURED on this box: `pane_current_command` is `zsh` for a pane whose Go Worker is running,
   * because kontra's hold shell keeps the pane's foreground process group. So the bar prints the
   * word tmux reported — and its tooltip says, in as many words, that it is not a reading.
   */
  it('prints a hold shell verbatim and refuses to call it a dead Worker', () => {
    const html = render({ command: 'zsh', health: { ...terminal().health, process: 'unknown' } });
    const command = cell(html, `tile-command-${ID}`);
    expect(command.text).toBe('zsh');
    expect(command.tag).toContain('NOT A');
    expect(cell(html, `tile-feed-${ID}`)).toBeDefined();
    // …and the bar is still the working colour: unknown is not a fault.
    expect(html).toContain('data-tone="ok"');
  });

  it('says exited WITH the status once the pane has reported one', () => {
    const html = render({
      command: 'zsh',
      exitStatus: '143',
      health: { ...terminal().health, process: 'exited', detail: 'exited with status 143' },
    });
    expect(cell(html, `tile-command-${ID}`).text).toBe('exited 143');
    // A finished Worker turns the whole bar, because it outranks every other state it carries.
    expect(html).toContain('data-tone="bad"');
    expect(html).toContain('data-process="exited"');
  });

  it('colours from the worst fact it carries, and only from a measured one', () => {
    const base = { feed: 'snapshot' as Feed, stale: false, frameAgeMs: 1000 };
    expect(barTone({ ...base, process: 'running' })).toBe('ok');
    expect(barTone({ ...base, process: 'unknown' })).toBe('ok');
    expect(barTone({ ...base, process: 'exited' })).toBe('bad');
    expect(barTone({ ...base, process: 'running', stale: true })).toBe('warn');
    expect(barTone({ ...base, process: 'running', frameAgeMs: STALE_FRAME_MS + 1 })).toBe('warn');
    // A frame that has never arrived is not a stale one — a tile that has not measured itself has
    // not subscribed, and painting that amber would make every fresh wall look broken.
    expect(barTone({ ...base, process: 'running', frameAgeMs: null })).toBe('ok');
    // An exited process outranks a dropped socket: both mean the screen is final, but only one of
    // them is about the Worker.
    expect(barTone({ ...base, process: 'exited', stale: true })).toBe('bad');
  });

  it('says which KIND of node the host is, because the stakes differ by mode', () => {
    expect(hostSentence(terminal(), parseTileRef(ID))).toContain('THIS host');
    const fleetId = 'fleet:kf-crawl-01/nscheck-0_1_0/actor';
    expect(
      hostSentence(terminal({ id: fleetId, machine: 'kf-crawl-01', publicIp: '203.0.113.9' }), parseTileRef(fleetId))
    ).toContain('203.0.113.9');
  });

  it('drops the address and the geometry on a narrow tile, never the identity or the liveness', () => {
    const html = render({}, { compact: true });
    expect(html).toContain(`data-testid="tile-host-${ID}"`);
    expect(html).toContain(`data-testid="tile-session-${ID}"`);
    expect(html).toContain(`data-testid="tile-command-${ID}"`);
    expect(html).toContain(`data-testid="tile-feed-${ID}"`);
    expect(html).not.toContain(`data-testid="tile-geometry-${ID}"`);
    expect(html).not.toContain(`data-testid="tile-ip-${ID}"`);
  });

  it('spends a cell on the address only when there is one, and says so when there should be', () => {
    // A local node has no address and the mode segment already says `local`; printing the word twice
    // on a bar this narrow spends a cell on nothing.
    expect(render()).not.toContain(`data-testid="tile-ip-${ID}"`);
    // A fleet Machine's address is what `ssh` takes, so it is shown…
    const fleetId = 'fleet:kf-crawl-01/nscheck-0_1_0/actor';
    const withIp = renderToStaticMarkup(
      createElement(StatusLine, {
        id: fleetId,
        terminal: terminal({ id: fleetId, machine: 'kf-crawl-01', publicIp: '203.0.113.9' }),
        ref_: parseTileRef(fleetId),
        feed: 'snapshot',
        frames: () => ({ lastFrameAt: null, lastChangeAt: null }),
        tileCols: 96,
        tileRows: 18,
      })
    );
    expect(withIp).toContain('203.0.113.9');
    expect(withIp).toContain('ssh root@203.0.113.9');
    // …and a Machine whose inventory carries none says that, rather than vanishing: a missing field
    // is a fact about the stack, not an absence to hide.
    const noIp = renderToStaticMarkup(
      createElement(StatusLine, {
        id: fleetId,
        terminal: terminal({ id: fleetId, machine: 'kf-crawl-01', publicIp: '' }),
        ref_: parseTileRef(fleetId),
        feed: 'snapshot',
        frames: () => ({ lastFrameAt: null, lastChangeAt: null }),
        tileCols: 96,
        tileRows: 18,
      })
    );
    expect(noIp).toContain('no address');
  });

  it('renders a streamer that sends none of the new fields without inventing any of them', () => {
    // An older streamer: no command, no pane size, no process. Every one of those must read as an
    // absence, and the bar must still say who and where.
    const html = render({
      command: undefined,
      paneCols: undefined,
      paneRows: undefined,
      exitStatus: undefined,
      health: { reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' },
    });
    expect(cell(html, `tile-command-${ID}`).text).toBe('—');
    expect(cell(html, `tile-geometry-${ID}`).text).toBe('—');
    expect(html).toContain('data-process="unknown"');
    expect(cell(html, `tile-host-${ID}`).text).toBe('main-droplet');
  });

  it('prints the exact words for a pane whose command is genuinely running', () => {
    const running = commandCell(
      terminal({ command: 'journalctl', health: { ...terminal().health, process: 'running' } })
    );
    expect(running.text).toBe('journalctl');
    expect(running.title).toContain('genuinely running');
  });
});

/**
 * THE BAR, AS IT LOOKS — rendered, asserted as the string an operator reads.
 *
 * "And put the green banner on the bottom, like tmux." Everything below is about the LOOK: the
 * colours, the dark text, the cells that survive a narrow tile, and the one thing tmux never has to
 * think about — that a green band on a tile whose chips are red is the wall contradicting itself.
 */
describe('the bar an operator actually reads', () => {
  it('is tmux’s bar: a solid green band with BLACK text, in every theme', () => {
    // `bg=green,fg=black` is tmux's default `status-style`, and it is the whole reason this is
    // recognisable. It was `bg-emerald-600 text-emerald-50` — a pale-on-green app chip, in a theme
    // pair — which is a status line nobody has ever seen at the bottom of a terminal.
    const html = render();
    expect(html).toContain('bg-green-600 text-black');
    // ONE PALETTE, NO `dark:` VARIANT: a tmux status line does not follow the reader's app theme.
    expect(html).not.toMatch(/dark:bg-\w+-\d+\/?\d*\s+dark:text/);
  });

  it('reads, left to right, as the tmux line it replaces', () => {
    expect(bar(render())).toBe(
      '[ok] main-droplet | nscheck-0_1_0:actor | zsh | nscheck@0.1.0 | local | 200×50 | snapshot · 2s ago'
    );
  });

  it('turns amber and NAMES THE REASON when the chips on the same tile are red', () => {
    /*
      THE CONTRADICTION THIS RESOLVES. tmux's bar is green whatever is happening in the pane, because
      tmux has no idea. Ours sits under a severity-ordered chip row, so a permanently green band
      beneath `▲ poller: NONE` is the loudest colour on the tile disagreeing with the reading right
      above it. Green now means "and nothing on this tile is failing".

      Amber rather than red: red is reserved for the two facts that say THIS SCREEN WILL NOT CHANGE
      AGAIN — the process exited, or the stream is dead — which is what the bar knows and the chips
      do not.
    */
    const failing = { ...terminal().health, poller: 'none' as const };
    expect(bar(render({ health: failing }))).toBe(
      '[warn] main-droplet | nscheck-0_1_0:actor | zsh | nscheck@0.1.0 | local | 200×50 | ▲ poller: NONE | snapshot · 2s ago'
    );
  });

  it('names the finding in the CHIP ROW’S OWN WORDS, and never a finding of its own', () => {
    // One derivation (`HealthChips.leadingFinding` is literally `orderChips(chipsFor(…))[0]`), so
    // the bar cannot pick a different axis, use different words for the same one, or disagree about
    // which axes apply to a `local` pane. A second severity rule here would be a second definition
    // of "failing" on one tile.
    const health = { ...terminal().health, poller: 'none' as const, session: 'absent' as const };
    const found = leadingFinding(health, 'local');
    expect(found?.label).toBe('session: ABSENT');
    expect(bar(render({ health }))).toContain('▲ session: ABSENT');
    // `session` leads because CHIP_SIGNALS orders it before `poller` and both are `bad` — exactly
    // what the row does.
    expect(bar(render({ health }))).not.toContain('poller: NONE');
  });

  it('does not turn amber for an axis this pane’s MODE cannot answer', () => {
    // The `b4f11b6` rule, reused rather than restated: `loads` needs a vmagent that only fleet
    // Machines have, so `loads: unknown` on a local pane is not a finding — and a bar that went
    // amber for it would be amber on every local tile forever, which is how a colour stops being
    // read. `unknown` is never a FINDING on any axis: amber is severity, not completeness.
    //
    // Completeness now has its own tone (`unmeasured`, slate) — see the tests below — but it is
    // reached only through the same mode filter, so a mode-inapplicable axis does not trigger that
    // either. This bar stays `[ok]`.
    expect(bar(render())).toContain('[ok]');
    expect(barTone({ process: 'running', feed: 'snapshot', stale: false, frameAgeMs: 1000, failing: false })).toBe('ok');
    // …and a `loads` that genuinely FAILS is a finding even on a local pane, because omission stays
    // one-directional all the way down.
    const html = render({ health: { ...terminal().health, loads: 'failing' } });
    expect(bar(html)).toContain('▲ loads: FAILING');
    expect(html).toContain('data-tone="warn"');
  });

  it('is SLATE, not green, when a signal that should be measured never was', () => {
    // THE HOLE THE CHIP ROW USED TO COVER. ADR 0020 forbids `unknown` rendering as healthy, and that
    // was discharged by the row: `poller: unknown` sat on the tile in its own tri-state whatever the
    // bar did. With the row gone this bar is the only health reading left on the wall, and it asked
    // only `leadingFinding` — which reports `bad` and nothing else. A fleet pane whose poller had
    // never been sampled therefore drew a solid tmux green, identical to one where every signal was
    // checked and fine. That is precisely the collapse the ADR exists to forbid.
    const html = render({ id: FLEET_ID, health: { ...terminal().health, poller: 'unknown' } });
    expect(html).toContain('data-tone="unmeasured"');
    expect(html).not.toContain('bg-green-600');
    // NAMED, not merely coloured — an operator has to know WHICH axis is uncertain, which is the
    // thing the chip row was actually good at.
    expect(html).toContain('data-unmeasured="poller"');
    expect(bar(html)).toContain('? poller');
  });

  it('marks an unmeasured signal with `?` and a finding with `▲` — two different facts', () => {
    // Slate says "nobody collected this"; amber says "this is broken". Conflating them would send an
    // operator to restart a healthy service, which is the mirror of the bug that produced this bar.
    const unmeasured = render({ id: FLEET_ID, health: { ...terminal().health, poller: 'unknown' } });
    const failing = render({ id: FLEET_ID, health: { ...terminal().health, poller: 'none' } });
    expect(unmeasured).toContain('data-kind="unmeasured"');
    expect(failing).toContain('data-kind="finding"');
    expect(failing).toContain('data-tone="warn"');
  });

  it('a failing signal outranks an unmeasured one — the bar explains the worse fact', () => {
    const html = render({
      id: FLEET_ID,
      health: { ...terminal().health, poller: 'none', loads: 'unknown' },
    });
    expect(html).toContain('data-tone="warn"');
    expect(bar(html)).toContain('▲ poller: NONE');
    expect(bar(html)).not.toContain('? loads');
  });

  it('never goes slate for `process`, which a kontra pane can never report', () => {
    // A Worker runs from a hold shell so a crash leaves its exit status on screen, and that shell
    // owns the pane's foreground process group — so tmux reports `zsh` whether the Worker is alive
    // or finished. `process: unknown` is the permanent, correct reading for every pane kontra
    // starts, not a gap in collection. Treating it as unmeasured would paint the entire wall slate
    // forever, which says as little as the always-green bar, and would bury the two signals that do
    // mean nobody is collecting them.
    const html = render({ health: { ...terminal().health, process: 'unknown' } });
    expect(html).toContain('data-tone="ok"');
  });

  it('keeps the finding on a NARROW tile, where everything else goes', () => {
    // The one exception to "drop what you can afford to lose": the only cell that can turn the band
    // amber has to be the last cell to leave, or a compact tile shows an unexplained colour.
    const html = render({ health: { ...terminal().health, poller: 'none' } }, { compact: true });
    expect(bar(html)).toBe('[warn] main-droplet | nscheck-0_1_0:actor | zsh | ▲ poller: NONE | snapshot · 2s ago');
  });

  it('a finished Worker is red, and does not spend a cell saying it twice', () => {
    // The bar already says `exited 143` in the command cell, so the finding cell — which exists to
    // explain a colour that would otherwise look fine — has nothing to add on a red bar. It comes
    // back the moment the bar is not red; the assertion below is the pair.
    const html = render({
      exitStatus: '143',
      health: { ...terminal().health, process: 'exited', poller: 'none' },
    });
    expect(bar(html)).toBe(
      '[bad] main-droplet | nscheck-0_1_0:actor | exited 143 | nscheck@0.1.0 | local | 200×50 | snapshot · 2s ago'
    );
    expect(html).not.toContain('tile-finding-');
    // …and the same tile, with the Worker still running, does carry the poller finding.
    expect(bar(render({ health: { ...terminal().health, poller: 'none' } }))).toContain('▲ poller: NONE');
  });
});
