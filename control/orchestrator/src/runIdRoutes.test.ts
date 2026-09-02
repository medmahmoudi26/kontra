/**
 * ONE RUN ID, SEVENTEEN ROUTES — that the string an operator addressed a Run by is the string every
 * route resolves, whichever half of an operation it is.
 *
 * WHY THE PAIR IS THE TEST AND NEITHER HALF IS. A route that decodes `:runId` a second time is
 * perfectly self-consistent: write through it, read through it, and the two agree on whatever
 * mangled key it produced. The bug is only visible when the WRITE and the READ are different
 * routes — stop a run, then read the run you stopped — and until slice 12 put the handlers in one
 * directory, those two sat 1,800 lines apart. So every case below drives a pair.
 *
 * WHAT THE MEASUREMENT SAID. Fastify 5 / find-my-way 9 already percent-decode a path parameter
 * before a handler sees it (`lib/url-sanitizer.js`: `safeDecodeURI` flags any `%XX` that is a
 * reserved character and `safeDecodeURIComponent` resolves it). Verified over real HTTP, not
 * `inject`: `GET /api/runs/kontra-fleet%2Fdns` arrives with `req.params.runId === 'kontra-fleet/dns'`.
 * A handler that then calls `decodeURIComponent` on it decodes the id TWICE, which is
 *
 *   - a 500 `URI malformed` for any id carrying a literal `%` (the second decode throws URIError),
 *   - a DIFFERENT id for any id whose text contains an escape sequence.
 *
 * Both clients encode exactly once and identically — the browser with `encodeURIComponent`
 * (`frontend/src/run/api.ts`), the CLI with Go's `url.PathEscape`, which escapes `/` — so the
 * second decode has no input it is right about. `routes/runId.ts` is the one place that says this;
 * this file is what fails if a seventeenth route decides otherwise.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { FastifyInstance } from 'fastify';

import { buildServer } from './server';
import { Repo } from './db/repo';
import { MemoryStore, ObjectStore } from './codec/objectStore';
import { DatasetRecordStore } from './data/datasetRecords';

/** The ids the routes are asked about, and the mutable log each fake authority appends to. */
const seen = vi.hoisted(() => ({ stopped: [] as string[], probed: [] as string[] }));

vi.mock('./temporalClient', async (original) => ({
  ...(await original<typeof import('./temporalClient')>()),
  describeRun: vi.fn(async () => undefined),
  listRuns: vi.fn(async () => []),
  describeRunHeartbeats: vi.fn(async () => ({})),
  fetchRunHistory: vi.fn(async () => undefined),
  signalRun: vi.fn(async () => undefined),
}));

// The two authorities a `:runId` route reaches that are NOT injectable through `buildServer`.
// Faked to record the id rather than to answer anything: what is under test is which string
// arrived, and a real answer would only add a cluster to the failure modes.
vi.mock('./workflowControl', async (original) => ({
  ...(await original<typeof import('./workflowControl')>()),
  stopRun: vi.fn(async (runId: string) => {
    seen.stopped.push(runId);
    return { runId, state: 'cancelled' };
  }),
}));

vi.mock('./probe', async (original) => ({
  ...(await original<typeof import('./probe')>()),
  readProbe: vi.fn(async (runId: string) => {
    seen.probed.push(runId);
    return { runId, status: 'running', results: [], isolated: [], done: false };
  }),
}));

/**
 * THE THREE SHAPES, and each is a fact about this repo rather than a fuzz case.
 *
 * `kontra-fleet/dns` is the id of every fleet bring-up and teardown here — a slash is the one
 * character that must be encoded to survive a path segment, and it is the case the issue was
 * written about. The second is what an id looks like once somebody composes one out of a string
 * that is ALREADY encoded; nothing mints one today, and it is here because it is the shape that
 * failed silently rather than loudly. The third is an ordinary name with a percent in it.
 */
const IDS = [
  'kontra-fleet/dns',
  'kontra-fleet/dns%2Fteardown',
  'sweep-50%-done',
];

interface Ctx {
  app: FastifyInstance;
  records: DatasetRecordStore;
  described: string[];
}

let ctx: Ctx;

beforeEach(() => {
  const described: string[] = [];
  const records = new DatasetRecordStore({ url: ':memory:' });
  ctx = {
    records,
    described,
    app: buildServer({
      repo: new Repo(':memory:'),
      store: new ObjectStore({ backing: new MemoryStore() }),
      records,
      // The EXECUTION authority, faked to record the id it was asked about. `undefined` is "no
      // such run", which is the honest answer for every id below and is not what is asserted.
      describeRun: async (runId: string) => {
        described.push(runId);
        return undefined;
      },
      webRoot: '',
    }),
  };
  seen.stopped.length = 0;
  seen.probed.length = 0;
});

afterEach(async () => {
  await ctx.app.close();
  vi.clearAllMocks();
});

describe('a run id survives every route that takes one', () => {
  /**
   * THE PAIR: stop a run, then read the run you stopped.
   *
   * `POST /api/runs/:runId/stop` decoded a second time and `GET /api/runs/:runId` did not, so for
   * two of the three ids below the operator cancelled one execution and was then shown the status
   * of a different one — or, for the third, was told the server had an internal error.
   */
  it.each(IDS)('stop and read resolve the same run: %s', async (runId) => {
    const path = encodeURIComponent(runId);

    const stop = await ctx.app.inject({ method: 'POST', url: `/api/runs/${path}/stop` });
    const read = await ctx.app.inject({ method: 'GET', url: `/api/runs/${path}` });

    // Neither half may fail on the id itself: a 500 here is the second decode throwing URIError.
    expect(stop.statusCode).not.toBe(500);
    expect(read.statusCode).not.toBe(500);
    // The write and the read agree, and they agree on what the operator typed. Asserted as a SET
    // because the invariant is which id was resolved, not how many describes a read costs.
    expect(seen.stopped).toEqual([runId]);
    expect(new Set(ctx.described)).toEqual(new Set([runId]));
  });

  /**
   * The other decoding route against the same non-decoding read. `GET /api/probes/:runId` is how a
   * probe's answer is fetched for a run the Runs list already showed, so the two are read one after
   * the other about the same execution by construction.
   */
  it.each(IDS)('the probe reading is about the run that was listed: %s', async (runId) => {
    const path = encodeURIComponent(runId);

    const probe = await ctx.app.inject({ method: 'GET', url: `/api/probes/${path}` });
    await ctx.app.inject({ method: 'GET', url: `/api/runs/${path}` });

    expect(probe.statusCode).not.toBe(500);
    expect(seen.probed).toEqual([runId]);
    expect(new Set(ctx.described)).toEqual(new Set([runId]));
  });

  /**
   * The other decoding route on the run surface, against the read beside it. Both reach the SAME
   * `describeRun`, so a disagreement is two entries in one log rather than a comparison across
   * fakes — which is the cheapest possible statement of the invariant.
   */
  it.each(IDS)('the asks and the status describe one execution: %s', async (runId) => {
    const path = encodeURIComponent(runId);

    const asks = await ctx.app.inject({ method: 'GET', url: `/api/runs/${path}/asks` });
    const read = await ctx.app.inject({ method: 'GET', url: `/api/runs/${path}` });

    expect(asks.statusCode).not.toBe(500);
    expect(read.statusCode).not.toBe(500);
    // Two describes — both halves asked — and one distinct id between them: one execution.
    expect(ctx.described.length).toBe(2);
    expect(new Set(ctx.described)).toEqual(new Set([runId]));
  });

  /**
   * THE DURABLE HALF: a tag written under an id is the tag removed under it (ADR 0029 §1).
   *
   * These four record writes never decoded, which is why they were always right — this is the net
   * that goes red if a later edit "fixes" them by adding one, and the only round trip here whose
   * two halves both reach a store rather than a fake.
   */
  it.each(IDS)('a tag is written and removed under one key: %s', async (runId) => {
    const path = encodeURIComponent(runId);

    const tagged = await ctx.app.inject({
      method: 'POST',
      url: `/api/datasets/runs/${path}/tags`,
      payload: { tag: 'keep' },
    });
    expect(tagged.statusCode).toBe(200);
    expect(await ctx.records.get(runId)).toMatchObject({ runId, tags: ['keep'] });

    const untagged = await ctx.app.inject({
      method: 'DELETE',
      url: `/api/datasets/runs/${path}/tags/keep`,
    });
    expect(untagged.json()).toMatchObject({ runId, tags: [] });
  });

  /**
   * The live-progress beat and the poll that reads it — the one pair whose two halves are the same
   * map, so a divergence would drop a running fleet's progress on the floor with nothing failing.
   */
  it.each(IDS)('a progress beat is read back under the id it was posted for: %s', async (runId) => {
    const path = encodeURIComponent(runId);

    await ctx.app.inject({
      method: 'POST',
      url: `/api/runs/${path}/progress`,
      payload: { node: 'n1', progress: { done: 3 }, total: 6 },
    });
    const got = await ctx.app.inject({ method: 'GET', url: `/api/runs/${path}/progress` });

    expect(got.json()).toMatchObject({ nodes: { n1: { progress: { done: 3 } } } });
  });
});
