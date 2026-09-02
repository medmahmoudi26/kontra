/**
 * Every count says what it counted — and two counts from two statements are never divided.
 *
 * THE MEASURED CONFUSION. `SELECT count(*) FROM lame` returns 1,246: two Runs of one workflow
 * appending to one Dataset name. The listing shows 623 against that name, because a listing row is
 * one dispatch. Both correct, neither labelled. These pin the vocabulary that makes the difference
 * sayable, and the refusal that keeps the two from being turned into a percentage.
 */

import { describe, expect, it } from 'vitest';

import type { DatasetInfo } from '../run/api';
import {
  asOfText,
  countText,
  counted,
  datasetTotal,
  ledgerStatement,
  listingScope,
  listingStatement,
  provenanceStatement,
  scopeWords,
  share,
} from './scope';

const AT = Date.UTC(2026, 7, 16, 2, 15, 7);

/** One listing row — one `(version, dt)` partition of a name, as `/api/datasets` returns it. */
function row(over: Partial<DatasetInfo> & { name: string }): DatasetInfo {
  return { kind: 'output', version: '0.1.0', dt: '2026-08-15T01-00-00', rows: 0, bytes: 0, ...over };
}

describe('a count states its scope in words', () => {
  it('says which rows each of the three scopes covers', () => {
    expect(countText(1246, 'dataset')).toBe('1,246 rows · every run');
    expect(countText(623, 'run')).toBe('623 rows · this run');
    expect(countText(623, 'dispatch')).toBe('623 rows · this dispatch');
  });

  it('does not speak of Runs for a list no Run wrote', () => {
    // `kontra dataset create` loads a standalone list. "every run" would claim a producer that
    // does not exist, and it has no partitions either — one table, one count.
    expect(countText(400, 'dataset', 'standalone')).toBe('400 rows · the whole list');
    expect(scopeWords('dataset', 'standalone').title).toMatch(/no Run wrote it/);
  });

  it('gives every scope a long form that names the rows exactly', () => {
    for (const scope of ['dataset', 'run', 'dispatch'] as const) {
      expect(scopeWords(scope).title.length).toBeGreaterThan(40);
      expect(scopeWords(scope).label.length).toBeGreaterThan(0);
    }
    // The three read differently at a glance — a scope you have to parse is one nobody reads.
    const labels = (['dataset', 'run', 'dispatch'] as const).map((s) => scopeWords(s).label);
    expect(new Set(labels).size).toBe(3);
  });

  it('warns, in the dispatch scope, that a dispatch is not a Run', () => {
    // `version` is a PARTITION column, so one listing row holds exactly one Actor version: a Run
    // that dispatched at two versions has two rows and neither is the whole of what it wrote.
    expect(scopeWords('dispatch').title).toMatch(/two Actor versions/);
  });

  it('reads a partitioned listing row as a dispatch and an unpartitioned one as the whole', () => {
    expect(listingScope(row({ name: 'lame' }))).toBe('dispatch');
    expect(listingScope({ dt: undefined })).toBe('dataset');
  });
});

describe('a count that may still be growing says as of when', () => {
  it('stamps the moment the statement answered', () => {
    expect(asOfText(AT)).toBe(`as of ${new Date(AT).toLocaleTimeString()}`);
  });
});

describe('two numbers from two statements are never divided', () => {
  const total = counted(1246, 'dataset', provenanceStatement({ name: 'lame', measuredAt: AT }), AT);

  it('divides two counts the same statement measured', () => {
    const part = counted(623, 'run', provenanceStatement({ name: 'lame', measuredAt: AT }), AT);
    expect(share(part, total)).toBeCloseTo(0.5, 10);
  });

  it('refuses a share across two authorities', () => {
    // The ledger knows what a Run committed; the catalog knows what the name holds. They are
    // fetched by different polls on different clocks, so "623 of 1,246" across them looks
    // measured and is not.
    const fromLedger = counted(623, 'run', ledgerStatement('lame', AT), AT);
    expect(share(fromLedger, total)).toBeNull();
  });

  it('refuses a share across two reads of the SAME authority', () => {
    // A Dataset a Run is still appending to grows between two queries — this repo has shipped
    // that bug. Same route, two moments, still two statements.
    const later = counted(1300, 'dataset', provenanceStatement({ name: 'lame', measuredAt: AT + 2000 }), AT + 2000);
    const part = counted(623, 'run', provenanceStatement({ name: 'lame', measuredAt: AT }), AT);
    expect(share(part, later)).toBeNull();
  });

  it('refuses to divide by nothing', () => {
    const empty = counted(0, 'dataset', listingStatement(AT), AT);
    expect(share(counted(0, 'run', listingStatement(AT), AT), empty)).toBeNull();
  });
});

describe("the Dataset's total is every dispatch of the name, summed", () => {
  /** `lame`: two Runs of one workflow, one listing row each. */
  const catalog: DatasetInfo[] = [
    row({ name: 'lame', dt: '2026-08-15T01-00-00', rows: 623, bytes: 100 }),
    row({ name: 'lame', dt: '2026-08-14T01-00-00', rows: 623, bytes: 90 }),
    row({ name: 'other', rows: 7, bytes: 5 }),
  ];

  it('adds every listing row of the name rather than taking the first', () => {
    // `catalog.find(d => d.name === name)` is what the console did, under a caption that said
    // "all runs" — one dispatch's number wearing the Dataset's label.
    const t = datasetTotal(catalog, 'lame', AT)!;
    expect(t.total.rows).toBe(1246);
    expect(t.total.scope).toBe('dataset');
    expect(t.dispatches).toBe(2);
    expect(t.bytes).toBe(190);
  });

  it('carries the poll that measured it, so the number is dated', () => {
    const t = datasetTotal(catalog, 'lame', AT)!;
    expect(t.total.measuredAt).toBe(AT);
    expect(t.total.statement).toBe(listingStatement(AT));
  });

  it('says an operator-loaded list in the noun it belongs to', () => {
    // No Run wrote it, so the total is "the whole list" wherever it is printed — the Catalog's
    // input lists are the case this exists for.
    const t = datasetTotal(
      [{ kind: 'standalone', name: 'scope_paid', rows: 400, bytes: 20 }],
      'scope_paid',
      AT
    )!;
    expect(t.kind).toBe('standalone');
    expect(countText(t.total.rows, 'dataset', t.kind)).toBe('400 rows · the whole list');
  });

  it('sums one row for a name only one dispatch wrote, and still calls it every run', () => {
    const t = datasetTotal(catalog, 'other', AT)!;
    expect(t.total.rows).toBe(7);
    expect(t.dispatches).toBe(1);
    expect(countText(t.total.rows, t.total.scope)).toBe('7 rows · every run');
  });

  it('is null for a name the catalog has never listed — not zero', () => {
    // Zero rows is a Dataset that exists and is empty. Nothing under the name is a different
    // answer, and the run detail draws it as absence rather than as an empty Dataset.
    expect(datasetTotal(catalog, 'never-written', AT)).toBeNull();
  });
});
