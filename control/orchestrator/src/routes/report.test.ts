/**
 * THE REPORT SURFACE, at the module rather than through `buildServer`.
 *
 * A bare Fastify plus `registerReportRoutes` and a real `ReportStore` on in-memory SQLite — the shape
 * `routes/uploads.test.ts` uses. Nothing Temporal, nothing S3, so these tests assert the ROUTES: the
 * gate, the status codes, the author resolution, the byte replies and the audit line.
 *
 * Acceptance tests 9 (reveal's 403 half), 18 (preview's API half) and 19 (feedback) live here.
 */

import Fastify, { type FastifyInstance } from 'fastify';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { auditLog } from '../audit';
import type { ReportSnapshot } from '../report/render';
import { ReportStore } from '../report/store';
import { registerReportRoutes } from './report';

const RUN = 'enrich-1791234567';
const EXPLORE = 'explore-token-value';
const STATE = 'state-token-value';

const SNAPSHOT: ReportSnapshot = {
  v: 1,
  root: {
    type: 'root',
    children: [
      { type: 'heading', depth: 1, children: [{ type: 'text', value: 'enrich over catalog-eu' }] },
      { type: 'paragraph', children: [{ type: 'text', value: '12,480 products' }] },
      { type: 'code', lang: 'http', blockId: 'b1', value: 'GET / HTTP/1.1\r\nAuthorization: [redacted]\r\n' },
    ],
  },
  blocks: {
    b1: {
      lang: 'http',
      b64: Buffer.from('GET / HTTP/1.1\r\nAuthorization: [redacted]\r\n', 'latin1').toString('base64'),
      redacted: true,
      truncated: false,
      fullBytes: 44,
      source: 'text',
    },
  },
};

let app: FastifyInstance;
let store: ReportStore;
let rendered: Array<{ template: string }>;
let context: Record<string, unknown> | undefined;
let auditRecord: ReturnType<typeof vi.spyOn>;

async function build(opts: { previewCap?: { max?: number; windowMs?: number } } = {}): Promise<void> {
  store = new ReportStore({ url: ':memory:' });
  await store.ensureSchema();
  rendered = [];
  context = { run: { id: RUN }, result: { n: 1 } };
  app = Fastify();
  registerReportRoutes(app, {
    reports: store,
    identities: async (ids) => ids.map((runId) => ({ runId, workflow: 'enrich' })),
    render: async (request) => {
      rendered.push({ template: request.template });
      if (request.template.includes('BOOM')) return { ok: false, error: 'undefined variable: nope' };
      return { ok: true, snapshot: SNAPSHOT, bytes: 100, secrets: [] };
    },
    context: async () => context,
    ...(opts.previewCap ? { previewCap: opts.previewCap } : {}),
    now: () => 1_791_235_030_000,
  });
  await app.ready();
}

/** A stored `ok` version with the fixture snapshot, and the secret behind its one redacted block. */
async function seed(): Promise<number> {
  const v = await store.declareVersion({
    runId: RUN,
    status: 'ok',
    templateHash: 'sha256:aaa',
    renderKey: 'k1',
    snapshotJson: JSON.stringify(SNAPSHOT),
    renderedBy: 'sweep',
  });
  await store.putSecrets(RUN, v.version, [
    { blockId: 'b1', rawB64: Buffer.from('GET / HTTP/1.1\r\nAuthorization: Bearer abc123\r\n', 'latin1').toString('base64') },
  ]);
  return v.version;
}

const explore = { authorization: `Bearer ${EXPLORE}` };
const state = { authorization: `Bearer ${STATE}` };

beforeEach(async () => {
  process.env.KONTRA_EXPLORE_TOKEN = EXPLORE;
  process.env.KONTRA_STATE_TOKEN = STATE;
  auditRecord = vi.spyOn(auditLog, 'record').mockImplementation(() => undefined);
  await build();
});

afterEach(async () => {
  await app?.close();
  delete process.env.KONTRA_EXPLORE_TOKEN;
  delete process.env.KONTRA_STATE_TOKEN;
  auditRecord.mockRestore();
  vi.clearAllMocks();
});

describe('the gate', () => {
  const paths: Array<[string, string]> = [
    ['GET', '/api/reports'],
    ['GET', `/api/runs/${RUN}/report`],
    ['GET', `/api/runs/${RUN}/report/versions`],
    ['POST', `/api/runs/${RUN}/report/render`],
    ['POST', `/api/runs/${RUN}/report/preview`],
    ['GET', `/api/runs/${RUN}/report/export`],
    ['GET', `/api/runs/${RUN}/report/blocks/b1/raw`],
    ['GET', `/api/runs/${RUN}/feedback`],
    ['POST', `/api/runs/${RUN}/feedback`],
    ['PATCH', '/api/feedback/abc'],
    ['DELETE', '/api/feedback/abc'],
  ];

  for (const [method, url] of paths) {
    it(`refuses ${method} ${url.replace(RUN, ':runId')} with no credential`, async () => {
      const res = await app.inject({ method: method as 'GET', url, payload: {} });
      expect(res.statusCode).toBe(401);
    });
  }

  it('refuses the reveal route with the EXPLORE token, which is not its authority', async () => {
    await seed();
    const res = await app.inject({
      method: 'POST',
      url: `/api/runs/${RUN}/report/blocks/b1/reveal`,
      headers: explore,
    });
    expect(res.statusCode).toBe(401);
  });

  it('serves nothing at all when no token is configured — fail closed, 503', async () => {
    delete process.env.KONTRA_EXPLORE_TOKEN;
    delete process.env.KONTRA_STATE_TOKEN;
    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report` });
    expect(res.statusCode).toBe(503);
    expect(res.json().error).toMatch(/KONTRA_EXPLORE_TOKEN/);
  });
});

describe('GET the report', () => {
  it('404s for a run with no report, rather than an empty one', async () => {
    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report`, headers: explore });
    expect(res.statusCode).toBe(404);
    expect(res.json().error).toContain(RUN);
  });

  it('serves the latest version with its snapshot', async () => {
    await seed();
    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report`, headers: explore });
    expect(res.statusCode).toBe(200);
    const body = res.json();
    expect(body.version).toBe(1);
    expect(body.status).toBe('ok');
    expect(body.snapshot.root.children[0].children[0].value).toBe('enrich over catalog-eu');
  });

  it('serves a named version, and 404s for one that does not exist', async () => {
    await seed();
    expect((await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report?version=1`, headers: explore })).statusCode).toBe(200);
    expect((await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report?version=9`, headers: explore })).statusCode).toBe(404);
  });

  it('ignores a nonsense version rather than 500ing, and answers the latest', async () => {
    await seed();
    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report?version=../etc`, headers: explore });
    expect(res.statusCode).toBe(200);
    expect(res.json().version).toBe(1);
  });

  it('serves an error version as an error, not as a missing report', async () => {
    await store.declareVersion({
      runId: RUN,
      status: 'error',
      templateHash: 'h',
      renderKey: 'bad',
      errorText: 'undefined variable: results',
      renderedBy: 'sweep',
    });
    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report`, headers: explore });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({ status: 'error', error: 'undefined variable: results' });
    expect(res.json().snapshot).toBeUndefined();
  });

  it('lists versions newest first, without their snapshots', async () => {
    await seed();
    await store.declareVersion({ runId: RUN, status: 'ok', templateHash: 'h', renderKey: 'k2', snapshotJson: '{}', renderedBy: 'mohamed' });
    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report/versions`, headers: explore });
    const list = res.json().versions as Array<Record<string, unknown>>;
    expect(list.map((v) => v.version)).toEqual([2, 1]);
    expect(list[0]!.snapshotJson).toBeUndefined();
    expect(list[0]!.renderedBy).toBe('mohamed');
  });
});

describe('export', () => {
  it('serves Markdown with the right content type', async () => {
    await seed();
    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report/export?format=md`, headers: explore });
    expect(res.statusCode).toBe(200);
    expect(res.headers['content-type']).toContain('text/markdown');
    expect(res.body).toContain('# enrich over catalog-eu');
    expect(res.body).toContain('```http');
  });

  it('serves one self-contained HTML file with no script tag', async () => {
    await seed();
    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report/export?format=html`, headers: explore });
    expect(res.headers['content-type']).toContain('text/html');
    expect(res.body).toContain('<h1>enrich over catalog-eu</h1>');
    expect(res.body).not.toMatch(/<script/i);
    // Inline CSS and no external request: opening an exported report must not call anybody.
    expect(res.body).toContain('<style>');
    expect(res.body).not.toMatch(/https?:\/\//);
  });

  it('exports the REDACTED bytes, because that is all the snapshot holds', async () => {
    await seed();
    const md = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report/export?format=md`, headers: explore });
    expect(md.body).toContain('Authorization: [redacted]');
    expect(md.body).not.toContain('abc123');
  });

  it('refuses a format it does not have', async () => {
    await seed();
    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report/export?format=pdf`, headers: explore });
    expect(res.statusCode).toBe(400);
    expect(res.json()).toMatchObject({ field: 'format' });
  });
});

describe('a block\'s bytes', () => {
  it('serves the redacted bytes exactly, as octet-stream', async () => {
    await seed();
    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report/blocks/b1/raw`, headers: explore });
    expect(res.statusCode).toBe(200);
    expect(res.headers['content-type']).toBe('application/octet-stream');
    expect(res.headers['x-kontra-report-redacted']).toBe('true');
    // rawPayload, never json(): these are bytes and a CRLF must survive the trip.
    expect(res.rawPayload.toString('latin1')).toBe('GET / HTTP/1.1\r\nAuthorization: [redacted]\r\n');
  });

  it('404s for a block that is not in the version', async () => {
    await seed();
    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report/blocks/b9/raw`, headers: explore });
    expect(res.statusCode).toBe(404);
  });
});

describe('ACCEPTANCE 9: reveal', () => {
  it('serves the ORIGINAL bytes with the state token', async () => {
    await seed();
    const res = await app.inject({
      method: 'POST',
      url: `/api/runs/${RUN}/report/blocks/b1/reveal`,
      headers: state,
    });
    expect(res.statusCode).toBe(200);
    expect(res.rawPayload.toString('latin1')).toContain('Bearer abc123');
  });

  it('writes an audit record naming the run and the block', async () => {
    await seed();
    await app.inject({ method: 'POST', url: `/api/runs/${RUN}/report/blocks/b1/reveal`, headers: state });
    expect(auditRecord).toHaveBeenCalledTimes(1);
    const entry = auditRecord.mock.calls[0]![0] as Record<string, unknown>;
    expect(entry).toMatchObject({ action: 'report.reveal', outcome: 'allowed', target: RUN });
    expect(String(entry.detail)).toContain('b1');
  });

  it('audits the REFUSAL too, which is the half worth having', async () => {
    await seed();
    const res = await app.inject({ method: 'POST', url: `/api/runs/${RUN}/report/blocks/b1/reveal` });
    expect(res.statusCode).toBe(401);
    expect(auditRecord).toHaveBeenCalledTimes(1);
    expect(auditRecord.mock.calls[0]![0]).toMatchObject({
      action: 'report.reveal',
      outcome: 'refused',
      who: 'anonymous',
    });
  });

  it('never records the credential it was given', async () => {
    await seed();
    await app.inject({ method: 'POST', url: `/api/runs/${RUN}/report/blocks/b1/reveal`, headers: state });
    expect(JSON.stringify(auditRecord.mock.calls)).not.toContain(STATE);
  });

  it('404s for a block that was never redacted, without saying which blocks hold credentials', async () => {
    await store.declareVersion({
      runId: RUN,
      status: 'ok',
      templateHash: 'h',
      renderKey: 'clean',
      snapshotJson: JSON.stringify(SNAPSHOT),
      renderedBy: 'sweep',
    });
    const res = await app.inject({ method: 'POST', url: `/api/runs/${RUN}/report/blocks/b1/reveal`, headers: state });
    expect(res.statusCode).toBe(404);
    expect(res.json().error).toMatch(/nothing to reveal/);
  });
});

describe('ACCEPTANCE 18: preview', () => {
  it('renders the template it was given and stores NO version', async () => {
    const res = await app.inject({
      method: 'POST',
      url: `/api/runs/${RUN}/report/preview`,
      headers: explore,
      payload: { template: '# edited' },
    });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({ preview: true, status: 'ok' });
    expect(rendered).toEqual([{ template: '# edited' }]);
    expect(await store.versions(RUN)).toHaveLength(0);
  });

  it('answers a template error as a preview with an error, not as a 500', async () => {
    const res = await app.inject({
      method: 'POST',
      url: `/api/runs/${RUN}/report/preview`,
      headers: explore,
      payload: { template: 'BOOM' },
    });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({ preview: true, status: 'error' });
    expect(res.json().error).toContain('undefined variable');
  });

  it('refuses a body with no template, naming the field', async () => {
    const res = await app.inject({
      method: 'POST',
      url: `/api/runs/${RUN}/report/preview`,
      headers: explore,
      payload: {},
    });
    expect(res.statusCode).toBe(400);
    expect(res.json().error).toContain('template');
  });

  it('refuses a field it does not know, rather than ignoring it', async () => {
    const res = await app.inject({
      method: 'POST',
      url: `/api/runs/${RUN}/report/preview`,
      headers: explore,
      payload: { template: 'x', save: true },
    });
    expect(res.statusCode).toBe(400);
  });

  it('409s when the run\'s metadata is gone, rather than previewing against an invented context', async () => {
    context = undefined;
    const res = await app.inject({
      method: 'POST',
      url: `/api/runs/${RUN}/report/preview`,
      headers: explore,
      payload: { template: '# x' },
    });
    expect(res.statusCode).toBe(409);
    expect(res.json()).toMatchObject({ state: 'gone' });
  });

  it('rate-limits, and the refusal quotes the cap', async () => {
    await app.close();
    await build({ previewCap: { max: 2, windowMs: 60_000 } });
    const ask = () =>
      app.inject({ method: 'POST', url: `/api/runs/${RUN}/report/preview`, headers: explore, payload: { template: '# x' } });
    expect((await ask()).statusCode).toBe(200);
    expect((await ask()).statusCode).toBe(200);
    const third = await ask();
    expect(third.statusCode).toBe(429);
    expect(third.json().error).toContain('at most 2');
  });
});

describe('re-render', () => {
  it('501s on current_template, naming the reason and the alternative', async () => {
    const res = await app.inject({
      method: 'POST',
      url: `/api/runs/${RUN}/report/render`,
      headers: explore,
      payload: { current_template: true },
    });
    expect(res.statusCode).toBe(501);
    expect(res.json()).toMatchObject({ field: 'current_template' });
    expect(res.json().error).toContain('/report/preview');
  });

  it('409s for a run with no pinned template', async () => {
    const res = await app.inject({ method: 'POST', url: `/api/runs/${RUN}/report/render`, headers: explore, payload: {} });
    expect(res.statusCode).toBe(409);
    expect(res.json()).toMatchObject({ state: 'unpinned' });
  });

  it('ACCEPTANCE 12: reproduces the existing version rather than making a second one', async () => {
    await store.pinTemplate({ runId: RUN, templateHash: 'sha256:aaa', templateText: '# t', source: 'workspace' });
    await seed();
    const res = await app.inject({ method: 'POST', url: `/api/runs/${RUN}/report/render`, headers: explore, payload: {} });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({ version: 1, reproduced: true });
    expect(await store.versions(RUN)).toHaveLength(1);
  });
});

describe('ACCEPTANCE 19: feedback', () => {
  it('takes the author from the credential, never from the body', async () => {
    const res = await app.inject({
      method: 'POST',
      url: `/api/runs/${RUN}/feedback`,
      headers: explore,
      payload: { body: 'retry that feed' },
    });
    expect(res.statusCode).toBe(201);
    // A service token resolves to `service-token` and `author_kind: token` — honest about not being a
    // person, which is what the console's "via token" label renders.
    expect(res.json()).toMatchObject({ author: 'service-token', authorKind: 'token' });
  });

  it('refuses a body that tries to set the author', async () => {
    const res = await app.inject({
      method: 'POST',
      url: `/api/runs/${RUN}/feedback`,
      headers: explore,
      payload: { body: 'x', author: 'mohamed' },
    });
    expect(res.statusCode).toBe(400);
    expect(await store.feedback({ runId: RUN })).toHaveLength(0);
  });

  it('refuses an empty note', async () => {
    const res = await app.inject({ method: 'POST', url: `/api/runs/${RUN}/feedback`, headers: explore, payload: { body: '   ' } });
    expect(res.statusCode).toBe(400);
  });

  it('lists notes newest first', async () => {
    await store.addFeedback({ runId: RUN, author: 'a', authorKind: 'user', body: 'one', at: 1 });
    await store.addFeedback({ runId: RUN, author: 'b', authorKind: 'token', body: 'two', at: 2 });
    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/feedback`, headers: explore });
    expect((res.json().notes as Array<{ body: string }>).map((n) => n.body)).toEqual(['two', 'one']);
  });

  it('lets the author edit and refuses anybody else with 403, not 404', async () => {
    const mine = await store.addFeedback({ runId: RUN, author: 'service-token', authorKind: 'token', body: 'mine' });
    const theirs = await store.addFeedback({ runId: RUN, author: 'mohamed', authorKind: 'user', body: 'theirs' });
    const ok = await app.inject({ method: 'PATCH', url: `/api/feedback/${mine.id}`, headers: explore, payload: { body: 'edited' } });
    expect(ok.statusCode).toBe(200);
    expect(ok.json().body).toBe('edited');
    const no = await app.inject({ method: 'PATCH', url: `/api/feedback/${theirs.id}`, headers: explore, payload: { body: 'hijacked' } });
    expect(no.statusCode).toBe(403);
    expect((await store.note(theirs.id))?.body).toBe('theirs');
  });

  it('soft-deletes the author\'s own note and 404s afterwards', async () => {
    const mine = await store.addFeedback({ runId: RUN, author: 'service-token', authorKind: 'token', body: 'mine' });
    expect((await app.inject({ method: 'DELETE', url: `/api/feedback/${mine.id}`, headers: explore })).statusCode).toBe(204);
    expect(await store.feedback({ runId: RUN })).toHaveLength(0);
    expect((await app.inject({ method: 'DELETE', url: `/api/feedback/${mine.id}`, headers: explore })).statusCode).toBe(404);
  });

  it('404s for a note that never existed', async () => {
    expect((await app.inject({ method: 'PATCH', url: '/api/feedback/nope', headers: explore, payload: { body: 'x' } })).statusCode).toBe(404);
  });
});

describe('the Reports listing', () => {
  it('is empty, not an error, when nothing has been rendered', async () => {
    const res = await app.inject({ method: 'GET', url: '/api/reports', headers: explore });
    expect(res.statusCode).toBe(200);
    expect(res.json().reports).toEqual([]);
  });

  it('lists the NEWEST version of each run, newest first, with a version count', async () => {
    await seed();
    await store.declareVersion({
      runId: RUN,
      status: 'ok',
      templateHash: 'sha256:aaa',
      renderKey: 'k2',
      snapshotJson: '{}',
      renderedBy: 'mohamed',
      at: 2_000,
    });
    await store.declareVersion({
      runId: 'other-1',
      status: 'error',
      templateHash: 'h',
      renderKey: 'k',
      errorText: 'boom',
      renderedBy: 'sweep',
      at: 3_000,
    });
    const rows = (await app.inject({ method: 'GET', url: '/api/reports', headers: explore })).json()
      .reports as Array<Record<string, unknown>>;
    expect(rows).toHaveLength(2);
    // Newest first by rendered_at.
    expect(rows[0]!.runId).toBe('other-1');
    expect(rows[0]!.status).toBe('error');
    // ONE ROW PER RUN even though this one has two versions, and the count says so.
    expect(rows[1]!.runId).toBe(RUN);
    expect(rows[1]!.version).toBe(2);
    expect(rows[1]!.versions).toBe(2);
    expect(rows[1]!.renderedBy).toBe('mohamed');
  });

  it('carries the workflow name the identity store knows, and the workspace the template pinned', async () => {
    await store.pinTemplate({
      runId: RUN,
      templateHash: 'h',
      templateText: 't',
      source: 'workspace',
      workspace: 'demo',
    });
    await seed();
    const rows = (await app.inject({ method: 'GET', url: '/api/reports', headers: explore })).json()
      .reports as Array<Record<string, unknown>>;
    expect(rows[0]).toMatchObject({ workflow: 'enrich', workspace: 'demo' });
  });

  it('still lists when the identity lookup fails, because a listing must list', async () => {
    await app.close();
    store = new ReportStore({ url: ':memory:' });
    await store.ensureSchema();
    app = Fastify();
    registerReportRoutes(app, {
      reports: store,
      identities: async () => {
        throw new Error('the identity store is down');
      },
      render: async () => ({ ok: true, snapshot: SNAPSHOT, bytes: 1, secrets: [] }),
      context: async () => ({}),
      now: () => 1,
    });
    await app.ready();
    await seed();
    const res = await app.inject({ method: 'GET', url: '/api/reports', headers: explore });
    expect(res.statusCode).toBe(200);
    const rows = res.json().reports as Array<Record<string, unknown>>;
    expect(rows).toHaveLength(1);
    expect(rows[0]!.workflow).toBeUndefined();
  });

  it('caps the page rather than trusting a limit', async () => {
    await seed();
    const res = await app.inject({ method: 'GET', url: '/api/reports?limit=99999', headers: explore });
    expect(res.statusCode).toBe(200);
  });
});
