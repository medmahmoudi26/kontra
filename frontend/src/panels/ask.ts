/**
 * One ask, as the operator's side reads it: how it stands, what its clock says, and the draft it is
 * answered with. No DOM, no React, no fetch.
 *
 * THE FORM IS NOT BUILT HERE AND THAT IS THE POINT. An ask's `takes` comes off the SAME pydantic
 * derivation an Actor's Method does — `_schema_of` in `sdk/python/actorkit/hitl.py` — so it is read
 * by `schemaTree`, drafted by `draftForInput` and coerced by `coerceValues`, which is the exact
 * path that starts a run and calls a Method. Three uses, one renderer. A bespoke form here would
 * have been a fourth reading of JSON Schema in a codebase that went to some trouble to have one,
 * and it would have been the reading that quietly did not understand a Python `$ref`.
 *
 * WHAT THIS MODULE DOES OWN IS THE CLOCK AND THE ENDING, because those are the two things an ask
 * has that a run's input does not:
 *
 *   THE ENDING IS THE RUN'S, NEVER INFERRED. `state` is one of four words the WORKFLOW wrote into
 *   its own memo (`hitl.ts`: "declared by the RUN, not inferred here"), and the four are four
 *   because a deadline nobody met and a run somebody cancelled are different endings — an operator
 *   reading `abandoned` must not be told a human failed to answer in time. So nothing below derives
 *   a standing from `answeredAt` being absent; it reads what the run said and prints that.
 *
 *   THE CLOCK IS THE BROWSER'S, AND DELIBERATELY NOT THE ROUTE'S. `waitedMs` and `remainingMs`
 *   arrive on the wire already computed, and they are a SNAPSHOT: the transcript only re-renders
 *   when the run's log actually changes (`follow.ts` coalesces on a fingerprint), so a parked run
 *   sitting still would show a countdown frozen at whatever it read last — the one number on this
 *   surface whose whole meaning is that it is moving. `askedAt` and `deadlineAt` are absolute
 *   instants the RUN chose, so measuring against a ticking `now` here is both honest and alive.
 *   An ANSWERED ask measures to `answeredAt` instead and stops, which is what it should do.
 *
 * AND NO DEADLINE IS AN ANSWER. `deadline=None` is a legitimate authored choice — an approval gate
 * on a run that must not proceed unattended is the case for it — so {@link readAsk} carries
 * `{kind: 'none'}` and the surface says "waits indefinitely". A blank where a countdown goes, or a
 * countdown stopped at zero, would both read as a value that failed to arrive.
 *
 * AN UNREADABLE ENTRY IS STILL AN ASK. The route already refuses to drop one (`hitl.ts`: "A LEGIBLE
 * ROW, NEVER A BLANK ONE") and this refuses to throw on one: the run IS parked either way, and a
 * reading that threw would take the asks beside it — which are perfectly readable — down with it.
 */

import type { JsonSchema } from '../types';
import type { RunRow } from '../run/api';
import { fmtDuration } from '../run/api';
import type { RunAsk } from '../run/turns';
import { draftForInput, inputOf, type InputDraft, type InputResult } from './workflowInput';

/* ───────────────────────────── how one ask stands ───────────────────────────── */

/**
 * The four endings a run declares, in the surface's own words.
 *
 * `waiting` IS `pending` RENAMED AND NOTHING ELSE. The wire word is the workflow's; this one is
 * what a row says, and "waiting" is the half of it that is about the person reading.
 */
export type AskStanding = 'waiting' | 'answered' | 'expired' | 'abandoned';

/** What the deadline the author declared currently says. */
export type AskDeadline =
  /** The author passed none. This run waits indefinitely — a choice, not a missing value. */
  | { kind: 'none' }
  /** Milliseconds left before the deadline. */
  | { kind: 'left'; ms: number }
  /** Milliseconds since it went by. */
  | { kind: 'passed'; ms: number };

/** One ask, read for drawing. */
export interface AskReading {
  ask: RunAsk;
  standing: AskStanding;
  /**
   * This entry could not be read as an ask — the route said so and kept the row anyway.
   *
   * IT IS NOT A STANDING. A malformed entry still has whatever `state` the memo carried (the route
   * defaults an unreadable one to `pending`), so the run may genuinely still be parked on it. The
   * two facts are printed side by side rather than one replacing the other.
   */
  unreadable: boolean;
  /**
   * Milliseconds the run has been parked on this ask — to the answer, or to `now`.
   *
   * `null` WHERE THERE IS NOTHING TO MEASURE FROM. An entry with no readable `askedAt` has no
   * elapsed time, and printing `0s` or `56y` for it would be inventing one.
   */
  waitedMs: number | null;
  deadline: AskDeadline;
  /**
   * Whether this ask is still offered as answerable: the run says `pending` AND the run is open.
   *
   * A CLOSED RUN IS NEVER PARKED, whatever its asks say — the rule `runWord` states one level up,
   * and it bites hardest here. A run that FAILED never gets to rewrite its memo (`_close` runs on
   * expiry, on cancellation and on an answer, and on none of those), so its question sits at
   * `pending` in the archive forever. Offering a form for it would be a button that signals a
   * workflow which is not there to receive it.
   */
  answerable: boolean;
  /**
   * The run closed with this question still open.
   *
   * ITS OWN FACT, not `expired` and not `abandoned`. Nobody failed to answer in time and nothing
   * cancelled it: the run ended some other way and took the question with it, which is what a
   * failed run's asks look like in the archive from then on.
   */
  stranded: boolean;
  /**
   * The deadline went by and the run has not yet recorded an ending.
   *
   * TWO AUTHORITIES, AND THEY CAN DISAGREE FOR A MOMENT. The deadline passing is arithmetic this
   * browser can do; the ask becoming `expired` is the workflow's timer firing and rewriting its own
   * memo, which needs a worker. A parked run whose worker is down is the case where the gap is not
   * a moment, and it is exactly the case this whole surface exists to make visible — so it is said
   * out loud rather than papered over by calling the ask expired on the browser's authority.
   */
  overdue: boolean;
}

/**
 * One ask, against a clock.
 *
 * `live` IS THE RUN'S, not the ask's, and it is what separates a question waiting for you from one
 * the run took to the grave. It defaults to open because that is the reading with a form under it,
 * and a caller that does not know is better off offering the answer than hiding it.
 */
export function readAsk(ask: RunAsk, now: number, live = true): AskReading {
  const standing = standingOf(ask);
  const unreadable = ask.malformed === true;
  const askedAt = Number.isFinite(ask.askedAt) && ask.askedAt > 0 ? ask.askedAt : 0;
  const until = ask.answeredAt !== undefined && ask.answeredAt > 0 ? ask.answeredAt : now;
  const stranded = standing === 'waiting' && !live;
  return {
    ask,
    standing,
    unreadable,
    waitedMs: askedAt === 0 ? null : Math.max(0, until - askedAt),
    deadline: deadlineOf(ask, now),
    answerable: standing === 'waiting' && live,
    stranded,
    overdue:
      standing === 'waiting' &&
      live &&
      ask.deadlineAt !== undefined &&
      ask.deadlineAt > 0 &&
      now >= ask.deadlineAt,
  };
}

/** Every ask a run published, read against one clock, in the order they were asked. */
export function readAsks(asks: readonly RunAsk[], now: number, live = true): AskReading[] {
  return asks.map((a) => readAsk(a, now, live));
}

/** The ones still waiting on a human. What "parked" means, and the only ones with a form under them. */
export function waitingAsks(readings: readonly AskReading[]): AskReading[] {
  return readings.filter((r) => r.answerable);
}

function standingOf(ask: RunAsk): AskStanding {
  switch (ask.state) {
    case 'answered':
      return 'answered';
    case 'expired':
      return 'expired';
    case 'abandoned':
      return 'abandoned';
    default:
      return 'waiting';
  }
}

function deadlineOf(ask: RunAsk, now: number): AskDeadline {
  // ABSENT IS AN ANSWER, and the only place this module tests for absence rather than for zero:
  // `deadlineAt: 0` would be an instant in 1970, which no author declared.
  if (ask.deadlineAt === undefined || !Number.isFinite(ask.deadlineAt) || ask.deadlineAt <= 0) {
    return { kind: 'none' };
  }
  // A SETTLED ASK'S DEADLINE IS MEASURED AGAINST ITS ENDING, not against the wall clock. An ask
  // answered with an hour to spare should still read that way a week later.
  const at = ask.answeredAt !== undefined && ask.answeredAt > 0 ? ask.answeredAt : now;
  const left = ask.deadlineAt - at;
  return left > 0 ? { kind: 'left', ms: left } : { kind: 'passed', ms: -left };
}

/* ───────────────────────────── the words ───────────────────────────── */

/** How long the run has been on this question. The answer to "am I the bottleneck?". */
export function waitedWords(reading: AskReading): string {
  if (reading.waitedMs === null) return 'how long it has waited is not readable from this entry';
  const spent = reading.waitedMs < 1000 ? 'less than a second' : fmtDuration(reading.waitedMs);
  return reading.answerable ? `parked ${spent}` : `waited ${spent}`;
}

/**
 * What the deadline says, in one clause.
 *
 * NO DEADLINE GETS A SENTENCE AND NOT A DASH. "waits indefinitely" is the author's choice stated as
 * a choice; a blank in the same slot would read as a value that failed to arrive, which is the one
 * reading this must never produce about a run that is sitting still on purpose.
 */
export function deadlineWords(reading: AskReading): string {
  const { deadline, standing } = reading;
  if (deadline.kind === 'none') {
    return reading.stranded
      ? 'no deadline was declared — this run would have waited indefinitely'
      : 'no deadline — this run waits indefinitely';
  }
  if (reading.stranded) {
    return deadline.kind === 'left'
      ? 'the run ended with the deadline still ahead of it'
      : `the deadline passed ${fmtDuration(deadline.ms)} ago`;
  }
  if (standing === 'answered') {
    return deadline.ms > 0
      ? `answered with ${fmtDuration(deadline.ms)} to spare`
      : 'answered after the deadline had already passed';
  }
  if (standing === 'expired') return 'the deadline passed with nobody answering';
  if (standing === 'abandoned') return 'the run ended before the deadline mattered';
  return deadline.kind === 'left'
    ? `${fmtDuration(deadline.ms)} left`
    : `the deadline passed ${fmtDuration(deadline.ms)} ago`;
}

/** The one term a row is filed under, and the sentence under it. */
export function standingWords(reading: AskReading): { label: string; because: string } {
  switch (reading.standing) {
    case 'answered':
      return { label: 'answered', because: 'a human answered this and the run went on' };
    case 'expired':
      return {
        label: 'expired',
        because:
          'the deadline the author declared passed with nobody answering, and the run has moved on — what that meant is the workflow’s own decision',
      };
    case 'abandoned':
      return {
        label: 'abandoned',
        because: 'the run ended while this question was still open — nobody failed to answer in time',
      };
    default:
      return reading.stranded
        ? {
            // NOT "expired" AND NOT "abandoned". Nobody failed to answer in time and nothing
            // cancelled it: the run ended some other way — most often it failed, which never gets
            // to rewrite its own memo — and took the question with it.
            label: 'never answered',
            because:
              'the run closed with this question still open, so nobody will answer it now — this is not a deadline that passed, and it is not a cancellation',
          }
        : reading.overdue
        ? {
            label: 'waiting on you',
            because:
              'the deadline has gone by and this run has not recorded an ending for the ask — a parked run whose worker is down looks exactly like this',
          }
        : { label: 'waiting on you', because: 'this run is parked on a question and nothing else moves until it is answered' };
  }
}

/* ───────────────────────────── the operator label ───────────────────────────── */

/**
 * What the label beside an answer is, said in the place someone would cite it.
 *
 * ATTRIBUTION, WORDED AS ATTRIBUTION. The appliance is loopback with no credential (ADR 0031), so
 * `by` is whatever the answering client called itself and nothing checked it — `hitl.ts` and
 * `hitl.py` both say so at length, and this is the third place it has to be true, because it is the
 * only one a human reads. The distinction matters most in exactly the situation someone would cite
 * the name, so the sentence is next to the name rather than in a footnote about the architecture.
 */
export const ATTRIBUTION_NOTE =
  'Attribution, not authentication: whoever answered typed this name about themselves. kontra records it and checks nothing, and no decision here is gated on it.';

/**
 * The line an answered ask carries. Never "verified by", and never a name nobody gave.
 *
 * IT TAKES THE LABEL AND NOTHING ELSE, deliberately: there is no other field on an ask that could
 * make this sentence stronger, and a signature that asked for the whole ask would invite one.
 */
export function attributionWords(ask: { by?: string }): string {
  const by = (ask.by ?? '').trim();
  // AN UNLABELLED ANSWER IS UNLABELLED. Filling this with `operator`, or with a hostname, would be
  // kontra asserting something no human said — which is the one sentence this field must never
  // become (`defaultOperator` in `hitl.ts` refuses the same invention on the server).
  return by ? `answered by ${by}, who said so themselves` : 'answered — whoever answered left no name';
}

/** The helper under the label box on a form. Same fact, in the imperative. */
export const ATTRIBUTION_HINT =
  'Recorded beside your answer as attribution. It is not a sign-in — kontra keeps the name and checks nothing. Leave it blank and this appliance’s own KONTRA_OPERATOR is used, or nothing if it has none.';

/* ───────────────────────────── the form ───────────────────────────── */

/**
 * Why an ask has the form it has.
 *
 *   fields      the ask declared a shape with properties. The ordinary case, and a real form.
 *   free        it declared no shape at all (`takes=None`). The author chose not to constrain the
 *               answer, so a JSON box is the honest control and inventing a constraint here would
 *               refuse answers to somebody's own question.
 *   open        it declared an open shape (a `dict`, an `anyOf`) — a different sentence from `free`,
 *               because one is an omission and the other is a decision.
 *   unreadable  the entry carried something in `schema` that is not a schema. The appliance will
 *               refuse an answer to it (`validateAnswer` throws rather than accepting unchecked), so
 *               this is said BEFORE the operator types rather than after they submit.
 *   settled     the ask is over. `_close` drops the schema from the memo deliberately, so there is
 *               no form to draw and nothing to answer.
 */
export type AskShape = 'fields' | 'free' | 'open' | 'unreadable' | 'settled';

export interface AskForm {
  shape: AskShape;
  /** The draft, through the shared renderer's own types. Empty and unused when `shape` is
   *  `settled` or `unreadable` — both are rows to read, not forms to fill. */
  draft: InputDraft;
}

/**
 * The form for one ask.
 *
 * `draftForInput` IS CALLED WITH THE ASK'S OWN SCHEMA and nothing else. What it hands back is the
 * same `InputDraft` the Workflows page starts a run from, so `FieldGroup` draws it, `coerceValues`
 * collects it and a nested Python model nests here for free.
 */
export function askForm(ask: RunAsk): AskForm {
  if (ask.state !== 'pending') return { shape: 'settled', draft: { kind: 'json', text: '', why: 'undeclared' } };
  const schema = ask.schema;
  if (schema === undefined || schema === null) {
    return { shape: 'free', draft: draftForInput(undefined) };
  }
  if (typeof schema !== 'object' || Array.isArray(schema)) {
    // The appliance compiles this before it signals and refuses what it cannot read, so a form
    // drawn over it would collect an answer that can only be rejected.
    return { shape: 'unreadable', draft: { kind: 'json', text: '', why: 'undeclared' } };
  }
  const draft = draftForInput(schema as JsonSchema);
  return { shape: draft.kind === 'fields' ? 'fields' : 'open', draft };
}

/** What the draft currently answers with, or the first field that refuses. */
export function answerOf(draft: InputDraft): InputResult {
  return inputOf(draft);
}

/** The sentence a JSON-box ask carries, per reason. `fields` never reaches here. */
export function shapeWords(shape: AskShape): string {
  switch (shape) {
    case 'free':
      return 'This ask declares no shape for its answer — the author left it open. Type the answer as JSON; anything that parses is accepted.';
    case 'open':
      return 'This ask declares an open shape (any object). Type the answer as JSON.';
    case 'unreadable':
      return 'This ask carries something in place of a schema that could not be read. The appliance refuses an answer it cannot check, so this one cannot be answered from here — the run is still parked on it.';
    default:
      return '';
  }
}

/* ───────────────────────────── the chrome ───────────────────────────── */

/**
 * One run known to be waiting on a human, as the chrome carries it.
 *
 * IT LIVES IN THE APP STORE RATHER THAN ON THE WORKFLOWS PAGE, which is the whole of why this type
 * exists. Surfaces swap in `App.tsx` — a page that unmounts takes its state with it — and the
 * operator being told they are the bottleneck WHILE LOOKING AT SOMETHING ELSE is precisely the
 * requirement. So the fact outlives the page that learned it.
 */
export interface ParkedRun {
  runId: string;
  /** The thread it belongs to, so the mark has somewhere to send you. Empty when the page that
   *  noticed did not know — the rail then says which run without saying which workflow. */
  workflow: string;
  /** How many of its asks are waiting. */
  pending: number;
  /** Epoch ms the OLDEST waiting ask was asked. What "waiting since" is measured from. */
  since: number;
}

/**
 * What one run's asks say for the chrome — or `null` when it is not parked.
 *
 * ONLY EVER THE RUNS SOMETHING HAS ACTUALLY BEEN READ ABOUT. A park lives on the run's own memo, so
 * knowing which of two hundred runs is parked costs two hundred reads; this is fed by the run whose
 * transcript is open, whose asks were fetched anyway. A run nothing has been read about is a run
 * this says nothing about — never one it declares un-parked.
 */
export function parkedRun(runId: string, workflow: string, asks: readonly RunAsk[]): ParkedRun | null {
  const waiting = asks.filter((a) => a.state === 'pending');
  if (waiting.length === 0) return null;
  const asked = waiting.map((a) => a.askedAt).filter((t) => Number.isFinite(t) && t > 0);
  return {
    runId,
    workflow,
    pending: waiting.length,
    // An entry with no readable instant contributes none: `0` here would date the park to 1970 and
    // put "waiting 56 years" in the chrome.
    since: asked.length > 0 ? Math.min(...asked) : 0,
  };
}

/**
 * The parked runs still worth signalling, given what the run list currently says.
 *
 * A CLOSED RUN IS NEVER PARKED, which is `runWord`'s rule one level up and matters more here: the
 * chrome outlives the page, so an entry nothing pruned would leave a mark on the rail forever after
 * the run it was about failed. An ask left `pending` on a run that died is a question nobody will
 * ever answer, and painting a live mark for it is worse than saying nothing.
 *
 * A RUN THE LIST HAS NOT DISCOVERED YET IS KEPT. `/api/runs` lists runs that have DISPATCHED, so a
 * run parked before its first dispatch is absent from the list and is not evidence of anything —
 * dropping it would hide the one park that most needs a human.
 */
export function stillParked(parked: readonly ParkedRun[], runs: readonly RunRow[]): ParkedRun[] {
  const closed = new Set(runs.filter((r) => r.status !== 'running').map((r) => r.runId));
  return parked.filter((p) => !closed.has(p.runId));
}

/** The one line the rail carries. Counts runs, not asks: "which conversation needs me" is the
 *  question a mark in the chrome answers, and the asks are all in the one place anyway. */
export function parkedWords(parked: readonly ParkedRun[]): string {
  if (parked.length === 0) return '';
  const asks = parked.reduce((n, p) => n + p.pending, 0);
  const runs = parked.length === 1 ? '1 run is' : `${parked.length} runs are`;
  const questions = asks === 1 ? '1 question' : `${asks} questions`;
  return `${runs} parked, waiting on a human — ${questions} unanswered`;
}
