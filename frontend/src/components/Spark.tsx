/**
 * Sparklines and run streaks — the two chart shapes this control plane uses.
 *
 * Both draw nothing when they have nothing. See `spark.ts` for why that is a rule rather than a
 * nicety: an empty series must not render as a flat line, because a flat line is a measurement.
 */

import { SPARK_MIN, areaPoints, sparkPoints, streakBars, type StreakBar } from './spark';

/** Colours by meaning, so a caller names the reading rather than a hex value. `idle` is deliberately
 *  the muted foreground: a series with no movement should recede. */
export const SPARK_COLORS = {
  throughput: '#34d399',
  rate: '#7dd3fc',
  activity: '#a78bfa',
  failure: '#fca5a5',
  idle: 'currentColor',
} as const;

export function Spark({
  series,
  color = SPARK_COLORS.throughput,
  width = 100,
  height = 16,
  fill = false,
  title,
  className,
}: {
  series: readonly number[];
  color?: string;
  /** viewBox width. The SVG itself is always 100% wide — `preserveAspectRatio="none"` stretches it,
   *  which is what lets one component sit in a 46px tile header and a 160px nav footer. */
  width?: number;
  height?: number;
  fill?: boolean;
  title?: string;
  className?: string;
}): JSX.Element | null {
  const points = sparkPoints(series, width, height);
  if (!points) return null;
  return (
    <svg
      width="100%"
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      className={`block ${className ?? ''}`}
      data-testid="spark"
      data-samples={series.length}
      role={title ? 'img' : 'presentation'}
      aria-label={title}
    >
      {title && <title>{title}</title>}
      {fill && <polygon points={areaPoints(points, width, height)} fill={color} opacity={0.15} />}
      <polyline
        points={points}
        fill="none"
        stroke={color}
        strokeWidth={1.3}
        strokeLinejoin="round"
        strokeLinecap="round"
        vectorEffect="non-scaling-stroke"
      />
    </svg>
  );
}

/** The colour of one bar. `running` is amber rather than green: a run still going is not yet a
 *  success, and a streak that counted it as one would be a claim made early. */
const BAR_FILL: Record<StreakBar['status'], string> = {
  completed: '#34d399',
  failed: '#f87171',
  cancelled: '#a1a1aa',
  running: '#fbbf24',
  pending: '#fbbf24',
};

/** The last N runs of one workflow: bar height is how long it took, colour is how it ended. */
export function Streak({
  bars,
  width = 100,
  height = 17,
  className,
}: {
  bars: readonly StreakBar[];
  width?: number;
  height?: number;
  className?: string;
}): JSX.Element | null {
  if (bars.length === 0) return null;
  const rects = streakBars(bars, width, height);
  return (
    <svg
      width="100%"
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      className={`block ${className ?? ''}`}
      data-testid="streak"
      data-bars={bars.length}
    >
      {rects.map((r, i) => (
        <rect
          key={bars[i]!.label}
          {...r}
          rx={0.6}
          fill={BAR_FILL[bars[i]!.status]}
          opacity={bars[i]!.status === 'completed' ? 0.85 : 1}
        >
          <title>{bars[i]!.label}</title>
        </rect>
      ))}
    </svg>
  );
}

export { SPARK_MIN };
export type { StreakBar };
