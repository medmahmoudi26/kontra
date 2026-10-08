/**
 * The live row-tail SSE endpoint (live-datasets slice 05): `GET /api/datasets/rows/stream?run=<id>`
 * streams the durable count of a Run's `units/run=<id>/` path over its OWN endpoint — not
 * `/api/events`. This spins a real listening server (an SSE stream cannot be `inject`ed — inject
 * waits for a response that never ends) and reads the first frames off the socket.
 */

import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';

import { MemoryStore, ObjectStore } from './codec/objectStore';
import { Repo } from './db/repo';
import { runPrefix } from './codec/shard';
import { buildServer, type ServerOptions } from './server';

interface Ctx {
  app: FastifyInstance;
  store: ObjectStore;
  port: number;
}

/** Write one pushed record to the durable path, the way an actor host's unitstore would. */
async function pushRow(store: ObjectStore, runId: string, unit: number): Promise<void> {
  const key = `${runPrefix(runId)}dt=2026-08-19/actor=probe/shard=0001/unit=${String(unit).padStart(5, '0')}/${'a'.repeat(64)}.json`;
  await store.put(key, Buffer.from(JSON.stringify([{ i: unit }]), 'utf8'));
}

/** Read SSE frames off a live response until `want(frames)` is satisfied or the budget runs out.
 *  Returns the accumulated raw text. Aborts the request on the way out. */
async function readFrames(
  url: string,
  want: (text: string) => boolean,
  opts: { headers?: Record<string, string>; timeoutMs?: number } = {}
): Promise<string> {
  const ctrl = new AbortController();
  const res = await fetch(url, { headers: { accept: 'text/event-stream', ...(opts.headers ?? {}) }, signal: ctrl.signal });
  expect(res.headers.get('content-type')).toContain('text/event-stream');
  const reader = res.body!.getReader();
  const decoder = new TextDecoder();
  let text = '';
  const deadline = Date.now() + (opts.timeoutMs ?? 4000);
  try {
    while (Date.now() < deadline) {
      const { value, done } = await reader.read();
      if (done) break;
      text += decoder.decode(value, { stream: true });
      if (want(text)) return text;
    }
    return text;
  } finally {
    ctrl.abort();
    await reader.cancel().catch(() => {});
  }
}

/** The parsed `data:` payloads of every snapshot frame seen so far. */
function snapshots(text: string): Array<{ rows: number; lastChunkAt: number | null }> {
  const out: Array<{ rows: number; lastChunkAt: number | null }> = [];
  for (const line of text.split('\n')) {
    if (!line.startsWith('data:')) continue;
    const body = line.slice('data:'.length).trim();
    try {
      const parsed = JSON.parse(body);
      if (typeof parsed.rows === 'number') out.push(parsed);
    } catch {
      /* a control payload without rows — ignore */
    }
  }
  return out;
}

async function build(caps?: ServerOptions['rowStreamCaps']): Promise<Ctx> {
  const dir = mkdtempSync(join(tmpdir(), 'kontra-rowstream-'));
  const store = new ObjectStore({ backing: new MemoryStore(), prefix: '', endpoint: 'http://localhost:8333' });
  const app = buildServer({
    repo: new Repo(':memory:'),
    store,
    webRoot: '',
    lake: { catalog: join(dir, 'cat.ducklake') },
    ...(caps ? { rowStreamCaps: caps } : {}),
  });
  const address = await app.listen({ port: 0, host: '127.0.0.1' });
  const port = Number(new URL(address).port);
  return { app, store, port };
}

describe('GET /api/datasets/rows/stream', () => {
  let ctx: Ctx;
  beforeEach(async () => {
    ctx = await build();
  });
  afterEach(async () => {
    await ctx.app.close();
  });

  it('streams the durable row count of a run while it is being written', async () => {
    await pushRow(ctx.store, 'DnsSweep-live', 1);
    await pushRow(ctx.store, 'DnsSweep-live', 2);
    await pushRow(ctx.store, 'DnsSweep-live', 3);

    const text = await readFrames(
      `http://127.0.0.1:${ctx.port}/api/datasets/rows/stream?run=DnsSweep-live`,
      (t) => snapshots(t).some((s) => s.rows === 3)
    );
    // The first count off the wire is the object count of the durable path — three pushed records.
    expect(snapshots(text).some((s) => s.rows === 3)).toBe(true);
    // It carries a seq as the SSE id, so a reconnect can resume.
    expect(text).toMatch(/\nid: \d+\n/);
  });

  it('requires a run id', async () => {
    const res = await fetch(`http://127.0.0.1:${ctx.port}/api/datasets/rows/stream`);
    expect(res.statusCode ?? res.status).toBe(400);
    await res.body?.cancel().catch(() => {});
  });

  it('two connections on one run see the same count (one server-side poller, fanned out)', async () => {
    await pushRow(ctx.store, 'r2', 1);
    await pushRow(ctx.store, 'r2', 2);
    const url = `http://127.0.0.1:${ctx.port}/api/datasets/rows/stream?run=r2`;
    const [a, b] = await Promise.all([
      readFrames(url, (t) => snapshots(t).some((s) => s.rows === 2)),
      readFrames(url, (t) => snapshots(t).some((s) => s.rows === 2)),
    ]);
    const lastA = snapshots(a).at(-1)!;
    const lastB = snapshots(b).at(-1)!;
    expect(lastA.rows).toBe(2);
    expect(lastB.rows).toBe(lastA.rows);
  });
});

/* ───────────────────────────── the bounds ───────────────────────────── */

/** Hold an SSE stream open until the test hangs up, and answer what the server said on the way in. */
async function open(url: string): Promise<{ status: number; close: () => void; body: Promise<void> }> {
  const ctrl = new AbortController();
  const res = await fetch(url, { headers: { accept: 'text/event-stream' }, signal: ctrl.signal });
  const drain = res.body
    ? (async () => {
        const reader = res.body!.getReader();
        try {
          for (;;) {
            const { done } = await reader.read();
            if (done) return;
          }
        } catch {
          /* aborted, which is how this always ends */
        }
      })()
    : Promise.resolve();
  return { status: res.status, close: () => ctrl.abort(), body: drain };
}

/** Count every LIST the hub makes, through the same instance method the server closed over. */
function countLists(store: ObjectStore): { calls: () => number; prefixes: string[] } {
  const prefixes: string[] = [];
  const original = store.list.bind(store);
  store.list = async (prefix: string) => {
    prefixes.push(prefix);
    return original(prefix);
  };
  return { calls: () => prefixes.length, prefixes };
}

const wait = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

/**
 * The three caps and the teardown, on the real route.
 *
 * THE REPRODUCTION THIS ANSWERS (`.scratch/post-merge-review/TRIAGE-2026-08-25.md` §7): 250
 * concurrent streams from ONE client, no credential, on 250 fictional run ids — 250 pollers and 750
 * object-store LISTs in five seconds, because `subscribe` minted a `RunState` and an interval for any
 * string it had not seen. The teardown half was already correct and is pinned here anyway: it is the
 * thing every other bound rests on, and an untested correct behaviour is one refactor from being an
 * untested wrong one.
 */
describe('GET /api/datasets/rows/stream is bounded', () => {
  it('refuses a caller past the per-client cap, and gives the budget back when they hang up', async () => {
    const ctx = await build({ maxPerClient: 2 });
    const held: Array<{ close: () => void; body: Promise<void> }> = [];
    try {
      const url = (run: string): string => `http://127.0.0.1:${ctx.port}/api/datasets/rows/stream?run=${run}`;
      const a = await open(url('made-up-0'));
      const b = await open(url('made-up-1'));
      held.push(a, b);
      expect(a.status).toBe(200);
      expect(b.status).toBe(200);

      // The third is the 250-connection flood in miniature: a DIFFERENT run id every time, so no
      // per-run bound would ever see it. This one is about the CALLER.
      const refused = await fetch(url('made-up-2'));
      expect(refused.status).toBe(429);
      expect(await refused.json()).toMatchObject({ error: expect.stringContaining('cap 2') });

      a.close();
      await a.body;
      // A counter that only ever went up would turn into a permanent refusal for the one browser
      // that happens to have opened and closed a few Datasets.
      for (let i = 0; i < 40; i += 1) {
        const retry = await fetch(url('made-up-2'), { headers: { accept: 'text/event-stream' } });
        if (retry.status === 200) {
          await retry.body?.cancel().catch(() => {});
          return;
        }
        await retry.body?.cancel().catch(() => {});
        await wait(25);
      }
      throw new Error('the per-client budget was never given back');
    } finally {
      for (const h of held) h.close();
      await ctx.app.close();
    }
  });

  it('refuses a run past the install-wide cap, with a different status and a different sentence', async () => {
    const ctx = await build({ maxRuns: 1 });
    const held: Array<{ close: () => void; body: Promise<void> }> = [];
    try {
      const first = await open(`http://127.0.0.1:${ctx.port}/api/datasets/rows/stream?run=only-one`);
      held.push(first);
      expect(first.status).toBe(200);

      const refused = await fetch(`http://127.0.0.1:${ctx.port}/api/datasets/rows/stream?run=made-up-1`);
      // 503, not 429: "you have too many" and "this install is full" are different problems with
      // different fixes, and an operator reading a log needs to be able to tell them apart.
      expect(refused.status).toBe(503);
      expect(await refused.json()).toMatchObject({ error: expect.stringContaining('cap 1') });
    } finally {
      for (const h of held) h.close();
      await ctx.app.close();
    }
  });

  it('refuses a reader past the per-run cap — the hole the other two leave', async () => {
    // Both other caps are satisfied by a flood that puts every connection on ONE id from one client:
    // one poller, one RunState, and an unbounded set of sinks the poller walks on every tick.
    const ctx = await build({ maxSinks: 1, maxPerClient: 8, maxRuns: 8 });
    const held: Array<{ close: () => void; body: Promise<void> }> = [];
    try {
      const first = await open(`http://127.0.0.1:${ctx.port}/api/datasets/rows/stream?run=popular`);
      held.push(first);
      expect(first.status).toBe(200);
      const refused = await fetch(`http://127.0.0.1:${ctx.port}/api/datasets/rows/stream?run=popular`);
      expect(refused.status).toBe(503);
      expect(await refused.json()).toMatchObject({ error: expect.stringContaining('readers') });
    } finally {
      for (const h of held) h.close();
      await ctx.app.close();
    }
  });

  it('a closed reader stops its stream — the LISTs stop when the socket does', async () => {
    const ctx = await build();
    try {
      const counted = countLists(ctx.store);
      const held = await open(`http://127.0.0.1:${ctx.port}/api/datasets/rows/stream?run=watched`);
      expect(held.status).toBe(200);

      // It really is polling: the first LIST is immediate, and `ROW_TAIL_POLL_MS` brings more.
      await wait(300);
      const whileOpen = counted.calls();
      expect(whileOpen).toBeGreaterThan(0);
      expect(counted.prefixes.every((p) => p.includes('run=watched'))).toBe(true);

      held.close();
      await held.body;
      // One poll interval is 2s; three seconds with no new LIST is the poller being gone rather than
      // merely slow. A run nobody is watching costs this install nothing.
      await wait(400);
      const settled = counted.calls();
      await wait(3_000);
      expect(counted.calls()).toBe(settled);
    } finally {
      await ctx.app.close();
    }
  }, 15_000);
});
