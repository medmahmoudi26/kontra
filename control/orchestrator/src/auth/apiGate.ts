/**
 * DENY BY DEFAULT ON `/api/*` — the hook, and the table that is the whole API's auth posture.
 *
 * ── WHY A TABLE AND NOT A FLAG ON EACH ROUTE ────────────────────────────────────────────────────
 *
 * Admission used to be a call each route made for itself, which means a route that forgets is OPEN
 * and nothing says so. There are 115 of them across 30 files. The posture of the surface as a whole
 * was not written down anywhere and could not be read off any one file.
 *
 * So it is written down HERE, once, and `apiSurface.test.ts` enforces it in both directions: every
 * route the server registers must appear below, and every entry below must still be a route. A new
 * route is therefore a FAILING TEST until somebody classifies it, and until then this hook answers
 * it 403 — which is the deny-by-default part, and the part that holds for code nobody has written
 * yet.
 *
 * ── WHAT THIS HOOK DOES AND DOES NOT DO ─────────────────────────────────────────────────────────
 *
 * It refuses UNDECLARED routes. It does not re-check the declared ones: `gated` routes call
 * `checkBearer` themselves, against the token vars their own surface uses, and four different
 * service tokens are in play (`state`, `explore`, `run`, `secrets`). A second generic check here
 * would either duplicate that correctly or weaken it, and the test already proves each `gated`
 * route really does refuse an anonymous caller.
 *
 * ── THE 54 `legacy` ENTRIES ARE AN EXPOSURE, AND THEY ARE DELIBERATELY STILL OPEN ───────────────
 *
 * Measured, not assumed: `apiSurface.test.ts` boots the real server with all four service tokens
 * configured and probes every route with no credential. 49 refuse. 58 reach the handler, and that
 * includes `DELETE /api/actors/:key`, `DELETE /api/datasets/:name`, `GET /api/datasets/:name/preview`
 * and `GET /api/datasets/rows/stream`.
 *
 * They cannot be closed from this file alone, because THEIR CLIENTS SEND NO CREDENTIAL:
 *
 *   cli/api.go:121    "the rest of this API predates admission control and is unauthenticated"
 *                     — `newAPI()` sets no bearer; only `newAuthAPI()` does.
 *   runtime/go/registrar/registrar.go:146   POST /api/actors, Content-Type only
 *   runtime/python/internals/catalog.py:348 POST /api/workflows/catalog, Content-Type only
 *   sdk/python/kontra/secrets.py:252        POST /api/slots/declare, Content-Type only
 *   runtime/python/internals/engine.py:207  POST /api/runs/:id/progress, Content-Type only
 *
 * Gating them before those clients carry a token would break the CLI and, worse, break worker
 * registration silently — `catalog.py` is best-effort by design, so an actor would serve traffic
 * while its workflows never appeared and its slots were never bindable. That is a credential
 * question, not a routing one, and it belongs with the scoped-credential work.
 *
 * The four `worker` entries are called THE SAME WAY and are named apart from `legacy` only because
 * they are the ones a fleet Machine depends on, so they are the ones whose closure has to be
 * sequenced with the worker's credential rather than the CLI's.
 */

import type { FastifyInstance, FastifyReply, FastifyRequest } from 'fastify';

/** What a route's admission is. */
export type Posture =
  /** No credential needed, by design: the sign-in surface and the health probe. */
  | 'public'
  /** The route refuses an anonymous caller itself. Verified per route by the surface test. */
  | 'gated'
  /** Reachable with no credential because its CLIENT sends none. An exposure, not a decision. */
  | 'worker'
  /** Reachable with no credential: the pre-admission-control surface the CLI calls. */
  | 'legacy';

/**
 * `METHOD /path` for every `/api` route the server registers, with the path spelled exactly as
 * Fastify has it — `:param` and all, because that is the key the hook can look up at request time.
 */
export const POSTURE: Readonly<Record<string, Posture>> = {
  // ── public ───────────────────────────────────────────────────────────────────────────────
  'GET /api/health': 'public',
  'GET /api/login': 'public',
  'POST /api/login': 'public',
  'POST /api/logout': 'public',

  // ── worker ───────────────────────────────────────────────────────────────────────────────
  'POST /api/actors': 'worker',
  'POST /api/runs/:runId/progress': 'worker',
  'POST /api/slots/declare': 'worker',
  'POST /api/workflows/catalog': 'worker',

  // ── legacy ───────────────────────────────────────────────────────────────────────────────
  'DELETE /api/actors/:key': 'legacy',
  'DELETE /api/datasets/:name': 'legacy',
  'DELETE /api/graphs/:id': 'legacy',
  'DELETE /api/scratch/:id': 'legacy',
  'DELETE /api/sources/:kind/:id': 'legacy',
  'GET /api/actors': 'legacy',
  'GET /api/datasets': 'legacy',
  'GET /api/datasets/:name/preview': 'legacy',
  'GET /api/datasets/:name/provenance': 'legacy',
  'GET /api/datasets/retention/preview': 'legacy',
  'GET /api/datasets/rows/stream': 'legacy',
  'GET /api/datasets/runs': 'legacy',
  'GET /api/fleet/operations': 'legacy',
  'GET /api/graphs': 'legacy',
  'GET /api/graphs/:id': 'legacy',
  'GET /api/images': 'legacy',
  'GET /api/pollers': 'legacy',
  'GET /api/probes/:runId': 'legacy',
  'GET /api/pulse': 'legacy',
  'GET /api/queues/:queue/pollers': 'legacy',
  'GET /api/runs': 'legacy',
  'GET /api/runs/:runId': 'legacy',
  'GET /api/runs/:runId/asks': 'legacy',
  'GET /api/runs/:runId/datasets': 'legacy',
  'GET /api/runs/:runId/heartbeats': 'legacy',
  'GET /api/runs/:runId/history': 'legacy',
  'GET /api/runs/:runId/io': 'legacy',
  'GET /api/runs/:runId/phase': 'legacy',
  'GET /api/runs/:runId/progress': 'legacy',
  /* THE REPORT SURFACE (ADR 0055) — every entry `gated`, and each one really calls `checkBearer`:
     `EXPLORE_TOKEN_VARS` for the report and the thread, because that list's own header says the surface
     it guards "routinely contains targets and sometimes secrets" and a report is a rendering of exactly
     that; `STATE_TOKEN_VARS` plus the `report:reveal` scope for the unredacted bytes. A console session
     is admitted through the `console` scope, which is what lets the report page and the feedback
     composer work in a browser. */
  'GET /api/reports': 'gated',
  'GET /api/runs/:runId/report': 'gated',
  // ADR 0062. The SAME posture as the stored report, because it is the same document on a shorter
  // clock — and it must be declared here or ADR 0058's "undeclared is closed" answers it 403, which
  // is the gate working rather than a bug to route around.
  'GET /api/runs/:runId/report/live': 'gated',
  'GET /api/runs/:runId/report/versions': 'gated',
  'POST /api/runs/:runId/report/render': 'gated',
  'POST /api/runs/:runId/report/preview': 'gated',
  'GET /api/runs/:runId/report/export': 'gated',
  'GET /api/runs/:runId/report/blocks/:blockId/raw': 'gated',
  'POST /api/runs/:runId/report/blocks/:blockId/reveal': 'gated',
  'GET /api/runs/:runId/feedback': 'gated',
  'POST /api/runs/:runId/feedback': 'gated',
  'PATCH /api/feedback/:id': 'gated',
  'DELETE /api/feedback/:id': 'gated',
  'GET /api/runs/:runId/stream': 'legacy',
  'GET /api/scratch': 'legacy',
  'GET /api/scratch/:id': 'legacy',
  'GET /api/scratch/:id/spec': 'legacy',
  'GET /api/sources/:kind': 'legacy',
  'GET /api/sources/:kind/:id/file': 'legacy',
  'GET /api/sources/:kind/:id/files': 'legacy',
  'GET /api/sources/:kind/:id/serves': 'legacy',
  'GET /api/sources/:kind/:id/watch': 'legacy',
  'GET /api/sources/actor/:id/schema': 'legacy',
  'GET /api/sources/actor/:id/schema/stream': 'legacy',
  'GET /api/stuck': 'legacy',
  'GET /api/summaries/health': 'legacy',
  'GET /api/summaries/hourly': 'legacy',
  'GET /api/summaries/runs/:runId': 'legacy',
  'GET /api/workflows': 'legacy',
  'GET /api/workflows/exposure': 'legacy',
  'GET /api/workflows/file/:name': 'legacy',
  'GET /api/workflows/stream': 'legacy',
  'GET /api/workspaces': 'legacy',
  'POST /api/actors/:key/digest': 'legacy',
  'POST /api/graphs': 'legacy',
  'POST /api/scratch': 'legacy',
  'POST /api/sources/:kind': 'legacy',
  'POST /api/sources/actor/:id/caller': 'legacy',

  // ── gated ───────────────────────────────────────────────────────────────────────────────
  'DELETE /api/datasets/runs/:runId/name': 'gated',
  'DELETE /api/datasets/runs/:runId/tags/:tag': 'gated',
  'DELETE /api/secrets/:name': 'gated',
  'DELETE /api/slots/actor/:actor/:slot': 'gated',
  'GET /api/audit': 'gated',
  'GET /api/datasets/export': 'gated',
  'GET /api/datasets/schema': 'gated',
  'GET /api/infra/ops/:fqn': 'gated',
  'GET /api/infra/stacks': 'gated',
  'GET /api/infra/stacks/:fqn/history': 'gated',
  'GET /api/infra/stacks/:fqn/leases': 'gated',
  'GET /api/infra/stacks/:fqn/state': 'gated',
  'GET /api/logs/coverage': 'gated',
  'GET /api/logs/hits': 'gated',
  'GET /api/logs/query': 'gated',
  'GET /api/logs/tail': 'gated',
  'GET /api/runs/:runId/explore': 'gated',
  'GET /api/runs/:runId/progress-stream': 'gated',
  'GET /api/secrets': 'gated',
  'GET /api/secrets/:name': 'gated',
  'GET /api/slots': 'gated',
  'GET /api/slots/actor/:actor': 'gated',
  'GET /api/slots/audit': 'gated',
  'GET /api/state/:tier': 'gated',
  'POST /api/datasets/query': 'gated',
  'POST /api/datasets/query/stream': 'gated',
  'POST /api/datasets/runs/:runId/tags': 'gated',
  'POST /api/infra/stacks/:fqn/:op': 'gated',
  'POST /api/runs': 'gated',
  'POST /api/runs/:runId/asks/:askId': 'gated',
  'POST /api/runs/:runId/stop': 'gated',
  'POST /api/secrets/:name/versions/:version/revoke': 'gated',
  'POST /api/secrets/identity': 'gated',
  'POST /api/secrets/resolve': 'gated',
  'POST /api/slots/preflight': 'gated',
  'POST /api/slots/resolve': 'gated',
  'POST /api/sources/actor/:id/build': 'gated',
  'POST /api/sources/actor/:id/probe': 'gated',
  'POST /api/sources/actor/:id/serve': 'gated',
  'POST /api/uploads': 'gated',
  'POST /api/workflows/file/:name/pause': 'gated',
  'POST /api/workflows/file/:name/resume': 'gated',
  'POST /api/workflows/serve': 'gated',
  'POST /api/workspaces': 'gated',
  'PUT /api/datasets/runs/:runId/name': 'gated',
  'PUT /api/runs/:runId/workflow': 'gated',
  // The image store (ADR 0061). All eight REFUSE without a credential; the three that would change
  // something also require `infra`, because ADR 0054 already decided that a privileged capability is a
  // service-token one a session cannot inherit — and inventing a second vocabulary for the same idea is
  // worse than reusing the one that was argued.
  'DELETE /api/images/actors/:name/:version': 'gated',
  'GET /api/images/actors': 'gated',
  'GET /api/images/actors/:name/:version': 'gated',
  'GET /api/images/layers': 'gated',
  'GET /api/images/runtimes': 'gated',
  'GET /api/images/storage': 'gated',
  'POST /api/images/gc': 'gated',
  'POST /api/images/rebase': 'gated',
  'PUT /api/secrets/:name': 'gated',
  'PUT /api/slots/actor/:actor/:slot': 'gated',
  'PUT /api/workspaces/current': 'gated',
};

/** The postures a request is allowed through on. */
const ALLOWED: ReadonlySet<Posture> = new Set<Posture>(['public', 'gated', 'worker', 'legacy']);

/**
 * Install the gate.
 *
 * `onRequest` is the earliest hook with `routeOptions` populated, so the lookup uses the ROUTE's
 * url (`/api/runs/:runId`) rather than the request's (`/api/runs/abc`) — matching on the concrete
 * url would need a router of its own and would miss every parameterised route.
 *
 * A request that matched no route at all has no `routeOptions.url`; Fastify answers those 404 on
 * its own and this stays out of the way.
 */
export function installApiGate(app: FastifyInstance): void {
  app.addHook('onRequest', async (req: FastifyRequest, reply: FastifyReply) => {
    const url = req.routeOptions?.url;
    if (!url || !url.startsWith('/api')) return;
    const key = `${req.method} ${url}`;
    const posture = POSTURE[key];
    if (posture && ALLOWED.has(posture)) return;
    // 403 AND NOT 401. There is no credential that would help — the route has not said what it
    // wants — so inviting the caller to authenticate would be a lie, and a 401 signs the console
    // out (see ADR 0054).
    return reply.code(403).send({
      error:
        `refused: ${key} has not declared an auth posture. Add it to POSTURE in ` +
        `src/auth/apiGate.ts — a route whose admission nobody stated is closed, not open.`,
    });
  });
}
