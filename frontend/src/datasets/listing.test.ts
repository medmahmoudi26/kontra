/**
 * The listing grid's ROW MODEL — the values every column sorts, filters and draws from.
 *
 * These are pure functions for the reason the other panel suites record: this runs in node with no
 * jsdom and no testing-library, so the grid is tested through the values it is handed rather than by
 * mounting AG Grid. What is pinned here is what a column MEANS; `panels/datasetGrid.test.ts` pins
 * that the columns themselves cannot be rebuilt by a poll.
 */

import { describe, expect, it } from 'vitest';

import type { DatasetInfo } from '../run/api';
import { groupDatasets } from './grouped';
import { dispatchCell, listingRowId, listingRows, tagMeaning } from './listing';

const AT = 1_786_895_804_639;
/** A run started at 14:49:20 UTC — the instant the derived name carries. */
const DERIVED = 'wf-nscheck-0.1.0--2026-08-19T14-49-20Z--5dd94a';

function ds(over: Partial<DatasetInfo> & Pick<DatasetInfo, 'name'>): DatasetInfo {
  return {
    kind: 'output',
    version: '0.1.0',
    dt: '2026-08-19T14-49-20',
    rows: 623,
    bytes: 38_777,
    updatedAt: AT,
    ...over,
  } as DatasetInfo;
}

const NO_INPUT = { series: {}, columns: null };

function rowsOf(catalog: DatasetInfo[], input: Partial<Parameters<typeof listingRows>[1]> = {}) {
  return listingRows(groupDatasets(catalog, AT), { ...NO_INPUT, ...input });
}

describe('the name column: stored UTC, drawn local, SORTED on the stored string', () => {
  const catalog = [ds({ name: 'lame_demo', runId: 'nscheck-1', datasetName: DERIVED, state: 'sealed' })];

  it('carries the canonical name as the value and the localised one as the text', () => {
    // ADR 0029 §2 fixes both halves: the identity is UTC (two controllers mint names), and "the UI
    // renders local". They live in two fields so the column can obey both at once.
    const [row] = rowsOf(catalog, { name: { locale: 'en-GB', timeZone: 'Europe/Paris' } });
    expect(row!.name).toBe(DERIVED);
    expect(row!.nameLocal).toContain('19 Aug 2026 16:49:20');
    expect(row!.nameLocal).not.toBe(row!.name);
    expect(row!.localized).toBe(true);
  });

  it('sorts identically for two viewers in two zones', () => {
    // The whole reason the sort value is the canonical string. Two Datasets a minute apart must come
    // out in one order everywhere; sorting the DRAWN text sorts a locale's month spelling.
    const catalog2 = [
      ds({ name: 'a', runId: 'r1', datasetName: 'wf-x-0.1.0--2026-08-19T14-49-20Z--aaa111' }),
      ds({ name: 'b', runId: 'r2', datasetName: 'wf-x-0.1.0--2026-08-19T14-50-20Z--bbb222' }),
    ];
    const order = (timeZone: string) =>
      rowsOf(catalog2, { name: { locale: 'en-GB', timeZone } })
        .slice()
        .sort((x, y) => x.name.localeCompare(y.name))
        .map((r) => r.dataset);
    expect(order('Europe/Paris')).toEqual(['a', 'b']);
    expect(order('Pacific/Auckland')).toEqual(order('Europe/Paris'));
  });

  it('lets a RENAME win over the derived default, and leaves it un-zoned', () => {
    // A rename is the operator's own words (§4) — no instant to localise, and nothing to re-render.
    const [row] = rowsOf([
      ds({ name: 'lame_demo', runId: 'nscheck-1', datasetName: DERIVED, renamedTo: 'the sweep' }),
    ]);
    expect(row!.name).toBe('the sweep');
    expect(row!.nameLocal).toBe('the sweep');
    expect(row!.renamed).toBe(true);
    expect(row!.localized).toBe(false);
  });

  it('is empty — not guessed — when no dispatch resolved a Run to name', () => {
    const [row] = rowsOf([ds({ kind: 'standalone', name: 'seeds', version: undefined, dt: undefined })]);
    expect(row!.name).toBe('');
    expect(row!.nameLocal).toBe('');
  });
});

describe('the numeric columns are numbers, so they sort as sizes and not as text', () => {
  it('keeps rows, bytes, the rate and the write time as numbers', () => {
    const [row] = rowsOf([ds({ name: 'lame', rows: 1234, bytes: 4_718_592 })], {
      series: { 'output:lame:0.1.0:2026-08-19T14-49-20': [1, 2, 7] },
    });
    expect(row!.rows).toBe(1234);
    expect(row!.bytes).toBe(4_718_592);
    expect(row!.updatedAt).toBe(AT);
    expect(row!.rate).toEqual([1, 2, 7]);
    // The newest sample is what the column sorts on: "which Dataset is being written to fastest".
    expect(row!.rateNow).toBe(7);
  });

  it('sorts 512 KB below 4.5 MB — the thing a formatted string gets wrong', () => {
    const sizes = rowsOf([ds({ name: 'big', bytes: 4_718_592 }), ds({ name: 'small', bytes: 524_288 })])
      .slice()
      .sort((a, b) => a.bytes - b.bytes)
      .map((r) => r.dataset);
    expect(sizes).toEqual(['small', 'big']);
  });

  it('reports no rate for a Dataset nothing has measured', () => {
    const [row] = rowsOf([ds({ name: 'lame' })]);
    expect(row!.rate).toEqual([]);
    expect(row!.rateNow).toBe(0);
  });
});

describe('the run column refuses the singular where it would be a lie', () => {
  it('names the one Run when one wrote the partition', () => {
    const [row] = rowsOf([ds({ name: 'lame_demo', runId: 'nscheck-1', contributingRuns: ['nscheck-1'] })]);
    expect(row!.runId).toBe('nscheck-1');
    expect(row!.soleRun).toBe('nscheck-1');
    expect(row!.runs).toEqual(['nscheck-1']);
  });

  /**
   * THE REGRESSION THIS TEST EXISTS FOR, caught in a browser against the live controller rather
   * than here: `lame` is FIVE Runs' output, one Run per dispatch, so every dispatch row carries a
   * perfectly good singular `runId` and the group's is the newest of the five. A `run` column
   * reading that field showed `nscheck-1786901873` under a heading that says "run" — the first of
   * five dressed up as the one, which is exactly what ADR 0029 §2's plural exists to prevent.
   *
   * The KEY is a different fact and survives: a tag still has to address one record, and it does.
   */
  it('counts, and does not name, when each of several dispatches has its own Run', () => {
    const row = rowsOf([
      ds({ name: 'lame', dt: '2026-08-16T17-37-53', runId: 'nscheck-3', contributingRuns: ['nscheck-3'] }),
      ds({ name: 'lame', dt: '2026-08-16T15-52-51', runId: 'nscheck-2', contributingRuns: ['nscheck-2'] }),
      ds({ name: 'lame', dt: '2026-08-15T18-37-25', runId: 'nscheck-1', contributingRuns: ['nscheck-1'] }),
    ])[0]!;
    expect(row.soleRun).toBe('');
    expect(row.runs).toEqual(['nscheck-1', 'nscheck-2', 'nscheck-3']);
    // The record key is still the newest — the tag affordance has one record to act on, and the
    // prompt names it. Three facts, three fields (ADR 0017).
    expect(row.runId).toBe('nscheck-3');
  });

  it('names a Run the LEDGER resolved even when no row carries one', () => {
    // Output written before `run_id` was stamped on rows has an empty `contributingRuns` and a
    // perfectly good ledger-resolved `runId`. Counting only the lake's statistics would report a
    // Dataset with no Run at all.
    const [row] = rowsOf([ds({ name: 'echo', runId: 'run-abc' })]);
    expect(row!.runs).toEqual(['run-abc']);
    expect(row!.soleRun).toBe('run-abc');
  });

  it('leaves the singular EMPTY for a partition several Runs contributed rows to', () => {
    // ADR 0029 §2 / ADR 0017: the plural fact and the singular one never contradict each other,
    // because the singular exists only where a singular answer does. The empty sort value is the
    // honest ordering — a Dataset no single Run owns has no id to be ordered by.
    const [row] = rowsOf([
      ds({ name: 'lame', dt: '2026-08-19T14-49-20', contributingRuns: ['r1', 'r2'] }),
      ds({ name: 'lame', dt: '2026-08-16T15-52-51', contributingRuns: ['r3'] }),
    ]);
    expect(row!.soleRun).toBe('');
    expect(row!.runId).toBe(''); // no dispatch resolved one either, so there is no record key
    expect(row!.runs).toEqual(['r1', 'r2', 'r3']);
  });

  it('carries the lower-bound flag when a data file spans Runs', () => {
    const [row] = rowsOf([ds({ name: 'lame', contributingRuns: ['r1', 'r2'], contributingRunsPartial: true })]);
    expect(row!.runsPartial).toBe(true);
  });
});

describe('a temporary Dataset', () => {
  const temp = ds({
    name: 'tmp_4e9b1b23',
    temporary: true,
    owner: 'nscheck-1',
    runId: 'nscheck-1',
    state: 'sealed',
    tags: ['keep-this'],
  });

  it('is marked temporary, names its owner, and IS addressable for a tag', () => {
    // The record is keyed by the Run (ADR 0029 §4) and a temp has exactly one — its owner — so the
    // tag affordance has a key to act on. This is the fact the page's tag button depends on.
    const [row] = rowsOf([temp]);
    expect(row!.temporary).toBe(true);
    expect(row!.owner).toBe('nscheck-1');
    expect(row!.runId).toBe('nscheck-1');
    expect(row!.tags).toEqual(['keep-this']);
  });

  it('says what a tag means HERE, which is not what it means on a durable Dataset', () => {
    // The two landed decisions meeting: §3 "tagged output is kept", ADR 0028 "a temp is staging,
    // owned, explicitly deletable". A temp has no clock to extend and the delete button still
    // works, so the affordance must not promise a protection it does not give.
    const temporary = tagMeaning({ temporary: true });
    expect(temporary).toContain('never swept');
    expect(temporary).toContain('delete button still removes it');
    expect(temporary).toContain('DURABLE output of the same Run');

    const durable = tagMeaning({ temporary: false });
    expect(durable).toContain('untagged output expires');
    expect(durable).not.toContain('delete button');
  });
});

describe('the row identity is stable across polls', () => {
  it('keeps one id while rows, bytes and lifecycle move', () => {
    // What `getRowId` needs. Without it AG Grid replaces the row set on every 2 s catalog tick and
    // the operator's scroll and selection go with it — the listing's half of issue #14.
    const [first] = rowsOf([ds({ name: 'lame', rows: 10, state: 'open' })]);
    const [later] = rowsOf([ds({ name: 'lame', rows: 4000, bytes: 99, state: 'sealed' })]);
    expect(later!.id).toBe(first!.id);
    expect(listingRowId(later!)).toBe(first!.id);
  });

  it('separates an output Dataset from a standalone list of the same name', () => {
    const rows = rowsOf([ds({ name: 'seeds' }), ds({ kind: 'standalone', name: 'seeds', version: undefined, dt: undefined })]);
    expect(new Set(rows.map((r) => r.id)).size).toBe(2);
  });

  it('holds the live tail on the RUN, never on a moving count', () => {
    // A cell value that moved every two seconds would have the grid refresh the cell every two
    // seconds — tearing down the SSE subscription and re-opening it on each catalog poll.
    const open = (rows: number) => rowsOf([ds({ name: 'lame', rows, state: 'open', runId: 'nscheck-1' })])[0]!;
    expect(open(10).live).toBe('nscheck-1');
    expect(open(9999).live).toBe(open(10).live);
    // A sealed Dataset has nothing to stream, and neither has one with no Run to address.
    expect(rowsOf([ds({ name: 'lame', state: 'sealed', runId: 'r' })])[0]!.live).toBe('');
    expect(rowsOf([ds({ name: 'lame', state: 'open' })])[0]!.live).toBe('');
  });
});

describe('the dispatch cell states the plurality the grain hides', () => {
  it('shows the newest partition and counts the rest', () => {
    const [row] = rowsOf([
      ds({ name: 'lame', dt: '2026-08-19T14-49-20' }),
      ds({ name: 'lame', dt: '2026-08-16T15-52-51' }),
      ds({ name: 'lame', dt: '2026-08-15T18-37-25' }),
    ]);
    expect(row!.dispatches).toBe(3);
    expect(dispatchCell(row!)).toBe('2026-08-19T14-49-20 · +2');
  });

  it('shows a single dispatch bare, and a standalone list as what it is', () => {
    const [one] = rowsOf([ds({ name: 'lame' })]);
    expect(dispatchCell(one!)).toBe('2026-08-19T14-49-20');
    const [list] = rowsOf([ds({ kind: 'standalone', name: 'seeds', version: undefined, dt: undefined })]);
    expect(dispatchCell(list!)).toBe('standalone list');
  });

  it('carries every Actor version that wrote the name', () => {
    const [row] = rowsOf([
      ds({ name: 'lame', version: '0.2.0', dt: '2026-08-19T14-49-20' }),
      ds({ name: 'lame', version: '0.1.0', dt: '2026-08-16T15-52-51' }),
    ]);
    expect(row!.version).toBe('v0.2.0, v0.1.0');
  });
});

describe('the columns cell', () => {
  it('is empty while the schema call is still out, and filled once it answers', () => {
    expect(rowsOf([ds({ name: 'lame' })])[0]!.columns).toEqual([]);
    const [row] = rowsOf([ds({ name: 'lame' })], { columns: { lame: ['domain', 'ok'] } });
    expect(row!.columns).toEqual(['domain', 'ok']);
  });
});

/**
 * THE EXPIRES COLUMN — retention, visible on the row, BEFORE it bites.
 *
 * The sweep is armed and hourly (issue 18). Until now the only place a Dataset said what would happen
 * to it was its own page, which is no help to an operator scanning a hundred rows for the ones about
 * to age out. The reading is computed per row here, not in a cell, so the column's sort and filter act
 * on the same words the cell draws.
 */
describe('the expires column: the sweep’s reading, on every row', () => {
  const AGED = AT - (24 + 6 + 1) * 3_600_000;

  it('says an untagged, sealed, aged Dataset would be collected', () => {
    const [row] = rowsOf([ds({ name: 'doomed', runId: 'r1', state: 'sealed', updatedAt: AGED })], {
      now: AT,
    });
    expect(row!.expiry.disposition).toBe('collect');
    expect(row!.expiry.label).toBe('would be collected');
    expect(row!.expiry.due).toBe(true);
  });

  it('says a TAGGED one is kept, and stops glowing the moment the tag lands', () => {
    const [row] = rowsOf(
      [ds({ name: 'kept', runId: 'r1', state: 'sealed', updatedAt: AGED, tags: ['keep'] })],
      { now: AT }
    );
    expect(row!.expiry.disposition).toBe('kept-tagged');
    expect(row!.expiry.due).toBe(false);
  });

  it('counts down for one that is not there yet, in hours', () => {
    const [row] = rowsOf(
      [ds({ name: 'young', runId: 'r1', state: 'sealed', updatedAt: AT - 3_600_000 })],
      { now: AT }
    );
    expect(row!.expiry.disposition).toBe('kept-fresh');
    expect(row!.expiry.label).toContain('would be collected in');
  });

  /** The sweep keys on every contributing Run, so a Dataset with no SINGULAR run is still swept —
   *  the row must not read "not swept" over the largest Datasets on the box. */
  it('reads a Dataset several Runs wrote as a candidate, not as unswept', () => {
    const [row] = rowsOf(
      [
        ds({ name: 'shared', runId: undefined, state: 'sealed', updatedAt: AGED, contributingRuns: ['a', 'b'] }),
      ],
      { now: AT }
    );
    expect(row!.soleRun).toBe('');
    expect(row!.expiry.disposition).toBe('collect');
  });

  it('never marks a standalone list, which the sweep does not enumerate', () => {
    const [row] = rowsOf([ds({ name: 'scope', kind: 'standalone', updatedAt: AGED })], { now: AT });
    expect(row!.expiry.disposition).toBe('not-a-candidate');
    expect(row!.expiry.due).toBe(false);
  });
});
