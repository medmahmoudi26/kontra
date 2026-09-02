/**
 * A Terminal id, taken apart in the browser (ADR 0020 slice 6's id grammar, read from the client side).
 *
 * `<mode>:<node>/<session>/<window>` — `backend/src/panels/ids.ts` is the authority, and it is
 * STRICT by necessity: server-side those segments are interpolated into `ssh` arguments, a
 * `ControlPath` on disk and `docker exec` argv, so `parseTerminalId` throws on anything it has not
 * whitelisted. That strictness is a security property of the *server* and copying it here would be
 * cargo cult — a browser interpolates an id into a React key and a `<code>`, and the worst a malformed
 * one can do is look wrong.
 *
 * SO THIS PARSE IS LENIENT AND TOTAL, and that is the deliberate difference. The sidebar tree groups
 * every Terminal the inventory reports; a single id the streamer minted from a future mode, or an id
 * shape this build predates, must put a row in the tree rather than throw inside a `useMemo` and take
 * the whole Dashboard down with it. An id that does not parse is grouped under what it does say, and
 * says so.
 *
 * WHY THE CLIENT HAS TO PARSE AT ALL: `panelsClient.Terminal` carries `machine` and `window` as
 * fields, but not `mode` and not `session` — the wire kept `machine` for compatibility when slice 6
 * generalised it (`ids.ts` says so), and the session was never a field because until `local` mode one
 * node had exactly one. The tree's four levels are mode → node → session → window, so two of them
 * exist only inside the id. `panelsClient.ts` belongs to slice 1, so the parse lives here.
 */

import { healthReading, isHealthy, type Terminal } from '../panelsClient';

/** The three modes slice 6 defines, in `ids.ts`'s order — `fleet` first, because it owns Machines. */
export const TILE_MODES = ['fleet', 'docker', 'local'] as const;
export type TileMode = (typeof TILE_MODES)[number];

/** What an id says it is. `mode` widens to `string` on purpose: a streamer from a later build may mint
 * a mode this one has never heard of, and the honest rendering of that is the word it sent. */
export interface TileRef {
  mode: string;
  node: string;
  session: string;
  window: string;
  /** False when the id did not have the `<mode>:<node>/<session>/<window>` shape. The tree shows these
   * rather than hiding them: a Terminal missing from the tree but present on the wall is a worse bug
   * than an ugly row. */
  wellFormed: boolean;
}

export function isTileMode(value: string): value is TileMode {
  return (TILE_MODES as readonly string[]).includes(value);
}

/**
 * How dangerous is it to crash this node's tmux server?
 *
 * ADR 0020 finding (2) measured a stalled control-mode client segfaulting a tmux server, which
 * destroys every session on that socket — and the ADR's Consequences make the asymmetry an invariant
 * rather than a preference: on the FLEET, `machine.ts` keeps the actor and handler under systemd and
 * puts only journals in panes, so a crashed server costs the view. LOCAL panes hold the actual actor
 * and handler processes (`cli/tmux.go`), and a DOCKER pane may too, so there the same crash costs a
 * running Worker.
 *
 * CONTRACT.md's slice 6 amendment says this asymmetry "must be stated in the UI". This function is
 * where the UI gets the words, so the sidebar and the tile header cannot drift from each other.
 */
export function modeStakes(mode: string): { level: 'view' | 'worker' | 'unknown'; sentence: string } {
  switch (mode) {
    case 'fleet':
      return {
        level: 'view',
        sentence:
          'fleet: panes hold journals only (the actor and handler run under systemd), so losing this ' +
          'tmux server costs the view and not the Worker.',
      };
    case 'local':
      return {
        level: 'worker',
        sentence:
          'local: these panes hold the ACTUAL actor and handler processes (cli/tmux.go), so losing ' +
          'this tmux server kills a running Worker — not just the view.',
      };
    case 'docker':
      return {
        level: 'worker',
        sentence:
          'docker: a worker container’s pane may hold the real process, so losing this tmux server ' +
          'can kill a running Worker and not just the view.',
      };
    default:
      return {
        level: 'unknown',
        sentence: `${mode}: this build does not know what this mode’s panes hold — assume a crashed tmux server costs a Worker.`,
      };
  }
}

/**
 * Take an id apart. Never throws.
 *
 * The shape check is exactly the server's — one `:`, then exactly two `/` — but a failure produces a
 * best-effort ref instead of an exception. `session` becomes `'?'` rather than `''` so a tree row can
 * never be an invisible empty label.
 */
export function parseTileRef(id: string): TileRef {
  const colon = id.indexOf(':');
  if (colon <= 0) {
    return { mode: '?', node: id || '?', session: '?', window: '?', wellFormed: false };
  }
  const mode = id.slice(0, colon);
  const parts = id.slice(colon + 1).split('/');
  if (parts.length !== 3) {
    return {
      mode,
      node: parts[0] ?? '?',
      session: parts[1] ?? '?',
      window: parts.slice(2).join('/') || '?',
      wellFormed: false,
    };
  }
  const [node, session, window] = parts as [string, string, string];
  if (node === '' || session === '' || window === '') {
    return {
      mode,
      node: node || '?',
      session: session || '?',
      window: window || '?',
      wellFormed: false,
    };
  }
  return { mode, node, session, window, wellFormed: true };
}

/**
 * The session a Terminal belongs to, preferring the id and falling back to the fields.
 *
 * The fallback matters for the `machine`/`window` pair: those arrive as real fields on the wire, so a
 * malformed id still has a usable node and window even when its session is unknowable.
 */
export function tileRefFor(terminal: { id: string; machine: string; window: string }): TileRef {
  const ref = parseTileRef(terminal.id);
  if (ref.wellFormed) return ref;
  return {
    ...ref,
    node: terminal.machine || ref.node,
    window: terminal.window || ref.window,
  };
}

/**
 * Is this Terminal serving? `panelsClient.isHealthy`, read in the mode the Terminal's own id carries.
 *
 * ONE PLACE PAIRS A TERMINAL WITH ITS MODE, and that is the whole reason this three-line function
 * exists. `isHealthy` is mode-aware because two of its axes are answered by machinery that only the
 * fleet has, and a caller that forgets the argument gets the `fleet` default — which is the strict
 * reading, so it fails safe, but it is also exactly the bug that reported a healthy `local` actor as
 * `0 serving`. `Terminal.mode` is not on the wire (`tileRef.ts`'s header says why), so the mode has
 * to be parsed out of the id, and a second caller parsing it its own way is how the Actors page and
 * the Monitor start disagreeing about which Machines are up.
 */
export function terminalIsHealthy(terminal: Terminal): boolean {
  return isHealthy(terminal.health, tileRefFor(terminal).mode);
}

/**
 * The same reading, TRI-STATE — for a surface that says a word rather than counts a number.
 *
 * `panelsClient.healthReading` through the same mode parse, for the same reason as above: the mode
 * is only in the id, and two callers parsing it their own way is how two surfaces start disagreeing.
 * A count wants {@link terminalIsHealthy}; a LABEL wants this, because `false` there means both "we
 * looked and it is broken" and "nobody looked", and only one of those is something to go and fix.
 */
export function terminalHealthReading(terminal: Terminal): boolean | null {
  return healthReading(terminal.health, tileRefFor(terminal).mode);
}
