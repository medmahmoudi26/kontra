/**
 * What a widget tile calls itself (ADR 0020, slice 7b).
 *
 * The precedence is explicit protocol first, then the document, then the caller's fallback (the
 * Machine and window, which is what every other tile shows):
 *
 *   `__TITLE__:` → `__FILE__:`'s basename → the document's first heading → the fallback
 *
 * TWO DIVERGENCES FROM tmuxy's `getWidgetTitle`, both because the string ends up in chrome:
 *
 *  - **No URL-derived titles.** tmuxy pulls the filename out of the first `http(s)://…` it finds in
 *    the content. On this wall the content is scanner output, so that rule lets a crawled page choose
 *    the words above a tile — and a tile header is the one label an operator reads without
 *    questioning it. A heading is a document's own claim about itself; a URL found in the body is not.
 *  - **Everything is flattened and capped** ({@link WIDGET_TITLE_CAP}). A "title" with 4000
 *    characters, a newline, or an ANSI escape in it is a broken header, and pane text can contain all
 *    three.
 *
 * Pure, so the precedence is pinned by tests rather than by looking at a screenshot.
 */

import { WIDGET_TITLE_CAP, fileLabel, type Widget } from './parseWidget';

/** How far into a document a heading may be and still be its title. A heading below this is a
 * section, not the subject. */
const HEADING_SCAN_LINES = 20;

function flatten(raw: string): string {
  const flat = raw.replace(/\s+/g, ' ').trim();
  if (flat === '') return '';
  return flat.length > WIDGET_TITLE_CAP ? `${flat.slice(0, WIDGET_TITLE_CAP - 1)}…` : flat;
}

/** The first ATX heading in the opening lines of a document, without its hashes. Setext headings
 * (`===` underlines) are deliberately not read: a table's separator row and a horizontal rule are
 * both close enough to one that guessing costs more than it gains. */
export function firstHeading(body: string): string | undefined {
  const lines = body.split('\n', HEADING_SCAN_LINES);
  for (const line of lines) {
    const m = /^\s{0,3}(#{1,6})\s+(.+?)\s*#*\s*$/.exec(line);
    if (m) {
      const text = flatten(m[2] ?? '');
      if (text !== '') return text;
    }
  }
  return undefined;
}

/**
 * The title for a widget, or `fallback` when the document offers none.
 *
 * `fallback` is what the tile already shows (`<machine> · <window>`), so a widget that says nothing
 * about itself does not lose the identity every other tile carries.
 */
export function getWidgetTitle(widget: Widget, fallback?: string): string | undefined {
  if (widget.title !== undefined && widget.title !== '') return flatten(widget.title);
  if (widget.file !== undefined && widget.file !== '') {
    const label = flatten(fileLabel(widget.file));
    if (label !== '') return label;
  }
  const heading = firstHeading(widget.body);
  if (heading !== undefined) return heading;
  return fallback === undefined ? undefined : flatten(fallback);
}
