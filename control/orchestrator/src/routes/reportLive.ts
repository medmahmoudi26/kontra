/**
 * `GET /api/runs/:runId/report/live` — a run's report, re-rendered as it progresses (ADR 0062).
 *
 * ── THE ROUTE SPELLING IS LOAD-BEARING ─────────────────────────────────────────────────────────
 *
 * `:runId`, NEVER `:id`. `openapi.ts`'s `GATES` is longest-prefix over the raw Fastify url, so
 * `/api/runs/:runId/report/live` inherits `exploreToken` from the `/api/runs/:runId/report` entry
 * already there and needs no new entry. The `:id` spelling falls through to `/api/runs` and would be
 * published as `runToken` — a DIFFERENT credential from the one the handler checks. `openapi.test.ts`
 * cannot catch that: it verifies that paths documented OPEN are open, so a wrong-but-documented gate
 * passes. Two such mismatches already exist in this tree.
 *
 * ── SSE OVER fetch, NOT EventSource ────────────────────────────────────────────────────────────
 *
 * The console installs its credential by WRAPPING `window.fetch`, and `checkBearer` reads only the
 * `Authorization` header. `EventSource` goes through neither, and cannot set a header. So the client
 * is `fetch` + `ReadableStream` with hand-written reconnection, which is the shape `logstream.ts`
 * already uses. A token in the query string was rejected there because it lands in the access log,
 * and that rejection stands here.
 */

import type { FastifyInstance, FastifyReply, FastifyRequest } from 'fastify';

import { observesCommits, runEvents } from '../runEvents';
import { LiveHub, type LiveEvent, type LiveRunKey, type RenderOnce } from '../report/live';
import { errMessage } from './errors';

export interface ReportLiveDeps {
  /** The §2.4 context plus its template, rendered. The SAME assembler the sweep and preview use. */
  renderOnce: RenderOnce;
  /**
   * Resolve a run id to its CURRENT execution, or `undefined` if there is no such run.
   *
   * CALLED BEFORE ANY PER-RUN STATE IS MINTED. `rowTail` records the measured incident this prevents:
   * one unauthenticated client opened 250 streams on 250 FICTIONAL ids and got 250 pollers, because
   * the id was never validated.
   */
  resolveRun: (runId: string) => Promise<{ runStartedAt: number; status: string; closedAt: number } | undefined>;
  /**
   * Resolves when the run reaches a terminal state, with the stored version if one was minted.
   *
   * Optional. Without it a watched run still freezes — the 60-second reconciliation pass renders and
   * stores it — just not within seconds. Supplied by the server as a Temporal history long poll,
   * which is why it is a dep rather than an import: the route stays testable without a Temporal.
   */
  watchTerminal?: (key: LiveRunKey) => Promise<{ version?: number } | undefined>;
  /** The gate the rest of the report surface uses. Shared so live cannot drift from it. */
  admit: (req: FastifyRequest, reply: FastifyReply) => boolean;
  onError?: (err: unknown, runId?: string) => void;
  hub?: LiveHub;
}

/** One SSE frame. Named events, so a client switches rather than sniffing a payload. */
function frame(event: LiveEvent): string {
  return `event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`;
}

export function registerReportLiveRoute(app: FastifyInstance, deps: ReportLiveDeps): LiveHub {
  const degraded = observesCommits()
    ? undefined
    : 'this process does not run the materializer role, so it cannot observe batch commits — this ' +
      'report will not update until the run ends (KONTRA_ROLES)';

  const hub =
    deps.hub ??
    new LiveHub({
      renderOnce: deps.renderOnce,
      degraded,
      onError: (err) => deps.onError?.(err),
    });

  // ONE SUBSCRIPTION FOR THE PROCESS, not one per viewer. The hub routes by run, and an unwatched
  // run's event finds no session and costs a map lookup.
  runEvents.onData((e) => hub.onData({ runId: e.runId, runStartedAt: e.runStartedAt }));
  runEvents.onStatus((e) =>
    hub.statusChanged({ runId: e.runId, runStartedAt: e.runStartedAt }, e.status)
  );

  app.get('/api/runs/:runId/report/live', async (req, reply) => {
    if (!deps.admit(req, reply)) return undefined;
    const { runId } = req.params as { runId: string };

    let run: Awaited<ReturnType<ReportLiveDeps['resolveRun']>>;
    try {
      run = await deps.resolveRun(runId);
    } catch (err) {
      return reply.code(502).send({ error: `could not resolve run ${runId}: ${errMessage(err)}` });
    }
    if (!run) {
      return reply.code(404).send({
        error: `no run ${runId}. A live report needs a run that exists — see GET /api/runs.`,
      });
    }
    if (run.closedAt > 0) {
      // ALREADY FINISHED, SO THERE IS NOTHING TO STREAM. The stored version is the answer, and
      // pretending otherwise would hold a connection open for a document that cannot change.
      return reply.code(409).send({
        error: `run ${runId} has already ended; read its stored report at GET /api/runs/${runId}/report`,
        state: 'closed',
      });
    }

    const key: LiveRunKey = { runId, runStartedAt: run.runStartedAt };
    const queue: string[] = [];
    const push = (event: LiveEvent): void => {
      queue.push(frame(event));
      flush();
    };

    const opened = hub.open(key, push);
    if (!opened.ok) {
      // 503 RATHER THAN 429: the service is at capacity, not this caller over a quota, and a client
      // should retry rather than back off for a window it cannot discover.
      return reply.code(503).send({
        error:
          opened.reason === 'per-run'
            ? `run ${runId} already has the maximum number of live viewers`
            : 'the orchestrator is at its live-report capacity',
        reason: opened.reason,
      });
    }

    reply.raw.writeHead(200, {
      'content-type': 'text/event-stream',
      'cache-control': 'no-store',
      connection: 'keep-alive',
      // Nginx and friends buffer an event stream into uselessness otherwise.
      'x-accel-buffering': 'no',
    });

    let open = true;
    function flush(): void {
      if (!open) return;
      while (queue.length > 0) reply.raw.write(queue.shift()!);
    }

    const stop = (): void => {
      if (!open) return;
      open = false;
      opened.detach();
      reply.raw.end();
    };
    req.raw.on('close', stop);
    req.raw.on('error', stop);

    // THE SNAPSHOT IS THE RENDER ALREADY IN HAND WHEN THERE IS ONE — which is what makes a reconnect
    // a replay rather than a second render, and what makes the tenth tab free.
    const held = opened.session.snapshotEvent();
    if (held) push(held);
    else await opened.session.renderNow();

    if (deps.watchTerminal) {
      void deps
        .watchTerminal(key)
        .then((end) => {
          if (!open) return;
          hub.finalize(key, end?.version ?? 0);
          stop();
        })
        .catch((err) => deps.onError?.(err, runId));
    }

    // Fastify must not also try to answer: the socket is ours until the run ends or the client goes.
    return reply;
  });

  return hub;
}
