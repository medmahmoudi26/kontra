/**
 * The sentences the two slot surfaces put on screen, as pure comparisons.
 *
 * NOTHING HERE TOUCHES THE DOM, so the vocabulary is pinned separately from the markup — and the
 * vocabulary is the part that decides whether an operator does the right thing. "revoked" drawn as
 * "not bound" sends them to re-grant a credential that is already granted, and the page would look
 * identical afterwards.
 */

import { describe, expect, it } from 'vitest';

import type { ActorSlots, Resolution, SlotBinding, SlotStatus } from '../run/api';
import {
  SLOT_WORD,
  bindingGroups,
  blocking,
  resolutionRows,
  summariseSlots,
  unbindConfirm,
  unusedBindings,
  versionDiff,
} from './slots';

const AUG = (day: number): number => Date.UTC(2026, 7, day, 9, 14, 0);

const slot = (over: Partial<SlotStatus> & { slot: string }): SlotStatus => ({
  state: 'unbound',
  detail: 'declared, and nothing is bound to it',
  ...over,
});

const view = (over: Partial<ActorSlots> = {}): ActorSlots => ({
  actor: 'probe',
  version: '0.2.0',
  slots: [slot({ slot: 'api_key' })],
  added: [],
  ...over,
});

describe('the four states are four words', () => {
  it('never spells two of them the same', () => {
    expect(new Set(Object.values(SLOT_WORD)).size).toBe(4);
  });

  it('does not call a revoked secret "not bound"', () => {
    // The two need different acts: bind something, versus write a new version of what is bound.
    expect(SLOT_WORD.revoked).not.toBe(SLOT_WORD.unbound);
    expect(SLOT_WORD.missing).not.toBe(SLOT_WORD.unbound);
  });

  it('counts everything but `bound` as blocking, the way the server refuses', () => {
    const rows = [
      slot({ slot: 'a', state: 'bound' }),
      slot({ slot: 'b', state: 'unbound' }),
      slot({ slot: 'c', state: 'revoked' }),
      slot({ slot: 'd', state: 'missing' }),
    ];
    expect(blocking(rows).map((r) => r.slot)).toEqual(['b', 'c', 'd']);
  });
});

describe('the summary line', () => {
  it('says a run will be refused, not "1 slot"', () => {
    expect(summariseSlots(view())).toMatch(/refused at the start/);
  });

  it('says so plainly when everything is bound', () => {
    expect(summariseSlots(view({ slots: [slot({ slot: 'api_key', state: 'bound' })] }))).toBe(
      'Asks for 1 credential, all bound.'
    );
  });

  it('has a sentence for an actor that asks for nothing', () => {
    expect(summariseSlots(view({ slots: [] }))).toMatch(/no credentials/);
  });
});

describe('a new version that adds a slot', () => {
  it('names the version, the version before it, and the slot', () => {
    const said = versionDiff(view({ added: ['webhook_secret'], comparedWith: '0.1.0' })) ?? '';
    expect(said).toContain('0.2.0');
    expect(said).toContain('0.1.0');
    expect(said).toContain('webhook_secret');
  });

  it('says nothing for a FIRST version, which is not the same as "added nothing"', () => {
    // A first version has `added` populated (everything is new) and no `comparedWith`. Drawing a
    // change strip there would report a diff against a version that never existed.
    expect(versionDiff(view({ added: ['api_key'] }))).toBeNull();
  });

  it('says nothing when a version only dropped a slot', () => {
    expect(versionDiff(view({ added: [], comparedWith: '0.1.0' }))).toBeNull();
  });
});

describe('the binding groups', () => {
  const grant = (over: Partial<SlotBinding> = {}): SlotBinding => ({
    actor: 'probe',
    slot: 'api_key',
    secret: 'stripe-prod',
    boundAt: AUG(20),
    ...over,
  });

  it('lists an actor that has GRANTS but has never registered', () => {
    // The state a careful operator preparing an install is in. A page that showed nothing here
    // would read as "it did not save".
    const [group] = bindingGroups([], [grant()]);
    expect(group?.actor).toBe('probe');
    expect(group?.version).toBe('');
    expect(group?.unused.map((b) => b.slot)).toEqual(['api_key']);
  });

  it('draws a grant no declared version asks for, rather than hiding it', () => {
    const groups = bindingGroups([view()], [grant(), grant({ slot: 'gone_in_0_2' })]);
    expect(groups[0]?.unused.map((b) => b.slot)).toEqual(['gone_in_0_2']);
  });

  it('counts what blocks a run, per actor', () => {
    const groups = bindingGroups(
      [view({ slots: [slot({ slot: 'a', state: 'bound' }), slot({ slot: 'b' })] })],
      []
    );
    expect(groups[0]?.blocking).toBe(1);
  });

  it('is empty when nothing has declared and nothing is granted', () => {
    expect(bindingGroups([], [])).toEqual([]);
  });

  it('finds no unused grant when every binding is for a declared slot', () => {
    expect(unusedBindings(view(), [grant()])).toEqual([]);
  });
});

describe('the ledger', () => {
  const entry = (over: Partial<Resolution> = {}): Resolution => ({
    at: AUG(25),
    actor: 'probe',
    version: '0.2.0',
    slot: 'api_key',
    run: 'nscheck-17',
    secret: 'stripe-prod',
    secretVersion: 3,
    outcome: 'resolved',
    ...over,
  });

  it('answers "which actor read my key, and when" in one sentence', () => {
    const [row] = resolutionRows([entry()]);
    expect(row?.when).toBe('2026-08-25 09:14');
    expect(row?.sentence).toBe('probe@0.2.0 read "stripe-prod" v3 through slot "api_key" for run nscheck-17');
    expect(row?.refused).toBe(false);
  });

  it('draws a REFUSAL as a refusal, and names which kind', () => {
    const [row] = resolutionRows([entry({ outcome: 'undeclared', secret: '', secretVersion: 0 })]);
    expect(row?.refused).toBe(true);
    expect(row?.sentence).toMatch(/was refused slot "api_key"/);
    expect(row?.sentence).toMatch(/not declared/);
  });

  it('says nothing about a run when the worker named none, rather than inventing one', () => {
    const [row] = resolutionRows([entry({ run: '' })]);
    expect(row?.sentence).not.toContain('for run');
  });

  it('has a word for every outcome the server can record', () => {
    const outcomes: Resolution['outcome'][] = [
      'resolved',
      'undeclared',
      'unbound',
      'revoked',
      'missing',
      'forbidden',
    ];
    for (const outcome of outcomes) {
      expect(resolutionRows([entry({ outcome })])[0]?.word).toBeTruthy();
    }
  });
});

describe('withdrawing a grant', () => {
  it('says what stops working, and that the secret itself is untouched', () => {
    const said = unbindConfirm({ actor: 'probe', slot: 'api_key', secret: 'stripe-prod' });
    expect(said).toContain('probe');
    expect(said).toContain('api_key');
    expect(said).toContain('stripe-prod');
    expect(said).toMatch(/refused at the start/);
    expect(said).toMatch(/secret itself is untouched/);
  });
});
