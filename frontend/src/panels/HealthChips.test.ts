/**
 * HealthChips, rendered.
 *
 * `renderToStaticMarkup` rather than a DOM: `vite.config.ts` runs vitest with `environment: 'node'`
 * and an `src/**\/*.test.ts` include, and there is no jsdom or testing-library in
 * `frontend/package.json` — a file this slice does not own and cannot add a dependency to.
 * Server rendering is not a lesser substitute here: every assertion below is about ATTRIBUTES AND
 * TEXT that the contract pins (`data-testid`, `data-state`, `data-value`), all of which are in the
 * markup, and the Playwright `contract`/`real` projects already drive the same component in a real
 * browser where layout and colour are what matter.
 */

import { describe, expect, it } from 'vitest';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import HealthChips, { CHIP_SIGNALS, chipState, detailLines } from './HealthChips';
import type { TerminalHealth } from './panelsClient';

function render(health: TerminalHealth, terminalId?: string, mode?: string): string {
  return renderToStaticMarkup(createElement(HealthChips, { health, terminalId, mode }));
}

/**
 * THE ROW AS AN OPERATOR SEES IT — glyph, words, in render order, chips separated by two spaces.
 *
 * A string, not a set of booleans, and that is the point: the bug this file grew a section for was
 * never that a chip had the wrong `data-state`. Every state was correct. The row was unreadable —
 * five chips of prose, wrapping, three of them saying nothing about the pane in front of you — and no
 * assertion on an attribute can fail for that. This one can.
 */
function row(html: string): string {
  const items = html.match(/<li[^>]*data-testid="(?:chip-|health-detail)[^"]*"[^>]*>.*?<\/li>/gs) ?? [];
  return items
    .filter((li) => /data-testid="chip-/.test(li))
    .map((li) =>
      li
        .replace(/<li[^>]*>/, '')
        .replace(/<\/li>$/, '')
        .replace(/<[^>]*>/g, ' ')
        .replace(/&#x27;/g, "'")
        .replace(/&quot;/g, '"')
        .replace(/&amp;/g, '&')
        .replace(/\s+/g, ' ')
        .trim()
    )
    .join('  ');
}

/** The prose under the row, one sentence per line — '' when the tile is quiet. */
function prose(html: string): string {
  const items = html.match(/<li[^>]*data-testid="health-detail-line[^"]*"[^>]*>(.*?)<\/li>/gs) ?? [];
  return items
    .map((li) =>
      li
        .replace(/<[^>]*>/g, '')
        .replace(/&#x27;/g, "'")
        .replace(/&quot;/g, '"')
        .replace(/&amp;/g, '&')
        .replace(/&lt;/g, '<')
        .replace(/&gt;/g, '>')
        .trim()
    )
    .join('\n');
}

/** One chip's attributes, read back out of the markup by its test hook. */
function chip(html: string, signal: string, suffix = ''): { state: string; value: string; text: string } {
  const re = new RegExp(`<li[^>]*data-testid="chip-${signal}${suffix}"[^>]*>(.*?)</li>`, 's');
  const m = re.exec(html);
  if (!m) throw new Error(`no chip-${signal}${suffix} in:\n${html}`);
  const [tag] = /<li[^>]*>/.exec(m[0]) ?? [''];
  const state = /data-state="([^"]*)"/.exec(tag)?.[1] ?? '';
  const value = /data-value="([^"]*)"/.exec(tag)?.[1] ?? '';
  const text = (m[1] ?? '').replace(/<[^>]*>/g, '');
  return { state, value, text };
}

const HEALTHY: TerminalHealth = {
  reachable: 'ok',
  session: 'present',
  process: 'running',
  poller: 'live',
  loads: 'ok',
};

const UNMEASURED: TerminalHealth = {
  reachable: 'unknown',
  session: 'unknown',
  process: 'unknown',
  poller: 'unknown',
  loads: 'unknown',
};

describe('five chips, never one light', () => {
  it('renders exactly one element per signal', () => {
    const html = render(HEALTHY);
    for (const signal of CHIP_SIGNALS) expect(chip(html, signal).state).toBe('ok');
    // FIVE since the pane's process became a signal of its own. `process` sits beside `session`
    // because that is the pair an operator confuses: a Worker that exited leaves its session
    // present, its screen full and its tile painting, and the fifth chip is what says so.
    expect(CHIP_SIGNALS).toEqual(['reachable', 'session', 'process', 'poller', 'loads']);
    // Five, and no sixth chip: `detail` is prose, not a signal.
    expect(html.match(/data-testid="chip-/g)).toHaveLength(5);
  });

  it('never reads a present session as a running process', () => {
    // The failure this signal exists for: everything else is fine and the Worker is over.
    const html = render({ ...HEALTHY, process: 'exited', detail: 'exited 143' });
    expect(chip(html, 'session').state).toBe('ok');
    expect(chip(html, 'process').state).toBe('bad');
    expect(chip(html, 'process').value).toBe('exited');
    expect(chip(html, 'process').text).toContain('EXITED');
  });

  it('keeps a failing signal from colouring the other three', () => {
    // The round-3 shape: the session is present, SSH is fine, the poller is live, and the actor host
    // is eating an eighth of every sweep. A rolled-up light would be green.
    const html = render({ ...HEALTHY, loads: 'failing' });
    expect(chip(html, 'reachable').state).toBe('ok');
    expect(chip(html, 'session').state).toBe('ok');
    expect(chip(html, 'poller').state).toBe('ok');
    expect(chip(html, 'loads').state).toBe('bad');
  });

  it('carries the RAW enum in data-value beside the normalized state', () => {
    // Amendment 14: `data-state` is the tri-state a spec asserts, `data-value` is the specific
    // failure, so `absent` and `no-tmux` are tellable apart without a second vocabulary.
    const absent = render({ ...HEALTHY, session: 'absent' });
    expect(chip(absent, 'session')).toMatchObject({ state: 'bad', value: 'absent' });

    const noTmux = render({ ...HEALTHY, session: 'no-tmux' });
    expect(chip(noTmux, 'session')).toMatchObject({ state: 'bad', value: 'no-tmux' });

    const none = render({ ...HEALTHY, poller: 'none' });
    expect(chip(none, 'poller')).toMatchObject({ state: 'bad', value: 'none' });

    const failing = render({ ...HEALTHY, loads: 'failing' });
    expect(chip(failing, 'loads')).toMatchObject({ state: 'bad', value: 'failing' });

    const unknown = render(UNMEASURED);
    expect(chip(unknown, 'poller')).toMatchObject({ state: 'unknown', value: 'unknown' });
  });
});

describe('unknown is never ok', () => {
  it('gives every unmeasured signal its own state, not the healthy one', () => {
    const html = render(UNMEASURED);
    for (const signal of CHIP_SIGNALS) expect(chip(html, signal).state).toBe('unknown');
    expect(html).not.toContain('data-state="ok"');
  });

  it('says "unknown" in the words as well as the attribute', () => {
    // Visibly distinct, not merely distinct in an attribute — the same assertion the Playwright spec
    // makes, held here so a wording change fails fast.
    const html = render(UNMEASURED);
    for (const signal of CHIP_SIGNALS) expect(chip(html, signal).text).toContain('unknown');
  });

  it('never lets a chip claim ok while its words say unknown', () => {
    for (const health of [HEALTHY, UNMEASURED, { ...HEALTHY, poller: 'unknown' as const }]) {
      const html = render(health);
      for (const signal of CHIP_SIGNALS) {
        const c = chip(html, signal);
        if (c.state === 'ok') expect(c.text).not.toContain('unknown');
      }
    }
  });

  it('differs from ok by more than a shade — hue, fill, border style and glyph', () => {
    const okHtml = render(HEALTHY);
    const unknownHtml = render(UNMEASURED);
    const okChip = /<li[^>]*data-testid="chip-loads"[^>]*>/.exec(okHtml)?.[0] ?? '';
    const unknownChip = /<li[^>]*data-testid="chip-loads"[^>]*>/.exec(unknownHtml)?.[0] ?? '';

    // Not the same hue at a different opacity: different colour family entirely.
    expect(okChip).toContain('emerald');
    expect(unknownChip).not.toContain('emerald');
    // A dashed, unfilled border is the channel that survives greyscale and colour-blindness.
    expect(okChip).toContain('border-solid');
    expect(unknownChip).toContain('border-dashed');
    expect(unknownChip).toContain('bg-transparent');
    // And a glyph, for the operator skimming a wall rather than reading it.
    expect(okHtml).toContain('●');
    expect(unknownHtml).toContain('?');
  });

  it('normalizes null to unknown rather than to bad', () => {
    expect(chipState(null)).toBe('unknown');
    expect(chipState(true)).toBe('ok');
    expect(chipState(false)).toBe('bad');
  });
});

describe('the detail sentences', () => {
  it('surfaces a failing signal sentence, one line per signal', () => {
    const html = render({
      ...HEALTHY,
      poller: 'none',
      loads: 'failing',
      detail:
        'queue webcrawl-0.2.0 is registered but nothing is polling it · kf-crawl-01 is reloading ' +
        'its actor resource on 99% of batches',
    });
    // Two failures are two lines. A joined paragraph is the prose form of the collapse the chips
    // exist to prevent.
    expect(html.match(/data-testid="health-detail-line"/g)).toHaveLength(2);
    expect(html).toContain('nothing is polling it');
    expect(html).toContain('99% of batches');
  });

  it('puts the sentence on the failing chip itself, and not on a healthy one', () => {
    const html = render({ ...HEALTHY, loads: 'failing', detail: 'kf-crawl-01 is reloading' });
    const loads = /<li[^>]*data-testid="chip-loads"[^>]*>/.exec(html)?.[0] ?? '';
    const session = /<li[^>]*data-testid="chip-session"[^>]*>/.exec(html)?.[0] ?? '';
    // The signal's OWN sentence leads, then the server's prose. Both are on the element that failed;
    // the chip's WORDS are now just `loads: FAILING`, so the tooltip is where the clause that says
    // what to do about it went.
    expect(loads).toContain('loads: FAILING');
    expect(loads).toContain('kf-crawl-01 is reloading');
    expect(session).not.toContain('title=');
  });

  it('titles an UNKNOWN chip too — why we do not know is also actionable', () => {
    const html = render({
      ...HEALTHY,
      loads: 'unknown',
      detail: 'no kontra_resource_reloads_total series for instance="kf-crawl-01"',
    });
    const loads = /<li[^>]*data-testid="chip-loads"[^>]*>/.exec(html)?.[0] ?? '';
    expect(loads).toContain('title=');
  });

  it('renders no detail block when nothing is failing', () => {
    expect(render(HEALTHY)).not.toContain('health-detail');
  });

  it('splits on the separator the server joins with', () => {
    expect(detailLines(undefined)).toEqual([]);
    expect(detailLines('')).toEqual([]);
    expect(detailLines('one')).toEqual(['one']);
    expect(detailLines('one · two')).toEqual(['one', 'two']);
    // A sentence containing a middot but not the separator stays one sentence.
    expect(detailLines('a·b')).toEqual(['a·b']);
  });
});

describe('test hooks', () => {
  it('stays chip-<signal> with no id, so slice 1 specs and the stub keep selecting it', () => {
    const html = render(HEALTHY);
    expect(html).toContain('data-testid="chip-reachable"');
    expect(html).toContain('data-testid="chip-loads"');
  });

  it('scopes every hook by Terminal id when a wall needs them unique', () => {
    const id = 'fleet:kf-crawl-01/kontra-webcrawl/actor';
    const html = render({ ...HEALTHY, loads: 'failing', detail: 'sick' }, id);
    expect(chip(html, 'loads', `-${id}`).state).toBe('bad');
    expect(html).toContain(`data-testid="health-${id}"`);
    expect(html).toContain(`data-testid="health-detail-${id}"`);
  });
});

describe('a health object missing a signal', () => {
  it('renders the absent signal as unknown rather than dropping the chip', () => {
    // A streamer built before a signal existed. A missing chip is worse than an unknown one: the
    // wall would look like it has nothing to say.
    const partial = { reachable: 'ok', session: 'present' } as unknown as TerminalHealth;
    const html = render(partial);
    expect(html.match(/data-testid="chip-/g)).toHaveLength(5);
    expect(chip(html, 'poller').state).toBe('unknown');
    expect(chip(html, 'loads').state).toBe('unknown');
    // `process` is the newest, so it is the one a live streamer is most likely not to send yet.
    expect(chip(html, 'process').state).toBe('unknown');
    expect(chip(html, 'process').value).toBe('unknown');
  });
});

/**
 * THE ROW, AS IT LOOKS. Four real panes, rendered, asserted as the string an operator reads.
 *
 * Every payload below is verbatim from `GET /api/panels/terminals` on the box this was written on —
 * including the sentences, which are the streamer's own words and not a fixture author's. The
 * `local` ones are the tiles that produced the complaint: five chips, two lines, and the only
 * actionable one (`poller: NONE`) fourth and styled like the four that said nothing.
 */
describe('the row an operator actually reads', () => {
  /** `local:localhost/kontra-0/claude` — a Worker that is up, on this host. */
  const HEALTHY_LOCAL: TerminalHealth = {
    reachable: 'ok',
    session: 'present',
    process: 'running',
    poller: 'unknown',
    loads: 'unknown',
    detail:
      'a local session names its actor (0) but not its version, and the shared queue is ' +
      '<actor>-<version> — so Temporal cannot be asked about this Worker from here; ' +
      '`kontra workers list` can · could not ask VictoriaMetrics about localhost’s load ' +
      'health: fetch failed',
  };

  /** `local:localhost/nscheck-0_1_0/actor` — the tile in the screenshot. */
  const POLLER_NONE: TerminalHealth = {
    reachable: 'ok',
    session: 'present',
    process: 'unknown',
    poller: 'none',
    loads: 'unknown',
    detail:
      'queue nscheck-0.1.0 is polled by 1 worker(s) (main-droplet) but none from localhost — its ' +
      'kontra-handler.service is probably down · could not ask VictoriaMetrics about ' +
      'localhost’s load health: fetch failed · this pane’s foreground command is `zsh`',
  };

  it('a HEALTHY LOCAL pane says the three things that are true of it, and marks the two that cannot be', () => {
    // Was: `● ssh: reachable  ● session: present  ● process: running  ? poller: unknown (not
    //       measured yet)  ? loads: unknown (not measured yet)` — five chips over two lines, two of
    //       them about machinery that does not exist for a local pane.
    expect(row(render(HEALTHY_LOCAL, undefined, 'local'))).toBe(
      '? poller: unknown  ● session: present  ● process: running  – n/a: reachable, loads'
    );
    // Quiet: nothing here is failing, so the sentences stay in the tooltips and the drawer rather
    // than spending three lines of terminal explaining two unknowns on every tile of every node.
    expect(prose(render(HEALTHY_LOCAL, undefined, 'local'))).toBe('');
  });

  it('a HEALTHY FLEET pane still shows all five — nothing is inapplicable on a Machine', () => {
    // The applicability rule is about MODE, not about tidiness: a fleet Machine is dialled over SSH
    // and scraped by vmagent, so both axes are real there and both are shown.
    expect(row(render(HEALTHY))).toBe(
      '● ssh: reachable  ● session: present  ● process: running  ● poller: live  ● loads: ok'
    );
    expect(row(render(HEALTHY))).not.toContain('n/a');
  });

  it('a pane with POLLER: NONE leads with it, and the sentence that says what to do is on the tile', () => {
    const html = render(POLLER_NONE, undefined, 'local');
    // Was: `● ssh: reachable  ● session: present  ? process: unknown (tmux reports the hold shell
    //       either way)  ▲ poller: NONE — registered but nothing polling` + a lone `? loads:
    //       unknown (not measured yet)` wrapped onto a second line.
    expect(row(html)).toBe(
      '▲ poller: NONE  ? process: unknown  ● session: present  – n/a: reachable, loads'
    );
    // FIRST, not fourth. The one chip anybody has to act on is the one the eye lands on.
    expect(row(html).indexOf('poller')).toBe(2);
    // And the streamer's sentence — which already names who IS polling the queue and who is not —
    // is on the tile, because something is genuinely failing here.
    expect(prose(html)).toContain(
      'queue nscheck-0.1.0 is polled by 1 worker(s) (main-droplet) but none from localhost'
    );
  });

  it('a pane whose PROCESS EXITED leads with it, beside a session that is still perfectly present', () => {
    // The failure the fifth signal exists for: `cli/tmux.go` holds the window open, so the session
    // is present, the screen is full and the tile paints. `session: present` must stay green and
    // `process: EXITED` must be the first thing in the row — the two must not average out.
    const html = render(
      { ...HEALTHY, process: 'exited', detail: 'the pane reported exit status 143' },
      undefined,
      'fleet'
    );
    expect(row(html)).toBe(
      '▲ process: EXITED  ● ssh: reachable  ● session: present  ● poller: live  ● loads: ok'
    );
    expect(prose(html)).toBe('the pane reported exit status 143');
  });

  it('never wraps: one row, and what falls off the end is the healthy end', () => {
    // The layout complaint, as an invariant rather than a screenshot. `flex-nowrap` is what makes a
    // narrow tile clip instead of growing a second line with one chip on it; the severity order is
    // what makes clipping tolerable, because the chip that falls off is always the least urgent one.
    const html = render(POLLER_NONE, undefined, 'local');
    expect(html).toContain('flex-nowrap');
    expect(html).not.toContain('flex-wrap gap');
    const states = [...html.matchAll(/data-testid="chip-[^"]*"\s+data-state="([^"]*)"/g)].map(
      (m) => m[1]
    );
    expect(states).toEqual(['bad', 'unknown', 'ok', 'n/a']);
  });
});

/**
 * NOT APPLICABLE IS NOT UNKNOWN — and the guard that keeps it inside ADR 0020.
 */
describe('an axis that cannot apply to this pane', () => {
  const LOCAL: TerminalHealth = {
    reachable: 'ok',
    session: 'present',
    process: 'running',
    poller: 'live',
    loads: 'unknown',
  };

  it('drops the chip, names the axis, and explains itself on hover', () => {
    const html = render(LOCAL, undefined, 'local');
    expect(html).not.toContain('data-testid="chip-reachable"');
    expect(html).not.toContain('data-testid="chip-loads"');
    const marker = /<li[^>]*data-testid="chip-na"[^>]*>/.exec(html)?.[0] ?? '';
    expect(marker).toContain('data-value="reachable,loads"');
    expect(marker).toContain('there is no hop to be reachable over');
    expect(marker).toContain('nothing has ever scraped a local Worker');
  });

  it('does the same for docker, and nothing at all for fleet', () => {
    expect(row(render(LOCAL, undefined, 'docker'))).toContain('n/a: reachable, loads');
    expect(row(render(LOCAL, undefined, 'fleet'))).not.toContain('n/a');
    // An unrecognised mode — a streamer from a later build — gets every chip rather than a silently
    // short row. Omission must never be the default for something this build does not understand.
    expect(row(render(LOCAL, undefined, 'kubernetes'))).not.toContain('n/a');
  });

  it('SHOWS AN INAPPLICABLE AXIS THE MOMENT IT FAILS — omission is one-directional', () => {
    // The whole safety argument. If applicability could hide a `bad` reading, this would be the
    // collapse ADR 0020 forbids, and the round-3 shape (one signal failing among four fine ones)
    // would be hideable. It is not: a failing axis is rendered whatever its mode says.
    const failing = render({ ...LOCAL, loads: 'failing' }, undefined, 'local');
    expect(chip(failing, 'loads')).toMatchObject({ state: 'bad', value: 'failing' });
    expect(row(failing)).toContain('▲ loads: FAILING');
    expect(row(failing)).toContain('n/a: reachable');

    const unreachable = render({ ...LOCAL, reachable: 'fail' }, undefined, 'local');
    expect(chip(unreachable, 'reachable').state).toBe('bad');
    // …and it is called what actually carried the probe. `sh -c` on this host is not ssh, and the
    // chip was the last place in the codebase still saying it was.
    expect(row(unreachable)).toContain('▲ shell: UNREACHABLE');
  });

  it('never renders a dropped axis as healthy: the marker is a fourth word, not a fourth state', () => {
    const html = render(LOCAL, undefined, 'local');
    // A spec — or an operator — counting green chips must not count this one.
    expect(html).not.toMatch(/data-testid="chip-na"[^>]*data-state="ok"/);
    const marker = /<li[^>]*data-testid="chip-na"[^>]*>/.exec(html)?.[0] ?? '';
    expect(marker).toContain('data-state="n/a"');
    // Muted and borderless — it borrows none of the four channels the tri-state uses, so it cannot
    // be mistaken for a reading at a glance across a wall.
    expect(marker).not.toContain('emerald');
    expect(marker).toContain('text-muted-foreground');
    expect(marker).toContain('border-transparent');
  });
});
