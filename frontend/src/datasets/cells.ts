/**
 * How a cell reads.
 *
 * TWO COMPLAINTS, ONE FILE. A grid over actor output has to answer both, and it answered neither:
 *
 *   "the grid doesn't preview or show big values unless I resize the row myself" — a crawl's
 *   markdown body is thousands of characters in a 120-pixel column, and clipping it to `Lorem ip…`
 *   makes the one column somebody opened the Dataset to read the only one they cannot.
 *
 *   "nor does it show objects properly like duckdb does" — a STRUCT went through
 *   `JSON.stringify` and came out `{"ns":"a.example.com","ok":true}`. The value is right and the
 *   NOTATION is not: every other surface an operator meets these rows in — `kontra dataset query`,
 *   the duckdb CLI, an EXPLAIN — prints `{'ns': a.example.com, 'ok': true}`. Two spellings of one
 *   value across two windows of the same data is a thing to translate rather than read.
 *
 * SO THE GRID SPEAKS DUCKDB. {@link duckdbText} is that notation, verified against duckdb 1.5.5
 * rather than inferred: keys single-quoted, strings UNQUOTED at every depth, `NULL` in capitals,
 * lists in square brackets. It is display only — {@link inspect} is what hands over something to
 * copy, and that is JSON, because JSON is what a value pasted into a script has to be.
 *
 * The two are deliberately different and neither is a fallback for the other.
 */

/** Longer than this and a cell cannot be read in a grid row, whatever the column width. */
export const BIG_TEXT = 120;

/**
 * One value, as DuckDB prints it.
 *
 * VERIFIED, NOT GUESSED. From `duckdb -c "select {'a': 'x y', 'b': ['p','q'], 'c': {'d': 'z'}}"`:
 *
 *     {'a': x y, 'b': [p, q], 'c': {'d': z}}
 *
 * — so keys are quoted and values are not, at every depth, and a nested list keeps its brackets.
 * An empty string therefore renders as nothing at all, which is exactly what DuckDB shows and is
 * why {@link inspect} exists for the times that matters.
 */
export function duckdbText(v: unknown): string {
  if (v === null || v === undefined) return 'NULL';
  if (Array.isArray(v)) return `[${v.map(duckdbText).join(', ')}]`;
  if (v instanceof Date) return v.toISOString();
  if (typeof v === 'object') {
    const entries = Object.entries(v as Record<string, unknown>);
    return `{${entries.map(([k, val]) => `'${k}': ${duckdbText(val)}`).join(', ')}}`;
  }
  return String(v);
}

/**
 * Is this value one the grid cannot show in a row?
 *
 * A NESTED VALUE ALWAYS IS, however short. `{'ok': true}` fits in a column and still wants opening —
 * its shape is the thing being read, and a shape is not something you scan across a row.
 */
export function isBig(v: unknown, threshold = BIG_TEXT): boolean {
  if (v === null || v === undefined) return false;
  if (typeof v === 'object') return true;
  return String(v).length > threshold;
}

/** What the inspector shows: a body, and which of the two ways to read it. */
export interface CellDetail {
  /** `json` for a nested value — pretty-printed and copyable into anything that speaks JSON.
   *  `text` for a long string, kept byte-for-byte because it is somebody's document. */
  kind: 'json' | 'text';
  body: string;
  /** How much there is, said in the unit that matters for that kind. */
  size: string;
}

/**
 * One value, opened.
 *
 * JSON FOR A NESTED VALUE, deliberately not the DuckDB notation the grid shows. DuckDB's form is
 * unambiguous to read and lossy to parse — an unquoted string containing `, ` is indistinguishable
 * from two elements — so what an operator copies out of here has to be the form that round-trips.
 *
 * TEXT VERBATIM. A markdown body is the reason this panel exists; re-wrapping it, trimming it or
 * running it through `JSON.stringify` (which would show `\n` where the newlines are) would hide the
 * thing being inspected behind an escaping of it.
 */
export function inspect(v: unknown): CellDetail {
  if (v !== null && v !== undefined && typeof v === 'object') {
    const body = JSON.stringify(v, null, 2) ?? String(v);
    const n = Array.isArray(v) ? v.length : Object.keys(v as object).length;
    return {
      kind: 'json',
      body,
      size: Array.isArray(v)
        ? `${n} ${n === 1 ? 'element' : 'elements'}`
        : `${n} ${n === 1 ? 'field' : 'fields'}`,
    };
  }
  // NULL is a value with a detail worth showing: "this cell is empty" and "this cell is NULL" are
  // different facts about a Dataset, and the grid draws both as an empty rectangle.
  const body = v === null || v === undefined ? 'NULL' : String(v);
  const lines = body.split('\n').length;
  return {
    kind: 'text',
    body,
    size: `${body.length.toLocaleString()} chars · ${lines.toLocaleString()} ${lines === 1 ? 'line' : 'lines'}`,
  };
}
