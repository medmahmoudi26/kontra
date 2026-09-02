/**
 * LIVENESS, AT THE TWO SCOPES ANYTHING POLLING THIS API CARES ABOUT.
 *
 *   `GET /api/health`  is this PROCESS up. No dependency, no I/O, no cluster — which is the whole
 *                      contract: a compose healthcheck, `kontra up`'s readiness wait and the CLI's
 *                      progress bar all use it to tell "the port is bound" from "the port is not",
 *                      and a health route that could 502 on somebody else's outage would make that
 *                      reading useless.
 *   `GET /api/pulse`   is ANYTHING HAPPENING at all, anywhere — two bounded Temporal reads.
 *
 * The two live together because they are the same question at different radii, and because putting
 * the trivial one in `buildServer` would leave that function holding exactly one route: the shape
 * this split exists to end.
 *
 * WHAT IT NEEDS: {@link PulseDeps}, or nothing — with none injected it defaults to the real
 * cluster. That default is BUILT ONCE, here, rather than per request, so a test that injects
 * deps injects them for every poll the chrome makes; and `countRuns`/`listOpenRuns` are reached
 * only from inside those closures, so no Temporal connection is opened by registering the routes.
 */

import type { FastifyInstance } from 'fastify';

import { clusterPulse, type PulseDeps } from '../pulse';
import { countRuns, listOpenRuns } from '../temporalClient';
import { errMessage } from './errors';

export function registerPulseRoutes(app: FastifyInstance, injected?: PulseDeps): void {
  // The pulse's two reads, defaulted to the real cluster. Built once here rather than per request so
  // a test that injects them injects them for every poll the chrome makes.
  const pulse: PulseDeps = injected ?? {
    count: (filter) => countRuns(filter),
    open: (cap) => listOpenRuns(cap),
    now: () => Date.now(),
  };

  app.get('/api/health', async () => ({ ok: true }));

  /**
   * IS ANYTHING HAPPENING AT ALL, ANYWHERE — the question the retired Runs surface used to answer.
   *
   * A COUNT AND A WAY IN, NEVER A LIST. `GET /api/runs` is the list and costs what a list costs; the
   * chrome polls this instead, on every surface, and it is two bounded reads: a count off the
   * visibility index, and the open runs' memos, which ride on their listing for free. Nothing here
   * grows with how many runs this controller has ever held — see `pulse.ts` for the whole argument
   * and for why `stalled` is deliberately not among the answers.
   *
   * 502, NEVER A ZERO, WHEN TEMPORAL CANNOT BE REACHED. A chrome badge reading "idle" because the
   * cluster was unreachable is the exact failure mode this appliance keeps writing defences against
   * — "we could not look" and "nothing is happening" are the two readings it keeps furthest apart —
   * so an unreachable Temporal must arrive at the browser as an error it can render as a sentence.
   */
  app.get('/api/pulse', async (_req, reply) => {
    try {
      return await clusterPulse(pulse);
    } catch (err) {
      return reply.code(502).send({ error: `could not read the pulse: ${errMessage(err)}` });
    }
  });
}
