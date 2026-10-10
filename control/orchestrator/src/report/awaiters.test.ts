import { describe, expect, it } from 'vitest';

import { CompletionAwaiters } from './awaiters';

const tick = () => new Promise((r) => setImmediate(r));

describe('CompletionAwaiters', () => {
  it('finalises a started run in its own namespace, once', async () => {
    const seen: string[] = [];
    let release!: () => void;
    const done = new Promise<void>((r) => (release = r));
    const a = new CompletionAwaiters(async (runId, ns) => {
      seen.push(`${ns}/${runId}`);
      await done;
    });
    expect(a.track('canary-1', 'ws-hello')).toBe(true);
    expect(a.track('canary-1', 'ws-hello')).toBe(false);
    expect(a.size).toBe(1);
    release();
    await tick();
    expect(seen).toEqual(['ws-hello/canary-1']);
    expect(a.size).toBe(0);
  });

  it('leaves runs past the cap to the sweep, and frees a slot when one finishes', async () => {
    let release!: () => void;
    const done = new Promise<void>((r) => (release = r));
    const a = new CompletionAwaiters(() => done, 2);
    expect(a.track('a', 'default')).toBe(true);
    expect(a.track('b', 'default')).toBe(true);
    expect(a.track('c', 'default')).toBe(false);
    release();
    await tick();
    expect(a.track('c', 'default')).toBe(true);
  });

  it('reports a failed finalise and still frees the slot', async () => {
    const errors: string[] = [];
    const a = new CompletionAwaiters(
      async () => {
        throw new Error('history unreadable');
      },
      1,
      (err, runId) => errors.push(`${runId}: ${(err as Error).message}`)
    );
    a.track('x', 'default');
    await tick();
    await tick();
    expect(errors).toEqual(['x: history unreadable']);
    expect(a.size).toBe(0);
  });
});
