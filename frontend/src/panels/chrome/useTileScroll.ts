/**
 * The wheel proxy and touch scrolling, wired to one tile's xterm (slice 7a).
 *
 * NON-PASSIVE, AND THAT IS WHY THIS IS A HOOK AND NOT AN `onWheel` PROP. React registers `wheel`,
 * `touchstart` and `touchmove` on its root container as PASSIVE listeners, so `preventDefault()` inside
 * a JSX `onWheel` is a console warning and a no-op — the browser scrolls anyway, and on a wall the thing
 * it scrolls is not the thing under the cursor. `addEventListener(..., { passive: false })` on the tile's
 * own wrapper is the only way to hold the default action, which is the entire mechanism: hold it, decide,
 * then move a `scrollTop` ourselves. `scroll.ts` holds the two decisions; this file is the plumbing.
 *
 * XTERM IS TOLD TO KEEP ITS HANDS OFF, via the public `attachCustomWheelEventHandler(() => false)`.
 * Without that there are two owners of one gesture: xterm's own wheel listener sits on the terminal
 * element, INSIDE our wrapper, so it runs first and would scroll the viewport before we got to decide —
 * and our proxy would then add a second scroll on top of it. Returning `false` from the custom handler is
 * xterm's documented "I handled it" and takes its whole wheel path off the table.
 *
 * WHICH ALSO REMOVES A BYTE PATH, and it is worth being precise about what was and was not there
 * before. Read from the shipped bundle: xterm's wheel listener, for a terminal with NO scrollback,
 * builds `ESC [ A`/`ESC [ B` from the wheel direction and calls `coreService.triggerDataEvent` — the
 * `onData` path. That was already inert here, because `triggerDataEvent`'s first line returns when
 * `disableStdin` is set (also read from the bundle) and because nothing subscribes `onData` and no
 * message in the outbound union carries bytes. So this is defence in depth, not a fix for a live bug:
 * ADR 0020's read-only guarantee is meant to rest on there being nothing to write to, and a code path
 * that assembles cursor keys at all is one `disableStdin: false` away from mattering. Now the branch is
 * unreachable.
 *
 * TOUCH IS THE SAME PROXY. `touchmove` follows the finger into the same `scrollTop`, and the flick
 * afterwards decays through `decayVelocity`. A tap is left alone: it must NOT go live (that takes the
 * header's own control — ADR 0020's cost model, and slice 2 amendment 9: never a click) and it must not
 * focus anything that could become a keyboard route into a read-only screen.
 */

import { useEffect, useMemo, useRef, type RefObject } from 'react';
import type { Terminal as XTerm } from '@xterm/xterm';
import {
  absorbableScroll,
  decayVelocity,
  flickVelocity,
  pushSample,
  wheelPixels,
  type TouchSample,
} from './scroll';

export interface UseTileScrollOptions {
  /** The tile's non-scrolling wrapper — where the listeners go. Non-scrolling on purpose: if this
   * element could scroll, the browser would have somewhere to put the delta before we saw it. */
  wrapperRef: RefObject<HTMLElement | null>;
  /** The tile's xterm, once it exists. */
  termRef: RefObject<XTerm | null>;
  /**
   * Told how far back the tile is scrolled, in lines, whenever it changes.
   *
   * The tile shows it, and that is not decoration: a tile scrolled up is showing OLD output while its
   * stream keeps arriving, which on a wall of near-identical journals is indistinguishable from a
   * Machine that stopped printing. Reported here because this hook is what moved the viewport.
   */
  onScrollback?(lines: number): void;
}

export interface TileScroll {
  /** Put the tile back at the tail. What the "jump to live output" control calls. */
  toBottom(): void;
}

/** The viewport xterm scrolls: `.xterm-viewport`, its `overflow-y: scroll` div. Looked up per event
 * rather than cached — xterm rebuilds its DOM on a renderer change, and a stale element would be a tile
 * that silently stops scrolling. */
function viewportOf(term: XTerm | null): HTMLElement | null {
  const el = term?.element;
  if (!el) return null;
  return el.querySelector<HTMLElement>('.xterm-viewport');
}

export function useTileScroll(options: UseTileScrollOptions): TileScroll {
  const { wrapperRef, termRef, onScrollback } = options;

  const onScrollbackRef = useRef(onScrollback);
  onScrollbackRef.current = onScrollback;
  /** Last reported scrollback depth, so a gesture that moves nothing reports nothing. */
  const reportedRef = useRef(-1);
  /**
   * The returned handle is STABLE and indirects through a ref.
   *
   * Returning the object the effect builds would have handed the caller the no-op that existed during
   * the first render and never the real one — a `toBottom` that silently does nothing is exactly the
   * class of bug this file is otherwise about.
   */
  const toBottomRef = useRef<() => void>(() => undefined);
  const api = useMemo<TileScroll>(() => ({ toBottom: () => toBottomRef.current() }), []);

  useEffect(() => {
    const wrapper = wrapperRef.current;
    if (!wrapper) return;

    let momentum: number | null = null;
    const samples: TouchSample[] = [];
    let lastTouchY = 0;

    /** Report how many LINES back the viewport is, derived from the pixels it actually holds. Lines
     * rather than pixels because that is the unit an operator thinks in and the unit the streamer's
     * geometry is in. */
    const report = (view: HTMLElement): void => {
      const term = termRef.current;
      const rows = term?.rows ?? 0;
      const behind = view.scrollHeight - view.clientHeight - view.scrollTop;
      const lineHeight = rows > 0 ? view.clientHeight / rows : 0;
      const lines = lineHeight > 0 ? Math.round(behind / lineHeight) : 0;
      if (lines === reportedRef.current) return;
      reportedRef.current = lines;
      onScrollbackRef.current?.(Math.max(0, lines));
    };

    const stopMomentum = (): void => {
      if (momentum !== null) {
        cancelAnimationFrame(momentum);
        momentum = null;
      }
    };

    const scrollBy = (pixels: number): boolean => {
      const view = viewportOf(termRef.current);
      if (!view) return false;
      const absorbable = absorbableScroll(view, pixels);
      if (absorbable === 0) return false;
      view.scrollTop += absorbable;
      report(view);
      return true;
    };

    const onWheel = (event: WheelEvent): void => {
      // A modifier wheel is the browser's zoom (and xterm's own fast-scroll modifier). Left alone, so
      // Ctrl+wheel still zooms the page rather than scrolling one tile by a screen.
      if (event.ctrlKey || event.metaKey || event.shiftKey) return;
      const view = viewportOf(termRef.current);
      if (!view) return;
      const term = termRef.current;
      const rows = term?.rows ?? 0;
      const lineHeight = rows > 0 ? view.clientHeight / rows : 16;
      const pixels = wheelPixels(event, { lineHeight, pageHeight: view.clientHeight });
      const absorbable = absorbableScroll(view, pixels);
      // THE HANDOFF. Nothing absorbable — the top of the scrollback going up, the tail going down, or a
      // snapshot tile with no scrollback at all — means the default action is left intact and the WALL
      // scrolls. Anything else is ours, and `preventDefault` is what stops the wall moving under a line
      // somebody is reading.
      if (absorbable === 0) return;
      event.preventDefault();
      stopMomentum();
      view.scrollTop += absorbable;
      report(view);
    };

    const onTouchStart = (event: TouchEvent): void => {
      if (event.touches.length !== 1) return;
      stopMomentum();
      const touch = event.touches[0];
      if (!touch) return;
      lastTouchY = touch.clientY;
      samples.length = 0;
      pushSample(samples, { t: event.timeStamp, y: touch.clientY });
    };

    const onTouchMove = (event: TouchEvent): void => {
      if (event.touches.length !== 1) return;
      const touch = event.touches[0];
      if (!touch) return;
      const deltaY = touch.clientY - lastTouchY;
      lastTouchY = touch.clientY;
      pushSample(samples, { t: event.timeStamp, y: touch.clientY });
      // Finger down means show me what came BEFORE, so the delta is negated. Same convention as the
      // reference implementation and as every native scroll view.
      if (!scrollBy(-deltaY)) return;
      // Only prevented when the tile actually took the delta: otherwise the page (and pull-to-refresh)
      // keeps working at the boundary, which is the touch equivalent of the wheel handoff above.
      event.preventDefault();
    };

    const onTouchEnd = (): void => {
      let velocity = flickVelocity(samples);
      samples.length = 0;
      if (velocity === 0) return;
      let previous = performance.now();
      const step = (now: number): void => {
        momentum = null;
        const elapsed = now - previous;
        previous = now;
        velocity = decayVelocity(velocity, elapsed);
        if (velocity === 0) return;
        if (!scrollBy(-velocity * elapsed)) return; // hit an end: the glide stops rather than chaining
        momentum = requestAnimationFrame(step);
      };
      momentum = requestAnimationFrame(step);
    };

    // Native scrolling still happens when the wheel is handed back, and the tile's own scrollbar can
    // still be dragged, so the depth is also read straight from the viewport's scroll event.
    const onViewportScroll = (): void => {
      const view = viewportOf(termRef.current);
      if (view) report(view);
    };

    wrapper.addEventListener('wheel', onWheel, { passive: false });
    wrapper.addEventListener('touchstart', onTouchStart, { passive: true });
    wrapper.addEventListener('touchmove', onTouchMove, { passive: false });
    wrapper.addEventListener('touchend', onTouchEnd, { passive: true });
    wrapper.addEventListener('touchcancel', stopMomentum, { passive: true });
    wrapper.addEventListener('scroll', onViewportScroll, true);

    toBottomRef.current = () => {
      stopMomentum();
      const view = viewportOf(termRef.current);
      if (!view) return;
      view.scrollTop = view.scrollHeight;
      report(view);
    };

    return () => {
      stopMomentum();
      wrapper.removeEventListener('wheel', onWheel);
      wrapper.removeEventListener('touchstart', onTouchStart);
      wrapper.removeEventListener('touchmove', onTouchMove);
      wrapper.removeEventListener('touchend', onTouchEnd);
      wrapper.removeEventListener('touchcancel', stopMomentum);
      wrapper.removeEventListener('scroll', onViewportScroll, true);
    };
    // Registered once per tile. The handlers read `termRef` and `onScrollbackRef` when they FIRE, so a
    // terminal that is created after this effect ran is still found — re-registering on every xterm
    // change would drop a listener mid-gesture.
  }, [termRef, wrapperRef]);

  return api;
}
