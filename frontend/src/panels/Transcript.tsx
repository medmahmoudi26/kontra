/**
 * A Run's account of itself, drawn.
 *
 * IT RENDERS A READING, IT DOES NOT MAKE ONE. Every word on screen here comes from
 * `@core/vocabulary` — the term, the label, the tone, the sentence under it, the concerns and the
 * chips — and this file decides only where each one goes. That split is what lets the whole
 * vocabulary be asserted in `vocabulary.test.ts` against bytes on disk while this file is asserted
 * as markup, and it is why a better word for something ships by changing one row of `TERMS` rather
 * than by touching a component.
 *
 * THE `because` IS ON THE ROW, NOT IN A TOOLTIP. A translation layer that renames confidently is
 * worse than raw events (ADR 0027, one level down), so every name here is shown beside the metadata
 * it rests on — the type, the endpoint, the attempt, the count of members. An operator who
 * disagrees with a name can see what it was read from and say so, which is the difference between
 * a vocabulary and a skin.
 *
 * THE CONCERNS ARE ABOVE THE TURNS, deliberately, and this is the half that is not cosmetic. A run
 * whose Units all failed closes `completed` with an empty Dataset and not one failure event
 * anywhere in its history; a dispatch onto a queue nobody polls reads `running` until a timeout
 * fires. Neither has a row that looks wrong, so neither is findable by reading rows — they are
 * raised at the top, from the same metadata, or they are invisible exactly as they have always
 * been.
 *
 * A FOLDED LOOP IS A `<details>`, WITH NO STATE ANYWHERE. Two hundred dispatches are one line until
 * somebody opens them, and the browser already owns that interaction — so expanding costs no
 * `useState`, no re-render of the transcript, and nothing this file has to remember when the
 * operator changes run. It also means the expanded case renders under `renderToStaticMarkup`, so
 * "the member that failed is in there" is a string assertion rather than a click.
 *
 * NOTHING HERE FETCHES AND NOTHING HERE POLLS, WHICH IS STILL TRUE NOW THAT IT STREAMS. Following is
 * `run/follow.ts`'s job; this file is handed a fresh reading whenever the run actually did something
 * and re-renders in place. There is no pulse on an arriving turn and no spinner between reads —
 * arrival is arrival, not animation — and the one thing this file DOES say about a live run is which
 * of the two it is: still open and being followed, or still open and not.
 *
 * EVERY TURN DRILLS. A name nobody can check is a name nobody should trust (ADR 0027), so each row
 * carries the count of Temporal events it folded and opens onto them. It is a button rather than a
 * hover, because a reader who has to discover the check does not have it.
 *
 * AND IT DRAWS THE DOMAIN ACCOUNT, NOT THE EVENT LOG. `named.turns` is what kontra has a word for;
 * the turns it has none for are `named.untranslated` and belong to the sibling tab, verbatim. That
 * boundary is `vocabulary.ts`'s and is not re-decided here — this file could not draw a
 * `WorkflowTaskScheduled` if it tried, which is the point of making the split upstream. It cost a
 * real run's only human-written sentence its reader to have both in one column: three rows of
 * scheduler bookkeeping either side of it, and the operator scrolled past the one line that
 * mattered. What the pane still owes that reader is the COUNT of what it is not drawing, which is
 * {@link Chips}' second half.
 */

import { filtersOf, nameTurn, type Concern, type Naming, type NamedTurn, type Tone } from '@core/vocabulary';
import { expand, type Turn } from '@core/transcript';

import type { FollowPhase } from '../run/follow';
import type { RunTurns } from '../run/turns';
import { AskTurn, type AskDeck } from './AskTurn';
import { deadlineWords, readAsks, waitedWords, waitingAsks, type AskReading } from './ask';
import { turnKey } from './transcriptDrill';

/**
 * How each tone is painted.
 *
 * `unknown` IS MUTED AND IS NOT AMBER. It means this release had no word for the event, which is a
 * gap in the vocabulary rather than a fact about the run — colouring it like a warning would make
 * every untranslated Temporal type look like something the operator had to act on.
 */
const TONE: Record<Tone, string> = {
  ok: 'text-emerald-500',
  busy: 'text-sky-500',
  wrong: 'text-rose-500',
  unknown: 'text-muted-foreground',
};

const DOT: Record<Tone, string> = {
  ok: 'bg-emerald-400',
  busy: 'bg-sky-400',
  wrong: 'bg-rose-400',
  unknown: 'bg-zinc-400',
};

/**
 * What the pill on a still-open run says, per reader state.
 *
 * `settled` IS NOT IN HERE because the pill is only drawn on a live transcript, and a reading that
 * says `live` while its reader says `settled` is one frame of a run that just closed — the verdict
 * beside it is already the closing one. `none` is a caller with no reader at all, which is a real
 * state and the honest default: a snapshot.
 */
const FOLLOW_WORD: Record<FollowPhase | 'none', string> = {
  following: 'still open - following',
  reading: 'still open - reading',
  settled: 'still open - following',
  stale: 'still open - NOT following',
  refused: 'still open - NOT following',
  none: 'still open - not following',
};

const FOLLOW_TITLE: Record<FollowPhase | 'none', string> = {
  following: 'this run has not closed, and this account is being re-read. New turns arrive here without a refresh.',
  reading: 'the first read of this run is out; the account below arrives when it answers',
  settled: 'this run has not closed, and this account is being re-read',
  stale: 'this run has not closed and the last read of it FAILED. What is below was true when it was read.',
  refused: 'too many runs are already being followed on this page, so this one is not. What is below was true when it was read.',
  none: 'this run has not closed. The account below is what had been recorded when it was read.',
};

/** `+12.34s`. The offset from the run's first event, which is the clock a transcript is read on. */
function offset(t: number): string {
  return `+${Math.round(t * 100) / 100}s`;
}

/**
 * The wall clock a sentence was said at, in the READER'S timezone and the reader's own format.
 *
 * THE ONE EXCEPTION TO THE OFFSET, AND ONLY FOR THE ONE TURN THAT IS A HUMAN SENTENCE. Every other
 * row on this panel is measured from the run's first event, because for a derived turn the elapsed
 * shape IS the interest — a dispatch at +0.4s and its close at +156s is the fact. A narration is not
 * derived from anything: an author wrote a line for a person to read, and "how many milliseconds
 * after the run began" is not how that person knows when their afternoon happened. `toLocaleTimeString`
 * rather than a fixed pattern for the same reason — it is their clock, not ours.
 *
 * `at` is epoch ms off the event, so this never reads `Date.now()` and a row renders the same string
 * every time it is drawn, which is what makes it assertable at all.
 */
export function clockOf(at: number): string {
  if (!at) return '';
  return new Date(at).toLocaleTimeString();
}

/** `1.24s`, `2m 05s`. Seconds, because that is the unit every duration on a turn is already in. */
function secs(s: number): string {
  if (!Number.isFinite(s) || s <= 0) return '';
  if (s < 60) return `${Math.round(s * 100) / 100}s`;
  const m = Math.floor(s / 60);
  return `${m}m ${String(Math.floor(s % 60)).padStart(2, '0')}s`;
}

/* ───────────────────────────── the whole reading ───────────────────────────── */

export interface TranscriptViewProps {
  turns: RunTurns;
  /**
   * What the reader is doing, when a reader is attached.
   *
   * OPTIONAL, AND ABSENT MEANS NOT FOLLOWING. A caller that hands a reading in with no follower is
   * showing a snapshot, and this must say so rather than assume the good case — a live run drawn as
   * though something were watching it is how an operator stops watching a run that is still going.
   */
  follow?: FollowPhase;
  /** Open the raw Temporal events behind one turn. Absent on a surface with nowhere to put them. */
  onDrill?: (named: NamedTurn) => void;
  /** The turn whose raw events are open, by {@link turnKey}. Marked, never highlighted loudly:
   *  knowing which row the panel below is about should cost no attention. */
  openTurn?: string | null;
  /**
   * What an operator needs to ANSWER a question this run is parked on.
   *
   * OPTIONAL, AND ABSENT MEANS READ-ONLY — which is a real state and not a degraded one. An
   * archived run's asks are history; a surface with nowhere to send a signal must draw the
   * questions and the answers and no button. The transcript reads the same either way.
   */
  deck?: AskDeck;
  /**
   * The clock the parks are measured against.
   *
   * PASSED IN, NEVER `Date.now()` TAKEN HERE, for the reason `turns.ts` gives about the reader's
   * own `now`: "parked for 4 minutes" has to be a testable number, and a component that read the
   * wall clock during render would make every ask row untestable and every re-render a different
   * answer. The page owns the tick.
   */
  now?: number;
}

export function TranscriptView({
  turns,
  follow,
  onDrill,
  openTurn,
  deck,
  now,
}: TranscriptViewProps): JSX.Element {
  const { named, runId, live, asks } = turns;
  const { transcript, verdict, concerns, turns: rows, untranslated } = named;

  // The clock only matters where there is a park to measure, so a caller with no asks pays nothing
  // for not passing one. `readAsks` is a walk over a handful of entries, never over the log.
  const clock = now ?? Date.now();
  const readings = readAsks(asks, clock, live);
  const byId = new Map(readings.map((r) => [r.ask.id, r]));
  const waiting = waitingAsks(readings);

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden" data-testid="transcript" data-run={runId}>
      <Verdict verdict={verdict} live={live} follow={follow} archived={transcript.archived} />

      {/* ABOVE THE CONCERNS, because it is the one thing on this panel that is about the reader.
          A concern is something the run did; a park is something the run is waiting for THEM to
          do, and an operator who scrolls past it is the reason the run is not moving. */}
      {waiting.length > 0 && <Waiting waiting={waiting} />}

      {concerns.length > 0 && <Concerns concerns={concerns} />}

      <Chips turns={rows} elsewhere={untranslated} />

      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-2" data-testid="transcript-turns">
        {rows.length === 0 ? (
          <p className="m-0 text-[12px] text-muted-foreground" data-testid="transcript-empty">
            {/* AN EMPTY LOG IS NOT AN EMPTY RUN, and saying "this run did nothing" would be a
                claim about the run rather than about the account of it. */}
            No events in this account. The log is empty — which says nothing about what the run did,
            only that there is nothing recorded here to read.
          </p>
        ) : (
          rows.map((t) => {
            // ONE IDENTITY FOR THE ROW AND FOR THE DRILL. `turnKey` is React's reconciliation key
            // AND the answer to "which turn is the panel below about" — the same string, from one
            // function, so a loop that gains members while the drill is open cannot silently swap
            // which turn is being shown.
            const key = turnKey(t.turn);
            // A PARK IS A QUESTION AND IT IS DRAWN AS ONE — in place, in order, with the turns that
            // produced it above and whatever happened next below. The route's reading is looked up
            // by id because the parked turn carries only the half `transcript.ts` declares; a park
            // whose ask did not arrive falls through to the ordinary row, which still says the run
            // is waiting on a human. Missing the form is enormously better than missing the turn.
            if (t.turn.kind === 'parked') {
              const reading = byId.get(t.turn.ask.id);
              if (reading) {
                return (
                  <AskTurn
                    key={key}
                    reading={reading}
                    {...(deck ? { deck } : {})}
                    testId={`ask-${reading.ask.id}`}
                  />
                );
              }
            }
            return (
              <TurnRow
                key={key}
                named={t}
                turnKey={key}
                {...(onDrill ? { onDrill } : {})}
                open={openTurn === key}
              />
            );
          })
        )}
      </div>
    </div>
  );
}

/**
 * The one line at the top: how this run reads, in one term.
 *
 * `completed` IS THREE ANSWERS AND THIS IS WHERE THAT SHOWS. A run that published something, one
 * that published nothing, and one that published nothing while things were failing inside it are
 * three different mornings, and Temporal calls all three `WorkflowExecutionCompleted`. The verdict
 * is `vocabulary.ts`'s, not a status pill's, precisely so the three cannot render the same.
 */
function Verdict({
  verdict,
  live,
  follow,
  archived,
}: {
  verdict: Naming;
  live: boolean;
  follow?: FollowPhase;
  archived: boolean;
}): JSX.Element {
  return (
    <div
      className="shrink-0 border-b border-border px-4 py-2.5"
      data-testid="transcript-verdict"
      data-term={verdict.term}
      data-tone={verdict.tone}
    >
      <div className="flex flex-wrap items-baseline gap-2">
        <span className={`size-[7px] shrink-0 rounded-full ${DOT[verdict.tone]}`} />
        <strong className={`text-[13px] font-semibold ${TONE[verdict.tone]}`}>{verdict.label}</strong>
        {live && (
          // WHICH OF THE TWO LIVE RUNS THIS IS. Following and not-following are the same picture and
          // opposite facts, so the word is read from the reader itself rather than assumed. It is a
          // label, not an indicator: it does not pulse, and it does not change on a poll that found
          // nothing — because nothing happened.
          <span
            className="rounded border border-border px-1.5 py-px font-mono text-[9.5px] uppercase text-muted-foreground"
            data-testid="transcript-live"
            data-follow={follow ?? 'none'}
            title={FOLLOW_TITLE[follow ?? 'none']}
          >
            {FOLLOW_WORD[follow ?? 'none']}
          </span>
        )}
        {archived && (
          <span
            className="rounded border border-border px-1.5 py-px font-mono text-[9.5px] uppercase text-muted-foreground"
            data-testid="transcript-archived"
            title="Temporal has dropped this execution; the account was read from the archive (ADR 0025)"
          >
            from the archive
          </span>
        )}
      </div>
      <p className="m-0 mt-1 font-mono text-[11px] text-muted-foreground">{verdict.because}</p>
    </div>
  );
}

/**
 * The run is waiting for YOU — said once, at the top, before the account it is buried in.
 *
 * A THIRD THING, DISTINCT FROM RUNNING AND FROM STALLED, and that is the whole reason it is a strip
 * of its own rather than a row further down. A run grinding through Batches, a run blocked on a
 * queue nobody polls, and a run parked on a question all read `running` to Temporal and all look
 * identical in a status pill. Only one of the three is waiting on the person reading this, and it
 * is the only one they can do something about in the next ten seconds.
 *
 * SKY AND NOT ROSE, AND NOTHING MOVES. A park is the workflow working as designed — the author put
 * an approval gate there on purpose — so this is a fact stated calmly and not an alarm. It can
 * legitimately be on screen for hours, and an alarm that is correct for hours has trained somebody
 * to ignore an alarm.
 *
 * IT LISTS THE QUESTIONS RATHER THAN COUNTING THEM. Two pending asks is two decisions with two
 * clocks, and "2 questions" at the top of a panel would make the operator go and find out which.
 */
function Waiting({ waiting }: { waiting: readonly AskReading[] }): JSX.Element {
  return (
    <div
      className="shrink-0 border-b border-sky-500/30 bg-sky-500/5 px-4 py-2"
      data-testid="transcript-waiting"
      data-pending={waiting.length}
    >
      <strong className="text-[12px] font-semibold text-sky-600 dark:text-sky-400">
        {waiting.length === 1
          ? 'This run is waiting for you to answer a question.'
          : `This run is waiting for you to answer ${waiting.length} questions.`}
      </strong>
      <span className="ml-2 font-mono text-[10.5px] text-muted-foreground">
        nothing else in it moves until then — each is answered on its own, below
      </span>
      <ul className="m-0 mt-1 list-none p-0">
        {waiting.map((r) => (
          <li key={r.ask.id} className="py-px text-[11.5px]" data-testid={`waiting-${r.ask.id}`}>
            <span className="font-mono text-[10px] text-muted-foreground">{r.ask.id}</span>
            <span className="ml-2">{r.ask.prompt}</span>
            <span className="ml-2 font-mono text-[10px] tabular-nums text-muted-foreground">
              {waitedWords(r)} · {deadlineWords(r)}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}

/**
 * What no failure event will tell you.
 *
 * Every concern is printed with the metadata it was read from, for the same reason every name is.
 * They are not deduplicated across turns: three dispatches onto three unpolled queues is three
 * problems, and rolling them into one line would hide two of them.
 */
function Concerns({ concerns }: { concerns: readonly Concern[] }): JSX.Element {
  return (
    <ul
      className="m-0 max-h-40 shrink-0 list-none overflow-y-auto border-b border-rose-500/30 bg-rose-500/5 px-4 py-2"
      data-testid="transcript-concerns"
    >
      {concerns.map((c, i) => (
        <li key={`${c.term}:${c.t}:${c.events[0] ?? i}`} className="py-px text-[12px]" data-concern={c.term}>
          <span className="font-semibold text-rose-500">{c.label}</span>
          <span className="ml-2 font-mono text-[10.5px] text-muted-foreground">{c.because}</span>
          {c.events.length > 0 && (
            <span className="ml-2 font-mono text-[10px] text-muted-foreground/70">
              event{c.events.length === 1 ? '' : 's'} {c.events.slice(0, 6).join(', ')}
              {c.events.length > 6 ? '…' : ''}
            </span>
          )}
        </li>
      ))}
    </ul>
  );
}

/**
 * The chips, in domain order.
 *
 * READ-ONLY IN THIS SLICE, and that is a deliberate half. `filtersOf` already answers what a chip
 * is and what is behind it — a term this run actually produced, so the bar never offers a filter
 * that finds nothing — and printing the counts is most of the value: "12 methods failed" is the
 * number an operator is looking for. Making them selectable is a state machine, and a state machine
 * is a slice.
 *
 * IT COUNTS THE TURNS THIS PANE DOES NOT DRAW, AND THAT IS HOW "NOTHING IS DROPPED" STAYS CHECKABLE.
 * The rows below are the domain account; the events kontra has no word for are drawn verbatim on the
 * Event log. A bar that counted only what is on screen would let an operator believe the account IS
 * the log, which is the belief the two-pane split has to avoid creating. So the other pane's terms
 * are here too, marked `data-pane="events"` and saying where they are — a count with an address
 * rather than a row that competes with the run's own story.
 */
function Chips({
  turns,
  elsewhere,
}: {
  turns: readonly NamedTurn[];
  elsewhere: readonly NamedTurn[];
}): JSX.Element | null {
  // The drawn half is built here rather than read off `named.filters` for one reason: a chip has to
  // say which pane its rows are in, and `filtersOf` answers about the run rather than about this
  // panel. The other half IS `filtersOf`, over the other list — because a chip for a row nobody can
  // see has to be named from its TERM ("untranslated") and not from the first member's Temporal
  // type, which is what `Naming.label` carries and is a fact about one row rather than a count.
  const drawn = tally(turns);
  const other = filtersOf(elsewhere);
  if (drawn.size === 0 && other.length === 0) return null;
  return (
    <div className="flex shrink-0 flex-wrap gap-1 border-b border-border px-4 py-1.5" data-testid="transcript-chips">
      {[...drawn.entries()].map(([term, c]) => (
        <span
          key={term}
          data-chip={term}
          data-pane="transcript"
          className="rounded border border-border px-1.5 py-px font-mono text-[9.5px] text-muted-foreground"
        >
          {c.label}
          <span className="ml-1 tabular-nums text-foreground">{c.count}</span>
        </span>
      ))}
      {other.map((f) => (
        <span
          key={f.term}
          data-chip={f.term}
          data-pane="events"
          title="this release has no word for these — they are on the Event log, with Temporal's own type on every row"
          className="rounded border border-dashed border-border px-1.5 py-px font-mono text-[9.5px] text-muted-foreground/70"
        >
          {f.label}
          <span className="ml-1 tabular-nums text-muted-foreground">{f.count}</span>
          <span className="ml-1">· in the Event log</span>
        </span>
      ))}
    </div>
  );
}

/** Turns per term, counting a folded loop as its members — "12 methods failed" is the number an
 *  operator is looking for and "1 row" is not. */
function tally(turns: readonly NamedTurn[]): Map<string, { label: string; count: number }> {
  const counts = new Map<string, { label: string; count: number }>();
  for (const t of turns) {
    const have = counts.get(t.term);
    if (have) have.count += Math.max(1, t.turn.count);
    else counts.set(t.term, { label: t.label, count: Math.max(1, t.turn.count) });
  }
  return counts;
}

/* ───────────────────────────── one turn ───────────────────────────── */

function TurnRow({
  named,
  turnKey: key,
  onDrill,
  open,
}: {
  named: NamedTurn;
  turnKey: string;
  onDrill?: (named: NamedTurn) => void;
  open: boolean;
}): JSX.Element {
  const { turn } = named;
  const members = turn.count > 1 ? expand(turn) : [];

  const drill = onDrill ? (
    // THE CHECK, ON EVERY ROW INCLUDING THE ONES WITH NO WORD FOR THEMSELVES. It prints the
    // count of Temporal events this name was read from, which is metadata worth having anyway —
    // so the affordance costs no ink it was not already spending. `preventDefault` because a row
    // that folds a loop is a `<summary>`, and opening the raw view must not also expand two
    // hundred members.
    <button
      type="button"
      data-testid={`turn-drill-${key}`}
      title="the raw Temporal events this turn folded"
      onClick={(e) => {
        e.preventDefault();
        e.stopPropagation();
        onDrill(named);
      }}
      className="shrink-0 rounded px-1 font-mono text-[9.5px] tabular-nums text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
    >
      {turn.events.length === 0 ? 'no event' : `${turn.events.length}ev`}
    </button>
  ) : null;

  /**
   * A SENTENCE IS A LOG LINE, NOT A TURN OF THE SHAPE BELOW.
   *
   * Every other row here answers "what KIND of thing happened, and how far into the run" — that is
   * what a derived turn has to say, and the kind label and the `+0.4s` are the answer. A narration
   * is the one row that is not derived from anything: an author called `speak` and wrote a line for
   * a person to read. Drawn in the common shape it came out as `+0.4s  note  <sentence>`, with two
   * pieces of machinery standing in front of the only thing anybody wanted — and the `note` label
   * says nothing the sentence does not.
   *
   * So it reads `<wall clock> — <sentence>`, in the reader's own timezone: an operator wants to know
   * WHEN something was said in the same terms as the rest of their day. The offset is not thrown
   * away, it is demoted to the row's `title`; and the drill stays, because a name nobody can check
   * is a name nobody should trust (ADR 0027) and that rule has no exceptions.
   */
  if (turn.kind === 'narration') {
    return (
      <div
        className={`flex items-start gap-2 border-b border-border/40 py-1 ${open ? 'bg-accent/40' : ''}`}
        data-testid="turn"
        data-term={named.term}
        data-tone={named.tone}
        data-kind={turn.kind}
        data-drilled={open ? 'true' : undefined}
      >
        <span className={`mt-[5px] size-[6px] shrink-0 rounded-full ${DOT[named.tone]}`} />
        <span
          className="min-w-[4rem] shrink-0 whitespace-nowrap text-right font-mono text-[10px] tabular-nums text-muted-foreground"
          data-testid="turn-clock"
          title={`${offset(turn.t)} into the run`}
        >
          {clockOf(turn.at)}
        </span>
        <span className="min-w-0 flex-1 text-[12px]" data-testid="turn-said">
          <span className="mr-1.5 text-muted-foreground">—</span>
          <span className={TONE[named.tone]}>{named.because}</span>
        </span>
        {drill}
      </div>
    );
  }

  const body = (
    <>
      <span className={`mt-[5px] size-[6px] shrink-0 rounded-full ${DOT[named.tone]}`} />
      <span className="w-16 shrink-0 text-right font-mono text-[10px] tabular-nums text-muted-foreground">
        {offset(turn.t)}
      </span>
      <span className="min-w-0 flex-1">
        <span className={`text-[12px] ${named.untranslated ? 'font-mono text-muted-foreground' : TONE[named.tone]}`}>
          {named.label}
        </span>
        {turn.count > 1 && (
          <span
            className="ml-1.5 rounded bg-muted px-1 py-px font-mono text-[9.5px] tabular-nums"
            data-testid="turn-count"
          >
            ×{turn.count}
          </span>
        )}
        {turn.attempt > 1 && (
          <span className="ml-1.5 font-mono text-[9.5px] text-amber-500" title="Temporal's attempt counter — something retried">
            attempt {turn.attempt}
          </span>
        )}
        <span className="ml-2 font-mono text-[10.5px] text-muted-foreground">{named.because}</span>
        {turn.error && (
          // KEPT APART FROM `because`. The opener's facts survive the failure — which endpoint,
          // which queue — and the message is the thing that says what broke.
          <span className="ml-2 font-mono text-[10.5px] text-rose-500" data-testid="turn-error">
            {turn.error}
          </span>
        )}
      </span>
      <span className="shrink-0 font-mono text-[10px] tabular-nums text-muted-foreground">{secs(turn.dur)}</span>
      {drill}
    </>
  );

  if (members.length === 0) {
    return (
      <div
        className={`flex items-start gap-2 border-b border-border/40 py-1 ${open ? 'bg-accent/40' : ''}`}
        data-testid="turn"
        data-term={named.term}
        data-tone={named.tone}
        data-kind={turn.kind}
        data-drilled={open ? 'true' : undefined}
      >
        {body}
      </div>
    );
  }

  return (
    <details
      className={`border-b border-border/40 ${open ? 'bg-accent/40' : ''}`}
      data-testid="turn"
      data-term={named.term}
      data-tone={named.tone}
      data-kind={turn.kind}
      data-folded={turn.count}
      data-drilled={open ? 'true' : undefined}
    >
      <summary className="flex cursor-pointer list-none items-start gap-2 py-1 hover:bg-accent/40">{body}</summary>
      <div className="border-l border-border/60 pl-4" data-testid="turn-members">
        {members.map((m: Turn, i: number) => {
          const one = nameTurn(m);
          return (
            <div
              key={`${m.events[0] ?? i}`}
              className="flex items-start gap-2 py-px"
              data-testid="turn-member"
              data-term={one.term}
              data-tone={one.tone}
            >
              <span className="w-16 shrink-0 text-right font-mono text-[10px] tabular-nums text-muted-foreground">
                {offset(m.t)}
              </span>
              <span className={`min-w-0 flex-1 text-[11.5px] ${TONE[one.tone]}`}>{one.label}</span>
              <span className="shrink-0 font-mono text-[9.5px] text-muted-foreground">
                {/* THE DRILL PATH. A domain turn is never a dead end: these are the rows in the
                    event log it was built from, so the friendly view never costs the real one. */}
                {m.events.join(', ')}
              </span>
            </div>
          );
        })}
      </div>
    </details>
  );
}
