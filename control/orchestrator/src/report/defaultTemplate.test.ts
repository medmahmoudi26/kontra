import { describe, expect, it } from 'vitest';

import type { RefResolution } from './codeTag';
import { DEFAULT_TEMPLATE, defaultContext, defaultTemplateId } from './defaultTemplate';
import { renderReport, type EngineDeps } from './engine';
import { buildSnapshot, countNodes, type MdNode } from './render';

/**
 * ACCEPTANCE 15: a workflow without `report.md` gets the default report.
 *
 * These tests render the default template through the real engine and parser, because the thing most
 * likely to be wrong about a loop-built GFM table is its whitespace — and a table that lost its
 * structure parses as paragraphs, silently, and still "renders".
 */

const deps: EngineDeps = {
  async resolveRef(): Promise<RefResolution> {
    return { unresolved: 'the default report resolves no refs' };
  },
  redactLatin1: (t) => t,
  redactText: (t) => t,
};

const RUN = {
  id: 'r_8f3c1a',
  workflow_id: 'enrich-catalog-eu',
  status: 'completed',
  started_at: '2026-10-06T12:00:00.000Z',
  ended_at: '2026-10-06T12:07:41.000Z',
  duration_s: 461,
  error: null,
};

async function renderDefault(run: Record<string, unknown>, result: unknown) {
  const { markdown, blocks } = await renderReport(
    DEFAULT_TEMPLATE,
    {
      run,
      workflow: { name: 'enrich', version: 'abc1234', workspace: 'default' },
      input: {},
      result,
      report: { rendered_at: '2026-10-06T12:07:45.000Z', template_hash: 'default@0.1.0', version: 1 },
      default: defaultContext(run, result),
    },
    deps
  );
  const { snapshot } = buildSnapshot(markdown, blocks);
  return { markdown, snapshot };
}

function tables(root: MdNode): MdNode[] {
  const out: MdNode[] = [];
  const visit = (n: MdNode) => {
    if (n.type === 'table') out.push(n);
    for (const c of n.children ?? []) visit(c);
  };
  visit(root);
  return out;
}

function cellsOf(table: MdNode): string[][] {
  return (table.children as MdNode[]).map((row) =>
    (row.children as MdNode[]).map((c) => (c.children ?? []).map((t) => String(t.value ?? '')).join(''))
  );
}

describe('the run block', () => {
  it('renders as one table with a row per field, in a fixed order', async () => {
    const { snapshot } = await renderDefault(RUN, null);
    const [runTable] = tables(snapshot.root);
    expect(runTable).toBeDefined();
    const cells = cellsOf(runTable!);
    expect(cells[0]).toEqual(['field', 'value']);
    expect(cells.map((r) => r[0])).toEqual([
      'field',
      'id',
      'workflow_id',
      'status',
      'started_at',
      'ended_at',
      'duration_s',
    ]);
    expect(cells[1]).toEqual(['id', 'r_8f3c1a']);
  });

  it('omits a field the run does not have, rather than printing an empty row', async () => {
    const { snapshot } = await renderDefault({ id: 'r_1', status: 'failed' }, null);
    const cells = cellsOf(tables(snapshot.root)[0]!);
    expect(cells.map((r) => r[0])).toEqual(['field', 'id', 'status']);
  });

  it('shows the error for a run that failed', async () => {
    const { snapshot } = await renderDefault(
      { ...RUN, status: 'failed', error: { type: 'TimeoutError', message: 'the feed did not answer' } },
      null
    );
    const text = JSON.stringify(snapshot.root);
    expect(text).toContain('It ended with an error');
    expect(text).toContain('TimeoutError');
    expect(text).toContain('the feed did not answer');
  });
});

describe('the result, flattened', () => {
  it('turns a flat object into a field/value table', async () => {
    const { snapshot } = await renderDefault(RUN, { products: 12480, missing_image: 214, isolated: 6 });
    const t = tables(snapshot.root);
    expect(t).toHaveLength(2);
    const cells = cellsOf(t[1]!);
    expect(cells[0]).toEqual(['field', 'value']);
    expect(cells.slice(1)).toEqual([
      ['products', '12480'],
      ['missing_image', '214'],
      ['isolated', '6'],
    ]);
  });

  it('turns a list of objects into a table with a column per field', async () => {
    const { snapshot } = await renderDefault(RUN, {
      price_changes: [
        { sku: 'EU-44871', old: '24.90', new: '31.50' },
        { sku: 'EU-10233', old: '129.00', new: '109.00' },
      ],
    });
    const priceTable = tables(snapshot.root).at(-1)!;
    const cells = cellsOf(priceTable);
    expect(cells[0]).toEqual(['sku', 'old', 'new']);
    expect(cells[1]).toEqual(['EU-44871', '24.90', '31.50']);
    expect(cells).toHaveLength(3);
  });

  it('keeps a column a later row is missing, filling the cell instead of losing the column', async () => {
    const { snapshot } = await renderDefault(RUN, {
      rows: [{ a: 1, b: 2 }, { a: 3 }],
    });
    const cells = cellsOf(tables(snapshot.root).at(-1)!);
    expect(cells[0]).toEqual(['a', 'b']);
    expect(cells[2]).toEqual(['3', '']);
  });

  it('gives a nested object its own table, titled by its path', async () => {
    const { snapshot } = await renderDefault(RUN, { summary: 'ok', counts: { high: 2, low: 9 } });
    const titles = JSON.stringify(snapshot.root);
    expect(titles).toContain('Result · counts');
    expect(tables(snapshot.root)).toHaveLength(3);
  });

  it('describes a deeply nested value instead of printing [object Object]', async () => {
    const { snapshot } = await renderDefault(RUN, { a: { b: { c: { d: { e: 1 } } } } });
    const text = JSON.stringify(snapshot.root);
    expect(text).not.toContain('[object Object]');
    expect(text).toMatch(/\{1 field\}/);
  });

  it('caps a long list and says how many rows it did not print', async () => {
    const many = Array.from({ length: 250 }, (_, i) => ({ i }));
    const { snapshot } = await renderDefault(RUN, { many });
    const cells = cellsOf(tables(snapshot.root).at(-1)!);
    expect(cells).toHaveLength(201); // header + 200
    expect(JSON.stringify(snapshot.root)).toContain('50 further rows are not shown');
  });

  it('says so plainly when a completed run returned nothing', async () => {
    const { snapshot } = await renderDefault(RUN, null);
    expect(JSON.stringify(snapshot.root)).toContain('returned nothing');
  });

  it('does not say that about a run that failed, where nothing returned is expected', async () => {
    const { snapshot } = await renderDefault({ ...RUN, status: 'failed' }, null);
    expect(JSON.stringify(snapshot.root)).not.toContain('returned nothing');
  });
});

describe('the default report is a real report', () => {
  it('escapes a hostile value in a result exactly as an authored template would', async () => {
    const { snapshot } = await renderDefault(RUN, { note: 'x | y\n**z** <script>' });
    expect(countNodes(snapshot.root, 'html')).toBe(0);
    const t = tables(snapshot.root).at(-1)!;
    // One row, two cells: the pipe did not add a column and the newline did not add a row.
    expect(cellsOf(t)).toEqual([
      ['field', 'value'],
      ['note', 'x | y **z** <script>'],
    ]);
  });

  it('tells the reader it is the default, and how to replace it', async () => {
    const { snapshot } = await renderDefault(RUN, { a: 1 });
    expect(JSON.stringify(snapshot.root)).toContain('has no');
    expect(JSON.stringify(snapshot.root)).toContain('report.md');
  });

  it('names itself with the kontra version, per spec §4.6', () => {
    expect(defaultTemplateId('0.1.0')).toBe('default@0.1.0');
  });

  it('produces the same bytes twice, so a re-render reproduces a version', async () => {
    const a = await renderDefault(RUN, { n: 1 });
    const b = await renderDefault(RUN, { n: 1 });
    expect(a.markdown).toBe(b.markdown);
  });
});
