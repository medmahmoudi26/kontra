/**
 * The cross-workspace SHARE grant (ADR 0053): the key is the ADDRESS, the shape is PRESENCE, and
 * only deviation is stored — an unshared Dataset has no row.
 *
 * Runs against SQLite by default; set `KONTRA_TEST_PG` to a Postgres URL and the same suite runs
 * against Postgres too, so a dialect difference in `ON CONFLICT` cannot hide behind "it passed
 * locally" — the same discipline `datasetRecords.test.ts` follows, and for the same reason: this
 * store's idempotence claim IS an `ON CONFLICT` clause.
 *
 * THE TESTS THAT MATTER MOST HERE ARE THE ONES ABOUT THE KEY. A grant stored under the wrong
 * workspace, or shared between two kinds of Dataset that happen to have one name, is an exposure
 * nobody asked for — so the address is pinned field by field rather than as a whole.
 */

import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterAll, beforeEach, describe, expect, it } from 'vitest';

import { WorkspaceRefused } from '../workspaces';
import {
  InvalidShareError,
  SHARED_NAME_MAX_LENGTH,
  SharedDatasetStore,
  normalizeSharedName,
  sharedKind,
} from './sharedDatasets';

describe('normalizeSharedName', () => {
  it('trims, so leading space never mints a second address for one table', () => {
    expect(normalizeSharedName('  lame ')).toBe('lame');
  });

  it('refuses a name that is not already its own safeName', () => {
    // The point of the rule: every writer names its table through `safeName`, so a name this
    // changes is a name no table has — and a grant that stored it would name one string while the
    // read path interpolated another.
    expect(() => normalizeSharedName('scope paid')).toThrow(InvalidShareError);
    expect(() => normalizeSharedName('lame"; DROP TABLE x --')).toThrow(InvalidShareError);
    expect(() => normalizeSharedName('output/lame')).toThrow(InvalidShareError);
    expect(() => normalizeSharedName('9lives')).toThrow(InvalidShareError);
  });

  it('accepts the names this lake really holds', () => {
    for (const n of ['lame', 'scope_h1paid', 'crawl4ai', 'exchanges_v2', 'a.b-c']) {
      expect(normalizeSharedName(n)).toBe(n);
    }
  });

  it('refuses empty and over-long with a typed error a route maps to 400', () => {
    expect(() => normalizeSharedName('   ')).toThrow(InvalidShareError);
    expect(() => normalizeSharedName('x'.repeat(SHARED_NAME_MAX_LENGTH + 1))).toThrow(
      InvalidShareError
    );
  });
});

describe('sharedKind', () => {
  it('coerces to output unless standalone was asked for — one spelling reaches the lake', () => {
    expect(sharedKind('standalone')).toBe('standalone');
    expect(sharedKind('output')).toBe('output');
    expect(sharedKind(undefined)).toBe('output');
    expect(sharedKind('OUTPUT')).toBe('output');
  });
});

// A FILE, not `:memory:`, so two stores reach the same rows — the console's process and the read
// path's are two readers of one grant, and that is the property worth being able to test.
const sqliteDir = mkdtempSync(join(tmpdir(), 'kontra-shared-'));

const backends: Array<{ name: string; make: () => SharedDatasetStore }> = [
  { name: 'sqlite', make: () => new SharedDatasetStore({ url: join(sqliteDir, 'shares.db') }) },
];
if (process.env.KONTRA_TEST_PG) {
  backends.push({
    name: 'postgres',
    make: () => new SharedDatasetStore({ url: process.env.KONTRA_TEST_PG!, schema: 'kontra_test' }),
  });
}

afterAll(() => rmSync(sqliteDir, { recursive: true, force: true }));

for (const backend of backends) {
  describe(`SharedDatasetStore (${backend.name})`, () => {
    let store: SharedDatasetStore;

    beforeEach(async () => {
      store = backend.make();
      await store.ensureSchema();
      for (const ws of ['bugbounty', 'scraping', 'demo']) await store.purgeWorkspace(ws);
    });

    afterAll(async () => {
      await store?.close().catch(() => undefined);
    });

    it('has NO row for an unshared Dataset — only deviation is stored', async () => {
      expect(await store.get('bugbounty', 'lame', 'output')).toBeUndefined();
      expect(await store.list()).toEqual([]);
    });

    it('shares, and the grant carries the whole address', async () => {
      const grant = await store.share('bugbounty', 'lame', 'output');
      expect(grant).toMatchObject({ workspace: 'bugbounty', name: 'lame', kind: 'output' });
      expect(grant.sharedAt).toBeGreaterThan(0);
      expect(await store.get('bugbounty', 'lame', 'output')).toMatchObject({ name: 'lame' });
    });

    it('unshares back to NO ROW, not to a false column', async () => {
      await store.share('bugbounty', 'lame', 'output');
      expect(await store.unshare('bugbounty', 'lame', 'output')).toBe(true);
      expect(await store.get('bugbounty', 'lame', 'output')).toBeUndefined();
      // Revoking what is not there is success, and `removed` is what tells the two apart.
      expect(await store.unshare('bugbounty', 'lame', 'output')).toBe(false);
    });

    it('is idempotent, and a repeat grant keeps the FIRST sharedAt', async () => {
      // The audit answer to "since when has this been exposed" must not be "since you last asked".
      const first = await store.share('bugbounty', 'lame', 'output');
      await new Promise((r) => setTimeout(r, 2));
      const again = await store.share('bugbounty', 'lame', 'output');
      expect(again.sharedAt).toBe(first.sharedAt);
      expect((await store.list()).length).toBe(1);
    });

    it('two writers granting the same Dataset converge on one row', async () => {
      // Presence, not a scalar: concurrent grants cannot clobber each other because they insert the
      // same primary key. (`node:sqlite` is synchronous, so this proves the rows land; the race
      // itself is only real on Postgres — which is why that backend runs the same suite.)
      const a = backend.make();
      const b = backend.make();
      await Promise.all([
        a.share('bugbounty', 'lame', 'output'),
        b.share('bugbounty', 'lame', 'output'),
      ]);
      expect((await store.list('bugbounty')).length).toBe(1);
      await a.close().catch(() => undefined);
      await b.close().catch(() => undefined);
    });

    it('KEYS ON THE WORKSPACE: one workspace sharing a name does not share another workspace’s', async () => {
      // The one mistake this key exists to prevent. Two workspaces hold a Dataset called `lame`;
      // granting one must not expose the other, and no WHERE clause decides that — the row does.
      await store.share('bugbounty', 'lame', 'output');
      expect(await store.get('scraping', 'lame', 'output')).toBeUndefined();
      expect(await store.listFor('scraping', [{ kind: 'output', name: 'lame' }])).toEqual([]);
    });

    it('KEYS ON THE KIND: sharing a standalone list does not share an output table of that name', async () => {
      // An output Dataset and a loaded list live in different schemas and can share a name — the
      // console's own row identity is `kind:name` for exactly this reason.
      await store.share('bugbounty', 'scope_h1paid', 'standalone');
      expect(await store.get('bugbounty', 'scope_h1paid', 'standalone')).toBeDefined();
      expect(await store.get('bugbounty', 'scope_h1paid', 'output')).toBeUndefined();
    });

    it('refuses a workspace name the read path could not turn into an address', async () => {
      // `assertWorkspaceName` is the SAME validator `workspaceAddress` uses, so a grant can never
      // be stored under a name no reader could resolve — an exposure on the screen that nothing can
      // use is a lie in the other direction.
      await expect(store.share('Client_A', 'lame', 'output')).rejects.toThrow(WorkspaceRefused);
      await expect(store.share('', 'lame', 'output')).rejects.toThrow(WorkspaceRefused);
    });

    it('lists ACROSS workspaces, ordered, and narrows to one owner', async () => {
      await store.share('scraping', 'pages', 'output');
      await store.share('bugbounty', 'lame', 'output');
      await store.share('bugbounty', 'scope_h1paid', 'standalone');
      const all = await store.list();
      expect(all.map((g) => `${g.workspace}/${g.kind}/${g.name}`)).toEqual([
        'bugbounty/output/lame',
        'bugbounty/standalone/scope_h1paid',
        'scraping/output/pages',
      ]);
      expect((await store.list('bugbounty')).length).toBe(2);
    });

    it('listFor answers only the pairs asked about, in one workspace', async () => {
      await store.share('bugbounty', 'lame', 'output');
      await store.share('bugbounty', 'other', 'output');
      await store.share('scraping', 'lame', 'output');
      const got = await store.listFor('bugbounty', [
        { kind: 'output', name: 'lame' },
        { kind: 'output', name: 'absent' },
        { kind: 'standalone', name: 'lame' },
      ]);
      expect(got.map((g) => g.name)).toEqual(['lame']);
      expect(got[0]!.workspace).toBe('bugbounty');
    });

    it('listFor pages past the bind cap rather than raising `too many SQL variables`', async () => {
      // The failure `DatasetRecordStore.list` documents: a listing sized by the lake overruns one
      // statement's bind budget, the route swallows the error, and the symptom is Datasets quietly
      // losing their badge. Two binds per pair, so 1,200 pairs is well past a single page.
      await store.share('bugbounty', 'lame', 'output');
      const names = Array.from({ length: 1200 }, (_, i) => ({
        kind: 'output' as const,
        name: `t${i}`,
      }));
      const got = await store.listFor('bugbounty', [...names, { kind: 'output', name: 'lame' }]);
      expect(got.map((g) => g.name)).toEqual(['lame']);
    });

    it('listFor is empty for an unnamed workspace — the legacy address has no grants', async () => {
      // `activeLakeWorkspace()` answers `''` for an install with no `.current` and no
      // KONTRA_LAKE_WORKSPACE. There is no workspace to key a grant on, so there is nothing to
      // join, and no query is issued.
      await store.share('bugbounty', 'lame', 'output');
      expect(await store.listFor('', [{ kind: 'output', name: 'lame' }])).toEqual([]);
    });

    it('purgeWorkspace drops every grant that workspace owns and nothing else', async () => {
      await store.share('bugbounty', 'lame', 'output');
      await store.share('bugbounty', 'scope_h1paid', 'standalone');
      await store.share('scraping', 'pages', 'output');
      expect(await store.purgeWorkspace('bugbounty')).toBe(2);
      expect((await store.list()).map((g) => g.workspace)).toEqual(['scraping']);
    });
  });
}
