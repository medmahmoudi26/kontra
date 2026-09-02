/**
 * The sidebar tree: mode → node → session → window, with counts and health rollups (slice 7a).
 *
 * WHAT THE TREE IS FOR, AND WHAT THE WALL CANNOT DO. The wall is selectors: it shows the Terminals a
 * slot matched, in slot order, and it is deliberately allowed to be a view of PART of a Fleet
 * (`grid/DashboardGrid.tsx` reports the unmatched count for exactly that reason). So the wall can never
 * answer "what does the Fleet have?" — only "what did I ask for?". The tree answers the first question:
 * every Terminal the inventory reports, whether or not a slot matched it, arranged the way the id is
 * arranged. `kf-crawl-07` missing from the wall is a slot to fix; missing from the TREE is a Machine to
 * go and look at, and those are different mornings.
 *
 * A ROLLUP IS A WORST-OF, NEVER AN AVERAGE, AND NEVER A GREEN LIGHT ON ITS OWN. ADR 0020 keeps four
 * health signals apart "never collapsed into one light", because the round-3 incident — 81 of 82
 * resource loads failing on one Machine while the run reported `completed` — was three good signals
 * outvoting the one that mattered. A tree of 200 Terminals still needs a per-branch summary, so the
 * rule this file follows is the one that keeps the ADR's guarantee intact:
 *
 *   1. the rollup is the WORST descendant state, so one failing Terminal colours its Machine, its
 *      session and its mode;
 *   2. `unknown` beats `ok` — a branch is only `ok` when every signal of every descendant that COULD
 *      be measured in that pane's mode was measured and good, which is `panelsClient.isHealthy`'s
 *      rule (including its `signalApplies` clause) applied upward;
 *   3. the rollup never travels alone. {@link BranchHealth} carries the COUNTS beside it, so a row
 *      renders "12 · 1 failing" and not merely a coloured dot. A rollup is a pointer at a branch to
 *      open, and the four chips on the tile are still the only place a signal is read.
 *
 * PURE. No React, no DOM, no fetch: the grouping and the rollup are the two things a browser test
 * cannot show you got wrong — a tree with a Machine under the wrong session still renders as a tree —
 * so they are pinned by unit tests instead. Same discipline as `selectors.ts`.
 */

import { compareNatural } from '../selectors';
import { signalApplies, type HealthSignal, type Terminal, type TerminalHealth } from '../panelsClient';
import { modeStakes, tileRefFor, type TileRef } from './tileRef';

/** The tri-state, identical to `HealthChips`'s on purpose: one health vocabulary in the whole
 * Dashboard, so a dashed border means the same thing in the tree as it does on a chip. */
export type RollupState = 'ok' | 'bad' | 'unknown';

export interface BranchHealth {
  /** The worst state among the descendants. Never `ok` unless every one of them is fully measured and
   * good. */
  state: RollupState;
  /** Terminals under this branch. */
  total: number;
  /** …of which at least one signal is definitely bad. */
  failing: number;
  /** …of which nothing is bad but at least one signal was never measured. */
  unmeasured: number;
  /** …that are on the wall right now. The tree shows the whole inventory, so "this Machine exists but
   * no slot matched it" has to be visible as a number rather than as an absence. */
  onWall: number;
  /** …that the streamer says are a live PTY attach. Shown because a live tile is the one thing on this
   * page that costs a 2 GB Machine an `sshd` session and a PTY. */
  live: number;
}

/** The worst of two states, under rule (2): `bad` > `unknown` > `ok`. */
export function worse(a: RollupState, b: RollupState): RollupState {
  if (a === 'bad' || b === 'bad') return 'bad';
  if (a === 'unknown' || b === 'unknown') return 'unknown';
  return 'ok';
}

/**
 * One Terminal's signals, rolled into one state.
 *
 * `bad` if ANY signal is definitely bad; else `unknown` if any that COULD have been measured was not;
 * else `ok`. That third clause is `isHealthy` — and the reason this is not simply
 * `isHealthy(h) ? 'ok' : 'bad'` is the whole point of the tri-state: a Machine nobody has probed yet
 * must not be painted as a failing one, or the first thirty seconds after a `fleet up` are a wall of
 * red an operator learns to ignore.
 *
 * MODE-AWARE THROUGH `signalApplies`, THE SAME CALL `isHealthy` AND THE CHIPS MAKE. Two of the axes
 * are answered by machinery only the fleet has — `reachable` is a transport only `fleet` crosses,
 * `loads` comes from a vmagent only `infra/programs/machine.ts` installs — so on a `local` or
 * `docker` node they are permanently `unknown` for a structural reason rather than an operational
 * one, and counting them made every local branch in this tree permanently "not fully measured". A
 * second rule for which axes count would be worse than the bug: the tree, the chips and the Actors
 * page have to mean the same thing by healthy.
 *
 * OMISSION STAYS ONE-DIRECTIONAL, which is what keeps this inside ADR 0020: the `bad` scan below runs
 * over EVERY signal whatever its mode says, so an inapplicable axis that actually fails still colours
 * the branch. Only a green or an unknown on an axis nothing could ever answer is dropped.
 *
 * `mode` DEFAULTS TO `fleet`, where every axis applies, so a caller that does not pass one gets the
 * strictest reading and can never accidentally paint a branch green.
 */
export function rollupOne(health: TerminalHealth, mode = 'fleet'): RollupState {
  // `process` is in the list for its BAD value and would be there for its unknown one anyway: a
  // Worker that exited inside a session that is still present is exactly the failure a tree of
  // green branches would hide, and `unknown` here is no worse than the `unknown` `poller` already
  // contributes on a deployment with no Temporal reachable. Absent (an older streamer) reads as
  // `unknown`, never as fine.
  const signals: [HealthSignal, string][] = [
    ['reachable', health.reachable],
    ['session', health.session],
    ['process', health.process ?? 'unknown'],
    ['poller', health.poller],
    ['loads', health.loads],
  ];
  if (
    signals.some(
      ([, s]) =>
        s === 'fail' || s === 'absent' || s === 'no-tmux' || s === 'none' || s === 'failing' || s === 'exited'
    )
  ) {
    return 'bad';
  }
  if (signals.some(([signal, s]) => s === 'unknown' && signalApplies(signal, mode))) return 'unknown';
  return 'ok';
}

function emptyHealth(): BranchHealth {
  return { state: 'ok', total: 0, failing: 0, unmeasured: 0, onWall: 0, live: 0 };
}

function accumulate(
  into: BranchHealth,
  health: TerminalHealth,
  mode: string,
  onWall: boolean,
  live: boolean
): void {
  const state = rollupOne(health, mode);
  into.total += 1;
  if (state === 'bad') into.failing += 1;
  else if (state === 'unknown') into.unmeasured += 1;
  if (onWall) into.onWall += 1;
  if (live) into.live += 1;
  into.state = into.total === 1 ? state : worse(into.state, state);
}

function merge(into: BranchHealth, from: BranchHealth): void {
  const first = into.total === 0;
  into.total += from.total;
  into.failing += from.failing;
  into.unmeasured += from.unmeasured;
  into.onWall += from.onWall;
  into.live += from.live;
  into.state = first ? from.state : worse(into.state, from.state);
}

/** One sentence for a branch, so a rollup is never only a colour. Assembled here rather than in the
 * component so the tree and its `title` attributes cannot disagree. */
export function describeBranch(health: BranchHealth): string {
  const parts = [`${health.total} Terminal${health.total === 1 ? '' : 's'}`];
  if (health.failing > 0) parts.push(`${health.failing} failing`);
  if (health.unmeasured > 0) parts.push(`${health.unmeasured} not fully measured`);
  if (health.total > 0 && health.failing === 0 && health.unmeasured === 0) parts.push('all signals ok');
  parts.push(
    health.onWall === health.total ? 'all on the wall' : `${health.onWall} on the wall`
  );
  if (health.live > 0) parts.push(`${health.live} live`);
  return parts.join(' · ');
}

// --- the tree ------------------------------------------------------------------------------------

export interface WindowNode {
  kind: 'window';
  /** The Terminal id — the leaf IS a Terminal, which is why clicking one can reveal a tile. */
  id: string;
  label: string;
  terminal: Terminal;
  ref: TileRef;
  onWall: boolean;
  live: boolean;
  state: RollupState;
}

export interface SessionNode {
  kind: 'session';
  key: string;
  label: string;
  health: BranchHealth;
  windows: WindowNode[];
}

export interface NodeNode {
  kind: 'node';
  key: string;
  label: string;
  health: BranchHealth;
  sessions: SessionNode[];
}

export interface ModeNode {
  kind: 'mode';
  key: string;
  label: string;
  /** ADR 0020's asymmetry, in words, from `tileRef.modeStakes`. */
  stakes: ReturnType<typeof modeStakes>;
  /**
   * The fleets this mode's Terminals say they belong to, distinct and in first-seen order.
   *
   * A FLEET HAS A NAME AND IT IS NOT THE WORD "fleet". `machinesFromStack` reads it off the Pulumi
   * stack a run provisioned, so the name is the workflow's doing — and `local.ts` sets it to the
   * empty string deliberately, because a local Worker belongs to no fleet and inventing `'local'`
   * would put the mode in two places. So this is empty for every mode but `fleet`, and the tree
   * says a name only where one was actually provisioned rather than labelling the group after the
   * mode that owns it.
   */
  fleets: string[];
  health: BranchHealth;
  nodes: NodeNode[];
}

export interface TreeInput {
  /** Every Terminal the inventory reports — NOT the wall. */
  inventory: readonly Terminal[];
  /** Ids currently materialised onto the wall. */
  onWall: ReadonlySet<string>;
  /** Ids the streamer says are a live PTY attach. */
  live: ReadonlySet<string>;
}

/**
 * Group the inventory into the four levels.
 *
 * ORDERING IS `compareNatural` AT EVERY LEVEL, borrowed from `selectors.ts` rather than reimplemented,
 * and for the same reason it exists there: `kf-crawl-2` must precede `kf-crawl-10`, and an unstable
 * order reshuffles the tree under an operator's cursor on every 30-second inventory refresh. Modes are
 * the one exception — they sort by `TILE_MODES` order (fleet, docker, local) because that is a
 * meaningful ladder and alphabetical would put `docker` above the mode that owns Machines.
 */
export function buildTree(input: TreeInput): ModeNode[] {
  const modes = new Map<string, ModeNode>();

  for (const terminal of input.inventory) {
    const ref = tileRefFor(terminal);
    const onWall = input.onWall.has(terminal.id);
    const live = input.live.has(terminal.id);

    let mode = modes.get(ref.mode);
    if (!mode) {
      mode = {
        kind: 'mode',
        /* THE MODE ROW STAYS THE MODE'S OWN WORD — `local`, `docker`, `fleet`. It was briefly
           relabelled `localhost`, which put the same word on two consecutive rows: the mode row is
           a PLACEMENT (where a Worker was put) and the node row under it is the MACHINE. The
           machine is what had to say localhost, and it does — see `panels/local.ts:localHost`,
           which was answering with the orchestrator container's 12-hex hostname. */
        key: ref.mode,
        label: ref.mode,
        stakes: modeStakes(ref.mode),
        fleets: [],
        health: emptyHealth(),
        nodes: [],
      };
      modes.set(ref.mode, mode);
    }
    const fleet = terminal.fleet ?? '';
    if (fleet !== '' && !mode.fleets.includes(fleet)) mode.fleets.push(fleet);

    let node = mode.nodes.find((n) => n.label === ref.node);
    if (!node) {
      node = {
        kind: 'node',
        key: `${ref.mode}:${ref.node}`,
        label: ref.node,
        health: emptyHealth(),
        sessions: [],
      };
      mode.nodes.push(node);
    }

    let session = node.sessions.find((s) => s.label === ref.session);
    if (!session) {
      session = {
        kind: 'session',
        key: `${ref.mode}:${ref.node}/${ref.session}`,
        label: ref.session,
        health: emptyHealth(),
        windows: [],
      };
      node.sessions.push(session);
    }

    session.windows.push({
      kind: 'window',
      id: terminal.id,
      label: ref.window,
      terminal,
      ref,
      onWall,
      live,
      state: rollupOne(terminal.health, ref.mode),
    });
    accumulate(session.health, terminal.health, ref.mode, onWall, live);
  }

  // Rolled up from the leaves, not recomputed per level: a session's counts already summed its
  // windows, so a Machine that merely re-adds them cannot disagree with what its own rows show.
  const order = new Map(['fleet', 'docker', 'local'].map((m, i) => [m, i]));
  const out = [...modes.values()];
  for (const mode of out) {
    for (const node of mode.nodes) {
      for (const session of node.sessions) {
        session.windows.sort((a, b) => compareNatural(a.label, b.label) || compareNatural(a.id, b.id));
        merge(node.health, session.health);
      }
      node.sessions.sort((a, b) => compareNatural(a.label, b.label));
      merge(mode.health, node.health);
    }
    mode.nodes.sort((a, b) => compareNatural(a.label, b.label));
  }
  out.sort((a, b) => {
    const ai = order.get(a.key) ?? order.size;
    const bi = order.get(b.key) ?? order.size;
    return ai - bi || compareNatural(a.label, b.label);
  });
  return out;
}

// --- flattening ----------------------------------------------------------------------------------

/**
 * One keyboard-navigable row.
 *
 * FLATTENED RATHER THAN NESTED, which is the technique the reference implementation uses and it is
 * worth saying why: a nested render makes "the next row down" a tree walk, and every arrow key then
 * has to reproduce the collapse state to know where it lands. Flattened, `ArrowDown` is `index + 1`
 * and a collapsed branch simply does not contribute rows. The `key` is stable across refreshes so the
 * cursor survives the 30-second inventory poll — otherwise holding `ArrowDown` fights the refresh.
 */
export type TreeRow =
  | { kind: 'mode'; key: string; depth: 0; node: ModeNode; expandable: true }
  | { kind: 'node'; key: string; depth: 1; node: NodeNode; expandable: true }
  | { kind: 'session'; key: string; depth: 2; node: SessionNode; expandable: true }
  | { kind: 'window'; key: string; depth: 3; node: WindowNode; expandable: false };

/**
 * Flatten to rows, honouring what is collapsed.
 *
 * COLLAPSE IS A SET OF WHAT IS CLOSED, not of what is open, so the default — an empty set — is a fully
 * expanded tree. That matters on first open: a Dashboard whose tree starts collapsed hides the very
 * Machine the operator came to find, and "expand everything" is not a discoverable first action.
 *
 * A single-session node still renders its session row. Skipping it would be tidier and would make the
 * tree lie by omission the day a `local` host runs `kontra-webcrawl` and `kontra-parse` at once —
 * which is the case `ids.ts` says broke the one-session-per-node assumption in the first place.
 */
export function flattenTree(tree: readonly ModeNode[], collapsed: ReadonlySet<string>): TreeRow[] {
  const rows: TreeRow[] = [];
  for (const mode of tree) {
    rows.push({ kind: 'mode', key: mode.key, depth: 0, node: mode, expandable: true });
    if (collapsed.has(mode.key)) continue;
    for (const node of mode.nodes) {
      rows.push({ kind: 'node', key: node.key, depth: 1, node, expandable: true });
      if (collapsed.has(node.key)) continue;
      for (const session of node.sessions) {
        rows.push({ kind: 'session', key: session.key, depth: 2, node: session, expandable: true });
        if (collapsed.has(session.key)) continue;
        for (const window of session.windows) {
          rows.push({ kind: 'window', key: window.id, depth: 3, node: window, expandable: false });
        }
      }
    }
  }
  return rows;
}

/** Every branch key in the tree — what "collapse all" has to write, since the collapse set stores what
 * is CLOSED. */
export function allBranchKeys(tree: readonly ModeNode[]): string[] {
  const keys: string[] = [];
  for (const mode of tree) {
    keys.push(mode.key);
    for (const node of mode.nodes) {
      keys.push(node.key);
      for (const session of node.sessions) keys.push(session.key);
    }
  }
  return keys;
}
