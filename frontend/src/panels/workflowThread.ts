/**
 * A workflow thread: which of the runs in the list are THIS workflow's, which one is open, and
 * what every tab is therefore about.
 *
 * A WORKFLOW IS A THREAD AND EACH RUN OF IT IS A CONVERSATION. That sentence is the whole of the
 * layout — the runs down one side, six readings of the open one beside them — and it is also the
 * whole of the arithmetic, because "which conversation" is a question that must be answered
 * against THIS thread and never against whatever the app happened to load last.
 *
 * IT LIVES APART FROM THE PAGE, like `workflowRow.ts` beside it and for the same mechanical
 * reason: `WorkflowsPage.tsx` imports CodeMirror, which cannot load under the test runner, so
 * anything that has to be asserted in isolation cannot ride in that file.
 *
 * THE JOIN IS `rowStatus`'s, CALLED RATHER THAN COPIED, and that is the point of this module
 * existing at all. #13 shipped a Workflows list that drew a RUNNING workflow as `NEVER RUN` because
 * it joined rows to runs by `guessTypeFromFilename` — `dhmonitor` guesses to `Dhmonitor`, the class
 * is `DockerLeakMonitor`, and the run list keyed on the real one. A page that re-derived the join
 * here would be a second place for that bug to live and a second place to have to fix it, so there
 * is one join, one test suite pinning it (`workflowRow.test.ts`), and this module inherits both.
 *
 * THE SAME BUG HAS A SECOND FLOOR, and it is the one this module is actually for. #13 was a wrong
 * STATUS on switch; a page with a run selected has a wrong RUN on switch — open `dnssweep`'s
 * `sweep-9`, click `nscheck`, and every one of five tabs is still drawing `sweep-9` unless
 * something says otherwise. So a selection is not a value this module carries over: it is a lookup
 * INSIDE the thread, re-answered on every render, and a run that is not this workflow's does not
 * become "the run" merely by having been one a moment ago. {@link threadOf} has three different
 * words for a run it cannot show, because "you switched threads", "it has not dispatched yet" and
 * "nobody selected anything" are three different sentences and only the first is a mistake.
 */

import type { NamedTurn } from '@core/vocabulary';

import type { RunRow } from '../run/api';
import type { RunTurns } from '../run/turns';
import { turnKey } from './transcriptDrill';
import { rowStatus, type RowStatus } from './workflowRow';

/* ───────────────────────────── the six readings ───────────────────────────── */

/**
 * The six tabs, as a type.
 *
 * The union is what makes {@link TAB_DETAIL}'s `Record` a compile-time guard — the same device
 * `surfaces.ts` uses one level up, for the same reason: a seventh tab that nobody gives a label and
 * a hint fails to build the day it is added, rather than rendering as a blank strip.
 *
 * `chat` IS GONE, AND IT IS NOT KEPT AS AN ALIAS. The word was doing double duty: what is on that
 * tab is an operator reading a machine's account of itself, and borrowing a consumer-chat name for
 * it imports the aesthetic the PRD ruled out. `transcript` is what the module that produces the
 * reading has always been called. A remembered `'chat'` fails {@link isTab} and lands on
 * {@link DEFAULT_TAB} — which is the very tab it named — so refusing it costs an operator nothing,
 * and accepting it would keep the word alive in the one place nobody would think to look.
 *
 * `events` IS A SIBLING AND NOT A DRAWER. It used to be a panel folded under the transcript, which
 * made two different readings share one pane and compete for it; they are not substitutable in
 * either direction, so each gets a tab.
 */
export type TabId = 'transcript' | 'events' | 'code' | 'scratch' | 'monitor' | 'data';

export interface Tab {
  id: TabId;
  label: string;
  /** One sentence, for the tooltip. It says what the tab HOLDS, not what it is called. */
  hint: string;
}

/**
 * The order they are drawn in, which is the order the questions get asked.
 *
 * Transcript first because it is the reading an operator wants and has never had: what the run DID,
 * in kontra's words, rather than twenty thousand rows of `NexusOperationScheduled`.
 *
 * EVENT LOG SECOND, AND NOT LAST, because it is the other half of the first. A rename nobody can
 * check is a rename nobody should trust (ADR 0027), so the check has to sit beside the claim rather
 * than at the end of a row of tabs about other subjects entirely.
 *
 * Then: Code is what it ran. Scratch is what it was meant to be. Monitor is the machines under it.
 * Data is what came out.
 */
const ORDER: readonly TabId[] = ['transcript', 'events', 'code', 'scratch', 'monitor', 'data'];

const TAB_DETAIL: Record<TabId, Omit<Tab, 'id'>> = {
  transcript: {
    label: 'Transcript',
    hint: "the run's own account of itself — what it set out to do, what it called, what it wrote, and how it ended",
  },
  events: {
    label: 'Event log',
    hint: 'what Temporal actually recorded — the raw rows the account was read from, and the ones any one turn folded',
  },
  code: {
    label: 'Code',
    hint: 'the workflow source, as it is on this disk now — read-only (ADR 0030)',
  },
  scratch: {
    label: 'Scratch',
    hint: 'the sketch behind this workflow — a drawing with a subject at last (ADR 0026)',
  },
  monitor: {
    label: 'Monitor',
    hint: "this run's own Machines — its worker, the Actors it dispatched to, and any fleet it brought up",
  },
  data: {
    label: 'Data',
    hint: 'the Datasets this run wrote — each one click away, and scoped to this run when it opens',
  },
};

/** The six, in tab order. */
export const TABS: readonly Tab[] = ORDER.map((id) => ({ id, ...TAB_DETAIL[id] }));

/** Where a thread opens, and where an unreadable remembered tab lands. A deep link to a run names
 *  the run and never the lens, so it opens here. */
export const DEFAULT_TAB = 'transcript' satisfies TabId;

/** Is this string one of ours? Used at the `localStorage` boundary, where the value is whatever was
 *  there last release. An unknown one is not an error — it is {@link DEFAULT_TAB}. */
export function isTab(raw: unknown): raw is TabId {
  return typeof raw === 'string' && (ORDER as readonly string[]).includes(raw);
}

/* ───────────────────────────── where a drill lands ───────────────────────────── */

/** The tab the raw Temporal events are read on, now that they have one of their own. */
export const EVENT_LOG_TAB = 'events' satisfies TabId;

/** Opening one turn's raw events: WHICH turn, and WHERE they are now read. */
export interface DrillOpen {
  /** The turn whose events are open, by `turnKey` — the same identity the transcript keys its rows
   *  by, so a growing loop never swaps which turn the panel is about. */
  openTurn: string;
  /** The tab that shows them. Always {@link EVENT_LOG_TAB}; the point is that it comes back BESIDE
   *  the key rather than being left to each caller to remember. */
  tab: TabId;
}

/**
 * What a click on a turn's drill affordance means, as one value.
 *
 * ONE FUNCTION BECAUSE THE TWO FACTS MUST NOT DRIFT APART. While the raw view was a panel folded
 * under the transcript, drilling was ONE fact — the turn — and the pane was wherever the reader
 * already was. Splitting the readings into two tabs makes it two facts, and a caller that set the
 * turn and forgot the tab is the exact regression the split threatens: a click that appears to do
 * nothing, on the affordance the whole vocabulary is bought with (ADR 0027). A reader who cannot
 * check a rename should not accept one, so the check may not be the thing that quietly breaks.
 *
 * IT SAYS NOTHING ABOUT DEPTH, and it does not need to. Descending into a child is `eventDrill.ts`'s
 * stack, inside the panel, on the tab this already put the operator on — so a turn that folded a
 * child workflow lands in the same place as one that folded three activities.
 */
export function openDrill(named: NamedTurn): DrillOpen {
  return { openTurn: turnKey(named.turn), tab: EVENT_LOG_TAB };
}

/**
 * The transcript's drill affordance, with the tab move already inside it.
 *
 * THE COMPONENT OWNS THIS AND THE PAGE DOES NOT, which is a testability decision and a correctness
 * one at once. `WorkflowsPage.tsx` imports CodeMirror and cannot load under the test runner, so a
 * `setTab` that lived up there would be the load-bearing half of {@link openDrill} in the one file
 * nothing can assert. Composed here, "drilling a turn reaches the Event log" is a call in node.
 *
 * ABSENT IN, ABSENT OUT. A caller that hands in no drill handler has nowhere to put raw events, and
 * the rows stay unclickable — a control that changed tab to show an empty pane would be worse than
 * no control, which is the rule the scope bar's stop buttons follow one section down.
 */
export function drillHandler(
  onDrillTurn: ((named: NamedTurn) => void) | undefined,
  onTab: (id: TabId) => void
): ((named: NamedTurn) => void) | undefined {
  if (!onDrillTurn) return undefined;
  return (named) => {
    onDrillTurn(named);
    onTab(openDrill(named).tab);
  };
}

/* ───────────────────────────── the thread ───────────────────────────── */

/**
 * Why this thread is not showing the run the address names.
 *
 *  - `stray`      the run exists and belongs to ANOTHER workflow. This is the switch bug, caught:
 *                 the only one of the three that means something went wrong, and the only one worth
 *                 saying out loud.
 *  - `undiscovered` no run in the list carries this id. Ordinary and usually temporary — `/api/runs`
 *                 lists runs that have DISPATCHED, so a run started ten seconds ago is not there
 *                 yet, and neither is one Temporal dropped at retention.
 */
export type MissingRun =
  | { why: 'stray'; runId: string; /** The workflow type that actually produced it. */ type: string }
  | { why: 'undiscovered'; runId: string };

/** One workflow, its runs, and the one that is open. */
export interface Thread {
  /** The `@workflow.defn` type this thread is keyed to. `undefined` until the source has been read
   *  — and while it is undefined nothing may be said about runs at all. */
  type: string | undefined;
  /** This workflow's own runs, in the order given (the caller passes them newest-first). */
  runs: RunRow[];
  /** The run every tab is scoped to. `null` is a real state: `/workflows/<w>` is the thread with no
   *  conversation open. */
  selected: RunRow | null;
  /** Set when a run WAS asked for and this thread is not showing it. See {@link MissingRun}. */
  missing: MissingRun | null;
  /** The dot and the pill, from `workflowRow.ts` — the one join, unchanged. */
  status: RowStatus;
  /**
   * The type is known and no run carries it.
   *
   * DISTINCT FROM {@link unresolved}, which is the #13 lie in its original disguise: "no run of a
   * type I cannot name" is not "this has never run". A page that collapsed the two would be back to
   * drawing a live workflow as NEVER RUN, one layer up from where it did it the first time.
   */
  neverRun: boolean;
  /** The source has not been read, or could not be. Nothing about runs is knowable yet. */
  unresolved: boolean;
}

/**
 * The thread for the workflow being VIEWED — its runs, and which one is open.
 *
 * `type` IS THE KEY AND IT IS PASSED IN, never inferred from a filename and never remembered from
 * the row before. Every field below is derived from it in this one call, so there is no path by
 * which a thread can carry a value belonging to a workflow that is no longer on screen: switch
 * workflows and `type` changes, so `runs`, `selected`, `status` and `neverRun` all change with it
 * in the same render. That is what "keyed to the workflow being viewed" has to mean to be true —
 * not a `useEffect` that clears state after the fact, which is a frame late by construction and is
 * exactly how #13 got on screen.
 *
 * `allRuns` IS THE WHOLE LIST, not this workflow's, and it has to be: telling "you switched
 * threads" from "that run has not dispatched yet" is a question about runs this thread does NOT
 * own, and a caller that pre-filtered would have thrown away the evidence.
 */
export function threadOf(
  type: string | undefined,
  allRuns: readonly RunRow[],
  wantedRunId: string | null
): Thread {
  const status = rowStatus(type, allRuns);
  const runs = status.runs;
  const unresolved = !type;

  if (wantedRunId === null || wantedRunId === '') {
    return {
      type,
      runs,
      selected: null,
      missing: null,
      status,
      neverRun: !unresolved && runs.length === 0,
      unresolved,
    };
  }

  const selected = runs.find((r) => r.runId === wantedRunId) ?? null;
  return {
    type,
    runs,
    selected,
    missing: selected ? null : missingRun(wantedRunId, allRuns),
    status,
    neverRun: !unresolved && runs.length === 0,
    unresolved,
  };
}

/** Which of the two absences this is. Never guessed: the global list is the evidence, and a run it
 *  does not carry is undiscovered rather than somebody else's. */
function missingRun(runId: string, allRuns: readonly RunRow[]): MissingRun {
  const elsewhere = allRuns.find((r) => r.runId === runId);
  return elsewhere
    ? { why: 'stray', runId, type: elsewhere.type }
    : { why: 'undiscovered', runId };
}

/* ───────────────────────────── what a run row says ───────────────────────────── */

/**
 * The word one run row shows.
 *
 * `parked` IS NOT A TEMPORAL STATUS AND CANNOT BE READ OFF THE LIST. A run waiting on a human is
 * `running` to Temporal for the whole time it is waiting; what makes it parked is a pending ask,
 * which lives on the run's own memo and costs a read per run. So the caller passes the ids it has
 * actually read — in practice the open run, whose asks the transcript needed anyway — and every
 * other row shows Temporal's word.
 *
 * SILENCE, NEVER INVENTION. A row not in `parked` is a row nothing has been read about, which is
 * not evidence that it is un-parked; it says `running`, which is true. The one thing this must
 * never do is decide a run is parked because it has been running a while.
 *
 * A CLOSED RUN IS NEVER PARKED, whatever its asks say. An ask left `pending` on a run that failed
 * is a question nobody will ever answer, and painting that run as "waiting on a human" would put a
 * live-looking row on a dead run.
 */
export function runWord(run: RunRow, parked: ReadonlySet<string> = new Set()): string {
  return run.status === 'running' && parked.has(run.runId) ? 'parked' : run.status;
}

/* ───────────────────────────── the anti-stale rule ───────────────────────────── */

/**
 * The transcript to draw, or `null` for "not this run's".
 *
 * ONE LINE, AND IT IS THE WHOLE DEFENCE. A transcript is a plain reading with nothing on it that
 * says whose it is, so a component that simply held the last one it fetched would draw the previous
 * run's account under the current run's heading for as long as the new fetch took — which on a
 * closed run in the archive is long enough to read. `RunTurns` carries the id it is about
 * (`run/turns.ts`) precisely so that this comparison exists to be made, and making it here rather
 * than inside a component means it is asserted without a browser.
 *
 * IT REFUSES RATHER THAN GUESSING. `null` means the tab shows that it is loading, which is true;
 * the alternative is a confident heading over somebody else's run, which is the shape of every bug
 * this page was briefed against.
 */
export function transcriptFor(loaded: RunTurns | null, selected: RunRow | null): RunTurns | null {
  if (!loaded || !selected) return null;
  return loaded.runId === selected.runId ? loaded : null;
}
