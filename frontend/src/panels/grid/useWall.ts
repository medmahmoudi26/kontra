/**
 * The wall as React state, persisted to `localStorage`.
 *
 * The only file in the grid that touches storage, and deliberately thin: every edit is a pure
 * function from `wall.ts`, so what a test drives is the tile list, not a hook.
 *
 * WRITES ARE BEST-EFFORT AND FAILURES ARE VISIBLE. `localStorage.setItem` throws on a full quota and
 * in a browser with storage disabled, and a wall that silently stops saving is worse than one that
 * says it cannot: the operator finds out by losing an arrangement they spent ten minutes on.
 *
 * NOT ZUSTAND, for the reason the slot document was not either: this document is read once on mount,
 * written on an operator's edit, and must survive a schema change by RESETTING rather than merging.
 * The app store's job is the opposite — heal what it can, field by field.
 */

import { useCallback, useEffect, useRef, useState } from 'react';

import {
  WALL_STORAGE_KEY,
  compactWall,
  moveTile,
  parseWall,
  resetWall,
  resizeTile,
  serializeWall,
  syncWall,
  type WallTile,
} from './wall';

export interface WallApi {
  tiles: WallTile[];
  /** One sentence when the saved wall could not be used, or could not be written. Rendered on the
   * page, never swallowed. */
  warning: string | null;
  dismissWarning(): void;
  /** Bring the wall in line with the inventory. Idempotent, and a no-op when nothing changed — it
   * runs on every inventory poll. */
  sync(ids: readonly string[]): void;
  move(id: string, x: number, y: number): void;
  resize(id: string, w: number, h: number): void;
  compact(): void;
  reset(ids: readonly string[]): void;
}

function read(): { tiles: WallTile[]; warning?: string } {
  try {
    return parseWall(globalThis.localStorage?.getItem(WALL_STORAGE_KEY) ?? null);
  } catch (err) {
    // Reading storage can itself throw — a browser with cookies-and-site-data blocked throws on
    // ACCESS to `localStorage`, not on the value. The wall still has to come up.
    return {
      tiles: [],
      warning: `this browser would not let the Monitor read its saved layout (${String(err)})`,
    };
  }
}

/** Did the arrangement actually change? `sync` runs on every inventory poll, and replacing the array
 * by identity would re-render the wall — and therefore every tile — for nothing. */
function same(a: readonly WallTile[], b: readonly WallTile[]): boolean {
  if (a.length !== b.length) return false;
  return a.every((t, i) => {
    const other = b[i];
    return (
      other !== undefined &&
      t.id === other.id &&
      t.x === other.x &&
      t.y === other.y &&
      t.w === other.w &&
      t.h === other.h
    );
  });
}

export function useWall(): WallApi {
  const initial = useRef<{ tiles: WallTile[]; warning?: string } | null>(null);
  initial.current ??= read();

  const [tiles, setTiles] = useState<WallTile[]>(initial.current.tiles);
  const [warning, setWarning] = useState<string | null>(initial.current.warning ?? null);
  /** Suppress the first write: mounting must not overwrite a saved wall with the empty one this
   * hook holds for the render before `sync` has seen the inventory. */
  const loaded = useRef(false);

  useEffect(() => {
    if (!loaded.current) {
      loaded.current = true;
      return;
    }
    try {
      globalThis.localStorage?.setItem(WALL_STORAGE_KEY, serializeWall(tiles));
    } catch (err) {
      setWarning(`the Monitor could not save this layout (${String(err)})`);
    }
  }, [tiles]);

  const edit = useCallback((fn: (prev: readonly WallTile[]) => WallTile[]): void => {
    setTiles((prev) => {
      const next = fn(prev);
      return same(prev, next) ? prev : next;
    });
  }, []);

  return {
    tiles,
    warning,
    dismissWarning: useCallback(() => setWarning(null), []),
    sync: useCallback((ids: readonly string[]) => edit((prev) => syncWall(prev, ids)), [edit]),
    move: useCallback((id: string, x: number, y: number) => edit((prev) => moveTile(prev, id, x, y)), [edit]),
    resize: useCallback((id: string, w: number, h: number) => edit((prev) => resizeTile(prev, id, w, h)), [edit]),
    compact: useCallback(() => edit(compactWall), [edit]),
    reset: useCallback((ids: readonly string[]) => edit(() => resetWall(ids)), [edit]),
  };
}
