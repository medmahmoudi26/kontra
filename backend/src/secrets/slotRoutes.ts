/**
 * The slot surface's HTTP routes: what an actor DECLARES, what an operator BINDS, what a run is
 * preflighted against, and the ledger of every resolution.
 *
 * ── THE ONE ROUTE THAT ANSWERS WITH A VALUE ───────────────────────────────────────────────────
 *
 * `POST /api/slots/resolve`, and it is one grep from any reviewer, exactly as
 * `routes.ts`'s own resolve is. Everything else here reads names, states and dates. The resolve
 * route ANSWERS WITH THE SLOT AND THE VALUE AND NOTHING ELSE — no secret name, no secret version,
 * no owner — because telling an actor which of the operator's secrets answered would hand back the
 * inventory the indirection exists to withhold.
 *
 * ── THE THREE ADMISSION POSTURES, WHICH ARE NOT THE SAME AS `routes.ts`'s ─────────────────────
 *
 *  - **Binding, unbinding, reading the states and reading the ledger are OPT-IN**
 *    (`checkOptionalBearer`, the same `SECRETS_TOKEN_VARS`): open when no token is configured,
 *    enforced when one is. Same posture as managing the secrets themselves, because a binding is
 *    the operator's half of a credential grant and belongs behind the same door as the credential.
 *  - **DECLARING TAKES NO TOKEN AT ALL**, matching `POST /api/actors`, which is how a worker
 *    registers what it is. This is worth stating rather than assuming, because "anyone on the
 *    loopback API can declare a slot" sounds like an escalation and is not: a declaration GRANTS
 *    NOTHING. It says what an actor will ask for; the answer to that ask is the operator's
 *    binding, and reaching a binding needs that actor's own signed identity. The worst a forged
 *    declaration does is make a page list a slot nobody asks for, or drop one — the same exposure
 *    the catalog already has for the schemas beside it, and it fails CLOSED (a dropped declaration
 *    makes resolution refuse, never succeed).
 *  - **Resolving takes the ACTOR's identity token and no operator token**, for `routes.ts`'s
 *    reason: an operator token would be a second way to the same bytes, held by more processes.
 *
 * ── WHY `/api/slots` AND NOT `/api/secrets/slots` ────────────────────────────────────────────
 *
 * Fastify's router prefers a STATIC segment over a parameterised one, so `/api/secrets/slots`
 * would have shadowed `GET /api/secrets/:name` for a secret an operator called `slots` — legal
 * under `store.ts:SECRET_NAME_RE`, unreachable afterwards, and silent about it. The same trap
 * exists one level down: `/api/slots/:actor` beside `/api/slots/declare` makes an actor named
 * `declare` unreadable. So every parameterised path here sits under its own static segment
 * (`/api/slots/actor/:actor/:slot`), and no name in either alphabet can collide with a verb.
 */

import type { FastifyInstance, FastifyReply } from 'fastify';

import { checkOptionalBearer, type AuthFailure } from '../auth';
import { SECRETS_TOKEN_VARS } from './routes';
import { SlotRefused, SlotUndeclared, SlotUnbound, type ActorRef, type SlotSpec } from './slots';
import type { SlotStore } from './slotStore';
import type { SecretStore } from './store';
import { NoSuchSecret, SecretForbidden, SecretRefused, SecretRevoked } from './types';

/** The bearer prefix an actor sends its identity token under. Same as `routes.ts`'s. */
const BEARER = 'Bearer ';

export function registerSlotRoutes(app: FastifyInstance, secrets: SecretStore, slots: SlotStore): void {
  const denyManage = (auth: string | undefined): AuthFailure | null =>
    checkOptionalBearer(auth, SECRETS_TOKEN_VARS);

  /**
   * Everything Settings' binding view draws: every actor that has declared, the newest version's
   * slots joined against the store, and every grant — including grants for slots no declared
   * version asks for, which the page shows as unused rather than hiding.
   */
  app.get('/api/slots', async (req, reply) => {
    const denied = denyManage(req.headers.authorization);
    if (denied) return reply.code(denied.code).send(denied.body);
    return {
      location: slots.location,
      actors: await slots.views(),
      bindings: slots.bindings(),
      // The NAMES an operator can bind to — the same list `GET /api/secrets` returns, reduced to
      // what a picker needs. Owned secrets are included: they are bindable, to their owner's slots.
      secrets: (await secrets.list()).map((s) => ({
        name: s.name,
        ...(s.owner ? { owner: s.owner } : {}),
        usable: s.current !== undefined,
      })),
    };
  });

  /** One actor's slots, for the Actors page. `?version=` pins a build; the default is the newest
   *  that has declared, which is the one the card is showing. */
  app.get('/api/slots/actor/:actor', async (req, reply) => {
    const denied = denyManage(req.headers.authorization);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { actor } = req.params as { actor: string };
    const { version } = (req.query ?? {}) as { version?: string };
    try {
      return await slots.view(decodeURIComponent(actor), version || undefined);
    } catch (err) {
      return fail(reply, err);
    }
  });

  /**
   * A worker declaring what this `(actor, version)` asks for. NO OPERATOR TOKEN — see the header.
   *
   * The response is the joined view, so the worker that just declared can print what it will and
   * will not be able to resolve. That is the whole of the "visible before it runs" property from
   * the side that can act on it fastest: an operator serving an actor for the first time is told,
   * in the pane they are already watching, which credentials they now have to bind.
   */
  app.post('/api/slots/declare', async (req, reply) => {
    const body = (req.body ?? {}) as { actor?: unknown; version?: unknown; slots?: unknown };
    if (typeof body.actor !== 'string' || !body.actor) {
      return reply.code(400).send({ error: 'a slot declaration needs an `actor` name' });
    }
    if (typeof body.version !== 'string' || !body.version) {
      return reply.code(400).send({ error: 'a slot declaration needs the actor `version` it is for' });
    }
    if (!Array.isArray(body.slots)) {
      return reply.code(400).send({ error: 'a slot declaration needs a `slots` array' });
    }
    const specs: SlotSpec[] = [];
    for (const raw of body.slots) {
      // A bare string is accepted beside `{name, description}` because that is what a slot with no
      // author sentence looks like on the wire, and refusing it would make the terse form the one
      // that silently registers nothing.
      if (typeof raw === 'string') specs.push({ name: raw });
      else if (raw && typeof raw === 'object' && typeof (raw as SlotSpec).name === 'string') {
        const spec = raw as SlotSpec;
        specs.push({
          name: spec.name,
          ...(typeof spec.description === 'string' ? { description: spec.description } : {}),
        });
      } else {
        return reply.code(400).send({ error: 'each slot is a name, or `{ "name": …, "description": … }`' });
      }
    }
    try {
      const view = await slots.declare(body.actor, body.version, specs);
      req.log?.info?.(
        { actor: body.actor, version: body.version, slots: specs.map((s) => s.name) },
        'actor slots declared'
      );
      return view;
    } catch (err) {
      return fail(reply, err);
    }
  });

  /** Bind a slot to one of the operator's secrets, or rebind it. One verb for both. */
  app.put('/api/slots/actor/:actor/:slot', async (req, reply) => {
    const denied = denyManage(req.headers.authorization);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { actor, slot } = req.params as { actor: string; slot: string };
    const body = (req.body ?? {}) as { secret?: unknown };
    if (typeof body.secret !== 'string' || !body.secret) {
      return reply.code(400).send({ error: 'a binding needs the `secret` name to bind the slot to' });
    }
    try {
      const binding = await slots.bind(decodeURIComponent(actor), decodeURIComponent(slot), body.secret);
      // The GRANT is logged, never a value — there is none on this path to log.
      req.log?.info?.({ actor: binding.actor, slot: binding.slot, secret: binding.secret }, 'slot bound');
      return { binding, actor: await slots.view(binding.actor) };
    } catch (err) {
      return fail(reply, err);
    }
  });

  /** Withdraw a grant. 404 when there was none, so a page can tell "removed" from "was not there". */
  app.delete('/api/slots/actor/:actor/:slot', async (req, reply) => {
    const denied = denyManage(req.headers.authorization);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { actor, slot } = req.params as { actor: string; slot: string };
    try {
      const name = decodeURIComponent(actor);
      const key = decodeURIComponent(slot);
      const had = await slots.unbind(name, key);
      if (!had) return reply.code(404).send({ error: `${name} has no binding for slot ${JSON.stringify(key)}` });
      req.log?.info?.({ actor: name, slot: key }, 'slot unbound');
      return { unbound: true, actor: await slots.view(name) };
    } catch (err) {
      return fail(reply, err);
    }
  });

  /**
   * Would a run naming these actors start? THE SAME CHECK `startRun` MAKES, over HTTP.
   *
   * One implementation, two callers, deliberately: a CLI or a page that answered "this would run"
   * from a second reading of the same state is how a preflight comes to disagree with the gate it
   * is previewing, and the disagreement is only ever discovered by a run that was promised.
   */
  app.post('/api/slots/preflight', async (req, reply) => {
    const denied = denyManage(req.headers.authorization);
    if (denied) return reply.code(denied.code).send(denied.body);
    const body = (req.body ?? {}) as { actors?: unknown };
    if (!Array.isArray(body.actors)) {
      return reply.code(400).send({ error: 'a preflight needs an `actors` array' });
    }
    try {
      const refusal = await slots.refuseRun(body.actors.map(toActorRef));
      return { ok: refusal === null, ...(refusal ? { refusal } : {}) };
    } catch (err) {
      return fail(reply, err);
    }
  });

  /**
   * The resolution ledger — "which actor read my key, and when".
   *
   * Filtered by actor, slot, run or secret, newest first. It answers about REFUSALS too, which is
   * the half worth having: a slot an actor asked for and did not declare is the event this design
   * exists to refuse, and a trail of successes only would go quiet exactly when it matters.
   */
  app.get('/api/slots/audit', async (req, reply) => {
    const denied = denyManage(req.headers.authorization);
    if (denied) return reply.code(denied.code).send(denied.body);
    const q = (req.query ?? {}) as { actor?: string; slot?: string; run?: string; secret?: string; limit?: string };
    const limit = Number(q.limit);
    return {
      location: slots.log.location,
      resolutions: slots.log.list({
        ...(q.actor ? { actor: q.actor } : {}),
        ...(q.slot ? { slot: q.slot } : {}),
        ...(q.run ? { run: q.run } : {}),
        ...(q.secret ? { secret: q.secret } : {}),
        ...(Number.isFinite(limit) && limit > 0 ? { limit } : {}),
      }),
    };
  });

  /**
   * THE ACTOR'S OWN SLOT FETCH — the one route in this file that answers with a value.
   *
   * The actor is whoever its signed token says it is, and it may ask only for a slot the version
   * it says it is running DECLARED. The answer carries the slot and the value; what secret was
   * behind it stays the operator's business.
   */
  app.post('/api/slots/resolve', async (req, reply) => {
    const header = req.headers.authorization ?? '';
    if (!header.startsWith(BEARER)) {
      return reply.code(401).send({ error: 'an actor identity token is required to resolve a slot' });
    }
    const body = (req.body ?? {}) as { slot?: unknown; version?: unknown; run?: unknown };
    if (typeof body.slot !== 'string' || !body.slot) {
      return reply.code(400).send({ error: 'resolving a slot needs a `slot` name' });
    }
    if (typeof body.version !== 'string' || !body.version) {
      // REQUIRED, and this is the field a first implementation leaves optional. The declaration is
      // per version, so a request that did not say which version it is could only be checked
      // against "any version that ever declared this slot" — which is the union, which is exactly
      // the leak a versioned declaration exists to close.
      return reply.code(400).send({
        error: 'resolving a slot needs the actor `version` asking for it — a slot is declared per version',
      });
    }
    // VERIFYING IS ITS OWN STEP AND ITS OWN STATUS, for `routes.ts`'s reason: an expired token is
    // 401 ("be somebody"), a valid identity asking for the wrong thing is 403 ("not that").
    let identity: string;
    try {
      identity = secrets.verifyIdentity(header.slice(BEARER.length));
    } catch (err) {
      return reply.code(401).send({ error: err instanceof Error ? err.message : 'unauthorized' });
    }
    try {
      const got = await slots.resolveSlot({
        identity,
        slot: body.slot,
        version: body.version,
        ...(typeof body.run === 'string' ? { run: body.run } : {}),
      });
      // The act is logged here as well as written into the ledger, so an operator reading server
      // logs during an incident sees it without opening a second surface. The VALUE is in neither.
      req.log?.info?.({ identity, slot: got.slot, version: body.version }, 'slot resolved');
      return got;
    } catch (err) {
      return fail(reply, err);
    }
  });
}

/** `"probe@0.2.0"` or `{name, version}` → a ref. Both forms, because a manifest writes the first
 *  and an API caller writes the second, and refusing either would be a gate nobody can invoke. */
export function toActorRef(raw: unknown): ActorRef {
  if (typeof raw === 'string') {
    const at = raw.lastIndexOf('@');
    return at > 0 ? { name: raw.slice(0, at), version: raw.slice(at + 1) } : { name: raw };
  }
  if (raw && typeof raw === 'object') {
    const o = raw as { name?: unknown; version?: unknown };
    return {
      name: typeof o.name === 'string' ? o.name : '',
      ...(typeof o.version === 'string' && o.version ? { version: o.version } : {}),
    };
  }
  return { name: '' };
}

/**
 * Domain error → status. NOTHING HERE INTERPOLATES A VALUE, and it cannot: every error it maps is
 * thrown by code that does not hold one at the point it throws.
 *
 * The three slot statuses are chosen so a caller can act on them without parsing a sentence:
 * 403 the actor asked for something it never declared, 409 the operator has not granted it, 410
 * the grant points at bytes that are gone.
 */
function fail(reply: FastifyReply, err: unknown): FastifyReply {
  if (err instanceof SlotUndeclared) return reply.code(403).send({ error: err.message });
  if (err instanceof SlotUnbound) return reply.code(409).send({ error: err.message });
  if (err instanceof SlotRefused) return reply.code(400).send({ error: err.message });
  if (err instanceof SecretRefused) return reply.code(400).send({ error: err.message });
  if (err instanceof NoSuchSecret) return reply.code(404).send({ error: err.message });
  if (err instanceof SecretRevoked) return reply.code(410).send({ error: err.message });
  if (err instanceof SecretForbidden) return reply.code(403).send({ error: err.message });
  return reply.code(500).send({ error: err instanceof Error ? err.message : String(err) });
}
