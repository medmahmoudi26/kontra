/**
 * THE REPORT A RUN GETS WHEN ITS WORKFLOW FOLDER HAS NO `report.md`.
 *
 * ── THE PAGE IS NEVER EMPTY, WHICH IS THE WHOLE REQUIREMENT ────────────────────────────────────
 *
 * Most runs will never have a template. If "no template" meant "no report", the console's report tab
 * would be blank for the common case and the feature would read as broken rather than as unused. So
 * every terminal run gets a report, and the default one is rendered through the SAME pipeline as an
 * authored template — same engine, same escaping, same sanitiser, same snapshot shape. One renderer,
 * one set of guarantees; a second code path would be a second place for a breakout to be missed.
 *
 * ── WHY THE TEMPLATE IS A TS CONSTANT AND NOT A `.md` FILE ─────────────────────────────────────
 *
 * The spec says "ship the default as an embedded template file", and a `.md` beside this module is
 * the obvious reading. It would not survive the build: this package's `build` script is `tsc` alone,
 * tsc emits `.js` and `.d.ts` and copies nothing else, so `dist/src/report/defaultTemplate.md` would
 * not exist and the orchestrator container would fail at the first run with no template — at
 * runtime, in production, on the path that is the common case. A template literal is emitted by
 * definition. The file is still the single source of the default; it just has a `.ts` extension.
 *
 * ── LIQUID CANNOT WALK AN UNKNOWN SHAPE, SO TYPESCRIPT WALKS IT FIRST ──────────────────────────
 *
 * §2.5 asks for "`result` as nested key/value tables (lists of objects become tables)". Liquid has no
 * recursion and `{{ result }}` is refused outright by the escaper, so a template cannot introspect an
 * arbitrary return value — not with any amount of cleverness. {@link defaultContext} therefore
 * flattens the run and the result into a list of ready-made tables, and the template below only
 * loops. The hard part is in TypeScript where it can be tested; the template stays something a
 * person can read.
 *
 * The extra `default` key exists ONLY for this template. An authored template sees exactly the §2.4
 * contract and nothing more, which is why that contract can be published as the whole truth.
 */

import { createHash } from 'node:crypto';

/** A table the default template can render without knowing anything about the shape it came from. */
export interface DefaultTable {
  title: string;
  /** `kv` is a two-column field/value table; `rows` is a list of objects sharing columns. */
  kind: 'kv' | 'rows';
  columns: string[];
  rows: Array<Record<string, string>>;
  /** Set when a list was longer than the row cap, so the template can say so rather than lie. */
  omitted?: number;
}

/** Rows beyond this, per table, are summarised rather than printed. */
const MAX_ROWS = 200;
/** How deep into a nested result the walk goes before it stops describing and starts summarising. */
const MAX_DEPTH = 3;

/**
 * One scalar, as a cell.
 *
 * NOT ESCAPED HERE. These strings go into the context and reach the document through `{{ }}`, which
 * escapes them like any other value — escaping twice would put backslashes in the output.
 */
function cell(value: unknown): string {
  if (value === null || value === undefined) return '';
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'boolean' || typeof value === 'bigint') {
    return String(value);
  }
  if (value instanceof Date) return value.toISOString();
  if (Array.isArray(value)) return `${value.length} item${value.length === 1 ? '' : 's'}`;
  // A nested object that the walk decided not to open: described, not printed. `[object Object]` is
  // the one thing a generated report must never contain.
  return `{${Object.keys(value as object).length} field${Object.keys(value as object).length === 1 ? '' : 's'}}`;
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value) && !(value instanceof Date);
}

/** A list of objects that share a shape becomes a table; anything else becomes a field/value row. */
function tablesFor(title: string, value: unknown, depth: number, out: DefaultTable[]): void {
  if (depth > MAX_DEPTH) {
    // DESCRIBED, NOT DROPPED. Returning here silently was the first version, and it meant a value
    // nested five deep left no trace at all in the report — the reader could not tell the difference
    // between "the workflow returned nothing there" and "the default report declined to go deeper".
    // That is the same confusion an unresolved claim-check ref would cause, and it gets the same
    // treatment: say what is there and say it was not opened.
    out.push({
      title,
      kind: 'kv',
      columns: ['field', 'value'],
      rows: [{ k: 'not expanded', v: cell(value) }],
    });
    return;
  }

  if (Array.isArray(value)) {
    const objects = value.filter(isPlainObject);
    if (objects.length === value.length && value.length > 0) {
      // Union of keys across rows, in first-seen order: a row missing a field gets an empty cell
      // rather than the table losing a column.
      const columns: string[] = [];
      for (const row of objects) for (const k of Object.keys(row)) if (!columns.includes(k)) columns.push(k);
      const shown = objects.slice(0, MAX_ROWS);
      out.push({
        title,
        kind: 'rows',
        columns,
        rows: shown.map((row) => Object.fromEntries(columns.map((c) => [c, cell(row[c])]))),
        ...(objects.length > shown.length ? { omitted: objects.length - shown.length } : {}),
      });
      return;
    }
    // A list of scalars: one column, so the values are readable rather than joined into a sentence.
    const shown = value.slice(0, MAX_ROWS);
    out.push({
      title,
      kind: 'rows',
      columns: ['value'],
      rows: shown.map((v) => ({ value: cell(v) })),
      ...(value.length > shown.length ? { omitted: value.length - shown.length } : {}),
    });
    return;
  }

  if (isPlainObject(value)) {
    const scalars: Array<Record<string, string>> = [];
    const nested: Array<[string, unknown]> = [];
    for (const [k, v] of Object.entries(value)) {
      if (Array.isArray(v) || isPlainObject(v)) nested.push([k, v]);
      else scalars.push({ k, v: cell(v) });
    }
    if (scalars.length) out.push({ title, kind: 'kv', columns: ['field', 'value'], rows: scalars });
    for (const [k, v] of nested) tablesFor(`${title} · ${k}`, v, depth + 1, out);
    return;
  }

  // A bare scalar return value. Still a table, so the template has one shape to render.
  out.push({ title, kind: 'kv', columns: ['field', 'value'], rows: [{ k: 'value', v: cell(value) }] });
}

/**
 * The extra context the default template needs, on top of the §2.4 contract.
 *
 * `run` is flattened here too rather than looped in the template, so the field ORDER is decided in
 * code — a report whose rows move between runs is harder to read than one whose rows do not.
 */
export function defaultContext(run: Record<string, unknown>, result: unknown): {
  run: Array<Record<string, string>>;
  tables: DefaultTable[];
} {
  const order = ['id', 'workflow_id', 'status', 'started_at', 'ended_at', 'duration_s'];
  const rows: Array<Record<string, string>> = [];
  for (const key of order) {
    if (run[key] !== undefined && run[key] !== null) rows.push({ k: key, v: cell(run[key]) });
  }
  const tables: DefaultTable[] = [];
  if (result !== null && result !== undefined) tablesFor('Result', result, 0, tables);
  return { run: rows, tables };
}

/**
 * The template.
 *
 * Whitespace control (`-%}`) is on every loop tag: without it Liquid leaves the newline that followed
 * the tag in the output, and one stray blank line inside a GFM table ends the table. The tables here
 * are built by loops, so every one of those hyphens is load-bearing.
 */
export const DEFAULT_TEMPLATE = `# {{ workflow.name }} · {{ run.status }}

| field | value |
|---|---|
{% for row in default.run -%}
| {{ row.k }} | {{ row.v }} |
{% endfor -%}
{% if run.error %}
## It ended with an error

**{{ run.error.type }}** — {{ run.error.message }}
{% endif %}
{% if default.tables.size > 0 -%}
{% for t in default.tables %}
## {{ t.title }}
{% if t.kind == "kv" %}
| field | value |
|---|---|
{% for row in t.rows -%}
| {{ row.k }} | {{ row.v }} |
{% endfor -%}
{% else %}
|{% for c in t.columns %} {{ c }} |{% endfor %}
|{% for c in t.columns %}---|{% endfor %}
{% for row in t.rows -%}
|{% for c in t.columns %} {{ row[c] }} |{% endfor %}
{% endfor -%}
{% endif -%}
{% if t.omitted %}
{{ t.omitted }} further row{% if t.omitted != 1 %}s{% endif %} are not shown. A workflow that wants them all in its report should return them in a shape it controls, with a \`report.md\` beside its \`workflow.py\`.
{% endif -%}
{% endfor -%}
{% elsif run.status == "completed" %}
This run completed and returned nothing. A workflow puts things in its report by returning them.
{% endif %}
*This is the default report: the workflow folder has no \`report.md\`. Adding one replaces this page entirely.*
`;

/**
 * Identifies the default template in `report_template.template_hash`.
 *
 * §4.6 asks for `default@<kontra version>`. This defaults instead to `default@<digest of the template
 * itself>`, and the reason is that the version is the weaker identifier of the two: the point of a
 * template hash is that re-rendering reproduces a version (acceptance test 12), and a package version
 * does not change when this template's text does — so two different default reports would both be
 * `default@0.1.0` and neither would be reproducible from its own name. A content digest changes
 * exactly when the thing it names changes.
 *
 * It still takes an explicit version, because an install that wants its release in the name should be
 * able to say so, and because that is what the specification asked for.
 */
export function defaultTemplateId(version?: string): string {
  if (version !== undefined && version !== '') return `default@${version}`;
  return `default@${createHash('sha256').update(DEFAULT_TEMPLATE, 'utf8').digest('hex').slice(0, 12)}`;
}
