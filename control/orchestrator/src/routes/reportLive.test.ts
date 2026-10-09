/**
 * `GET /api/runs/:runId/report/live` at the module (ADR 0062).
 *
 * A bare Fastify plus `registerReportLiveRoute`, the same shape `report.test.ts` uses. The refusal
 * paths go through `inject`; the streaming path goes through a real socket, because the thing most
 * likely to be wrong about an event stream is its framing, and `inject` would never see a frame.
 *
 * Acceptance 7's HTTP half (503 at capacity) and 8's transport half (a hostile value arrives as text)
 * live here. The session semantics are in `report/live.test.ts`.
 */

import Fastify, { type FastifyInstance } from 'fastify';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { MdNode, ReportSnapshot } from '../report/render';
import { MAX_VIEWERS_PER_RUN, type LiveRunKey } from '../report/live';
import { registerReportLiveRoute, type ReportLiveDeps } from './reportLive';

const RUN = 'enrich-1791234567';
const STARTED = 1_791_234_567_000;

function snap(children: MdNode[]): ReportSnapshot {
  return { v: 1, root: { type: 'root', children } as unknown as MdNode, blocks: {} } as ReportSnapshot;
}

function para(value: string): MdNode {
  return { type: 'paragraph', children: [{ type: 'text', value }] } as unknown as MdNode;
}

let app: FastifyInstance;

function build(over: Partial<ReportLiveDeps> = {}): FastifyInstance {
  app = Fastify();
  registerReportLiveRoute(app, {
    admit: () => true,
    resolveRun: async (runId) =>
      runId === RUN ? { runStartedAt: STARTED, status: 'running', closedAt: 0 } : undefined,
    renderOnce: async () => ({ snapshot: snap([para('hello')]) }),
    ...over,
  });
  return app;
}

afterEach(async () => {
  await app?.close();
});

describe('the gate', () => {
  it('refuses when `admit` says no, before resolving anything', async () => {
    const resolveRun = vi.fn();
    build({
      admit: (_req, reply) => {
        reply.code(401).send({ error: 'no' });
        return false;
      },
      resolveRun: resolveRun as unknown as ReportLiveDeps['resolveRun'],
    });
    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report/live` });
    expect(res.statusCode).toBe(401);
    // The gate runs FIRST, so an unauthenticated caller cannot even make us describe a run.
    expect(resolveRun).not.toHaveBeenCalled();
  });
});

describe('a run that cannot be streamed', () => {
  /**
   * THE MEASURED ABUSE `rowTail` RECORDS: 250 streams on 250 FICTIONAL ids became 250 pollers because
   * the id was never validated. Here an unknown id is a 404 and no session is minted.
   */
  it('404s an id that does not exist', async () => {
    build();
    const res = await app.inject({ method: 'GET', url: '/api/runs/not-a-run/report/live' });
    expect(res.statusCode).toBe(404);
    expect(res.json().error).toContain('no run not-a-run');
  });

  /** A finished run's document cannot change, so holding a connection open would be a lie. */
  it('409s a run that has already ended, and names where the answer is', async () => {
    build({
      resolveRun: async () => ({ runStartedAt: STARTED, status: 'completed', closedAt: STARTED + 1 }),
    });
    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report/live` });
    expect(res.statusCode).toBe(409);
    expect(res.json()).toMatchObject({ state: 'closed' });
    expect(res.json().error).toContain(`/api/runs/${RUN}/report`);
  });

  it('502s when the run cannot be resolved at all', async () => {
    build({
      resolveRun: async () => {
        throw new Error('temporal down');
      },
    });
    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report/live` });
    expect(res.statusCode).toBe(502);
    expect(res.json().error).toContain('temporal down');
  });
});

describe('the stream', () => {
  /** A real socket, because `inject` cannot observe SSE framing. */
  async function listen(): Promise<string> {
    await app.listen({ port: 0, host: '127.0.0.1' });
    const addr = app.server.address();
    if (addr === null || typeof addr === 'string') throw new Error('no port');
    return `http://127.0.0.1:${addr.port}`;
  }

  /** Read frames until `want` of them have arrived, then give up the socket. */
  async function frames(url: string, want: number, ms = 4_000): Promise<string[]> {
    const ac = new AbortController();
    const timer = setTimeout(() => ac.abort(), ms);
    const out: string[] = [];
    try {
      const res = await fetch(url, { signal: ac.signal });
      expect(res.headers.get('content-type')).toContain('text/event-stream');
      const reader = res.body!.getReader();
      const dec = new TextDecoder();
      let buf = '';
      while (out.length < want) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let i: number;
        while ((i = buf.indexOf('\n\n')) !== -1) {
          out.push(buf.slice(0, i));
          buf = buf.slice(i + 2);
        }
      }
    } catch {
      // Aborted once we had what we came for, or timed out — the assertions below say which.
    } finally {
      clearTimeout(timer);
      ac.abort();
    }
    return out;
  }

  it('answers 200 BEFORE the first render — the browser must not wait on it', async () => {
    // THE BUG THIS PINS, measured on the live install: 41 SECONDS to first byte on an open run.
    // `writeHead` only sets headers; Node sends them with the first body write, and the first
    // write here is the snapshot — which waits on a render that calls Temporal twice with
    // `historyEventFilterType: CLOSE_EVENT`, each long-polling 20s on a RUNNING workflow. So the
    // browser had not received the 200 at all and the page could only show "No report yet",
    // which is indistinguishable from there being no stream. `flushHeaders()` is the fix.
    //
    // Asserted as TIME TO HEADERS, not as the presence of a frame: a test that waited for the
    // snapshot would pass with the headers still stuck behind it, which is exactly the shape of
    // the bug. The render here is a stub, so a generous bound still fails loudly on a regression.
    build({ renderOnce: async () => { await new Promise((r) => setTimeout(r, 1_500)); return { snapshot: snap([para('hello')]) }; } });
    const base = await listen();
    const ac = new AbortController();
    const began = Date.now();
    try {
      const res = await fetch(`${base}/api/runs/${RUN}/report/live`, { signal: ac.signal });
      const toHeaders = Date.now() - began;
      expect(res.status).toBe(200);
      expect(res.headers.get('content-type')).toContain('text/event-stream');
      expect(toHeaders).toBeLessThan(1_000); // the render takes 1.5s; headers must beat it
    } finally {
      ac.abort();
    }
  });

  it('sends a named `snapshot` event carrying the block list', async () => {
    build();
    const base = await listen();
    const got = await frames(`${base}/api/runs/${RUN}/report/live`, 1);

    expect(got).toHaveLength(1);
    expect(got[0]).toContain('event: snapshot');
    const payload = JSON.parse(got[0]!.split('data: ')[1]!);
    expect(payload.type).toBe('snapshot');
    expect(payload.blocks).toHaveLength(1);
    expect(payload.blocks[0].index).toBe(0);
  });

  /**
   * ACCEPTANCE 8's transport half. A value carrying a fence, a pipe and a script tag must arrive as
   * TEXT in an mdast text node — the escaping is the engine's, and this asserts the stream does not
   * undo it by shipping raw markup.
   */
  it('ships a hostile value as a text node, not as markup', async () => {
    const hostile = '``` | <script>alert(1)</script>';
    build({ renderOnce: async () => ({ snapshot: snap([para(hostile)]) }) });
    const base = await listen();
    const got = await frames(`${base}/api/runs/${RUN}/report/live`, 1);

    const payload = JSON.parse(got[0]!.split('data: ')[1]!);
    const node = payload.blocks[0].node;
    expect(node.type).toBe('paragraph');
    expect(node.children[0].type).toBe('text');
    expect(node.children[0].value).toBe(hostile);
    // No html node anywhere — the script tag is a string, not a tree.
    expect(JSON.stringify(node)).not.toContain('"type":"html"');
  });

  it('emits `final` with the stored version when the run ends', async () => {
    let end!: (v: { version: number }) => void;
    build({
      watchTerminal: () =>
        new Promise((resolve) => {
          end = resolve as (v: { version: number }) => void;
        }),
    });
    const base = await listen();
    const got = frames(`${base}/api/runs/${RUN}/report/live`, 2);
    // Let the snapshot land before the run ends.
    await new Promise((r) => setTimeout(r, 150));
    end({ version: 7 });

    const all = await got;
    expect(all[0]).toContain('event: snapshot');
    expect(all[1]).toContain('event: final');
    expect(JSON.parse(all[1]!.split('data: ')[1]!)).toEqual({ type: 'final', version: 7 });
  });
});

describe('capacity', () => {
  /** ACCEPTANCE 7's HTTP half: 503, not 429 — the service is full, not this caller over a quota. */
  it(`503s the ${MAX_VIEWERS_PER_RUN + 1}th viewer of one run`, async () => {
    const hub = registerReportLiveRoute((app = Fastify()), {
      admit: () => true,
      resolveRun: async () => ({ runStartedAt: STARTED, status: 'running', closedAt: 0 }),
      renderOnce: async () => ({ snapshot: snap([para('x')]) }),
    });
    const key: LiveRunKey = { runId: RUN, runStartedAt: STARTED };
    for (let i = 0; i < MAX_VIEWERS_PER_RUN; i++) {
      expect(hub.open(key, () => undefined).ok).toBe(true);
    }

    const res = await app.inject({ method: 'GET', url: `/api/runs/${RUN}/report/live` });
    expect(res.statusCode).toBe(503);
    expect(res.json()).toMatchObject({ reason: 'per-run' });
    // The existing viewers are untouched.
    expect(hub.get(key)!.viewers.size).toBe(MAX_VIEWERS_PER_RUN);
  });
});
