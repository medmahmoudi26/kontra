/**
 * A parked run's asks: reading them, and answering one.
 *
 * WHY THERE IS NO QUERY HERE. Asking a workflow a question is the house idiom — `workflows/
 * tmuxSession.ts` and `workflows/stack.ts` both answer queries, and a query would have been
 * cheaper than any of this. It fails in the two places that matter. A query is answered by a
 * WORKER, so a parked run whose worker is down could not be read at all, and a parked run is
 * precisely the one most likely to outlive the process that parked it. And a query writes no
 * history, so an ask would never reach the archived reduced log (ADR 0025) while its ANSWER would,
 * since `signal` is already a reduced-log category (`history.ts`). An archive holding an answer
 * and not its question is a record of somebody approving something unspecified.
 *
 * SO AN ASK IS THE RUN'S OWN MEMO, written by `sdk/python/actorkit/hitl.py` with `upsert_memo`.
 * That is one `WorkflowPropertiesModified` history event per ask and one per answer, AND it rides
 * on `DescribeWorkflowExecution` and on the visibility listing — so everything below reads a
 * parked run with one RPC, no history scan, and no worker anywhere.
 *
 * THE LIST IS THE CONTRACT. Parallel branches each needing a decision is normal in these
 * workflows, so `readAsks` answers with all of them and the route serves an array even when there
 * is one. A singular route could not have been widened later without breaking both its own
 * contract and the transcript's rendering.
 *
 * NOTHING HERE IS AUTHENTICATION. The `by` on an answer is whatever the answering client said it
 * was: the appliance is loopback with no credential (ADR 0031), so there is no identity to record
 * and this module does not invent one. It is attribution — useful on a shared box, and useful
 * reading your own history six weeks later — and no decision in kontra is gated on it. Do not
 * build one that is.
 *
 * AN UNREADABLE ASK IS STILL AN ASK. A memo entry this module cannot make sense of becomes a
 * legible row saying so rather than an exception: the run IS parked either way, and a read that
 * threw would take the whole list — including the asks that are perfectly readable — down with it.
 */

import Ajv2020 from 'ajv/dist/2020';
import type { ErrorObject } from 'ajv';

/**
 * The memo key one ask is filed under, and the signal that answers it.
 *
 * CROSS-LANGUAGE LITERALS, written independently on each side per the house decoupling rule — the
 * Python peer is `sdk/python/actorkit/hitl.py` and the Go one lands with issue 13. `hitl.test.ts`
 * pins these against that file's bytes, because a drift here is not a type error: it is a run that
 * publishes an ask nothing reads, and an answer signalled to a name nothing handles.
 */
export const ASK_MEMO_PREFIX = 'kontra.ask.';
export const ANSWER_SIGNAL_PREFIX = 'kontra.answer/';

/**
 * How one ask ended — declared by the RUN, not inferred here.
 *
 * FOUR WORDS, NOT THREE. A deadline nobody met and a run somebody cancelled are different endings,
 * and an operator finding a cancelled run must not read that a human failed to answer in time.
 * `pending` is the only state a route will signal.
 */
export type AskState = 'pending' | 'answered' | 'expired' | 'abandoned';

const ASK_STATES: ReadonlySet<string> = new Set<AskState>([
  'pending',
  'answered',
  'expired',
  'abandoned',
]);

/**
 * One question a run published.
 *
 * FIELD-FOR-FIELD `transcript.ts`'s `Ask`, plus what only the run itself knows. Nothing translates
 * between the two, which is why `test_hitl_ask.py` asserts the Python emitter against the
 * TypeScript declaration: a rename on either side has to be a test failure rather than a field
 * that silently arrives `undefined` in a rendered form.
 */
export interface RunAsk {
  id: string;
  /** The sentence the workflow asked. */
  prompt: string;
  /** Epoch ms the workflow parked on it. */
  askedAt: number;
  /** JSON Schema for the answer — what the form renders from and what an answer is validated
   *  against. Absent on an ask that declared no shape, and dropped once the ask settles. */
  schema?: unknown;
  /** Whatever the workflow attached — the Dataset, the counts, the sample. */
  context?: unknown;
  /** Epoch ms the ask expires. ABSENT means the author declared no deadline and this run waits
   *  indefinitely, which is a legitimate choice and not a missing value. */
  deadlineAt?: number;
  /** Epoch ms it was answered. Absent while it is still pending. */
  answeredAt?: number;
  /** The operator label on the answer. SELF-ASSERTED — see the module header. */
  by?: string;
  /** Which of the four endings the RUN says happened. */
  state: AskState;
  /** Milliseconds the run has been parked on this ask — to the answer, or to now. The answer to
   *  "am I the bottleneck?". */
  waitedMs: number;
  /** Milliseconds left before the deadline, floored at 0. ABSENT where there is no deadline: a
   *  countdown rendered over "waits indefinitely" is a number nobody meant. */
  remainingMs?: number;
  /** This entry was not readable as an ask. The row is kept and marked rather than dropped —
   *  the run is parked either way, and an ask that vanished is unrecoverable from the UI. */
  malformed?: boolean;
}

/** Every ask a run published, oldest first — pending, answered, expired and abandoned alike. */
export function readAsks(memo: unknown, now: number): RunAsk[] {
  if (!memo || typeof memo !== 'object') return [];
  const out: RunAsk[] = [];
  for (const [key, raw] of Object.entries(memo as Record<string, unknown>)) {
    if (!key.startsWith(ASK_MEMO_PREFIX)) continue;
    out.push(oneAsk(key.slice(ASK_MEMO_PREFIX.length), raw, now));
  }
  // By the instant it was ASKED, so a run's questions read in the order a human met them. An
  // unreadable entry has no instant of its own and sorts to the front rather than being dropped.
  return out.sort((a, b) => a.askedAt - b.askedAt);
}

/** The asks still waiting on a human. What "parked" means, and the only ones a route will signal. */
export function pendingAsks(asks: readonly RunAsk[]): RunAsk[] {
  return asks.filter((a) => a.state === 'pending');
}

function oneAsk(id: string, raw: unknown, now: number): RunAsk {
  const bag = (raw && typeof raw === 'object' ? raw : {}) as Record<string, unknown>;
  const prompt = typeof bag.prompt === 'string' ? bag.prompt : '';
  const askedAt = num(bag.askedAt);
  const state = ASK_STATES.has(bag.state as string) ? (bag.state as AskState) : 'pending';
  const answeredAt = num(bag.answeredAt);
  const deadlineAt = num(bag.deadlineAt);
  const until = answeredAt > 0 ? answeredAt : num(bag.expiredAt) || num(bag.abandonedAt) || now;

  const ask: RunAsk = {
    id: typeof bag.id === 'string' && bag.id ? bag.id : id,
    // A LEGIBLE ROW, NEVER A BLANK ONE. The run is parked whether or not this server can read
    // what it asked, and a surface that dropped the row would leave an operator with a stuck run
    // and nothing to click.
    prompt: prompt || '(this run published an ask that could not be read)',
    askedAt,
    state,
    waitedMs: Math.max(0, until - askedAt),
  };
  if (!prompt || askedAt <= 0) ask.malformed = true;
  if (bag.schema !== undefined && bag.schema !== null) ask.schema = bag.schema;
  if (bag.context !== undefined && bag.context !== null) ask.context = bag.context;
  if (deadlineAt > 0) {
    ask.deadlineAt = deadlineAt;
    ask.remainingMs = Math.max(0, deadlineAt - now);
  }
  if (answeredAt > 0) ask.answeredAt = answeredAt;
  if (typeof bag.by === 'string' && bag.by) ask.by = bag.by;
  return ask;
}

function num(raw: unknown): number {
  return typeof raw === 'number' && Number.isFinite(raw) ? raw : 0;
}

/* ───────────────────────────── validating an answer ───────────────────────────── */

/**
 * `strict: false` and `allErrors: true`, exactly as `catalog.ts` builds its own — an ask's schema
 * comes off the same pydantic derivation an Actor's does, carrying `title` and formats ajv does
 * not know, and refusing those would refuse real asks over their emitter's habits.
 *
 * ITS OWN INSTANCE, not the catalog's. Compiling registers a document's `$id` globally in an ajv
 * instance and a second registration of the same `$id` throws (`schema with key or id … already
 * exists`) — the bug the dispatch path already hit once. An ask's schema is compiled on every
 * answer, so sharing that registry with the catalog's is a collision waiting for the first author
 * who gives two things the same `$id`.
 */
const ajv = new Ajv2020({ strict: false, allErrors: true });

/** An answer the ask's own schema does not accept. Carries WHERE, because "invalid" without a
 *  field is a form an operator has to guess their way around. */
export class AnswerRefused extends Error {
  constructor(
    message: string,
    /** The instance path ajv objected to — `/approve`, or `/` for the document itself. */
    readonly field: string
  ) {
    super(message);
    this.name = 'AnswerRefused';
  }
}

/** No such ask on that run — or no such run. */
export class AskNotFound extends Error {
  constructor(runId: string, askId: string) {
    super(`run ${runId} has no ask ${askId}`);
    this.name = 'AskNotFound';
  }
}

/** The ask exists and is no longer waiting. Distinct from {@link AskNotFound} because the two are
 *  different things to tell an operator: one is a typo, the other is "somebody got there first,
 *  or the deadline passed". */
export class AskNotPending extends Error {
  constructor(
    readonly ask: RunAsk,
    runId: string
  ) {
    super(`ask ${ask.id} on run ${runId} is ${ask.state}, not pending`);
    this.name = 'AskNotPending';
  }
}

/**
 * Check one answer against the schema its ask declared.
 *
 * AGAINST THE ASK'S OWN SCHEMA, not a shape this server keeps. The question is contextual and so
 * is the answer's shape; validating against anything else would be this module deciding what the
 * workflow meant. An ask that declared no schema accepts what it is handed — the author chose not
 * to constrain it, and inventing a constraint here would refuse answers to their own question.
 */
export function validateAnswer(schema: unknown, value: unknown): void {
  if (schema === undefined || schema === null) return;
  if (typeof schema !== 'object') {
    throw new AnswerRefused('the ask declared a schema this server cannot read', '/');
  }
  let check;
  try {
    check = ajv.compile(schema as object);
  } catch (err) {
    // A schema in a dialect this ajv does not hold. REFUSE rather than accept: a workflow asked
    // for a shape and nothing here checked it, so signalling anyway would hand the run a value it
    // did not agree to take.
    throw new AnswerRefused(
      `the ask's schema could not be compiled: ${err instanceof Error ? err.message : String(err)}`,
      '/'
    );
  }
  if (check(value)) return;
  const first = (check.errors ?? [])[0];
  throw new AnswerRefused(explain(check.errors), first?.instancePath || '/');
}

/** ajv's errors as one line an operator can act on: where, and what is wrong there. */
function explain(errors: ErrorObject[] | null | undefined): string {
  const said = (errors ?? []).map((e) => `${e.instancePath || '/'} ${e.message ?? ''}`.trim());
  return said.length > 0 ? said.join('; ') : 'the answer does not fit the ask';
}

/* ───────────────────────────── answering one ───────────────────────────── */

/** The two reads and the one write this needs, injected so the decision is testable with no
 *  cluster — the same seam `RunLifecycle` takes for the execution dimension. */
export interface AnswerDeps {
  /** Every ask that run published. `undefined` when neither Temporal nor the archive knows it. */
  asks: (runId: string) => Promise<RunAsk[] | undefined>;
  /** Send one signal to the run. */
  signal: (runId: string, name: string, payload: unknown) => Promise<void>;
}

export interface AnswerRequest {
  value: unknown;
  /** The operator label. SELF-ASSERTED — recorded, never checked, never gated on. */
  by?: string;
}

/**
 * Answer one ask: find it, check it against its own declared schema, and only then signal.
 *
 * VALIDATION BEFORE THE SIGNAL, and that ordering is the whole point. A signal is durable and
 * unacknowledged — once it is in the run's history the workflow will act on it, and there is no
 * taking it back. So an answer that does not fit is refused HERE, naming the field, with the run
 * left exactly as parked as it was; the operator fixes the form and tries again.
 */
export async function answerAsk(
  deps: AnswerDeps,
  runId: string,
  askId: string,
  request: AnswerRequest
): Promise<RunAsk> {
  const asks = await deps.asks(runId);
  if (!asks) throw new AskNotFound(runId, askId);
  const ask = asks.find((a) => a.id === askId);
  if (!ask) throw new AskNotFound(runId, askId);
  if (ask.state !== 'pending') throw new AskNotPending(ask, runId);

  validateAnswer(ask.schema, request.value);

  const by = typeof request.by === 'string' ? request.by.slice(0, 200).trim() : '';
  await deps.signal(runId, `${ANSWER_SIGNAL_PREFIX}${ask.id}`, { value: request.value, by });
  return { ...ask, by: by || undefined, state: 'answered' };
}

/**
 * The operator label this appliance puts on an answer nobody labelled.
 *
 * A NAME FROM LOCAL CONFIG, and the honest thing it is: whoever set `KONTRA_OPERATOR` on this box
 * said so about themselves. Unset means unlabelled — an answer attributed to `operator` or to a
 * hostname would be kontra asserting something no human said, which is exactly the sentence this
 * field must never become.
 */
export function defaultOperator(env: NodeJS.ProcessEnv = process.env): string {
  return (env.KONTRA_OPERATOR ?? '').trim();
}
