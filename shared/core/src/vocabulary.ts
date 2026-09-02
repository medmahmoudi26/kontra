/**
 * kontra's words for what a Run did.
 *
 * WHY THIS EXISTS. `history.ts` files every event under one of Temporal's own layers — workflow,
 * task, activity, failure, timer, marker, child, signal — which is the right index for an event log
 * and the wrong vocabulary for an operator. Nobody watching a sweep is looking for a "nexus
 * operation"; they are looking for a **Method** that was called, a **Batch** that was dispatched, a
 * **Fleet** that came up, a **Dataset** that was sealed. This is that translation.
 *
 * IT IS A FUNCTION, NOT A FIELD, AND THAT IS THE WHOLE DESIGN. The archive (ADR 0025) stores the
 * reduced TEMPORAL log — event types, categories, the two timestamps, `dur`, `attempt`, the workflow
 * an event names, and the one line `history.ts` built from `activityType`, `taskQueue`, `endpoint`
 * and the failure MESSAGE. It stores not one word of what is below. So the day this module learns to
 * name something it could not name before, every run archived weeks ago reads better — no
 * migration, no rewrite, nothing to backfill. Freeze a label into the archive instead and today's
 * mapping is the only story those runs will ever tell. `vocabulary.test.ts` proves this against
 * bytes on disk rather than asserting it.
 *
 * IT NAMES WHAT IT CAN PROVE, AND SAYS SO WHEN IT CANNOT. Every naming carries a
 * {@link Naming.because} — the metadata the name rests on — and a turn this module has no word for
 * keeps its Temporal type verbatim and is marked {@link Naming.untranslated}. A layer that renames
 * confidently when it is unsure is worse than raw events: that is the failure ADR 0027 was written
 * about, one level down, and it is why there is no rule below of the form "it is probably a …".
 *
 * AND `untranslated` IS NOW THE LINE BETWEEN TWO PANES. The account of a run and the log it was read
 * from are two tabs, not one column, so a turn with no word here has somewhere better to be: the
 * Event log, drawn verbatim. {@link NamedTranscript.turns} is therefore the DOMAIN account and
 * {@link NamedTranscript.untranslated} is what the log is left holding — the same list, split by the
 * one question this module answers, and nothing removed from either. Splitting on the turn's KIND
 * instead would have been the wrong cut twice over: `run-worker-lost` and `run-handed-over` are raw
 * turns with real domain words, and a word a later revision teaches has to move a row into the
 * account without any surface changing. The counts stay whole in {@link NamedTranscript.filters},
 * so a reader can see how many events went to the other pane rather than having to trust that none
 * did.
 *
 * NAMING IS WHAT MAKES A FAILURE VISIBLE, which is the part of this that is not cosmetic.
 * `history.ts` concedes in its own header that a dispatch onto a queue nobody polls, an activity in
 * retry backoff and a workflow task that timed out "all look identical from outside" — `running`, no
 * new rows, three different disasters. They look identical because none of them has a name. Three
 * terms below are those three facts, each derived from metadata the reduced log already carries:
 *
 *   `queue-unpolled`    a dispatch Temporal closed without ever recording a worker taking it.
 *   `dispatch-stalled`  `attempt` moved, or the failure line says `RETRY_STATE_IN_PROGRESS`.
 *   `run-worker-lost`   the caller's OWN workflow task timed out — nobody answered for the run.
 *
 * A fourth, `run-produced-nothing`, is the documented case where a node whose **Units** all failed
 * reports `completed` with empty output. It is a term precisely so that it can never read as a
 * clean success, and {@link Concern} is how it reaches a surface that is only showing headlines.
 *
 * METADATA ONLY. Nothing here decodes a payload, fetches a blob, or reads a clock it was not handed
 * — the same constraint as `transcript.ts`, inherited from ADR 0007, with the same consequence: a
 * fact that lives inside a payload is ABSENT rather than defaulted. `units-isolated` cannot fire
 * until a Summary carries the counts, because a **Batch** that dropped every **Unit** and one that
 * legitimately found nothing must not be told apart by inventing a zero (ADR 0023 §13).
 */

import {
  expand,
  readTranscript,
  type DatasetTurn,
  type DispatchTurn,
  type FailedTurn,
  type FinishedTurn,
  type FleetTurn,
  type ParkedTurn,
  type RawTurn,
  type Transcript,
  type TranscriptHistory,
  type TranscriptOptions,
  type Turn,
} from './transcript';

/* ───────────────────────────── the vocabulary ───────────────────────────── */

/**
 * What a filter chips by — kontra's own seams, not Temporal's layers.
 *
 * `temporal` IS ONE OF THEM, deliberately. Everything this module could not translate has to land
 * somewhere an operator can find it, and the honest place is a group that says whose vocabulary it
 * is. A `temporal` chip with a count on it is this module telling you where it ran out of words.
 */
export type Domain = 'run' | 'fleet' | 'actor' | 'dataset' | 'human' | 'temporal';

/**
 * How a term should read.
 *
 * `wrong` COVERS CANCELLED, following `history.ts`'s own rule: a completed cancellation is a failure
 * "in the only sense that matters here — the output is missing". The word `cancelled` stays in the
 * label so the operator who asked for it recognises their own act; the tone is what stops a
 * cancelled dispatch rendering the same green as one that returned.
 */
export type Tone = 'ok' | 'busy' | 'wrong' | 'unknown';

/** Every word this release has. A term is an ID, not a label: the label can improve, and a filter
 *  built on the ID keeps working when it does. */
export type Term =
  // ── the run itself ──
  | 'run-started'
  | 'run-running'
  | 'run-handed-over'
  | 'run-finished'
  | 'run-produced-nothing'
  | 'run-nothing-worked'
  | 'run-failed'
  | 'run-cancelled'
  | 'run-worker-lost'
  | 'author-note'
  // ── a human ──
  | 'run-parked'
  | 'ask-answered'
  // ── the fleet ──
  | 'fleet-requested'
  | 'fleet-building'
  | 'fleet-ready'
  | 'fleet-torn-down'
  | 'fleet-failed'
  | 'fleet-cancelled'
  // ── one Machine's lifecycle, as its Warden's blocked watcher records it (ADR 0037) ──
  | 'machine-watching'
  | 'machine-assignment-changed'
  | 'machine-worker-started'
  | 'machine-worker-exited'
  | 'machine-unreachable'
  // ── actors, methods, batches, units ──
  | 'method-called'
  | 'batch-dispatched'
  | 'actor-activated'
  | 'method-returned'
  | 'method-failed'
  | 'method-cancelled'
  | 'dispatch-stalled'
  | 'queue-unpolled'
  | 'units-isolated'
  | 'every-unit-isolated'
  // ── the lake ──
  | 'dataset-read'
  | 'dataset-opened'
  | 'batch-published'
  | 'dataset-sealing'
  | 'dataset-sealed'
  | 'dataset-unsealed'
  | 'dataset-promoted'
  | 'dataset-tagged'
  // ── whose vocabulary it is when it is not ours ──
  | 'temporal-plumbing'
  | 'untranslated';

/** What one term is, as data. */
export interface TermSpec {
  /** The phrase an operator reads. */
  label: string;
  /** The filter group it belongs to. */
  domain: Domain;
  tone: Tone;
}

/**
 * Today's mapping.
 *
 * DATA, AND EXPORTED AS DATA, because it is the thing that is meant to improve. A surface builds its
 * filter chips from this table rather than from a list of strings it keeps in step by hand, and a
 * future revision that finds a better sentence changes one row here and every archived run reads
 * differently the next time it is opened.
 */
export const TERMS: Readonly<Record<Term, TermSpec>> = {
  'run-started': { label: 'run started', domain: 'run', tone: 'ok' },
  'run-running': { label: 'run still running', domain: 'run', tone: 'busy' },
  'run-handed-over': { label: 'run handed over', domain: 'run', tone: 'busy' },
  'run-finished': { label: 'run finished', domain: 'run', tone: 'ok' },
  'run-produced-nothing': { label: 'run finished, produced nothing', domain: 'run', tone: 'wrong' },
  'run-nothing-worked': { label: 'run finished, and nothing worked', domain: 'run', tone: 'wrong' },
  'run-failed': { label: 'run failed', domain: 'run', tone: 'wrong' },
  'run-cancelled': { label: 'run cancelled', domain: 'run', tone: 'wrong' },
  'run-worker-lost': { label: "the run's worker stopped answering", domain: 'run', tone: 'wrong' },
  'author-note': { label: 'note', domain: 'run', tone: 'ok' },

  'run-parked': { label: 'run parked, waiting on a human', domain: 'human', tone: 'busy' },
  'ask-answered': { label: 'ask answered', domain: 'human', tone: 'ok' },

  'fleet-requested': { label: 'fleet requested', domain: 'fleet', tone: 'busy' },
  'fleet-building': { label: 'fleet building', domain: 'fleet', tone: 'busy' },
  'fleet-ready': { label: 'fleet ready', domain: 'fleet', tone: 'ok' },
  // THE TEARDOWN, WHICH USED TO SAY `fleet ready`. A run that provisioned Droplets and destroyed
  // them showed "ready" twice and never showed them go away — for the one event an operator paying
  // by the hour most needs to see. `ok` and not `busy`: a destroyed fleet is a good outcome, and
  // the thing that would be alarming is its absence.
  'fleet-torn-down': { label: 'fleet destroyed', domain: 'fleet', tone: 'ok' },
  'fleet-failed': { label: 'fleet failed', domain: 'fleet', tone: 'wrong' },
  'fleet-cancelled': { label: 'fleet cancelled', domain: 'fleet', tone: 'wrong' },

  // A MACHINE'S LIFECYCLE IS FLEET VOCABULARY, not a domain of its own. `infra/CONTEXT.md` puts
  // **Machine** in the Fleet context and a sixth chip group would split one operator question —
  // "what is my capacity doing" — across two filters. `busy` on `machine-watching` because a
  // watcher that is waiting is the resting state and must not read as an achievement.
  'machine-watching': { label: 'machine watched', domain: 'fleet', tone: 'busy' },
  'machine-assignment-changed': { label: 'machine assignment changed', domain: 'fleet', tone: 'ok' },
  'machine-worker-started': { label: 'worker started', domain: 'fleet', tone: 'ok' },
  // A WORKER THAT STOPPED WITHOUT BEING ASKED TO IS `wrong`. The Warden restarts it, so the Fleet
  // recovers — and a surface that rendered the recovery and not the fault would hide a crash loop
  // behind a green row. See the Go side: a Worker removed from the assignment is NOT this.
  'machine-worker-exited': { label: 'worker exited', domain: 'fleet', tone: 'wrong' },
  'machine-unreachable': { label: 'machine unreachable', domain: 'fleet', tone: 'wrong' },

  'method-called': { label: 'method called', domain: 'actor', tone: 'busy' },
  'batch-dispatched': { label: 'batch dispatched', domain: 'actor', tone: 'busy' },
  'actor-activated': { label: 'actor activated', domain: 'actor', tone: 'busy' },
  'method-returned': { label: 'method returned', domain: 'actor', tone: 'ok' },
  'method-failed': { label: 'method failed', domain: 'actor', tone: 'wrong' },
  'method-cancelled': { label: 'method cancelled', domain: 'actor', tone: 'wrong' },
  'dispatch-stalled': { label: 'stalled, retrying', domain: 'actor', tone: 'wrong' },
  'queue-unpolled': { label: 'dispatched onto a queue nobody polled', domain: 'actor', tone: 'wrong' },
  'units-isolated': { label: 'units isolated', domain: 'actor', tone: 'wrong' },
  'every-unit-isolated': { label: 'every unit isolated', domain: 'actor', tone: 'wrong' },

  'dataset-read': { label: 'dataset read', domain: 'dataset', tone: 'ok' },
  'dataset-opened': { label: 'dataset opened', domain: 'dataset', tone: 'ok' },
  'batch-published': { label: 'batch published', domain: 'dataset', tone: 'ok' },
  'dataset-sealing': { label: 'dataset sealing', domain: 'dataset', tone: 'busy' },
  'dataset-sealed': { label: 'dataset sealed', domain: 'dataset', tone: 'ok' },
  'dataset-unsealed': { label: 'dataset left unsealed', domain: 'dataset', tone: 'wrong' },
  'dataset-promoted': { label: 'dataset promoted', domain: 'dataset', tone: 'ok' },
  'dataset-tagged': { label: 'dataset tagged', domain: 'dataset', tone: 'ok' },

  'temporal-plumbing': { label: "Temporal's own bookkeeping", domain: 'temporal', tone: 'ok' },
  'untranslated': { label: 'untranslated', domain: 'temporal', tone: 'unknown' },
};

/** The order a filter bar puts the groups in: the run, then what it built, then what it called,
 *  then what it wrote, then what it is waiting on, then everything this module had no word for. */
export const DOMAINS: readonly Domain[] = ['run', 'fleet', 'actor', 'dataset', 'human', 'temporal'];

/**
 * The vocabulary a reading uses. Absent means this release's — {@link TERMS} and the rules below.
 *
 * IT IS A PARAMETER BECAUSE IT IS NOT IN THE ARCHIVE, which is the property this whole module
 * exists to keep. Passing a different one is exactly what a future release does to a run that
 * closed months ago: the stored bytes do not move, and the run reads differently. That is the
 * difference between a vocabulary and a schema, and it is asserted rather than claimed.
 */
export interface Vocabulary {
  /** Better words for terms this release already has — an overlay, so a revision changes the rows
   *  it improved and inherits the rest. */
  terms?: Partial<Record<Term, Partial<TermSpec>>>;
  /**
   * A word for something this release has none for.
   *
   * TRIED LAST, AFTER EVERY BUILT-IN RULE HAS DECLINED. A revision can therefore teach the reader a
   * new word without any risk of silently re-pointing one that already reads right — the failure
   * that would make every archived run's account quietly wrong instead of quietly incomplete.
   */
  name?: (turn: Turn) => Term | undefined;
}

/* ───────────────────────────── what comes out ───────────────────────────── */

/** One turn, named. */
export interface Naming {
  term: Term;
  /**
   * What the row says.
   *
   * NOT ALWAYS THE TERM'S LABEL. An untranslated turn's label is its raw Temporal type, because the
   * rule is that it renders as ITSELF. A filter chip uses `TERMS[term].label`; a row uses this.
   */
  label: string;
  domain: Domain;
  tone: Tone;
  /** Nothing translated this. The label is Temporal's word, shown rather than guessed at. */
  untranslated: boolean;
  /**
   * The metadata this name rests on.
   *
   * SO A WRONG NAME IS ARGUABLE RATHER THAN MYSTERIOUS. Every sentence here quotes something the
   * reduced log actually carries — a type, an `attempt`, a state, a count of members — and never a
   * payload. An operator who disagrees with a name can see what it was read from.
   */
  because: string;
}

/** A fact that is wrong and has no failure event of its own. */
export type ConcernTerm =
  | 'queue-unpolled'
  | 'stalled'
  | 'worker-lost'
  | 'units-isolated'
  | 'every-unit-isolated'
  | 'open-at-close'
  | 'produced-nothing'
  | 'log-elided';

/**
 * Something an operator has to be told, which no `failure` event will tell them.
 *
 * THIS IS THE HALF THAT IS NOT COSMETIC. A run whose **Units** all failed closes `completed` with an
 * empty **Dataset** and not one failure event anywhere in its history; a dispatch onto an unpolled
 * queue is `running` until a timeout fires. Both are invisible to a surface that renders only what
 * Temporal called a failure, so they are raised HERE, beside the naming, from the same metadata.
 */
export interface Concern {
  term: ConcernTerm;
  /** The sentence, already written for a human. */
  label: string;
  /** The metadata it was read from. Never a payload. */
  because: string;
  /** The Temporal event ids it is about — the drill path, same as a turn's. Empty on a concern
   *  about the run as a whole rather than about anything in it. */
  events: number[];
  /** Seconds from the run's first event. `0` on a run-level concern. */
  t: number;
}

/** One turn with its name and whatever it made this module worry about. */
export interface NamedTurn extends Naming {
  turn: Turn;
  concerns: Concern[];
}

/** One filter chip: a domain term with the count behind it. */
export interface Filter {
  term: Term;
  /** The TERM's label — not any one row's. See {@link Naming.label}. */
  label: string;
  domain: Domain;
  count: number;
}

/** A whole run, in kontra's words. */
export interface NamedTranscript {
  /** The reading this was named from. Carried so a caller never has to read the log twice, and so
   *  every number a surface prints still comes from the one authority on turns. */
  transcript: Transcript;
  /**
   * The domain account: every turn kontra has a word for, in order.
   *
   * WHAT THE TRANSCRIPT PANE DRAWS, AND ALL OF IT. A run's account of itself is what it set out to
   * do, what it built, what it called, what it wrote, what it asked and how it ended — Temporal's
   * scheduler bookkeeping is an account of nothing, and three `WorkflowTaskScheduled` rows between
   * two turns are three rows an operator has to read past. They cost a real `speak` sentence its
   * reader on a real parked run, which is what this split is for.
   */
  turns: NamedTurn[];
  /**
   * The turns with no word in this release — where they went, not that they went.
   *
   * NOTHING IS DROPPED, AND THIS FIELD IS THE PROOF. They are named (as their own Temporal type),
   * counted in {@link filters}, and drawn verbatim in the Event log; their {@link Concern}s are in
   * {@link concerns} exactly as before. A caller that wants the whole account in one list still has
   * `transcript.turns`, which is untouched — this is a partition of it, not a filter over it.
   */
  untranslated: NamedTurn[];
  /** The one line at the top: how this run reads, in one term. */
  verdict: Naming;
  /** Every concern, in the order the run raised them, run-level ones last. */
  concerns: Concern[];
  /** The chips, in {@link DOMAINS} order and then first appearance. */
  filters: Filter[];
}

/* ───────────────────────────── the reader ───────────────────────────── */

/**
 * One run's reduced log, read in kontra's words.
 *
 * Pure, like everything it stands on: reduced events in, names out, no fetch and no clock. The same
 * call answers for a live run and for one Temporal dropped months ago, because the archive holds the
 * same reduced events either way.
 */
export function readVocabulary(
  history: TranscriptHistory,
  options: TranscriptOptions = {},
  vocab?: Vocabulary
): NamedTranscript {
  return nameTranscript(readTranscript(history, options), vocab, history);
}

/** The same reading, for a caller that already has the transcript. */
export function nameTranscript(
  transcript: Transcript,
  vocab?: Vocabulary,
  history?: TranscriptHistory
): NamedTranscript {
  const closed = !transcript.live;
  const named: NamedTurn[] = transcript.turns.map((turn) => ({
    ...nameTurn(turn, vocab),
    turn,
    concerns: concernsOf(turn, closed),
  }));

  // THE SPLIT IS MADE ONCE, HERE, so no component can decide it differently. `nameTurn` has already
  // been asked of every turn — including the extension's last word — so a type this release learned
  // a name for lands in the account and one it did not lands in the log, from the same pass.
  const turns = named.filter((t) => !t.untranslated);
  const untranslated = named.filter((t) => t.untranslated);

  // FROM BOTH HALVES, ALWAYS. A workflow task that timed out is an untranslated turn on any release
  // that has no word for it, and it is still the run's own worker going quiet — a concern that
  // followed its row into the other pane would be a failure nobody is told about.
  const concerns = named.flatMap((t) => t.concerns);
  concerns.push(...runConcerns(transcript, history));

  return {
    transcript,
    turns,
    untranslated,
    verdict: verdictOf(transcript, vocab),
    concerns,
    // OVER BOTH HALVES, so the bar still accounts for every event the log carried. A chip for a term
    // whose rows are in the other pane is the honest statement: 47 of Temporal's own, and they are
    // one tab away — rather than a bar that silently stops summing to the run.
    filters: filtersOf(named, vocab),
  };
}

/**
 * The word for one turn.
 *
 * Exported because a folded group is one row until an operator expands it, and every member of that
 * group has to be nameable on its own when they do. `expand(turn).map((t) => nameTurn(t))` is the
 * whole of what an expanded loop needs.
 */
export function nameTurn(turn: Turn, vocab?: Vocabulary): Naming {
  switch (turn.kind) {
    case 'started':
      return say('run-started', `workflowType=${turn.workflowType || '?'}`, vocab);
    case 'fleet':
      return nameFleet(turn, vocab);
    case 'dispatch':
      return nameDispatch(turn, vocab);
    case 'dataset':
      return nameDataset(turn, vocab);
    case 'narration':
      // THE ONE TURN WHOSE DETAIL IS THE CONTENT, NOT A DESCRIPTION OF THE MECHANISM. Every other
      // case here explains what KIND of thing happened, because that is all there is to say —
      // "Temporal's own scheduler bookkeeping" is the whole of a WorkflowTaskScheduled. A narration
      // is the opposite: an author wrote a sentence FOR THIS LINE, and describing the machinery
      // that carried it ("a Summary the author set on their own event") is the one reading that
      // discards the only thing anybody wanted. `speak` exists to put words here.
      //
      // An empty sentence says so rather than rendering a blank row, because a narration that
      // reached the log carrying nothing is an author's bug and should look like one.
      return say('author-note', turn.text || '(an empty sentence)', vocab);
    case 'parked':
      return namePark(turn, vocab);
    case 'finished':
      return nameFinished(turn, vocab);
    case 'failed':
      return say(
        turn.outcome === 'cancelled' ? 'run-cancelled' : 'run-failed',
        turn.where ? `failed at event ${turn.where.event}: ${turn.where.detail}` : 'the run closed on its own terms',
        vocab
      );
    case 'raw':
      return nameRaw(turn, vocab);
  }
}

/** A **Fleet** operation, by what Temporal's child events say happened to it. */
function nameFleet(turn: FleetTurn, vocab?: Vocabulary): Naming {
  const term: Term =
    turn.state === 'requested'
      ? 'fleet-requested'
      : turn.state === 'running'
        ? 'fleet-building'
        : turn.state === 'ready'
          ? // THE SECOND CLOSED OPERATION ON ONE FLEET ID IS THE TEARDOWN. `up`/`destroy` rides in
            // the child's input, which the log never decodes — but `fleet.up()` is a context
            // manager, so a scope opens with one child and closes with another sharing its id.
            // Inference from shape, and only ever applied to an operation that has CLOSED.
            turn.seq !== undefined && turn.seq > 1
            ? 'fleet-torn-down'
            : 'fleet-ready'
          : turn.state === 'cancelled'
            ? 'fleet-cancelled'
            : 'fleet-failed';
  // The machine counts ride inside the child's input, which the log never decodes — so the reason
  // is the child's id and its state, which is all a payload-free reader can honestly point at.
  return say(term, `child workflow ${turn.fleet}, state=${turn.state}`, vocab);
}

/**
 * A call to an **Actor**, and the three failures that hide inside one.
 *
 * ORDER IS THE DESIGN HERE. The two invisible states are checked BEFORE the dispatch's own state,
 * because both of them present as an ordinary `queued` or `failed` and would otherwise be told in
 * Temporal's words — which is how they stayed invisible. Isolation is checked LAST, and only on a
 * dispatch that returned, because a **Method** that failed outright already has a headline and
 * `units-isolated` exists for the one that did not: a call that reported success while dropping the
 * work (ADR 0023 §13).
 */
function nameDispatch(turn: DispatchTurn, vocab?: Vocabulary): Naming {
  const members = expand(turn);
  const unpolled = members.filter(neverPickedUp);
  if (unpolled.length === members.length && unpolled.length > 0) {
    return say(
      'queue-unpolled',
      `${plural(unpolled.length, 'call')} closed by a failure with no ${startedWord(turn)} event — no worker was ever recorded taking it${turn.queue ? ` off ${turn.queue}` : ''}`,
      vocab
    );
  }

  const stall = stalledBecause(turn);
  if (stall) return say('dispatch-stalled', stall, vocab);

  const where = turn.endpoint ? `endpoint=${turn.endpoint}` : turn.queue ? `taskQueue=${turn.queue}` : `actor=${turn.actor || '?'}`;
  switch (turn.state) {
    case 'failed':
      return say('method-failed', `${where}${retryState(turn) ? ` · ${retryState(turn)}` : ''}`, vocab);
    case 'cancelled':
      return say('method-cancelled', where, vocab);
    case 'queued':
      // A dispatch that has only been ASKED FOR reads as the ask. It is not called unpolled: with no
      // close and no `Started`, "nobody is polling" and "the worker is busy" are the same two events,
      // and guessing between them is the thing this module refuses to do.
      return say(
        turn.endpoint ? 'method-called' : 'batch-dispatched',
        `${where} · scheduled at ${offset(turn.t)}, no worker has taken it yet`,
        vocab
      );
    case 'running':
      return say('actor-activated', `${where}${turn.queued !== undefined ? ` · ${turn.queued}s on the queue` : ''}`, vocab);
    case 'done': {
      const units = turn.units;
      if (units?.dropped !== undefined && units.dropped > 0) {
        const all = units.in !== undefined && units.dropped >= units.in;
        return say(
          all ? 'every-unit-isolated' : 'units-isolated',
          `dropped=${units.dropped}${units.in !== undefined ? ` of in=${units.in}` : ''} — the call returned, the Units did not`,
          vocab
        );
      }
      return say('method-returned', `${where}${turn.attempt > 1 ? ` · after ${turn.attempt} attempts` : ''}`, vocab);
    }
  }
}

/** What the lake activity did, in the lake's own words. */
function nameDataset(turn: DatasetTurn, vocab?: Vocabulary): Naming {
  const named = turn.dataset ? ` dataset=${turn.dataset}` : '';
  switch (turn.action) {
    case 'read':
      return say('dataset-read', `pageDataset${named}`, vocab);
    case 'open':
      return say('dataset-opened', `openTempDataset${named}`, vocab);
    case 'publish':
      return say('batch-published', `${plural(turn.count, 'publishBatch call')}${named}`, vocab);
    case 'promote':
      return say('dataset-promoted', `promoteDataset${named}`, vocab);
    case 'tag':
      return say('dataset-tagged', `tagDataset${named}`, vocab);
    case 'close':
      // SEALED IS READ FROM THE ACT, NOT FROM THE STATE FIELD, and this is the one place that
      // distinction has to be stated. `closeDataset` takes a `state` inside its payload — the log
      // never sees it — so what this can witness is whether the caller's declaration LANDED. A
      // Dataset is "sealed when the caller declares it complete, and abandoned if the Run died
      // first" (CONTEXT.md): a close that completed is that declaration, and a close that failed is
      // a Dataset nobody has declared anything about. Neither reading invents the payload's word.
      if (turn.failures > 0) return say('dataset-unsealed', `closeDataset failed${named} — nothing declared this Dataset complete`, vocab);
      if (turn.open) return say('dataset-sealing', `closeDataset in flight${named}`, vocab);
      return say('dataset-sealed', `closeDataset completed${named}`, vocab);
  }
}

function namePark(turn: ParkedTurn, vocab?: Vocabulary): Naming {
  return turn.pending
    ? say('run-parked', `asked at ${offset(turn.t)}, waiting ${round(turn.waited)}s${turn.expired ? ' — past the deadline the author declared' : ''}`, vocab)
    : say('ask-answered', `answered after ${round(turn.waited)}s${turn.ask.by ? ` by ${turn.ask.by}` : ''}`, vocab);
}

/**
 * The run's own closing event.
 *
 * A COMPLETED RUN THAT WROTE NOTHING GETS ITS OWN TERM. This is the documented failure — a node
 * whose **Units** all failed reports `completed` with empty output — and giving it the same word as
 * a successful run is precisely how it stayed invisible. `published` is counted from the run's own
 * dispatch metadata by `transcript.ts` and is never a row count, so this says "produced nothing",
 * which is what the log can witness, rather than "wrote no rows", which it cannot.
 */
function nameFinished(turn: FinishedTurn, vocab?: Vocabulary): Naming {
  if (!turn.empty) return say('run-finished', `published=${turn.published}`, vocab);
  return say('run-produced-nothing', 'the run completed and scheduled no publish at all', vocab);
}

/**
 * A turn `transcript.ts` had no word for.
 *
 * TWO OF THEM ARE TRANSLATABLE HERE AND NOT THERE, which is this module earning its keep: the
 * reader decides which events become a turn, and this decides what a turn is called, so a type the
 * reader files as raw can still have a name. Everything else keeps its Temporal type, verbatim.
 */
function nameRaw(turn: RawTurn, vocab?: Vocabulary): Naming {
  if (turn.type === 'WorkflowExecutionContinuedAsNew') {
    return say('run-handed-over', 'the run continued as new — a fresh execution, the same Run', vocab);
  }
  if (turn.type === 'WorkflowTaskTimedOut') {
    // THE THIRD OF THE THREE. A workflow task that timed out means nobody answered for the CALLER's
    // own workflow: its worker died, hung, or was never polling its queue. From outside it is a run
    // that is simply "running", which is why it needs a word of its own.
    return say('run-worker-lost', `${turn.type} — the caller's own workflow task went unanswered`, vocab);
  }
  const machine = namesAMachine(turn.label);
  if (machine) return say(machine.term, machine.because, vocab);
  const extra = vocab?.name?.(turn);
  if (extra) return say(extra, `${turn.type}, named by a later revision of the vocabulary`, vocab);
  const spec = specOf(turn.plumbing ? 'temporal-plumbing' : 'untranslated', vocab);
  return {
    term: turn.plumbing ? 'temporal-plumbing' : 'untranslated',
    // RENDERED AS ITSELF. The Temporal type is the label, so an operator is never confidently told
    // the wrong thing about an event this module misread — they are told nothing, and shown the row.
    label: turn.type,
    domain: spec.domain,
    tone: turn.failures > 0 ? 'wrong' : spec.tone,
    untranslated: true,
    because: turn.plumbing ? "Temporal's own scheduler bookkeeping" : 'no rule in this release names this type',
  };
}

/* ───────────────────────────── a Machine's own lifecycle ───────────────────────────── */

/**
 * How a **Warden** spells a decision about its **Machine**, and the second cross-language literal in
 * this module.
 *
 * `cli/warden/warden_workflow.go` writes `kontra.machine · <kind> · <subject> · <detail>` as a Temporal
 * user-metadata Summary on the event that arms the next watch. There is no import to make: that side
 * is Go, this side is bundled into the browser, and the house rule for a shared literal is to write
 * it on each side and pin it with a test — which is what `transcript.ts` already does for the ask's
 * memo prefix. `vocabulary.test.ts` reads `warden_workflow.go`'s BYTES and asserts the prefix and all
 * five kinds, so a rename in Go is a red test rather than a Fleet that quietly stops appearing.
 *
 * WHY THIS IS A RULE HERE AND NOT A TURN KIND IN `transcript.ts`. The reader's question is "which
 * events become a turn", and the answer for these is already right: one activity per decision, its
 * Summary folded onto the turn as {@link TurnBase.label}. The question this module answers is "is
 * there a word for it", and until this release there was not — the rows were real, drawn in the Event
 * log as `ActivityTaskScheduled`. Naming them moves them into the account with no surface changing,
 * which is the property {@link nameTranscript} exists to have, and it improves every Warden workflow
 * already archived.
 */
const MACHINE_SUMMARY_PREFIX = 'kontra.machine';

/** What each kind is called on the wire, and the term it becomes. Data, so the Go side's five
 *  spellings and this table are one thing a test can compare rather than five branches. */
const MACHINE_KINDS: Record<string, Term> = {
  watching: 'machine-watching',
  'assignment changed': 'machine-assignment-changed',
  'worker started': 'machine-worker-started',
  'worker exited': 'machine-worker-exited',
  'machine unreachable': 'machine-unreachable',
};

/**
 * Read a **Warden**'s Summary back, or nothing.
 *
 * NOTHING IS GUESSED. A label that starts with the prefix but names a kind this release has no term
 * for returns `undefined` and the row stays untranslated in the Event log, verbatim — which is the
 * correct outcome for a Machine running a newer Warden than the control plane reading it, and it is
 * the version skew ADR 0037 calls "a permanent cost". A confident wrong name would be worse than the
 * raw event, which is this module's whole first principle.
 */
function namesAMachine(label: string | undefined): { term: Term; because: string } | undefined {
  if (!label || !label.startsWith(MACHINE_SUMMARY_PREFIX + ' · ')) return undefined;
  const [, kind, subject, ...rest] = label.split(' · ');
  const term = kind ? MACHINE_KINDS[kind] : undefined;
  if (!term) return undefined;
  // The subject and the detail are the Warden's own words, quoted rather than paraphrased: they name
  // the Worker or the Machine, and the reason. Nothing here is derived from a payload.
  const detail = rest.join(' · ');
  return { term, because: [subject, detail].filter(Boolean).join(' · ') || kind! };
}

/* ───────────────────────────── the concerns ───────────────────────────── */

/** What one turn makes this module worry about. `closed` is whether the RUN has ended, which is
 *  what turns "still open" from a normal state into a fact. */
function concernsOf(turn: Turn, closed: boolean): Concern[] {
  const out: Concern[] = [];
  if (turn.kind === 'dispatch') {
    const members = expand(turn);
    const unpolled = members.filter(neverPickedUp);
    if (unpolled.length > 0) {
      out.push({
        term: 'queue-unpolled',
        label:
          unpolled.length === members.length
            ? 'dispatched onto a queue nobody polled'
            : `${unpolled.length} of ${members.length} calls in this group were never picked up`,
        because: `closed by a failure with no ${startedWord(turn)} event${turn.queue ? ` · taskQueue=${turn.queue}` : ''}${turn.endpoint ? ` · endpoint=${turn.endpoint}` : ''}`,
        events: unpolled.flatMap((m) => m.events),
        t: unpolled[0]!.t,
      });
    }
    const stall = stalledBecause(turn);
    if (stall) {
      out.push({
        term: 'stalled',
        label: 'in retry backoff, not working',
        because: stall,
        events: turn.events,
        t: turn.t,
      });
    }
    const units = turn.units;
    if (units?.dropped !== undefined && units.dropped > 0) {
      const all = units.in !== undefined && units.dropped >= units.in;
      out.push({
        term: all ? 'every-unit-isolated' : 'units-isolated',
        label: all
          ? 'every Unit was isolated — this call returned having done nothing'
          : `${units.dropped} Units were isolated and dropped`,
        because: `dropped=${units.dropped}${units.in !== undefined ? ` of in=${units.in}` : ''}`,
        events: turn.events,
        t: turn.t,
      });
    }
  }
  if (turn.kind === 'raw' && turn.type === 'WorkflowTaskTimedOut') {
    out.push({
      term: 'worker-lost',
      label: "the run's own worker stopped answering",
      because: 'WorkflowTaskTimedOut on the caller\'s workflow',
      events: turn.events,
      t: turn.t,
    });
  }
  // STILL OPEN WHEN THE RUN ENDED. Not a Temporal failure and not an accounting error: it is work
  // this run caused and then stopped waiting for, which is exactly the shape of a sweep whose
  // output never arrived. Parked asks are exempt — an unanswered ask on a closed run is the ask
  // route's business, and `run-parked` already says it.
  if (closed && turn.open && turn.kind !== 'parked' && turn.kind !== 'raw') {
    out.push({
      term: 'open-at-close',
      label: 'still outstanding when the run ended',
      because: `no closing event for ${turn.types.join(', ')}`,
      events: turn.events,
      t: turn.t,
    });
  }
  return out;
}

/** What the run as a whole makes this module worry about. */
function runConcerns(t: Transcript, history?: TranscriptHistory): Concern[] {
  const out: Concern[] = [];
  if (!t.live && t.outcome === 'completed' && t.empty) {
    out.push({
      term: 'produced-nothing',
      label: 'the run completed and published nothing',
      because: 'published=0 across every dispatch this run scheduled',
      events: [],
      t: 0,
    });
  }
  const elided = t.elided || history?.elided || 0;
  if (elided > 0 || t.truncated) {
    // A HOLE NOBODY MENTIONS IS WORSE THAN NO ACCOUNT. Every name above is a name for the events
    // that survived the cap, so an account with a gap has to carry the gap beside the names.
    out.push({
      term: 'log-elided',
      label: `${elided} events are missing from the middle of this account`,
      because: t.truncated ? 'the log was capped and paging stopped before the end' : 'the log was capped',
      events: [],
      t: 0,
    });
  }
  return out;
}

/* ───────────────────────────── the verdict ───────────────────────────── */

/**
 * How the whole run reads, in one term.
 *
 * `completed` IS NOT ONE ANSWER, IT IS THREE. A run that finished having published something, one
 * that finished having published nothing, and one that finished having published nothing while
 * things were failing inside it are three different mornings for whoever reads it — and Temporal
 * calls all three `WorkflowExecutionCompleted`. Separating them here is the whole of the
 * invisible-failure half of this module.
 */
function verdictOf(t: Transcript, vocab?: Vocabulary): Naming {
  if (t.live) {
    if (t.continued) return say('run-handed-over', 'the last event is a continue-as-new — the Run is still running', vocab);
    return say('run-running', 'no closing event in this log', vocab);
  }
  if (t.outcome === 'cancelled') return say('run-cancelled', 'WorkflowExecutionCanceled', vocab);
  if (t.outcome === 'failed') return say('run-failed', failedBecause(t), vocab);
  if (!t.empty) return say('run-finished', `published=${t.published}`, vocab);
  const broke = t.turns.filter((turn) => turn.failures > 0 || (turn.kind === 'dispatch' && neverPickedUp(turn)));
  if (broke.length > 0) {
    return say(
      'run-nothing-worked',
      `completed, published=0, and ${plural(broke.length, 'turn')} carried a failure`,
      vocab
    );
  }
  return say('run-produced-nothing', 'completed, published=0', vocab);
}

function failedBecause(t: Transcript): string {
  const last = t.turns.find((turn) => turn.kind === 'failed') as FailedTurn | undefined;
  if (!last) return 'the run closed on a failure event';
  return last.where ? `failed at event ${last.where.event}: ${last.where.detail}` : last.because;
}

/* ───────────────────────────── the filters ───────────────────────────── */

/**
 * The chips a surface offers, with what is behind each.
 *
 * DOMAIN TERMS, NOT TEMPORAL'S LAYERS. This is what replaces the `EventCategory` filter bar: a
 * chip is a term this run actually produced, which means the bar never offers a filter that finds
 * nothing, and never omits one that would. `temporal` sorts last so what could not be translated is
 * present and findable without leading the reading.
 */
export function filtersOf(turns: readonly NamedTurn[], vocab?: Vocabulary): Filter[] {
  const counts = new Map<Term, number>();
  const order: Term[] = [];
  for (const t of turns) {
    const have = counts.get(t.term);
    if (have === undefined) order.push(t.term);
    // A folded loop is ONE row and N calls; the chip counts the calls, because "12 methods failed"
    // is the number an operator is looking for and "1 row" is not.
    counts.set(t.term, (have ?? 0) + Math.max(1, t.turn.count));
  }
  return order
    .map((term) => {
      const s = specOf(term, vocab);
      return { term, label: s.label, domain: s.domain, count: counts.get(term)! };
    })
    .sort((a, b) => DOMAINS.indexOf(a.domain) - DOMAINS.indexOf(b.domain) || order.indexOf(a.term) - order.indexOf(b.term));
}

/* ───────────────────────────── reading the metadata back ───────────────────────────── */

/** Every event whose name says a worker took the work. The absence of one across a whole family is
 *  the only payload-free proof that nobody ever did. */
const STARTED = /Started$/;

/**
 * Was this call closed without anyone ever taking it?
 *
 * THE UNPOLLED QUEUE, PROVEN RATHER THAN GUESSED. Temporal writes a `…Started` event for every
 * dispatch a worker picks up, and `transcript.ts` folds it into the same turn. A family that CLOSED
 * on a failure with no such event in it is a family whose schedule-to-start (or schedule-to-close)
 * timeout fired before any worker asked for the work — which is what "nobody is polling that queue"
 * looks like in metadata, and is not what a slow **Method** looks like: a slow one started.
 *
 * The close is required. A dispatch that is merely still queued proves nothing at all — "no worker
 * yet" and "no worker ever" are the same two events until the timeout fires — and naming it would
 * be the confident rename this module exists to refuse.
 */
export function neverPickedUp(turn: Turn): boolean {
  if (turn.kind !== 'dispatch') return false;
  return !turn.open && turn.failures > 0 && !turn.types.some((type) => STARTED.test(type));
}

/**
 * Is this call in retry backoff rather than working?
 *
 * TWO INDEPENDENT SIGNALS, BOTH METADATA. `attempt` is Temporal's own counter, carried on the event
 * that says a worker took the work — greater than 1 on a family nothing has closed means the work is
 * between attempts, which is the definition. `RETRY_STATE_IN_PROGRESS` is the second: `history.ts`
 * appends Temporal's `retryState` to the failure line, and that value means the server intends to
 * try again. Either way the answer is "stalled", which is the word that separates it from a
 * **Method** that is simply slow — and from a queue nobody polls, which never started at all.
 */
export function stalledBecause(turn: Turn): string | undefined {
  const state = retryState(turn);
  if (state === 'RETRY_STATE_IN_PROGRESS') return `${state} — Temporal intends to try again`;
  if (turn.open && turn.attempt > 1) return `attempt=${turn.attempt} and nothing has closed it`;
  return undefined;
}

/** Temporal's `retryState`, which `history.ts` appended to the failure line as a bare token. Read
 *  back here because it is the difference between "it will try again" and "this is over". */
export function retryState(turn: Turn): string | undefined {
  return /RETRY_STATE_[A-Z_]+/.exec(turn.error ?? turn.detail)?.[0];
}

/* ───────────────────────────── small things ───────────────────────────── */

function specOf(term: Term, vocab?: Vocabulary): TermSpec {
  const over = vocab?.terms?.[term];
  return over ? { ...TERMS[term], ...over } : TERMS[term];
}

function say(term: Term, because: string, vocab?: Vocabulary): Naming {
  const s = specOf(term, vocab);
  return { term, label: s.label, domain: s.domain, tone: s.tone, untranslated: false, because };
}

/** The event name a worker taking THIS kind of dispatch would have produced — quoted in a `because`
 *  so the absence names the thing that is absent. */
function startedWord(turn: Turn): string {
  return turn.kind === 'dispatch' && turn.endpoint ? 'NexusOperationStarted' : 'ActivityTaskStarted';
}

function offset(t: number): string {
  return `+${round(t)}s`;
}

function round(n: number): number {
  return Math.round(n * 100) / 100;
}

function plural(n: number, word: string): string {
  return `${n} ${word}${n === 1 ? '' : 's'}`;
}
