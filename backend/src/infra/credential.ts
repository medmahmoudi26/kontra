/**
 * The cloud credential: a NAME that workflow code carries, and a value that exists for the length
 * of one converge (ADR 0034 §4).
 *
 * ── WHAT MOVED, AND WHAT DID NOT ──────────────────────────────────────────────────────────────
 *
 * Nothing about *where* the value is allowed to be has weakened. What changed is the KIND of
 * guarantee. `workspace.ts` used to read `DIGITALOCEAN_TOKEN` out of the process environment, and
 * "the cloud token lives only in `orchestrator-infra`" was true because of which process happened
 * to hold that variable — an accident of topology that any refactor could quietly break by reading
 * the same variable somewhere else, and one that cost a `.env` edit and a service restart to
 * rotate. Now the value comes out of the secret store at the point of use, and what crosses every
 * boundary above that point is a {@link SecretRef}: a name, and optionally a pinned version.
 *
 * ── THE FIVE NEGATIVES, WHICH ARE HOW THIS IS TESTED ──────────────────────────────────────────
 *
 * The credential's value is never a workflow argument, never an activity argument, never part of a
 * **Batch**, never in Pulumi stack config or state, and never on a **Machine**. Only the name
 * crosses, and a name is not a secret.
 *
 *   1-3 are the secret store's own property and its own regression guard — `secrets/history.test.ts`
 *       runs a workflow that names a credential and sweeps the whole `History` proto for the value.
 *       This module inherits it by carrying a `SecretRef` and never a string value: there is no
 *       field on {@link CloudCredentialUse} a value could be put in.
 *     4 `pulumiState.test.ts` runs a REAL `pulumi up` against a `file://` backend with a sentinel
 *       credential and sweeps the state directory and the workspace. Its control case passes the
 *       same sentinel as stack config and finds it, so the assertion is not vacuous.
 *     5 `machineSecret.test.ts` sweeps every byte this stack sends to a Machine — cloud-init, the
 *       install script, the teardown — for the same sentinel. The structural half of that is in
 *       `stacks.ts`: **the credential never enters `FleetArgs`**, so the one provider-coupled file
 *       that writes cloud-init and the remote command cannot reach it even by accident.
 *
 * ── WHY RESOLUTION IS NOT AN ACTIVITY ─────────────────────────────────────────────────────────
 *
 * `secrets/lastHop.ts` states the rule and this module obeys it: an activity's RESULT is recorded
 * in history exactly as its arguments are, so a `resolveCredential` activity would write the token
 * into `ActivityTaskCompleted` for the namespace's whole retention. {@link resolveProviderEnv} is
 * therefore called from INSIDE the activity that runs the converge, and what leaves that activity
 * is Pulumi's summary.
 *
 * {@link checkCredential} is the one thing that *looks* like a resolver activity and is not: it
 * reads METADATA only — {@link SecretStore.describe}, which by construction cannot return bytes —
 * so a missing, destroyed or fully-revoked credential fails at the start of `fleet.up()`, naming
 * the secret, instead of surfacing as a provider 401 several minutes into a converge.
 */

import { ResolutionLog, type Resolution, type ResolutionOutcome } from '../secrets/audit';
import { resolveAtLastHop } from '../secrets/lastHop';
import { secretStore, type SecretStore } from '../secrets/store';
import { NoSuchSecret, SecretRefused, type SecretRef } from '../secrets/types';

/**
 * What a control plane calls its cloud credential when a caller does not name one.
 *
 * A NAME IS NOT A SECRET, which is why this one may live in the environment while the value may
 * not. It exists because `kontra fleet up` and `kontra fleet down` (`cli/fleet.go`) have no
 * `--credential` flag: an installation whose secret is called `do-prod` would otherwise have every
 * CLI-driven teardown look for `do-token` and fail — with Machines still running, which is the one
 * failure this repo refuses to build.
 *
 * Read per call rather than frozen at module load, deliberately, and for the opposite of
 * `FLEET_DEFAULTS`' reason: this is the fallback for callers that named nothing, and a test that
 * sets it must not need a fresh process.
 */
export function defaultCloudCredential(env: NodeJS.ProcessEnv = process.env): string {
  return (env.KONTRA_CLOUD_CREDENTIAL ?? '').trim() || 'do-token';
}

/**
 * The pseudo-slot every fleet resolution is filed under in the ledger (`secrets/audit.ts`).
 *
 * Issue 20 specifies the audit tuple as `(actor, version, slot, run)` and this is not a slot an
 * actor declared — a fleet credential is the OPERATOR's, resolved by a worker at the last hop, not
 * granted to an actor through a binding. It is filed under one reserved name so the ledger answers
 * "which of my credentials was read, by what, for which run" with one shape rather than two, and
 * the name is RESERVED (`secrets/slots.ts:assertSlotName`) so no actor can declare a slot that
 * produces lines indistinguishable from these.
 */
export const FLEET_CREDENTIAL_SLOT = 'fleet_credential';

/** A missing, destroyed or fully-revoked credential, named. The class exists so the activity can
 *  mark it non-retryable: three attempts at a secret that does not exist is three identical
 *  failures and a slower answer. */
export class CloudCredentialUnavailable extends Error {}

/**
 * Who is asking, for the ledger. THERE IS NO VALUE FIELD HERE AND THERE CANNOT BE ONE — every
 * member is a name, and each is already in workflow history in the clear.
 */
export interface CloudCredentialUse {
  /** The NAME of the operator secret. Never its value. */
  credential: SecretRef;
  /** The environment variable this provider's SDK reads the token from, at the last hop. */
  providerEnvVar: string;
  /** The Actor this fleet places, for the ledger. Empty for a Machines-only converge. */
  actor: string;
  /** That Actor's version, as the caller named it. */
  version: string;
  /** The Run this converge serves — the caller's workflow id, or empty for a CLI-driven op. */
  run: string;
}

export interface CredentialOptions {
  store?: SecretStore;
  log?: ResolutionLog;
}

/**
 * Prefixes a DigitalOcean token carries, refused where a NAME is expected.
 *
 * THE NAME ALPHABET CANNOT CATCH THIS, which is the whole reason for the list. A DigitalOcean token
 * is `dop_v1_` followed by lowercase hex, and every one of those characters is legal in a secret
 * name — so a caller that pasted the token into `credential` sends a perfectly well-formed name.
 *
 * REFUSING HERE DOES NOT UN-WRITE HISTORY, and saying so is more useful than pretending otherwise:
 * by the time this runs the value is already in `StackWorkflowInput.args`, inline and in the clear.
 * What it does is stop the mistake being USED — no converge runs against a name that is a token,
 * and the operator gets a sentence saying to rotate it rather than a provider 404 they will read as
 * a typo. The guard that catches it BEFORE it crosses is on the caller's side
 * (`sdk/python/actorkit/fleet.py:CREDENTIAL_LOOKS_LIKE_A_TOKEN`); this is the same refusal for
 * every other door — the CLI, the HTTP route, a second SDK.
 */
const LOOKS_LIKE_A_TOKEN = ['dop_v1_', 'doo_v1_', 'dor_v1_'];

/** True when a caller put a value where the name goes. Never quotes what it was given. */
export function looksLikeAToken(name: string): boolean {
  return LOOKS_LIKE_A_TOKEN.some((p) => name.startsWith(p));
}

/**
 * Untrusted JSON → a credential NAME.
 *
 * Accepts `"do-prod"` and `{ name: "do-prod", version: 3 }`, and NOTHING ELSE — in particular a
 * caller cannot smuggle a value through this by any spelling, because the only fields read are
 * `name` and `version`. An absent or empty credential means {@link defaultCloudCredential}.
 *
 * A VERSION PIN IS ACCEPTED AND IS NOT THE DEFAULT. Unpinned is what makes a rotation take effect
 * on the next converge without editing the code that names the secret, which is the whole point of
 * moving the credential here; a pin is for a caller that has a reason to want yesterday's value.
 */
export function coerceCredential(raw: unknown, env: NodeJS.ProcessEnv = process.env): SecretRef {
  if (typeof raw === 'string') {
    const name = raw.trim();
    return { name: name || defaultCloudCredential(env) };
  }
  if (raw && typeof raw === 'object') {
    const obj = raw as { name?: unknown; version?: unknown };
    const name = typeof obj.name === 'string' ? obj.name.trim() : '';
    const version = Number(obj.version);
    return {
      name: name || defaultCloudCredential(env),
      ...(Number.isInteger(version) && version > 0 ? { version } : {}),
    };
  }
  return { name: defaultCloudCredential(env) };
}

/** The credential named in a stack's arguments, or this control plane's default. */
export function credentialFrom(
  args: Record<string, unknown> | undefined,
  env: NodeJS.ProcessEnv = process.env
): SecretRef {
  return coerceCredential(args?.credential, env);
}

/** How a reference reads in an error and in the ledger — `do-prod`, or `do-prod@3` when pinned. */
export function credentialLabel(ref: SecretRef): string {
  return ref.version ? `${ref.name}@${ref.version}` : ref.name;
}

/**
 * START-TIME REFUSAL: is this credential resolvable, without reading a byte of it.
 *
 * METADATA ONLY. {@link SecretStore.describe} returns {@link SecretMeta}, which by construction
 * carries no value and no fingerprint, so this check cannot leak what it is checking — and it can
 * therefore run in an activity whose result goes into workflow history. `meta.current` is the
 * highest un-revoked version; absent means every version has been revoked, which is a different
 * sentence to an operator than "no such secret" and gets one.
 *
 * WHAT IT DOES NOT PROVE, said plainly so nobody reads more into a green check: that the value is
 * the right token, that the provider still accepts it, or that the store can decrypt it. A
 * credential revoked at the CLOUD looks healthy here and fails at the provider — this replaces the
 * failure that arrives thirty lines into a shell script, not the one that needs a round trip to
 * DigitalOcean.
 */
export async function checkCredential(
  ref: SecretRef,
  opts: CredentialOptions = {}
): Promise<{ credential: string; version: number }> {
  const store = opts.store ?? secretStore();
  if (looksLikeAToken(ref.name)) {
    // NEVER QUOTED BACK. This message reaches a log line, an HTTP body and a workflow failure
    // event, so the one thing it must not do is repeat the value it is refusing.
    throw new CloudCredentialUnavailable(
      'the cloud credential named for this fleet looks like a DigitalOcean TOKEN rather than the ' +
        'NAME of a secret. Put the token in the store — Settings, or `PUT /api/secrets/<name>` — ' +
        'and name that instead. Rotate it too: a credential passed as a workflow argument is ' +
        'already in history in the clear, and no rotation takes that back.'
    );
  }
  let meta;
  try {
    meta = await store.describe(ref.name);
  } catch (err) {
    // A malformed name is the caller's mistake and its message already names the alphabet.
    if (err instanceof SecretRefused) throw new CloudCredentialUnavailable(err.message);
    throw err;
  }
  if (!meta) {
    throw new CloudCredentialUnavailable(
      `this fleet needs the cloud credential ${JSON.stringify(ref.name)} and there is no secret by ` +
        `that name. Create it in Settings, or with \`PUT /api/secrets/${ref.name}\`. ` +
        'Nothing was provisioned.'
    );
  }
  if (meta.owner) {
    throw new CloudCredentialUnavailable(
      `${JSON.stringify(ref.name)} belongs to ${meta.owner} — a fleet's cloud credential is an ` +
        'OPERATOR secret, resolved at the last hop by the infra worker. An actor-owned secret ' +
        'cannot be one.'
    );
  }
  if (ref.version !== undefined) {
    const pinned = meta.versions.find((v) => v.version === ref.version);
    if (!pinned) {
      throw new CloudCredentialUnavailable(
        `${JSON.stringify(ref.name)} has no version ${ref.version} — it is at version ${meta.current ?? 0}.`
      );
    }
    if (pinned.revokedAt) {
      throw new CloudCredentialUnavailable(
        `version ${ref.version} of ${JSON.stringify(ref.name)} has been revoked. Unpin the ` +
          'reference to use the current version, or write a new one.'
      );
    }
    return { credential: ref.name, version: ref.version };
  }
  if (meta.current === undefined) {
    throw new CloudCredentialUnavailable(
      `every version of the cloud credential ${JSON.stringify(ref.name)} has been revoked. Write a ` +
        `new one — Settings, or \`PUT /api/secrets/${ref.name}\` — nothing was provisioned.`
    );
  }
  return { credential: ref.name, version: meta.current };
}

/**
 * THE LAST HOP: a name becomes the one environment variable the provider SDK reads, inside the
 * activity that is about to run the converge.
 *
 * `resolveAtLastHop` rather than `withSecret` because the value has to outlive a single call — the
 * Pulumi workspace holds it in `envVars` for the length of the converge, and a callback scope
 * cannot express that. `secrets/lastHop.ts` names that function for the place it may be used, and
 * this is the place: nothing above `stackUp` ever holds the string.
 *
 * EVERY RESOLUTION IS A LEDGER LINE, including the failures — a refusal nobody can see is a
 * refusal nobody acts on, and "the credential could not be read" is exactly the event an operator
 * needs when a run will not start.
 */
export async function resolveProviderEnv(
  use: CloudCredentialUse,
  opts: CredentialOptions = {}
): Promise<Record<string, string>> {
  const store = opts.store ?? secretStore();
  const ledger = opts.log ?? new ResolutionLog();
  const record = (outcome: ResolutionOutcome, secretVersion: number): void => {
    ledger.record({
      at: Date.now(),
      actor: use.actor,
      version: use.version,
      slot: FLEET_CREDENTIAL_SLOT,
      run: use.run,
      secret: use.credential.name,
      secretVersion,
      outcome,
    } satisfies Resolution);
  };

  // The metadata check first, so the message an operator sees names the secret and says what to do
  // about it rather than being whatever the backend threw.
  const { version } = await checkCredential(use.credential, { store }).catch((err) => {
    record(err instanceof CloudCredentialUnavailable ? 'missing' : 'forbidden', 0);
    throw err;
  });

  let value: string;
  try {
    value = await resolveAtLastHop(use.credential, store);
  } catch (err) {
    record(err instanceof NoSuchSecret ? 'missing' : 'forbidden', version);
    throw err;
  }
  record('resolved', version);
  return { [use.providerEnvVar]: value };
}

/**
 * ONE-TIME MIGRATION off the environment variable, run at the infra worker's boot.
 *
 * An installation upgrading into this change has its token in a `.env` on a controller and nothing
 * in the store; without this, its next `fleet up` — and, worse, its next `fleet down` — would fail
 * on a credential nobody had created yet. So the process that legitimately holds the variable today
 * writes it into the store once, under the name this control plane resolves by default.
 *
 * IT IS A MIGRATION AND NOT A FALLBACK, and the difference is the whole reason it is safe:
 *
 *  - it runs ONCE, at boot, and only when the store has NOTHING under that name. It never
 *    overwrites, so an operator who rotated in the store does not get last month's `.env` value put
 *    back on the next restart.
 *  - after it, `DIGITALOCEAN_TOKEN` is read NOWHERE — not by `engineEnv`, not by the program. An
 *    operator who edits the variable and restarts sees no change, which is why this is LOUD: the
 *    log line says the variable is no longer read and names the command that rotates the secret.
 *
 * NEVER THROWS. A store that cannot be written is a reason to refuse a provision, at the point of
 * provisioning, with a message about the credential — not a reason for the worker that also serves
 * the Monitor and the retention sweep to refuse to start.
 */
export async function adoptLegacyCloudToken(
  opts: CredentialOptions & { env?: NodeJS.ProcessEnv; onLog?: (line: string) => void } = {}
): Promise<{ adopted: boolean; name: string; because: string }> {
  const env = opts.env ?? process.env;
  const name = defaultCloudCredential(env);
  const log = opts.onLog ?? ((line: string) => console.log(line)); // eslint-disable-line no-console
  const token = (env.DIGITALOCEAN_TOKEN ?? '').trim();
  if (!token) return { adopted: false, name, because: 'no DIGITALOCEAN_TOKEN in the environment' };
  try {
    const store = opts.store ?? secretStore();
    if (await store.describe(name)) {
      return { adopted: false, name, because: `${name} is already in the secret store` };
    }
    const { version } = await store.put(name, token);
    log(
      `[infra] adopted DIGITALOCEAN_TOKEN into the secret store as ${JSON.stringify(name)} ` +
        `(version ${version}). The environment variable is NO LONGER READ — rotate with ` +
        `Settings (or \`PUT /api/secrets/${name}\`), and a new fleet uses the new value with no restart.`
    );
    return { adopted: true, name, because: 'imported from the environment' };
  } catch (err) {
    log(
      `[infra] could not adopt DIGITALOCEAN_TOKEN into the secret store as ${JSON.stringify(name)}: ` +
        `${err instanceof Error ? err.message : String(err)}. A fleet operation will refuse and say so.`
    );
    return { adopted: false, name, because: 'the store refused the write' };
  }
}
