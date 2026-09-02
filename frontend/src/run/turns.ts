/**
 * One Run's account of itself, as the console reads it: the reduced log in, named turns out.
 *
 * THE REDUCTION HAPPENS HERE, IN THE BROWSER, AND THAT IS A DECISION RATHER THAN A CONVENIENCE.
 * `@core/transcript` and `@core/vocabulary` are pure — no DOM, no fetch, no clock they were not
 * handed, and between them exactly one import: `transcript.ts` takes `EventLink` and `RunEvent`
 * from `history.ts` as TYPES, and `history.ts` imports nothing at all. So the whole reader is a
 * pure function over bytes this package already holds, and `@core` is the alias the web app has
 * used to reach the orchestrator's pure core since `@core/scratch` and `@core/actorControl`.
 * `transcript.ts` says so itself, about the very type this module hands it: "STRUCTURAL, so both
 * spellings of the reduced log satisfy it … the reader is imported by both sides".
 *
 * WHICH IS WHY THERE IS NO `/api/runs/:id/turns`. A turns route would re-serialise a nine-arm
 * discriminated union over JSON to tell the browser something it can already compute from the
 * response it just received, and it would need its own cap, its own archive fallback and its own
 * `?exec=` — a second spelling of `/history` that could drift from the first. The claim-check
 * constraint (ADR 0007) is unaffected either way: `/history` is payload-free BY CONSTRUCTION, the
 * reducer never decodes and never fetches, so moving it across the wire moves no payload with it.
 *
 * THE ONE SEAM. Everything above the transcript asks this module for a run's turns and nothing
 * else; if the reduction ever does have to move to the server, this file changes and no renderer
 * does.
 *
 * NOTHING HERE THROWS. A run Temporal dropped for retention, a run whose first event has not landed
 * and a cluster that cannot be asked are three different sentences, and every one of them is
 * something the Transcript tab has to be able to print beside a run that is still in the list. They are
 * told apart on the way out ({@link RunTurnsRead}) rather than collapsed into a rejected promise.
 */

import { readVocabulary, type NamedTranscript } from '@core/vocabulary';
import type { Ask } from '@core/transcript';

import { readHistory, type RunHistory } from './api';

const BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api';

/**
 * One ask, as `/api/runs/:runId/asks` sends it.
 *
 * IT EXTENDS THE READER'S OWN `Ask` rather than restating its fields, which is the opposite of what
 * `run/api.ts` does for `RunEvent` and is deliberate: `hitl.ts` declares `RunAsk` "FIELD-FOR-FIELD
 * `transcript.ts`'s `Ask`, plus what only the run itself knows", and inheriting the base half here
 * makes a rename on that side a compile error rather than a field that silently arrives
 * `undefined` in a rendered transcript. The server's own type is not imported because `hitl.ts`
 * reaches for `NodeJS.ProcessEnv`, and this package has no node types.
 */
export interface RunAsk extends Ask {
  /** Which of the four endings the RUN says happened. `pending` is the only one a route signals. */
  state: 'pending' | 'answered' | 'expired' | 'abandoned';
  /** Milliseconds the run has been parked on this ask — to the answer, or to now. */
  waitedMs: number;
  /** Milliseconds left before the deadline, floored at 0. Absent where there is no deadline. */
  remainingMs?: number;
  /** This entry was not readable as an ask. Kept and marked rather than dropped. */
  malformed?: boolean;
}

/**
 * A run's turns, WITH THE ID THEY ARE ABOUT.
 *
 * THE ID IS THE WHOLE POINT OF THE WRAPPER. A transcript is a plain reading with nothing on it that
 * says whose it is, so a component holding one in state has no way to tell a fresh answer from the
 * answer to the question before last. That is #13's bug one level down — the wrong workflow's
 * status — and `runId` here is what lets `transcriptFor` in `workflowThread.ts` refuse to draw a
 * reading against a run nobody selected.
 */
export interface RunTurns {
  runId: string;
  named: NamedTranscript;
  /**
   * True while nothing has closed this run — terminal event or nothing, never a clock.
   *
   * IT IS WHAT STOPS THE READER, which is why it belongs on the reading rather than beside it.
   * `run/follow.ts` re-reads a run until this goes false and then disarms, so "should I ask again"
   * and "is this account complete" are one fact read from one authority instead of a poll loop
   * guessing from a run list that lags.
   */
  live: boolean;
  /**
   * Every ask this run published, as the route sent them — carried BESIDE the turns rather than
   * only inside them.
   *
   * BECAUSE THE PARKED TURN CANNOT CARRY ALL OF IT. `transcript.ts`'s `Ask` is the half the
   * workflow and the reader share — prompt, schema, context, the two instants — and the run's own
   * declaration of how an ask ENDED is not in it: `state` tells `expired` from `abandoned` from
   * `answered`, and neither is derivable from `answeredAt` being absent. `malformed` is the same
   * shape of fact. A surface that re-derived either would be inventing a verdict the run already
   * gave (`hitl.ts`: "declared by the RUN, not inferred here"), so the list travels intact and the
   * transcript looks an ask up by id.
   *
   * EMPTY IS THE ORDINARY CASE and is not a failure: a run that asked nobody anything, and a run
   * whose asks could not be read, both arrive here as `[]` — see {@link fetchRunAsks}.
   */
  asks: readonly RunAsk[];
}

/**
 * A reading, or the reason there is none.
 *
 * `gone` IS NOT AN ERROR AND IS NOT AN EMPTY TRANSCRIPT. Temporal drops an execution at retention
 * long before anyone stops caring what it did, and the archive (ADR 0025) answers for most of
 * those — so a run that reaches here `gone` is one BOTH authorities have lost. Rendering that as
 * "no turns" would say the run did nothing; rendering it as a failure would say the appliance is
 * broken. It is neither, so it is its own arm.
 */
export type RunTurnsRead =
  | {
      ok: true;
      turns: RunTurns;
      /**
       * The reduced log the reading was made FROM, handed back rather than dropped.
       *
       * BECAUSE THE DRILL MUST NOT COST A SECOND READ. Every turn carries the event ids it folded
       * (`transcript.ts`: "THE DRILL PATH"), and the rows behind those ids are in the response this
       * call already received — so a domain turn opens onto its raw Temporal events with no fetch
       * at all. That is what "the friendly view never costs the real one" means arithmetically: not
       * that the raw view is reachable, but that reaching it costs nothing, so nobody is ever
       * tempted to make the vocabulary the only view because checking it is expensive.
       *
       * A CHILD IS STILL A SECOND READ, and has to be: it is another workflow id with its own
       * history, which is exactly why one route takes any id (`history.ts`).
       */
      history: RunHistory;
    }
  | { ok: false; runId: string; gone: boolean; detail: string };

/**
 * The reduction itself, pure and separate from the fetching.
 *
 * Exported so every state of a transcript can be asserted from literal reduced events, with no
 * network and no clock — the same property `transcript.ts` and `vocabulary.ts` have, kept intact
 * across the package boundary rather than lost at it.
 *
 * `now` IS NOT DEFAULTED TO `Date.now()`. The reader's own default is the last instant the log
 * knows about, which is the most recent thing it can honestly say happened — and on a FINISHED run
 * that is exactly right. Passing the wall clock would make "parked for 4 minutes" grow every render
 * on a run that closed last week.
 *
 * ON A LIVE RUN IT IS ALSO WHAT MAKES FOLLOWING QUIET. The reading is then a pure function of the
 * bytes that were read, so `run/follow.ts` can tell a poll that found something from a poll that
 * found nothing and say nothing about the second — which is the whole of "a turn arriving is
 * arrival, not animation". The price is that a park's `waited` advances when the LOG advances rather
 * than with the clock; the ask route's own `waitedMs` is the authority for a surface that needs the
 * second-by-second number.
 */
export function readRunTurns(
  runId: string,
  history: RunHistory,
  asks: readonly RunAsk[] = [],
  now?: number
): RunTurns {
  const named = readVocabulary(history, {
    asks,
    ...(now === undefined ? {} : { now }),
  });
  return { runId, named, live: named.transcript.live, asks };
}

/**
 * Every ask this run published — pending, answered, expired and abandoned alike.
 *
 * BEST-EFFORT, AND EMPTY ON ANYTHING THAT IS NOT AN ANSWER. An ask is a SECOND authority beside the
 * event log (`transcript.ts`: "It does not come from the event log, and cannot yet"), so a run
 * whose asks cannot be read still has a whole transcript to draw — it is missing the parks, and a
 * transcript missing its parks is enormously better than no transcript. A 404 here is ordinary: it
 * is what a run neither Temporal nor the archive holds answers with.
 */
export async function fetchRunAsks(runId: string): Promise<RunAsk[]> {
  try {
    const res = await fetch(`${BASE}/runs/${encodeURIComponent(runId)}/asks`);
    if (!res.ok) return [];
    const body = (await res.json()) as { asks?: unknown };
    // A `200` carrying something that is not a list reaches `.map()` inside a render and unmounts
    // the surface — the failure `fetchFleetOperations` documents, one route over.
    return Array.isArray(body.asks) ? (body.asks as RunAsk[]) : [];
  } catch {
    return [];
  }
}

/**
 * Answering one ask, or the reason the appliance would not take it.
 *
 * A RESULT AND NOT A THROW, which is the opposite of `stopRun` beside it and is deliberate. Every
 * refusal here belongs BESIDE THE FORM the operator is looking at rather than in a page-level
 * notice: a `400` names the field that does not fit and the operator fixes that field; a `409` says
 * the ask is no longer pending, which is somebody else having answered first or the deadline having
 * passed, and is not a mistake to correct. A thrown error would flatten those into one red line at
 * the top of a page holding three other asks that are perfectly answerable.
 */
export type AnswerSent =
  | { ok: true; ask: RunAsk }
  | {
      ok: false;
      /** The appliance's own sentence — surfaced verbatim, never replaced with "request failed". */
      refused: string;
      /** The instance path the schema objected to (`/approve`, `/` for the document). 400 only. */
      field?: string;
      /** What the run says the ask is now, when it is no longer pending. 409 only. */
      state?: string;
    };

/**
 * Answer one ask.
 *
 * NO AUTHORIZATION HEADER, exactly as `stopRun` sends none. The route is gated the same way and by
 * the same variables, and the appliance is open by default (ADR 0031) — a header invented here
 * would be a credential this page does not have.
 *
 * `by` IS SENT ONLY WHEN THE OPERATOR TYPED ONE. Omitted, the appliance falls back to its own
 * `KONTRA_OPERATOR`; sending `""` would be this form asserting "nobody" over a box whose whole
 * purpose is to say who.
 */
export async function answerRunAsk(
  runId: string,
  askId: string,
  answer: { value: unknown; by?: string }
): Promise<AnswerSent> {
  const by = (answer.by ?? '').trim();
  try {
    const res = await fetch(
      `${BASE}/runs/${encodeURIComponent(runId)}/asks/${encodeURIComponent(askId)}`,
      {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ value: answer.value, ...(by ? { by } : {}) }),
      }
    );
    const body = (await res.json().catch(() => ({}))) as {
      error?: unknown;
      field?: unknown;
      state?: unknown;
      ask?: unknown;
    };
    if (!res.ok) {
      return {
        ok: false,
        refused:
          typeof body.error === 'string' && body.error.trim()
            ? body.error
            : `${res.status} ${res.statusText}`,
        ...(typeof body.field === 'string' ? { field: body.field } : {}),
        ...(typeof body.state === 'string' ? { state: body.state } : {}),
      };
    }
    return { ok: true, ask: body.ask as RunAsk };
  } catch (err) {
    // THE RUN IS STILL PARKED. A request that never arrived changed nothing, so this says the
    // answer did not land rather than anything about the ask.
    return { ok: false, refused: `the answer did not reach the appliance: ${message(err)}` };
  }
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/**
 * One run's turns, off the wire.
 *
 * TWO READS, AND THE SECOND ONE MAY FAIL WITHOUT COSTING THE FIRST. The log is the transcript; the
 * asks are an overlay on it. So the history decides whether there is an answer at all, and the asks
 * are gathered beside it in the same tick and dropped quietly if they are not there.
 */
export async function fetchRunTurns(runId: string): Promise<RunTurnsRead> {
  const [read, asks] = await Promise.all([readHistory(runId), fetchRunAsks(runId)]);
  if (!read.ok) return { ok: false, runId, gone: read.gone, detail: read.detail };
  return { ok: true, turns: readRunTurns(runId, read.history, asks), history: read.history };
}
