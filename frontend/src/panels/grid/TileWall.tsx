/**
 * The wall: absolutely-placed tiles you drag by the banner and resize by the corner.
 *
 * NO DRAG-AND-DROP DEPENDENCY, AND THE REASON IS NOT BUNDLE SIZE. Every library in this space —
 * react-grid-layout, dnd-kit, react-dnd with a grid backend — moves the dragged element with a CSS
 * `transform`, and several place tiles that way too. ADR 0020 decision 12 forbids exactly that: a
 * Terminal must not live inside a transformed or zoomable canvas, because `addon-fit`'s measured
 * cols/rows is what the streamer puts into `stty` before `tmux attach`, and a scaled or translated
 * container makes that measurement a lie about the remote pane's shape. A library would also own the
 * mount/unmount decisions, and an unmounted tile disposes its xterm — which clears the screen and
 * drops the stream.
 *
 * SO A DRAG MOVES `left`/`top`, NEVER `transform`. That is the one substantive difference from the
 * slot wall this replaced, which highlighted a drop target and moved nothing: a position change is
 * not a size change, so it fires no `ResizeObserver`, costs no `fit()`, and cannot re-`stty` a live
 * attach. The tile follows the pointer because that is what "smoothly" means, and it is free.
 *
 * A RESIZE DOES CHANGE THE BOX, and that cost is real and unchanged: the `ResizeObserver` inside
 * every tile fires per pointer move. `TerminalTile`'s `SETTLE_MS` debounce is what bounds it to one
 * measurement at the end of the drag — the same arrangement the slot wall's live re-layout had, and
 * that file's header records the measurement. The DOCUMENT is written once, on pointer-up.
 *
 * A TILE IS KEYED BY ITS TERMINAL, so React moves DOM nodes rather than recreating them: an xterm
 * instance survives a drag, a resize, a compact and an inventory refresh. That is the one thing here
 * that cannot be checked from a green typecheck — a remounted tile looks exactly like a Machine that
 * stopped printing.
 *
 * THE GHOST IS THE ONLY THING THAT MOVES DURING A DRAG BESIDES THE TILE. It marks where the tile
 * will land once the wall settles, which on a gravity wall is often not where the pointer is.
 */

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';

import { useTileGhosts } from '../chrome/useTileGhosts';
import '../chrome/chrome.css';
import type { Terminal } from '../panelsClient';
import {
  WALL_COLUMNS,
  WALL_GAP_PX,
  WALL_MIN_H,
  WALL_MIN_W,
  WALL_ROW_PX,
  clampTile,
  columnPx,
  settle,
  tileRect,
  wallRows,
  type WallTile,
} from './wall';
import type { WallApi } from './useWall';

export interface TileWallProps {
  wall: WallApi;
  /** The Terminals to draw, in inventory order. A tile with no Terminal is not drawn — the wall is
   * synced from this list, so that state lasts one render at most. */
  inventory: readonly Terminal[];
  /**
   * `dragHandle` is what makes the BANNER the grab point without this file knowing what a tile's
   * chrome looks like. The alternative — a drag strip drawn here, above the tile — would cost a row
   * of terminal on every tile on the wall, and `grid/wall.ts` records that a tile only has about
   * five.
   */
  renderTile(
    terminal: Terminal,
    context: {
      tile: WallTile;
      /** Too NARROW for the banner in full. */
      compact: boolean;
      /** Too SHORT to spend rows on prose — see {@link DENSE_PX}. */
      dense: boolean;
      dragHandle(e: React.PointerEvent<HTMLElement>): void;
    }
  ): ReactNode;
  /** Called after a geometry change has been committed. The page uses it to make every tile
   * re-measure: a tile resized by drag would otherwise keep an xterm sized for the old rectangle. */
  onLayoutCommit?(): void;
  /** Hand the page each tile's container element, so the sidebar can scroll one into view. A
   * REGISTRY, not a `querySelector` from the page: the latter works today and silently stops working
   * the moment a test hook is renamed. */
  registerTile?(id: string, el: HTMLElement | null): void;
  /** The Terminal the sidebar just revealed. It gets a ring that fades — a reveal, not a selection. */
  revealed?: string | null;
  /**
   * Terminals that must not be DRAWN right now — the filter's non-matches, the panes an operator
   * hid, and the zoomed one (which is drawn full-bleed over this wall instead).
   *
   * A VIEW, NOT AN EDIT, and that distinction is the whole reason this is a separate prop rather
   * than a shorter `inventory`. The wall syncs its saved document to the inventory it is handed, so
   * passing a filtered list would DELETE every hidden tile's saved rectangle — the operator's
   * arrangement, thrown away by typing in a search box. (That is not hypothetical: mounting a wall
   * with an empty inventory wiped exactly that, which is why `WallEmpty` exists.) The document keeps
   * every tile; only what is drawn shrinks, and the drawn tiles are re-settled so a filter that
   * matches two Machines does not leave ten holes where the others were.
   */
  hidden?: ReadonlySet<string>;
}

/** A tile narrower than this many pixels drops the parts of its banner it can afford to lose.
 * Measured against the banner's own content: below ~340 px the identity line and the address line
 * both wrap, which costs two rows of terminal on a tile that has about five. */
const COMPACT_PX = 340;

/**
 * A tile shorter than this drops the health DETAIL to one clipped line.
 *
 * MEASURED on the default two-row tile: banner ~40 px, four health chips wrapping to two rows ~46
 * px, and two detail sentences ~34 px, out of 276 px. That is a quarter of the tile spent on prose
 * which repeats verbatim on every tile of the same Machine, and it left `91×6` — six rows of
 * terminal. The chips are never dropped; only the sentences are clipped, and they keep their place
 * in the DOM and move to the container's `title`. Three rows (420 px) is where a tile can afford
 * them, which is also the size an operator drags a tile to when they want to read it.
 */
const DENSE_PX = 3 * WALL_ROW_PX;

interface Drag {
  id: string;
  mode: 'move' | 'resize';
  startX: number;
  startY: number;
  /** The tile as it was when the pointer went down. */
  base: WallTile;
  dx: number;
  dy: number;
}

export default function TileWall({
  wall,
  inventory,
  renderTile,
  onLayoutCommit,
  registerTile,
  revealed = null,
  hidden,
}: TileWallProps): JSX.Element {
  const canvasRef = useRef<HTMLDivElement | null>(null);
  const [width, setWidth] = useState(0);
  const [drag, setDrag] = useState<Drag | null>(null);
  /** The tile the operator last touched. It floats above the others and keeps a ring, so a wall of
   * near-identical journals does not lose the one that was just moved. */
  const [focused, setFocused] = useState<string | null>(null);

  /** Which tiles just left the wall, and what they were called. See `chrome/useTileGhosts.ts`. */
  const ids = useMemo(() => inventory.map((t) => t.id), [inventory]);
  const labels = useMemo(() => {
    const map = new Map<string, string>();
    for (const t of inventory) map.set(t.id, `${t.machine} · ${t.window}`);
    return map;
  }, [inventory]);
  const ghosts = useTileGhosts(ids, labels);

  // The wall follows the inventory. Every poll, because that is what makes `fleet up --count 10`
  // fill it with no edit and no reload; `sync` is a no-op when nothing changed.
  const idKey = ids.join('\n');
  const syncWallTiles = wall.sync;
  useEffect(() => {
    syncWallTiles(idKey === '' ? [] : idKey.split('\n'));
  }, [idKey, syncWallTiles]);

  // The canvas's own width, which is what a column is measured from. A `ResizeObserver` rather than
  // `window.resize`: the sidebar and the detail drawer are flex SIBLINGS, so opening either changes
  // this width without the window changing at all.
  useEffect(() => {
    const el = canvasRef.current;
    if (!el || typeof ResizeObserver === 'undefined') {
      if (el) setWidth(el.clientWidth);
      return;
    }
    const observer = new ResizeObserver(() => setWidth(el.clientWidth));
    observer.observe(el);
    setWidth(el.clientWidth);
    return () => observer.disconnect();
  }, []);

  const col = columnPx(width);
  const unitX = col + WALL_GAP_PX;
  const unitY = WALL_ROW_PX + WALL_GAP_PX;

  /** What is DRAWN. Identical to the document unless a filter is on, in which case the surviving
   *  tiles fall upward into the space the hidden ones left — see `hidden` on the props. */
  const layout = useMemo(() => {
    if (!hidden || hidden.size === 0) return wall.tiles;
    return settle(
      wall.tiles.filter((t) => !hidden.has(t.id)),
      null
    );
  }, [hidden, wall.tiles]);

  /** Where the dragged tile would land once the wall settles. Computed from the SAME pure functions
   * the commit uses, so the ghost cannot promise a position the drop does not produce. */
  const preview = useMemo(() => {
    if (!drag) return null;
    const asked =
      drag.mode === 'move'
        ? { ...drag.base, x: drag.base.x + drag.dx / unitX, y: drag.base.y + drag.dy / unitY }
        : { ...drag.base, w: drag.base.w + drag.dx / unitX, h: drag.base.h + drag.dy / unitY };
    const wanted = clampTile(asked);
    const settled = settle(
      layout.map((t) => (t.id === drag.id ? wanted : t)),
      drag.id
    );
    return settled.find((t) => t.id === drag.id) ?? wanted;
  }, [drag, layout, unitX, unitY]);

  const start = useCallback(
    (e: React.PointerEvent<HTMLElement>, tile: WallTile, mode: Drag['mode']): void => {
      if (e.button !== 0) return;
      e.preventDefault();
      e.stopPropagation();
      setFocused(tile.id);
      setDrag({ id: tile.id, mode, startX: e.clientX, startY: e.clientY, base: tile, dx: 0, dy: 0 });
    },
    []
  );

  // The pointer listeners live on the WINDOW for the duration of the drag rather than on the handle.
  // A drag that leaves the tile — which every drag does, since the tile is what is moving — would
  // otherwise stop receiving moves, and the tile would stick to the pointer with no way to drop it.
  useEffect(() => {
    if (!drag) return;
    const move = (e: PointerEvent): void => {
      setDrag((prev) =>
        prev === null ? prev : { ...prev, dx: e.clientX - prev.startX, dy: e.clientY - prev.startY }
      );
    };
    const up = (): void => {
      setDrag((prev) => {
        if (prev === null) return null;
        // Committed HERE rather than per pointer move. A live commit would write the document — and
        // on a resize re-fit every xterm on the wall — sixty times a second.
        if (prev.mode === 'move') {
          wall.move(
            prev.id,
            prev.base.x + Math.round(prev.dx / unitX),
            prev.base.y + Math.round(prev.dy / unitY)
          );
        } else {
          wall.resize(
            prev.id,
            Math.max(WALL_MIN_W, prev.base.w + Math.round(prev.dx / unitX)),
            Math.max(WALL_MIN_H, prev.base.h + Math.round(prev.dy / unitY))
          );
        }
        return null;
      });
      onLayoutCommit?.();
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', up);
    window.addEventListener('pointercancel', up);
    return () => {
      window.removeEventListener('pointermove', move);
      window.removeEventListener('pointerup', up);
      window.removeEventListener('pointercancel', up);
    };
  }, [drag, onLayoutCommit, unitX, unitY, wall]);

  const byId = useMemo(() => new Map(inventory.map((t) => [t.id, t])), [inventory]);
  const rows = wallRows(layout);

  /** Nudge a tile with the keyboard. A pointer-only wall is unusable for anyone who cannot make a
   * 300-pixel drag, and the banner is already focusable because it is a button. */
  const nudge = useCallback(
    (e: React.KeyboardEvent, tile: WallTile, mode: 'move' | 'resize'): void => {
      const step =
        e.key === 'ArrowLeft' ? [-1, 0] : e.key === 'ArrowRight' ? [1, 0]
        : e.key === 'ArrowUp' ? [0, -1] : e.key === 'ArrowDown' ? [0, 1] : null;
      if (!step) return;
      e.preventDefault();
      const [dx, dy] = step as [number, number];
      if (mode === 'move') wall.move(tile.id, tile.x + dx, tile.y + dy);
      else wall.resize(tile.id, tile.w + dx, tile.h + dy);
      onLayoutCommit?.();
    },
    [onLayoutCommit, wall]
  );

  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col gap-1">
      <div
        ref={canvasRef}
        data-testid="tile-wall"
        data-columns={WALL_COLUMNS}
        data-tiles={layout.length}
        data-hidden={hidden && hidden.size > 0 ? hidden.size : undefined}
        data-dragging={drag ? drag.mode : undefined}
        className="min-h-0 flex-1 overflow-auto pr-1"
      >
        <div
          className="relative"
          style={{ height: `${Math.max(1, rows) * unitY - WALL_GAP_PX}px`, width: '100%' }}
        >
          {preview && (
            // Where it will land once gravity has had its say — which on a wall that closes holes
            // upward is frequently not where the pointer is. Without it, dropping a tile two rows
            // above where you were holding it reads as the wall ignoring the drag.
            <div
              data-testid="tile-ghost-target"
              className="pointer-events-none absolute rounded border border-dashed border-primary/70 bg-primary/10"
              style={{ ...pxRect(tileRect(preview, col)), zIndex: 0 }}
            />
          )}

          {layout.map((tile) => {
            const terminal = byId.get(tile.id);
            if (!terminal) return null;
            const dragging = drag?.id === tile.id;
            const rect = tileRect(tile, col);
            // Only the DRAGGED tile follows the pointer in pixels; every other tile sits at the
            // settled geometry, so the wall rearranges under it live.
            if (dragging && drag.mode === 'move') {
              rect.left += drag.dx;
              rect.top += drag.dy;
            } else if (dragging) {
              rect.width = Math.max(120, rect.width + drag.dx);
              rect.height = Math.max(72, rect.height + drag.dy);
            }
            const compact = rect.width < COMPACT_PX;
            const dense = rect.height < DENSE_PX;
            return (
              <div
                // Keyed by the TERMINAL. A tile that moves on the wall or survives an inventory
                // refresh keeps its component instance — and therefore its xterm, its screen and its
                // stream. It is also what makes the enter animation correct for free: a CSS
                // animation plays when an element is CREATED, so a tile that merely moves never
                // replays it, and only a genuinely new Terminal fades in.
                key={tile.id}
                ref={(el) => registerTile?.(tile.id, el)}
                // `slot-tile-`, not `tile-`: `tile-empty-<id>` is slice 2's hook on the tile's "no
                // session" overlay, and a prefix selector for one must never match the other.
                data-testid={`slot-tile-${tile.id}`}
                data-revealed={revealed === tile.id ? 'true' : undefined}
                data-dragging={dragging ? 'true' : undefined}
                // `tabIndex={-1}` so the page can move focus here when the sidebar reveals it —
                // programmatically reachable, never a tab-stop.
                tabIndex={-1}
                className={`kontra-tile-enter kontra-tile-frame absolute flex flex-col overflow-hidden rounded border bg-card outline-none ${
                  focused === tile.id ? 'border-primary/60' : 'border-border'
                }`}
                style={{
                  ...pxRect(rect),
                  zIndex: dragging ? 40 : focused === tile.id ? 20 : 1,
                  boxShadow: dragging ? '0 18px 40px rgba(0,0,0,.45)' : undefined,
                  // The box is NOT transitioned while it is being dragged, and neither dimension is
                  // ever transitioned on a resize: interpolating width or height fires the tile's
                  // `ResizeObserver` sixty times, and each one is a `fit()`.
                  transition: drag
                    ? 'none'
                    : 'left .16s cubic-bezier(.2,.7,.3,1), top .16s cubic-bezier(.2,.7,.3,1)',
                }}
                onPointerDownCapture={() => setFocused(tile.id)}
              >
                {renderTile(terminal, {
                  tile,
                  compact,
                  dense,
                  dragHandle: (e) => start(e, tile, 'move'),
                })}

                {/* The drag handle is the BANNER, which the tile renders — so it is passed down
                    rather than drawn here. What is drawn here is the resize corner, because it
                    belongs to the rectangle and not to the Terminal inside it. */}
                <button
                  type="button"
                  data-testid={`tile-resize-${tile.id}`}
                  aria-label={`resize ${tile.id}`}
                  title="drag to resize, or use the arrow keys"
                  className="absolute bottom-0 right-0 h-4 w-4 cursor-nwse-resize rounded-br border-b-2 border-r-2 border-muted-foreground/50 bg-transparent"
                  onPointerDown={(e) => start(e, tile, 'resize')}
                  onKeyDown={(e) => nudge(e, tile, 'resize')}
                />
              </div>
            );
          })}

          {layout.length === 0 &&
            (hidden && hidden.size > 0 ? (
              // A wall whose tiles are all hidden is NOT an empty wall, and saying so with
              // `WallEmpty` would tell an operator their Fleet is gone when they have only mistyped
              // a hostname or hidden the last pane.
              //
              // CAUSE-NEUTRAL WORDING, because this component genuinely does not know which it was:
              // `hidden` is the union of the filter's set and the panes an operator hid, and naming
              // the filter would send somebody to clear a filter that is not on. Both routes back
              // are on the page above — the filter bar and the "N hidden" list.
              <p
                data-testid="wall-filtered-empty"
                className="absolute inset-x-0 top-0 rounded border border-dashed p-4 text-center text-xs text-muted-foreground"
              >
                No Terminal is being drawn. {hidden.size} {hidden.size === 1 ? 'is' : 'are'} hidden —
                clear the filter, or restore them from the list above the wall.
              </p>
            ) : (
              <WallEmpty className="absolute inset-x-0 top-0" />
            ))}
        </div>
      </div>

      {ghosts.length > 0 && (
        // A tile can vanish for three different reasons — the node left the Fleet, its session was
        // killed, the streamer stopped reporting it — and all three look identical: a rectangle that
        // is no longer there. These say WHICH Terminal left, for a fifth of a second, and hold no
        // xterm and no subscription (see `chrome/useTileGhosts.ts`).
        <div className="flex flex-wrap gap-2" data-testid="wall-ghosts">
          {ghosts.map((ghost) => (
            <span
              key={ghost.id}
              data-testid={`tile-ghost-${ghost.id}`}
              className="kontra-tile-ghost rounded border border-dashed px-2 py-1 text-[11px] text-muted-foreground"
            >
              {ghost.label} left the wall
            </span>
          ))}
        </div>
      )}
    </div>
  );
}

/**
 * What an empty wall says, in one place.
 *
 * Exported because the PAGE renders it when the inventory is empty, rather than mounting a
 * `TileWall` to say it. MEASURED, and the bug was mine: a `TileWall` syncs its layout to the
 * inventory it is given, and on first paint the inventory is `[]` because the fetch has not
 * returned — so mounting one for the empty case wiped the operator's saved arrangement to disk
 * before the real Terminals arrived, and every tile came back at its default size after a reload.
 * A wall must only ever sync against an inventory that was actually read.
 */
export function WallEmpty({ className }: { className?: string }): JSX.Element {
  return (
    <div
      data-testid="wall-empty"
      className={`rounded border border-dashed p-4 text-center text-xs text-muted-foreground ${className ?? ''}`}
    >
      No Terminals. <code className="font-mono">kontra fleet up</code> creates Machines;{' '}
      <code className="font-mono">kontra workflow serve … --tmux</code> creates a local session. A
      Machine with no session is still a tile, so an empty wall means there is nothing at all — if
      you expected some, the panels streamer is a forked child of{' '}
      <code className="font-mono">orchestrator-infra</code> and that container being up does not
      mean the child is.
    </div>
  );
}

function pxRect(rect: { left: number; top: number; width: number; height: number }): {
  left: string;
  top: string;
  width: string;
  height: string;
} {
  return {
    left: `${rect.left}px`,
    top: `${rect.top}px`,
    width: `${rect.width}px`,
    height: `${rect.height}px`,
  };
}
