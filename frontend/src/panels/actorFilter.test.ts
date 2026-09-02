/**
 * Narrowing the Actors grid — the decision, and the row that drives it.
 *
 * WHAT THESE GUARD is the pair of ways a filter goes wrong in a way nobody notices: it hides
 * something the operator asked to see, or it hides something and does not say so. The first is
 * covered by the match tests below; the second by the count, which is asserted to be drawn even when
 * nothing is filtered, because a grid that quietly loses eight cards reads as a catalog that lost
 * eight Actors.
 *
 * `renderToStaticMarkup` for the reason `HealthChips.test.ts` records: node env, no jsdom, and every
 * assertion here is about text and `data-testid`, both of which are in the markup.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import {
  ANY,
  actorFilterActive,
  actorNames,
  matchActor,
  EMPTY_ACTOR_FILTER,
  type ActorFilter,
  type ActorStanding,
  type FilterableActor,
} from './actorFilter';
import { ActorFilterBar } from './ActorFilterBar';

function actor(over: Partial<FilterableActor> = {}): FilterableActor {
  return {
    name: 'cachebuster',
    version: '0.3.0',
    key: 'cachebuster@0.3.0',
    source: '/srv/checkout/examples/python/cachebuster',
    operations: [{ name: 'probe', description: 'Asks one origin for one path twice.' }],
    ...over,
  };
}

const SERVING: ActorStanding = { serving: 2, machines: 2, onDisk: true };
const DARK: ActorStanding = { serving: 0, machines: 0, onDisk: false };

function filter(over: Partial<ActorFilter> = {}): ActorFilter {
  return { ...EMPTY_ACTOR_FILTER, ...over };
}

describe('the empty filter', () => {
  it('is not active, so the clear button is not drawn for it', () => {
    expect(actorFilterActive(EMPTY_ACTOR_FILTER)).toBe(false);
  });

  it('keeps every Actor, in every standing', () => {
    // The default is "show me the page". A sentinel that accidentally narrowed would empty the grid
    // on first paint, which reads as an installation with nothing registered.
    expect(matchActor(actor(), EMPTY_ACTOR_FILTER, SERVING)).toBe(true);
    expect(matchActor(actor(), EMPTY_ACTOR_FILTER, DARK)).toBe(true);
  });

  it('counts whitespace as no query', () => {
    expect(actorFilterActive(filter({ query: '   ' }))).toBe(false);
    expect(matchActor(actor(), filter({ query: '   ' }), DARK)).toBe(true);
  });

  it('is active as soon as any one clause is set', () => {
    expect(actorFilterActive(filter({ query: 'probe' }))).toBe(true);
    expect(actorFilterActive(filter({ name: 'probe' }))).toBe(true);
    expect(actorFilterActive(filter({ servingOnly: true }))).toBe(true);
    expect(actorFilterActive(filter({ onDiskOnly: true }))).toBe(true);
  });
});

describe('the name menu', () => {
  it('collapses the versions, which is the whole point of it', () => {
    const names = actorNames([
      actor({ name: 'cachebuster', version: '0.1.0' }),
      actor({ name: 'cachebuster', version: '0.4.0' }),
      actor({ name: 'beacon' }),
    ]);
    expect(names).toEqual(['beacon', 'cachebuster']);
  });

  it('offers nothing for an empty page rather than a stale list', () => {
    expect(actorNames([])).toEqual([]);
  });
});

describe('the query box', () => {
  it('is case-insensitive over the name', () => {
    expect(matchActor(actor(), filter({ query: 'CACHE' }), SERVING)).toBe(true);
  });

  it('searches the version, which is how two checkouts get told apart', () => {
    expect(matchActor(actor({ version: '0.4.0' }), filter({ query: '0.4' }), SERVING)).toBe(true);
    expect(matchActor(actor({ version: '0.3.0' }), filter({ query: '0.4' }), SERVING)).toBe(false);
  });

  it('searches the source path', () => {
    expect(matchActor(actor(), filter({ query: 'examples/python' }), SERVING)).toBe(true);
  });

  it('searches every Method name and its author’s sentence', () => {
    // A Method is the unit (ADR 0023 §9). Somebody typing `origin` is looking for the Actor that
    // declares the Method, and the Actor's name is the half they do not remember.
    expect(matchActor(actor(), filter({ query: 'origin' }), SERVING)).toBe(true);
    expect(matchActor(actor(), filter({ query: 'probe' }), SERVING)).toBe(true);
  });

  it('says no when nothing on the card holds the text', () => {
    expect(matchActor(actor(), filter({ query: 'smuggling' }), SERVING)).toBe(false);
  });

  it('survives an Actor with no Methods and no source — the unserved folder’s stand-in', () => {
    // A folder nobody has served is drawn from a stand-in with empty operations. A haystack that
    // threw or read `undefined` here would hide exactly the card register → edit → serve starts on.
    const unserved = actor({ operations: [], source: undefined });
    expect(matchActor(unserved, filter({ query: 'cache' }), DARK)).toBe(true);
    expect(matchActor(unserved, filter({ query: 'origin' }), DARK)).toBe(false);
  });
});

describe('the two state toggles', () => {
  it('serving means a healthy Machine, not a Machine', () => {
    // A worker whose probe is unhappy is not serving whatever the catalog says, and an operator
    // asking what is up does not want the dead one back.
    const unhappy: ActorStanding = { serving: 0, machines: 3, onDisk: true };
    expect(matchActor(actor(), filter({ servingOnly: true }), unhappy)).toBe(false);
    expect(matchActor(actor(), filter({ servingOnly: true }), SERVING)).toBe(true);
  });

  it('on-disk means a folder that is actually there', () => {
    const absent: ActorStanding = { serving: 1, machines: 1, onDisk: false };
    expect(matchActor(actor(), filter({ onDiskOnly: true }), absent)).toBe(false);
    expect(matchActor(actor(), filter({ onDiskOnly: true }), SERVING)).toBe(true);
  });
});

describe('the clauses are ANDed', () => {
  it('needs every set clause, which is what a row of controls implies', () => {
    const f = filter({ name: 'cachebuster', query: 'origin', servingOnly: true });
    expect(matchActor(actor(), f, SERVING)).toBe(true);
    expect(matchActor(actor(), f, DARK)).toBe(false);
    expect(matchActor(actor({ name: 'beacon' }), f, SERVING)).toBe(false);
  });

  it('takes the name EXACTLY, so `probe` does not drag in `probe-lite`', () => {
    expect(matchActor(actor({ name: 'probe-lite' }), filter({ name: 'probe' }), SERVING)).toBe(
      false
    );
  });

  it('treats the sentinel as “every name”, not as a name', () => {
    expect(matchActor(actor({ name: ANY }), filter({ name: ANY }), SERVING)).toBe(true);
    expect(matchActor(actor({ name: '' }), filter({ name: ANY }), SERVING)).toBe(true);
  });
});

describe('the filter row, drawn', () => {
  const draw = (over: Partial<ActorFilter> = {}, shown = 4, total = 9): string =>
    renderToStaticMarkup(
      createElement(ActorFilterBar, {
        filter: filter(over),
        names: ['beacon', 'cachebuster'],
        onChange: () => {},
        shown,
        total,
      })
    );

  it('says how many of how many even when nothing is filtered', () => {
    // NEVER CONDITIONAL. The denominator is the defence against a filter reading as a page that
    // lost cards, and it only defends if it is there before the operator touches anything.
    const html = draw({}, 9, 9);
    expect(html).toContain('showing 9 of 9 actors');
  });

  it('carries the two numbers as data, so the count cannot drift from the grid', () => {
    const html = draw({ query: 'probe' });
    expect(html).toContain('data-shown="4"');
    expect(html).toContain('data-total="9"');
  });

  it('says “actor” for one', () => {
    expect(draw({}, 1, 1)).toContain('showing 1 of 1 actor<');
  });

  it('offers every name on the page and an all-actors option', () => {
    const html = draw();
    expect(html).toContain('all actors');
    expect(html).toContain('>beacon<');
    expect(html).toContain('>cachebuster<');
  });

  it('draws clear only once something is narrowed', () => {
    expect(draw()).not.toContain('actor-filter-clear');
    expect(draw({ servingOnly: true })).toContain('actor-filter-clear');
  });

  it('marks an engaged toggle, because a filter nobody can see is applied is the bug', () => {
    expect(draw({ servingOnly: true })).toContain('data-testid="actor-filter-serving" data-active');
    expect(draw({})).not.toContain('data-active');
  });
});
