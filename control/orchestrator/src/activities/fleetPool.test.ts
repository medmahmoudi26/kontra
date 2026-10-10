/**
 * A PROVIDER UNDER THE POOL'S ACTIVITY: what it is lent (progress, retry, cancellation), and the
 * keepalive that stops a droplet converge — minutes of silence inside one install command — from
 * tripping the pool's 5-minute heartbeatTimeout.
 */
import { MockActivityEnvironment } from '@temporalio/testing';
import { describe, expect, it, vi } from 'vitest';

import { PROVIDER_KEEPALIVE_MS, withProviderRun } from './fleetPool';

describe('withProviderRun', () => {
  it("turns the provider's progress into heartbeats, and says when the attempt is a retry", async () => {
    const env = new MockActivityEnvironment({ attempt: 2 });
    const beats: unknown[] = [];
    env.on('heartbeat', (d: unknown) => beats.push(d));
    const seen = await env.run(() =>
      withProviderRun(async (run) => {
        run.progress?.({ op: 'create', urn: 'urn:droplet' });
        return { retrying: run.retrying, aborts: run.signal instanceof AbortSignal };
      })
    );
    expect(seen).toEqual({ retrying: true, aborts: true });
    expect(beats).toEqual([{ op: 'create', urn: 'urn:droplet' }]);
  });

  it('is not a retry on the first attempt', async () => {
    const seen = await new MockActivityEnvironment({ attempt: 1 }).run(() => withProviderRun(async (run) => run.retrying));
    expect(seen).toBe(false);
  });

  it('keeps beating the last progress while the provider is silent, and stops when it returns', async () => {
    vi.useFakeTimers();
    try {
      const env = new MockActivityEnvironment();
      const beats: unknown[] = [];
      env.on('heartbeat', (d: unknown) => beats.push(d));
      let release: (() => void) | undefined;
      const done = env.run(() =>
        withProviderRun(async (run) => {
          run.progress?.({ op: 'create', urn: 'urn:install' });
          await new Promise<void>((r) => (release = r));
        })
      );
      await vi.waitFor(() => expect(release).toBeDefined());
      await vi.advanceTimersByTimeAsync(PROVIDER_KEEPALIVE_MS * 3);
      release!();
      await done;
      // One from the progress, then one per keepalive tick — each naming what is still in flight.
      expect(beats.length).toBeGreaterThanOrEqual(4);
      expect(beats.every((b) => JSON.stringify(b) === JSON.stringify({ op: 'create', urn: 'urn:install' }))).toBe(true);
      const after = beats.length;
      await vi.advanceTimersByTimeAsync(PROVIDER_KEEPALIVE_MS * 3);
      expect(beats.length).toBe(after);
    } finally {
      vi.useRealTimers();
    }
  });

  it('runs the provider plainly outside an activity — a test, or the e2e job', async () => {
    const seen = await withProviderRun(async (run) => {
      run.progress?.({ op: 'create' });
      return { retrying: run.retrying, signal: run.signal };
    });
    expect(seen).toEqual({ retrying: false, signal: undefined });
  });
});
