/**
 * The Dataset record (ADR 0029 §1, §4): tags are a SET, the rename is a scalar, and only DEVIATION
 * is stored — an untagged, un-renamed Dataset has no row.
 *
 * Runs against SQLite by default; set `KONTRA_TEST_PG` to a Postgres URL and the same suite runs
 * against Postgres too, so a dialect difference in `ON CONFLICT` cannot hide behind "it passed
 * locally". The store touches no Temporal and no lake — it is a pure durable record — which is the
 * whole reason a Dataset can be tagged after its Run has closed.
 *
 * THE CONCURRENCY CLAIM NEEDS THE SECOND BACKEND TO MEAN ANYTHING. §1 makes tags a set so two
 * writers CONVERGE rather than race, and the only engine here that can actually run two writes at
 * once is Postgres: `node:sqlite` is synchronous, so a `Promise.all` against it is a queue wearing a
 * concurrency costume — it proves the rows land and nothing about the race. Both backends therefore
 * use a SHARED location (a temp file for SQLite, the database for Postgres) so the two-writer tests
 * below can hold two independent stores, which is what the author's process and the operator's
 * process actually are.
 */

import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterAll, beforeEach, describe, expect, it } from 'vitest';

import {
  DatasetRecordStore,
  InvalidDeviationError,
  TAG_MAX_LENGTH,
  normalizeTag,
} from './datasetRecords';

describe('normalizeTag / normalizeName', () => {
  it('trims, so leading/trailing space never mints a second member', () => {
    expect(normalizeTag('  prod ')).toBe('prod');
  });

  it('refuses an empty or over-long tag with a typed error a route maps to 400', () => {
    expect(() => normalizeTag('   ')).toThrow(InvalidDeviationError);
    expect(() => normalizeTag('x'.repeat(TAG_MAX_LENGTH + 1))).toThrow(InvalidDeviationError);
  });
});

// A FILE, not `:memory:`: two `make()`s have to reach the same rows for the two-writer tests to be
// about two writers. Torn down with the suite.
const sqliteDir = mkdtempSync(join(tmpdir(), 'kontra-dsrec-'));

const backends: Array<{ name: string; make: () => DatasetRecordStore }> = [
  { name: 'sqlite', make: () => new DatasetRecordStore({ url: join(sqliteDir, 'records.db') }) },
];
if (process.env.KONTRA_TEST_PG) {
  backends.push({
    name: 'postgres',
    make: () => new DatasetRecordStore({ url: process.env.KONTRA_TEST_PG!, schema: 'kontra_test' }),
  });
}

afterAll(() => rmSync(sqliteDir, { recursive: true, force: true }));

const RUN = 'a3f9c1e2-7b04-4a1d-9c88-0f21e6b3d5aa';
const OTHER = '0f2c9a1e-7b3d-4c58-9a10-6d2f8b4e1c37';

for (const backend of backends) {
  describe(`DatasetRecordStore (${backend.name})`, () => {
    let store: DatasetRecordStore;

    beforeEach(async () => {
      store = backend.make();
      await store.ensureSchema();
      await store.purgeRun(RUN);
      await store.purgeRun(OTHER);
    });

    afterAll(async () => {
      await store?.close().catch(() => undefined);
    });

    it('has NO record for an untouched Dataset — only deviation is stored', async () => {
      // The core of §4: the derived name and empty tags cost no row. `get` says undefined, not an
      // empty deviation, so a surface reads "use the derived name, show no tags".
      expect(await store.get(RUN)).toBeUndefined();
      expect(await store.list([RUN, OTHER])).toEqual([]);
    });

    it('is a SET: adding an existing tag is idempotent', async () => {
      await store.addTag(RUN, 'prod');
      await store.addTag(RUN, 'prod'); // same tag again — no error, no second row
      expect((await store.get(RUN))!.tags).toEqual(['prod']);
    });

    it('two adds of DIFFERENT tags both survive — the set converges, it does not overwrite', async () => {
      // A scalar would make the second add clobber the first. This is why tags are a set (§1): the
      // author's tag and the operator's tag coexist. Issued together to stand in for two writers.
      await Promise.all([store.addTag(RUN, 'author'), store.addTag(RUN, 'operator')]);
      expect((await store.get(RUN))!.tags).toEqual(['author', 'operator']); // sorted
    });

    it('remove takes one member and leaves the rest; removing an absent tag is a no-op', async () => {
      await store.addTag(RUN, 'keep');
      await store.addTag(RUN, 'drop');
      await store.removeTag(RUN, 'drop');
      await store.removeTag(RUN, 'never-there'); // not an error
      expect((await store.get(RUN))!.tags).toEqual(['keep']);
    });

    it('emptying the set removes the record — no row for a Dataset back to untagged', async () => {
      await store.addTag(RUN, 'only');
      await store.removeTag(RUN, 'only');
      expect(await store.get(RUN)).toBeUndefined();
    });

    it('is keyed by runId — one Run\'s tags never leak into another\'s', async () => {
      await store.addTag(RUN, 'mine');
      await store.addTag(OTHER, 'theirs');
      expect((await store.get(RUN))!.tags).toEqual(['mine']);
      expect((await store.get(OTHER))!.tags).toEqual(['theirs']);
    });

    it('stores a rename, and clearing it drops the record back to the derived default', async () => {
      await store.setRename(RUN, 'the-interesting-run');
      expect(await store.get(RUN)).toEqual({ runId: RUN, tags: [], renamedTo: 'the-interesting-run' });
      // Rename is a single choice, not a set — a second rename replaces, not accumulates.
      await store.setRename(RUN, 'renamed-again');
      expect((await store.get(RUN))!.renamedTo).toBe('renamed-again');
      await store.clearRename(RUN);
      expect(await store.get(RUN)).toBeUndefined();
    });

    it('carries tags and a rename together, each independently', async () => {
      await store.addTag(RUN, 'prod');
      await store.setRename(RUN, 'nightly-sweep');
      expect(await store.get(RUN)).toEqual({ runId: RUN, tags: ['prod'], renamedTo: 'nightly-sweep' });
      // Clearing the rename leaves the tags; the record persists because the tag is still a deviation.
      await store.clearRename(RUN);
      expect(await store.get(RUN)).toEqual({ runId: RUN, tags: ['prod'] });
    });

    it('lists deviations for a page of Runs in one call, omitting the untouched', async () => {
      await store.addTag(RUN, 'a');
      await store.addTag(RUN, 'b');
      await store.setRename(OTHER, 'other-name');
      const list = await store.list([RUN, OTHER, 'run-with-no-record']);
      expect(list).toContainEqual({ runId: RUN, tags: ['a', 'b'] });
      expect(list).toContainEqual({ runId: OTHER, tags: [], renamedTo: 'other-name' });
      expect(list).toHaveLength(2); // the untouched run contributes nothing
    });

    it('an empty id list touches nothing and returns nothing', async () => {
      expect(await store.list([])).toEqual([]);
    });

    /**
     * THE ID PAGE IS SIZED BY THE LAKE, NOT BY A PAGE OF ROWS, and past the engine's bind limit one
     * `IN (?,?,…)` throws — measured on this Node's `node:sqlite`: 32,766 binds is fine, 32,767
     * raises `too many SQL variables`. `GET /api/datasets` SWALLOWS that throw, so the symptom was
     * never an error: every Dataset silently lost its tags and its rename and fell back to the
     * derived default, which looks exactly like a correct answer. 33,000 is over the line by enough
     * that no build's off-by-one hides it.
     */
    it('reads an id page LARGER than the engine\'s bind limit', async () => {
      const ids = Array.from({ length: 33_000 }, (_, i) => `run-${i}`);
      await expect(store.list(ids)).resolves.toEqual([]);
    });

    it('finds a deviation whose id falls in a LATE page, not only the first', async () => {
      // The half a chunked read gets wrong quietly: query the pages and return the last one's rows,
      // or the first's, and the listing is still a 200 with most of its tags missing. The tagged Run
      // is deliberately at index 32,800 — past both the old limit and the first chunk boundary.
      const ids = Array.from({ length: 33_000 }, (_, i) => `run-${i}`);
      const late = ids[32_800]!;
      await store.addTag(late, 'keep');
      await store.setRename(late, 'the-good-one');
      try {
        expect(await store.list(ids)).toEqual([{ runId: late, tags: ['keep'], renamedTo: 'the-good-one' }]);
      } finally {
        await store.purgeRun(late);
      }
    });

    it('purge drops a Run\'s whole record — the retention arm', async () => {
      await store.addTag(RUN, 'x');
      await store.setRename(RUN, 'y');
      await store.purgeRun(RUN);
      expect(await store.get(RUN)).toBeUndefined();
    });

    it('refuses an empty tag rather than storing a blank member', async () => {
      await expect(store.addTag(RUN, '   ')).rejects.toBeInstanceOf(InvalidDeviationError);
      expect(await store.get(RUN)).toBeUndefined();
    });

    // --- the concurrency the SET exists for (ADR 0029 §1) --------------------------------------
    //
    // These are the tests the shared-location factory above is for. On SQLite they document intent
    // (the engine is synchronous, so nothing genuinely overlaps); on Postgres they are the real
    // thing — the pool hands out separate connections and two INSERTs contend for the same primary
    // key inside the server. Run them with KONTRA_TEST_PG set or they prove only that the rows land.

    it('converges under a DUPLICATE-tag race: N parallel adds, one row, no error', async () => {
      // THE `ON CONFLICT` PATH, actually raced. Sequential idempotence never reaches it: the second
      // insert simply finds a committed row. Concurrently, two inserts of one key contend for the
      // same index entry and one takes the conflict branch — which without `DO NOTHING` is a unique
      // violation surfacing as a 502 on a tag button that in fact worked.
      const adds = Array.from({ length: 8 }, () => store.addTag(RUN, 'prod'));
      await expect(Promise.all(adds)).resolves.toBeDefined();
      expect((await store.get(RUN))!.tags).toEqual(['prod']);
    });

    it('converges under TWO WRITERS on two connections — the author and the operator', async () => {
      // §4's two write paths are two PROCESSES: the author tagging at publish time and the operator
      // tagging afterwards. One store instance shares one pool, so this holds two — two independent
      // connections to the same rows, which is the shape the guarantee is actually made about.
      const author = backend.make();
      const operator = backend.make();
      try {
        await Promise.all([author.ensureSchema(), operator.ensureSchema()]);
        await Promise.all([
          author.addTag(RUN, 'nightly'),
          operator.addTag(RUN, 'interesting'),
          author.addTag(RUN, 'shared'), // both writers claim the same tag, at the same time
          operator.addTag(RUN, 'shared'),
        ]);
        // Neither writer clobbered the other, and the doubly-claimed tag is ONE member. A scalar
        // column would have left exactly one of these four and called it the answer.
        expect((await store.get(RUN))!.tags).toEqual(['interesting', 'nightly', 'shared']);
      } finally {
        await author.close().catch(() => undefined);
        await operator.close().catch(() => undefined);
      }
    });

    it('a concurrent add and remove of DIFFERENT tags leaves each with its own outcome', async () => {
      // Remove is a DELETE on one member, not a rewrite of the set, so it cannot take a tag a
      // different writer is adding at the same moment with it.
      await store.addTag(RUN, 'doomed');
      await Promise.all([store.removeTag(RUN, 'doomed'), store.addTag(RUN, 'kept')]);
      expect((await store.get(RUN))!.tags).toEqual(['kept']);
    });
  });
}
