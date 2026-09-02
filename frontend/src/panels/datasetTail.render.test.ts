/**
 * The Data tab's row expander, drawn — collapsed, and in every state it can be opened into
 * (issue 25).
 *
 * NODE, NO JSDOM, as everywhere else here: `renderToStaticMarkup` and string assertions.
 * {@link TailBody} is a pure function of `{state, known, lifecycle, how}`, which is what lets a
 * sealed tail, an open one following, an under-ten Dataset, an empty one and a failed read all be
 * asserted without a fetch — and `DatasetTail` itself is rendered too, because "collapsed reads no
 * rows" is a claim about the wrapper and not about the body.
 *
 * WHAT IS ASSERTED IS THE SENTENCES. "the last 10 of 42 rows" and "the first 10 rows · 223,378 in
 * all" draw the same table and mean opposite things, and only one of them is a tail.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import type { DatasetInfo, RunRow } from '../run/api';
import {
  TAIL_START,
  readTail,
  tailPlan,
  tailReduce,
  type TailColumn,
  type TailState,
} from '../datasets/tailRows';
import { datasetState } from '../datasets/state';
import { DatasetTail, TailBody } from './DatasetTail';
import { RunDataTab } from './RunMonitor';
import { runDatasets } from './runDatasets';

const T0 = 1_787_084_868_000;

const RUN: RunRow = {
  runId: 'sweep-1787084868',
  type: 'DnsSweep',
  status: 'running',
  tenant: 'default',
  startedAt: T0,
  closedAt: 0,
  dispatches: 3,
};

const COLUMNS: TailColumn[] = [
  { name: 'host', type: 'VARCHAR' },
  { name: 'ok', type: 'BOOLEAN' },
];

function ds(over: Partial<DatasetInfo> & { name: string }): DatasetInfo {
  return { kind: 'output', rows: 0, bytes: 0, ...over };
}

function rows(n: number, from = 0): unknown[][] {
  return Array.from({ length: n }, (_, i) => [`h${from + i}.example.com`, true]);
}

/** The state a component would be in after one successful read of a Dataset of `total` rows. */
function afterRead(total: number, at = T0): TailState {
  const plan = tailPlan(total);
  return tailReduce(TAIL_START, {
    type: 'rows',
    read: readTail({ columns: COLUMNS, rows: rows(total) }, plan, at),
  });
}

function drawBody(
  state: TailState,
  info: DatasetInfo,
  how: 'sole' | 'among' | 'owner' = 'sole'
): string {
  return renderToStaticMarkup(
    createElement(TailBody, {
      state,
      known: info.rows,
      lifecycle: datasetState(info),
      how,
      name: info.name,
    })
  );
}

function drawTab(catalog: DatasetInfo[], listedAt = T0, run: RunRow | null = RUN): string {
  return renderToStaticMarkup(
    createElement(RunDataTab, {
      run,
      datasets: runDatasets(catalog, run?.runId ?? null, listedAt),
      onOpen: () => {},
    })
  );
}

describe('collapsed, the Data tab is what it was', () => {
  const CATALOG = [
    ds({ name: 'lame', runId: RUN.runId, rows: 623, state: 'open', updatedAt: T0 + 2 }),
    ds({ name: 'apexes', contributingRuns: ['r0', RUN.runId], rows: 1246, state: 'sealed' }),
  ];

  it('still lists, attributes, states, counts and links out', () => {
    const html = drawTab(CATALOG);
    expect(html).toContain('data-testid="run-dataset-lame"');
    expect(html).toContain('data-how="sole"');
    expect(html).toContain('data-how="among"');
    expect(html).toContain('623 rows');
    expect(html).toContain('data-state="open"');
    expect(html).toContain('data-state="sealed"');
    expect(html).toContain('not this run’s');
  });

  it('reads no rows until something is opened', () => {
    // The body is mounted only once the twisty is, so a run with nine Datasets issues nine requests
    // for five hundred rows each ONLY if an operator asks for all nine.
    const html = drawTab(CATALOG);
    expect(html).toContain('data-testid="run-dataset-expander-lame"');
    expect(html).toContain('data-open="false"');
    expect(html).not.toContain('<table');
    expect(html).not.toContain('data-testid="run-tail-lame"');
  });

  it('promises on the twisty only what opening it will deliver', () => {
    const html = drawTab([
      ds({ name: 'lame', runId: RUN.runId, rows: 400, state: 'open' }),
      ds({ name: 'huge', runId: RUN.runId, rows: 223_378, state: 'sealed' }),
      ds({ name: 'tiny', runId: RUN.runId, rows: 4, state: 'sealed' }),
    ]);
    expect(html).toContain('the last 10 rows');
    expect(html).toContain('the first 10 rows');
    expect(html).toContain('all 4 rows');
  });

  it('still says a run wrote nothing, and says it as its own sentence', () => {
    // The expander is per-Dataset, so a run with NO Dataset has none of it — and the tab's careful
    // distinction between "nothing in the catalog carries this run" and "this run failed" is
    // untouched by anything in this slice.
    const html = drawTab([]);
    expect(html).toContain('data-testid="run-data-none"');
    expect(html).toContain('No Dataset in the lake carries this run.');
    expect(html).not.toContain('data-testid="run-dataset-expander-');
    // And "nobody looked" is still not "nothing there".
    expect(drawTab([], 0)).toContain('data-testid="run-data-unread"');
  });

  it('gives every Dataset its own expander, not one shared accordion', () => {
    const html = drawTab(CATALOG);
    expect(html).toContain('data-testid="run-dataset-expander-lame"');
    expect(html).toContain('data-testid="run-dataset-expander-apexes"');
    // `<details name=…>` would make them mutually exclusive — opening one would close the other,
    // which is precisely what "a run with several Datasets expands each independently" forbids.
    expect(html).not.toContain('<details name=');
    expect(renderToStaticMarkup(createElement(DatasetTail, {
      info: CATALOG[0]!,
      how: 'sole' as const,
    }))).not.toContain('name=');
  });

  it('is identical across a catalog tick that changed nothing', () => {
    // The list holds no tail state, so two polls of an unchanged catalog draw the same bytes — the
    // property that keeps an expander off the tick-driven-work path issue #14 closed.
    expect(drawTab(CATALOG.map((d) => ({ ...d })))).toBe(drawTab(CATALOG.map((d) => ({ ...d }))));
  });
});

describe('expanded', () => {
  it('shows a sealed Dataset’s last ten rows, and says they are final', () => {
    const info = ds({ name: 'apexes', runId: RUN.runId, rows: 42, state: 'sealed' });
    const html = drawBody(afterRead(42), info);
    expect(html).toContain('data-reach="tail"');
    expect(html).toContain('data-rows="10"');
    expect(html).toContain('the last 10 of 42 rows');
    expect(html).toContain('will not change');
    // The END of the Dataset, not the start of the window.
    expect(html).toContain('h41.example.com');
    expect(html).not.toContain('h0.example.com');
    // The columns are the read's own, drawn as a header.
    expect(html).toContain('>host</th>');
    expect(html).toContain('>ok</th>');
  });

  it('shows an open Dataset as still open, and its tail moves when rows commit', () => {
    const info = ds({ name: 'lame', runId: RUN.runId, rows: 42, state: 'open' });
    const first = drawBody(afterRead(42), info);
    expect(first).toContain('data-follows="true"');
    expect(first).toContain('data-state="open"');
    expect(first).toContain('Still open');
    expect(first).not.toContain('will not change');
    expect(first).toContain('h41.example.com');

    // The catalog's count moved from 42 to 45 — three rows committed — so the read is redone and the
    // last ten are now 35..44. Arrival is the rows changing; nothing about the markup blinks.
    const grown = ds({ ...info, rows: 45 });
    const second = drawBody(afterRead(45), grown);
    expect(second).toContain('h44.example.com');
    expect(second).toContain('the last 10 of 45 rows');
    expect(second).not.toContain('>h34.example.com<');
  });

  it('keeps the other two lifecycles apart from both', () => {
    const abandoned = drawBody(afterRead(42), ds({ name: 'x', rows: 42, state: 'abandoned' }));
    expect(abandoned).toContain('data-state="abandoned"');
    expect(abandoned).toContain('data-follows="false"');
    expect(abandoned).toContain('gave up');

    const none = drawBody(afterRead(42), ds({ name: 'x', rows: 42 }));
    expect(none).toContain('data-state="none"');
    expect(none).toContain('No lifecycle');
    // An unrecorded lifecycle is not a sealed one, here as everywhere else (§11).
    expect(none).not.toContain('Sealed —');
  });

  it('says a short Dataset is all of it', () => {
    const html = drawBody(afterRead(3), ds({ name: 'tiny', rows: 3, state: 'sealed' }));
    expect(html).toContain('all 3 rows — that is the whole of it');
    expect(html).toContain('data-rows="3"');
    expect(html).toContain('h0.example.com');
  });

  it('calls the first ten of a long Dataset the first ten, and points at the console', () => {
    const info = ds({ name: 'huge', runId: RUN.runId, rows: 223_378, state: 'sealed' });
    const state = tailReduce(TAIL_START, {
      type: 'rows',
      read: readTail({ columns: COLUMNS, rows: rows(10) }, tailPlan(223_378), T0),
    });
    const html = drawBody(state, info);
    expect(html).toContain('data-reach="head"');
    expect(html).toContain('the first 10 rows · 223,378 in all');
    expect(html).toContain('one click away in the console');
    expect(html).not.toContain('the last 10');
  });

  it('tells an empty open Dataset from an empty finished one', () => {
    const open = drawBody(afterRead(0), ds({ name: 'lame', rows: 0, state: 'open' }));
    expect(open).toContain('data-testid="run-tail-empty-lame"');
    expect(open).toContain('nothing has committed to it so far');

    const sealed = drawBody(afterRead(0), ds({ name: 'lame', rows: 0, state: 'sealed' }));
    expect(sealed).toContain('never written to');
  });

  it('warns that a shared partition’s rows are not only this run’s', () => {
    const info = ds({ name: 'apexes', contributingRuns: ['r0', RUN.runId], rows: 42, state: 'sealed' });
    const html = drawBody(afterRead(42), info, 'among');
    expect(html).toContain('data-testid="run-tail-among-apexes"');
    expect(html).toContain('not necessarily');
    expect(drawBody(afterRead(42), info, 'sole')).not.toContain('run-tail-among-');
  });

  it('says it is reading before the rows arrive, and never draws an empty table', () => {
    const html = drawBody(tailReduce(TAIL_START, { type: 'read' }), ds({ name: 'lame', rows: 42, state: 'open' }));
    expect(html).toContain('data-phase="reading"');
    expect(html).toContain('Reading the last 10 rows…');
    expect(html).not.toContain('<table');
  });

  it('keeps the rows a failed re-read already had, and prints the server’s sentence', () => {
    const failed = tailReduce(afterRead(42), {
      type: 'failed',
      error: 'preview dataset: no output dataset named "lame"',
    });
    const html = drawBody(failed, ds({ name: 'lame', rows: 42, state: 'open' }));
    expect(html).toContain('data-phase="failed"');
    expect(html).toContain('h41.example.com');
    expect(html).toContain('no output dataset named');
    expect(html).toContain('the last read that worked');
  });
});
