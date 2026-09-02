/**
 * The Run's caller-workflow identity (ADR 0029 §2): snapshotted at start, keyed by `runId`, and
 * ABSENT for every Run that predates the store — which is why the name renderer has a fallback.
 *
 * Runs against SQLite by default; set `KONTRA_TEST_PG` and the same suite runs against Postgres too,
 * so a dialect difference in the `ON CONFLICT` upsert cannot hide behind "it passed locally". The
 * store touches no Temporal and no lake: it is provenance, written once and read by a rendering.
 */

import { afterAll, beforeEach, describe, expect, it } from 'vitest';

import {
  InvalidRunWorkflowError,
  RUN_WORKFLOW_MAX_LENGTH,
  RunWorkflowStore,
} from './runWorkflows';

const backends: Array<{ name: string; make: () => RunWorkflowStore }> = [
  { name: 'sqlite', make: () => new RunWorkflowStore({ url: ':memory:' }) },
];
if (process.env.KONTRA_TEST_PG) {
  backends.push({
    name: 'postgres',
    make: () => new RunWorkflowStore({ url: process.env.KONTRA_TEST_PG!, schema: 'kontra_test' }),
  });
}

const RUN = 'nscheck-1755612727';
const OTHER = 'nscheck-1755612728';

for (const backend of backends) {
  describe(`RunWorkflowStore (${backend.name})`, () => {
    let store: RunWorkflowStore;

    beforeEach(async () => {
      store = backend.make();
      await store.ensureSchema();
      await store.purgeRun(RUN);
      await store.purgeRun(OTHER);
    });

    afterAll(async () => {
      await store?.close().catch(() => undefined);
    });

    it('has NO row for a Run nobody stamped — the fallback case, made observable', async () => {
      // Every Dataset written before this store existed is this case. `get` says undefined, and the
      // name renderer falls back to the producing Actor rather than showing nothing.
      expect(await store.get(RUN)).toBeUndefined();
      expect(await store.list([RUN, OTHER])).toEqual([]);
    });

    it('snapshots the caller workflow\'s manifest name and version, keyed by runId', async () => {
      await store.record(RUN, 'nscheck', '0.1.0');
      expect(await store.get(RUN)).toEqual({ runId: RUN, workflow: 'nscheck', version: '0.1.0' });
    });

    it('is an UPSERT — a re-stamp of one Run states the same fact, it does not accumulate', async () => {
      await store.record(RUN, 'nscheck', '0.1.0');
      await store.record(RUN, 'nscheck', '0.2.0'); // the folder was re-registered at a new version
      expect(await store.get(RUN)).toEqual({ runId: RUN, workflow: 'nscheck', version: '0.2.0' });
      expect(await store.list([RUN])).toHaveLength(1); // one row, not two
    });

    it('is keyed by runId — one Run\'s identity never leaks into another\'s', async () => {
      await store.record(RUN, 'nscheck', '0.1.0');
      await store.record(OTHER, 'recon', '2.0.0');
      expect((await store.get(RUN))!.workflow).toBe('nscheck');
      expect((await store.get(OTHER))!.workflow).toBe('recon');
    });

    it('lists a page of Runs in one call, omitting the unstamped', async () => {
      await store.record(RUN, 'nscheck', '0.1.0');
      const list = await store.list([RUN, OTHER, '']);
      expect(list).toEqual([{ runId: RUN, workflow: 'nscheck', version: '0.1.0' }]);
      expect(await store.list([])).toEqual([]); // an empty page never opens the store
    });

    /**
     * THE PAGE IS SIZED BY THE LEDGER. `GET /api/datasets` fans every `runId` the ledger resolved
     * into this one call with no limit, so a 40k-Run controller asked for 40,000 bind variables —
     * and this Node's `node:sqlite` raises `too many SQL variables` at 32,767 (Postgres past
     * 65,535). The route swallows it, so the Datasets page silently showed the Actor-grain fallback
     * name for everything instead of the run-grain one. 33,000 is over the line on either engine's
     * smaller half.
     */
    it('reads an id page LARGER than the engine\'s bind limit', async () => {
      const ids = Array.from({ length: 33_000 }, (_, i) => `run-${i}`);
      await expect(store.list(ids)).resolves.toEqual([]);
    });

    it('returns an identity whose id falls in a LATE page, not only the first', async () => {
      // A chunked read that forgets to merge is still a 200: it just names most Datasets after the
      // producing Actor. The recorded Run sits at index 32,800, past every chunk boundary before it.
      const ids = Array.from({ length: 33_000 }, (_, i) => `run-${i}`);
      const late = ids[32_800]!;
      await store.record(late, 'nscheck', '0.1.0');
      try {
        expect(await store.list(ids)).toEqual([{ runId: late, workflow: 'nscheck', version: '0.1.0' }]);
      } finally {
        await store.purgeRun(late);
      }
    });

    it('trims, so a manifest with stray whitespace does not mint a second spelling', async () => {
      await store.record(RUN, '  nscheck ', ' 0.1.0 ');
      expect(await store.get(RUN)).toEqual({ runId: RUN, workflow: 'nscheck', version: '0.1.0' });
    });

    it('REFUSES a half identity rather than storing one — it renders a worse name than none', async () => {
      // `wf--0.1.0--…` and `wf-nscheck---…` are both worse than the Actor-grain fallback, so a blank
      // half is a typed refusal a route maps to 400, never a silently-coerced row.
      await expect(store.record(RUN, '', '0.1.0')).rejects.toBeInstanceOf(InvalidRunWorkflowError);
      await expect(store.record(RUN, 'nscheck', '  ')).rejects.toBeInstanceOf(InvalidRunWorkflowError);
      await expect(
        store.record(RUN, 'x'.repeat(RUN_WORKFLOW_MAX_LENGTH + 1), '0.1.0')
      ).rejects.toBeInstanceOf(InvalidRunWorkflowError);
      expect(await store.get(RUN)).toBeUndefined();
    });

    it('purge drops the Run\'s identity — the retention arm', async () => {
      await store.record(RUN, 'nscheck', '0.1.0');
      expect(await store.purgeRun(RUN)).toBe(1);
      expect(await store.get(RUN)).toBeUndefined();
      expect(await store.purgeRun(RUN)).toBe(0); // purging twice is a no-op, not an error
    });
  });
}
