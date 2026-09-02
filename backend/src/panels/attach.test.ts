import { readFileSync } from 'node:fs';
import * as path from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  attachCommand,
  assertViewerSession,
  Attachment,
  BACKLOG_TIMEOUT_MS,
  backlogCommand,
  clampDimension,
  MIN_COLS,
  killViewerCommand,
  MAX_DIMENSION,
  newViewerSession,
  realPtyRunner,
  VIEWER_PREFIX,
  viewerSession,
  type AttachDeps,
  type AttachSink,
  type AttachTarget,
  type PtyEvents,
  type PtyRunner,
} from './attach';
import { CLEAR_HOME } from './tmux';
import type { SshResult, SshRunner, SshTarget } from './ssh';

/**
 * The live attach, against faked SSH and PTY seams.
 *
 * No fleet exists in this environment, so the fakes are the Machine — the same stance
 * `server.test.ts` and `probe.test.ts` take, and the same one `cli/workers.go` takes with its
 * `queueDescriber`. What a fake cannot prove is that the COMMAND works; that is
 * `attach.tmux.test.ts`, which runs the real shape against a real tmux on a private socket.
 */

/**
 * WAIT ON THE CONDITION, NOT ON THE CLOCK — `server.test.ts` and `attach.tmux.test.ts` each already
 * grew one of these; this is the third, for the two places in this file that were hand-rolling the
 * loop or, worse, sleeping a fixed number of milliseconds and hoping.
 *
 * THE CEILING IS A GIVING-UP POINT, NOT A DELAY, so a slow machine is slow rather than wrong. Note
 * what this is NOT for: several sleeps in this file exist to prove that something did NOT happen in
 * a window (`a second stop() returned before the kill landed`, `bytes after the exit`). There is no
 * condition to poll for an absence, and converting one would turn it into a test that always
 * passes. Those stay as they are, deliberately.
 */
async function until(ready: () => boolean, what: string, ms = 20_000): Promise<void> {
  const deadline = Date.now() + ms;
  while (!ready()) {
    if (Date.now() >= deadline) throw new Error(`waited ${ms} ms for ${what} and it never happened`);
    await new Promise((res) => setTimeout(res, 10));
  }
}

const TARGET: SshTarget = { machine: 'kf-crawl-01', host: '10.124.0.9' };

const SPEC: AttachTarget = {
  target: TARGET,
  session: 'kontra-webcrawl',
  window: 'actor',
  cols: 200,
  rows: 50,
};

class FakeSsh implements SshRunner {
  readonly calls: Array<{ remote: string; timeoutMs?: number }> = [];

  constructor(private readonly reply: (remote: string) => Partial<SshResult> = () => ({})) {}

  async run(_target: SshTarget, remote: string, opts?: { timeoutMs?: number }): Promise<SshResult> {
    const call: { remote: string; timeoutMs?: number } = { remote };
    if (opts?.timeoutMs !== undefined) call.timeoutMs = opts.timeoutMs;
    this.calls.push(call);
    return { code: 0, stdout: '', stderr: '', timedOut: false, ...this.reply(remote) };
  }

  /** Every remote command that looks like a kill of a per-viewer session. */
  kills(): string[] {
    return this.calls.filter((c) => c.remote.includes('kill-session')).map((c) => c.remote);
  }
}

class FakePty implements PtyRunner {
  readonly opened: Array<{ target: SshTarget; remote: string }> = [];
  killed = 0;
  private events: PtyEvents | undefined;

  open(target: SshTarget, remote: string, events: PtyEvents) {
    this.opened.push({ target, remote });
    this.events = events;
    return {
      noWritableInput: true,
      kill: (): void => {
        this.killed += 1;
      },
    };
  }

  data(s: string): void {
    this.events?.onData(Buffer.from(s, 'utf8'));
  }

  exit(code: number | null = 0, stderr = ''): void {
    this.events?.onExit(code, null, stderr);
  }

  spawnError(message: string): void {
    this.events?.onSpawnError(new Error(message));
  }
}

class Recorder implements AttachSink {
  readonly frames: Buffer[] = [];
  readonly transitions: string[] = [];
  readonly messages: string[] = [];
  elidedBytes = 0;

  bytes(payload: Buffer): void {
    this.frames.push(payload);
  }
  elided(bytes: number): void {
    this.elidedBytes += bytes;
  }
  live(): void {
    this.transitions.push('live');
  }
  failed(message: string): void {
    this.transitions.push('failed');
    this.messages.push(message);
  }
  ended(message: string): void {
    this.transitions.push('ended');
    this.messages.push(message);
  }

  text(): string {
    return Buffer.concat(this.frames).toString('utf8');
  }
}

interface Rig {
  ssh: FakeSsh;
  pty: FakePty;
  sink: Recorder;
  attach: Attachment;
  clock: { now: number };
}

function rig(over: { ssh?: FakeSsh; spec?: Partial<AttachTarget>; viewer?: string } = {}): Rig {
  const ssh = over.ssh ?? new FakeSsh(() => ({ stdout: 'the backlog\n' }));
  const pty = new FakePty();
  const sink = new Recorder();
  const clock = { now: 1_700_000_000_000 };
  const deps: AttachDeps = { ssh, pty, now: () => clock.now };
  const attach = new Attachment(deps, { ...SPEC, ...over.spec }, sink, over.viewer);
  return { ssh, pty, sink, attach, clock };
}

describe('the remote command shape', () => {
  it('is a PTY around a grouped session, sized, read-only, and exec-ed', () => {
    const cmd = attachCommand({ ...SPEC, viewer: 'kp-deadbeef' });
    expect(cmd).toBe(
      // `export TERM=` is not decoration: without it tmux answers `open terminal failed: terminal
      // does not support clear`. Nothing upstream supplies one — the streamer is a container child,
      // and for fleet this crosses `ssh` without `-t`. CI caught it; a developer box cannot.
      "script -q -c 'export TERM=xterm; stty rows 50 cols 200; " +
        'exec tmux new-session -A -d -s "kp-deadbeef" -t "kontra-webcrawl" ' +
        '\\; set-option -t "kp-deadbeef" status off ' +
        '\\; select-window -t "kp-deadbeef:actor" ' +
        "\\; attach-session -r -t \"kp-deadbeef\"' /dev/null"
    );
  });

  /**
   * ONE STATUS LINE PER TILE, AND IT IS THE BROWSER'S.
   *
   * MEASURED on this box, both ways: an attached client draws tmux's own green bar into the byte
   * stream (`^[[30m^[[42m[kp-…:actor* 2:handler# … "main-droplet" 17:41`), and `capture-pane` never
   * does — the status line belongs to the CLIENT, not to the pane. So the wall showed no bar on
   * snapshot tiles and tmux's on live ones, and the tile's own bar would have sat under a second,
   * differently-worded one on whichever tiles happened to be live.
   *
   * On the VIEWER session only. Verified on tmux 3.3a that this leaves the owner session's `status`
   * unset, so an operator attached on the box keeps their bar.
   */
  it('turns tmux’s own status line off for the viewer, so a live tile has ONE bar', () => {
    const cmd = attachCommand({ ...SPEC, viewer: 'kp-deadbeef' });
    expect(cmd).toContain('set-option -t "kp-deadbeef" status off');
    expect(cmd).not.toContain('set-option -t "kontra-webcrawl"');
  });

  /**
   * The correction this slice had to make, and the reason it is a test.
   *
   * CONTRACT.md pinned `select-window -t '<session>:<window>'`. Measured on tmux 3.3a
   * (`tmux -L kontratest`): that target moved the OWNER session's current window — the operator
   * attached on the box gets yanked to another window — and left the viewer session on whatever
   * window the group started on, so the tile streamed the wrong window. Both halves of that are
   * pinned in `attach.tmux.test.ts` against a real tmux.
   */
  it('selects the window in the VIEWER session, never in the owner session', () => {
    const cmd = attachCommand({ ...SPEC, viewer: 'kp-deadbeef' });
    expect(cmd).toContain('select-window -t "kp-deadbeef:actor"');
    expect(cmd).not.toContain('select-window -t "kontra-webcrawl:actor"');
  });

  it('never writes to the channel it opens: nothing in the command sends input', () => {
    const cmd = attachCommand({ ...SPEC, viewer: 'kp-deadbeef' });
    // ADR 0020 finding (3): `-r` executed both of these as root through a read-only client. The
    // boundary is that the command cannot contain them and the child has no writable input.
    for (const forbidden of ['send-keys', 'run-shell', 'paste-buffer', 'load-buffer']) {
      expect(cmd).not.toContain(forbidden);
    }
    expect(cmd).toContain('attach-session -r');
  });

  it('refuses every hostile part rather than quoting it', () => {
    expect(() => attachCommand({ ...SPEC, window: '$(id)', viewer: 'kp-1' })).toThrow(/not safe/);
    expect(() => attachCommand({ ...SPEC, session: "a'b", viewer: 'kp-1' })).toThrow(/not safe/);
    expect(() => attachCommand({ ...SPEC, window: 'a b', viewer: 'kp-1' })).toThrow(/not safe/);
    expect(() => attachCommand({ ...SPEC, viewer: 'kp-$(id)' })).toThrow(/not safe/);
    // The two numbers reach `stty`, so they are checked like every other interpolated value.
    expect(() => attachCommand({ ...SPEC, cols: 0, viewer: 'kp-1' })).toThrow(/not a terminal dimension/);
    expect(() => attachCommand({ ...SPEC, rows: 1.5, viewer: 'kp-1' })).toThrow(/not a terminal dimension/);
    expect(() => attachCommand({ ...SPEC, rows: 1e9, viewer: 'kp-1' })).toThrow(/not a terminal dimension/);
    expect(() =>
      attachCommand({ ...SPEC, cols: Number.NaN, viewer: 'kp-1' })
    ).toThrow(/not a terminal dimension/);
  });

  it('quotes so that `\\;` reaches tmux and not the shell', () => {
    const cmd = attachCommand({ ...SPEC, viewer: 'kp-1' });
    const inner = cmd.slice(cmd.indexOf("'") + 1, cmd.lastIndexOf("'"));
    // The inner command is single-quoted by the caller, so it must contain no single quote of its
    // own — that is why tmux targets are double-quoted, which is only safe because every value
    // above passed the whitelist.
    expect(inner).not.toContain("'");
    // Four tmux commands, three separators: new-session, set-option, select-window, attach-session.
    expect(inner.split('\\;')).toHaveLength(4);
  });

  it('seeds from capture-pane with real backlog, not the snapshot depth', () => {
    expect(backlogCommand('kontra-webcrawl', 'actor')).toBe(
      "tmux capture-pane -p -e -S -2000 -t 'kontra-webcrawl:actor'"
    );
  });

  it('bounds a browser-reported size instead of refusing it', () => {
    expect(clampDimension('cols', 120, 200)).toBe(120);
    expect(clampDimension('cols', 0, 200)).toBe(MIN_COLS);
    expect(clampDimension('cols', 1e9, 200)).toBe(MAX_DIMENSION);
    expect(clampDimension('cols', undefined, 200)).toBe(200);
    expect(clampDimension('cols', '80', 200)).toBe(200);
    expect(clampDimension('cols', Number.NaN, 200)).toBe(200);
    expect(clampDimension('cols', 80.7, 200)).toBe(80);
  });
});

describe('the per-viewer session name', () => {
  it('is unique per viewer, so two tabs cannot kill each other', () => {
    const names = new Set(Array.from({ length: 200 }, () => newViewerSession()));
    expect(names.size).toBe(200);
    for (const n of names) expect(n.startsWith(VIEWER_PREFIX)).toBe(true);
  });

  it('CANNOT be an owner session — the kill path refuses anything but kp-*', () => {
    // The catastrophic bug in this file would be killing the session every tile on the Machine
    // reads from, and which an operator may be attached to.
    expect(() => killViewerCommand('kontra-webcrawl')).toThrow(/not a per-viewer session/);
    expect(() => assertViewerSession('kp')).toThrow(/not a per-viewer session/);
    expect(() => assertViewerSession(VIEWER_PREFIX)).toThrow(/not a per-viewer session/);
    expect(() => viewerSession('')).toThrow(/not a per-viewer session/);
    expect(() => killViewerCommand('kp-x; rm -rf /')).toThrow(/not safe/);
    expect(killViewerCommand('kp-deadbeef')).toBe(
      "tmux kill-session -t 'kp-deadbeef' 2>/dev/null || true"
    );
  });
});

describe('going live', () => {
  it('seeds with a repaint, then appends live bytes', async () => {
    const r = rig();
    await r.attach.start();

    // The seed is a full screen, so it carries the clear-home prefix — otherwise the tile appends a
    // second screen under the snapshot it already had.
    expect(r.sink.frames[0]?.subarray(0, CLEAR_HOME.length).toString()).toBe(CLEAR_HOME);
    expect(r.sink.frames[0]?.toString()).toContain('the backlog');
    expect(r.sink.transitions).toEqual(['live']);
    expect(r.attach.mode).toBe('live');

    r.pty.data('a line from the journal\n');
    const live = r.sink.frames[r.sink.frames.length - 1];
    // A live frame is incremental: no clear, or the tile would flash a blank screen per chunk.
    expect(live?.toString()).toBe('a line from the journal\n');
    expect(live?.toString()).not.toContain(CLEAR_HOME);
  });

  it('reads the backlog BEFORE it attaches, over the shared connection', async () => {
    const r = rig();
    await r.attach.start();
    expect(r.ssh.calls[0]?.remote).toContain('capture-pane');
    expect(r.ssh.calls[0]?.timeoutMs).toBe(BACKLOG_TIMEOUT_MS);
    expect(r.pty.opened).toHaveLength(1);
    expect(r.pty.opened[0]?.target).toEqual(TARGET);
    expect(r.pty.opened[0]?.remote).toContain(`-s "${r.attach.viewer}"`);
  });

  it('replays what the tile was shown, as a repaint', async () => {
    const r = rig();
    await r.attach.start();
    r.pty.data('one\n');
    r.pty.data('two\n');
    const replay = r.attach.replay().toString();
    expect(replay.startsWith(CLEAR_HOME)).toBe(true);
    expect(replay).toContain('the backlog');
    expect(replay).toContain('one\ntwo\n');
  });
});

describe('a focus that cannot go live', () => {
  it('degrades to an error with a sentence when the Machine does not answer — it never hangs', async () => {
    const ssh = new FakeSsh(() => ({ code: 124, timedOut: true }));
    const r = rig({ ssh });
    await r.attach.start();
    expect(r.sink.transitions).toEqual(['failed']);
    expect(r.sink.messages[0]).toContain('kf-crawl-01');
    expect(r.sink.messages[0]).toContain('unreachable');
    expect(r.attach.mode).toBe('error');
    // No PTY was opened toward a Machine we could not even read from, so no grouped session can
    // have been created and there is nothing to leak.
    expect(r.pty.opened).toHaveLength(0);
    expect(r.ssh.kills()).toEqual([]);
  });

  it('says which window it could not read when capture-pane fails', async () => {
    const ssh = new FakeSsh(() => ({ code: 1, stderr: "can't find window: actor\nnoise\n" }));
    const r = rig({ ssh });
    await r.attach.start();
    expect(r.sink.transitions).toEqual(['failed']);
    expect(r.sink.messages[0]).toContain('kontra-webcrawl:actor');
    expect(r.sink.messages[0]).toContain("can't find window: actor");
    expect(r.sink.messages[0]).toContain('converge');
    expect(r.pty.opened).toHaveLength(0);
  });

  it('reports a validation failure instead of letting it reach a shell', async () => {
    const r = rig({ spec: { window: '$(touch /tmp/pwned)' } });
    await r.attach.start();
    expect(r.sink.transitions).toEqual(['failed']);
    expect(r.sink.messages[0]).toContain('not safe to interpolate');
    expect(r.pty.opened).toHaveLength(0);
    // It threw inside `start()`, not out of it: an unhandled rejection in this process is what
    // ADR 0020's finding (6) measured failing an unrelated `pulumi up`.
  });

  it('reports an ssh that cannot be spawned at all', async () => {
    const r = rig();
    await r.attach.start();
    r.pty.spawnError('spawn ssh ENOENT');
    expect(r.sink.transitions).toEqual(['live', 'failed']);
    expect(r.sink.messages[0]).toContain('ENOENT');
  });
});

describe('the grouped session is never left behind', () => {
  it('kills it when the attach exits, exactly once, by its own name', async () => {
    const r = rig();
    await r.attach.start();
    const viewer = r.attach.viewer;
    r.pty.exit(0, '');
    // The kill is fired from an event handler, so let its microtasks run.
    await new Promise((res) => setTimeout(res, 10));
    expect(r.sink.transitions).toEqual(['live', 'ended']);
    expect(r.sink.messages[0]).toContain('back to snapshots');
    expect(r.ssh.kills()).toEqual([`tmux kill-session -t '${viewer}' 2>/dev/null || true`]);
  });

  it('kills it on stop(), and kills the PTY with it', async () => {
    const r = rig();
    await r.attach.start();
    await r.attach.stop();
    expect(r.pty.killed).toBe(1);
    expect(r.ssh.kills()).toHaveLength(1);
    expect(r.ssh.kills()[0]).toContain(r.attach.viewer);
    // Idempotent: blur, then a disconnect, then shutdown all call this.
    await r.attach.stop();
    await r.attach.stop();
    expect(r.ssh.kills()).toHaveLength(1);
  });

  it('makes every concurrent stop() await the SAME kill', async () => {
    // The sequence this is for: a blur, then a SIGTERM a millisecond later. If the second caller
    // returned early, the process would exit while the kill the blur started was still in flight —
    // and a restarted streamer cannot find that session again, because it does not know the nonce.
    let finishKill = (): void => undefined;
    const ssh: SshRunner = {
      async run(_t, remote) {
        if (remote.includes('kill-session')) {
          await new Promise<void>((res) => {
            finishKill = res;
          });
        }
        return { code: 0, stdout: 'backlog', stderr: '', timedOut: false };
      },
    };
    const attach = new Attachment({ ssh, pty: new FakePty() }, SPEC, new Recorder());
    await attach.start();

    let both = false;
    const waited = Promise.all([attach.stop(), attach.stop()]).then(() => {
      both = true;
    });
    await new Promise((res) => setTimeout(res, 20));
    expect(both, 'a second stop() returned before the kill landed').toBe(false);
    finishKill();
    await waited;
    expect(both).toBe(true);
  });

  it('never kills anything when no attach was launched', async () => {
    const ssh = new FakeSsh(() => ({ code: 255, stderr: 'Permission denied (publickey).' }));
    const r = rig({ ssh });
    await r.attach.start();
    await r.attach.stop();
    expect(r.ssh.kills()).toEqual([]);
  });

  it('does not open a PTY when it was stopped while reading the backlog', async () => {
    let release = (): void => undefined;
    const gate = new Promise<void>((res) => {
      release = res;
    });
    const ssh: SshRunner = {
      async run(_t, remote) {
        if (remote.includes('capture-pane')) await gate;
        return { code: 0, stdout: 'x', stderr: '', timedOut: false };
      },
    };
    const pty = new FakePty();
    const sink = new Recorder();
    const attach = new Attachment({ ssh, pty }, SPEC, sink);
    const started = attach.start();
    // The tab closed while the seed was in flight — a slow Machine plus an impatient operator.
    await attach.stop();
    release();
    await started;
    expect(pty.opened).toHaveLength(0);
    expect(sink.transitions).toEqual([]);
  });

  it('reports a leak rather than hiding it when the kill itself fails', async () => {
    const lines: Array<{ line: string; extra?: Record<string, unknown> }> = [];
    const ssh = new FakeSsh((remote) =>
      remote.includes('kill-session') ? { code: 255, stderr: 'ssh: connect: timed out' } : { stdout: '' }
    );
    const attach = new Attachment(
      { ssh, pty: new FakePty(), log: (line, extra) => lines.push({ line, extra }) },
      SPEC,
      new Recorder()
    );
    await attach.start();
    await attach.stop();
    expect(lines.map((l) => l.line)).toContain('a per-viewer tmux session may have leaked');
    expect(lines[0]?.extra?.viewer).toBe(attach.viewer);
  });
});

describe('the byte caps apply to a live stream', () => {
  it('coalesces past 15 frames a second and releases what it held', async () => {
    const r = rig();
    await r.attach.start();
    const seedFrames = r.sink.frames.length;

    const chunks = 40;
    for (let i = 0; i < chunks; i += 1) r.pty.data(`line ${i}\n`);
    const delivered = r.sink.frames.length - seedFrames;
    // ≤15 frames/s per Terminal: a journal that prints 40 times in a millisecond must not become 40
    // WebSocket frames.
    expect(delivered).toBeLessThanOrEqual(15);
    expect(delivered).toBeGreaterThan(0);

    // A second later the budget frees up, and the flush tick — not the next byte — is what releases
    // the tail. Without it the last lines of a burst would wait for a Worker that has gone quiet.
    r.clock.now += 1100;
    // THE BUDGET WINDOW IS VIRTUAL, THE FLUSH TICK IS NOT. `clock` is a fake, but `FLUSH_MS` is a
    // real `setInterval` in `attach.ts` — so this waits for the tick's own observable effect (the
    // tail arriving) instead of betting that 250 ms of wall clock was enough for it to fire on a
    // machine somebody else is also using.
    await until(() => r.sink.text().includes(`line ${chunks - 1}\n`), 'the flush tick to release the tail');
    const text = r.sink.text();
    for (let i = 0; i < chunks; i += 1) expect(text).toContain(`line ${i}\n`);
    expect(r.sink.elidedBytes).toBe(0);
  });

  it('stops the flush tick when the attach ends, so a dead tile costs no timer', async () => {
    const r = rig();
    await r.attach.start();
    r.pty.exit(0);
    await new Promise((res) => setTimeout(res, 10));
    const framesAtExit = r.sink.frames.length;
    r.pty.data('bytes after the exit\n');
    await new Promise((res) => setTimeout(res, 150));
    expect(r.sink.frames).toHaveLength(framesAtExit);
    expect(r.sink.text()).not.toContain('bytes after the exit');
  });

  it('survives a client that throws while being written to', async () => {
    const hostile: AttachSink = {
      bytes: () => {
        throw new Error('socket is gone');
      },
      elided: () => undefined,
      live: () => undefined,
      failed: () => undefined,
      ended: () => undefined,
    };
    const pty = new FakePty();
    const attach = new Attachment(
      { ssh: new FakeSsh(() => ({ stdout: 'backlog' })), pty },
      SPEC,
      hostile
    );
    // A throw out of a 'data' handler is a process-level crash, which in this PID costs the whole
    // wall. It must cost one tile.
    await expect(attach.start()).resolves.toBeUndefined();
    expect(() => pty.data('more\n')).not.toThrow();
    await attach.stop();
  });
});

describe('the real PTY runner', () => {
  /**
   * The read-only boundary, on a real child process.
   *
   * `no-such-host.invalid` cannot resolve, so this dials nothing — it exercises the spawn, the
   * listeners and the descriptors, and ssh exits on its own. What matters is the first descriptor:
   * with `'ignore'` the child gets /dev/null, so there is no file descriptor a byte could be put
   * into. ADR 0020's finding (3) is why that, and not `-r`, is the boundary.
   */
  it('gives the child no writable input, and reports its exit', async () => {
    const events: Array<{ code: number | null; stderr: string }> = [];
    const channel = realPtyRunner.open(
      { machine: 'kf-crawl-01', host: 'no-such-host.invalid' },
      'true',
      {
        onData: () => undefined,
        onSpawnError: () => undefined,
        onExit: (code, _signal, stderr) => events.push({ code, stderr }),
      }
    );
    expect(channel.noWritableInput).toBe(true);
    await until(() => events.length > 0, 'the real PTY child to exit');
    expect(events).toHaveLength(1);
    expect(events[0]?.code).not.toBe(0);
  });

  it('spawns with an ignored first descriptor, and no code here can write to one', () => {
    // Pinned against the source the way `readonly.test.ts` pins `ssh.ts`: the invariant is that no
    // writable descriptor toward a Machine is ever created, and a type cannot say that.
    const source = readFileSync(path.join(__dirname, 'attach.ts'), 'utf8');
    expect(source).toContain("stdio: ['ignore', 'pipe', 'pipe']");
    expect(source).not.toMatch(/stdio:\s*\[\s*'pipe'/);
    expect(source).not.toMatch(/stdio:\s*'inherit'/);
    expect(source).not.toMatch(/\bstdin\b\s*[.?]\s*\w+\s*\(/);
  });
});
