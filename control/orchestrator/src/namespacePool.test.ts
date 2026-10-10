import { afterEach, describe, expect, it, vi } from 'vitest';

import { runPerNamespace, type PooledWorker } from './namespacePool';

/** A worker that runs until told how to stop. */
function controllable(): PooledWorker & { stop: () => void; fail: (e: Error) => void } {
  let stop!: () => void;
  let fail!: (e: Error) => void;
  const running = new Promise<void>((resolve, reject) => {
    stop = resolve;
    fail = reject;
  });
  return { run: () => running, stop, fail };
}

const flush = () => new Promise((r) => setImmediate(r));

describe('runPerNamespace', () => {
  afterEach(() => vi.useRealTimers());

  it('serves every namespace it is given, each made usable before its worker polls', async () => {
    const order: string[] = [];
    const workers = new Map<string, ReturnType<typeof controllable>>();
    const pool = runPerNamespace({
      label: 't',
      namespaces: () => ['default', 'ws-hello'],
      ensure: async (ns) => void order.push(`ensure ${ns}`),
      make: async (ns) => {
        order.push(`make ${ns}`);
        const w = controllable();
        workers.set(ns, w);
        return w;
      },
      log: () => undefined,
    });
    await flush();
    expect(order).toEqual(['ensure default', 'ensure ws-hello', 'make default', 'make ws-hello']);
    workers.get('default')!.stop();
    await expect(pool).resolves.toBeUndefined();
  });

  it('picks up a workspace created after it started, on the rescan clock', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    let names = ['default'];
    const made: string[] = [];
    const workers: Array<ReturnType<typeof controllable>> = [];
    const pool = runPerNamespace({
      label: 't',
      namespaces: () => names,
      ensure: async () => undefined,
      make: async (ns) => {
        made.push(ns);
        const w = controllable();
        workers.push(w);
        return w;
      },
      rescanMs: 1000,
      log: () => undefined,
    });
    await flush();
    expect(made).toEqual(['default']);
    names = ['default', 'ws-new'];
    vi.advanceTimersByTime(1000);
    await flush();
    expect(made).toEqual(['default', 'ws-new']);
    // Not twice: a namespace already served is not served again.
    vi.advanceTimersByTime(1000);
    await flush();
    expect(made).toEqual(['default', 'ws-new']);
    workers[0]!.stop();
    await pool;
  });

  it('fails the whole pool, naming the namespace, when one worker fails', async () => {
    const w = controllable();
    const pool = runPerNamespace({
      label: 'materializer',
      namespaces: () => ['ws-broken'],
      ensure: async () => undefined,
      make: async () => w,
      log: () => undefined,
    });
    await flush();
    w.fail(new Error('poller died'));
    await expect(pool).rejects.toThrow(/materializer worker for namespace ws-broken stopped: poller died/);
  });

  it('keeps serving when the workspaces folder cannot be read on one pass', async () => {
    const lines: string[] = [];
    const w = controllable();
    let calls = 0;
    const pool = runPerNamespace({
      label: 't',
      namespaces: () => {
        calls += 1;
        if (calls === 1) throw new Error('EACCES');
        return ['default'];
      },
      ensure: async () => undefined,
      make: async () => w,
      rescanMs: 5,
      log: (l) => lines.push(l),
    });
    await new Promise((r) => setTimeout(r, 30));
    expect(lines.some((l) => l.includes('could not read the workspaces: EACCES'))).toBe(true);
    w.stop();
    await pool;
  });
});
