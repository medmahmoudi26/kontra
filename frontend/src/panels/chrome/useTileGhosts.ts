/**
 * The leave animation, done with ghosts (slice 7a).
 *
 * WHY A GHOST INSTEAD OF ANIMATING THE TILE OUT. An exit animation needs the element to outlive React's
 * removal, and on this page the element that would be outliving it holds an xterm. Keeping a leaving tile
 * mounted for 220 ms means 220 ms in which it still has a frame writer, still has a subscription the page
 * has already cancelled, and — the part that actually bites — still has to dispose its xterm at the end of
 * the animation instead of at unmount, straight into the documented `Viewport.syncScrollArea` dispose race
 * (`TerminalTile`'s KNOWN OPEN BUG note). So the real tile unmounts immediately, exactly as it does today,
 * and what fades is a bordered rectangle with a name in it. It holds no terminal, no subscription and
 * nothing to dispose.
 *
 * AND IT CARRIES INFORMATION, WHICH IS WHY IT IS WORTH HAVING AT ALL. On a selector-driven wall a tile can
 * vanish for four different reasons — the selector was edited, an earlier slot claimed it, the Machine left
 * the Fleet, the wall hit its tile cap — and all four look identical: a rectangle that is no longer there.
 * A ghost that says `kf-crawl-03 · actor left the wall` for a fifth of a second is the difference between
 * "I think something disappeared" and knowing what did.
 *
 * WHAT IT IS NOT: a record of departures. Ghosts are dropped after one timeout and are not queued, capped
 * per slot, or persisted, because a wall that keeps a list of things that used to be on it is a log — and
 * ADR 0020 is explicit that a Terminal is not a record.
 */

import { useEffect, useRef, useState } from 'react';

/** Matches `.kontra-tile-ghost`'s animation in `chrome.css`. Long enough to be seen, short enough that a
 * reflow is not held up by a decoration. */
export const GHOST_MS = 220;

export interface TileGhost {
  id: string;
  label: string;
}

/**
 * Diff a list of ids against the previous render's and report what left.
 *
 * `labels` supplies the words, because by the time a tile has left the wall its Terminal is no longer in
 * the materialisation and its name would otherwise be gone with it.
 *
 * `enabled` is false on the FIRST pass by construction: a wall mounting for the first time has an empty
 * previous list, and diffing against that would announce nothing — but a wall whose inventory arrives one
 * fetch later would otherwise ghost every tile the moment the slots re-materialise. Guarded by seeding the
 * previous set on the first run and returning nothing from it.
 */
export function useTileGhosts(ids: readonly string[], labels: ReadonlyMap<string, string>): TileGhost[] {
  const previous = useRef<Set<string> | null>(null);
  const [ghosts, setGhosts] = useState<TileGhost[]>([]);
  const timers = useRef(new Map<string, ReturnType<typeof setTimeout>>());

  useEffect(() => {
    const current = new Set(ids);
    const before = previous.current;
    previous.current = current;
    if (before === null) return; // first pass: nothing has left yet

    const left: TileGhost[] = [];
    for (const id of before) {
      if (current.has(id)) continue;
      left.push({ id, label: labels.get(id) ?? id });
    }
    if (left.length === 0) return;

    setGhosts((prev) => [...prev.filter((g) => !left.some((l) => l.id === g.id)), ...left]);
    for (const ghost of left) {
      const existing = timers.current.get(ghost.id);
      if (existing) clearTimeout(existing);
      timers.current.set(
        ghost.id,
        setTimeout(() => {
          timers.current.delete(ghost.id);
          setGhosts((prev) => prev.filter((g) => g.id !== ghost.id));
        }, GHOST_MS)
      );
    }
    // `labels` is intentionally not a dependency: it changes identity on every inventory refresh, and
    // re-running this diff on a refresh that did not change the tile set would ghost nothing but would
    // reset `previous` twice per second on a busy Fleet. The ids ARE the trigger.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ids.join('\n')]);

  // A tile that comes BACK before its ghost expires must not have a ghost: a selector edited twice in
  // quick succession would otherwise leave a phantom beside the real tile.
  const live = new Set(ids);
  useEffect(() => {
    return () => {
      for (const timer of timers.current.values()) clearTimeout(timer);
      timers.current.clear();
    };
  }, []);

  return ghosts.filter((g) => !live.has(g.id));
}
