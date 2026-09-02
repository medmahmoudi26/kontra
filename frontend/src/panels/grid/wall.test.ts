/**
 * The wall's placement rules.
 *
 * These are the tests the slot document's `layout.test.ts` used to hold, asked of the thing that
 * replaced it. Two of them are the reason this module exists rather than being inlined into the
 * canvas component: `syncWall` is the answer to "does `fleet up --count 10` still fill the wall
 * without an edit", and `parseWall` is the answer to "can an operator recover from a bad saved
 * layout" — and neither can be checked from a green typecheck.
 */

import { describe, expect, it } from 'vitest';

import {
  WALL_COLUMNS,
  WALL_DEFAULT_H,
  WALL_DEFAULT_W,
  WALL_MIN_W,
  WALL_VERSION,
  clampTile,
  columnPx,
  compactWall,
  firstFreeSpot,
  moveTile,
  overlaps,
  parseWall,
  resetWall,
  resizeTile,
  serializeWall,
  settle,
  syncWall,
  tileRect,
  wallRows,
  type WallTile,
} from './wall';

const tile = (id: string, x: number, y: number, w = 6, h = 2): WallTile => ({ id, x, y, w, h });

describe('overlaps', () => {
  it('is false for tiles that only touch', () => {
    // Adjacency is the normal case on a packed wall — if touching counted as overlapping, every
    // settle would push the whole wall apart one row at a time.
    expect(overlaps(tile('a', 0, 0), tile('b', 6, 0))).toBe(false);
    expect(overlaps(tile('a', 0, 0, 6, 2), tile('b', 0, 2, 6, 2))).toBe(false);
  });

  it('is true for a one-cell corner intersection', () => {
    expect(overlaps(tile('a', 0, 0, 6, 2), tile('b', 5, 1, 6, 2))).toBe(true);
  });
});

describe('clampTile', () => {
  it('keeps a tile on the canvas when a drag runs past the right edge', () => {
    // Pointer arithmetic feeds this directly, so a fast drag routinely asks for x=14 on a
    // twelve-column wall. Off the canvas is invisible, which reads as a tile that vanished.
    expect(clampTile(tile('a', 99, 0, 6, 2))).toEqual({ id: 'a', x: WALL_COLUMNS - 6, y: 0, w: 6, h: 2 });
  });

  it('refuses a tile too narrow to read', () => {
    expect(clampTile(tile('a', 0, 0, 0, 2)).w).toBe(WALL_MIN_W);
  });

  it('never lets a tile above the top', () => {
    expect(clampTile(tile('a', 0, -5)).y).toBe(0);
  });

  it('shrinks x rather than w when a wide tile is dragged right', () => {
    // The operator asked for a POSITION; honouring it by narrowing the tile would resize something
    // they were only moving.
    const clamped = clampTile(tile('a', 10, 0, 12, 2));
    expect(clamped).toEqual({ id: 'a', x: 0, y: 0, w: 12, h: 2 });
  });
});

describe('settle', () => {
  it('pushes the tile that was under the drop DOWN, not the one that was dropped', () => {
    // The whole predictability rule: the tile in your hand keeps where you put it.
    const after = settle([tile('held', 0, 0), tile('other', 0, 0)], 'held');
    expect(after.find((t) => t.id === 'held')).toMatchObject({ x: 0, y: 0 });
    expect(after.find((t) => t.id === 'other')).toMatchObject({ x: 0, y: 2 });
  });

  it('closes the hole a departed tile left', () => {
    const after = settle([tile('a', 0, 0), tile('b', 0, 6)], null);
    expect(after.find((t) => t.id === 'b')).toMatchObject({ y: 2 });
  });

  it('never moves a tile sideways', () => {
    // Gravity is vertical only. A tile that slid under a neighbour during a compact would be a wall
    // rearranging itself for reasons the operator did not ask for.
    const after = compactWall([tile('a', 6, 4), tile('b', 0, 9)]);
    expect(after.map((t) => t.x)).toEqual([6, 0]);
  });

  it('keeps two displaced tiles in reading order rather than swapping them', () => {
    const after = settle([tile('held', 0, 0, 12, 2), tile('left', 0, 0), tile('right', 6, 0)], 'held');
    const left = after.find((t) => t.id === 'left');
    const right = after.find((t) => t.id === 'right');
    expect(left).toMatchObject({ x: 0, y: 2 });
    expect(right).toMatchObject({ x: 6, y: 2 });
  });

  it('returns the tiles in the INPUT order', () => {
    // The list is also the React key order, and re-ordering it on every drag would move DOM nodes —
    // which for a tile means disposing an xterm and dropping its stream.
    const after = settle([tile('a', 0, 8), tile('b', 0, 0)], null);
    expect(after.map((t) => t.id)).toEqual(['a', 'b']);
  });

  it('leaves a wall that is already settled exactly as it was', () => {
    const before = [tile('a', 0, 0), tile('b', 6, 0), tile('c', 0, 2)];
    expect(compactWall(before)).toEqual(before);
  });
});

describe('firstFreeSpot', () => {
  it('fills a gap instead of appending below it', () => {
    // A Terminal that appears mid-run should land beside its neighbours; appending would put it at
    // the bottom of a wall an operator has to scroll to find.
    expect(firstFreeSpot([tile('a', 6, 0)], 6, 2)).toEqual({ x: 0, y: 0 });
  });

  it('starts a new row when the wall is full', () => {
    expect(firstFreeSpot([tile('a', 0, 0), tile('b', 6, 0)], 6, 2)).toEqual({ x: 0, y: 2 });
  });

  it('answers for an empty wall', () => {
    expect(firstFreeSpot([], 6, 2)).toEqual({ x: 0, y: 0 });
  });
});

describe('syncWall', () => {
  it('places every Terminal of a fleet that just came up, with no edit', () => {
    // This is the property the slot document existed to protect, asked of the thing that replaced
    // it: `fleet up --count 10` must fill the wall without anyone arranging ten rectangles.
    const ids = Array.from({ length: 10 }, (_, i) => `fleet:kf-0${i}/kontra/0`);
    const tiles = syncWall([], ids);
    expect(tiles).toHaveLength(10);
    expect(tiles.every((t) => t.w === WALL_DEFAULT_W && t.h === WALL_DEFAULT_H)).toBe(true);
    // And no two of them are drawn on top of each other.
    for (let i = 0; i < tiles.length; i += 1) {
      for (let j = i + 1; j < tiles.length; j += 1) {
        expect(overlaps(tiles[i]!, tiles[j]!)).toBe(false);
      }
    }
  });

  it('keeps the rectangle an operator arranged', () => {
    const arranged = [tile('a', 3, 4, 9, 3)];
    expect(syncWall(arranged, ['a'])).toEqual(arranged);
  });

  it('drops a Terminal that left the Fleet', () => {
    expect(syncWall([tile('a', 0, 0), tile('b', 6, 0)], ['a'])).toEqual([tile('a', 0, 0)]);
  });

  it('adds one new Terminal next to the ones already there', () => {
    const after = syncWall([tile('a', 0, 0)], ['a', 'b']);
    expect(after).toHaveLength(2);
    expect(after[1]).toMatchObject({ id: 'b', x: 6, y: 0 });
  });

  it('follows the inventory order for a fresh wall', () => {
    expect(resetWall(['z', 'a']).map((t) => t.id)).toEqual(['z', 'a']);
  });
});

describe('moveTile / resizeTile', () => {
  it('drops a tile where it was put and sends the occupant down', () => {
    const after = moveTile([tile('a', 0, 0), tile('b', 6, 0)], 'b', 0, 0);
    expect(after.find((t) => t.id === 'b')).toMatchObject({ x: 0, y: 0 });
    expect(after.find((t) => t.id === 'a')).toMatchObject({ x: 0, y: 2 });
  });

  it('grows a tile over its neighbour and pushes the neighbour down', () => {
    const after = resizeTile([tile('a', 0, 0), tile('b', 6, 0)], 'a', 12, 2);
    expect(after.find((t) => t.id === 'a')).toMatchObject({ w: 12 });
    expect(after.find((t) => t.id === 'b')).toMatchObject({ y: 2 });
  });

  it('ignores an id that is not on the wall rather than throwing', () => {
    const before = [tile('a', 0, 0)];
    expect(moveTile(before, 'ghost', 4, 4)).toEqual(before);
  });
});

describe('geometry', () => {
  it('measures the canvas height from the lowest tile', () => {
    expect(wallRows([tile('a', 0, 0, 6, 2), tile('b', 6, 3, 6, 1)])).toBe(4);
    expect(wallRows([])).toBe(0);
  });

  it('never returns a zero column width for a canvas measured before layout', () => {
    // A zero here makes every pointer delta an Infinity, and a drag then teleports the tile.
    expect(columnPx(0)).toBeGreaterThan(0);
    expect(columnPx(-100)).toBeGreaterThan(0);
  });

  it('lays a full-width tile out edge to edge', () => {
    const px = columnPx(1200);
    const rect = tileRect(tile('a', 0, 0, WALL_COLUMNS, 1), px);
    expect(Math.round(rect.left)).toBe(0);
    expect(Math.round(rect.width)).toBe(1200);
  });
});

describe('parseWall', () => {
  it('reads back what it wrote', () => {
    const tiles = [tile('a', 0, 0), tile('b', 6, 0)];
    expect(parseWall(serializeWall(tiles))).toEqual({ tiles });
  });

  it('gives a fresh wall and a sentence for junk, rather than throwing', () => {
    // A wall that throws on load is a wall nobody can reset — the page that would let them reset it
    // is the page that will not mount.
    const got = parseWall('{not json');
    expect(got.tiles).toEqual([]);
    expect(got.warning).toContain('readable JSON');
  });

  it('drops a document from another schema version and says so', () => {
    const got = parseWall(JSON.stringify({ version: WALL_VERSION + 1, tiles: [tile('a', 0, 0)] }));
    expect(got.tiles).toEqual([]);
    expect(got.warning).toContain(String(WALL_VERSION));
  });

  it('drops a malformed tile and keeps the rest', () => {
    const got = parseWall(
      JSON.stringify({ version: WALL_VERSION, tiles: [tile('a', 0, 0), { id: 'b' }] })
    );
    expect(got.tiles.map((t) => t.id)).toEqual(['a']);
    expect(got.warning).toContain('1 tile');
  });

  it('un-overlaps a hand-edited document', () => {
    // Overlapping tiles are drawn on top of each other with no error anywhere, so a document that
    // was written by a crashed tab has to be repaired on the way in.
    const got = parseWall(
      JSON.stringify({ version: WALL_VERSION, tiles: [tile('a', 0, 0), tile('b', 0, 0)] })
    );
    expect(overlaps(got.tiles[0]!, got.tiles[1]!)).toBe(false);
  });

  it('treats a missing document as an empty wall with nothing to say', () => {
    expect(parseWall(null)).toEqual({ tiles: [] });
    expect(parseWall('')).toEqual({ tiles: [] });
  });

  it('keeps only the first of two tiles claiming one Terminal', () => {
    const got = parseWall(
      JSON.stringify({ version: WALL_VERSION, tiles: [tile('a', 0, 0), tile('a', 6, 0)] })
    );
    expect(got.tiles).toHaveLength(1);
  });
});
