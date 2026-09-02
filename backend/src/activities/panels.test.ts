import { afterEach, describe, expect, it } from 'vitest';
import {
  captureTmuxPanes,
  convergeTmuxSession,
  killTmuxSession,
  listTmuxPanes,
  resetSshRunner,
  useSshRunner,
} from './panels';
import { DEFAULT_WINDOWS } from '../panels/converge';
import { sshArgs, type SshResult, type SshRunner } from '../panels/ssh';

/**
 * No Machine in this environment can be SSHed to, so the runner is faked — the same seam
 * `cli/workers.go` uses to prove its Temporal join without dialing. What is actually being tested
 * here is the command that WOULD be sent, which is the part that has to be right.
 */
function fake(reply: Partial<SshResult> & { assert?: (remote: string) => void }): {
  runner: SshRunner;
  calls: Array<{ machine: string; host: string; remote: string }>;
} {
  const calls: Array<{ machine: string; host: string; remote: string }> = [];
  const runner: SshRunner = {
    async run(target, remote) {
      calls.push({ machine: target.machine, host: target.host, remote });
      reply.assert?.(remote);
      return { code: 0, stdout: '', stderr: '', timedOut: false, ...reply };
    },
  };
  return { runner, calls };
}

afterEach(() => resetSshRunner());

const TARGET = { machine: 'kf-crawl-01', host: '10.124.0.9' };

describe('convergeTmuxSession', () => {
  it('sends the converge in ONE round trip and reports what the Machine has', async () => {
    const { runner, calls } = fake({ stdout: 'KONTRA_TMUX created=1 windows=actor,handler,\n' });
    useSshRunner(runner);

    const res = await convergeTmuxSession({
      ...TARGET,
      session: 'kontra-webcrawl',
      windows: [...DEFAULT_WINDOWS],
    });

    expect(calls).toHaveLength(1);
    expect(calls[0]?.remote).toContain("tmux has-session -t 'kontra-webcrawl'");
    expect(res).toEqual({
      machine: 'kf-crawl-01',
      session: 'kontra-webcrawl',
      windows: ['actor', 'handler'],
      created: true,
    });
  });

  it('reports created=false for a session that was already there', async () => {
    useSshRunner(fake({ stdout: 'KONTRA_TMUX created=0 windows=actor,handler,' }).runner);
    const res = await convergeTmuxSession({
      ...TARGET,
      session: 'kontra-webcrawl',
      windows: [...DEFAULT_WINDOWS],
    });
    expect(res.created).toBe(false);
  });

  it('fails loudly, naming the Machine — a converge is not best-effort any more', async () => {
    useSshRunner(fake({ code: 100, stderr: 'E: Unable to locate package tmux' }).runner);
    await expect(
      convergeTmuxSession({ ...TARGET, session: 'kontra-webcrawl', windows: [...DEFAULT_WINDOWS] })
    ).rejects.toThrow(/kf-crawl-01[^]*Unable to locate package tmux/);
  });

  it('refuses a hostile machine, host, session or window before any ssh runs', async () => {
    const { runner, calls } = fake({});
    useSshRunner(runner);
    const base = { session: 'kontra-webcrawl', windows: [...DEFAULT_WINDOWS] };
    await expect(convergeTmuxSession({ ...base, machine: 'kf-crawl-01;id', host: 'h' })).rejects.toThrow(
      /not safe/
    );
    await expect(
      convergeTmuxSession({ ...base, machine: 'kf-crawl-01', host: '10.0.0.1 ; id' })
    ).rejects.toThrow(/not safe/);
    await expect(
      convergeTmuxSession({ ...TARGET, session: 'a`id`', windows: [...DEFAULT_WINDOWS] })
    ).rejects.toThrow(/not safe/);
    await expect(
      convergeTmuxSession({ ...TARGET, session: 'ok', windows: [{ name: 'w', command: '$(id)' }] })
    ).rejects.toThrow(/not safe/);
    expect(calls).toEqual([]);
  });
});

describe('the read probes', () => {
  it('lists every pane on the Machine in one exec', async () => {
    const { runner, calls } = fake({
      stdout: 'webcrawl-0_2_0 actor %0 200x50 0 c:journalctl x: actor:webcrawl:0.2.0\n',
    });
    useSshRunner(runner);
    const panes = await listTmuxPanes(TARGET);
    // `#{@kontra}` LAST. It is how the Monitor tells a kontra session from an operator's, replacing
    // the `kontra-` name prefix — and it is last because it is the one field kontra does not
    // control the shape of, so a value with a space in it can only corrupt itself.
    //
    // The two middle fields carry a LITERAL PREFIX (`c:`, `x:`) because `@kontra_exit` is empty on
    // every pane whose command has not exited — without it the line renders two adjacent spaces, the
    // whitespace split collapses them, and the tag arrives one field to the left, which takes every
    // tagged session off the wall.
    // THE HOST MARKER RUNS FIRST AND THE TMUX CALL LAST, and the order is load-bearing in both
    // directions. First, because `parseProbeHost` needs the name of the box that answered — the
    // streamer dials a recycled VPC address and would otherwise render another Fleet's live screen
    // under a destroyed Machine's name. Last, because `looksLikeNoTmux` reads this command's EXIT
    // CODE and a shell reports the LAST command's status, so anything appended after tmux would
    // make a Machine with no tmux indistinguishable from one that answered.
    //
    // `|| true` on the hostname half for the same reason: a scratch-based worker image has no
    // `hostname` binary, and a box that prints no marker is UNATTRIBUTED, not mismatched.
    //
    // Written out in full rather than compared against `LIST_PANES_COMMAND`, which would assert
    // that the constant equals itself. This is the independent copy; if it disagrees, one of the
    // two moved and the disagreement is the point.
    expect(calls[0]?.remote).toBe(
      "hostname 2>/dev/null | sed 's/^/KONTRA_HOST /' || true; " +
        "tmux list-panes -a -F '#{session_name} #{window_name} #{pane_id} #{pane_width}x#{pane_height} " +
        "#{pane_dead} c:#{pane_current_command} x:#{@kontra_exit} #{@kontra}'"
    );
    expect(panes).toEqual([
      {
        session: 'webcrawl-0_2_0',
        window: 'actor',
        paneId: '%0',
        cols: 200,
        rows: 50,
        dead: false,
        // What is actually running in the pane — a journal follower on a fleet Machine, which is a
        // real reading because it is not a shell.
        command: 'journalctl',
        exitStatus: '',
        kontra: 'actor:webcrawl:0.2.0',
      },
    ]);
  });

  it('captures several windows in one exec and returns them per window', async () => {
    const { runner, calls } = fake({ stdout: '\x1eactor\x1fA\n\x1ehandler\x1fB\n' });
    useSshRunner(runner);
    const screens = await captureTmuxPanes({
      ...TARGET,
      session: 'kontra-webcrawl',
      windows: ['actor', 'handler'],
    });
    expect(calls).toHaveLength(1);
    expect(screens).toEqual({ actor: 'A\n', handler: 'B\n' });
  });

  it('kills a session idempotently', async () => {
    const { runner, calls } = fake({});
    useSshRunner(runner);
    await killTmuxSession({ ...TARGET, session: 'kontra-webcrawl' });
    expect(calls[0]?.remote).toContain("kill-session -t 'kontra-webcrawl'");
    expect(calls[0]?.remote).toContain('|| true');
  });
});

describe('the ssh command line', () => {
  it('carries the flags ADR 0020 pins, multiplexed per Machine', () => {
    const args = sshArgs(TARGET, 'tmux list-panes -a', { controlPath: '/run/kontra-panels/cm-x' });
    const joined = args.join(' ');
    for (const flag of [
      'BatchMode=yes',
      'StrictHostKeyChecking=accept-new',
      'ConnectTimeout=10',
      'ServerAliveInterval=15',
      'ServerAliveCountMax=3',
      'ControlMaster=auto',
      'ControlPersist=60',
    ]) {
      expect(joined, flag).toContain(flag);
    }
    expect(args).toContain('root@10.124.0.9');
    // The remote command is ONE argv element. No local shell is ever involved, which is what makes
    // "there is no channel to inject into" true rather than aspirational.
    expect(args[args.length - 1]).toBe('tmux list-panes -a');
  });

  it('validates the Machine name before it becomes a ControlPath', () => {
    expect(() => sshArgs({ machine: '../../etc/passwd', host: 'h' }, 'x')).toThrow(/not safe/);
  });
});
