/**
 * Child supervision for the streamer (ADR 0020, finding 6).
 *
 * This is the file the PID split rests on, so the reason is worth stating precisely. Pulumi's Node
 * language host installs process-global `unhandledRejection` / `uncaughtException` handlers for the
 * duration of every inline run. Measured on @pulumi/pulumi 3.256.0: an unhandled rejection, an
 * uncaught throw, or an `EventEmitter` `'error'` with no listener, injected two seconds into an
 * inline `up()`, made `up()` **throw** in every case, reporting the foreign error as
 * `error: [runtime] Unhandled exception`. **The process survived every time.**
 *
 * So co-locating the streamer with the provisioner does not crash anything anyone notices. It fails
 * `kontra fleet deploy`, blames panel code in the provisioner's error, and leaves the provisioner
 * reporting healthy. A forked child is enough, because the handlers are process-global and not
 * container-global — same container, same read-only key mount, same Fleet authority, different PID.
 *
 * Which makes the parent's obligations exact: it must never AWAIT anything the child produces, and
 * it must never let a child's failure become a promise in its own process. Hence a supervisor with
 * no async surface at all — `start` returns a handle, not a promise.
 */

export interface SupervisedChild {
  readonly pid?: number | undefined;
  on(event: 'error', listener: (err: Error) => void): unknown;
  once(event: 'exit', listener: (code: number | null, signal: string | null) => void): unknown;
  kill(signal?: NodeJS.Signals | number): unknown;
}

export interface SupervisorOptions {
  /** Fork the child. Injected so a test can supervise a fake and never spawn a process. */
  spawn(): SupervisedChild;
  log(line: string, extra?: Record<string, unknown>): void;
  /** First restart delay; doubles per consecutive failure. */
  minBackoffMs?: number;
  maxBackoffMs?: number;
  /** How long a child must stay up before its next exit is treated as a fresh failure rather than a
   * continuing crash loop. Without this, a child that runs fine for a day and then dies waits out
   * the maximum backoff for no reason. */
  healthyAfterMs?: number;
  now?(): number;
}

export interface StopOptions {
  /** How long the child gets to exit on SIGTERM before it is SIGKILLed. */
  killAfterMs?: number;
  /**
   * Called once the child has actually exited — or, if it cannot be killed at all, once the deadline
   * has passed anyway. A CALLBACK, not a promise: this file has no async surface by design (see the
   * header), and a caller that must not outlive its child still needs to know when it may exit.
   */
  done?: () => void;
}

export interface Supervisor {
  /** Stop supervising and kill the current child. Idempotent. */
  stop(opts?: StopOptions): void;
  readonly restarts: number;
  /** The delay the NEXT restart will wait. Exposed for tests and for a log line. */
  readonly backoffMs: number;
  readonly child: SupervisedChild | null;
}

export const MIN_BACKOFF_MS = 1000;
export const MAX_BACKOFF_MS = 30_000;
export const HEALTHY_AFTER_MS = 60_000;

/**
 * How long a child gets between SIGTERM and SIGKILL.
 *
 * SIGTERM alone is not enough, and the reason is measured. A child that hangs while shutting down
 * never exits, so a parent that sends SIGTERM and then exits itself ORPHANS it — still holding port
 * 8090 and the read-only fleet key. That is the same orphan `infra.ts`'s signal handlers were added
 * to prevent, reached through a different door: slice 2 found `PanelServer.close()` hanging forever
 * on node 22 whenever a browser tab had gone away (an upgraded socket the client destroyed stays in
 * `getConnections()`, so `http.close()`'s callback never fires). That specific cause is fixed, but a
 * shutdown path with no upper bound is a class of bug, not one bug.
 */
export const KILL_AFTER_MS = 5000;

/**
 * Start a child and keep it started.
 *
 * Synchronous by construction. Nothing here returns a promise, so there is nothing for the Pulumi
 * engine's handlers to catch on and nothing for `infra.ts` to accidentally await.
 */
export function superviseChild(opts: SupervisorOptions): Supervisor {
  const min = opts.minBackoffMs ?? MIN_BACKOFF_MS;
  const max = opts.maxBackoffMs ?? MAX_BACKOFF_MS;
  const healthyAfter = opts.healthyAfterMs ?? HEALTHY_AFTER_MS;
  const now = opts.now ?? (() => Date.now());

  let stopped = false;
  let restarts = 0;
  let backoff = min;
  let child: SupervisedChild | null = null;
  let timer: NodeJS.Timeout | null = null;

  const start = (): void => {
    if (stopped) return;
    const startedAt = now();
    let spawned: SupervisedChild;
    try {
      spawned = opts.spawn();
    } catch (err) {
      // A fork that cannot even be attempted (a missing dist file after a partial build) must not
      // become an exception in the provisioner's stack.
      opts.log('could not fork the panels child', { err: String(err), backoffMs: backoff });
      schedule();
      return;
    }
    child = spawned;
    opts.log('panels child started', { pid: spawned.pid });

    spawned.on('error', (err: Error) => {
      // An 'error' with no listener is a process-level throw. That is precisely the injection that
      // failed an in-flight `up` in finding (6), so this listener is not optional politeness.
      opts.log('panels child error', { err: String(err) });
    });

    spawned.once('exit', (code: number | null, signal: string | null) => {
      if (child === spawned) child = null;
      if (stopped) return;
      const ranFor = now() - startedAt;
      if (ranFor >= healthyAfter) backoff = min;
      restarts += 1;
      opts.log('panels child exited; restarting', {
        code,
        signal,
        ranForMs: ranFor,
        backoffMs: backoff,
        restarts,
      });
      schedule();
    });
  };

  const schedule = (): void => {
    if (stopped) return;
    const delay = backoff;
    backoff = Math.min(backoff * 2, max);
    timer = setTimeout(start, delay);
    // A pending restart must not be the reason `node dist/src/infra.js` refuses to exit.
    timer.unref?.();
  };

  /**
   * Stop, and make sure the child is really gone.
   *
   * SIGTERM, then SIGKILL after `killAfterMs` if it has not exited, then `done()` either way. The
   * escalation is what stops a child that hangs in its own shutdown from outliving this process; the
   * unconditional `done()` is what stops THIS process from hanging on a child it cannot kill. Both
   * failure modes end in the same place — a streamer nobody is supervising, holding the panels port
   * and the fleet key — so neither is left to chance.
   */
  const stop = (stopOpts?: StopOptions): void => {
    stopped = true;
    if (timer) clearTimeout(timer);
    timer = null;
    const victim = child;
    child = null;
    if (!victim) {
      stopOpts?.done?.();
      return;
    }

    const killAfter = stopOpts?.killAfterMs ?? KILL_AFTER_MS;
    let finished = false;
    let killTimer: NodeJS.Timeout | undefined;
    let giveUpTimer: NodeJS.Timeout | undefined;
    const finish = (why: string): void => {
      if (finished) return;
      finished = true;
      if (killTimer) clearTimeout(killTimer);
      if (giveUpTimer) clearTimeout(giveUpTimer);
      opts.log('panels child stopped', { why, pid: victim.pid });
      stopOpts?.done?.();
    };

    victim.once('exit', () => finish('exited'));
    try {
      victim.kill('SIGTERM');
    } catch {
      // Already gone: there is nothing to wait for.
      finish('already gone');
      return;
    }
    killTimer = setTimeout(() => {
      opts.log('panels child ignored SIGTERM; sending SIGKILL', { pid: victim.pid });
      try {
        victim.kill('SIGKILL');
      } catch {
        /* it raced us and exited */
      }
    }, killAfter);
    // A process in an uninterruptible wait survives even SIGKILL. Waiting on it forever would make
    // the parent the thing that hangs, so the deadline is absolute.
    giveUpTimer = setTimeout(() => {
      opts.log('panels child outlived SIGKILL; giving up on it', { pid: victim.pid });
      finish('gave up');
    }, killAfter * 2);
  };

  start();

  return {
    stop,
    get restarts() {
      return restarts;
    },
    get backoffMs() {
      return backoff;
    },
    get child() {
      return child;
    },
  };
}
