/**
 * A sidecar that can be resized, collapsed and moved to the other edge.
 *
 * THIN, LIKE `PaneResizer`. Every decision is a pure function in `sideDock.ts`; this file is the
 * four things that cannot be pure — `localStorage`, pointer capture, a DOM element to grab, and the
 * rail a collapsed panel leaves behind.
 *
 * POINTER CAPTURE RATHER THAN WINDOW LISTENERS, for `PaneResizer`'s reason: a drag that leaves the
 * handle is every drag, because the handle is six pixels wide, and `setPointerCapture` keeps the
 * moves coming without a global listener that outlives the gesture. It also ends the drag on
 * `pointerup` ANYWHERE, including outside the window.
 *
 * A REAL SEPARATOR, not a styled div: `role="separator"` with `aria-valuenow` is what makes the
 * arrow keys expected rather than surprising, and a layout that can only be changed with a mouse is
 * one an operator on a trackpad cannot change at all.
 *
 * THE RAIL IS NOT DECORATION. Collapsing to nothing leaves an edge nobody can find again; the rail
 * carries the panel's own name, reads vertically, and is the click that brings it back.
 */

import { useCallback, useEffect, useRef, useState } from 'react';
import {
  clampSideWidth,
  dragWidth,
  flipSide,
  keyWidth,
  parseSideDock,
  sideDockKey,
  SIDE_BOUNDS,
  type DockSide,
  type SideBounds,
  type SideDockState,
} from './sideDock';

export interface SideDock extends SideDockState {
  setWidth(px: number): void;
  setSide(side: DockSide): void;
  flip(): void;
  setCollapsed(collapsed: boolean): void;
  toggle(): void;
}

/**
 * One sidecar's layout, persisted.
 *
 * READ ONCE, LAZILY — a hook body runs on every render and this touches storage. Writes are
 * best-effort and silent, on `usePaneHeight`'s reasoning: a layout that stops persisting costs one
 * drag on the next reload, and a banner about it would be noise on a page whose warnings need to
 * mean something.
 */
export function useSideDock(
  pane: string,
  fallback: SideDockState,
  bounds: SideBounds = SIDE_BOUNDS
): SideDock {
  const [state, set] = useState<SideDockState>(() => {
    try {
      return parseSideDock(globalThis.localStorage?.getItem(sideDockKey(pane)) ?? null, fallback, bounds);
    } catch {
      // Storage can throw on ACCESS, not only on the value — a browser with site data blocked. The
      // page still has to come up.
      return { ...fallback, width: clampSideWidth(fallback.width, bounds) };
    }
  });

  useEffect(() => {
    try {
      globalThis.localStorage?.setItem(sideDockKey(pane), JSON.stringify(state));
    } catch {
      /* see above */
    }
  }, [pane, state]);

  const setWidth = useCallback(
    (px: number) => set((prev) => ({ ...prev, width: clampSideWidth(px, bounds) })),
    [bounds]
  );
  const setSide = useCallback((side: DockSide) => set((prev) => ({ ...prev, side })), []);
  const flip = useCallback(() => set((prev) => ({ ...prev, side: flipSide(prev.side) })), []);
  const setCollapsed = useCallback((collapsed: boolean) => set((prev) => ({ ...prev, collapsed })), []);
  const toggle = useCallback(() => set((prev) => ({ ...prev, collapsed: !prev.collapsed })), []);

  return { ...state, setWidth, setSide, flip, setCollapsed, toggle };
}

export interface SideResizerProps {
  /** The sidecar's current width, in pixels — the value this handle moves. */
  width: number;
  onWidth(px: number): void;
  /** Which edge the sidecar is docked to. It decides which way the pointer widens it. */
  side: DockSide;
  /** What is being resized, for the screen reader and the tooltip: "the workflow list". */
  label: string;
  bounds?: SideBounds;
  testid?: string;
}

export function SideResizer({
  width,
  onWidth,
  side,
  label,
  bounds = SIDE_BOUNDS,
  testid,
}: SideResizerProps): JSX.Element {
  const [dragging, setDragging] = useState(false);
  /** Where the gesture started. Anchoring to it is what keeps the panel edge under the pointer after
   *  a drag has pinned it against a bound — see `dragWidth`. */
  const origin = useRef<{ x: number; width: number } | null>(null);

  return (
    <div
      role="separator"
      aria-orientation="vertical"
      aria-label={`resize ${label}`}
      aria-valuenow={width}
      aria-valuemin={bounds.min}
      aria-valuemax={bounds.max}
      tabIndex={0}
      data-testid={testid}
      data-dragging={dragging ? 'true' : undefined}
      title={`Drag to resize ${label} — or focus and use ← →`}
      className={`group relative w-1.5 shrink-0 cursor-col-resize touch-none self-stretch border-x border-transparent bg-border/40 outline-none transition-colors hover:bg-primary/40 focus-visible:bg-primary/60 ${
        dragging ? 'bg-primary/60' : ''
      }`}
      onPointerDown={(e) => {
        // Primary button only: a right-click on a handle is a context menu, not a drag.
        if (e.button !== 0) return;
        e.preventDefault();
        e.currentTarget.setPointerCapture(e.pointerId);
        origin.current = { x: e.clientX, width };
        setDragging(true);
      }}
      onPointerMove={(e) => {
        const from = origin.current;
        if (!from) return;
        onWidth(dragWidth(from.width, e.clientX - from.x, side, bounds));
      }}
      onPointerUp={(e) => {
        if (e.currentTarget.hasPointerCapture(e.pointerId)) {
          e.currentTarget.releasePointerCapture(e.pointerId);
        }
        origin.current = null;
        setDragging(false);
      }}
      onPointerCancel={() => {
        origin.current = null;
        setDragging(false);
      }}
      onKeyDown={(e) => {
        const next = keyWidth(width, e.key, side, bounds);
        if (next === null) return;
        // The page scrolls sideways on an arrow key otherwise, which moves the thing being resized
        // out from under the person resizing it.
        e.preventDefault();
        onWidth(next);
      }}
    >
      {/* The grip. Invisible until the handle is worth noticing, because a permanent line down the
          side of every panel is chrome on a page that is mostly content. */}
      <span className="pointer-events-none absolute left-1/2 top-1/2 h-8 w-0.5 -translate-x-1/2 -translate-y-1/2 rounded-full bg-muted-foreground/0 transition-colors group-hover:bg-muted-foreground/50 group-focus-visible:bg-muted-foreground/70" />
    </div>
  );
}

/**
 * What a collapsed sidecar leaves behind.
 *
 * It carries the panel's own name and a count when the caller has one, because the question a rail
 * has to answer is not "is something hidden here" but "is what is hidden worth the width" — the
 * same reasoning as the Monitor tree's closed button, which is the shape this generalises.
 */
export function SideRail({
  label,
  badge,
  onExpand,
  testid,
}: {
  label: string;
  /** A count or a word to show beside the name — what is in there, before it is opened. */
  badge?: string | number;
  onExpand(): void;
  testid?: string;
}): JSX.Element {
  return (
    <button
      type="button"
      data-testid={testid}
      title={`Show ${label} — it takes its width from the page, so what is beside it gets narrower`}
      className="flex w-7 shrink-0 cursor-pointer items-center justify-center self-stretch border-r border-border bg-transparent px-0 text-[11px] text-muted-foreground outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/50"
      onClick={onExpand}
    >
      <span className="[writing-mode:vertical-rl]">
        ☰ {label}
        {badge === undefined ? '' : ` ${badge}`}
      </span>
    </button>
  );
}

/**
 * The collapse and dock-side controls, for a sidecar's own header.
 *
 * BOTH, TOGETHER, ALWAYS. A panel that can be collapsed but not moved is still stuck; one that can
 * be moved but not collapsed still costs its width on a narrow screen. They are one control group
 * so a reader learns the pair once and finds it in the same place on every surface.
 */
export function SideDockControls({
  dock,
  label,
  testid,
}: {
  dock: SideDock;
  label: string;
  testid?: string;
}): JSX.Element {
  return (
    <span className="ml-auto flex shrink-0 items-center gap-0.5">
      <button
        type="button"
        data-testid={testid ? `${testid}-flip` : undefined}
        title={`Move ${label} to the ${flipSide(dock.side)} of the page`}
        className="rounded px-1 text-[11px] leading-5 text-muted-foreground outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/50"
        onClick={dock.flip}
      >
        {dock.side === 'left' ? '⇥' : '⇤'}
      </button>
      <button
        type="button"
        data-testid={testid ? `${testid}-collapse` : undefined}
        title={`Hide ${label} — the rail it leaves brings it back, at this width`}
        className="rounded px-1 text-[11px] leading-5 text-muted-foreground outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/50"
        onClick={() => dock.setCollapsed(true)}
      >
        ✕
      </button>
    </span>
  );
}
