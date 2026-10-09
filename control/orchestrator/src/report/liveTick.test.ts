import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { LiveHub, type RenderOnce } from './live';

/**
 * The tick and the producers' lifetime (ADR 0062, correction). A live session re-renders on a clock
 * while somebody watches, because its context now moves without a commit, and whatever feeds that
 * context is subscribed when the session opens and released when it closes.
 */

const key = { runId: 'canary-1', runStartedAt: 1 };
const snapshot = { v: 1, root: { type: 'root', children: [] }, blocks: {} };

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

function hub(extra: Partial<ConstructorParameters<typeof LiveHub>[0]> = {}) {
  const renderOnce = vi.fn<RenderOnce>(async () => ({ snapshot }) as never);
  return { renderOnce, hub: new LiveHub({ renderOnce, debounceMs: 10, ...extra }) };
}

describe('the tick', () => {
  it('re-renders an open session every tickMs, and stops when the last viewer leaves', async () => {
    const { hub: h, renderOnce } = hub({ tickMs: 2_000 });
    const opened = h.open(key, () => {});
    if (!opened.ok) throw new Error('refused');

    await vi.advanceTimersByTimeAsync(2_000 + 10);
    expect(renderOnce).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(renderOnce).toHaveBeenCalledTimes(2);

    opened.detach();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(renderOnce).toHaveBeenCalledTimes(2);
  });

  it('is off when tickMs is not set, which is what every caller before this had', async () => {
    const { hub: h, renderOnce } = hub();
    h.open(key, () => {});
    await vi.advanceTimersByTimeAsync(10_000);
    expect(renderOnce).not.toHaveBeenCalled();
  });
});

describe('onOpen', () => {
  it('runs once per session, not per viewer, and its cleanup runs when the session closes', () => {
    const cleanup = vi.fn();
    const onOpen = vi.fn(() => cleanup);
    const { hub: h } = hub({ onOpen });
    const a = h.open(key, () => {});
    const b = h.open(key, () => {});
    if (!a.ok || !b.ok) throw new Error('refused');
    expect(onOpen).toHaveBeenCalledTimes(1);

    a.detach();
    expect(cleanup).not.toHaveBeenCalled();
    b.detach();
    expect(cleanup).toHaveBeenCalledOnce();
  });

  it('cleans up on finalize as well, which is how a watched run usually ends', () => {
    const cleanup = vi.fn();
    const { hub: h } = hub({ onOpen: () => cleanup });
    h.open(key, () => {});
    h.finalize(key, 1);
    expect(cleanup).toHaveBeenCalledOnce();
  });

  it('a producer that throws does not refuse the viewer', () => {
    const { hub: h } = hub({
      onOpen: () => {
        throw new Error('row tail at its cap');
      },
    });
    expect(h.open(key, () => {}).ok).toBe(true);
  });
});
