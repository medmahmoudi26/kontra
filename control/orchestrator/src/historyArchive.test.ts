/**
 * The archive that outlives Temporal retention (ADR 0025).
 *
 * Everything here runs against an in-memory backing store and injected reads, because the two
 * things worth asserting — WHICH runs get archived, and WHEN the archive is allowed to answer —
 * are decisions, not I/O. The one fact that needed a live cluster is recorded rather than mocked:
 * a TERMINATED workflow reports a `closeTime`, which is why a sweep over closed runs catches the
 * runs that died (probe `kontra-archive-probe-1`, 2026-08-16, Temporal Server 1.31.0).
 */

import { describe, expect, it, vi } from 'vitest';

import { MemoryStore, ObjectStore } from './codec/objectStore';
import {
  ARCHIVE_VERSION,
  HistoryArchive,
  archiveKey,
  isArchivable,
  readHistoryOrArchive,
  sweepClosedRuns,
  GONE_IDS_CAP,
  startHistoryArchiver,
} from './historyArchive';
import { EVENT_CAP, HEAD_KEEP, mapHistory, type RawHistoryEvent, type RunHistory } from './history';
import { LIST_LIMIT, type RunRow } from './temporalClient';

function store(): { store: ObjectStore; backing: MemoryStore } {
  const backing = new MemoryStore();
  return { store: new ObjectStore({ backing }), backing };
}

/** A run row as `listRuns` reports one. */
function run(over: Partial<RunRow> = {}): RunRow {
  return {
    runId: 'nscheck-1786831339',
    status: 'completed',
    type: 'NsCheck',
    tenant: '',
    startedAt: 1_786_831_339_000,
    closedAt: 1_786_831_633_000,
    dispatches: 12,
    ...over,
  };
}

/** A reduced log with `n` events, through the real reducer — never a hand-built shape, so the cap
 *  and the elision under test are the ones the live path produces. */
function log(n: number): RunHistory {
  const raw: RawHistoryEvent[] = [];
  for (let i = 1; i <= n; i += 1) {
    raw.push({
      eventId: i,
      eventTime: { seconds: 1_786_831_339 + i, nanos: 0 },
      activityTaskScheduledEventAttributes: { activityType: { name: 'RunBatch' } },
    });
  }
  return mapHistory(raw);
}

describe('where an archived log lives', () => {
  it('files it under the run key, `run=` first, with the start instant as the partition', () => {
    // The measured layout (`units/run=…`), not an echo of it: the only read this key serves is
    // scoped to one run.
    expect(archiveKey('nscheck-1786831339', 1_786_831_339_000)).toBe(
      'history/run=nscheck-1786831339/dt=2026-08-15T22-02-19/log.json'
    );
  });

  it('cannot be forged into another prefix by a workflow id', () => {
    // A workflow id is a caller's string. `kontra-fleet/dns` is a REAL id in this system, and it
    // carries a slash.
    expect(archiveKey('kontra-fleet/dns', 0)).toBe(
      'history/run=kontra-fleet_dns/dt=1970-01-01T00-00-00/log.json'
    );
  });

  it('gives a REUSED workflow id its own object rather than overwriting the story before it', async () => {
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    await archive.write({ runId: 'kontra-fleet/dns', startedAt: 1_000_000, closedAt: 2_000_000 }, log(3));
    await archive.write({ runId: 'kontra-fleet/dns', startedAt: 9_000_000, closedAt: 9_500_000 }, log(7));

    // Both survive, and the read takes the newer one — `dt=` sorts lexicographically in time order.
    const got = await archive.readEnvelope('kontra-fleet/dns');
    expect(got?.startedAt).toBe(9_000_000);
    expect(got?.history.events).toHaveLength(7);
  });
});

describe('what an archived log holds', () => {
  it('round-trips the reducer output and marks it archived', async () => {
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    const written = log(5);
    await archive.write(run(), written, 1_786_900_000_000);

    const read = await archive.read('nscheck-1786831339');
    expect(read?.events).toEqual(written.events);
    expect(read?.scanned).toBe(5);
    expect(read?.archived).toBe(true);
    expect(read?.archivedAt).toBe(1_786_900_000_000);
    // A live read never carries the flag, so "archived" cannot be a thing the console guesses.
    expect(written.archived).toBeUndefined();
  });

  it('keeps the same cap and the same explicit elided count as the live path', async () => {
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    const big = log(EVENT_CAP + 500);
    await archive.write(run(), big);

    const read = await archive.read('nscheck-1786831339');
    expect(read?.events).toHaveLength(EVENT_CAP);
    expect(read?.elided).toBe(500);
    expect(read?.scanned).toBe(EVENT_CAP + 500);
    // The head survives the cap in the archive exactly as it does live: event 1 is what the run set
    // out to do, and a tail-only archive would lose it permanently rather than until the next poll.
    expect(read?.events[0]?.id).toBe(1);
    expect(read?.events[HEAD_KEEP]?.id).toBe(501 + HEAD_KEEP);
  });

  it('carries no payload, because there is none to carry', async () => {
    const { store: s, backing } = store();
    await new HistoryArchive(s).write(run(), log(3));
    const key = archiveKey('nscheck-1786831339', run().startedAt);
    const body = Buffer.from(backing.map.get(key)!).toString('utf8');
    const parsed = JSON.parse(body) as { v: number; history: { events: Array<Record<string, unknown>> } };
    expect(parsed.v).toBe(ARCHIVE_VERSION);
    for (const ev of parsed.history.events) {
      expect(ev).not.toHaveProperty('input');
      expect(ev).not.toHaveProperty('result');
    }
  });

  it('reads a corrupt or foreign object as NO archive rather than as half a log', async () => {
    const { store: s, backing } = store();
    backing.map.set(
      'history/run=broken/dt=2026-08-15T21-42-19/log.json',
      Buffer.from('{not json', 'utf8')
    );
    expect(await new HistoryArchive(s).read('broken')).toBeUndefined();

    backing.map.set(
      'history/run=empty/dt=2026-08-15T21-42-19/log.json',
      Buffer.from(JSON.stringify({ v: 1, runId: 'empty' }), 'utf8')
    );
    expect(await new HistoryArchive(s).read('empty')).toBeUndefined();
  });
});

describe('the history read falls back to the archive', () => {
  it('serves Temporal when Temporal has it, and never touches the archive', async () => {
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    await archive.write(run(), log(2)); // an archive exists, and must not win
    const live = vi.fn(async () => log(9));

    const got = await readHistoryOrArchive('nscheck-1786831339', undefined, archive, live);
    expect(got?.events).toHaveLength(9);
    expect(got?.archived).toBeUndefined();
  });

  it('serves the archive when Temporal answers not-found', async () => {
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    await archive.write(run(), log(4));

    const got = await readHistoryOrArchive(
      'nscheck-1786831339',
      undefined,
      archive,
      async () => undefined
    );
    expect(got?.events).toHaveLength(4);
    expect(got?.archived).toBe(true);
  });

  it('answers nothing when neither authority has anything', async () => {
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    expect(await readHistoryOrArchive('never-ran', undefined, archive, async () => undefined)).toBeUndefined();
  });

  it('propagates a cluster outage instead of serving a stale log as if it were live', async () => {
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    await archive.write(run(), log(4));
    const live = async (): Promise<RunHistory | undefined> => {
      throw new Error('14 UNAVAILABLE: connection refused');
    };
    // The distinction the route's 404-vs-502 rests on: not-found is an answer, an outage is not.
    await expect(readHistoryOrArchive('nscheck-1786831339', undefined, archive, live)).rejects.toThrow(
      'UNAVAILABLE'
    );
  });

  it('refuses an execution-pinned request rather than answering with the wrong execution', async () => {
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    await archive.write({ runId: 'kontra-fleet/dns', startedAt: 1_000, closedAt: 2_000 }, log(4));

    // `?exec=` exists because `kontra-fleet/dns` is the id of every bring-up AND teardown. The
    // archive records no execution id, so it cannot honour the pin — and a plausible wrong answer
    // is the one thing worse than no answer.
    expect(await archive.read('kontra-fleet/dns', '01a00772-8296')).toBeUndefined();
    expect(
      await readHistoryOrArchive('kontra-fleet/dns', '01a00772-8296', archive, async () => undefined)
    ).toBeUndefined();
  });
});

describe('the sweep', () => {
  it('archives a run that FAILED, because a failed run is a closed run', async () => {
    // The whole reason the archive is an external sweep rather than a step inside the caller's
    // workflow. MEASURED on the cluster: a terminated execution carries a closeTime.
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    const report = await sweepClosedRuns(archive, {
      list: async () => [run({ status: 'failed' })],
      read: async () => log(6),
      now: () => 1_786_900_000_000,
    });

    expect(report).toMatchObject({ scanned: 1, closed: 1, archived: 1, present: 0, gone: 0, failed: 0 });
    expect((await archive.read('nscheck-1786831339'))?.events).toHaveLength(6);
  });

  it('leaves an OPEN run alone — including a continued-as-new one, whose leg has closed', async () => {
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    const read = vi.fn(async () => log(3));
    const report = await sweepClosedRuns(archive, {
      // `closedAt` is set — each leg of a continue-as-new chain closes as it hands over — and the
      // run is still going. Archiving on `closedAt` alone would file a partial story and then skip
      // the run forever.
      list: async () => [run({ status: 'running', closedAt: 1_786_831_500_000 }), run({ closedAt: 0 })],
      read,
    });

    expect(report).toMatchObject({ scanned: 2, closed: 0, archived: 0 });
    expect(read).not.toHaveBeenCalled();
  });

  it('skips an already-archived run WITHOUT reading its history', async () => {
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    await archive.write(run(), log(6));
    const read = vi.fn(async () => log(6));

    const report = await sweepClosedRuns(archive, { list: async () => [run()], read });
    expect(report).toMatchObject({ closed: 1, present: 1, archived: 0 });
    // The steady-state cost of a pass: one HEAD per closed run, not a history read.
    expect(read).not.toHaveBeenCalled();
  });

  it('counts a run whose history is already gone rather than inventing an empty one', async () => {
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    const report = await sweepClosedRuns(archive, {
      list: async () => [run()],
      read: async () => undefined, // retention took it between the describe and the read
    });

    expect(report).toMatchObject({ closed: 1, gone: 1, archived: 0 });
    expect(await archive.read('nscheck-1786831339')).toBeUndefined();
  });

  it('one unreadable run does not stop the pass', async () => {
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    const errors: string[] = [];
    const report = await sweepClosedRuns(archive, {
      list: async () => [run({ runId: 'bad' }), run({ runId: 'good' })],
      read: async (runId) => {
        if (runId === 'bad') throw new Error('history read failed');
        return log(2);
      },
      onError: (_err, runId) => errors.push(runId ?? ''),
    });

    expect(report).toMatchObject({ closed: 2, archived: 1, failed: 1 });
    expect(errors).toEqual(['bad']);
    expect(await archive.read('good')).toBeDefined();
  });

  it('reads `archivable` off both dimensions of the status, not off the timestamp', () => {
    expect(isArchivable({ status: 'completed', closedAt: 1 })).toBe(true);
    expect(isArchivable({ status: 'failed', closedAt: 1 })).toBe(true);
    expect(isArchivable({ status: 'cancelled', closedAt: 1 })).toBe(true);
    expect(isArchivable({ status: 'running', closedAt: 1 })).toBe(false);
    expect(isArchivable({ status: 'pending', closedAt: 0 })).toBe(false);
    expect(isArchivable({ status: 'completed', closedAt: 0 })).toBe(false);
  });
});

/**
 * issue F5 — a lost run is NAMED, and a pass that did nothing says so.
 *
 * `gone` was a count with nothing behind it. Events lost to Temporal's retention are unrecoverable,
 * so the one moment they can be reconciled is the moment they are noticed — and an integer is not
 * something anybody can reconcile against, nor act on, nor even use to say what was in them.
 *
 * The two silent no-ops belong to the same finding. A store nobody configured and an archive
 * somebody switched off both produced NO log at all, so the first anybody knew was a run whose
 * history had aged out with no record of it.
 */
describe('what the sweep loses, and what it could not do', () => {
  it('names the runs that aged out, beside the count', async () => {
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    const report = await sweepClosedRuns(archive, {
      list: async () => [run({ runId: 'gone-a' }), run({ runId: 'gone-b' }), run({ runId: 'kept' })],
      read: async (id) => (id === 'kept' ? log(3) : undefined),
      now: () => 1_786_900_000_000,
    });

    expect(report.gone).toBe(2);
    // THE ASSERTION THE ISSUE IS ABOUT. The count was already right; it was the only thing there.
    expect(report.goneIds).toEqual(['gone-a', 'gone-b']);
    expect(report.archived).toBe(1);
  });

  it('keeps the count exact when it stops listing', async () => {
    // A report is a log line. A pass that lost hundreds has a bigger problem than the list — but
    // the NUMBER must stay true, or the bound quietly becomes the answer (the F3 mistake again).
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    const many = Array.from({ length: GONE_IDS_CAP + 7 }, (_, i) => run({ runId: `r${i}` }));
    const report = await sweepClosedRuns(archive, {
      list: async () => many,
      read: async () => undefined,
      now: () => 1_786_900_000_000,
    });

    expect(report.gone).toBe(GONE_IDS_CAP + 7);
    expect(report.goneIds).toHaveLength(GONE_IDS_CAP);
  });

  it('says when the listing came back FULL, because a full page is not a complete one', async () => {
    const { store: s } = store();
    const archive = new HistoryArchive(s);
    const full = Array.from({ length: LIST_LIMIT }, (_, i) => run({ runId: `r${i}` }));
    const capped = await sweepClosedRuns(archive, {
      list: async () => full,
      read: async () => log(2),
      now: () => 1_786_900_000_000,
    });
    expect(capped.capped).toBe(true);

    // Non-vacuous partner: one short of the cap is a complete page and must not claim otherwise.
    const short = await sweepClosedRuns(archive, {
      list: async () => full.slice(0, LIST_LIMIT - 1),
      read: async () => log(2),
      now: () => 1_786_900_000_000,
    });
    expect(short.capped).toBe(false);
  });

  it('reports an archive that is OFF, and one with no store, as DIFFERENT sentences', async () => {
    // Two ways to do nothing with two different fixes. Collapsing them is how an operator spends an
    // afternoon looking for a broken store that was never configured, or for a bug in a feature
    // somebody switched off on purpose.
    const notes: string[] = [];
    const before = process.env.KONTRA_HISTORY_ARCHIVE;

    process.env.KONTRA_HISTORY_ARCHIVE = 'off';
    startHistoryArchiver(store().store, { onNote: (n) => notes.push(n) })();
    expect(notes.at(-1), 'an OFF archive said nothing').toMatch(/OFF/);

    delete process.env.KONTRA_HISTORY_ARCHIVE;
    startHistoryArchiver(new ObjectStore({ endpoint: '' }), { onNote: (n) => notes.push(n) })();
    expect(notes.at(-1)).toMatch(/DISABLED/);
    expect(notes.at(-1)).toMatch(/KONTRA_S3_ENDPOINT/);
    // And they are not the same sentence, which is the whole point.
    expect(notes.at(-1)).not.toBe(notes.at(-2));

    if (before === undefined) delete process.env.KONTRA_HISTORY_ARCHIVE;
    else process.env.KONTRA_HISTORY_ARCHIVE = before;
  });

  it('does not route an ordinary state through the error channel', async () => {
    // `onError` is for faults. An operator who is told "error" about a deliberate configuration
    // learns to stop reading the errors.
    const errors: unknown[] = [];
    const before = process.env.KONTRA_HISTORY_ARCHIVE;
    process.env.KONTRA_HISTORY_ARCHIVE = 'off';
    startHistoryArchiver(store().store, { onError: (e) => errors.push(e) })();
    expect(errors).toEqual([]);
    if (before === undefined) delete process.env.KONTRA_HISTORY_ARCHIVE;
    else process.env.KONTRA_HISTORY_ARCHIVE = before;
  });
});
