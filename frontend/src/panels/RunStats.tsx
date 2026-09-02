/**
 * The open run's numbers, drawn — one line under the scope bar, on every tab.
 *
 * UNDER THE SCOPE BAR RATHER THAN INSIDE A TAB, for the reason `ScopeBar` gives for being one bar
 * and not five headings: the scope is a property of the page, not of whichever tab happens to be
 * open — and so is "is this run moving". An operator reading the Machines under a run is exactly
 * the operator who needs its rate, and a rate that lived on the Chat tab would be a reading you had
 * to leave the evidence to check.
 *
 * ONE LINE, BECAUSE IT IS ALWAYS THERE. The retired Runs surface drew these as four 78-pixel cards
 * on a page that showed nothing else; this sits above five panels that each want the height, so the
 * numbers keep their size and their notes and give up the tiles. Nothing was dropped to fit.
 *
 * GROUPED BY AUTHORITY, WITH THE AUTHORITY NAMED IN THE SURFACE. `runStats.ts` owns why; what this
 * file does with it is refuse to let a group's numbers sit under anybody else's name — which is the
 * whole of "each keeps its own source" as a reader experiences it.
 *
 * NOTHING HERE ANIMATES AND NOTHING HERE IS SYNTHESISED. The spark draws the samples the app's one
 * catalog poll measured and draws NOTHING under two of them (`components/spark.ts`); the streak
 * draws runs that happened. Counts are `tabular-nums` so a digit changing does not re-flow the line
 * beside it.
 *
 * PURE, AND ASSERTED AS MARKUP. Every state — no run, running, finished, failed, produced nothing,
 * retrying, and the one-sample case that must draw no line — renders in node with no jsdom.
 */

import { Spark, SPARK_COLORS, Streak } from '../components/Spark';
import type { Reading, RunStats, StatGroup } from './runStats';

/**
 * The bar, or nothing at all.
 *
 * `null` IS THE POINT OF THE NULL. A thread with no conversation open has no run to measure, and a
 * strip of four em-dashes under it would be chrome dressed as a measurement — the same reason the
 * spark refuses to draw a flat line for an empty series.
 */
export function RunStatsBar({ stats }: { stats: RunStats | null }): JSX.Element | null {
  if (stats === null) return null;
  return (
    <div
      className="shrink-0 border-b border-border"
      data-testid="run-stats"
      data-run={stats.runId}
      data-alarms={stats.alarms.length}
    >
      <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1 px-4 py-1">
        {stats.groups.map((g, i) => (
          <Group key={g.source} group={g} first={i === 0} />
        ))}
      </div>
      {/* AN ALARM IS NOT A COLOUR ON A NUMBER ABOVE. It is a sentence, in its own band, under the
          name of whoever said it — because the three things it raises are the three that no status,
          no badge and no failure event on this page will report (`runStats.ts`). */}
      {stats.alarms.length > 0 && (
        <div
          className="border-t border-amber-500/40 bg-amber-500/10 px-4 py-1"
          data-testid="run-stats-alarms"
        >
          {stats.alarms.map((a) => (
            <p
              key={a.id}
              className="m-0 text-[11px] text-amber-700 dark:text-amber-300"
              data-testid={`run-alarm-${a.id}`}
              data-source={a.source}
            >
              <span className="mr-1.5 font-mono text-[8.5px] uppercase tracking-wider opacity-70">
                {a.source}
              </span>
              {a.text}
            </p>
          ))}
        </div>
      )}
    </div>
  );
}

/** One authority's numbers, under its name. The rule divides the groups so the eye does not have to
 *  read the labels to see that there are four different answers here. */
function Group({ group, first }: { group: StatGroup; first: boolean }): JSX.Element {
  return (
    <span
      className={`flex flex-wrap items-baseline gap-x-2 gap-y-0.5 ${
        first ? '' : 'border-l border-border pl-4'
      }`}
      data-testid={`run-stat-group-${group.source.replace(/\s+/g, '-')}`}
    >
      <span
        className="shrink-0 font-mono text-[8.5px] uppercase tracking-wider text-muted-foreground"
        title={group.title}
      >
        {group.source}
      </span>
      {group.readings.map((r) => (
        <ReadingView key={r.id} reading={r} />
      ))}
    </span>
  );
}

function ReadingView({ reading }: { reading: Reading }): JSX.Element {
  const { id, value, note, title, alarming, series, bars } = reading;
  return (
    <span
      className="flex items-baseline gap-1"
      data-testid={`stat-${id}`}
      data-value={value}
      title={title}
    >
      <span
        className={`font-mono text-[12.5px] font-semibold leading-none tabular-nums ${
          alarming ? 'text-rose-400' : ''
        }`}
      >
        {value}
      </span>
      <span className="font-mono text-[9.5px] text-muted-foreground">{note}</span>
      {/* NOTHING WHERE THERE IS NO SERIES TO DRAW. Both of these return `null` under their own
          minimum, so an unmeasured slot is empty rather than a flat line claiming a measured zero —
          and the slot keeps its width either way, so the line does not jump when the first two
          samples land. */}
      {series && (
        <span className="h-[12px] w-[46px] shrink-0 self-center">
          <Spark
            series={series}
            height={12}
            color={SPARK_COLORS.throughput}
            title="rows landing in this run’s own partitions, between two catalog samples"
          />
        </span>
      )}
      {bars && (
        <span className="h-[12px] w-[46px] shrink-0 self-center">
          <Streak bars={bars} height={12} />
        </span>
      )}
    </span>
  );
}
