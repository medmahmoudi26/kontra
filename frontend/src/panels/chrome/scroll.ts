/**
 * The arithmetic behind the wheel proxy and touch scrolling (slice 7a).
 *
 * THE PROBLEM THIS SOLVES IS SPECIFIC TO A WALL. A tile's scrollback lives in `.xterm-viewport`, an
 * `overflow-y: scroll` div; the wall lives in `dashboard-grid`, another one. Two nested scrollers under
 * one cursor is the worst default the browser has: the wheel over a tile scrolls whichever the browser
 * picks, and both choices are wrong at different moments. If the tile always wins, the wall cannot be
 * scrolled without hunting for a gap between tiles. If the wall always wins, a live journal cannot be
 * read back at all. And when a tile is at the end of its scrollback, the browser's chaining kicks in
 * mid-gesture and yanks the wall — so the line an operator was reading leaves the screen while they
 * are reading it.
 *
 * THE PROXY PATTERN, from the reference implementation's `TerminalPane`: the tile's wrapper takes the
 * wheel with a NON-PASSIVE native listener, decides who should move, and adjusts `scrollTop` itself.
 * `useTileScroll.ts` is that listener; this file is the two decisions it makes, extracted so they can be
 * unit-tested without a DOM — the boundary handoff and the momentum decay are precisely the parts whose
 * bugs are invisible in a screenshot.
 *
 * NOT ADOPTED FROM THE REFERENCE, AND THIS IS THE IMPORTANT ONE. `hooks/scrollUtils.ts` there routes a
 * wheel three ways, and two of them are `send-keys` into the pane: SGR mouse reports for a
 * mouse-tracking app, and arrow keys for the alternate screen. Both are BYTES ON THE WIRE TOWARD A
 * SESSION, which ADR 0020's whole read-only guarantee rests on not having — finding (3) measured
 * `run-shell` and `send-keys` both executing as root through a *read-only* tmux client with no error, so
 * the guarantee cannot come from tmux and has to come from there being nothing to write to. Only the
 * third route survives here: proxy the pixels to a scroll container in the browser. A tile therefore
 * cannot scroll an application's own viewport (`less`, `vim` in a pane) and that is correct — it is a
 * screen we are watching, not a program we are driving.
 */

/** What the arithmetic needs from a scroll container. An interface so the tests do not need a DOM. */
export interface ScrollBox {
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
}

/**
 * How much of `deltaY` this container can absorb, in pixels; `0` means "hand the wheel back".
 *
 * ZERO IS THE LOAD-BEARING RETURN VALUE. The caller only calls `preventDefault()` when this is
 * non-zero, so at the top of the scrollback scrolling up — and at the bottom scrolling down — the event
 * keeps its default action and the WALL scrolls, exactly as a single scroller would. That is the
 * boundary handoff, and it is why this returns a clamped amount rather than a boolean: a gesture that
 * has 300 px of momentum and 40 px of scrollback left must consume the 40 and then chain, not choose
 * one of the two for the whole gesture.
 *
 * A tile with no scrollback (`scrollHeight <= clientHeight`) absorbs nothing at all, which is the
 * resting state of every SNAPSHOT tile: `applyFrame` calls `reset()` on each repaint, so a tile
 * repainting every three seconds has exactly one screen and nothing behind it. The wheel over a
 * snapshot tile therefore scrolls the wall, with no special case for it here.
 */
export function absorbableScroll(box: ScrollBox, deltaY: number): number {
  const max = box.scrollHeight - box.clientHeight;
  if (max <= 0 || deltaY === 0) return 0;
  if (deltaY < 0) {
    const room = Math.min(-deltaY, Math.max(0, box.scrollTop));
    return room < 1 ? 0 : -room;
  }
  const room = Math.min(deltaY, Math.max(0, max - box.scrollTop));
  return room < 1 ? 0 : room;
}

/**
 * Normalise a wheel event's delta to pixels.
 *
 * `deltaMode` is not decoration: a Firefox mouse wheel reports `DOM_DELTA_LINE` (1) and a page-up
 * gesture reports `DOM_DELTA_PAGE` (2), so treating `deltaY` as pixels unconditionally scrolls a
 * three-line notch by three pixels. xterm's own `getLinesScrolled` handles the same three cases; this
 * is the same conversion in the opposite direction.
 */
export function wheelPixels(
  event: { deltaY: number; deltaMode: number },
  metrics: { lineHeight: number; pageHeight: number }
): number {
  if (event.deltaMode === 1) return event.deltaY * Math.max(1, metrics.lineHeight);
  if (event.deltaMode === 2) return event.deltaY * Math.max(1, metrics.pageHeight);
  return event.deltaY;
}

// --- touch ---------------------------------------------------------------------------------------

/**
 * Momentum, tuned by the reference implementation and kept at its numbers.
 *
 * `0.995` per millisecond is a ~0.6-second glide, which is what a native scroll view feels like; the
 * floor stops the loop instead of animating a tenth of a pixel forever, and the ceiling exists because
 * a fast flick on a trackpad can report a velocity that would scroll two thousand lines in one frame.
 */
export const DECELERATION_PER_MS = 0.995;
export const MIN_VELOCITY_PX_PER_MS = 0.03;
export const MAX_VELOCITY_PX_PER_MS = 5;
/** How many recent touch samples the velocity is averaged over. One sample is a jittery number; the
 * whole gesture is a number that ignores the flick at the end of it. */
export const VELOCITY_SAMPLES = 3;

export interface TouchSample {
  t: number;
  y: number;
}

/** Velocity in px/ms from the recent samples, clamped. `0` when there is nothing to derive it from —
 * a tap, or two samples with the same timestamp. */
export function flickVelocity(samples: readonly TouchSample[]): number {
  if (samples.length < 2) return 0;
  const first = samples[0];
  const last = samples[samples.length - 1];
  if (!first || !last) return 0;
  const dt = last.t - first.t;
  if (dt <= 0) return 0;
  const v = (last.y - first.y) / dt;
  if (v > MAX_VELOCITY_PX_PER_MS) return MAX_VELOCITY_PX_PER_MS;
  if (v < -MAX_VELOCITY_PX_PER_MS) return -MAX_VELOCITY_PX_PER_MS;
  return v;
}

/** One frame of decay. Returns the velocity for the next frame; the caller stops when it is below the
 * floor. Exponential in the elapsed time rather than per-frame, so a dropped frame does not make the
 * glide longer — which is how momentum ends up feeling different on a loaded box than an idle one. */
export function decayVelocity(velocity: number, elapsedMs: number): number {
  const next = velocity * Math.pow(DECELERATION_PER_MS, Math.max(0, elapsedMs));
  return Math.abs(next) < MIN_VELOCITY_PX_PER_MS ? 0 : next;
}

/** Keep the sample window bounded. Mutates, because it is called on every `touchmove` and allocating a
 * new array per move is the one place on this page where that would show up. */
export function pushSample(samples: TouchSample[], sample: TouchSample): void {
  samples.push(sample);
  if (samples.length > VELOCITY_SAMPLES + 1) samples.splice(0, samples.length - VELOCITY_SAMPLES - 1);
}
