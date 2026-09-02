import { describe, expect, it } from 'vitest';
import { BIG_TEXT, duckdbText, inspect, isBig } from './cells';

/**
 * The grid's notation, and what opening a cell hands over.
 *
 * THE EXPECTED STRINGS ARE MEASURED, not inferred. Every one below was read out of duckdb 1.5.5:
 *
 *     D select {'a': 1, 'b': 'x', 'c': NULL} as s, [1,2,3] as l, ['a','b'] as ls;
 *     │ {'a': 1, 'b': x, 'c': NULL} │ [1, 2, 3] │ [a, b] │
 *
 *     D select {'a': 'x y', 'b': ['p','q'], 'c': {'d': 'z'}} as s, ['a b', NULL] as l;
 *     │ {'a': x y, 'b': [p, q], 'c': {'d': z}} │ [a b, NULL] │
 *
 * Which is the point of the file: the grid and `kontra dataset query` are two windows onto the same
 * rows, and two spellings of one value across them is a thing to translate rather than read.
 */

describe('the grid prints what duckdb prints', () => {
  it('renders a struct with quoted keys and unquoted values', () => {
    expect(duckdbText({ a: 1, b: 'x', c: null })).toBe("{'a': 1, 'b': x, 'c': NULL}");
  });

  it('renders a list in brackets, its strings unquoted', () => {
    expect(duckdbText([1, 2, 3])).toBe('[1, 2, 3]');
    expect(duckdbText(['a', 'b'])).toBe('[a, b]');
    expect(duckdbText(['a b', null])).toBe('[a b, NULL]');
  });

  it('keeps the rule at every depth', () => {
    expect(duckdbText({ a: 'x y', b: ['p', 'q'], c: { d: 'z' } })).toBe(
      "{'a': x y, 'b': [p, q], 'c': {'d': z}}"
    );
    expect(duckdbText([{ k: 1 }])).toBe("[{'k': 1}]");
  });

  it('says NULL, in capitals, rather than drawing nothing', () => {
    // An empty cell and a NULL cell are different facts about a Dataset.
    expect(duckdbText(null)).toBe('NULL');
    expect(duckdbText(undefined)).toBe('NULL');
  });

  it('is not JSON, which is what it replaced', () => {
    // `JSON.stringify` produced `{"ns":"a.example.com","ok":true}` — right value, wrong notation for
    // every other window an operator meets these rows in.
    const struct = { ns: 'a.example.com', ok: true };
    expect(duckdbText(struct)).toBe("{'ns': a.example.com, 'ok': true}");
    expect(duckdbText(struct)).not.toBe(JSON.stringify(struct));
  });

  it('leaves scalars alone', () => {
    expect(duckdbText('a.example.com')).toBe('a.example.com');
    expect(duckdbText(0)).toBe('0');
    expect(duckdbText(false)).toBe('false');
    expect(duckdbText('')).toBe('');
  });
});

describe('which cells cannot be read in a row', () => {
  it('counts any nested value, however short', () => {
    // Its SHAPE is the thing being read, and a shape is not something you scan across a row.
    expect(isBig({ ok: true })).toBe(true);
    expect(isBig([])).toBe(true);
  });

  it('counts long text', () => {
    expect(isBig('x'.repeat(BIG_TEXT + 1))).toBe(true);
    expect(isBig('x'.repeat(BIG_TEXT))).toBe(false);
  });

  it('leaves ordinary scalars alone, so the affordance means something', () => {
    expect(isBig('a.example.com')).toBe(false);
    expect(isBig(42)).toBe(false);
    expect(isBig(null)).toBe(false);
  });
});

describe('what opening a cell hands over', () => {
  it('gives JSON for a nested value, not the notation the grid drew', () => {
    // DuckDB's form is unambiguous to read and lossy to parse: an unquoted string containing ", "
    // is indistinguishable from two elements. What gets copied has to round-trip.
    const d = inspect({ ns: 'a.example.com', ok: true });
    expect(d.kind).toBe('json');
    expect(JSON.parse(d.body)).toEqual({ ns: 'a.example.com', ok: true });
    expect(d.size).toBe('2 fields');
  });

  it('counts a list in elements and a struct in fields', () => {
    expect(inspect([1, 2, 3]).size).toBe('3 elements');
    expect(inspect([1]).size).toBe('1 element');
    expect(inspect({ a: 1 }).size).toBe('1 field');
  });

  it('gives long text back byte for byte', () => {
    // A markdown body is the reason this panel exists. `JSON.stringify` would show `\n` where the
    // newlines are, which hides the document behind an escaping of it.
    const md = '# Title\n\nA paragraph with "quotes" and a \\backslash.\n';
    const d = inspect(md);
    expect(d.kind).toBe('text');
    expect(d.body).toBe(md);
    expect(d.size).toContain('4 lines');
    expect(d.size).toContain(`${md.length} chars`);
  });

  it('has something to say about NULL', () => {
    // The grid draws NULL and empty identically — as an empty rectangle — so the inspector is where
    // the difference is stated.
    expect(inspect(null)).toMatchObject({ kind: 'text', body: 'NULL' });
    expect(inspect('')).toMatchObject({ kind: 'text', body: '' });
  });
});
