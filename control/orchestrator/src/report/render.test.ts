import { describe, expect, it } from 'vitest';

import type { RefResolution } from './codeTag';
import { renderReport, type EngineDeps } from './engine';
import {
  ALLOWED_NODES,
  SnapshotTooLargeError,
  buildSnapshot,
  countNodes,
  parseMarkdown,
  type MdNode,
} from './render';

/**
 * The pipeline's tests run the REAL engine into the REAL parser, because the thing being asserted is
 * a property of the whole chain: what a value can do to a document. A unit test of the sanitiser
 * alone would pass while the escaping that feeds it was broken.
 *
 * Acceptance tests 2 (table breakout) and 3 (HTML and links) live here.
 */

const deps: EngineDeps = {
  async resolveRef(): Promise<RefResolution> {
    return { bytes: Buffer.from('stored bytes'), fullBytes: 12 };
  },
  redactLatin1: (t) => t,
  redactText: (t) => t,
};

async function snapshotOf(template: string, ctx: Record<string, unknown> = {}) {
  const { markdown, blocks } = await renderReport(template, ctx, deps);
  return { markdown, ...buildSnapshot(markdown, blocks) };
}

/** Every node in a tree, flattened, for assertions about what types survived. */
function allNodes(root: MdNode): MdNode[] {
  return [root, ...(root.children ?? []).flatMap(allNodes)];
}

function find(root: MdNode, type: string): MdNode | undefined {
  return allNodes(root).find((n) => n.type === type);
}

describe('ACCEPTANCE 3: HTML and links', () => {
  it('produces no html node from a raw <script> in the TEMPLATE', async () => {
    const { snapshot } = await snapshotOf('# T\n\n<script>alert(1)</script>\n\nafter\n');
    expect(countNodes(snapshot.root, 'html')).toBe(0);
    // The text after it is still there: dropping the html node does not drop the document.
    expect(JSON.stringify(snapshot.root)).toContain('after');
  });

  it('renders an <img onerror> VALUE as text, not as a node', async () => {
    const { snapshot } = await snapshotOf('{{ v }}', { v: '<img src=x onerror=alert(1)>' });
    expect(countNodes(snapshot.root, 'html')).toBe(0);
    expect(countNodes(snapshot.root, 'image')).toBe(0);
    const text = find(snapshot.root, 'text');
    expect(text?.value).toContain('<img src=x onerror=alert(1)>');
  });

  it('turns a javascript: link into its own words', async () => {
    const { snapshot } = await snapshotOf('[click me](javascript:alert(1))');
    expect(countNodes(snapshot.root, 'link')).toBe(0);
    expect(find(snapshot.root, 'text')?.value).toBe('click me');
  });

  it('turns a data: link into its own words', async () => {
    const { snapshot } = await snapshotOf('[doc](data:text/html;base64,PHNjcmlwdD4=)');
    expect(countNodes(snapshot.root, 'link')).toBe(0);
  });

  it('turns a relative link into its own words, because it means something different wherever it is read', async () => {
    const { snapshot } = await snapshotOf('[runs](../runs/r_1)');
    expect(countNodes(snapshot.root, 'link')).toBe(0);
    expect(find(snapshot.root, 'text')?.value).toBe('runs');
  });

  it('keeps an http and an https link', async () => {
    const { snapshot } = await snapshotOf('[a](http://x.test/p) [b](https://y.test/q)');
    expect(countNodes(snapshot.root, 'link')).toBe(2);
  });

  it('turns an image into its alt text', async () => {
    const { snapshot } = await snapshotOf('![a screenshot](https://x.test/s.png)');
    expect(countNodes(snapshot.root, 'image')).toBe(0);
    expect(find(snapshot.root, 'text')?.value).toBe('a screenshot');
  });

  it('drops a reference definition and keeps the reference\'s words', async () => {
    const { snapshot } = await snapshotOf('see [the docs][d]\n\n[d]: javascript:alert(1)\n');
    expect(countNodes(snapshot.root, 'definition')).toBe(0);
    expect(countNodes(snapshot.root, 'linkReference')).toBe(0);
    expect(JSON.stringify(snapshot.root)).toContain('the docs');
  });

  it('holds nothing outside the allowlist, whatever the template throws at it', async () => {
    const { snapshot } = await snapshotOf(
      '# H\n\n> quote\n\n- a\n- b\n\n| x | y |\n|---|---|\n| 1 | 2 |\n\n---\n\n`code` **b** _i_ ~~s~~\n\n<div>html</div>\n\n![i](https://x.test/i.png)\n\n[l](javascript:1)\n'
    );
    const types = new Set(allNodes(snapshot.root).map((n) => n.type));
    for (const t of types) {
      expect(ALLOWED_NODES.has(t), `node type "${t}" is not on the allowlist but reached a snapshot`).toBe(
        true
      );
    }
  });
});

describe('ACCEPTANCE 2: a hostile value cannot change a table', () => {
  it('lands pipes, a newline and emphasis in ONE cell, leaving the column count alone', async () => {
    const { snapshot } = await snapshotOf(
      '| a | b |\n|---|---|\n| {{ v }} | ok |\n',
      { v: 'x | y\n**z**' }
    );
    const table = find(snapshot.root, 'table')!;
    const rows = table.children as MdNode[];
    expect(rows).toHaveLength(2);
    for (const row of rows) {
      expect((row.children as MdNode[]).length, 'the value added a column').toBe(2);
    }
    const firstCell = (rows[1]!.children as MdNode[])[0]!;
    // One text node holding the literal value, not emphasis and not a second cell.
    expect(countNodes(firstCell, 'emphasis')).toBe(0);
    expect(countNodes(firstCell, 'strong')).toBe(0);
    expect(firstCell.children?.map((c) => c.value).join('')).toBe('x | y **z**');
  });

  it('cannot end the table early with a value that looks like a row', async () => {
    const { snapshot } = await snapshotOf('| a |\n|---|\n| {{ v }} |\n', { v: '|\n| injected |' });
    const table = find(snapshot.root, 'table')!;
    expect((table.children as MdNode[])).toHaveLength(2);
  });
});

describe('blocks are matched by their marker, never by position', () => {
  it('attaches bytes to the tag\'s block and leaves an author\'s literal fence empty-handed', async () => {
    const { snapshot } = await snapshotOf('```sh\nls -la\n```\n\n{% code "http", v %}\n', { v: 'GET / HTTP/1.1\r\n' });
    const codes = allNodes(snapshot.root).filter((n) => n.type === 'code');
    expect(codes).toHaveLength(2);
    expect(codes[0]!.lang).toBe('sh');
    expect(codes[0]!.blockId, 'the author\'s own fence was given a run\'s bytes').toBeUndefined();
    expect(codes[1]!.blockId).toBe('b1');
    expect(Object.keys(snapshot.blocks)).toEqual(['b1']);
    expect(Buffer.from(snapshot.blocks.b1!.b64, 'base64').toString('latin1')).toBe('GET / HTTP/1.1\r\n');
  });

  it('cannot have a block stolen by a literal fence imitating the marker', async () => {
    // A template author writes an info string that looks like plumbing. The real block still wins,
    // because an id is consumed once and the imitation arrives second.
    const { snapshot } = await snapshotOf('{% code "http", v %}\n\n```text kontra-block=b1\nnot mine\n```\n', {
      v: 'real',
    });
    const codes = allNodes(snapshot.root).filter((n) => n.type === 'code');
    expect(codes[0]!.blockId).toBe('b1');
    expect(codes[1]!.blockId).toBeUndefined();
    expect(Buffer.from(snapshot.blocks.b1!.b64, 'base64').toString()).toBe('real');
  });

  it('strips the marker so no reader ever sees it', async () => {
    const { snapshot } = await snapshotOf('{% code "http", "x" %}', {});
    for (const node of allNodes(snapshot.root)) {
      expect(node.meta, 'the plumbing leaked into the stored tree').toBeUndefined();
    }
  });

  it('strips remark\'s source positions, which describe a string nothing keeps', async () => {
    const { snapshot } = await snapshotOf('# H\n\ntext\n');
    for (const node of allNodes(snapshot.root)) {
      expect(node.position).toBeUndefined();
    }
  });

  it('carries the ref and the unresolved reason into the snapshot block', async () => {
    const { markdown, blocks } = await renderReport('{% code "http", v %}', { v: { ref: 'cc://x' } }, {
      ...deps,
      async resolveRef(): Promise<RefResolution> {
        return { unresolved: 'no actor served this ref within 5s' };
      },
    });
    const { snapshot } = buildSnapshot(markdown, blocks);
    expect(snapshot.blocks.b1!.unresolved).toBe('no actor served this ref within 5s');
    expect(snapshot.blocks.b1!.ref).toBe('cc://x');
    expect(snapshot.blocks.b1!.source).toBe('ref');
  });
});

describe('the snapshot size cap', () => {
  it('measures the SERIALISED json, so base64 inflation counts against the cap', async () => {
    // 1 MiB of bytes is ~1.37 MiB of base64. A cap applied to the Markdown would miss that.
    const big = Buffer.alloc(300 * 1024, 0x41);
    const { bytes } = await snapshotOf('{% code "text", v %}', { v: { b64: big.toString('base64') } });
    expect(bytes).toBeGreaterThan(300 * 1024 * 1.3);
  });

  it('throws over the cap, and the message says which knob to turn', async () => {
    const big = Buffer.alloc(200 * 1024, 0x41);
    const { markdown, blocks } = await renderReport('{% code "text", v %}', { v: { b64: big.toString('base64') } }, deps);
    expect(() => buildSnapshot(markdown, blocks, { limitBytes: 1024 })).toThrow(SnapshotTooLargeError);
    expect(() => buildSnapshot(markdown, blocks, { limitBytes: 1024 })).toThrow(
      /KONTRA_REPORT_SNAPSHOT_LIMIT_BYTES/
    );
  });
});

describe('the parser itself', () => {
  it('reads GFM tables, which plain CommonMark does not', () => {
    const tree = parseMarkdown('| a |\n|---|\n| 1 |\n');
    expect(countNodes(tree as MdNode, 'table')).toBe(1);
  });

  it('reads strikethrough, also GFM', () => {
    expect(countNodes(parseMarkdown('~~x~~') as MdNode, 'delete')).toBe(1);
  });
});
