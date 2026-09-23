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
import { SECRETS_TOKEN_VARS } from './secrets/routes';
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
const GATES: ReadonlyArray<{
  prefix: string;
  scheme: string;
  vars: readonly string[];
  /** For a credential that is NOT an environment variable — see `/api/secrets/resolve`. */
  describe?: string;
}> = [
  { prefix: '/api/fleet', scheme: 'stateToken', vars: STATE_TOKEN_VARS },
  { prefix: '/api/infra', scheme: 'stateToken', vars: STATE_TOKEN_VARS },
  { prefix: '/api/datasets/query', scheme: 'exploreToken', vars: EXPLORE_TOKEN_VARS },
  { prefix: '/api/explore', scheme: 'exploreToken', vars: EXPLORE_TOKEN_VARS },
  // THE SAME PRIVILEGE AS THE QUERY WORKBENCH, and fail-closed for the same reason: a Run's log
  // lines "routinely contain targets and sometimes secrets". `routes/logs.ts` gates all three of
  // query/hits/tail through one `admit` helper on EXPLORE_TOKEN_VARS, but the prefix was missing
  // here — so the generated spec published them as "Open: no credential is checked" over routes
  // that 401. Same failure as `/api/uploads` below, caught the same way, by
  // `everyOpenPathIsActuallyOpen`.
  { prefix: '/api/logs', scheme: 'exploreToken', vars: EXPLORE_TOKEN_VARS },
  // THE AUDIT TRAIL, AND IT IS THE ONE SURFACE ON THIS TOKEN THAT FAILS CLOSED.
  //
  // The rest of the secrets surface uses `checkOptionalBearer` — "require the token if one is
  // configured" — which is right for a Settings page that must work on a fresh appliance, and is
  // why `/api/secrets` and `/api/slots` are correctly absent from this table: unconfigured, they
  // ARE open. `/api/audit` uses `checkBearer`, so it 401s whether or not a token is set, and a
  // world-readable audit log is not a trade worth making for convenience on first boot.
  { prefix: '/api/audit', scheme: 'secretsToken', vars: SECRETS_TOKEN_VARS },
  { prefix: '/api/workflows/serve', scheme: 'runToken', vars: RUN_TOKEN_VARS },
  { prefix: '/api/workflows/start', scheme: 'runToken', vars: RUN_TOKEN_VARS },
  { prefix: '/api/workflows/stop', scheme: 'runToken', vars: RUN_TOKEN_VARS },
  { prefix: '/api/datasets/runs', scheme: 'runToken', vars: RUN_TOKEN_VARS },
  { prefix: '/api/runs', scheme: 'runToken', vars: RUN_TOKEN_VARS },
  { prefix: '/api/probe', scheme: 'runToken', vars: RUN_TOKEN_VARS },
  { prefix: '/api/sources', scheme: 'runToken', vars: RUN_TOKEN_VARS },
  // WRITES BYTES TO THE OBJECT STORE, so it is fail-closed on the state token like `/api/fleet`
  // and `/api/infra` — not the run surface's opt-in. It was missing here and the generated spec
  // therefore published it as "Open: no credential is checked", which was the exact opposite of
  // what the route does. `everyOpenPathIsActuallyOpen` in the suite is what catches the next one.
  { prefix: '/api/uploads', scheme: 'stateToken', vars: STATE_TOKEN_VARS },
  // NOT A SERVICE TOKEN, and that is why it needs its own scheme. An actor resolves its OWN secret
  // authenticated AS ITSELF: the bearer is a signed identity naming one actor, minted per serve, and
  // there is no `KONTRA_*` variable behind it. This was documented as "Open: no credential is
  // checked" over a route that 401s on a missing header — found by `everyOpenPathIsActuallyOpen`,
  // which is the whole reason that test exists.
  //
  // BOTH RESOLVE ROUTES, and they are the pair the test found. An actor resolves its OWN secret and
  // its OWN slot, authenticated AS ITSELF — a signed identity naming one actor, minted per serve,
  // with no `KONTRA_*` variable behind it. Both were documented as "Open: no credential is checked"
  // over routes that 401 on a missing header.
  {
    prefix: '/api/secrets/resolve',
    scheme: 'actorIdentity',
    vars: [],
    describe: 'Gated by an ACTOR IDENTITY token (KONTRA_ACTOR_TOKEN on the worker) — not a service token, and not a console session.',
  },
  {
    prefix: '/api/slots/resolve',
    scheme: 'actorIdentity',
    vars: [],
    describe: 'Gated by an ACTOR IDENTITY token (KONTRA_ACTOR_TOKEN on the worker) — not a service token, and not a console session.',
  },
];

function gateFor(
  url: string
): { scheme: string; vars: readonly string[]; describe?: string } | undefined {
  // Longest prefix wins, so `/api/datasets/query` is not claimed by a shorter `/api/datasets` —
  // and `/api/secrets/resolve` is not claimed by `/api/secrets`.
  let best: { scheme: string; vars: readonly string[]; describe?: string } | undefined;
  let bestLen = -1;
  for (const g of GATES) {
    if (url.startsWith(g.prefix) && g.prefix.length > bestLen) {
      best = { scheme: g.scheme, vars: g.vars, ...(g.describe ? { describe: g.describe } : {}) };
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
            // A CONSOLE SESSION IS NOT AN ALTERNATIVE TO EVERY GATE. `/api/secrets/resolve` admits
            // an actor identity and nothing else — offering `consoleSession` there would document
            // a way in that does not exist.
            security: gate.describe
              ? [{ [gate.scheme]: [] }]
              : [{ [gate.scheme]: [] }, { consoleSession: [] }],
            description:
              gate.describe ?? `Gated by ${gate.vars.join(' or ')}, or a console session (ADR 0045).`,
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
        secretsToken: {
          type: 'http',
          scheme: 'bearer',
          description:
            'KONTRA_SECRETS_TOKEN, falling back to state. Fail-closed on /api/audit: unset means ' +
            '503, not open.',
        },
        actorIdentity: {
          type: 'http',
          scheme: 'bearer',
          description:
            'A signed token naming ONE actor, minted when it is served (KONTRA_ACTOR_TOKEN). ' +
            'It resolves what that actor owns and nothing more — it is not a service token.',
        },
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
    // `w[0]` is `string | undefined` under noUncheckedIndexedAccess even after `filter(Boolean)`.
    .map((w) => (w[0] ?? '').toUpperCase() + w.slice(1))
    .join('');
}
