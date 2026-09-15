/**
 * The sharing and the teardown, which are the only two things `schemaWatch.ts` adds to `fs.watch`.
 *
 * Real directories and real writes: the bug being guarded against is a file descriptor that
 * outlives its reader, and a mocked `fs.watch` cannot be wrong about that.
 */

import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, describe, expect, it } from 'vitest';

import { MAX_WATCHED_DIRS, watchDir, watchedDirCount } from './schemaWatch';

const made: string[] = [];
const stops: Array<() => void> = [];

function dir(): string {
  const d = mkdtempSync(join(tmpdir(), 'kontra-schemawatch-'));
  made.push(d);
  return d;
}

/** Subscribe, remembering the unsubscribe so a failing assertion cannot leak a watcher. */
function subscribe(d: string, onChange: () => void): () => void {
  const stop = watchDir(d, onChange);
  stops.push(stop);
  return stop;
}

afterEach(() => {
  while (stops.length) stops.pop()?.();
  while (made.length) rmSync(made.pop() as string, { recursive: true, force: true });
});

describe('watchDir', () => {
  it('tells a subscriber that a file in the folder changed', async () => {
    const d = dir();
    const seen: number[] = [];
    subscribe(d, () => seen.push(1));

    writeFileSync(join(d, 'actor.py'), 'def run(name: str): ...\n');
    // fs.watch is asynchronous even for a synchronous write; poll rather than guess a sleep.
    for (let i = 0; i < 100 && seen.length === 0; i++) {
      await new Promise((r) => setTimeout(r, 10));
    }
    expect(seen.length).toBeGreaterThan(0);
  });

  it('opens ONE watcher for two readers of the same folder', () => {
    const d = dir();
    const before = watchedDirCount();
    subscribe(d, () => {});
    subscribe(d, () => {});
    expect(watchedDirCount()).toBe(before + 1);
  });

  it('closes the watcher only when the LAST reader leaves', () => {
    const d = dir();
    const before = watchedDirCount();
    const first = watchDir(d, () => {});
    const second = watchDir(d, () => {});

    first();
    expect(watchedDirCount()).toBe(before + 1); // second is still reading

    second();
    expect(watchedDirCount()).toBe(before);
  });

  it('ignores a second unsubscribe, rather than closing a later reader\'s watcher', () => {
    const d = dir();
    const before = watchedDirCount();
    const stop = watchDir(d, () => {});
    stop();
    expect(watchedDirCount()).toBe(before);

    // A NEW reader on the same directory, then the stale unsubscribe fires again — as Fastify's
    // `close` can. The new reader must survive it.
    subscribe(d, () => {});
    stop();
    expect(watchedDirCount()).toBe(before + 1);
  });

  it('refuses past the cap instead of running the process out of descriptors', () => {
    const dirs = Array.from({ length: MAX_WATCHED_DIRS + 1 }, () => dir());
    let refusal = '';
    try {
      for (const d of dirs) subscribe(d, () => {});
    } catch (err) {
      refusal = err instanceof Error ? err.message : String(err);
    }
    // The cap is what it says, and the message carries the number an operator would need.
    expect(refusal).toContain(String(MAX_WATCHED_DIRS));
  });
});
