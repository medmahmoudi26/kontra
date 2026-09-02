/**
 * WHERE THE CHROME SITS ON A TILE — asserted as an order, because that is the whole complaint.
 *
 * "And put the green banner on the bottom, like tmux." The status line was correct in every other
 * respect and in the one place tmux never draws it: row 2 of the tile header. Nothing about a
 * `data-tone`, a `data-feed` or a health signal could have failed for that, and the browser suite
 * does not select on the tile's internal order either. So this file renders the whole tile and reads
 * its chrome top to bottom.
 *
 * IT ALSO GUARDS THE HEIGHT BUDGET, which is the reason the bar could not simply be ADDED at the
 * bottom. `grid/wall.ts`'s `WALL_ROW_PX` is measured against the chrome a tile carries — a third row
 * costs every tile on the wall a line of a Worker's output — so the header had to give its second
 * row back in the same change. "One status line, and it is at the bottom" is therefore two
 * assertions, not one, and the second is the one a careless future edit will break.
 *
 * `renderToStaticMarkup`, not a DOM: this package's vitest runs `environment: 'node'`. Effects do not
 * run, so xterm never mounts — which is fine, because everything asserted here is chrome the tile
 * renders on the first pass. The screen itself is proven in a browser by the Playwright suite.
 */

import { describe, expect, it, vi } from 'vitest';

// @xterm/addon-fit ships a UMD bundle whose wrapper reads `self`; see `terminalTile.test.ts`.
vi.hoisted(() => {
  (globalThis as unknown as { self?: unknown }).self ??= globalThis;
});

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import TerminalTile, { type TerminalTileProps } from './TerminalTile';
import type { Terminal } from './panelsClient';

const ID = 'local:localhost/nscheck-0_1_0/actor';

const TERMINAL: Terminal = {
  id: ID,
  machine: 'localhost',
  host: 'localhost',
  publicIp: '',
  tag: '',
  fleet: '',
  actor: 'nscheck',
  version: '0.1.0',
  window: 'actor',
  command: 'zsh',
  paneCols: 80,
  paneRows: 24,
  exitStatus: '',
  // Verbatim from `GET /api/panels/terminals` on this box: a local pane whose queue nothing on this
  // host is polling. It is the tile that produced the complaint, and the one whose chips are red.
  health: {
    reachable: 'ok',
    session: 'present',
    process: 'unknown',
    poller: 'none',
    loads: 'unknown',
  },
};

function render(over: Partial<TerminalTileProps> = {}): string {
  return renderToStaticMarkup(
    createElement(TerminalTile, {
      terminal: TERMINAL,
      mode: 'snapshot',
      subscribe: () => () => {},
      onFocus: () => {},
      onBlur: () => {},
      theme: {},
      fontSize: 12,
      paletteIsDark: true,
      ...over,
    })
  );
}

/** The tile's chrome, top to bottom, by test hook — what an operator's eye travels down. */
function chrome(html: string): string[] {
  const hooks = ['tile-header-', 'health-', 'terminal-', 'tile-status-'];
  return [...html.matchAll(/data-testid="([^"]*)"/g)]
    .map((m) => m[1] ?? '')
    .filter((id) => hooks.some((h) => id.startsWith(h)))
    .map((id) => id.replace(ID, '<id>'));
}

describe('the status line is at the FOOT of the tile, like tmux', () => {
  it('renders header, screen, bar — in that order', () => {
    // Two moves are folded into this one expectation, in the order they happened.
    //
    // First the bar went to the bottom: it was `tile-header`, `tile-status`, `health`, `terminal`
    // — the bar SECOND, above the chips, above the screen. tmux draws its status line under the
    // pane and an operator reading a wall looks for it there.
    //
    // Then the chip row went entirely, on the operator's instruction, so `health-<id>` is no longer
    // between the header and the screen. What it used to say is not lost: the bar below names the
    // leading finding through the same `leadingFinding` derivation, and the drawer still lists every
    // signal separately (ADR 0020 asks that the signals stay independent, not that they stay on
    // screen). The test three below pins that the bar really does carry it.
    expect(chrome(render())).toEqual([
      'tile-header-<id>',
      'terminal-<id>',
      'tile-status-<id>',
    ]);
  });

  it('is NOT inside the header any more, and the header is one row', () => {
    // The height half of the move. If the bar were added at the bottom while the header kept two
    // rows, the tile would be 15 px taller than `WALL_ROW_PX` was measured against — which is a line
    // of output gone from every tile on the wall, silently.
    const html = render();
    // The header is everything up to the screen. It used to be sliced at the chip row, which no
    // longer exists — anchoring on the terminal is also simply more durable, since the screen is the
    // one thing on a tile that can never be removed.
    const header = html.slice(
      html.indexOf('data-testid="tile-header-'),
      html.indexOf(`data-testid="terminal-${ID}"`)
    );
    expect(header).not.toContain('tile-status-');
    expect(header).not.toContain('tile-feed-');
    // One row inside the header, not two.
    expect(header.match(/<div class="flex min-w-0 items-center/g) ?? []).toHaveLength(1);
    // And exactly one bar on the tile, whatever else moved.
    expect(html.match(/data-testid="tile-status-/g)).toHaveLength(1);
  });

  it('spends no row on the health chips, which the tile no longer draws at all', () => {
    // The row cost every tile a line of output to report `session: present`, `process: unknown` and
    // `n/a: reachable, loads` — three restatements of what a healthy pane always is. Asserted as an
    // ABSENCE so that re-adding it has to come through this file and past the sentence above.
    expect(render()).not.toContain('data-testid="health-');
  });

  it('draws it AFTER the screen and outside it, never over the newest output', () => {
    const html = render();
    expect(html.indexOf('data-testid="tile-status-')).toBeGreaterThan(
      html.indexOf(`data-testid="terminal-${ID}"`)
    );
    // Not an overlay: the screen's last line is a Worker's newest output, and a band floating on it
    // would cover exactly the row somebody is reading.
    expect(html).not.toMatch(/absolute[^"]*"[^>]*data-testid="tile-status-/);
  });

  it('a bare tile has no bar, for the same reason it has no banner', () => {
    // `WorkerPane` carries its own identity and controls; a second bar saying where it is would cost
    // the rows of output `bare` exists to save.
    const html = render({ bare: true });
    expect(chrome(html)).toEqual(['terminal-<id>']);
  });
});

describe('the band carries the finding the chip row used to, now that it is the only one saying it', () => {
  it('goes amber and NAMES the failing signal, in the words the chips used', () => {
    // This began as "the bar must not be green while the chips above it are red" — the real payload
    // from this box, where the chips led with `▲ poller: NONE` under a solid green bar.
    //
    // With the chip row gone the test matters MORE, not less: the bar is now the only thing on the
    // tile that reports a health signal at all, so if it were to go back to being unconditionally
    // green, a broken pane would look exactly like a healthy one and nothing on the wall would say
    // otherwise. That is the ADR 0020 failure this whole plane exists to prevent.
    const html = render();
    expect(html).toContain('data-tone="warn"');
    expect(html).toContain('data-finding="poller"');
    // Named, not merely coloured — the words are the ones the chip row used, because both come from
    // the single `leadingFinding` derivation.
    expect(html).toContain('▲ poller: NONE');
  });

  it('is green only when there is nothing to report', () => {
    const html = render({
      terminal: {
        ...TERMINAL,
        health: { reachable: 'ok', session: 'present', process: 'running', poller: 'live', loads: 'ok' },
      },
    });
    expect(html).toContain('data-tone="ok"');
    expect(html).toContain('bg-green-600 text-black');
    expect(html).not.toContain('data-testid="tile-finding-');
  });
});
