/**
 * Pane content as a widget — detection and parsing, pure (ADR 0020, slice 7b).
 *
 * A tile whose captured screen OPENS with `__KONTRA_WIDGET__:markdown` is rendered as markdown
 * instead of into xterm. That is tmuxy's model and it is cheap for us because a snapshot is already
 * text: `tmux capture-pane -p -e` prints a screen, so a Worker that wants to say something
 * structured — a crawl summary, a queue table, a topology — prints the marker and then markdown.
 *
 * PANE CONTENT IS UNTRUSTED, AND THAT IS THE POINT OF THIS FILE. A pane holds crawler and scanner
 * output: a crawled `<title>`, a response header or a scan finding is chosen by whoever is being
 * scanned. Three decisions here follow from that, and each is pinned by a test:
 *
 *  1. **The marker must be the FIRST non-empty line.** tmuxy scans every line for its marker,
 *     because there a pane is a shell and the marker arrives under a prompt. We do not: a fleet pane
 *     is `journalctl -fu`, and a scanner that logs a crawled page whose title happens to be
 *     `__KONTRA_WIDGET__:markdown` would otherwise flip the tile into a renderer of its own output.
 *     Controlling the top of a 50-line screen is a much narrower capability than appearing anywhere
 *     in it.
 *
 *  2. **Metadata is only read from the header block** — the contiguous run of `__TITLE__:` /
 *     `__FILE__:` / `__SEQ__:` lines (and blanks) immediately after the marker. Scanning the whole
 *     screen would let a crawled string inside the BODY choose the tile's title, which is a phishing
 *     surface on the one piece of chrome an operator reads without thinking.
 *
 *  3. **`__FILE__:` IS METADATA AND IS NEVER FETCHED.** In tmuxy it names a path the UI then reads
 *     through its own `GET /api/file?path=…`. We have no such route and must not add one: that route
 *     is an arbitrary file read on the Controller, and the string driving it comes out of a scanner's
 *     output. So `file` is displayed as a label and nothing more, `seq` is a render key, and
 *     `widgets/untrusted.test.ts` greps this directory to keep it that way.
 *
 * THE CAP IS A FALLBACK, NOT A TRUNCATION. Past {@link WIDGET_TEXT_CAP} this returns `null` and the
 * tile keeps painting into xterm, whose 2000-line scrollback is already bounded. A pane that emits a
 * megabyte of "markdown" must not become the page's memory profile, and truncating it instead would
 * render a *fragment* of a document as if it were the document.
 *
 * EVERYTHING HERE IS PURE — no React, no DOM, no fetch — so detection is testable without a browser
 * and slice 7a's tile can branch on it before it decides what to mount.
 */

/** The marker, and the only widget kind implemented. `<kind>` is parsed rather than assumed so an
 * unknown kind is a *refusal with a sentence* instead of a markdown render of something else. */
export const WIDGET_MARKER = '__KONTRA_WIDGET__:';

/** The kinds this build renders. A one-member union today, and a union on purpose: the next kind
 * (a table, an image) must be added here rather than by loosening the marker test. */
export const WIDGET_KINDS = ['markdown'] as const;
export type WidgetKind = (typeof WIDGET_KINDS)[number];

/**
 * The most pane text this will parse, in UTF-16 code units of the RAW capture.
 *
 * 64 KiB is ~6 full 200×50 screens, so a legitimate widget never comes close: a snapshot is one
 * screen and a live attach's increments are what accumulate. Measured against the raw text rather
 * than the stripped text because the raw text is what the page is actually holding.
 */
export const WIDGET_TEXT_CAP = 64 * 1024;

/** How much of an over-cap screen is inspected for the marker, so the tile can still explain
 * itself. Bounded for the same reason the cap exists. */
const FALLBACK_SCAN = 4 * 1024;

/** The most characters a widget title may contribute to the tile's header. */
export const WIDGET_TITLE_CAP = 120;

export interface MarkdownWidget {
  kind: 'markdown';
  /** From `__TITLE__:`, else `__FILE__:`'s basename, else absent. Sanitised and capped: it goes in a
   * header, and pane text chose it. */
  title?: string;
  /** The markdown itself — everything after the header block. */
  body: string;
  /** `__FILE__:` VERBATIM, as a label. Nothing reads a file. */
  file?: string;
  /** `__SEQ__:` — a version stamp a producer bumps when it re-emits. Used as a render key so a new
   * screen with the same body is not mistaken for the same content. */
  seq?: string;
}

export type Widget = MarkdownWidget;

/** ESC, as a code rather than an escape literal: a source file that contains a real control
 * character is a file every editor, diff and grep treats as binary. */
const ESC = 0x1b;
const BEL = 0x07;
const BRACKET = 0x5b; // [
const CLOSE_BRACKET = 0x5d; // ]
const BACKSLASH = 0x5c;
const TAB = 0x09;
const NEWLINE = 0x0a;
const SPACE = 0x20;
const DEL = 0x7f;

/**
 * Strip the terminal control sequences a captured screen carries.
 *
 * `capture-pane -e` emits SGR (`ESC [ 1;32 m`) deliberately — that is how the wall keeps colour —
 * and the streamer prefixes a full screen with `ESC [ H ESC [ 2 J` so a repaint does not append. A
 * live attach adds alternate-screen (`ESC [ ? 1049 h`) and a scroll region. None of that is
 * markdown, and leaving it in would both break the parse and put raw escapes into the DOM.
 *
 * Written as a scanner rather than a chain of regexes because the grammar is small and the failure
 * mode of a regex here is unbounded: a malformed OSC with no terminator, which a crawled URL can
 * produce, must eat to the end of the screen ONCE rather than backtrack.
 */
export function stripAnsi(text: string): string {
  let out = '';
  let i = 0;
  while (i < text.length) {
    const code = text.charCodeAt(i);

    if (code === ESC) {
      i += 1;
      const next = i < text.length ? text.charCodeAt(i) : -1;

      if (next === BRACKET) {
        // CSI: parameter bytes (0x30–0x3f), intermediates (0x20–0x2f), one final byte (0x40–0x7e).
        i += 1;
        while (i < text.length && text.charCodeAt(i) >= 0x30 && text.charCodeAt(i) <= 0x3f) i += 1;
        while (i < text.length && text.charCodeAt(i) >= 0x20 && text.charCodeAt(i) <= 0x2f) i += 1;
        if (i < text.length) i += 1;
        continue;
      }

      if (next === CLOSE_BRACKET) {
        // OSC: runs to BEL or to ST (`ESC \`). An unterminated one ends the string, which is the
        // bounded choice: the alternative is treating the rest of the screen as content.
        i += 1;
        while (i < text.length) {
          const c = text.charCodeAt(i);
          if (c === BEL) {
            i += 1;
            break;
          }
          if (c === ESC && i + 1 < text.length && text.charCodeAt(i + 1) === BACKSLASH) {
            i += 2;
            break;
          }
          i += 1;
        }
        continue;
      }

      // A two-character escape (`ESC M`, `ESC ( B`, …) or a trailing lone ESC.
      if (i < text.length) i += 1;
      continue;
    }

    // Control characters that are not content. Tab and newline are; a lone CR is a cursor move.
    if ((code < SPACE && code !== TAB && code !== NEWLINE) || code === DEL) {
      i += 1;
      continue;
    }

    out += text[i];
    i += 1;
  }
  return out;
}

/** Decode one binary frame's payload into pane text. The frame is UTF-8 terminal bytes — the same
 * bytes `TerminalTile` hands to xterm — so a widget check on the live path costs one decode. */
export function paneText(payload: Uint8Array): string {
  return stripAnsi(new TextDecoder().decode(payload));
}

/** Collapse to one line, drop what a header cannot show, and cap. Pane text chose this string. */
function sanitiseTitle(raw: string): string {
  const flat = raw.replace(/\s+/g, ' ').trim();
  if (flat === '') return '';
  return flat.length > WIDGET_TITLE_CAP ? `${flat.slice(0, WIDGET_TITLE_CAP - 1)}…` : flat;
}

/** `__NAME__:value` on a header line, or null. */
function metaLine(line: string): { key: string; value: string } | null {
  const m = /^__([A-Z]+)__:(.*)$/.exec(line.trim());
  if (!m) return null;
  return { key: m[1] ?? '', value: (m[2] ?? '').trim() };
}

/** The last path segment of a `__FILE__:` value, for a title. Purely a string operation — no path
 * resolution, because nothing here touches a filesystem. */
export function fileLabel(file: string): string {
  const parts = file.split(/[\\/]/).filter((p) => p !== '');
  return parts[parts.length - 1] ?? file;
}

/**
 * Detect and parse a widget in captured pane text.
 *
 * Returns `null` — meaning "render this in xterm, as usual" — for anything that is not unmistakably
 * a widget: no marker on the first non-empty line, an unknown kind, an empty body, or text past
 * {@link WIDGET_TEXT_CAP}.
 */
export function parseWidget(text: string): Widget | null {
  if (typeof text !== 'string' || text.length === 0) return null;
  // The cap is checked FIRST, before anything walks the input: the point is to not process a
  // megabyte, and a scan that rejects it afterwards has already paid for it.
  if (text.length > WIDGET_TEXT_CAP) return null;

  const lines = stripAnsi(text).split('\n');
  let i = 0;
  while (i < lines.length && (lines[i] ?? '').trim() === '') i += 1;
  const first = (lines[i] ?? '').trim();
  if (!first.startsWith(WIDGET_MARKER)) return null;

  const kind = first.slice(WIDGET_MARKER.length).trim().toLowerCase();
  if (!(WIDGET_KINDS as readonly string[]).includes(kind)) return null;

  // The header block: metadata and blanks, contiguous, immediately after the marker. The first line
  // that is neither ends it, and everything from there is body.
  let file: string | undefined;
  let seq: string | undefined;
  let title: string | undefined;
  let j = i + 1;
  for (; j < lines.length; j += 1) {
    const line = lines[j] ?? '';
    if (line.trim() === '') continue;
    const meta = metaLine(line);
    if (!meta) break;
    if (meta.key === 'FILE' && meta.value !== '') file = meta.value;
    else if (meta.key === 'SEQ' && meta.value !== '') seq = meta.value;
    else if (meta.key === 'TITLE' && meta.value !== '') title = sanitiseTitle(meta.value);
    // An unknown `__…__:` line is consumed rather than rendered: it is protocol we do not implement,
    // and showing it as markdown would put a producer's private metadata on the wall.
  }

  // Trailing whitespace per line goes (a captured screen is padded to the pane's width), leading and
  // trailing blank lines go, and the document ends with exactly ONE newline — normalised rather than
  // preserved so a body is the same string whether the marker was the whole screen or the top of it.
  const body = `${lines
    .slice(j)
    .map((l) => l.replace(/\s+$/, ''))
    .join('\n')
    .replace(/^\n+/, '')
    .replace(/\n+$/, '')}\n`;

  // A marker with nothing under it is not a document. Rendering it would replace a tile's screen
  // with an empty box, which reads as a broken stream.
  if (body.trim() === '') return null;

  const widget: MarkdownWidget = { kind: 'markdown', body };
  if (file !== undefined) widget.file = file;
  if (seq !== undefined) widget.seq = seq;
  const resolved = title ?? (file !== undefined ? sanitiseTitle(fileLabel(file)) : undefined);
  if (resolved !== undefined && resolved !== '') widget.title = resolved;
  return widget;
}

/** {@link parseWidget} straight off a binary frame — what a tile has in hand. */
export function parseWidgetFrame(payload: Uint8Array): Widget | null {
  return parseWidget(paneText(payload));
}

/**
 * Why a screen that LOOKS like a widget was not rendered as one.
 *
 * A refusal has to be visible: a producer that prints the marker and gets a plain terminal has no
 * other way to find out why, and "the tile silently ignored my document" is the failure this
 * sentence exists to prevent. `null` when the text is not claiming to be a widget at all.
 */
export function describeWidgetFallback(text: string): string | null {
  if (typeof text !== 'string' || text === '') return null;
  const head = stripAnsi(text.slice(0, FALLBACK_SCAN));
  const line = head.split('\n').find((l) => l.trim() !== '')?.trim() ?? '';
  if (!line.startsWith(WIDGET_MARKER)) return null;
  // Total rather than "call me only after a null": a caller that asks about a screen which parsed
  // fine gets `null`, not a sentence explaining a refusal that did not happen. The second parse is
  // bounded by the cap and only runs for text already claiming to be a widget.
  if (parseWidget(text) !== null) return null;
  if (text.length > WIDGET_TEXT_CAP) {
    return (
      `this pane emitted ${text.length} characters of widget content and the cap is ` +
      `${WIDGET_TEXT_CAP} — showing the terminal instead, which is bounded by its own scrollback`
    );
  }
  const kind = line.slice(WIDGET_MARKER.length).trim().toLowerCase();
  if (!(WIDGET_KINDS as readonly string[]).includes(kind)) {
    return `unknown widget kind ${JSON.stringify(kind)} — this build renders ${WIDGET_KINDS.join(', ')}`;
  }
  return 'this widget had no content under its marker — showing the terminal instead';
}
