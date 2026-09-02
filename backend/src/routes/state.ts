/**
 * RAW STATE INSPECTION — one route, read-only, token-gated.
 *
 * The three state tiers, for an operator or the MCP tool; the orchestrator holds the Redis
 * credential so no reader ever does. This and the infra routes are the AUTHENTICATED ROUTES on this
 * API — the rest predate this work and are open, which is recorded in
 * `.scratch/federated-grafana/FINDINGS.md` rather than quietly inherited. A token here is not a
 * substitute for that; it just means this surface is not itself a new way to read live scan data
 * unauthenticated.
 *
 * WHAT IT NEEDS: nothing injected, and that is deliberate. THE READER IS BUILT ON FIRST READ, NOT
 * AT BOOT — the API must start with no Redis running, the same way it starts with no Temporal
 * running — so the lazy handle lives in this module and `buildServer` never learns Redis exists.
 * `createStateReader` returning nothing is a 503 that names the variable, not a crash.
 *
 * Bounded by construction: identity is mandatory, the glob is rooted at a literal app-id prefix,
 * the key walk is capped, the response is byte-capped, and credential-shaped keys are redacted by
 * name. The audit line logs KEYS ONLY, never values.
 */

import type { FastifyInstance } from 'fastify';

import { STATE_TOKEN_VARS, checkBearer } from '../auth';
import { STATE_TIERS, type StateTier, requiresEntity } from '../state';
import { type StateReader, createStateReader } from '../stateStore';
import { errMessage } from './errors';

export function registerStateRoutes(app: FastifyInstance): void {
  // Built on first state read, not at boot: the API must start with no Redis running, the same
  // way it starts with no Temporal running.
  let stateReader: StateReader | null = null;

  app.get('/api/state/:tier', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, STATE_TOKEN_VARS);
    if (denied) {
      if (denied.code === 401) req.log?.warn?.({ path: req.url, ip: req.ip }, 'state: rejected');
      return reply.code(denied.code).send(denied.body);
    }

    const { tier } = req.params as { tier: string };
    if (!STATE_TIERS.includes(tier as StateTier)) {
      return reply.code(400).send({ error: `unknown tier '${tier}'; want one of ${STATE_TIERS.join('|')}` });
    }
    const t = tier as StateTier;
    const { actor, entity = '', key } = req.query as Partial<{
      actor: string;
      entity: string;
      key: string;
    }>;
    if (!actor) return reply.code(400).send({ error: 'requires actor query param' });
    if (requiresEntity(t) && !entity) {
      return reply
        .code(400)
        .send({ error: `tier '${t}' requires an entity query param (the actor id: the Session's key, else its id)` });
    }

    const reader = stateReader ?? (stateReader = createStateReader());
    if (!reader) {
      return reply.code(503).send({ error: 'state inspection disabled: KONTRA_REDIS_HOST unset' });
    }

    try {
      const r = await reader.read(t, actor, entity, key);
      // Audit: who asked for whose state, and how much came back. Keys only, never values.
      req.log?.info?.(
        { ip: req.ip, tier: t, actor, entity, keys: r.total, truncated: r.truncated },
        'state: read'
      );
      return {
        tier: t,
        actor,
        entity,
        // Freshness is explicit on every response so a dashboard can label a snapshot's age
        // instead of implying it is live. Nothing else in this API carries one.
        captured_at: r.capturedAt,
        source: r.source,
        truncated: r.truncated,
        total: r.total,
        entries: r.entries,
      };
    } catch (err) {
      return reply.code(502).send({ error: `could not read state: ${errMessage(err)}` });
    }
  });
}
