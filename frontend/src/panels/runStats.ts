/**
 * The open run's own numbers — the fourth reading, which this page did not have.
 *
 * A RUN HAS FOUR READINGS AND NONE SUBSTITUTES FOR ANOTHER: the STATS (this), the per-Machine RAILS,
 * the DATASETS filling, and the EVENT LOG. The Workflow page carried three. The missing one answers
 * **"is this run moving, and at what rate"** — a run sitting in a retry backoff and a run doing
 * steady work both read `running`, and without a rate they read identically. That question is why
 * these numbers came back from the retired Runs surface rather than being re-invented smaller.
 *
 * FOUR AUTHORITIES, DRAWN AS FOUR GROUPS, NEVER BLENDED INTO ONE "PROGRESS" NUMBER. That grouping is
 * the distinction the retired Runs surface drew in prose, made structural — each number is printed
 * under the name of the thing that said it, so no reader has to hover to learn which question it
 * answers. `RunStats.tsx` is what draws these groups:
 *
 *   the LEDGER    what this run COMMITTED — the materialization records, run-scoped by
 *                 construction (ADR 0017). It says `unrecorded` rather than `0` when it holds
 *                 nothing, because those are different statements and only one is about the run.
 *   the LAKE      how fast rows are LANDING in the partitions this run owns, from the catalog
 *                 listing the whole app polls once (`App.tsx`). The only run-scoped RATE there is.
 *   TEMPORAL      activities scheduled and re-attempts made, from its own history.
 *   the RUN LIST  how the last twenty runs of this workflow ended.
 *
 * A ratio across two of them is refused for the same reason `datasets/scope.ts` refuses one: two
 * authorities on two polls are two moments, and a percentage over them looks measured and is not.
 *
 * THE RATE IS SAMPLED, NEVER SYNTHESISED. It reuses the store's existing series — `App.tsx`'s single
 * catalog poller pushes one rate sample per Dataset per tick (`components/spark.ts`), so two samples
 * of one clock already exist and nothing here re-derives one. `SPARK_MIN` then decides whether there
 * is a line: under two samples the spark draws NOTHING, because a flat line is a measured zero and
 * an empty slot is "nobody looked".
 *
 * ERRORS ARE A READING, NOT A COLOUR — see {@link Alarm}. The three that raise one are the three
 * nothing else on this page will tell you.
 *
 * PURE, AND FED ENTIRELY BY READINGS THE PAGE ALREADY HAS: the run list row, the followed run's
 * reduced history, the catalog listing and its rate series, and the run's own account. It costs no
 * request — which is also why every number here moves at the cadence of the poll that owns it
 * rather than at one this module invented.
 */

import type { Concern } from '@core/vocabulary';

import type { RunHistory, RunRow } from '../run/api';
import { streakOf } from '../run/runState';
import { greenStreak, sumSeries, type StreakBar } from '../components/spark';
import { scopeWords } from '../datasets/scope';
import { datasetKey } from '../state/store';
import { eventStats } from './workflowEvents';
import type { RunDatasets } from './runDatasets';

/**
 * Who said it. The label is drawn IN THE SURFACE above each group of numbers, not in a tooltip: a
 * scope that lives only on hover is unlabelled to everyone who does not hover, which is the exact
 * asymmetry that made 623 and 1,246 look like a contradiction.
 */
export type StatSource = 'ledger' | 'lake' | 'temporal' | 'run list';

/** One number, and everything the surface needs to draw it honestly. */
export interface Reading {
  /** Stable id — the test id and the React key. */
  id: string;
  /** The number as drawn. `—` means no authority has answered; it is never a stand-in for zero. */
  value: string;
  /** What was counted and at what scope, drawn beside the number. */
  note: string;
  /** The long form, for the sentence that will not fit beside the number. */
  title: string;
  /** Coloured only when the number is one to act on. Everything else stays the body colour: a bar
   *  that is always red is a bar nobody reads. */
  alarming?: boolean;
  /** The samples behind this number. Handed over WHATEVER its length — `SPARK_MIN` owns the
   *  decision to draw, so there is exactly one place that rule lives. */
  series?: readonly number[];
  /** The bars behind this number, oldest at the left. */
  bars?: readonly StreakBar[];
}

/** One authority's numbers, under its name. */
export interface StatGroup {
  source: StatSource;
  /** What this authority is being asked — the tooltip on its label. */
  title: string;
  readings: Reading[];
}

/**
 * Something wrong that NOTHING ELSE ON THIS PAGE WILL SAY.
 *
 * THE BAR IS NOT A SECOND FAILURE LIST, and the filter is deliberate. An activity failure is
 * already the run's status, a rose `n failed` on the retries reading, and a named failure in the
 * transcript — repeating it here would be noise, and a bar that shouts about ordinary retries is a
 * bar an operator learns to skip. What is raised is the SILENT set, the same doctrine
 * `@core/vocabulary`'s `Concern` is built on: a run whose Units were all dropped closes `completed`
 * with an empty Dataset and not one failure event anywhere in its history.
 */
export interface Alarm {
  id: string;
  /** The sentence, already written for a human. */
  text: string;
  /** Which authority said it. Two never share one line. */
  source: StatSource;
}

/** Everything the bar draws for one run. `null` from {@link runStats} when no run is open. */
export interface RunStats {
  runId: string;
  groups: StatGroup[];
  alarms: Alarm[];
}

export interface RunStatsInput {
  /** The run list's row for the open run — the LEDGER dimension and the execution one arrive on it
   *  together (`run/api.ts`), which is why the stats need no second fetch. */
  run: RunRow | null;
  /** Temporal's own reduced history, as the follower read it. `null` is "nobody has read one",
   *  which is a different answer from a run with no events and is drawn as one. */
  history: RunHistory | null;
  /** This workflow's runs, newest first. The streak, and the only definition-scoped number here. */
  siblings: readonly RunRow[];
  /** What the CATALOG says this run wrote (`runDatasets.ts`) — the partitions the rate is over. */
  wrote: RunDatasets;
  /** The app-level rate series, keyed by {@link datasetKey}. One poller measured every one of
   *  these on one clock, which is the property a rate needs and the reason it is not re-derived. */
  series: Readonly<Record<string, readonly number[]>>;
  /** The run's own account, for the isolation concerns. Empty until the transcript has arrived —
   *  and empty is silence, never evidence that nothing was dropped. */
  concerns: readonly Concern[];
  now: number;
}

/**
 * The concern terms this bar repeats out of the transcript, and the only ones.
 *
 * DROPPED UNITS ARE THE ONE FAILURE WITH NO FAILURE EVENT. A Method that isolated every Unit
 * returns normally, the workflow completes, and Temporal's history is clean (ADR 0023 §13) — so
 * this is the single fact an operator can miss on every tab at once, which is why it is lifted out
 * of the Chat tab and onto a bar that is always on screen. Every OTHER concern stays in the
 * transcript, where it has the turn it belongs to beside it.
 */
export const ISOLATION_TERMS: readonly string[] = ['every-unit-isolated', 'units-isolated'];

/**
 * `12/s`, `0.4/s`.
 *
 * ONE DECIMAL UNDER TEN, which the fleet rail does not need and a run does. The rail aggregates
 * every Dataset on the appliance, so rounding loses nothing; one run committing a row every three
 * seconds rounds to `0`, and reporting a moving run as stopped is the confusion this reading exists
 * to remove.
 */
export function perSecText(v: number): string {
  if (!Number.isFinite(v) || v < 0) return '—';
  return v >= 10 ? Math.round(v).toLocaleString() : v.toFixed(1);
}

/**
 * The rate samples that are THIS RUN'S — and the ones deliberately left out.
 *
 * A PARTITION SEVERAL RUNS WROTE HAS NO RUN-SCOPED RATE. `runDatasets` marks how the catalog
 * attributed each row (ADR 0029 §4): `sole` and `owner` mean the server named exactly one Run
 * behind that partition, so its write rate IS this run's; `among` means the Dataset accumulates
 * across Runs, and summing its rate here would credit this run with somebody else's rows. Those
 * rows are counted and reported instead — an excluded partition is stated, never silently dropped.
 */
export function runThroughput(
  wrote: RunDatasets,
  series: Readonly<Record<string, readonly number[]>>
): { series: number[]; mine: number; shared: number } {
  const mine = wrote.wrote.filter((d) => d.how !== 'among');
  const rates = mine
    .map((d) => series[datasetKey(d.info)])
    .filter((s): s is readonly number[] => Array.isArray(s));
  return { series: sumSeries(rates), mine: mine.length, shared: wrote.wrote.length - mine.length };
}

/** Is this run still open? Temporal's own word, never a clock — a run with no close is running. */
function isOpen(run: RunRow): boolean {
  return run.status === 'running' || run.status === 'pending';
}

/**
 * Every number the bar draws, for the run that is open.
 *
 * `null` WHEN THERE IS NO RUN, so the bar is absent rather than empty. A strip of four em-dashes
 * under a thread nobody has opened a conversation in is chrome claiming to be a measurement.
 */
export function runStats(input: RunStatsInput): RunStats | null {
  const { run, history, siblings, wrote, series, concerns, now } = input;
  if (run === null) return null;

  const open = isOpen(run);
  const ledger = run.materialization ?? null;
  const recorded = ledger !== null && ledger.total > 0;
  const events = history?.events ?? [];
  const counts = eventStats(events);
  const flow = runThroughput(wrote, series);
  const bars = streakOf(siblings, now);
  const green = greenStreak(siblings.map((r) => r.status));
  // The same words the Datasets console uses for this scope, not a second phrasing of the same
  // idea: two vocabularies for one scope is how a reader ends up believing they are two scopes.
  const runScope = scopeWords('run');

  const groups: StatGroup[] = [
    {
      source: 'ledger',
      title:
        'the materialization ledger — what this run COMMITTED, run-scoped by construction. It is ' +
        'not the Dataset total: a Dataset outlives the Run that wrote it.',
      readings: [
        {
          id: 'committed',
          value: recorded ? ledger.rows.toLocaleString() : '—',
          // THREE SILENCES, THREE WORDS (`run/runState.ts`). A ledger nobody could read, a ledger
          // holding no record for this run, and a record that committed zero rows are three
          // different statements; collapsing any two is the defect this dimension exists to end.
          note:
            ledger === null
              ? 'ledger unread'
              : recorded
                ? `committed · ${runScope.label} · ${ledger.complete}/${ledger.total} datasets`
                : 'unrecorded',
          title:
            ledger === null
              ? 'The ledger could not be read. This is NOT a claim that the run wrote nothing.'
              : recorded
                ? runScope.title
                : 'No writer recorded a Dataset for this run. A Dataset a caller published itself records no row here.',
          // A RUN THAT COMMITTED NOTHING MUST NOT LOOK LIKE ONE THAT COMMITTED STEADILY, and the
          // three states are already three different strings — `623`, `0` and `—`. The colour is
          // held back until the run has SETTLED, because zero committed is the ordinary reading of
          // a sweep that has not reached its first commit yet.
          alarming: recorded && ledger.rows === 0 && !open,
        },
      ],
    },
    {
      source: 'lake',
      title:
        'the Dataset catalog — rows LANDING in the partitions this run owns, measured between two ' +
        "samples of the app's one catalog poll",
      readings: [
        {
          id: 'throughput',
          // A CLOSED RUN HAS NO RATE, and this is the same refusal the rails make when they say
          // "the run has settled — nothing is in flight". A rate is a statement about now; drawing
          // the lake's current 0/s under a run that finished on Tuesday would answer a question
          // nobody asked with a number that reads as a verdict on the run.
          value: !open || flow.series.length === 0 ? '—' : perSecText(flow.series[flow.series.length - 1]!),
          note: !open ? 'settled — nothing is landing' : rateNote(flow, wrote),
          title: !open
            ? 'The run has closed. What it committed is on the left; a rate is a statement about now.'
            : 'Rows per second landing in this run’s own partitions. Two samples of one clock — ' +
              'the catalog poll every surface shares — and nothing is drawn under two.',
          // ALWAYS HANDED OVER, WHATEVER ITS LENGTH, and only while the run is open. `SPARK_MIN`
          // decides whether there is a line to draw, which keeps that rule in one place.
          ...(open ? { series: flow.series } : {}),
        },
      ],
    },
    {
      source: 'temporal',
      title: "Temporal's own history for this run — what it scheduled, and what it re-attempted",
      readings: [
        {
          id: 'activities',
          value: history === null ? '—' : counts.activities.toLocaleString(),
          note: history === null ? 'activities · history unread' : 'activities scheduled',
          title:
            'Activities SCHEDULED, not started or completed: a dispatch nobody picked up is exactly ' +
            'the case worth counting, and counting completions would report zero for a run stuck ' +
            'against an unserved queue.',
        },
        {
          id: 'retries',
          value: history === null ? '—' : counts.retries.toLocaleString(),
          // Retries and failures are DIFFERENT numbers and are never collapsed: one failing Batch
          // that retries five times is one problem and five retries, and a single count hides which.
          note:
            history === null
              ? 'retries'
              : counts.failures > 0
                ? `retries · ${counts.failures} failed`
                : 'retries · none failed',
          title:
            'Re-attempts, counted from the attempt Temporal stamps on each retry — kept apart from ' +
            'the failures beside them, because one failing Batch retried five times is one problem.',
          alarming: counts.failures > 0,
        },
      ],
    },
    {
      source: 'run list',
      title: 'the run list — how the last runs of this workflow ended. The only number here that is ' +
        'about the WORKFLOW rather than about this run.',
      readings: [
        {
          id: 'streak',
          value: siblings.length === 0 ? '—' : String(green),
          note: siblings.length === 0 ? 'no runs to compare' : `green · last ${bars.length}`,
          title:
            'How many of the most recent runs of this workflow completed before the first that did ' +
            'not. Bar height is how long each took; colour is how it ended.',
          ...(bars.length > 0 ? { bars } : {}),
        },
      ],
    },
  ];

  return { runId: run.runId, groups, alarms: alarmsOf(run, open, concerns) };
}

/**
 * Why there is no rate yet, in the words that say which kind of nothing it is.
 *
 * FOUR DIFFERENT SILENCES. The lake unlisted, a run nothing in the lake carries yet, a run whose
 * only partitions are shared with other runs, and one sample where a rate needs two. Only the last
 * resolves itself by waiting, and an operator who cannot tell them apart will wait for all four.
 */
function rateNote(flow: { series: number[]; mine: number; shared: number }, wrote: RunDatasets): string {
  if (flow.series.length > 0) return 'units/s · this run’s partitions';
  if (!wrote.read) return 'the lake has not been listed yet';
  if (wrote.wrote.length === 0) return 'nothing in the lake carries this run yet';
  if (flow.mine === 0) return `${flow.shared} shared partitions — no run-scoped rate`;
  return 'one sample so far — a rate needs two';
}

/**
 * The three things that would otherwise go unsaid.
 *
 * ISOLATION IS FIRST because it is the one with no failure event anywhere. COMMITTED NOTHING is
 * second because `completed` is true and useless on its own — MEASURED on a four-Machine sweep
 * whose second Method isolated all 623 of its units, where the run row, `kontra runs` and the
 * lifecycle badge all said what they say about a perfect run. A FAILED MATERIALIZATION is third
 * because the rows exist and are not queryable, which no execution status reports.
 */
function alarmsOf(run: RunRow, open: boolean, concerns: readonly Concern[]): Alarm[] {
  const alarms: Alarm[] = [];

  // ONE LINE PER TERM, NOT PER CALL. A sweep that dropped Units in nine dispatches raises nine
  // concerns; nine copies of one sentence is a wall, and the count is the part that differs.
  for (const term of ISOLATION_TERMS) {
    const raised = concerns.filter((c) => c.term === term);
    const first = raised[0];
    if (!first) continue;
    alarms.push({
      id: term,
      text: raised.length === 1 ? first.label : `${first.label} — in ${raised.length} calls`,
      source: 'temporal',
    });
  }

  const ledger = run.materialization ?? null;
  if (!ledger) return alarms;

  if (!open && ledger.total > 0 && ledger.rows === 0) {
    alarms.push({
      id: 'committed-nothing',
      text:
        'This run finished and committed no rows. A dropped Unit does not fail a workflow, so a ' +
        'run whose Method isolated everything still reports completed — check the workflow’s ' +
        'return value for its dropped count, and the actor’s pane on the Monitor for why.',
      source: 'ledger',
    });
  }

  if (ledger.failed > 0) {
    alarms.push({
      id: 'output-failed',
      text: `${ledger.failed} of ${ledger.total} Datasets failed to materialize — those rows are not queryable.`,
      source: 'ledger',
    });
  }

  return alarms;
}
