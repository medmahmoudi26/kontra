/**
 * Narrowing a wall of Terminals down to the ones you meant.
 *
 * WHY A FILTER AND NOT A SEARCH BOX. A ten-Machine sweep puts thirty-odd panes on the wall — one
 * per window per Machine — and the question an operator arrives with is almost never "find this
 * string". It is one of four: which Machines are running THIS actor, what is happening on THAT
 * address, where is this tmux session, and what is actually attached right now. Three of those are
 * exact matches over a small closed set, which is a menu; the fourth is a toggle. The free-text box
 * is there for the fifth question nobody anticipated, and it searches every field at once because
 * an operator who is typing does not know which field their fragment lives in.
 *
 * HIDING IS NOT REMOVING. The filter never touches the saved wall document — see `TileWall`'s
 * `hidden` prop for what that would cost.
 */

import type { Terminal } from './panelsClient';
import { parseTileRef } from './chrome/tileRef';

/** The sentinel a menu uses for "do not narrow on this". Not the empty string, because a Terminal
 *  with no actor registered has an empty `actor` and must remain selectable as its own group. */
export const ANY = '*';

export interface PaneFilter {
  actor: string;
  ip: string;
  session: string;
  query: string;
  /** Only Terminals holding a real PTY attach. The expensive ones (ADR 0020), and the only ones
   *  whose content is moving. */
  liveOnly: boolean;
}

export const EMPTY_FILTER: PaneFilter = {
  actor: ANY,
  ip: ANY,
  session: ANY,
  query: '',
  liveOnly: false,
};

export function filterActive(f: PaneFilter): boolean {
  return (
    f.actor !== ANY || f.ip !== ANY || f.session !== ANY || f.query.trim() !== '' || f.liveOnly
  );
}

/** What a Terminal's address reads as. An empty `publicIp` is a LOCAL session — `kontra workflow
 *  serve … --tmux` on this host — which is a real and common category, not missing data. */
export function paneAddress(t: Terminal): string {
  return t.publicIp || 'local';
}

/** What a Terminal's actor reads as. A Machine with no worker registered yet still has panes. */
export function paneActor(t: Terminal): string {
  return t.actor || 'unregistered';
}

export function paneSession(t: Terminal): string {
  return parseTileRef(t.id).session;
}

/** The menu contents, built from the inventory rather than from a fixed list — a filter that offers
 *  a value nothing has is a dead end, and one that omits a value something has is a lie. */
export function paneOptions(inventory: readonly Terminal[]): {
  actors: string[];
  ips: string[];
  sessions: string[];
} {
  const uniq = (values: string[]): string[] => [...new Set(values)].sort();
  return {
    actors: uniq(inventory.map(paneActor)),
    ips: uniq(inventory.map(paneAddress)),
    sessions: uniq(inventory.map(paneSession)),
  };
}

/**
 * Does this Terminal survive the filter?
 *
 * Every clause is an AND, which is the reading the controls imply: three menus and a box sitting in
 * one row say "all of these at once". `liveOnly` takes the live set as an argument rather than
 * reading a field, because whether a Terminal is attached is the PAGE's state — the socket's — and
 * not a property the inventory carries.
 */
export function matchPane(
  t: Terminal,
  f: PaneFilter,
  live: ReadonlySet<string>
): boolean {
  if (f.actor !== ANY && paneActor(t) !== f.actor) return false;
  if (f.ip !== ANY && paneAddress(t) !== f.ip) return false;
  if (f.session !== ANY && paneSession(t) !== f.session) return false;
  if (f.liveOnly && !live.has(t.id)) return false;
  const q = f.query.trim().toLowerCase();
  if (!q) return true;
  // Every field at once: someone typing `10.124` does not want to be told it was the wrong box.
  return [t.host, t.machine, paneAddress(t), paneSession(t), t.actor, t.version, t.tag, t.window]
    .join(' ')
    .toLowerCase()
    .includes(q);
}

/** The ids the wall must hide. Returned as the complement of the matches because that is what the
 *  wall takes — it keeps every tile in its document and draws all but these. */
export function hiddenPanes(
  inventory: readonly Terminal[],
  f: PaneFilter,
  live: ReadonlySet<string>
): Set<string> {
  if (!filterActive(f)) return new Set();
  return new Set(inventory.filter((t) => !matchPane(t, f, live)).map((t) => t.id));
}
