/**
 * The secret store every caller talks to: the rules that hold whatever backend is underneath.
 *
 * Two things live here and nowhere else, because they must be true of a KMS backend the day it
 * arrives without that backend re-deriving them:
 *
 *  1. **WHAT A NAME AND A VALUE MAY BE.** A name is in URLs, in workflow arguments and in an
 *     actor's source, so it is bounded to a small, lowercase, unambiguous alphabet — `DO_TOKEN`
 *     and `do-token` being two different secrets is a mistake somebody makes at 3am with a
 *     production credential.
 *  2. **WHO MAY RESOLVE WHAT.** An owned secret belongs to one actor and is resolvable only by
 *     that actor, authenticated as itself. An unowned one is the operator's and is resolvable only
 *     IN PROCESS, at the last hop, by a worker that already holds the store — never over HTTP.
 *     Neither can be reached through the other's door, and both directions are asserted.
 *
 *     THERE IS NOW A THIRD DOOR AND IT OPENS NEITHER OF THOSE (issue 20): an actor may reach ONE
 *     unowned secret over HTTP when the OPERATOR bound it to a slot that actor declared. It is a
 *     grant the operator made by name, it is checked against the exact secret it was made for, and
 *     it still cannot reach another actor's owned secret. See {@link Principal}.
 *
 * THE VALUE IS TRIMMED ON THE WAY IN, and that is a deliberate mutation of an operator's bytes.
 * A token pasted from a terminal or a provider's console carries a trailing newline about half the
 * time, and every provider rejects it — as a 401, which reads as "wrong credential" rather than
 * "right credential, one byte too long". Since no read path can ever show what is stored, an
 * operator has no way to see that byte and no way to diagnose it. Trailing whitespace is worth
 * strictly less than the days that failure costs. It is stated in the API's response so nobody
 * discovers it by experiment.
 */

import {
  NoSuchSecret,
  SecretForbidden,
  SecretRefused,
  actorIdentity,
  type SecretBackend,
  type SecretMeta,
  type SecretRef,
} from './types';
import { FileSecretBackend } from './fileBackend';
import { mintActorToken, verifyActorToken, type MintedIdentity } from './identity';
import { secretsDir } from './keyring';

/** Lowercase, and it may not start with punctuation. Bounded so a name is safe in a URL, a task
 *  queue and a filename without anybody having to escape it.
 *
 *  DECLARED IN `@kontra/core` because the console's name field applies the same rule, and it used
 *  to do so from a second copy — see that module's header. Re-exported here so this file is still
 *  the one a reader of the store looks at. */
export { SECRET_NAME_RE } from '@kontra/core/secrets';
import { SECRET_NAME_RE } from '@kontra/core/secrets';

/** Same bound as `actorControl.ts:ACTOR_RE`, so an owner is a name that can really reach a worker. */
const ACTOR_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

/**
 * 64 KiB. Large enough for any credential anybody actually holds (a PEM, a service-account JSON)
 * and small enough that the store file stays a thing you can read with `jq`. It is NOT related to
 * the codec's 128 KiB threshold, and the coincidence of scale is worth a sentence: no secret is
 * ever a payload, so no secret is ever measured against that threshold.
 */
const MAX_VALUE_BYTES = 64 * 1024;

/**
 * Who is asking. There are exactly THREE kinds of resolver and they are not interchangeable —
 * each names a different reason the caller is entitled to bytes, and no one of them can be reached
 * through another's door.
 */
export type Principal =
  /** In-process, at the last hop: an activity about to make the call the credential is for. */
  | { kind: 'worker' }
  /** An actor, authenticated as itself by a signed identity token (`identity.ts`). */
  | { kind: 'actor'; identity: string }
  /**
   * An actor resolving a SLOT the operator BOUND to one of their secrets (`slots.ts`).
   *
   * THE THIRD DOOR, and it is a real widening rather than a convenience — this is the one way an
   * actor reaches a secret it does not own, so it is worth stating exactly what it does and does
   * not open. What makes it legitimate is that the OPERATOR named the secret: the actor declared
   * `api_key`, the operator bound `api_key → stripe-prod`, and the binding IS the grant. The actor
   * never names a secret; it names a slot it declared, and gets whatever the operator pointed at
   * it, or nothing.
   *
   * WHAT IT STILL DOES NOT OPEN, both asserted in `slotStore.test.ts`:
   *   - it cannot be used to name an arbitrary operator secret. {@link SecretStore.resolve} refuses
   *     unless `ref.name` is exactly the `secret` this principal was minted for, and the only
   *     minter is `slotStore.ts:resolveSlot`, from a binding it has just read off disk.
   *   - it cannot reach ANOTHER ACTOR's owned secret. An owned secret still requires the asker to
   *     be its owner, exactly as the `actor` door does — so an operator cannot bind one actor's
   *     credential into a second actor's slot and quietly re-point it (`slotStore.ts:bind`
   *     refuses that binding at the moment it is made, and this refuses it again at use).
   */
  | { kind: 'binding'; identity: string; slot: string; secret: string };

/** The in-process last hop. Not a value — a constant, so the call sites read as what they are. */
export const LAST_HOP: Principal = { kind: 'worker' };

export interface SecretStoreOptions {
  /**
   * Where the key that SIGNS this store's actor identities lives.
   *
   * IT MUST BE THE SAME STORE'S KEY, and that is not a detail — an identity token is a claim about
   * who may resolve secrets HERE, so verifying it with some other key directory means two stores on
   * one machine cross-authenticate each other's actors. MEASURED as a straight 401: the route
   * verified with the process default while the store read from a temp directory, and every
   * resolution failed as a forgery.
   *
   * Defaults to the local keyring (`keyring.ts:secretsDir`), which is also what a future KMS
   * backend gets: the values move off the machine, the identity key does not have to.
   */
  keyDir?: string;
}

export class SecretStore {
  private readonly keyDir: string;

  constructor(
    private readonly backend: SecretBackend,
    opts: SecretStoreOptions = {}
  ) {
    this.keyDir = opts.keyDir ?? secretsDir();
  }

  /**
   * Mint an identity for one actor — see `identity.ts` for what a token is and is not.
   *
   * ON THE STORE rather than free-standing, because the token has to be signed by the key of the
   * store it will be presented to, and a free function's default was exactly the bug above.
   */
  mintIdentity(actor: string, ttlSeconds?: number): MintedIdentity {
    return mintActorToken(actor, { dir: this.keyDir, ...(ttlSeconds ? { ttlSeconds } : {}) });
  }

  /** The identity a token proves, or a {@link SecretForbidden}. */
  verifyIdentity(token: string): string {
    return verifyActorToken(token, { dir: this.keyDir });
  }

  /** Which backend this appliance is configured with — shown in Settings. */
  get backendKind(): string {
    return this.backend.kind;
  }

  /** Where the values rest, when the backend can say. Shown in Settings for the same reason the
   *  backend kind is: an operator who cannot read a secret back still has to know where it is. */
  get location(): string | undefined {
    const loc = (this.backend as { location?: unknown }).location;
    return typeof loc === 'string' ? loc : undefined;
  }

  list(): Promise<SecretMeta[]> {
    return this.backend.list();
  }

  describe(name: string): Promise<SecretMeta | undefined> {
    return this.backend.describe(assertName(name));
  }

  /**
   * Create a secret, or rotate it by writing a new version.
   *
   * Returns the metadata AND which version this write became, because that is the one thing the
   * caller cannot work out for itself and the one thing an operator wants to see: "shodan-key is
   * now at version 3" is the confirmation that replaces reading the value back.
   */
  async put(
    name: string,
    value: string,
    opts: { owner?: string } = {}
  ): Promise<{ secret: SecretMeta; version: number; rotated: boolean }> {
    const key = assertName(name);
    const trimmed = assertValue(value);
    const owner = opts.owner === undefined ? undefined : assertOwner(opts.owner);
    const before = await this.backend.describe(key);
    if (before && owner && owner !== before.owner) {
      // Ownership is set at creation and never moves — `fileBackend.ts:put` says why. Refusing here
      // rather than silently ignoring the field is the difference between a rejected request and an
      // operator believing they have re-pointed a credential.
      throw new SecretRefused(
        `${JSON.stringify(key)} already belongs to ${before.owner ?? 'the operator'} — ` +
          'destroy it and create it again to change who may resolve it'
      );
    }
    const secret = await this.backend.put(key, trimmed, owner ? { owner } : {});
    const version = secret.versions[secret.versions.length - 1]!.version;
    return { secret, version, rotated: Boolean(before) };
  }

  /** Make one version unusable. Idempotent: revoking a revoked version is a no-op, not an error —
   *  a second click during an incident must not read as a failure. */
  revoke(name: string, version: number): Promise<SecretMeta> {
    if (!Number.isInteger(version) || version < 1) {
      throw new SecretRefused(`${JSON.stringify(String(version))} is not a version number`);
    }
    return this.backend.revoke(assertName(name), version);
  }

  destroy(name: string): Promise<boolean> {
    return this.backend.destroy(assertName(name));
  }

  /**
   * Name → value, at the last hop. THE ONLY PATH IN THIS PACKAGE THAT RETURNS BYTES.
   *
   * `by` is not optional and has no default. A default would make the safe call and the privileged
   * call look identical at the call site, and the privileged one is the one that hands an actor
   * somebody else's credential.
   *
   * THREE DOORS, AND THIS IS THE ONE PLACE THAT DECIDES BETWEEN THEM. `worker` reaches an operator
   * secret in-process; `actor` reaches its OWN; `binding` reaches the one secret an operator bound
   * to a slot that actor declared. Every widening of this method's authority has to be written
   * here, in the branch below, rather than in whichever caller wanted it — which is why
   * `slotStore.ts` mints a principal and asks, instead of resolving the backend itself.
   */
  async resolve(ref: SecretRef, by: Principal): Promise<{ value: string; version: number }> {
    const name = assertName(ref.name);
    // THE BINDING PRINCIPAL MAY ONLY ASK FOR WHAT IT WAS MINTED FOR, checked before anything is
    // read. A binding is a grant of ONE secret to ONE slot; without this the type would carry the
    // grant and the call would carry the name, and the first caller to pass the wrong `ref` would
    // have turned a per-slot grant into ambient access to the whole store.
    if (by.kind === 'binding' && name !== by.secret) {
      throw new SecretForbidden(
        `slot ${JSON.stringify(by.slot)} is bound to ${JSON.stringify(by.secret)}, not to ${JSON.stringify(name)}`
      );
    }
    const meta = await this.backend.describe(name);
    if (!meta) throw new NoSuchSecret(`no secret named ${JSON.stringify(name)}`);
    if (meta.owner) {
      // AN OWNED SECRET IS ITS OWNER'S THROUGH EITHER DOOR. A binding does not launder ownership:
      // an operator who bound one actor's credential into a second actor's slot would otherwise
      // have re-pointed it with a write that looks like a routine grant, which is exactly the act
      // `fileBackend.ts:put` refuses to let a rotation perform.
      const asker = by.kind === 'worker' ? undefined : by.identity;
      if (asker !== meta.owner) {
        // The message says nothing about the secret beyond the name the caller already used. An
        // actor probing for other actors' credentials learns only that it is not them.
        throw new SecretForbidden(`${JSON.stringify(name)} does not belong to you`);
      }
    } else if (by.kind === 'actor') {
      throw new SecretForbidden(
        `${JSON.stringify(name)} is an operator secret — it is resolved at the last hop by a worker, not fetched over HTTP`
      );
    }
    const version = ref.version ?? meta.current;
    const value = await this.backend.resolve({ name, ...(ref.version ? { version: ref.version } : {}) });
    return { value, version: version ?? 0 };
  }
}

function assertName(name: string): string {
  if (typeof name !== 'string' || !SECRET_NAME_RE.test(name)) {
    throw new SecretRefused(
      `${JSON.stringify(String(name))} is not a secret name — lowercase letters, digits, ` +
        '`.`, `-` and `_`, starting with a letter or digit, at most 64 characters'
    );
  }
  return name;
}

/** The value, trimmed, or a refusal that NEVER quotes what was sent. */
function assertValue(value: string): string {
  if (typeof value !== 'string') throw new SecretRefused('a secret value must be a string');
  const trimmed = value.trim();
  if (!trimmed) throw new SecretRefused('a secret value cannot be empty');
  const size = Buffer.byteLength(trimmed, 'utf8');
  if (size > MAX_VALUE_BYTES) {
    throw new SecretRefused(`a secret value is at most ${MAX_VALUE_BYTES} bytes (this one is ${size})`);
  }
  return trimmed;
}

function assertOwner(owner: string): string {
  const actor = owner.startsWith('actor:') ? owner.slice('actor:'.length) : owner;
  if (!ACTOR_RE.test(actor)) {
    throw new SecretRefused(`${JSON.stringify(owner)} is not an owner — it must be an actor name, or \`actor:<name>\``);
  }
  return actorIdentity(actor);
}

let shared: SecretStore | null = null;

/**
 * The process-wide store — what the last hop and the API both hold.
 *
 * Built on first use rather than at boot, for the same reason the state reader is: a fresh
 * appliance must start with no store on disk, and the store directory (and its key) come into
 * existence when somebody first writes a secret, not when the server starts.
 */
export function secretStore(): SecretStore {
  if (!shared) {
    const dir = secretsDir();
    shared = new SecretStore(new FileSecretBackend({ dir }), { keyDir: dir });
  }
  return shared;
}

/** TESTS ONLY: drop the process-wide store so the next call rebuilds it from the environment. */
export function resetSecretStore(): void {
  shared = null;
}
