/**
 * The four cases, as pure state: declared-and-unbound, bound, revoked, and the destroyed secret —
 * plus the version diff that turns a new slot into a change somebody sees.
 *
 * NO FILESYSTEM AND NO STORE. `slotStatuses` takes metadata, so every state a page can draw is a
 * comparison here rather than a fixture on disk — and the reason that is worth having is not
 * speed: there is no code path in this module that could reach a value, and a test that had to
 * build a real store to check a LABEL would be a test that made one reachable.
 */

import { describe, expect, it } from 'vitest';

import {
  RESERVED_SLOT_NAMES,
  SLOT_NAME_RE,
  addedSlots,
  assertSlotName,
  slotStatuses,
  unboundRefusal,
  unresolvable,
  type ActorSlots,
  type SlotBinding,
} from './slots';
import type { SecretMeta } from './types';

const AUG = (day: number): number => Date.UTC(2026, 7, day, 12, 0, 0);

const bind = (slot: string, secret: string): SlotBinding => ({
  actor: 'probe',
  slot,
  secret,
  boundAt: AUG(20),
});

const meta = (name: string, over: Partial<SecretMeta> = {}): SecretMeta => ({
  name,
  createdAt: AUG(1),
  updatedAt: AUG(1),
  versions: [{ version: 1, createdAt: AUG(1) }],
  current: 1,
  ...over,
});

const index = (...metas: SecretMeta[]): Map<string, SecretMeta> =>
  new Map(metas.map((m) => [m.name, m]));

describe('the four cases', () => {
  it('DECLARED AND UNBOUND is its own state, and says the actor will ask and get nothing', () => {
    const [row] = slotStatuses([{ name: 'api_key', description: 'the vendor key' }], [], index());
    expect(row?.state).toBe('unbound');
    expect(row?.secret).toBeUndefined();
    expect(row?.description).toBe('the vendor key');
    expect(row?.detail).toMatch(/nothing is bound/);
  });

  it('BOUND names the secret and the version that would answer today', () => {
    const [row] = slotStatuses(
      [{ name: 'api_key' }],
      [bind('api_key', 'stripe-prod')],
      index(meta('stripe-prod', { current: 3 }))
    );
    expect(row?.state).toBe('bound');
    expect(row?.secret).toBe('stripe-prod');
    expect(row?.secretVersion).toBe(3);
  });

  it('REVOKED is NOT unbound — the grant stands and the bytes are gone', () => {
    // The distinction is the whole reason there are three states: "bind something" and "write a
    // new version of the thing you already bound" are different fixes, and one label over both
    // sends an operator to re-grant a credential that is already granted.
    const [row] = slotStatuses(
      [{ name: 'api_key' }],
      [bind('api_key', 'stripe-prod')],
      index(meta('stripe-prod', { current: undefined, versions: [{ version: 1, createdAt: AUG(1), revokedAt: AUG(9) }] }))
    );
    expect(row?.state).toBe('revoked');
    expect(row?.secret).toBe('stripe-prod');
    expect(row?.secretVersion).toBeUndefined();
    expect(row?.detail).toMatch(/write a new version/);
  });

  it('a DESTROYED secret is its own state again, because "it is gone" is not "rotate it"', () => {
    const [row] = slotStatuses([{ name: 'api_key' }], [bind('api_key', 'stripe-prod')], index());
    expect(row?.state).toBe('missing');
    expect(row?.detail).toMatch(/no longer exists/);
  });

  it('only `bound` resolves — every other state blocks a run', () => {
    const rows = slotStatuses(
      [{ name: 'a' }, { name: 'b' }, { name: 'c' }],
      [bind('b', 'live'), bind('c', 'dead')],
      index(meta('live'), meta('dead', { current: undefined }))
    );
    expect(rows.map((r) => r.state)).toEqual(['unbound', 'bound', 'revoked']);
    expect(unresolvable(rows).map((r) => r.slot)).toEqual(['a', 'c']);
  });
});

describe('a new version that adds a slot', () => {
  it('names what THIS version asks for that the one before it did not', () => {
    expect(addedSlots([{ name: 'api_key' }, { name: 'webhook_secret' }], [{ name: 'api_key' }])).toEqual([
      'webhook_secret',
    ]);
  });

  it('reports nothing for a version that only DROPPED a slot — an addition is the risk', () => {
    expect(addedSlots([{ name: 'api_key' }], [{ name: 'api_key' }, { name: 'webhook_secret' }])).toEqual([]);
  });

  it('treats a first version as having added nothing, which is not a claim it asks for nothing', () => {
    // `ActorSlots.comparedWith` is what tells the two apart; the diff itself cannot.
    expect(addedSlots([{ name: 'api_key' }], [])).toEqual(['api_key']);
  });
});

describe('the refusal a run start is answered with', () => {
  const view = (over: Partial<ActorSlots> = {}): ActorSlots => ({
    actor: 'probe',
    version: '0.2.0',
    slots: slotStatuses([{ name: 'api_key' }], [], index()),
    added: [],
    ...over,
  });

  it('names the actor, the version and the slot', () => {
    const said = unboundRefusal([view()]) ?? '';
    expect(said).toContain('probe@0.2.0');
    expect(said).toContain('api_key');
    expect(said).toMatch(/cannot start/);
  });

  it('names EVERY blocked slot at once, so one fix is one round trip', () => {
    const said =
      unboundRefusal([
        view({ slots: slotStatuses([{ name: 'api_key' }, { name: 'webhook_secret' }], [], index()) }),
      ]) ?? '';
    expect(said).toContain('api_key');
    expect(said).toContain('webhook_secret');
  });

  it('is null when everything resolves — a gate that always refuses is a gate nobody keeps', () => {
    expect(
      unboundRefusal([
        view({ slots: slotStatuses([{ name: 'api_key' }], [bind('api_key', 'live')], index(meta('live'))) }),
      ])
    ).toBeNull();
  });

  it('is null for an actor that declares nothing', () => {
    expect(unboundRefusal([view({ slots: [] })])).toBeNull();
  });
});

describe('the alphabet', () => {
  it('is a source identifier, narrower than a secret name', () => {
    expect(SLOT_NAME_RE.test('api_key')).toBe(true);
    // `.` and `-` are legal in a SECRET name and not here: a slot is a name in the actor's own
    // code, and keeping the two alphabets apart keeps the two words apart.
    expect(SLOT_NAME_RE.test('api-key')).toBe(false);
    expect(SLOT_NAME_RE.test('API_KEY')).toBe(false);
    expect(SLOT_NAME_RE.test('9lives')).toBe(false);
  });

  it('refuses with the alphabet rather than a bare no', () => {
    expect(() => assertSlotName('API_KEY')).toThrow(/lowercase letters/);
  });

  it('refuses the names the resolution ledger already uses', () => {
    // `fleet_credential` is the pseudo-slot the control plane's OWN cloud credential is filed
    // under (`infra/credential.ts`, ADR 0034 §4). It is not a grant to an actor at all, and an
    // actor slot by the same name would produce ledger lines an operator could not tell apart
    // from the ones that provisioned their Machines.
    expect(RESERVED_SLOT_NAMES).toContain('fleet_credential');
    // Well-formed by the alphabet, and still refused — which is the only way this can be a rule.
    expect(SLOT_NAME_RE.test('fleet_credential')).toBe(true);
    expect(() => assertSlotName('fleet_credential')).toThrow(/reserved/);
    // …and nothing else is: the reservation is one name, not a namespace.
    expect(() => assertSlotName('fleet_credentials')).not.toThrow();
    expect(() => assertSlotName('fleet_key')).not.toThrow();
  });
});
