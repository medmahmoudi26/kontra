import { describe, expect, it } from 'vitest';
import { discoverMachines, machinesFromStack, sessionNameFor, type StackReader } from './discovery';
import { interpretProbe, terminalsForMachine } from './probe';
import {
  parseBatchSnapshot,
  parseListPanes,
  batchSnapshotCommand,
  paneExitFromScreen,
  paneProcess,
  CLEAR_HOME,
} from './tmux';
import type { StackState } from '../infra/state';

const MACHINE = {
  machine: 'kf-crawl-01',
  host: '10.124.0.9',
  publicIp: '203.0.113.9',
  tag: 'crawl',
  fleet: 'run-apex-119',
  actor: 'webcrawl',
  version: '0.2.0',
  session: 'kontra-webcrawl',
  windows: ['actor', 'handler'],
};

/** A checkpoint's `outputs`, as `programs/fleet.ts` writes them. */
function outputs(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    tag: 'crawl',
    machines: 2,
    actorName: 'webcrawl',
    actorVersion: '0.2.0',
    inventory: {
      'kf-crawl-01': {
        name: 'kf-crawl-01',
        host: '10.124.0.9',
        publicIp: '203.0.113.9',
        tag: 'crawl',
      },
      'kf-crawl-02': {
        name: 'kf-crawl-02',
        host: '10.124.0.10',
        publicIp: '203.0.113.10',
        tag: 'crawl',
      },
    },
    ...over,
  };
}

describe('discovery from the Fleet inventory', () => {
  it('reads the inventory Fleet publishes, and nothing provider-shaped', () => {
    const machines = machinesFromStack('kontra-fleet/run-apex-119', outputs());
    expect(machines.map((m) => m.machine)).toEqual(['kf-crawl-01', 'kf-crawl-02']);
    expect(machines[0]).toMatchObject({
      host: '10.124.0.9',
      publicIp: '203.0.113.9',
      tag: 'crawl',
      fleet: 'run-apex-119',
      actor: 'webcrawl',
      version: '0.2.0',
      session: 'webcrawl-0_2_0',
    });
  });

  it('keeps a Machine with NO placement — it is a tile, not an absence', () => {
    // `fleet up` with no deploy: the stack echoes empty placement fields. A Machine that exists
    // with no session must still be visible, with a converge.
    const machines = machinesFromStack('kontra-fleet/c1', outputs({ actorName: '', actorVersion: '' }));
    expect(machines).toHaveLength(2);
    expect(machines[0]?.actor).toBe('');
    expect(machines[0]?.session).toBe('crawl'); // falls back to the fleet's tag
  });

  it('drops a machine name that could not be a Terminal id rather than repairing it', () => {
    const hostile = machinesFromStack('kontra-fleet/c1', {
      inventory: {
        evil: { name: 'kf-crawl-01; id', host: '10.0.0.1', publicIp: '1.1.1.1', tag: 'crawl' },
        controller: { name: 'main-droplet', host: '10.0.0.2', publicIp: '1.1.1.2', tag: 'crawl' },
      },
    });
    expect(hostile).toEqual([]);
  });

  it('reads only kontra-fleet stacks', async () => {
    const reader: StackReader = {
      listStacks: async () => ['kontra-fleet/c1', 'someone-elses/stack'],
      readStack: async (fqn) =>
        fqn === 'kontra-fleet/c1'
          ? ({ fqn, project: 'kontra-fleet', stack: 'c1', outputs: outputs(), resources: [] } as StackState)
          : ({
              fqn,
              project: 'x',
              stack: 'y',
              outputs: outputs(),
              resources: [],
            } as StackState),
    };
    const machines = await discoverMachines(reader);
    expect(machines).toHaveLength(2);
    expect(machines.every((m) => m.fleet === 'c1')).toBe(true);
  });

  it('survives a stack that has never been converged', async () => {
    const reader: StackReader = {
      listStacks: async () => ['kontra-fleet/never'],
      readStack: async () => null,
    };
    expect(await discoverMachines(reader)).toEqual([]);
  });

  it('names a session after the ACTOR AND ITS VERSION, with no prefix', () => {
    // THE TABLE OF ANSWERS moved to conformance/queues.json §tmux_session, which `cli/fleet.go`
    // executes too — this Machine's session name is minted on both sides and a drift draws a
    // Machine whose Worker is running perfectly as one with NO SESSION. What stays here is the
    // sentence a reader of THIS file needs: the version is the substantive half. `kontra-webcrawl`
    // said which Actor was on a Machine and not which BUILD, so two fleets running two versions of
    // one Actor produced two identical session names and `tmux attach -t kontra-webcrawl` was
    // ambiguous the moment a deploy was in flight. The prefix went because it was the Monitor's
    // discovery mechanism, and discovery is a tmux user option now — one that can also say what
    // KIND a session is.
    expect(sessionNameFor('webcrawl', '0.2.0')).not.toBe(sessionNameFor('webcrawl', '0.3.0'));
    expect(sessionNameFor('webcrawl', '0.2.0').startsWith('kontra-')).toBe(false);
  });
});

describe('the list-panes probe — three failures, three tiles', () => {
  it('reports a reachable Machine with the session present', () => {
    const stdout = [
      'kontra-webcrawl actor %0 200x50',
      'kontra-webcrawl handler %1 200x50',
      'someone-elses actor %2 80x24',
    ].join('\n');
    const probe = interpretProbe(MACHINE, 0, stdout, '');
    expect(probe).toMatchObject({ reachable: 'ok', session: 'present', windows: ['actor', 'handler'] });
    expect(probe.detail).toBeUndefined();
  });

  it('separates no-tmux from no-session from unreachable', () => {
    const noTmux = interpretProbe(MACHINE, 127, '', 'bash: tmux: command not found');
    expect(noTmux).toMatchObject({ reachable: 'ok', session: 'no-tmux' });
    expect(noTmux.detail).toMatch(/no tmux/);

    const noServer = interpretProbe(MACHINE, 1, '', 'no server running on /tmp/tmux-0/default');
    expect(noServer).toMatchObject({ reachable: 'ok', session: 'absent' });

    const dead = interpretProbe(MACHINE, 255, '', 'ssh: connect to host 10.124.0.9 port 22: No route to host');
    expect(dead).toMatchObject({ reachable: 'fail', session: 'unknown' });

    const wedged = interpretProbe(MACHINE, 124, '', '', true);
    expect(wedged).toMatchObject({ reachable: 'fail', session: 'unknown' });
    expect(wedged.detail).toMatch(/timed out/);
  });

  it('reports a Machine whose tmux runs OTHER sessions as absent, with words', () => {
    const probe = interpretProbe(MACHINE, 0, 'unrelated shell %0 80x24', '');
    expect(probe).toMatchObject({ reachable: 'ok', session: 'absent', windows: [] });
    expect(probe.detail).toContain('kontra-webcrawl');
  });

  it('parses list-panes tolerantly', () => {
    const rows = parseListPanes('s w %0 200x50\ngarbage line\n\ns2 w2 %1 80x24');
    expect(rows).toHaveLength(2);
    // No `@kontra` on these lines: an untagged session, which is what every pane on a tmux server
    // kontra did not create looks like, and what a Worker started before the tag existed looks like.
    // These are the SHORT layout — no `pane_dead`, no command — and the pane fields come out empty
    // rather than guessed, which is what a tile then renders as `—`.
    expect(rows[0]).toEqual({
      session: 's',
      window: 'w',
      paneId: '%0',
      cols: 200,
      rows: 50,
      dead: false,
      command: '',
      exitStatus: '',
      kontra: '',
    });
  });

  /**
   * The pane fields, and the empty-field bug that shipped in the first version of them.
   *
   * `@kontra_exit` is empty on every pane whose command has not exited, so an unprefixed layout
   * rendered two adjacent spaces, `split(/\s+/)` collapsed them, and the `@kontra` tag arrived one
   * field to the left — which took every tagged session off the wall. `local.tmux.test.ts` caught it
   * against a real tmux. `c:`/`x:` make both fields non-empty by construction.
   */
  it('reads the pane fields, and an empty exit status does not shift the tag', () => {
    const rows = parseListPanes(
      'nscheck-0_1_0 actor %0 200x50 0 c:zsh x: actor:nscheck:0.1.0\n' +
        'nscheck-0_1_0 handler %1 200x50 0 c:zsh x:143 actor:nscheck:0.1.0\n' +
        'kf-crawl-01 actor %2 200x50 1 c: x: \n'
    );
    expect(rows[0]).toMatchObject({ command: 'zsh', dead: false, exitStatus: '', kontra: 'actor:nscheck:0.1.0' });
    expect(rows[1]).toMatchObject({ command: 'zsh', exitStatus: '143', kontra: 'actor:nscheck:0.1.0' });
    // A dead pane may report no command at all, and that must not shift anything either.
    expect(rows[2]).toMatchObject({ dead: true, command: '', exitStatus: '', kontra: '' });
  });

  /**
   * THE ONE INFERENCE THIS FILE REFUSES TO MAKE.
   *
   * MEASURED on this box (tmux 3.3a): `pane_current_command` reports `zsh` for a pane whose Go
   * Worker is running, because kontra's hold shell does not put the Worker in its own process group
   * and tmux reads the foreground pgid. `ps --ppid <pane_pid>` showed the Worker alive under the
   * same shell tmux was calling the current command. So a shell is not evidence of an exit, and the
   * verdict for one is `unknown` with the reason — never `exited`.
   */
  it('never reads a hold shell as a dead Worker, and says why it cannot tell', () => {
    const running = paneProcess({ dead: false, command: 'zsh', exitStatus: '' });
    expect(running.process).toBe('unknown');
    expect(running.detail).toMatch(/hold shell|running or finished/);

    // A non-shell command IS a reading: something is genuinely in the foreground.
    expect(paneProcess({ dead: false, command: 'journalctl', exitStatus: '' }).process).toBe('running');
    expect(paneProcess({ dead: false, command: 'python', exitStatus: '' }).detail).toBeUndefined();

    // The two sound ways to learn it is over.
    expect(paneProcess({ dead: true, command: '', exitStatus: '' }).process).toBe('exited');
    const held = paneProcess({ dead: false, command: 'zsh', exitStatus: '143' });
    expect(held.process).toBe('exited');
    expect(held.exitStatus).toBe('143');
    expect(held.detail).toMatch(/143/);
  });

  /**
   * The screen is the third source, and the only one that works on a Worker started before
   * `@kontra_exit` existed — which on this host was every Worker running when this landed.
   */
  it('reads the hold banner off a screen, and only as the last non-empty line', () => {
    expect(paneExitFromScreen('some output\n[exited 143] press any key to close this window\n\n')).toBe('143');
    // With the `-e` capture the line arrives wrapped in escape sequences.
    expect(paneExitFromScreen('\x1b[m[exited 0] press any key to close this window\x1b[K\n')).toBe('0');
    // A worker that PRINTED the banner and kept going is not finished: the hold blocks on `read`, so
    // nothing can follow the real banner.
    expect(paneExitFromScreen('[exited 1] press any key to close this window\nstill going\n')).toBe('');
    expect(paneExitFromScreen('ordinary output\n')).toBe('');
    expect(paneExitFromScreen('')).toBe('');
  });

  /** A window's pane facts reach the Terminal, per window — one can be finished while its sibling
   *  runs, and a Machine-level answer would have to be wrong about one of them. */
  it('carries each window’s own pane command, geometry and verdict onto its Terminal', () => {
    const stdout = [
      'kontra-webcrawl actor %0 200x50 0 c:python x: ',
      'kontra-webcrawl handler %1 120x30 0 c:zsh x:2 ',
    ].join('\n');
    const probe = interpretProbe(MACHINE, 0, stdout, '');
    const terminals = terminalsForMachine(MACHINE, probe);
    const actor = terminals.find((t) => t.window === 'actor');
    const handler = terminals.find((t) => t.window === 'handler');
    expect(actor).toMatchObject({ command: 'python', paneCols: 200, paneRows: 50, exitStatus: '' });
    expect(actor?.health.process).toBe('running');
    expect(handler).toMatchObject({ command: 'zsh', paneCols: 120, paneRows: 30, exitStatus: '2' });
    expect(handler?.health.process).toBe('exited');
    expect(handler?.health.detail).toMatch(/status 2/);
    // The pane's sentence never overwrites the Machine's, and the session signal is untouched by it.
    expect(handler?.health.session).toBe('present');
  });

  it('reads the @kontra tag, and never lets it shift the fields that address a pane', () => {
    // The tag is the one field kontra does not control the shape of — an operator may have set
    // `@kontra` on a session of their own, with anything in it. Last means a value with a space in
    // it can only corrupt itself.
    const rows = parseListPanes(
      'nscheck-0.1.0 actor %0 200x50 actor:nscheck:0.1.0\n' +
        'mine w %1 80x24 something an operator typed'
    );
    expect(rows[0]).toMatchObject({ session: 'nscheck-0.1.0', kontra: 'actor:nscheck:0.1.0' });
    expect(rows[1]).toMatchObject({
      session: 'mine',
      window: 'w',
      cols: 80,
      rows: 24,
      kontra: 'something an operator typed',
    });
  });
});

describe('Terminals for a Machine', () => {
  it('never reports unmeasured signals as healthy', () => {
    const terminals = terminalsForMachine(MACHINE, {
      reachable: 'ok',
      session: 'present',
      windows: ['actor', 'handler'],
    });
    for (const t of terminals) {
      // Slice 3 fills these in. Until then they are `unknown`, which a tile must not draw as ok.
      expect(t.health.poller).toBe('unknown');
      expect(t.health.loads).toBe('unknown');
      expect(t.health.reachable).toBe('ok');
    }
  });

  it('shows tiles for a Machine with no session at all', () => {
    // Acceptance criterion 5: `tmux kill-session` renders "no session — converge", never a quiet
    // tile and never a missing one.
    const terminals = terminalsForMachine(MACHINE, {
      reachable: 'ok',
      session: 'absent',
      windows: [],
      detail: 'kf-crawl-01 is up but has no session kontra-webcrawl — converge to create it',
    });
    expect(terminals.map((t) => t.id)).toEqual([
      'fleet:kf-crawl-01/kontra-webcrawl/actor',
      'fleet:kf-crawl-01/kontra-webcrawl/handler',
    ]);
    expect(terminals[0]?.health.session).toBe('absent');
    expect(terminals[0]?.health.detail).toContain('converge');
  });

  it('reports one killed window as absent while its sibling stays present', () => {
    const terminals = terminalsForMachine(MACHINE, {
      reachable: 'ok',
      session: 'present',
      windows: ['actor'],
    });
    expect(terminals.find((t) => t.window === 'actor')?.health.session).toBe('present');
    const handler = terminals.find((t) => t.window === 'handler');
    expect(handler?.health.session).toBe('absent');
    expect(handler?.health.detail).toContain('converge');
  });

  it('surfaces an operator’s hand-made window, and skips one that cannot be an id', () => {
    const terminals = terminalsForMachine(MACHINE, {
      reachable: 'ok',
      session: 'present',
      windows: ['actor', 'handler', 'scratch', 'a window with spaces'],
    });
    expect(terminals.map((t) => t.window)).toEqual(['actor', 'handler', 'scratch']);
  });
});

describe('the batched snapshot', () => {
  it('is ONE exec covering every window asked for', () => {
    // Two browsers on one Machine cost one exec per interval (acceptance criterion 7); the
    // per-window command shape is the one ADR 0020 pins.
    const cmd = batchSnapshotCommand('kontra-webcrawl', ['actor', 'handler']);
    expect(cmd.match(/tmux capture-pane/g)).toHaveLength(2);
    expect(cmd).toContain("tmux capture-pane -p -e -S -50 -t 'kontra-webcrawl:actor'");
    // One killed window must not cost the Machine its whole snapshot.
    expect(cmd.match(/\|\| true/g)).toHaveLength(2);
    expect(() => batchSnapshotCommand('kontra-webcrawl', ['a;id'])).toThrow();
    expect(() => batchSnapshotCommand('kontra-webcrawl', [])).toThrow();
  });

  it('splits the reply back per window, and pane content cannot forge a boundary', () => {
    const screens = parseBatchSnapshot('\x1eactor\x1fline one\nline two\n\x1ehandler\x1fother');
    expect(screens.get('actor')).toBe('line one\nline two\n');
    expect(screens.get('handler')).toBe('other');
    // RS/US are control characters a `capture-pane -e` does not emit, so a pane printing the words
    // does not create a record.
    expect(parseBatchSnapshot('\x1eactor\x1f\x1b[31mRS US\x1b[0m').get('actor')).toContain('RS US');
  });

  it('prefixes a repaint with home+clear', () => {
    expect(CLEAR_HOME).toBe('\x1b[H\x1b[2J');
  });
});
