/**
 * The SPA fallback, and the redeploy failure it caused.
 *
 * `/runs/<id>` is a surface, not a file, so a cold load must come back as `index.html`. A chunk the
 * build no longer emits is a file, and must 404 — because `index.html` is `no-cache` while the
 * chunks it names are `immutable`, so a browser holding a previous shell asks for a deleted chunk,
 * and answering it with HTML makes the module loader parse HTML as JavaScript. The page dies with
 * no useful error. Observed live: `/assets/DatasetPage-PvDWLHct.js`, deleted by a redeploy minutes
 * earlier, answered `200 text/html`.
 */

import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';

import { Repo } from './db/repo';
import { SPA_SURFACES, buildServer } from './server';

let app: FastifyInstance | undefined;
afterEach(async () => {
  await app?.close();
  app = undefined;
});

function serve(): FastifyInstance {
  const dir = mkdtempSync(join(tmpdir(), 'kontra-spa-'));
  mkdirSync(join(dir, 'assets'), { recursive: true });
  writeFileSync(join(dir, 'index.html'), '<!doctype html><div id="root"></div>');
  writeFileSync(join(dir, 'assets', 'index-live.js'), 'export const ok = 1;\n');
  return buildServer({ repo: new Repo(':memory:'), webRoot: dir });
}

describe('the SPA fallback', () => {
  it('answers a surface address with the shell, so a reload keeps your place', async () => {
    app = serve();
    for (const url of [
      '/',
      '/workflows',
      '/workflows/dnssweep/nscheck-1787150959',
      '/settings',
      '/runs/nscheck-1787150959',
      '/datasets/lame_demo',
      '/monitor',
    ]) {
      const res = await app.inject({ method: 'GET', url });
      expect(res.statusCode, url).toBe(200);
      expect(res.headers['content-type'], url).toContain('text/html');
    }
  });

  it('serves an asset that exists', async () => {
    app = serve();
    const res = await app.inject({ method: 'GET', url: '/assets/index-live.js' });
    expect(res.statusCode).toBe(200);
    expect(res.body).toContain('export const ok');
  });

  it('404s a chunk a redeploy deleted, instead of returning HTML a module loader will choke on', async () => {
    app = serve();
    const res = await app.inject({ method: 'GET', url: '/assets/DatasetPage-PvDWLHct.js' });
    expect(res.statusCode).toBe(404);
    expect(res.headers['content-type']).not.toContain('text/html');
  });

  it('404s any missing file, not just one under /assets/', async () => {
    app = serve();
    for (const url of ['/favicon.ico', '/index-old.css', '/assets/gone.js.map']) {
      const res = await app.inject({ method: 'GET', url });
      expect(res.statusCode, url).toBe(404);
    }
  });

  it('still 404s an unknown API route as JSON', async () => {
    app = serve();
    const res = await app.inject({ method: 'GET', url: '/api/nope' });
    expect(res.statusCode).toBe(404);
    expect(res.json()).toEqual({ error: 'not found' });
  });
});

/**
 * THE IDS THIS SUITE USED TO DUCK.
 *
 * Every address above is dot-free (`nscheck-1787150959`, `lame_demo`), and that is the only reason
 * the extension heuristic looked correct: `encodeURIComponent` does not escape `.`, so the moment an
 * id carries one the last segment matches `/\.[A-Za-z0-9]+$/` and the surface 404s. Each id below is
 * taken from what the system actually produces, not invented to fail.
 */
describe('a surface address whose id contains a dot', () => {
  const DOTTED = [
    // `address.ts`'s own comment: "A Terminal id carries a colon (`kontra-recon:0.1`) and, on tmux,
    // a dot." Encoded, the colon becomes %3A and the dot stays a dot.
    `/monitor/${encodeURIComponent('local:localhost/nscheck-0.1.0/actor')}`,
    `/monitor/${encodeURIComponent('kontra-recon:0.1')}`,
    // `safeName` (data/parquet.ts) permits `.`: name.replace(/[^A-Za-z0-9_.-]/g, '_')
    '/datasets/acme.com',
    `/datasets/${encodeURIComponent('wf-nscheck-0.1.0--2026-08-19T14-32-07Z--2faa3d')}`,
    // a workflow started with `--id sweep-v1.2`, at both its retired address and its new one
    '/runs/sweep-v1.2',
    '/workflows/nscheck-0.1.0/sweep-v1.2',
  ];

  it('comes back as the shell, not as a 404', async () => {
    app = serve();
    for (const url of DOTTED) {
      const res = await app.inject({ method: 'GET', url });
      expect(res.statusCode, url).toBe(200);
      expect(res.headers['content-type'], url).toContain('text/html');
    }
  });

  it('does not resurrect the deleted-chunk bug for a dotted path OUTSIDE a surface', async () => {
    app = serve();
    // The fix keys on the FIRST segment, so nothing under a non-surface prefix changed.
    for (const url of ['/assets/DatasetPage-PvDWLHct.js', '/vendor/thing-0.1.js', '/favicon.ico']) {
      const res = await app.inject({ method: 'GET', url });
      expect(res.statusCode, url).toBe(404);
      expect(res.headers['content-type'], url).not.toContain('text/html');
    }
  });

  it('serves every surface the frontend can address, so the copied list cannot rot quietly', async () => {
    app = serve();
    // Mirrors `frontend/src/state/surfaces.ts`'s SPA_SEGMENTS, which is the authority. A
    // surface added there and not here 404s on cold load, which is the failure this pins.
    //
    // `runs` and `scratch` are RETIRED surfaces and are in the list on purpose: the frontend
    // redirects them, and a redirect is code that has to load first. Serving them is what lets
    // `/runs/sweep-v1.2` — a dot in the id — reach the shell instead of the extension heuristic.
    for (const surface of ['workflows', 'actors', 'datasets', 'monitor', 'settings', 'runs', 'scratch']) {
      expect(SPA_SURFACES.has(surface), surface).toBe(true);
      const res = await app.inject({ method: 'GET', url: `/${surface}/an.id.with.dots` });
      expect(res.statusCode, surface).toBe(200);
    }
    expect(SPA_SURFACES.size).toBe(7);
  });
});
