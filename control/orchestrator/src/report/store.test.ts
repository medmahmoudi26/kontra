/**
 * The four report tables.
 *
 * Runs against SQLite by default; set `KONTRA_TEST_PG` and the same suite runs against Postgres too,
 * so a dialect difference in the `ON CONFLICT` upsert or in the version-allocating `INSERT … SELECT`
 * cannot hide behind "it passed locally" — the family's rule, and this store leans on both.
 *
 * Acceptance tests 12 (pinning), 13 (idempotent render), 16 (deletion) and 19 (feedback) live here in
 * their storage half.
 */

import { afterAll, beforeEach, describe, expect, it } from 'vitest';

import { FEEDBACK_MAX_BYTES, InvalidFeedbackError, ReportStore } from './store';

const backends: Array<{ name: string; make: () => ReportStore }> = [
  { name: 'sqlite', make: () => new ReportStore({ url: ':memory:' }) },
];
if (process.env.KONTRA_TEST_PG) {
  backends.push({
    name: 'postgres',
    make: () => new ReportStore({ url: process.env.KONTRA_TEST_PG!, schema: 'kontra_test' }),
  });
}

const RUN = 'enrich-1791234567';
const OTHER = 'enrich-1791234568';

for (const backend of backends) {
  describe(`ReportStore (${backend.name})`, () => {
    let store: ReportStore;

    beforeEach(async () => {
      store = backend.make();
      await store.ensureSchema();
      await store.purgeRun(RUN);
      await store.purgeRun(OTHER);
    });

    afterAll(async () => {
      await store?.close().catch(() => undefined);
    });

    describe('the pinned template — ACCEPTANCE 12', () => {
      it('stores the TEXT, so editing report.md afterwards cannot change this run', async () => {
        await store.pinTemplate({
          runId: RUN,
          templateHash: 'sha256:aaa',
          templateText: '# original\n',
          source: 'workspace',
        });
        const pinned = await store.template(RUN);
        expect(pinned?.templateText).toBe('# original\n');
        expect(pinned?.source).toBe('workspace');
      });

      it('upserts by run id, so a retried stamp costs one row and no error', async () => {
        await store.pinTemplate({ runId: RUN, templateHash: 'h1', templateText: 'a', source: 'workspace' });
        await store.pinTemplate({ runId: RUN, templateHash: 'h2', templateText: 'b', source: 'default' });
        const pinned = await store.template(RUN);
        expect(pinned?.templateHash).toBe('h2');
        expect(pinned?.source).toBe('default');
      });

      it('answers undefined for a run nothing pinned, which is the fallback case', async () => {
        expect(await store.template('never-started')).toBeUndefined();
      });
    });

    describe('versions — ACCEPTANCE 13', () => {
      const render = (renderKey: string, snapshot = '{"v":1}') => ({
        runId: RUN,
        status: 'ok' as const,
        templateHash: 'sha256:aaa',
        renderKey,
        snapshotJson: snapshot,
        renderedBy: 'sweep',
      });

      it('numbers versions from 1 and increments', async () => {
        const first = await store.declareVersion(render('k1'));
        const second = await store.declareVersion(render('k2'));
        expect(first).toEqual({ created: true, version: 1 });
        expect(second).toEqual({ created: true, version: 2 });
      });

      it('the SAME render key produces ONE version, which is what the completion hook firing twice needs', async () => {
        const first = await store.declareVersion(render('same'));
        const again = await store.declareVersion(render('same'));
        expect(first).toEqual({ created: true, version: 1 });
        expect(again).toEqual({ created: false, version: 1 });
        expect(await store.versions(RUN)).toHaveLength(1);
      });

      it('numbers each run independently', async () => {
        await store.declareVersion(render('k1'));
        const other = await store.declareVersion({ ...render('k1'), runId: OTHER });
        expect(other.version).toBe(1);
      });

      it('serves the latest version when none is named', async () => {
        await store.declareVersion(render('k1', '{"v":1,"n":1}'));
        await store.declareVersion(render('k2', '{"v":1,"n":2}'));
        const latest = await store.version(RUN);
        expect(latest?.version).toBe(2);
        expect(latest?.snapshotJson).toBe('{"v":1,"n":2}');
      });

      it('serves a specific version, and nothing for one that does not exist', async () => {
        await store.declareVersion(render('k1', '{"v":1,"n":1}'));
        expect((await store.version(RUN, 1))?.snapshotJson).toBe('{"v":1,"n":1}');
        expect(await store.version(RUN, 9)).toBeUndefined();
      });

      it('never modifies a version — a re-render adds one, and the first is byte-identical', async () => {
        await store.declareVersion(render('k1', '{"v":1,"first":true}'));
        await store.declareVersion(render('k2', '{"v":1,"second":true}'));
        expect((await store.version(RUN, 1))?.snapshotJson).toBe('{"v":1,"first":true}');
        expect(await store.versions(RUN)).toHaveLength(2);
      });

      it('stores an error version with no snapshot, and the reverse for an ok one', async () => {
        await store.declareVersion({
          runId: RUN,
          status: 'error',
          templateHash: 'h',
          renderKey: 'bad',
          errorText: 'undefined variable: results',
          renderedBy: 'sweep',
        });
        const v = await store.version(RUN, 1);
        expect(v?.status).toBe('error');
        expect(v?.errorText).toContain('undefined variable');
        // NULL means ABSENT, not `undefined` — the discipline db/repo.ts asserts key-for-key.
        expect('snapshotJson' in (v as object)).toBe(false);
      });

      it('leaves the snapshots out of the version list, because a selector does not need 50 MiB', async () => {
        await store.declareVersion(render('k1', `{"v":1,"pad":"${'x'.repeat(5000)}"}`));
        const list = await store.versions(RUN);
        expect(list).toHaveLength(1);
        expect('snapshotJson' in (list[0] as object)).toBe(false);
        expect(list[0]!.templateHash).toBe('sha256:aaa');
        expect(list[0]!.renderedBy).toBe('sweep');
      });

      it('lists newest first', async () => {
        await store.declareVersion(render('k1'));
        await store.declareVersion(render('k2'));
        expect((await store.versions(RUN)).map((v) => v.version)).toEqual([2, 1]);
      });

      it('answers which runs have a report, in one statement per page', async () => {
        await store.declareVersion(render('k1'));
        const have = await store.haveReports([RUN, OTHER, '']);
        expect(have.has(RUN)).toBe(true);
        expect(have.has(OTHER)).toBe(false);
      });

      it('asks nothing of the store for an empty id list', async () => {
        expect((await store.haveReports([])).size).toBe(0);
      });
    });

    describe('the unredacted bytes', () => {
      it('round-trips a block, keyed by run, version and block id', async () => {
        await store.putSecrets(RUN, 1, [{ blockId: 'b1', rawB64: Buffer.from('Bearer abc').toString('base64') }]);
        const got = await store.secret(RUN, 1, 'b1');
        expect(Buffer.from(got!, 'base64').toString()).toBe('Bearer abc');
      });

      it('does not confuse two versions of the same block id', async () => {
        await store.putSecrets(RUN, 1, [{ blockId: 'b1', rawB64: 'AAA=' }]);
        await store.putSecrets(RUN, 2, [{ blockId: 'b1', rawB64: 'BBB=' }]);
        expect(await store.secret(RUN, 1, 'b1')).toBe('AAA=');
        expect(await store.secret(RUN, 2, 'b1')).toBe('BBB=');
      });

      it('writes nothing for an empty list, and never opens the store for it', async () => {
        await expect(store.putSecrets(RUN, 1, [])).resolves.toBeUndefined();
        expect(await store.secret(RUN, 1, 'b1')).toBeUndefined();
      });
    });

    describe('feedback — ACCEPTANCE 19', () => {
      it('records the author and the kind the caller resolved from the credential', async () => {
        const note = await store.addFeedback({
          runId: RUN,
          workflow: 'enrich',
          author: 'mohamed',
          authorKind: 'user',
          body: 'retry that feed with a longer timeout',
        });
        expect(note.author).toBe('mohamed');
        expect(note.authorKind).toBe('user');
        expect(note.id).toMatch(/^[0-9a-f-]{36}$/);
      });

      it('refuses an empty note and one over the byte cap', async () => {
        await expect(store.addFeedback({ runId: RUN, author: 'a', authorKind: 'user', body: '   ' })).rejects.toThrow(
          InvalidFeedbackError
        );
        await expect(
          store.addFeedback({ runId: RUN, author: 'a', authorKind: 'user', body: 'x'.repeat(FEEDBACK_MAX_BYTES + 1) })
        ).rejects.toThrow(/at most/);
      });

      it('lists newest first, filtered by run and by workflow', async () => {
        await store.addFeedback({ runId: RUN, workflow: 'enrich', author: 'a', authorKind: 'user', body: 'one', at: 1 });
        await store.addFeedback({ runId: RUN, workflow: 'enrich', author: 'b', authorKind: 'token', body: 'two', at: 2 });
        await store.addFeedback({ runId: OTHER, workflow: 'other', author: 'c', authorKind: 'user', body: 'three', at: 3 });
        expect((await store.feedback({ runId: RUN })).map((n) => n.body)).toEqual(['two', 'one']);
        expect((await store.feedback({ workflow: 'other' })).map((n) => n.body)).toEqual(['three']);
      });

      it('lets the author edit, and refuses anybody else — the second lock behind the route 403', async () => {
        const note = await store.addFeedback({ runId: RUN, author: 'mohamed', authorKind: 'user', body: 'first' });
        expect(await store.editFeedback(note.id, 'mohamed', 'second')).toBe(true);
        expect(await store.editFeedback(note.id, 'someone-else', 'hijacked')).toBe(false);
        const after = await store.note(note.id);
        expect(after?.body).toBe('second');
        expect(after?.editedAt).toBeGreaterThan(0);
      });

      it('soft-deletes: the row stays, the listing does not show it, and an edit cannot revive it', async () => {
        const note = await store.addFeedback({ runId: RUN, author: 'mohamed', authorKind: 'user', body: 'oops' });
        expect(await store.deleteFeedback(note.id, 'mohamed')).toBe(true);
        expect(await store.feedback({ runId: RUN })).toHaveLength(0);
        expect((await store.note(note.id))?.deletedAt).toBeGreaterThan(0);
        expect(await store.editFeedback(note.id, 'mohamed', 'back')).toBe(false);
        expect(await store.deleteFeedback(note.id, 'mohamed')).toBe(false);
      });
    });

    describe('purgeRun — ACCEPTANCE 16', () => {
      it('removes a run\'s templates, versions, secrets and feedback', async () => {
        await store.pinTemplate({ runId: RUN, templateHash: 'h', templateText: 't', source: 'workspace' });
        const v = await store.declareVersion({
          runId: RUN,
          status: 'ok',
          templateHash: 'h',
          renderKey: 'k',
          snapshotJson: '{"v":1}',
          renderedBy: 'sweep',
        });
        await store.putSecrets(RUN, v.version, [{ blockId: 'b1', rawB64: 'AAA=' }]);
        await store.addFeedback({ runId: RUN, author: 'a', authorKind: 'user', body: 'note' });

        expect(await store.purgeRun(RUN)).toBe(1);

        expect(await store.template(RUN)).toBeUndefined();
        expect(await store.versions(RUN)).toHaveLength(0);
        expect(await store.secret(RUN, v.version, 'b1')).toBeUndefined();
        expect(await store.feedback({ runId: RUN })).toHaveLength(0);
      });

      it('leaves another run alone', async () => {
        await store.declareVersion({
          runId: OTHER,
          status: 'ok',
          templateHash: 'h',
          renderKey: 'k',
          snapshotJson: '{}',
          renderedBy: 'sweep',
        });
        await store.purgeRun(RUN);
        expect(await store.versions(OTHER)).toHaveLength(1);
      });

      it('is a no-op for a run that left nothing, and says zero', async () => {
        expect(await store.purgeRun('never-ran')).toBe(0);
      });

      it('removes the SECRETS even when it cannot finish — the order is the mitigation', async () => {
        // There is no transaction in this family (see the store's header), so the guarantee this
        // test pins is the ORDER: whatever else survives a half-purge, the unredacted bytes do not.
        await store.declareVersion({
          runId: RUN,
          status: 'ok',
          templateHash: 'h',
          renderKey: 'k',
          snapshotJson: '{}',
          renderedBy: 'sweep',
        });
        await store.putSecrets(RUN, 1, [{ blockId: 'b1', rawB64: 'AAA=' }]);
        const calls: string[] = [];
        const spy = store as unknown as { driver: { run: (sql: string, b?: unknown[]) => Promise<number> } };
        const real = spy.driver.run.bind(spy.driver);
        spy.driver.run = async (sql: string, binds?: unknown[]) => {
          const table = /DELETE FROM (\S+)/.exec(sql)?.[1] ?? sql.slice(0, 20);
          calls.push(table);
          // Fail on the second delete, which is the version table.
          if (calls.length === 2) throw new Error('the database went away mid-purge');
          return real(sql, binds);
        };
        await expect(store.purgeRun(RUN)).rejects.toThrow('went away');
        spy.driver.run = real;
        expect(calls[0], 'the secrets table was not deleted first').toMatch(/report_secret/);
        expect(await store.secret(RUN, 1, 'b1'), 'a credential survived a half purge').toBeUndefined();
      });
    });
  });
}
