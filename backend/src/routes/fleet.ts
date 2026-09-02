/**
 * THE FLEET SURFACE — what this cluster has provisioned, and what a bring-up is doing right now.
 *
 * Two routes, and they are grouped by what they ANSWER ABOUT rather than by the prefix they hang
 * off. `GET /api/fleet/operations` is the history of every bring-up, preview and teardown;
 * `GET /api/runs/:runId/phase` sits on the run prefix because a Fleet child is addressed by the
 * Run that started it, but what it reports is the Fleet's phase and nothing else. Splitting them
 * across two modules on the strength of a URL segment would put the two halves of one page's data
 * in two files.
 *
 * WHAT IT NEEDS: nothing injected. `listFleetOperations` dials Temporal's visibility index and
 * `readFleetPhase` queries a child workflow directly; both are reached only from inside a handler,
 * so registering these routes opens no connection.
 *
 * NEITHER ROUTE IS A RUN ROUTE, and ADR 0017's rule against merging two authorities into one field
 * is why: a Fleet operation dispatches no Actors and writes no Dataset, so it is not a `kind`
 * column on the run list.
 */

import type { FastifyInstance } from 'fastify';

import { readFleetPhase } from '../fleetPhase';
import { listFleetOperations } from '../temporalClient';
import { errMessage } from './errors';
import { runIdOf } from './runId';

export function registerFleetRoutes(app: FastifyInstance): void {
  /**
   * Fleet operation history — every bring-up, preview and teardown this cluster still holds.
   *
   * A sibling of `GET /api/runs`, not part of it. A Fleet operation is not a Run (it dispatches no
   * Actors and writes no Dataset), and ADR 0017's rule against merging two authorities into one
   * field is why it gets its own route rather than a `kind` column on the run list.
   *
   * It exists because there was NO fleet history anywhere: four `kontra-fleet/nscheck-0.1.0`
   * executions on this controller appeared on no surface, and Temporal's own list is subject to
   * retention. A teardown that failed and was never noticed is a droplet still being billed.
   */
  app.get('/api/fleet/operations', async (req, reply) => {
    const { limit } = req.query as Partial<{ limit: string }>;
    const n = limit ? Number.parseInt(limit, 10) : undefined;
    try {
      return await listFleetOperations(Number.isFinite(n) ? (n as number) : undefined);
    } catch (err) {
      return reply.code(502).send({ error: `could not list fleet operations: ${errMessage(err)}` });
    }
  });

  // The phase a Fleet child reports about ITSELF, for the run that started it. MEASURED on
  // `nscheck-1786831339`: 156 of 294 seconds read "nothing dispatched yet" while `kontra-fleet/dns`
  // was answering `{phase, op}` to anyone who asked. Nobody asked.
  //
  // `:runId` IS A WORKFLOW ID, exactly as it is on `/history` — the console reads the fleet
  // child's id off the run's own reduced event log (`RunEvent.link`) and asks here, so there is no
  // second discovery path and no second read of the history. `?exec=` pins the execution for the
  // same reason it does there: `kontra-fleet/dns` is the id of every bring-up AND teardown.
  //
  // IT DOES NOT FAIL. "Could not ask" and "it did not answer" are answers the run detail must be
  // able to print beside a window whose duration the history already knows; a 502 would blank all
  // of it. See fleetPhase.ts for what the answer may carry, and why it carries no more.
  app.get('/api/runs/:runId/phase', async (req) => {
    const runId = runIdOf(req);
    const { exec } = (req.query ?? {}) as { exec?: string };
    return readFleetPhase(runId, exec || undefined);
  });
}
