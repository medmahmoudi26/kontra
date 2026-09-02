/**
 * Building markdown SAFELY — the other half of rendering it safely (ADR 0020, slice 7b).
 *
 * Three of the four widget surfaces generate their own markdown (the detail drawer, the topology
 * diagram, the run summaries) out of values that came from somewhere else: a Pulumi stack output, a
 * tmux session name, a materialisation error string, an actor's name. `Markdown.tsx` makes sure the
 * RENDERER cannot be talked into executing anything; this file makes sure the DOCUMENT cannot be
 * talked into being a different document.
 *
 * The three injections that matter, each with the function that closes it:
 *
 *  - A value containing a newline plus `## ` adds headings and paragraphs to a document that was
 *    supposed to contain one row — {@link cell} and {@link line} flatten.
 *  - A value containing `|` splits a GFM table cell into two, silently shifting every column after it
 *    — {@link cell} escapes it. This one is not hypothetical: a materialisation error is a
 *    stringified exception, and Python's `KeyError: 'a|b'` is enough.
 *  - A value containing three backticks CLOSES A FENCE, so a string inside a ```mermaid block can
 *    end the block and start whatever it likes — including another fence. {@link fenceBody} is what
 *    the topology generator runs its labels through.
 *
 * The caps are part of the escaping, not politeness: a 4 MB "run name" in a table cell is a
 * renderer problem however well it is escaped.
 */

/** The most any single interpolated value may contribute. Generous for a name, far below the point
 * where one value dominates a document. */
export const VALUE_CAP = 200;

function truncate(value: string, cap: number): string {
  return value.length > cap ? `${value.slice(0, cap - 1)}…` : value;
}

/** Flatten to one line and drop control characters. Everything below funnels through this: a value's
 * own newlines are the cheapest way to change a generated document's structure. */
export function line(value: unknown, cap = VALUE_CAP): string {
  const raw = typeof value === 'string' ? value : String(value ?? '');
  let out = '';
  for (let i = 0; i < raw.length; i += 1) {
    const code = raw.charCodeAt(i);
    // Tabs, newlines and everything else in C0 become a single space; a run collapses below.
    out += code < 0x20 || code === 0x7f ? ' ' : raw[i];
  }
  return truncate(out.replace(/\s+/g, ' ').trim(), cap);
}

/**
 * One GFM table cell.
 *
 * `|` is escaped rather than removed so the value is still readable — the operator needs to see the
 * error text, and an escaped pipe renders as a pipe. Backslashes are escaped first, or a value ending
 * in one would escape our own escape.
 */
export function cell(value: unknown, cap = VALUE_CAP): string {
  const flat = line(value, cap);
  if (flat === '') return '—';
  return flat.replace(/\\/g, '\\\\').replace(/\|/g, '\\|');
}

/**
 * A value as inline code.
 *
 * Backticks in the value are replaced (not escaped) with a modifier-grave lookalike: the correct
 * markdown escape for a backtick inside a code span is to change the fence length, which is a
 * per-value decision this would have to get right every time. A command an operator copies must not
 * be able to end its own span.
 */
export function inlineCode(value: unknown, cap = VALUE_CAP): string {
  const flat = line(value, cap).replace(/`/g, 'ˋ');
  return flat === '' ? '—' : `\`${flat}\``;
}

/**
 * A body destined for a fenced block, with every fence-closing sequence neutralised.
 *
 * Newlines SURVIVE here — a fence's content is lines — so this is the one function that cannot simply
 * flatten. What it removes instead is the ability to end the fence: a run of three or more backticks
 * or tildes at the start of a line, plus C0 control characters, which inside a ```mermaid block would
 * otherwise reach the parser.
 */
export function fenceBody(value: string, cap = 32 * 1024): string {
  const cleaned = value
    .split('\n')
    .map((raw) => {
      let out = '';
      for (let i = 0; i < raw.length; i += 1) {
        const code = raw.charCodeAt(i);
        out += (code < 0x20 && code !== 0x09) || code === 0x7f ? ' ' : raw[i];
      }
      return out.replace(/^(\s*)([`~]{3,})/, '$1ˋˋˋ');
    })
    .join('\n');
  return truncate(cleaned, cap);
}

/** A fenced block. The language tag is fixed by the caller (never by data), and the body is scrubbed
 * by {@link fenceBody}. */
export function fence(language: string, body: string): string {
  return ['```' + language, fenceBody(body), '```'].join('\n');
}

/** A two-column definition table — the shape every summary here uses for scalars. Rows whose value is
 * `undefined` are dropped, so an absent field is absent rather than "undefined". */
export function definitionTable(rows: ReadonlyArray<readonly [string, unknown]>): string {
  const kept = rows.filter(([, value]) => value !== undefined && value !== null && value !== '');
  if (kept.length === 0) return '';
  return [
    '| field | value |',
    '| --- | --- |',
    ...kept.map(([key, value]) => `| ${cell(key, 60)} | ${cell(value)} |`),
  ].join('\n');
}
