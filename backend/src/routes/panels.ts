/**
 * THE PANELS PROXY — the two same-origin reads that keep the Dashboard's credential out of the SPA.
 *
 * Neither route serves a panel. Both forward to the streamer (`orchestrator-infra`, port 8090)
 * carrying `KONTRA_PANEL_TOKEN` from THIS process's environment, so the browser POSTs here with no
 * credential and the WebSocket that follows carries only the minted ticket.
 *
 * WHAT IT NEEDS: nothing injected. Both the token and the streamer's base URL are read from the
 * environment PER REQUEST, not captured at registration, so an operator who sets
 * `KONTRA_PANEL_TOKEN` and restarts only the streamer does not also have to restart this process to
 * have it noticed.
 *
 * THE STATUS IS PASSED THROUGH UNCHANGED. 401 and 503 send an operator to different machines, and
 * collapsing them here would undo a distinction `cli/panels.go` relies on. The one status this file
 * invents is the 502, which exists because the streamer is a FORKED CHILD of `orchestrator-infra`:
 * that container can be healthy while the child is down, so "cannot reach it" has to be its own
 * diagnosis rather than a generic gateway error.
 *
 * WHAT THIS DOES NOT DO, stated because the difference matters: it does not create authentication
 * where this API has none. Anyone who can reach `:8088` can still mint a ticket, exactly as anyone
 * who could reach it could previously read the token baked into the bundle by
 * `VITE_KONTRA_PANEL_TOKEN`. What it removes is the DURABLE leak — a secret shipped inside an
 * artifact, which outlives the container, travels with the image, and cannot be rotated without a
 * rebuild. Closing the rest means putting this API behind auth, which `auth.ts` records as an open
 * gap for the whole surface, not a panels problem.
 */

import type { FastifyInstance, FastifyReply } from 'fastify';

import { errMessage } from './errors';

export function registerPanelRoutes(app: FastifyInstance): void {
  /** Both panel reads go through one function so the token can only ever be read in one place. */
  const proxyPanels = async (
    req: { body?: unknown },
    reply: FastifyReply,
    path: string,
    method: 'GET' | 'POST'
  ): Promise<unknown> => {
    const token = process.env.KONTRA_PANEL_TOKEN;
    if (!token) {
      // Fail closed, and say which side is unconfigured: the streamer 503s for the same reason with
      // its own message, and an operator chasing a blank Dashboard needs to know which process to fix.
      return reply
        .code(503)
        .send({ error: 'disabled: set KONTRA_PANEL_TOKEN on orchestrator-api to mint Dashboard tickets' });
    }
    const base = process.env.KONTRA_PANEL_URL ?? 'http://orchestrator-infra:8090';
    try {
      const res = await fetch(`${base}${path}`, {
        method,
        headers: { authorization: `Bearer ${token}`, 'content-type': 'application/json' },
        body: method === 'POST' ? JSON.stringify(req.body ?? {}) : undefined,
        signal: AbortSignal.timeout(5_000),
      });
      const text = await res.text();
      // Pass the streamer's status through unchanged — 401 vs 503 send an operator to different
      // machines, and collapsing them here would undo the distinction `cli/panels.go` relies on.
      return reply.code(res.status).type(res.headers.get('content-type') ?? 'application/json').send(text);
    } catch (err) {
      // The streamer is a forked child of orchestrator-infra: that container can be healthy while
      // the child is down, so this is its own diagnosis rather than a generic 502.
      return reply.code(502).send({
        error: `cannot reach the panels streamer at ${base}: ${errMessage(err)} — it is a forked child of orchestrator-infra, so the container being up does not mean the child is`,
      });
    }
  };

  // The two panel reads the SPA needs. Both same-origin and credential-free from the browser's side;
  // the WebSocket that follows carries the minted ticket and nothing else, straight to the streamer.
  app.post('/api/panels/ticket', (req, reply) => proxyPanels(req, reply, '/api/panels/ticket', 'POST'));
  app.get('/api/panels/terminals', (req, reply) =>
    proxyPanels(req, reply, '/api/panels/terminals', 'GET')
  );

  // THE THIRD ROUTE IS NOT FOR THE BROWSER. A **Warden** on a **Machine** dials this to deliver its
  // panes and telemetry (ADR 0037), and it goes through the same proxy for one reason: the streamer
  // is bound to the Controller's loopback (`docker-compose.yml` publishes `127.0.0.1:8090:8090`),
  // and this API is the only surface a fleet Machine can reach. So the Machine posts to :8088 and
  // the token this process holds is what admits it to :8090.
  //
  // WHICH MEANS THE MACHINE'S OWN IDENTITY IS NOT WHAT AUTHENTICATES THE REPORT, and that gap is
  // named in `panels/warden.ts` rather than left to be found: ADR 0037 has the Controller deriving a
  // Machine's identity from the certificate its handshake proved, which happens on the mTLS
  // enrolment server and not here. Anyone who can reach :8088 can file a report — the same standing
  // exposure `auth.ts` records for this whole API, now reaching one route more.
  app.post('/api/panels/report', (req, reply) => proxyPanels(req, reply, '/api/panels/report', 'POST'));
}
