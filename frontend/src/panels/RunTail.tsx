/**
 * The open run, on the page that started it.
 *
 * ── WHY THIS EXISTS ───────────────────────────────────────────────────────────────────────────
 *
 * Pressing Run used to navigate to a global Runs surface. That is the wrong move at exactly the
 * wrong moment: the thing an author wants after starting a run is to WATCH IT — against the code
 * they just wrote, with the worker's pane beside it — and the navigation took all of that away and
 * replaced it with a page about runs in general. The editor, the file list, the pane and the input
 * they are about to change were all one click behind them, and the first thing they did was click
 * it.
 *
 * SO THE RUN COMES TO THE PAGE — and the surface it used to leave for is now retired, because a run
 * belongs to the workflow that produced it. This is the compact form: is it going, what has it done
 * lately, how far through each Batch is it, and what did it write. The full account of one run — its
 * transcript, its drill into the raw Temporal events, its fleet windows — lands on this same surface
 * under `/workflows/<workflow>/<run>`, which is the address this panel's run already has.
 *
 * ── WHAT "EMITS" CAN HONESTLY MEAN HERE ───────────────────────────────────────────────────────
 *
 * Not the workflow's return payload. `backend/src/history.ts` decodes NO payloads, on purpose
 * — a history read must not materialize a claim-checked Batch — so a panel promising "what it
 * emitted" as values would be promising something this architecture declines to fetch.
 *
 * What is real, and is what a run actually emits as it goes:
 *
 *   • THE EVENT LOG — every dispatch, every activity, every failure, with the Method's name on it.
 *   • THE HEARTBEATS — `done/total` per Batch, beaten by the `RunBatch` activity itself. This is
 *     the only live progress signal that keeps arriving from an actor whose host has stopped
 *     posting healthchecks, which is precisely when you want one.
 *   • THE MATERIALIZATION — which Datasets it wrote and how many rows landed. A run is `completed`
 *     with failed output often enough that these are two dimensions and never one (ADR 0017).
 *
 * POLLS ONLY WHILE A RUN IS OPEN. A settled run's history does not change, and a panel that kept
 * asking would be a request per second per open tab, forever, about an answer that is final.
 */

import { useCallback, useEffect, useState } from 'react';
import { ChevronDown, ChevronRight, RefreshCw } from 'lucide-react';
import {
  fetchRun,
  fetchRunHeartbeats,
  fetchRunHistory,
  type RunDetail,
  type RunEvent,
  type RunHeartbeat,
  type RunHistory,
} from '../run/api';
import { eventAside, eventName, eventSummary, withoutBookkeeping } from './workflowEvents';
import { usePersistedFlag } from './chrome/persistedFlag';
import { Button } from '@/components/ui/button';

/** How many of the newest events the tail draws. A tail, not a log: the full history — with its
 *  filters, its drill stack and its table mode — is the run's own view, and duplicating it here
 *  would be a second implementation to keep in agreement with the first. */
const TAIL = 40;

/** How often an OPEN run is re-read. Matches the run's own view so two readings of one run cannot
 *  show visibly different ages. */
const POLL_MS = 2000;

/**
 * COLLAPSIBLE BECAUSE IT IS NOT ALWAYS THE THING BEING READ. This panel takes height from the
 * editor on a page whose main job is editing, and an author who is writing code rather than
 * watching a run should be able to put it away — and find it still away tomorrow. Folded keeps the
 * HEADER, never the whole panel: a run that is going has to stay visible as a line even when its
 * log is not, or "did I start it?" becomes a question the page stopped answering.
 *
 * The remembering itself is `chrome/persistedFlag.ts`, shared with the nav rail — the storage
 * guards are subtler than they look (access can throw, not only the value) and one copy of them is
 * the point.
 */
const useFolded = usePersistedFlag;

export function RunTail({ runId }: { runId: string }): JSX.Element {
  const [folded, setFolded] = useFolded('kontra.runTail.folded');
  // OFF BY DEFAULT, AND REMEMBERED. `usePersistedFlag` is false until somebody sets it, so the
  // resting state of this log is the events that say something — and an operator who wants
  // Temporal's decision loop back gets it, on this run and on every run after it.
  const [paperwork, setPaperwork] = useFolded('kontra.runTail.paperwork');
  const [detail, setDetail] = useState<RunDetail | null>(null);
  const [history, setHistory] = useState<RunHistory | null>(null);
  const [beats, setBeats] = useState<Record<string, RunHeartbeat>>({});
  const [error, setError] = useState<string | null>(null);

  // A DIFFERENT RUN LEAVES NOTHING OF THE LAST ONE. An event log and a duration from the previous
  // run, sitting under a new run id, is worse than an empty panel — it is a wrong answer that
  // looks like a fast one.
  useEffect(() => {
    setDetail(null);
    setHistory(null);
    setBeats({});
    setError(null);
  }, [runId]);

  const read = useCallback(async () => {
    try {
      const [d, h, b] = await Promise.all([
        fetchRun(runId),
        fetchRunHistory(runId),
        fetchRunHeartbeats(runId).catch(() => ({}) as Record<string, RunHeartbeat>),
      ]);
      setDetail(d);
      setHistory(h);
      setBeats(b);
      setError(null);
      return d;
    } catch (err) {
      // REPORTED, NOT SWALLOWED — but the panel keeps whatever it last had. A poll that failed
      // once against a run that is fine must not blank the log somebody is reading.
      setError(err instanceof Error ? err.message : String(err));
      return null;
    }
  }, [runId]);

  useEffect(() => {
    let live = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const tick = async (): Promise<void> => {
      const d = await read();
      if (!live) return;
      // SETTLED STOPS THE POLL. A finished run's history is final; asking again forever is a
      // request per tab per second about an answer that cannot change.
      if (d?.settled) return;
      timer = setTimeout(() => void tick(), POLL_MS);
    };
    void tick();
    return () => {
      live = false;
      if (timer) clearTimeout(timer);
    };
  }, [read]);

  const events = history?.events ?? [];
  // FILTERED BEFORE IT IS WINDOWED, WHICH IS THE WHOLE POINT. Windowing first would spend the
  // 40 rows on the paperwork and then hide most of them, leaving a tail of four real events on a
  // run that had forty.
  const { rows: worth, held } = withoutBookkeeping(events);
  const tail = (paperwork ? events : worth).slice(-TAIL).reverse();
  const wrote = detail?.materializationRecords ?? [];
  const inFlight = Object.entries(beats).filter(([, b]) => b && typeof b.total === 'number');

  return (
    <div
      className="flex min-h-0 flex-col"
      data-testid="run-tail"
      data-run={runId}
      data-folded={folded ? 'true' : undefined}
    >
      <div className="flex shrink-0 flex-wrap items-center gap-2 border-b border-border px-3 py-1.5">
        {/* THE FOLD IS ON THE HEADER, and the header never folds with it. A run that is going has
            to stay one line on the page even when its log is put away, or "did I start it?"
            becomes a question this panel stopped answering. */}
        <button
          type="button"
          data-testid="run-tail-fold"
          aria-expanded={!folded}
          title={folded ? 'show this run’s event log' : 'hide the log — the run keeps going'}
          className="shrink-0 rounded px-0.5 text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
          onClick={() => setFolded(!folded)}
        >
          {folded ? <ChevronRight size={13} /> : <ChevronDown size={13} />}
        </button>
        <span className="font-mono text-[11px] font-semibold" data-testid="run-tail-id">
          {runId}
        </span>
        {/* THE TWO DIMENSIONS, NEVER COLLAPSED (ADR 0017). `completed` is Temporal's answer about
            the caller's workflow; whether its output is queryable is the ledger's, and a run is
            `completed` with failed output often enough that one badge would be a lie. */}
        <Badge
          label={detail?.execution ?? '…'}
          tone={
            detail?.execution === 'completed'
              ? 'text-emerald-500'
              : detail?.execution === 'running'
                ? 'text-sky-500'
                : detail?.execution
                  ? 'text-rose-500'
                  : 'text-muted-foreground'
          }
          testid="run-tail-execution"
        />
        {/* THE OUTPUT DIMENSION, as counts rather than as one word. `MaterializationSummary` has
            no single state and should not: `failed` and `complete` are both non-zero on a run that
            wrote three Datasets and lost one, and any word covering that case is the collapse
            ADR 0017 forbids. `n failed` is drawn only when it is not zero, and then it is the
            loudest thing here. */}
        {detail?.materialization && detail.materialization.total > 0 && (
          <Badge
            label={`${detail.materialization.rows.toLocaleString()} rows`}
            tone="text-muted-foreground"
            testid="run-tail-rows"
          />
        )}
        {detail?.materialization && detail.materialization.failed > 0 && (
          <Badge
            label={`${detail.materialization.failed} output failed`}
            tone="text-rose-500"
            testid="run-tail-output-failed"
          />
        )}
        <span className="font-mono text-[10px] tabular-nums text-muted-foreground">
          {events.length} event{events.length === 1 ? '' : 's'}
        </span>
        {/* THE PAPERWORK IS NAMED AND COUNTED, NEVER JUST GONE. A log that drew 59 of 140 rows and
            said nothing about the other 81 would be lying by omission — the same failure as the
            log that drew all 140 and buried the six that mattered. */}
        {held > 0 && (
          <button
            type="button"
            data-testid="run-tail-paperwork"
            aria-pressed={paperwork}
            title={
              paperwork
                ? 'hide Temporal’s own decision loop — WorkflowTaskScheduled/Started/Completed, three per decision, naming nothing about the run'
                : 'show Temporal’s own decision loop as well'
            }
            onClick={() => setPaperwork(!paperwork)}
            className={`rounded border border-border px-1.5 font-mono text-[10px] tabular-nums outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/50 ${
              paperwork ? 'text-foreground' : 'text-muted-foreground'
            }`}
          >
            {paperwork ? `hide ${held} bookkeeping` : `+${held} bookkeeping`}
          </button>
        )}
        <div className="ml-auto flex items-center gap-1">
          <Button
            variant="ghost"
            size="sm"
            className="h-6 px-1.5"
            data-testid="run-tail-refresh"
            title="re-read this run now"
            onClick={() => void read()}
          >
            <RefreshCw size={11} />
          </Button>
          {/* THE LINK OUT IS GONE, AND SO IS THE PLACE IT WENT. It said "Runs" and jumped to the
              global Runs surface; that surface is retired, because a run is reached through the
              workflow that produced it. The run this panel is showing is already the run the
              address names (`/workflows/<workflow>/<run>`), so the URL in the bar IS the link —
              there is nothing left for a button to do that copying the address does not. */}
        </div>
      </div>

      {/* AN ERROR IS NOT FOLDED AWAY. Everything below this point is hidden while folded; a
          failure to read the run is not, because a panel that quietly stopped updating and said
          nothing about it is the exact shape of bug this whole page exists to prevent. */}
      {error && (
        <p
          className="m-0 shrink-0 border-b border-border bg-destructive/10 px-3 py-1.5 text-[10.5px] text-destructive"
          data-testid="run-tail-error"
        >
          {error}
        </p>
      )}

      {folded ? null : (
        <>

      {/* WHAT IS IN FLIGHT RIGHT NOW. `done/total` comes from the `RunBatch` activity's own
          heartbeat, so it keeps arriving from an actor whose host has stopped posting
          healthchecks — which is exactly the situation where every other signal goes quiet. */}
      {inFlight.length > 0 && (
        <div className="shrink-0 border-b border-border px-3 py-1.5" data-testid="run-tail-beats">
          {inFlight.map(([id, b]) => (
            <div key={id} className="flex items-center gap-2 font-mono text-[10.5px]">
              <span className="min-w-0 flex-1 truncate text-muted-foreground">{id}</span>
              <span className="tabular-nums">
                {b.done ?? 0}/{b.total ?? 0}
              </span>
            </div>
          ))}
        </div>
      )}

      {/* WHAT THE RUN WROTE, per Actor. A dispatch's output lands in a Dataset partition keyed by
          the Actor and version that produced it, so that pair is the row's identity — and `state`
          rides beside the count because `complete` with `rows: 0` is a successful empty result and
          `failed` with rows is a partial one, and neither reads correctly as a bare number. */}
      {wrote.length > 0 && (
        <div className="shrink-0 border-b border-border px-3 py-1.5" data-testid="run-tail-wrote">
          {wrote.map((r, i) => (
            <div
              key={`${r.actor ?? ''}@${r.version ?? ''}/${r.node ?? i}`}
              className="flex items-center gap-2 font-mono text-[10.5px]"
            >
              <span className="min-w-0 flex-1 truncate">
                {r.actor ? `${r.actor}${r.version ? `@${r.version}` : ''}` : (r.node ?? '—')}
              </span>
              {r.state && (
                <span
                  className={r.state === 'failed' ? 'text-rose-500' : 'text-muted-foreground'}
                >
                  {r.state}
                </span>
              )}
              <span className="tabular-nums text-muted-foreground">
                {typeof r.rows === 'number' ? `${r.rows.toLocaleString()} rows` : ''}
              </span>
            </div>
          ))}
        </div>
      )}

      {/* NEWEST FIRST. A tail is read from the top — the question is "what just happened", and a
          log that put the answer at the bottom of a scroll would need following to be useful. */}
      <div className="min-h-0 flex-1 overflow-y-auto" data-testid="run-tail-events">
        {tail.length === 0 ? (
          <p className="m-0 p-3 text-[11px] text-muted-foreground">
            {detail === null && error === null
              ? 'reading the history…'
              : 'no events yet — a run that has just started has none until its first workflow task closes'}
          </p>
        ) : (
          tail.map((e) => <EventRow key={e.id} event={e} />)
        )}
          </div>
        </>
      )}
    </div>
  );
}

/** `4:51:18 PM` in the reader's own locale — the format the transcript already prints beside a
 *  narration, so two readings of one run agree on what to call the same instant. */
function clockOf(at: number): string {
  if (!at) return '';
  return new Date(at).toLocaleTimeString(undefined, {
    hour: 'numeric',
    minute: '2-digit',
    second: '2-digit',
  });
}

function EventRow({ event }: { event: RunEvent }): JSX.Element {
  // `eventName` puts the Summary first where the event carries one, so a `speak` reads as its
  // sentence and a dispatch as its Method — the two facts this tail could not say before. What the
  // Summary displaced follows it, muted, so no row is poorer than it was.
  const name = eventName(event);
  const aside = eventAside(event);
  return (
    <div
      className="flex items-baseline gap-2 border-b border-border/40 px-3 py-1 font-mono text-[10.5px]"
      data-testid={`run-tail-event-${event.id}`}
      data-cat={event.cat}
    >
      {/* THE WALL CLOCK FIRST, THEN THE OFFSET. `+1731.9s` answers "how far into the run" and
          nothing else; a run that parked on an ask for nineteen minutes has a gap there that only
          a clock explains, and correlating a row with a tmux pane or somebody else's log needs the
          time of day rather than an offset from an origin only this page knows. */}
      <span className="w-[70px] shrink-0 text-right tabular-nums text-muted-foreground">
        {clockOf(event.at)}
      </span>
      <span className="w-14 shrink-0 text-right tabular-nums text-muted-foreground/60">
        +{event.t.toFixed(1)}s
      </span>
      <span
        className={`shrink-0 ${event.cat === 'failure' ? 'text-rose-500' : 'text-foreground'}`}
      >
        {event.type}
      </span>
      {name && (
        <span
          className="min-w-0 flex-1 truncate text-muted-foreground"
          title={aside ? `${name} · ${aside}` : name}
        >
          <span className={eventSummary(event) ? 'text-foreground' : ''}>{name}</span>
          {aside && <span className="ml-2">{aside}</span>}
        </span>
      )}
    </div>
  );
}

function Badge({
  label,
  tone,
  testid,
}: {
  label: string;
  tone: string;
  testid: string;
}): JSX.Element {
  return (
    <span
      className={`rounded-full bg-muted px-1.5 font-mono text-[9.5px] uppercase tracking-wide ${tone}`}
      data-testid={testid}
    >
      {label}
    </span>
  );
}
