/**
 * What the two slot surfaces know — all of it derived, none of it fetched twice.
 *
 * PURE, so every state either page can be in is a node test with no DOM: nothing here reads the
 * network, and both components take rows as a prop.
 *
 * THE VOCABULARY IS THE POINT, exactly as it is in `secrets.ts`. An operator looking at a slot has
 * to be able to tell four situations apart at a glance, and three of them look like "not working":
 *
 *   UNBOUND  you have granted nothing              → bind it
 *   BOUND    granted, and the secret resolves      → nothing to do
 *   REVOKED  granted, and the bytes are gone       → write a new version of that secret
 *   MISSING  granted, and the secret was destroyed → re-create it, or bind something else
 *
 * Collapsing revoked into unbound would send an operator to re-grant a credential that is already
 * granted, and they would find the same slot still red afterwards with nothing on screen
 * explaining why. That is the failure this file's whole vocabulary exists to prevent.
 *
 * AND THERE IS A FIFTH THING TO SAY, which is not a slot state at all: a binding for a slot NO
 * declared version asks for. It is drawn ({@link unusedBindings}) rather than hidden, because a
 * grant nobody can see is a grant nobody withdraws — an actor that dropped a slot in its newest
 * version leaves one behind every time, and they accumulate silently.
 */

import type { ActorSlots, Resolution, SlotBinding, SlotState, SlotStatus } from '../run/api';

/** `2026-08-25` — an ISO date, so it reads the same in every timezone a screenshot lands in. */
export const isoDate = (ms: number): string => new Date(ms).toISOString().slice(0, 10);

/** `2026-08-25 09:14` — the ledger needs a time; a date alone cannot answer "and when". */
export const isoMinute = (ms: number): string => new Date(ms).toISOString().slice(0, 16).replace('T', ' ');

/** How each state reads on a badge. The word an operator sees, not the enum. */
export const SLOT_WORD: Record<SlotState, string> = {
  unbound: 'not bound',
  bound: 'bound',
  revoked: 'revoked',
  missing: 'secret gone',
};

/** Which states block a run. Everything but `bound`, and the page says so with the same rule the
 *  server refuses with (`secrets/slots.ts:unresolvable`). */
export function blocking(slots: readonly SlotStatus[]): SlotStatus[] {
  return slots.filter((s) => s.state !== 'bound');
}

/**
 * The one line above an actor's slots.
 *
 * IT NEVER SAYS "3 SLOTS" AND STOPS. The count is the least useful fact available: what an
 * operator opening this card needs is whether this actor can run, and if not, how many credentials
 * stand between them and a run.
 */
export function summariseSlots(view: ActorSlots): string {
  const total = view.slots.length;
  if (total === 0) return 'This actor asks for no credentials.';
  const bad = blocking(view.slots).length;
  const plural = total === 1 ? 'credential' : 'credentials';
  return bad === 0
    ? `Asks for ${total} ${plural}, all bound.`
    : `Asks for ${total} ${plural}; ${bad} cannot be resolved, so a run using this actor is refused at the start.`;
}

/**
 * What a NEW VERSION changed — the sentence that turns a new slot into a diff somebody reads.
 *
 * `null` when there is nothing to say, which covers both "this version added nothing" and "there
 * was no earlier version to compare against". Those are different facts and the second one must
 * not be drawn as the first: `comparedWith` is what tells them apart, and a first version simply
 * gets no strip rather than a reassuring one.
 */
export function versionDiff(view: ActorSlots): string | null {
  if (!view.comparedWith || view.added.length === 0) return null;
  const names = view.added.join(', ');
  return view.added.length === 1
    ? `${view.version} asks for a credential ${view.comparedWith} did not: ${names}.`
    : `${view.version} asks for ${view.added.length} credentials ${view.comparedWith} did not: ${names}.`;
}

/** One row of Settings' binding view: an actor, its slots, and what it changed. */
export interface BindingGroup {
  actor: string;
  version: string;
  slots: SlotStatus[];
  /** The change sentence, or null. */
  diff: string | null;
  /** Grants for slots no declared version asks for — drawn, never hidden. */
  unused: SlotBinding[];
  /** How many slots cannot be resolved. The number that decides whether a run starts. */
  blocking: number;
}

/**
 * Bindings whose slot no declared version of that actor asks for.
 *
 * An operator can bind before an actor has ever been served (there is nothing to declare against
 * yet), and an actor can drop a slot in a new version. Both leave a grant standing that nothing
 * consumes — harmless in itself, and exactly the kind of thing that is never cleaned up because
 * nothing ever mentions it.
 */
export function unusedBindings(view: ActorSlots | undefined, bindings: readonly SlotBinding[]): SlotBinding[] {
  const declared = new Set((view?.slots ?? []).map((s) => s.slot));
  return bindings.filter((b) => !declared.has(b.slot));
}

/**
 * Settings' rows: every actor that has declared a slot, and every actor that only has grants.
 *
 * BOTH SIDES, deliberately. Grouping by declaration alone would hide an operator's own grants for
 * an actor that has not registered yet — which is the state a careful operator preparing an
 * install is in, and the one where a page that showed nothing would read as "it did not save".
 */
export function bindingGroups(
  actors: readonly ActorSlots[],
  bindings: readonly SlotBinding[]
): BindingGroup[] {
  const names = [...new Set([...actors.map((a) => a.actor), ...bindings.map((b) => b.actor)])].sort();
  return names.map((actor) => {
    const view = actors.find((a) => a.actor === actor);
    const slots = view?.slots ?? [];
    return {
      actor,
      version: view?.version ?? '',
      slots,
      diff: view ? versionDiff(view) : null,
      unused: unusedBindings(view, bindings.filter((b) => b.actor === actor)),
      blocking: blocking(slots).length,
    };
  });
}

/** How each ledger outcome reads. The refusals are the half worth having, so they get real words. */
export const OUTCOME_WORD: Record<Resolution['outcome'], string> = {
  resolved: 'read',
  undeclared: 'REFUSED — not declared',
  unbound: 'refused — not bound',
  revoked: 'refused — revoked',
  missing: 'refused — secret gone',
  forbidden: 'refused',
};

/** One line of the ledger, as a row a page draws. */
export interface ResolutionRow extends Resolution {
  when: string;
  word: string;
  /** True for anything that did NOT hand a value over — drawn differently, and never as a success. */
  refused: boolean;
  /** `probe@0.2.0 read "stripe-prod" for run nscheck-17` — the whole answer in one sentence. */
  sentence: string;
}

export function resolutionRows(entries: readonly Resolution[]): ResolutionRow[] {
  return entries.map((e) => {
    const who = `${e.actor}@${e.version || '?'}`;
    const what = e.secret ? `"${e.secret}"${e.secretVersion ? ` v${e.secretVersion}` : ''}` : `slot "${e.slot}"`;
    const forRun = e.run ? ` for run ${e.run}` : '';
    return {
      ...e,
      when: isoMinute(e.at),
      word: OUTCOME_WORD[e.outcome] ?? e.outcome,
      refused: e.outcome !== 'resolved',
      sentence:
        e.outcome === 'resolved'
          ? `${who} read ${what} through slot "${e.slot}"${forRun}`
          : `${who} was refused slot "${e.slot}" (${OUTCOME_WORD[e.outcome] ?? e.outcome})${forRun}`,
    };
  });
}

/**
 * The confirmation before a grant is withdrawn — NAMING WHAT STOPS WORKING, the way
 * `secrets.ts:revocationConfirm` does.
 *
 * Unbinding destroys nothing, and the sentence says so: this is the one destructive-looking act on
 * these pages that is fully reversible, and an operator who believes otherwise will leave a grant
 * in place that they meant to move.
 */
export function unbindConfirm(args: { actor: string; slot: string; secret: string }): string {
  return [
    `Unbind "${args.slot}" from "${args.secret}"?`,
    `${args.actor} will ask for this credential and get nothing, and a run using it will be refused at the start.`,
    'The secret itself is untouched — this withdraws the grant, not the value.',
  ].join('\n');
}
