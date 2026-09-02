/**
 * The Datasets surface's pure parts.
 *
 * `openingQuery` is the one worth pinning: opening a dataset runs it immediately, so a name this
 * gets wrong is a Catalog Error the operator sees before they have typed anything — and the names
 * that need quoting are exactly the ones a real run produces (`nscheck-0.1.0`, `apex.domains`).
 */

import { describe, expect, it } from 'vitest';

import { fmtBytes, keyOf, openingQuery, quoteIfNeeded, snippets } from './DatasetPage';

describe('quoteIfNeeded', () => {
  it('leaves a plain identifier alone', () => {
    expect(quoteIfNeeded('domains')).toBe('domains');
    expect(quoteIfNeeded('_ns_facts2')).toBe('_ns_facts2');
  });

  it('quotes what a bare identifier cannot be', () => {
    // Every one of these is a name the lake actually holds: an actor's output is named after the
    // actor, and actor names carry hyphens and dots.
    expect(quoteIfNeeded('nscheck-0.1.0')).toBe('"nscheck-0.1.0"');
    expect(quoteIfNeeded('2domains')).toBe('"2domains"');
  });

  it('escapes an embedded double quote rather than ending the identifier early', () => {
    expect(quoteIfNeeded('od"d')).toBe('"od""d"');
  });
});

describe('openingQuery', () => {
  it('is a query that runs, for a name that needs quoting', () => {
    expect(openingQuery('nscheck-0.1.0')).toContain('FROM "nscheck-0.1.0"');
  });

  it('is bounded', () => {
    // An unbounded opening query against a 37k-row sweep output is a page that hangs on arrival.
    expect(openingQuery('domains')).toMatch(/LIMIT \d+;/);
  });

  it('reads every Run of the name when nothing scoped it', () => {
    // A Dataset name spans Runs: this is the 1,246-row query, and the panel above it says so.
    expect(openingQuery('lame')).not.toContain('WHERE');
  });

  it('filters to one Run when the console is scoped to one', () => {
    // The grid has to show the rows the counts are talking about. `run_id` is the column the lake
    // has always carried, and it is the one an operator would otherwise have to know to type.
    const sql = openingQuery('lame', 'nightly-2026-08-15');
    expect(sql).toContain("WHERE run_id = 'nightly-2026-08-15'");
    expect(sql).toMatch(/LIMIT \d+;/);
  });

  it('escapes a quote in a run id rather than ending the literal early', () => {
    // A run id is a caller's workflow id — an arbitrary string that reaches here from a URL and
    // from the lake, so it is escaped exactly like the identifier beside it.
    expect(openingQuery('lame', "r'; DROP TABLE lame --")).toContain(
      "WHERE run_id = 'r''; DROP TABLE lame --'"
    );
  });
});

describe('fmtBytes', () => {
  it('reads as an operator does', () => {
    expect(fmtBytes(512)).toBe('512 B');
    expect(fmtBytes(1536)).toBe('1.5 KB');
    expect(fmtBytes(5 * 1024 * 1024)).toBe('5.0 MB');
    expect(fmtBytes(20 * 1024 * 1024)).toBe('20 MB');
  });

  it('is blank for nothing, so a zero-byte dataset draws an em dash rather than "0 B"', () => {
    expect(fmtBytes(0)).toBe('');
  });
});

describe('keyOf', () => {
  it('separates two dispatches of one actor', () => {
    const a = { kind: 'output' as const, name: 'nscheck', version: '0.1.0', dt: '2026-08-15T10-00-00' };
    const b = { ...a, dt: '2026-08-15T11-00-00' };
    expect(keyOf(a)).not.toBe(keyOf(b));
  });

  it('separates an output from a standalone list of the same name', () => {
    expect(keyOf({ kind: 'output', name: 'domains' })).not.toBe(
      keyOf({ kind: 'standalone', name: 'domains' })
    );
  });
});

describe('snippets', () => {
  // A snippet naming a column this table does not have is a Binder Error dressed up as a shortcut.
  it('builds every snippet from the dataset and its OWN first column', () => {
    const got = snippets('lame', 'domain');
    expect(got).toHaveLength(3);
    for (const s of got) expect(s.sql).toContain('lame');
    expect(got[1]!.sql).toContain('domain');
    expect(got[2]!.sql).toContain('domain');
  });

  it('quotes a name that is not a legal bare identifier, on both sides', () => {
    const group = snippets('my dataset', 'has space')[1]!;
    expect(group.sql).toContain('"my dataset"');
    expect(group.sql).toContain('"has space"');
  });

  it('labels by what the query does, not by the SQL it contains', () => {
    expect(snippets('lame', 'domain').map((s) => s.label)).toEqual([
      'count the rows',
      'group by domain',
      'filter on domain',
    ]);
  });
});
