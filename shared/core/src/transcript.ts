/**
 * A Run's transcript: run events in, ordered turns out.
 *
 * WHY THIS EXISTS. `history.ts` answers "what events did Temporal record", which is the right
 * question for an event log and the wrong one for a run's account of itself. An operator watching
 * a 623-unit sweep does not want 20,000 rows of `NexusOperationScheduled`; they want to read what
 * the run set out to do, what it built, what it called, what it wrote, and how it ended. That is a
 * different shape — turns, not events — and it is the shape every surface of the Workflows page
 * rests on. It lives here, pure, so that every state of it can be asserted without a browser.
 *
 * IT READS THE REDUCED LOG, NOT A RAW HISTORY, and that is the decision the whole module hangs on.
 * The reduced log is what the archive stores (ADR 0025), so a vocabulary that is a pure function
 * over it improves runs that closed weeks ago the moment the function improves. A reader that took
 * raw history could only ever speak about runs Temporal still holds — 24 hours on this controller.
 *
 * PAYLOADS ARE NEVER DECODED, inherited unchanged from `history.ts` (ADR 0007). Every fact below
 * comes from event metadata that was already read once, on the server, into the reduced log: the
 * event type, its category, its timestamps, Temporal's `attempt`, the workflow an event names, and
 * the one-line `detail` built from `activityType`, `taskQueue`, `endpoint` and the failure MESSAGE.
 * Nothing here fetches, and nothing here would fan out into the blob store if it did.
 *
 * WHICH IS WHY SEVERAL FIELDS BELOW ARE OPTIONAL. A Batch's unit counts, a Dataset's name and its
 * row count all travel inside payloads, and the reduced log carries none of them. They are
 * declared, never invented: absent is a fact an operator can act on and `0` is a plausible wrong
 * answer. The route by which they arrive is decided and now runs — a Temporal user-metadata Summary
 * on the scheduled event, ≤200 bytes, metadata rather than payload — and the Method name is the
 * first fact to travel it. See {@link methodOf}.
 *
 * A RUN THAT PRODUCED NOTHING IS NOT A RUN THAT FAILED. They are two different turns — a
 * {@link FinishedTurn} with `empty`, and a {@link FailedTurn} — because they are two different
 * facts, and an operator who cannot tell a clean empty result from a disaster has been told
 * nothing. Nothing in this module ever collapses them.
 *
 * COLLAPSING BELONGS HERE, NOT TO THE RENDERER. A loop over two hundred batches is one line by
 * default and two hundred on demand, and which of those it is must not depend on which component
 * drew it. See {@link collapse}.
 *
 * NOTHING IS DROPPED, AND THE TRANSCRIPT IS NOT THE EVENT LOG. An event this module has no word for
 * becomes a {@link RawTurn} carrying its Temporal type verbatim. A translation layer that guesses is
 * worse than raw events, because it renames confidently — so the untranslated case is a first-class
 * output rather than a hole. What the two-tab split changed is WHERE such a turn is drawn, not
 * whether it exists: when the account and the log shared one pane an untranslated event had nowhere
 * else to go, and now it does. The Transcript draws the turns kontra has a word for; the ones it has
 * no word for are the Event log's, verbatim, and every domain turn still carries the ids of the raw
 * events it folded so a reader can check any translation against them.
 *
 * THAT SPLIT IS MADE BY `vocabulary.ts` AND NOT HERE, because "is there a word for this" is its
 * question. Two of the types this module files as raw — `WorkflowExecutionContinuedAsNew` and
 * `WorkflowTaskTimedOut` — already have domain words over there, and a type a later revision learns
 * a word for has to move panes with no change to this file. Splitting on {@link RawTurn} instead
 * would have frozen the boundary at today's vocabulary and hidden a run's own worker going quiet.
 *
 * THE NAMES ARE STRUCTURAL, NOT THE OPERATOR'S. "fleet requested", "actor activated", "batch
 * dispatched" are a separate function over this output. This module decides WHICH events become a
 * turn and what each turn carries; it does not decide what a turn is called.
 */

import type { EventLink, RunEvent } from './history';

/* ───────────────────────────── what goes in ───────────────────────────── */

/**
 * One event of the reduced log.
 *
 * STRUCTURAL, so both spellings of the reduced log satisfy it: `RunEvent` in `history.ts` and the
 * console's mirror of it in `frontend/src/run/api.ts`. The reader is imported by both sides
 * and must not force either to adopt the other's type.
 */
export interface TranscriptEvent extends RunEvent {
  /**
   * Temporal's user-metadata Summary for this event, when the reduced log carries one.
   *
   * A SIBLING OF THE ATTRIBUTE BAG, not part of it: `userMetadata` hangs off the `HistoryEvent`
   * itself, which is why `history.ts` reads it in its own step rather than out of `attrs`. It is
   * the route for every fact a payload otherwise hides: the Method a dispatch called, the Dataset
   * a publish wrote to, and an author's own sentence. One line each, set at schedule time, and
   * rendered on the bar label in Temporal's own UI — so the two surfaces agree by construction.
   */
  summary?: string;
}

/**
 * The reduced log, as this module needs it. Minimal on purpose: `RunHistory` from either side
 * satisfies it, and a test needs four fields rather than eight.
 */
export interface TranscriptHistory {
  events: readonly TranscriptEvent[];
  /** Events dropped from the MIDDLE of the log to stay under the cap. Carried through rather than
   *  swallowed: a transcript with a hole nobody mentions is worse than no transcript. */
  elided?: number;
  truncated?: boolean;
  /** This log came from the archive rather than from Temporal (ADR 0025). */
  archived?: boolean;
  archivedAt?: number;
}

/**
 * A question a parked run is waiting on — prompt, schema, context.
 *
 * IT DOES NOT COME FROM THE EVENT LOG, and cannot yet: an ask is contextual, it is published by the
 * workflow at park time, and the route that reads one does not exist. The caller passes what it
 * read; this module places it in the transcript at the instant it was asked. When the ask becomes a
 * history event — which is the decided shape, so that an archive never holds an answer without its
 * question — the reader gains a second source and this one stays valid.
 */
export interface Ask {
  id: string;
  /** The sentence the workflow asked. */
  prompt: string;
  /** Epoch ms the workflow parked on it. */
  askedAt: number;
  /** JSON Schema for the answer, rendered by the same form renderer that starts a run. */
  schema?: unknown;
  /** Whatever the workflow attached — the dataset, the counts, the sample. */
  context?: unknown;
  /** Epoch ms the ask expires, when the author declared a deadline. */
  deadlineAt?: number;
  /** Epoch ms it was answered. Absent while it is still pending. */
  answeredAt?: number;
  /**
   * The operator label on the answer.
   *
   * SELF-ASSERTED, NEVER AUTHENTICATION. The appliance is loopback with no credential, so there is
   * no authenticated identity to record. Useful attribution; not proof, and never described as it.
   */
  by?: string;
}

export interface TranscriptOptions {
  /**
   * What the operator submitted, as the form rendered it.
   *
   * SUPPLIED, NEVER DECODED. The run's input is a payload on `WorkflowExecutionStarted` and may be
   * a claim-check `$ref`; the form that collected it already holds the values, so the one place
   * that knows them hands them over rather than the log paying a blob GET to recover them.
   */
  input?: unknown;
  /** The asks this run has published, pending or already answered. */
  asks?: readonly Ask[];
  /** The clock, injected so "parked for 4 minutes" is a testable number. Defaults to the last
   *  instant the log knows about — the most recent thing this reader can honestly say happened. */
  now?: number;
}

/* ───────────────────────────── what comes out ───────────────────────────── */

export type TurnKind =
  | 'started'
  | 'fleet'
  | 'dispatch'
  | 'narration'
  | 'dataset'
  | 'parked'
  | 'finished'
  | 'failed'
  /** An event this module has no word for. Never dropped, never guessed. */
  | 'raw';

/** What every turn carries, whatever it is about. */
export interface TurnBase {
  kind: TurnKind;
  /**
   * The Temporal event ids this turn folded, in order.
   *
   * THE DRILL PATH. A domain turn is never a dead end: these are the rows in the event log it was
   * built from, so the friendly view never costs the real one. Empty only on a turn that came from
   * another authority — a parked ask, which has no event yet.
   */
  events: number[];
  /** The raw Temporal types behind it, deduped, in order. Shown rather than hidden, so an operator
   *  is never confidently told the wrong thing about an event this module misread. */
  types: string[];
  /** Seconds since the run's first event — where the turn opens. */
  t: number;
  /** Seconds since the run's first event of the LAST event folded in. Equals `t` for an instant,
   *  and spans the whole loop on a collapsed group. */
  endT: number;
  /** Epoch ms the turn opened, for a wall clock beside the offset. */
  at: number;
  /** Seconds of work this turn accounts for. Summed across a collapsed group, so 200 dispatches
   *  report the time the loop actually spent rather than the time it spanned. */
  dur: number;
  /** How many turns folded into this one. 1 when it stands alone. */
  count: number;
  /** The folded turns, in order — what "expand" renders. Absent when `count` is 1. */
  folded?: Turn[];
  /** Temporal's attempt counter, the highest folded in. Greater than 1 means something retried. */
  attempt: number;
  /** How many folded turns carry a failure event. 0 is a clean turn — and a clean turn on a run
   *  that produced nothing is still a clean turn. */
  failures: number;
  /** True while nothing has closed this turn. A dispatch that is still open on a finished run is
   *  the interesting case, not an accounting error. */
  open: boolean;
  /** The opening event's own one-line metadata, verbatim from the reduced log. */
  detail: string;
  /** The failure MESSAGE Temporal stringified beside the (possibly claim-checked) payload, when a
   *  failure closed this turn. Kept apart from `detail` so the opener's facts survive the failure. */
  error?: string;
  /** The Summary the SDK or the author set on the opening event, when the log carries one. */
  label?: string;
  /**
   * Seconds this turn spent waiting before anything ran it — Temporal's own schedule-to-start.
   *
   * KEPT OUT OF `dur`, which is the one number an operator reads as "how long did this take". A
   * dispatch that sat two minutes on a queue nobody polled and then ran in 2s is not a two-minute
   * dispatch; it is a 2-second dispatch behind a two-minute queue, and those are different problems
   * with different fixes. Absent until the event that says a worker took it.
   */
  queued?: number;
  /** The workflow this turn is about — what a row drills into. Absent on turns that are about
   *  nothing else, and that absence is what makes a turn navigable or not. */
  link?: EventLink;
}

/** The run started. */
export interface StartedTurn extends TurnBase {
  kind: 'started';
  /** The caller's workflow type, e.g. `NsCheck`. */
  workflowType: string;
  /** The queue the caller's own workflow runs on. */
  queue: string;
  /** Who started it, as Temporal recorded the identity. */
  identity: string;
  /** What the operator submitted, when the caller supplied it ({@link TranscriptOptions.input}). */
  input?: unknown;
}

/** One fleet operation this run started. */
export interface FleetTurn extends TurnBase {
  kind: 'fleet';
  /** The child's workflow id — `kontra-fleet/dns`. Every bring-up AND teardown shares it, which is
   *  why `link.execId` is the thing to drill with. */
  fleet: string;
  state: 'requested' | 'running' | 'ready' | 'failed' | 'cancelled';
  /**
   * WHICH operation this is for that fleet id within this run — 1 for the first, 2 for the second.
   *
   * IT IS HOW A TEARDOWN STOPS READING AS A SECOND BRING-UP. `up`/`preview`/`destroy` rides in the
   * child's INPUT, a payload the event log never decodes, so nothing here can read the operation
   * directly. What it can read is that `fleet.up()` is a context manager: the scope opens with one
   * child and closes with another, in that order, sharing an id. So the second completed operation
   * on one fleet id is the teardown — an INFERENCE FROM SHAPE, not a decoded fact, and it is
   * labelled as the teardown only when it has actually closed.
   *
   * This existed as a real defect: both children rendered `fleet ready · state=ready`, so a run
   * that provisioned two Droplets and destroyed them showed "ready" twice and never showed them
   * going away — the one line an operator paying for machines most needs to see.
   */
  seq?: number;
  /**
   * Machines requested and machines ready.
   *
   * ABSENT, ALWAYS, TODAY. A `StackOp` travels in the child's input — a payload the log never
   * decodes — and the child's own `getProgress` answers a phase and an operation name, with the
   * per-operation change counts deliberately behind the `/api/infra/*` token (`fleetPhase.ts`).
   * Declared because it is what the turn is FOR; absent rather than zero, because a fleet that
   * reported "0 machines ready" while four were booting is the plausible wrong answer.
   */
  machines?: { requested?: number; ready?: number };
}

/** One call to an Actor: a Nexus operation from the caller, or the `RunBatch` behind it. */
export interface DispatchTurn extends TurnBase {
  kind: 'dispatch';
  /** The Actor, as the endpoint or the task queue names it. */
  actor: string;
  /** Its version. `0.1.0`, restored from the endpoint's collapsed spelling where that is the only
   *  source; exact when it came off the task queue. Empty when neither named one. */
  version: string;
  /** The Nexus endpoint — the actor AND its version, exactly as dispatched. */
  endpoint?: string;
  /** The actor's shared task queue, on the paths that name one. */
  queue?: string;
  /**
   * The Method that was called.
   *
   * READ FROM {@link TranscriptEvent.summary}, NEVER FROM THE PAYLOAD. It also lives inside the
   * dispatch payload, which may be a claim-check `$ref` — so the copy this reads is the one on the
   * scheduling event's user metadata, and reading it costs nothing. See {@link methodOf}.
   *
   * ABSENT is an ordinary answer: a run that closed before the Summary existed, a dispatch made by
   * the Go SDK, or a single-Method Actor called without naming one. There is no blank stand-in.
   */
  method?: string;
  /** `queued` until Temporal records a worker taking it: a dispatch that stays queued is a queue
   *  nobody polls, which is the failure that reads identically to a slow run. */
  state: 'queued' | 'running' | 'done' | 'failed' | 'cancelled';
  /**
   * Units in, units out, Units the Actor isolated.
   *
   * ABSENT TODAY, for the same reason as {@link FleetTurn.machines}: a Batch's counts ride on its
   * ref's meta, inside the payload. A dispatch that dropped every Unit and one that legitimately
   * found nothing must not render identically (ADR 0023 §13), so this is left absent until the
   * Summary carries it — never defaulted to 0, which would make exactly that mistake.
   */
  units?: { in?: number; out?: number; dropped?: number };
}

/**
 * An author's own sentence about their own run.
 *
 * THE ONE TURN NOTHING DERIVES. Every other kind here is inferred from event metadata — which
 * Actor, which Method, how many machines, what failed. None of that can say what the run MEANT by
 * any of it, because that is not in the log. A narration is the author putting it there.
 *
 * IT ARRIVES AS A SUMMARY ON A TIMER (`sdk/python/actorkit/narrate.py` writes it as
 * `workflow.sleep(0, summary=…)`), because a timer is the only command a workflow can issue that
 * carries user metadata and runs nothing. That is a writer's detail and this reader does not
 * depend on it: what makes a narration is a Summary on an event that is not about anything else,
 * so a sentence written some other way — another SDK, a marker — reads identically.
 *
 * AN ASK IS NOT A SENTENCE, AND THAT IS THE ONE EXCEPTION THE RULE NEEDS. The ask route writes on a
 * timer too — `wait_condition(timeout=…, timeout_summary='kontra.ask/<id>')` — so the deadline of a
 * park is a Summary on an event about nothing else and matches the rule exactly. It is the ask
 * talking about its own machinery, not an author writing a line, and reading it as one puts a note
 * whose text is an id directly under the {@link ParkedTurn} that already says it properly. See
 * {@link namesAnAsk}.
 */
export interface NarrationTurn extends TurnBase {
  kind: 'narration';
  text: string;
}

/** What a Dataset activity did. `read` is the input side — paging a Dataset into Batches. */
export type DatasetAction = 'read' | 'open' | 'publish' | 'close' | 'promote' | 'tag';

/** One interaction with the lake. */
export interface DatasetTurn extends TurnBase {
  kind: 'dataset';
  action: DatasetAction;
  /**
   * The Dataset's name, and the rows that landed.
   *
   * ABSENT TODAY: both travel in the activity's input and its result. `close` cannot even say
   * whether the Dataset was SEALED or ABANDONED — the state is a payload field — and the difference
   * is precisely how a reader tells "the producer died" from "there was nothing to find". The
   * Summary fixes all three at once.
   */
  dataset?: string;
  rows?: number;
}

/** The run is waiting for a human. */
export interface ParkedTurn extends TurnBase {
  kind: 'parked';
  ask: Ask;
  /** True while nobody has answered. */
  pending: boolean;
  /** Seconds the run has been parked — to the answer, or to now. "Am I the bottleneck?" */
  waited: number;
  /** True when the deadline the author declared has passed unanswered. */
  expired: boolean;
}

/** The run ended, having done what it did. */
export interface FinishedTurn extends TurnBase {
  kind: 'finished';
  outcome: 'completed';
  /**
   * How many Batches this run published, counted from its own dispatch metadata.
   *
   * NOT A ROW COUNT, and never presented as one: rows land in a payload's result. This counts
   * publishes the caller's workflow scheduled, which is the fact the log can actually witness.
   */
  published: number;
  /**
   * This run wrote nothing into the lake.
   *
   * A DIFFERENT FACT FROM FAILING, and the whole reason `finished` and `failed` are two turns. It
   * is a statement about THIS run's history: a run whose output was written by some other workflow
   * would need the materialization dimension (ADR 0017) to say otherwise, and that dimension stays
   * the authority on whether output is queryable. What this says is narrower and true — nothing in
   * this run's account of itself published anything.
   */
  empty: boolean;
}

/** The run ended badly, and where. */
export interface FailedTurn extends TurnBase {
  kind: 'failed';
  /** `cancelled` stays its own word rather than being folded into `failed`: an operator who
   *  cancelled expects this, and one whose run failed is finding out that it did. */
  outcome: 'failed' | 'cancelled';
  /** The failure message Temporal stringified, innermost cause first where there is one. */
  because: string;
  /**
   * The last thing that failed BEFORE the run did — the answer to "where".
   *
   * A workflow's own failure event says the run failed and, at best, repeats the message. What an
   * operator needs is the activity, the dispatch or the child that broke, which is an earlier event
   * with its own metadata and its own drill link. Absent only when nothing failed before the end,
   * which is itself informative: the run failed on its own terms.
   */
  where?: { event: number; t: number; detail: string; link?: EventLink };
}

/**
 * An event with no domain word HERE — rendered as itself.
 *
 * NOT THE SAME AS "NO DOMAIN WORD ANYWHERE", and the difference is which pane draws it.
 * `vocabulary.ts` names two of these types outright and a later revision can name more, so a raw
 * turn is a turn this module declined to interpret rather than a turn nobody can. The ones still
 * unnamed after that pass are the Event log's; the rest stay in the account. Nothing is dropped
 * either way — the type is verbatim, the event ids are on the turn, and both panes read the same
 * reduced log.
 */
export interface RawTurn extends TurnBase {
  kind: 'raw';
  /** The Temporal type, verbatim. */
  type: string;
  /**
   * Temporal's own bookkeeping rather than anything the run did.
   *
   * A 305-event run is roughly half workflow tasks; a surface that showed them by default would
   * bury the run in its own scheduler. They are MARKED rather than dropped, because a workflow task
   * that TIMED OUT is one of the three failures that look identical from outside — and a failure is
   * never plumbing, whatever layer produced it.
   */
  plumbing: boolean;
}

export type Turn =
  | StartedTurn
  | FleetTurn
  | DispatchTurn
  | NarrationTurn
  | DatasetTurn
  | ParkedTurn
  | FinishedTurn
  | FailedTurn
  | RawTurn;

/** A run's whole account of itself. */
export interface Transcript {
  turns: Turn[];
  /**
   * The run's story can still grow.
   *
   * TERMINAL EVENT OR NOTHING. A run is live until its history records it closing — not until a
   * clock says so, and not because a poll came back unchanged. A log we could not read at all reads
   * as live, which is the safe direction: labelling a running run "finished" stops an operator
   * watching it.
   */
  live: boolean;
  /** How it ended, when it has. Absent while it is live. */
  outcome?: 'completed' | 'failed' | 'cancelled';
  /** The run handed over to a fresh execution. NOT an ending: each leg of the chain closes as it
   *  hands over, and the run is still running. */
  continued: boolean;
  /** Batches published. See {@link FinishedTurn.published}. */
  published: number;
  /** Nothing was published. Independent of `outcome`, because empty and failed are two facts. */
  empty: boolean;
  /** Events the reduced log dropped from its middle. Printed, never swallowed. */
  elided: number;
  truncated: boolean;
  /** This account came from the archive, not from Temporal. */
  archived: boolean;
  archivedAt?: number;
  /** Seconds from the first event read to the last. */
  span: number;
}

/* ───────────────────────────── the vocabulary of shapes ───────────────────────────── */

/**
 * The workflow type a Fleet operation runs under.
 *
 * THE ONLY SPELLING OF IT THE CONSOLE READS, since the browser-side copy in
 * `frontend/src/run/fleetWindow.ts` went with the page that was its only caller. `fleetPhase.test.ts`
 * pins the name against `workflows/stack.ts` itself, which is the assertion that makes one spelling
 * enough. A rename over there draws no fleet turn rather than a wrong one: the operation still
 * appears, as a child turn with its Temporal type on it.
 */
export const FLEET_WORKFLOW_TYPE = 'stackWorkflow';

/** The child-event types that OPEN a window on another workflow. `Initiated` is the parent asking;
 *  `Started` is Temporal answering with an execution, which is the event that pins it. */
const CHILD_OPENS = /^(StartChildWorkflowExecutionInitiated|ChildWorkflowExecutionStarted)$/;

/** The child-event types that CLOSE one, and what each says happened. `StartChildWorkflowExecution
 *  Failed` is here because a child that never started is a window that ended — the run waited for
 *  it and got nothing. Same table as the console's, deliberately: a fleet operation must not end in
 *  one word here and another there. */
const CHILD_CLOSES: Record<string, 'ready' | 'failed' | 'cancelled'> = {
  ChildWorkflowExecutionCompleted: 'ready',
  ChildWorkflowExecutionFailed: 'failed',
  ChildWorkflowExecutionTimedOut: 'failed',
  ChildWorkflowExecutionTerminated: 'failed',
  ChildWorkflowExecutionCanceled: 'cancelled',
  ChildWorkflowExecutionCancelled: 'cancelled',
  StartChildWorkflowExecutionFailed: 'failed',
};

/**
 * The activity names the SDK schedules against the lake, and what each one is doing.
 *
 * THE SIXTH SPELLING OF EACH, and the failure mode is the house one: a rename in
 * `sdk/python/actorkit/catalog.py` makes the turn read as a raw activity rather than as a wrong
 * one. `datasetState` is deliberately NOT here — reading a Dataset's state is bookkeeping the
 * workflow does, not a thing that happened to the Dataset.
 */
const DATASET_ACTIVITIES: Record<string, DatasetAction> = {
  pageDataset: 'read',
  openTempDataset: 'open',
  publishBatch: 'publish',
  closeDataset: 'close',
  promoteDataset: 'promote',
  tagDataset: 'tag',
};

/** The activity a Method call runs as, once a dispatch reaches the Actor's own workflow. Drilling
 *  into a dispatch lands on that workflow, and this is what its history is made of. */
const DISPATCH_ACTIVITY = 'RunBatch';

/** Activities that move refs around rather than doing anything an operator asked for. Marked
 *  plumbing so the default reading stays calm; never dropped. */
const PLUMBING_ACTIVITIES = new Set(['resolveBatch', 'splitBatch', 'kontra.fetch_blob', 'datasetState']);

/**
 * How the ask route spells ITSELF in the reduced log, in the two places it reaches one.
 *
 * BOTH ARE THE ASK TALKING ABOUT ITS OWN MACHINERY, and neither is an author's sentence. A park is
 * `upsert_memo({'kontra.ask.<id>': …})` (`sdk/python/actorkit/hitl.py`), which is one
 * `WorkflowPropertiesModified` whose `detail` names the memo KEY and never its value (`history.ts`);
 * and where the author declared a deadline, the wait that follows carries
 * `timeout_summary='kontra.ask/<id>'` so Temporal's own UI labels the timer bar with the question it
 * belongs to rather than with a sentence.
 *
 * CROSS-LANGUAGE LITERALS, spelled here rather than imported. This module is pure and is bundled
 * into the browser; `hitl.ts`, which exports the same memo prefix for the server, reaches for
 * `NodeJS.ProcessEnv`. The house rule for that is to write the literal on each side and pin it with
 * a test, which `transcript.test.ts` does against both `hitl.ts` and the Python emitter's own bytes
 * — so a rename on either side is a failure rather than an ask that quietly reads as a note again.
 */
const ASK_MEMO_PREFIX = 'kontra.ask.';
const ASK_TIMER_PREFIX = 'kontra.ask/';

/**
 * Is this event the ask route talking about itself?
 *
 * IT GUARDS THE NARRATION RULE, whose whole content is "a Summary on an event that is not about
 * anything else". A park's deadline timer is exactly that shape, so without this it becomes a
 * {@link NarrationTurn} whose text is `kontra.ask/approve-1` — drawn under the {@link ParkedTurn}
 * that already renders the prompt, the schema and the wait. Two rows for one question, one of them
 * an id, and the author's real `speak` line sits between them and gets missed.
 *
 * THE MEMO IS CHECKED AS WELL AS THE SUMMARY, and today only the summary can fire: Temporal's SDKs
 * attach user metadata to timers, activities and children, and not to the `ModifyWorkflowProperties`
 * command a memo upsert becomes. It is here because the RULE is about what an event is about, not
 * about which SDK wrote it — an event that files an ask under `kontra.ask.<id>` IS that ask, and a
 * writer that one day puts a Summary beside the memo must not reintroduce the note.
 */
function namesAnAsk(e: TranscriptEvent, meta: Record<string, string>): boolean {
  if (e.summary?.startsWith(ASK_TIMER_PREFIX)) return true;
  // `history.ts` joins a memo's keys with a comma, sorted and capped; the values are payloads it
  // never touches, so a key is all there is to match on and all that is needed.
  return (meta.memo ?? '').split(',').some((key) => key.startsWith(ASK_MEMO_PREFIX));
}

/** Temporal's own scheduler bookkeeping. Three of these per turn of any workflow loop. */
const PLUMBING_TYPES = new Set([
  'WorkflowTaskScheduled',
  'WorkflowTaskStarted',
  'WorkflowTaskCompleted',
  'UpsertWorkflowSearchAttributes',
]);

/** How a run's own closing event ends it. Timed-out and terminated are failures in the only sense
 *  that matters here: the output is missing. The precise Temporal word survives on `types`. */
const RUN_CLOSES: Record<string, 'completed' | 'failed' | 'cancelled'> = {
  WorkflowExecutionCompleted: 'completed',
  WorkflowExecutionFailed: 'failed',
  WorkflowExecutionTimedOut: 'failed',
  WorkflowExecutionTerminated: 'failed',
  WorkflowExecutionCanceled: 'cancelled',
  WorkflowExecutionCancelled: 'cancelled',
};

/* ───────────────────────────── reading the metadata back ───────────────────────────── */

/**
 * The `key=value` pairs `history.ts` joined into one line.
 *
 * PARSING A DISPLAY STRING IS NOT WHERE THIS WANTS TO BE, and it is where it has to be: the reduced
 * log flattens `activityType`, `taskQueue` and `endpoint` into `detail` and keeps no structured
 * copy, and the archive stores exactly that. A reader that demanded a structured bag would speak
 * only about runs archived after the change. Unknown keys are ignored, a failure message that
 * happens to contain `=` is not a key (the guard is on the key's own shape), and a `detail` that is
 * just the event type yields nothing rather than a guess.
 */
export function metaOf(detail: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const part of detail.split(' · ')) {
    const eq = part.indexOf('=');
    if (eq <= 0) continue;
    const key = part.slice(0, eq);
    if (!/^[a-z][A-Za-z0-9]*$/.test(key)) continue;
    out[key] = part.slice(eq + 1);
  }
  return out;
}

/**
 * The Actor and version behind a Nexus endpoint — `kontra-nscheck-0-1-0` → `nscheck`, `0.1.0`.
 *
 * BEST-EFFORT, AND SAYS SO. `endpointName` collapses every non-alphanumeric to `-`
 * (`nexusRegistry.ts`), so the dots of a version and the dashes of an Actor's own name arrive
 * indistinguishable; `dnsfacts-0.1.0` and `dns-facts-0.1.0` both land as dashes. Three trailing
 * numeric segments are read as the version because that is what a version is here, and an endpoint
 * that does not end in one yields the whole stem as the name rather than a guessed split. The raw
 * endpoint is carried on the turn either way, so nothing is lost to this heuristic.
 */
export function actorOf(endpoint: string): { actor: string; version: string } {
  const stem = endpoint.startsWith('kontra-') ? endpoint.slice(7) : endpoint;
  const m = /^(.+)-(\d+)-(\d+)-(\d+)((?:-[0-9A-Za-z]+)*)$/.exec(stem);
  if (!m) return { actor: stem, version: '' };
  return { actor: m[1]!, version: `${m[2]}.${m[3]}.${m[4]}${m[5] ?? ''}` };
}

/**
 * The Actor and version behind a task queue — `nscheck-0.1.0` → `nscheck`, `0.1.0`.
 *
 * EXACT, unlike {@link actorOf}: the shared queue is `<name>-<version>` with the version's dots
 * intact (`sharedQueue`), so the version anchors the split rather than being guessed at.
 */
export function actorOfQueue(queue: string): { actor: string; version: string } {
  const m = /^(.+)-(\d+\.\d+\.\d+.*)$/.exec(queue);
  if (!m) return { actor: queue, version: '' };
  return { actor: m[1]!, version: m[2]! };
}

/** What a Summary joins its fields with. The writer is `SUMMARY_SEP` in
 *  `sdk/python/actorkit/catalog.py`; the same separator the reduced log already puts between the
 *  `key=value` pairs of a `detail` line. */
const SUMMARY_SEP = ' · ';

/**
 * What a Method name may look like, and the whole guard against inventing one.
 *
 * An identifier, because that is what a Method is — `crawl`, `extract_links`, and `.`/`-` allowed
 * for the callers that reach one by string. What matters is what it EXCLUDES: no `@`, no space, no
 * `…`. An Actor's field always carries the `@` that separates it from its version (`echo@` even
 * with no version, deliberately — see `dispatch_summary`), a count always carries a space, and any
 * field the writer had to shorten always carries the ellipsis it was shortened with. So no field
 * of a Summary that is not a Method name can pass this, and the one thing this reader must never
 * do — put a confident wrong name on a dispatch — it cannot do.
 */
const METHOD_NAME = /^[A-Za-z_][A-Za-z0-9_.-]{0,63}$/;

/**
 * The Method a dispatch called, off its Summary — `crawl · crawler@0.1.0[acme.com] · 12 units`.
 *
 * THE FIRST FIELD, because the Method is the one fact in that line the rest of the log cannot
 * recover: the Actor and its version are in the Nexus endpoint and exactly in the task queue, and
 * the unit count is at least plausible elsewhere. The writer pays it first out of the 200-byte
 * budget for the same reason (`dispatch_summary`).
 *
 * NOTHING IS RETURNED FOR A LINE THIS DOES NOT RECOGNISE, and that case is the common one rather
 * than the exotic one: every run archived before the Summary existed, every dispatch made by the
 * Go SDK, and every single-Method Actor called without naming one. Those dispatches read exactly
 * as they do today — an Actor, a version, a state, no Method — while the raw line survives on
 * {@link TurnBase.label} for anyone who wants to see what was actually written.
 */
export function methodOf(summary: string | undefined): string | undefined {
  if (!summary) return undefined;
  const cut = summary.indexOf(SUMMARY_SEP);
  const head = cut < 0 ? summary : summary.slice(0, cut);
  return METHOD_NAME.test(head) ? head : undefined;
}

/* ───────────────────────────── the reader ───────────────────────────── */

/**
 * One run's reduced log, read as turns.
 *
 * Pure. No DOM, no fetch, no clock it was not handed. The entire behaviour — which events become a
 * turn, how a three-event family becomes one, how a loop collapses, what "live" means — is
 * assertable against literal objects.
 */
export function readTranscript(
  history: TranscriptHistory,
  options: TranscriptOptions = {}
): Transcript {
  const events = history.events;
  const first = events.length > 0 ? events[0]!.at : 0;
  const last = events.length > 0 ? events[events.length - 1]! : undefined;
  const now = options.now ?? last?.at ?? first;

  const built = readEvents(events, options);
  const merged = mergeAsks(built.turns, options.asks ?? [], first, now);
  const turns = collapse(merged);

  return {
    turns,
    live: built.outcome === undefined,
    ...(built.outcome ? { outcome: built.outcome } : {}),
    continued: built.continued,
    published: built.published,
    empty: built.published === 0,
    elided: history.elided ?? 0,
    truncated: history.truncated ?? false,
    archived: history.archived ?? false,
    ...(history.archivedAt !== undefined ? { archivedAt: history.archivedAt } : {}),
    span: last ? last.t : 0,
  };
}

/** Every turn folded into one, or the turn itself. What "expand" renders, and the only place a
 *  caller should reach for a collapsed group's members. */
export function expand(turn: Turn): Turn[] {
  return turn.folded ?? [turn];
}

/**
 * One open family of events: an activity, a dispatch or a child, from its opener to its close.
 *
 * `at` IS THE JOIN KEY, and it is arithmetic rather than a heuristic. The reduced log drops
 * Temporal's `scheduledEventId`/`startedEventId` back-references, but it keeps `dur`, which
 * `history.ts` computed as the closing event's distance from exactly that opener. So
 * `close.at - close.dur × 1000` IS the opener's `at`, to the millisecond, and a family is rejoined
 * without the field that was dropped. Where two families of the same shape opened in the same
 * millisecond the older one is taken first — the only ambiguity, and it costs a duration attributed
 * to a sibling that did the same thing on the same queue.
 */
interface Family {
  turn: Turn;
  at: number;
}

interface Built {
  turns: Turn[];
  outcome?: 'completed' | 'failed' | 'cancelled';
  continued: boolean;
  published: number;
}

function readEvents(events: readonly TranscriptEvent[], options: TranscriptOptions): Built {
  const turns: Turn[] = [];
  /** Families keyed by the workflow id they are about — exact, for children and dispatches. */
  const byLink = new Map<string, Family>();
  /** How many fleet operations this run has opened per fleet id — see {@link FleetTurn.seq}. */
  const fleetSeq = new Map<string, number>();
  /** Families keyed by the epoch ms of their most recent event — for everything else. */
  const byAt = new Map<number, Family[]>();
  /**
   * Narration families, in their OWN index rather than in `byAt`.
   *
   * BECAUSE `joinAt` CONSUMES WHAT IT FINDS. A shared index would let an activity that completed
   * in the same millisecond a sentence was written take the sentence's slot — and, worse, let a
   * `TimerFired` take the activity's. Two indexes cannot cross. Within this one the ambiguity is
   * the same as `byAt`'s and costs the same: two timers started in the same millisecond, one of
   * them a sentence, and the sentence folds in the other one's `TimerFired` — an event id on the
   * drill path, and nothing an operator reads.
   */
  const said = new Map<number, Family[]>();
  /** The last thing that failed, which is what a failed run's `where` points at. */
  let lastFailure: FailedTurn['where'];
  let outcome: Built['outcome'];
  let continued = false;
  let published = 0;

  const openIn = (index: Map<number, Family[]>, family: Family): void => {
    const bucket = index.get(family.at);
    if (bucket) bucket.push(family);
    else index.set(family.at, [family]);
  };
  /** The family this closing event closes, by the arithmetic above. */
  const joinIn = (index: Map<number, Family[]>, e: TranscriptEvent): Family | undefined => {
    const openedAt = Math.round(e.at - e.dur * 1000);
    const bucket = index.get(openedAt);
    if (!bucket || bucket.length === 0) return undefined;
    const family = bucket.shift()!;
    if (bucket.length === 0) index.delete(openedAt);
    return family;
  };
  const openAt = (family: Family): void => openIn(byAt, family);
  const joinAt = (e: TranscriptEvent): Family | undefined => joinIn(byAt, e);

  for (const e of events) {
    const meta = metaOf(e.detail);
    if (e.cat === 'failure' && !RUN_CLOSES[e.type]) {
      lastFailure = { event: e.id, t: e.t, detail: e.detail, ...(e.link ? { link: e.link } : {}) };
    }

    // ── the run's own lifecycle ──
    if (e.type === 'WorkflowExecutionStarted') {
      turns.push({
        ...base(e, 'started'),
        workflowType: meta.workflowType ?? '',
        queue: meta.taskQueue ?? '',
        identity: meta.identity ?? '',
        ...(options.input !== undefined ? { input: options.input } : {}),
      } as StartedTurn);
      continue;
    }
    if (e.type === 'WorkflowExecutionContinuedAsNew') {
      // NOT AN ENDING. Each leg of a continued chain closes as it hands over, and reading that as
      // the run finishing would report a sweep that is still running as done.
      continued = true;
      turns.push(rawTurn(e));
      continue;
    }
    const closes = RUN_CLOSES[e.type];
    if (closes) {
      outcome = closes;
      if (closes === 'completed') {
        turns.push({
          ...base(e, 'finished'),
          outcome: 'completed',
          published,
          empty: published === 0,
        } as FinishedTurn);
      } else {
        turns.push({
          ...base(e, 'failed'),
          outcome: closes,
          because: e.detail,
          ...(lastFailure ? { where: lastFailure } : {}),
        } as FailedTurn);
      }
      continue;
    }

    // ── a child: a fleet operation, or another workflow this run started ──
    if (e.type.startsWith('ChildWorkflowExecution') || e.type.startsWith('StartChildWorkflowExecution')) {
      const id = e.link?.workflowId;
      const open = id ? byLink.get(`child:${id}`) : undefined;
      const ends = CHILD_CLOSES[e.type];
      if (ends === undefined) {
        if (!CHILD_OPENS.test(e.type)) {
          turns.push(rawTurn(e));
          continue;
        }
        if (open) {
          // `ChildWorkflowExecutionStarted` following the `Initiated` that opened this turn: the
          // one event that names the execution, and the moment the child becomes safe to ask.
          attach(open, e);
          if (open.turn.kind === 'fleet') open.turn.state = 'running';
          continue;
        }
        const turn: Turn =
          e.link?.type === FLEET_WORKFLOW_TYPE
            ? ({
                ...base(e, 'fleet'),
                fleet: e.link.workflowId,
                state: e.link.execId ? 'running' : 'requested',
                seq: (fleetSeq.set(e.link.workflowId, (fleetSeq.get(e.link.workflowId) ?? 0) + 1),
                  fleetSeq.get(e.link.workflowId)),
              } as FleetTurn)
            : rawTurn(e);
        turns.push(turn);
        if (id) byLink.set(`child:${id}`, { turn, at: e.at });
        continue;
      }
      const family = open ?? { turn: closeOnlyChild(e), at: e.at };
      if (!open) {
        // A close with no opener in the events we read: the middle of the log was elided. The
        // operation is real and its duration is recorded; its start is not, and is not back-dated.
        // It is still a FLEET operation — the closing event names the workflow type too, and
        // demoting it to a raw row would lose the one window an operator most wants to see.
        turns.push(family.turn);
      } else {
        attach(family, e);
      }
      if (family.turn.kind === 'fleet') family.turn.state = ends;
      family.turn.open = false;
      if (id) byLink.delete(`child:${id}`);
      continue;
    }

    // ── a dispatch: a Nexus operation the caller scheduled against an Actor's endpoint ──
    if (e.type.startsWith('NexusOperation')) {
      const key = e.link ? `nexus:${e.link.workflowId}` : undefined;
      // AN OPENER NEVER ASKS THE JOIN. Its `dur` is 0, so the arithmetic would look up its own
      // instant — and `joinAt` CONSUMES what it finds, so a dispatch scheduled in the same
      // millisecond as some other family closed would silently take that family's close.
      const opener = e.type === 'NexusOperationScheduled';
      const open = opener ? undefined : key ? byLink.get(key) : joinAt(e);
      if (opener || !open) {
        const endpoint = meta.endpoint ?? '';
        const { actor, version } = actorOf(endpoint);
        const turn: DispatchTurn = {
          ...base(e, 'dispatch'),
          actor,
          version,
          ...(endpoint ? { endpoint } : {}),
          ...namedMethod(e),
          state: 'queued',
        } as DispatchTurn;
        if (e.type !== 'NexusOperationScheduled') applyDispatchClose(turn, e);
        turns.push(turn);
        const family = { turn: turn as Turn, at: e.at };
        if (key) byLink.set(key, family);
        else openAt(family);
        continue;
      }
      // A cancel REQUEST is an operator asking; the operation runs until Temporal says otherwise.
      const ends = e.type !== 'NexusOperationStarted' && !/CancelRequested$/.test(e.type);
      attach(open, e);
      if (open.turn.kind === 'dispatch') {
        if (e.type === 'NexusOperationStarted') open.turn.state = 'running';
        else applyDispatchClose(open.turn, e);
      }
      if (!ends) {
        // STILL OPEN, so it has to stay findable. A family keyed by its operation token is still in
        // `byLink`; one without a token was just CONSUMED out of the timestamp index by the join
        // that found it, and must be put back under the instant its close will measure from.
        if (!key) openAt(open);
      } else if (key) {
        byLink.delete(key);
      }
      continue;
    }

    // ── an activity: the lake, a Method call, or plumbing ──
    if (e.type.startsWith('ActivityTask')) {
      if (e.type === 'ActivityTaskScheduled') {
        const activity = meta.activityType ?? '';
        const turn = activityTurn(e, meta, activity);
        if (turn.kind === 'dataset' && (turn.action === 'publish' || turn.action === 'promote')) {
          published += 1;
        }
        turns.push(turn);
        openAt({ turn, at: e.at });
        continue;
      }
      const open = joinAt(e);
      if (!open) {
        // Its opener was elided, or two of a kind opened in the same millisecond and the other one
        // took the join. Either way the event is real: it is shown as itself rather than dropped.
        turns.push(rawTurn(e));
        continue;
      }
      attach(open, e);
      if (e.type === 'ActivityTaskStarted') {
        if (open.turn.kind === 'dispatch') open.turn.state = 'running';
        openAt(open);
        continue;
      }
      if (e.type === 'ActivityTaskCancelRequested') {
        // AN OPERATOR ASKING IS NOT AN ENDING. The activity is still running until Temporal says
        // otherwise, and the family stays open so its real close still finds it.
        openAt(open);
        continue;
      }
      open.turn.open = false;
      if (open.turn.kind === 'dispatch') applyDispatchClose(open.turn, e);
      continue;
    }

    // ── a timer: an author's own sentence, or a wait the author asked for ──
    if (e.type.startsWith('Timer')) {
      // AN ASK'S DEADLINE IS NOT AN AUTHOR'S SENTENCE, even though it is written the same way. It
      // falls through to a raw turn: the park itself is already in the account, from the authority
      // that holds the prompt, and the timer under it is machinery an operator reads in the log.
      if (e.type === 'TimerStarted' && e.summary && !namesAnAsk(e, meta)) {
        const turn = sentence(e);
        turns.push(turn);
        openIn(said, { turn, at: e.at });
        continue;
      }
      // The `TimerFired` that closes a sentence carries no metadata of its own and is nothing an
      // operator needs a row for — but it is a real event, and it belongs to the turn as part of
      // its drill path rather than beside it as a second line saying nothing.
      const spoken = e.type === 'TimerFired' ? joinIn(said, e) : undefined;
      if (spoken) attach(spoken, e);
      // A TIMER THAT SAID NOTHING IS A WAIT, and it reads as itself. Narration is the one thing
      // this branch translates; an author who slept for four hours did that on purpose.
      else turns.push(rawTurn(e));
      continue;
    }

    // ── an author's own sentence, written some other way ──
    //
    // A SUMMARY ON AN EVENT THAT IS ABOUT NOTHING ELSE. Everything with a place to put one has
    // already taken it above — a dispatch's Method, a Dataset's name — so what is left is a
    // sentence somebody wrote. Kept general on purpose: the timer above is how this SDK writes
    // one today, and a second writer must not need a second branch here to be read. The one thing
    // this must not sweep up is the ask, which is ABOUT something else — see `namesAnAsk`.
    if (e.summary && !namesAnAsk(e, meta)) {
      turns.push(sentence(e));
      continue;
    }

    turns.push(rawTurn(e));
  }

  return { turns, ...(outcome ? { outcome } : {}), continued, published };
}

/** The activity turn one `ActivityTaskScheduled` opens — the lake, an Actor, or plumbing. */
function activityTurn(
  e: TranscriptEvent,
  meta: Record<string, string>,
  activity: string
): Turn {
  const action = DATASET_ACTIVITIES[activity];
  if (action) {
    return {
      ...base(e, 'dataset'),
      action,
      ...(e.summary ? { dataset: e.summary } : {}),
    } as DatasetTurn;
  }
  if (activity === DISPATCH_ACTIVITY) {
    const queue = meta.taskQueue ?? '';
    const { actor, version } = actorOfQueue(queue);
    return {
      ...base(e, 'dispatch'),
      actor,
      version,
      ...(queue ? { queue } : {}),
      ...namedMethod(e),
      state: 'queued',
    } as DispatchTurn;
  }
  const turn = rawTurn(e);
  if (PLUMBING_ACTIVITIES.has(activity)) turn.plumbing = true;
  return turn;
}

/**
 * One event carrying a Summary that is about nothing else, read as the sentence somebody wrote.
 *
 * `open` IS FORCED SHUT, and that is not a detail. The event this arrives on is `TimerStarted`,
 * which ends in `Started` — so the default would leave every sentence in the transcript
 * permanently open, and a run whose author narrated would read as a run with unfinished business
 * in it. A sentence is written and finished in the same breath; the timer under it exists only to
 * carry the bytes, and it is zero-duration by construction.
 */
function sentence(e: TranscriptEvent): NarrationTurn {
  return { ...base(e, 'narration'), open: false, text: e.summary ?? '' } as NarrationTurn;
}

/** `{ method }` when the event's Summary names one, `{}` when it does not — so a dispatch with no
 *  Method name carries no key at all rather than an empty string pretending to be one. */
function namedMethod(e: TranscriptEvent): { method?: string } {
  const name = methodOf(e.summary);
  return name ? { method: name } : {};
}

/** What a closing event says about a dispatch. `CancelRequested` is deliberately not a close: it is
 *  an operator asking, and the operation is still open until Temporal says otherwise. */
function applyDispatchClose(turn: DispatchTurn, e: TranscriptEvent): void {
  if (/CancelRequested$/.test(e.type)) return;
  if (/Canceled$|Cancelled$/.test(e.type)) turn.state = 'cancelled';
  else if (e.cat === 'failure') turn.state = 'failed';
  else if (/Completed$/.test(e.type)) turn.state = 'done';
  if (e.type !== 'NexusOperationStarted') turn.open = false;
}

/**
 * Fold one more event of the same family into the turn it belongs to.
 *
 * THE TWO DURATIONS ARE KEPT APART. Every family's middle event — `ActivityTaskStarted`,
 * `NexusOperationStarted`, `ChildWorkflowExecutionStarted` — measures back to the event that
 * SCHEDULED it, so its `dur` is time on a queue and not time doing anything. Adding it to `dur`
 * would report a dispatch that waited two minutes for a worker as a two-minute dispatch; it becomes
 * {@link TurnBase.queued} instead, and `dur` stays the closing event's own recorded duration — the
 * same 156.31 seconds the console's fleet window prints for the same child.
 */
function attach(family: Family, e: TranscriptEvent): void {
  const turn = family.turn;
  turn.events.push(e.id);
  if (!turn.types.includes(e.type)) turn.types.push(e.type);
  turn.endT = Math.max(turn.endT, e.t);
  turn.attempt = Math.max(turn.attempt, e.attempt);
  if (/Started$/.test(e.type)) {
    if (e.dur > 0) turn.queued = e.dur;
  } else if (e.dur > turn.dur) {
    turn.dur = e.dur;
  }
  if (e.cat === 'failure') {
    turn.failures += 1;
    turn.error = e.detail;
  }
  // The link arrives on exactly one event of a family — the one that names the execution — and it
  // is what every row of the family drills into.
  if (!turn.link && e.link) turn.link = e.link;
  else if (turn.link && e.link?.execId && !turn.link.execId) turn.link = e.link;
  if (!turn.label && e.summary) turn.label = e.summary;
  family.at = e.at;
}

/** The fields every turn has, off one event. */
function base(e: TranscriptEvent, kind: TurnKind): TurnBase {
  return {
    kind,
    events: [e.id],
    types: [e.type],
    t: e.t,
    endT: e.t,
    at: e.at,
    dur: e.dur,
    count: 1,
    attempt: e.attempt,
    failures: e.cat === 'failure' ? 1 : 0,
    open: opensSomething(e.type),
    detail: e.detail,
    ...(e.cat === 'failure' ? { error: e.detail } : {}),
    ...(e.summary ? { label: e.summary } : {}),
    ...(e.link ? { link: e.link } : {}),
  };
}

/** Does this event start something that has to close? Everything else is an instant. */
function opensSomething(type: string): boolean {
  return /(Scheduled|Initiated|Started)$/.test(type) && !type.startsWith('WorkflowExecution');
}

/** The turn a child's CLOSING event opens when its opener was elided. Same test as the opener's:
 *  the workflow type is on the closing event too, so a fleet operation stays one. */
function closeOnlyChild(e: TranscriptEvent): Turn {
  if (e.link?.type !== FLEET_WORKFLOW_TYPE) return rawTurn(e);
  return { ...base(e, 'fleet'), fleet: e.link.workflowId, state: 'running' } as FleetTurn;
}

function rawTurn(e: TranscriptEvent): RawTurn {
  return {
    ...base(e, 'raw'),
    kind: 'raw',
    type: e.type,
    // A FAILURE IS NEVER PLUMBING, whatever layer produced it. A workflow task that timed out is
    // one of the three things that look identical from outside, and hiding it under Temporal's
    // bookkeeping is exactly how it stayed invisible.
    plumbing: PLUMBING_TYPES.has(e.type) && e.cat !== 'failure',
  } as RawTurn;
}

/* ───────────────────────────── the asks ───────────────────────────── */

/**
 * Place each ask in the transcript at the instant the workflow parked on it.
 *
 * MERGED BY TIME, not appended: a question asked in the middle of a run belongs in the middle of
 * its account. An ask that predates the first event we read sorts to the front rather than being
 * dropped — the events may have been elided, the ask was still asked.
 */
function mergeAsks(turns: Turn[], asks: readonly Ask[], first: number, now: number): Turn[] {
  if (asks.length === 0) return turns;
  const parked: ParkedTurn[] = asks.map((ask) => {
    const until = ask.answeredAt ?? now;
    return {
      kind: 'parked',
      events: [],
      types: [],
      t: first > 0 ? Math.max(0, (ask.askedAt - first) / 1000) : 0,
      endT: first > 0 ? Math.max(0, (until - first) / 1000) : 0,
      at: ask.askedAt,
      dur: Math.max(0, (until - ask.askedAt) / 1000),
      count: 1,
      attempt: 1,
      failures: 0,
      open: ask.answeredAt === undefined,
      detail: ask.prompt,
      ask,
      pending: ask.answeredAt === undefined,
      waited: Math.max(0, (until - ask.askedAt) / 1000),
      // An expired ask raises inside the workflow, and what that means is the author's decision —
      // so this reports the deadline passing and never a verdict about it.
      expired:
        ask.deadlineAt !== undefined && ask.answeredAt === undefined && now > ask.deadlineAt,
    };
  });

  const out = [...turns, ...parked];
  // Stable by construction: `Array.prototype.sort` is stable, so turns sharing an instant keep the
  // order they were built in — which for event turns is Temporal's own monotonic event order.
  return out.sort((a, b) => a.at - b.at);
}

/* ───────────────────────────── collapsing ───────────────────────────── */

/**
 * Fold a repeated call into one line.
 *
 * WHAT "CONSECUTIVE" HAS TO MEAN HERE. A real loop is not a run of identical turns: `dnssweep`
 * pages a Dataset, dispatches to one Actor, re-pages, dispatches to another, publishes, and goes
 * round again. Two hundred iterations of that interleave four kinds of turn, so folding only
 * strictly-adjacent turns would fold nothing at all — the rule would pass a test built from a run
 * of identical events and do nothing whatsoever to the run it exists for.
 *
 * So a group is every turn sharing one key within a BARRIER-FREE REGION, anchored at its first
 * member. Barriers are the turns that mean the run moved on: it started, it built a fleet, it asked
 * a human, an author said something, a Dataset was opened or sealed or promoted or tagged, it
 * ended. Between two barriers, the same call however many times is one line; across one, it is two
 * lines, because something happened in between that an operator was meant to read.
 *
 * FAILURE IS NOT A BARRIER, deliberately. A sweep where unit 37 of 200 failed must not shatter into
 * two hundred lines to say so — the group carries `failures`, and expanding finds the one that
 * broke. Blowing the transcript apart is how a single bad Unit becomes unreadable.
 */
function collapse(turns: Turn[]): Turn[] {
  const out: Turn[] = [];
  /** key → the members gathered under it in the region we are in. */
  let region = new Map<string, Turn[]>();
  const heads: Array<{ key: string; index: number }> = [];

  const flush = (): void => {
    for (const { key, index } of heads) {
      const members = region.get(key);
      if (!members || members.length < 2) continue;
      out[index] = fold(members);
    }
    heads.length = 0;
    region = new Map();
  };

  for (const turn of turns) {
    const key = collapseKey(turn);
    if (key === undefined) {
      flush();
      out.push(turn);
      continue;
    }
    const members = region.get(key);
    if (members) {
      members.push(turn);
      continue;
    }
    region.set(key, [turn]);
    heads.push({ key, index: out.length });
    out.push(turn);
  }
  flush();
  return out;
}

/** What folds with what, or `undefined` for a turn that is a barrier and stands alone. */
function collapseKey(turn: Turn): string | undefined {
  switch (turn.kind) {
    case 'dispatch':
      // THE METHOD IS THE IDENTITY, so two Methods on one Actor are two groups wherever the
      // Summary names them. Where it does not — an older run, a Go dispatch — the endpoint is the
      // closest thing there is and both fold together. Under-separating is the safe direction: the
      // group is expandable, and every member keeps its own events.
      return `dispatch:${turn.endpoint ?? turn.queue ?? turn.actor}:${turn.method ?? ''}`;
    case 'dataset':
      // Accrual folds; lifecycle does not. Two hundred publishes are one line, but the seal that
      // ends them is a milestone an operator reads, and so is the open that began them.
      return turn.action === 'read' || turn.action === 'publish'
        ? `dataset:${turn.action}:${turn.dataset ?? ''}`
        : undefined;
    case 'raw':
      // A RAW TURN THAT CARRIES A SUMMARY NEVER FOLDS — the same rule as a narration, three cases
      // up, and for the same reason: a Summary is somebody deliberately naming ONE event, and a
      // group renders one member's sentence and hides the rest behind a count.
      //
      // IT IS ALSO AN ORDERING BUG AND NOT ONLY A HIDING ONE. A group is every turn sharing a key
      // in a barrier-free REGION, anchored at its first member — deliberately, so an interleaved
      // dispatch loop folds — so a Machine that flapped `started, exited, started` renders the
      // second start folded up into the first, ABOVE the exit that came between them. A crash loop
      // reads as one start ×2 and one exit, in the wrong order. Found against the real shape of a
      // Warden's watcher — one `ActivityTaskScheduled` per decision, forever — and pinned by "never
      // folds two raw events that carry different sentences, or reorders them", with the control for
      // the other direction beside it.
      if (turn.label) return undefined;
      // All of Temporal's bookkeeping still folds into ONE line however its three types alternate;
      // anything else nobody named folds only with its own type.
      return turn.plumbing ? 'raw:plumbing' : `raw:${turn.type}`;
    default:
      return undefined;
  }
}

/** One group, as a turn of the same kind carrying totals and its members. */
function fold(members: Turn[]): Turn {
  const head: Turn = { ...members[0]!, folded: members };
  head.events = [];
  head.types = [];
  for (const m of members) {
    head.events.push(...m.events);
    for (const type of m.types) if (!head.types.includes(type)) head.types.push(type);
  }
  head.count = members.length;
  head.endT = Math.max(...members.map((m) => m.endT));
  // SUMMED, not spanned. What an operator wants from a folded loop is the time the calls actually
  // took; the wall-clock span is `t`..`endT`, which is beside it and says something different.
  head.dur = members.reduce((sum, m) => sum + m.dur, 0);
  head.attempt = Math.max(...members.map((m) => m.attempt));
  head.failures = members.reduce((sum, m) => sum + m.failures, 0);
  head.open = members.some((m) => m.open);
  const failed = members.find((m) => m.error);
  if (failed?.error) head.error = failed.error;
  if (head.kind === 'dispatch') {
    // The state of a group is the worst thing that happened in it: a loop with one failure in it is
    // not a clean loop, and a loop still waiting on a queue nobody polls is not a finished one.
    const states = members.map((m) => (m.kind === 'dispatch' ? m.state : 'done'));
    head.state = states.includes('failed')
      ? 'failed'
      : states.includes('cancelled')
        ? 'cancelled'
        : states.includes('queued')
          ? 'queued'
          : states.includes('running')
            ? 'running'
            : 'done';
    head.units = sumUnits(members);
  }
  if (head.kind === 'dataset') {
    const rows = members.reduce<number | undefined>(
      (sum, m) => (m.kind === 'dataset' && m.rows !== undefined ? (sum ?? 0) + m.rows : sum),
      undefined
    );
    if (rows !== undefined) head.rows = rows;
  }
  return head;
}

/**
 * The totals of a folded group of dispatches.
 *
 * `undefined` UNLESS SOMETHING COUNTED. Summing 200 absent counts to 0 would report a sweep that
 * dropped nothing and found nothing, which is the one sentence a collapsed loop must never say by
 * accident.
 */
function sumUnits(members: Turn[]): DispatchTurn['units'] {
  let out: { in?: number; out?: number; dropped?: number } | undefined;
  for (const m of members) {
    if (m.kind !== 'dispatch' || !m.units) continue;
    out ??= {};
    if (m.units.in !== undefined) out.in = (out.in ?? 0) + m.units.in;
    if (m.units.out !== undefined) out.out = (out.out ?? 0) + m.units.out;
    if (m.units.dropped !== undefined) out.dropped = (out.dropped ?? 0) + m.units.dropped;
  }
  return out;
}
