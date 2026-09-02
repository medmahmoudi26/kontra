/**
 * Selectors: how a Dashboard slot decides which Terminals it is showing (ADR 0020 decision 9).
 *
 * A DASHBOARD IS SELECTORS, NOT PINNED HOSTS. A slot holds a selector — any subset of
 * `tag`/`fleet`/`actor`/`window`/`machine` — and the tiles on the wall are materialised by
 * evaluating those selectors against the Terminal inventory. `fleet up --count 10` therefore fills the
 * wall with no edit, which a hand-placed tile list cannot do: it would be stale the moment the Fleet
 * scaled, and 12 Machines × 2 windows is already 24 tiles. A slot that names a `machine` IS the
 * "pinned Machine" case — the same mechanism, narrowed, rather than a second one with its own rules.
 *
 * EVERYTHING HERE IS PURE. No React, no DOM, no storage: the ordering rule and the slot→tile mapping
 * are the two things in this slice that a browser test cannot show you went wrong (a shuffled wall
 * looks like a wall), so they are pinned by unit tests instead.
 *
 * TWO RULES CARRY THE WEIGHT:
 *
 * 1. **The order is total and stable** — machine, then window, then id, by a numeric-aware compare so
 *    `kf-crawl-2` precedes `kf-crawl-10`. The inventory arrives from a fresh `GET
 *    /api/panels/terminals` every refresh, and any instability here reshuffles the wall under an
 *    operator's cursor every 30 seconds.
 *
 * 2. **A Terminal appears at most once on a wall**, claimed by the first slot (in slot order) whose
 *    selector matches it. Two tiles for one Terminal would mean two xterm instances, two subscriptions
 *    and one writer — the page routes a tagged frame to ONE writer per id — so the second tile would
 *    silently stay blank. A slot that lost its Terminals to an earlier slot is told so, in words, on
 *    the wall.
 *
 * The React key for a tile is {@link tileKey}, and it is a function of the TERMINAL, not of its
 * position or its slot. That is the whole defence against the failure mode that matters most in this
 * slice: remounting a tile disposes its xterm, which clears the screen and drops the stream, and a
 * re-render caused by a health update or a drag must never do that.
 */

/** The fields of a Terminal a selector can match on. A subset of `panelsClient.Terminal`, so this
 * module is testable with three-line fixtures and reusable by anything that has less than a full
 * Terminal in hand. */
export interface SelectableTerminal {
  id: string;
  machine: string;
  tag: string;
  fleet: string;
  actor: string;
  window: string;
}

/**
 * The keys a slot may select on, in the order they are shown as chips.
 *
 * `machine` FIRST because it is the narrowest and the most surprising: a slot pinned to one Machine
 * should read as pinned at a glance. The other four are ADR 0020 decision 9's set, ordered
 * outside-in — the fleet a Machine belongs to, the tag it plays in it, the actor placed on it,
 * and finally which window of its session.
 */
export const SELECTOR_KEYS = ['machine', 'fleet', 'tag', 'actor', 'window'] as const;
export type SelectorKey = (typeof SELECTOR_KEYS)[number];

/** Any subset of the keys. `{}` means "every Terminal in the inventory" — the default wall. */
export type Selector = Partial<Record<SelectorKey, string>>;

export interface SelectorTerm {
  key: SelectorKey;
  value: string;
}

/**
 * Drop what is not a usable term: unknown keys, non-strings, and blanks.
 *
 * A blank is dropped rather than kept because `''` is a REAL value in this inventory — a Machine with
 * no placement carries `actor: ''` (CONTRACT.md's `Terminal`) — so a selector `{actor: ''}` read as
 * "any actor" would match those Machines and nothing else, which is the opposite of what an empty
 * input box means. Absent means "don't care"; present means "equals this".
 */
export function normalizeSelector(raw: unknown): Selector {
  const out: Selector = {};
  if (!raw || typeof raw !== 'object') return out;
  const src = raw as Record<string, unknown>;
  for (const key of SELECTOR_KEYS) {
    const value = src[key];
    if (typeof value !== 'string') continue;
    const trimmed = value.trim();
    if (trimmed !== '') out[key] = trimmed;
  }
  return out;
}

/** The terms of a selector, in {@link SELECTOR_KEYS} order — what a chip row renders. */
export function selectorTerms(selector: Selector): SelectorTerm[] {
  const terms: SelectorTerm[] = [];
  for (const key of SELECTOR_KEYS) {
    const value = selector[key];
    if (value !== undefined && value !== '') terms.push({ key, value });
  }
  return terms;
}

/** A slot that names a `machine` is the deliberate pin ADR 0020 decision 9 allows. */
export function isPinnedToMachine(selector: Selector): boolean {
  return selectorTerms(selector).some((t) => t.key === 'machine');
}

/** One line of prose for a selector — the tooltip behind the chips, and the words a slot with no
 * terms needs so an empty chip row is never a mystery. */
export function describeSelector(selector: Selector): string {
  const terms = selectorTerms(selector);
  if (terms.length === 0) return 'every Terminal in the Fleet inventory';
  return terms.map((t) => `${t.key}=${t.value}`).join(' and ');
}

/** Does this Terminal match? Every present term must be an exact match; an empty selector matches
 * everything. Exact and case-sensitive: these values are ids and stack outputs, not search text. */
export function matchesSelector(selector: Selector, terminal: SelectableTerminal): boolean {
  for (const { key, value } of selectorTerms(selector)) {
    if (terminal[key] !== value) return false;
  }
  return true;
}

/**
 * Compare two strings with runs of digits compared as numbers.
 *
 * `kf-crawl-2` vs `kf-crawl-10`: machine names are `kf-<tag>-NN` and zero-padded today, so plain
 * lexicographic ordering happens to agree — until a Fleet passes 99 Machines, or a name is written
 * without the pad, at which point the wall reorders for reasons no operator can see. Cheap to do
 * once, here.
 */
export function compareNatural(a: string, b: string): number {
  const parts = /(\d+)/;
  const as = a.split(parts);
  const bs = b.split(parts);
  const n = Math.max(as.length, bs.length);
  for (let i = 0; i < n; i += 1) {
    const x = as[i] ?? '';
    const y = bs[i] ?? '';
    if (x === y) continue;
    const xn = /^\d+$/.test(x);
    const yn = /^\d+$/.test(y);
    if (xn && yn) {
      const d = Number(x) - Number(y);
      if (d !== 0) return d < 0 ? -1 : 1;
      continue; // `01` and `1` are numerically equal; the fallback below breaks that tie
    }
    return x < y ? -1 : 1;
  }
  // Different strings that compared equal part by part — `kf-crawl-1` and `kf-crawl-01`. Falling
  // through to 0 here would make this compare non-antisymmetric, which means `sort` decides by
  // input order and the wall shuffles between refreshes. That is precisely the bug this ordering
  // exists to prevent, so the tie is broken rather than ignored.
  return a === b ? 0 : a < b ? -1 : 1;
}

/**
 * The wall's total order: machine, then window, then id.
 *
 * Total on purpose — `id` is the tiebreak and it is unique, so the sort is a function of the SET of
 * Terminals and nothing else. Two consecutive inventory refreshes therefore produce the same wall,
 * whatever order the streamer's `Map` iterated in.
 */
export function compareTerminals(a: SelectableTerminal, b: SelectableTerminal): number {
  return (
    compareNatural(a.machine, b.machine) ||
    compareNatural(a.window, b.window) ||
    compareNatural(a.id, b.id)
  );
}

/** The inventory in wall order. Copies rather than sorting in place: the caller's array is usually
 * React state, and sorting it in place mutates a rendered value. */
export function orderTerminals<T extends SelectableTerminal>(terminals: readonly T[]): T[] {
  return [...terminals].sort(compareTerminals);
}

/** Evaluate one selector against the inventory, in wall order. */
export function selectTerminals<T extends SelectableTerminal>(
  selector: Selector,
  terminals: readonly T[]
): T[] {
  return orderTerminals(terminals.filter((t) => matchesSelector(selector, t)));
}

/** The distinct values of one selector key present in the inventory, in natural order — what the
 * selector editor offers and what the presets are built from. */
export function inventoryValues<T extends SelectableTerminal>(
  key: SelectorKey,
  terminals: readonly T[]
): string[] {
  const seen = new Set<string>();
  for (const t of terminals) {
    const value = t[key];
    if (value !== '') seen.add(value);
  }
  return [...seen].sort(compareNatural);
}

/**
 * The React key for a tile: the Terminal's own id.
 *
 * NOT the slot's id, and not an index. A Terminal that moves between slots — because a selector was
 * edited, or an earlier slot was deleted and stopped claiming it — keeps its key, so React keeps the
 * component instance, so the xterm instance and its stream survive the move. Keying on `${slot}/${id}`
 * would remount on every such move; keying on the index would remount half the wall every time a
 * Machine appeared. Both clear the screen, which is the failure this whole ordering discipline exists
 * to prevent.
 */
export function tileKey(terminal: SelectableTerminal): string {
  return terminal.id;
}

/** The shape {@link materialiseSlots} needs from a slot. `grid/layout.ts`'s `Slot` satisfies it. */
export interface SelectorSlot {
  id: string;
  selector: Selector;
}

export interface MaterialisedSlot<S extends SelectorSlot, T extends SelectableTerminal> {
  slot: S;
  /** The tiles this slot shows, in wall order. */
  terminals: T[];
  /** Matched this selector, but an earlier slot already claimed them. Shown as words on the slot:
   * a slot that silently renders nothing is indistinguishable from a broken selector. */
  claimedByEarlierSlot: number;
  /** Matched, unclaimed, and dropped by the wall's tile cap. */
  overCap: number;
}

export interface Materialisation<S extends SelectorSlot, T extends SelectableTerminal> {
  slots: MaterialisedSlot<S, T>[];
  /** Every tile on the wall, in slot order then wall order — the order the DOM is built in, which is
   * what makes "the first tile is the first Terminal" a property rather than a coincidence. */
  tiles: T[];
  /** Terminals in the inventory that no slot matched. Not an error: a wall is allowed to be a view of
   * part of a Fleet, but the count is worth showing so "where is kf-crawl-07" has an answer. */
  unmatched: number;
  /** Dropped by the cap, across the wall. */
  overCap: number;
}

/**
 * The wall's tile cap.
 *
 * Twelve Machines × 2 windows = 24 tiles is the size ADR 0020 designs for, and each tile is an xterm
 * instance with a 2000-line scrollback. Double that is the ceiling here: past it a browser tab, not a
 * Machine, is the thing that falls over, and an operator who selected 200 Terminals wants to be told
 * so rather than have the tab die. The overflow is reported, never silently dropped.
 */
export const WALL_TILE_CAP = 48;

/**
 * Slots → tiles, with one claim per Terminal.
 *
 * First slot in slot order wins a contested Terminal. That rule is arbitrary but it has to be SOME
 * rule and it has to be stable: what is not arbitrary is that a Terminal may appear once, because the
 * page routes each tagged frame to exactly one writer per Terminal id and a second tile for the same
 * id would sit there blank and plausible.
 */
export function materialiseSlots<S extends SelectorSlot, T extends SelectableTerminal>(
  slots: readonly S[],
  terminals: readonly T[],
  options: { cap?: number } = {}
): Materialisation<S, T> {
  const cap = options.cap ?? WALL_TILE_CAP;
  const ordered = orderTerminals(terminals);
  const claimed = new Set<string>();
  // Ids, not a count: two slots matching the same over-cap Terminal each report it, and adding those
  // reports together would make the wall claim more Terminals than the Fleet has.
  const overCapIds = new Set<string>();
  const out: MaterialisedSlot<S, T>[] = [];
  const tiles: T[] = [];

  for (const slot of slots) {
    const selector = normalizeSelector(slot.selector);
    const mine: T[] = [];
    let taken = 0;
    let dropped = 0;
    for (const terminal of ordered) {
      if (!matchesSelector(selector, terminal)) continue;
      if (claimed.has(terminal.id)) {
        taken += 1;
        continue;
      }
      if (tiles.length >= cap) {
        dropped += 1;
        overCapIds.add(terminal.id);
        continue;
      }
      claimed.add(terminal.id);
      mine.push(terminal);
      tiles.push(terminal);
    }
    out.push({ slot, terminals: mine, claimedByEarlierSlot: taken, overCap: dropped });
  }

  return {
    slots: out,
    tiles,
    unmatched: ordered.length - claimed.size - overCapIds.size,
    overCap: overCapIds.size,
  };
}
