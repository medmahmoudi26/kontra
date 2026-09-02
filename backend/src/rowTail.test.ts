/**
 * The live row tail (live-datasets slice 05): watch a Run's rows land from the ONE durable path.
 *
 * Every acceptance criterion this file pins is about NOT lying — the count is the object count of
 * `units/run=<id>/` and nothing else, two subscribers see the identical snapshot, and a reconnect
 * resumes on the seq the client last saw. The poller is driven by hand (an injected `schedule` that
 * records the tick, and a public `poll`) so none of this waits on a real timer.
 */

import { describe, expect, it, vi } from 'vitest';

import type { ListedObject } from './codec/objectStore';
import { runPrefix } from './codec/shard';
import {
  ROW_TAIL_RING,
  RowTailHub,
  RowTailRefused,
  rowTailFrame,
  tallyEqual,
  tallyRows,
  type RowTailEvent,
  type RowTailSnapshot,
} from './rowTail';

function blob(key: string, at?: number): ListedObject {
  return { key, size: 1, lastModified: at !== undefined ? new Date(at) : undefined };
}

/** A blob at the hive key a real actor host would write for one pushed record. */
function unit(runId: string, n: number, at: number): ListedObject {
  return blob(`units/run=${runId}/dt=2026-08-19/actor=probe/shard=0001/unit=${String(n).padStart(5, '0')}/${'a'.repeat(64)}.json`, at);
}

/** Collect the snapshots a sink received, latest last. */
function snapshotsOf(events: RowTailEvent[]): RowTailSnapshot[] {
  return events.filter((e): e is Extract<RowTailEvent, { kind: 'snapshot' }> => e.kind === 'snapshot').map((e) => e.snapshot);
}

describe('tallyRows — the durable count is the object count', () => {
  it('counts .json unit blobs and takes the newest mtime as the last chunk', () => {
    const t = tallyRows([unit('r', 1, 1000), unit('r', 2, 3000), unit('r', 3, 2000)]);
    expect(t.rows).toBe(3);
    expect(t.lastChunkAt).toBe(3000);
  });

  it('does not count a non-row key (a prefix marker is not a pushed record)', () => {
    const t = tallyRows([unit('r', 1, 1000), blob('units/run=r/_marker', 5000)]);
    expect(t.rows).toBe(1);
    // The marker's mtime is ignored because it was not counted.
    expect(t.lastChunkAt).toBe(1000);
  });

  it('reports lastChunkAt null when the store gives no mtime — honest absence, not a zero', () => {
    const t = tallyRows([blob(`units/run=r/dt=x/actor=a/shard=0001/unit=00001/${'b'.repeat(64)}.json`)]);
    expect(t.rows).toBe(1);
    expect(t.lastChunkAt).toBeNull();
  });

  it('empty prefix is zero rows, null age', () => {
    expect(tallyRows([])).toEqual({ rows: 0, lastChunkAt: null });
  });

  it('tallyEqual coalesces identical readings including a null age', () => {
    expect(tallyEqual({ rows: 2, lastChunkAt: null }, { rows: 2, lastChunkAt: null })).toBe(true);
    expect(tallyEqual({ rows: 2, lastChunkAt: 1 }, { rows: 2, lastChunkAt: 2 })).toBe(false);
    expect(tallyEqual({ rows: 1, lastChunkAt: 1 }, { rows: 2, lastChunkAt: 1 })).toBe(false);
  });
});

/** A hub whose poller is driven manually: `schedule` records the tick instead of arming a timer, and
 *  tests call `hub.poll(run)` directly. `list` returns whatever the test staged for the prefix. */
function manualHub(objectsFor: (prefix: string) => ListedObject[], now: () => number = () => 1_000) {
  const ticks: Array<() => void> = [];
  const listed: string[] = [];
  const hub = new RowTailHub({
    list: async (prefix) => {
      listed.push(prefix);
      return objectsFor(prefix);
    },
    now,
    schedule: (tick) => {
      ticks.push(tick);
      return () => {};
    },
  });
  return { hub, ticks, listed };
}

describe('RowTailHub — one poller per run, fanned out', () => {
  it('reads exactly the durable prefix of the subscribed run', async () => {
    let objects: ListedObject[] = [];
    const { hub, listed } = manualHub(() => objects);
    const got: RowTailEvent[] = [];
    hub.subscribe('DnsSweep-abc', (e) => got.push(e));
    objects = [unit('DnsSweep-abc', 1, 1000)];
    await hub.poll('DnsSweep-abc');
    expect(listed).toContain(runPrefix('DnsSweep-abc'));
    expect(listed.every((p) => p === runPrefix('DnsSweep-abc'))).toBe(true);
    hub.close();
  });

  it('a snapshot carries the durable count and the last-chunk age', async () => {
    let objects: ListedObject[] = [];
    const { hub } = manualHub(() => objects, () => 5_000);
    const got: RowTailEvent[] = [];
    hub.subscribe('r', (e) => got.push(e));
    objects = [unit('r', 1, 1000), unit('r', 2, 4000)];
    await hub.poll('r');
    const [snap] = snapshotsOf(got);
    expect(snap).toMatchObject({ runId: 'r', rows: 2, lastChunkAt: 4000, seq: 1, at: 5_000 });
    hub.close();
  });

  it('TWO subscribers to one run receive the SAME snapshot object with the same seq (two tabs agree)', async () => {
    let objects: ListedObject[] = [];
    const { hub } = manualHub(() => objects);
    const a: RowTailEvent[] = [];
    const b: RowTailEvent[] = [];
    hub.subscribe('r', (e) => a.push(e));
    hub.subscribe('r', (e) => b.push(e));
    objects = [unit('r', 1, 1000)];
    await hub.poll('r');
    const [sa] = snapshotsOf(a);
    const [sb] = snapshotsOf(b);
    expect(sa).toBe(sb); // identical object — one producer, fanned out
    expect(sa!.seq).toBe(sb!.seq);
    hub.close();
  });

  it('coalesces an unchanged reading — no new seq while the run is idle', async () => {
    let objects: ListedObject[] = [unit('r', 1, 1000)];
    const { hub } = manualHub(() => objects);
    const got: RowTailEvent[] = [];
    hub.subscribe('r', (e) => got.push(e));
    await hub.poll('r'); // seq 1
    await hub.poll('r'); // identical → no emit
    objects = [unit('r', 1, 1000), unit('r', 2, 2000)];
    await hub.poll('r'); // changed → seq 2
    const seqs = snapshotsOf(got).map((s) => s.seq);
    expect(seqs).toEqual([1, 2]);
    hub.close();
  });

  it('seeds a late subscriber with the latest snapshot and keeps the same seq', async () => {
    let objects: ListedObject[] = [unit('r', 1, 1000)];
    const { hub } = manualHub(() => objects);
    const first: RowTailEvent[] = [];
    hub.subscribe('r', (e) => first.push(e));
    await hub.poll('r');
    objects = [unit('r', 1, 1000), unit('r', 2, 2000)];
    await hub.poll('r'); // seq 2

    const late: RowTailEvent[] = [];
    hub.subscribe('r', (e) => late.push(e));
    // The late tab is seeded with the current truth, not replayed from the start.
    expect(snapshotsOf(late).map((s) => ({ rows: s.rows, seq: s.seq }))).toEqual([{ rows: 2, seq: 2 }]);
    hub.close();
  });

  it('stops polling a run when its last subscriber leaves, and reads nothing for an unwatched run', async () => {
    const cancel = vi.fn();
    let objects: ListedObject[] = [];
    const listed: string[] = [];
    const hub = new RowTailHub({
      list: async (p) => {
        listed.push(p);
        return objects;
      },
      schedule: () => cancel,
    });
    const off = hub.subscribe('r', () => {});
    expect(hub.activeRuns()).toEqual(['r']);
    off();
    expect(hub.activeRuns()).toEqual([]);
    expect(cancel).toHaveBeenCalledOnce();
    // A poll for a run nobody watches is a no-op — no LIST.
    const before = listed.length;
    await hub.poll('r');
    expect(listed.length).toBe(before);
    hub.close();
  });
});

describe('RowTailHub resume — Last-Event-ID', () => {
  it('replays only snapshots newer than the seq the client last saw', async () => {
    let objects: ListedObject[] = [unit('r', 1, 1000)];
    const { hub } = manualHub(() => objects);
    hub.subscribe('r', () => {}); // holds the run open and advances its ring
    await hub.poll('r'); // seq 1
    objects = [unit('r', 1, 1000), unit('r', 2, 2000)];
    await hub.poll('r'); // seq 2
    objects = [unit('r', 1, 1000), unit('r', 2, 2000), unit('r', 3, 3000)];
    await hub.poll('r'); // seq 3

    const resumed: RowTailEvent[] = [];
    hub.subscribe('r', (e) => resumed.push(e), { lastEventId: 1 });
    expect(snapshotsOf(resumed).map((s) => s.seq)).toEqual([2, 3]);
    expect(resumed.some((e) => e.kind === 'resync')).toBe(false);
    hub.close();
  });

  it('when the client seq has been evicted from the ring, sends resync then the current truth', async () => {
    let objects: ListedObject[] = [];
    // A ring of 2, so early seqs fall out.
    const hub = new RowTailHub({ list: async () => objects, ringCap: 2, schedule: () => () => {} });
    hub.subscribe('r', () => {});
    for (let i = 1; i <= 4; i += 1) {
      objects = Array.from({ length: i }, (_, k) => unit('r', k + 1, (k + 1) * 1000));
      await hub.poll('r'); // seqs 1..4, ring holds only the last two (3,4)
    }
    const resumed: RowTailEvent[] = [];
    hub.subscribe('r', (e) => resumed.push(e), { lastEventId: 1 }); // 1 is long gone
    expect(resumed[0]).toEqual({ kind: 'resync', runId: 'r' });
    // …followed by the latest snapshot so the client is immediately correct.
    expect(snapshotsOf(resumed).map((s) => s.seq)).toEqual([4]);
    hub.close();
  });

  it('the ring is bounded by its own cap, not EVENT_CAP', async () => {
    let objects: ListedObject[] = [];
    const hub = new RowTailHub({ list: async () => objects, schedule: () => () => {} });
    hub.subscribe('r', () => {});
    for (let i = 1; i <= ROW_TAIL_RING + 10; i += 1) {
      objects = Array.from({ length: i }, (_, k) => unit('r', k + 1, (k + 1) * 1000));
      await hub.poll('r');
    }
    // A fresh subscriber whose base predates the cap gets a resync, proving the ring evicted — it did
    // not grow to hold every one of the (ROW_TAIL_RING + 10) snapshots.
    const resumed: RowTailEvent[] = [];
    hub.subscribe('r', (e) => resumed.push(e), { lastEventId: 1 });
    expect(resumed[0]).toEqual({ kind: 'resync', runId: 'r' });
    hub.close();
  });
});

describe('RowTailHub poll safety', () => {
  it('a LIST failure keeps the last good snapshot rather than tearing the stream down', async () => {
    const errors: unknown[] = [];
    let fail = false;
    let objects: ListedObject[] = [unit('r', 1, 1000)];
    const hub = new RowTailHub({
      list: async () => {
        if (fail) throw new Error('seaweed 500');
        return objects;
      },
      schedule: () => () => {},
      onError: (_run, err) => errors.push(err),
    });
    const got: RowTailEvent[] = [];
    hub.subscribe('r', (e) => got.push(e));
    await hub.poll('r'); // seq 1
    fail = true;
    await hub.poll('r'); // errors, emits nothing
    expect(errors).toHaveLength(1);
    expect(snapshotsOf(got).map((s) => s.seq)).toEqual([1]); // last good count still stands
    hub.close();
  });
});

describe('rowTailFrame — the SSE wire format', () => {
  it('a snapshot carries the seq as the id and the count as data', () => {
    const frame = rowTailFrame({
      kind: 'snapshot',
      snapshot: { runId: 'r', rows: 1203, lastChunkAt: 40, at: 44, seq: 7 },
    });
    expect(frame).toBe(`id: 7\ndata: ${JSON.stringify({ runId: 'r', rows: 1203, lastChunkAt: 40, at: 44 })}\n\n`);
  });

  it('a resync is a named event with no id — it is not a resumable position', () => {
    expect(rowTailFrame({ kind: 'resync', runId: 'r' })).toBe(`event: resync\ndata: ${JSON.stringify({ runId: 'r' })}\n\n`);
  });
});

/**
 * The caps, which is the half that was missing.
 *
 * REPRODUCED BEFORE IT WAS FIXED (`.scratch/post-merge-review/TRIAGE-2026-08-25.md` §7): one client,
 * no credential, 250 streams on 250 FICTIONAL run ids, 250 pollers, 750 LISTs in five seconds. The
 * id is never checked against anything — `made-up-0` is not a Run and was polled anyway — so the only
 * thing that can stop a flood here is arithmetic. Teardown was already correct and is pinned above;
 * these are the two bounds that were not there.
 */
describe('RowTailHub is bounded in total, and per run', () => {
  function hubWith(over: { maxRuns?: number; maxSinks?: number }): { hub: RowTailHub; listed: string[] } {
    const listed: string[] = [];
    const hub = new RowTailHub({
      list: async (p) => {
        listed.push(p);
        return [];
      },
      schedule: () => () => {},
      ...over,
    });
    return { hub, listed };
  }

  it('refuses an unseen run past the total cap, and starts no poller for it', async () => {
    const { hub, listed } = hubWith({ maxRuns: 2 });
    hub.subscribe('made-up-0', () => {});
    hub.subscribe('made-up-1', () => {});
    expect(hub.refusalFor('made-up-2')).toBe('too-many-runs');
    expect(() => hub.subscribe('made-up-2', () => {})).toThrow(RowTailRefused);
    // NOT WATCHED AND NOT SILENTLY POLLED. The 250-connection reproduction is exactly this line
    // failing: a `RunState` and an interval minted for any string the hub had not seen.
    expect(hub.activeRuns().sort()).toEqual(['made-up-0', 'made-up-1']);
    const before = listed.length;
    await hub.poll('made-up-2');
    expect(listed.length).toBe(before);
    hub.close();
  });

  it('still admits a run it is ALREADY watching when the total cap is reached', () => {
    const { hub } = hubWith({ maxRuns: 2 });
    hub.subscribe('a', () => {});
    hub.subscribe('b', () => {});
    // The cap is on RUNS, not on tabs: a second browser on a Run this appliance already polls costs
    // no new LIST and must not be turned away.
    expect(hub.refusalFor('a')).toBeNull();
    expect(() => hub.subscribe('a', () => {})).not.toThrow();
    expect(hub.activeRuns().sort()).toEqual(['a', 'b']);
    hub.close();
  });

  it('refuses a reader past the per-run cap — the hole the total cap alone leaves', () => {
    // A flood that puts every connection on ONE id satisfies `maxRuns` with one poller and an
    // unbounded `Set` of sinks that poller walks on every tick.
    const { hub } = hubWith({ maxSinks: 2 });
    hub.subscribe('r', () => {});
    hub.subscribe('r', () => {});
    expect(hub.refusalFor('r')).toBe('too-many-readers');
    expect(() => hub.subscribe('r', () => {})).toThrow(/too many|readers/i);
    hub.close();
  });

  it('a refusal names which cap said no, because they have different fixes', () => {
    const { hub } = hubWith({ maxRuns: 1, maxSinks: 1 });
    hub.subscribe('r', () => {});
    try {
      hub.subscribe('r', () => {});
      expect.unreachable('the per-run cap should have refused');
    } catch (err) {
      expect(err).toBeInstanceOf(RowTailRefused);
      expect((err as RowTailRefused).refusal).toBe('too-many-readers');
    }
    try {
      hub.subscribe('other', () => {});
      expect.unreachable('the total cap should have refused');
    } catch (err) {
      expect((err as RowTailRefused).refusal).toBe('too-many-runs');
    }
    hub.close();
  });

  it('releasing a subscriber gives its budget back', () => {
    const { hub } = hubWith({ maxRuns: 1 });
    const off = hub.subscribe('a', () => {});
    expect(hub.refusalFor('b')).toBe('too-many-runs');
    off();
    // The last unsubscribe drops the run, so the appliance can watch a different one. A cap that
    // only ever counted up would turn into a permanent refusal after enough runs had been opened.
    expect(hub.refusalFor('b')).toBeNull();
    hub.close();
  });
});
