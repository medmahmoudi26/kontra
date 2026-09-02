/**
 * The selector model, pinned (ADR 0020 decision 9, slice 4).
 *
 * Three of this module's properties are invisible in a browser, which is why they are asserted here
 * rather than left to the Playwright suite:
 *
 *   - **a stable order** — a wall that reshuffles between refreshes still renders, still streams and
 *     still passes every e2e assertion; it just moves under the operator's cursor;
 *   - **one claim per Terminal** — a second tile for the same id gets no frames (the page routes each
 *     tagged frame to one writer per id) and looks like a Machine that stopped printing;
 *   - **a tile key that follows the Terminal** — the wrong key remounts the tile, which disposes its
 *     xterm, which clears the screen and drops the stream. That is the failure mode of this slice.
 */

import { describe, expect, it } from 'vitest';
import {
  compareNatural,
  compareTerminals,
  describeSelector,
  inventoryValues,
  isPinnedToMachine,
  materialiseSlots,
  matchesSelector,
  normalizeSelector,
  orderTerminals,
  SELECTOR_KEYS,
  selectorTerms,
  selectTerminals,
  tileKey,
  WALL_TILE_CAP,
  type SelectableTerminal,
} from './selectors';

const SESSION = 'kontra-webcrawl';

function terminal(machine: string, window: string, over: Partial<SelectableTerminal> = {}): SelectableTerminal {
  return {
    id: `fleet:${machine}/${SESSION}/${window}`,
    machine,
    tag: 'crawl',
    fleet: 'fleet-apex-119',
    actor: 'webcrawl',
    window,
    ...over,
  };
}

/** A Fleet shaped like the one ADR 0020 sizes for, in an order deliberately not the wall's. */
function fleet(count = 3): SelectableTerminal[] {
  const out: SelectableTerminal[] = [];
  for (let i = count; i >= 1; i -= 1) {
    const machine = `kf-crawl-${String(i).padStart(2, '0')}`;
    out.push(terminal(machine, 'handler'), terminal(machine, 'actor'));
  }
  return out;
}

describe('a selector is any subset of the five keys', () => {
  it('keeps only usable terms, and drops a blank rather than matching on it', () => {
    // `''` is a REAL value in this inventory: CONTRACT.md's Terminal carries `actor: ''` for a Machine
    // with no placement. So a blank input box has to mean "don't care" and not "actor is empty" —
    // otherwise clearing a field silently selects exactly the unplaced Machines.
    expect(normalizeSelector({ tag: 'crawl', actor: '', window: '  ' })).toEqual({ tag: 'crawl' });
    expect(normalizeSelector({ tag: '  crawl  ' })).toEqual({ tag: 'crawl' });
    expect(normalizeSelector({ nonsense: 'x', tag: 3, machine: null })).toEqual({});
    expect(normalizeSelector(null)).toEqual({});
    expect(normalizeSelector('role=crawl')).toEqual({});
  });

  it('matches every present term and ignores the absent ones', () => {
    const t = terminal('kf-crawl-01', 'actor');
    expect(matchesSelector({}, t)).toBe(true);
    expect(matchesSelector({ tag: 'crawl' }, t)).toBe(true);
    expect(matchesSelector({ tag: 'crawl', window: 'actor' }, t)).toBe(true);
    expect(matchesSelector({ tag: 'crawl', window: 'handler' }, t)).toBe(false);
    // Exact and case-sensitive: these are stack outputs and ids, not search text.
    expect(matchesSelector({ tag: 'Crawl' }, t)).toBe(false);
    expect(matchesSelector({ machine: 'kf-crawl-0' }, t)).toBe(false);
  });

  it('does not match an unplaced Machine on an actor term', () => {
    const unplaced = terminal('kf-crawl-09', 'actor', { actor: '' });
    expect(matchesSelector({ actor: 'webcrawl' }, unplaced)).toBe(false);
    // …and it is still on the wall, because a Machine with no actor is a visible tile (ADR 0020).
    expect(matchesSelector({}, unplaced)).toBe(true);
  });

  it('shows its terms in chip order, machine first', () => {
    const terms = selectorTerms({ window: 'actor', machine: 'kf-crawl-01', tag: 'crawl' });
    expect(terms.map((t) => t.key)).toEqual(['machine', 'tag', 'window']);
    expect(SELECTOR_KEYS[0]).toBe('machine');
    expect(isPinnedToMachine({ machine: 'kf-crawl-01' })).toBe(true);
    expect(isPinnedToMachine({ tag: 'crawl' })).toBe(false);
  });

  it('has words for the empty selector, because an empty chip row explains nothing', () => {
    expect(describeSelector({})).toBe('every Terminal in the Fleet inventory');
    expect(describeSelector({ tag: 'crawl', window: 'actor' })).toBe('tag=crawl and window=actor');
  });
});

describe('the wall order is total and stable', () => {
  it('orders machine then window, and does not depend on the inventory order', () => {
    const ordered = orderTerminals(fleet(3));
    expect(ordered.map((t) => `${t.machine}/${t.window}`)).toEqual([
      'kf-crawl-01/actor',
      'kf-crawl-01/handler',
      'kf-crawl-02/actor',
      'kf-crawl-02/handler',
      'kf-crawl-03/actor',
      'kf-crawl-03/handler',
    ]);
    // The same set in any other arrival order is the same wall. This is the property that keeps a
    // 30-second inventory refresh from moving tiles under the cursor.
    const shuffled = [...fleet(3)].reverse();
    expect(orderTerminals(shuffled).map((t) => t.id)).toEqual(ordered.map((t) => t.id));
  });

  it('sorts kf-crawl-2 before kf-crawl-10', () => {
    // `fleet up --count 10` is the case ADR 0020 names, and lexicographic ordering puts `-10` second.
    const ids = orderTerminals([
      terminal('kf-crawl-10', 'actor'),
      terminal('kf-crawl-2', 'actor'),
      terminal('kf-crawl-1', 'actor'),
    ]).map((t) => t.machine);
    expect(ids).toEqual(['kf-crawl-1', 'kf-crawl-2', 'kf-crawl-10']);
  });

  it('never calls two different names equal', () => {
    // A compare that returns 0 for distinct strings hands the decision to `sort`'s stability, which
    // means the fetch order decides — and the fetch order is a `Map` iteration on the streamer.
    expect(compareNatural('kf-crawl-01', 'kf-crawl-1')).not.toBe(0);
    expect(compareNatural('kf-crawl-01', 'kf-crawl-01')).toBe(0);
    expect(Math.sign(compareNatural('a1', 'a01'))).toBe(-Math.sign(compareNatural('a01', 'a1')));
    const a = terminal('kf-crawl-1', 'actor');
    const b = terminal('kf-crawl-01', 'actor');
    expect(compareTerminals(a, b)).not.toBe(0);
    expect(Math.sign(compareTerminals(a, b))).toBe(-Math.sign(compareTerminals(b, a)));
  });

  it('selects in wall order, not in inventory order', () => {
    const actors = selectTerminals({ window: 'actor' }, fleet(3));
    expect(actors.map((t) => t.machine)).toEqual(['kf-crawl-01', 'kf-crawl-02', 'kf-crawl-03']);
  });

  it('offers the values the inventory actually has', () => {
    const inventory = [
      terminal('kf-crawl-02', 'actor'),
      terminal('kf-api-01', 'handler', { tag: 'api', actor: '' }),
      terminal('kf-crawl-01', 'actor'),
    ];
    expect(inventoryValues('tag', inventory)).toEqual(['api', 'crawl']);
    expect(inventoryValues('machine', inventory)).toEqual(['kf-api-01', 'kf-crawl-01', 'kf-crawl-02']);
    // A Machine with no placement contributes no actor value — an editor offering `actor=''` offers a
    // term that cannot be typed and would not mean what it looked like.
    expect(inventoryValues('actor', inventory)).toEqual(['webcrawl']);
  });
});

describe('slots materialise into tiles, one claim per Terminal', () => {
  const slots = [
    { id: 's1', selector: { window: 'actor' } },
    { id: 's2', selector: { window: 'handler' } },
  ];

  it('gives each slot its matches, in wall order', () => {
    const m = materialiseSlots(slots, fleet(2));
    expect(m.slots[0]?.terminals.map((t) => t.id)).toEqual([
      `fleet:kf-crawl-01/${SESSION}/actor`,
      `fleet:kf-crawl-02/${SESSION}/actor`,
    ]);
    expect(m.slots[1]?.terminals.map((t) => t.window)).toEqual(['handler', 'handler']);
    // The flattened order is slot order then wall order — which is the DOM order the page builds, so
    // "the first tile is the first Terminal of the first slot" is a property and not a coincidence.
    expect(m.tiles.map((t) => `${t.machine}/${t.window}`)).toEqual([
      'kf-crawl-01/actor',
      'kf-crawl-02/actor',
      'kf-crawl-01/handler',
      'kf-crawl-02/handler',
    ]);
    expect(m.unmatched).toBe(0);
  });

  it('lets the first slot win a contested Terminal, and tells the second so', () => {
    // Two overlapping selectors is a normal thing to build by accident: `role=crawl` and
    // `window=actor` overlap on every crawl actor. The second tile would receive no frames at all,
    // because the page keeps one writer per Terminal id — so the overlap has to be visible.
    const overlapping = [
      { id: 's1', selector: { tag: 'crawl' } },
      { id: 's2', selector: { window: 'actor' } },
    ];
    const m = materialiseSlots(overlapping, fleet(2));
    expect(m.slots[0]?.terminals).toHaveLength(4);
    expect(m.slots[1]?.terminals).toHaveLength(0);
    expect(m.slots[1]?.claimedByEarlierSlot).toBe(2);
    // One tile per Terminal on the wall, whatever the slots asked for.
    expect(new Set(m.tiles.map((t) => t.id)).size).toBe(m.tiles.length);
  });

  it('counts what no slot matched rather than hiding it', () => {
    const m = materialiseSlots([{ id: 's1', selector: { window: 'actor' } }], fleet(3));
    expect(m.tiles).toHaveLength(3);
    expect(m.unmatched).toBe(3); // the three handler windows
  });

  it('caps the wall and reports the overflow once per Terminal', () => {
    const big = fleet(20); // 40 Terminals
    const m = materialiseSlots([{ id: 's1', selector: {} }], big, { cap: 6 });
    expect(m.tiles).toHaveLength(6);
    expect(m.overCap).toBe(34);
    expect(m.slots[0]?.overCap).toBe(34);
    // Two slots both matching an over-cap Terminal each report it, but the WALL must not claim more
    // Terminals than the Fleet has: 6 shown + 34 over cap + 0 unmatched = 40.
    const twice = materialiseSlots(
      [
        { id: 's1', selector: {} },
        { id: 's2', selector: {} },
      ],
      big,
      { cap: 6 }
    );
    expect(twice.overCap).toBe(34);
    expect(twice.tiles.length + twice.overCap + twice.unmatched).toBe(big.length);
    expect(WALL_TILE_CAP).toBeGreaterThanOrEqual(24); // 12 Machines × 2 windows, the ADR's target
  });

  it('is deterministic across a re-materialisation with the same set', () => {
    // What a 30-second inventory refresh does. Same tiles, same order, same keys — so React keeps
    // every component instance and no xterm is disposed.
    const a = materialiseSlots(slots, fleet(4));
    const b = materialiseSlots(slots, [...fleet(4)].reverse());
    expect(b.tiles.map(tileKey)).toEqual(a.tiles.map(tileKey));
  });

  it('keys a tile by its Terminal, so a tile that changes slot is not remounted', () => {
    // The key is what React uses to decide "same component or new one", and a new one disposes the
    // old xterm: the screen clears and the stream stops. A Terminal that moves between slots —
    // because an earlier slot was deleted, or a selector was edited — must keep its key.
    const before = materialiseSlots(
      [
        { id: 's1', selector: { tag: 'crawl' } },
        { id: 's2', selector: { window: 'actor' } },
      ],
      fleet(1)
    );
    const after = materialiseSlots([{ id: 's2', selector: { window: 'actor' } }], fleet(1));
    const actorId = `fleet:kf-crawl-01/${SESSION}/actor`;
    expect(before.slots[0]?.terminals.map(tileKey)).toContain(actorId);
    expect(after.slots[0]?.terminals.map(tileKey)).toEqual([actorId]);
    expect(tileKey(terminal('kf-crawl-01', 'actor'))).toBe(actorId);
  });

  it('normalizes the selector a slot arrives with', () => {
    const m = materialiseSlots(
      [{ id: 's1', selector: { tag: 'crawl', actor: '' } as Record<string, string> }],
      fleet(1)
    );
    expect(m.slots[0]?.terminals).toHaveLength(2);
  });
});
