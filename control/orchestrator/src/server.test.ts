import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { buildServer } from './server';
import { WorkflowNotFoundError } from '@temporalio/client';
import { Repo } from './db/repo';
import { ObjectStore, MemoryStore } from './codec/objectStore';
import { dtPartition, writeDatasetParquet } from './data/parquet';
import { runFragment } from './data/datasetName';
import { MATERIALIZATION_SCHEMA_VERSION } from './data/materialization';
import { MaterializationStore } from './data/materializationStore';
import { DatasetRecordStore } from './data/datasetRecords';
import { RunWorkflowStore } from './data/runWorkflows';
import { HistoryArchive } from './historyArchive';
import { mapHistory, type RunHistory } from './history';

/** The server-minted dispatch instant, and the `dt=` partition derived from it. */
const RUN_STARTED = 1_700_000_000_000;
const DT = dtPartition(RUN_STARTED);

// Stub the Temporal client so the run routes stay hermetic (no cluster). There is nothing to stub
// for STARTING a run — not because the orchestrator cannot (`POST /api/runs` starts one execution
// of a REGISTERED caller's workflow, by folder and type) but because every start this suite asks
// for REFUSES BEFORE IT DIALS: no folder, or no worker polling that folder's queue, answered
// without a client. So a start that ever reached `getClient` here would fail loudly on the missing
// mock, which is the reading this stub wants.
vi.mock('./temporalClient', () => ({
  describeRun: vi.fn(),
  listRuns: vi.fn(),
  describeRunHeartbeats: vi.fn(),
  // The history route's LIVE authority. Stubbed rather than reached, so the archive fallback below
  // can be asserted on the two answers that decide it: `undefined` (gRPC NOT_FOUND) and a throw.
  fetchRunHistory: vi.fn(),
  // The one WRITE the run surface makes: answering a parked run's ask. Stubbed rather than
  // reached, for the same reason as the reads.
  signalRun: vi.fn(),
}));

describe('orchestrator API', () => {
  let app: FastifyInstance;
  let repo: Repo;

  beforeEach(() => {
    // ':memory:' repo + no web root → hermetic, no Temporal, no static plugin.
    repo = new Repo(':memory:');
    app = buildServer({ repo, webRoot: '' });
  });
  afterEach(async () => {
    await app.close();
  });

  it('health responds', async () => {
    const res = await app.inject({ method: 'GET', url: '/api/health' });
    expect(res.json()).toEqual({ ok: true });
  });

  it('stores and returns live @actor.healthcheck progress beats', async () => {
    // empty until the host beats.
    expect((await app.inject({ method: 'GET', url: '/api/runs/r1/progress' })).json()).toEqual({ nodes: {} });

    const beat = await app.inject({
      method: 'POST',
      url: '/api/runs/r1/progress',
      payload: { node: 'n1', progress: { done: 3, engine: 1 }, total: 6 },
    });
    expect(beat.json()).toEqual({ ok: true });

    // a later beat for the same node overwrites; a second node coexists.
    await app.inject({ method: 'POST', url: '/api/runs/r1/progress', payload: { node: 'n1', progress: { done: 6, engine: 1 }, total: 6 } });
    await app.inject({ method: 'POST', url: '/api/runs/r1/progress', payload: { node: 'n2', progress: { done: 1, engine: 2 }, total: 6 } });

    const got = (await app.inject({ method: 'GET', url: '/api/runs/r1/progress' })).json() as {
      nodes: Record<string, { progress: { done: number } }>;
    };
    expect(got.nodes.n1.progress.done).toBe(6);
    expect(got.nodes.n2.progress.done).toBe(1);
  });

  /**
   * THE PROGRESS CAP MUST NOT EVICT, because the route it protects takes no token.
   *
   * `POST /api/runs/:runId/progress` is the one write on the run surface with no
   * `checkOptionalBearer` — its only caller is a worker container `kontra deploy` never gives
   * `KONTRA_RUN_TOKEN` to, which is why it stays that way (`routes/runs.ts` says so at the handler).
   * The cap used to delete its OLDEST entry to make room, so those two facts composed into a data
   * loss primitive: anyone who could reach the port could beat 500 invented ids and silently drop
   * the beats of the live fleet somebody was watching. A visibility aid that lies is worse than one
   * that is missing.
   *
   * So a run already in the map is never displaced by a new one. The live run below is the OLDEST
   * entry — precisely the one eviction took first.
   */
  it('does not let 500 invented run ids evict a live run’s beats', async () => {
    const beat = (runId: string) =>
      app.inject({
        method: 'POST',
        url: `/api/runs/${encodeURIComponent(runId)}/progress`,
        payload: { node: 'n1', progress: { done: 3 }, total: 6 },
      });

    expect((await beat('live-fleet')).json()).toEqual({ ok: true });
    for (let i = 0; i < 600; i++) await beat(`junk-${i}`);

    const still = (await app.inject({ method: 'GET', url: '/api/runs/live-fleet/progress' })).json();
    expect(still).toMatchObject({ nodes: { n1: { progress: { done: 3 } } } });

    // The cap still holds — it is a bound on memory, and the flood hits it rather than the run.
    // Refused honestly: `{ ok: true }` about a beat that was not recorded is the lie this replaces.
    expect((await beat('one-more')).json()).toMatchObject({ ok: false });
    expect((await app.inject({ method: 'GET', url: '/api/runs/one-more/progress' })).json()).toEqual({
      nodes: {},
    });

    // …and a run that IS in the map keeps beating through a full cap, which is the whole point.
    expect((await beat('live-fleet')).json()).toEqual({ ok: true });
  });

  it('CRUDs actors', async () => {
    expect((await app.inject({ method: 'GET', url: '/api/actors' })).json()).toEqual([]);

    const created = await app.inject({
      method: 'POST',
      url: '/api/actors',
      payload: { key: 'echo@0.1.0', name: 'echo', version: '0.1.0', operations: [] },
    });
    expect(created.statusCode).toBe(200);
    expect(created.json()).toMatchObject({ key: 'echo@0.1.0', name: 'echo', version: '0.1.0' });

    expect((await app.inject({ method: 'GET', url: '/api/actors' })).json()).toHaveLength(1);

    const del = await app.inject({ method: 'DELETE', url: '/api/actors/echo@0.1.0' });
    expect(del.json()).toEqual({ ok: true });
    expect((await app.inject({ method: 'GET', url: '/api/actors' })).json()).toEqual([]);
  });

  it('rejects an incomplete actor', async () => {
    const res = await app.inject({ method: 'POST', url: '/api/actors', payload: { name: 'x' } });
    expect(res.statusCode).toBe(400);
  });

  // --- the registration gate (src/catalog.ts) ---

  /** A descriptor as an SDK posts one. */
  const actor = (operations: Array<Record<string, unknown>>) => ({
    key: 'demo@1.2.3',
    name: 'demo',
    version: '1.2.3',
    schemaVersion: 'kontra.actor.v1',
    operations,
  });
  const register = (payload: unknown) => app.inject({ method: 'POST', url: '/api/actors', payload });
  const catalogued = async () =>
    (await app.inject({ method: 'GET', url: '/api/actors' })).json() as Array<{
      key: string;
      digest?: string;
      operations: Array<Record<string, unknown>>;
    }>;

  it('refuses a descriptor that does not validate, and stores nothing', async () => {
    // The body the old cast let through: `(body.operations ?? []) as ActorOperation[]` is erased
    // at build time, so an operations array of numbers registered with a 200 and the first code
    // to look inside an operation — a page, a dispatch resolving a Method by name — found out.
    const res = await register(actor([1, 2, 3] as unknown as Array<Record<string, unknown>>));
    expect(res.statusCode).toBe(400);
    expect((res.json() as { error: string }).error).toContain('/operations/0');
    expect(await catalogued()).toEqual([]);
  });

  it('re-registering the same (name, version) with the same schema succeeds', async () => {
    // A worker restarting posts its descriptor again on every boot; this must never fail.
    const ops = [{ name: 'fetch', input: { type: 'object' }, output: { type: 'object' } }];
    expect((await register(actor(ops))).statusCode).toBe(200);
    expect((await register(actor(ops))).statusCode).toBe(200);
    expect(await catalogued()).toHaveLength(1);
  });

  it('re-registering a Method that advertises nothing, on both sides, succeeds', async () => {
    // `probe` in the golden descriptor declares neither `takes` nor `emits`. Unset has to compare
    // equal to unset, or an actor with an untyped Method could never restart.
    expect((await register(actor([{ name: 'probe' }]))).statusCode).toBe(200);
    expect((await register(actor([{ name: 'probe' }]))).statusCode).toBe(200);
  });

  it('refuses a changed I/O schema, naming the Method and telling the operator to bump', async () => {
    // THE DEV-LOOP COST, deliberately paid: edit an actor's types, re-serve without bumping, and
    // registration fails — loudly, here, instead of silently and later inside a run typed against
    // the shape that was overwritten (ADR 0004).
    await register(actor([{ name: 'fetch', input: { type: 'object', properties: { host: { type: 'string' } } } }]));
    const res = await register(
      actor([{ name: 'fetch', input: { type: 'object', properties: { host: { type: 'integer' } } } }])
    );
    expect(res.statusCode).toBe(409);
    const { error } = res.json() as { error: string };
    expect(error).toContain('Method "fetch"');
    expect(error).toMatch(/bump/);
    // …and the catalogued schema is the FIRST one: a refusal that wrote anyway would be the
    // overwrite it exists to stop.
    const [row] = await catalogued();
    expect(row?.operations[0]?.input).toEqual({ type: 'object', properties: { host: { type: 'string' } } });
  });

  /** A descriptor for one version of `probe`, for the cross-version cases below. */
  const probe = (version: string, operations: Array<Record<string, unknown>>) => ({
    key: `probe@${version}`,
    name: 'probe',
    version,
    schemaVersion: 'kontra.actor.v1',
    operations,
  });
  const rows = async () =>
    (await app.inject({ method: 'GET', url: '/api/actors' })).json() as Array<{
      key: string;
      incompatibilities?: Array<{ method: string; field: string; rule: string; previous: string; detail: string }>;
    }>;

  it('REPORTS a new version that breaks a caller, and registers it anyway', async () => {
    // The gate above refuses a schema change under an UNCHANGED version. A NEW version is exactly
    // where a deliberate break belongs, so this must not refuse — a gate that did would have to be
    // bypassable, and then the same silence comes back under a flag that sounds responsible. What
    // it must not do is let it pass unrecorded, which is what happened before: `probe@0.2.0`
    // dropped a field `0.1.0` emitted, registered with a 200, and every caller reading that field
    // found out inside a run.
    const emits = (props: Record<string, unknown>) => ({ type: 'object', properties: props });
    expect(
      (await register(probe('0.1.0', [{ name: 'head', output: emits({ body: { type: 'string' }, status: { type: 'integer' } }) }])))
        .statusCode
    ).toBe(200);

    const res = await register(probe('0.2.0', [{ name: 'head', output: emits({ body: { type: 'string' } }) }]));
    expect(res.statusCode).toBe(200);

    const catalogue = await rows();
    const newer = catalogue.find((r) => r.key === 'probe@0.2.0');
    expect(newer?.incompatibilities).toHaveLength(1);
    expect(newer?.incompatibilities?.[0]).toMatchObject({
      method: 'head',
      field: 'output',
      rule: 'FORWARD',
      previous: '0.1.0',
    });
    expect(newer?.incompatibilities?.[0]?.detail).toContain('"status"');

    // …and the version it was compared against carries nothing: the finding sits on the version
    // that INTRODUCED it, not on the one it broke.
    const older = catalogue.find((r) => r.key === 'probe@0.1.0') ?? {};
    expect('incompatibilities' in older).toBe(false);
  });

  it('says nothing about a new version that only adds', async () => {
    // An added optional input and an added output field are how an actor grows without breaking
    // anybody. A report that fired on those is a report that gets scrolled past.
    await register(
      probe('0.1.0', [
        { name: 'head', input: { type: 'object', properties: { host: { type: 'string' } }, required: ['host'] } },
      ])
    );
    await register(
      probe('0.2.0', [
        {
          name: 'head',
          input: {
            type: 'object',
            properties: { host: { type: 'string' }, timeout: { type: 'integer' } },
            required: ['host'],
          },
        },
      ])
    );
    const newer = (await rows()).find((r) => r.key === 'probe@0.2.0') ?? {};
    expect('incompatibilities' in newer).toBe(false);
  });

  it('keeps the finding when the worker re-registers the same version on restart', async () => {
    // A worker posts its descriptor on every boot. The comparison runs again against the same
    // predecessor and must land on the same answer — a finding that disappeared on the first
    // restart would be worse than never having been made.
    const before = probe('0.1.0', [
      { name: 'head', input: { type: 'object', properties: { host: { type: 'string' } }, required: ['host'] } },
    ]);
    const after = probe('0.2.0', [
      {
        name: 'head',
        input: {
          type: 'object',
          properties: { host: { type: 'string' }, timeout: { type: 'integer' } },
          required: ['host', 'timeout'],
        },
      },
    ]);
    await register(before);
    await register(after);
    expect((await register(after)).statusCode).toBe(200);
    const newer = (await rows()).find((r) => r.key === 'probe@0.2.0');
    expect(newer?.incompatibilities?.[0]).toMatchObject({ method: 'head', rule: 'BACKWARD' });
  });

  it('lets a worker self-register its digest against a version the gate has pinned', async () => {
    // The digest path declares no schemas and runs on EVERY worker start (ADR 0011). Gating it
    // would fail a fact about an image over a contract it never states.
    await register(actor([{ name: 'fetch', input: { type: 'object' } }]));
    const reg = await app.inject({
      method: 'POST',
      url: '/api/actors/demo@1.2.3/digest',
      payload: { name: 'demo', version: '1.2.3', digest: 'sha256:w' },
    });
    expect(reg.statusCode).toBe(200);
    const [row] = await catalogued();
    expect(row?.digest).toBe('sha256:w');
    expect(row?.operations).toHaveLength(1); // schemas untouched
  });

  it('accepts the descriptor that follows a digest-first registration', async () => {
    // `setActorDigest` creates a row with `operations: []` when the worker's digest lands before
    // its descriptor. Refusing new Methods would make that ORDERING look like a contract change.
    await app.inject({
      method: 'POST',
      url: '/api/actors/demo@1.2.3/digest',
      payload: { name: 'demo', version: '1.2.3', digest: 'sha256:w' },
    });
    const res = await register(actor([{ name: 'fetch', input: { type: 'object' } }]));
    expect(res.statusCode).toBe(200);
    const [row] = await catalogued();
    expect(row?.digest).toBe('sha256:w');
    expect(row?.operations).toHaveLength(1);
  });

  it('leaves an already-catalogued actor readable, whatever the gate would say about it', async () => {
    // The gate applies to WRITES. A row written before it existed — here one keyed by hand, which
    // the route would now refuse — still lists and still resolves.
    repo.upsertActor({
      key: 'legacy',
      name: 'legacy',
      version: '0.1.0',
      schemaVersion: 'kontra.actor.v1',
      operations: [{ name: 'run', input: { type: 'nonsense' } }],
    });
    expect(await catalogued()).toHaveLength(1);
    expect((await catalogued())[0]?.key).toBe('legacy');
  });

  it('saves, lists, gets, and deletes designs (opaque documents)', async () => {
    // The document is opaque to the server — the UI owns its shape (here a stand-in).
    const document = { version: 1, runId: 'r', catalog: [], nodes: [{ id: 'n1' }], edges: [] };
    const saved = await app.inject({ method: 'POST', url: '/api/graphs', payload: { name: 'demo', document } });
    expect(saved.statusCode).toBe(200);
    const { id } = saved.json() as { id: string };
    expect(id).toBeTruthy();

    const list = (await app.inject({ method: 'GET', url: '/api/graphs' })).json() as unknown[];
    expect(list).toHaveLength(1);
    expect(list[0]).not.toHaveProperty('document'); // listing is metadata-only

    const got = await app.inject({ method: 'GET', url: `/api/graphs/${id}` });
    expect((got.json() as { document: { nodes: unknown[] } }).document.nodes).toHaveLength(1);

    expect((await app.inject({ method: 'DELETE', url: `/api/graphs/${id}` })).json()).toEqual({ ok: true });
    expect((await app.inject({ method: 'GET', url: `/api/graphs/${id}` })).statusCode).toBe(404);
  });

  it('rejects a design with no document', async () => {
    const res = await app.inject({ method: 'POST', url: '/api/graphs', payload: { name: 'x' } });
    expect(res.statusCode).toBe(400);
  });

  // --- runs: the caller's workflow is the run (ADR 0023 §12) ---

  it('reads a run by the id the caller started its workflow under', async () => {
    const { describeRun } = await import('./temporalClient');
    vi.mocked(describeRun).mockResolvedValueOnce({
      runId: 'nightly-sweep',
      status: 'running',
      type: 'Sweep',
      tenant: 'acme',
      startedAt: 5,
      closedAt: 0,
    });

    const res = await app.inject({ method: 'GET', url: '/api/runs/nightly-sweep' });

    expect(res.statusCode).toBe(200);
    const body = res.json() as { runId: string; execution: string; lifecycle: string };
    expect(body.runId).toBe('nightly-sweep');
    expect(body.execution).toBe('running');
    // Both dimensions, always — the projection never replaces them (ADR 0017).
    expect(body).toHaveProperty('materialization');
    expect(body.lifecycle).toBeTruthy();
    expect(vi.mocked(describeRun)).toHaveBeenCalledWith('nightly-sweep');
  });

  it('404s a run neither Temporal nor the lake has heard of', async () => {
    const { describeRun } = await import('./temporalClient');
    vi.mocked(describeRun).mockResolvedValueOnce(undefined);
    const res = await app.inject({ method: 'GET', url: '/api/runs/never-ran' });
    expect(res.statusCode).toBe(404);
  });

  it('502s when Temporal itself is unwell, rather than reporting an empty run', async () => {
    const { describeRun } = await import('./temporalClient');
    vi.mocked(describeRun).mockRejectedValueOnce(new Error('connection refused'));
    const res = await app.inject({ method: 'GET', url: '/api/runs/r1' });
    expect(res.statusCode).toBe(502);
  });

  // --- the event log after retention drops the execution (ADR 0025) ---

  describe('the history route falls back to the archive', () => {
    /** A server over an in-memory blob plane, plus the archive that writes into it. */
    async function withArchive(): Promise<{
      app: FastifyInstance;
      archive: HistoryArchive;
    }> {
      const store = new ObjectStore({ backing: new MemoryStore(), prefix: '' });
      return {
        app: buildServer({ repo: new Repo(':memory:'), store, webRoot: '' }),
        archive: new HistoryArchive(store),
      };
    }

    /** One reduced log, through the real reducer. */
    function log(): RunHistory {
      return mapHistory([
        {
          eventId: 1,
          eventTime: { seconds: 1_786_831_339, nanos: 0 },
          workflowExecutionStartedEventAttributes: { workflowType: { name: 'NsCheck' } },
        },
      ]);
    }

    it('serves the archive when Temporal answers not-found, labelled as archived', async () => {
      const { fetchRunHistory } = await import('./temporalClient');
      const { app: app2, archive } = await withArchive();
      try {
        await archive.write(
          { runId: 'nscheck-1786831339', startedAt: 1_786_831_339_000, closedAt: 1_786_831_633_000 },
          log(),
          1_786_900_000_000
        );
        vi.mocked(fetchRunHistory).mockResolvedValueOnce(undefined);

        const res = await app2.inject({ method: 'GET', url: '/api/runs/nscheck-1786831339/history' });
        expect(res.statusCode).toBe(200);
        const body = res.json() as { archived: boolean; archivedAt: number; events: unknown[] };
        // The console reads exactly this to know it must not offer to follow.
        expect(body.archived).toBe(true);
        expect(body.archivedAt).toBe(1_786_900_000_000);
        expect(body.events).toHaveLength(1);
      } finally {
        await app2.close();
      }
    });

    it('404s when neither Temporal nor the archive has the run', async () => {
      const { fetchRunHistory } = await import('./temporalClient');
      const { app: app2 } = await withArchive();
      try {
        vi.mocked(fetchRunHistory).mockResolvedValueOnce(undefined);
        const res = await app2.inject({ method: 'GET', url: '/api/runs/never-ran/history' });
        // A 404 now means BOTH authorities have nothing — which is what lets the console say so
        // plainly instead of rendering a run with no events.
        expect(res.statusCode).toBe(404);
      } finally {
        await app2.close();
      }
    });

    it('502s on a cluster outage rather than serving the archive as if it were live', async () => {
      const { fetchRunHistory } = await import('./temporalClient');
      const { app: app2, archive } = await withArchive();
      try {
        await archive.write(
          { runId: 'nscheck-1786831339', startedAt: 1_786_831_339_000, closedAt: 1_786_831_633_000 },
          log()
        );
        vi.mocked(fetchRunHistory).mockRejectedValueOnce(new Error('14 UNAVAILABLE'));

        const res = await app2.inject({ method: 'GET', url: '/api/runs/nscheck-1786831339/history' });
        expect(res.statusCode).toBe(502);
      } finally {
        await app2.close();
      }
    });

    it('serves Temporal unlabelled while Temporal still has the execution', async () => {
      const { fetchRunHistory } = await import('./temporalClient');
      const { app: app2, archive } = await withArchive();
      try {
        await archive.write(
          { runId: 'nscheck-1786831339', startedAt: 1_786_831_339_000, closedAt: 1_786_831_633_000 },
          log()
        );
        vi.mocked(fetchRunHistory).mockResolvedValueOnce(log());

        const res = await app2.inject({ method: 'GET', url: '/api/runs/nscheck-1786831339/history' });
        expect(res.json()).not.toHaveProperty('archived');
      } finally {
        await app2.close();
      }
    });
  });

  it('lists runs, passing tenant/status/limit through', async () => {
    const { listRuns, describeRun } = await import('./temporalClient');
    const rows = [
      {
        runId: 'wf-1',
        status: 'running' as const,
        type: 'Sweep',
        tenant: 'acme',
        startedAt: 2,
        closedAt: 0,
        dispatches: 3,
      },
    ];
    vi.mocked(listRuns).mockResolvedValueOnce(rows);
    // The ACTIVITY dimension describes the OPEN runs of a page — one RPC each, capped, and never
    // for a closed run. Without it the row would have to say `null`, which is the honest answer
    // and not the same one as "it is fine".
    vi.mocked(describeRun).mockResolvedValueOnce({
      runId: 'wf-1',
      status: 'running',
      type: 'Sweep',
      tenant: 'acme',
      startedAt: 2,
      closedAt: 0,
      memo: {},
      inFlight: [],
    });

    const res = await app.inject({ method: 'GET', url: '/api/runs?tenant=acme&status=running&limit=10' });

    expect(res.statusCode).toBe(200);
    // Temporal's row travels through untouched, and the other two dimensions are added BESIDE it —
    // a list row carries all three or it is back to one label over three outcomes.
    expect(res.json()).toEqual([
      {
        ...rows[0],
        materialization: expect.objectContaining({ total: 0 }),
        lifecycle: 'executing',
        activity: 'running',
        pendingAsks: 0,
      },
    ]);
    expect(vi.mocked(listRuns)).toHaveBeenCalledWith({ tenant: 'acme', status: 'running', limit: 10 });
  });

  it('502s when the run list cannot be read', async () => {
    const { listRuns } = await import('./temporalClient');
    vi.mocked(listRuns).mockRejectedValueOnce(new Error('cluster down'));
    const res = await app.inject({ method: 'GET', url: '/api/runs' });
    expect(res.statusCode).toBe(502);
  });

  it('refuses a GRAPH on the start route — that is what §12 deleted', async () => {
    // THIS TEST CHANGED, and what it protects did not.
    //
    // It used to assert `POST /api/runs` was a 404: at the time nothing could start a run, so
    // "the route is absent" and "the interpreter is gone" were the same statement. The route
    // exists again — the Serve page starts a Run the same way `kontra workflow start` does — so
    // absence can no longer carry the invariant, and the invariant has to be asserted directly.
    //
    // It is this: the server may start a CALLER'S WORKFLOW from a registered FOLDER. It may not be
    // handed something to execute, and it may not be handed a queue string (GitHub #15). A graph
    // body names no folder, so there is nothing to resolve and nothing to start.
    const res = await app.inject({
      method: 'POST',
      url: '/api/runs',
      payload: { graph: { nodes: [], edges: [] } },
    });
    expect(res.statusCode).toBe(400);
    expect(res.json().error).toMatch(/file is required/);
  });

  it('starts a run from a FOLDER, never a queue string, and refuses before dialling', async () => {
    // The queue is derived from the folder server-side (GitHub #15), so a body carrying a queue
    // cannot route a Run — the queue field is ignored, and every one of these refuses at folder
    // resolution, BEFORE any Temporal dial, rather than starting onto a queue nobody polls.
    for (const payload of [
      {}, // no folder
      { type: 'NsCheck', queue: 'recon' }, // a queue string is not a folder — ignored, still refused
      { file: '../../etc/passwd.py' }, // outside the checkout
      { file: 'does-not-exist', queue: 'recon' }, // no such workflow; the queue does not save it
    ]) {
      const res = await app.inject({ method: 'POST', url: '/api/runs', payload });
      expect(res.statusCode, JSON.stringify(payload)).toBe(400);
    }
  });

  it('refuses to serve a workflow file from outside the checkout', async () => {
    // `serve` runs a file from this host's disk. The route picks WHICH file, and only from
    // inside the checkout — the same "which, never what" rule the infra dispatch table follows.
    // There is no queue field to send (GitHub #15); the path alone is refused.
    const res = await app.inject({
      method: 'POST',
      url: '/api/workflows/serve',
      payload: { file: '../../etc/passwd.py' },
    });
    expect(res.statusCode).toBe(400);
  });

  it('has no write route for a workflow file — the viewer is read-only (ADR 0030)', async () => {
    // The Workflows page reads source and never writes it: `GET …/file/:name` reads the bytes on
    // disk, and changing a workflow is the operator's own editor plus a re-serve. A browser write
    // let the file and the registered digest disagree, which is the class of bug this closes — the
    // same reason ADR 0020 gave Terminals no input path. The GET still answers; only the PUT is gone.
    const put = await app.inject({
      method: 'PUT',
      url: '/api/workflows/file/nscheck',
      payload: { source: '# rewritten from the browser\n' },
    });
    expect(put.statusCode).toBe(404);
  });

  it('registers what a served workflow declares, and lists it beside the files', async () => {
    // The whole point of the feature: before this, `GET /api/workflows` could say a file exists
    // and nothing about what it accepts, returns or is for. The body is the one Python posts.
    const posted = await app.inject({
      method: 'POST',
      url: '/api/workflows/catalog',
      payload: {
        name: 'NsCheck',
        description: 'Check every domain delegation.',
        input: { type: 'object', additionalProperties: true },
        output: { type: 'object', additionalProperties: true },
      },
    });
    expect(posted.statusCode).toBe(200);
    expect(posted.json().savedAt).toBeGreaterThan(0);

    const listed = (await app.inject({ method: 'GET', url: '/api/workflows' })).json() as {
      registered: Array<{ name: string; description?: string }>;
    };
    expect(listed.registered.map((w) => w.name)).toEqual(['NsCheck']);
    expect(listed.registered[0]!.description).toBe('Check every domain delegation.');
  });

  it('lets a re-served workflow overwrite its own descriptor', async () => {
    // The opposite of the actor gate, and deliberately: there is no version to bump, so editing a
    // workflow and re-serving it is the ordinary way to change one. Refusing the second
    // registration would leave the catalog describing code that is no longer running.
    const post = (description: string) =>
      app.inject({ method: 'POST', url: '/api/workflows/catalog', payload: { name: 'Ping', description } });
    await post('The first words.');
    const again = await post('The second words.');
    expect(again.statusCode).toBe(200);

    const listed = (await app.inject({ method: 'GET', url: '/api/workflows' })).json() as {
      registered: Array<{ name: string; description?: string }>;
    };
    expect(listed.registered).toHaveLength(1);
    expect(listed.registered[0]!.description).toBe('The second words.');
  });

  it('refuses a workflow descriptor that is not one, and writes nothing', async () => {
    // A worker posts this unauthenticated, and a page takes it apart later. `POST /api/actors`
    // learnt this the expensive way: it checked three strings and cast, so any JSON registered
    // with a 200 and the first reader was the first check.
    for (const payload of [{ description: 'no name' }, { name: 'X', input: 'string' }]) {
      const res = await app.inject({ method: 'POST', url: '/api/workflows/catalog', payload });
      expect(res.statusCode, JSON.stringify(payload)).toBe(400);
    }
    const listed = (await app.inject({ method: 'GET', url: '/api/workflows' })).json() as {
      registered: unknown[];
    };
    expect(listed.registered).toEqual([]);
  });

  it('reports what the control surface admits', async () => {
    // The operator chose an open surface; the page reads this to SAY so. A posture that is only
    // in a source comment is one the next person meets as a surprise.
    const res = await app.inject({ method: 'GET', url: '/api/workflows/exposure' });
    expect(res.statusCode).toBe(200);
    expect(typeof res.json().open).toBe('boolean');
    expect(res.json().detail).toMatch(/KONTRA_RUN_TOKEN/);
  });

  it('does not dispatch a saved design', async () => {
    const saved = await app.inject({
      method: 'POST',
      url: '/api/graphs',
      payload: { name: 'd1', document: { nodes: [], edges: [] } },
    });
    const { id } = saved.json() as { id: string };
    const res = await app.inject({ method: 'POST', url: `/api/graphs/${id}/dispatch`, payload: {} });
    expect(res.statusCode).toBe(404);
  });

  it('reads per-node heartbeats for a run (#06)', async () => {
    const { describeRunHeartbeats } = await import('./temporalClient');
    const nodes = { n1: { node: 'n1', done: 3, total: 6, attempt: 1, lastBeat: 1234 } };
    vi.mocked(describeRunHeartbeats).mockResolvedValueOnce(nodes);
    const res = await app.inject({ method: 'GET', url: '/api/runs/r1/heartbeats' });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toEqual({ nodes });
    expect(vi.mocked(describeRunHeartbeats)).toHaveBeenCalledWith('r1');
  });

  it('worker digest self-registration updates the digest without clobbering schemas', async () => {
    await app.inject({
      method: 'POST',
      url: '/api/actors',
      payload: {
        key: 'echo@0.1.0',
        name: 'echo',
        version: '0.1.0',
        operations: [{ name: 'run', input: { type: 'object' } }],
      },
    });
    const reg = await app.inject({
      method: 'POST',
      url: '/api/actors/echo@0.1.0/digest',
      payload: { name: 'echo', version: '0.1.0', digest: 'sha256:w' },
    });
    expect(reg.statusCode).toBe(200);
    const list = (await app.inject({ method: 'GET', url: '/api/actors' })).json() as Array<{
      key: string;
      digest?: string;
      operations: unknown[];
    }>;
    const echo = list.find((a) => a.key === 'echo@0.1.0');
    expect(echo?.digest).toBe('sha256:w');
    expect(echo?.operations).toHaveLength(1); // schemas preserved
  });

  it('rejects a digest registration missing fields (400)', async () => {
    const res = await app.inject({
      method: 'POST',
      url: '/api/actors/x@1/digest',
      payload: { digest: 'sha256:y' },
    });
    expect(res.statusCode).toBe(400);
  });

  it('lists datasets per (actor, version, dispatch) — DuckLake, local data path', async () => {
    // Drive the real DuckLake path with a local DATA_PATH standing in for S3 (the httpfs
    // S3->S3 read/write needs a live stack). The route resolves the lake config from env,
    // so point KONTRA_DUCKLAKE_* at temp dirs and give the store a publicEndpoint (not an
    // S3 endpoint) — that keeps the lake local (resolveLakeConfig s3=false).
    const dir = mkdtempSync(join(tmpdir(), 'kontra-srv-lake-'));
    const dataPath = join(dir, 'data');
    mkdirSync(dataPath, { recursive: true });
    const prevData = process.env.KONTRA_DUCKLAKE_DATA_PATH;
    const prevCatalog = process.env.KONTRA_DUCKLAKE_CATALOG;
    process.env.KONTRA_DUCKLAKE_DATA_PATH = `${dataPath}/`;
    process.env.KONTRA_DUCKLAKE_CATALOG = join(dir, 'cat.ducklake');

    const store = new ObjectStore({
      backing: new MemoryStore(),
      prefix: '',
      publicEndpoint: 'http://localhost:8333', // presign only; no S3 endpoint => local lake
    });
    const app2 = buildServer({ repo: new Repo(':memory:'), store, webRoot: '' });
    try {
      // Materialize a node output the real way (decode a {results} envelope from a local
      // JSON source), exactly as the materializer activity would — into the same
      // env-resolved lake.
      const src = join(mkdtempSync(join(tmpdir(), 'kontra-srv-src-')), 'blob.json');
      writeFileSync(src, JSON.stringify({ results: [{ url: 'a', n: 1 }], failures: [] }));
      const out = await writeDatasetParquet(
        store,
        {
          sha256: 'deadbeef',
          actor: 'echo',
          version: '0.1.0',
          runId: 'r1',
          node: 'n1',
          runStartedAt: RUN_STARTED,
        },
        { sourceUri: src }
      );
      // The table is named after the ACTOR now — no `ds_<hash>`, no registry to join.
      expect(out.tbl).toBe('echo');
      expect(out.rows).toBe(1);

      const list = await app2.inject({ method: 'GET', url: '/api/datasets' });
      expect(list.json()).toMatchObject([
        { kind: 'output', name: 'echo', version: '0.1.0', dt: DT, rows: 1 },
      ]);

      // `dt` is a PREFIX filter, so a day narrows without anyone naming a dispatch instant.
      const day = await app2.inject({
        method: 'GET',
        url: `/api/datasets?name=echo&version=0.1.0&dt=${DT.slice(0, 10)}`,
      });
      expect(day.json()).toHaveLength(1);

      // …and a selector that matches nothing returns nothing, rather than everything.
      const other = await app2.inject({ method: 'GET', url: '/api/datasets?name=echo&dt=1999-01-01' });
      expect(other.json()).toEqual([]);

      // The preview the Datasets page draws: rows as JSON, from the server's own connection.
      // The page used to presign these files and range-read them with 76 MB of DuckDB-WASM.
      const preview = await app2.inject({
        method: 'GET',
        url: '/api/datasets/echo/preview?kind=output&limit=10',
      });
      expect(preview.statusCode).toBe(200);
      const body = preview.json() as { columns: Array<{ name: string }>; rows: unknown[][] };
      const at = (col: string) => body.rows[0]![body.columns.findIndex((c) => c.name === col)];
      expect(body.rows).toHaveLength(1);
      expect(at('url')).toBe('a');
      expect(at('n')).toBe(1);
      // The stamped identity columns ride along, so a copied row still says which dispatch it
      // came from — but the ADDRESS is name/version/dt, which is what the page renders.
      expect(body.columns.map((c) => c.name)).toEqual(expect.arrayContaining(['url', 'n', 'version', 'dt']));

      // An unknown name is the caller's mistake — 404, not a 502 that reads like a lake outage.
      // Classified by `NoSuchDatasetError` rather than by the message starting with "no ", which
      // is why the same assertion is made about PROVENANCE below: the two routes used to carry
      // separate copies of that string match, so they could disagree without anything failing.
      const missing = await app2.inject({ method: 'GET', url: '/api/datasets/nope/preview' });
      expect(missing.statusCode).toBe(404);
      const missingProv = await app2.inject({ method: 'GET', url: '/api/datasets/nope/provenance' });
      expect(missingProv.statusCode).toBe(404);
      expect(missingProv.json()).toEqual({ error: 'no output dataset named "nope"' });
      const unknown = await app2.inject({ method: 'GET', url: '/api/datasets?name=nobody' });
      expect(unknown.json()).toEqual([]);

      // Which Machines and which RUNS wrote it — UNGATED, like the preview beside it: the SQL is
      // composed on the server and no object-store URL leaves the process, so the panel must not
      // need a token the listing does not. The values come back verbatim (this fixture stamped
      // `n1` and ran as `r1`); reading one as a Machine, a gap or the legacy placeholder is the
      // console's job. `measuredAt` dates the numbers, because a count over a Dataset a Run is
      // still appending to is true of a moment.
      const prov = await app2.inject({
        method: 'GET',
        url: '/api/datasets/echo/provenance?kind=output',
      });
      expect(prov.statusCode).toBe(200);
      expect(prov.json()).toEqual({
        name: 'echo',
        kind: 'output',
        rows: 1,
        groups: [{ machine: 'n1', version: '0.1.0', run: 'r1', rows: 1 }],
        carriesProvenance: true,
        carriesRun: true,
        measuredAt: expect.any(Number),
      });
      const noProvenance = await app2.inject({ method: 'GET', url: '/api/datasets/nope/provenance' });
      expect(noProvenance.statusCode).toBe(404);
    } finally {
      await app2.close();
      if (prevData === undefined) delete process.env.KONTRA_DUCKLAKE_DATA_PATH;
      else process.env.KONTRA_DUCKLAKE_DATA_PATH = prevData;
      if (prevCatalog === undefined) delete process.env.KONTRA_DUCKLAKE_CATALOG;
      else process.env.KONTRA_DUCKLAKE_CATALOG = prevCatalog;
    }
  });

  it('names each output Dataset with the derived run-grain name (ADR 0029 §2)', async () => {
    // The API is the ONE surface that computes the derived name — the Datasets page and
    // `kontra dataset ls` render what it returns, so it must actually emit it. This drives the real
    // DuckLake write for `echo` AND a ledger record for the Run that wrote it, then asserts
    // `/api/datasets` joins the two into `wf-echo-0.1.0--<dt>Z--<runFragment(runId)>`.
    const dir = mkdtempSync(join(tmpdir(), 'kontra-srv-name-'));
    const dataPath = join(dir, 'data');
    mkdirSync(dataPath, { recursive: true });
    const prevData = process.env.KONTRA_DUCKLAKE_DATA_PATH;
    const prevCatalog = process.env.KONTRA_DUCKLAKE_CATALOG;
    process.env.KONTRA_DUCKLAKE_DATA_PATH = `${dataPath}/`;
    process.env.KONTRA_DUCKLAKE_CATALOG = join(dir, 'cat.ducklake');

    const store = new ObjectStore({ backing: new MemoryStore(), prefix: '', publicEndpoint: 'http://localhost:8333' });
    const status = new MaterializationStore({ url: ':memory:' });
    await status.ensureSchema();
    const runId = 'echo-1755612727'; // the shape both start paths mint: <type>-<unixseconds>
    // NO recorded caller identity — the FALLBACK path, which is what every Dataset written before
    // ADR 0029 §2's identity record is in. An empty store rather than the process singleton so the
    // assertion is about the fallback and not about whatever `orchestrator.db` happens to hold.
    const runWorkflows = new RunWorkflowStore({ url: ':memory:' });
    const app2 = buildServer({ repo: new Repo(':memory:'), store, materialization: status, runWorkflows, webRoot: '' });
    try {
      const src = join(mkdtempSync(join(tmpdir(), 'kontra-srv-src-')), 'blob.json');
      writeFileSync(src, JSON.stringify({ results: [{ url: 'a', n: 1 }], failures: [] }));
      await writeDatasetParquet(
        store,
        { sha256: 'deadbeef', actor: 'echo', version: '0.1.0', runId, node: 'n1', runStartedAt: RUN_STARTED },
        { sourceUri: src }
      );
      // The ledger record for the SAME (actor, version, dt) is what resolves the Run behind the row.
      const key = { runId, actor: 'echo', version: '0.1.0', node: 'n1', schemaVersion: MATERIALIZATION_SCHEMA_VERSION };
      await status.claim(key, RUN_STARTED);
      await status.complete(key, { rows: 1, bytes: 64, snapshotId: 1, tbl: 'echo' });

      const list = await app2.inject({ method: 'GET', url: '/api/datasets' });
      expect(list.json()).toMatchObject([
        { kind: 'output', name: 'echo', version: '0.1.0', dt: DT, datasetName: `wf-echo-0.1.0--${DT}Z--${runFragment(runId)}` },
      ]);
    } finally {
      await app2.close();
      if (prevData === undefined) delete process.env.KONTRA_DUCKLAKE_DATA_PATH;
      else process.env.KONTRA_DUCKLAKE_DATA_PATH = prevData;
      if (prevCatalog === undefined) delete process.env.KONTRA_DUCKLAKE_CATALOG;
      else process.env.KONTRA_DUCKLAKE_CATALOG = prevCatalog;
    }
  });

  it('names a Run\'s output after the CALLER WORKFLOW, once for the whole Run (ADR 0029 §2)', async () => {
    // THE DEFECT, END TO END. A workflow `foo` at `1.2.0` dispatches TWO actors, `bar` and `baz`,
    // which write two tables in one Run. §2 derives the name from the caller WORKFLOW's manifest and
    // the Consequences say the name "labels the run's output, which MAY SPAN SEVERAL ACTOR TABLES" —
    // so both rows must read `wf-foo-1.2.0--…`, one Run, one name. Rendering the ACTOR's identity
    // (what shipped first) fails this twice over: it names the wrong thing, and it names it twice.
    //
    // Driven through the REAL routes and the REAL stores: two DuckLake writes, two ledger records,
    // the identity stamped over `PUT /api/runs/:runId/workflow` (the CLI's start path), and the
    // listing read back from `/api/datasets`.
    const dir = mkdtempSync(join(tmpdir(), 'kontra-srv-wfname-'));
    const dataPath = join(dir, 'data');
    mkdirSync(dataPath, { recursive: true });
    const prevData = process.env.KONTRA_DUCKLAKE_DATA_PATH;
    const prevCatalog = process.env.KONTRA_DUCKLAKE_CATALOG;
    process.env.KONTRA_DUCKLAKE_DATA_PATH = `${dataPath}/`;
    process.env.KONTRA_DUCKLAKE_CATALOG = join(dir, 'cat.ducklake');

    const store = new ObjectStore({ backing: new MemoryStore(), prefix: '', publicEndpoint: 'http://localhost:8333' });
    const status = new MaterializationStore({ url: ':memory:' });
    await status.ensureSchema();
    const runWorkflows = new RunWorkflowStore({ url: ':memory:' });
    await runWorkflows.ensureSchema();
    const runId = 'foo-1755612727'; // `<type>-<unixseconds>`, the shape both start paths mint
    const app2 = buildServer({
      repo: new Repo(':memory:'),
      store,
      materialization: status,
      runWorkflows,
      webRoot: '',
    });
    try {
      const src = join(mkdtempSync(join(tmpdir(), 'kontra-srv-src-')), 'blob.json');
      writeFileSync(src, JSON.stringify({ results: [{ url: 'a', n: 1 }], failures: [] }));
      // Two ACTORS, two versions, ONE Run — the fan-out a caller workflow does every time it
      // dispatches more than one Actor.
      for (const [actor, version] of [['bar', '3.0.0'], ['baz', '0.9.1']] as const) {
        await writeDatasetParquet(
          store,
          { sha256: `deadbeef-${actor}`, actor, version, runId, node: 'n1', runStartedAt: RUN_STARTED },
          { sourceUri: src }
        );
        const key = { runId, actor, version, node: 'n1', schemaVersion: MATERIALIZATION_SCHEMA_VERSION };
        await status.claim(key, RUN_STARTED);
        await status.complete(key, { rows: 1, bytes: 64, snapshotId: 1, tbl: actor });
      }

      // BEFORE the stamp: the fallback, and it is actor-grain — two tables, two DIFFERENT names.
      // This is the old behaviour, asserted rather than described, so the fix below is a measured
      // change and not a claim.
      const before = (await app2.inject({ method: 'GET', url: '/api/datasets' })).json() as Array<{
        name: string;
        datasetName?: string;
      }>;
      expect(before.map((r) => r.datasetName)).toEqual([
        `wf-bar-3.0.0--${DT}Z--${runFragment(runId)}`,
        `wf-baz-0.9.1--${DT}Z--${runFragment(runId)}`,
      ]);

      // The CLI's half of the stamp — the route `kontra workflow start` calls after Temporal
      // accepted the start. Idempotent: stated twice, it is the same fact.
      const stamped = await app2.inject({
        method: 'PUT',
        url: `/api/runs/${runId}/workflow`,
        payload: { workflow: 'foo', version: '1.2.0' },
      });
      expect(stamped.statusCode).toBe(200);
      expect(stamped.json()).toEqual({ runId, workflow: 'foo', version: '1.2.0' });
      const again = await app2.inject({
        method: 'PUT',
        url: `/api/runs/${runId}/workflow`,
        payload: { workflow: 'foo', version: '1.2.0' },
      });
      expect(again.json()).toEqual({ runId, workflow: 'foo', version: '1.2.0' });

      // AFTER: ONE Run, ONE name, and it is the WORKFLOW's — over two actor tables that keep their
      // own physical identity.
      const after = (await app2.inject({ method: 'GET', url: '/api/datasets' })).json() as Array<{
        name: string;
        runId?: string;
        datasetName?: string;
      }>;
      const expected = `wf-foo-1.2.0--${DT}Z--${runFragment(runId)}`;
      expect(after.map((r) => r.datasetName)).toEqual([expected, expected]);
      expect(after.map((r) => r.name)).toEqual(['bar', 'baz']); // storage identity is untouched
      expect(after.map((r) => r.runId)).toEqual([runId, runId]);

      // A HALF identity is refused, not coerced: `wf--1.2.0--…` is a worse name than the fallback.
      const blank = await app2.inject({
        method: 'PUT',
        url: `/api/runs/${runId}/workflow`,
        payload: { workflow: '', version: '1.2.0' },
      });
      expect(blank.statusCode).toBe(400);
    } finally {
      await app2.close();
      if (prevData === undefined) delete process.env.KONTRA_DUCKLAKE_DATA_PATH;
      else process.env.KONTRA_DUCKLAKE_DATA_PATH = prevData;
      if (prevCatalog === undefined) delete process.env.KONTRA_DUCKLAKE_CATALOG;
      else process.env.KONTRA_DUCKLAKE_CATALOG = prevCatalog;
    }
  });

  it('names and tags a Dataset when the id page is LARGER than the SQL bind limit', async () => {
    // THE SWALLOW, END TO END (post-merge triage 2026-08-25 §1, reproduced by hand there).
    //
    // `GET /api/datasets` fans EVERY `runId` the ledger resolved into one `IN (?,?,…)` — the id page
    // is sized by the ledger, not by a page of rows, because `listDispatches` is called with no
    // limit. Past 32,766 binds `node:sqlite` raises `too many SQL variables`, and BOTH id reads are
    // inside a `try {} catch {}` that returns the rows anyway. So the failure was never visible as a
    // failure: HTTP 200, every Dataset renamed back to the Actor-grain fallback, the operator's
    // rename and tags gone. That is worse than a 502, which is why it is pinned at the ROUTE.
    //
    // The ledger is stubbed rather than filled: 33,000 real `claim`/`complete` pairs would take a
    // minute to prove a property about a bind COUNT. One ref matches the parquet written below; the
    // other 33,000 are the rest of a long-lived controller's ledger, which is exactly what makes the
    // page oversized. (The record store's own overflow is pinned in `data/datasetRecords.test.ts` —
    // it needs 33k listing ROWS, and one Dataset is what makes this assertion legible.)
    const dir = mkdtempSync(join(tmpdir(), 'kontra-srv-binds-'));
    const dataPath = join(dir, 'data');
    mkdirSync(dataPath, { recursive: true });
    const prevData = process.env.KONTRA_DUCKLAKE_DATA_PATH;
    const prevCatalog = process.env.KONTRA_DUCKLAKE_CATALOG;
    process.env.KONTRA_DUCKLAKE_DATA_PATH = `${dataPath}/`;
    process.env.KONTRA_DUCKLAKE_CATALOG = join(dir, 'cat.ducklake');

    const store = new ObjectStore({ backing: new MemoryStore(), prefix: '', publicEndpoint: 'http://localhost:8333' });
    const status = new MaterializationStore({ url: ':memory:' });
    await status.ensureSchema();
    const runId = 'run-1755612727';
    const runWorkflows = new RunWorkflowStore({ url: ':memory:' });
    await runWorkflows.ensureSchema();
    await runWorkflows.record(runId, 'run', '3.0.0');
    const records = new DatasetRecordStore({ url: ':memory:' });
    await records.addTag(runId, 'keep');
    await records.setRename(runId, 'the-good-one');

    const mine = {
      actor: 'echo',
      version: '0.1.0',
      runId,
      runStartedAt: RUN_STARTED,
      dt: DT,
      nodes: 1,
      rows: 1,
      state: 'complete' as const,
    };
    status.listDispatches = async () => [
      mine,
      ...Array.from({ length: 33_000 }, (_, i) => ({ ...mine, actor: `other${i}`, runId: `run-${i}` })),
    ];

    const app2 = buildServer({
      repo: new Repo(':memory:'),
      store,
      materialization: status,
      records,
      runWorkflows,
      webRoot: '',
    });
    try {
      const src = join(mkdtempSync(join(tmpdir(), 'kontra-srv-src-')), 'blob.json');
      writeFileSync(src, JSON.stringify({ results: [{ url: 'a', n: 1 }], failures: [] }));
      await writeDatasetParquet(
        store,
        { sha256: 'deadbeef', actor: 'echo', version: '0.1.0', runId, node: 'n1', runStartedAt: RUN_STARTED },
        { sourceUri: src }
      );

      const list = await app2.inject({ method: 'GET', url: '/api/datasets' });
      expect(list.statusCode).toBe(200);
      // The CALLER WORKFLOW's name, the operator's rename and the tag — all three of which came back
      // as `wf-echo-0.1.0--…`, no rename and no tags while the identity read was throwing.
      expect(list.json()).toMatchObject([
        {
          name: 'echo',
          runId,
          datasetName: `wf-run-3.0.0--${DT}Z--${runFragment(runId)}`,
          renamedTo: 'the-good-one',
          tags: ['keep'],
        },
      ]);
    } finally {
      await app2.close();
      if (prevData === undefined) delete process.env.KONTRA_DUCKLAKE_DATA_PATH;
      else process.env.KONTRA_DUCKLAKE_DATA_PATH = prevData;
      if (prevCatalog === undefined) delete process.env.KONTRA_DUCKLAKE_CATALOG;
      else process.env.KONTRA_DUCKLAKE_CATALOG = prevCatalog;
    }
  });

  it('tags and renames a Dataset by runId, and the record is authoritative — never Temporal (ADR 0029 §1, §4)', async () => {
    // The whole of issue 02 through the API: tags are a SET (add is idempotent, remove takes one),
    // the rename is a scalar the derived default falls back to, and NOTHING here touches Temporal —
    // the run below never exists to the stubbed client, which is exactly the "tag after the run has
    // closed" case (§4). Every mutation returns the run's whole deviation so a caller updates in place.
    const records = new DatasetRecordStore({ url: ':memory:' });
    await records.ensureSchema();
    const app2 = buildServer({ repo: new Repo(':memory:'), records, webRoot: '' });
    const runId = 'echo-1755612727'; // the shape both start paths mint: <type>-<unixseconds>
    const tags = `/api/datasets/runs/${runId}/tags`;
    const name = `/api/datasets/runs/${runId}/name`;
    try {
      // Add a tag — the response is the post-state deviation.
      const added = await app2.inject({ method: 'POST', url: tags, payload: { tag: 'prod' } });
      expect(added.statusCode).toBe(200);
      expect(added.json()).toEqual({ runId, tags: ['prod'] });

      // Adding the same tag again is idempotent; a different tag converges — both survive.
      await app2.inject({ method: 'POST', url: tags, payload: { tag: 'prod' } });
      const two = await app2.inject({ method: 'POST', url: tags, payload: { tag: 'nightly' } });
      expect(two.json()).toEqual({ runId, tags: ['nightly', 'prod'] }); // sorted

      // Remove one — the DELETE addresses the tag in the path.
      const removed = await app2.inject({ method: 'DELETE', url: `${tags}/prod` });
      expect(removed.json()).toEqual({ runId, tags: ['nightly'] });

      // Rename, then reset back to the derived default.
      const renamed = await app2.inject({ method: 'PUT', url: name, payload: { name: 'the-sweep' } });
      expect(renamed.json()).toEqual({ runId, tags: ['nightly'], renamedTo: 'the-sweep' });
      const reset = await app2.inject({ method: 'DELETE', url: name });
      expect(reset.json()).toEqual({ runId, tags: ['nightly'] });

      // The record is the durable authority — a fresh read of the store agrees with the last response.
      expect(await records.get(runId)).toEqual({ runId, tags: ['nightly'] });

      // An empty tag is refused at the boundary, 400 not 500, with the store's own sentence.
      const bad = await app2.inject({ method: 'POST', url: tags, payload: { tag: '   ' } });
      expect(bad.statusCode).toBe(400);
      expect((bad.json() as { error: string }).error).toContain('cannot be empty');
    } finally {
      await app2.close();
      await records.close();
    }
  });

  it('the record routes answer to KONTRA_RUN_TOKEN, because untagging is what DELETES a Dataset', async () => {
    // These four were registered with no admission check at all, justified as "no operator SQL
    // crosses the wire". SQL is the wrong test: ADR 0029 §3 makes a tag the KEEP/COLLECT authority
    // `classifySweep` reads, so DELETE …/tags/:tag is the delete button for a Dataset one sweep tick
    // later. The bug this pins is not that they were open — the whole Run surface is open by default
    // — but that they were UNPROTECTABLE: an operator who set KONTRA_RUN_TOKEN closed `start`,
    // `cancel` and `terminate` and still could not close the one route that destroys data.
    const records = new DatasetRecordStore({ url: ':memory:' });
    await records.ensureSchema();
    const runId = 'echo-1755612727';
    const tags = `/api/datasets/runs/${runId}/tags`;
    const name = `/api/datasets/runs/${runId}/name`;
    const prev = process.env.KONTRA_RUN_TOKEN;
    process.env.KONTRA_RUN_TOKEN = 'sekrit';
    const app2 = buildServer({ repo: new Repo(':memory:'), records, webRoot: '' });
    try {
      // Seed a tag with the right credential, so the DELETE below has something real to destroy.
      const seeded = await app2.inject({
        method: 'POST',
        url: tags,
        headers: { authorization: 'Bearer sekrit' },
        payload: { tag: 'keep' },
      });
      expect(seeded.statusCode).toBe(200);

      // Every mutating route refuses an unauthenticated caller. The DELETE is the one that matters:
      // stripping `keep` is what would hand this Dataset to the sweeper.
      for (const [method, url] of [
        ['POST', tags],
        ['DELETE', `${tags}/keep`],
        ['PUT', name],
        ['DELETE', name],
      ] as const) {
        const res = await app2.inject({ method, url, payload: { tag: 'x', name: 'x' } });
        expect(res.statusCode, `${method} ${url}`).toBe(401);
      }

      // …and a wrong token is refused too, not merely a missing one.
      const wrong = await app2.inject({
        method: 'DELETE',
        url: `${tags}/keep`,
        headers: { authorization: 'Bearer nope' },
      });
      expect(wrong.statusCode).toBe(401);

      // The tag survived every refused attempt — this is the claim that matters, not the status code.
      expect(await records.get(runId)).toEqual({ runId, tags: ['keep'] });
    } finally {
      await app2.close();
      await records.close();
      if (prev === undefined) delete process.env.KONTRA_RUN_TOKEN;
      else process.env.KONTRA_RUN_TOKEN = prev;
    }
  });

  it('…and stays OPEN with no token set, like the rest of the Run surface', async () => {
    // The posture is deliberate (`checkOptionalBearer`, not `checkBearer`): this box runs with
    // KONTRA_RUN_TOKEN deliberately empty, and fail-closed would 503 every tag on it. Pinned so the
    // gate above cannot quietly become fail-closed and break a local operator's Datasets page.
    const records = new DatasetRecordStore({ url: ':memory:' });
    await records.ensureSchema();
    const prev = process.env.KONTRA_RUN_TOKEN;
    delete process.env.KONTRA_RUN_TOKEN;
    const app2 = buildServer({ repo: new Repo(':memory:'), records, webRoot: '' });
    try {
      const res = await app2.inject({
        method: 'POST',
        url: '/api/datasets/runs/echo-1755612727/tags',
        payload: { tag: 'prod' },
      });
      expect(res.statusCode).toBe(200);
      expect(res.json()).toEqual({ runId: 'echo-1755612727', tags: ['prod'] });
    } finally {
      await app2.close();
      await records.close();
      if (prev !== undefined) process.env.KONTRA_RUN_TOKEN = prev;
    }
  });

  it('the listing carries a Dataset\'s tags and rename, keyed by the resolved run (ADR 0029)', async () => {
    // End to end: a real lake write + a ledger record resolve the Run behind the row, then the
    // Dataset record layers tags and a rename over the derived name. The listing shows both, and the
    // derived name stays present under the rename (issue 01's invariant).
    const dir = mkdtempSync(join(tmpdir(), 'kontra-srv-tag-'));
    const dataPath = join(dir, 'data');
    mkdirSync(dataPath, { recursive: true });
    const prevData = process.env.KONTRA_DUCKLAKE_DATA_PATH;
    const prevCatalog = process.env.KONTRA_DUCKLAKE_CATALOG;
    process.env.KONTRA_DUCKLAKE_DATA_PATH = `${dataPath}/`;
    process.env.KONTRA_DUCKLAKE_CATALOG = join(dir, 'cat.ducklake');

    const store = new ObjectStore({ backing: new MemoryStore(), prefix: '', publicEndpoint: 'http://localhost:8333' });
    const status = new MaterializationStore({ url: ':memory:' });
    await status.ensureSchema();
    const records = new DatasetRecordStore({ url: ':memory:' });
    await records.ensureSchema();
    const runId = 'echo-1755612727'; // the shape both start paths mint: <type>-<unixseconds>
    const app2 = buildServer({ repo: new Repo(':memory:'), store, materialization: status, records, webRoot: '' });
    try {
      const src = join(mkdtempSync(join(tmpdir(), 'kontra-srv-src-')), 'blob.json');
      writeFileSync(src, JSON.stringify({ results: [{ url: 'a', n: 1 }], failures: [] }));
      await writeDatasetParquet(
        store,
        { sha256: 'deadbeef', actor: 'echo', version: '0.1.0', runId, node: 'n1', runStartedAt: RUN_STARTED },
        { sourceUri: src }
      );
      const key = { runId, actor: 'echo', version: '0.1.0', node: 'n1', schemaVersion: MATERIALIZATION_SCHEMA_VERSION };
      await status.claim(key, RUN_STARTED);
      await status.complete(key, { rows: 1, bytes: 64, snapshotId: 1, tbl: 'echo' });

      // Tag and rename the resolved Run's Dataset.
      await records.addTag(runId, 'prod');
      await records.setRename(runId, 'nightly-echo');

      const list = await app2.inject({ method: 'GET', url: '/api/datasets' });
      expect(list.json()).toMatchObject([
        {
          kind: 'output',
          name: 'echo',
          runId,
          datasetName: `wf-echo-0.1.0--${DT}Z--${runFragment(runId)}`, // the derived default is still here…
          renamedTo: 'nightly-echo', // …and the rename beside it, for `renamedTo ?? datasetName`
          tags: ['prod'],
        },
      ]);
    } finally {
      await app2.close();
      await records.close();
      if (prevData === undefined) delete process.env.KONTRA_DUCKLAKE_DATA_PATH;
      else process.env.KONTRA_DUCKLAKE_DATA_PATH = prevData;
      if (prevCatalog === undefined) delete process.env.KONTRA_DUCKLAKE_CATALOG;
      else process.env.KONTRA_DUCKLAKE_CATALOG = prevCatalog;
    }
  });

  /**
   * TAGGING A TEMPORARY DATASET, THROUGH THE ROUTES THE PAGE ACTUALLY CALLS.
   *
   * The Datasets page offers the tag affordance on a temp because the listing stamps a `runId` on it
   * — the owner marker resolves one before any row lands (ADR 0029 §2). This walks the whole thing
   * over HTTP rather than through the stores: POST the tag against the temp's owner, read the
   * listing, then DELETE the tagged temp.
   *
   * The two facts the surface has to state, both proven here rather than argued:
   *
   *   1. THE TAG IS RUN-GRAIN. One POST puts the tag on the temp AND on the durable Dataset that
   *      Run promoted into, because the record is keyed by `runId` and both tables are that Run's.
   *      Measured on the live controller too: `tmp_4e9b1b23` and `lame_demo` carry one Run.
   *   2. A TAG DOES NOT LOCK A TEMP. The DELETE route still removes it (ADR 0028's ownership is not
   *      overridden by a label), and the durable Dataset keeps the tag — which is what the tag was
   *      for. The page's confirmation names the tags so this is never a surprise.
   */
  it('tags a TEMPORARY Dataset by its owner Run, and the tag survives deleting it (ADR 0029 §4, ADR 0028)', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'kontra-srv-temptag-'));
    const dataPath = join(dir, 'data');
    mkdirSync(dataPath, { recursive: true });
    const prevData = process.env.KONTRA_DUCKLAKE_DATA_PATH;
    const prevCatalog = process.env.KONTRA_DUCKLAKE_CATALOG;
    process.env.KONTRA_DUCKLAKE_DATA_PATH = `${dataPath}/`;
    process.env.KONTRA_DUCKLAKE_CATALOG = join(dir, 'cat.ducklake');

    const store = new ObjectStore({ backing: new MemoryStore(), prefix: '', publicEndpoint: 'http://localhost:8333' });
    const records = new DatasetRecordStore({ url: ':memory:' });
    await records.ensureSchema();
    const runId = 'nscheck-1755612727';
    const app2 = buildServer({ repo: new Repo(':memory:'), store, records, webRoot: '' });
    try {
      // One Run writes its temp and the durable Dataset it promoted into — the shape on the live box.
      const src = join(mkdtempSync(join(tmpdir(), 'kontra-srv-src-')), 'blob.json');
      writeFileSync(src, JSON.stringify({ results: [{ url: 'a', ok: false }], failures: [] }));
      for (const actor of ['tmp_stage', 'lame_demo']) {
        await writeDatasetParquet(
          store,
          { sha256: 'deadbeef', actor, version: '0.1.0', runId, node: 'n1', runStartedAt: RUN_STARTED },
          { sourceUri: src }
        );
      }
      // The owner marker is what makes the temp a temp — never the name prefix (temp-datasets 01).
      await store.put(
        `datasets/tmp_stage/_owner.json`,
        Buffer.from(JSON.stringify({ owner: runId, createdAt: RUN_STARTED }), 'utf8')
      );

      // The page's tag button, over the wire: keyed by the temp's OWNER Run.
      const tagged = await app2.inject({
        method: 'POST',
        url: `/api/datasets/runs/${runId}/tags`,
        payload: { tag: 'keep-this' },
      });
      expect(tagged.statusCode).toBe(200);
      expect(tagged.json()).toEqual({ runId, tags: ['keep-this'] });

      const listed = (await app2.inject({ method: 'GET', url: '/api/datasets' })).json() as Array<{
        name: string;
        temporary?: boolean;
        owner?: string;
        runId?: string;
        tags?: string[];
      }>;
      const temp = listed.find((r) => r.name === 'tmp_stage');
      const durable = listed.find((r) => r.name === 'lame_demo');
      // The temp IS addressable and IS tagged — and is still a temp: nothing in the record store
      // touches the owner marker, so the badge, the DELETE route and the sweep read one authority.
      expect(temp).toMatchObject({ temporary: true, owner: runId, runId, tags: ['keep-this'] });
      // ...and so is the durable Dataset of the same Run. ONE tag, one Run, two tables.
      expect(durable).toMatchObject({ runId, tags: ['keep-this'] });
      expect(durable!.temporary).toBeUndefined();

      // A tag marks; it does not lock. The explicit verb still removes the tagged temp.
      const deleted = await app2.inject({ method: 'DELETE', url: '/api/datasets/tmp_stage' });
      expect(deleted.statusCode).toBe(200);
      expect(deleted.json()).toMatchObject({ deleted: true, name: 'tmp_stage', owner: runId });

      const after = (await app2.inject({ method: 'GET', url: '/api/datasets' })).json() as Array<{
        name: string;
        tags?: string[];
      }>;
      expect(after.map((r) => r.name)).toEqual(['lame_demo']);
      // The tag outlives the temp, on the output it was really keeping.
      expect(after[0]!.tags).toEqual(['keep-this']);
      expect(await records.get(runId)).toEqual({ runId, tags: ['keep-this'] });
    } finally {
      await app2.close();
      await records.close();
      if (prevData === undefined) delete process.env.KONTRA_DUCKLAKE_DATA_PATH;
      else process.env.KONTRA_DUCKLAKE_DATA_PATH = prevData;
      if (prevCatalog === undefined) delete process.env.KONTRA_DUCKLAKE_CATALOG;
      else process.env.KONTRA_DUCKLAKE_CATALOG = prevCatalog;
    }
  });

  it('resolves actor@version to a dispatch — the caller names an actor, never a UUID', async () => {
    // The addressing surface. It reads the LEDGER, not the lake: the ledger already knows
    // which actor and version ran and exactly when, which is precisely what an operator
    // names. The run id comes BACK in the answer and stays inside the tooling.
    const status = new MaterializationStore({ url: ':memory:' });
    await status.ensureSchema();
    const app2 = buildServer({ repo: new Repo(':memory:'), materialization: status, webRoot: '' });
    try {
      const key = {
        runId: '0f2c9a1e-7b3d-4c58-9a10-6d2f8b4e1c37',
        actor: 'crawl4ai',
        version: '1.0.0',
        node: 'n1',
        schemaVersion: MATERIALIZATION_SCHEMA_VERSION,
      };
      await status.claim(key, RUN_STARTED);
      await status.complete(key, { rows: 7, bytes: 128, snapshotId: 1, tbl: 'crawl4ai' });

      const res = await app2.inject({ method: 'GET', url: '/api/datasets/runs?actor=crawl4ai' });
      expect(res.statusCode).toBe(200);
      const { runs } = res.json() as { runs: Array<Record<string, unknown>> };
      expect(runs).toHaveLength(1);
      // `dt` is the addressable half — it matches the `dt=` directory the output lives under.
      expect(runs[0]).toMatchObject({
        actor: 'crawl4ai',
        version: '1.0.0',
        dt: DT,
        runId: key.runId,
        nodes: 1,
        rows: 7,
        state: 'complete',
      });

      // A dispatch that is still running is addressable BEFORE it finishes — that is the
      // point of reading the ledger rather than the lake.
      const running = { ...key, runId: 'run-in-flight', node: 'n2' };
      await status.claim(running, RUN_STARTED + 3_600_000);
      const both = await app2.inject({ method: 'GET', url: '/api/datasets/runs?actor=crawl4ai' });
      const list = (both.json() as { runs: Array<Record<string, unknown>> }).runs;
      expect(list).toHaveLength(2);
      expect(list[0]).toMatchObject({ runId: 'run-in-flight', state: 'running' }); // newest first

      // `dt` narrows by PREFIX, and an actor nobody ran resolves to nothing.
      const day = await app2.inject({
        method: 'GET',
        url: `/api/datasets/runs?actor=crawl4ai&dt=${DT}`,
      });
      expect((day.json() as { runs: unknown[] }).runs).toHaveLength(1);
      const none = await app2.inject({ method: 'GET', url: '/api/datasets/runs?actor=nobody' });
      expect((none.json() as { runs: unknown[] }).runs).toEqual([]);
    } finally {
      await app2.close();
      await status.close();
    }
  });

  it('lists no datasets when the store is disabled (no S3, no backing)', async () => {
    // default beforeEach app builds a plain ObjectStore (no endpoint) → nothing to list.
    const res = await app.inject({ method: 'GET', url: '/api/datasets' });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toEqual([]);
  });

});

describe('static assets', () => {
  let dir: string;
  let app: FastifyInstance;

  beforeEach(async () => {
    dir = mkdtempSync(join(tmpdir(), 'kontra-web-'));
    mkdirSync(join(dir, 'assets'));
    // Big enough to clear the compression threshold, repetitive enough to actually compress.
    writeFileSync(join(dir, 'assets', 'index-DEADBEEF.js'), 'console.log("x");'.repeat(400));
    writeFileSync(join(dir, 'index.html'), '<!doctype html><title>k</title>');
    app = buildServer({ repo: new Repo(':memory:'), webRoot: dir });
    await app.ready();
  });
  afterEach(async () => app.close());

  /**
   * Vite content-hashes these names, so the bytes behind one can never change. They were served
   * `max-age=0`, i.e. re-validated on every visit.
   *
   * The subtlety this pins: fastify-static invokes `setHeaders` and THEN applies send's own
   * computed headers, so setting Cache-Control in the hook does nothing unless send's
   * `cacheControl` is disabled. The header looked set in the source and was not on the wire.
   */
  it('caches a content-hashed asset immutably', async () => {
    const res = await app.inject({ method: 'GET', url: '/assets/index-DEADBEEF.js' });
    expect(res.statusCode).toBe(200);
    expect(res.headers['cache-control']).toBe('public, max-age=31536000, immutable');
  });

  /**
   * index.html names the current asset hashes. Cache it and a deploy becomes invisible.
   *
   * `no-store`, NOT `no-cache`, AND THE DIFFERENCE IS THE WHOLE BUG. `no-cache` does not mean
   * "do not cache" — it means "cache, but revalidate", so the browser still sent a conditional
   * request and the server was free to answer 304. It did: the shell went out with
   * `etag: W/"184-0"`, where `-0` is the mtime, and the image extracts the SPA from a tarball
   * with a FIXED mtime (deliberately — that is what makes the bundle's sha reproducible) while
   * Vite's asset hashes keep the file the same length. Identical validator on every build this
   * image has ever served, so every console rebuild was invisible to an already-loaded browser.
   *
   * Turning the validators off was not enough either: `lastModified: false` stops the header
   * going out but send still honours an incoming If-Modified-Since. The shell is read once at
   * boot and served from memory with `no-store`, so there is no conditional path left to take.
   */
  it('does not cache index.html', async () => {
    const res = await app.inject({ method: 'GET', url: '/' });
    expect(res.headers['cache-control']).toBe('no-store');
  });

  it('compresses when the client accepts it, and only then', async () => {
    const gz = await app.inject({
      method: 'GET',
      url: '/assets/index-DEADBEEF.js',
      headers: { 'accept-encoding': 'gzip' },
    });
    expect(gz.headers['content-encoding']).toBe('gzip');
    expect(gz.rawPayload.length).toBeLessThan(1000);

    const raw = await app.inject({ method: 'GET', url: '/assets/index-DEADBEEF.js' });
    expect(raw.headers['content-encoding']).toBeUndefined();
    expect(raw.rawPayload.length).toBe(6800);
  });
});
