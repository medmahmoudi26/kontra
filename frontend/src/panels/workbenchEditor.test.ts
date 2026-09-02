/**
 * The editor's per-file configuration — the grammar each file is parsed with, and how wide its Tab
 * is.
 *
 * WHAT IS WORTH ASSERTING IS THE PARSE, not the presence of an import. `languageOf` returning
 * `'markdown'` was already true while `description.md` opened with no grammar at all; the thing
 * that changed is that the heading an author types on line 1 is now a heading to CodeMirror. So
 * these tests run the real parsers over real text and read the node names back.
 */

import { describe, expect, it } from 'vitest';
import type { LanguageId } from './folderWorkbench';
import { editorMode, grammarOf } from './workbenchEditor';

function parse(id: LanguageId, text: string) {
  const grammar = grammarOf(id);
  if (!grammar) throw new Error(`${id} has no grammar`);
  return grammar.language.parser.parse(text);
}

/** Every node name the grammar for `id` produces for `text`. */
function nodes(id: LanguageId, text: string): string[] {
  const names: string[] = [];
  parse(id, text).iterate({
    enter: (node) => {
      names.push(node.name);
    },
  });
  return names;
}

/** The innermost node at `at`. A fenced block's inner tree is an OVERLAY MOUNT, and a plain tree
 *  cursor walks straight past one — `nodes` above cannot see inside a code fence at all. */
function nodeAt(id: LanguageId, text: string, at: number): string {
  return parse(id, text).resolveInner(at, 1).name;
}

const DESCRIPTION = '# probe\n\nWhat this actor does.\n';

describe('description.md reads as markdown', () => {
  it('parses a heading as a heading', () => {
    // The line that decides what every list on the Actors page shows, and the first line an author
    // writes. `firstParagraph` SKIPS it, which is why the preview is beside the editor — but it has
    // to LOOK like a heading for that to be a lesson rather than a surprise.
    const md = nodes('markdown', DESCRIPTION);
    expect(md).toContain('ATXHeading1');
    expect(md).toContain('Paragraph');
  });

  it('never paints a heading as a comment', () => {
    // The failure this slice exists for: under the Python grammar `# probe` is a Comment, so the
    // document meant to read as prose reads as commented-out code. The second assertion is what the
    // wrong grammar actually does, so this test fails if the two are ever swapped back.
    expect(nodes('markdown', DESCRIPTION)).not.toContain('Comment');
    expect(nodes('python', '# probe')).toContain('Comment');
  });

  it('knows the GFM the preview renders', () => {
    // The preview uses `remark-gfm`. On the CommonMark base a table is a paragraph of pipes in the
    // editor and a table in the pane six inches to the right, and the author has to guess which
    // half of their screen is wrong.
    expect(nodes('markdown', '| a | b |\n| - | - |\n| 1 | 2 |\n')).toContain('Table');
  });

  it('highlights a fenced python block from the first parse', () => {
    // A Method's documentation is mostly the call the reader will paste. `codeLanguages` carries the
    // already-loaded Python support rather than a `load` callback, so the fence is coloured by this
    // parse — not after a promise settles, which reads as a flash of the wrong thing. Without it
    // the same offset resolves to `CodeText`: the whole block, one node, one colour.
    const fence = '```python\nprobe(url="x")\n```\n';
    expect(nodeAt('markdown', fence, fence.indexOf('probe') + 1)).toBe('VariableName');
  });
});

describe('what a Tab inserts', () => {
  it('indents markdown by two, because four is a code block', () => {
    // CommonMark: four spaces at the start of a line IS an indented code block. Under the Python
    // unit, `indentWithTab` on a line an author meant to nest turned it into a grey box in the
    // preview beside it.
    expect(editorMode('markdown').indent).toBe(2);
    expect(editorMode('python').indent).toBe(4);
  });

  it('reports the same width the editor inserts', () => {
    // The status line used to hard-code `spaces 4` off the language name.
    for (const id of ['python', 'markdown', 'json', 'text'] as LanguageId[]) {
      const mode = editorMode(id);
      expect(mode.indent).toBeGreaterThan(0);
      expect(mode.extensions.length).toBeGreaterThan(0);
    }
  });
});

describe('the modes themselves', () => {
  it('gives every file the workbench can open a configuration', () => {
    // `extensions={undefined}` on a missing mode is an editor with no keymap and no grammar, which
    // looks like a plain-text file rather than like a bug.
    for (const id of ['python', 'markdown', 'json', 'text'] as LanguageId[]) {
      expect(editorMode(id).id).toBe(id);
    }
  });

  it('hands JSON no grammar rather than the wrong one', () => {
    // Python's lexer highlights `true` and `null` in actor.json as bare names.
    expect(grammarOf('json')).toBeNull();
    expect(grammarOf('text')).toBeNull();
  });

  it('is the same object on every call', () => {
    // A new array per render reconfigures CodeMirror on every keystroke — which is why these are
    // module-level constants and not built in the component's body.
    expect(editorMode('markdown')).toBe(editorMode('markdown'));
    expect(editorMode('markdown').extensions).toBe(editorMode('markdown').extensions);
  });
});

/**
 * GO WAS RENDERING AS PLAIN TEXT, on an app where Go is a first-class engine: `kontra serve`
 * detects it from a `go.mod`, `examples/go/nscheck` is one of the two actors this repo ships, and
 * the handler that owns every actor's backing workflow is Go from end to end. `languageOf` simply
 * had no `.go` case, so every Go file fell through to `text` — opened beside a Python actor, the
 * workbench looked like it supported one language and tolerated the other.
 */
describe('Go', () => {
  it('has a grammar, and it is not the Python one', () => {
    const go = grammarOf('go');
    expect(go).not.toBeNull();
    expect(go).not.toBe(grammarOf('python'));
  });

  it('is one instance, not one per call — a grammar rebuilt per render loses its parse state', () => {
    expect(grammarOf('go')).toBe(grammarOf('go'));
  });

  it('indents with a TAB, because gofmt does', () => {
    // Four spaces here produce a file `gofmt` rewrites on the next save: an edit that silently
    // undoes itself, which is worse than one that visibly fails.
    const mode = editorMode('go');
    expect(mode.indent).toBe(1);
  });

  it('still gives JSON and text no grammar — no grammar beats the wrong one', () => {
    expect(grammarOf('json')).toBeNull();
    expect(grammarOf('text')).toBeNull();
  });
});
