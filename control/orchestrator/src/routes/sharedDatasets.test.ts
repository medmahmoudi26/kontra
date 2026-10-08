/**
 * The cross-workspace boundary as HTTP (ADR 0053): the admission posture, the address in the path,
 * and the two refusals that must look identical from outside.
 *
 * WHAT THIS FILE PINS THAT NOTHING ELSE CAN. `data/sharedRead.test.ts` proves the mechanism against
 * two real DuckLakes — READ_ONLY on the attach, the grant as the admission, the sort grammar. What
 * only a route test can prove is the POSTURE: that all four routes fail CLOSED when no token is
 * configured, and that a grant cannot be made by a caller the read path would refuse. The repo has
 * shipped the inverse of that pairing before — `routes/datasets.ts` records that "a route that can
 * read every dataset was gated while the route that can DELETE one was not" — so the two halves
 * are asserted together, per route.
 *
 * The lake is never reached: every case here is admitted or refused before a DuckLake is opened, and
 * the one case that would read rows asserts the 404 instead. That keeps the suite fast and keeps the
 * subject of the file the boundary rather than the query.
 */

import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import Fastify, { type FastifyInstance } from 'fastify';
import { afterAll, beforeEach, describe, expect, it } from 'vitest';

import { MemoryStore, ObjectStore } from '../codec/objectStore';
import { SharedDatasetStore } from '../data/sharedDatasets';
import { registerSharedDatasetRoutes } from './sharedDatasets';

const dir = mkdtempSync(join(tmpdir(), 'kontra-shared-routes-'));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const TOKEN = 'test-explore-token';
const AUTH = { authorization: `Bearer ${TOKEN}` };

let seq = 0;
let shares: SharedDatasetStore;

function serve(): FastifyInstance {
  const app = Fastify();
  const store = new ObjectStore({ backing: new MemoryStore(), prefix: '' });
  // A lake that is NOT configured: `lakeEnabled` is false, so nothing here can accidentally open a
  // DuckLake. Every assertion below is about admission, which happens first.
  registerSharedDatasetRoutes(app, { store, lake: {}, shares });
  return app;
}

/** The route surface is fail-closed, so every admitted case needs the var the guard reads. */
async function withToken(fn: () => Promise<void>): Promise<void> {
  const before = process.env.KONTRA_EXPLORE_TOKEN;
  process.env.KONTRA_EXPLORE_TOKEN = TOKEN;
  try {
    await fn();
  } finally {
    if (before === undefined) delete process.env.KONTRA_EXPLORE_TOKEN;
    else process.env.KONTRA_EXPLORE_TOKEN = before;
  }
}

/** And the refused cases need it ABSENT — including `KONTRA_STATE_TOKEN`, which `EXPLORE_TOKEN_VARS`
 *  falls back to, so a box that sets only the state token still admits this surface. */
async function withNoToken(fn: () => Promise<void>): Promise<void> {
  const explore = process.env.KONTRA_EXPLORE_TOKEN;
  const state = process.env.KONTRA_STATE_TOKEN;
  delete process.env.KONTRA_EXPLORE_TOKEN;
  delete process.env.KONTRA_STATE_TOKEN;
  try {
    await fn();
  } finally {
    if (explore !== undefined) process.env.KONTRA_EXPLORE_TOKEN = explore;
    if (state !== undefined) process.env.KONTRA_STATE_TOKEN = state;
  }
}

beforeEach(async () => {
  shares = new SharedDatasetStore({ url: join(dir, `s${(seq += 1)}.db`) });
  await shares.ensureSchema();
});

/** The four routes, so the posture assertions below cannot miss one that is added later. */
const ROUTES: Array<{ method: 'GET' | 'PUT' | 'DELETE'; url: string }> = [
  { method: 'GET', url: '/api/datasets/shared' },
  { method: 'PUT', url: '/api/datasets/shared/bugbounty/lame' },
  { method: 'DELETE', url: '/api/datasets/shared/bugbounty/lame' },
  { method: 'GET', url: '/api/datasets/shared/bugbounty/lame/preview' },
];

describe('the shared surface fails CLOSED', () => {
  it('answers 503 on EVERY route when no token is configured', async () => {
    // The direction that matters for a boundary. `checkOptionalBearer` — what the tag routes beside
    // this use — would answer 200 here and share the Dataset on an unconfigured box; `auth.ts` says
    // in its own words that nothing reading live scan data may use it, and a read of another
    // workspace's lake is live scan data by construction.
    await withNoToken(async () => {
      const app = serve();
      for (const r of ROUTES) {
        const res = await app.inject({ method: r.method, url: r.url });
        expect(res.statusCode, `${r.method} ${r.url}`).toBe(503);
        expect(res.json().error).toMatch(/KONTRA_EXPLORE_TOKEN/);
      }
      // AND NOTHING WAS WRITTEN. A 503 that had already recorded the grant would be the worst of
      // both: refused on the wire, exposed in the store.
      expect(await shares.list()).toEqual([]);
      await app.close();
    });
  });

  it('answers 401 on EVERY route for a wrong bearer', async () => {
    await withToken(async () => {
      const app = serve();
      for (const r of ROUTES) {
        const res = await app.inject({
          method: r.method,
          url: r.url,
          headers: { authorization: 'Bearer nope' },
        });
        expect(res.statusCode, `${r.method} ${r.url}`).toBe(401);
      }
      expect(await shares.list()).toEqual([]);
      await app.close();
    });
  });
});

describe('granting and revoking', () => {
  it('grants, is idempotent, and keeps the FIRST sharedAt across repeats', async () => {
    await withToken(async () => {
      const app = serve();
      const first = await app.inject({
        method: 'PUT',
        url: '/api/datasets/shared/bugbounty/lame',
        headers: AUTH,
      });
      expect(first.statusCode).toBe(200);
      expect(first.json()).toMatchObject({
        shared: true,
        workspace: 'bugbounty',
        name: 'lame',
        kind: 'output',
      });
      const again = await app.inject({
        method: 'PUT',
        url: '/api/datasets/shared/bugbounty/lame',
        headers: AUTH,
      });
      // The audit answer to "since when has this been exposed" must not be "since you last asked".
      expect(again.json().sharedAt).toBe(first.json().sharedAt);
      expect((await shares.list()).length).toBe(1);
      await app.close();
    });
  });

  it('takes the KIND as a query parameter, so a standalone list is its own address', async () => {
    await withToken(async () => {
      const app = serve();
      const res = await app.inject({
        method: 'PUT',
        url: '/api/datasets/shared/bugbounty/scope_h1paid?kind=standalone',
        headers: AUTH,
      });
      expect(res.json()).toMatchObject({ kind: 'standalone' });
      // And granting the standalone did NOT grant an output table of the same name.
      expect(await shares.get('bugbounty', 'scope_h1paid', 'output')).toBeUndefined();
      await app.close();
    });
  });

  it('revokes, and says whether a row was actually removed', async () => {
    await withToken(async () => {
      const app = serve();
      await shares.share('bugbounty', 'lame', 'output');
      const first = await app.inject({
        method: 'DELETE',
        url: '/api/datasets/shared/bugbounty/lame',
        headers: AUTH,
      });
      expect(first.json()).toMatchObject({ shared: false, removed: true });
      // Revoking what is not there is SUCCESS — the caller asked for it not to be shared, and it is
      // not — and `removed` is what keeps "already closed" distinguishable from "just closed".
      const again = await app.inject({
        method: 'DELETE',
        url: '/api/datasets/shared/bugbounty/lame',
        headers: AUTH,
      });
      expect(again.statusCode).toBe(200);
      expect(again.json()).toMatchObject({ removed: false });
      await app.close();
    });
  });

  it('refuses a workspace name the read path could not turn into an address — 400, not 502', async () => {
    // `assertWorkspaceName` is the SAME validator `workspaceAddress` uses. A grant stored under a
    // name no reader can resolve would be an exposure on the operator's screen that nothing could
    // ever use: a lie in the other direction, and one that costs a 400 to prevent.
    await withToken(async () => {
      const app = serve();
      const res = await app.inject({
        method: 'PUT',
        url: '/api/datasets/shared/Client_A/lame',
        headers: AUTH,
      });
      expect(res.statusCode).toBe(400);
      expect(res.json().error).toMatch(/lowercase letters, digits and dashes/);
      await app.close();
    });
  });

  it('refuses a Dataset name that is not its own safeName — 400', async () => {
    await withToken(async () => {
      const app = serve();
      const res = await app.inject({
        method: 'PUT',
        url: `/api/datasets/shared/bugbounty/${encodeURIComponent('lame"; DROP TABLE x --')}`,
        headers: AUTH,
      });
      expect(res.statusCode).toBe(400);
      expect(await shares.list()).toEqual([]);
      await app.close();
    });
  });
});

describe('the exposure listing', () => {
  it('lists every workspace by default and STATES its scope', async () => {
    // A cross-workspace listing whose breadth is implicit is how an operator comes to believe they
    // have audited the boundary when they have audited one corner of it.
    await withToken(async () => {
      const app = serve();
      await shares.share('bugbounty', 'lame', 'output');
      await shares.share('scraping', 'pages', 'output');
      const all = await app.inject({ url: '/api/datasets/shared', headers: AUTH });
      expect(all.json().scope).toBe('all-workspaces');
      expect(all.json().entries.map((e: { workspace: string }) => e.workspace)).toEqual([
        'bugbounty',
        'scraping',
      ]);
      // Each entry carries where it POINTS, derived by `workspaceAddress` — the same derivation the
      // read path uses, so the two cannot disagree about which bucket a reader opens.
      expect(all.json().entries[0].address).toMatchObject({ bucket: 'ws-bugbounty' });

      const one = await app.inject({
        url: '/api/datasets/shared?workspace=scraping',
        headers: AUTH,
      });
      expect(one.json().scope).toBe('scraping');
      expect(one.json().entries.length).toBe(1);
      await app.close();
    });
  });
});

describe('the read refuses', () => {
  it('404s an ungranted Dataset, with a sentence true whether or not it exists', async () => {
    // Deliberately ONE status for both refusals — "no grant" and "the grant points at nothing" —
    // so this surface cannot become a way to ask whether another workspace holds a table of a given
    // name. Matched on the TYPE in the route, never on the message text, because the console prints
    // the sentence. The second refusal needs a real catalog to produce, so it is pinned in
    // `data/sharedRead.test.ts` ("a grant pointing at a Dataset the catalog does not hold fails
    // LOUDLY"), against two real DuckLakes.
    await withToken(async () => {
      const app = serve();
      const res = await app.inject({
        url: '/api/datasets/shared/bugbounty/lame/preview',
        headers: AUTH,
      });
      expect(res.statusCode).toBe(404);
      expect(res.json().error).toMatch(/is shared from workspace "bugbounty"/);
      await app.close();
    });
  });

  it('says NO LAKE rather than answering zero rows, when the install has none', async () => {
    // The direction this has to fail. A listing may answer `[]` for an unconfigured lake — "no
    // lake" and "no datasets" are one answer to "what is in the lake". A granted cross-workspace
    // read cannot: zero rows would read as "the owner's Dataset is empty", which is a claim about
    // another workspace's data that this process is in no position to make.
    await withToken(async () => {
      const app = serve();
      await shares.share('bugbounty', 'lame', 'output');
      const res = await app.inject({
        url: '/api/datasets/shared/bugbounty/lame/preview',
        headers: AUTH,
      });
      expect(res.statusCode).toBe(502);
      expect(res.json().error).toMatch(/no lake is configured/);
      await app.close();
    });
  });
});
