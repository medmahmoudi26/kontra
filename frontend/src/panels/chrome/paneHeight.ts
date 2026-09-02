/**
 * Drag-to-resize, as arithmetic.
 *
 * TWO PANES ON THIS APP WERE PINNED HEIGHTS. The Catalog's editor was `h-[420px]` and the Runs
 * event log was `h-[352px]` — numbers chosen once, against one screen, for two panes whose right
 * size depends entirely on what you are doing. Reading a traceback wants a tall editor; comparing
 * an event log against the rails above it wants a tall log; a 1080p laptop wants neither of the
 * numbers a 1440p monitor wanted.
 *
 * THE HANDLE'S SIDE IS THE WHOLE OF THE COMPLEXITY, which is why it is here and not in a component.
 * A handle at a pane's BOTTOM edge grows it as the pointer moves down; a handle at its TOP edge
 * shrinks it. Get that backwards and the pane runs away from the cursor — the kind of bug that is
 * obvious in use and invisible in review, so it is stated as a value and tested.
 *
 * CLAMPED AT BOTH ENDS. A pane dragged to zero is a pane an operator has to know to drag back, and
 * one dragged past the viewport pushes everything below it off screen. Neither is recoverable by
 * looking at the thing that broke.
 */

/** Which way the pointer has to move to make the pane TALLER. */
export type PaneGrow = 'down' | 'up';

export interface PaneBounds {
  min: number;
  max: number;
}

/** Small enough to still be a pane, tall enough to still be a page. Both are pixels. */
export const PANE_BOUNDS: PaneBounds = { min: 120, max: 900 };

/**
 * How much of the window a resizable pane must leave for everything under it.
 *
 * MEASURED, on the live control plane at 1280×800: the Workflows editor stood at 428px with its top
 * edge at y=465, so its bottom edge was at **893 — ninety-three pixels below an 800px viewport**.
 * And `document.body.scrollHeight` was exactly 800, because every ancestor is `overflow-hidden` by
 * design: THE PAGE DOES NOT SCROLL. So the bottom of the editor, its resize handle, and both panes
 * beneath it were not merely awkward to reach — they did not exist on screen and no gesture could
 * bring them back. It reads as "I cannot scroll the editor", and the editor's own scroller was
 * fine the whole time (`overflow-y: auto`, `scrollHeight` 3586 over a 428px client, wheel working).
 *
 * The header above already said a pane "dragged past the viewport pushes everything below it off
 * screen". `max: 900` did not enforce it: 900 is taller than a 800px window, so the clamp that was
 * supposed to prevent this permitted it on any laptop. A bound in absolute pixels cannot express a
 * rule about the viewport.
 *
 * 160px is the tab strip plus a few rows of whatever is beneath — enough that what is down there
 * announces itself, so you know to drag rather than wonder where it went.
 */
export const PANE_RESERVE = 160;

/**
 * {@link PANE_BOUNDS} narrowed to a window that may be shorter than the constant.
 *
 * `min` WINS WHEN THE WINDOW IS TINY. On a 200px-tall window the reserve would compute a max below
 * the min, and a max under a min makes `clampPaneHeight` return the max — collapsing the pane to
 * something unusable. A pane that is too tall for a very short window is the lesser fault: it is
 * visible, and it is what the operator asked for.
 */
export function fitBounds(viewportH: number, bounds: PaneBounds = PANE_BOUNDS): PaneBounds {
  if (!Number.isFinite(viewportH) || viewportH <= 0) return bounds;
  return { min: bounds.min, max: Math.max(bounds.min, Math.min(bounds.max, viewportH - PANE_RESERVE)) };
}

/** One keyboard press. Coarse enough to be worth pressing, fine enough to land somewhere chosen. */
export const PANE_STEP = 24;

export function clampPaneHeight(px: number, bounds: PaneBounds = PANE_BOUNDS): number {
  if (!Number.isFinite(px)) return bounds.min;
  return Math.round(Math.min(Math.max(px, bounds.min), bounds.max));
}

/**
 * The height a drag has reached: where it started, plus how far the pointer has moved, signed by
 * which edge the handle is on.
 *
 * FROM THE START OF THE GESTURE, not from the last frame. Accumulating per-move deltas drifts
 * against the clamp — once a drag pins the pane at `min`, every further pixel upward is still added
 * to a running total, and the pane does not start growing again until the pointer has travelled all
 * the way back. Anchoring to the gesture's origin means the pane is always exactly where the
 * pointer says it is.
 */
export function dragHeight(
  startHeight: number,
  deltaY: number,
  grow: PaneGrow,
  bounds: PaneBounds = PANE_BOUNDS
): number {
  return clampPaneHeight(startHeight + (grow === 'down' ? deltaY : -deltaY), bounds);
}

/** One arrow key. `up`/`down` are the KEY, not the edge — a pane grows on the key that points away
 *  from its handle, which is what every resizable pane in every editor does. */
export function keyHeight(
  height: number,
  key: string,
  grow: PaneGrow,
  bounds: PaneBounds = PANE_BOUNDS
): number | null {
  const towards = key === 'ArrowDown' ? 1 : key === 'ArrowUp' ? -1 : 0;
  if (towards === 0) return null;
  return dragHeight(height, towards * PANE_STEP, grow, bounds);
}

/** Where one pane's height is remembered. Namespaced per pane, because two panes sharing a key is a
 *  resize on one page that moves a pane on another. */
export function paneHeightKey(pane: string): string {
  return `kontra.pane.${pane}.height`;
}

/** A stored height, or the caller's default when nothing usable is stored. Never throws: a corrupt
 *  value must cost the default, not the page. */
export function parsePaneHeight(
  raw: string | null,
  fallback: number,
  bounds: PaneBounds = PANE_BOUNDS
): number {
  if (raw === null) return clampPaneHeight(fallback, bounds);
  const n = Number(raw);
  if (!Number.isFinite(n) || n <= 0) return clampPaneHeight(fallback, bounds);
  return clampPaneHeight(n, bounds);
}
