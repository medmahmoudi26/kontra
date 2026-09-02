/**
 * The sidebar tree's two invisible rules (slice 7a).
 *
 * A TREE WITH A MACHINE UNDER THE WRONG SESSION STILL RENDERS AS A TREE, and a rollup that quietly says
 * `ok` over a failing Terminal still renders as a green dot. Those are the two failures a browser cannot
 * show you, so they are pinned here — the same reason `selectors.test.ts` exists for the wall's ordering.
 *
 * The rollup rule is the one that carries ADR 0020's weight: the four health signals are "never collapsed
 * into one light", and a branch summary is the one place in this UI that has to collapse something. The
 * tests below are what stop that collapse from becoming a lie — `unknown` never rounds to `ok`, one failing
 * leaf colours every ancestor, and the counts always travel beside the state.
 */

import { describe, expect, it } from 'vitest';
import type { Terminal, TerminalHealth } from '../panelsClient';
import {
  allBranchKeys,
  buildTree,
  describeBranch,
  flattenTree,
  rollupOne,
  worse,
} from './tree';

const OK: TerminalHealth = {
  reachable: 'ok',
  session: 'present',
  process: 'running',
  poller: 'live',
  loads: 'ok',
};
const UNMEASURED: TerminalHealth = {
  reachable: 'ok',
  session: 'present',
  process: 'running',
  poller: 'unknown',
  loads: 'unknown',
};
const FAILING: TerminalHealth = {
  reachable: 'ok',
  session: 'absent',
  process: 'unknown',
  poller: 'live',
  loads: 'ok',
  detail: 'kf-crawl-02 is up but has no session',
};
/**
 * A Worker that FINISHED inside a session that is still present — the failure the other three
 * fixtures cannot express, and the one this pass exists for. Every other signal is fine.
 */
const EXITED: TerminalHealth = {
  reachable: 'ok',
  session: 'present',
  process: 'exited',
  poller: 'live',
  loads: 'ok',
  detail: 'the command in this pane exited with status 143',
};

function terminal(id: string, health: TerminalHealth = OK): Terminal {
  // The tree reads `machine`/`window` only as a fallback for a malformed id, so the fields here are
  // derived from the id to keep the fixtures honest about what a real inventory carries.
  const [, rest = ''] = id.split(':');
  const [machine = '', , window = ''] = rest.split('/');
  return {
    id,
    machine,
    host: '10.0.0.1',
    publicIp: '',
    tag: 'crawl',
    fleet: 'webcrawl',
    actor: 'crawler',
    version: '1.2.3',
    window,
    health,
  };
}

const NONE: ReadonlySet<string> = new Set();

/**
 * WHICH FLEET, and only when there is one.
 *
 * The panel holding this tree used to call itself "Fleet", which put `local` inside a heading named
 * after the one mode it is not. The mode row is where a fleet can be named, and the name is never
 * the word `fleet`: `machinesFromStack` reads it off the Pulumi stack a run provisioned, and
 * `local.ts` leaves it empty deliberately — a local Worker belongs to no fleet, and inventing
 * `'local'` would put the mode in two places and let a selector disagree with itself.
 */
describe('the fleet a mode belongs to', () => {
  const inFleet = (id: string, fleet: string): Terminal => ({ ...terminal(id), fleet });

  it('collects the one fleet its Terminals name', () => {
    const [mode] = buildTree({
      inventory: [
        inFleet('fleet:kf-crawl-01/s/actor', 'sweep-aug'),
        inFleet('fleet:kf-crawl-02/s/actor', 'sweep-aug'),
      ],
      onWall: NONE,
      live: NONE,
    });
    expect(mode?.fleets).toEqual(['sweep-aug']);
  });

  it('keeps both when one inventory holds two fleets, rather than picking one', () => {
    const [mode] = buildTree({
      inventory: [
        inFleet('fleet:kf-crawl-01/s/actor', 'sweep-aug'),
        inFleet('fleet:kf-parse-01/s/actor', 'parse-sep'),
      ],
      onWall: NONE,
      live: NONE,
    });
    expect(mode?.fleets).toEqual(['sweep-aug', 'parse-sep']);
  });

  it('names no fleet for a local Machine, because nothing provisioned one', () => {
    // `local.ts` sets `fleet: ''` on every locally-served Worker, and an empty list is exactly what
    // keeps the row reading `local` instead of `local · something`.
    const [mode] = buildTree({
      inventory: [inFleet('local:main-droplet/kontra-probe/actor', '')],
      onWall: NONE,
      live: NONE,
    });
    expect(mode?.label).toBe('local');
    expect(mode?.fleets).toEqual([]);
  });
});

describe('worse', () => {
  it('orders bad over unknown over ok', () => {
    expect(worse('ok', 'ok')).toBe('ok');
    expect(worse('ok', 'unknown')).toBe('unknown');
    expect(worse('unknown', 'ok')).toBe('unknown');
    expect(worse('unknown', 'bad')).toBe('bad');
    expect(worse('bad', 'ok')).toBe('bad');
  });
});

describe('rollupOne', () => {
  it('is ok only when every signal was measured and good', () => {
    expect(rollupOne(OK)).toBe('ok');
  });

  it('never rounds unmeasured up to ok — the whole point of the tri-state', () => {
    expect(rollupOne(UNMEASURED)).toBe('unknown');
  });

  it('never rounds unmeasured down to bad either: a Machine nobody probed is not a failing one', () => {
    // The first thirty seconds after `fleet up` are unmeasured, and a wall of red then is a wall an
    // operator learns to ignore.
    expect(rollupOne({ reachable: 'unknown', session: 'unknown', poller: 'unknown', loads: 'unknown' })).toBe(
      'unknown'
    );
  });

  it('is bad when any one signal is definitely bad, whatever the other three say', () => {
    // The round-3 incident in miniature: three good signals must not outvote the one that failed.
    expect(rollupOne(FAILING)).toBe('bad');
    // A Worker that exited inside a present session is bad, and it is the case a tree of green
    // branches would otherwise hide: nothing else about it is wrong.
    expect(rollupOne(EXITED)).toBe('bad');
    // A streamer that predates the signal sends no `process` at all — unknown, never ok.
    expect(rollupOne({ reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' })).toBe(
      'unknown'
    );
    expect(rollupOne({ ...OK, loads: 'failing' })).toBe('bad');
    expect(rollupOne({ ...OK, reachable: 'fail' })).toBe('bad');
    expect(rollupOne({ ...OK, poller: 'none' })).toBe('bad');
    expect(rollupOne({ ...OK, session: 'no-tmux' })).toBe('bad');
  });

  /**
   * IT SKIPS THE AXES A MODE CANNOT ANSWER — the same `signalApplies` call `isHealthy` and the chips
   * make, and not a second rule.
   *
   * `loads` needs a vmagent only fleet Machines have and `reachable` is a transport only `fleet`
   * crosses, so on a local node those two are `unknown` for a structural reason. Counting them made
   * every local branch of this tree permanently "not fully measured", which is the tree's version of
   * the `0 serving` bug on the Actors page.
   */
  it('does not call a local branch unmeasured because of axes nothing could ever measure', () => {
    const local = {
      reachable: 'ok',
      session: 'present',
      process: 'running',
      poller: 'live',
      loads: 'unknown',
    } as const;
    expect(rollupOne(local, 'local')).toBe('ok');
    expect(rollupOne(local, 'docker')).toBe('ok');
    // Unchanged where the axis is real, and unchanged for a caller that does not pass a mode.
    expect(rollupOne(local, 'fleet')).toBe('unknown');
    expect(rollupOne(local)).toBe('unknown');
    // An unrecognised mode gets every axis, so a later build's mode is noisy rather than green.
    expect(rollupOne(local, 'kubernetes')).toBe('unknown');
  });

  it('still colours a local branch for an axis that genuinely fails, or one it could have measured', () => {
    // Omission is one-directional here too: the `bad` scan runs over every signal whatever the mode
    // says, and an axis that DOES apply is still unknown when it was not measured.
    const local = {
      reachable: 'ok',
      session: 'present',
      process: 'running',
      poller: 'live',
      loads: 'unknown',
    } as const;
    expect(rollupOne({ ...local, loads: 'failing' }, 'local')).toBe('bad');
    expect(rollupOne({ ...local, reachable: 'fail' }, 'local')).toBe('bad');
    expect(rollupOne({ ...local, poller: 'unknown' }, 'local')).toBe('unknown');
    expect(rollupOne({ ...local, process: 'unknown' }, 'local')).toBe('unknown');
  });
});

describe('buildTree', () => {
  it('groups by mode, node, session and window', () => {
    const tree = buildTree({
      inventory: [
        terminal('fleet:kf-crawl-01/kontra-webcrawl/actor'),
        terminal('fleet:kf-crawl-01/kontra-webcrawl/handler'),
        terminal('fleet:kf-crawl-02/kontra-webcrawl/actor'),
        terminal('local:dev-box/kontra-parse/actor'),
      ],
      onWall: NONE,
      live: NONE,
    });
    expect(tree.map((m) => m.key)).toEqual(['fleet', 'local']);
    expect(tree[0]?.nodes.map((n) => n.label)).toEqual(['kf-crawl-01', 'kf-crawl-02']);
    expect(tree[0]?.nodes[0]?.sessions.map((s) => s.label)).toEqual(['kontra-webcrawl']);
    expect(tree[0]?.nodes[0]?.sessions[0]?.windows.map((w) => w.label)).toEqual(['actor', 'handler']);
  });

  it('keeps two sessions on one node apart — the case that broke one-session-per-node', () => {
    // `ids.ts`: a `local` host runs `kontra-webcrawl` and `kontra-parse` at the same time, and keying by
    // the host would silently have kept one and dropped the other's tiles.
    const tree = buildTree({
      inventory: [
        terminal('local:dev-box/kontra-webcrawl/actor'),
        terminal('local:dev-box/kontra-parse/actor'),
      ],
      onWall: NONE,
      live: NONE,
    });
    expect(tree[0]?.nodes).toHaveLength(1);
    expect(tree[0]?.nodes[0]?.sessions.map((s) => s.label)).toEqual(['kontra-parse', 'kontra-webcrawl']);
    expect(tree[0]?.health.total).toBe(2);
  });

  it('orders nodes naturally, so kf-crawl-2 precedes kf-crawl-10', () => {
    const tree = buildTree({
      inventory: [
        terminal('fleet:kf-crawl-10/s/actor'),
        terminal('fleet:kf-crawl-2/s/actor'),
      ],
      onWall: NONE,
      live: NONE,
    });
    expect(tree[0]?.nodes.map((n) => n.label)).toEqual(['kf-crawl-2', 'kf-crawl-10']);
  });

  it('puts fleet first even though `docker` sorts before it alphabetically', () => {
    const tree = buildTree({
      inventory: [
        terminal('docker:worker-1/s/actor'),
        terminal('local:dev-box/s/actor'),
        terminal('fleet:kf-crawl-01/s/actor'),
      ],
      onWall: NONE,
      live: NONE,
    });
    expect(tree.map((m) => m.key)).toEqual(['fleet', 'docker', 'local']);
  });

  it('states the mode asymmetry ADR 0020 requires the UI to state', () => {
    const tree = buildTree({
      inventory: [terminal('fleet:kf-crawl-01/s/actor'), terminal('local:dev-box/s/actor')],
      onWall: NONE,
      live: NONE,
    });
    expect(tree[0]?.stakes.level).toBe('view');
    expect(tree[1]?.stakes.level).toBe('worker');
    expect(tree[1]?.stakes.sentence).toContain('kills a running Worker');
  });

  it('rolls one failing leaf all the way up, and never reports ok above it', () => {
    const tree = buildTree({
      inventory: [
        terminal('fleet:kf-crawl-01/s/actor', OK),
        terminal('fleet:kf-crawl-01/s/handler', FAILING),
        terminal('fleet:kf-crawl-02/s/actor', OK),
      ],
      onWall: NONE,
      live: NONE,
    });
    const [fleet] = tree;
    expect(fleet?.health.state).toBe('bad');
    expect(fleet?.health.total).toBe(3);
    expect(fleet?.health.failing).toBe(1);
    // …and the healthy Machine beside it still reads ok: the rollup colours ancestors, not siblings.
    expect(fleet?.nodes[0]?.health.state).toBe('bad');
    expect(fleet?.nodes[1]?.health.state).toBe('ok');
  });

  it('reports unknown, not ok, for a branch holding one unmeasured Terminal', () => {
    const tree = buildTree({
      inventory: [terminal('fleet:kf-crawl-01/s/actor', OK), terminal('fleet:kf-crawl-01/s/handler', UNMEASURED)],
      onWall: NONE,
      live: NONE,
    });
    expect(tree[0]?.health.state).toBe('unknown');
    expect(tree[0]?.health.unmeasured).toBe(1);
    expect(tree[0]?.health.failing).toBe(0);
  });

  it('counts what is on the wall and what is live, per branch', () => {
    const onWall = new Set(['fleet:kf-crawl-01/s/actor']);
    const live = new Set(['fleet:kf-crawl-01/s/actor']);
    const tree = buildTree({
      inventory: [terminal('fleet:kf-crawl-01/s/actor'), terminal('fleet:kf-crawl-01/s/handler')],
      onWall,
      live,
    });
    expect(tree[0]?.health.onWall).toBe(1);
    expect(tree[0]?.health.live).toBe(1);
    expect(tree[0]?.nodes[0]?.sessions[0]?.windows[0]?.onWall).toBe(true);
    expect(tree[0]?.nodes[0]?.sessions[0]?.windows[1]?.onWall).toBe(false);
  });

  it('shows a Terminal whose id is malformed rather than throwing it away', () => {
    // A streamer from a later build, or an id shape this one predates: a row in the tree is a far smaller
    // problem than a `useMemo` that throws and takes the Dashboard down.
    const tree = buildTree({
      inventory: [{ ...terminal('fleet:kf-crawl-01/s/actor'), id: 'nonsense', machine: 'kf-crawl-09', window: 'actor' }],
      onWall: NONE,
      live: NONE,
    });
    expect(tree).toHaveLength(1);
    expect(tree[0]?.nodes[0]?.label).toBe('kf-crawl-09');
    expect(tree[0]?.nodes[0]?.sessions[0]?.label).toBe('?');
  });
});

describe('describeBranch', () => {
  it('never reports a state without the counts behind it', () => {
    const tree = buildTree({
      inventory: [terminal('fleet:kf-crawl-01/s/actor', FAILING), terminal('fleet:kf-crawl-01/s/handler', OK)],
      onWall: new Set(['fleet:kf-crawl-01/s/actor']),
      live: NONE,
    });
    const words = describeBranch(tree[0]!.health);
    expect(words).toContain('2 Terminals');
    expect(words).toContain('1 failing');
    expect(words).toContain('1 on the wall');
  });

  it('says all signals ok only when nothing is failing and nothing is unmeasured', () => {
    const tree = buildTree({ inventory: [terminal('fleet:kf-crawl-01/s/actor', OK)], onWall: NONE, live: NONE });
    expect(describeBranch(tree[0]!.health)).toContain('all signals ok');
    const unknown = buildTree({
      inventory: [terminal('fleet:kf-crawl-01/s/actor', UNMEASURED)],
      onWall: NONE,
      live: NONE,
    });
    expect(describeBranch(unknown[0]!.health)).not.toContain('all signals ok');
  });
});

describe('flattenTree', () => {
  const tree = buildTree({
    inventory: [
      terminal('fleet:kf-crawl-01/s/actor'),
      terminal('fleet:kf-crawl-01/s/handler'),
      terminal('fleet:kf-crawl-02/s/actor'),
    ],
    onWall: NONE,
    live: NONE,
  });

  it('is fully expanded by default: the collapse set stores what is CLOSED', () => {
    // A tree that starts collapsed hides the Machine the operator came to find, and "expand everything" is
    // not a discoverable first action.
    const rows = flattenTree(tree, new Set());
    expect(rows.map((r) => r.kind)).toEqual([
      'mode',
      'node',
      'session',
      'window',
      'window',
      'node',
      'session',
      'window',
    ]);
  });

  it('drops a collapsed branch’s descendants and keeps the branch', () => {
    const rows = flattenTree(tree, new Set(['fleet:kf-crawl-01']));
    expect(rows.map((r) => r.key)).toEqual([
      'fleet',
      'fleet:kf-crawl-01',
      'fleet:kf-crawl-02',
      'fleet:kf-crawl-02/s',
      'fleet:kf-crawl-02/s/actor',
    ]);
  });

  it('collapses the whole tree to one row per mode', () => {
    expect(flattenTree(tree, new Set(allBranchKeys(tree))).map((r) => r.key)).toEqual(['fleet']);
  });

  it('gives every row a key that survives a refresh, so the keyboard cursor does', () => {
    const rows = flattenTree(tree, new Set());
    expect(new Set(rows.map((r) => r.key)).size).toBe(rows.length);
    // Rebuilt from the same inventory: identical keys, so an arrow-key cursor is not moved by the
    // 30-second poll.
    const again = flattenTree(
      buildTree({
        inventory: [
          terminal('fleet:kf-crawl-02/s/actor'),
          terminal('fleet:kf-crawl-01/s/handler'),
          terminal('fleet:kf-crawl-01/s/actor'),
        ],
        onWall: NONE,
        live: NONE,
      }),
      new Set()
    );
    expect(again.map((r) => r.key)).toEqual(rows.map((r) => r.key));
  });
});

/**
 * THE HEADER'S NUMBER AND THE ROW'S NAME.
 *
 * "MACHINES 5" over a single host running five panes is what the user caught: the noun said
 * machines and the number counted Terminals. And the machine under it read `54af48ee4c6a` — the
 * orchestrator CONTAINER's hostname, which names nothing anybody can act on (see
 * `panels/local.ts:localHost`, where that is fixed). The mode row stays `local`: it is a placement,
 * not a machine, and relabelling it `localhost` put the same word on two consecutive rows.
 */
describe('what the Machines panel counts and calls things', () => {
  it('keeps the mode row as the placement, not as a machine name', () => {
    const [mode] = buildTree({
      inventory: [terminal('local:localhost/probe-0_1_0/actor')],
      onWall: NONE,
      live: NONE,
    });
    expect(mode?.label).toBe('local');
    expect(mode?.key).toBe('local');
    expect(mode?.nodes[0]?.label).toBe('localhost');
  });

  /**
   * MACHINES ARE NODES, and the header's number has to be one of them.
   *
   * `MACHINES 5` over a single host running five panes is the count the user caught: the noun said
   * machines and the number counted Terminals. This is the arithmetic the header now does.
   */
  it('counts one machine per node, not one per Terminal', () => {
    const tree = buildTree({
      inventory: [
        terminal('local:localhost/kontra-0/claude'),
        terminal('local:localhost/nscheck/workflow'),
        terminal('local:localhost/probe-0_1_0/actor'),
        terminal('local:localhost/probe-0_1_0/handler'),
      ],
      onWall: NONE,
      live: NONE,
    });
    expect(tree.reduce((n, mode) => n + mode.nodes.length, 0)).toBe(1);
    expect(tree[0]?.health.total).toBe(4);
  });
});
