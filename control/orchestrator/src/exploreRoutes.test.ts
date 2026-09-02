/**
 * The new API surface of ADR 0017: the two-dimensional lifecycle, the token-gated
 * exact-dispatch explore manifest, and the bounded operational summaries.
 *
 * These are route-level tests against a real Fastify instance, a real SQLite status store
 * and a real DuckLake on a local data path — so the assertions are about what a client
 * actually receives, not about what the modules would return in isolation.
 *
 * THE ADDRESSING MODEL THESE PIN DOWN: output is ONE TABLE PER ACTOR under
 * `output/<actor>/version=<v>/dt=<dispatch>/`, so a manifest carries one `datasets` entry
 * per actor. Several Method calls of one Actor in one Run share ONE dataset, and `dt` (the
 * dispatch time to the second) is what scopes the presigned URLs.
 */

import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';

import { MemoryStore, ObjectStore } from './codec/objectStore';
import { Repo } from './db/repo';
import { MATERIALIZATION_SCHEMA_VERSION } from './data/materialization';
import { MaterializationStore } from './data/materializationStore';
import { dtPartition, resetLakeConnections, writeDatasetParquet, type LakeConfig } from './data/parquet';
import { SummaryStore } from './data/summaries';
import { viewName } from './data/explore';
import { buildServer } from './server';

const TOKEN = 'a'.repeat(64);
const RUN = 'run-explore-1';
const ACTOR = 'crawl4ai';
const VERSION = '0.5.0';
/** The server-minted dispatch instant. `dt` is derived from it — nothing else. */
const RUN_STARTED = 1_700_000_000_000;
const DT = dtPartition(RUN_STARTED);

/** One `ExploreDataset` as the route serialises it. */
interface Dataset {
  actor: string;
  version: string;
  dt: string;
  view: string;
  urls: string[];
  columns: Array<{ name: string; type: string }>;
  rows: number;
  nodes: string[];
  state: string;
  error: string | null;
}

interface Ctx {
  app: FastifyInstance;
  store: ObjectStore;
  status: MaterializationStore;
  summaries: SummaryStore;
  lake: Partial<LakeConfig>;
  blobs: string;
  /** Materialize one node's output into the local lake, the way the materializer would. */
  materialize: (
    node: string,
    units: unknown[],
    over?: { runId?: string; runStartedAt?: number }
  ) => Promise<void>;
}

function build(): Ctx {
  const dir = mkdtempSync(join(tmpdir(), 'kontra-explore-'));
  const dataPath = join(dir, 'data');
  const blobs = join(dir, 'blobs');
  mkdirSync(dataPath, { recursive: true });
  mkdirSync(blobs, { recursive: true });
  const store = new ObjectStore({
    backing: new MemoryStore(),
    prefix: '',
    publicEndpoint: 'http://localhost:8333',
  });
  const lake: Partial<LakeConfig> = {
    catalog: join(dir, 'cat.ducklake'),
    dataPath: `${dataPath}/`,
    s3: false,
    blobBase: `${blobs}/`,
  };
  const status = new MaterializationStore({ url: ':memory:' });
  const summaries = new SummaryStore({ url: ':memory:' });
  const app = buildServer({
    repo: new Repo(':memory:'),
    store,
    materialization: status,
    summaries,
    lake,
    webRoot: '',
    // HERMETIC ABOUT TEMPORAL, and it has to be stated rather than assumed.
    //
    // `RunLifecycle.read` rethrows a Temporal error when the materialization authority has
    // nothing to say, deliberately — an outage must not render as "no such run". So every
    // assertion below about a run with no materialization records was, until this line, an
    // assertion against whatever cluster happened to be listening on the machine running it.
    // It passed on a dev box with a Temporal dev server up and 502'd in CI, where the failure
    // presented as a 10-second connect timeout rather than as a missing dependency.
    //
    // Exactly the trap the beforeEach below already guards for the token: an ambient
    // environment turning a fail-closed assertion green.
    describeRun: async () => undefined,
  });
  let seq = 0;
  return {
    app,
    store,
    status,
    summaries,
    lake,
    blobs,
    async materialize(node, units, over = {}) {
      const name = `u${seq++}.json`;
      const body = JSON.stringify(units);
      writeFileSync(join(blobs, name), body);
      const src = join(mkdtempSync(join(tmpdir(), 'kontra-explore-src-')), 'blob.json');
      writeFileSync(
        src,
        JSON.stringify({
          results: [
            { $ref: { key: name, sha256: createHash('sha256').update(body).digest('hex'), size: body.length } },
          ],
          failures: [],
        })
      );
      await writeDatasetParquet(
        store,
        {
          sha256: 'x',
          actor: ACTOR,
          version: VERSION,
          runId: over.runId ?? RUN,
          node,
          // `dt` comes from HERE. A record whose runStartedAt disagrees with this addresses a
          // different partition directory and finds no files — same as in production.
          runStartedAt: over.runStartedAt ?? RUN_STARTED,
        },
        { ...lake, sourceUri: src }
      );
    },
  };
}

const key = (node: string) => ({
  runId: RUN,
  actor: ACTOR,
  version: VERSION,
  node,
  schemaVersion: MATERIALIZATION_SCHEMA_VERSION,
});

/**
 * The object key inside a presigned URL, decoded. The signer percent-encodes `=`, so
 * `version=0.5.0` reaches the wire as `version%3D0.5.0`; decoding lets the assertions read
 * like the storage layout they are about.
 */
const keyOf = (url: string) => decodeURIComponent(new URL(url).pathname);

describe('GET /api/runs/:runId/explore', () => {
  let ctx: Ctx;

  beforeEach(async () => {
    resetLakeConnections();
    // Both vars are cleared first: `checkBearer` falls back from KONTRA_EXPLORE_TOKEN to
    // KONTRA_STATE_TOKEN, so an ambient KONTRA_STATE_TOKEN in the developer's shell would
    // silently satisfy the "no token configured" case and turn a fail-closed assertion
    // green. Caught exactly that way — the suite passed until it was run with `.env` sourced.
    delete process.env.KONTRA_EXPLORE_TOKEN;
    delete process.env.KONTRA_STATE_TOKEN;
    process.env.KONTRA_EXPLORE_TOKEN = TOKEN;
    ctx = build();
    await ctx.status.ensureSchema();
  });

  afterEach(async () => {
    delete process.env.KONTRA_EXPLORE_TOKEN;
    delete process.env.KONTRA_STATE_TOKEN;
    await ctx.app.close();
  });

  const get = (url: string, token: string | null = TOKEN) =>
    ctx.app.inject({
      method: 'GET',
      url,
      headers: token ? { authorization: `Bearer ${token}` } : {},
    });

  it('fails CLOSED when no token is configured', async () => {
    // Presigning without authentication would make "this token scopes access to one
    // dispatch" false by construction, so there is deliberately no open fallback to inherit.
    delete process.env.KONTRA_EXPLORE_TOKEN;
    delete process.env.KONTRA_STATE_TOKEN; // the fallback must be gone too, or this is vacuous
    const res = await get(`/api/runs/${RUN}/explore`, null);
    expect(res.statusCode).toBe(503);
    expect(res.json().error).toMatch(/KONTRA_EXPLORE_TOKEN/);
  });

  it('rejects a wrong token', async () => {
    expect((await get(`/api/runs/${RUN}/explore`, 'b'.repeat(64))).statusCode).toBe(401);
  });

  it('rejects a token of the right value but no Bearer prefix', async () => {
    const res = await ctx.app.inject({
      method: 'GET',
      url: `/api/runs/${RUN}/explore`,
      headers: { authorization: TOKEN },
    });
    expect(res.statusCode).toBe(401);
  });

  it('accepts KONTRA_STATE_TOKEN as the fallback credential', async () => {
    delete process.env.KONTRA_EXPLORE_TOKEN;
    process.env.KONTRA_STATE_TOKEN = TOKEN;
    expect((await get(`/api/runs/${RUN}/explore`)).statusCode).toBe(200);
  });

  it('returns labelled datasets — never a physical table name or a node id', async () => {
    await ctx.materialize('n1', [{ host: 'a.example', status_code: 200 }]);
    await ctx.status.claim(key('n1'), RUN_STARTED);
    await ctx.status.complete(key('n1'), { rows: 1, bytes: 100, snapshotId: 1, tbl: ACTOR });

    const body = (await get(`/api/runs/${RUN}/explore`)).json();
    // ONE ENTRY PER ACTOR — and no `nodes` array on the manifest at all. Node ids are a
    // scheduling detail; they are not how output is addressed.
    expect(body.nodes).toBeUndefined();
    expect(body.datasets).toHaveLength(1);
    const d: Dataset = body.datasets[0];

    // The view the CLI creates is the ACTOR name — that is what an operator types.
    expect(d.view).toBe('crawl4ai');
    expect(d.view).toBe(viewName(ACTOR));
    expect(d).toMatchObject({ actor: ACTOR, version: VERSION, dt: DT, state: 'complete', rows: 1 });
    expect(d.nodes).toEqual(['n1']); // still visible for DIAGNOSIS, never as an address
    expect(d.columns.map((c) => c.name)).toContain('host');
    expect(d.urls[0]).toContain('X-Amz-Signature='); // a real presigned GET, not a path

    // The operator-facing fields must not carry a hashed physical identity — the whole point
    // is that nobody types `ds_cc1dcc89c996f795`. (Under the old scheme these WERE the hash.)
    expect(JSON.stringify([d.view, d.actor, d.version, d.dt])).not.toMatch(/ds_[0-9a-f]{16}/);

    // The URL necessarily carries the storage path, because that is where the file lives —
    // it is opaque machinery the CLI hands to DuckDB. What matters is that the path is the
    // legible layout and that it is scoped to this dispatch.
    const path = keyOf(d.urls[0]!);
    expect(path).toContain(`output/${ACTOR}/`);
    expect(path).toContain(`version=${VERSION}/dt=${DT}/`);
    expect(path).not.toMatch(/ds_[0-9a-f]{16}/);
  });

  it('folds a sharded dispatch of one actor into ONE dataset', async () => {
    // Five shards of one actor is five nodes and one dataset. Rows are summed and the node
    // ids are kept for diagnosis; there is no per-node view to pick between.
    await ctx.materialize('n1', [{ host: 'a.example' }]);
    await ctx.materialize('n2', [{ host: 'b.example' }]);
    for (const n of ['n1', 'n2']) {
      await ctx.status.claim(key(n), RUN_STARTED);
      await ctx.status.complete(key(n), { rows: 1, bytes: 50, snapshotId: 1, tbl: ACTOR });
    }

    const body = (await get(`/api/runs/${RUN}/explore`)).json();
    expect(body.datasets).toHaveLength(1);
    const d: Dataset = body.datasets[0];
    expect(d.view).toBe('crawl4ai');
    expect(d.nodes).toEqual(['n1', 'n2']);
    expect(d.rows).toBe(2);
    expect(d.urls.length).toBeGreaterThanOrEqual(2); // both shards' files, one dataset
  });

  it('scopes presigned URLs to THIS dispatch only', async () => {
    await ctx.materialize('n1', [{ host: 'mine' }]);
    await ctx.status.claim(key('n1'), RUN_STARTED);
    await ctx.status.complete(key('n1'), { rows: 1, bytes: 100, snapshotId: 1, tbl: ACTOR });

    // A SECOND dispatch of the SAME actor into the same table — a different run at a
    // different instant, so a different `dt=` directory. Its files must not be presigned here.
    const otherStarted = RUN_STARTED + 60_000;
    const otherDt = dtPartition(otherStarted);
    expect(otherDt).not.toBe(DT);
    await ctx.materialize('n1', [{ host: 'theirs' }], {
      runId: 'someone-elses-run',
      runStartedAt: otherStarted,
    });

    const body = (await get(`/api/runs/${RUN}/explore`)).json();
    const urls: string[] = body.datasets.flatMap((d: Dataset) => d.urls);
    expect(urls.length).toBeGreaterThan(0);
    for (const u of urls) {
      // `dt` is the dispatch to the SECOND, so the partition directory holds one dispatch —
      // that physical exclusivity, not a generated WHERE clause, is the access boundary.
      expect(keyOf(u)).toContain(`/dt=${DT}/`);
      expect(keyOf(u)).not.toContain(`/dt=${otherDt}/`);
    }
  });

  it('shows a FAILED dataset as failed, with its reason — never as an empty result', async () => {
    // The defect ADR 0017 exists to remove: "found nothing" and "materialization broke"
    // must not reach the operator as the same thing.
    await ctx.status.claim(key('n9'), RUN_STARTED);
    await ctx.status.fail(key('n9'), 'unit object verification failed (missing: units/…/u3.json)');

    const body = (await get(`/api/runs/${RUN}/explore`)).json();
    // Named as failed BY ACTOR — that is the grain an operator addresses.
    expect(body.failed).toEqual([ACTOR]);
    const d: Dataset = body.datasets.find((x: Dataset) => x.actor === ACTOR);
    expect(d).toBeDefined();
    // Present, and presented as a FAILURE with the reason — not as a healthy zero-row dataset.
    expect(d.state).toBe('failed');
    expect(d.error).toMatch(/unit object verification failed/);
    expect(d.urls).toEqual([]);
    expect(body.materialization.failed).toBeGreaterThanOrEqual(1);
  });

  it('shows a zero-row SUCCESS as a dataset, not as a missing entry', async () => {
    await ctx.status.claim(key('n2'), RUN_STARTED);
    await ctx.status.complete(key('n2'), { rows: 0, bytes: 0, snapshotId: 1, tbl: '' });

    const body = (await get(`/api/runs/${RUN}/explore`)).json();
    const d: Dataset = body.datasets.find((x: Dataset) => x.actor === ACTOR);
    expect(d).toBeDefined();
    expect(d.state).toBe('complete');
    expect(d.rows).toBe(0);
    expect(d.urls).toEqual([]);
    expect(d.error).toBeNull();
    expect(body.failed).toEqual([]); // an empty success is not a failure
  });

  it('reports a still-running actor as pending rather than as a finished empty dataset', async () => {
    // Records are the only input that decides what exists, so a dispatch in flight is
    // already addressable — with the parts that are not ready named as such.
    await ctx.status.claim(key('n3'), RUN_STARTED);

    const body = (await get(`/api/runs/${RUN}/explore`)).json();
    expect(body.pending).toEqual([ACTOR]);
    const d: Dataset = body.datasets.find((x: Dataset) => x.actor === ACTOR);
    expect(d.state).toBe('running');
  });

  it('clamps a caller-supplied TTL — expiry is not negotiable upward', async () => {
    const body = (await get(`/api/runs/${RUN}/explore?ttl=999999`)).json();
    expect(body.expiresAt - Date.now()).toBeLessThanOrEqual(900 * 1000 + 5_000);
  });
});

describe('GET /api/runs/:runId — both status dimensions', () => {
  let ctx: Ctx;

  beforeEach(async () => {
    resetLakeConnections();
    ctx = build();
    await ctx.status.ensureSchema();
  });

  afterEach(async () => {
    await ctx.app.close();
  });

  it('reports both dimensions and the projection over them', async () => {
    await ctx.status.claim(key('n1'), RUN_STARTED);
    const body = (await ctx.app.inject({ method: 'GET', url: `/api/runs/${RUN}` })).json();
    // Execution is unknown to Temporal here, so it stays at the durable default; the point
    // is that both dimensions come back SEPARATELY alongside the projection.
    expect(body).toHaveProperty('execution');
    expect(body).toHaveProperty('materialization');
    expect(body).toHaveProperty('lifecycle');
    expect(body.materialization).toMatchObject({ total: 1, running: 1, complete: 0, failed: 0 });
  });

  it('does not require a token — status is not a credential-bearing surface', async () => {
    await ctx.status.claim(key('n1'), RUN_STARTED);
    const res = await ctx.app.inject({ method: 'GET', url: `/api/runs/${RUN}` });
    expect(res.statusCode).toBe(200);
  });

  it('404s only when NEITHER authority has heard of the run', async () => {
    const res = await ctx.app.inject({ method: 'GET', url: '/api/runs/never-existed' });
    expect(res.statusCode).toBe(404);
  });
});

describe('GET /api/summaries/*', () => {
  let ctx: Ctx;

  beforeEach(async () => {
    resetLakeConnections();
    ctx = build();
    await ctx.summaries.ensureSchema();
  });

  afterEach(async () => {
    await ctx.app.close();
  });

  it('serves fleet materialization health', async () => {
    await ctx.summaries.refreshHealth({ pending: 1, running: 2, complete: 3, failed: 4 }, 5);
    const body = (await ctx.app.inject({ method: 'GET', url: '/api/summaries/health' })).json();
    expect(body).toMatchObject({ pending: 1, running: 2, complete: 3, failed: 4, stale: 5 });
  });

  it('404s for a run with no summary rather than inventing zeros', async () => {
    const res = await ctx.app.inject({ method: 'GET', url: '/api/summaries/runs/nope' });
    expect(res.statusCode).toBe(404);
  });

  it('clamps the hourly window so a dashboard refresh cannot pull the whole table', async () => {
    const body = (await ctx.app.inject({ method: 'GET', url: '/api/summaries/hourly?hours=99999' })).json();
    expect(body.hours).toBe(24 * 30);
  });
});
