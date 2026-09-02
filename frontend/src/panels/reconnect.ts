/**
 * Reconnecting a panel socket after it drops — without losing or duplicating output (ADR 0020, slice 05).
 *
 * ONE TRANSPORT, ONE RECONNECT STORY. tmux is the only log path off a Worker (ADR 0020, and the
 * instrument-panel PRD keeps it that way on purpose), so a browser that loses its panel socket gets
 * back on the SAME transport rather than reaching for a second one. The two consumers — the Monitor's
 * whole-wall socket (`DashboardPage`) and the instrument panel's single pane (`useSinglePane`) — share
 * this module so "how long before we retry, and how do we re-subscribe without doubling up" is answered
 * once.
 *
 * WHY A RECONNECT CANNOT LOSE OR DUPLICATE. The wire is idempotent by construction. A snapshot is a
 * whole-screen repaint (`TerminalTile.applyFrame` calls `reset()` on the `CLEAR_HOME` prefix), so the
 * first frame after a reconnect REPLACES whatever stale screen the tile froze on — nothing older has to
 * be replayed and nothing is shown twice. The browser holds no bytes across the gap. The only thing the
 * client must get right is the re-subscribe: a fresh socket starts with an empty subscription set, so
 * {@link resubscribePlan} asks for each wanted Terminal exactly once. Ask twice and a live PTY attach is
 * torn down and rebuilt on a 2 GB Machine; miss one and a tile freezes forever.
 *
 * THIS IS ONLY EVER A RE-SUBSCRIBE. Nothing here assembles a keystroke or any byte bound for a session —
 * read-only survives a reconnect because the reconnect's whole vocabulary is `subscribe`/`unsubscribe`.
 */

/** First retry after a drop waits this long; each further attempt doubles it. */
export const RECONNECT_BASE_MS = 1_000;
/** …capped here, so a streamer that is down for an hour is retried every 15 s rather than never — and a
 *  single browser reconnecting to its own streamer is not a herd worth jittering. */
export const RECONNECT_MAX_MS = 15_000;

/**
 * How long to wait before reconnect attempt `attempt` (0 = the first retry after a drop).
 *
 * Exponential with a cap and no jitter: deterministic, so a test can assert the schedule, and bounded,
 * so a persistently-down streamer costs one attempt every {@link RECONNECT_MAX_MS} rather than a spin.
 */
export function reconnectDelay(attempt: number): number {
  const n = Math.max(0, Math.floor(attempt));
  // Clamp the exponent before the shift so a large attempt count cannot overflow past the cap compare.
  const raw = RECONNECT_BASE_MS * 2 ** Math.min(n, 20);
  return Math.min(RECONNECT_MAX_MS, raw);
}

/** The two message kinds a reconnect (or an inventory change) is ever allowed to produce. Named so a
 *  test can pin that the plan carries no third, byte-bearing kind. */
export interface Resubscription {
  /** Terminals to `subscribe` — each wanted id the socket does not already hold, once. */
  subscribe: string[];
  /** Terminals to `unsubscribe` — each held id that is no longer wanted. */
  unsubscribe: string[];
}

/**
 * The subscribe / unsubscribe delta that brings a socket's subscriptions in line with what the wall
 * wants — the whole of "reconnect without loss or duplication" as one pure function.
 *
 * `wanted` is the Terminals that should be streaming — on the wall AND already measured, so the caller
 * has dropped anything sized 0×0 (a 0×0 subscribe asks a Machine for a zero-sized screen). `subscribed`
 * is what THIS socket has already been told about. On a fresh socket after a reconnect that set is
 * empty, so every wanted id is subscribed exactly once — no loss (every wanted id appears) and no
 * duplication (an already-held id is skipped, and a duplicate in `wanted` folds to one). On the live
 * socket after an inventory change it is the delta instead. Either way an id is subscribed at most once,
 * because a duplicate `subscribe` on a promoted tile is a torn-down and rebuilt PTY attach.
 */
export function resubscribePlan(
  wanted: readonly string[],
  subscribed: ReadonlySet<string>
): Resubscription {
  const seen = new Set<string>();
  const subscribe: string[] = [];
  for (const id of wanted) {
    if (subscribed.has(id) || seen.has(id)) continue;
    seen.add(id);
    subscribe.push(id);
  }
  const wantSet = seen.size === wanted.length ? seen : new Set(wanted);
  const unsubscribe = [...subscribed].filter((id) => !wantSet.has(id));
  return { subscribe, unsubscribe };
}

/**
 * What a pane or wall SAYS while its socket is down.
 *
 * A dropped socket must say so rather than freeze silently on its last frame — that frozen screen is
 * now stale, and on a wall of near-identical journals a frozen tile and a Machine that stopped printing
 * are indistinguishable (the same confusion ADR 0020's held-repaint badge and four-signal rule exist to
 * prevent). `attempt` is 0-based; the wording counts from one because an operator does not think in
 * zero-based attempts.
 */
export function disconnectedNotice(attempt: number, delayMs: number): string {
  const secs = Math.max(1, Math.round(delayMs / 1000));
  const nth = attempt <= 0 ? 'reconnecting' : `reconnecting (attempt ${attempt + 1})`;
  return (
    `the panel stream dropped — ${nth} in ${secs}s. ` +
    'The screen below is frozen at its last frame until it does; a Terminal is not a record.'
  );
}
