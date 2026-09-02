/**
 * The rule that makes a snapshot pane fill its tile — no wrapped lines, no dead columns.
 *
 * "There is still empty space, the text must automatically fill it. There seems to be some kind of
 * asymmetry in the dimensions."
 *
 * The asymmetry was real and was visible on one wall at once. A snapshot tile renders what
 * `capture-pane` drew at the FAR END's geometry, and the two ends never agreed: panes on the
 * controller measured 80x25, 115x44 and 200x50 while a default wall tile holds about 110 columns at
 * 12px. So the 200-column pane wrapped every long line and the 80-column pane left a third of its
 * tile blank — one tile too wide, its neighbour too narrow, neither filling.
 *
 * `addon-fit` alone cannot resolve that: it sizes the GRID to the box, which is only the right
 * answer when the grid is also what the far end draws at. So the far end's width becomes fixed and
 * the type scales to it.
 */

import { describe, expect, it, vi } from 'vitest';

// @xterm/addon-fit ships a UMD bundle whose wrapper reads `self`; see `terminalTile.test.ts`.
// Importing the tile at all pulls it in, even though nothing here touches the real addon.
vi.hoisted(() => {
  (globalThis as unknown as { self?: unknown }).self ??= globalThis;
});

import { fitSourceWidth } from './TerminalTile';

/**
 * A stand-in xterm + addon-fit pair.
 *
 * `fit()` re-derives `cols` from a fixed pixel width the way the real addon does — columns are
 * inversely proportional to font size — so a test asserts on the COLUMNS THAT RESULT rather than on
 * the font arithmetic. `fits` counts reflows, which is what the sub-pixel guard exists to avoid.
 */
function harness(widthPx: number, fontSize: number) {
  // The real addon's ratio for this font stack, near enough: 12px type gives a ~7.2px cell.
  const CELL_RATIO = 0.6;
  const term = { cols: 0, options: { fontSize } as { fontSize?: number } };
  let fits = 0;
  const fit = {
    fit() {
      fits += 1;
      // ROUNDED TO A WHOLE PIXEL, as xterm does — and this is the detail that matters. Model the
      // cell as continuous and a single `font * cols / sourceCols` lands exactly on the target, so
      // the test passes against an implementation that does not converge. With the rounding in,
      // one division falls short: a 200-column pane in a 666px tile settled at 162 columns on this
      // box, still wrapping by 38, which is what sent the loop into `fitSourceWidth`.
      const cell = Math.max(1, Math.round((term.options.fontSize ?? 1) * CELL_RATIO));
      term.cols = Math.max(1, Math.floor(widthPx / cell));
    },
  };
  fit.fit(); // measureNow always fits once before calling fitSourceWidth
  return { term, fit, fits: () => fits };
}

/** A default wall tile: 6 of 12 columns on a ~1600px canvas. */
const TILE_PX = 794;

describe('fitSourceWidth makes the source pane fill the tile', () => {
  it('shrinks the type so a 200-column pane stops wrapping in a 113-column tile', () => {
    const { term, fit } = harness(TILE_PX, 12);
    expect(term.cols).toBe(113); // what the tile held before — every line past 113 wrapped

    fitSourceWidth(term, fit, 200, 'snapshot');

    // The whole pane lands. Not `toBe(200)`: whole-pixel cells mean the grid can rarely be hit
    // exactly, and the asymmetry is deliberate — a column of slack is a sliver of unused tile,
    // a column short is every long line wrapping.
    expect(term.cols).toBeGreaterThanOrEqual(200);
    expect(term.options.fontSize).toBeLessThan(12);
  });

  it('grows the type so an 80-column pane stops leaving a third of the tile blank', () => {
    const { term, fit } = harness(TILE_PX, 12);
    expect(term.cols).toBe(113); // 33 columns of the tile showing nothing

    fitSourceWidth(term, fit, 80, 'snapshot');

    expect(term.options.fontSize).toBeGreaterThan(12);
    expect(term.cols).toBeGreaterThanOrEqual(80);
    // Most of the gap is closed, and the rest is the cell quantum: at this size one pixel of cell
    // width moves the grid by ~9 columns, and the nearer size wraps. Erring toward slack is the
    // same choice as above.
    expect(term.cols).toBeLessThan(92);
  });

  it('stops shrinking at the readability floor rather than scaling to nothing', () => {
    // A tile dragged down to the wall's minimum, showing a very wide pane.
    const { term, fit } = harness(200, 12);

    fitSourceWidth(term, fit, 400, 'snapshot');

    expect(term.options.fontSize).toBe(5);
    // It still wraps at that width — and that is the deliberate trade. A wrapped line you can read
    // beats an unwrapped one rendered at 2px, which is just a grey bar.
    expect(term.cols).toBeLessThan(400);
  });

  it('stops growing at the ceiling, so a narrow pane is not blown up into a mistake', () => {
    const { term, fit } = harness(TILE_PX, 12);

    fitSourceWidth(term, fit, 20, 'snapshot');

    expect(term.options.fontSize).toBe(22);
  });

  it('leaves a LIVE tile alone — its attach is stty’d to the tile’s own measurement', () => {
    // Scaling here would fight the re-attach: the source grid already IS the tile's grid.
    const { term, fit } = harness(TILE_PX, 12);
    const before = { cols: term.cols, fontSize: term.options.fontSize };

    fitSourceWidth(term, fit, 200, 'live');

    expect(term.cols).toBe(before.cols);
    expect(term.options.fontSize).toBe(before.fontSize);
  });

  it('leaves the fit alone when the streamer has not reported a pane width yet', () => {
    const { term, fit } = harness(TILE_PX, 12);
    const before = term.cols;

    fitSourceWidth(term, fit, 0, 'snapshot');

    expect(term.cols).toBe(before);
  });

  it('does not reflow for a sub-pixel correction', () => {
    const { term, fit, fits } = harness(TILE_PX, 12);
    const after = fits();

    // The tile already holds almost exactly the source width.
    fitSourceWidth(term, fit, term.cols, 'snapshot');

    expect(fits()).toBe(after);
  });

  it('is idempotent — measuring twice does not compound the scaling', () => {
    // The bug this guards is subtle and fatal: derive from the PREVIOUS derived size and every
    // `ResizeObserver` tick shrinks the type again until the pane is unreadable. `measureNow`
    // restores the requested size before each call; this proves the helper is safe under that.
    const { term, fit } = harness(TILE_PX, 12);
    fitSourceWidth(term, fit, 200, 'snapshot');
    const once = term.options.fontSize;

    term.options.fontSize = 12; // what measureNow does at the top of the next pass
    fit.fit();
    fitSourceWidth(term, fit, 200, 'snapshot');

    expect(term.options.fontSize).toBe(once);
  });
});
