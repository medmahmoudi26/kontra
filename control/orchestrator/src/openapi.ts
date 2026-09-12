/**
 * The orchestrator's OpenAPI document, GENERATED from the routes that exist.
 *
 * WHY GENERATED AND NOT WRITTEN. A hand-maintained spec is a second source of truth that is correct
 * on the day it is written. This one is built from the `onRoute` inventory `buildServer` collects,
 * so a route cannot be added, renamed or moved without the document following — and `openapi.test.ts`
 * fails when the checked-in file drifts from the live server.
 *
 * WHAT IT DOES AND DOES NOT DESCRIBE, said plainly so nobody reads more into it than is there:
 *
 *   ✓ every path and method the server serves
 *   ✓ which of them require a bearer, and which are open — the first place that is answerable in
 *     ONE view rather than by reading 20 route files
 *   ✗ request and response BODIES. Fastify can derive those, but only from per-route JSON schemas,
 *     and this server has ~100 routes with none. Adding them is worth doing and is a separate piece
 *     of work; claiming shapes here that nothing validates would be worse than omitting them.
 *
 * So this is an inventory with an auth map, not a contract. The console's generated client is typed
 * on the PATHS, which is what makes a moved route a compile error instead of an empty panel.
 */

import type { FastifyInstance } from 'fastify';

import { EXPLORE_TOKEN_VARS, STATE_TOKEN_VARS } from './auth';
import { RUN_TOKEN_VARS } from './workflowControl';

export interface OpenApiDoc {
  openapi: string;
  info: { title: string; version: string; description: string };
  components: { securitySchemes: Record<string, unknown> };
  paths: Record<string, Record<string, unknown>>;
}

/**
 * How a path is gated, by prefix.
 *
 * DERIVED FROM THE ROUTE FILES' OWN CHOICES rather than restated: each entry names the token
 * variable the route actually checks, so this table is reviewable against `auth.ts` in one read.
 * The default is `open`, which is the truth about most of this API and is recorded rather than
 * hidden — `auth.ts` says the same thing in prose.
 */
const GATES: ReadonlyArray<{ prefix: string; scheme: string; vars: readonly string[] }> = [
  { prefix: '/api/fleet', scheme: 'stateToken', vars: STATE_TOKEN_VARS },
  { prefix: '/api/infra', scheme: 'stateToken', vars: STATE_TOKEN_VARS },
  { prefix: '/api/datasets/query', scheme: 'exploreToken', vars: EXPLORE_TOKEN_VARS },
  { prefix: '/api/explore', scheme: 'exploreToken', vars: EXPLORE_TOKEN_VARS },
  { prefix: '/api/workflows/serve', scheme: 'runToken', vars: RUN_TOKEN_VARS },
  { prefix: '/api/workflows/start', scheme: 'runToken', vars: RUN_TOKEN_VARS },
  { prefix: '/api/workflows/stop', scheme: 'runToken', vars: RUN_TOKEN_VARS },
  { prefix: '/api/datasets/runs', scheme: 'runToken', vars: RUN_TOKEN_VARS },
  { prefix: '/api/runs', scheme: 'runToken', vars: RUN_TOKEN_VARS },
  { prefix: '/api/probe', scheme: 'runToken', vars: RUN_TOKEN_VARS },
  { prefix: '/api/sources', scheme: 'runToken', vars: RUN_TOKEN_VARS },
];

function gateFor(url: string): { scheme: string; vars: readonly string[] } | undefined {
  // Longest prefix wins, so `/api/datasets/query` is not claimed by a shorter `/api/datasets`.
  let best: { scheme: string; vars: readonly string[] } | undefined;
  let bestLen = -1;
  for (const g of GATES) {
    if (url.startsWith(g.prefix) && g.prefix.length > bestLen) {
      best = { scheme: g.scheme, vars: g.vars };
      bestLen = g.prefix.length;
    }
  }
  return best;
}

/** Fastify's `:param` becomes OpenAPI's `{param}`. */
export function toOpenApiPath(url: string): string {
  return url.replace(/:([A-Za-z0-9_]+)/g, '{$1}');
}

export function buildOpenApi(app: FastifyInstance, version: string): OpenApiDoc {
  const paths: Record<string, Record<string, unknown>> = {};

  for (const route of app.kontraRoutes) {
    // Static assets and the SPA fallback are not API surface and do not belong in a client.
    if (!route.url.startsWith('/api/')) continue;
    const p = toOpenApiPath(route.url);
    const gate = gateFor(route.url);
    (paths[p] ??= {})[route.method.toLowerCase()] = {
      operationId: operationId(route.method, p),
      responses: { '200': { description: 'ok' } },
      ...(gate
        ? {
            security: [{ [gate.scheme]: [] }, { consoleSession: [] }],
            description: `Gated by ${gate.vars.join(' or ')}, or a console session (ADR 0045).`,
          }
        : {
            security: [],
            description: 'Open: no credential is checked. See auth.ts and THREAT_MODEL.md EP6.',
          }),
    };
  }

  return {
    openapi: '3.1.0',
    info: {
      title: 'kontra orchestrator',
      version,
      description:
        'Generated from the routes this server registers. Paths and auth are authoritative; ' +
        'request and response bodies are deliberately absent until routes carry JSON schemas.',
    },
    components: {
      securitySchemes: {
        consoleSession: { type: 'http', scheme: 'bearer', description: 'A session from POST /api/login.' },
        stateToken: { type: 'http', scheme: 'bearer', description: 'KONTRA_STATE_TOKEN. Can spend money.' },
        runToken: { type: 'http', scheme: 'bearer', description: 'KONTRA_RUN_TOKEN. Open when unset.' },
        exploreToken: { type: 'http', scheme: 'bearer', description: 'KONTRA_EXPLORE_TOKEN, falling back to state.' },
      },
    },
    paths: Object.fromEntries(Object.entries(paths).sort(([a], [b]) => a.localeCompare(b))),
  };
}

/** A stable name a generated client can use as a function name. */
export function operationId(method: string, path: string): string {
  const parts = path
    .replace(/^\/api\//, '')
    .split('/')
    .filter(Boolean)
    .map((seg) =>
      seg.startsWith('{') ? 'By' + cap(seg.slice(1, -1)) : cap(seg.replace(/[^A-Za-z0-9]+/g, ' ').trim())
    );
  return method.toLowerCase() + parts.join('');
}

function cap(s: string): string {
  return s
    .split(/\s+/)
    .filter(Boolean)
    .map((w) => w[0].toUpperCase() + w.slice(1))
    .join('');
}
