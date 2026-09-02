import { EventEmitter } from 'node:events';
import { spawn } from 'node:child_process';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MAX_BACKOFF_MS, superviseChild, type SupervisedChild } from './supervisor';

/** A child that never runs anything — the supervisor's whole contract is observable from here. */
class FakeChild extends EventEmitter implements SupervisedChild {
  killed: string | number | undefined;
  constructor(readonly pid = 1234) {
    super();
  }
  kill(signal?: NodeJS.Signals | number): boolean {
    this.killed = signal;
    return true;
  }
}

afterEach(() => vi.useRealTimers());

describe('the panels supervisor', () => {
  it('restarts a child that exits, backing off and capping', () => {
    vi.useFakeTimers();
    const spawned: FakeChild[] = [];
    const lines: string[] = [];
    const sup = superviseChild({
      spawn: () => {
        const c = new FakeChild(spawned.length + 1);
        spawned.push(c);
        return c;
      },
      log: (line) => lines.push(line),
    });

    expect(spawned).toHaveLength(1);
    for (let i = 0; i < 8; i += 1) {
      spawned[spawned.length - 1]?.emit('exit', 1, null);
      vi.advanceTimersByTime(MAX_BACKOFF_MS + 1);
    }
    expect(spawned).toHaveLength(9);
    expect(sup.restarts).toBe(8);
    // Doubling, capped — a crash loop must not become a spin loop.
    expect(sup.backoffMs).toBe(MAX_BACKOFF_MS);
    expect(lines.some((l) => l.includes('restarting'))).toBe(true);
    sup.stop();
  });

  it('resets the backoff after a child that stayed up', () => {
    vi.useFakeTimers();
    let clock = 0;
    const spawned: FakeChild[] = [];
    const sup = superviseChild({
      spawn: () => {
        const c = new FakeChild();
        spawned.push(c);
        return c;
      },
      log: () => undefined,
      now: () => clock,
      healthyAfterMs: 1000,
    });
    spawned[0]?.emit('exit', 1, null);
    vi.advanceTimersByTime(5000);
    expect(sup.backoffMs).toBeGreaterThan(1000);

    clock += 60_000; // this one ran for a minute before dying
    spawned[spawned.length - 1]?.emit('exit', 0, 'SIGTERM');
    expect(sup.backoffMs).toBe(2000); // back to the floor, then doubled once for this exit
    sup.stop();
  });

  it('stops supervising, kills the child, and schedules nothing more', () => {
    vi.useFakeTimers();
    const spawned: FakeChild[] = [];
    const sup = superviseChild({
      spawn: () => {
        const c = new FakeChild();
        spawned.push(c);
        return c;
      },
      log: () => undefined,
    });
    sup.stop();
    expect(spawned[0]?.killed).toBe('SIGTERM');
    spawned[0]?.emit('exit', 0, 'SIGTERM');
    vi.advanceTimersByTime(MAX_BACKOFF_MS * 10);
    expect(spawned).toHaveLength(1);
    // Idempotent: a second stop (compose stopping a container that already lost its worker) is not
    // an error.
    expect(() => sup.stop()).not.toThrow();
  });

  it('survives a spawn that throws instead of becoming an exception in the provisioner', () => {
    vi.useFakeTimers();
    let attempts = 0;
    const lines: string[] = [];
    const sup = superviseChild({
      spawn: () => {
        attempts += 1;
        if (attempts < 3) throw new Error('ENOENT dist/src/panels.js');
        return new FakeChild();
      },
      log: (line) => lines.push(line),
    });
    expect(lines[0]).toContain('could not fork');
    vi.advanceTimersByTime(MAX_BACKOFF_MS * 4);
    expect(attempts).toBeGreaterThanOrEqual(3);
    sup.stop();
  });

  it('escalates SIGTERM to SIGKILL, then calls back either way', () => {
    vi.useFakeTimers();
    // A child that ignores SIGTERM — a shutdown that hangs, which slice 2 measured happening for
    // real: `PanelServer.close()` never resolved once a browser tab had gone away, because node 22
    // keeps an upgraded-then-destroyed socket in `getConnections()` forever.
    const stubborn = new (class extends FakeChild {
      signals: Array<string | number | undefined> = [];
      override kill(signal?: NodeJS.Signals | number): boolean {
        this.signals.push(signal);
        return true;
      }
    })();
    const sup = superviseChild({ spawn: () => stubborn, log: () => undefined });

    let doneCalled = 0;
    sup.stop({ killAfterMs: 1000, done: () => (doneCalled += 1) });
    expect(stubborn.signals).toEqual(['SIGTERM']);
    // Not yet: the child still has its grace period, and exiting now would orphan it.
    expect(doneCalled).toBe(0);

    vi.advanceTimersByTime(1001);
    expect(stubborn.signals).toEqual(['SIGTERM', 'SIGKILL']);
    expect(doneCalled).toBe(0);

    // …and if even SIGKILL does not land (an uninterruptible wait), the PARENT must not be the thing
    // that hangs. The deadline is absolute.
    vi.advanceTimersByTime(1001);
    expect(doneCalled).toBe(1);
    // Idempotent: a second callback would exit a process twice.
    vi.advanceTimersByTime(10_000);
    expect(doneCalled).toBe(1);
  });

  it('calls back as soon as a well-behaved child exits, without waiting out the grace period', () => {
    vi.useFakeTimers();
    const child = new FakeChild();
    const sup = superviseChild({ spawn: () => child, log: () => undefined });
    let doneCalled = 0;
    sup.stop({ killAfterMs: 5000, done: () => (doneCalled += 1) });
    expect(doneCalled).toBe(0);
    child.emit('exit', 0, 'SIGTERM');
    expect(doneCalled).toBe(1);
    // No SIGKILL was ever needed, and the timers are cleared rather than left to fire later.
    vi.advanceTimersByTime(20_000);
    expect(child.killed).toBe('SIGTERM');
    expect(doneCalled).toBe(1);
  });

  it('calls back immediately when there is no child to stop', () => {
    vi.useFakeTimers();
    const child = new FakeChild();
    const sup = superviseChild({ spawn: () => child, log: () => undefined });
    child.emit('exit', 1, null); // died, and the restart is still pending behind its backoff
    let doneCalled = 0;
    sup.stop({ done: () => (doneCalled += 1) });
    expect(doneCalled).toBe(1);
  });

  it('listens for the child’s error event', () => {
    vi.useFakeTimers();
    const child = new FakeChild();
    const lines: string[] = [];
    const sup = superviseChild({ spawn: () => child, log: (l) => lines.push(l) });
    // An 'error' with no listener is a process-level throw. Finding (6) measured exactly that shape
    // failing an in-flight `pulumi up`, so this listener is load-bearing, not politeness.
    expect(child.listenerCount('error')).toBe(1);
    child.emit('error', new Error('EPIPE'));
    expect(lines.some((l) => l.includes('child error'))).toBe(true);
    sup.stop();
  });
});

/**
 * The PID split itself.
 *
 * ADR 0020's finding (6): an unhandled rejection injected into a process running an inline Pulumi
 * `up()` makes that `up()` throw, every time, while the process survives — so the failure lands on
 * `kontra fleet deploy` and the provisioner still reports healthy. The fix is that the streamer is a
 * different PID.
 *
 * The full regression test the ADR asks for (inject into the streamer while a converge is in flight,
 * and require the converge to succeed) needs a real Pulumi backend and a real Machine, neither of
 * which exists here. What IS provable without them is the property the fix rests on: a child dying
 * of an unhandled rejection leaves the parent's own in-flight promise untouched, and the supervisor
 * brings the child back. If someone folds `panels.js` back into `infra.js`, that stops being true.
 */
/**
 * WAIT ON THE CONDITION, NOT ON THE CLOCK — the same defect as `workflows/stack.test.ts`, in a
 * file nobody suspected. A ceiling is a giving-up point, not a delay: it makes a slow machine slow
 * rather than wrong.
 */
async function waitFor(what: string, ready: () => boolean, ceilingMs = 30_000): Promise<void> {
  const deadline = Date.now() + ceilingMs;
  while (!ready()) {
    if (Date.now() >= deadline) {
      throw new Error(`waited ${ceilingMs} ms for ${what} and it never happened`);
    }
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}

describe('a crashing child does not disturb the parent (finding 6)', () => {
  it('lets the parent’s in-flight promise resolve, and restarts the child', async () => {
    // THE PARENT'S WORK, and the only fixed timer left in this test. It is legitimate: this
    // promise is the thing under OBSERVATION, not a synchronisation — a timer always resolves, so
    // no assertion below rides on 400 ms having been long enough for anything.
    const inFlight = new Promise<string>((resolve) => setTimeout(() => resolve('converged'), 400));
    const exits: number[] = [];
    let stderr = '';
    let spawns = 0;

    const sup = superviseChild({
      spawn: () => {
        spawns += 1;
        // Exactly what finding (6) injected, in its own PID: an unhandled rejection, a tick in.
        const child = spawn(
          process.execPath,
          ['-e', "setTimeout(() => { Promise.reject(new Error('panel stream died')); }, 30)"],
          { stdio: ['ignore', 'ignore', 'pipe'] }
        );
        child.stderr.on('data', (c: Buffer) => {
          stderr += c.toString();
        });
        child.stderr.on('error', () => undefined);
        child.once('exit', (code) => exits.push(code ?? -1));
        return child;
      },
      log: () => undefined,
      minBackoffMs: 20,
      maxBackoffMs: 20,
    });

    // The parent must not be the thing that notices. Nothing here awaits anything the child does.
    // THIS is the property, and it holds the moment the promise resolves.
    await expect(inFlight).resolves.toBe('converged');

    // EVERYTHING BELOW IS ABOUT THE CHILD, and it used to ride on the same 400 ms. Booting a real
    // `node -e`, rejecting a tick in, dying, flushing its stderr to us and being respawned behind a
    // 20 ms backoff is five asynchronous hops on a machine we do not control — MEASURED 2026-08-29
    // failing here on `exits.length` at loadavg ~15 while passing 9/9 in isolation. Each is now
    // waited for by its own observable effect.
    await waitFor('the child to exit', () => exits.length > 0);
    // Exit 1 with the rejection in stderr — the child died of the injection itself, not of a bad
    // command line (which Node exits 9 for).
    expect(exits[0]).toBe(1);
    // stderr is a separate stream and can land after the exit event, so it gets its own wait.
    await waitFor('the child’s stderr', () => stderr.includes('panel stream died'));
    await waitFor('the supervisor to bring it back', () => spawns > 1);
    sup.stop();
    // The parent's own handlers are untouched: nothing in this path installs one, which is the
    // property that stops a panel failure from being reported as `[runtime] Unhandled exception`.
    expect(process.listenerCount('unhandledRejection')).toBeLessThanOrEqual(1);
  });
});
