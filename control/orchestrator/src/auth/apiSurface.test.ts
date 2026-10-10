/**
 * THE AUTH POSTURE OF THE WHOLE `/api` SURFACE, measured against the real server.
 *
 * Admission was a call each route made for itself across 30 files, which means a route that forgets
 * is open and nothing says so. This boots `buildServer()`, enumerates every `/api` route Fastify
 * actually registered, and probes each one with NO credential while all four service tokens are
 * configured — so "ungated" is distinguished from "gated but unconfigured", which is the only
 * distinction that matters and the one a grep cannot make.
 *
 * It then holds `POSTURE` to that measurement in both directions:
 *
 *   a route the server has and the table does not   → FAIL, naming it (a new route is unclassified)
 *   a table entry that is no longer a route         → FAIL, naming it (the table has gone stale)
 *   a `gated` entry that does not actually refuse   → FAIL (the claim is false)
 *   a `public` entry that does not actually admit   → FAIL (sign-in is broken)
 *
 * Which makes the table trustworthy rather than aspirational. `installApiGate` then answers anything
 * undeclared with 403, so a route added without classification is closed in production and red in
 * CI at the same time.
 */

import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

import Fastify from 'fastify';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { buildServer } from '../server';
import { POSTURE, installApiGate, type Posture } from './apiGate';

/** The four service tokens, set so no surface falls back to admitting anonymous callers. */
const TOKEN_VARS = [
  'KONTRA_STATE_TOKEN',
  'KONTRA_EXPLORE_TOKEN',
  'KONTRA_RUN_TOKEN',
  'KONTRA_SECRETS_TOKEN',
] as const;

/** Routes whose probe cannot complete: an SSE stream never ends, so it is classified by reaching it. */
const NEVER_ENDS = new Set(['GET /api/runs/:runId/stream', 'GET /api/workflows/stream']);

interface Probe {
  key: string;
  /** `true` when the request was refused for want of a credential. */
  refused: boolean;
  status: number | string;
}

let probes: Probe[] = [];
let app: Awaited<ReturnType<typeof buildServer>>;

/**
 * Walk `printRoutes` into `METHOD /path` keys.
 *
 * `buildServer` registers its own routes, so an `onRoute` hook cannot be installed ahead of them
 * from out here — the printed tree is the only view of what was registered.
 */
function registeredApiRoutes(tree: string): Array<{ method: string; url: string }> {
  const out: Array<{ method: string; url: string }> = [];
  const stack: string[] = [];
  for (const line of tree.split('\n')) {
    const m = /^([\s│├└─]*)([^\s(]*)\s*(?:\(([^)]*)\))?/.exec(line);
    if (!m) continue;
    const depth = Math.floor((m[1] ?? '').length / 4);
    const seg = m[2] ?? '';
    const methods = m[3];
    if (!seg && !methods) continue;
    stack.length = depth;
    stack[depth] = seg;
    if (!methods) continue;
    const url = stack.slice(0, depth + 1).join('');
    if (!url.startsWith('/api')) continue;
    for (const method of methods.split(',').map((s) => s.trim())) {
      // HEAD is Fastify's free companion to every GET and carries no posture of its own.
      if (method && method !== 'HEAD') out.push({ method, url });
    }
  }
  return out;
}

beforeAll(async () => {
  process.env.KONTRA_ORCHESTRATOR_DB = path.join(mkdtempSync(path.join(tmpdir(), 'surface-')), 'o.db');
  for (const v of TOKEN_VARS) process.env[v] = 'a-token-this-test-never-sends';

  app = buildServer();
  await app.ready();

  for (const r of registeredApiRoutes(app.printRoutes({ commonPrefix: false }))) {
    const key = `${r.method} ${r.url}`;
    if (NEVER_ENDS.has(key)) {
      // Reaching the handler at all is the finding; waiting for the stream to end is not possible.
      probes.push({ key, refused: false, status: 'stream' });
      continue;
    }
    // Every `:param` gets a concrete value so the request reaches the handler rather than 404ing
    // in the router, which would look like a refusal.
    const url = r.url.replace(/:([A-Za-z0-9_]+)/g, 'probe');
    let status: number | string;
    try {
      status = await Promise.race([
        app
          .inject({ method: r.method as 'GET', url, payload: r.method === 'GET' ? undefined : {} })
          .then((res) => res.statusCode),
        new Promise<string>((resolve) => setTimeout(() => resolve('hang'), 4000)),
      ]);
    } catch {
      status = 'throw';
    }
    probes.push({ key, refused: status === 401 || status === 403, status });
  }
}, 540_000);

afterAll(async () => {
  await app?.close();
  for (const v of TOKEN_VARS) delete process.env[v];
});

describe('the table and the server agree', () => {
  it('found a surface to judge', () => {
    // NON-VACUOUS. Every assertion below loops over `probes`, so an empty walk would pass all of
    // them — this repository's most repeated failure shape.
    expect(probes.length).toBeGreaterThan(100);
  });

  it('classifies every route the server registers', () => {
    const undeclared = probes.filter((p) => !POSTURE[p.key]).map((p) => p.key);
    expect(
      undeclared,
      'these routes have no entry in POSTURE (src/auth/apiGate.ts). The gate answers them 403 ' +
        'until one is added — say whether each is public, gated, worker or legacy'
    ).toEqual([]);
  });

  it('has no entry for a route that no longer exists', () => {
    const live = new Set(probes.map((p) => p.key));
    const stale = Object.keys(POSTURE).filter((k) => !live.has(k));
    expect(stale, 'POSTURE entries with no matching route — the table has gone stale').toEqual([]);
  });
});

describe('what the table claims is what the server does', () => {
  it('every `gated` route refuses a request with no credential', () => {
    const lying = probes
      .filter((p) => POSTURE[p.key] === 'gated' && !p.refused)
      .map((p) => `${p.key} -> ${p.status}`);
    expect(
      lying,
      'declared `gated` but admitted an anonymous caller — with all four service tokens set, so ' +
        'this is not a configuration gap'
    ).toEqual([]);
  });

  it('every `public` route admits a request with no credential', () => {
    // The other direction, and it is not symmetry for its own sake: a `public` route that started
    // refusing is a console that cannot sign in, which is an outage rather than a hardening.
    const broken = probes
      .filter((p) => POSTURE[p.key] === 'public' && p.refused)
      .map((p) => `${p.key} -> ${p.status}`);
    expect(broken, 'declared `public` but refused — sign-in or the health probe is broken').toEqual([]);
  });

  it('every `worker` and `legacy` route is in fact still open, so the exposure is not overstated', () => {
    // If one of these starts refusing, the table is claiming an exposure that no longer exists —
    // and an exposure list nobody trusts is an exposure list nobody reads. It also catches the
    // happy case: a route that got gated should be RECLASSIFIED, not left here.
    const closed = probes
      .filter((p) => (POSTURE[p.key] === 'legacy' || POSTURE[p.key] === 'worker') && p.refused)
      .map((p) => `${p.key} -> ${p.status}`);
    expect(
      closed,
      'declared open but actually refuses — if this route is now gated, move it to `gated`'
    ).toEqual([]);
  });
});

describe('the size of the exposure, stated out loud', () => {
  /**
   * A COUNT, SO SHRINKING IT IS A DELIBERATE EDIT. These routes are reachable with no credential
   * because their CLIENTS send none — `cli/api.go:121` says so in as many words, and the Go
   * registrar, `catalog.py`, `secrets.declare` and the progress heartbeat all post with
   * `Content-Type` alone. Closing them is a credential change in four languages, not a routing
   * change here.
   *
   * When that work lands, this number goes down and this test fails until the number is updated.
   * That is the point: the exposure cannot shrink or grow quietly.
   */
  it('is 54 legacy routes and 4 worker routes', () => {
    const count = (p: Posture) => Object.values(POSTURE).filter((v) => v === p).length;
    expect({ legacy: count('legacy'), worker: count('worker') }).toEqual({ legacy: 53, worker: 4 });
  });

  it('and 70 routes that do refuse, which is the half that works', () => {
    const count = (p: Posture) => Object.values(POSTURE).filter((v) => v === p).length;
    // 49 when this gate landed, plus the image surface's 8 and the report surface's 12 (ADR 0055).
    // This number going UP is the direction that needs no justification; it going DOWN is the edit
    // worth noticing.
    expect({ gated: count('gated'), public: count('public') }).toEqual({ gated: 71, public: 4 });
  });
});

describe('deny by default, which is the part that holds for routes nobody has written yet', () => {
  it('answers an undeclared /api route 403, and says what to do about it', async () => {
    const bare = Fastify();
    installApiGate(bare);
    bare.get('/api/a-route-nobody-classified', async () => ({ secret: 'should not be reachable' }));
    await bare.ready();

    const res = await bare.inject({ method: 'GET', url: '/api/a-route-nobody-classified' });
    expect(res.statusCode).toBe(403);
    expect(res.json().error).toContain('has not declared an auth posture');
    expect(res.json().error).toContain('apiGate.ts');
    await bare.close();
  });

  it('403 and not 401, because no credential would help', async () => {
    // A 401 invites the caller to authenticate, which is a lie when the route has not said what it
    // wants — and the console clears its token on one (ADR 0054), so it would sign the operator out
    // over a developer's omission.
    const bare = Fastify();
    installApiGate(bare);
    bare.get('/api/undeclared', async () => ({}));
    await bare.ready();

    const res = await bare.inject({
      method: 'GET',
      url: '/api/undeclared',
      headers: { authorization: 'Bearer anything-at-all' },
    });
    expect(res.statusCode).toBe(403);
    await bare.close();
  });

  it('leaves everything outside /api alone', async () => {
    // The console's own bundle, the static assets and `/healthz` are not this hook's business, and
    // a gate that 403s the SPA is a blank page.
    const bare = Fastify();
    installApiGate(bare);
    bare.get('/', async () => ({ ok: true }));
    bare.get('/assets/app.js', async () => ({ ok: true }));
    await bare.ready();

    expect((await bare.inject({ method: 'GET', url: '/' })).statusCode).toBe(200);
    expect((await bare.inject({ method: 'GET', url: '/assets/app.js' })).statusCode).toBe(200);
    await bare.close();
  });

  it('admits a declared route through the hook', async () => {
    // NON-VACUOUS the other way: if the hook refused everything, the 403 tests above would pass
    // while the gate was simply broken.
    const bare = Fastify();
    installApiGate(bare);
    bare.get('/api/health', async () => ({ ok: true }));
    await bare.ready();

    expect(POSTURE['GET /api/health']).toBe('public');
    expect((await bare.inject({ method: 'GET', url: '/api/health' })).statusCode).toBe(200);
    await bare.close();
  });
});
