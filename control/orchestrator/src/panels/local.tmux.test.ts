import { execFileSync } from 'node:child_process';
import { afterAll, describe, expect, it } from 'vitest';
import { attachCommand, killViewerCommand, newViewerSession, realPtyRunner } from './attach';
import { discoverLocalSessions, localRunner, localTarget } from './local';
import { probeMachine, terminalsForMachine } from './probe';
import {
  batchSnapshotCommand,
  LIST_PANES_COMMAND,
  paneExitFromScreen,
  parseBatchSnapshot,
} from './tmux';
import type { CommandRunner, PtyChannel } from './transport';

/**
 * MODE `local`, end to end, against a REAL tmux — discovery, probe, snapshot and a live attach.
 *
 * This is the file that makes slice 6 worth building. Every other proof of a Terminal in this repo
 * stops at a fake, because no fleet exists in this environment and no Machine can be SSHed to. Local
 * mode has no such gap: the transport is a local shell, so the same command strings, the same
 * `list-panes` parse, the same `capture-pane` framing and the same PTY attach can be run for real, on
 * this box, in under two seconds. What that proves about the fleet path is everything except SSH.
 *
 * THE SOCKET IS PRIVATE, and that is not a style rule. Establishing ADR 0020's findings on the default
 * socket segfaulted the operator's own tmux server and destroyed a session that had been running for a
 * day, unrecoverably. So: `-L kontratest`, every session named after this process, and nothing else
 * ever killed. The one deliberate difference from production is that `-L` — which is exactly the
 * difference `attach.tmux.test.ts` already lives with, and it is injected the same way.
 *
 * NOTE ON WHY THE SOCKET CAN BE INJECTED AT ALL: `discoverLocalSessions` takes its list command as a
 * parameter and every other step takes a `CommandRunner`. That seam exists for this test, and this
 * test is why it exists.
 */

const SOCKET = 'kontratest';
const SESSION = `kontra-slice6-${process.pid}`;
/** A session named the way kontra names them NOW — no prefix — found only by its `@kontra` tag. */
// Underscores, because tmux would rewrite a dot to one anyway — see `tmuxSafeName`.
const TAGGED = `tagged-${process.pid}-0_2_0`;
/** An operator's own session: no prefix, no tag. It must stay off the wall. */
const MINE = `not-kontras-${process.pid}`;
/** A session tagged with a kind this build does not know — ADR 0043 says it stays off the wall
 *  until an operator re-tags it `watch:`, which is the assertion that its panes hold nothing
 *  that exists only there. */
const UNKNOWN_KIND = `unknown-kind-${process.pid}`;
/** A Worker under the real hold wrapper, so its process can be killed under it. */
const HELD = `held-${process.pid}-0_1_0`;
const HOST = 'kontratest-host';
const VIEWER = newViewerSession();

function tmux(...args: string[]): string {
  return execFileSync('tmux', ['-L', SOCKET, ...args], {
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  }).trim();
}

function tmuxOk(...args: string[]): boolean {
  try {
    tmux(...args);
    return true;
  } catch {
    return false;
  }
}

function have(bin: string): boolean {
  try {
    execFileSync('sh', ['-c', `command -v ${bin}`], { stdio: 'ignore' });
    return true;
  } catch {
    return false;
  }
}

/** The private socket, injected into whatever command the panels path built. The FIRST `tmux ` of
 * each command is the invocation; `capture-pane` batches several, hence the global replace. */
function socketize(command: string): string {
  return command.replaceAll('tmux ', `tmux -L ${SOCKET} `);
}

/**
 * The local transport, pointed at this test's own socket.
 *
 * A wrapper around the REAL `localRunner` rather than a fake: the child, its descriptors, the shell
 * that parses the command and the tmux that answers it are all real, and the only thing this adds is
 * the socket. If the seam were not a seam, this wrapper could not exist.
 */
const privateRunner: CommandRunner = {
  run: (target, command, opts) => localRunner.run(target, socketize(command), opts),
};

const runnable = have('tmux') && have('script');

afterAll(() => {
  // Only what this test created, by exact name. Never `kill-server`, never a prefix sweep.
  tmuxOk('kill-session', '-t', VIEWER);
  tmuxOk('kill-session', '-t', SESSION);
  tmuxOk('kill-session', '-t', TAGGED);
  tmuxOk('kill-session', '-t', UNKNOWN_KIND);
  tmuxOk('kill-session', '-t', MINE);
  tmuxOk('kill-session', '-t', HELD);
});

describe.skipIf(!runnable)('mode local against a real tmux (private socket)', () => {
  it(
    'discovers, probes, snapshots and attaches — the whole read path, no fleet involved',
    async () => {
      // A local worker session, shaped exactly as `kontra serve --actor <dir> --mode local --tmux`
      // shapes one: `kontra-<actor>`, one window per process, and the processes THEMSELVES in the
      // panes — which is the safety asymmetry this mode has to surface, and the reason this test uses
      // `echo` loops rather than anything that matters.
      tmux('new-session', '-d', '-s', SESSION, '-n', 'actor',
        'sh -c "while true; do echo actor-line; sleep 0.3; done"');
      tmux('new-window', '-d', '-t', `${SESSION}:`, '-n', 'handler',
        'sh -c "while true; do echo handler-line; sleep 0.3; done"');
      tmux('set-option', '-t', SESSION, 'history-limit', '20000');

      // 1. DISCOVERY — the session list IS the inventory. No Pulumi checkpoint, no stack outputs.
      const machines = await discoverLocalSessions(privateRunner, {
        host: HOST,
        listCommand: socketize(LIST_PANES_COMMAND),
      });
      const m = machines.find((x) => x.session === SESSION);
      expect(m, `discovered: ${machines.map((x) => x.session).join(',')}`).toBeDefined();
      if (!m) return;
      expect(m.mode).toBe('local');
      expect(m.machine).toBe(HOST);
      expect(m.windows).toEqual(['actor', 'handler']);
      expect(m.actor).toBe(`slice6-${process.pid}`);

      // 2. PROBE — the same `list-panes` interpretation the fleet gets, over the local transport.
      const probe = await probeMachine(privateRunner, m);
      expect(probe).toMatchObject({ reachable: 'ok', session: 'present' });
      expect(probe.windows).toEqual(['actor', 'handler']);

      // …and the same Terminal construction, so the ids come out mode-prefixed with no special case.
      const terminals = terminalsForMachine(m, probe);
      expect(terminals.map((t) => t.id)).toEqual([
        `local:${HOST}/${SESSION}/actor`,
        `local:${HOST}/${SESSION}/handler`,
      ]);
      expect(terminals.every((t) => t.mode === 'local')).toBe(true);

      // 3. SNAPSHOT — one exec for both windows, RS/US framed, split back per window.
      const snap = await privateRunner.run(
        localTarget(HOST),
        batchSnapshotCommand(m.session, m.windows),
        { timeoutMs: 15_000 }
      );
      expect(snap.code).toBe(0);
      const screens = parseBatchSnapshot(snap.stdout);
      expect([...screens.keys()].sort()).toEqual(['actor', 'handler']);
      expect(screens.get('actor')).toContain('actor-line');
      expect(screens.get('handler')).toContain('handler-line');
      // A snapshot is a SCREEN of a known size, not a log: the pane is 50-odd lines whatever the
      // process has printed since it started.
      expect((screens.get('actor') ?? '').split('\n').length).toBeLessThan(200);

      // 4. LIVE ATTACH — the real PTY, through the real opener, in local mode: `sh -c 'script …'`
      // rather than `ssh … 'script …'`, and nothing else differs.
      const remote = socketize(
        attachCommand({
          target: localTarget(HOST),
          session: m.session,
          window: 'handler',
          cols: 120,
          rows: 40,
          viewer: VIEWER,
        })
      );
      let out = '';
      let exited = false;
      let channel: PtyChannel | undefined;
      try {
        channel = realPtyRunner.open(localTarget(HOST), remote, {
          onData: (chunk) => {
            out += chunk.toString('utf8');
          },
          onSpawnError: () => undefined,
          onExit: () => {
            exited = true;
          },
        });
        // THE READ-ONLY BOUNDARY, in the mode where breaking it would type into a running Worker.
        expect(channel.noWritableInput).toBe(true);

        const deadline = Date.now() + 15_000;
        while (!out.includes('handler-line') && !exited && Date.now() < deadline) {
          await new Promise((r) => setTimeout(r, 100));
        }
        expect(out, 'bytes from the local attach').toContain('handler-line');
        // The right window, and the owner untouched — same two facts `attach.tmux.test.ts` measured
        // for the fleet, re-measured here because the transport is what changed.
        expect(out).not.toContain('actor-line');
        expect(tmuxOk('has-session', '-t', VIEWER)).toBe(true);
        expect(tmux('display-message', '-p', '-t', VIEWER, '#{window_name}')).toBe('handler');
        expect(tmux('display-message', '-p', '-t', SESSION, '#{window_name}')).toBe('actor');
      } finally {
        channel?.kill();
      }

      // 5. CLEANUP — the viewer session dies and the WORKER'S OWN session does not. On the fleet a
      // leaked `kp-*` costs a Machine that cannot be quiesced; here it would keep a dev box's session
      // alive after the operator killed it.
      const killed = await privateRunner.run(localTarget(HOST), killViewerCommand(VIEWER), {
        timeoutMs: 10_000,
      });
      expect(killed.code).toBe(0);
      expect(tmuxOk('has-session', '-t', VIEWER)).toBe(false);
      expect(tmuxOk('has-session', '-t', SESSION)).toBe(true);
      expect(tmux('list-windows', '-t', SESSION, '-F', '#{window_name}').split('\n').sort()).toEqual([
        'actor',
        'handler',
      ]);
    },
    60_000
  );

  it('reports a session that is not there as an empty inventory, not as a tile', async () => {
    // The fleet shows a Machine with no session as a tile with a converge, because the Machine exists
    // either way. A local host with no Worker has nothing to show — and offering a converge would be
    // offering to START one from a browser.
    const machines = await discoverLocalSessions(privateRunner, {
      host: HOST,
      listCommand: socketize(`tmux list-panes -a -t kontra-does-not-exist-${process.pid}`),
    });
    expect(machines).toEqual([]);
  });

  /**
   * A DEAD WORKER SAYS SO — and a live one is not called dead. Both halves, against a real tmux.
   *
   * This is the test that stops the obvious implementation from shipping. The obvious one reads
   * `pane_current_command`: a bare shell means the Worker exited. MEASURED here, it does not — the
   * pane below reports `sh`/`zsh` while its `sleep` is running, because `cli/internal/tmux/tmux.go`'s hold shell
   * keeps the pane's foreground process group. On this host every kontra session reported `zsh` with
   * its Worker up, so that implementation would have called the whole fleet dead.
   *
   * So the assertions are in this order deliberately: FIRST that a running Worker is not reported as
   * exited, THEN that a finished one is — with its status, from both of the sound sources.
   */
  it(
    'reports a finished pane as exited — and a running one under the same shell as not',
    async () => {
      // The wrapper `cli/internal/tmux/tmux.go` really uses, including the pane option it records the status in.
      const hold =
        `trap ':' INT; sleep 120; kontra_status=$?; ` +
        `[ -n "$TMUX" ] && tmux -L ${SOCKET} set-option -p -t "$TMUX_PANE" @kontra_exit ` +
        `"$kontra_status" 2>/dev/null; ` +
        `printf '\\n[exited %s] press any key to close this window\\n' "$kontra_status"; read -r _`;
      tmux('new-session', '-d', '-s', HELD, '-n', 'actor', '-x', '200', '-y', '50', hold);
      tmux('set-option', '-t', HELD, '@kontra', `actor:held-${process.pid}:0.1.0`);

      const machines = await discoverLocalSessions(privateRunner, {
        host: HOST,
        listCommand: socketize(LIST_PANES_COMMAND),
      });
      const m = machines.find((x) => x.session === HELD);
      expect(m, `discovered: ${machines.map((x) => x.session).join(',')}`).toBeDefined();
      if (!m) return;

      // 1. RUNNING. tmux reports the hold shell — and the tile must NOT read that as an exit.
      const alive = terminalsForMachine(m, await probeMachine(privateRunner, m))[0];
      expect(alive?.command, 'tmux really does report the hold shell for a running Worker').toMatch(
        /^-?(sh|bash|zsh|dash|ash|ksh|fish)$/
      );
      expect(alive?.health.process).toBe('unknown');
      expect(alive?.health.detail).toMatch(/running or finished/);
      expect(alive?.exitStatus).toBe('');
      // The pane's own geometry, which is what the status line shows — not a browser tile's guess.
      expect([alive?.paneCols, alive?.paneRows]).toEqual([200, 50]);

      // 2. FINISHED. Kill only the process INSIDE this test's own pane, by pid, from tmux itself.
      const panePid = Number(tmux('list-panes', '-t', `${HELD}:actor`, '-F', '#{pane_pid}'));
      expect(Number.isInteger(panePid) && panePid > 1).toBe(true);
      execFileSync('pkill', ['-P', String(panePid)], { stdio: 'ignore' });

      const deadline = Date.now() + 10_000;
      let dead = alive;
      while (Date.now() < deadline) {
        await new Promise((r) => setTimeout(r, 200));
        dead = terminalsForMachine(m, await probeMachine(privateRunner, m))[0];
        if (dead?.health.process === 'exited') break;
      }
      expect(dead?.health.process, 'the pane never reported its exit').toBe('exited');
      // SIGTERM is 143, and the number is the point: "it exited" and "it exited 143" send an
      // operator to different places.
      expect(dead?.exitStatus).toBe('143');
      expect(dead?.health.detail).toMatch(/143/);
      // …and the session is still PRESENT. That is the whole failure mode: every other signal is
      // fine, the tile paints, and the Worker is over.
      expect(dead?.health.session).toBe('present');
      // The screen says the same thing independently — the source that works on a Worker started
      // before the pane option existed.
      const snap = await privateRunner.run(localTarget(HOST), batchSnapshotCommand(HELD, ['actor']), {
        timeoutMs: 15_000,
      });
      expect(paneExitFromScreen(parseBatchSnapshot(snap.stdout).get('actor') ?? '')).toBe('143');
    },
    60_000
  );

  it(
    'finds a session by its @kontra tag, whatever it is called',
    async () => {
      // THE MECHANISM THAT REPLACED THE NAME PREFIX, against a real tmux rather than a fake — the
      // whole point of this file. `webcrawl-0.2.0` is what a session is called now; nothing about
      // that name says kontra made it, and the tag is what does.
      //
      // Also the case the prefix could never handle: the tag carries the VERSION, so a local
      // Worker finally resolves the Temporal queue it actually polls. A session name never had one.
      tmux('new-session', '-d', '-s', TAGGED, '-n', 'actor', 'sh -c "while true; do sleep 1; done"');
      tmux('set-option', '-t', TAGGED, '@kontra', `actor:tagged-${process.pid}:0.2.0`);

      const machines = await discoverLocalSessions(privateRunner, {
        host: HOST,
        listCommand: socketize(LIST_PANES_COMMAND),
      });
      const m = machines.find((x) => x.session === TAGGED);
      expect(m, `discovered: ${machines.map((x) => x.session).join(',')}`).toBeDefined();
      if (!m) return;
      expect(m.actor).toBe(`tagged-${process.pid}`);
      expect(m.version).toBe('0.2.0');

      // …and an UNTAGGED session with no kontra-ish name is still somebody else's business.
      tmux('new-session', '-d', '-s', MINE, '-n', 'vim', 'sh -c "while true; do sleep 1; done"');
      const after = await discoverLocalSessions(privateRunner, {
        host: HOST,
        listCommand: socketize(LIST_PANES_COMMAND),
      });
      expect(after.map((x) => x.session)).not.toContain(MINE);
    },
    20_000
  );

  it(
    'refuses a tag whose kind it does not know, and admits watch: — ADR 0043, against a real tmux',
    async () => {
      // THE CASE 0043 EXISTS FOR, exercised through tmux rather than through a fake PaneRow.
      // `isKontraSession` used to be `(row.kontra ?? '') !== ''`, so ONE `set-option` with any value
      // at all put a session on the wall — and `SAFE.command`, the grammar that admits only a
      // journal, runs on the converge path and never here. An agent's session is the case that
      // matters: a stalled viewer segfaults the tmux server and destroys every session on the
      // socket, which journald survives and an in-flight conversation does not.
      tmux('new-session', '-d', '-s', UNKNOWN_KIND, '-n', 'agent', 'sh -c "while true; do sleep 1; done"');
      tmux('set-option', '-t', UNKNOWN_KIND, '@kontra', 'agent:claude');

      const refused = await discoverLocalSessions(privateRunner, {
        host: HOST,
        listCommand: socketize(LIST_PANES_COMMAND),
      });
      expect(
        refused.map((x) => x.session),
        'a tag of an unknown kind must not reach the wall'
      ).not.toContain(UNKNOWN_KIND);

      // AND THE OPT-IN WORKS, or the rule would be a wall with no door. Re-tagging the SAME session
      // is what an operator does after reading the refusal, so this also proves the refusal is about
      // the tag and not about anything else on the session.
      tmux('set-option', '-t', UNKNOWN_KIND, '@kontra', 'watch:agent');
      const admitted = await discoverLocalSessions(privateRunner, {
        host: HOST,
        listCommand: socketize(LIST_PANES_COMMAND),
      });
      expect(
        admitted.map((x) => x.session),
        'watch: is the explicit opt-in and must be admitted'
      ).toContain(UNKNOWN_KIND);

      // A `watch:` session carries no Actor. Inventing one would put an operator's own terminal on
      // the Actors surface and give it a Temporal queue nobody polls.
      const w = admitted.find((x) => x.session === UNKNOWN_KIND);
      expect(w?.actor).toBe('');
      expect(w?.version).toBe('');
    },
    20_000
  );
});
