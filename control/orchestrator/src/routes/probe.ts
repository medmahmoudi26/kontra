/**
 * THE PROBE SURFACE (ADR 0033) — start one Method call, and read what it answered.
 *
 * Two routes, and they are a surface rather than a corner of the sources module because of what
 * they do rather than where they hang: `POST /api/sources/actor/:id/probe` STARTS A RUN, and
 * `GET /api/probes/:runId` reads the return value of the one workflow type kontra owns. Every
 * other route on the `/api/sources/` prefix reads or writes a folder and starts nothing.
 *
 * WHAT IT NEEDS: the {@link SourceStore} — SHARED with `routes/sources.ts`, because the Actor and
 * the version a probe runs are the FOLDER's, exactly as the caller-generation route takes them, so
 * a body cannot aim a probe somewhere the card is not showing. `startProbe` and `readProbe` reach
 * Temporal from inside the handlers.
 *
 * ADMISSION: starting is gated like `POST /api/runs` (`checkOptionalBearer` + `RUN_TOKEN_VARS`),
 * because it is the same act and a Run costs whatever the Actor costs. Reading is open, and bounded
 * by `readProbe` refusing a Run that is not a probe.
 */

import type { FastifyInstance } from 'fastify';

import { checkOptionalBearer } from '../auth';
import { probeRequest, readProbe, startProbe } from '../probe';
import type { SourceStore } from '../sourceStore';
import { ControlRefused, RUN_TOKEN_VARS } from '../workflowControl';
import { errMessage } from './errors';
import { runIdOf } from './runId';

export function registerProbeRoutes(app: FastifyInstance, sources: SourceStore): void {
  /**
   * PROBE ONE METHOD — start the one-shot workflow that calls it over this Batch (ADR 0033).
   *
   * ONE ACTOR, ONE VERSION, ONE METHOD, ONE BATCH, and `probeRequest` refuses everything else by
   * name. The Actor and the version are the FOLDER's, exactly as the caller route in
   * `routes/sources.ts` takes them, so a body cannot aim this somewhere the card is not showing.
   *
   * GATED LIKE `POST /api/runs`, because it is the same act: it starts a Run, and a Run costs
   * whatever the Actor costs. `checkOptionalBearer` — open when `KONTRA_RUN_TOKEN` is unset, which
   * is the operator's choice and the same posture `serve` and `start` take beside it.
   *
   * 400 IS "FIX YOUR REQUEST OR YOUR CLUSTER", and every refusal `startProbe` raises names the fix:
   * a topology, an Actor with no Nexus endpoint, a queue whose only pollers are stale, or a probe
   * worker nobody started. 502 is ours.
   */
  app.post('/api/sources/actor/:id/probe', async (req, reply) => {
    const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { id } = req.params as { id: string };
    const source = sources.get('actor', decodeURIComponent(id));
    if (!source) return reply.code(404).send({ error: `${id}: no such registered folder` });
    try {
      const started = await startProbe(probeRequest(req.body ?? {}, source.name, source.version));
      return reply.code(201).send(started);
    } catch (err) {
      if (err instanceof ControlRefused) return reply.code(400).send({ error: err.message });
      return reply.code(502).send({ error: `could not start the probe: ${errMessage(err)}` });
    }
  });

  /**
   * WHAT ONE PROBE ANSWERED — the probe workflow's own return value, while and after it runs.
   *
   * NOT A "PROBE RUN" KIND (ADR 0033's first consequence). The Run is an ordinary Run: it is in the
   * Runs list, it has a Temporal history, a two-dimensional status and an ordinary Dataset, and
   * every one of those surfaces reads it without knowing what it is. This route reads the RETURN
   * VALUE of the one workflow type kontra owns, which no general surface has a shape for — and
   * which is where `results`, `isolated` and `done` live (ADR 0028 §4).
   *
   * IT REFUSES A RUN THAT IS NOT A PROBE, in `readProbe`, and that bound is the reason this is safe
   * to expose at all: a workflow returns whatever its author put in it.
   */
  app.get('/api/probes/:runId', async (req, reply) => {
    const runId = runIdOf(req);
    try {
      return await readProbe(runId);
    } catch (err) {
      if (err instanceof ControlRefused) return reply.code(404).send({ error: err.message });
      return reply.code(502).send({ error: `could not read the probe: ${errMessage(err)}` });
    }
  });
}
