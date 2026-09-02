/**
 * Panes an operator has taken off the wall, and the fact that hiding one has to COST LESS, not the
 * same.
 *
 * "And add the ability to hide a pane." Three properties, and none of them is `display:none`:
 *
 *   1. HIDDEN MUST NOT LOOK LIKE GONE. A tile that silently disappears is indistinguishable from a
 *      Worker that died, which is the confusion this whole surface exists to end — `TileWall`'s
 *      ghosts, the `no session` scrim and the four-signal chip row are all the same argument. So the
 *      count is always on the page (`chrome/HiddenPanes.tsx`), it names each pane, and it puts them
 *      back. A hidden set that is not rendered anywhere is a bug, not a feature.
 *
 *   2. HIDING STOPS THE COST, NOT THE PIXELS. The filter already hides tiles, and what it saves is
 *      nothing at all: the id stays in `DashboardPage`'s wanted set, so the streamer keeps taking its
 *      `capture-pane` every three seconds and keeps sending the bytes to a tile that is not there.
 *      This set is subtracted from that wanted set instead, so hiding UNSUBSCRIBES — see
 *      `DashboardPage.sync` for exactly what the streamer then stops doing and what it does not.
 *      That difference is most of the value on a wall of many tiles, and it is the reason this is a
 *      separate concept from the filter rather than another clause in `matchPane`.
 *
 *      MEASURED against the running streamer, one client and one Terminal, twelve seconds either
 *      side of an `unsubscribe`: 5 binary frames while subscribed, 0 after, and no further
 *      `{t:'state'}` for that id.
 *
 *   3. IT SURVIVES A RELOAD, under `localStorage`, the same treatment `grid/wall.ts` gives the
 *      arrangement — versioned in the VALUE, never throwing on read, and dropped wholesale rather
 *      than migrated when the version does not match.
 *
 * THE GRAIN IS ONE WINDOW — one tile, one id, `<mode>:<node>/<session>/<window>`. Not a session and
 * not a Machine. A `kontra` session's two windows are an actor and its handler and an operator
 * hiding a noisy handler must not lose the actor beside it; a control that took the whole Machine
 * would be doing more than the tile it sits on says. The affordance says `window` in as many words
 * so nobody has to infer it.
 *
 * PURE FUNCTIONS PLUS A THIN HOOK, the split `grid/wall.ts` and `grid/useWall.ts` use, because what a
 * test should drive is the id list and not a React hook.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

/** One key per browser, beside the wall's. Versioned in the VALUE, not the key: an operator clearing
 * a stale key by hand should not be the migration path. */
export const HIDDEN_STORAGE_KEY = 'kontra-monitor-hidden';

/** Bump when a stored document cannot be read as-is. Anything else is dropped for an empty set — a
 * hidden set that fails open shows every pane, which is the safe direction: the failure mode of
 * guessing is a wall with panes missing and no explanation. */
export const HIDDEN_VERSION = 1;

export interface HiddenStore {
  version: number;
  /** Terminal ids. Shaped as an array rather than a set for the same reason `WallStore` holds one:
   *  this document is aimed at a future `GET/PUT /api/panels/hidden`. */
  ids: string[];
}

/**
 * Read a stored hidden set. NEVER THROWS, and never returns a partial one.
 *
 * The rule `parseWall` states applies here with more force: a document that throws on load takes the
 * page down, and the page is the only thing that could have un-hidden anything. Unreadable means
 * "nothing is hidden", plus a sentence, because a set that quietly reset looks exactly like one that
 * never saved — and in this case "quietly reset" is the harmless direction and still has to be said.
 */
export function parseHidden(raw: string | null): { ids: string[]; warning?: string } {
  if (raw === null || raw.trim() === '') return { ids: [] };
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { ids: [], warning: 'the saved list of hidden panes was not readable JSON, so every pane is shown' };
  }
  if (!parsed || typeof parsed !== 'object') {
    return { ids: [], warning: 'the saved list of hidden panes was not a list, so every pane is shown' };
  }
  const store = parsed as Partial<HiddenStore>;
  if (store.version !== HIDDEN_VERSION) {
    return {
      ids: [],
      warning:
        `the saved list of hidden panes is version ${String(store.version)} and this build reads ` +
        `${HIDDEN_VERSION}, so every pane is shown`,
    };
  }
  if (!Array.isArray(store.ids)) {
    return { ids: [], warning: 'the saved list of hidden panes had no ids, so every pane is shown' };
  }
  const seen = new Set<string>();
  const ids: string[] = [];
  let dropped = 0;
  for (const id of store.ids) {
    if (typeof id !== 'string' || id === '' || seen.has(id)) {
      dropped += 1;
      continue;
    }
    seen.add(id);
    ids.push(id);
  }
  if (dropped > 0) {
    return {
      ids,
      warning: `${dropped} entr${dropped === 1 ? 'y' : 'ies'} in the saved list of hidden panes could not be read`,
    };
  }
  return { ids };
}

export function serializeHidden(ids: readonly string[]): string {
  return JSON.stringify({ version: HIDDEN_VERSION, ids: [...ids] } satisfies HiddenStore);
}

/** Hide or restore one id. Order is INSERTION order, so the affordance lists the most recently
 *  hidden pane last and an operator can undo their way back out. */
export function toggleHidden(ids: readonly string[], id: string): string[] {
  return ids.includes(id) ? ids.filter((x) => x !== id) : [...ids, id];
}

/**
 * WHAT THE SOCKET SHOULD BE SUBSCRIBED TO — on the wall, MEASURED, and not hidden.
 *
 * Pure and here rather than inline in `DashboardPage.sync`, because the whole claim hiding makes is
 * about this list: fed to `reconnect.resubscribePlan` it produces an `unsubscribe` for a pane the
 * moment it is hidden, which is what takes the window out of the streamer's `capture-pane` argument
 * list and gives up any attach it held. A property stated in a comment is a property that quietly
 * becomes false; this one is a function a test can drive.
 *
 * `measured` is anything with a `has` — `DashboardPage` passes the Map of reported geometries. A tile
 * that has not measured itself has not laid out, and subscribing it would ask a node for a
 * zero-sized screen.
 */
export function wantedSubscriptions(
  tiles: readonly { id: string }[],
  measured: { has(id: string): boolean },
  hidden: ReadonlySet<string>
): string[] {
  return tiles.filter((t) => measured.has(t.id) && !hidden.has(t.id)).map((t) => t.id);
}

export interface HiddenApi {
  /** What the wall must not draw and the page must not subscribe. */
  ids: ReadonlySet<string>;
  /** The same thing in the order they were hidden, for a list a person reads. */
  order: readonly string[];
  /** One sentence when the saved set could not be read or written. Rendered, never swallowed. */
  warning: string | null;
  dismissWarning(): void;
  hide(id: string): void;
  show(id: string): void;
  showAll(): void;
}

function read(): { ids: string[]; warning?: string } {
  try {
    return parseHidden(globalThis.localStorage?.getItem(HIDDEN_STORAGE_KEY) ?? null);
  } catch (err) {
    // Storage can throw on ACCESS, not only on the value — a browser with site data blocked does
    // exactly that, and `chrome/persistedFlag.ts` records the same gotcha for its own flags.
    return { ids: [], warning: `this browser would not let the Monitor read its hidden panes (${String(err)})` };
  }
}

/** The hook form. Thin on purpose: everything above is what a test drives. */
export function useHiddenPanes(): HiddenApi {
  const initial = useRef<{ ids: string[]; warning?: string } | null>(null);
  initial.current ??= read();

  const [order, setOrder] = useState<string[]>(initial.current.ids);
  const [warning, setWarning] = useState<string | null>(initial.current.warning ?? null);
  /** Suppress the first write, exactly as `useWall` does: mounting must not overwrite a saved set
   *  with the one this hook is still holding from before the read landed. */
  const loaded = useRef(false);

  useEffect(() => {
    if (!loaded.current) {
      loaded.current = true;
      return;
    }
    try {
      globalThis.localStorage?.setItem(HIDDEN_STORAGE_KEY, serializeHidden(order));
    } catch (err) {
      setWarning(`the Monitor could not save which panes are hidden (${String(err)})`);
    }
  }, [order]);

  const dismissWarning = useCallback(() => setWarning(null), []);
  const hide = useCallback(
    (id: string) => setOrder((prev) => (prev.includes(id) ? prev : [...prev, id])),
    []
  );
  const show = useCallback(
    (id: string) => setOrder((prev) => (prev.includes(id) ? prev.filter((x) => x !== id) : prev)),
    []
  );
  const showAll = useCallback(() => setOrder((prev) => (prev.length === 0 ? prev : [])), []);

  /**
   * ONE OBJECT IDENTITY PER HIDDEN SET, not per render.
   *
   * `DashboardPage` hangs `useCallback`s off this and hands them to memoized tiles; an object rebuilt
   * on every render would give every tile on the wall a new `onHide` on every render and defeat the
   * `memo` that stops a wall of xterms re-rendering. It changes when the SET changes, which is the
   * only time anything downstream needs to notice.
   */
  return useMemo(
    () => ({ ids: new Set(order), order, warning, dismissWarning, hide, show, showAll }),
    [dismissWarning, hide, order, show, showAll, warning]
  );
}
