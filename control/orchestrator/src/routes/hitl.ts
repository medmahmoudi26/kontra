/**
 * THE ASK SURFACE — every decision a parked Run is waiting on, and the one verb that answers it.
 *
 * Two routes on the run prefix, and a surface of their own because an ask is a fact with its own
 * authority: the run's MEMO, read off the describe, so a run parked with its worker down answers
 * here exactly as well as a healthy one. That is the whole reason an ask is emitted rather than
 * exposed as a query handler; see hitl.ts.
 *
 * WHAT IT NEEDS:
 *   - `runs`     the {@link RunLifecycle}, for the live memo. SHARED with the run and explore
 *                modules.
 *   - `archive`  the {@link HistoryArchive} (ADR 0025), for the runs Temporal has dropped. SHARED
 *                with the history module, and built over the SAME object store the dataset browser
 *                reads — an archive in a different bucket from the run's units is an archive
 *                nobody finds.
 *
 * The two are asked in that order, on both routes, so an archived run answers `[]` rather than 404
 * and a live run never pays for a store read. `signalRun` is the one WRITE, reached only after the
 * answer has been validated — see below for why that ordering is the load-bearing part.
 *
 * ADMISSION: reading is open; answering takes `checkOptionalBearer` + `RUN_TOKEN_VARS`, because
 * answering "yes" to a workflow parked before `fleet.up()` provisions cloud machines.
 */

import type { FastifyInstance } from 'fastify';

import { checkOptionalBearer } from '../auth';
import {
  AnswerRefused,
  AskNotFound,
  AskNotPending,
  answerAsk,
  defaultOperator,
} from '../hitl';
import type { HistoryArchive } from '../historyArchive';
import type { RunLifecycle } from '../runs';
import { signalRun } from '../temporalClient';
import { RUN_TOKEN_VARS } from '../workflowControl';
import { errMessage } from './errors';
import { runIdOf } from './runId';

export interface HitlRouteDeps {
  /** The live authority: a Run's asks off its own memo. */
  runs: RunLifecycle;
  /** The archived authority (ADR 0025), for a Run Temporal no longer has. */
  archive: HistoryArchive;
}

export function registerHitlRoutes(app: FastifyInstance, deps: HitlRouteDeps): void {
  const { runs, archive } = deps;

  /**
   * EVERY PENDING (and past) ASK ON ONE RUN — a LIST, always, even when there is one.
   *
   * Parallel branches each needing a decision is normal in these workflows, so a singular route
   * would have had to be widened later and would have broken both its own contract and the
   * transcript's rendering when it was. It is a list from the first commit.
   *
   * NO WORKER IS CONSULTED. The asks are the run's own memo, read off the describe — so a run
   * parked with its worker down answers here exactly as well as a healthy one. That is the whole
   * reason an ask is emitted rather than exposed as a query handler; see hitl.ts.
   *
   * AND IT FALLS BACK TO THE ARCHIVE (ADR 0025), for the same reason `/history` does: retention
   * drops the execution long before anyone stops caring what was approved and by whom. 404 means
   * BOTH authorities have nothing; an archived run that asked nobody anything answers `[]`.
   */
  app.get('/api/runs/:runId/asks', async (req, reply) => {
    const runId = runIdOf(req);
    try {
      const asks = (await runs.asks(runId)) ?? (await archive.asks(runId));
      if (!asks) return reply.code(404).send({ error: 'run not found' });
      return { runId, asks, pending: asks.filter((a) => a.state === 'pending') };
    } catch (err) {
      return reply.code(502).send({ error: `could not read asks: ${errMessage(err)}` });
    }
  });

  /**
   * ANSWER ONE ASK. Validated against the schema that ask declared, and only then signalled.
   *
   * VALIDATION BEFORE THE SIGNAL is the ordering that matters: a signal is durable and there is no
   * taking it back, so an answer that does not fit is refused here — naming the field — with the
   * run left exactly as parked as it was.
   *
   * GATED BY THE RUN TOKEN, as `start` and `stop` are, and for the same reason: answering "yes" to
   * a workflow parked before `fleet.up()` provisions cloud machines. Open by default because that
   * was the choice (`describeExposure()` says so out loud); a token turns it on with no other
   * change.
   *
   * `by` IS ATTRIBUTION AND NOT AUTHENTICATION. The appliance is loopback with no credential, so
   * this records what the client called itself, falling back to this box's `KONTRA_OPERATOR`.
   * Nothing is gated on it, and nothing in this codebase may describe it as proof.
   */
  app.post('/api/runs/:runId/asks/:askId', async (req, reply) => {
    const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const runId = runIdOf(req);
    const { askId } = req.params as { askId: string };
    const body = (req.body ?? {}) as { value?: unknown; by?: unknown };
    try {
      const answered = await answerAsk(
        {
          asks: async (id) => (await runs.asks(id)) ?? (await archive.asks(id)),
          signal: signalRun,
        },
        runId,
        decodeURIComponent(askId),
        {
          value: body.value,
          by: typeof body.by === 'string' && body.by ? body.by : defaultOperator(),
        }
      );
      return { ok: true, ask: answered };
    } catch (err) {
      if (err instanceof AnswerRefused) {
        return reply.code(400).send({ error: err.message, field: err.field });
      }
      if (err instanceof AskNotFound) return reply.code(404).send({ error: err.message });
      // 409, not 400: the request was well formed and arrived too late (or twice). An operator who
      // is told "invalid" about an answer somebody else already gave goes looking at their form.
      if (err instanceof AskNotPending) {
        return reply.code(409).send({ error: err.message, state: err.ask.state });
      }
      return reply.code(502).send({ error: `could not answer ask: ${errMessage(err)}` });
    }
  });
}
