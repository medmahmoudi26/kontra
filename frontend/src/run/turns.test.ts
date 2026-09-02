/**
 * The seam that gets a Run's turns into the browser.
 *
 * THIS SUITE IS THE ARCHITECTURAL CLAIM, EXECUTED. `transcript.ts` and `vocabulary.ts` live in the
 * `orchestrator` package; this one is `frontend`. If the reducer could not be imported
 * across that boundary the reduction would have to become an HTTP route, so the first thing below
 * is that it CAN be — the same `@core` alias `@core/scratch` and `@core/actorControl` already use,
 * with `readVocabulary` producing kontra's own words from a reduced log this package fetched. It is
 * asserted rather than asserted-in-a-comment: a build that broke the alias fails here.
 *
 * NO PAYLOAD ANYWHERE IN THIS FILE, and that is the second claim. Every fixture is a REDUCED event
 * — the shape `/api/runs/:id/history` serves and the archive stores (ADR 0025) — with no `input`,
 * no `result` and no failure payload, because there is none in the thing the reader reads. A test
 * that had to hand the reducer a payload to get a turn out of it would be the first sign the
 * claim-check constraint (ADR 0007) had been broken by moving the reduction across the wire.
 *
 * `cat` IS NEVER HAND-WRITTEN. It comes from `history.ts`'s own `categorize`, so a fixture cannot
 * quietly disagree with the reducer about what counts as a failure — which is the one thing the
 * invisible-failure half of the vocabulary rests on.
 *
 * Node, no jsdom. `fetch` is a stub that routes by URL, so the two reads and their three failure
 * arms are asserted without a server.
 */

import { afterEach, describe, expect, it, vi } from 'vitest';

import { categorize } from '@core/history';
import type { ParkedTurn } from '@core/transcript';

import type { RunEvent, RunHistory } from './api';
import { answerRunAsk, fetchRunAsks, fetchRunTurns, readRunTurns, type RunAsk } from './turns';

/** The first event's instant, so offsets read like a real run's. */
const BASE = 1_786_831_339_151;

function ev(id: number, type: string, ms: number, extra: Partial<RunEvent> = {}): RunEvent {
  return {
    id,
    type,
    cat: categorize(type),
    t: ms / 1000,
    at: BASE + ms,
    detail: type,
    attempt: 1,
    dur: 0,
    ...extra,
  };
}

/** A closing event, carrying the duration Temporal recorded back to the event it closes. `dur` IS
 *  the join — the reduced log drops `scheduledEventId` — so a fixture written any other way would
 *  be a closer the reader cannot place, exactly as in production. */
function closes(id: number, type: string, ms: number, openerMs: number, extra: Partial<RunEvent> = {}): RunEvent {
  return ev(id, type, ms, { dur: (ms - openerMs) / 1000, ...extra });
}

function history(events: RunEvent[], extra: Partial<RunHistory> = {}): RunHistory {
  return { events, scanned: events.length, elided: 0, truncated: false, ...extra };
}

const started = ev(1, 'WorkflowExecutionStarted', 0, {
  detail: 'workflowType=DnsSweep · taskQueue=wf-dnssweep · identity=1@65520c20a6a4',
});

/** A run that read a Dataset, dispatched to an Actor and published a Batch. */
function workingRun(): RunEvent[] {
  return [
    started,
    ev(2, 'ActivityTaskScheduled', 1_000, { detail: 'activityType=pageDataset' }),
    closes(3, 'ActivityTaskCompleted', 1_400, 1_000, { detail: 'activityType=pageDataset' }),
    ev(4, 'NexusOperationScheduled', 1_500, { detail: 'endpoint=kontra-nscheck-0-1-0' }),
    closes(5, 'NexusOperationCompleted', 9_500, 1_500, { detail: 'endpoint=kontra-nscheck-0-1-0' }),
    ev(6, 'ActivityTaskScheduled', 9_600, { detail: 'activityType=publishBatch' }),
    closes(7, 'ActivityTaskCompleted', 9_900, 9_600, { detail: 'activityType=publishBatch' }),
  ];
}

const REAL_FETCH = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = REAL_FETCH;
  vi.restoreAllMocks();
});

/** A `fetch` that answers the two routes this module reads, and nothing else. */
function stubFetch(routes: { history?: () => Response; asks?: () => Response }): void {
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes('/history')) {
      return routes.history?.() ?? new Response('nope', { status: 500 });
    }
    if (url.includes('/asks')) {
      return routes.asks?.() ?? new Response(JSON.stringify({ asks: [] }), { status: 200 });
    }
    throw new Error(`unexpected fetch: ${url}`);
  }) as typeof fetch;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
}

/* ───────────────────────── the reducer crosses the package boundary ───────────────────────── */

describe('the web package reduces a run itself', () => {
  it('turns a reduced log into kontra’s own words, with no route in between', () => {
    const turns = readRunTurns('sweep-1', history(workingRun().concat(ev(8, 'WorkflowExecutionCompleted', 10_000))));

    expect(turns.runId).toBe('sweep-1');
    expect(turns.live).toBe(false);
    // Not "NexusOperationCompleted". The whole point of the module is that this reads as a Method.
    const words = turns.named.turns.map((t) => t.label);
    expect(words).toContain('run started');
    expect(words).toContain('method returned');
    expect(words).toContain('batch published');
    expect(turns.named.verdict.term).toBe('run-finished');
    expect(turns.named.verdict.because).toBe('published=1');
  });

  it('carries the drill path on every turn — a domain turn is never a dead end', () => {
    const turns = readRunTurns('sweep-1', history(workingRun()));
    for (const t of turns.named.turns) {
      // A turn built from events quotes the event ids it folded. (A parked ask has none, and there
      // is no ask in this fixture.)
      expect(t.turn.events.length, t.label).toBeGreaterThan(0);
      expect(t.turn.types.length, t.label).toBeGreaterThan(0);
    }
  });

  it('reads a run that is still open as LIVE, not as finished', () => {
    // Terminal event or nothing. A log with no closing event is a run that has not closed, and
    // labelling it "finished" is what stops an operator watching a run that is still going.
    const turns = readRunTurns('sweep-1', history(workingRun()));
    expect(turns.live).toBe(true);
    expect(turns.named.verdict.term).toBe('run-running');
  });
});

/* ───────────────────────── the failures that have no failure event ───────────────────────── */

describe('a completed run is three different answers', () => {
  it('finished, having published something', () => {
    const turns = readRunTurns('r', history([...workingRun(), ev(8, 'WorkflowExecutionCompleted', 10_000)]));
    expect(turns.named.verdict.term).toBe('run-finished');
    expect(turns.named.concerns.map((c) => c.term)).not.toContain('produced-nothing');
  });

  it('finished, having produced nothing — which is NOT the same word', () => {
    // The documented failure: a node whose Units all failed reports `completed` with empty output.
    const turns = readRunTurns(
      'r',
      history([
        started,
        ev(2, 'ActivityTaskScheduled', 1_000, { detail: 'activityType=pageDataset' }),
        closes(3, 'ActivityTaskCompleted', 1_400, 1_000, { detail: 'activityType=pageDataset' }),
        ev(4, 'WorkflowExecutionCompleted', 2_000),
      ])
    );
    expect(turns.named.verdict.term).toBe('run-produced-nothing');
    expect(turns.named.concerns.map((c) => c.term)).toContain('produced-nothing');
  });

  it('finished, and nothing worked', () => {
    const turns = readRunTurns(
      'r',
      history([
        started,
        ev(2, 'NexusOperationScheduled', 1_000, { detail: 'endpoint=kontra-nscheck-0-1-0' }),
        closes(3, 'NexusOperationFailed', 5_000, 1_000, {
          detail: 'endpoint=kontra-nscheck-0-1-0 · dial tcp: connection refused',
        }),
        ev(4, 'WorkflowExecutionCompleted', 6_000),
      ])
    );
    expect(turns.named.verdict.term).toBe('run-nothing-worked');
    // A dispatch Temporal closed with a failure and no `…Started` event is a queue nobody polled —
    // the failure that reads identically to a slow Method until something names it.
    expect(turns.named.concerns.map((c) => c.term)).toContain('queue-unpolled');
  });

  it('failed, and says WHERE rather than only that it failed', () => {
    const turns = readRunTurns(
      'r',
      history([
        started,
        ev(2, 'ActivityTaskScheduled', 1_000, { detail: 'activityType=RunBatch · taskQueue=nscheck-0.1.0' }),
        closes(3, 'ActivityTaskFailed', 4_000, 1_000, {
          detail: 'activityType=RunBatch · NXDOMAIN for every name in the batch',
        }),
        ev(4, 'WorkflowExecutionFailed', 4_500, { detail: 'activity task failed' }),
      ])
    );
    expect(turns.named.verdict.term).toBe('run-failed');
    expect(turns.named.verdict.because).toContain('failed at event 3');
    expect(turns.named.verdict.because).toContain('NXDOMAIN');
  });

  it('prints the hole in a capped log rather than swallowing it', () => {
    const turns = readRunTurns('r', history([started, ev(2, 'WorkflowExecutionCompleted', 10)], { elided: 4_312 }));
    const elided = turns.named.concerns.find((c) => c.term === 'log-elided');
    expect(elided?.label).toContain('4312');
  });

  it('says when the account came from the archive rather than from Temporal', () => {
    const turns = readRunTurns('r', history([started], { archived: true, archivedAt: BASE + 99 }));
    expect(turns.named.transcript.archived).toBe(true);
    expect(turns.named.transcript.archivedAt).toBe(BASE + 99);
  });
});

/* ───────────────────────── the asks, merged into the middle ───────────────────────── */

describe('a parked run', () => {
  const ask: RunAsk = {
    id: 'approve-fleet',
    prompt: 'Bring up 10 machines in sfo3?',
    askedAt: BASE + 2_000,
    state: 'pending',
    waitedMs: 8_000,
  };

  it('places the question at the instant it was asked, not at the end', () => {
    const turns = readRunTurns('r', history(workingRun()), [ask]);
    const kinds = turns.named.turns.map((t) => t.turn.kind);
    const at = kinds.indexOf('parked');
    expect(at).toBeGreaterThan(0);
    expect(at).toBeLessThan(kinds.length - 1);
  });

  it('reads as waiting on a human, with the sentence the workflow asked', () => {
    const turns = readRunTurns('r', history(workingRun()), [ask]);
    const parked = turns.named.turns.find((t) => t.turn.kind === 'parked');
    expect(parked?.term).toBe('run-parked');
    expect(parked?.domain).toBe('human');
    expect((parked?.turn as ParkedTurn).ask.prompt).toBe('Bring up 10 machines in sfo3?');
    expect((parked?.turn as ParkedTurn).pending).toBe(true);
  });

  it('reads an answered ask as answered, and by whom', () => {
    const answered: RunAsk = { ...ask, state: 'answered', answeredAt: BASE + 6_000, by: 'mohamed' };
    const turns = readRunTurns('r', history(workingRun()), [answered]);
    const parked = turns.named.turns.find((t) => t.turn.kind === 'parked');
    expect(parked?.term).toBe('ask-answered');
    expect(parked?.because).toContain('mohamed');
  });

  it('reads the whole run without any asks at all', () => {
    // The asks are an overlay on the log, not a prerequisite for it: a run whose asks could not be
    // read still has an entire transcript, missing only its parks.
    const turns = readRunTurns('r', history(workingRun()));
    expect(turns.named.turns.some((t) => t.turn.kind === 'parked')).toBe(false);
    expect(turns.named.turns.length).toBeGreaterThan(3);
  });
});

/* ───────────────────────── off the wire ───────────────────────── */

describe('fetchRunTurns', () => {
  it('reads the history and the asks, and answers with one reading', async () => {
    stubFetch({
      history: () => json(history([...workingRun(), ev(8, 'WorkflowExecutionCompleted', 10_000)])),
      asks: () => json({ runId: 'sweep-1', asks: [], pending: [] }),
    });
    const read = await fetchRunTurns('sweep-1');
    expect(read.ok).toBe(true);
    if (!read.ok) return;
    expect(read.turns.runId).toBe('sweep-1');
    expect(read.turns.named.verdict.term).toBe('run-finished');
  });

  it('tells GONE apart from broken — 404 is retention, not an outage', async () => {
    stubFetch({ history: () => json({ error: 'no history for that run' }, 404) });
    const read = await fetchRunTurns('old-1');
    expect(read).toEqual({
      ok: false,
      runId: 'old-1',
      gone: true,
      detail: 'Temporal has no history under that id',
    });
  });

  it('does not call an unwell cluster “gone”', async () => {
    // A 502 must never be served as a run that aged out: one means the account is lost forever and
    // the other means ask again in a minute.
    stubFetch({ history: () => json({ error: 'could not read history' }, 502) });
    const read = await fetchRunTurns('sweep-1');
    expect(read.ok).toBe(false);
    if (read.ok) return;
    expect(read.gone).toBe(false);
    expect(read.detail).toContain('502');
  });

  it('draws the transcript even when the asks route is unreachable', async () => {
    stubFetch({
      history: () => json(history([...workingRun(), ev(8, 'WorkflowExecutionCompleted', 10_000)])),
      asks: () => json({ error: 'nope' }, 502),
    });
    const read = await fetchRunTurns('sweep-1');
    expect(read.ok).toBe(true);
    if (!read.ok) return;
    expect(read.turns.named.turns.some((t) => t.turn.kind === 'parked')).toBe(false);
    expect(read.turns.named.verdict.term).toBe('run-finished');
  });
});

describe('fetchRunAsks', () => {
  it('refuses a 200 carrying something that is not a list', async () => {
    // A proxy's error page, a stub, a route answering for the wrong path. Reaching `.map()` inside
    // a render is how a bad shape unmounts the whole surface with the cause buried in a trace.
    stubFetch({ asks: () => json({ asks: { id: 'not-a-list' } }) });
    await expect(fetchRunAsks('r')).resolves.toEqual([]);
  });

  it('answers empty rather than throwing when the network is gone', async () => {
    globalThis.fetch = vi.fn(async () => {
      throw new TypeError('Failed to fetch');
    }) as unknown as typeof fetch;
    await expect(fetchRunAsks('r')).resolves.toEqual([]);
  });
});

/* ───────────────────────── answering one ───────────────────────── */

describe('answerRunAsk', () => {
  /** The answer route, which `stubFetch` does not know about — it is a POST to `/asks/<id>`. */
  function stubAnswer(reply: (body: unknown) => Response): void {
    globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (!url.includes('/asks/')) throw new Error(`unexpected fetch: ${url}`);
      return reply(JSON.parse(String(init?.body ?? '{}')) as unknown);
    }) as typeof fetch;
  }

  it('sends the value and the label, and hands back the settled ask', async () => {
    let sent: unknown;
    stubAnswer((body) => {
      sent = body;
      return json({ ok: true, ask: { id: 'ask-1', state: 'answered', by: 'mo' } });
    });
    const got = await answerRunAsk('sweep-1', 'ask-1', { value: { approve: true }, by: 'mo' });
    expect(sent).toEqual({ value: { approve: true }, by: 'mo' });
    expect(got.ok).toBe(true);
    if (!got.ok) return;
    expect(got.ask.state).toBe('answered');
  });

  it('sends NO label when nobody typed one, so the appliance can use its own', async () => {
    // `""` would be this form asserting "nobody" over a box whose whole purpose is to say who; an
    // absent key is what lets `defaultOperator()` answer instead.
    let sent: Record<string, unknown> = {};
    stubAnswer((body) => {
      sent = body as Record<string, unknown>;
      return json({ ok: true, ask: {} });
    });
    await answerRunAsk('sweep-1', 'ask-1', { value: 1, by: '   ' });
    expect('by' in sent).toBe(false);
  });

  it('a refusal carries the appliance’s own sentence and the field it named', async () => {
    stubAnswer(() => json({ error: '/approve must be boolean', field: '/approve' }, 400));
    const got = await answerRunAsk('sweep-1', 'ask-1', { value: { approve: 'yes' } });
    expect(got).toEqual({ ok: false, refused: '/approve must be boolean', field: '/approve' });
  });

  it('an ask that is no longer pending says WHAT it is now, not that the request was invalid', async () => {
    // 409, not 400: somebody got there first, or the deadline passed. An operator told "invalid"
    // goes looking at their own form for a mistake that is not there.
    stubAnswer(() => json({ error: 'ask ask-1 on run sweep-1 is expired, not pending', state: 'expired' }, 409));
    const got = await answerRunAsk('sweep-1', 'ask-1', { value: 1 });
    expect(got.ok).toBe(false);
    if (got.ok) return;
    expect(got.state).toBe('expired');
    expect(got.refused).toContain('expired');
  });

  it('a request that never arrived says so, rather than throwing into a click handler', async () => {
    globalThis.fetch = vi.fn(async () => {
      throw new TypeError('Failed to fetch');
    }) as unknown as typeof fetch;
    const got = await answerRunAsk('sweep-1', 'ask-1', { value: 1 });
    expect(got.ok).toBe(false);
    if (got.ok) return;
    // The run is exactly as parked as it was — this says the answer did not land, and nothing
    // about the ask.
    expect(got.refused).toContain('did not reach the appliance');
  });
});
