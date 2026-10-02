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
  windowCandidates,
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

/**
 * THE WINDOW: the tail carries what is landing, not only how much (issue 05).
 *
 * "1,187 rows" and "1,187 rows of the same 403 page" are the same number, and telling them apart is
 * most of "is this run doing the right thing". These hold the three properties that make the window
 * safe to add to a stream whose whole design is about not becoming a payload.
 */
describe('RowTailHub — the bounded row window', () => {
  /** A blob with a real size, so the byte budget has something to bind on. */
  function sized(runId: string, n: number, at: number, size: number): ListedObject {
    return { ...unit(runId, n, at), size };
  }

  function windowHub(
    objects: ListedObject[],
    bodies: Record<string, unknown>,
    opts: { windowRows?: number; windowBytes?: number } = {}
  ) {
    const reads: string[] = [];
    const hub = new RowTailHub({
      list: async () => objects,
      get: async (key) => {
        reads.push(key);
        const body = bodies[key];
        return body === undefined ? null : new TextEncoder().encode(JSON.stringify(body));
      },
      now: () => 1_000,
      schedule: () => () => {},
      ...opts,
    });
    return { hub, reads };
  }

  it('carries the newest rows, oldest-first, alongside the count', async () => {
    const objects = [sized('r', 1, 1000, 10), sized('r', 2, 2000, 10), sized('r', 3, 3000, 10)];
    const bodies = Object.fromEntries(objects.map((o, i) => [o.key, { host: `h${i + 1}` }]));
    const { hub } = windowHub(objects, bodies, { windowRows: 2 });

    const got: RowTailEvent[] = [];
    hub.subscribe('r', (e) => got.push(e));
    await hub.poll('r');

    const snap = snapshotsOf(got).at(-1)!;
    expect(snap.rows).toBe(3); // the COUNT is still every committed row
    expect(snap.window?.recent).toEqual([{ host: 'h2' }, { host: 'h3' }]); // the window is the newest two
    hub.close();
  });

  /**
   * THE BOUND THAT ACTUALLY BINDS. A count cap alone lets a Run emitting full header JSON carry
   * orders of magnitude more per snapshot than one emitting hostnames — the eviction asymmetry the
   * issue names. The size comes off the LIST, so an oversized blob costs no GET at all.
   */
  it('clips on bytes before it clips on rows, and says so', async () => {
    const objects = [sized('r', 1, 1000, 400), sized('r', 2, 2000, 400), sized('r', 3, 3000, 400)];
    const bodies = Object.fromEntries(objects.map((o, i) => [o.key, { i }]));
    const { hub, reads } = windowHub(objects, bodies, { windowRows: 5, windowBytes: 900 });

    const got: RowTailEvent[] = [];
    hub.subscribe('r', (e) => got.push(e));
    await hub.poll('r');

    const snap = snapshotsOf(got).at(-1)!;
    expect(snap.window?.recent).toHaveLength(2); // 2 x 400 fits in 900, a third does not
    expect(snap.window?.clipped).toBe(true);
    expect(reads).toHaveLength(2); // and the one that did not fit was never fetched
    hub.close();
  });

  it('a wide run cannot carry more bytes per snapshot than a narrow one', async () => {
    const narrow = [sized('n', 1, 1000, 100), sized('n', 2, 2000, 100), sized('n', 3, 3000, 100)];
    const wide = [sized('w', 1, 1000, 9_000), sized('w', 2, 2000, 9_000), sized('w', 3, 3000, 9_000)];
    const budget = 10_000;

    expect(windowCandidates(narrow, 5, budget).keys).toHaveLength(3);
    const w = windowCandidates(wide, 5, budget);
    expect(w.keys).toHaveLength(1);
    expect(w.clipped).toBe(true);
  });

  /** A chunk commits many blobs in one millisecond and some backends report whole seconds, so ties
   *  are the normal case. Without a stable tie-break the window reshuffles between identical polls
   *  and the panel flickers through rows at random. */
  it('orders ties by key, so two identical polls agree', async () => {
    const tied = [sized('r', 3, 1000, 10), sized('r', 1, 1000, 10), sized('r', 2, 1000, 10)];
    const a = windowCandidates(tied, 2, 1_000);
    const b = windowCandidates([...tied].reverse(), 2, 1_000);
    expect(a.keys).toEqual(b.keys);
  });

  /** The window is read from the SAME store the count is — it cannot report a row the durable path
   *  does not hold. A key that is gone is skipped, never invented. */
  it('cannot report a row the store does not hold', async () => {
    const objects = [sized('r', 1, 1000, 10), sized('r', 2, 2000, 10)];
    const { hub } = windowHub(objects, { [objects[1]!.key]: { host: 'only-this-one' } });

    const got: RowTailEvent[] = [];
    hub.subscribe('r', (e) => got.push(e));
    await hub.poll('r');

    expect(snapshotsOf(got).at(-1)!.window?.recent).toEqual([{ host: 'only-this-one' }]);
    hub.close();
  });

  /** A half-written object exists exactly mid-chunk, which is exactly when somebody is watching.
   *  Dropping one row beats tearing down the stream that was reporting the Run. */
  it('skips a blob that will not parse rather than failing the poll', async () => {
    const objects = [sized('r', 1, 1000, 10), sized('r', 2, 2000, 10)];
    const hub = new RowTailHub({
      list: async () => objects,
      get: async (key) =>
        new TextEncoder().encode(key === objects[0]!.key ? '{"half-writ' : '{"ok":true}'),
      now: () => 1_000,
      schedule: () => () => {},
    });
    const got: RowTailEvent[] = [];
    hub.subscribe('r', (e) => got.push(e));
    await hub.poll('r');

    const snap = snapshotsOf(got).at(-1)!;
    expect(snap.rows).toBe(2); // the count is unaffected — it is a LIST, not a read
    expect(snap.window?.recent).toEqual([{ ok: true }]);
    hub.close();
  });

  it('re-reads only what is new since the last poll', async () => {
    let objects = [sized('r', 1, 1000, 10), sized('r', 2, 2000, 10)];
    const bodies: Record<string, unknown> = {};
    for (const o of objects) bodies[o.key] = { k: o.key };
    const reads: string[] = [];
    const hub = new RowTailHub({
      list: async () => objects,
      get: async (key) => {
        reads.push(key);
        return new TextEncoder().encode(JSON.stringify(bodies[key] ?? { k: key }));
      },
      now: () => 1_000,
      schedule: () => () => {},
      windowRows: 3,
    });
    hub.subscribe('r', () => {});
    await hub.poll('r');
    expect(reads).toHaveLength(2);

    const third = sized('r', 3, 3000, 10);
    objects = [...objects, third];
    await hub.poll('r');
    expect(reads).toHaveLength(3); // the two it already had were not fetched again
    expect(reads[2]).toBe(third.key);
    hub.close();
  });

  /**
   * THE RING STAYS COUNTS-ONLY. `ringCap x ROW_TAIL_WINDOW_BYTES` behind every watched Run is 64 MB
   * across the run cap, on a controller with 512 MB for everything — so a replayed snapshot carries
   * the counts and the next live poll carries the rows.
   */
  it('keeps the window out of the ring, so a replay is counts-only', async () => {
    let objects = [sized('r', 1, 1000, 10)];
    const bodies: Record<string, unknown> = { [objects[0]!.key]: { a: 1 } };
    const hub = new RowTailHub({
      list: async () => objects,
      get: async (key) => new TextEncoder().encode(JSON.stringify(bodies[key] ?? {})),
      now: () => 1_000,
      schedule: () => () => {},
    });
    hub.subscribe('r', () => {});
    await hub.poll('r');
    const second = sized('r', 2, 2000, 10);
    bodies[second.key] = { b: 2 };
    objects = [...objects, second];
    await hub.poll('r');

    // A second reader resuming from seq 1 replays seq 2 out of the ring.
    const replayed: RowTailEvent[] = [];
    hub.subscribe('r', (e) => replayed.push(e), { lastEventId: 1 });
    const snap = snapshotsOf(replayed).at(-1)!;
    expect(snap.rows).toBe(2);
    expect(snap.window).toBeUndefined();
    hub.close();
  });

  /** `recent: []` would say the Run had committed nothing while the count beside it said otherwise.
   *  Absent is the honest shape for "this snapshot is not carrying rows". */
  it('omits recent from the frame entirely when there is no window', () => {
    const frame = rowTailFrame({
      kind: 'snapshot',
      snapshot: { runId: 'r', seq: 4, at: 10, rows: 9, lastChunkAt: 5 },
    });
    expect(frame).not.toContain('recent');
    expect(JSON.parse(frame.split('data: ')[1]!)).toEqual({
      runId: 'r',
      rows: 9,
      lastChunkAt: 5,
      at: 10,
    });
  });

  it('puts recent and clipped flat on the frame when there is one', () => {
    const frame = rowTailFrame({
      kind: 'snapshot',
      snapshot: {
        runId: 'r',
        seq: 5,
        at: 10,
        rows: 9,
        lastChunkAt: 5,
        window: { recent: [{ host: 'a.com' }], clipped: true },
      },
    });
    expect(JSON.parse(frame.split('data: ')[1]!)).toMatchObject({
      recent: [{ host: 'a.com' }],
      clipped: true,
    });
  });

  /** Without a `get` the tail is exactly what it was before windows existed — which is what makes
   *  this an addition rather than a requirement on every caller. */
  it('behaves exactly as before when no reader is injected', async () => {
    const objects = [sized('r', 1, 1000, 10)];
    const hub = new RowTailHub({ list: async () => objects, now: () => 1, schedule: () => () => {} });
    const got: RowTailEvent[] = [];
    hub.subscribe('r', (e) => got.push(e));
    await hub.poll('r');

    const snap = snapshotsOf(got).at(-1)!;
    expect(snap.rows).toBe(1);
    expect(snap.window).toBeUndefined();
    hub.close();
  });
});
