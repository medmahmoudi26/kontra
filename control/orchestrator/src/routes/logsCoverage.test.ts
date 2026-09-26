import { afterEach, describe, expect, it, vi } from 'vitest';

import type { QueueDescriber } from '../pollers';
import { coverage } from './logsCoverage';

/** Just enough `Repo` for the queue set. The real one carries thirty other things. */
function repoOf(actors: Array<{ name: string; version: string }>, queues: string[] = []) {
  return {
    listActors: () => actors,
    listWorkflows: () => queues.map((queue) => ({ queue })),
  } as unknown as Parameters<typeof coverage>[1];
}

/** A describer that answers from a map, and throws for anything not in it. */
function describerOf(byQueue: Record<string, string[]>): QueueDescriber {
  return {
    pollers: async (queue: string, type: string) => {
      const ids = byQueue[queue];
      if (!ids) throw new Error(`no such queue ${queue}`);
      // Both polled types are asked; answering only the activity one keeps the fold honest without
      // making every fixture list its identities twice.
      return type === 'activity' ? ids.map((identity) => ({ identity, lastAccess: 1 })) : [];
    },
  } as unknown as QueueDescriber;
}

/** VictoriaLogs answers JSON lines. `uniq by (worker)` yields one object per distinct value. */
function logsAnswering(workers: string[]) {
  return vi.fn(async () => ({
    ok: true,
    status: 200,
    text: async () => workers.map((worker) => JSON.stringify({ worker })).join('\n'),
  })) as unknown as typeof fetch;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

const DESYNC = '4147627@kf-desync-01@desync-0.3.1';
const DNS = '11@kf-dns-01@nscheck-0.1.0';

describe('coverage is a set difference over one identity', () => {
  it('reports nothing missing when every polling worker also logged', async () => {
    vi.stubGlobal('fetch', logsAnswering([DESYNC, DNS]));
    const got = await coverage(
      describerOf({ 'desync-0.3.1': [DESYNC], 'nscheck-0.1.0': [DNS] }),
      repoOf([
        { name: 'desync', version: '0.3.1' },
        { name: 'nscheck', version: '0.1.0' },
      ])
    );
    expect(got.ok).toBe(true);
    expect(got.missing).toEqual([]);
  });

  it('names the worker that is polling and not logging', async () => {
    // THE WHOLE POINT. This Worker is up — Temporal has seen it poll — and nothing it wrote has
    // arrived. Before the identity work the two sides had no common key and this was not a
    // question anything could be asked.
    vi.stubGlobal('fetch', logsAnswering([DNS]));
    const got = await coverage(
      describerOf({ 'desync-0.3.1': [DESYNC], 'nscheck-0.1.0': [DNS] }),
      repoOf([
        { name: 'desync', version: '0.3.1' },
        { name: 'nscheck', version: '0.1.0' },
      ])
    );
    expect(got.ok).toBe(false);
    expect(got.missing).toEqual([DESYNC]);
  });

  it('names a worker that logs but polls no queue this control plane knows about', async () => {
    // A stale deploy still running, an actor removed from the catalog while its Machine lives on,
    // or a Worker pointed at the wrong namespace. Worth reporting; not an error.
    vi.stubGlobal('fetch', logsAnswering([DNS, '9@kf-ghost-01@removed-9.9.9']));
    const got = await coverage(describerOf({ 'nscheck-0.1.0': [DNS] }), repoOf([{ name: 'nscheck', version: '0.1.0' }]));
    expect(got.unexpected).toEqual(['9@kf-ghost-01@removed-9.9.9']);
    // And it does NOT make the check fail: nothing expected is missing.
    expect(got.ok).toBe(true);
  });

  it('covers workflow queues too, not only actor queues', async () => {
    const WF = '31@kontra-api@wf-hunt-9f2c4e';
    vi.stubGlobal('fetch', logsAnswering([]));
    const got = await coverage(describerOf({ 'wf-hunt-9f2c4e': [WF] }), repoOf([], ['wf-hunt-9f2c4e']));
    expect(got.expected).toEqual([WF]);
    expect(got.missing).toEqual([WF]);
  });
});

describe('an unknown is not a zero', () => {
  it('reports null rather than "everything is missing" when the backend cannot be asked', async () => {
    // Reporting a backend outage as total log loss is the same category error the empty rail makes
    // one layer up — and it is the one that gets a check like this ignored after the second alarm.
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new Error('ECONNREFUSED');
      })
    );
    const got = await coverage(describerOf({ 'desync-0.3.1': [DESYNC] }), repoOf([{ name: 'desync', version: '0.3.1' }]));
    expect(got.ok).toBeNull();
    expect(got.missing).toBeNull();
    expect(got.seen).toBeNull();
    // The roster is still reported — that half succeeded, and saying so is what distinguishes
    // "could not ask the logs" from "could not ask anything".
    expect(got.expected).toEqual([DESYNC]);
    expect(got.detail).toContain('UNKNOWN rather than');
  });

  it('treats a non-200 from the backend as unknown, not as an empty answer', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => ({ ok: false, status: 503, text: async () => '' })) as unknown as typeof fetch
    );
    const got = await coverage(describerOf({ 'desync-0.3.1': [DESYNC] }), repoOf([{ name: 'desync', version: '0.3.1' }]));
    expect(got.ok).toBeNull();
    expect(got.detail).toContain('503');
  });
});

describe('one bad queue does not poison the answer', () => {
  it('skips a queue that cannot be described rather than failing the check', async () => {
    // The alternative is that one unreachable queue reports every other Worker as unexpected,
    // which is a false alarm about the wrong thing.
    vi.stubGlobal('fetch', logsAnswering([DNS]));
    const got = await coverage(
      describerOf({ 'nscheck-0.1.0': [DNS] }),
      repoOf([
        { name: 'nscheck', version: '0.1.0' },
        { name: 'gone', version: '1.0.0' },
      ])
    );
    expect(got.expected).toEqual([DNS]);
    expect(got.ok).toBe(true);
    expect(got.unexpected).toEqual([]);
  });

  it('skips a malformed log line rather than losing the coverage answer', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => ({
        ok: true,
        status: 200,
        text: async () => `${JSON.stringify({ worker: DNS })}\n{"worker": tru\n`,
      })) as unknown as typeof fetch
    );
    const got = await coverage(describerOf({ 'nscheck-0.1.0': [DNS] }), repoOf([{ name: 'nscheck', version: '0.1.0' }]));
    expect(got.seen).toEqual([DNS]);
    expect(got.ok).toBe(true);
  });
});
