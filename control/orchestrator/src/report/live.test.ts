/**
 * LIVE REPORT MODE (ADR 0062) — the session, the diff and the caps.
 *
 * Acceptance 1 (N viewers cost one render, reads == renders), 2 (a patch never carries an unchanged
 * block) and 7 (the 51st viewer is refused and the existing 50 are unaffected) live here. 3
 * (reconnect replays the latest render) is here too, because a reconnect is a second `open` against a
 * session that already holds a snapshot.
 */

import { describe, expect, it, vi } from 'vitest';

import type { MdNode, ReportSnapshot, SnapshotBlock } from './render';
import {
  blockHash,
  blockHashes,
  boundBlock,
  changedBlocks,
  framesOf,
  LiveHub,
  LiveSession,
  liveKey,
  MAX_LIVE_RUNS,
  MAX_VIEWERS_PER_RUN,
  type LiveEvent,
  type LiveRunKey,
} from './live';

const KEY: LiveRunKey = { runId: 'enrich-1791234567', runStartedAt: 1_791_234_567_000 };

function text(value: string): MdNode {
  return { type: 'paragraph', children: [{ type: 'text', value }] } as unknown as MdNode;
}

function codeNode(blockId: string): MdNode {
  return { type: 'code', lang: 'http', blockId } as unknown as MdNode;
}

function block(b64: string): SnapshotBlock {
  return { lang: 'http', b64, redacted: false, truncated: false, fullBytes: b64.length } as SnapshotBlock;
}

function snap(children: MdNode[], blocks: Record<string, SnapshotBlock> = {}): ReportSnapshot {
  return { v: 1, root: { type: 'root', children } as unknown as MdNode, blocks } as ReportSnapshot;
}

/** A session whose render is a stub, so a test drives exactly how many renders happen. */
function session(
  renders: ReportSnapshot[],
  over: Partial<Parameters<typeof LiveSession.prototype.constructor>[1]> = {}
): { s: LiveSession; calls: () => number } {
  let i = 0;
  let calls = 0;
  const s = new LiveSession(KEY, {
    renderOnce: async () => {
      calls += 1;
      const snapshot = renders[Math.min(i, renders.length - 1)]!;
      i += 1;
      return { snapshot };
    },
    // Synchronous, so a test does not wait on a timer. The debounce is asserted separately.
    schedule: (fn) => {
      fn();
      return {};
    },
    ...over,
  });
  return { s, calls: () => calls };
}

describe('a block’s identity covers its bytes, not only its node', () => {
  /**
   * THE BUG THIS PREVENTS. A `code` node carries a `blockId` indexing a SIBLING map that holds the
   * bytes, so hashing the subtree alone makes a block whose CONTENT changed hash as unchanged — and
   * it would never be patched.
   */
  it('changes when the referenced block’s bytes change but the node does not', () => {
    const node = codeNode('b1');
    const a = blockHash(node, { b1: block('aGVsbG8=') });
    const b = blockHash(node, { b1: block('Z29vZGJ5ZQ==') });
    expect(b).not.toBe(a);
  });

  it('is stable when nothing changed', () => {
    const node = codeNode('b1');
    const blocks = { b1: block('aGVsbG8=') };
    expect(blockHash(node, blocks)).toBe(blockHash(node, blocks));
  });

  it('finds a blockId nested below the top-level child', () => {
    const nested = { type: 'blockquote', children: [codeNode('b7')] } as unknown as MdNode;
    const a = blockHash(nested, { b7: block('x') });
    const b = blockHash(nested, { b7: block('y') });
    expect(b).not.toBe(a);
  });

  it('hashes one child per top-level block', () => {
    expect(blockHashes(snap([text('a'), text('b'), text('c')]))).toHaveLength(3);
  });
});

describe('the diff', () => {
  it('names only the blocks whose hash moved', () => {
    expect(changedBlocks(['a', 'b', 'c'], ['a', 'X', 'c'])).toEqual([1]);
  });

  it('is empty when nothing moved', () => {
    expect(changedBlocks(['a', 'b'], ['a', 'b'])).toEqual([]);
  });

  /** One more row in a `{% for %}` shifts every block after it, so an index patch would misapply. */
  it('refuses to diff by index when the block count changed', () => {
    expect(changedBlocks(['a', 'b'], ['a', 'b', 'c'])).toBeNull();
    expect(changedBlocks(['a', 'b', 'c'], ['a', 'b'])).toBeNull();
  });
});

describe('a render is shared by every viewer', () => {
  /** ACCEPTANCE 1. Ten tabs must not cost ten renders, and reads must equal renders. */
  it('renders ONCE for ten viewers, and reads once', async () => {
    const hub = new LiveHub({
      renderOnce: async () => ({ snapshot: snap([text('one')]) }),
      schedule: (fn) => {
        fn();
        return {};
      },
    });
    const seen: LiveEvent[][] = [];
    for (let i = 0; i < 10; i++) {
      const box: LiveEvent[] = [];
      seen.push(box);
      const r = hub.open(KEY, (e) => box.push(e));
      expect(r.ok).toBe(true);
    }
    const s = hub.get(KEY)!;
    await s.renderNow();

    expect(s.renders).toBe(1);
    expect(s.reads).toBe(1);
    expect(s.reads).toBe(s.renders);
    // Every viewer got the frame from that one render.
    for (const box of seen) expect(box.filter((e) => e.type === 'snapshot')).toHaveLength(1);
  });

  /** ACCEPTANCE 1, the other half: 20 data changes produce at most 20 renders. */
  it('coalesces a burst into one render while one is in flight', async () => {
    let resolve!: () => void;
    const gate = new Promise<void>((r) => {
      resolve = r;
    });
    let calls = 0;
    const s = new LiveSession(KEY, {
      renderOnce: async () => {
        calls += 1;
        await gate;
        return { snapshot: snap([text(`n${calls}`)]) };
      },
      schedule: (fn) => {
        fn();
        return {};
      },
    });

    const first = s.renderNow();
    // Twenty arrivals while the first render is still running.
    for (let i = 0; i < 20; i++) s.touch();
    resolve();
    await first;
    await new Promise((r) => setImmediate(r));

    // One in flight plus exactly one more for everything that arrived during it.
    expect(calls).toBeLessThanOrEqual(2);
  });
});

describe('a patch carries only what changed', () => {
  /** ACCEPTANCE 2. */
  it('sends the first render whole, then only the changed block', async () => {
    const { s } = session([snap([text('a'), text('b')]), snap([text('a'), text('CHANGED')])]);
    const events: LiveEvent[] = [];
    s.viewers.add((e) => events.push(e));

    await s.renderNow();
    await s.renderNow();

    expect(events[0]).toMatchObject({ type: 'snapshot' });
    expect(events[0]!.type === 'snapshot' && events[0].blocks).toHaveLength(2);
    expect(events[1]).toMatchObject({ type: 'patch' });
    const patch = events[1]!;
    expect(patch.type === 'patch' && patch.blocks.map((b) => b.index)).toEqual([1]);
  });

  /** A render whose output is identical produces NO frame at all, not an empty patch. */
  it('sends nothing when the render did not change the document', async () => {
    const same = snap([text('a'), text('b')]);
    const { s } = session([same, same]);
    const events: LiveEvent[] = [];
    s.viewers.add((e) => events.push(e));

    await s.renderNow();
    await s.renderNow();

    expect(events).toHaveLength(1);
    expect(events[0]).toMatchObject({ type: 'snapshot' });
  });

  it('falls back to a whole snapshot when the block count changed', async () => {
    const { s } = session([snap([text('a')]), snap([text('a'), text('b')])]);
    const events: LiveEvent[] = [];
    s.viewers.add((e) => events.push(e));

    await s.renderNow();
    await s.renderNow();

    expect(events.map((e) => e.type)).toEqual(['snapshot', 'snapshot']);
  });
});

describe('a reconnect replays the latest render', () => {
  /** ACCEPTANCE 3: no gap and no duplicate — the joiner gets what the stream already produced. */
  it('hands a late viewer the snapshot the session already holds', async () => {
    const hub = new LiveHub({
      renderOnce: async () => ({ snapshot: snap([text('already rendered')]) }),
      schedule: (fn) => {
        fn();
        return {};
      },
    });
    hub.open(KEY, () => undefined);
    await hub.get(KEY)!.renderNow();

    const late: LiveEvent[] = [];
    const r = hub.open(KEY, (e) => late.push(e));
    expect(r.ok).toBe(true);
    const replay = hub.get(KEY)!.snapshotEvent()!;
    expect(replay).toMatchObject({ type: 'snapshot' });
    // The SAME session, so joining cost no second render.
    expect(hub.get(KEY)!.renders).toBe(1);
    expect(hub.size()).toBe(1);
  });
});

describe('the caps are checked before any per-run state exists', () => {
  const renderOnce = async (): Promise<{ snapshot: ReportSnapshot }> => ({ snapshot: snap([text('x')]) });

  /** ACCEPTANCE 7. */
  it(`refuses the ${MAX_VIEWERS_PER_RUN + 1}th viewer of one run and leaves the others attached`, () => {
    const hub = new LiveHub({ renderOnce });
    for (let i = 0; i < MAX_VIEWERS_PER_RUN; i++) {
      expect(hub.open(KEY, () => undefined).ok).toBe(true);
    }
    const refused = hub.open(KEY, () => undefined);
    expect(refused).toEqual({ ok: false, reason: 'per-run' });
    expect(hub.get(KEY)!.viewers.size).toBe(MAX_VIEWERS_PER_RUN);
  });

  /**
   * 50-per-run alone is satisfied by a flood spread over DIFFERENT ids — which is the measured
   * `rowTail` incident, where 250 fictional ids became 250 pollers. Hence a global arm too.
   */
  it('refuses a flood spread across run ids once the run cap is reached', () => {
    const hub = new LiveHub({ renderOnce });
    for (let i = 0; i < MAX_LIVE_RUNS; i++) {
      const k = { runId: `r${i}`, runStartedAt: i + 1 };
      expect(hub.open(k, () => undefined).ok).toBe(true);
    }
    const refused = hub.open({ runId: 'one-too-many', runStartedAt: 1 }, () => undefined);
    expect(refused).toEqual({ ok: false, reason: 'runs' });
    expect(hub.size()).toBe(MAX_LIVE_RUNS);
  });

  it('ends the session when the last viewer leaves, so an unwatched run costs nothing', () => {
    const hub = new LiveHub({ renderOnce });
    const a = hub.open(KEY, () => undefined);
    const b = hub.open(KEY, () => undefined);
    expect(a.ok && b.ok).toBe(true);
    expect(hub.size()).toBe(1);

    if (a.ok) a.detach();
    expect(hub.size()).toBe(1);
    if (b.ok) b.detach();
    expect(hub.size()).toBe(0);
    expect(hub.viewerCount()).toBe(0);
  });

  it('keys a session by the EXECUTION, so a reused run id is a different session', () => {
    const hub = new LiveHub({ renderOnce });
    hub.open({ runId: 'same', runStartedAt: 1 }, () => undefined);
    hub.open({ runId: 'same', runStartedAt: 2 }, () => undefined);
    expect(hub.size()).toBe(2);
    expect(liveKey({ runId: 'same', runStartedAt: 1 })).not.toBe(
      liveKey({ runId: 'same', runStartedAt: 2 })
    );
  });
});

describe('the freeze', () => {
  it('emits `final` with the stored version and ends the session', () => {
    const hub = new LiveHub({ renderOnce: async () => ({ snapshot: snap([text('x')]) }) });
    const events: LiveEvent[] = [];
    hub.open(KEY, (e) => events.push(e));
    hub.finalize(KEY, 3);

    expect(events).toEqual([{ type: 'final', version: 3 }]);
    expect(hub.size()).toBe(0);
  });

  it('passes a status change through without ending the session', () => {
    const hub = new LiveHub({ renderOnce: async () => ({ snapshot: snap([text('x')]) }) });
    const events: LiveEvent[] = [];
    hub.open(KEY, (e) => events.push(e));
    hub.statusChanged(KEY, 'running');

    expect(events).toEqual([{ type: 'status', status: 'running' }]);
    expect(hub.size()).toBe(1);
  });
});

describe('a failed live render is not published', () => {
  /**
   * The viewer keeps the last good document, and NOTHING writes an `error` version — which
   * `versionByKey` would find and skip for ever, welding the report shut on a transient failure.
   */
  it('reports the error and emits no frame', async () => {
    const onError = vi.fn();
    const s = new LiveSession(KEY, {
      renderOnce: async () => ({ error: 'template render limit exceeded' }),
      onError,
      schedule: (fn) => {
        fn();
        return {};
      },
    });
    const events: LiveEvent[] = [];
    s.viewers.add((e) => events.push(e));

    await s.renderNow();

    expect(events).toHaveLength(0);
    expect(onError).toHaveBeenCalledTimes(1);
    expect(s.snapshotEvent()).toBeUndefined();
  });
});

describe('one block cannot blow a frame', () => {
  it('replaces a block over the byte cap with a marker that says where to find it', () => {
    const huge = text('x'.repeat(1024 * 1024 + 10));
    const out = JSON.stringify(boundBlock(huge));
    expect(out.length).toBeLessThan(1024);
    expect(out).toContain('is in the stored report');
  });

  it('leaves an ordinary block untouched', () => {
    const small = text('hello');
    expect(boundBlock(small)).toBe(small);
  });

  it('frames every child when no indices are named', () => {
    expect(framesOf(snap([text('a'), text('b')]))).toHaveLength(2);
  });
});

describe('a degraded process says so in the snapshot', () => {
  /** `KONTRA_ROLES=api` never sees a commit, so a live view there would be frozen for no stated reason. */
  it('carries the reason on the snapshot event', async () => {
    const { s } = session([snap([text('x')])], {
      degraded: 'this process does not observe batch commits',
    });
    await s.renderNow();
    const event = s.snapshotEvent()!;
    expect(event.type === 'snapshot' && event.degraded).toBe(
      'this process does not observe batch commits'
    );
  });
});
