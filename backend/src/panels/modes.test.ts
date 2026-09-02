import { describe, expect, it } from 'vitest';
import { enabledModes, execArgv, MODE_BINARY, panelRunner, runnerFor } from './modes';
import { localRunner, localSessionsFromPanes, actorFromSession, localTarget } from './local';
import { dockerExecArgs, dockerRunner, machineFromContainer, parseWorkerContainers } from './docker';
import { realSshRunner } from './ssh';
import { modeOf, parseModes, reachWord, type ExecTarget } from './transport';
import { MODES } from './ids';
import { PanelServer, type PanelDeps } from './server';
import type { MachineTarget } from './discovery';
import type { ProbeResult } from './probe';

/**
 * The transport seam, and the three modes behind it (slice 6).
 *
 * WHAT THIS FILE IS FOR. The claim the mode model rests on is "one seam, three implementations:
 * converge, probe, snapshot and attach are unchanged in logic — only how a command reaches the tmux
 * server differs". A claim like that is only worth anything if the argv for each mode is pinned side
 * by side (so a change to one is visible against the other two) and if the code above the seam can be
 * shown never to branch on a mode. Both are here.
 */

const FLEET: ExecTarget = { mode: 'fleet', machine: 'kf-crawl-01', host: '10.124.0.9' };
const DOCKER: ExecTarget = { mode: 'docker', machine: 'kontra-webcrawl-0.2.0-0', host: 'kontra-webcrawl-0.2.0-0' };
const LOCAL: ExecTarget = { mode: 'local', machine: 'main-droplet', host: 'main-droplet' };

const PROBE = "tmux list-panes -a -F '#{session_name} #{window_name} #{pane_id} #{pane_width}x#{pane_height}'";

describe('the transport seam', () => {
  it('carries the SAME command string to three different places', () => {
    // The one property the whole slice rests on: the command is identical in all three modes, and a
    // single argv element in all three. `tmux.ts` builds shell; every mode runs shell.
    const fleet = execArgv(FLEET, PROBE);
    const docker = execArgv(DOCKER, PROBE);
    const local = execArgv(LOCAL, PROBE);

    expect(fleet.file).toBe('ssh');
    expect(fleet.args[fleet.args.length - 1]).toBe(PROBE);
    expect(fleet.args).toContain('root@10.124.0.9');

    expect(docker.file).toBe('docker');
    expect(docker.args).toEqual(['exec', 'kontra-webcrawl-0.2.0-0', 'sh', '-c', PROBE]);

    expect(local.file).toBe('sh');
    expect(local.args).toEqual(['-c', PROBE]);

    // …and the command is never split, quoted or wrapped on the way: exactly one argv element equals
    // it, so no local shell ever parses it.
    for (const { args } of [fleet, docker, local]) {
      expect(args.filter((a) => a === PROBE)).toHaveLength(1);
    }
  });

  it('defaults an unmarked target to the fleet, in one place', () => {
    // Slice 1 and slice 2 construct `{machine, host}` because fleet was the only mode. Those literals
    // must keep meaning `fleet`, and the default must live in ONE function so a mode cannot be
    // resolved differently at two call sites.
    expect(modeOf({ machine: 'kf-crawl-01', host: '10.0.0.1' })).toBe('fleet');
    expect(execArgv({ machine: 'kf-crawl-01', host: '10.0.0.1' }, PROBE).file).toBe('ssh');
    expect(modeOf({ mode: 'local', machine: 'x', host: 'x' })).toBe('local');
  });

  it('routes each mode to its own runner, and names the binary it needs', () => {
    expect(runnerFor('fleet')).toBe(realSshRunner);
    expect(runnerFor('docker')).toBe(dockerRunner);
    expect(runnerFor('local')).toBe(localRunner);
    expect(MODE_BINARY).toEqual({ fleet: 'ssh', docker: 'docker', local: 'sh' });
    // Every mode has a binary and a runner — a fourth mode added to `MODES` without wiring it here
    // fails this rather than silently falling through to `ssh`.
    for (const mode of MODES) {
      expect(MODE_BINARY[mode], mode).toBeTruthy();
      expect(runnerFor(mode), mode).toBeTruthy();
    }
  });

  it('refuses a node that does not match the mode claiming it, BEFORE any argv is built', () => {
    // The docker node becomes an argv element of a command that runs as the container's root. It is
    // admitted by `SAFE.container`, not by "whatever docker ps printed".
    expect(() => dockerExecArgs({ mode: 'docker', machine: 'a container', host: '' }, PROBE)).toThrow();
    expect(() => dockerExecArgs({ mode: 'docker', machine: '$(id)', host: '' }, PROBE)).toThrow();
    expect(() => execArgv({ mode: 'fleet', machine: 'main-droplet', host: '10.0.0.1' }, PROBE)).toThrow();
    expect(() => execArgv({ mode: 'fleet', machine: 'kf-crawl-01', host: 'a;id' }, PROBE)).toThrow();
  });

  it('says which transport failed, in the words of that transport', () => {
    // A sentence naming `ssh` on a local Terminal sends an operator looking for a key that is not
    // involved. The fleet's wording is unchanged from slice 1 — `probe.test.ts` reads it.
    expect(reachWord('fleet', 'kf-crawl-01')).toBe('ssh to kf-crawl-01');
    expect(reachWord('docker', 'kontra-webcrawl-0.2.0-0')).toContain('docker exec');
    expect(reachWord('local', 'main-droplet')).toContain('local tmux server');
  });

  it('parses KONTRA_PANEL_MODES, and a typo never means "all of them"', () => {
    expect(parseModes(undefined, MODES, MODES).modes).toEqual(['fleet', 'docker', 'local']);
    expect(parseModes('', MODES, ['fleet']).modes).toEqual(['fleet']);
    expect(parseModes('local', MODES, MODES).modes).toEqual(['local']);
    expect(parseModes(' FLEET , local ,local', MODES, MODES).modes).toEqual(['fleet', 'local']);
    // A word that is not a mode is dropped AND reported: quietly enabling everything would put the
    // docker socket and the host's tmux server in front of a Dashboard nobody asked to.
    const typo = parseModes('fleet,kubernetes', MODES, MODES);
    expect(typo.modes).toEqual(['fleet']);
    expect(typo.unknown).toEqual(['kubernetes']);
    expect(parseModes('nonsense', MODES, MODES).modes).toEqual([]);
  });

  it('reads the environment for the enabled modes, defaulting to all three', () => {
    const saved = process.env.KONTRA_PANEL_MODES;
    try {
      delete process.env.KONTRA_PANEL_MODES;
      expect(enabledModes().modes).toEqual(['fleet', 'docker', 'local']);
      process.env.KONTRA_PANEL_MODES = 'fleet';
      expect(enabledModes().modes).toEqual(['fleet']);
    } finally {
      if (saved === undefined) delete process.env.KONTRA_PANEL_MODES;
      else process.env.KONTRA_PANEL_MODES = saved;
    }
  });

  it('dispatches a real run through the LOCAL transport with no ssh anywhere', async () => {
    // The one mode whose runner can be proven for real without a fleet, a daemon or a network: a
    // local `sh` is always here. Exit codes, stdout and stderr all come back through the same
    // `ExecResult` every mode answers with.
    const ok = await panelRunner.run(localTarget('main-droplet'), 'printf hello; printf oops >&2');
    expect(ok).toMatchObject({ code: 0, stdout: 'hello', stderr: 'oops', timedOut: false });
    const bad = await panelRunner.run(localTarget('main-droplet'), 'exit 7');
    expect(bad.code).toBe(7);
  });

  it('bounds a local command that never returns, and reports the timeout as one', async () => {
    // The bound is shared with every other mode (`captureChild`), which is why proving it once here
    // proves it for the two that need a Machine or a daemon to prove it on.
    //
    // REGRESSION, and it is the bug this test was written after finding: awaiting `'close'` alone
    // HUNG here forever. Measured on dash — `sh -c 'sleep 30'` FORKS, so SIGKILLing the shell leaves
    // `sleep` holding the write end of our stdout pipe, `'close'` never fires, and one wedged probe
    // pins the streamer's discovery loop. Fixed twice over: the child is spawned in its own process
    // group so the whole group is killed, and `'exit'` starts a bounded drain either way.
    const started = Date.now();
    const res = await panelRunner.run(localTarget('main-droplet'), 'sleep 30', { timeoutMs: 150 });
    expect(res.timedOut).toBe(true);
    expect(res.code).not.toBe(0);
    // Generous, because it is asserting "bounded", not a latency. The failure mode it catches is
    // 30 seconds or forever.
    expect(Date.now() - started).toBeLessThan(5000);
  });

  it('never opens a writable descriptor toward a session, in any mode', async () => {
    // The read-only boundary, measured rather than asserted about: a command that READS its stdin
    // gets EOF immediately, because the child's first descriptor is /dev/null in every transport.
    const res = await panelRunner.run(localTarget('main-droplet'), 'cat; printf "[eof]"');
    expect(res.code).toBe(0);
    expect(res.stdout).toBe('[eof]');
  });
});

describe('mode local: the inventory IS the session list', () => {
  it('groups panes into one target per kontra session, and ignores everyone else', () => {
    const rows = [
      { session: 'webcrawl-0_2_0', window: 'actor', paneId: '%0', cols: 200, rows: 50, kontra: 'actor:webcrawl:0.2.0' },
      { session: 'webcrawl-0_2_0', window: 'handler', paneId: '%1', cols: 200, rows: 50, kontra: 'actor:webcrawl:0.2.0' },
      { session: 'webcrawl-0_2_0', window: 'handler', paneId: '%2', cols: 200, rows: 50, kontra: 'actor:webcrawl:0.2.0' },
      { session: 'parse-1_0_0', window: 'actor', paneId: '%3', cols: 80, rows: 24, kontra: 'actor:parse:1.0.0' },
      // An operator's own work is not the Dashboard's business — and it is untagged, which is now
      // the whole of what distinguishes it. The name no longer has to carry a namespace.
      { session: 'my-editor', window: 'vim', paneId: '%4', cols: 80, rows: 24, kontra: '' },
      // A window name tmux allows but a Terminal id cannot carry is dropped, never repaired.
      { session: 'webcrawl-0_2_0', window: 'a window', paneId: '%5', cols: 80, rows: 24, kontra: 'actor:webcrawl:0.2.0' },
    ];
    const targets = localSessionsFromPanes(rows, 'main-droplet');
    expect(targets.map((t) => t.session)).toEqual(['parse-1_0_0', 'webcrawl-0_2_0']);
    expect(targets.every((t) => t.mode === 'local' && t.machine === 'main-droplet')).toBe(true);
    // TWO sessions on ONE node — the case that made the streamer key by node-and-session.
    const webcrawl = targets.find((t) => t.session === 'webcrawl-0_2_0');
    expect(webcrawl?.windows).toEqual(['actor', 'handler']);
    expect(webcrawl?.actor).toBe('webcrawl');
    // THE VERSION, from the tag. It used to be '' because a session name never carried one and
    // guessing would have named the wrong Temporal queue — so `queueForMachine` could not resolve a
    // local Worker's queue at all. The tag was written by the thing that knew both halves.
    expect(webcrawl?.version).toBe('0.2.0');
  });

  it('still finds a Worker that was already running before the tag existed', () => {
    // A session named `kontra-<actor>` with no `@kontra` on it. Dropping it from the wall would
    // report a live Worker as absent, which is the one thing ADR 0020 says a tile may never do.
    const targets = localSessionsFromPanes(
      [{ session: 'kontra-webcrawl', window: 'actor', paneId: '%0', cols: 80, rows: 24, kontra: '' }],
      'main-droplet'
    );
    expect(targets.map((t) => t.session)).toEqual(['kontra-webcrawl']);
    expect(targets[0]?.actor).toBe('webcrawl');
    // And it still reports no version rather than inventing one from a name that has none.
    expect(targets[0]?.version).toBe('');
  });

  it('does not put a served WORKFLOW on the Actors surface', () => {
    // `workflow:<name>` is a kontra session and is not an actor. Reading a name out of it would
    // invent an Actor nobody deployed, and name a Temporal queue nothing polls.
    const targets = localSessionsFromPanes(
      [{ session: 'enumerate_scope', window: 'workflow', paneId: '%0', cols: 80, rows: 24, kontra: 'workflow:enumerate_scope' }],
      'main-droplet'
    );
    expect(targets).toHaveLength(1);
    expect(targets[0]?.actor).toBe('');
    expect(targets[0]?.version).toBe('');
  });

  it('promises no windows a session does not have — there is no local converge', () => {
    // The fleet unions "what the converge would create" with "what the probe found", because a
    // Machine with no session is a tile with a converge. Locally the converge would mean starting a
    // Worker, so a session that is not there is simply not there.
    expect(localSessionsFromPanes([], 'main-droplet')).toEqual([]);
    expect(actorFromSession('kontra-webcrawl')).toBe('webcrawl');
    expect(actorFromSession('something-else')).toBe('');
  });
});

describe('mode docker: the label cli/scale.go writes IS the filter', () => {
  it('reads name, actor and version off `docker ps`, and drops what cannot be an id', () => {
    const containers = parseWorkerContainers(
      [
        'kontra-webcrawl-0.2.0-0\twebcrawl@0.2.0\trunning',
        'kontra-webcrawl-0.2.0-1\twebcrawl@0.2.0\texited',
        // Several names on one container: the first is the one `scale` created it with.
        'kontra-parse-1.0-0,alias\tparse@1.0\trunning',
        // Hostile or unusable rows are dropped rather than repaired — the name becomes an argv
        // element of a command that runs as the container's root.
        '$(id)\tevil@1.0\trunning',
        'a container\tx@1\trunning',
        '',
      ].join('\n')
    );
    expect(containers.map((c) => c.container)).toEqual([
      'kontra-parse-1.0-0',
      'kontra-webcrawl-0.2.0-0',
      'kontra-webcrawl-0.2.0-1',
    ]);
    expect(containers[1]).toEqual({
      container: 'kontra-webcrawl-0.2.0-0',
      actor: 'webcrawl',
      version: '0.2.0',
      state: 'running',
    });
  });

  it('turns a container into a target with the session a worker WOULD hold', () => {
    const m = machineFromContainer({
      container: 'kontra-webcrawl-0.2.0-0',
      actor: 'webcrawl',
      version: '0.2.0',
      state: 'running',
    });
    expect(m).toMatchObject({
      mode: 'docker',
      machine: 'kontra-webcrawl-0.2.0-0',
      // `<actor>-<version>`, the same rule a Machine follows. Two versions scaled side by side get
      // two sessions; `kontra-webcrawl` gave them one name and one tile.
      session: 'webcrawl-0_2_0',
      actor: 'webcrawl',
      version: '0.2.0',
    });
    // The expected window names, so a container with NO session is a visible tile with a sentence
    // rather than an absence — nothing here claims they exist, the probe decides that.
    expect(m.windows).toEqual(['actor', 'handler']);
  });
});

/**
 * The streamer with a MIXED inventory.
 *
 * `refresh()` and `terminalList()` are public, so the whole discovery→probe→Terminal path can be
 * driven without a socket. What it pins is the bug that keying by node would have caused: two local
 * sessions on ONE host.
 */
describe('a wall with all three modes on it', () => {
  const fleet: MachineTarget = {
    mode: 'fleet',
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
  const local = (session: string): MachineTarget => ({
    mode: 'local',
    machine: 'main-droplet',
    host: 'main-droplet',
    publicIp: '',
    tag: '',
    fleet: '',
    actor: session.replace('kontra-', ''),
    version: '',
    session,
    windows: ['actor', 'handler'],
  });
  const container: MachineTarget = {
    mode: 'docker',
    machine: 'kontra-webcrawl-0.2.0-0',
    host: 'kontra-webcrawl-0.2.0-0',
    publicIp: '',
    tag: '',
    fleet: '',
    actor: 'webcrawl',
    version: '0.2.0',
    session: 'kontra-webcrawl',
    windows: ['actor', 'handler'],
  };

  function deps(over: Partial<PanelDeps> = {}): PanelDeps {
    return {
      discover: async () => [fleet, local('kontra-webcrawl'), local('kontra-parse'), container],
      probe: async (): Promise<ProbeResult> => ({
        reachable: 'ok',
        session: 'present',
        windows: ['actor', 'handler'],
      }),
      snapshot: async () => new Map<string, string>(),
      log: () => undefined,
      ...over,
    };
  }

  it('keeps every session of every node, and prefixes each id with its mode', async () => {
    const server = new PanelServer(deps());
    await server.refresh();
    expect(server.terminalList().map((t) => t.id)).toEqual([
      'docker:kontra-webcrawl-0.2.0-0/kontra-webcrawl/actor',
      'docker:kontra-webcrawl-0.2.0-0/kontra-webcrawl/handler',
      'fleet:kf-crawl-01/kontra-webcrawl/actor',
      'fleet:kf-crawl-01/kontra-webcrawl/handler',
      // BOTH local sessions, on one host. Keyed by node alone, one of these two pairs would have
      // silently replaced the other and its tiles would have vanished from the wall.
      'local:main-droplet/kontra-parse/actor',
      'local:main-droplet/kontra-parse/handler',
      'local:main-droplet/kontra-webcrawl/actor',
      'local:main-droplet/kontra-webcrawl/handler',
    ]);
    // Three nodes, not four sessions: `machines` on the health route has always meant nodes.
    expect(server.nodeCount()).toBe(3);
    await server.close();
  });

  it('puts the mode on the wire beside the id, for a tile that must say which it is', async () => {
    const server = new PanelServer(deps());
    await server.refresh();
    for (const t of server.terminalList()) {
      expect(t.mode, t.id).toBe(t.id.slice(0, t.id.indexOf(':')));
    }
    await server.close();
  });

  it('asks the probe for every node exactly once, whatever its mode', async () => {
    // The claim: the probe does not branch on a mode. It is called with a target and answers, and the
    // transport underneath it is the only thing that differs.
    const asked: string[] = [];
    const server = new PanelServer(
      deps({
        probe: async (m) => {
          asked.push(`${m.mode ?? 'fleet'}:${m.machine}/${m.session}`);
          return { reachable: 'ok', session: 'present', windows: ['actor'] };
        },
      })
    );
    await server.refresh();
    expect(asked.sort()).toEqual([
      'docker:kontra-webcrawl-0.2.0-0/kontra-webcrawl',
      'fleet:kf-crawl-01/kontra-webcrawl',
      'local:main-droplet/kontra-parse',
      'local:main-droplet/kontra-webcrawl',
    ]);
    await server.close();
  });
});
