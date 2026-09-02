/**
 * The count on a Dataset's page, drawn — all four lifecycle readings, an empty Dataset, a lost
 * stream, and the regression guard that this whole component exists to satisfy.
 *
 * `renderToStaticMarkup` for the reason the other render suites record: this runs in node with no
 * jsdom, and every assertion is about the text and the `data-` hooks in the markup. Effects do not
 * run under it, which is exactly right here — no `EventSource` is constructed, so the wired
 * component can be rendered in the same suite as the pure one.
 *
 * THE GUARD IS THE POINT OF THE LAST DESCRIBE. Issue #14 fixed the results grid resetting on every
 * catalog poll; a live row count is the change that reintroduces it. What stops that is not care, it
 * is shape: the count's state lives in a component BESIDE the grid, and the grid's memo key is a
 * named function whose parameters cannot include a count.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import DatasetAccrual, { AccrualLine } from './DatasetAccrual';
import { buildColumnDefs, resultsGridKey } from './DatasetPage';
import { ROW_TAIL_START, rowTailReduce, type RowTailState } from '../datasets/rowTail';
import type { DatasetState } from '@contract/datasets';
import type { DatasetInfo } from '../run/api';

const NOW = 2_000_000;

/** One opened Dataset, as the console holds it. */
function ds(over: Partial<DatasetInfo>): DatasetInfo {
  return { kind: 'output', name: 'lame', rows: 1246, bytes: 38_777, ...over } as DatasetInfo;
}

const drawWired = (d: DatasetInfo): string =>
  renderToStaticMarkup(createElement(DatasetAccrual, { dataset: d }));

const drawLine = (tail: RowTailState, over: Partial<Parameters<typeof AccrualLine>[0]> = {}): string =>
  renderToStaticMarkup(
    createElement(AccrualLine, {
      rows: 1246,
      scope: 'dataset' as const,
      kind: 'output' as const,
      phase: 'accruing' as const,
      tail,
      now: NOW,
      ...over,
    })
  );

describe('the four lifecycle readings, each its own answer about the NUMBER', () => {
  it('an OPEN Dataset with a Run reads as accruing, and opens the live readout', () => {
    const html = drawWired(ds({ state: 'open', runId: 'nscheck-1' }));
    expect(html).toContain('data-phase="accruing"');
    // Labelled `live`, beside a catalog total that is not: two row counts of one Dataset that
    // legitimately differ must never be drawn as one number. `>live<` rather than `live`, because
    // the readout's own test id contains the word and would pass an assertion that means nothing.
    expect(html).toContain('>live<');
    expect(html).toContain('1,246 rows · every run');
    expect(html).toContain('data-testid="live-rowtail"');
  });

  it('an OPEN Dataset with NO Run says so, rather than watching forever', () => {
    const html = drawWired(ds({ state: 'open' }));
    expect(html).toContain('data-phase="open-untailed"');
    expect(html).toContain('nothing to tail');
    expect(html).not.toContain('data-testid="live-rowtail"'); // no socket, no readout
  });

  it('a SEALED Dataset says the count is final', () => {
    const html = drawWired(ds({ state: 'sealed', runId: 'nscheck-1' }));
    expect(html).toContain('data-phase="sealed"');
    expect(html).toContain('final');
    expect(html).not.toContain('data-testid="live-rowtail"');
  });

  it('an ABANDONED Dataset says partial AND final — the reading that is both', () => {
    const html = drawWired(ds({ state: 'abandoned', runId: 'nscheck-1' }));
    expect(html).toContain('data-phase="abandoned"');
    expect(html).toContain('final · partial');
  });

  it('a Dataset with NO LIFECYCLE says nothing recorded one, not that it is done', () => {
    const html = drawWired(ds({ runId: 'nscheck-1' }));
    expect(html).toContain('data-phase="none"');
    expect(html).toContain('not recorded');
  });

  it('reads an unknown state from a newer peer as no lifecycle, never as sealed', () => {
    // CAST ON PURPOSE: `quiesced` is not in the union and that IS the test — a word a newer peer
    // knows and this build does not. Now that `DatasetState` is the shared contract rather than a
    // bare string, saying so takes a cast, which is the honest way to write "deliberately invalid".
    const html = drawWired(ds({ state: 'quiesced' as DatasetState, runId: 'nscheck-1' }));
    expect(html).toContain('data-phase="none"');
  });
});

describe('empty, and lost', () => {
  it('an EMPTY Dataset counts zero rows and still states its scope', () => {
    // Not blank and not a dash: "0 rows · every run" is a measurement, and an operator reading it
    // knows the query below will return nothing rather than wondering whether the page failed.
    const html = drawWired(ds({ rows: 0, state: 'sealed', runId: 'r' }));
    expect(html).toContain('0 rows · every run');
    expect(html).toContain('data-rows="0"');
  });

  it('a DEGRADED stream says the count is no longer current, and keeps the last one', () => {
    // The failure the tail exists to make visible: a frozen number and a live one look identical.
    const live = rowTailReduce(ROW_TAIL_START, {
      type: 'snapshot',
      snapshot: { rows: 1203, lastChunkAt: NOW - 4000, at: NOW },
    });
    const lost = rowTailReduce(live, { type: 'error' });
    const html = drawLine(lost);
    expect(html).toContain('stream lost · last known 1,203 rows');
    expect(html).toContain('data-phase="accruing"'); // the Dataset is still open — the PIPE died
    expect(html).toContain('amber');
  });

  it('before the first snapshot it says it is watching, not "0 rows"', () => {
    expect(drawLine(ROW_TAIL_START)).toContain('watching for rows…');
  });
});

describe('counts get tabular figures', () => {
  it('so a digit changing does not re-flow the line beside it', () => {
    // Arrival is the number moving; a proportional font makes that a shudder of the whole row, which
    // is the animation the house doctrine refuses.
    expect(drawWired(ds({ state: 'sealed', runId: 'r' }))).toContain('tabular-nums');
    const live = rowTailReduce(ROW_TAIL_START, {
      type: 'snapshot',
      snapshot: { rows: 1203, lastChunkAt: NOW, at: NOW },
    });
    expect(drawLine(live)).toContain('tabular-nums');
  });

  it('draws no animation in any phase — arrival is the row and the number', () => {
    for (const state of ['open', 'sealed', 'abandoned', undefined] as (DatasetState | undefined)[]) {
      const html = drawWired(ds({ state, runId: 'nscheck-1' }));
      expect(html).not.toMatch(/animate-|animation:/);
    }
  });
});

describe('THE GUARD: a live count is not something the grid is handed', () => {
  const columns = [
    { name: 'domain', type: 'VARCHAR' },
    { name: 'ok', type: 'BOOLEAN' },
  ];

  it('moves the readout while the results grid’s memo key stays byte-identical', () => {
    // A tick of the live count is a new number on screen and NOTHING for the grid: the key it
    // rebuilds on is a function of the schema and the row-height mode, and this asserts both halves
    // in one place — the readout changed, the key did not.
    const first = rowTailReduce(ROW_TAIL_START, {
      type: 'snapshot',
      snapshot: { rows: 1200, lastChunkAt: NOW, at: NOW },
    });
    const keyBefore = resultsGridKey(columns, false);
    const second = rowTailReduce(first, {
      type: 'snapshot',
      snapshot: { rows: 1400, lastChunkAt: NOW, at: NOW },
    });
    const keyAfter = resultsGridKey(columns, false);

    expect(drawLine(second)).not.toBe(drawLine(first));
    expect(drawLine(second)).toContain('1,400 rows');
    expect(keyAfter).toBe(keyBefore);
    // ...and the defs the memo would produce across that tick carry the same content, because they
    // are a function of the same two things (serialised, since a def holds formatter closures).
    // `useMemo` keyed on an unchanged key is what turns "same content" into the SAME reference,
    // which is what AG Grid needs to leave the operator's widths and scroll alone.
    expect(JSON.stringify(buildColumnDefs(columns, false))).toBe(
      JSON.stringify(buildColumnDefs(columns, false))
    );
  });

  it('rebuilds only for a changed schema or a changed row height', () => {
    const base = resultsGridKey(columns, false);
    expect(resultsGridKey([...columns], false)).toBe(base); // a fresh result, same shape
    expect(resultsGridKey(columns, true)).not.toBe(base); // the operator pressed Tall rows
    expect(resultsGridKey([...columns, { name: 'ns', type: 'VARCHAR' }], false)).not.toBe(base);
    expect(resultsGridKey([{ name: 'domain', type: 'BOOLEAN' }, columns[1]!], false)).not.toBe(base);
  });

  it('cannot take a count as a parameter — the key is (schema, tall) and nothing else', () => {
    // The dependency is a NAMED function rather than an inline expression precisely so this is
    // assertable: adding a live count to what the grid rebuilds on would have to change this arity,
    // which no longer happens by accident in a dep array.
    expect(resultsGridKey).toHaveLength(2);
  });
});
