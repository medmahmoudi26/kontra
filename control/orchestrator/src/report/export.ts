/**
 * A SNAPSHOT, BACK OUT AS MARKDOWN OR AS ONE HTML FILE.
 *
 * ── BOTH ARE WRITTEN FROM THE TREE, WHICH IS THE WHOLE POINT ───────────────────────────────────
 *
 * The stored snapshot is mdast, not a string. So an export is a walk over a closed set of node types
 * — the 18 `ALLOWED_NODES` — and anything else cannot appear because it never reached the tree. That
 * is why the HTML export has no sanitiser: there is no HTML in the input to sanitise. A `<script>` a
 * template author typed became nothing at parse time, and a value that looked like markup became
 * text.
 *
 * ── MINIMAL ESCAPING HERE, MAXIMAL ESCAPING UPSTREAM ───────────────────────────────────────────
 *
 * `escape.ts` escapes every Markdown special, context-free, so the parse was safe. That output was
 * never meant to be read: a raw-`.md` reader would see `kontra\-fleet` and `12\,480`. This serialiser
 * re-escapes only what its own position needs — a `|` inside a table cell, a leading `#` that would
 * start a heading — so the exported file is clean prose that round-trips to the same tree.
 *
 * Getting that backwards is how a table breakout survives: escape lightly at the parse boundary and
 * the tree is already wrong, and nothing downstream can fix it.
 *
 * ── THE EXPORTS ARE ALWAYS REDACTED ────────────────────────────────────────────────────────────
 *
 * §5: "Export always uses the redacted form." The snapshot's `blocks[id].b64` IS the redacted bytes —
 * the originals never entered it — so this module cannot leak an unredacted credential even by
 * mistake. The only path to the originals is the audited reveal route, which reads a different table.
 */

import type { MdNode, ReportSnapshot, SnapshotBlock } from './render';

/** `\` before the characters that would otherwise be structure in the position they land in. */
function escapeInline(text: string): string {
  // Only the characters that can start or alter inline structure. A `-` mid-sentence is a hyphen; a
  // `#` mid-sentence is a hash. Position-sensitive cases are handled by their own writers below.
  return text.replace(/([\\`*_[\]<>])/g, '\\$1');
}

/** The same, plus the pipe, for a table cell — where a pipe ends the cell.
 *
 *  ONE PASS, BACKSLASH INCLUDED. Escaping the pipe in a second `replace` over the first one's output
 *  was correct (the first had already doubled every backslash) but read as a sanitizer that forgets
 *  backslashes — CodeQL js/incomplete-sanitization — and a reader has to know the order to see it is
 *  safe. A single class with every structural character cannot be reordered wrong. */
function escapeCell(text: string): string {
  return text.replace(/([\\`*_[\]<>|])/g, '\\$1');
}

/** A fence one backtick longer than the longest run inside, minimum three — `codeTag.ts`'s rule. */
function fenceFor(content: string): string {
  let longest = 0;
  let run = 0;
  for (const ch of content) {
    if (ch === '`') {
      run += 1;
      if (run > longest) longest = run;
    } else run = 0;
  }
  return '`'.repeat(Math.max(3, longest + 1));
}

function textOf(node: MdNode): string {
  if (typeof node.value === 'string') return node.value;
  return (node.children ?? []).map(textOf).join('');
}

/** "showing 1.0 MiB of 4.3 MiB" — the marker §9.2 shows as chrome and an export must say in words. */
export function truncationNote(block: SnapshotBlock): string | undefined {
  if (block.unresolved) return `this block was not read: ${block.unresolved}`;
  if (!block.truncated) return undefined;
  return (
    `showing ${humanBytes(Buffer.from(block.b64, 'base64').length)} of ${humanBytes(block.fullBytes)}` +
    ' — the full bytes are downloadable from the report page'
  );
}

/**
 * A size a person reads.
 *
 * THE UNIT IS CHOSEN PER VALUE, which the first version did not do: a fixed MiB made a 2 KiB block
 * truncated out of a 4 MiB object read "showing 0.0 MiB of 4.1 MiB" — a measurement that says nothing
 * about the thing it measures. Found by looking at the rendered page rather than by a test.
 *
 * The console carries the same function (`report/snapshot.ts`'s `humanBytes`) because it is a different
 * repository; the two are one rule in two spellings, and an exported note and an on-screen note must
 * read identically or a reader comparing them has to translate.
 */
export function humanBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MiB`;
}

// --- Markdown ------------------------------------------------------------------------------------

function inlineMd(nodes: readonly MdNode[]): string {
  return nodes.map(oneInlineMd).join('');
}

function oneInlineMd(node: MdNode): string {
  switch (node.type) {
    case 'text':
      return escapeInline(String(node.value ?? ''));
    case 'strong':
      return `**${inlineMd(node.children ?? [])}**`;
    case 'emphasis':
      return `_${inlineMd(node.children ?? [])}_`;
    case 'delete':
      return `~~${inlineMd(node.children ?? [])}~~`;
    case 'inlineCode': {
      const value = String(node.value ?? '');
      // A span's delimiter must be longer than any backtick run inside it, and needs padding spaces
      // when the content starts or ends with a backtick.
      const ticks = fenceFor(value).slice(0, Math.max(1, fenceFor(value).length - 2));
      const pad = value.startsWith('`') || value.endsWith('`') ? ' ' : '';
      return `${ticks}${pad}${value}${pad}${ticks}`;
    }
    case 'break':
      // Two trailing spaces is the other spelling and is invisible in a diff. A backslash is explicit.
      return '\\\n';
    case 'link':
      return `[${inlineMd(node.children ?? [])}](${String(node.url ?? '')})`;
    default:
      return escapeInline(textOf(node));
  }
}

function blockMd(node: MdNode, snapshot: ReportSnapshot, depth = 0): string[] {
  switch (node.type) {
    case 'root':
      return (node.children ?? []).flatMap((c) => blockMd(c, snapshot, depth));
    case 'heading': {
      const level = Math.min(Math.max(Number(node.depth ?? 1), 1), 6);
      return [`${'#'.repeat(level)} ${inlineMd(node.children ?? [])}`];
    }
    case 'paragraph':
      return [inlineMd(node.children ?? [])];
    case 'thematicBreak':
      return ['---'];
    case 'blockquote':
      return [
        (node.children ?? [])
          .flatMap((c) => blockMd(c, snapshot, depth))
          .join('\n\n')
          .split('\n')
          .map((line) => (line ? `> ${line}` : '>'))
          .join('\n'),
      ];
    case 'list': {
      const ordered = node.ordered === true;
      const start = Number(node.start ?? 1);
      const items = (node.children ?? []).map((item, i) => {
        const marker = ordered ? `${start + i}. ` : '- ';
        const body = (item.children ?? [])
          .flatMap((c) => blockMd(c, snapshot, depth + 1))
          .join('\n\n')
          .split('\n');
        // Continuation lines are indented by the marker's width, which is what keeps a nested list a
        // child rather than a sibling.
        return body.map((line, n) => (n === 0 ? marker + line : ' '.repeat(marker.length) + line)).join('\n');
      });
      return [items.join('\n')];
    }
    case 'code': {
      const value = String(node.value ?? '');
      const fence = fenceFor(value);
      const lang = typeof node.lang === 'string' ? node.lang : '';
      const block = typeof node.blockId === 'string' ? snapshot.blocks[node.blockId] : undefined;
      const note = block ? truncationNote(block) : undefined;
      const fenced = `${fence}${lang}\n${value}${value.endsWith('\n') ? '' : '\n'}${fence}`;
      // The note goes AFTER the fence, in italics, because it is about the block rather than in it —
      // putting it inside would make it bytes the block does not actually contain.
      return note ? [fenced, `_${escapeInline(note)}_`] : [fenced];
    }
    case 'table': {
      const rows = (node.children ?? []).map((row) =>
        (row.children ?? []).map((cell) => inlineMdCell(cell))
      );
      if (rows.length === 0) return [];
      const header = rows[0]!;
      const width = header.length;
      const lines = [
        `| ${header.join(' | ')} |`,
        `|${'---|'.repeat(width)}`,
        ...rows.slice(1).map((r) => `| ${pad(r, width).join(' | ')} |`),
      ];
      return [lines.join('\n')];
    }
    default:
      // A node type the allowlist admits but this writer has not special-cased contributes its text
      // rather than vanishing. The allowlist is closed, so this is a belt on braces.
      return [escapeInline(textOf(node))];
  }
}

function inlineMdCell(cell: MdNode): string {
  return (cell.children ?? []).map((n) => (n.type === 'text' ? escapeCell(String(n.value ?? '')) : oneInlineMd(n))).join('');
}

/** A short row is padded rather than shortening the table — GFM requires a rectangle. */
function pad(row: string[], width: number): string[] {
  return row.length >= width ? row.slice(0, width) : [...row, ...Array(width - row.length).fill('')];
}

/** The snapshot as Markdown. Redacted, by construction — see the header. */
export function toMarkdown(snapshot: ReportSnapshot): string {
  const body = blockMd(snapshot.root, snapshot).filter((s) => s !== '').join('\n\n');
  const warnings = (snapshot.warnings ?? []).map((w) => `> **Note:** ${escapeInline(w)}`);
  return [...warnings, body].filter((s) => s !== '').join('\n\n') + '\n';
}

// --- HTML ----------------------------------------------------------------------------------------

/** The five characters that can change HTML structure. Everything that reaches HTML goes through. */
function h(text: string): string {
  return text
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function inlineHtml(nodes: readonly MdNode[]): string {
  return nodes.map(oneInlineHtml).join('');
}

function oneInlineHtml(node: MdNode): string {
  switch (node.type) {
    case 'text':
      return h(String(node.value ?? ''));
    case 'strong':
      return `<strong>${inlineHtml(node.children ?? [])}</strong>`;
    case 'emphasis':
      return `<em>${inlineHtml(node.children ?? [])}</em>`;
    case 'delete':
      return `<del>${inlineHtml(node.children ?? [])}</del>`;
    case 'inlineCode':
      return `<code>${h(String(node.value ?? ''))}</code>`;
    case 'break':
      return '<br>';
    case 'link': {
      const url = String(node.url ?? '');
      // The scheme was already checked by the sanitiser — a non-http(s) link is text by the time it
      // reaches a snapshot. Re-checked here anyway, because this writer is the last thing between a
      // stored tree and a file somebody opens in a browser, and an export is forwarded.
      const safe = /^https?:\/\//i.test(url);
      return safe
        ? `<a href="${h(url)}" rel="noreferrer noopener">${inlineHtml(node.children ?? [])}</a>`
        : inlineHtml(node.children ?? []);
    }
    default:
      return h(textOf(node));
  }
}

function blockHtml(node: MdNode, snapshot: ReportSnapshot): string {
  switch (node.type) {
    case 'root':
      return (node.children ?? []).map((c) => blockHtml(c, snapshot)).join('\n');
    case 'heading': {
      const level = Math.min(Math.max(Number(node.depth ?? 1), 1), 6);
      return `<h${level}>${inlineHtml(node.children ?? [])}</h${level}>`;
    }
    case 'paragraph':
      return `<p>${inlineHtml(node.children ?? [])}</p>`;
    case 'thematicBreak':
      return '<hr>';
    case 'blockquote':
      return `<blockquote>${(node.children ?? []).map((c) => blockHtml(c, snapshot)).join('\n')}</blockquote>`;
    case 'list': {
      const tag = node.ordered === true ? 'ol' : 'ul';
      const start = node.ordered === true && Number(node.start ?? 1) !== 1 ? ` start="${Number(node.start)}"` : '';
      const items = (node.children ?? [])
        .map((item) => `<li>${(item.children ?? []).map((c) => blockHtml(c, snapshot)).join('\n')}</li>`)
        .join('\n');
      return `<${tag}${start}>\n${items}\n</${tag}>`;
    }
    case 'code': {
      const value = String(node.value ?? '');
      const lang = typeof node.lang === 'string' ? node.lang : '';
      const block = typeof node.blockId === 'string' ? snapshot.blocks[node.blockId] : undefined;
      const note = block ? truncationNote(block) : undefined;
      const pre = `<pre><code class="lang-${h(lang)}">${h(value)}</code></pre>`;
      return note ? `${pre}\n<p class="note">${h(note)}</p>` : pre;
    }
    case 'table': {
      const rows = node.children ?? [];
      if (rows.length === 0) return '';
      const head = (rows[0]!.children ?? []).map((c) => `<th>${inlineHtml(c.children ?? [])}</th>`).join('');
      const body = rows
        .slice(1)
        .map(
          (r) => `<tr>${(r.children ?? []).map((c) => `<td>${inlineHtml(c.children ?? [])}</td>`).join('')}</tr>`
        )
        .join('\n');
      return `<div class="scroll"><table>\n<thead><tr>${head}</tr></thead>\n<tbody>\n${body}\n</tbody>\n</table></div>`;
    }
    default:
      return `<p>${h(textOf(node))}</p>`;
  }
}

/**
 * ONE SELF-CONTAINED FILE: inline CSS, no scripts, no external requests.
 *
 * §7.3. No `<script>` because nothing here needs one and a report is forwarded to people who did not
 * generate it; no external stylesheet or font because opening the file must not become a request to
 * anybody; `<div class="scroll">` around a table because a wide table in a narrow window should scroll
 * rather than push the page sideways, which is the same rule the console follows.
 *
 * The print rules live here too, so a printed export and a printed console page break in the same
 * places: a page break before every `h2`, code blocks that wrap instead of clipping, and no shadow.
 */
export function toHtml(snapshot: ReportSnapshot, title: string): string {
  const warnings = (snapshot.warnings ?? [])
    .map((w) => `<p class="warning">${h(w)}</p>`)
    .join('\n');
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${h(title)}</title>
<style>
:root { color-scheme: light dark; }
body { margin: 0 auto; max-width: 54rem; padding: 2rem 1rem 4rem;
  font: 15px/1.6 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif; }
h1 { font-size: 1.6rem; letter-spacing: -0.01em; }
h2 { font-size: 1.2rem; margin-top: 2rem; }
code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px; }
pre { padding: 0.9rem 1rem; overflow-x: auto; border: 1px solid rgba(128,128,128,0.35); border-radius: 6px; }
code { background: rgba(128,128,128,0.14); padding: 0.1em 0.3em; border-radius: 3px; }
pre code { background: none; padding: 0; }
.scroll { overflow-x: auto; }
table { border-collapse: collapse; width: 100%; font-size: 13.5px; }
th, td { text-align: left; padding: 0.45rem 0.7rem; border-bottom: 1px solid rgba(128,128,128,0.3); }
th { font-weight: 600; }
blockquote { margin: 1rem 0; padding: 0.1rem 1rem; border-left: 3px solid rgba(128,128,128,0.4); }
.note { font-size: 12.5px; opacity: 0.75; margin-top: -0.4rem; }
.warning { padding: 0.6rem 0.9rem; border: 1px solid rgba(190,140,0,0.5);
  background: rgba(190,140,0,0.08); border-radius: 6px; font-size: 13.5px; }
@media print {
  body { max-width: none; padding: 0; }
  h2 { break-before: page; }
  pre { white-space: pre-wrap; word-break: break-word; border: 1px solid #999; }
  .scroll { overflow: visible; }
}
</style>
</head>
<body>
${warnings}
${blockHtml(snapshot.root, snapshot)}
</body>
</html>
`;
}
