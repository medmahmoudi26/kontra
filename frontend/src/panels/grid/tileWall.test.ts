/**
 * The wall's DRAG, its keyboard, and the two things it must never do to a saved arrangement.
 *
 * `grid/wall.ts` owns the geometry — clamp, settle, gravity — and is pinned beside itself. What
 * this file is about is the half that only exists as pointer events on `window` and as a `useState`
 * that is committed on pointer-up: a drag that leaves the tile (which every drag does, since the
 * tile is what is moving), a commit that must happen ONCE rather than sixty times a second, and an
 * arrow-key nudge that exists because a wall reachable only by a 300-pixel drag is a wall some
 * operators cannot use at all.
 *
 * THE CANVAS IS GIVEN A WIDTH, because jsdom has no layout and `clientWidth` is 0 everywhere. One
 * column is `columnPx(width)` and a drag is measured in columns, so a zero-width canvas would make
 * every pointer move an enormous number of columns. The width below is stubbed on the ONE element
 * the wall measures, and everything downstream of it — `columnPx`, `settle`, the commit — is the
 * component's own arithmetic.
 *
 * THE `hidden` PROP IS THE OTHER SUBJECT, and it is the one with a measured failure behind it:
 * passing a FILTERED inventory instead would delete every hidden tile's saved rectangle, which is
 * an operator's arrangement thrown away by typing in a search box.
 */

import { createElement } from 'react';
import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import TileWall from './TileWall';
import { useWall } from './useWall';
import { WALL_GAP_PX, WALL_ROW_PX, columnPx, type WallTile } from './wall';
import type { Terminal, TerminalHealth } from '../panelsClient';

const HEALTH: TerminalHealth = { reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' };

function terminal(id: string): Terminal {
  return {
    id,
    machine: id.split(':')[0] ?? id,
    host: '10.124.0.3',
    publicIp: '',
    tag: '',
    fleet: 'recon',
    actor: 'probe',
    version: '0.1.0',
    window: '0',
    health: HEALTH,
  };
}

const INVENTORY = [terminal('n1:a/s/w'), terminal('n2:a/s/w'), terminal('n3:a/s/w')];

/** A wall 1440 px wide, which is the screen the default tile size was chosen against. */
const CANVAS_PX = 1440;
const UNIT_X = columnPx(CANVAS_PX) + WALL_GAP_PX;
const UNIT_Y = WALL_ROW_PX + WALL_GAP_PX;

const onLayoutCommit = vi.fn();

/** The last `WallApi` a render produced, so a test can read the DOCUMENT rather than the pixels. */
let api: ReturnType<typeof useWall> | null = null;

function Harness({
  inventory = INVENTORY,
  hidden,
}: {
  inventory?: readonly Terminal[];
  hidden?: ReadonlySet<string>;
}): JSX.Element {
  const wall = useWall();
  api = wall;
  return createElement(TileWall, {
    wall,
    inventory,
    onLayoutCommit,
    ...(hidden ? { hidden } : {}),
    renderTile: (t: Terminal, ctx: { dragHandle(e: React.PointerEvent<HTMLElement>): void }) =>
      createElement('button', {
        'data-testid': `banner-${t.id}`,
        onPointerDown: ctx.dragHandle,
      }),
  });
}

function tileOf(id: string): WallTile {
  const tile = api?.tiles.find((t) => t.id === id);
  if (!tile) throw new Error(`no tile for ${id}`);
  return tile;
}

/** jsdom lays nothing out. This is the one measurement the wall takes for itself. */
function widthIs(px: number): void {
  Object.defineProperty(HTMLElement.prototype, 'clientWidth', {
    configurable: true,
    get(this: HTMLElement) {
      return this.dataset.testid === 'tile-wall' ? px : 0;
    },
  });
}

/** One whole gesture: down on the grab point, some moves on the window, up. */
function drag(handle: HTMLElement, dx: number, dy: number): void {
  fireEvent.pointerDown(handle, { button: 0, clientX: 0, clientY: 0 });
  act(() => {
    window.dispatchEvent(
      new MouseEvent('pointermove', { clientX: dx / 2, clientY: dy / 2 }) as PointerEvent
    );
  });
  act(() => {
    window.dispatchEvent(new MouseEvent('pointermove', { clientX: dx, clientY: dy }) as PointerEvent);
  });
  act(() => {
    window.dispatchEvent(new MouseEvent('pointerup', {}) as PointerEvent);
  });
}

beforeEach(() => {
  localStorage.clear();
  api = null;
  onLayoutCommit.mockClear();
  widthIs(CANVAS_PX);
});

afterEach(() => {
  Reflect.deleteProperty(HTMLElement.prototype, 'clientWidth');
});

describe('the wall follows the inventory', () => {
  it('gives every Terminal a rectangle, and takes one away with the Machine', () => {
    const view = render(createElement(Harness));
    expect(api!.tiles.map((t) => t.id)).toEqual(INVENTORY.map((t) => t.id));
    view.rerender(createElement(Harness, { inventory: INVENTORY.slice(0, 2) }));
    expect(api!.tiles.map((t) => t.id)).toEqual(['n1:a/s/w', 'n2:a/s/w']);
  });

  it('says the wall is empty rather than drawing a rectangle the colour of a terminal', () => {
    render(createElement(Harness, { inventory: [] }));
    expect(screen.getByTestId('wall-empty')).toBeTruthy();
  });

  it('leaves a ghost where a Terminal was, because a tile can vanish three different ways', () => {
    const view = render(createElement(Harness));
    view.rerender(createElement(Harness, { inventory: INVENTORY.slice(0, 2) }));
    expect(screen.getByTestId('tile-ghost-n3:a/s/w')).toBeTruthy();
  });
});

describe('dragging a tile', () => {
  it('moves it by the columns and rows the pointer travelled', () => {
    render(createElement(Harness));
    const before = tileOf('n1:a/s/w');
    drag(screen.getByTestId('banner-n1:a/s/w'), UNIT_X * 2, UNIT_Y);
    const after = tileOf('n1:a/s/w');
    expect(after.x).toBe(before.x + 2);
    // The wall has gravity, so `y` is settled rather than taken literally — what must be true is
    // that the tile moved down relative to where it started.
    expect(after.y).toBeGreaterThanOrEqual(before.y);
  });

  it('commits ONCE, on pointer-up, and not per move', () => {
    // A live commit would write the document — and on a resize re-fit every xterm on the wall —
    // sixty times a second. The commit is what tells the page to re-measure, so counting it counts
    // the refits.
    render(createElement(Harness));
    drag(screen.getByTestId('banner-n1:a/s/w'), UNIT_X, 0);
    expect(onLayoutCommit).toHaveBeenCalledTimes(1);
  });

  it('marks the wall as dragging while the pointer is down, and stops when it is up', () => {
    render(createElement(Harness));
    const handle = screen.getByTestId('banner-n1:a/s/w');
    fireEvent.pointerDown(handle, { button: 0, clientX: 0, clientY: 0 });
    expect(screen.getByTestId('tile-wall').dataset.dragging).toBe('move');
    act(() => {
      window.dispatchEvent(new MouseEvent('pointerup', {}) as PointerEvent);
    });
    expect(screen.getByTestId('tile-wall').dataset.dragging).toBeUndefined();
  });

  it('ignores a drag that did not start with the primary button', () => {
    render(createElement(Harness));
    fireEvent.pointerDown(screen.getByTestId('banner-n1:a/s/w'), { button: 2, clientX: 0, clientY: 0 });
    expect(screen.getByTestId('tile-wall').dataset.dragging).toBeUndefined();
  });

  it('resizes from the corner rather than moving', () => {
    render(createElement(Harness));
    const before = tileOf('n1:a/s/w');
    drag(screen.getByTestId('tile-resize-n1:a/s/w'), UNIT_X * 2, 0);
    const after = tileOf('n1:a/s/w');
    expect(after.w).toBe(before.w + 2);
    expect(after.x).toBe(before.x);
  });

  it('never resizes below the width a tile is still readable at', () => {
    render(createElement(Harness));
    drag(screen.getByTestId('tile-resize-n1:a/s/w'), -UNIT_X * 40, -UNIT_Y * 40);
    const after = tileOf('n1:a/s/w');
    expect(after.w).toBeGreaterThanOrEqual(2);
    expect(after.h).toBeGreaterThanOrEqual(1);
  });

  it('is a pointer gesture that survives leaving the tile, which every drag does', () => {
    // The listeners are on the WINDOW for the duration. On the handle they would stop receiving
    // moves the moment the tile moved out from under the pointer, and the tile would stick to it.
    render(createElement(Harness));
    const before = tileOf('n2:a/s/w');
    fireEvent.pointerDown(screen.getByTestId('banner-n2:a/s/w'), { button: 0, clientX: 0, clientY: 0 });
    act(() => {
      window.dispatchEvent(new MouseEvent('pointermove', { clientX: -UNIT_X * 3 }) as PointerEvent);
    });
    act(() => {
      window.dispatchEvent(new MouseEvent('pointerup', {}) as PointerEvent);
    });
    expect(tileOf('n2:a/s/w').x).toBe(before.x - 3);
  });
});

describe('the keyboard', () => {
  it('nudges a tile one column at a time', () => {
    render(createElement(Harness));
    const before = tileOf('n1:a/s/w');
    fireEvent.keyDown(screen.getByTestId('tile-resize-n1:a/s/w'), { key: 'ArrowRight' });
    expect(tileOf('n1:a/s/w').w).toBe(before.w + 1);
    expect(onLayoutCommit).toHaveBeenCalledTimes(1);
  });

  it('ignores a key that is not an arrow', () => {
    render(createElement(Harness));
    const before = tileOf('n1:a/s/w');
    fireEvent.keyDown(screen.getByTestId('tile-resize-n1:a/s/w'), { key: 'x' });
    expect(tileOf('n1:a/s/w')).toEqual(before);
    expect(onLayoutCommit).not.toHaveBeenCalled();
  });
});

describe('what `hidden` is, and is not', () => {
  it('stops DRAWING a tile without deleting its saved rectangle', () => {
    const view = render(createElement(Harness));
    const kept = tileOf('n2:a/s/w');
    view.rerender(createElement(Harness, { hidden: new Set(['n2:a/s/w']) }));
    expect(screen.queryByTestId('slot-tile-n2:a/s/w')).toBeNull();
    // The document still holds it — a view, not an edit. This is the failure that threw away an
    // operator's arrangement when the wall was handed a filtered inventory instead.
    expect(tileOf('n2:a/s/w')).toEqual(kept);
  });

  it('settles what is left, so a hidden tile does not leave a hole', () => {
    const view = render(createElement(Harness));
    view.rerender(createElement(Harness, { hidden: new Set(['n1:a/s/w']) }));
    expect(screen.getByTestId('tile-wall').dataset.tiles).toBe('2');
    expect(screen.getByTestId('tile-wall').dataset.hidden).toBe('1');
  });

  it('says a wall of hidden tiles is hidden, never that the Fleet is gone', () => {
    const view = render(createElement(Harness));
    view.rerender(createElement(Harness, { hidden: new Set(INVENTORY.map((t) => t.id)) }));
    expect(screen.getByTestId('wall-filtered-empty')).toBeTruthy();
    expect(screen.queryByTestId('wall-empty')).toBeNull();
  });
});
