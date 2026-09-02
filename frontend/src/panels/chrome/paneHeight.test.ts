import { describe, expect, it } from 'vitest';
import {
  clampPaneHeight,
  dragHeight,
  fitBounds,
  keyHeight,
  paneHeightKey,
  parsePaneHeight,
  PANE_BOUNDS,
  PANE_RESERVE,
  PANE_STEP,
} from './paneHeight';

/**
 * The arithmetic behind the two panes that used to be pinned heights — the Catalog's editor
 * (`h-[420px]`) and the Runs event log (`h-[352px]`).
 *
 * Its bugs are the kind that are obvious in use and invisible in review: a pane that runs away from
 * the cursor, or one that will not start growing again after being dragged to its floor.
 */

describe('which way the pointer grows a pane', () => {
  it('grows a pane whose handle is BELOW it as the pointer moves down', () => {
    // The Catalog's editor: the handle sits at its bottom edge.
    expect(dragHeight(400, 60, 'down')).toBe(460);
    expect(dragHeight(400, -60, 'down')).toBe(340);
  });

  it('SHRINKS a pane whose handle is ABOVE it as the pointer moves down', () => {
    // The Runs event log: the handle sits at its top edge, so down means "give the space back to
    // the panels above". Getting this backwards is a pane that runs away from the cursor.
    expect(dragHeight(400, 60, 'up')).toBe(340);
    expect(dragHeight(400, -60, 'up')).toBe(460);
  });
});

describe('a drag is anchored to where it started', () => {
  it('follows the pointer back out of a bound without a dead zone', () => {
    // The bug this refuses. Accumulating per-move deltas keeps adding to a running total while the
    // pane is pinned at `min`, so a drag 400 px past the floor has to travel 400 px back before the
    // pane moves at all. Anchoring to the gesture's origin means the pane is where the pointer says.
    const start = 200;
    expect(dragHeight(start, -400, 'down')).toBe(PANE_BOUNDS.min); // pinned
    // One pixel back off the floor, from the same origin — and it moves immediately.
    expect(dragHeight(start, -79, 'down')).toBe(121);
  });
});

describe('a pane cannot be dragged out of existence', () => {
  it('clamps at both ends', () => {
    expect(dragHeight(400, -5000, 'down')).toBe(PANE_BOUNDS.min);
    expect(dragHeight(400, 5000, 'down')).toBe(PANE_BOUNDS.max);
    // Neither end is recoverable by looking at the thing that broke: a zero-height pane gives an
    // operator nothing to grab, and one past the viewport pushes the page off screen.
    expect(PANE_BOUNDS.min).toBeGreaterThan(0);
  });

  it('refuses a height that is not a number', () => {
    // Both fall to the floor rather than one falling to the ceiling: neither is a measurement, and
    // a garbage value that produced a 900-pixel pane would look like a deliberate layout.
    expect(clampPaneHeight(Number.NaN)).toBe(PANE_BOUNDS.min);
    expect(clampPaneHeight(Number.POSITIVE_INFINITY)).toBe(PANE_BOUNDS.min);
  });

  it('is always an integer, because it becomes a pixel', () => {
    expect(clampPaneHeight(420.7)).toBe(421);
    expect(Number.isInteger(dragHeight(400, 0.5, 'down'))).toBe(true);
  });
});

describe('the keyboard', () => {
  it('moves a pane by one step, in the direction the key points', () => {
    expect(keyHeight(400, 'ArrowDown', 'down')).toBe(400 + PANE_STEP);
    expect(keyHeight(400, 'ArrowUp', 'down')).toBe(400 - PANE_STEP);
    // A pane whose handle is above it grows on the key that points away from the handle — the same
    // gesture as the drag.
    expect(keyHeight(400, 'ArrowUp', 'up')).toBe(400 + PANE_STEP);
  });

  it('leaves every other key alone', () => {
    // Returning a height for Tab or Enter would swallow the key from the surface underneath.
    expect(keyHeight(400, 'Enter', 'down')).toBeNull();
    expect(keyHeight(400, 'Tab', 'down')).toBeNull();
    expect(keyHeight(400, 'ArrowLeft', 'down')).toBeNull();
  });

  it('clamps like a drag does', () => {
    expect(keyHeight(PANE_BOUNDS.min, 'ArrowUp', 'down')).toBe(PANE_BOUNDS.min);
    expect(keyHeight(PANE_BOUNDS.max, 'ArrowDown', 'down')).toBe(PANE_BOUNDS.max);
  });
});

describe('what is remembered', () => {
  it('keys each pane separately', () => {
    // A shared key is a resize on one page that moves a pane on another.
    expect(paneHeightKey('catalog-editor')).not.toBe(paneHeightKey('run-event-log'));
    expect(paneHeightKey('catalog-editor')).toContain('catalog-editor');
  });

  it('falls back rather than throwing on anything unusable', () => {
    expect(parsePaneHeight(null, 420)).toBe(420);
    expect(parsePaneHeight('', 420)).toBe(420);
    expect(parsePaneHeight('not a number', 420)).toBe(420);
    expect(parsePaneHeight('0', 420)).toBe(420);
    expect(parsePaneHeight('-30', 420)).toBe(420);
  });

  it('clamps a stored height, so bounds that tighten cannot strand a pane', () => {
    expect(parsePaneHeight('99999', 420)).toBe(PANE_BOUNDS.max);
    expect(parsePaneHeight('4', 420)).toBe(PANE_BOUNDS.min);
  });

  it('reads back what it wrote', () => {
    expect(parsePaneHeight(String(dragHeight(352, 100, 'up')), 352)).toBe(252);
  });
});

/**
 * A PANE MUST FIT THE WINDOW IT IS IN, and `PANE_BOUNDS.max` could not express that.
 *
 * MEASURED on the live control plane at 1280x800: the Workflows editor stood at 428px with its top
 * at y=465, so its bottom edge was at 893 — 93px below the fold — while `document.body.scrollHeight`
 * was exactly 800, because every ancestor is `overflow-hidden` and the page does not scroll. The
 * bottom of the editor, its own resize handle, and both panes under it were unreachable by any
 * gesture. It presents as "I cannot scroll the code editor"; the editor's scroller was healthy
 * throughout (`overflow-y: auto`, scrollHeight 3586 over a 428px client, wheel moving it).
 *
 * The module header already said a pane "dragged past the viewport pushes everything below it off
 * screen". A constant of 900 cannot enforce that on an 800px window — which is every laptop.
 */
describe('fitting the window', () => {
  it('caps the max at what the window can spare, not at the constant', () => {
    expect(fitBounds(800).max).toBe(800 - PANE_RESERVE);
    expect(fitBounds(800).max).toBeLessThan(PANE_BOUNDS.max);
  });

  it('leaves the constant alone on a window tall enough for it', () => {
    expect(fitBounds(1440).max).toBe(PANE_BOUNDS.max);
  });

  it('never lets max fall under min, which would collapse the pane instead of capping it', () => {
    // A max below a min makes `clampPaneHeight` return the max — so a 200px window would pin every
    // pane to 40px. Too tall for a tiny window is the lesser fault: it is visible, and it is what
    // the operator asked for.
    const tiny = fitBounds(200);
    expect(tiny.max).toBe(PANE_BOUNDS.min);
    expect(tiny.max).toBeGreaterThanOrEqual(tiny.min);
    expect(clampPaneHeight(500, tiny)).toBe(PANE_BOUNDS.min);
  });

  it('is a no-op when the viewport is unknown, rather than clamping to nonsense', () => {
    // Server render, or a jsdom-less test: `window.innerHeight` is 0 and there is nothing to fit.
    expect(fitBounds(0)).toEqual(PANE_BOUNDS);
    expect(fitBounds(Number.NaN)).toEqual(PANE_BOUNDS);
  });

  it('pulls a stored height that no longer fits back INSIDE the window', () => {
    // The path that actually fires: nobody drags a pane off screen deliberately, they reopen the
    // app on a smaller display than the one they set it on.
    const stored = parsePaneHeight('880', 460);
    expect(stored).toBe(880);
    expect(clampPaneHeight(stored, fitBounds(800))).toBe(640);
  });

  it('keeps a drag inside the window too', () => {
    expect(dragHeight(600, 400, 'down', fitBounds(800))).toBe(640);
  });
});
