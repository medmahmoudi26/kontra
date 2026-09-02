/**
 * The LAST HOP: where a name a workflow carried becomes a value, inside the activity that uses it.
 *
 * ── THE ONE RULE ──────────────────────────────────────────────────────────────────────────────
 *
 * **Resolution is never an activity of its own.** It is tempting — a `resolveSecret` activity is
 * two lines and every workflow could call it — and it would put the credential in workflow history
 * with a bow on it: an activity's RESULT is recorded exactly as its arguments are, so a resolver
 * activity writes the value into `ActivityTaskCompleted` for the namespace's whole retention. The
 * codec would not save it either; a seventy-byte token is far under the claim-check threshold and
 * rides inline in the clear (`codec/claimCheck.ts`, ADR 0034 §4 finding 6).
 *
 * So the value is resolved INSIDE the activity that makes the call it is for, and what comes back
 * out of that activity is the RESULT of the call — never the credential, never a header built from
 * it, never an error message quoting the request it was in.
 *
 * ── HOW A WORKFLOW NAMES ONE ──────────────────────────────────────────────────────────────────
 *
 *     // workflow: carries a NAME. This is safe in history, because a name is not a secret.
 *     await callProvider({ endpoint, credential: { name: 'do-token' } });
 *
 *     // activity: the last hop.
 *     export async function callProvider(input: { endpoint: string; credential: SecretRef }) {
 *       return withSecret(input.credential, async (token) =>
 *         fetch(input.endpoint, { headers: { authorization: `Bearer ${token}` } })
 *       );
 *     }
 *
 * {@link withSecret} is the shape to copy: the value exists only inside the callback, and what
 * escapes is whatever the callback returns. {@link resolveAtLastHop} is the same resolution
 * without the scope, for an activity that has to hold a client open across calls — it is the one
 * a reviewer should look twice at, which is why it is named for the place it may be used.
 */

import { LAST_HOP, secretStore, type SecretStore } from './store';
import type { SecretRef } from './types';

/**
 * Name → value, for an operator secret, in a worker process.
 *
 * Refuses an actor-OWNED secret: those belong to the actor and are fetched by it, authenticated as
 * itself (`store.ts:resolve`). A worker resolving another party's credential because it happens to
 * hold the store is the ambient authority this store exists to remove.
 */
export async function resolveAtLastHop(ref: SecretRef, store: SecretStore = secretStore()): Promise<string> {
  const { value } = await store.resolve(ref, LAST_HOP);
  return value;
}

/**
 * Resolve for the duration of one call and no longer.
 *
 * THE VALUE IS NOT RETURNED, and that is the whole point of the shape: `fn`'s result is what comes
 * back, so a call site that wanted to leak the credential has to write a line that obviously does.
 * There is no zeroing here and nothing pretends there is — a JavaScript string cannot be wiped, it
 * lives until the GC collects it, and a `.fill(0)` on a copy would be theatre. What this bounds is
 * REACH, not lifetime: nothing above the callback ever holds the value.
 */
export async function withSecret<T>(
  ref: SecretRef,
  fn: (value: string) => Promise<T>,
  store: SecretStore = secretStore()
): Promise<T> {
  return fn(await resolveAtLastHop(ref, store));
}
