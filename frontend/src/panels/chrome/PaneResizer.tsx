/**
 * The drag handle between two panes, and the height it owns.
 *
 * THIN, LIKE `useTerminalStyle`. Every decision is a pure function in `paneHeight.ts`; this file is
 * the three things that cannot be pure — `localStorage`, pointer capture, and a DOM element to grab.
 *
 * POINTER CAPTURE RATHER THAN WINDOW LISTENERS. A drag that leaves the handle — which is every drag,
 * because the handle is six pixels tall — has to keep receiving moves, and `setPointerCapture` is
 * the API that says so without a global listener that outlives the gesture. It also means the drag
 * ends on `pointerup` ANYWHERE, including outside the window, which a `mouseup` on the handle does
 * not.
 *
 * IT IS A REAL SEPARATOR, not a styled div. `role="separator"` with `aria-valuenow` is what makes
 * the arrow keys expected rather than surprising, and a resize that can only be done with a mouse is
 * a layout an operator on a trackpad cannot change.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  clampPaneHeight,
  dragHeight,
  fitBounds,
  keyHeight,
  paneHeightKey,
  parsePaneHeight,
  PANE_BOUNDS,
  type PaneBounds,
  type PaneGrow,
} from './paneHeight';

/**
 * One pane's height, persisted.
 *
 * READ ONCE, LAZILY — a hook body runs on every render and this touches storage. Writes are
 * best-effort and silent, on `useTerminalStyle`'s reasoning: a height that stops persisting costs
 * one drag on the next reload, and a banner about it would be noise on a page whose warnings need to
 * mean something.
 */
export function usePaneHeight(
  pane: string,
  fallback: number,
  bounds: PaneBounds = PANE_BOUNDS
): [number, (px: number) => void] {
  /**
   * THE BOUNDS ARE THE WINDOW'S, NOT A CONSTANT'S. `PANE_BOUNDS.max` is 900px and a laptop is 800,
   * so the clamp that exists to keep a pane inside the viewport permitted a pane taller than the
   * viewport — and since every ancestor here is `overflow-hidden`, the page cannot scroll to
   * recover what that pushes off the bottom. See `PANE_RESERVE` for the measurement.
   *
   * Recomputed on resize, because the offending height is usually not one the operator just
   * dragged: it is one restored from storage, chosen on a taller screen, or left behind by a window
   * that has since been made shorter.
   */
  const [viewportH, setViewportH] = useState(() =>
    typeof window === 'undefined' ? 0 : window.innerHeight
  );
  useEffect(() => {
    if (typeof window === 'undefined') return;
    const onResize = () => setViewportH(window.innerHeight);
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
  }, []);
  const fitted = useMemo(() => fitBounds(viewportH, bounds), [viewportH, bounds]);

  const [height, set] = useState(() => {
    try {
      return parsePaneHeight(globalThis.localStorage?.getItem(paneHeightKey(pane)) ?? null, fallback, bounds);
    } catch {
      // Storage can throw on ACCESS, not only on the value — a browser with site data blocked. The
      // page still has to come up.
      return clampPaneHeight(fallback, bounds);
    }
  });

  useEffect(() => {
    try {
      globalThis.localStorage?.setItem(paneHeightKey(pane), String(height));
    } catch {
      /* see above */
    }
  }, [pane, height]);

  const setHeight = useCallback((px: number) => set(clampPaneHeight(px, fitted)), [fitted]);

  /* A stored height that no longer fits is pulled back INTO the window rather than left to overflow
     it. This is the path that actually fires for most people: nobody drags a pane off screen on
     purpose, they open the app on a smaller display than the one they set it on. */
  const capped = clampPaneHeight(height, fitted);
  return [capped, setHeight];
}

export interface PaneResizerProps {
  /** The pane's current height, in pixels — the value this handle moves. */
  height: number;
  onHeight(px: number): void;
  /** Which way the pointer moves to make the pane TALLER. See `paneHeight.ts`. */
  grow: PaneGrow;
  /** What is being resized, for the screen reader and the tooltip: "the editor", "the event log". */
  label: string;
  bounds?: PaneBounds;
  testid?: string;
}

export function PaneResizer({
  height,
  onHeight,
  grow,
  label,
  bounds: given = PANE_BOUNDS,
  testid,
}: PaneResizerProps): JSX.Element {
  const [dragging, setDragging] = useState(false);
  /* FITTED HERE TOO, not only in `usePaneHeight`. The hook clamps the value it stores, so a drag
     could not actually push a pane off screen — but this component reports `aria-valuemax` and runs
     the drag arithmetic, and a handle that announces a maximum 100px taller than the window can
     reach is a handle that lies to a screen reader and feels dead for the last stretch of a drag. */
  const [viewportH, setViewportH] = useState(() =>
    typeof window === 'undefined' ? 0 : window.innerHeight
  );
  useEffect(() => {
    if (typeof window === 'undefined') return;
    const onResize = () => setViewportH(window.innerHeight);
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
  }, []);
  const bounds = useMemo(() => fitBounds(viewportH, given), [viewportH, given]);
  /** Where the gesture started. Anchoring to it is what keeps the pane under the pointer after a
   *  drag has pinned it against a bound — see `dragHeight`. */
  const origin = useRef<{ y: number; height: number } | null>(null);

  return (
    <div
      role="separator"
      aria-orientation="horizontal"
      aria-label={`resize ${label}`}
      aria-valuenow={height}
      aria-valuemin={bounds.min}
      aria-valuemax={bounds.max}
      tabIndex={0}
      data-testid={testid}
      data-dragging={dragging ? 'true' : undefined}
      title={`Drag to resize ${label} — or focus and use ↑ ↓`}
      className={`group relative h-1.5 shrink-0 cursor-row-resize touch-none border-y border-transparent bg-border/40 outline-none transition-colors hover:bg-primary/40 focus-visible:bg-primary/60 ${
        dragging ? 'bg-primary/60' : ''
      }`}
      onPointerDown={(e) => {
        // Primary button only: a right-click on a handle is a context menu, not a drag.
        if (e.button !== 0) return;
        e.preventDefault();
        e.currentTarget.setPointerCapture(e.pointerId);
        origin.current = { y: e.clientY, height };
        setDragging(true);
      }}
      onPointerMove={(e) => {
        const from = origin.current;
        if (!from) return;
        onHeight(dragHeight(from.height, e.clientY - from.y, grow, bounds));
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
        const next = keyHeight(height, e.key, grow, bounds);
        if (next === null) return;
        // The page scrolls on an arrow key otherwise, which moves the thing being resized out from
        // under the person resizing it.
        e.preventDefault();
        onHeight(next);
      }}
    >
      {/* The grip. Invisible until the handle is worth noticing, because a permanent line between
          every pair of panes is chrome on a page that is mostly content. */}
      <span className="pointer-events-none absolute left-1/2 top-1/2 h-0.5 w-8 -translate-x-1/2 -translate-y-1/2 rounded-full bg-muted-foreground/0 transition-colors group-hover:bg-muted-foreground/50 group-focus-visible:bg-muted-foreground/70" />
    </div>
  );
}
