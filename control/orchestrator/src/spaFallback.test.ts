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
import {
  SPA_SURFACES,
  SVELTE_ROUTES,
  SVELTE_SURFACES,
  assertBundlesAreDisjoint,
  buildServer,
} from './server';

let app: FastifyInstance | undefined;
afterEach(async () => {
  await app?.close();
  app = undefined;
});

function serve(): FastifyInstance {
  const dir = mkdtempSync(join(tmpdir(), 'kontra-spa-'));
  mkdirSync(join(dir, 'assets'), { recursive: true });
  writeFileSync(join(dir, 'index.html'), '<!doctype html><div id="root"></div>');
  // THE SECOND DOCUMENT. Both bundles build into one directory (ADR 0048 §1); a fixture with only
  // `index.html` would 404 every Svelte route and read as a routing bug rather than a missing file.
  // The marker is what the assertions below tell the two documents apart by.
  writeFileSync(join(dir, 'svelte.html'), '<!doctype html><div id="app" data-bundle="svelte"></div>');
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
    const surfaces = [
      'catalog',
      'workflows',
      'actors',
      'datasets',
      'monitor',
      'secrets',
      'settings',
      'runs',
      'scratch',
    ];
    for (const surface of surfaces) {
      // EITHER BUNDLE. Surfaces migrate one at a time (ADR 0048), so which set owns one changes
      // over the life of this migration — what must never change is that SOMETHING serves it. This
      // asserted `SPA_SURFACES.has` and went red the moment `catalog` moved, which is the test
      // reporting a successful migration step as a regression.
      expect(
        SPA_SURFACES.has(surface) || SVELTE_SURFACES.has(surface),
        `${surface} is owned by neither bundle`
      ).toBe(true);
      const res = await app.inject({ method: 'GET', url: `/${surface}/an.id.with.dots` });
      expect(res.statusCode, surface).toBe(200);
    }
    // THE SIZE IS THE HALF THAT CATCHES AN OMISSION. Every loop above passes on a list that is
    // missing a surface — it only ever asserts what it was told to look for — so the count is what
    // makes a surface added on the console side and forgotten here fail on this side too. It has
    // already been wrong once: `secrets` shipped in the console and never reached this set.
    // COUNTED ACROSS BOTH BUNDLES, for the same reason the loop now checks both: this asserted
    // `SPA_SURFACES.size` and the count fell to 8 the moment `catalog` migrated — a green suite
    // turning red to report that the migration worked. What must hold is that the TOTAL is
    // unchanged: a surface may move between bundles, and may not vanish from both.
    expect(SPA_SURFACES.size + SVELTE_SURFACES.size).toBe(surfaces.length);
    expect(SPA_SURFACES.size + SVELTE_SURFACES.size).toBe(9);
  });
});

/**
 * TWO BUNDLES, ONE ORIGIN (ADR 0048 §1).
 *
 * The console is migrating a Surface at a time and the seam is a pair of allowlists. Everything the
 * single-bundle fallback could get wrong is now available twice, and one thing is new: a segment in
 * BOTH sets serves whichever branch the handler tests first, which is a coin-flip decided by the
 * order of two `if`s.
 */
describe('the two-bundle split', () => {
  it('refuses to boot when a surface claims both bundles', () => {
    // The guard is proven by BREAKING it, not by observing that it passes on today's sets — which
    // it would do just as happily if the function body were `return`.
    expect(() =>
      assertBundlesAreDisjoint(new Set(['catalog', 'actors']), new Set(['actors']))
    ).toThrow(/claimed by both bundles.*actors/s);

    // …and the real sets are disjoint, which is the fact the guard exists to keep true.
    expect(() => assertBundlesAreDisjoint()).not.toThrow();
  });

  it('serves the svelte document for a svelte surface, and the react one for a react surface', async () => {
    app = serve();

    for (const surface of SVELTE_SURFACES) {
      const res = await app.inject({ method: 'GET', url: `/${surface}/an.id.with.dots` });
      expect(res.statusCode, surface).toBe(200);
      expect(res.body, surface).toContain('data-bundle="svelte"');
    }
    for (const route of SVELTE_ROUTES) {
      const res = await app.inject({ method: 'GET', url: `/${route}` });
      expect(res.statusCode, route).toBe(200);
      expect(res.body, route).toContain('data-bundle="svelte"');
    }
    // A React surface must NOT have started answering with the other document. This is the
    // assertion that fails if a segment is moved to the Svelte set and not removed from this one.
    // Taken FROM the set rather than named, so it survives the next surface migrating.
    const stillReact = [...SPA_SURFACES][0]!;
    const react = await app.inject({ method: 'GET', url: `/${stillReact}/an.id.with.dots` });
    expect(react.statusCode).toBe(200);
    expect(react.body).not.toContain('data-bundle="svelte"');
  });

  it('counts both sets, so a surface added to neither cannot pass unnoticed', () => {
    // THE HALF THAT CATCHES AN OMISSION, restated for two sets: the loops above iterate whatever
    // they are given, so an empty Svelte set makes them vacuous. The console declares seven
    // surfaces; every one must be owned by exactly one bundle.
    const owned = new Set([...SPA_SURFACES, ...SVELTE_SURFACES]);
    for (const surface of ['catalog', 'workflows', 'actors', 'datasets', 'monitor', 'secrets', 'settings']) {
      expect(owned.has(surface), `${surface} is owned by neither bundle`).toBe(true);
    }
    expect(owned.size).toBe(SPA_SURFACES.size + SVELTE_SURFACES.size);
  });
});
