/**
 * The workflow thread: this workflow's runs down one side, six readings of the open one beside
 * them.
 *
 * A WORKFLOW IS A THREAD AND EACH RUN OF IT IS A CONVERSATION, which is the sentence the whole
 * layout is. The global Runs surface is retired because it made a run id and the code that produced
 * it two different searches; the fix is not a better list, it is putting the runs next to the
 * workflow and letting one selection govern everything else on screen.
 *
 * ONE SELECTION, SIX READINGS, AND NONE OF THEM SUBSTITUTABLE. Transcript is what the run did, in
 * kontra's words. Event log is what Temporal actually recorded. Code is what it ran. Scratch is what
 * it was meant to be. Monitor is the machines under it. Data is what came out. An operator who has
 * only the event log can reconstruct none of the other five, and an operator who has only the
 * Dataset cannot tell an empty result from a disaster — so they are tabs over one subject rather
 * than six places to go looking.
 *
 * IT TAKES THE HEAVY PANELS AS PROPS, which is the same split `Shell.tsx` makes and for the same
 * mechanical reason: the source viewer is CodeMirror and the worker pane is xterm, and neither can
 * load under the test runner. Handing them in means every state of this page — no runs, a run that
 * failed, a run that is parked, two hundred runs, and a selection that belongs to a workflow you
 * just left — renders in node as a string. It also means the Code tab shows the EXISTING editor
 * rather than a second one built to fit here, which is the instruction and is also the right call:
 * a read-only viewer with the folder's path and the registered digest beside it (ADR 0030) is not
 * something to have two of.
 *
 * NOTHING HERE STARTS A RUN. A **Run** is one execution of a caller's workflow (ADR 0023 §12), and
 * this page reads runs rather than minting them — there is no graph to interpret and no arbitrary
 * workflow to launch from a thread. Starting one stays where the input form and the queue already
 * are, on the page that knows which folder is registered.
 *
 * IT FOLLOWS NOW, AND IT STILL SAYS WHICH IT IS DOING. A live run's account arrives in place as the
 * run produces it, and a finished one reopens to the same transcript from the same reader — one
 * surface for "what is happening" and "what happened", which is the whole point of putting them on
 * one page. What the surface must never do is imply following it is not doing: `follow` is read from
 * the reader and printed beside the verdict, so a run nobody is watching says so.
 *
 * AND EVERY TURN STILL DRILLS, ACROSS THE SPLIT. The account is a rename of Temporal's events, and
 * a rename nobody can check is one nobody should trust (ADR 0027). The raw view used to open beneath
 * the transcript, which made two different readings share one pane and compete for it; it is a
 * sibling tab now. What the split must not cost is the check itself — so drilling a turn does not
 * merely mark a row, it CARRIES the reader to the Event log with those events in view
 * (`openDrill`), and the descent into a child workflow continues there. A drill that silently went
 * nowhere because the panes are now separate would be the whole vocabulary going unbacked.
 */

import type { ReactNode } from 'react';
import { SideResizer, useSideDock } from './chrome/SideDock';
import type { SideBounds } from './chrome/sideDock';

import type { NamedTurn } from '@core/vocabulary';

import { fmtDuration, runDuration, type RunRow } from '../run/api';
import type { FollowPhase } from '../run/follow';
import type { RunTurns } from '../run/turns';
import type { AskDeck } from './AskTurn';
import { StatePill } from './StatePill';
import { TranscriptView } from './Transcript';
import {
  TABS,
  drillHandler,
  runWord,
  transcriptFor,
  type MissingRun,
  type TabId,
  type Thread,
} from './workflowThread';

/** Why the open run has no account, when it has none. Kept apart from "there is no run open": a
 *  run nobody selected and a run whose log could not be read are different sentences. */
export interface TurnsFailure {
  /** BOTH authorities have nothing — Temporal dropped it, and the archive has no copy. Ordinary at
   *  retention, and not an error to shout about. */
  gone: boolean;
  detail: string;
}

export interface WorkflowThreadProps {
  /** The name the row opens under — a folder on somebody's disk, not the decorated type. */
  workflow: string;
  thread: Thread;
  tab: TabId;
  onTab: (id: TabId) => void;
  /** Open one of this thread's runs, or `null` to close the conversation and keep the thread. */
  onOpenRun: (runId: string | null) => void;
  /**
   * Runs KNOWN to be waiting on a human.
   *
   * Only ever the ones whose asks have actually been read — see `runWord`. A run absent from this
   * set is a run nothing has been read about, which is not evidence that it is un-parked.
   */
  parked?: ReadonlySet<string>;
  /** The account of the selected run, once it has arrived. Guarded by `transcriptFor`, so an
   *  account of the PREVIOUS run is treated as not having arrived — which is what it is. */
  loaded: RunTurns | null;
  failure?: TurnsFailure | null;
  /**
   * What the reader following the open run is doing.
   *
   * ABSENT MEANS NOTHING IS FOLLOWING, which is a real state (a caller that fetched once) and is the
   * safe default: a live run drawn as though something were watching it is how an operator stops
   * watching a run that is still going.
   */
  follow?: FollowPhase;
  /**
   * Record which turn's raw Temporal events are open. Absent leaves every row unclickable, which is
   * what a surface with nowhere to put them should do rather than offer a dead affordance.
   *
   * IT IS ONLY HALF OF WHAT A CLICK DOES, and the other half is not the caller's to remember. The
   * raw events are a sibling tab now, so this component pairs it with `onTab` (`drillHandler`) —
   * marking a row without carrying the reader to the Event log would be a click that appears to do
   * nothing, on the affordance the whole vocabulary is bought with (ADR 0027).
   */
  onDrillTurn?: (named: NamedTurn) => void;
  /** Which turn's raw events are open, by `turnKey`. */
  openTurn?: string | null;
  /**
   * The raw-events panel for the open turn, handed in like the other heavy children.
   *
   * IT TAKES THE TAIL'S PLACE RATHER THAN CROWDING IT, and now it does so on the Event log tab
   * instead of under the transcript. Both are the run's events — the tail is the newest forty of
   * them, the drill is the ones ONE turn folded — and putting them on screen together would be two
   * event logs disagreeing about which rows matter. The drill carries its own way back out (its
   * `close`), which is what returns the tab to the whole log.
   */
  drill?: ReactNode;
  /** The existing read-only source viewer, handed in unchanged (ADR 0030). */
  code: ReactNode;
  /**
   * The Machines under the OPEN RUN, and one of their panes — `RunMonitor.tsx`, handed in for the
   * same reason the viewer is.
   *
   * IT IS SCOPED TO THE RUN NOW, which is the only thing that changed about this tab. It used to be
   * the worker serving the WORKFLOW and nothing else, with everything the run actually ran on a
   * surface away — a wall of every Terminal on the appliance, filtered by hand. The pane itself is
   * the same component it always was; what changed is which Terminals reach it.
   */
  monitor: ReactNode;
  /**
   * What the open run wrote, and the way to it — handed in like the rest.
   *
   * NOT BECAUSE IT IS HEAVY. `RunDataTab` renders a list; what it cannot do from here is NAVIGATE,
   * and nothing in this file knows a store or a URL exists (`state/address.ts` states that layering
   * from the other end). Opening a Dataset scoped to this run is a store action, so the tab is built
   * where the store is and framed here — the same split `scratch` makes one prop above.
   *
   * ABSENT DRAWS THE ABSENCE HONESTLY. A run's output and the run that made it were two searches,
   * which is the same complaint the retired Runs surface answered for runs; this is that fix on the
   * other side.
   */
  data?: ReactNode;
  /**
   * This workflow's own sketch — `WorkflowSketch.tsx`, handed in like the rest.
   *
   * NOT BECAUSE IT IS HEAVY, but because it is the only reading on this page that is NOT about the
   * open run: a sketch belongs to the workflow and outlives every run of it, so it is fed by a
   * fetch keyed to the workflow rather than by anything in `thread`. Taking it as a child keeps
   * that asymmetry where it can be seen instead of hidden behind a prop this component would then
   * have to explain it was ignoring.
   *
   * ABSENT DRAWS THE ABSENCE HONESTLY rather than a run affordance-free canvas with nothing in it.
   */
  scratch?: ReactNode;
  /**
   * The open run's own numbers — what it committed, at what rate, and how hard Temporal is working
   * — handed in like the rest.
   *
   * IT SITS UNDER THE SCOPE BAR AND ABOVE EVERY TAB, because it is the reading that says whether
   * the run is MOVING, and that question does not belong to one panel. A run in a retry backoff and
   * a run doing steady work both read `running`; every tab below is a different reading of the run
   * and not one of them answers it.
   *
   * IT IS A CHILD FOR THE REASON `data` IS: the numbers come from four authorities this file knows
   * nothing about — the ledger on the run row, the followed history, the catalog's rate series and
   * the run list — so it is built where those live and framed here.
   *
   * ABSENT DRAWS NOTHING, which is also what it does when no run is open.
   */
  stats?: ReactNode;
  /**
   * The compact live panel for the open run — the event log, the per-Batch heartbeats and what
   * landed — handed in like the other two.
   *
   * IT IS THE EVENT LOG TAB'S RESTING STATE NOW, rather than a strip under the transcript. `RunTail`'s
   * own header says what it is: the COMPACT form of the run's events, its per-Batch heartbeats and
   * what landed. Two readings sharing one pane is what the split undid, and this is the half that
   * moved — the transcript follows on its own, and what the tail still has that the transcript does
   * not is the heartbeats and the materialization, which are not in the event log at all.
   *
   * IT YIELDS TO `drill`, and only to that. Both are the run's events, so a raw view opened from a
   * turn takes this pane rather than fighting it for the same one.
   */
  tail?: ReactNode;
  /**
   * Stop the open run. `escalate` is `terminate`; absent is `cancel`.
   *
   * ON THE SCOPE BAR RATHER THAN ON EVERY ROW. It used to be two buttons per row of a run table —
   * N copies of an affordance that can only ever apply to the run you are looking at. A run is a
   * conversation now, so stopping one belongs beside the conversation's own name, where it cannot
   * be clicked on the wrong row.
   */
  onStop?: (runId: string, escalate: boolean) => void;
  /** The run a stop has been asked for and not yet answered — so both buttons refuse a second
   *  click rather than sending a second cancel. */
  stopping?: string | null;
  /**
   * What an operator needs to answer a question the open run is parked on.
   *
   * ABSENT DRAWS THE QUESTIONS AND NO FORM, which is the honest state for a surface with nowhere to
   * send a signal. It rides down to the transcript untouched — the asks are turns in the
   * conversation, so the deck belongs to the Transcript tab and to nothing else on this page.
   */
  deck?: AskDeck;
  /** The clock, injected so "took 4m 12s" on an open run is a testable number. */
  now: number;
}

/** A run id plus a date plus a duration needs more than 256px and less than half the page. */
const RUN_RAIL_BOUNDS: SideBounds = { min: 180, max: 560 };

export function WorkflowThread({
  workflow,
  thread,
  tab,
  onTab,
  onOpenRun,
  parked,
  loaded,
  failure,
  follow,
  onDrillTurn,
  openTurn,
  drill,
  code,
  monitor,
  data,
  scratch,
  stats,
  tail,
  onStop,
  stopping,
  deck,
  now,
}: WorkflowThreadProps): JSX.Element {
  // THE ANTI-STALE RULE, APPLIED ONCE, AT THE TOP. Everything below reads `turns`; nothing below
  // reads `loaded`. A reading that belongs to a run this thread is not showing never reaches a
  // renderer at all, so there is no component that could draw it by forgetting to check.
  const turns = transcriptFor(loaded, thread.selected);
  const run = thread.selected;
  // A DRILL IS A TURN AND A TAB. Composed here rather than at the call site so the move to the
  // Event log cannot be forgotten by a caller — see `drillHandler`.
  const onDrill = drillHandler(onDrillTurn, onTab);

  return (
    <div className="flex min-h-0 flex-1 overflow-hidden" data-testid="workflow-thread" data-workflow={workflow}>
      <RunRail thread={thread} parked={parked} onOpenRun={onOpenRun} now={now} />

      <section className="flex min-w-0 flex-1 flex-col overflow-hidden">
        <TabStrip tab={tab} onTab={onTab} />
        <ScopeBar
          run={run}
          missing={thread.missing}
          onOpenRun={onOpenRun}
          {...(onStop ? { onStop } : {})}
          stopping={stopping ?? null}
        />

        {/* THE RUN'S OWN NUMBERS, ABOVE EVERY TAB AND UNDER THE BAR THAT NAMES THE RUN. Same
            argument the scope bar makes for itself: "is this run moving, and at what rate" is a
            property of the conversation, not of the panel somebody happens to have open. */}
        {stats}

        {/* EVERY TAB IS RENDERED FROM THE SAME `run`, which is what "selecting a run changes what
            every tab shows" has to mean to be true rather than decorative. */}
        <div className="flex min-h-0 flex-1 flex-col overflow-hidden" data-testid="tab-panel" data-tab={tab}>
          {tab === 'transcript' ? (
            <TranscriptTab
              run={run}
              turns={turns}
              failure={failure ?? null}
              thread={thread}
              {...(follow ? { follow } : {})}
              {...(onDrill ? { onDrill } : {})}
              openTurn={openTurn ?? null}
              {...(deck ? { deck } : {})}
              now={now}
            />
          ) : tab === 'events' ? (
            <EventLogTab run={run} drill={drill} tail={tail} />
          ) : tab === 'code' ? (
            <CodeTab run={run}>{code}</CodeTab>
          ) : tab === 'monitor' ? (
            <MonitorTab run={run}>{monitor}</MonitorTab>
          ) : tab === 'scratch' ? (
            // THE ONE TAB THAT IS NOT ABOUT THE OPEN RUN, and it is deliberately not handed `run`.
            // A sketch is what this workflow was MEANT to be; it is drawn before the first run and
            // is still the same drawing after the hundredth, so scoping it to a conversation would
            // be inventing a relationship the document does not have. ADR 0026 survives unchanged
            // and is strengthened by the move — a drawing finally has a subject.
            (scratch ?? (
              <Later
                tab="scratch"
                run={run}
                what="the sketch behind this workflow"
                why="ADR 0026 survives unchanged and is strengthened by the move — a drawing finally has a subject."
              />
            ))
          ) : (
            (data ?? (
              <Later
                tab="data"
                run={run}
                what="the Datasets this run wrote, and what landed in each"
                why="A run that produced nothing and a run that failed are two different facts, and only the ledger can tell them apart (ADR 0017)."
              />
            ))
          )}
        </div>
      </section>
    </div>
  );
}

/* ───────────────────────────── the runs ───────────────────────────── */

/**
 * This workflow's own runs, newest first.
 *
 * IT NEVER SAYS `never run` ABOUT A TYPE IT CANNOT NAME. The rail draws whatever
 * `workflowThread.ts` resolved, and that module refuses to answer at all until the decorated type
 * is known — because "no run of a type I cannot name" is not "this has never run", and the
 * difference between those two sentences is #13.
 */
function RunRail({
  thread,
  parked,
  onOpenRun,
  now,
}: {
  thread: Thread;
  parked?: ReadonlySet<string>;
  onOpenRun: (runId: string | null) => void;
  now: number;
}): JSX.Element {
  // RESIZABLE, AND PERSISTED PER BROWSER. It was `w-64` — a fixed 256px for a column holding run
  // ids, timestamps and durations, three things whose length is not 256px wide. A run id like
  // `nscheck-1788009647` plus a date plus `29m 54s` does not fit, and nothing could be done about
  // it. Same hook, same handle and same keyboard support as the workflow list beside it, so the two
  // rails behave identically rather than one being special.
  const dock = useSideDock('run-rail', { width: 256, side: 'left', collapsed: false }, RUN_RAIL_BOUNDS);
  return (
    <>
    <aside
      className="flex shrink-0 flex-col overflow-hidden border-r border-border"
      style={{ width: dock.width }}
      data-testid="run-rail"
      data-type={thread.type ?? ''}
    >
      <header className="flex shrink-0 items-baseline gap-2 border-b border-border px-3 py-2">
        <span className="text-[10px] uppercase tracking-wide text-muted-foreground">Runs</span>
        <span className="min-w-0 flex-1 truncate font-mono text-[11px]" title={thread.type ?? ''}>
          {thread.type ?? '—'}
        </span>
        <span className="font-mono text-[10px] tabular-nums text-muted-foreground" data-testid="run-rail-count">
          {thread.runs.length}
        </span>
      </header>

      {thread.unresolved ? (
        <p className="m-0 px-3 py-2 text-[11.5px] text-muted-foreground" data-testid="run-rail-unresolved">
          {/* THE #13 LIE IN ITS ORIGINAL DISGUISE, REFUSED. The join key is the `@workflow.defn`
              type and the source has not been read, so nothing about runs is knowable yet. */}
          Reading this workflow’s source to find the type its runs are filed under. Until then
          nothing here can honestly say whether it has ever run.
        </p>
      ) : thread.neverRun ? (
        <p className="m-0 px-3 py-2 text-[11.5px] text-muted-foreground" data-testid="run-rail-never">
          Never run. Nothing in the run list carries <code className="font-mono">{thread.type}</code>
          {' '}— which is a different answer from a run that failed, and from a run that produced nothing.
        </p>
      ) : (
        <ol className="m-0 min-h-0 flex-1 list-none overflow-y-auto p-0" data-testid="run-rail-list">
          {thread.runs.map((r) => (
            <RunRailRow
              key={r.runId}
              run={r}
              selected={thread.selected?.runId === r.runId}
              parked={parked}
              onOpenRun={onOpenRun}
              now={now}
            />
          ))}
        </ol>
      )}
    </aside>
    <SideResizer
      width={dock.width}
      onWidth={dock.setWidth}
      side="left"
      label="the run list"
      bounds={RUN_RAIL_BOUNDS}
      testid="run-rail-resizer"
    />
    </>
  );
}

function RunRailRow({
  run,
  selected,
  parked,
  onOpenRun,
  now,
}: {
  run: RunRow;
  selected: boolean;
  parked?: ReadonlySet<string>;
  onOpenRun: (runId: string | null) => void;
  now: number;
}): JSX.Element {
  const word = runWord(run, parked);
  return (
    <li>
      <button
        type="button"
        data-testid={`run-${run.runId}`}
        data-selected={selected ? 'true' : undefined}
        data-word={word}
        aria-current={selected ? 'true' : undefined}
        title={`open ${run.runId}`}
        onClick={() => onOpenRun(run.runId)}
        className={`flex w-full flex-col items-stretch gap-1 border-b border-border/50 px-3 py-2 text-left outline-none hover:bg-accent/50 focus-visible:ring-2 focus-visible:ring-ring/50 ${
          selected ? 'bg-accent' : ''
        }`}
      >
        <span className="flex items-baseline gap-2">
          <StatePill state={word} testid={`run-word-${run.runId}`} small />
          <span className="min-w-0 flex-1 truncate font-mono text-[11px]" title={run.runId}>
            {run.runId}
          </span>
        </span>
        <span className="flex items-baseline gap-2 font-mono text-[9.5px] text-muted-foreground">
          <span>{new Date(run.startedAt).toLocaleString()}</span>
          <span className="ml-auto tabular-nums">{fmtDuration(runDuration(run, now))}</span>
          {/* HOW THE RUN LIST FOUND IT. `/api/runs` lists workflows that made Actor dispatches, so
              this number is also the reason the row exists — and `0` is why a run started seconds
              ago is not in the list at all. */}
          <span className="tabular-nums" title="Actor dispatches this run made — how the run list discovered it">
            {run.dispatches}
          </span>
        </span>
      </button>
    </li>
  );
}

/* ───────────────────────────── the tabs ───────────────────────────── */

function TabStrip({ tab, onTab }: { tab: TabId; onTab: (id: TabId) => void }): JSX.Element {
  return (
    <div className="flex shrink-0 items-stretch border-b border-border" role="tablist" data-testid="tab-strip">
      {TABS.map((t) => (
        <button
          key={t.id}
          type="button"
          role="tab"
          aria-selected={tab === t.id}
          title={t.hint}
          data-testid={`tab-${t.id}`}
          data-active={tab === t.id ? 'true' : undefined}
          onClick={() => onTab(t.id)}
          className={`border-b-2 px-3.5 py-1.5 text-[12px] outline-none focus-visible:ring-2 focus-visible:ring-ring/50 ${
            tab === t.id
              ? 'border-foreground font-semibold text-foreground'
              : 'border-transparent text-muted-foreground hover:text-foreground'
          }`}
        >
          {t.label}
        </button>
      ))}
    </div>
  );
}

/**
 * Which run every tab below is about — and, when there is none, why.
 *
 * IT IS ONE BAR RATHER THAN FIVE HEADINGS because the scope is a property of the page and not of
 * whichever tab happens to be open. An operator changing tab must not have to re-establish which
 * conversation they are in.
 */
function ScopeBar({
  run,
  missing,
  onOpenRun,
  onStop,
  stopping,
}: {
  run: RunRow | null;
  missing: MissingRun | null;
  onOpenRun: (runId: string | null) => void;
  onStop?: (runId: string, escalate: boolean) => void;
  stopping: string | null;
}): JSX.Element {
  if (run) {
    return (
      <div
        className="flex shrink-0 flex-wrap items-baseline gap-2 border-b border-border bg-muted/40 px-4 py-1.5"
        data-testid="run-scope"
        data-run={run.runId}
      >
        <span className="text-[10px] uppercase tracking-wide text-muted-foreground">Run</span>
        <span className="font-mono text-[11.5px]">{run.runId}</span>
        <StatePill state={run.status} testid="run-scope-state" small />

        {/* OFFERED ONLY ON A RUN THAT IS OPEN. A stop button on a run that closed last week does
            nothing and says nothing, which is how an operator learns not to trust the controls. */}
        {onStop && run.status === 'running' && (
          <span className="ml-auto flex items-baseline gap-1.5">
            <button
              type="button"
              data-testid={`cancel-${run.runId}`}
              disabled={stopping !== null}
              title="graceful: its scopes run their exits, so a fleet it holds is destroyed"
              onClick={() => onStop(run.runId, false)}
              className="rounded border border-border px-1.5 text-[10.5px] outline-none hover:bg-accent disabled:opacity-50 focus-visible:ring-2 focus-visible:ring-ring/50"
            >
              {stopping === run.runId ? '…' : 'cancel'}
            </button>
            <button
              type="button"
              data-testid={`terminate-${run.runId}`}
              disabled={stopping !== null}
              title="cancels first, then terminates if that does not land. Terminating skips the scope exits, so a fleet it held would keep billing."
              onClick={() => onStop(run.runId, true)}
              className="rounded px-1.5 text-[10.5px] text-destructive outline-none hover:bg-destructive/10 disabled:opacity-50 focus-visible:ring-2 focus-visible:ring-ring/50"
            >
              stop
            </button>
          </span>
        )}

        <button
          type="button"
          data-testid="close-run"
          title="close this conversation and keep the thread"
          onClick={() => onOpenRun(null)}
          className={`rounded px-1.5 text-[10.5px] text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50 ${
            onStop && run.status === 'running' ? '' : 'ml-auto'
          }`}
        >
          close
        </button>
      </div>
    );
  }

  if (missing?.why === 'stray') {
    return (
      <div
        className="shrink-0 border-b border-amber-500/40 bg-amber-500/10 px-4 py-1.5 text-[11.5px]"
        data-testid="run-stray"
        data-run={missing.runId}
      >
        {/* THE SWITCH BUG, CAUGHT AND SAID OUT LOUD. Silently showing this run anyway is how a
            page ends up drawing one workflow's conversation under another's heading. */}
        <strong className="font-semibold">
          <code className="font-mono">{missing.runId}</code> is not a run of this workflow.
        </strong>{' '}
        It was produced by <code className="font-mono">{missing.type}</code>, so nothing below is
        showing it. Pick a run on the left.
      </div>
    );
  }

  if (missing?.why === 'undiscovered') {
    return (
      <div
        className="shrink-0 border-b border-border bg-muted/40 px-4 py-1.5 text-[11.5px] text-muted-foreground"
        data-testid="run-undiscovered"
        data-run={missing.runId}
      >
        {/* NOT AN ERROR AND USUALLY NOT PERMANENT. `/api/runs` lists runs that have DISPATCHED, so
            a run started seconds ago is not there yet — and neither is one Temporal dropped. */}
        No run <code className="font-mono">{missing.runId}</code> in the list yet. A run is listed
        once it has dispatched to an Actor; a run Temporal has dropped for retention never will be.
      </div>
    );
  }

  return (
    <div
      className="shrink-0 border-b border-border bg-muted/40 px-4 py-1.5 text-[11.5px] text-muted-foreground"
      data-testid="run-none"
    >
      No run open. Pick one on the left — the thread is the workflow, and each run of it is a
      conversation.
    </div>
  );
}

/* ───────────────────────────── the six panels ───────────────────────────── */

/**
 * The run's own account of itself, and nothing else in the pane.
 *
 * NOTHING ELSE IS THE CHANGE. The raw events used to open in a strip beneath this, so an operator
 * reading the account and an operator auditing the log were fighting for one pane — two readings
 * that substitute for each other in neither direction. The log is a sibling tab now; what stayed
 * here is the drill AFFORDANCE on every row, because the row is where a disputed name is, and the
 * click takes the reader to the tab where the evidence is (`openDrill`).
 */
function TranscriptTab({
  run,
  turns,
  failure,
  thread,
  follow,
  onDrill,
  openTurn,
  deck,
  now,
}: {
  run: RunRow | null;
  turns: RunTurns | null;
  failure: TurnsFailure | null;
  thread: Thread;
  follow?: FollowPhase;
  onDrill?: (named: NamedTurn) => void;
  openTurn: string | null;
  deck?: AskDeck;
  now: number;
}): JSX.Element {
  if (!run) {
    return (
      <Empty testid="transcript-no-run">
        {thread.runs.length === 0
          ? 'There is no conversation to read yet.'
          : 'Pick a run on the left to read what it did.'}
      </Empty>
    );
  }

  return turns ? (
    <TranscriptView
      turns={turns}
      {...(follow ? { follow } : {})}
      {...(onDrill ? { onDrill } : {})}
      openTurn={openTurn}
      {...(deck ? { deck } : {})}
      now={now}
    />
  ) : failure ? (
    <Empty testid={failure.gone ? 'transcript-gone' : 'transcript-error'}>
      {failure.gone
        ? // BOTH AUTHORITIES HAVE NOTHING. Not "this run did nothing", and not "the appliance is
          // broken" — the account aged out, which is ordinary and is its own sentence.
          'Neither Temporal nor the archive still holds an account of this run. Its output may well be in the lake; its story is gone.'
        : `Could not read this run’s account: ${failure.detail}`}
    </Empty>
  ) : (
    // NEITHER A READING NOR A REASON: the fetch for THIS run has not answered. Saying "loading" is
    // true; drawing the previous run's transcript under this heading would not be.
    <Empty testid="transcript-loading">Reading {run.runId}…</Empty>
  );
}

/**
 * What Temporal actually recorded — the other reading, in its own pane at last.
 *
 * IT IS TWO STATES AND THE SAME SUBJECT. At rest it is the run's own compact log: the newest events
 * as they arrive, the per-Batch heartbeats and what landed. Drill a turn on the transcript and this
 * pane becomes the rows THAT TURN folded, with the descent into any child workflow it opened — and
 * closing that raw view gives the pane back to the whole log. Two event logs side by side would be
 * two answers to one question, so only ever one of them is here.
 *
 * THE RAW TEMPORAL TYPE IS ON EVERY ROW OF BOTH STATES, which is the promise the vocabulary rests
 * on: an event this release has no word for renders as ITSELF, never dropped and never guessed at.
 * HERE is where it renders, and only here. The transcript beside it is the domain account and stops
 * at the edge of what kontra can name — it says how many rows it is not drawing and that they are on
 * this tab, which is the honest half of the split. An operator who wants Temporal's own record has a
 * tab for it rather than three scheduler rows between two turns of the run's story.
 *
 * NEITHER HALF IS BUILT HERE. Both are handed in — `RunTail` polls and `TurnDrill` fetches a child's
 * history — the same split the viewer and the worker pane make, and for the same reason: every state
 * of this page has to render in node as a string.
 */
function EventLogTab({
  run,
  drill,
  tail,
}: {
  run: RunRow | null;
  drill?: ReactNode;
  tail?: ReactNode;
}): JSX.Element {
  if (!run) {
    return (
      <Empty testid="event-log-no-run">
        Pick a run on the left to read what Temporal recorded for it — this tab is the raw log the
        account beside it was read from.
      </Empty>
    );
  }

  const below = drill ?? tail;
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden" data-testid="event-log-tab" data-run={run.runId}>
      <p className="m-0 shrink-0 border-b border-border px-4 py-1 text-[10.5px] text-muted-foreground">
        {drill
          ? // THE CHECK, ARRIVED AT. The reader clicked a name on the transcript and is now looking
            // at what that name was read from; the way back to the whole log is the panel's own close.
            'The raw events one turn of the transcript folded, with Temporal’s own type on every row. Close the raw view to come back to the whole log.'
          : `What Temporal recorded for ${run.runId} — its newest events, the per-Batch heartbeats, and what landed. The Transcript is kontra’s account of these rows; this is the rows.`}
      </p>
      {below ? (
        <div
          className="flex min-h-0 flex-1 flex-col overflow-hidden"
          data-testid={drill ? 'event-log-drill' : 'event-log-tail'}
        >
          {below}
        </div>
      ) : (
        // A PANE WITH NO READER IS NOT AN EMPTY LOG, and saying "no events" would be the wrong
        // sentence about a run that has thousands.
        <Empty testid="event-log-absent">
          Nothing on this page is reading this run’s events. Drilling a turn on the Transcript opens
          the rows that turn folded here.
        </Empty>
      )}
    </div>
  );
}

function CodeTab({ run, children }: { run: RunRow | null; children: ReactNode }): JSX.Element {
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden" data-testid="code-tab">
      {/* WHAT IS ON THIS DISK NOW — NOT NECESSARILY WHAT THAT RUN EXECUTED, and the gap is worth a
          sentence rather than a footnote. The provenance strip inside the viewer carries the digest
          a worker actually registered; this says why an operator should look at it. */}
      <p className="m-0 shrink-0 border-b border-border px-4 py-1 text-[10.5px] text-muted-foreground">
        {run
          ? `The source as it is on this disk now. ${run.runId} ran whatever was registered at the time — compare the digest below.`
          : 'The source as it is on this disk now, read-only (ADR 0030).'}
      </p>
      <div className="flex min-h-0 flex-1 flex-col overflow-hidden">{children}</div>
    </div>
  );
}

/**
 * The Machines under the open run.
 *
 * THE SENTENCE SAYS WHOSE MACHINES, because that is the whole change. A tab headed "the worker
 * serving this workflow" over a wall of every Terminal on the appliance is how an operator ends up
 * reading somebody else's screen; the panel below is scoped to `run`, and this says so above it.
 */
function MonitorTab({ run, children }: { run: RunRow | null; children: ReactNode }): JSX.Element {
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden" data-testid="monitor-tab">
      <p className="m-0 shrink-0 border-b border-border px-4 py-1 text-[10.5px] text-muted-foreground">
        {run
          ? `The Machines ${run.runId} ran on — its own worker, the Actors it dispatched to, and any fleet it brought up. A worker that booted and died leaves the run simply waiting, with no status on this page changing; the traceback is in one of these panes.`
          : 'The Machines under a run — its worker, the Actors it dispatched to, and any fleet it brought up.'}
      </p>
      <div className="flex min-h-0 flex-1 flex-col overflow-hidden">{children}</div>
    </div>
  );
}

/**
 * A tab whose contents are a later slice.
 *
 * IT NAMES THE RUN IT IS SCOPED TO, which is not filler: the scoping is the thing this slice has to
 * prove works, and a panel that said "coming soon" would prove nothing. What it must not do is
 * imply that the absence is a property of the run — so it says whose slice it is waiting on.
 */
function Later({
  tab,
  run,
  what,
  why,
}: {
  tab: string;
  run: RunRow | null;
  what: string;
  why: string;
}): JSX.Element {
  return (
    <div className="min-h-0 flex-1 overflow-y-auto p-4" data-testid={`${tab}-tab`} data-run={run?.runId ?? ''}>
      <p className="m-0 text-[12.5px]">
        {what}
        {run ? (
          <>
            {' — for '}
            <code className="font-mono">{run.runId}</code>.
          </>
        ) : (
          '.'
        )}
      </p>
      <p className="m-0 mt-1.5 text-[11.5px] text-muted-foreground">{why}</p>
      <p className="m-0 mt-1.5 text-[11.5px] text-muted-foreground">
        Not built yet. This tab is scoped and reachable; what goes in it is its own slice.
      </p>
    </div>
  );
}

function Empty({ testid, children }: { testid: string; children: ReactNode }): JSX.Element {
  return (
    <p className="m-0 p-4 text-[12.5px] text-muted-foreground" data-testid={testid}>
      {children}
    </p>
  );
}
