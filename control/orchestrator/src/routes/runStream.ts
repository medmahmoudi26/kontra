/**
 * `GET /api/runs/:runId/stream` — a run's transcript, pushed instead of polled.
 *
 * WHAT IT REPLACES. The console follows a Run by polling: `TranscriptHub` runs a `setInterval` and
 * each tick makes TWO requests — `/history` and `/asks`. That is per run, per interval, per open
 * tab, against the same event loop that serves the query workbench's DuckDB reads. Shortening the
 * interval and lowering the cost pull in opposite directions.
 *
 * THE WIN IS N TABS → ONE POLL. This route polls the same two sources once and fans the result out
 * to every subscriber, emitting only when the payload CHANGES. Three tabs on one Run stop being six
 * requests per tick and become two.
 *
 * ── SSE, NOT A WEBSOCKET ────────────────────────────────────────────────────────────────────────
 *
 * This is one-directional: the server has state, the browser draws it. SSE reconnects on its own,
 * survives proxies that mangle upgrades, and needs no protocol of its own. The panels streamer
 * already owns the bidirectional case on its own origin (ADR 0020) and keeps owning it — this must
 * not become a second socket layer.
 *
 * ── THE TRANSPORT CHANGES; THE READING DOES NOT ─────────────────────────────────────────────────
 *
 * kontra gives a Run more than one reading — Workflows, Monitor, Datasets — and none substitutes
 * for another. This makes an existing reading arrive faster. It must never become a FOURTH reading
 * that disagrees with the other three, which is why the payload is assembled by the same functions
 * the polled routes use rather than by a second query.
 */

import type { FastifyInstance, FastifyReply, FastifyRequest } from 'fastify';

/**
 * Where a stream writes. Split out so the LOOP can be tested without a socket.
 *
 * `app.inject()` waits for a response to END, and an event stream never does — so a route-level
 * test of this hangs for thirty seconds and tells you nothing. The same split the console already
 * uses between `MethodCall` (state and fetches) and `MethodCallPanes` (props in, markup out).
 */
export interface StreamSink {
  write: (chunk: string) => void;
  close: () => void;
}

/** How often the server re-reads a followed Run. One poll, however many subscribers. */
const POLL_MS = 1_000;

/**
 * A Run that has been terminal this long stops being watched.
 *
 * Not zero: a terminal Run's last events are exactly what somebody opening the page wants, and
 * closing the instant it completes would race the draw.
 */
const LINGER_MS = 10_000;

export interface RunStreamDeps {
  /** Read one Run's transcript — the SAME function the polled routes use. */
  read: (runId: string) => Promise<{ ok: boolean; terminal?: boolean; body: unknown }>;
  /** Injectable for the test; `setInterval` in production. */
  schedule?: (tick: () => void, ms: number) => () => void;
  now?: () => number;
}

/** One SSE frame. Named events so a client can tell a state push from an ending. */
function frame(event: string, data: unknown): string {
  return `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
}

/** The SSE headers, in one place so the route and any test agree about them. */
export const STREAM_HEADERS = {
  'Content-Type': 'text/event-stream',
  'Cache-Control': 'no-cache, no-transform',
  Connection: 'keep-alive',
  // Nginx and friends buffer by default, which turns a stream into one very late response.
  'X-Accel-Buffering': 'no',
} as const;

/**
 * Follow one Run and write frames to `sink` until it ends or the caller stops.
 *
 * Returns the stop function. This is the whole decision layer: what to send, when to stay quiet,
 * and when a Run is finished with.
 */
export function followRun(runId: string, sink: StreamSink, deps: RunStreamDeps): () => void {
  const schedule =
    deps.schedule ??
    ((tick: () => void, ms: number) => {
      const h = setInterval(tick, ms);
      return () => clearInterval(h);
    });
  const now = deps.now ?? (() => Date.now());

  {
    let last = '';
    // -1, NOT 0, and the distinction is a real bug this had. `now()` can legitimately BE 0 — it is
    // under a test clock, and it is with any injected monotonic source — so `if (!terminalAt)` read
    // a valid timestamp as "unset" and restarted the linger window on every tick. A terminal Run
    // then never ended and held its connection forever, which is the exact thing this window is for.
    let terminalAt = -1;
    let stop = () => {};
    let closed = false;

    const end = () => {
      if (closed) return;
      closed = true;
      stop();
      sink.close();
    };

    const tick = async () => {
      if (closed) return;
      let got;
      try {
        got = await deps.read(runId);
      } catch (err) {
        // A READ FAILURE IS NOT THE END OF THE STREAM. The orchestrator may be talking to a
        // Temporal that blipped; the client would reconnect into the same condition. Report it and
        // keep trying — the client decides when to give up.
        sink.write(frame('warn', { detail: err instanceof Error ? err.message : String(err) }));
        return;
      }

      // ONLY ON CHANGE. A Run that is thinking produces the same transcript every tick, and pushing
      // it would spend the bandwidth polling spent while claiming to be a stream.
      const encoded = JSON.stringify(got.body);
      if (encoded !== last) {
        last = encoded;
        sink.write(frame('state', got.body));
      }

      if (got.terminal) {
        if (terminalAt < 0) terminalAt = now();
        if (now() - terminalAt >= LINGER_MS) {
          // A COMPLETED RUN DOES NOT HOLD A CONNECTION OPEN FOREVER. `end` is a named event so the
          // client can stop rather than treating the close as a dropped stream and reconnecting
          // into a Run that will never change again.
          sink.write(frame('end', { runId }));
          end();
        }
      } else {
        terminalAt = -1;
      }
    };

    stop = schedule(() => void tick(), POLL_MS);
    void tick(); // the first frame is immediate, not one interval late

    return end;
  }
}

export function registerRunStream(app: FastifyInstance, deps: RunStreamDeps): void {
  app.get('/api/runs/:runId/stream', async (req: FastifyRequest, reply: FastifyReply) => {
    const { runId } = req.params as { runId: string };
    reply.raw.writeHead(200, STREAM_HEADERS);
    const end = followRun(
      runId,
      { write: (c) => reply.raw.write(c), close: () => reply.raw.end() },
      deps
    );
    req.raw.on('close', end);
  });
}
