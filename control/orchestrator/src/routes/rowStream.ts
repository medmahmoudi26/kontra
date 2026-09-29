/**
 * THE LIVE ROW TAIL — one SSE route, and the only streaming lifecycle on this API.
 *
 * It is a module of its own because it is the only route here that owns STATE BETWEEN REQUESTS: a
 * server-side poller per watched Run, a ring, three caps, and a per-client counter that must be
 * decremented exactly once when a socket dies. Everything else in `routes/` answers and forgets.
 *
 * WHAT IT NEEDS: the {@link ObjectStore} — the SAME one the dataset browser reads, because the
 * count on screen must be a reading of what is actually committed, not a side channel — and,
 * optionally, the three caps a test lowers so it can reach "too many" with two connections rather
 * than sixty-five.
 *
 * WHAT IT OWNS, and what `buildServer` therefore does not: the {@link RowTailHub}, the per-client
 * map, the effective cap numbers a refusal quotes, and the `onClose` hook that stops the pollers
 * when the server shuts down. The hub's timers are unref'd, but a clean close must not leave a
 * poller LISTing an object store nobody is reading — so the teardown is registered here, next to
 * the thing it tears down.
 *
 * THE THREE CAPS ARE NOT ONE CAP THREE TIMES. `maxPerClient` is about a CALLER (how many sockets
 * they hold); `ROW_TAIL_MAX_RUNS` and `ROW_TAIL_MAX_SINKS` are about the OBJECT STORE (how many
 * prefixes are being LISTed, and by how many readers). Conflating them is how 250 connections from
 * one machine looked like ordinary use: they were 250 different run ids, so every per-run bound in
 * the world would have said yes.
 */

import type { FastifyInstance } from 'fastify';

import type { ObjectStore } from '../codec/objectStore';
import { ROW_TAIL_MAX_RUNS, ROW_TAIL_MAX_SINKS, RowTailHub, rowTailFrame } from '../rowTail';

export interface RowStreamCaps {
  /** Total Runs the hub will watch at once. Default {@link ROW_TAIL_MAX_RUNS}. */
  maxRuns?: number;
  /** Readers on any ONE Run. Default {@link ROW_TAIL_MAX_SINKS}. */
  maxSinks?: number;
  /** Streams one caller may hold. Default 8. */
  maxPerClient?: number;
}

export function registerRowStreamRoute(
  app: FastifyInstance,
  store: ObjectStore,
  caps: RowStreamCaps = {}
): void {
  // The live row tail (live-datasets slice 05): one server-side poller per WATCHED Run, LISTing the
  // durable `units/run=<id>/` path and fanning the count out to every subscriber, so two tabs on one
  // Run agree. Reads the SAME store the dataset browser does — the count on screen must be a reading
  // of what is actually committed, not a side channel. Its own ring and cap, deliberately not the
  // `/api/events` state ring (which a large row scan would evict). Nothing is polled until a browser
  // subscribes, and it is torn down with the server.
  //
  // `get` IS THE SAME STORE AS `list`, and that identity is the point (issue 05): a row may only be
  // shown once its blob is committed and listed, so the window cannot report a row the durable path
  // does not hold. It reads at most `ROW_TAIL_WINDOW` blobs per poll, bounded by
  // `ROW_TAIL_WINDOW_BYTES`, and re-reads only what is new since the last one.
  const rowTail = new RowTailHub({
    list: (prefix) => store.list(prefix),
    get: (key) => store.get(key),
    ...(caps.maxRuns !== undefined ? { maxRuns: caps.maxRuns } : {}),
    ...(caps.maxSinks !== undefined ? { maxSinks: caps.maxSinks } : {}),
  });
  /**
   * How many row streams each caller is holding open right now — the PER-CLIENT cap the route in
   * front of the hub enforces.
   *
   * IT LIVES ON THE SERVER INSTANCE, not on the hub, because the hub's caps are about the object
   * store (how many prefixes are being LISTed) and this one is about a caller (how many sockets they
   * are holding). Conflating them is how 250 connections from one machine looked like ordinary use:
   * they were 250 different run ids, so every per-run bound in the world would have said yes.
   */
  const rowStreamsPerClient = new Map<string, number>();
  /**
   * How many row streams ONE caller may hold at once.
   *
   * 8 IS ABOVE ANY REAL BROWSER. HTTP/1.1 agents cap themselves at six connections per origin, so a
   * legitimate tab cannot reach this; the reproduction reached 250. Injectable only so a test can
   * prove the cap with two connections instead of nine.
   */
  const ROW_STREAM_MAX_PER_CLIENT = caps.maxPerClient ?? 8;
  // The EFFECTIVE hub caps, so a refusal quotes the number that actually refused rather than the
  // default it was compiled with — an operator debugging a 503 against a printed cap that is not the
  // one in force is being sent to the wrong place.
  const rowRunCap = caps.maxRuns ?? ROW_TAIL_MAX_RUNS;
  const rowSinkCap = caps.maxSinks ?? ROW_TAIL_MAX_SINKS;

  // Stop the row-tail pollers when the server shuts down (its timers are unref'd, but a clean close
  // must not leave a poller LISTing an object store nobody is reading).
  app.addHook('onClose', async () => rowTail.close());

  // Watch a Run's rows land while it is still `open` — its OWN SSE endpoint, ring and cap (live-datasets
  // slice 05), deliberately NOT `/api/events`: that ring is capped for small ordered facts, and one
  // large row scan would evict every state event behind it.
  //
  // ONE SOURCE OF TRUTH: the durable path. The hub LISTs `units/run=<id>/` and reports the object
  // count — one blob is one pushed record — so the number here can never diverge from what is
  // committed the way a producer-side counter would. Server-side and fanned out: two tabs on one Run
  // are two subscribers to one poller and see the same count with the same seq at the same instant,
  // which no per-tab polling can guarantee.
  //
  // SSE, so `EventSource` gives the browser reconnect and `Last-Event-ID` for free; a dropped socket
  // is what the client degrades on (it stops receiving snapshots), which is the "killed stream visibly
  // degrades rather than freezing" half handled on the far side. Hijacked off Fastify's reply pipeline
  // so the compress plugin never buffers the stream. Ungated, like the dataset reads beside it: it
  // carries a count and an mtime, less than the listing it mirrors, and no object-store URL leaves the
  // process.
  //
  // BOUNDED PER CLIENT AND IN TOTAL, which it was not — and the gap was reproduced rather than
  // reported: 250 concurrent streams from one caller, on 250 fictional run ids, with no credential
  // (`.scratch/post-merge-review/TRIAGE-2026-08-25.md` §7). Three caps, because one alone has a
  // hole. {@link ROW_STREAM_MAX_PER_CLIENT} stops one caller holding many; `ROW_TAIL_MAX_RUNS` stops
  // many callers between them watching more Runs than the store should be LISTed for;
  // `ROW_TAIL_MAX_SINKS` stops a flood that puts every connection on the SAME id and so satisfies
  // both of the others. Refusals are status codes an operator can read in a log — 429 for "you have
  // too many", 503 for "this appliance is full" — never a `200` that then goes silent.
  app.get('/api/datasets/rows/stream', (req, reply) => {
    const { run } = req.query as Partial<{ run: string }>;
    if (!run) {
      return reply.code(400).send({ error: 'a run id is required: /api/datasets/rows/stream?run=<id>' });
    }
    // WHO IS ASKING, as well as this process can know it. `req.ip` is the socket's remote address —
    // Fastify's `trustProxy` is off, so no header can forge it. Behind a shared reverse proxy every
    // caller collapses to one address and this degrades into a stricter GLOBAL cap of
    // ROW_STREAM_MAX_PER_CLIENT, which fails closed: the appliance is loopback and single-tenant, and
    // a cap that is too tight is recoverable in a way an unbounded poller farm is not.
    const client = req.ip || 'unknown';
    const held = rowStreamsPerClient.get(client) ?? 0;
    if (held >= ROW_STREAM_MAX_PER_CLIENT) {
      return reply.code(429).send({
        error: `already streaming rows on ${held} connections from ${client} (cap ${ROW_STREAM_MAX_PER_CLIENT}) — close one before opening another`,
      });
    }
    // ASKED BEFORE THE REPLY IS HIJACKED. Once the head is written the only way to refuse is to hang
    // up on a client that was just told `200 text/event-stream`, which reads as a network fault
    // rather than as a limit.
    const refusal = rowTail.refusalFor(run);
    if (refusal) {
      return reply.code(503).send({
        error:
          refusal === 'too-many-runs'
            ? `this appliance is already streaming rows for ${rowTail.activeRuns().length} runs (cap ${rowRunCap}) — try again once one finishes`
            : `too many readers are already watching ${run} (cap ${rowSinkCap})`,
      });
    }
    rowStreamsPerClient.set(client, held + 1);
    const raw = reply.raw;
    reply.hijack();
    raw.writeHead(200, {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache, no-transform',
      Connection: 'keep-alive',
      // Defeat any reverse-proxy response buffering — an SSE stream held until it "finishes" is a
      // frozen page, which is the exact failure this endpoint exists to avoid.
      'X-Accel-Buffering': 'no',
    });
    // A comment line opens the stream so the browser's EventSource fires `onopen` immediately, before
    // the first poll lands — the difference between "connecting" and "connected, zero rows so far".
    raw.write(': ok\n\n');

    // The client echoes the last seq it saw as `Last-Event-ID` on an automatic reconnect; honor it so
    // a blip resumes rather than reseeds.
    const header = req.headers['last-event-id'];
    const lastSeq = Array.isArray(header) ? header[0] : header;
    const parsed = lastSeq !== undefined ? Number.parseInt(lastSeq, 10) : NaN;

    // `let`, not `const`: `subscribe` delivers the seed synchronously (a late subscriber to an active
    // Run is handed the current snapshot before it returns), so the sink can fire before the real
    // unsubscribe is bound. A no-op placeholder makes that seed-time path safe; the socket-close
    // handler is the backstop that always tears the subscription down.
    let unsubscribe = (): void => {};
    unsubscribe = rowTail.subscribe(
      run,
      (event) => {
        // A write to a half-closed socket throws; drop the subscriber rather than crash the poller.
        try {
          raw.write(rowTailFrame(event));
        } catch {
          unsubscribe();
        }
      },
      Number.isFinite(parsed) ? { lastEventId: parsed } : {}
    );

    // A heartbeat comment keeps the connection (and any intermediary idle timer) alive through a Run
    // that is producing nothing right now, and — since a comment is not an event — costs the client
    // no state. It is ALSO how a killed server is noticed on the far side: the beats stop.
    const beat = setInterval(() => {
      try {
        raw.write(': beat\n\n');
      } catch {
        /* closed — the close handler below cleans up */
      }
    }, 15_000);
    if (typeof beat.unref === 'function') beat.unref();

    // IDEMPOTENT, because both `close` and `error` fire on a socket that failed and the per-client
    // counter must not be decremented twice — an under-count is a cap that stops capping, which is
    // the bug this route already had once.
    let torn = false;
    const done = (): void => {
      if (torn) return;
      torn = true;
      clearInterval(beat);
      unsubscribe();
      const now = rowStreamsPerClient.get(client) ?? 0;
      if (now <= 1) rowStreamsPerClient.delete(client);
      else rowStreamsPerClient.set(client, now - 1);
    };
    req.raw.on('close', done);
    req.raw.on('error', done);
  });
}
