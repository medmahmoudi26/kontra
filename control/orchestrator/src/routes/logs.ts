/**
 * `GET /api/logs/{query,tail,hits}` — VictoriaLogs, reached ONLY through here (ADR 0050, issue #17).
 *
 * ── WHY A PROXY AND NOT A URL THE CONSOLE FETCHES ───────────────────────────────────────────────
 *
 * **VictoriaLogs has no authentication.** Its multi-tenancy is an `AccountID`/`ProjectID` REQUEST
 * HEADER, which means the header is an authorisation decision made by whoever sets it. A console
 * talking to it directly would be a console that can read any tenant by changing a number — and the
 * number is not a secret, it is an integer.
 *
 * So the console never talks to it. This process does, and it stamps the tenant header ITSELF from
 * the session. That reuses an enforced boundary (ADR 0045) rather than inventing one, and it is why
 * `docker-compose.yml` publishes no host port for `victorialogs`: the binding is the control, not a
 * firewall rule — the 2026-09-12 incident is the whole argument, and `docker bypasses ufw` is why a
 * rule would not have helped.
 *
 * ── THE TENANT IS NOT NEGOTIABLE, AND NEITHER IS THE QUERY'S SCOPE ──────────────────────────────
 *
 * Two separate things have to be true, and only the first is obvious:
 *
 *   1. A client-supplied `AccountID`/`ProjectID` is STRIPPED, never forwarded. Stripping rather than
 *      rejecting, because a browser that sends one is far more likely to be a proxy adding headers
 *      than an attack, and a 400 there is a console that mysteriously stops working.
 *   2. A caller-supplied **LogsQL query cannot widen past its own tenant.** This is the one that is
 *      easy to miss: the tenant header scopes the STORE, but a query is still arbitrary LogsQL, and
 *      the console sends it verbatim. The scoping is done by CONJUNCTION — the caller's query is
 *      wrapped, never concatenated — so there is no operator precedence for a caller to exploit and
 *      no quoting for them to escape.
 *
 * ── SSE FOR `tail`, THE SAME SHAPE `runStream.ts` ALREADY USES ──────────────────────────────────
 *
 * One-directional, server has state, browser draws it. Reuses `STREAM_HEADERS` rather than
 * re-deciding them, so a buffering proxy cannot turn one route into a very late response while the
 * other streams.
 */

import type { FastifyInstance, FastifyReply, FastifyRequest } from 'fastify';

import { EXPLORE_TOKEN_VARS, checkBearer } from '../auth';
import { STREAM_HEADERS } from './runStream';

/** In-compose, unpublished. The same shape `panels/metrics.ts` states for the metrics store. */
export const DEFAULT_LOGS_URL = 'http://victorialogs:9428';

export function logsBase(): string {
  return process.env.KONTRA_LOGS_URL || DEFAULT_LOGS_URL;
}

/** How long a one-shot read may take. A wedged backend must cost the logs pane, never the API. */
const QUERY_TIMEOUT_MS = 10_000;

/**
 * The headers that decide WHICH tenant's logs these are, set here and nowhere else.
 *
 * A Tenant IS a Temporal namespace (ADR 0036 §7) and this deployment serves one, so the account is
 * derived from `KONTRA_NAMESPACE` — the same derivation `temporalClient.ts:tenantOf` makes for a
 * Run's tenant, rather than a second notion of one. When kontra serves more than one namespace this
 * is the line that changes, and it changes in ONE place.
 */
export function tenantHeaders(): Record<string, string> {
  // `AccountID` is numeric in VictoriaLogs. A named namespace has no number, so single-tenant
  // installs sit on account 0 and the namespace travels as a STREAM FIELD on the records
  // themselves (see #16) — which is what a LogsQL query can actually filter on.
  return { AccountID: '0', ProjectID: '0' };
}

/**
 * The caller's LogsQL, scoped so it cannot reach past this tenant.
 *
 * BY CONJUNCTION, NOT CONCATENATION. `(caller) AND tenant:"x"` would still be string-built; this
 * wraps the caller's whole expression in parentheses and ANDs the scope on, so precedence cannot be
 * subverted by a trailing `OR` and there is no quote for a caller to close. An empty query is `*`,
 * which is LogsQL for everything — everything IN SCOPE, which is the point.
 */
export function scopedQuery(caller: string | undefined, tenant: string): string {
  const q = (caller ?? '').trim() || '*';
  return `(${q}) AND _stream:{tenant=${JSON.stringify(tenant)}}`;
}

/** The namespace this control plane serves, which is the tenant — see `tenantHeaders`. */
export function tenant(): string {
  return process.env.KONTRA_NAMESPACE ?? 'default';
}

/** Strip anything the caller sent that would decide authorisation for us. */
function safeHeaders(extra: Record<string, string> = {}): Record<string, string> {
  return { ...extra, ...tenantHeaders() };
}

/**
 * A sentence, not an empty list.
 *
 * A logs view that renders nothing when the BACKEND is unreachable is indistinguishable from a Run
 * that logged nothing — and one of those is a missing feature while the other is a missing
 * dependency. This repository has the same rule for the Monitor wall and for the `loads` chip.
 */
function unreachable(reply: FastifyReply, err: unknown): void {
  const detail = String((err as Error)?.message ?? err);

  // OUR OWN TIMEOUT IS NOT THE BACKEND BEING DOWN, and saying so sends an operator to the wrong
  // place with confidence.
  //
  // `AbortSignal.timeout(QUERY_TIMEOUT_MS)` and VictoriaLogs' `-search.maxQueueDuration` are both
  // ten seconds and both start at the same instant, so when the select pool is full it is a coin
  // toss which fires. When ours wins, the AbortError lands in the same `catch` as ECONNREFUSED
  // and gets the sentence below — every clause of which is wrong: the backend is reachable,
  // healthy, `docker compose ps` shows it up, and it was 0.5s from answering. MEASURED: a browser
  // got this 503 while curl answered the identical query in 0.5s at the same moment, with
  // `vl_concurrent_select_current 3` of a capacity of 4.
  //
  // Raising the pool to 64 removed the trigger; it did not remove this. A pool full at ANY size
  // still produces the wrong sentence, and a saturated backend is exactly when an operator most
  // needs to be pointed at the right thing.
  const ourTimeout =
    (err as Error)?.name === 'TimeoutError' ||
    (err as Error)?.name === 'AbortError' ||
    /aborted due to timeout/i.test(detail);

  if (ourTimeout) {
    reply.code(504).send({
      error:
        `the logs backend did not answer within ${QUERY_TIMEOUT_MS / 1000}s — but this is OUR ` +
        `deadline expiring, NOT the backend being down. The usual cause is a full select pool: a ` +
        `live tail holds one slot for as long as it is open, so queries queue behind them. Check ` +
        `\`vl_concurrent_select_current\` against \`vl_concurrent_select_capacity\` on ` +
        `${logsBase()}/metrics before restarting anything. (${detail})`,
    });
    return;
  }

  reply.code(503).send({
    error:
      `the logs backend is not answering at ${logsBase()} — this is not "no logs". ` +
      `It is in docker-compose.yml as \`victorialogs\` and is deliberately not published to the ` +
      `host; check \`docker compose ps victorialogs\`. (${detail})`,
  });
}

export function registerLogsRoutes(app: FastifyInstance): void {
  /**
   * Admission: the same token the query workbench takes, and `checkBearer` — FAIL-CLOSED.
   *
   * Reading a Run's logs and reading its Datasets are the same privilege over the same Runs, so a
   * third token here would be one more thing to rotate for no boundary gained; a console session
   * admits both. FAIL-CLOSED because of what is IN a log line: the threat model says a Run's record
   * "routinely contains targets and sometimes secrets", and that is at least as true of the lines a
   * scanner writes as of the history it writes. With no token configured this surface answers 503
   * rather than opening, exactly as `explore.ts` does.
   */
  const admit = (req: FastifyRequest, reply: FastifyReply): boolean => {
    const fail = checkBearer(req.headers.authorization, EXPLORE_TOKEN_VARS);
    if (fail) {
      reply.code(fail.code).send(fail.body);
      return false;
    }
    return true;
  };

  /** One-shot LogsQL. Streams JSON lines back exactly as VictoriaLogs produced them. */
  app.get('/api/logs/query', async (req, reply) => {
    if (!admit(req, reply)) return;
    const q = (req.query as { query?: string; limit?: string }) ?? {};
    const body = new URLSearchParams({
      query: scopedQuery(q.query, tenant()),
      limit: String(Math.min(Number(q.limit ?? 200) || 200, 1000)),
    });
    try {
      const res = await fetch(`${logsBase()}/select/logsql/query`, {
        method: 'POST',
        headers: safeHeaders({ 'Content-Type': 'application/x-www-form-urlencoded' }),
        body,
        signal: AbortSignal.timeout(QUERY_TIMEOUT_MS),
      });
      reply.code(res.status).type('application/stream+json').send(await res.text());
    } catch (err) {
      unreachable(reply, err);
    }
  });

  /** Density over a window, for the bar above the log list. */
  app.get('/api/logs/hits', async (req, reply) => {
    if (!admit(req, reply)) return;
    const q = (req.query as { query?: string; step?: string; start?: string }) ?? {};
    const body = new URLSearchParams({
      query: scopedQuery(q.query, tenant()),
      step: q.step ?? '5m',
      start: q.start ?? '1h',
    });
    try {
      const res = await fetch(`${logsBase()}/select/logsql/hits`, {
        method: 'POST',
        headers: safeHeaders({ 'Content-Type': 'application/x-www-form-urlencoded' }),
        body,
        signal: AbortSignal.timeout(QUERY_TIMEOUT_MS),
      });
      reply.code(res.status).type('application/json').send(await res.text());
    } catch (err) {
      unreachable(reply, err);
    }
  });

  /**
   * Live tail, hijacked as SSE.
   *
   * VictoriaLogs answers `/select/logsql/tail` with an OPEN stream of JSON lines; this forwards each
   * line as one SSE frame. Hijacked for the reason `runStream.ts` hijacks: Fastify wants to own the
   * response lifecycle and a long-lived stream is exactly the case where it must not.
   */
  app.get('/api/logs/tail', (req, reply) => {
    if (!admit(req, reply)) return;
    const q = (req.query as { query?: string }) ?? {};
    reply.hijack();
    const controller = new AbortController();
    req.raw.on('close', () => controller.abort());

    // ── THE HEAD GOES OUT BEFORE THE UPSTREAM IS ASKED, AND THAT ORDER IS THE FIX ───────────────
    //
    // This used to `await fetch(...)` and only then `writeHead`. VictoriaLogs' `/select/logsql/tail`
    // withholds its OWN headers until it has a line to send, so on a quiet fleet this route sent
    // zero bytes — no status, no headers — for as long as nothing was logged. MEASURED with curl
    // against an idle fleet: 20 seconds, not one byte of header.
    //
    // A browser's `fetch()` does not resolve until headers arrive, so the client sat in
    // "connecting" forever — ABOVE a few hundred successfully backfilled lines. A working backend
    // rendering as a broken connection is precisely the confusion `unreachable()` below exists to
    // prevent, produced by the one route that streams.
    //
    // THE CONTRACT CHANGE THIS MAKES, STATED PLAINLY: a caller that used to see 502 for an
    // unreachable backend now sees 200 followed by `event: error`. That is not a regression, it is
    // what committing to a stream MEANS — once the head is written the status is spent, and every
    // SSE API reports late failures in-band for the same reason. The error frame carries the same
    // sentence the status used to.
    reply.raw.writeHead(200, STREAM_HEADERS);
    // A COMMENT FRAME, SO SOMETHING ARRIVES IMMEDIATELY. Headers alone can sit in a proxy buffer;
    // a first byte flushes the chain and gives the client a positive "the stream is open" signal
    // distinct from "still waiting to hear".
    reply.raw.write(': open\n\n');

    void (async () => {
      try {
        const res = await fetch(`${logsBase()}/select/logsql/tail`, {
          method: 'POST',
          headers: safeHeaders({ 'Content-Type': 'application/x-www-form-urlencoded' }),
          body: new URLSearchParams({ query: scopedQuery(q.query, tenant()) }),
          signal: controller.signal,
        });
        if (!res.ok || !res.body) {
          reply.raw.write(`event: error\ndata: ${JSON.stringify({ error: 'logs backend refused the tail' })}\n\n`);
          reply.raw.end();
          return;
        }
        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        let buf = '';
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += decoder.decode(value, { stream: true });
          // LINE-DELIMITED, AND THE PARTIAL TAIL IS KEPT. A chunk boundary falls mid-record often
          // enough that dropping the remainder loses lines only under load — the worst way to lose
          // them, because it looks like quiet.
          const lines = buf.split('\n');
          buf = lines.pop() ?? '';
          for (const line of lines) {
            if (line.trim()) reply.raw.write(`event: log\ndata: ${line}\n\n`);
          }
        }
        reply.raw.end();
      } catch (err) {
        if (controller.signal.aborted) return; // the browser left; not an error
        try {
          // NO `writeHead` HERE ANY MORE. The head went out before the upstream was contacted
          // (see above), so writing it again throws ERR_HTTP_HEADERS_SENT — which lands in the
          // empty `catch` below and loses the only explanation the client was going to get. The
          // frame IS the report now.
          reply.raw.write(
            `event: error\ndata: ${JSON.stringify({ error: `logs backend unreachable: ${String((err as Error)?.message ?? err)}` })}\n\n`
          );
          reply.raw.end();
        } catch {
          /* socket already gone */
        }
      }
    })();
  });
}
