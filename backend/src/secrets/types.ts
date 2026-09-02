/**
 * The secret store's vocabulary, and the ONE seam a second backend has to satisfy.
 *
 * WHY A SECRET IS A NAME AND NEVER A VALUE, stated here because every type below is shaped by it:
 * the payload codec is a CLAIM-CHECK, NOT ENCRYPTION (`codec/claimCheck.ts`). It offloads anything
 * over 128 KiB to the blob store and leaves everything smaller INLINE IN WORKFLOW HISTORY IN THE
 * CLEAR, for the namespace's whole retention, readable by anything that can describe the execution.
 * A seventy-byte API token is two thousand times under that threshold. There is no size at which a
 * credential is safely a workflow argument, because the mechanism that would protect a large one is
 * offload, not encryption — so a secret is never a workflow argument, never an activity argument
 * and never part of a **Batch**. Only a {@link SecretRef} crosses those boundaries, and a name is
 * not a secret (ADR 0034 §4).
 *
 * NOTHING IN THIS FILE HAS A `value` FIELD EXCEPT THE ARGUMENT TO A WRITE. That is the write-only
 * property expressed as types rather than as a rule to remember: {@link SecretBackend.resolve} is
 * the single method in the interface that returns bytes, it is named so a reviewer can grep for
 * every call site, and no HTTP read path is allowed to call it on the operator's behalf.
 */

/** What workflow code carries: a NAME, and optionally a pinned version. Safe in history. */
export interface SecretRef {
  name: string;
  /** Pin a version. Omitted means "whatever is current at the moment of use", which is what makes
   *  a rotation take effect without redeploying the code that names the secret. */
  version?: number;
}

/** One version of a secret. Metadata only — the value is not here and cannot be. */
export interface SecretVersionMeta {
  version: number;
  createdAt: number;
  /** When it was revoked. A revoked version resolves to an error, and the file backend has already
   *  destroyed its ciphertext by the time this is set — see `fileBackend.ts:revoke`. */
  revokedAt?: number;
}

/**
 * A secret as every read path sees it: what it is called, who may resolve it, and what versions
 * exist. This is the WHOLE of what the API and the UI ever learn.
 *
 * THERE IS NO FINGERPRINT FIELD, and that is deliberate rather than an omission. A "last four
 * characters" or a digest is the obvious affordance for "did I paste the right thing", and it is a
 * read path: a hash of a low-entropy secret is a brute-forceable copy of it, and a suffix is a
 * fraction of one. `createdAt` answers the question an operator actually has — is what I just
 * typed the thing that is in there now — without handing back any of the value.
 */
export interface SecretMeta {
  name: string;
  /**
   * The identity that may resolve this secret, or absent for an operator secret.
   *
   * `actor:<name>` is the only form minted today ({@link actorIdentity}). An owned secret belongs
   * to that actor and is resolvable ONLY by it, authenticated as itself; an unowned one is the
   * operator's and is resolvable ONLY in-process, at the last hop, by a worker that already holds
   * the store. Neither can be resolved through the other's door — see `store.ts:resolve`.
   */
  owner?: string;
  createdAt: number;
  /** Last write of any kind: a rotation, a revocation. */
  updatedAt: number;
  /** Ascending by version. Every version ever created, including revoked ones. */
  versions: SecretVersionMeta[];
  /**
   * The highest version that is NOT revoked — what an unpinned {@link SecretRef} resolves to.
   *
   * Absent when every version has been revoked, and resolution then fails rather than reaching for
   * an older one: a secret whose every version is revoked has no value, and answering with one
   * would defeat the act. Revoking only the NEWEST version deliberately does roll back to the one
   * before it, which is the "that rotation was wrong" recovery path.
   */
  current?: number;
}

/** `actor:probe` — the identity an actor authenticates as, and the only owner form minted today. */
export function actorIdentity(actor: string): string {
  return `actor:${actor}`;
}

/** Options for a write. `owner` is only honoured when the secret is being CREATED. */
export interface SecretPutOptions {
  owner?: string;
}

/**
 * The pluggable backend (issue 19; ADR 0034 §4 depends on it).
 *
 * The local encrypted-file implementation is what ships (`fileBackend.ts`). A KMS or vault
 * implementation is later work and MUST NOT require changing a caller: everything above this
 * interface — the routes, the Settings surface, the last hop, the actor's own fetch — talks to
 * {@link SecretStore} and knows nothing about where bytes rest.
 *
 * THE INTERFACE IS AT THE LEVEL OF THE DOMAIN, not of storage. `put` means "create or rotate" and
 * `revoke` means "make this version unusable" because those are native operations in a vault as
 * well as in a file; an interface of `readBlob`/`writeBlob` would have made every backend
 * re-implement versioning, and the first one to get it subtly different would be the one holding
 * the credentials.
 */
export interface SecretBackend {
  /** `file`, `kms`, … — shown in Settings so an operator knows where their credentials rest. */
  readonly kind: string;
  list(): Promise<SecretMeta[]>;
  describe(name: string): Promise<SecretMeta | undefined>;
  /** Create the secret, or add a version to it. Returns METADATA — never the value written. */
  put(name: string, value: string, opts?: SecretPutOptions): Promise<SecretMeta>;
  revoke(name: string, version: number): Promise<SecretMeta>;
  /** Forget the secret and every version of it. `false` when there was nothing to forget. */
  destroy(name: string): Promise<boolean>;
  /**
   * THE ONLY METHOD THAT RETURNS A VALUE. Every caller of it is a last hop by construction: an
   * activity about to make the call the credential is for, or the route that answers an actor
   * asking for its own. Nothing else may call it, and `routes.ts` states that as a rule with a
   * test behind it.
   */
  resolve(ref: SecretRef): Promise<string>;
}

/**
 * A refusal the CALLER can fix: a bad name, an empty value, a value past the size bound.
 *
 * ITS MESSAGE NEVER CARRIES THE VALUE. An error is a read path — it reaches a log line, an HTTP
 * body and a browser console — so the rule that no read path returns the value covers this class
 * too, and `secrets/routes.test.ts` asserts the sentinel appears in no error body.
 */
export class SecretRefused extends Error {}

/** There is no such secret, or no such version of it. Separate from {@link SecretRefused} because
 *  the route answers it 404 rather than 400. */
export class NoSuchSecret extends Error {}

/** The version exists and has been revoked. Its own class because it is the one failure an
 *  operator is meant to see and be reassured by. */
export class SecretRevoked extends Error {}

/** The caller is not who this secret belongs to. 403, and it says nothing about whether the secret
 *  exists — an existence oracle over other actors' credentials is worth nothing to the caller and
 *  something to an attacker. */
export class SecretForbidden extends Error {}
