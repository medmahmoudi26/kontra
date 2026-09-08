/**
 * The two routes a parked run is reached through, and the third status dimension they belong to.
 *
 * WHAT THESE PIN THAT `hitl.test.ts` CANNOT. That module is a function over a memo bag; these are
 * about the wiring around it — that reading asks costs one describe and no worker, that the read
 * falls back to the archive once Temporal has dropped the execution, that a refused answer never
 * reaches the workflow, and that `parked`, `running` and `stalled` are three distinct words on
 * EVERY surface a run's state is read from rather than only on the new one.
 *
 * Temporal is faked through the injected `describeRun` and a mocked `signalRun`, because what is
 * under test is the decision and the status code — and a decision proven against a live cluster is
 * one proven against whatever that cluster happened to be doing.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { FastifyInstance } from 'fastify';

import { buildServer } from './server';
import { Repo } from './db/repo';
import { MemoryStore, ObjectStore } from './codec/objectStore';
import { HistoryArchive } from './historyArchive';
import { ASK_MEMO_PREFIX, ANSWER_SIGNAL_PREFIX } from './hitl';
import { mapHistory } from './history';
import type { RunDescription, RunRow } from './temporalClient';

vi.mock('./temporalClient', async (original) => ({
  ...(await original<typeof import('./temporalClient')>()),
  signalRun: vi.fn(async () => undefined),
  listRuns: vi.fn(async () => [] as RunRow[]),
  describeRunHeartbeats: vi.fn(async () => ({})),
}));

const RUN = 'nightly-sweep-2026-08-25';
const APPROVAL = {
  type: 'object',
  properties: { approve: { type: 'boolean' } },
  required: ['approve'],
};

/** One memo entry as `kontra.hitl` writes it. */
function askMemo(id: string, over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    [`${ASK_MEMO_PREFIX}${id}`]: {
      id,
      prompt: `Approve ${id}?`,
      askedAt: Date.now() - 60_000,
      state: 'pending',
      schema: APPROVAL,
      context: { dataset: 'live', n: 12 },
      ...over,
    },
  };
}

function described(over: Partial<RunDescription> = {}): RunDescription {
  return {
    runId: RUN,
    status: 'running',
    type: 'NightlySweep',
    tenant: '',
    startedAt: Date.now() - 300_000,
    closedAt: 0,
    memo: {},
    inFlight: [],
    ...over,
  };
}

interface Ctx {
  app: FastifyInstance;
  backing: MemoryStore;
  store: ObjectStore;
}

function serverFor(describe_: (runId: string) => Promise<RunDescription | undefined>): Ctx {
  const backing = new MemoryStore();
  const store = new ObjectStore({ backing });
  return {
    app: buildServer({ repo: new Repo(':memory:'), store, describeRun: describe_, webRoot: '' }),
    backing,
    store,
  };
}

let ctx: Ctx | undefined;

afterEach(async () => {
  await ctx?.app.close();
  ctx = undefined;
  delete process.env.KONTRA_RUN_TOKEN;
  delete process.env.KONTRA_OPERATOR;
  vi.clearAllMocks();
});

describe('GET /api/runs/:runId/asks', () => {
  it('answers with a LIST, and says which of them are still waiting', async () => {
    ctx = serverFor(async () =>
      described({
        memo: {
          ...askMemo('ask-1'),
          ...askMemo('ask-2', { askedAt: Date.now() - 30_000 }),
          ...askMemo('ask-3', {
            askedAt: Date.now() - 90_000,
            state: 'answered',
            answeredAt: Date.now() - 80_000,
            by: 'mo',
          }),
        },
      })
    );

    const res = await ctx.app.inject({ method: 'GET', url: `/api/runs/${RUN}/asks` });
    expect(res.statusCode).toBe(200);
    const body = res.json();
    expect(body.asks.map((a: { id: string }) => a.id)).toEqual(['ask-3', 'ask-1', 'ask-2']);
    // Two branches parked at once and one already settled: the settled one stays in the account
    // (an archive holding an answer and not its question is the failure this shape prevents) and
    // only the two live ones are offered as answerable.
    expect(body.pending.map((a: { id: string }) => a.id)).toEqual(['ask-1', 'ask-2']);
    expect(body.pending[0].context).toEqual({ dataset: 'live', n: 12 });
    expect(body.pending[0].schema).toEqual(APPROVAL);
    expect(body.pending[0].waitedMs).toBeGreaterThanOrEqual(60_000);
  });

  it('is an empty list, not a 404, for a run that asked nobody anything', async () => {
    ctx = serverFor(async () => described());
    const res = await ctx.app.inject({ method: 'GET', url: `/api/runs/${RUN}/asks` });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toEqual({ runId: RUN, asks: [], pending: [] });
  });

  it('404s only when neither Temporal nor the archive has heard of the run', async () => {
    ctx = serverFor(async () => undefined);
    const res = await ctx.app.inject({ method: 'GET', url: `/api/runs/${RUN}/asks` });
    expect(res.statusCode).toBe(404);
  });

  it('answers from the archive once Temporal has dropped the execution', async () => {
    // Retention drops the execution long before anyone stops caring what was approved, and by
    // whom. The archive is written at the run's CLOSE, when the memo is both final and readable.
    ctx = serverFor(async () => undefined);
    const archive = new HistoryArchive(ctx.store);
    await archive.write(
      {
        runId: RUN,
        startedAt: 1_786_831_339_000,
        closedAt: 1_786_831_633_000,
        memo: askMemo('ask-1', {
          askedAt: 1_786_831_400_000,
          state: 'answered',
          answeredAt: 1_786_831_460_000,
          by: 'mo',
        }),
      },
      mapHistory([])
    );

    const res = await ctx.app.inject({ method: 'GET', url: `/api/runs/${RUN}/asks` });
    expect(res.statusCode).toBe(200);
    const [ask] = res.json().asks;
    expect(ask.prompt).toBe('Approve ask-1?');
    expect(ask.by).toBe('mo');
    // Measured against the run's own close, not against whenever the sweep came round: an
    // unanswered ask's recorded wait must not grow by however late the archiver was.
    expect(ask.waitedMs).toBe(60_000);
  });
});

describe('POST /api/runs/:runId/asks/:askId', () => {
  const answer = (app: FastifyInstance, id: string, body: unknown, token?: string) =>
    app.inject({
      method: 'POST',
      url: `/api/runs/${RUN}/asks/${id}`,
      payload: body as never,
      headers: token ? { authorization: `Bearer ${token}` } : {},
    });

  it('signals the workflow once the answer fits the schema its ask declared', async () => {
    ctx = serverFor(async () => described({ memo: askMemo('ask-1') }));
    const { signalRun } = await import('./temporalClient');

    const res = await answer(ctx.app, 'ask-1', { value: { approve: true }, by: 'mo' });

    expect(res.statusCode).toBe(200);
    expect(res.json().ask.by).toBe('mo');
    expect(signalRun).toHaveBeenCalledWith(RUN, `${ANSWER_SIGNAL_PREFIX}ask-1`, {
      value: { approve: true },
      by: 'mo',
    });
  });

  it('refuses an answer that does not fit, names the field, and leaves the run parked', async () => {
    // A signal is durable and unacknowledged: once it is in the run's history the workflow acts on
    // it. So the refusal has to happen before it, with the run untouched.
    ctx = serverFor(async () => described({ memo: askMemo('ask-1') }));
    const { signalRun } = await import('./temporalClient');

    const res = await answer(ctx.app, 'ask-1', { value: { approve: 'yes' } });

    expect(res.statusCode).toBe(400);
    expect(res.json().field).toBe('/approve');
    expect(signalRun).not.toHaveBeenCalled();
  });

  it('409s rather than 400s on an ask somebody already answered', async () => {
    // The request was well formed and arrived too late. An operator told "invalid" goes looking at
    // their own form for a mistake that is not there.
    ctx = serverFor(async () =>
      described({ memo: askMemo('ask-1', { state: 'answered', answeredAt: Date.now(), by: 'alice' }) })
    );
    const { signalRun } = await import('./temporalClient');

    const res = await answer(ctx.app, 'ask-1', { value: { approve: true } });

    expect(res.statusCode).toBe(409);
    expect(res.json().state).toBe('answered');
    expect(signalRun).not.toHaveBeenCalled();
  });

  it('404s on an ask id that run never published', async () => {
    ctx = serverFor(async () => described({ memo: askMemo('ask-1') }));
    expect((await answer(ctx.app, 'ask-9', { value: { approve: true } })).statusCode).toBe(404);
  });

  it('falls back to this box\'s operator label, and never invents one', async () => {
    // SELF-ASSERTED, NEVER AUTHENTICATION. `KONTRA_OPERATOR` is whoever set it saying so about
    // themselves; unset means the answer is recorded unlabelled rather than attributed to a
    // hostname nobody claimed to be.
    ctx = serverFor(async () => described({ memo: askMemo('ask-1', { schema: undefined }) }));
    const { signalRun } = await import('./temporalClient');

    process.env.KONTRA_OPERATOR = 'mo@appliance';
    await answer(ctx.app, 'ask-1', { value: true });
    expect(signalRun).toHaveBeenLastCalledWith(RUN, `${ANSWER_SIGNAL_PREFIX}ask-1`, {
      value: true,
      by: 'mo@appliance',
    });

    delete process.env.KONTRA_OPERATOR;
    await answer(ctx.app, 'ask-1', { value: true });
    expect(signalRun).toHaveBeenLastCalledWith(RUN, `${ANSWER_SIGNAL_PREFIX}ask-1`, {
      value: true,
      by: '',
    });
  });

  it('is admitted by the run token, exactly as start and stop are', async () => {
    // Answering "yes" to a workflow parked in front of `fleet.up()` provisions cloud machines. It
    // belongs behind the same switch as the other two verbs that can.
    process.env.KONTRA_RUN_TOKEN = 'test-token-0123456789abcdef';
    ctx = serverFor(async () => described({ memo: askMemo('ask-1', { schema: undefined }) }));
    const { signalRun } = await import('./temporalClient');

    expect((await answer(ctx.app, 'ask-1', { value: true })).statusCode).toBe(401);
    expect(signalRun).not.toHaveBeenCalled();
    expect(
      (await answer(ctx.app, 'ask-1', { value: true }, 'test-token-0123456789abcdef')).statusCode
    ).toBe(200);
  });
});

describe('parked, running and stalled everywhere run state is read', () => {
  it('reads the third dimension on GET /api/runs/:runId', async () => {
    ctx = serverFor(async () => described({ memo: askMemo('ask-1') }));
    const res = await ctx.app.inject({ method: 'GET', url: `/api/runs/${RUN}` });
    const body = res.json();
    // BESIDE the other two dimensions, never merged into either (ADR 0017).
    expect(body.execution).toBe('running');
    expect(body.activity).toBe('parked');
    expect(body.asks).toHaveLength(1);
    expect(body.parkedForMs).toBeGreaterThanOrEqual(60_000);
  });

  it('tells running, parked and stalled apart on the same route', async () => {
    const now = Date.now();
    const cases: Array<[string, Partial<RunDescription>, string | null]> = [
      ['running', { inFlight: [work({ startedAt: now - 1_000 })] }, 'running'],
      ['parked', { memo: askMemo('ask-1') }, 'parked'],
      ['stalled', { inFlight: [work({ attempt: 4, startedAt: 0 })] }, 'stalled'],
      // A closed run is none of the three, and the route says so rather than picking one.
      ['closed', { status: 'completed', closedAt: now }, null],
    ];
    for (const [name, over, expected] of cases) {
      ctx = serverFor(async () => described(over));
      const body = (await ctx.app.inject({ method: 'GET', url: `/api/runs/${RUN}` })).json();
      expect(body.activity, name).toBe(expected);
      await ctx.app.close();
      ctx = undefined;
    }
  });

  it('reads the same three words on the run LIST', async () => {
    // The list is where run state is read most, and a list that reported only Temporal's verdict
    // would show a run waiting on a human as one making progress.
    const { listRuns } = await import('./temporalClient');
    vi.mocked(listRuns).mockResolvedValue([
      row('parked-run'),
      row('busy-run'),
      row('stuck-run'),
      row('done-run', { status: 'completed', closedAt: Date.now() }),
    ]);
    const byId: Record<string, Partial<RunDescription>> = {
      'parked-run': { memo: askMemo('ask-1') },
      'busy-run': { inFlight: [work({ startedAt: Date.now() - 1_000 })] },
      'stuck-run': { inFlight: [work({ attempt: 6, startedAt: 0 })] },
    };
    ctx = serverFor(async (runId) => described({ runId, ...byId[runId] }));

    const rows = (await ctx.app.inject({ method: 'GET', url: '/api/runs' })).json();
    const activity = Object.fromEntries(
      rows.map((r: { runId: string; activity: string | null }) => [r.runId, r.activity])
    );
    expect(activity).toEqual({
      'parked-run': 'parked',
      'busy-run': 'running',
      'stuck-run': 'stalled',
      // A CLOSED RUN IS NEVER DESCRIBED for this. `null` is the answer, and it costs no RPC.
      'done-run': null,
    });
    const parked = rows.find((r: { runId: string }) => r.runId === 'parked-run');
    expect(parked.pendingAsks).toBe(1);
  });
});

function work(over: Record<string, number> = {}) {
  return {
    attempt: 1,
    scheduledAt: Date.now() - 300_000,
    startedAt: 0,
    heartbeatAt: 0,
    ...over,
  };
}

function row(runId: string, over: Partial<RunRow> = {}): RunRow {
  return {
    runId,
    status: 'running',
    type: 'NightlySweep',
    tenant: '',
    startedAt: Date.now() - 600_000,
    closedAt: 0,
    dispatches: 0,
    memo: {},
    ...over,
  };
}
