/**
 * The secret store's HTTP surface: five management routes that can never return a value, and one
 * resolution route that can — for exactly one caller, about exactly one secret.
 *
 * THE MANAGEMENT ROUTES HAVE NO READ PATH FOR THE VALUE. Not "the value is filtered out here" —
 * there is no code in this file that could produce one: they call `list`, `describe`, `put`,
 * `revoke` and `destroy`, and none of those returns bytes (`types.ts:SecretBackend`). The only
 * call to `SecretStore.resolve` in this file is in `POST /api/secrets/resolve`, which is one grep
 * away for any reviewer, and `routes.test.ts` beside this file asserts a sentinel value appears in
 * NO other response body, error body or header.
 *
 * ── ADMISSION, AND WHY THE THREE POSTURES DIFFER ──────────────────────────────────────────────
 *
 * `auth.ts` records that most of this API is unauthenticated, and this surface does not pretend to
 * fix that. What it does is set each posture from what the route can actually do:
 *
 *  - **Management is OPT-IN (`checkOptionalBearer`)** — open when no token is configured, enforced
 *    when one is, exactly like the serve/start control surface. Fail-closed was the first
 *    instinct and it is the wrong one HERE: a store nobody can reach on a default install means
 *    credentials stay in `.env`, which is the failure this slice exists to end. What bounds the
 *    damage is the store's own property rather than the door — an unauthenticated caller on the
 *    loopback API can write and destroy secrets, and cannot read one.
 *  - **Minting an actor identity is FAIL-CLOSED (`checkBearer`)** — it hands out the ability to
 *    resolve that actor's secrets, so it is the one route that must not be open by default. With
 *    no token configured it 503s and says which variable to set.
 *  - **Resolution takes NO operator token at all.** It is authenticated by the actor's signed
 *    identity (`identity.ts`) and authorised by ownership (`store.ts:resolve`). An operator token
 *    would be a second way in to the same bytes, held by more processes.
 */

import type { FastifyInstance, FastifyReply } from 'fastify';

import { checkBearer, checkOptionalBearer, type AuthFailure } from '../auth';
import type { SecretStore } from './store';
import { NoSuchSecret, SecretForbidden, SecretRefused, SecretRevoked } from './types';

/**
 * Managing secrets accepts the state token — the narrower of the two, never the explore token,
 * for the reason `infraRoutes.ts` gives: the explore token is handed out for the workbench, and
 * reading a dataset is not the same privilege as writing the credential a fleet is built with.
 * `KONTRA_SECRETS_TOKEN` is first so an operator can scope this surface on its own.
 */
export const SECRETS_TOKEN_VARS = ['KONTRA_SECRETS_TOKEN', 'KONTRA_STATE_TOKEN'] as const;

/** The bearer prefix an actor sends its identity token under. */
const BEARER = 'Bearer ';

export function registerSecretRoutes(app: FastifyInstance, store: SecretStore): void {
  const denyManage = (auth: string | undefined): AuthFailure | null =>
    checkOptionalBearer(auth, SECRETS_TOKEN_VARS);

  /** What Settings draws: the names, their versions, and where they rest. Never a value. */
  app.get('/api/secrets', async (req, reply) => {
    const denied = denyManage(req.headers.authorization);
    if (denied) return reply.code(denied.code).send(denied.body);
    return {
      backend: store.backendKind,
      location: store.location,
      secrets: await store.list(),
    };
  });

  app.get('/api/secrets/:name', async (req, reply) => {
    const denied = denyManage(req.headers.authorization);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { name } = req.params as { name: string };
    try {
      const found = await store.describe(name);
      if (!found) return reply.code(404).send({ error: `no secret named ${JSON.stringify(name)}` });
      return found;
    } catch (err) {
      return fail(reply, err);
    }
  });

  /**
   * Write a secret: create it, or rotate it by adding a version. ONE VERB FOR BOTH, so there is no
   * call that overwrites a version in place — see `fileBackend.ts:put`.
   *
   * The response says which version this became and whether the value was trimmed, because those
   * are the only two facts an operator can use to check that what they meant is what is stored,
   * and they will never see the value again.
   */
  app.put('/api/secrets/:name', async (req, reply) => {
    const denied = denyManage(req.headers.authorization);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { name } = req.params as { name: string };
    const body = (req.body ?? {}) as { value?: unknown; owner?: unknown };
    if (typeof body.value !== 'string') {
      return reply.code(400).send({ error: 'a secret write needs a string `value`' });
    }
    try {
      const written = await store.put(name, body.value, {
        ...(typeof body.owner === 'string' && body.owner ? { owner: body.owner } : {}),
      });
      // The VALUE is never logged; the act is. An operator reconstructing an incident needs to know
      // a rotation happened at 04:12, and the only field that could leak is the one omitted.
      req.log?.info?.({ secret: name, version: written.version, rotated: written.rotated }, 'secret written');
      return reply.code(written.rotated ? 200 : 201).send({
        secret: written.secret,
        version: written.version,
        rotated: written.rotated,
        trimmed: body.value !== body.value.trim(),
      });
    } catch (err) {
      return fail(reply, err);
    }
  });

  /** Make one version unusable. The file backend destroys its ciphertext — this is not a flag. */
  app.post('/api/secrets/:name/versions/:version/revoke', async (req, reply) => {
    const denied = denyManage(req.headers.authorization);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { name, version } = req.params as { name: string; version: string };
    try {
      const secret = await store.revoke(name, Number(version));
      req.log?.info?.({ secret: name, version: Number(version) }, 'secret version revoked');
      return { secret };
    } catch (err) {
      return fail(reply, err);
    }
  });

  /** Forget a secret and every version of it. */
  app.delete('/api/secrets/:name', async (req, reply) => {
    const denied = denyManage(req.headers.authorization);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { name } = req.params as { name: string };
    try {
      const destroyed = await store.destroy(name);
      if (!destroyed) return reply.code(404).send({ error: `no secret named ${JSON.stringify(name)}` });
      req.log?.info?.({ secret: name }, 'secret destroyed');
      return { destroyed: true, name };
    } catch (err) {
      return fail(reply, err);
    }
  });

  /**
   * Mint an actor identity token — FAIL-CLOSED, see the header.
   *
   * The console does not need this: when it serves an actor it mints one in-process and puts it in
   * the worker's environment (`actorControl.ts`). This route is for the worker nobody here started
   * — a `kontra serve` in somebody's terminal, and later a fleet **Machine** — which otherwise has
   * no way to be anybody. The token is shown ONCE, in this response, and is not stored: it is an
   * HMAC, so there is nothing to store.
   */
  app.post('/api/secrets/identity', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, SECRETS_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const body = (req.body ?? {}) as { actor?: unknown; ttlSeconds?: unknown };
    if (typeof body.actor !== 'string' || !body.actor) {
      return reply.code(400).send({ error: 'minting an identity needs an `actor` name' });
    }
    try {
      const ttl = typeof body.ttlSeconds === 'number' && body.ttlSeconds > 0 ? body.ttlSeconds : undefined;
      const minted = store.mintIdentity(body.actor, ttl);
      req.log?.info?.({ identity: minted.identity, expiresAt: minted.expiresAt }, 'actor identity minted');
      return minted;
    } catch (err) {
      return fail(reply, err);
    }
  });

  /**
   * THE ACTOR'S OWN FETCH — the one route in this file that answers with a value.
   *
   * It is the last hop for the actor path: the value goes from here into the process that will use
   * it, and touches no workflow argument, no activity argument and no **Batch** on the way. The
   * actor is whoever its signed token says it is, and it may resolve only what it OWNS — an
   * operator secret is refused here even to a valid identity (`store.ts:resolve`).
   */
  app.post('/api/secrets/resolve', async (req, reply) => {
    const header = req.headers.authorization ?? '';
    if (!header.startsWith(BEARER)) {
      return reply.code(401).send({ error: 'an actor identity token is required to resolve a secret' });
    }
    const body = (req.body ?? {}) as { name?: unknown; version?: unknown };
    if (typeof body.name !== 'string' || !body.name) {
      return reply.code(400).send({ error: 'resolving a secret needs a `name`' });
    }
    // VERIFYING IS ITS OWN STEP AND ITS OWN STATUS. A bad or expired token is 401 — "be somebody
    // else" — while a valid identity asking for a secret that is not its own is 403 — "not you".
    // Collapsing them into one code is how an actor whose token quietly expired gets diagnosed as
    // an ownership problem, which sends an operator to the wrong fix.
    let identity: string;
    try {
      identity = store.verifyIdentity(header.slice(BEARER.length));
    } catch (err) {
      return reply.code(401).send({ error: err instanceof Error ? err.message : 'unauthorized' });
    }
    try {
      const version = typeof body.version === 'number' ? body.version : undefined;
      const got = await store.resolve(
        { name: body.name, ...(version ? { version } : {}) },
        { kind: 'actor', identity }
      );
      // THE AUDIT LINE FOR EVERY RESOLUTION: who, what, which version, and never the value. This is
      // the record that answers "what did that actor have access to" after an incident.
      req.log?.info?.({ identity, secret: body.name, version: got.version }, 'secret resolved');
      return { name: body.name, version: got.version, value: got.value };
    } catch (err) {
      return fail(reply, err);
    }
  });
}

/**
 * Domain error → status. NOTHING HERE INTERPOLATES A VALUE, and it cannot: the errors it maps are
 * thrown by code that never holds one at the point it throws (`store.ts`, `fileBackend.ts`).
 */
function fail(reply: FastifyReply, err: unknown): FastifyReply {
  if (err instanceof SecretRefused) return reply.code(400).send({ error: err.message });
  if (err instanceof NoSuchSecret) return reply.code(404).send({ error: err.message });
  if (err instanceof SecretRevoked) return reply.code(410).send({ error: err.message });
  if (err instanceof SecretForbidden) return reply.code(403).send({ error: err.message });
  return reply.code(500).send({ error: err instanceof Error ? err.message : String(err) });
}
