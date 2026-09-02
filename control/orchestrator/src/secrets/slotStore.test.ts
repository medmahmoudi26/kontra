/**
 * Declaring, binding, resolving — and the two properties that are worth more than any of them:
 * the ledger records every outcome and no value, and the binding door opens nothing else.
 *
 * A REAL STORE ON A TEMP DIRECTORY, because this is the half that touches bytes. The sweep at the
 * bottom is the same shape as `routes.test.ts`'s: a sentinel is written as a real secret, resolved
 * through a real binding, and then the ledger and every read path are searched for it.
 */

import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { FileSecretBackend } from './fileBackend';
import { SlotStore } from './slotStore';
import { SlotRefused, SlotUndeclared, SlotUnbound } from './slots';
import { SecretStore } from './store';
import { SecretForbidden, SecretRevoked, actorIdentity } from './types';

const SENTINEL = 'sk_live_SENTINEL_never_in_a_ledger_4b71';

let dir: string;
let secrets: SecretStore;
let slots: SlotStore;
let clock: number;

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), 'kontra-slots-'));
  secrets = new SecretStore(new FileSecretBackend({ dir }), { keyDir: dir });
  clock = Date.UTC(2026, 7, 25, 9, 0, 0);
  slots = new SlotStore(secrets, { dir, now: () => clock });
});

afterEach(() => rmSync(dir, { recursive: true, force: true }));

/** The identity the orchestrator would have minted for this actor. */
const asActor = (name: string): string => actorIdentity(name);

describe('declaring what an actor will ask for', () => {
  it('records a version’s slots, and reads them back before anything has run', async () => {
    const view = await slots.declare('probe', '0.1.0', [
      { name: 'api_key', description: 'the vendor key' },
    ]);
    expect(view.slots).toHaveLength(1);
    expect(view.slots[0]).toMatchObject({ slot: 'api_key', state: 'unbound', description: 'the vendor key' });
  });

  it('is idempotent — a worker restarting is not a contract change', async () => {
    await slots.declare('probe', '0.1.0', [{ name: 'api_key' }]);
    const again = await slots.declare('probe', '0.1.0', [{ name: 'api_key' }]);
    expect(again.slots.map((s) => s.slot)).toEqual(['api_key']);
    expect(slots.declarations('probe')).toHaveLength(1);
  });

  it('SURFACES A NEW VERSION’S NEW SLOT AS A CHANGE, against the version immediately before it', async () => {
    await slots.declare('probe', '0.1.0', [{ name: 'api_key' }]);
    await slots.declare('probe', '0.2.0', [{ name: 'api_key' }, { name: 'webhook_secret' }]);
    const view = await slots.view('probe');
    expect(view.version).toBe('0.2.0');
    expect(view.added).toEqual(['webhook_secret']);
    expect(view.comparedWith).toBe('0.1.0');
  });

  it('compares against 0.9.0 rather than whatever sorts first — `0.10.0` is the newest build', async () => {
    await slots.declare('probe', '0.9.0', [{ name: 'api_key' }]);
    await slots.declare('probe', '0.10.0', [{ name: 'api_key' }, { name: 'webhook_secret' }]);
    const view = await slots.view('probe');
    expect(view.version).toBe('0.10.0');
    expect(view.comparedWith).toBe('0.9.0');
    expect(view.added).toEqual(['webhook_secret']);
  });

  it('says nothing about a first version’s diff, and marks it as uncompared', async () => {
    const view = await slots.declare('probe', '0.1.0', [{ name: 'api_key' }]);
    expect(view.comparedWith).toBeUndefined();
  });

  it('refuses a slot name that is not one', async () => {
    await expect(slots.declare('probe', '0.1.0', [{ name: 'API-KEY' }])).rejects.toThrow(SlotRefused);
  });
});

describe('binding', () => {
  it('binds, rebinds, and unbinds', async () => {
    await secrets.put('stripe-prod', SENTINEL);
    await secrets.put('stripe-test', 'another');
    await slots.declare('probe', '0.1.0', [{ name: 'api_key' }]);

    await slots.bind('probe', 'api_key', 'stripe-prod');
    expect((await slots.view('probe')).slots[0]).toMatchObject({ state: 'bound', secret: 'stripe-prod' });

    await slots.bind('probe', 'api_key', 'stripe-test');
    expect((await slots.view('probe')).slots[0]).toMatchObject({ state: 'bound', secret: 'stripe-test' });
    expect(slots.bindings('probe')).toHaveLength(1);

    expect(await slots.unbind('probe', 'api_key')).toBe(true);
    expect((await slots.view('probe')).slots[0]?.state).toBe('unbound');
    // A second click is not an error.
    expect(await slots.unbind('probe', 'api_key')).toBe(false);
  });

  it('refuses a secret that does not exist, rather than writing a grant that can only fail', async () => {
    await slots.declare('probe', '0.1.0', [{ name: 'api_key' }]);
    await expect(slots.bind('probe', 'api_key', 'nope')).rejects.toThrow(/no secret named "nope"/);
  });

  it('REFUSES BINDING ANOTHER ACTOR’S OWNED SECRET — a binding is not a way around ownership', async () => {
    await secrets.put('probe-key', SENTINEL, { owner: 'probe' });
    await slots.declare('scanner', '0.1.0', [{ name: 'api_key' }]);
    await expect(slots.bind('scanner', 'api_key', 'probe-key')).rejects.toThrow(/belongs to actor:probe/);
  });

  it('allows binding an actor’s OWN secret into its own slot', async () => {
    await secrets.put('probe-key', SENTINEL, { owner: 'probe' });
    await slots.declare('probe', '0.1.0', [{ name: 'api_key' }]);
    await expect(slots.bind('probe', 'api_key', 'probe-key')).resolves.toMatchObject({ secret: 'probe-key' });
  });

  it('allows binding a slot nothing has declared yet — preparing an install is not an error', async () => {
    await secrets.put('stripe-prod', SENTINEL);
    await expect(slots.bind('probe', 'api_key', 'stripe-prod')).resolves.toMatchObject({ slot: 'api_key' });
    // …and it is visible as a grant even though no version asks for it.
    expect(slots.bindings('probe')).toHaveLength(1);
  });
});

describe('resolving a slot', () => {
  beforeEach(async () => {
    await secrets.put('stripe-prod', SENTINEL);
    await slots.declare('probe', '0.1.0', [{ name: 'api_key' }]);
  });

  it('hands over the value, and NOT the secret’s name', async () => {
    await slots.bind('probe', 'api_key', 'stripe-prod');
    const got = await slots.resolveSlot({ identity: asActor('probe'), slot: 'api_key', version: '0.1.0', run: 'r-1' });
    expect(got).toEqual({ slot: 'api_key', value: SENTINEL });
    // The actor's author must not learn the operator's inventory by asking for what they granted.
    expect(JSON.stringify(got)).not.toContain('stripe-prod');
  });

  it('REFUSES a slot the version never declared — the fourth case', async () => {
    await slots.bind('probe', 'other', 'stripe-prod');
    await expect(
      slots.resolveSlot({ identity: asActor('probe'), slot: 'other', version: '0.1.0' })
    ).rejects.toThrow(SlotUndeclared);
  });

  it('refuses a slot declared by a DIFFERENT version of the same actor', async () => {
    // Inheriting a slot from a build you are not running would make the declaration a hint.
    await slots.declare('probe', '0.2.0', [{ name: 'api_key' }, { name: 'webhook_secret' }]);
    await slots.bind('probe', 'webhook_secret', 'stripe-prod');
    await expect(
      slots.resolveSlot({ identity: asActor('probe'), slot: 'webhook_secret', version: '0.1.0' })
    ).rejects.toThrow(SlotUndeclared);
  });

  it('refuses when nothing has declared for this version at all, and says to serve it', async () => {
    await expect(
      slots.resolveSlot({ identity: asActor('probe'), slot: 'api_key', version: '9.9.9' })
    ).rejects.toThrow(/nothing has registered/);
  });

  it('refuses an unbound slot, and the message is the fix', async () => {
    await expect(
      slots.resolveSlot({ identity: asActor('probe'), slot: 'api_key', version: '0.1.0' })
    ).rejects.toThrow(SlotUnbound);
  });

  it('reports a revoked secret as revoked, not as unbound', async () => {
    await slots.bind('probe', 'api_key', 'stripe-prod');
    await secrets.revoke('stripe-prod', 1);
    await expect(
      slots.resolveSlot({ identity: asActor('probe'), slot: 'api_key', version: '0.1.0' })
    ).rejects.toThrow(SecretRevoked);
  });

  it('cannot be used to reach another actor’s slot', async () => {
    await slots.bind('probe', 'api_key', 'stripe-prod');
    await expect(
      slots.resolveSlot({ identity: asActor('scanner'), slot: 'api_key', version: '0.1.0' })
    ).rejects.toThrow(/nothing has registered/);
  });
});

describe('the binding door opens nothing else', () => {
  it('an ACTOR still cannot name an operator secret directly', async () => {
    await secrets.put('stripe-prod', SENTINEL);
    await slots.declare('probe', '0.1.0', [{ name: 'api_key' }]);
    await slots.bind('probe', 'api_key', 'stripe-prod');
    // The slot resolves. Naming the secret over the actor door does not, and that is the property
    // the whole indirection rests on.
    await expect(
      secrets.resolve({ name: 'stripe-prod' }, { kind: 'actor', identity: asActor('probe') })
    ).rejects.toThrow(SecretForbidden);
  });

  it('a binding principal may only ask for the secret it was minted for', async () => {
    await secrets.put('stripe-prod', SENTINEL);
    await secrets.put('do-token', 'other');
    await expect(
      secrets.resolve(
        { name: 'do-token' },
        { kind: 'binding', identity: asActor('probe'), slot: 'api_key', secret: 'stripe-prod' }
      )
    ).rejects.toThrow(/is bound to "stripe-prod", not to "do-token"/);
  });

  it('a binding principal cannot reach another actor’s OWNED secret', async () => {
    await secrets.put('probe-key', SENTINEL, { owner: 'probe' });
    await expect(
      secrets.resolve(
        { name: 'probe-key' },
        { kind: 'binding', identity: asActor('scanner'), slot: 'api_key', secret: 'probe-key' }
      )
    ).rejects.toThrow(SecretForbidden);
  });

  it('the WORKER door is unchanged — an operator secret still resolves at the last hop', async () => {
    await secrets.put('stripe-prod', SENTINEL);
    await expect(secrets.resolve({ name: 'stripe-prod' }, { kind: 'worker' })).resolves.toMatchObject({
      version: 1,
    });
  });
});

describe('the ledger', () => {
  beforeEach(async () => {
    await secrets.put('stripe-prod', SENTINEL);
    await slots.declare('probe', '0.2.0', [{ name: 'api_key' }]);
  });

  it('records (actor, version, slot, run) and the secret NAME — never the value', async () => {
    await slots.bind('probe', 'api_key', 'stripe-prod');
    await slots.resolveSlot({ identity: asActor('probe'), slot: 'api_key', version: '0.2.0', run: 'nscheck-17' });
    const [entry] = slots.log.list();
    expect(entry).toMatchObject({
      actor: 'probe',
      version: '0.2.0',
      slot: 'api_key',
      run: 'nscheck-17',
      secret: 'stripe-prod',
      secretVersion: 1,
      outcome: 'resolved',
    });
    // NOT A FINGERPRINT AND NOT A LENGTH. The store refuses those for a reason and the ledger is a
    // worse place for one than the store is.
    expect(JSON.stringify(entry)).not.toContain(SENTINEL);
    expect(Object.keys(entry ?? {}).sort()).toEqual(
      ['actor', 'at', 'outcome', 'run', 'secret', 'secretVersion', 'slot', 'version'].sort()
    );
  });

  it('RECORDS THE REFUSALS TOO — the interesting event is the one that handed nothing over', async () => {
    await slots
      .resolveSlot({ identity: asActor('probe'), slot: 'api_key', version: '0.2.0', run: 'r-1' })
      .catch(() => undefined);
    await slots
      .resolveSlot({ identity: asActor('probe'), slot: 'nope', version: '0.2.0', run: 'r-1' })
      .catch(() => undefined);
    expect(slots.log.list().map((e) => e.outcome)).toEqual(['undeclared', 'unbound']);
  });

  it('records a revoked binding as revoked', async () => {
    await slots.bind('probe', 'api_key', 'stripe-prod');
    await secrets.revoke('stripe-prod', 1);
    await slots.resolveSlot({ identity: asActor('probe'), slot: 'api_key', version: '0.2.0' }).catch(() => undefined);
    expect(slots.log.list()[0]).toMatchObject({ outcome: 'revoked', secret: 'stripe-prod' });
  });

  it('answers "which actor read my key, and when", filtered', async () => {
    await slots.declare('scanner', '0.1.0', [{ name: 'api_key' }]);
    await slots.bind('probe', 'api_key', 'stripe-prod');
    await slots.bind('scanner', 'api_key', 'stripe-prod');
    await slots.resolveSlot({ identity: asActor('probe'), slot: 'api_key', version: '0.2.0', run: 'r-1' });
    clock += 1000;
    await slots.resolveSlot({ identity: asActor('scanner'), slot: 'api_key', version: '0.1.0', run: 'r-2' });

    expect(slots.log.list({ secret: 'stripe-prod' }).map((e) => e.actor)).toEqual(['scanner', 'probe']);
    expect(slots.log.list({ actor: 'probe' }).map((e) => e.run)).toEqual(['r-1']);
    expect(slots.log.list({ run: 'r-2' }).map((e) => e.actor)).toEqual(['scanner']);
  });

  it('the file on disk holds no part of the value', async () => {
    await slots.bind('probe', 'api_key', 'stripe-prod');
    await slots.resolveSlot({ identity: asActor('probe'), slot: 'api_key', version: '0.2.0' });
    expect(readFileSync(join(dir, 'resolutions.log'), 'utf8')).not.toContain(SENTINEL);
    expect(readFileSync(join(dir, 'slots.json'), 'utf8')).not.toContain(SENTINEL);
  });
});

describe('the run preflight', () => {
  beforeEach(async () => {
    await secrets.put('stripe-prod', SENTINEL);
    await slots.declare('probe', '0.2.0', [{ name: 'api_key' }]);
  });

  it('refuses a run whose actor has an unbound slot, naming the slot', async () => {
    const said = await slots.refuseRun([{ name: 'probe', version: '0.2.0' }]);
    expect(said).toContain('probe@0.2.0');
    expect(said).toContain('api_key');
  });

  it('lets the run through once the slot is bound', async () => {
    await slots.bind('probe', 'api_key', 'stripe-prod');
    expect(await slots.refuseRun([{ name: 'probe', version: '0.2.0' }])).toBeNull();
  });

  it('does not refuse over an actor that has declared nothing', async () => {
    expect(await slots.refuseRun([{ name: 'unknown-actor' }])).toBeNull();
  });

  it('falls back to the newest declaration when the named version never declared, and says which', async () => {
    // Checking nothing would make the gate worthless; claiming to have checked 0.9.0 would be a lie.
    const said = await slots.refuseRun([{ name: 'probe', version: '0.9.0' }]);
    expect(said).toContain('probe@0.2.0');
  });

  it('resolves NOTHING to answer — a preflight that read the credentials would defeat itself', async () => {
    await slots.bind('probe', 'api_key', 'stripe-prod');
    await slots.refuseRun([{ name: 'probe', version: '0.2.0' }]);
    expect(slots.log.list()).toEqual([]);
  });
});
