/**
 * The order the panes inside a sidecar are stacked in, and which of them are folded away.
 *
 * A STACK WHOSE ORDER IS THE SOURCE ORDER IS A DECISION NOBODY MADE. The Workflows sidecar puts the
 * editor above the worker pane above the run list because that is the order they were written in;
 * an operator reading a traceback wants the pane at the top, and an operator comparing runs wants
 * the list there. Neither can say so, and both are looking at the same three panes.
 *
 * THE ORDER IS BY KEY, NEVER BY INDEX. A stored `[2,0,1]` is meaningless the moment a pane is added,
 * removed or renamed — it silently permutes a DIFFERENT set of panes, which is the failure mode that
 * makes a persisted layout worse than none. Keys that no longer exist are dropped and keys that are
 * new are appended, so a stored order survives the stack changing under it.
 *
 * COLLAPSED IS PER PANE AND IT IS NOT REORDERING. Folding a pane away and moving it are two things
 * an operator does for different reasons, and a fold that also moved the pane would lose the place
 * they folded it at.
 */

export interface PaneOrder {
  /** Pane keys, top to bottom. */
  order: string[];
  /** Keys of panes folded to their header. */
  folded: string[];
}

export const EMPTY_ORDER: PaneOrder = { order: [], folded: [] };

/**
 * The stored order, reconciled against the panes that actually exist.
 *
 * `known` is the stack's own list, in its source order, and it is the authority on WHAT exists —
 * this only decides the sequence. Anything stored that is no longer a pane is dropped; anything new
 * is appended in source order, at the bottom, because that is where a reader will look for something
 * they have not seen before.
 */
export function reconcileOrder(stored: readonly string[], known: readonly string[]): string[] {
  const exists = new Set(known);
  const seen = new Set<string>();
  const out: string[] = [];
  for (const key of stored) {
    if (!exists.has(key) || seen.has(key)) continue;
    seen.add(key);
    out.push(key);
  }
  for (const key of known) {
    if (!seen.has(key)) out.push(key);
  }
  return out;
}

/**
 * One pane moved to another position.
 *
 * BY KEY AND NOT BY INDEX, because the caller is a drag whose source and target are elements that
 * know their key and not their position — and because a mid-drag re-render that changed an index
 * would drop the pane somewhere nobody pointed at. Moving a key onto itself, or either key not
 * being in the list, returns the list unchanged rather than throwing: a drag that ended nowhere is
 * an ordinary gesture, not an error.
 */
export function moveBefore(order: readonly string[], moved: string, before: string): string[] {
  if (moved === before) return [...order];
  const from = order.indexOf(moved);
  const to = order.indexOf(before);
  if (from === -1 || to === -1) return [...order];
  const rest = order.filter((k) => k !== moved);
  const at = rest.indexOf(before);
  return [...rest.slice(0, at), moved, ...rest.slice(at)];
}

/** One pane moved one place up or down. The keyboard's half of the drag — a stack that can only be
 *  reordered with a pointer is one an operator on a trackpad will not reorder. */
export function nudge(order: readonly string[], key: string, towards: -1 | 1): string[] {
  const from = order.indexOf(key);
  if (from === -1) return [...order];
  const to = from + towards;
  if (to < 0 || to >= order.length) return [...order];
  const out = [...order];
  const [moved] = out.splice(from, 1);
  if (moved !== undefined) out.splice(to, 0, moved);
  return out;
}

/** With `key` folded, or unfolded. */
export function toggleFolded(folded: readonly string[], key: string): string[] {
  return folded.includes(key) ? folded.filter((k) => k !== key) : [...folded, key];
}

/** Where one stack's order is remembered. Namespaced per stack, for `paneHeightKey`'s reason. */
export function paneOrderKey(stack: string): string {
  return `kontra.stack.${stack}`;
}

/**
 * A stored order, or an empty one. Never throws: a corrupt value must cost the source order, not the
 * page — and a half-parsed one is how a stack comes back with three panes in two places.
 */
export function parsePaneOrder(raw: string | null): PaneOrder {
  if (raw === null) return EMPTY_ORDER;
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return EMPTY_ORDER;
  }
  if (typeof parsed !== 'object' || parsed === null) return EMPTY_ORDER;
  const got = parsed as Partial<PaneOrder>;
  const strings = (v: unknown): string[] =>
    Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [];
  return { order: strings(got.order), folded: strings(got.folded) };
}
