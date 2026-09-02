/**
 * A question the run is waiting on, drawn as a turn in its own transcript.
 *
 * IT IS A TURN AND NOT A MODAL, which is the whole shape of this slice. A run's account is a
 * conversation, and the moment it needs a human is a moment IN that conversation — in order, in
 * place, with the dispatches that produced the question above it and whatever happened next below.
 * A dialog over the top would have made the decision and the material it rests on two screens, and
 * the material is the only thing that makes it a decision rather than a guess.
 *
 * SEVERAL AT ONCE IS THE NORMAL CASE. Parallel branches each needing a decision is ordinary in these
 * workflows, so every ask is its own turn with its own form and its own button; answering one
 * changes nothing about the others. That is why the draft is keyed by ask id and why the answer
 * control is inside the row rather than at the foot of the panel.
 *
 * THE FORM IS THE SHARED ONE. `FieldGroup` — the same component that starts a run and calls a
 * Method, over `schemaTree`'s reading and `formFields`' coercion — so an ask whose `takes` is a
 * nested pydantic model nests here, an enum is a `<select>` here, and none of it is code this file
 * owns. What is local is the container, exactly as `MethodCallPanes` and `WorkflowInputForm` differ
 * from each other and share everything below.
 *
 * NOTHING HERE BLINKS. A pending ask is important, not urgent: a hairline down the left, one dot,
 * one line of clock. No pulse, no red, no animation — a run waiting on a human is a normal thing
 * for a durable workflow to be doing, and a surface that shouted about it would be shouting for as
 * long as the wait, which can legitimately be forever.
 *
 * PROPS IN, MARKUP OUT — no state, no fetch, nothing that reaches xterm or CodeMirror, so
 * `askTurn.render.test.ts` draws every state of it in node with no jsdom. The draft lives one level
 * up for the same reason it does on the Workflows page: a form whose values are component state
 * cannot be asserted in any state but its first.
 */

import { FieldGroup } from './FieldGroup';
import { setInputJson, setValues, type InputDraft } from './workflowInput';
import {
  answerOf,
  askForm,
  attributionWords,
  deadlineWords,
  shapeWords,
  standingWords,
  waitedWords,
  ATTRIBUTION_HINT,
  ATTRIBUTION_NOTE,
  type AskReading,
  type AskStanding,
} from './ask';

/**
 * How each standing is painted.
 *
 * `waiting` IS SKY AND NOT AMBER, matching the `busy` tone `vocabulary.ts` already gives
 * `run-parked`: a parked run is working as designed, and colouring it like a warning would teach an
 * operator that every approval gate is a fault. `expired` IS amber, because that one is a deadline
 * the author set going by with nobody there.
 */
const DOT: Record<AskStanding, string> = {
  waiting: 'bg-sky-400',
  answered: 'bg-emerald-400',
  expired: 'bg-amber-400',
  abandoned: 'bg-zinc-400',
};

const WORD: Record<AskStanding, string> = {
  waiting: 'text-sky-500',
  answered: 'text-emerald-500',
  expired: 'text-amber-500',
  abandoned: 'text-muted-foreground',
};

/**
 * Everything the transcript needs to let an operator answer — one prop rather than seven.
 *
 * ABSENT MEANS READ-ONLY, and that is a real state rather than a degraded one: an archived run's
 * asks are history, and a surface with nowhere to send a signal must not draw a button that does
 * nothing. The transcript renders the questions and the answers either way.
 */
export interface AskDeck {
  /**
   * The drafts under construction, by ask id. An id that is absent has not been touched — the row
   * seeds itself from the ask's own schema, so a declared default is on screen before the operator
   * types anything (`formFields.ts`: "a prefilled field is still a field").
   */
  drafts: Readonly<Record<string, InputDraft>>;
  onDraft(askId: string, draft: InputDraft): void;
  /** The operator label, one per session and not one per ask — it is the person at the keyboard. */
  by: string;
  onBy(next: string): void;
  onAnswer(askId: string): void;
  /** The ask whose answer is in flight. Its button refuses a second click rather than sending a
   *  second signal, which is durable and unacknowledged (`hitl.ts`). */
  sending: string | null;
  /** The appliance's own refusal, by ask id — beside the form it is about, never in a page-level
   *  notice, because a run with three asks can have one refused and two fine. */
  refused: Readonly<Record<string, string>>;
}

export function AskTurn({
  reading,
  deck,
  testId,
}: {
  reading: AskReading;
  deck?: AskDeck;
  /** What every `data-testid` in this row starts with. The ask's id is in it, which is what keeps
   *  three asks on one run addressable apart. */
  testId: string;
}): JSX.Element {
  const { ask, standing, unreadable } = reading;
  const words = standingWords(reading);
  // A QUESTION ON A RUN THAT HAS CLOSED IS NOT PAINTED LIKE A LIVE ONE. It reads `waiting` because
  // that is what the run's memo still says, and nothing is waiting on anybody.
  const paint: AskStanding = reading.stranded ? 'abandoned' : standing;

  return (
    <section
      // A HAIRLINE DOWN THE LEFT, the same device `FieldGroup`'s `INSIDE` uses to say "this is a
      // thing inside the thing above it" — two pixels rather than a panel, a tint or a badge.
      className="border-b border-l-2 border-border/40 py-2 pl-2"
      data-testid={testId}
      data-ask={ask.id}
      data-standing={standing}
      data-answerable={reading.answerable ? 'true' : undefined}
      data-unreadable={unreadable ? 'true' : undefined}
      data-overdue={reading.overdue ? 'true' : undefined}
      data-stranded={reading.stranded ? 'true' : undefined}
    >
      <header className="flex flex-wrap items-baseline gap-2">
        <span className={`size-[6px] shrink-0 rounded-full ${DOT[paint]}`} />
        <strong className={`text-[12px] font-semibold ${WORD[paint]}`} data-testid={`${testId}-standing`}>
          {words.label}
        </strong>
        <span
          className="font-mono text-[10px] tabular-nums text-muted-foreground"
          data-testid={`${testId}-clock`}
        >
          {waitedWords(reading)}
        </span>
        <span className="font-mono text-[10px] text-muted-foreground">·</span>
        <span
          className="font-mono text-[10px] text-muted-foreground"
          data-testid={`${testId}-deadline`}
          title={
            reading.deadline.kind === 'none'
              ? 'the author passed no deadline (hitl.ask(deadline=None)). A durable workflow genuinely can wait forever, and an approval gate on something that must not proceed unattended is the case for it.'
              : 'the deadline the author declared when this run parked'
          }
        >
          {deadlineWords(reading)}
        </span>
        <span className="ml-auto font-mono text-[9.5px] text-muted-foreground" title="the ask's own id — it is in the signal that answers it">
          {ask.id}
        </span>
      </header>

      <p className="m-0 mt-1 text-[12.5px]" data-testid={`${testId}-prompt`}>
        {ask.prompt}
      </p>
      <p className="m-0 mt-0.5 font-mono text-[10.5px] text-muted-foreground">{words.because}</p>

      {unreadable && (
        // NEVER A BLANK ROW, AND NEVER A THROWN RENDER. The run is parked whether or not this entry
        // can be read, and a surface that dropped it would leave an operator with a stuck run and
        // nothing to click — the one outcome that makes this unrecoverable from the UI.
        <p
          className="m-0 mt-1 rounded border border-amber-500/40 bg-amber-500/10 p-1.5 text-[11px] text-amber-700 dark:text-amber-300"
          data-testid={`${testId}-unreadable`}
        >
          This entry could not be read as an ask — its question or the instant it was asked is
          missing. It is kept and marked rather than dropped, because the run is parked either way.
          What it is waiting for is in the workflow’s source and in the raw events behind this turn.
        </p>
      )}

      <AskContext context={ask.context} testId={testId} />

      {standing === 'answered' ? (
        <Attribution ask={ask} testId={testId} />
      ) : reading.answerable && deck ? (
        <AnswerForm reading={reading} deck={deck} testId={testId} />
      ) : reading.answerable ? (
        <p className="m-0 mt-1.5 text-[11px] text-muted-foreground" data-testid={`${testId}-readonly`}>
          This run is still parked on this question. Answering it is not offered here — open the run
          on the Workflows surface, where a form can reach the appliance.
        </p>
      ) : null}
    </section>
  );
}

/* ───────────────────────────── the material to decide on ───────────────────────────── */

/**
 * What the workflow attached — the Dataset, the counts, the sample.
 *
 * BESIDE THE QUESTION, ALWAYS, because that is what makes this a decision rather than a guess. An
 * operator who has to open another tab to see which dataset "approve these 12 hosts" is about is an
 * operator approving a sentence.
 *
 * AN ABSENT CONTEXT IS SAID OUT LOUD. A question with nothing under it looks identical to a
 * renderer that failed to draw the context, and only one of those is the author's choice.
 *
 * IT IS NEVER TRUSTED AS MARKUP. Every value goes through React as text — an ask's context is
 * assembled by a workflow from whatever it just found on the internet, which is the least trusted
 * material on this surface.
 */
function AskContext({ context, testId }: { context: unknown; testId: string }): JSX.Element {
  if (context === undefined || context === null) {
    return (
      <p className="m-0 mt-1 text-[10.5px] italic text-muted-foreground" data-testid={`${testId}-nocontext`}>
        no context — the author attached nothing to this question. What it rests on is whatever is
        in the transcript above it.
      </p>
    );
  }

  const rows =
    typeof context === 'object' && !Array.isArray(context)
      ? Object.entries(context as Record<string, unknown>)
      : null;

  return (
    <div
      className="mt-1.5 max-h-48 overflow-auto rounded border border-border bg-muted/30 p-1.5"
      data-testid={`${testId}-context`}
    >
      {rows === null || rows.length === 0 ? (
        <pre className="m-0 whitespace-pre-wrap break-words font-mono text-[10.5px]">{asJson(context)}</pre>
      ) : (
        <dl className="m-0 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-0.5">
          {rows.map(([key, value]) => (
            <div key={key} className="contents" data-testid={`${testId}-context-${key}`}>
              <dt className="font-mono text-[10.5px] text-muted-foreground">{key}</dt>
              <dd className="m-0 min-w-0 break-words font-mono text-[10.5px]">{asText(value)}</dd>
            </div>
          ))}
        </dl>
      )}
    </div>
  );
}

/** A scalar as itself, anything else as the JSON it is. Never `[object Object]`. */
function asText(value: unknown): string {
  if (value === null) return 'null';
  if (typeof value === 'object') return asJson(value);
  return String(value);
}

function asJson(value: unknown): string {
  try {
    return JSON.stringify(value, null, 2) ?? String(value);
  } catch {
    // A context holding a cycle is still a context. It reached here as JSON off the wire so this
    // should not happen, and saying so beats an exception inside a transcript.
    return '(this context could not be rendered as JSON)';
  }
}

/* ───────────────────────────── the answer, as a turn ───────────────────────────── */

/**
 * The answer, permanently, carrying the label whoever answered typed about themselves.
 *
 * WORDED AS ATTRIBUTION AND NEVER AS VERIFICATION — the note is on the row rather than in a tooltip
 * about the architecture, because the moment somebody cites this name is exactly the moment the
 * distinction matters. See `ask.ts`, `backend/src/hitl.ts` and `sdk/python/actorkit/hitl.py`,
 * which all say the same thing and all have to.
 *
 * THE VALUE IS NOT HERE, and that is the run's decision rather than an omission on this side.
 * `_close` in `hitl.py` deliberately does not write the answer back into the memo — it is already
 * in history where the operator put it, and a second longer-lived copy of a human's judgement in a
 * place nobody chose is worse than a drill. The raw signal event is one click away on this turn.
 */
function Attribution({ ask, testId }: { ask: { by?: string; answeredAt?: number }; testId: string }): JSX.Element {
  return (
    <div className="mt-1.5" data-testid={`${testId}-answer`} data-by={ask.by ?? ''}>
      <p className="m-0 text-[11.5px]">
        <span className="text-emerald-600 dark:text-emerald-400">{attributionWords(ask)}</span>
        {ask.answeredAt !== undefined && ask.answeredAt > 0 && (
          <span className="ml-2 font-mono text-[10px] text-muted-foreground">
            {new Date(ask.answeredAt).toLocaleString()}
          </span>
        )}
      </p>
      <p
        className="m-0 mt-0.5 text-[10px] italic text-muted-foreground"
        data-testid={`${testId}-attribution-note`}
      >
        {ATTRIBUTION_NOTE}
      </p>
      <p className="m-0 mt-0.5 text-[10px] text-muted-foreground">
        The answer itself is not kept on the ask — it arrived as a signal and lives in this run’s
        events, which is the one copy of it anybody chose to make.
      </p>
    </div>
  );
}

/* ───────────────────────────── answering one ───────────────────────────── */

function AnswerForm({
  reading,
  deck,
  testId,
}: {
  reading: AskReading;
  deck: AskDeck;
  testId: string;
}): JSX.Element {
  const { ask } = reading;
  const form = askForm(ask);
  const draft = deck.drafts[ask.id] ?? form.draft;
  const result = answerOf(draft);
  const blocked = 'error' in result ? result.error : null;
  const sending = deck.sending === ask.id;
  const refused = deck.refused[ask.id];
  const dead = form.shape === 'unreadable';

  return (
    <div className="mt-2" data-testid={`${testId}-form`} data-shape={form.shape}>
      {form.shape !== 'fields' && (
        <p className="m-0 mb-1 text-[10.5px] text-muted-foreground" data-testid={`${testId}-why`}>
          {shapeWords(form.shape)}
        </p>
      )}

      {dead ? null : draft.kind === 'fields' ? (
        // THE SHARED DRAWING, ALL THE WAY DOWN. A nested object is a nested group and an array is a
        // repeatable row here exactly as they are on the Workflows and Actors pages, because it is
        // the same component — the container is what differs between the three, never the cell.
        <FieldGroup
          nodes={draft.fields}
          values={draft.values}
          onValues={(next) => deck.onDraft(ask.id, setValues(draft, next))}
          testPrefix={`${testId}-field`}
        />
      ) : (
        <textarea
          className="min-h-[60px] w-full resize-y rounded border border-border bg-background p-1.5 font-mono text-[11.5px]"
          data-testid={`${testId}-json`}
          spellCheck={false}
          value={draft.text}
          onChange={(e) => deck.onDraft(ask.id, setInputJson(draft, e.target.value))}
        />
      )}

      {!dead && (
        <div className="mt-1.5 flex flex-wrap items-center gap-2">
          <label className="flex items-baseline gap-1.5 text-[10.5px] text-muted-foreground">
            answering as
            <input
              className="w-40 rounded border border-border bg-background px-1.5 py-0.5 font-mono text-[11px]"
              data-testid={`${testId}-by`}
              placeholder="unlabelled"
              spellCheck={false}
              value={deck.by}
              onChange={(e) => deck.onBy(e.target.value)}
              title={ATTRIBUTION_NOTE}
            />
          </label>
          <button
            type="button"
            data-testid={`${testId}-submit`}
            disabled={sending || blocked !== null}
            onClick={() => deck.onAnswer(ask.id)}
            title="signals this answer to the run. A signal is durable and cannot be taken back, so the appliance checks it against the ask's own schema first."
            className="rounded border border-border px-2 py-0.5 text-[11px] outline-none hover:bg-accent disabled:opacity-50 focus-visible:ring-2 focus-visible:ring-ring/50"
          >
            {sending ? 'answering…' : 'answer'}
          </button>
          <span className="text-[10px] italic text-muted-foreground" data-testid={`${testId}-by-hint`}>
            {ATTRIBUTION_HINT}
          </span>
        </div>
      )}

      {blocked && (
        <p
          className="m-0 mt-1.5 rounded border border-amber-500/40 bg-amber-500/10 p-1.5 font-mono text-[11px] text-amber-700 dark:text-amber-300"
          data-testid={`${testId}-error`}
        >
          {blocked}
        </p>
      )}

      {refused && (
        // THE APPLIANCE'S OWN SENTENCE, beside the form it is about. The run is exactly as parked as
        // it was: validation happens before the signal precisely so a refused answer costs nothing.
        <p
          className="m-0 mt-1.5 rounded border border-rose-500/40 bg-rose-500/10 p-1.5 font-mono text-[11px] text-rose-600 dark:text-rose-400"
          data-testid={`${testId}-refused`}
        >
          {refused}
        </p>
      )}
    </div>
  );
}
