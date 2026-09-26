/**
 * `sweepUnits` — the four things that keep a run's units, each pinned on its own.
 *
 * THIS FUNCTION DID NOT EXIST AND FOUR COMMENTS SAID IT DID (`data/retention.ts` ×3,
 * `historyArchive.ts`), plus a wiki table that named its file, its default and its posture. Meanwhile
 * `units/` grew to 254,801 objects and 7.56 GiB — 99.1% of the object store — in six days, with
 * nothing collecting any of it. So these tests are less about regression than about the function
 * being real, and about the ONE safeguard the documentation never mentioned: keep #2.
 *
 * NO S3 AND NO LAKE. The decision is pure and the I/O is two injected methods, so every branch is
 * reachable directly. `maintenance.test.ts` is where the DuckLake half needs a real extension.
 */

import { describe, expect, it } from 'vitest';

import {
  UNITS_RETENTION_DEFAULT_MS,
  classifyUnits,
  sweepUnits,
  type MaterializationLike,
  type UnitsSweepDeps,
} from './maintenance';

const DAY = 24 * 60 * 60 * 1000;
const NOW = Date.UTC(2026, 8, 24, 12, 0, 0);

/** One unit blob, at the real key shape the Go and Python hosts write (ADR 0015). */
function unit(runId: string, n: number, ageDays: number, size = 1240) {
  return {
    key: `units/run=${runId}/dt=2026-09-18/actor=probe/shard=0001/unit=${String(n).padStart(5, '0')}/sha${n}.json`,
    size,
    lastModified: new Date(NOW - ageDays * DAY),
  };
}

/** A store over a fixed listing, recording what it was asked to delete. */
function storeOf(objects: ReturnType<typeof unit>[]) {
  const deletedKeys: string[] = [];
  return {
    deletedKeys,
    store: {
      async list() {
        return objects;
      },
      async deleteMany(keys: readonly string[]) {
        deletedKeys.push(...keys);
        return { deleted: keys.length, errors: [] };
      },
    },
  };
}

function deps(
  objects: ReturnType<typeof unit>[],
  state: Record<string, MaterializationLike> = {}
): UnitsSweepDeps & { deletedKeys: string[] } {
  const { store, deletedKeys } = storeOf(objects);
  return {
    store,
    deletedKeys,
    async materializationState() {
      return new Map(Object.entries(state));
    },
  };
}

const collecting = { dryRun: false, now: NOW, retentionMs: 7 * DAY };

describe('classifyUnits — the decision, and the order it is made in', () => {
  const base = { pinned: false, materialization: 'complete' as const, unknownAge: false, lastWriteAt: NOW - 30 * DAY, cutoff: NOW - 7 * DAY };

  it('collects a run that is old, complete and unpinned', () => {
    expect(classifyUnits(base)).toBe('collect');
  });

  it('keeps a pinned run whatever else is true', () => {
    expect(classifyUnits({ ...base, pinned: true })).toBe('kept-pinned');
  });

  it('keeps a run whose materialization has not finished, however old it is', () => {
    // KEEP #2, and the one the wiki never mentioned. `units/` is the SOURCE a retry reads
    // (`activities/datasets.ts:resolveBatch`), so collecting these turns a retryable failure into
    // permanent data loss. Age does not make a pending retry safe.
    expect(classifyUnits({ ...base, materialization: 'running' })).toBe('kept-materializing');
    expect(classifyUnits({ ...base, materialization: 'failed' })).toBe('kept-materializing');
  });

  it('collects a run the ledger has never heard of', () => {
    // ABSENT is not the same as INCOMPLETE: no ledger rows means no dispatch is waiting to retry
    // against these objects, so age alone decides. Conflating the two would keep every pre-ledger
    // run forever.
    expect(classifyUnits({ ...base, materialization: undefined })).toBe('collect');
  });

  it('keeps a run whose age it could not establish, rather than guessing', () => {
    expect(classifyUnits({ ...base, unknownAge: true })).toBe('kept-unknown-age');
    // A zero timestamp is the same absence by another spelling, and must not compare as 1970.
    expect(classifyUnits({ ...base, lastWriteAt: 0 })).toBe('kept-unknown-age');
  });

  it('keeps a run inside the window', () => {
    expect(classifyUnits({ ...base, lastWriteAt: NOW - 1 * DAY })).toBe('kept-fresh');
  });

  it('puts an incomplete materialization AHEAD of the age test', () => {
    // The order is the policy. A run that is both ancient and mid-retry is kept, and the report says
    // why — 'kept-materializing', never 'kept-fresh', which would be a lie about the reason.
    expect(
      classifyUnits({ ...base, materialization: 'failed', lastWriteAt: NOW - 900 * DAY })
    ).toBe('kept-materializing');
  });

  it('puts an unknown age AHEAD of the cutoff comparison', () => {
    expect(classifyUnits({ ...base, unknownAge: true, lastWriteAt: NOW - 900 * DAY })).toBe('kept-unknown-age');
  });
});

describe('sweepUnits', () => {
  it('defaults to a dry run, and a dry run deletes nothing', async () => {
    const d = deps([unit('old', 0, 30), unit('old', 1, 30)], { old: 'complete' });
    // No `dryRun` key at all.
    const report = await sweepUnits(d, { now: NOW, retentionMs: 7 * DAY });

    expect(report.dryRun).toBe(true);
    expect(report.deleted).toBe(0);
    expect(d.deletedKeys).toEqual([]);
    // But it still says what it WOULD free — a preview nobody can act on is not a preview.
    expect(report.runs[0]?.disposition).toBe('collect');
    expect(report.bytes).toBe(2480);
  });

  it('deletes every object of a collected run, and only those', async () => {
    const d = deps(
      [unit('old', 0, 30), unit('old', 1, 30), unit('fresh', 0, 1)],
      { old: 'complete', fresh: 'complete' }
    );
    const report = await sweepUnits(d, collecting);

    expect(report.deleted).toBe(2);
    expect(d.deletedKeys.every((k) => k.includes('run=old/'))).toBe(true);
    expect(d.deletedKeys).toHaveLength(2);
  });

  it('collects a run WHOLE or not at all', async () => {
    // A half-swept run is worse than either outcome: `rowTail` reports progress by counting objects
    // under exactly this prefix, so a partial delete shows up as a smaller number rather than an
    // error — a run that produced plenty, reported as having produced less.
    const objects = Array.from({ length: 50 }, (_, i) => unit('old', i, 30));
    const d = deps(objects, { old: 'complete' });
    const report = await sweepUnits(d, collecting);

    expect(report.deleted).toBe(50);
    expect(d.deletedKeys).toHaveLength(50);
  });

  it('never deletes a key it did not weigh', async () => {
    // The delete list is built from the SAME listing the decision was made on, not a second LIST —
    // otherwise an actor that started pushing to a run between the two would have its fresh objects
    // deleted under a decision taken before they existed.
    const d = deps([unit('old', 0, 30)], { old: 'complete' });
    const report = await sweepUnits(d, collecting);
    expect(d.deletedKeys).toEqual([
      'units/run=old/dt=2026-09-18/actor=probe/shard=0001/unit=00000/sha0.json',
    ]);
    expect(report.scanned).toBe(1);
  });

  it('one object with no mtime keeps the WHOLE run', async () => {
    const objects = [unit('old', 0, 30), unit('old', 1, 30)];
    // @ts-expect-error — deliberately modelling a store that reported no mtime for one key.
    objects[1].lastModified = undefined;
    const d = deps(objects, { old: 'complete' });
    const report = await sweepUnits(d, collecting);

    expect(report.runs[0]?.disposition).toBe('kept-unknown-age');
    expect(report.runs[0]?.lastWriteAt).toBe(0);
    expect(d.deletedKeys).toEqual([]);
  });

  it('clocks a run on its NEWEST object, never its oldest', async () => {
    // A run that appends for weeks must not have its first units collected while it is still
    // pushing to the same prefix — `data/retention.ts` fix #2, applied to the object plane.
    const d = deps([unit('long', 0, 40), unit('long', 1, 1)], { long: 'complete' });
    const report = await sweepUnits(d, collecting);

    expect(report.runs[0]?.disposition).toBe('kept-fresh');
    expect(d.deletedKeys).toEqual([]);
  });

  it('pins the runs it is told to keep', async () => {
    const d = deps([unit('old', 0, 30), unit('keep', 0, 30)], { old: 'complete', keep: 'complete' });
    const report = await sweepUnits(d, { ...collecting, keepRuns: ['keep'] });

    expect(report.runs.find((r) => r.runId === 'keep')?.disposition).toBe('kept-pinned');
    expect(d.deletedKeys.every((k) => k.includes('run=old/'))).toBe(true);
  });

  it('leaves a key under units/ that is not run-shaped entirely alone', async () => {
    // Not understood is not the same as not needed. It is counted as scanned and never deleted on
    // a guess about what it might be.
    const d = deps([
      { key: 'units/stray.json', size: 10, lastModified: new Date(NOW - 900 * DAY) },
      unit('old', 0, 30),
    ], { old: 'complete' });
    const report = await sweepUnits(d, collecting);

    expect(report.scanned).toBe(2);
    expect(report.runs.map((r) => r.runId)).toEqual(['old']);
    expect(d.deletedKeys.some((k) => k.includes('stray'))).toBe(false);
  });

  it('reports what the store confirmed, not what it was asked', async () => {
    // A sweep that reported its input length would claim to have freed space it did not free.
    const objects = [unit('old', 0, 30), unit('old', 1, 30)];
    const report = await sweepUnits(
      {
        store: {
          async list() {
            return objects;
          },
          async deleteMany() {
            return { deleted: 1, errors: ['sha1.json: AccessDenied nope'] };
          },
        },
        async materializationState() {
          return new Map([['old', 'complete' as const]]);
        },
      },
      collecting
    );

    expect(report.deleted).toBe(1);
    expect(report.errors).toEqual(['sha1.json: AccessDenied nope']);
  });

  it('makes no delete call at all when nothing qualifies', async () => {
    let called = false;
    const report = await sweepUnits(
      {
        store: {
          async list() {
            return [unit('fresh', 0, 1)];
          },
          async deleteMany() {
            called = true;
            return { deleted: 0, errors: [] };
          },
        },
        async materializationState() {
          return new Map([['fresh', 'complete' as const]]);
        },
      },
      collecting
    );
    expect(called).toBe(false);
    expect(report.deleted).toBe(0);
  });

  it('orders the report biggest-first, so the head is where the space is', async () => {
    const d = deps(
      [unit('small', 0, 30, 100), unit('big', 0, 30, 9000), unit('mid', 0, 30, 500)],
      { small: 'complete', big: 'complete', mid: 'complete' }
    );
    const report = await sweepUnits(d, { ...collecting, dryRun: true });
    expect(report.runs.map((r) => r.runId)).toEqual(['big', 'mid', 'small']);
  });

  it('defaults the window to the documented ninety days', async () => {
    // The wiki states 90 days and the code must not quietly disagree with it. That this is far too
    // long for a busy deployment is a DEPLOYMENT question (`KONTRA_UNITS_RETENTION_DAYS`), not a
    // reason for the library default to drift from what is written down.
    expect(UNITS_RETENTION_DEFAULT_MS).toBe(90 * DAY);
    const d = deps([unit('old', 0, 60)], { old: 'complete' });
    // 60 days old, no `retentionMs` — inside the 90-day default, so kept.
    const report = await sweepUnits(d, { now: NOW, dryRun: false });
    expect(report.runs[0]?.disposition).toBe('kept-fresh');
    expect(d.deletedKeys).toEqual([]);
  });

  it('lists only units/, so it cannot be pointed at cas/', async () => {
    // The strongest form of the wiki's warning: the prefix is a module constant, so there is no
    // argument a caller could pass that would make this sweep content-addressed storage by age.
    let asked: string | undefined;
    await sweepUnits(
      {
        store: {
          async list(prefix: string) {
            asked = prefix;
            return [];
          },
          async deleteMany() {
            return { deleted: 0, errors: [] };
          },
        },
        async materializationState() {
          return new Map();
        },
      },
      collecting
    );
    expect(asked).toBe('units/');
  });
});
