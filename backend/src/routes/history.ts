/**
 * THE EVENT-LOG SURFACE — one route, and the only drill-down this API has.
 *
 * `GET /api/runs/:runId` says a run is `running`; it cannot say whether that run is grinding
 * through Batches, sitting in a retry backoff, or blocked on a queue nobody polls, and only the
 * history tells those three apart. Payload-free by construction — see history.ts.
 *
 * IT IS ALSO THE DRILL-DOWN, AND THERE IS NO SECOND ONE. A child workflow and a dispatch's backing
 * workflow are just workflow ids, so the console reads a level below a run by asking HERE again
 * with the id off the event (`RunEvent.link`). That is why the route is a module rather than a line
 * in the run module: it answers about any execution the log names, not just the Run the caller
 * started.
 *
 * WHAT IT NEEDS: the {@link HistoryArchive} (ADR 0025) — SHARED with the hitl module, and built
 * over the SAME object store the dataset browser reads. `fetchRunHistory` is the live authority and
 * is imported directly; `readHistoryOrArchive` owns the order and the fallback rule.
 */

import type { FastifyInstance } from 'fastify';

import { type HistoryArchive, readHistoryOrArchive } from '../historyArchive';
import { fetchRunHistory } from '../temporalClient';
import { errMessage } from './errors';
import { runIdOf } from './runId';

export function registerHistoryRoutes(app: FastifyInstance, archive: HistoryArchive): void {
  // The run's Temporal event log. What `/api/runs/:runId` cannot say: a run is `running` whether
  // it is grinding through Batches, sitting in a retry backoff, or blocked on a queue nobody
  // polls, and only the history tells those three apart. Payload-free by construction — see
  // history.ts. 404 when Temporal has dropped the execution for retention, which is ordinary.
  //
  // THIS ROUTE IS ALSO THE DRILL-DOWN, and there is no second one. A child workflow and a
  // dispatch's backing workflow are just workflow ids, so the console reads a level below a run by
  // asking here again with the id off the event (`RunEvent.link`). `?exec=` pins Temporal's run id
  // for that execution, because `kontra-fleet/dns` is the id of both the bring-up and the teardown
  // and by id alone Temporal answers with the later one.
  //
  // AND IT FALLS BACK TO THE ARCHIVE (ADR 0025). Retention drops the execution long before the
  // Datasets it wrote expire, so this route answers from `history/run=…` when Temporal has nothing.
  // The fallback keys off NOT-FOUND alone: `fetchRunHistory` throws for anything that is not gRPC
  // code 5, so a cluster that is unwell still 502s here rather than quietly serving a stale log as
  // if it were live. A 404 now means BOTH authorities have nothing.
  app.get('/api/runs/:runId/history', async (req, reply) => {
    const runId = runIdOf(req);
    const { exec } = (req.query ?? {}) as { exec?: string };
    try {
      const history = await readHistoryOrArchive(runId, exec || undefined, archive, fetchRunHistory);
      return history ?? reply.code(404).send({ error: 'no history for that run' });
    } catch (err) {
      return reply.code(502).send({ error: `could not read history: ${errMessage(err)}` });
    }
  });
}
