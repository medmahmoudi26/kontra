/**
 * The geometry behind the sparklines, separated from the SVG so it can be tested.
 *
 * EVERY SERIES ON THIS APP IS MEASURED, NEVER SYNTHESISED. The design these came from is a mock,
 * and a mock fills its charts from `Math.sin(tick)`. A control plane must not: a line that moves
 * because it is animated, next to a number that is real, is a lie told in the same typeface as the
 * truth. So the rule here is that a Spark draws only what was sampled — and when nothing has been
 * sampled yet it draws NOTHING (see `SPARK_MIN`), rather than a flat line, which would read as a
 * measured zero.
 */

/** Below this many samples there is no line to draw. Two points is the minimum that can express a
 *  direction, and a direction is the only thing a sparkline says. */
export const SPARK_MIN = 2;

/** How many samples a rolling series keeps. At the 2 s poll the surfaces use, 48 is about a minute
 *  and a half of history — long enough to show a sweep spinning up, short enough that a page left
 *  open all day is not holding an unbounded array per dataset. */
export const SPARK_WINDOW = 48;

/**
 * Points for a polyline across `w × h`, newest at the right.
 *
 * NORMALISED TO THE SERIES' OWN MAXIMUM, not to a global one: a sparkline's job is the SHAPE of one
 * measurement, and scaling six of them to a shared axis makes five of them flat. The axis is
 * therefore never labelled and never implied — that is what the number beside it is for.
 *
 * A series that is entirely zero is still drawn, flat along the bottom. That is a real reading
 * ("nothing is being written"), distinct from having no reading at all, which draws nothing.
 */
export function sparkPoints(series: readonly number[], w: number, h: number): string {
  if (series.length < SPARK_MIN) return '';
  const max = Math.max(...series);
  const span = max > 0 ? max : 1;
  const last = series.length - 1;
  return series
    .map((v, i) => {
      const x = (i / last) * w;
      // 1px of padding top and bottom so a peak and a trough are both a visible stroke rather than
      // half a stroke clipped by the viewBox.
      const y = h - (Math.max(0, v) / span) * (h - 2) - 1;
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(' ');
}

/** Close a polyline into a filled area by dropping to the baseline at both ends. */
export function areaPoints(points: string, w: number, h: number): string {
  if (!points) return '';
  return `0,${h} ${points} ${w},${h}`;
}

/** Append a sample to a rolling window, without mutating the input. */
export function push(series: readonly number[], value: number): number[] {
  const next = [...series, Number.isFinite(value) ? value : 0];
  return next.length > SPARK_WINDOW ? next.slice(next.length - SPARK_WINDOW) : next;
}

/**
 * A rate from two cumulative samples.
 *
 * Clamped at zero on purpose. Counts here come from a catalog listing that can go DOWN — a dataset
 * compacted, a run's output abandoned, a listing that raced a write — and a negative "units/s" is
 * never a thing that happened. Reporting 0 for that interval is the honest reading: no forward
 * progress was observed.
 */
export function rate(prev: number, next: number, elapsedMs: number): number {
  if (elapsedMs <= 0) return 0;
  return Math.max(0, ((next - prev) * 1000) / elapsedMs);
}

/**
 * Add several series element-wise, aligned at the NEWEST end.
 *
 * Right-aligned because these are rolling windows sampled on the same tick: a Dataset that first
 * appeared thirty seconds into a sweep has a shorter series, and its samples are the most RECENT
 * thirty seconds, not the oldest. Aligning left would slide a young dataset's activity backwards
 * in time and show a spike where the run had not started.
 */
export function sumSeries(all: readonly (readonly number[])[]): number[] {
  const len = all.reduce((m, s) => Math.max(m, s.length), 0);
  const out = new Array<number>(len).fill(0);
  for (const s of all) {
    const offset = len - s.length;
    for (let i = 0; i < s.length; i++) out[offset + i] = (out[offset + i] ?? 0) + (s[i] ?? 0);
  }
  return out;
}

/** One bar of a run streak: how long a run took, and whether it worked. */
export interface StreakBar {
  /** Wall time in ms. Drives the bar's height. */
  ms: number;
  /** Temporal's execution status. `running` gets its own colour — a tall amber bar for a run still
   *  going is not the same statement as a tall green one. */
  status: 'completed' | 'failed' | 'cancelled' | 'running' | 'pending';
  /** For the tooltip: which run this bar is. */
  label: string;
}

/** Bar rectangles for a streak, oldest at the left. Heights are relative to the LONGEST run in the
 *  window, for the same reason sparkPoints normalises to its own max. */
export function streakBars(
  bars: readonly StreakBar[],
  w: number,
  h: number
): Array<{ x: number; y: number; width: number; height: number }> {
  if (bars.length === 0) return [];
  const bw = w / bars.length;
  const max = Math.max(...bars.map((b) => b.ms), 1);
  return bars.map((b, i) => {
    // A floor of 3px so a sub-second run is still a bar you can see and hover, rather than a run
    // that appears not to have happened.
    const height = Math.max(3, (Math.max(0, b.ms) / max) * h);
    return { x: i * bw + 0.4, y: h - height, width: Math.max(1, bw - 0.9), height };
  });
}

/** How many of the most recent runs succeeded before the first one that did not. The number the
 *  "run streak" stat reports, counted from the NEWEST end. */
export function greenStreak(statuses: readonly string[]): number {
  let n = 0;
  for (const s of statuses) {
    if (s !== 'completed') break;
    n += 1;
  }
  return n;
}
