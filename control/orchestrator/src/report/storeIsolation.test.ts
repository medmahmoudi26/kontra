/**
 * A REPORT IS ADDRESSED BY ITS WORKSPACE (ADR 0051). The defect this pins: from any workspace the
 * Reports page listed every workspace's reports, because the store was one set of tables and nothing
 * named a workspace. Now each workspace namespace has tables of its own, and the legacy namespace
 * keeps the tables every existing report is already in.
 */
import { afterAll, describe, expect, it } from 'vitest';

import { inNamespace } from '../workspaces';
import { ReportStore, reportScope } from './store';

const backends: Array<{ name: string; make: () => ReportStore }> = [
  { name: 'sqlite', make: () => new ReportStore({ url: ':memory:' }) },
];
if (process.env.KONTRA_TEST_PG) {
  backends.push({
    name: 'postgres',
    make: () => new ReportStore({ url: process.env.KONTRA_TEST_PG!, schema: 'kontra_iso_test' }),
  });
}

const RUN = 'canary-1791999999';

describe('reportScope', () => {
  it('keeps the legacy namespace in the tables existing reports are in', () => {
    const pg = reportScope('default', { schema: 'kontra' });
    expect(pg.table('report_version')).toBe('kontra.report_version');
    const lite = reportScope('default', { schema: null });
    expect(lite.table('report_version')).toBe('report_version');
    expect(lite.index('report_version_key')).toBe('report_version_key');
  });

  it('gives a workspace namespace its own schema, or its own table and index names', () => {
    expect(reportScope('ws-bug-bounty', { schema: 'kontra' }).table('report_version')).toBe(
      'kontra_ws_bug_bounty.report_version'
    );
    const lite = reportScope('ws-bug-bounty', { schema: null });
    expect(lite.table('report_version')).toBe('ws_bug_bounty__report_version');
    // SQLite index names are database-wide, so they are scoped as well.
    expect(lite.index('report_version_key')).toBe('ws_bug_bounty__report_version_key');
  });

  it('treats a renamed legacy namespace as legacy', () => {
    expect(reportScope('kontra', { schema: 'kontra' }).table('x')).toBe('kontra.x');
  });
});

for (const backend of backends) {
  describe(`report isolation (${backend.name})`, () => {
    const store = backend.make();
    const write = (ns: string) =>
      inNamespace(ns, async () => {
        await store.purgeRun(RUN);
        await store.declareVersion({
          runId: RUN,
          status: 'ok',
          templateHash: 'sha256:x',
          renderKey: `k-${ns}`,
          snapshotJson: JSON.stringify({ v: 1, ns }),
          renderedBy: 'sweep',
          at: 1,
        });
        await store.addFeedback({ runId: RUN, author: 'a', authorKind: 'user', body: `from ${ns}` });
      });

    afterAll(async () => {
      for (const ns of ['default', 'ws-alpha', 'ws-beta']) await inNamespace(ns, () => store.purgeRun(RUN));
    });

    it('lists and reads a report only from the workspace that rendered it', async () => {
      for (const ns of ['default', 'ws-alpha', 'ws-beta']) await inNamespace(ns, () => store.purgeRun(RUN));
      await write('ws-alpha');

      const fromAlpha = await inNamespace('ws-alpha', () => store.listReports());
      expect(fromAlpha.map((r) => r.runId)).toContain(RUN);
      expect(await inNamespace('ws-alpha', () => store.version(RUN))).toBeDefined();

      for (const other of ['ws-beta', 'default']) {
        expect((await inNamespace(other, () => store.listReports())).map((r) => r.runId)).not.toContain(RUN);
        expect(await inNamespace(other, () => store.version(RUN))).toBeUndefined();
        expect(await inNamespace(other, () => store.versions(RUN))).toEqual([]);
        expect(await inNamespace(other, () => store.feedback({ runId: RUN }))).toEqual([]);
      }
      expect((await inNamespace('ws-alpha', () => store.feedback({ runId: RUN }))).map((n) => n.body)).toEqual([
        'from ws-alpha',
      ]);
    });

    it('keeps two workspaces apart even when their run ids collide', async () => {
      await write('ws-alpha');
      await write('ws-beta');
      const a = await inNamespace('ws-alpha', () => store.version(RUN));
      const b = await inNamespace('ws-beta', () => store.version(RUN));
      expect(JSON.parse(a!.snapshotJson!).ns).toBe('ws-alpha');
      expect(JSON.parse(b!.snapshotJson!).ns).toBe('ws-beta');
      expect(a!.version).toBe(1);
      expect(b!.version).toBe(1);
    });
  });
}
