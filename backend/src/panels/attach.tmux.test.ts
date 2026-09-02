import { execFileSync, spawn } from 'node:child_process';
import { afterAll, describe, expect, it } from 'vitest';
import { attachCommand, killViewerCommand, newViewerSession } from './attach';

/**
 * The attach command, against a REAL tmux.
 *
 * Everything above this file is proven with fakes, which is the only way to prove it here — no fleet
 * exists in this environment and no Machine can be SSHed to. But a fake cannot tell you whether the
 * command itself is right, and this slice's command turned out NOT to be: the shape CONTRACT.md
 * pinned selected the window in the owner session, which moved the operator's current window and
 * left the tile streaming a different window's bytes. That is the kind of thing only a real tmux
 * says out loud, so this file exists to say it.
 *
 * WHAT IT PROVES: the PTY wrapper runs, a closed input descriptor does not kill `script` (the
 * measurement the whole read-only design rests on), the grouped session is created, `stty` sizes the
 * client, the selected window is the one asked for, the OWNER session is left alone, the viewer
 * session outlives its client (so cleanup is mandatory), and the cleanup kills the viewer without
 * touching the owner's windows or the processes in them.
 *
 * WHAT IT DOES NOT PROVE: anything about SSH. The remote string is handed to a local `sh` exactly as
 * `sshd` would hand it to the Machine's shell, and that is where the fidelity stops. `ControlMaster`
 * reuse, reconnect on reboot, and per-tile RSS on an `s-1vcpu-2gb` box remain unmeasured.
 *
 * THE SOCKET IS PRIVATE, and that is not a style rule. Establishing ADR 0020's findings on the
 * default socket segfaulted the operator's own tmux server and destroyed a session that had been
 * running for a day, unrecoverably. Everything below is on `-L kontratest`, every session it creates
 * is named after this process, and nothing else is ever killed.
 */

const SOCKET = 'kontratest';
const OWNER = `kontra-attachtest-${process.pid}`;
const VIEWER = newViewerSession();

function tmux(...args: string[]): string {
  // stderr is captured rather than inherited: `has-session` on a session this test already cleaned
  // up is a normal negative answer, not something to print in the suite's output.
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

const runnable = have('tmux') && have('script');

async function until(what: string, check: () => boolean, ms = 15_000): Promise<void> {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (check()) return;
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`timed out waiting for ${what}`);
}

afterAll(() => {
  // Only what this test created, by exact name. Never `kill-server`, never a prefix sweep: another
  // viewer's session on the same socket is not ours to end.
  tmuxOk('kill-session', '-t', VIEWER);
  tmuxOk('kill-session', '-t', OWNER);
});

describe.skipIf(!runnable)('the live attach against a real tmux (private socket)', () => {
  it(
    'attaches read-only to the right window, leaves the owner alone, and cleans up after itself',
    async () => {
      // The owner session, shaped the way the converge shapes one: two journal-ish windows, a fixed
      // geometry and `window-size manual`, so an attach cannot reflow anyone.
      tmux(
        'new-session',
        '-d',
        '-s',
        OWNER,
        '-n',
        'actor',
        'sh -c "while true; do echo actor-line; sleep 0.3; done"'
      );
      tmux(
        'new-window',
        '-d',
        '-t',
        `${OWNER}:`,
        '-n',
        'handler',
        'sh -c "while true; do echo handler-line; sleep 0.3; done"'
      );
      tmux('set-option', '-t', OWNER, 'history-limit', '20000');
      tmux('set-option', '-t', OWNER, 'window-size', 'manual');
      for (const w of ['actor', 'handler']) {
        tmux('resize-window', '-t', `${OWNER}:${w}`, '-x', '200', '-y', '50');
      }
      expect(tmux('display-message', '-p', '-t', OWNER, '#{window_name}')).toBe('actor');

      // The command under test, verbatim — except for `-L`, which is this test's own: a fleet
      // Machine has one tmux server on the default socket, and this must never touch ours.
      const remote = attachCommand({
        target: { machine: 'kf-crawl-01', host: '10.124.0.9' },
        session: OWNER,
        window: 'handler',
        cols: 200,
        rows: 50,
        viewer: VIEWER,
      }).replace('tmux ', `tmux -L ${SOCKET} `);

      // `sh -c` is what sshd does with a remote string, so this is the same parse the Machine does.
      const child = spawn('sh', ['-c', remote], { stdio: ['ignore', 'pipe', 'pipe'] });
      let out = '';
      let err = '';
      child.stdout.setEncoding('utf8');
      child.stderr.setEncoding('utf8');
      child.stdout.on('data', (c: string) => {
        out += c;
      });
      child.stderr.on('data', (c: string) => {
        err += c;
      });
      child.stdout.on('error', () => undefined);
      child.stderr.on('error', () => undefined);
      child.on('error', () => undefined);

      try {
        // THE READ-ONLY BOUNDARY, on a real child: there is no writable descriptor at all. ADR 0020's
        // finding (3) measured `-r` failing to stop `run-shell`, so this — not `-r` — is what makes a
        // Terminal read-only.
        expect(child.stdin).toBeNull();

        await until(`bytes from ${OWNER}:handler (stderr: ${err})`, () => out.includes('handler-line'));

        // …and a closed input did not make `script` exit, which is the measurement the design rests
        // on: if it had, a read-only PTY attach would be impossible and this slice would need a
        // channel it must not have.
        expect(child.exitCode).toBeNull();

        // The right window. Getting this wrong is silent — the tile shows a real, live, WRONG pane.
        expect(out).not.toContain('actor-line');

        const viewer = VIEWER;
        expect(tmuxOk('has-session', '-t', viewer)).toBe(true);
        expect(tmux('display-message', '-p', '-t', viewer, '#{window_name}')).toBe('handler');

        // The owner is untouched: still on `actor`. This is the assertion CONTRACT.md's original
        // shape failed — it dragged the owner to `handler`.
        expect(tmux('display-message', '-p', '-t', OWNER, '#{window_name}')).toBe('actor');

        // `stty` inside the PTY is what carries the tile's measured geometry to the far end.
        await until('the client to report its size', () =>
          tmuxOk('list-clients', '-t', viewer, '-F', '#{client_width}x#{client_height}')
        );
        expect(tmux('list-clients', '-t', viewer, '-F', '#{client_width}x#{client_height}')).toContain(
          '200x50'
        );

        // The client dies — a closed tab, a killed streamer, a dropped connection.
        child.kill('SIGKILL');
        await until('the attach process to go away', () => child.exitCode !== null || child.signalCode !== null);

        // MEASURED, and the reason `stop()` exists: the viewer session OUTLIVES its client. Nothing
        // reaps it, and while it exists the owner's windows survive a `kill-session` on the owner —
        // so a leaked one is a Machine that cannot be quiesced.
        expect(tmuxOk('has-session', '-t', viewer)).toBe(true);

        // The cleanup, exactly as `Attachment.stop()` issues it.
        execFileSync('sh', ['-c', killViewerCommand(viewer).replace('tmux ', `tmux -L ${SOCKET} `)], {
          stdio: 'ignore',
        });
        expect(tmuxOk('has-session', '-t', viewer)).toBe(false);

        // …and killing a viewer leaves the owner, its windows, and the processes in them alone.
        expect(tmuxOk('has-session', '-t', OWNER)).toBe(true);
        expect(tmux('list-windows', '-t', OWNER, '-F', '#{window_name}').split('\n').sort()).toEqual([
          'actor',
          'handler',
        ]);
        const before = tmux('capture-pane', '-p', '-t', `${OWNER}:handler`);
        // The owner's shell prints every 300 ms, so 700 ms of wall clock "should" cover two lines —
        // and that is exactly the reasoning that makes a test fail on a busy box rather than on a
        // broken one. Wait for the pane to actually change, with a ceiling.
        await until(
          'the owner pane to keep producing',
          () => tmux('capture-pane', '-p', '-t', `${OWNER}:handler`) !== before
        );
        const after = tmux('capture-pane', '-p', '-t', `${OWNER}:handler`);
        expect(after).not.toBe(before); // the pane is still producing
      } finally {
        if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
      }
    },
    60_000
  );
});
