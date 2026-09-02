/**
 * SLOTS: what an actor asks for, and what an operator grants it.
 *
 * ── THE INDIRECTION, AND THE TWO THINGS THAT MATTER MORE THAN IT ──────────────────────────────
 *
 * An actor declares a SLOT — `api_key` — and an operator BINDS that slot to one of their secrets
 * — `api_key → stripe-prod`. A third-party actor's author cannot know your secret names, so an
 * actor that named one directly would either be wrong on every installation but the author's, or
 * would be a request to be handed a credential it can name but you did not choose to give it.
 *
 * The indirection is the mechanism. The two properties it buys are the point:
 *
 *  1. **THE CREDENTIALS AN ACTOR WILL ASK FOR ARE VISIBLE BEFORE IT RUNS.** A declaration is
 *     registered when the worker registers itself, so the Actors page can list what this code will
 *     want — which turns a credential grant into a decision the operator made, rather than one
 *     they discovered afterwards in a log line.
 *  2. **A NEW SLOT IN A NEW VERSION IS A DIFF, NOT A RUNTIME SURPRISE.** Declarations are per
 *     `(actor, version)`, so {@link addedSlots} can say `0.3.0 asks for one credential 0.2.0 did
 *     not`. An actor that quietly grew an appetite between versions is exactly the change nobody
 *     notices, and it is the whole reason the declaration is versioned while the BINDING is not.
 *
 * ── WHY A BINDING IS PER-ACTOR AND A DECLARATION IS PER-VERSION ───────────────────────────────
 *
 * Binding per version would mean every version bump silently un-grants every credential, and an
 * operator would learn that from a run that failed. Binding per ACTOR means a bump keeps working,
 * and the one thing that CHANGES — a slot the new version added — surfaces as unbound, named, on
 * the page, before the run. The asymmetry is the design, not an oversight.
 *
 * ── THE FOUR CASES, AND WHY THE FOURTH IS A REFUSAL ───────────────────────────────────────────
 *
 * Three states are distinguishable on the page ({@link SlotState}) and the fourth is not a state
 * at all — it is a request that must fail:
 *
 *  - `unbound`  — declared, and the operator has granted nothing. The run is refused (see
 *                 {@link unboundRefusal}), because the actor will ask and there is no answer.
 *  - `bound`    — granted, and the secret has a usable version. This is the only case that resolves.
 *  - `revoked`  — granted, and every version of that secret has been revoked. NOT the same as
 *                 unbound: the operator's grant still stands and the credential behind it is gone,
 *                 which is a different thing to fix (write a new version, not bind something).
 *  - `missing`  — granted, and the secret was DESTROYED. Also not `unbound`: the binding names a
 *                 secret that no longer exists, and re-creating it under the same name restores
 *                 the grant. Kept apart from `revoked` because "rotate it" and "it is gone" send
 *                 an operator to two different places.
 *
 * And the fourth case: an actor asking for a slot **it never declared** is REFUSED
 * ({@link SlotUndeclared}), never satisfied. Satisfying it would delete the whole property — the
 * declaration would stop being the list of what this code can ask for, and would become a hint.
 *
 * ── NOTHING HERE TOUCHES A VALUE ──────────────────────────────────────────────────────────────
 *
 * This module reads {@link SecretMeta}, which by construction carries no value and no fingerprint
 * (`types.ts` says why), and it produces sentences for a page. The one path that resolves bytes is
 * `slotStore.ts:resolve`, and it is the only file in this pair that imports the store at all.
 */

import type { SecretMeta } from './types';

/**
 * A slot name, as an actor's SOURCE spells it: lowercase, an identifier.
 *
 * NARROWER THAN A SECRET NAME (`store.ts:SECRET_NAME_RE`, which also takes `.` and `-`), and
 * deliberately so — a slot is a name in the actor's own code, `api_key`, not a name in an
 * operator's inventory. Keeping the two alphabets different is a small thing that stops the two
 * words being used interchangeably in a sentence where only one of them is right.
 */
export const SLOT_NAME_RE = /^[a-z][a-z0-9_]{0,63}$/;

/** Same bound as `actorControl.ts:ACTOR_RE`, so a declaration names an actor that can be served. */
export const SLOT_ACTOR_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

/** One slot, as the actor's code declares it. `description` is the author's own sentence about
 *  what the credential is FOR, and it is the only thing an operator has to go on when deciding
 *  which of their secrets to point at it. */
export interface SlotSpec {
  name: string;
  description?: string;
}

/** What one `(actor, version)` asks for. Registered by the worker when it registers itself. */
export interface SlotDeclaration {
  actor: string;
  version: string;
  slots: SlotSpec[];
  declaredAt: number;
}

/**
 * The operator's grant: this actor's `slot` resolves THIS secret.
 *
 * It carries the secret's NAME and nothing else, for the reason every type in this package does:
 * a name is not a secret (ADR 0034 §4), and there is no field here that a value could be put in
 * by a later change without that change being obvious.
 */
export interface SlotBinding {
  actor: string;
  slot: string;
  /** A name in the operator's own store. Never returned to the actor — see `slotRoutes.ts`. */
  secret: string;
  boundAt: number;
}

/** The three distinguishable states of a declared slot, plus the destroyed-secret case. */
export type SlotState = 'unbound' | 'bound' | 'revoked' | 'missing';

/** One slot, joined against the bindings and the store, as a page draws it. */
export interface SlotStatus {
  slot: string;
  description?: string;
  state: SlotState;
  /** The secret the operator bound, when there is one. Shown to the OPERATOR, never to the actor. */
  secret?: string;
  /** Which version of that secret would answer today. Absent unless `state` is `bound`. */
  secretVersion?: number;
  boundAt?: number;
  /** One line: what this state is, and what to do about it. Never a value. */
  detail: string;
}

/** Everything one actor version asks for, and what it would get today. */
export interface ActorSlots {
  actor: string;
  /** The version these declarations came from. Empty when nothing has ever declared for it. */
  version: string;
  slots: SlotStatus[];
  /**
   * Slots this version declares that the version before it did not — the visible diff.
   *
   * Empty for a first version, which is NOT the same claim as "this version added nothing": there
   * was nothing to compare against. {@link ActorSlots.comparedWith} says which it is.
   */
  added: string[];
  /** The version `added` was computed against, or absent when there was no earlier declaration. */
  comparedWith?: string;
}

/** An actor named by a caller: `probe@0.2.0`, or `probe` for whatever version declared last. */
export interface ActorRef {
  name: string;
  version?: string;
}

/** The actor asked for a slot its declaration does not contain. 403 — see the header's fourth case. */
export class SlotUndeclared extends Error {}

/** The slot is declared and the operator has bound nothing to it. 409: the fix is a binding. */
export class SlotUnbound extends Error {}

/** A bind/declare the CALLER can fix: a bad name, an owned secret bound to somebody else's slot. */
export class SlotRefused extends Error {}

/**
 * Slot names an actor may not have, because something else already files ledger lines under them.
 *
 * `fleet_credential` is the pseudo-slot every FLEET credential resolution is recorded under
 * (`infra/credential.ts:FLEET_CREDENTIAL_SLOT`) — an operator secret read by the infra worker at
 * the last hop, which is not a grant to an actor at all. Reserving the name keeps the resolution
 * ledger unambiguous: a line under it is always the control plane provisioning Machines, never an
 * actor that chose a confusing name for its own slot.
 */
export const RESERVED_SLOT_NAMES: readonly string[] = ['fleet_credential'];

/** A slot name, or a refusal naming the alphabet. */
export function assertSlotName(slot: string): string {
  if (typeof slot !== 'string' || !SLOT_NAME_RE.test(slot)) {
    throw new SlotRefused(
      `${JSON.stringify(String(slot))} is not a slot name — lowercase letters, digits and ` +
        '`_`, starting with a letter, at most 64 characters'
    );
  }
  if (RESERVED_SLOT_NAMES.includes(slot)) {
    throw new SlotRefused(
      `${JSON.stringify(slot)} is reserved — the resolution ledger files the control plane's own ` +
        'cloud credential under it, and an actor slot with the same name would be indistinguishable'
    );
  }
  return slot;
}

/** An actor name, or a refusal. Same bound as everywhere else a name reaches a queue. */
export function assertSlotActor(actor: string): string {
  if (typeof actor !== 'string' || !SLOT_ACTOR_RE.test(actor)) {
    throw new SlotRefused(`${JSON.stringify(String(actor))} is not an actor name`);
  }
  return actor;
}

/** `probe@0.2.0` — the key a declaration is filed under, matching the catalog's own key form. */
export function declarationKey(actor: string, version: string): string {
  return `${actor}@${version}`;
}

/** `probe/api_key` — the key a binding is filed under. */
export function bindingKey(actor: string, slot: string): string {
  return `${actor}/${slot}`;
}

/**
 * Join a version's declared slots against the operator's bindings and the store's metadata.
 *
 * PURE, and it takes METADATA rather than a store: every state this produces is a node test with
 * no filesystem, and — the reason that matters — there is no code path in this function that could
 * reach a value even if somebody wanted it to.
 */
export function slotStatuses(
  declared: readonly SlotSpec[],
  bindings: readonly SlotBinding[],
  secrets: ReadonlyMap<string, SecretMeta>
): SlotStatus[] {
  return declared.map((spec) => {
    const bound = bindings.find((b) => b.slot === spec.name);
    if (!bound) {
      return {
        slot: spec.name,
        ...(spec.description ? { description: spec.description } : {}),
        state: 'unbound' as const,
        detail: 'declared, and nothing is bound to it — this actor will ask and there is no answer',
      };
    }
    const meta = secrets.get(bound.secret);
    const common = {
      slot: spec.name,
      ...(spec.description ? { description: spec.description } : {}),
      secret: bound.secret,
      boundAt: bound.boundAt,
    };
    if (!meta) {
      return {
        ...common,
        state: 'missing' as const,
        detail: `bound to "${bound.secret}", which no longer exists — the grant stands, the secret was destroyed`,
      };
    }
    if (meta.current === undefined) {
      return {
        ...common,
        state: 'revoked' as const,
        detail: `bound to "${bound.secret}", whose every version is revoked — write a new version to make it usable`,
      };
    }
    return {
      ...common,
      state: 'bound' as const,
      secretVersion: meta.current,
      detail: `bound to "${bound.secret}", version ${meta.current}`,
    };
  });
}

/**
 * Which slots this version asks for that the previous one did not.
 *
 * The comparison is against the version IMMEDIATELY PRECEDING it, which is the same question
 * `compat.ts` asks about schemas and it is asked the same way for the same reason: a diff against
 * "whatever version happens to sort first" would report an addition that was already there two
 * builds ago, and a reader who has been shown one false diff stops reading the true ones.
 */
export function addedSlots(current: readonly SlotSpec[], previous: readonly SlotSpec[]): string[] {
  const before = new Set(previous.map((s) => s.name));
  return current.filter((s) => !before.has(s.name)).map((s) => s.name);
}

/** The states a run cannot start on. `bound` is the only one that resolves. */
export function unresolvable(statuses: readonly SlotStatus[]): SlotStatus[] {
  return statuses.filter((s) => s.state !== 'bound');
}

/**
 * The sentence a run start is refused with — NAMING EVERY SLOT, and what to do about each.
 *
 * ONE MESSAGE FOR THE WHOLE RUN rather than one per actor, because the alternative is an operator
 * binding a credential, restarting, and being told about the next one. A refusal that names three
 * slots is fixed once.
 */
export function unboundRefusal(blocked: readonly ActorSlots[]): string | null {
  const named = blocked
    .map((a) => ({ a, bad: unresolvable(a.slots) }))
    .filter((x) => x.bad.length > 0);
  if (named.length === 0) return null;
  const lines = named.map(({ a, bad }) => {
    const each = bad.map((s) => `    ${s.slot} — ${s.detail}`).join('\n');
    return `  ${declarationKey(a.actor, a.version)}\n${each}`;
  });
  return (
    'this run cannot start: an actor it uses declares a credential slot that cannot be resolved.\n' +
    `${lines.join('\n')}\n` +
    'Bind each slot to one of your secrets in Settings, then start again. Failing here rather ' +
    'than at the actor is deliberate — a run that gets this far has already paid for whatever ' +
    'machines it was going to use.'
  );
}
