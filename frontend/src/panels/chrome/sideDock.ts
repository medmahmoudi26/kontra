/**
 * A sidecar's width, its side, and whether it is collapsed — as arithmetic.
 *
 * EVERY SIDECAR ON THIS APP WAS A PINNED WIDTH. The Workflows list was `w-[264px]`, the Scratch
 * inspector `w-[304px]`, the Monitor tree a constant in `theme.ts` — three numbers chosen once,
 * against one screen, for three panels whose right size depends entirely on what is in them. A path
 * like `/root/kontra-local/.claude/worktrees/workflows-client/.kontra/workflows/nscheck` wraps to
 * four lines in 264 pixels, and no amount of reading it does anything about that.
 *
 * `paneHeight.ts` IS THE MODEL AND NOT THE IMPLEMENTATION. The two share a shape — clamp, drag from
 * the gesture's origin, arrow keys, a namespaced key in storage — and they differ in the axis, the
 * bounds, and the fact that a sidecar also has a SIDE and a COLLAPSED state that a pane does not.
 * Folding them into one generic module would put an `axis` parameter through every signature to
 * save forty lines of arithmetic, and the direction bug this file exists to prevent is exactly the
 * kind that hides behind a parameter.
 *
 * THE SIDE IS THE WHOLE OF THE COMPLEXITY, the way the handle's edge is in `paneHeight.ts`. A
 * sidecar docked LEFT has its handle on its right edge and grows as the pointer moves right; docked
 * RIGHT the same pointer movement shrinks it. Get it backwards and the panel runs away from the
 * cursor — obvious in use, invisible in review, so it is a value and it is tested.
 *
 * COLLAPSED IS NOT WIDTH ZERO. A panel dragged to nothing is a panel an operator has to know to drag
 * back from an invisible edge; collapsing leaves a rail that says what it is and brings it back with
 * one click, and the width it had is still there when it returns.
 */

/** Which edge of the page a sidecar is docked to. */
export type DockSide = 'left' | 'right';

export interface SideBounds {
  min: number;
  max: number;
}

/** Narrow enough to still be a sidecar, wide enough to hold a path. Both are pixels. */
export const SIDE_BOUNDS: SideBounds = { min: 180, max: 640 };

/** One keyboard press. Coarse enough to be worth pressing, fine enough to land somewhere chosen. */
export const SIDE_STEP = 24;

/** What a sidecar remembers between visits. */
export interface SideDockState {
  width: number;
  side: DockSide;
  collapsed: boolean;
}

export function clampSideWidth(px: number, bounds: SideBounds = SIDE_BOUNDS): number {
  if (!Number.isFinite(px)) return bounds.min;
  return Math.round(Math.min(Math.max(px, bounds.min), bounds.max));
}

/**
 * The width a drag has reached: where it started, plus how far the pointer has moved, signed by the
 * side the panel is docked to.
 *
 * FROM THE START OF THE GESTURE, not from the last frame — `paneHeight.ts`'s reason, unchanged.
 * Accumulating per-move deltas drifts against the clamp: once a drag pins the panel at `min`, every
 * further pixel is still added to a running total and the panel does not start growing again until
 * the pointer has travelled all the way back.
 */
export function dragWidth(
  startWidth: number,
  deltaX: number,
  side: DockSide,
  bounds: SideBounds = SIDE_BOUNDS
): number {
  return clampSideWidth(startWidth + (side === 'left' ? deltaX : -deltaX), bounds);
}

/**
 * One arrow key.
 *
 * The key is a DIRECTION ON SCREEN, not a direction on the panel: ArrowRight moves the handle right,
 * which widens a left-docked panel and narrows a right-docked one. Any other reading makes the same
 * key do opposite things on two panels of one page.
 */
export function keyWidth(
  width: number,
  key: string,
  side: DockSide,
  bounds: SideBounds = SIDE_BOUNDS
): number | null {
  const towards = key === 'ArrowRight' ? 1 : key === 'ArrowLeft' ? -1 : 0;
  if (towards === 0) return null;
  return dragWidth(width, towards * SIDE_STEP, side, bounds);
}

/** The other side. */
export function flipSide(side: DockSide): DockSide {
  return side === 'left' ? 'right' : 'left';
}

/** Where one sidecar's layout is remembered. Namespaced per panel, because two sharing a key is a
 *  resize on one surface that moves a panel on another. */
export function sideDockKey(pane: string): string {
  return `kontra.side.${pane}`;
}

/**
 * A stored layout, or the caller's defaults when nothing usable is stored.
 *
 * NEVER THROWS, and never returns a partial. A corrupt value must cost the default layout, not the
 * page — and a half-parsed one (a width with no side) is how a panel comes back at the right size on
 * the wrong edge, which reads as the app having moved it by itself.
 */
export function parseSideDock(
  raw: string | null,
  fallback: SideDockState,
  bounds: SideBounds = SIDE_BOUNDS
): SideDockState {
  const base: SideDockState = { ...fallback, width: clampSideWidth(fallback.width, bounds) };
  if (raw === null) return base;
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return base;
  }
  if (typeof parsed !== 'object' || parsed === null) return base;
  const got = parsed as Partial<SideDockState>;
  return {
    width: typeof got.width === 'number' ? clampSideWidth(got.width, bounds) : base.width,
    side: got.side === 'left' || got.side === 'right' ? got.side : base.side,
    collapsed: typeof got.collapsed === 'boolean' ? got.collapsed : base.collapsed,
  };
}
