/**
 * The wall as free-placed rectangles — one per Terminal (ADR 0020 decision 9, revised).
 *
 * WHAT CHANGED, AND WHY IT IS NOT A REGRESSION. The wall used to be a list of SLOTS, each holding a
 * selector, with tiles materialised by evaluating those selectors against the inventory. The reason
 * given was `fleet up --count 10`: ten new Machines should fill the wall with no edit, and an
 * absolute rectangle per tile "cannot express that without inventing a rectangle per Machine at
 * materialisation time".
 *
 * So this file inventes one, on purpose, in {@link syncWall} — and that turns out to be the whole
 * job rather than a workaround. A Terminal that appears gets a rectangle at the first free spot; a
 * Terminal that leaves takes its rectangle with it. `fleet up --count 10` still fills the wall with
 * no edit, and in exchange an operator can put the crawler next to its handler by DRAGGING it there,
 * which a flow of selector slots could never do. What is genuinely given up is the saved *query* —
 * "one tile per Machine with role=crawler" as a durable statement. Nothing else read it: the sidebar
 * tree groups the whole inventory, and `selectors.ts` survives for the ordering the tree uses.
 *
 * STILL NO TRANSFORMS. ADR 0020 decision 12 forbids a Terminal inside a transformed or zoomable
 * canvas, because `addon-fit`'s measured cols/rows is what the streamer `stty`s before `tmux attach`
 * and a scaled container makes that measurement a lie. Tiles here are positioned with `left`/`top`
 * and sized with `width`/`height` in pixels — never `transform`, never `scale`. A drag therefore
 * changes a tile's POSITION and not its size, so it fires no `ResizeObserver` and costs no `fit()`;
 * a resize does change the size, at the same cost the old slot resize had, and `TerminalTile`'s
 * `SETTLE_MS` debounce is what bounds it.
 *
 * EVERY OPERATION IS A PURE FUNCTION OF A TILE LIST. No React, no `localStorage`, no `Date.now()`.
 * `useWall.ts` owns the state and the persistence; this file is what a test can drive.
 *
 * {@link parseWall} NEVER THROWS, for the reason the slot document had the same rule: a wall that
 * throws on load is a wall nobody can reset, because the thing that would let them reset it is the
 * page that will not mount.
 */

/** Columns the canvas is divided into. A tile's `x` and `w` are measured in these. */
export const WALL_COLUMNS = 12;

/** Pixel height of one row unit, and the gap between tiles. A tile is `h` rows tall.
 *
 * 132 px is MEASURED against the chrome a tile carries, the same way the slot wall's row heights
 * were: two banner lines (~34 px) plus slice 3's health chips (~22 px) leaves ~76 px of terminal,
 * which at 12 px type is about 5 rows — readable for a journal follower and small enough that a
 * default `h: 2` tile shows a real screen.
 *
 * THE TWO BANNER LINES ARE NO LONGER BOTH IN THE BANNER, and this number did not move because of it.
 * The status line went from row 2 of `chrome/TileHeader` to the FOOT of the tile ("like tmux"). Both
 * places are siblings in the same flex column, and the element is the same one — so the sum of the
 * chrome is unchanged by construction, and nothing here had to be re-measured. What was NOT allowed
 * is the version of that change where the bar is added at the bottom and the header keeps two rows:
 * that is a third row, and a third row costs every tile on this wall a line of a Worker's output.
 * It is the same reason the bar REPLACED a header row when it was first introduced. */
export const WALL_ROW_PX = 132;
export const WALL_GAP_PX = 12;

/** What a new Terminal gets: half the wall, two rows. Wide enough that an 80-column line does not
 * wrap on a 1440 px screen, which is the width most of this output is written for. */
export const WALL_DEFAULT_W = 6;
export const WALL_DEFAULT_H = 2;

/** The narrowest a tile may be dragged. One column of a twelve-column wall is ~100 px, which is
 * fewer than 12 terminal columns — a tile nobody can read is a tile that looks broken. */
export const WALL_MIN_W = 2;
export const WALL_MIN_H = 1;

/** Bump when a change to {@link WallTile} cannot be read by {@link parseWall} as-is. Anything stored
 * under a different version is dropped for a fresh wall rather than guessed at. */
export const WALL_VERSION = 1;

/** One key per browser. Versioned in the VALUE, not the key: an operator clearing a stale key by
 * hand should not be the migration path. */
export const WALL_STORAGE_KEY = 'kontra-monitor-wall';

/** One Terminal's rectangle, in grid units. */
export interface WallTile {
  /** The Terminal id — `<mode>:<node>/<session>/<window>`. */
  id: string;
  x: number;
  y: number;
  w: number;
  h: number;
}

/** The stored envelope. Shaped for a future `GET/PUT /api/panels/wall` the way the slot store was:
 * moving to the server is then a transport change, not a schema change. */
export interface WallStore {
  version: number;
  tiles: WallTile[];
}

export function overlaps(a: WallTile, b: WallTile): boolean {
  return a.x < b.x + b.w && a.x + a.w > b.x && a.y < b.y + b.h && a.y + a.h > b.y;
}

/** Force a rectangle onto the canvas: inside the columns, at least the minimum size, never above
 * the top. Callers pass pointer arithmetic straight in, so this is the only guard. */
export function clampTile(tile: WallTile): WallTile {
  const w = Math.min(WALL_COLUMNS, Math.max(WALL_MIN_W, Math.round(tile.w)));
  const h = Math.max(WALL_MIN_H, Math.round(tile.h));
  const x = Math.min(WALL_COLUMNS - w, Math.max(0, Math.round(tile.x)));
  const y = Math.max(0, Math.round(tile.y));
  return { id: tile.id, x, y, w, h };
}

/**
 * Resolve overlaps, then let everything fall.
 *
 * `pinned` is the tile the operator is holding: it keeps the rectangle it was given and everything
 * that collides with it is pushed DOWN, in reading order, until it fits. Then every tile — the
 * pinned one first, so it settles where it was dropped rather than under whatever used to be there
 * — rises as far as it can without colliding.
 *
 * Push-down-then-gravity, rather than a packing solver, because the operator has to be able to
 * PREDICT it while dragging: a tile dropped on top of another sends that one down, and holes close
 * upward. `null` means nothing is pinned, which is exactly the Compact button.
 */
export function settle(tiles: readonly WallTile[], pinned: string | null): WallTile[] {
  const working = tiles.map((t) => ({ ...t }));
  const held = pinned === null ? undefined : working.find((t) => t.id === pinned);

  // Reading order for the push-down, so two tiles displaced by the same drop keep their relative
  // arrangement instead of swapping.
  const rest = working
    .filter((t) => t !== held)
    .sort((a, b) => a.y - b.y || a.x - b.x);

  const placed: WallTile[] = held ? [held] : [];
  for (const tile of rest) {
    while (placed.some((p) => overlaps(p, tile))) tile.y += 1;
    placed.push(tile);
  }

  // Gravity. Re-sorted because the push-down changed who is topmost, and a tile can only rise into
  // space above tiles that have already been settled.
  const risen: WallTile[] = [];
  for (const tile of [...placed].sort((a, b) => a.y - b.y || a.x - b.x)) {
    while (tile.y > 0 && !risen.some((p) => overlaps(p, { ...tile, y: tile.y - 1 }))) tile.y -= 1;
    risen.push(tile);
  }

  // The INPUT's order is preserved in the output: the tile list is also the React key order, and
  // re-ordering it on every drag would move DOM nodes for no reason.
  const byId = new Map(working.map((t) => [t.id, t]));
  return tiles.map((t) => byId.get(t.id) ?? t);
}

/**
 * The first rectangle of this size that fits, scanning top-to-bottom then left-to-right.
 *
 * Bounded by the tallest occupied row plus one, so a wall with a gap in it fills the gap and a full
 * wall appends a new row. This is what makes a Terminal that appears mid-run land NEXT TO its
 * neighbours instead of at the bottom of a long scroll.
 */
export function firstFreeSpot(tiles: readonly WallTile[], w: number, h: number): { x: number; y: number } {
  const width = Math.min(WALL_COLUMNS, Math.max(WALL_MIN_W, w));
  const height = Math.max(WALL_MIN_H, h);
  const bottom = tiles.reduce((max, t) => Math.max(max, t.y + t.h), 0);
  for (let y = 0; y <= bottom; y += 1) {
    for (let x = 0; x + width <= WALL_COLUMNS; x += 1) {
      const candidate = { id: '', x, y, w: width, h: height };
      if (!tiles.some((t) => overlaps(t, candidate))) return { x, y };
    }
  }
  return { x: 0, y: bottom };
}

/**
 * Bring the wall in line with the inventory: drop what has gone, place what is new.
 *
 * THIS IS THE FUNCTION THE SLOT DOCUMENT EXISTED TO AVOID WRITING, and it is nine lines. A Terminal
 * that leaves the Fleet takes its rectangle with it rather than leaving a hole an operator has to
 * tidy; a Terminal that appears — `fleet up --count 10`, or a `kontra workflow serve` that just
 * created a `kontra-*` session — gets one at the first free spot, so the wall fills without an edit
 * and without a reload.
 *
 * Order follows `ids`, so a fresh wall reads in the inventory's own order.
 */
export function syncWall(tiles: readonly WallTile[], ids: readonly string[]): WallTile[] {
  const known = new Map(tiles.map((t) => [t.id, t]));
  const out: WallTile[] = [];
  for (const id of ids) {
    const existing = known.get(id);
    if (existing) {
      out.push(existing);
      continue;
    }
    const spot = firstFreeSpot(out, WALL_DEFAULT_W, WALL_DEFAULT_H);
    out.push({ id, x: spot.x, y: spot.y, w: WALL_DEFAULT_W, h: WALL_DEFAULT_H });
  }
  return out;
}

/** Move one tile and let the wall resolve around it. */
export function moveTile(tiles: readonly WallTile[], id: string, x: number, y: number): WallTile[] {
  const next = tiles.map((t) => (t.id === id ? clampTile({ ...t, x, y }) : t));
  return settle(next, id);
}

/** Resize one tile and let the wall resolve around it. */
export function resizeTile(tiles: readonly WallTile[], id: string, w: number, h: number): WallTile[] {
  const next = tiles.map((t) => (t.id === id ? clampTile({ ...t, w, h }) : t));
  return settle(next, id);
}

/** Close every gap without moving anything sideways — the Compact button. */
export function compactWall(tiles: readonly WallTile[]): WallTile[] {
  return settle(tiles, null);
}

/** Throw the arrangement away and lay the Terminals out in inventory order. */
export function resetWall(ids: readonly string[]): WallTile[] {
  return syncWall([], ids);
}

/** How tall the canvas has to be, in rows. */
export function wallRows(tiles: readonly WallTile[]): number {
  return tiles.reduce((max, t) => Math.max(max, t.y + t.h), 0);
}

/** A tile's pixel rectangle, given the measured width of one column. */
export function tileRect(
  tile: WallTile,
  columnPx: number
): { left: number; top: number; width: number; height: number } {
  return {
    left: tile.x * (columnPx + WALL_GAP_PX),
    top: tile.y * (WALL_ROW_PX + WALL_GAP_PX),
    width: tile.w * columnPx + (tile.w - 1) * WALL_GAP_PX,
    height: tile.h * WALL_ROW_PX + (tile.h - 1) * WALL_GAP_PX,
  };
}

/** One column in pixels, from the canvas's measured width. Never zero: a canvas measured before
 * layout would otherwise make every tile a point and every pointer delta an infinity. */
export function columnPx(canvasWidth: number): number {
  const usable = canvasWidth - WALL_GAP_PX * (WALL_COLUMNS - 1);
  return Math.max(40, usable / WALL_COLUMNS);
}

function isTile(raw: unknown): raw is WallTile {
  if (!raw || typeof raw !== 'object') return false;
  const t = raw as Record<string, unknown>;
  return (
    typeof t.id === 'string' &&
    t.id !== '' &&
    typeof t.x === 'number' &&
    typeof t.y === 'number' &&
    typeof t.w === 'number' &&
    typeof t.h === 'number' &&
    Number.isFinite(t.x) &&
    Number.isFinite(t.y) &&
    Number.isFinite(t.w) &&
    Number.isFinite(t.h)
  );
}

/**
 * Read a stored wall. Never throws; returns an empty wall and a sentence when it cannot.
 *
 * A wall with no tiles is not an error — {@link syncWall} fills it from the inventory on the next
 * render — so "could not read it" and "there was nothing to read" produce the same tiles and differ
 * only in whether an operator is told. The page renders the warning, because a layout that silently
 * reset looks exactly like a layout that never saved.
 */
export function parseWall(raw: string | null): { tiles: WallTile[]; warning?: string } {
  if (raw === null || raw.trim() === '') return { tiles: [] };
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { tiles: [], warning: 'the saved Monitor layout was not readable JSON, so the wall was laid out fresh' };
  }
  if (!parsed || typeof parsed !== 'object') {
    return { tiles: [], warning: 'the saved Monitor layout was not a layout, so the wall was laid out fresh' };
  }
  const store = parsed as Partial<WallStore>;
  if (store.version !== WALL_VERSION) {
    return {
      tiles: [],
      warning:
        `the saved Monitor layout is version ${String(store.version)} and this build reads ` +
        `${WALL_VERSION}, so the wall was laid out fresh`,
    };
  }
  if (!Array.isArray(store.tiles)) {
    return { tiles: [], warning: 'the saved Monitor layout had no tiles, so the wall was laid out fresh' };
  }
  const seen = new Set<string>();
  const tiles: WallTile[] = [];
  let dropped = 0;
  for (const raw of store.tiles) {
    if (!isTile(raw) || seen.has(raw.id)) {
      dropped += 1;
      continue;
    }
    seen.add(raw.id);
    tiles.push(clampTile(raw));
  }
  // Settled on read: a hand-edited or half-written document can overlap, and an overlapping wall is
  // tiles drawn on top of each other with no error anywhere.
  const settled = settle(tiles, null);
  if (dropped > 0) {
    return {
      tiles: settled,
      warning: `${dropped} tile${dropped === 1 ? '' : 's'} in the saved Monitor layout could not be read and ${dropped === 1 ? 'was' : 'were'} dropped`,
    };
  }
  return { tiles: settled };
}

export function serializeWall(tiles: readonly WallTile[]): string {
  return JSON.stringify({ version: WALL_VERSION, tiles: [...tiles] } satisfies WallStore);
}
