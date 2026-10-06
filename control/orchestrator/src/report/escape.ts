/**
 * MARKDOWN ESCAPING: the one function standing between a scanned target's bytes and a document
 * somebody reads.
 *
 * ── WHAT IT IS FOR ──────────────────────────────────────────────────────────────────────────────
 *
 * A report template is Markdown with Liquid holes in it. The template is written by the workflow
 * author and is structure; everything that arrives through a hole is DATA, and in this product the
 * data came off a target. A value that can write Markdown can move a table border, start a heading,
 * open a code fence, or end the cell it was supposed to sit in — which turns "the report said what
 * the run found" into "the report said what the target wanted it to say".
 *
 * So every `{{ }}` output goes through {@link markdownEscape}, registered as LiquidJS's
 * `outputEscape`. Template text does not: that is the difference between structure and data, and it
 * is the whole escaping model. `{{ "**bold**" }}` renders as literal asterisks even though the
 * author typed them, because the author had a way to write bold and chose a hole instead.
 *
 * ── ESCAPE MAXIMALLY HERE, SERIALISE MINIMALLY LATER ────────────────────────────────────────────
 *
 * This function escapes every character CommonMark gives meaning to, context-free, which makes the
 * raw Liquid output ugly: `kontra\-fleet`, `12\,480`. That is deliberate and it costs nothing,
 * because nobody reads this stage. The pipeline is escape → parse to mdast → drop HTML → store the
 * TREE. Both the console and the Markdown export are generated from that tree, where a serialiser
 * re-escapes only what its own position needs. A reader of the exported `.md` therefore sees clean
 * prose, and the escaping that made the parse safe has already done its work and gone.
 *
 * Getting this backwards — escaping lightly to keep the intermediate readable — is how a table
 * breakout survives to the tree.
 *
 * ── A NEWLINE IS NOT A CHARACTER TO ESCAPE, IT IS ONE TO REMOVE ─────────────────────────────────
 *
 * There is no escape for a line break in Markdown: `\` before a newline IS a hard break, and inside
 * a table row a raw newline ends the row no matter what precedes it. A value therefore cannot be
 * allowed to contain one. All three forms (`\r\n`, `\r`, `\n`) become a single space, so a
 * multi-line value lands in one cell as one line — lossy in layout, exact in content, and the only
 * option that keeps the table's column count under the template author's control.
 *
 * ── CONTROL CHARACTERS GO, BECAUSE AN INVISIBLE DIFFERENCE IS A LIE ─────────────────────────────
 *
 * C0 controls other than tab are stripped and tab becomes a space. A NUL in a document is at best
 * a tool that stops reading at it and at worst a file that `grep` calls binary and silently finds
 * nothing in — a failure this codebase has already paid for once, in `packages/svelte/src/infra/load.ts`,
 * where a raw NUL in a template literal made three greps return nothing for a string the file
 * contained ten times.
 *
 * DEL (0x7F) is stripped too, which §4.2 of the spec does not list. §9.2 of the same spec renders
 * "other C0 control characters and DEL" as visible markers inside code blocks, so the spec's two
 * halves disagree about whether DEL is worth naming; this follows the stricter half. Inside a code
 * block DEL survives and is SHOWN, because that is evidence; in prose it is invisible and is
 * removed.
 *
 * ── WHAT THIS FUNCTION REFUSES TO GUESS ────────────────────────────────────────────────────────
 *
 * An object or an array is a RENDER ERROR naming the path, never `[object Object]` and never
 * Liquid's comma-join. `{{ result.price_changes }}` is a template bug — the author meant to loop —
 * and a report that prints `[object Object]` has turned a bug into a finished document that nobody
 * can tell is wrong. Failing names the mistake while the author is still looking at it.
 */

/**
 * Characters CommonMark and GFM give meaning to, escaped with a backslash.
 *
 * `\` IS FIRST IN THE CLASS AND THAT IS NOT COSMETIC: one regex pass handles it with everything
 * else, so a backslash in the value is escaped exactly once. Escaping the set first and the
 * backslash afterwards would double every backslash this function itself added.
 *
 * `|` earns its place in GFM alone: it is the table cell separator, and it is the character a value
 * most plausibly contains by accident (a shell pipeline, a regex alternation, a User-Agent string).
 */
const MARKDOWN_SPECIALS = /[\\`*_{}[\]<>()#+\-!|~]/g;

/** `\r\n` before the singles, so a CRLF becomes ONE space rather than two. */
const LINE_BREAKS = /\r\n|\r|\n/g;

/** C0 except tab (handled separately, as a space) and the line breaks (handled above), plus DEL. */
const CONTROLS = /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/g;

/**
 * Thrown when a value cannot be rendered into Markdown at all, rather than being rendered wrongly.
 *
 * Carries no value in its message — only the shape and what to do about it. A render error reaches
 * a stored snapshot and from there a console page, so it is read by people who are not the author,
 * and a message quoting the offending data would put target bytes in an error string that every
 * layer logs.
 */
export class MarkdownEscapeError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'MarkdownEscapeError';
  }
}

/**
 * Escape one `{{ }}` output for inclusion in Markdown.
 *
 * INVARIANT: the return value contains no character that can change the document's structure —
 * no unescaped Markdown punctuation, no line break of any kind, no control character. Whatever a
 * target put in this value, a reader sees it as text in the position the template put it.
 *
 * `null` and `undefined` become the empty string, which is NOT the same as a missing variable:
 * LiquidJS's `strictVariables` has already turned an unknown NAME into an error before this runs.
 * A field that exists and is null is an author's "nothing here", and printing nothing is right.
 */
export function markdownEscape(value: unknown): string {
  if (value === null || value === undefined) return '';

  if (typeof value === 'number' || typeof value === 'boolean' || typeof value === 'bigint') {
    // No escaping needed and none applied: `String(42)` and `String(true)` cannot contain Markdown.
    // NaN and Infinity print as themselves, which is more honest than an empty cell.
    return String(value);
  }

  if (typeof value === 'string') return escapeString(value);

  if (Array.isArray(value)) {
    throw new MarkdownEscapeError(
      'cannot print an array directly: loop over it with {% for %} and print its fields, ' +
        'or pick one element'
    );
  }

  if (typeof value === 'object') {
    // A Date is an object but has one obvious rendering, and a report full of timestamps would
    // otherwise force every author through a filter. ISO 8601, UTC, seconds — the form the rest of
    // this control plane writes.
    if (value instanceof Date) return escapeString(value.toISOString());
    throw new MarkdownEscapeError(
      'cannot print an object directly: pick a field from it, or loop over it with {% for %}'
    );
  }

  // Functions and symbols reach here only from a context this module built, which would be a bug in
  // the renderer rather than in a template. Named rather than coerced.
  throw new MarkdownEscapeError(`cannot print a value of type ${typeof value}`);
}

/**
 * The string half, in a fixed order that matters.
 *
 * 1. Controls out FIRST, so a stripped NUL cannot sit between a backslash and the character it
 *    escapes.
 * 2. Tabs and line breaks to spaces, before the backslash pass, so no escape is applied to
 *    whitespace that is about to be replaced anyway.
 * 3. Backslash-escape the specials LAST, in one pass, including the backslash itself.
 */
function escapeString(raw: string): string {
  return raw
    .replace(CONTROLS, '')
    .replace(LINE_BREAKS, ' ')
    .replace(/\t/g, ' ')
    .replace(MARKDOWN_SPECIALS, (ch) => `\\${ch}`);
}
