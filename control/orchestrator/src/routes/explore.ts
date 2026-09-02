/**
 * THE EXACT-RUN EXPLORE MANIFEST — one route, and the only place this API hands out a presigned
 * object-store URL.
 *
 * What `kontra explore <run>` fetches: one Run's Dataset identities, their materialization status,
 * column schemas and presigned GET URLs for THIS RUN'S data files only — never a catalog
 * connstring, never an object-store credential. See data/explore.ts for why PHYSICAL
 * run-exclusivity, not the generated `WHERE run_id`, is what makes that scope real.
 *
 * IT IS NOT PART OF THE WORKBENCH, even though both are gated by the explore token. The workbench
 * runs an operator's statement inside a hardened connection and nothing leaves the process; this
 * hands the caller URLs and steps out of the way. Different failure modes, different blast radius,
 * different file.
 *
 * FAILS CLOSED (`checkBearer`, not the run surface's opt-in): presigning without authentication
 * would make the whole scoping claim false, so with no token configured this 503s and says which
 * variable to set. The TTL is CLAMPED rather than honoured — a caller-chosen expiry is a
 * caller-chosen security boundary, and the point of a short URL is that it is not negotiable
 * upward.
 *
 * WHAT IT NEEDS: the {@link ObjectStore} (for the presigner), the lake overrides, and the
 * {@link RunLifecycle} — the manifest carries the run's two-dimensional status, so the same read
 * the run surface makes decides what `kontra explore` prints while a run is still executing.
 */

import type { FastifyInstance } from 'fastify';

import { EXPLORE_TOKEN_VARS, checkBearer } from '../auth';
import type { ObjectStore } from '../codec/objectStore';
import { DEFAULT_URL_TTL_SECONDS, buildExploreManifest } from '../data/explore';
import type { LakeConfig } from '../data/parquet';
import type { RunLifecycle } from '../runs';
import { errMessage } from './errors';
import { runIdOf } from './runId';

export interface ExploreRouteDeps {
  store: ObjectStore;
  lake: Partial<LakeConfig>;
  /** The two-authority run read — SHARED with the run and hitl modules. */
  runs: RunLifecycle;
}

export function registerExploreRoutes(app: FastifyInstance, deps: ExploreRouteDeps): void {
  const { store, lake, runs } = deps;

  // --- exact-run explore manifest (token-gated, short-lived presigned URLs) ------------
  //
  // What `kontra explore <run>` fetches. It returns the run's Dataset identities, their
  // materialization status, column schemas and presigned GET URLs for THIS RUN'S data files
  // only — never
  // a catalog connstring, never an object-store credential. See data/explore.ts for why
  // physical run-exclusivity, not the generated `WHERE run_id`, is what makes that scope
  // real. Fails closed with no token configured: presigning without authentication would
  // make the whole scoping claim false.
  app.get('/api/runs/:runId/explore', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, EXPLORE_TOKEN_VARS);
    if (denied) {
      if (denied.code === 401) req.log?.warn?.({ path: req.url, ip: req.ip }, 'explore: rejected');
      return reply.code(denied.code).send(denied.body);
    }
    const runId = runIdOf(req);
    const { ttl } = req.query as Partial<{ ttl: string }>;
    const requested = ttl ? Number.parseInt(ttl, 10) : DEFAULT_URL_TTL_SECONDS;
    // Clamp rather than honour: a caller-chosen expiry is a caller-chosen security
    // boundary, and the whole point of a short URL is that it is not negotiable upward.
    const ttlSeconds = Number.isFinite(requested)
      ? Math.min(Math.max(requested, 60), DEFAULT_URL_TTL_SECONDS)
      : DEFAULT_URL_TTL_SECONDS;
    try {
      const view = await runs.read(runId);
      const manifest = await buildExploreManifest(store, runId, view?.materializationRecords ?? [], {
        ttlSeconds,
        lake,
      });
      req.log?.info?.(
        { ip: req.ip, runId, datasets: manifest.datasets.length, ttlSeconds },
        'explore: manifest issued'
      );
      return {
        ...manifest,
        lifecycle: view?.lifecycle ?? 'executing',
        execution: view?.execution ?? 'pending',
        materialization: view?.materialization,
      };
    } catch (err) {
      return reply.code(502).send({ error: `could not build explore manifest: ${errMessage(err)}` });
    }
  });
}
