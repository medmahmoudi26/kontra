/**
 * WHO IS POLLING A TASK QUEUE — one route, and a surface of its own because Temporal is its only
 * authority.
 *
 * A workflow is running, or served-and-idle, or neither, and only the first was knowable from this
 * API. "Served" is not a property of a file (the workflow surface) or of a run (the run surface):
 * it is whether a worker is POLLING. Registration says a workflow exists; a poller says it can run.
 *
 * WHAT IT NEEDS: `queueDescriber`, a lazily-built Temporal connection SHARED with the run module —
 * `POST /api/runs` refuses a start into a queue nobody polls using the same read. Building one per
 * request would open a connection per poll of every workflow on the page, and the page polls, which
 * is why the singleton lives in `buildServer` and arrives here as a getter rather than a value.
 *
 * The machinery already existed and had no way out of the process — `panels/pollers.ts` is what
 * `fleet.ready()` uses through the `queuePollers` activity, precisely so a run does not
 * dispatch into a queue nobody serves and then report as slow. This is that read, over HTTP.
 */

import type { FastifyInstance } from 'fastify';

import type { Repo } from '../db/repo';
import { describeQueue, type QueueDescriber } from '../panels/pollers';
import { QUEUE_RE } from '../workflowControl';
import { errMessage } from './errors';

export function registerPollerRoutes(
  app: FastifyInstance,
  queueDescriber: () => QueueDescriber,
  repo: Repo,
): void {
  /**
   * EVERY QUEUE AT ONCE — what the console actually asks for, and what was missing.
   *
   * `Actors.svelte` and `catalog/load.ts` both GET `/api/pollers` and key the result by queue. Only
   * the per-queue route below existed, so all three call sites 404ed and fell back to `{}` — and
   * because an ABSENT report is indistinguishable from an unreachable cluster, every actor on the
   * page rendered "UNKNOWN / the cluster could not be asked" while its worker was polling happily.
   * A 404 swallowed by `.catch(() => ({}))` is the silent-empty shape this codebase keeps finding:
   * the page was not wrong about what it had, it was never given anything.
   *
   * ONE ROUND TRIP, NOT N. The page knows N queues and polls; asking per queue would open a
   * describe per actor per poll against the shared Temporal connection. They are gathered
   * concurrently here instead.
   *
   * A QUEUE THAT CANNOT BE DESCRIBED STILL GETS A ROW, carrying `error`. Dropping it would make it
   * absent, and absent reads as "no such queue" rather than "could not ask" — precisely the
   * distinction the per-queue route below goes to such lengths to preserve.
   */
  app.get('/api/pollers', async () => {
    const queues = new Set<string>();
    for (const a of repo.listActors()) queues.add(`${a.name}-${a.version}`);
    for (const w of repo.listWorkflows()) if (w.queue) queues.add(w.queue);

    const named = [...queues].filter((q) => QUEUE_RE.test(q));
    const reports = await Promise.all(
      named.map(async (queue) => {
        try {
          const state = await describeQueue(queueDescriber(), queue);
          return [
            queue,
            {
              queue,
              pollers: state.identities.length,
              identities: state.identities,
              workers: state.workers,
              lastPoll: state.lastPoll,
              ...(state.error === undefined ? {} : { error: state.error }),
            },
          ] as const;
        } catch (err) {
          return [
            queue,
            { queue, pollers: 0, identities: [], workers: [], lastPoll: 0, error: errMessage(err) },
          ] as const;
        }
      }),
    );
    return Object.fromEntries(reports);
  });

  /**
   * IS ANYBODY SERVING THIS QUEUE — the third state a workflow can be in.
   *
   * ZERO AND UNKNOWN ARE DIFFERENT ANSWERS, and the shape keeps them apart. `pollers: 0` with no
   * error means nothing is polling; `error` set means TEMPORAL COULD NOT BE ASKED, and the count is
   * meaningless rather than zero. Rendering both as "idle" would put a grey dot on a workflow that
   * is serving perfectly, which is the same defect as a green label over lost work (ADR 0017).
   *
   * AND ONE TIMESTAMP PER POLLER, not just the freshest across the queue. `lastPoll` answers "is
   * anything serving this queue"; it cannot answer "is THIS worker serving", and the two come apart
   * exactly where it costs — Temporal keeps a poller listed for about five minutes after it stops,
   * so a queue with one live worker and one killed three minutes ago has a fresh `lastPoll` and two
   * identities. A reader picking a dispatch target from `identities` would pick the dead one. Both
   * shapes are sent: `identities` for callers that only ask about the queue, `workers` for the ones
   * that ask about a worker.
   *
   * Open like every other read here. It names a queue and reports a count.
   */
  app.get('/api/queues/:queue/pollers', async (req, reply) => {
    const { queue } = req.params as { queue: string };
    if (!QUEUE_RE.test(queue)) {
      return reply.code(400).send({ error: `queue ${JSON.stringify(queue)} is not a task-queue name` });
    }
    try {
      const state = await describeQueue(queueDescriber(), queue);
      return {
        queue,
        pollers: state.identities.length,
        identities: state.identities,
        workers: state.workers,
        lastPoll: state.lastPoll,
        ...(state.error === undefined ? {} : { error: state.error }),
      };
    } catch (err) {
      // Never a 502. A streamer that cannot reach Temporal must report "unknown", not "down" — the
      // whole point of the shape above.
      return { queue, pollers: 0, identities: [], workers: [], lastPoll: 0, error: errMessage(err) };
    }
  });
}
