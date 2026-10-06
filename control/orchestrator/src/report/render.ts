/**
 * FROM LIQUID OUTPUT TO A STORED SNAPSHOT: parse, sanitise, attach bytes, cap, serialise.
 *
 * ── THE ORDER IS THE SECURITY MODEL ────────────────────────────────────────────────────────────
 *
 *     escape  →  parse to mdast  →  drop what is not allowed  →  store the TREE
 *
 * Escaping (escape.ts) makes the parse safe. Parsing turns a string into a tree. Dropping happens on
 * the tree, where a node's type is a fact rather than a guess about what some bytes might mean. And
 * the TREE is what is stored — not HTML, not the Markdown — so the console renders from structure
 * and never from a string it has to trust. There is no `innerHTML` anywhere downstream because there
 * is no HTML anywhere upstream.
 *
 * Reversing any two of those steps breaks it. Sanitising the string before parsing is regex-against-
 * a-grammar, which loses. Storing HTML means something has to re-parse it.
 *
 * ── THE NODE SET IS AN ALLOWLIST, FOR THE SAME REASON THE FILTER SET IS ────────────────────────
 *
 * {@link ALLOWED_NODES} is what a snapshot may contain. A node type that is not in it is converted
 * to its text or dropped — never passed through "because remark produced it". Two consequences, both
 * wanted: the console's `ReportView` has a FINITE job (one component per allowed type, and a reviewer
 * can check the list is covered), and a remark or GFM upgrade that starts emitting a new node type
 * cannot quietly introduce a rendering path nobody wrote.
 *
 * ── WHAT IS DROPPED, AND WHY EACH ONE ──────────────────────────────────────────────────────────
 *
 *   • `html`, inline and block. The second defence after escaping: a template author who types
 *     `<script>` gets nothing, and acceptance test 3 asserts no html node survives.
 *   • A link whose URL is not `http:` or `https:` becomes its own text. `javascript:` is the obvious
 *     one; `data:` can carry a whole document; a relative path means something different in every
 *     place the report is read.
 *   • An image becomes its alt text, per spec §11: images are reserved and not built, and an `<img>`
 *     that silently fetches from a target's host would turn reading a report into a request to them.
 *   • Reference-style links and footnotes become text. They need a second pass over `definition`
 *     nodes to resolve, and a definition that fails to resolve renders as literal brackets — a
 *     failure mode with no upside in a document generated from a template. Inline links work.
 *   • Frontmatter (`yaml`, `toml`) is dropped: a report has metadata, and it is in the store's
 *     columns, not in a block at the top of the document that only some readers understand.
 */

import remarkGfm from 'remark-gfm';
import remarkParse from 'remark-parse';
import { unified } from 'unified';

import type { CodeBlockRecord } from './codeTag';

/**
 * The mdast subset a snapshot may hold.
 *
 * Declared locally rather than imported from `@types/mdast`, which is a transitive dependency this
 * package does not name. Writing the shapes out is also the point: this file and the console's
 * renderer have to agree on them, and the agreement is easier to check against a list than against
 * somebody else's package.
 */
export interface MdNode {
  type: string;
  children?: MdNode[];
  value?: string;
  [key: string]: unknown;
}

/** The marker the `{% code %}` tag writes into a fence's info string, e.g. ```` ```http b1 ````. */
const BLOCK_META = /^kontra-block=(b\d+)$/;

/** Node types a stored snapshot may contain. Everything else is converted to text or dropped. */
export const ALLOWED_NODES: ReadonlySet<string> = new Set([
  'root',
  'paragraph',
  'heading',
  'text',
  'strong',
  'emphasis',
  'delete',
  'inlineCode',
  'code',
  'blockquote',
  'list',
  'listItem',
  'table',
  'tableRow',
  'tableCell',
  'thematicBreak',
  'break',
  'link',
]);

/** Schemes a link may use. Everything else becomes the link's own text. */
const ALLOWED_SCHEMES = new Set(['http:', 'https:']);

/** 5 MiB per snapshot, per spec §4.4. The serialised JSON, blocks included, is what is measured. */
export const DEFAULT_SNAPSHOT_LIMIT_BYTES = 5 * 1024 * 1024;

export function snapshotLimitBytes(): number {
  const raw = Number(process.env.KONTRA_REPORT_SNAPSHOT_LIMIT_BYTES);
  return Number.isFinite(raw) && raw > 0 ? Math.floor(raw) : DEFAULT_SNAPSHOT_LIMIT_BYTES;
}

/** A block as the snapshot stores it: redacted bytes only, base64 for JSON. */
export interface SnapshotBlock {
  lang: string;
  /** The REDACTED bytes. The originals live in `report_secrets`, behind the audited reveal route. */
  b64: string;
  redacted: boolean;
  truncated: boolean;
  /** Length before truncation, so a reader can be told "showing 1.0 MiB of 4.3 MiB". */
  fullBytes: number;
  source: CodeBlockRecord['source'];
  ref?: string;
  /** Present when a ref was not read. The block's visible text is a marker, not bytes. */
  unresolved?: string;
}

export interface ReportSnapshot {
  /** The SNAPSHOT FORMAT's version, which is not the report's version number. */
  v: 1;
  root: MdNode;
  blocks: Record<string, SnapshotBlock>;
  /**
   * Things a reader should know about how this report was made, rather than about what it says.
   *
   * IN THE SNAPSHOT AND NOT A COLUMN, because §4.6 says "a warning recorded in the snapshot" and
   * because a warning belongs with the bytes it qualifies: a snapshot copied, exported or re-read
   * carries its own caveats. The console shows them above the report.
   */
  warnings?: string[];
}

export class SnapshotTooLargeError extends Error {
  constructor(bytes: number, limit: number) {
    super(
      `the rendered report is ${bytes} bytes, over the ${limit}-byte snapshot cap ` +
        '(KONTRA_REPORT_SNAPSHOT_LIMIT_BYTES): return fewer rows from the workflow, or move large ' +
        'values to a claim-check ref so only the part shown is stored'
    );
    this.name = 'SnapshotTooLargeError';
  }
}

/** Parse Markdown to mdast with GFM tables, strikethrough and task lists. No HTML is interpreted. */
export function parseMarkdown(markdown: string): MdNode {
  return unified().use(remarkParse).use(remarkGfm).parse(markdown) as unknown as MdNode;
}

/** The text a node and its descendants carry, for the cases where a node is replaced by its text. */
function textOf(node: MdNode): string {
  if (typeof node.value === 'string') return node.value;
  if (!node.children) return '';
  return node.children.map(textOf).join('');
}

function isAllowedUrl(url: unknown): boolean {
  if (typeof url !== 'string') return false;
  try {
    // PARSED WITH NO BASE, which is the whole point: a url that cannot stand alone has no scheme of
    // its own, and `URL` throwing is how that is detected. Supplying a base instead — which this
    // function did at first — RESOLVES `../runs/r_1` to `https://…/runs/r_1`, whose protocol then
    // passes the check: the guard admitted precisely the input it was written to reject, and a test
    // caught it. Protocol-relative `//host/path` fails here for the same reason, and should: which
    // scheme it means depends on where the report is being read.
    return ALLOWED_SCHEMES.has(new URL(url).protocol);
  } catch {
    return false;
  }
}

/**
 * Walk the tree and enforce the allowlist.
 *
 * Returns the nodes that replace the one given: none (dropped), one (kept or converted), or several
 * (a disallowed container's children, promoted). Promoting children rather than dropping them is why
 * a `javascript:` link keeps its words — the words are a finding, the href is not.
 */
function sanitise(node: MdNode): MdNode[] {
  // HTML goes first and goes entirely: it is the one type whose CONTENT is the risk.
  if (node.type === 'html') return [];

  // Frontmatter: metadata belongs in columns, not in the document.
  if (node.type === 'yaml' || node.type === 'toml') return [];

  // An image becomes its alt text. §11 reserves images; a fetch to a target's host on page load is
  // the specific thing being avoided.
  if (node.type === 'image' || node.type === 'imageReference') {
    const alt = typeof node.alt === 'string' ? node.alt : '';
    return alt ? [{ type: 'text', value: alt }] : [];
  }

  // Reference-style links and footnotes: converted to the text they carry, never resolved.
  if (node.type === 'linkReference' || node.type === 'footnoteReference') {
    const text = textOf(node) || String(node.identifier ?? '');
    return text ? [{ type: 'text', value: text }] : [];
  }
  if (node.type === 'definition' || node.type === 'footnoteDefinition') return [];

  // A link with a scheme that is not http(s) keeps its words and loses its destination.
  if (node.type === 'link' && !isAllowedUrl(node.url)) {
    const text = textOf(node);
    return text ? [{ type: 'text', value: text }] : [];
  }

  const children = node.children ? node.children.flatMap(sanitise) : undefined;

  if (!ALLOWED_NODES.has(node.type)) {
    // An unknown container contributes its children; an unknown leaf contributes its text. Neither
    // contributes itself. This is what makes a new remark node type inert rather than unhandled.
    if (children && children.length) return children;
    const text = textOf(node);
    return text ? [{ type: 'text', value: text }] : [];
  }

  const out: MdNode = { ...node };
  if (children) out.children = children;
  else delete out.children;
  // `position` is remark's source offsets. They describe the Liquid output, which nothing keeps, so
  // they would be bytes of noise in every stored snapshot — and a reader who thought they pointed at
  // report.md would be wrong.
  delete out.position;
  return [out];
}

/**
 * Attach each `{% code %}` record to the code node that came from it.
 *
 * MATCHED BY THE MARKER IN THE INFO STRING, not by position. A template may contain its own literal
 * fenced block — that is ordinary Markdown and stays ordinary Markdown — so counting code nodes and
 * zipping them against the records would attach a run's bytes to an author's example as soon as
 * anyone wrote one.
 *
 * An id is consumed at most once. A literal fence whose info string imitates the marker therefore
 * cannot steal a block: the second claim finds the id already taken and is treated as what it is, a
 * fence with an odd info string.
 */
function attachBlocks(root: MdNode, records: readonly CodeBlockRecord[]): Record<string, SnapshotBlock> {
  const byId = new Map(records.map((r) => [r.id, r]));
  const used = new Set<string>();
  const blocks: Record<string, SnapshotBlock> = {};

  const visit = (node: MdNode): void => {
    if (node.type === 'code') {
      const meta = typeof node.meta === 'string' ? node.meta.trim() : '';
      const match = BLOCK_META.exec(meta);
      // The info string is never shown, and is cleared either way: it is this pipeline's plumbing,
      // and a reader seeing `kontra-block=b1` beside a code block would reasonably wonder what it is.
      delete node.meta;
      if (!match) return;
      const id = match[1]!;
      const record = byId.get(id);
      if (!record || used.has(id)) return;
      used.add(id);
      node.blockId = id;
      blocks[id] = {
        lang: record.lang,
        b64: record.bytes.toString('base64'),
        redacted: record.redacted,
        truncated: record.truncated,
        fullBytes: record.fullBytes,
        source: record.source,
        ...(record.ref ? { ref: record.ref } : {}),
        ...(record.unresolved ? { unresolved: record.unresolved } : {}),
      };
    }
    for (const child of node.children ?? []) visit(child);
  };

  visit(root);
  return blocks;
}

/**
 * Build the stored snapshot from Liquid's output and the blocks that render produced.
 *
 * THROWS {@link SnapshotTooLargeError} over the cap, and the measurement is of the SERIALISED JSON
 * rather than of the Markdown: blocks are base64 inside it, so a 1 MiB block costs 1.37 MiB of
 * snapshot, and a cap applied to the Markdown would let four such blocks through a 5 MiB limit.
 */
export function buildSnapshot(
  markdown: string,
  records: readonly CodeBlockRecord[],
  opts: { limitBytes?: number } = {}
): { snapshot: ReportSnapshot; bytes: number } {
  const parsed = parseMarkdown(markdown);
  const sanitised = sanitise(parsed);
  // `sanitise` returns a list because any node may be dropped or promoted, `root` included. A root
  // that sanitised away leaves an empty document rather than a crash.
  const root: MdNode =
    sanitised.length === 1 && sanitised[0]!.type === 'root'
      ? sanitised[0]!
      : { type: 'root', children: sanitised };

  const blocks = attachBlocks(root, records);
  const snapshot: ReportSnapshot = { v: 1, root, blocks };
  const bytes = Buffer.byteLength(JSON.stringify(snapshot), 'utf8');
  const limit = opts.limitBytes ?? snapshotLimitBytes();
  if (bytes > limit) throw new SnapshotTooLargeError(bytes, limit);
  return { snapshot, bytes };
}

/** Every `html` node in a tree, for the test that asserts there are none. */
export function countNodes(root: MdNode, type: string): number {
  let n = root.type === type ? 1 : 0;
  for (const child of root.children ?? []) n += countNodes(child, type);
  return n;
}
