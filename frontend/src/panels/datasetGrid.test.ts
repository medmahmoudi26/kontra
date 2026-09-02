/**
 * The results grid's stability, and the saved-query presets — the two halves of issue #14.
 *
 * THE RESET BUG. The app-level catalog poll re-renders the Datasets console every ~2 s while any Run
 * is live. Each such tick used to hand AG Grid a fresh `defaultColDef={{ flex: 1 }}` and rebuild the
 * column definitions off the `result` object, which re-shared column widths and dropped the
 * operator's resize / sort / filter / scroll mid-read. The fix moved the grid's identity onto two
 * things that DO NOT change on a catalog tick: one frozen shared `defaultColDef`, and column defs
 * keyed on the schema SIGNATURE (names + types) rather than on the result object. This suite pins
 * that: a refresh that does not change the schema must not change what the grid is handed.
 *
 * These are pure functions for the reason the other panel suites record — this runs in node with no
 * jsdom and no testing-library, so the grid's React memoization is tested through the values its
 * memos key on, not by mounting AG Grid.
 */

import { describe, expect, it } from 'vitest';
import type { ColDef, ValueGetterParams } from 'ag-grid-community';

import {
  DATASET_LISTING_COLUMNS,
  GRID_DEFAULT_COL_DEF,
  LISTING_DEFAULT_COL_DEF,
  buildColumnDefs,
  loadPresets,
  opensConsole,
  removePreset,
  savePresets,
  schemaSignature,
  tagPrompt,
  upsertPreset,
  visiblePresets,
  type SqlPreset,
} from './DatasetPage';
import { listingRows, type DatasetListingRow } from '../datasets/listing';
import { groupDatasets } from '../datasets/grouped';
import type { DatasetInfo } from '../run/api';

describe('schemaSignature — what the grid rebuilds on', () => {
  const schema = [
    { name: 'domain', type: 'VARCHAR' },
    { name: 'ok', type: 'BOOLEAN' },
  ];

  it('is unchanged when only the rows change', () => {
    // A catalog tick, or a re-run of the same query, returns the same columns with different rows.
    // The signature is what the columnDefs memo keys on, so an unchanged signature is what keeps the
    // operator's column widths, sort and filter across that refresh. This is the regression guard.
    const before = schemaSignature(schema);
    const after = schemaSignature([...schema]); // a fresh result object, identical shape
    expect(after).toBe(before);
  });

  it('changes when a column is added, renamed, or retyped', () => {
    const base = schemaSignature(schema);
    expect(schemaSignature([...schema, { name: 'ns', type: 'VARCHAR' }])).not.toBe(base);
    expect(schemaSignature([{ name: 'host', type: 'VARCHAR' }, schema[1]!])).not.toBe(base);
    expect(schemaSignature([schema[0]!, { name: 'ok', type: 'VARCHAR' }])).not.toBe(base);
  });

  it('does not collide two schemas onto one signature', () => {
    // A printable separator would let `{a, b_c}` read the same as `{a_b, c}` and freeze the grid on
    // the wrong columns; a DuckDB type carries spaces and parens (`DECIMAL(10, 2)`), so the join
    // uses control characters no identifier or type can contain.
    const a = schemaSignature([{ name: 'a', type: 'INT b' }]);
    const b = schemaSignature([{ name: 'a', type: 'INT' }, { name: 'b', type: '' }]);
    expect(a).not.toBe(b);
  });
});

describe('the grid defaults do not re-share widths on a re-render', () => {
  it('are one frozen shared object, not a per-render literal', () => {
    // A new object each render — even `{ flex: 1 }` with identical contents — counts as a changed
    // prop and makes AG Grid re-apply flex, which is exactly the reset. Freezing makes the
    // re-sharing intent impossible to reintroduce by mutation, and the reference is module-shared.
    expect(Object.isFrozen(GRID_DEFAULT_COL_DEF)).toBe(true);
    expect(GRID_DEFAULT_COL_DEF).toBe(GRID_DEFAULT_COL_DEF);
  });

  it('keeps flex in the shared default, never per column', () => {
    // Per-column flex would resize on every columnDefs rebuild; width sharing belongs to the one
    // stable default the grid keeps for its whole life.
    const defs = buildColumnDefs([{ name: 'domain', type: 'VARCHAR' }], false);
    expect(defs).toHaveLength(1);
    expect(defs[0]).not.toHaveProperty('flex');
    expect(defs[0]!.field).toBe('domain');
    expect(defs[0]!.minWidth).toBe(120);
  });

  it('wraps only in tall mode', () => {
    const dense = buildColumnDefs([{ name: 'body', type: 'VARCHAR' }], false);
    const tall = buildColumnDefs([{ name: 'body', type: 'VARCHAR' }], true);
    expect(dense[0]).not.toHaveProperty('wrapText');
    expect(tall[0]!.wrapText).toBe(true);
  });
});

/**
 * THE LISTING GRID — the same reset bug, one surface over.
 *
 * The listing is polled every ~2 s by the app-level catalog fetch, and its rows legitimately MOVE
 * while a Run writes. So it needs the #14 discipline plus one more thing the results grid does not:
 * a stable row id, or AG Grid replaces the whole row set on each tick and takes the scroll with it.
 */
describe('the listing grid cannot be rebuilt by a catalog poll', () => {
  it('is ONE frozen module constant, not a value a render can rebuild', () => {
    // The results grid memoises on a schema signature because its columns are the query's; the
    // listing's columns are fixed by the source file, so they can be stronger than a memo — nothing
    // rebuilds them at all. A new array, even with identical contents, re-applies every width.
    expect(Object.isFrozen(DATASET_LISTING_COLUMNS)).toBe(true);
    expect(DATASET_LISTING_COLUMNS).toBe(DATASET_LISTING_COLUMNS);
  });

  it('carries every fact the listing knows, each as its own column', () => {
    // Named one by one because each is load-bearing and a silent drop is the failure mode: the
    // Dataset, the run-grain name, the tags, the two axes (kind, temporary), the lifecycle, the
    // live tail, the numbers, the partition, the Run the record is keyed on, and the write time.
    //
    // THE ORDER IS PART OF THE CONTRACT. Seventeen columns do not fit a laptop, so AG Grid virtualises
    // the ones off-screen right — measured at 1600px, the last four are not rendered at all. `tags`
    // sat there in the first cut, which made the affordance this work exists for unreachable without
    // a horizontal scroll. It is third now, and the pinned `dataset` and the pinned `actions` keep
    // the identity and the delete visible however far right the operator scrolls.
    //
    // `expires` is FOURTH, immediately after `tags`, for the same reason: it is the consequence of
    // not tagging, the sweep that acts on it is armed and hourly, and a retention warning nobody can
    // see without scrolling is a warning that arrives after the delete.
    expect(DATASET_LISTING_COLUMNS.map((c) => c.colId)).toEqual([
      'dataset',
      'name',
      'tags',
      'expires',
      'state',
      'temporary',
      'kind',
      'rows',
      'bytes',
      'rate',
      'live',
      'run',
      'version',
      'dt',
      'columns',
      'updatedAt',
      'actions',
    ]);
    expect(DATASET_LISTING_COLUMNS[0]!.pinned).toBe('left');
    expect(DATASET_LISTING_COLUMNS.at(-1)!.pinned).toBe('right');
  });

  it('keeps the shared defaults frozen and free of flex', () => {
    // `flex` in the shared default is what re-shared every column's width on each poll in #14. The
    // listing's two elastic columns declare it themselves, inside the frozen defs, so it is applied
    // once rather than re-applied per render.
    expect(Object.isFrozen(LISTING_DEFAULT_COL_DEF)).toBe(true);
    expect(LISTING_DEFAULT_COL_DEF).not.toHaveProperty('flex');
    expect(LISTING_DEFAULT_COL_DEF.sortable).toBe(true);
    expect(LISTING_DEFAULT_COL_DEF.resizable).toBe(true);
    // No type inference, for the reason the results grid records: AG Grid gives a boolean column a
    // checkbox renderer, and `temporary` would draw tick boxes instead of a badge.
    expect(LISTING_DEFAULT_COL_DEF.cellDataType).toBe(false);
  });
});

describe('the listing columns hold VALUES, so a sort means what it says', () => {
  const AT = 1_786_895_804_639;
  const DERIVED = 'wf-nscheck-0.1.0--2026-08-19T14-49-20Z--5dd94a';
  const ds = (over: Partial<DatasetInfo> & Pick<DatasetInfo, 'name'>): DatasetInfo =>
    ({
      kind: 'output',
      version: '0.1.0',
      dt: '2026-08-19T14-49-20',
      rows: 623,
      bytes: 38_777,
      updatedAt: AT,
      ...over,
    }) as DatasetInfo;
  const row = (...catalog: DatasetInfo[]): DatasetListingRow =>
    listingRows(groupDatasets(catalog, AT), { series: {}, columns: null })[0]!;
  const col = (id: string): ColDef<DatasetListingRow> =>
    DATASET_LISTING_COLUMNS.find((c) => c.colId === id)!;
  /** What AG Grid would sort and filter this column on, for one row. */
  const value = (id: string, data: DatasetListingRow): unknown => {
    const def = col(id);
    const getter = def.valueGetter;
    if (typeof getter === 'function') {
      return getter({ data } as unknown as ValueGetterParams<DatasetListingRow>);
    }
    return (data as unknown as Record<string, unknown>)[String(def.field)];
  };

  it('sorts the name on the SERVER’S UTC string, never on the drawn local one', () => {
    // ADR 0029 §2: the identity is UTC (two controllers), "the UI renders local". Sorting the drawn
    // string would order one operator's list differently from another's, for the same Datasets.
    const r = row(ds({ name: 'lame_demo', runId: 'nscheck-1', datasetName: DERIVED }));
    expect(value('name', r)).toBe(DERIVED);
    expect(r.nameLocal).not.toBe(DERIVED); // the cell draws something else
  });

  it('holds the SINGULAR run only where a singular answer exists', () => {
    const one = row(ds({ name: 'lame_demo', runId: 'nscheck-1', contributingRuns: ['nscheck-1'] }));
    expect(value('run', one)).toBe('nscheck-1');
    const several = row(
      ds({ name: 'lame', dt: '2026-08-19T14-49-20', contributingRuns: ['r1', 'r2'] }),
      ds({ name: 'lame', dt: '2026-08-16T15-52-51', contributingRuns: ['r3'] })
    );
    expect(value('run', several)).toBe('');
    expect(several.runs).toEqual(['r1', 'r2', 'r3']);
  });

  it('does NOT sort on the record key, which is filled even for a five-Run Dataset', () => {
    // The bug this pins was found in a browser, not here: the column read `runId` — the key a tag
    // addresses, which is the NEWEST of the five Runs that wrote `lame` — and so displayed and
    // sorted one of five under a heading that says "run".
    const lame = row(
      ds({ name: 'lame', dt: '2026-08-16T17-37-53', runId: 'nscheck-3', contributingRuns: ['nscheck-3'] }),
      ds({ name: 'lame', dt: '2026-08-15T18-37-25', runId: 'nscheck-1', contributingRuns: ['nscheck-1'] })
    );
    expect(lame.runId).toBe('nscheck-3'); // the key is there, for the tag affordance
    expect(value('run', lame)).toBe(''); // and the column refuses to call it the answer
  });

  it('sorts rows, size and write time as numbers', () => {
    const r = row(ds({ name: 'lame', rows: 1234, bytes: 4_718_592 }));
    expect(value('rows', r)).toBe(1234);
    expect(value('bytes', r)).toBe(4_718_592);
    expect(value('updatedAt', r)).toBe(AT);
    // ...and DRAWS them for a human, which is the split that lets 512 KB sort below 4.5 MB.
    expect(col('bytes').valueFormatter).toBeTypeOf('function');
  });

  it('makes the tag set findable by the column filter', () => {
    const r = row(ds({ name: 'lame_demo', runId: 'nscheck-1', tags: ['keep-this', 'prod'] }));
    expect(value('tags', r)).toBe('keep-this prod');
  });

  it('reads temporary as a word, so the column filters and sorts instead of drawing tick boxes', () => {
    expect(value('temporary', row(ds({ name: 'tmp_x', temporary: true, owner: 'r' })))).toBe('temporary');
    expect(value('temporary', row(ds({ name: 'lame' })))).toBe('durable');
  });

  it('keeps the actions column out of the sort and filter entirely', () => {
    expect(col('actions').sortable).toBe(false);
    expect(col('actions').filter).toBe(false);
  });
});

describe('a click on a row action is not a click on the row', () => {
  // AG Grid listens on the ROW element, which is an ancestor of the button and a descendant of
  // React's delegated root — so the grid's open-the-console handler runs BEFORE any React
  // `stopPropagation` could stop it. The decision has to be made from the click target.
  const target = (isAction: boolean) => ({ closest: (sel: string) => (isAction && sel === '[data-row-action]' ? {} : null) });

  it('opens the console for an ordinary cell click', () => {
    expect(opensConsole(target(false))).toBe(true);
  });

  it('does NOT open the console when the delete, tag or rename button was clicked', () => {
    expect(opensConsole(target(true))).toBe(false);
  });

  it('opens rather than swallowing the click when there is no element to ask', () => {
    expect(opensConsole(null)).toBe(true);
    expect(opensConsole(undefined)).toBe(true);
  });
});

describe('the tag prompt says what the tag will key on', () => {
  it('warns that a temp’s tag is the RUN’s, so it marks the durable output too', () => {
    // Measured on the live controller: `tmp_4e9b1b23` and `lame_demo` carry the same runId, and the
    // record is keyed by the Run (ADR 0029 §4). Tagging the temp therefore tags both — which is
    // usually what an operator means, and never what "tag this table" looks like.
    const msg = tagPrompt({ name: 'tmp_4e9b1b23', runId: 'nscheck-1', temporary: true });
    expect(msg).toContain('nscheck-1');
    expect(msg).toContain('everything else that Run wrote');
    expect(msg).toContain('never swept on a clock');
    expect(msg).toContain('delete button still removes it');
  });

  it('says plainly what a tag does on a durable Dataset', () => {
    const msg = tagPrompt({ name: 'lame', runId: 'nscheck-1', temporary: false });
    expect(msg).toContain('retention TTL');
    expect(msg).not.toContain('delete button');
  });
});

describe('saved SQL presets', () => {
  const lame = 'SELECT * FROM lame WHERE ok;';
  const domains = 'SELECT count(*) FROM domains;';

  it('offers a dataset its own presets plus the global ones, and hides another dataset’s', () => {
    const all: SqlPreset[] = [
      { name: 'live only', sql: lame, dataset: 'lame' },
      { name: 'row count', sql: 'SELECT count(*)' }, // global — no dataset
      { name: 'apexes', sql: domains, dataset: 'domains' },
    ];
    const onLame = visiblePresets(all, 'lame').map((p) => p.name);
    expect(onLame).toContain('live only');
    expect(onLame).toContain('row count');
    expect(onLame).not.toContain('apexes');
  });

  it('overwrites by (name, dataset) so re-saving a name does not duplicate it', () => {
    let all: SqlPreset[] = [];
    all = upsertPreset(all, { name: 'live', sql: 'v1', dataset: 'lame' });
    all = upsertPreset(all, { name: 'live', sql: 'v2', dataset: 'lame' });
    const live = all.filter((p) => p.name === 'live' && p.dataset === 'lame');
    expect(live).toHaveLength(1);
    expect(live[0]!.sql).toBe('v2');
  });

  it('treats the same name on two datasets as two presets', () => {
    let all: SqlPreset[] = [];
    all = upsertPreset(all, { name: 'summary', sql: lame, dataset: 'lame' });
    all = upsertPreset(all, { name: 'summary', sql: domains, dataset: 'domains' });
    expect(all).toHaveLength(2);
  });

  it('deletes one and leaves every other one — applying or deleting must not disturb the rest', () => {
    const all: SqlPreset[] = [
      { name: 'a', sql: '1', dataset: 'lame' },
      { name: 'b', sql: '2', dataset: 'lame' },
    ];
    const after = removePreset(all, { name: 'a', dataset: 'lame' });
    expect(after.map((p) => p.name)).toEqual(['b']);
  });
});

describe('preset storage', () => {
  it('degrades to no presets on a browser with no storage, rather than throwing', () => {
    // node has no localStorage of its own; opening a console must not crash where a preset list
    // cannot be read.
    const saved = globalThis.localStorage;
    // @ts-expect-error — removing the optional global for the duration of the assertion
    delete globalThis.localStorage;
    expect(loadPresets()).toEqual([]);
    if (saved) globalThis.localStorage = saved;
  });

  it('round-trips through a storage that is present, and drops a corrupt value', () => {
    const store = new Map<string, string>();
    const mock = {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => void store.set(k, v),
      removeItem: (k: string) => void store.delete(k),
      clear: () => store.clear(),
      key: () => null,
      length: 0,
    } as unknown as Storage;
    const saved = globalThis.localStorage;
    globalThis.localStorage = mock;

    savePresets([{ name: 'keep', sql: 'SELECT 1', dataset: 'lame' }]);
    expect(loadPresets()).toEqual([{ name: 'keep', sql: 'SELECT 1', dataset: 'lame' }]);

    // A garbled or foreign value is "no presets", never a throw on the way to opening a console.
    store.set('kontra.dataset.presets', '{not json');
    expect(loadPresets()).toEqual([]);
    store.set('kontra.dataset.presets', JSON.stringify([{ name: 'x' }, 42, { sql: 'no name' }]));
    expect(loadPresets()).toEqual([]);

    if (saved) globalThis.localStorage = saved;
    else {
      // @ts-expect-error — restore the absent global
      delete globalThis.localStorage;
    }
  });
});
