/**
 * The spec must describe the server that exists.
 *
 * THE FAILURE THIS PREVENTS IS A 404 NOBODY SEES. A route renamed or moved with a client still
 * calling the old path does not fail loudly — a panel renders empty, or a button does nothing, and
 * the cause is three files away. Making the spec generated and this test the gate turns that into a
 * red build.
 *
 * EVERY ASSERTION BELOW IS PRECEDED BY A COUNT. A walk that finds zero routes would satisfy "every
 * route is in the spec" trivially, which is how a guard reports success while checking nothing —
 * this repo has been bitten by that shape more than once.
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import { buildOpenApi, operationId, toOpenApiPath } from './openapi';
import { buildServer } from './server';

const SPEC = join(__dirname, '..', '..', '..', 'docs', 'openapi.json');

function live() {
  const app = buildServer({});
  const doc = buildOpenApi(app, '0.1.0');
  const routes = app.kontraRoutes.filter((r) => r.url.startsWith('/api/'));
  void app.close();
  return { doc, routes };
}

describe('the route inventory', () => {
  it('finds routes at all', () => {
    const { routes } = live();
    // The guard on the guard. If `onRoute` ever stops firing — a Fastify upgrade, a hook registered
    // too late — every other test here would pass over an empty list.
    expect(routes.length).toBeGreaterThan(30);
  });

  it('does not include HEAD, which Fastify invents for every GET', () => {
    const { routes } = live();
    expect(routes.some((r) => r.method === 'HEAD')).toBe(false);
    // …and it really did see GETs, so the filter is not simply matching nothing.
    expect(routes.some((r) => r.method === 'GET')).toBe(true);
  });
});

describe('the checked-in spec', () => {
  it('is valid JSON and describes this server', () => {
    const onDisk = JSON.parse(readFileSync(SPEC, 'utf8'));
    expect(onDisk.openapi).toMatch(/^3\./);
    expect(Object.keys(onDisk.paths).length).toBeGreaterThan(30);
  });

  it('contains every route the server registers', () => {
    const { routes } = live();
    const onDisk = JSON.parse(readFileSync(SPEC, 'utf8'));

    const missing: string[] = [];
    for (const r of routes) {
      const p = toOpenApiPath(r.url);
      if (!onDisk.paths[p]?.[r.method.toLowerCase()]) missing.push(`${r.method} ${p}`);
    }
    expect(missing, 'run `npx tsx scripts/generate-openapi.ts` — the spec is stale').toEqual([]);
  });

  it('is byte-identical to what the generator produces', () => {
    // STRICTER THAN "CONTAINS", deliberately: a route DELETED from the server but left in the spec
    // is a client that keeps a method for a path that 404s, which the containment check above
    // cannot see.
    const { doc } = live();
    const onDisk = readFileSync(SPEC, 'utf8');
    expect(onDisk).toBe(JSON.stringify(doc, null, 2) + '\n');
  });
});

describe('auth is described, because it is the thing worth reading in one view', () => {
  it('names every scheme the server actually uses', () => {
    const { doc } = live();
    expect(Object.keys(doc.components.securitySchemes).sort()).toEqual([
      'consoleSession',
      'exploreToken',
      'runToken',
      'stateToken',
    ]);
  });

  it('marks the money routes as state-token gated', () => {
    const { doc } = live();
    const fleet = Object.entries(doc.paths).filter(([p]) => p.startsWith('/api/fleet'));
    expect(fleet.length).toBeGreaterThan(0);
    for (const [, ops] of fleet) {
      for (const op of Object.values(ops) as Array<{ security?: Array<Record<string, unknown>> }>) {
        expect(op.security?.some((s) => 'stateToken' in s)).toBe(true);
      }
    }
  });

  it('marks open routes as open rather than omitting the question', () => {
    const { doc } = live();
    const open = Object.values(doc.paths)
      .flatMap((ops) => Object.values(ops) as Array<{ security?: unknown[]; description?: string }>)
      .filter((op) => Array.isArray(op.security) && op.security.length === 0);
    // Most of this API is unauthenticated and `auth.ts` says so; the spec must not imply otherwise
    // by leaving it blank.
    expect(open.length).toBeGreaterThan(0);
    expect(open[0].description).toContain('Open');
  });

  it('lets the longest prefix win', () => {
    // `/api/datasets/query` is explore-gated while `/api/datasets` is open; a naive first-match
    // would hand the workbench the wrong scheme.
    const { doc } = live();
    const q = doc.paths['/api/datasets/query'] as Record<string, { security?: Array<Record<string, unknown>> }>;
    expect(q).toBeDefined();
    expect(Object.values(q)[0].security?.some((s) => 'exploreToken' in s)).toBe(true);
  });
});

describe('operation ids', () => {
  it('are stable, readable function names', () => {
    expect(operationId('GET', '/api/datasets')).toBe('getDatasets');
    expect(operationId('POST', '/api/datasets/query')).toBe('postDatasetsQuery');
    expect(operationId('DELETE', '/api/datasets/runs/{runId}/tags/{tag}')).toBe(
      'deleteDatasetsRunsByRunIdTagsByTag'
    );
  });

  it('are unique across the whole surface', () => {
    const { doc } = live();
    const ids = Object.values(doc.paths).flatMap((ops) =>
      Object.values(ops as Record<string, { operationId: string }>).map((o) => o.operationId)
    );
    expect(ids.length).toBeGreaterThan(30);
    expect(new Set(ids).size, 'two routes share an operationId; a client would lose one').toBe(ids.length);
  });
});
