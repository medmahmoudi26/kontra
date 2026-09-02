/**
 * How an actor proves it is itself when it fetches its own secret.
 *
 * THE ACTOR'S OWN SECRET IS NOT HANDED TO IT, and this file exists because of that. A credential
 * that arrived through a **Batch**, a workflow argument or an activity argument would be inline in
 * workflow history in the clear (`types.ts` says why), so the actor asks for it instead — and an
 * asker has to be somebody. This is the smallest thing that makes "authenticated as itself" real:
 * a token the orchestrator SIGNS, naming one actor identity and an expiry, verifiable with no
 * lookup and no second store.
 *
 * HMAC AND NOT A RANDOM BEARER, deliberately. A random token needs a table of issued tokens, which
 * is a second thing to keep, to back up and to leak. An HMAC over the identity carries its own
 * claim: nothing is stored, and the orchestrator can verify a token minted by a previous process.
 * The signing key is a PURPOSE-SCOPED SUBKEY of the store's master key (`keyring.ts`), so a
 * signing bug can never become a decryption oracle for the values themselves.
 *
 * WHAT IT IS NOT — said plainly, because a token in an environment variable is exactly the shape
 * of thing this slice is replacing:
 *   - It is NOT a credential. It resolves secrets that are OWNED BY THAT ACTOR and nothing else;
 *     an operator secret cannot be fetched with one at all (`store.ts:resolve`).
 *   - It CANNOT BE REVOKED INDIVIDUALLY yet. The bound is its expiry, and the recovery from a leak
 *     is the store's own: revoke the version the token could reach. A per-actor epoch that makes
 *     one identity's tokens invalid is a small change to make when there is a reason to.
 *   - Its `sub` IS ONLY AS TRUE AS THE MACHINE'S CODE-SERVING BOUNDARY. Whoever can serve an actor
 *     on this appliance can serve one that calls itself anything, and be minted that identity. The
 *     store trusts the same boundary `auth.ts` records for `serve`; it does not invent a stronger
 *     one and pretend the actor's code was attested.
 */

import { createHmac, timingSafeEqual } from 'node:crypto';

import { masterKey, subkey } from './keyring';
import { SecretForbidden, actorIdentity } from './types';

/** HKDF label for the signing subkey. Appended to, never edited — see `keyring.ts`. */
const IDENTITY_KEY_PURPOSE = 'actor-identity-token.v1';

/** Version prefix, so a format change is a different token rather than a confusing failure. */
const PREFIX = 'kai1';

/**
 * 30 days.
 *
 * NOT AN HOUR, and the reason is behavioural rather than cryptographic: `@actor.load` runs every
 * time the actor's resource opens, which on a long-lived worker is once per **Session** for as
 * long as that worker polls — days or weeks. A short expiry would make an actor that has worked
 * all week fail its next load with an auth error, at a moment nobody connects to a token minted
 * when it was served. `KONTRA_ACTOR_TOKEN_TTL` (seconds) moves it.
 */
const DEFAULT_TTL_SECONDS = 30 * 24 * 60 * 60;

export interface MintedIdentity {
  token: string;
  /** `actor:<name>` — what a secret's `owner` is compared against. */
  identity: string;
  expiresAt: number;
}

interface Claims {
  sub: string;
  iat: number;
  exp: number;
}

export function identityTtlSeconds(): number {
  const raw = Number(process.env.KONTRA_ACTOR_TOKEN_TTL);
  return Number.isFinite(raw) && raw > 0 ? Math.floor(raw) : DEFAULT_TTL_SECONDS;
}

/** Mint a token that names one actor. `dir` selects the store whose key signs it (tests). */
export function mintActorToken(
  actor: string,
  opts: { ttlSeconds?: number; now?: number; dir?: string } = {}
): MintedIdentity {
  const identity = actorIdentity(actor);
  const now = opts.now ?? Date.now();
  const expiresAt = now + (opts.ttlSeconds ?? identityTtlSeconds()) * 1000;
  const claims: Claims = { sub: identity, iat: now, exp: expiresAt };
  const body = b64url(Buffer.from(JSON.stringify(claims), 'utf8'));
  return { token: `${PREFIX}.${body}.${b64url(sign(body, opts.dir))}`, identity, expiresAt };
}

/**
 * The identity a token proves, or a refusal.
 *
 * EVERY FAILURE IS THE SAME CLASS AND A SHORT MESSAGE. Which of "wrong signature", "expired" and
 * "not a token" happened is useful to an attacker with a token generator and to nobody else; the
 * actor's own diagnosis is the same either way — it was served without a current identity.
 */
export function verifyActorToken(token: string, opts: { now?: number; dir?: string } = {}): string {
  const parts = typeof token === 'string' ? token.split('.') : [];
  if (parts.length !== 3 || parts[0] !== PREFIX) throw new SecretForbidden('not a valid actor identity token');
  const body = parts[1]!;
  const given = Buffer.from(parts[2]!, 'base64url');
  const expected = sign(body, opts.dir);
  if (given.length !== expected.length || !timingSafeEqual(given, expected)) {
    throw new SecretForbidden('not a valid actor identity token');
  }
  let claims: Claims;
  try {
    claims = JSON.parse(Buffer.from(body, 'base64url').toString('utf8')) as Claims;
  } catch {
    throw new SecretForbidden('not a valid actor identity token');
  }
  const now = opts.now ?? Date.now();
  if (!claims || typeof claims.sub !== 'string' || !claims.sub.startsWith('actor:')) {
    throw new SecretForbidden('not a valid actor identity token');
  }
  if (!Number.isFinite(claims.exp) || claims.exp <= now) {
    // The one distinguishable failure, because it is the one an operator has to act on: re-serve
    // the actor. It says nothing a token generator could use — an expiry is in the token already.
    throw new SecretForbidden('this actor identity token has expired — serve the actor again to mint a new one');
  }
  return claims.sub;
}

function sign(body: string, dir?: string): Buffer {
  return createHmac('sha256', subkey(masterKey(dir), IDENTITY_KEY_PURPOSE)).update(body).digest();
}

function b64url(buf: Buffer): string {
  return buf.toString('base64url');
}

/** The environment variable an actor's worker reads its identity from. Named here because two
 *  places set it (`actorControl.ts` when the console serves an actor, a deploy when it does not)
 *  and one reads it (`sdk/python/actorkit/secrets.py`). */
export const ACTOR_TOKEN_VAR = 'KONTRA_ACTOR_TOKEN';
