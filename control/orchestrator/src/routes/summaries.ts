/**
 * OPERATIONAL SUMMARIES — bounded scalars, the "is it moving?" plane.
 *
 * Three routes over the relational summary tables: materialization health, one Run's row, and an
 * hourly series. The "is it moving?" question reads THESE, not the findings files — four scalar
 * columns from a relational table instead of a Parquet scan per refresh, and no struct types to
 * flatten. "What did it find?" is answered by `kontra explore`, on the operator's machine.
 *
 * THAT DIVISION IS WHY THIS IS NOT PART OF THE DATASET BROWSER. Every route in
 * `routes/datasets.ts` reads the lake; every route here reads a table beside it, and the whole
 * point of the summaries is that a dashboard polling them never touches a Parquet file.
 *
 * WHAT IT NEEDS: the {@link SummaryStore} — SHARED with the retention module, which purges a
 * collected Run's row through it.
 *
 * The hourly window is CLAMPED. An unbounded one here would let a dashboard refresh pull the whole
 * table every 60 seconds; ungated, like the rest of this read surface, so the clamp is the only
 * thing standing between a poll and a table scan.
 */

import type { FastifyInstance } from 'fastify';

import type { SummaryStore } from '../data/summaries';
import { errMessage } from './errors';
import { runIdOf } from './runId';

export function registerSummaryRoutes(app: FastifyInstance, summaries: SummaryStore): void {
  app.get('/api/summaries/health', async (_req, reply) => {
    try {
      return await summaries.getHealth();
    } catch (err) {
      return reply.code(502).send({ error: `could not read materialization health: ${errMessage(err)}` });
    }
  });

  app.get('/api/summaries/runs/:runId', async (req, reply) => {
    const runId = runIdOf(req);
    try {
      const row = await summaries.getRun(runId);
      return row ?? reply.code(404).send({ error: 'no summary for that run' });
    } catch (err) {
      return reply.code(502).send({ error: `could not read run summary: ${errMessage(err)}` });
    }
  });

  app.get('/api/summaries/hourly', async (req, reply) => {
    const { hours } = req.query as Partial<{ hours: string }>;
    const n = hours ? Number.parseInt(hours, 10) : 24;
    // Clamped: an unbounded window here would let a dashboard refresh pull the whole
    // table every 60 seconds.
    const window = Number.isFinite(n) ? Math.min(Math.max(n, 1), 24 * 30) : 24;
    try {
      return { hours: window, series: await summaries.hourly(Date.now() - window * 3_600_000) };
    } catch (err) {
      return reply.code(502).send({ error: `could not read hourly output: ${errMessage(err)}` });
    }
  });
}
