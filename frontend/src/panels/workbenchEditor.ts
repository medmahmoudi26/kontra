/**
 * What CodeMirror is configured with for the file the workbench has open.
 *
 * A MODULE RATHER THAN ARRAYS IN THE COMPONENT, because the extension list must be built once: a
 * new array on every render reconfigures the editor on every keystroke. That was already why
 * `FolderWorkbench` held its Python extensions at module level — this is the same constant, moved
 * somewhere a test can reach it. The component imports `WorkerPane`, which imports xterm, which
 * touches `self` at module load, so nothing that lives in that file can be asserted on in this
 * suite.
 *
 * `description.md` GETS THE MARKDOWN GRAMMAR, and it is not a cosmetic upgrade. Until
 * `@codemirror/lang-markdown` was a dependency the workbench opened it with no grammar at all —
 * deliberately, because the Python one paints every `#` heading as a comment, so the document that
 * is meant to read as prose reads as commented-out code, and the heading is exactly the line an
 * author writes first.
 *
 * THE GRAMMAR IS GFM, not CommonMark, because the preview beside the editor renders with
 * `remark-gfm`: with the CommonMark base a table would render as a table on the right and as four
 * lines of pipes and dashes on the left, and the author would have had to guess which half of their
 * own screen was wrong.
 */

import { indentWithTab } from '@codemirror/commands';
import { go } from '@codemirror/lang-go';
import { markdown, markdownLanguage } from '@codemirror/lang-markdown';
import { python } from '@codemirror/lang-python';
import { LanguageDescription, LanguageSupport, indentUnit } from '@codemirror/language';
import { keymap } from '@codemirror/view';
import type { LanguageId } from './folderWorkbench';
import { PY_INDENT } from './workflowSource';

/** One instance each, not one per call: a grammar rebuilt per render is a parser rebuilt per
 *  render, and the editor loses its parse state along with it. */
const PYTHON = python();

/**
 * Go, for the same reason Python is here: it is a real engine, not a curiosity. `kontra serve`
 * detects it from a `go.mod`, `examples/go/nscheck` is one of the two actors this repo ships, and
 * the handler that owns every actor's backing workflow is Go throughout. A Go actor opened beside a
 * Python one and rendered as flat text made the workbench look like it only really supported one of
 * them.
 */
const GO = go();

/**
 * Languages a fenced block inside a description is highlighted as.
 *
 * `support` and not `load`, so the block is highlighted by the first parse: a `load` callback makes
 * the fence plain text until a promise settles, which for a file this short is a visible flash of
 * the wrong thing.
 *
 * Python because that is what a Method's documentation shows — the call the reader is going to
 * paste. Without this the snippet that is the point of the file renders flat while the same code
 * one row up in the file list is coloured.
 */
const FENCED = [
  LanguageDescription.of({
    name: 'python',
    alias: ['py', 'python3'],
    extensions: ['py'],
    support: PYTHON,
  }),
  LanguageDescription.of({
    name: 'go',
    alias: ['golang'],
    extensions: ['go'],
    support: GO,
  }),
];

const MARKDOWN = markdown({ base: markdownLanguage, codeLanguages: FENCED });

/**
 * How wide one Tab is, per language.
 *
 * MARKDOWN IS TWO, AND PYTHON IS FOUR, and the difference is not taste: four spaces at the start of
 * a line is an indented CODE BLOCK in CommonMark, so `indentWithTab` under the Python unit turns a
 * paragraph an author tried to nest into a grey box — in the preview, right beside the line they
 * typed it on. Python's four is `PY_INDENT`, because a stray inconsistent indent there is a
 * `TabError` the worker dies on several clicks later.
 */
const MD_INDENT = '  ';

/** The grammar for a file, or `null` where no grammar is better than the wrong one — JSON under
 *  Python's lexer highlights `true` and `null` as bare names. */
export function grammarOf(id: LanguageId): LanguageSupport | null {
  if (id === 'python') return PYTHON;
  if (id === 'go') return GO;
  if (id === 'markdown') return MARKDOWN;
  return null;
}

/** `unit` is the string Tab inserts, not a width: `PY_INDENT` is passed through verbatim, so the
 *  workbench cannot drift from the Workflows editor by rebuilding four spaces of its own. */
function mode(id: LanguageId, unit: string) {
  const grammar = grammarOf(id);
  return {
    id,
    /** What the status line reports — the width the editor actually inserts, not a guess. */
    indent: unit.length,
    extensions: [...(grammar ? [grammar] : []), indentUnit.of(unit), keymap.of([indentWithTab])],
  };
}

/** Every language `languageOf` can return has a mode, so a new extension cannot fall through to
 *  `undefined` and take the editor's `extensions` prop with it. */
const MODES = {
  python: mode('python', PY_INDENT),
  /* A TAB, because gofmt indents with tabs and `indentWithTab` inserting four spaces into a .go
     file produces something `gofmt` rewrites on the next save — an edit that silently un-does
     itself. The status line reports width 1, which is what this actually inserts. */
  go: mode('go', '\t'),
  markdown: mode('markdown', MD_INDENT),
  json: mode('json', PY_INDENT),
  text: mode('text', PY_INDENT),
};

export type EditorMode = (typeof MODES)[LanguageId];

/** The editor configuration for an open file. Stable across renders — see the header. */
export function editorMode(id: LanguageId): EditorMode {
  return MODES[id];
}
