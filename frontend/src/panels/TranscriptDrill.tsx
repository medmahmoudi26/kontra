/**
 * The raw Temporal events behind one turn, and the child workflows they lead to.
 *
 * TEMPORAL IS ONE CLICK AWAY, WHICH IS WHAT THE VOCABULARY IS BOUGHT WITH. Every row in the
 * transcript is a rename of some events, and a rename nobody can check is a rename nobody should
 * trust. This is the check: the event ids the turn folded, their Temporal types verbatim, their
 * attempts, their one-line metadata, in the log's own order. An operator who disagrees with a name
 * can see exactly what it was read from and say so.
 *
 * IT IS A PANEL, NOT A PAGE, and it opens on the run already on screen. The retired Runs surface
 * made an event log a place you navigated TO, which meant leaving the account to check it — and
 * having left, the way back was another search. Here the transcript stays where it is and the raw
 * view opens beside it.
 *
 * NOTHING HERE FETCHES AT THE ROOT. The rows come from the reduced log the account was read from
 * (`RunTurnsRead.history`), so opening a turn is instant and costs nothing. A CHILD costs one read,
 * because a child is a different workflow with its own history — and that read is `useDrillLevel`'s,
 * one at a time, torn down when the level changes or the panel closes.
 *
 * NOTHING BLINKS. A live child's rows arrive in place on the same cadence the transcript uses; there
 * is no pulse on a new row and no spinner between polls. The only moving thing on screen is content.
 */

import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';

import type { NamedTurn } from '@core/vocabulary';

import { readHistory, type RunEvent, type RunHistory } from '../run/api';
import { FOLLOW_MAX_MISSES, FOLLOW_POLL_MS } from '../run/follow';
import {
  drillInto,
  historyClosed,
  linkLabel,
  popTo,
  rootLevel,
  shortId,
  spanMs,
  type DrillLevel,
} from './eventDrill';
import { openerFor, rowsFor, subjectOf, turnKey, type DrillSubject } from './transcriptDrill';
import { eventAside, eventSummary } from './workflowEvents';

/** What one level of the drill has to draw. */
export interface DrillLevelView {
  rows: RunEvent[];
  /** Ids the turn names that the log on hand does not carry — an elision, printed. */
  missing: number[];
  /** A child's history has been asked for and has not answered yet. Never true at the root, which
   *  reads from bytes the page already has. */
  loading: boolean;
  /** Why this level has no rows. `gone` is Temporal having no execution under that id — which for a
   *  child is retention or a start that failed, and is not "we could not ask". */
  failure: { gone: boolean; detail: string } | null;
  /** Wall time this level spans, in ms. MEASURED on `nscheck-1786831339`: the fleet child is four
   *  opaque rows in the parent covering 156 seconds, which is the number this prints. */
  span: number;
  /** This level's own execution has closed. A closed child inside a running run stops being re-read;
   *  a running child inside a settled run keeps being re-read. */
  closed: boolean;
}

/**
 * One level's rows: the root's out of the log in hand, a child's off the history route.
 *
 * EXACTLY ONE POLLER, BY CONSTRUCTION. One level is open at a time, so this hook holds at most one
 * interval however deep the stack goes — and it stops the moment the level closes, the stack moves,
 * or the panel unmounts. `FOLLOW_MAX_MISSES` bounds a level that will never answer, so a drill left
 * open on a child Temporal has dropped is not a request every two seconds forever.
 */
export function useDrillLevel(
  stack: readonly DrillLevel[],
  root: { history: RunHistory | null; subject: DrillSubject | null }
): DrillLevelView {
  const depth = stack.length - 1;
  const level = depth > 0 ? stack[depth] : undefined;
  const [child, setChild] = useState<DrillLevelView | null>(null);
  // The level a `child` belongs to, so a state left over from the level we just came from is never
  // drawn under this one's crumb: one child's events under another child's heading is the same
  // class of lie as the previous run's transcript under this run's heading.
  const shown = useRef<string>('');

  const key = level ? `${level.workflowId} ${level.execId ?? ''}` : '';
  const workflowId = level?.workflowId;
  const execId = level?.execId;
  useEffect(() => {
    if (!workflowId) {
      setChild(null);
      shown.current = '';
      return;
    }
    let alive = true;
    let misses = 0;
    let timer: ReturnType<typeof setInterval> | null = null;
    const stop = (): void => {
      if (timer !== null) clearInterval(timer);
      timer = null;
    };
    const read = (): void => {
      void readHistory(workflowId, execId).then((r) => {
        if (!alive) return;
        shown.current = `${workflowId} ${execId ?? ''}`;
        if (r.ok) {
          misses = 0;
          const closed = historyClosed(r.history.events);
          if (closed) stop();
          setChild({
            rows: r.history.events,
            missing: [],
            loading: false,
            failure: null,
            span: spanMs(r.history.events),
            closed,
          });
          return;
        }
        misses += 1;
        if (misses >= FOLLOW_MAX_MISSES) stop();
        setChild((prev) => ({
          rows: prev?.rows ?? [],
          missing: [],
          loading: false,
          failure: { gone: r.gone, detail: r.detail },
          span: prev?.span ?? 0,
          closed: prev?.closed ?? false,
        }));
      });
    };
    setChild({ rows: [], missing: [], loading: true, failure: null, span: 0, closed: false });
    read();
    timer = setInterval(read, FOLLOW_POLL_MS);
    return () => {
      alive = false;
      stop();
    };
  }, [workflowId, execId]);

  if (level) {
    if (child && shown.current === key) return child;
    return { rows: [], missing: [], loading: true, failure: null, span: 0, closed: false };
  }
  const subject = root.subject;
  if (!subject) return { rows: [], missing: [], loading: false, failure: null, span: 0, closed: true };
  const { rows, missing } = rowsFor(root.history, subject.events);
  return { rows, missing, loading: false, failure: null, span: spanMs(rows), closed: true };
}

export interface TranscriptDrillProps {
  /** The run the root level is about. A **Run** is its caller workflow's id and nothing else. */
  runId: string;
  /** The turn the drill was opened from — its name, and the metadata that name rests on. */
  named: NamedTurn;
  /** The levels below it, root first. `eventDrill.ts` owns how this grows and shrinks. */
  stack: readonly DrillLevel[];
  view: DrillLevelView;
  /** Descend from a row. The EVENT, not a link, so `drillInto`'s return-not-push rule is the only
   *  rule there is. */
  onDrill: (event: RunEvent) => void;
  /** Return to a level of the chain, dropping everything below it. */
  onPop: (index: number) => void;
  onClose: () => void;
}

export function TranscriptDrill({
  runId,
  named,
  stack,
  view,
  onDrill,
  onPop,
  onClose,
}: TranscriptDrillProps): JSX.Element {
  const depth = stack.length - 1;
  const subject = subjectOf(named.turn);
  const targets = depth === 0 ? subject.links : [];

  return (
    <div
      className="flex min-h-0 flex-1 flex-col overflow-hidden"
      data-testid="turn-drill"
      data-run={runId}
      data-depth={depth}
    >
      <header className="flex shrink-0 flex-wrap items-baseline gap-2 border-b border-border bg-muted/40 px-4 py-1.5">
        <span className="text-[10px] uppercase tracking-wide text-muted-foreground">Raw events</span>
        <span className="text-[11.5px]" data-testid="drill-of">
          {named.label}
        </span>
        {/* THE NAME AND WHAT IT RESTS ON, TOGETHER — the same rule the transcript row follows. The
            drill is where a disputed name gets settled, so the claim has to be beside the evidence. */}
        <span className="font-mono text-[10px] text-muted-foreground">{named.because}</span>
        <button
          type="button"
          data-testid="drill-close"
          title="close the raw view and keep the account"
          onClick={onClose}
          className="ml-auto rounded px-1.5 text-[10.5px] text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
        >
          close
        </button>
      </header>

      <Crumbs runId={runId} stack={stack} onPop={onPop} />

      <div className="flex shrink-0 flex-wrap items-baseline gap-x-3 gap-y-1 border-b border-border px-4 py-1 font-mono text-[10px] text-muted-foreground">
        {/* THE TYPES, VERBATIM. This is the row that makes an untranslated turn worth opening: its
            label already IS the Temporal type, and here are the others the same turn folded. */}
        <span data-testid="drill-types">{subject.types.join(' | ') || '-'}</span>
        {depth === 0 && subject.events.length > 0 && (
          <span data-testid="drill-events">
            event{subject.events.length === 1 ? '' : 's'} {subject.events.join(', ')}
          </span>
        )}
        {view.span > 0 && (
          <span className="tabular-nums" data-testid="drill-span">
            {(view.span / 1000).toFixed(2)}s of wall time
          </span>
        )}
        {depth > 0 && !view.closed && !view.loading && (
          <span data-testid="drill-open">still running - this level is being re-read</span>
        )}
      </div>

      {targets.length > 0 && (
        <div
          className="flex shrink-0 flex-wrap items-baseline gap-1.5 border-b border-border px-4 py-1"
          data-testid="drill-targets"
        >
          <span className="font-mono text-[10px] uppercase tracking-wide text-muted-foreground">Into</span>
          {targets.map((link) => {
            const opener = openerFor(view.rows, link);
            return (
              <button
                key={`${link.workflowId}:${link.execId ?? ''}`}
                type="button"
                disabled={!opener}
                data-testid={`drill-into-${link.workflowId}`}
                title={
                  opener
                    ? `open ${link.workflowId}${link.execId ? ` (execution ${link.execId})` : ''}`
                    : // The opener is not in the rows this level holds - an elided log. Saying so is
                      // better than a button that does nothing.
                      'the event that opened this workflow is not in the log on hand'
                }
                onClick={() => opener && onDrill(opener)}
                className="rounded border border-border px-1.5 py-px font-mono text-[10px] outline-none hover:bg-accent disabled:opacity-40 focus-visible:ring-2 focus-visible:ring-ring/50"
              >
                {linkLabel(link)}
              </button>
            );
          })}
        </div>
      )}

      <div className="min-h-0 flex-1 overflow-y-auto" data-testid="drill-rows">
        {view.loading ? (
          <Note testid="drill-loading">Reading {shortId(stack[depth]?.workflowId ?? runId)}...</Note>
        ) : view.failure ? (
          <Note testid={view.failure.gone ? 'drill-gone' : 'drill-error'}>
            {view.failure.gone
              ? // NOT THE SAME SENTENCE AS THE RUN'S OWN. A child that aged out, and a child whose
                // start failed so no execution was ever recorded, both land here - and neither means
                // the appliance is unwell.
                `Temporal has no execution under ${stack[depth]?.workflowId ?? runId}. It aged out at retention, or it never started.`
              : `Could not read this workflow: ${view.failure.detail}`}
          </Note>
        ) : subject.eventless && depth === 0 ? (
          <Note testid="drill-eventless">
            {/* THE ONE TURN WITH NO EVENT, AND IT SAYS WHY. An ask is published by the workflow at
                park time and read from its own route; it has no history event yet, so there is
                nothing here to check - which is not the same as a log that lost its rows. */}
            This turn came from the run{"'"}s asks, not from its event log, so there is no Temporal
            event behind it to show. Every other turn on the transcript has one.
          </Note>
        ) : view.rows.length === 0 ? (
          <Note testid="drill-empty">No events at this level.</Note>
        ) : (
          <table className="w-full border-collapse text-left">
            <tbody>
              {view.rows.map((e) => (
                <Row key={e.id} event={e} onDrill={onDrill} />
              ))}
            </tbody>
          </table>
        )}

        {view.missing.length > 0 && (
          <p className="m-0 px-4 py-1 text-[11px] text-amber-500" data-testid="drill-missing">
            {/* THE HOLE, NAMED. `/history` drops events from the middle of a long log to stay under
                its cap; a turn built before that can name a row that is no longer on hand. Showing
                the rest without saying so would make the check quietly incomplete. */}
            {view.missing.length} event{view.missing.length === 1 ? '' : 's'} this turn folded (
            {view.missing.join(', ')}) {view.missing.length === 1 ? 'is' : 'are'} not in the log on
            hand - the history was elided to stay under its cap.
          </p>
        )}
      </div>
    </div>
  );
}

/**
 * Where you are, and everywhere you can go back to.
 *
 * A STACK, NOT A BACK BUTTON, and `eventDrill.ts` says why: a dispatch inside a fleet-scoped child
 * has two places to return to, and "back" would have to mean one of them.
 */
function Crumbs({
  runId,
  stack,
  onPop,
}: {
  runId: string;
  stack: readonly DrillLevel[];
  onPop: (index: number) => void;
}): JSX.Element | null {
  if (stack.length < 2) return null;
  return (
    <nav
      className="flex shrink-0 flex-wrap items-baseline gap-1 border-b border-border px-4 py-1"
      data-testid="drill-crumbs"
    >
      {stack.map((lv, i) => (
        <span key={`${lv.workflowId}:${lv.execId ?? ''}`} className="flex items-baseline gap-1">
          {i > 0 && <span className="text-[10px] text-muted-foreground">/</span>}
          <button
            type="button"
            data-testid={`drill-crumb-${i}`}
            data-current={i === stack.length - 1 ? 'true' : undefined}
            // THE WHOLE ID AND THE EXECUTION BEHIND IT: the crumb is shortened to fit, and the one
            // thing that tells a fleet bring-up from its teardown is the execution id.
            title={`${lv.workflowId}${lv.execId ? ` - execution ${lv.execId}` : ''}${lv.from ? ` - from event ${lv.from}` : ''}`}
            onClick={() => onPop(i)}
            className={`rounded px-1 font-mono text-[10.5px] outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/50 ${
              i === stack.length - 1 ? 'font-semibold text-foreground' : 'text-muted-foreground'
            }`}
          >
            {i === 0 ? shortId(runId) : lv.type || shortId(lv.workflowId)}
          </button>
        </span>
      ))}
    </nav>
  );
}

function Row({ event, onDrill }: { event: RunEvent; onDrill: (e: RunEvent) => void }): JSX.Element {
  return (
    <tr
      className="border-b border-border/40 align-baseline"
      data-testid={`drill-row-${event.id}`}
      data-cat={event.cat}
      data-type={event.type}
    >
      <td className="w-10 px-4 py-0.5 text-right font-mono text-[10px] tabular-nums text-muted-foreground">
        {event.id}
      </td>
      <td className="w-16 py-0.5 text-right font-mono text-[10px] tabular-nums text-muted-foreground">
        +{Math.round(event.t * 100) / 100}s
      </td>
      <td className="py-0.5 pl-2 font-mono text-[11px]">
        {/* THE TEMPORAL TYPE, ALWAYS, AT EVERY DEPTH. This column is the whole reason the panel
            exists: whatever the transcript called this, here is what Temporal called it. */}
        {event.type}
        {event.attempt > 1 && (
          <span className="ml-1.5 text-[9.5px] text-amber-500" title="Temporal's attempt counter">
            attempt {event.attempt}
          </span>
        )}
      </td>
      {/* THE SUMMARY, THEN THE MACHINERY THAT CARRIED IT. This is the pane a reader arrives at to
          CHECK a name, so the line a human wrote has to survive the trip: a narration's raw row is
          `TimerStarted` + `timerId=1` without it, which checks nothing. `eventAside` keeps the
          metadata beside it, so a row with no Summary is untouched. */}
      <td className="min-w-0 py-0.5 pl-2 font-mono text-[10.5px] text-muted-foreground">
        {eventSummary(event) ? (
          <>
            <span className="text-foreground">{eventSummary(event)}</span>
            {eventAside(event) && <span className="ml-2">{eventAside(event)}</span>}
          </>
        ) : (
          event.detail
        )}
      </td>
      <td className="w-16 py-0.5 pr-4 text-right font-mono text-[10px] tabular-nums text-muted-foreground">
        {event.dur > 0 ? `${Math.round(event.dur * 100) / 100}s` : ''}
      </td>
      <td className="w-24 py-0.5 pr-4 text-right">
        {event.link && (
          <button
            type="button"
            data-testid={`drill-row-into-${event.id}`}
            title={`open ${event.link.workflowId}`}
            onClick={() => onDrill(event)}
            className="rounded border border-border px-1 font-mono text-[9.5px] outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/50"
          >
            {linkLabel(event.link)}
          </button>
        )}
      </td>
    </tr>
  );
}

function Note({ testid, children }: { testid: string; children: ReactNode }): JSX.Element {
  return (
    <p className="m-0 px-4 py-2 text-[11.5px] text-muted-foreground" data-testid={testid}>
      {children}
    </p>
  );
}

/* ───────────────────────────── the drill, wired ───────────────────────────── */

/**
 * The drill with its own stack and its own level read — what a page mounts.
 *
 * THE STACK IS RESET BY IDENTITY, NOT BY OBJECT. A followed transcript hands down a NEW `NamedTurn`
 * on every arrival, so resetting on the prop itself would drop the operator back to the root every
 * two seconds while they were reading a child. It resets on the RUN and on the TURN KEY, which are
 * strings and change only when the subject really does.
 *
 * WHICH IS ALSO WHY THE CALLER PASSES A TURN AND NOT A KEY. The page looks the turn up in the CURRENT
 * reading each render, so a drilled loop that gains members shows the new events in place — the same
 * arrival the transcript row above it is having, in the panel below it, without a reopen.
 */
export function TurnDrill({
  runId,
  named,
  history,
  onClose,
}: {
  runId: string;
  named: NamedTurn;
  /** The reduced log the account was read from. The root level reads from this and fetches nothing. */
  history: RunHistory | null;
  onClose: () => void;
}): JSX.Element {
  const key = turnKey(named.turn);
  const [stack, setStack] = useState<DrillLevel[]>(() => [rootLevel(runId)]);
  useEffect(() => setStack([rootLevel(runId)]), [runId, key]);
  const subject = useMemo(() => subjectOf(named.turn), [named.turn]);
  const view = useDrillLevel(stack, { history, subject });
  return (
    <TranscriptDrill
      runId={runId}
      named={named}
      stack={stack}
      view={view}
      onDrill={(e) => setStack((prev) => drillInto(prev, e))}
      onPop={(i) => setStack((prev) => popTo(prev, i))}
      onClose={onClose}
    />
  );
}
