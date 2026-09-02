/**
 * The editor's state for one registered folder: which files are in it, which one is open, and what
 * is in the buffer for each.
 *
 * A BUFFER PER FILE, NOT ONE BUFFER. The Workflows editor holds a single `source` string because it
 * opens a single file; a folder has `actor.py`, `actor.json` and `description.md`, and the first
 * thing anybody does is edit the code, click description.md to check what it claims, and click back.
 * With one buffer that round trip silently discards whatever was typed — the click that loses the
 * work is the same click as the one that does not. So an edited file keeps its text until it is
 * saved, and the list marks which files are still unsaved.
 *
 * DIRTY IS COMPUTED, not flagged. `text !== saved` rather than a boolean set on every keystroke:
 * typing a character and deleting it again left the Workflows editor claiming unsaved changes, and
 * the operator cannot tell that state from a real one.
 *
 * A REFUSED SAVE KEEPS THE BUFFER. `writeInside` refuses a name that is not a bare filename and
 * anything over 512 KB, and the server's sentence is the whole answer — but a refusal that also
 * dropped the text would turn "your file is too big" into "your file is gone". `refused` touches
 * only the error.
 */

import type { SourceFile, SourceKind } from '../run/api';

/** One file's text, and the text the server last confirmed for it. */
export interface FileBuffer {
  text: string;
  /** What was on disk as of the last successful read or write. `text !== saved` is dirty. */
  saved: string;
}

export interface WorkbenchState {
  /** The folder's files, as the server listed them. */
  files: SourceFile[];
  /** The open file's name, or '' before the first one lands. */
  selected: string;
  buffers: Record<string, FileBuffer>;
  /** The server's own sentence about the last failed read or write, verbatim. */
  error: string | null;
}

export function emptyWorkbench(): WorkbenchState {
  return { files: [], selected: '', buffers: {}, error: null };
}

/**
 * The file list landed — at mount, and again on every reload.
 *
 * A CLEAN BUFFER IS DROPPED, A DIRTY ONE IS NOT, and both halves are the point. Dropped, because
 * the reload button exists for the case where the folder changed underneath the browser (a `git
 * pull`, an editor in a terminal) and a kept buffer would show yesterday's file while the row
 * beside it reported today's size. Kept when it is dirty, because nothing the server says is a
 * reason to discard what somebody typed — the reload has no idea it is competing with an author.
 *
 * At mount there are no buffers, so this is the same as keeping them.
 */
export function listed(state: WorkbenchState, files: SourceFile[], kind: SourceKind): WorkbenchState {
  const selected = state.selected || firstToOpen(files, kind);
  const buffers: Record<string, FileBuffer> = {};
  for (const [name, buffer] of Object.entries(state.buffers)) {
    if (buffer.text !== buffer.saved) buffers[name] = buffer;
  }
  return { ...state, files, selected, buffers, error: null };
}

/**
 * Which file an operator meant when they clicked the Actor rather than a filename.
 *
 * THE CODE, not the manifest. `filesIn` sorts alphabetically, so "the first file" is `actor.json` —
 * four lines of name and version, and never the file anyone opened the folder to change.
 */
export function firstToOpen(files: SourceFile[], kind: SourceKind): string {
  const names = files.map((f) => f.name);
  const preferred = kind === 'actor'
    ? ['actor.py', 'actor.go', 'main.py', 'actor.json']
    : ['workflow.py', 'workflow.json'];
  for (const want of preferred) if (names.includes(want)) return want;
  return names[0] ?? '';
}

/**
 * A file's content arrived from the server.
 *
 * A LATE READ DOES NOT LAND ON TYPING. Clicking two files quickly leaves two reads in flight, and
 * the slower one used to arrive after the operator had started editing the file it was for —
 * replacing what they had typed with what was on disk, with no save and no undo.
 */
export function opened(state: WorkbenchState, name: string, text: string): WorkbenchState {
  const held = state.buffers[name];
  const buffers =
    held && held.text !== held.saved
      ? state.buffers
      : { ...state.buffers, [name]: { text, saved: text } };
  return { ...state, selected: name, buffers, error: null };
}

/**
 * Switch to another file in the folder.
 *
 * The buffers are not touched — that IS the feature. A file with unsaved edits comes back with them.
 */
export function selectFile(state: WorkbenchState, name: string): WorkbenchState {
  return { ...state, selected: name, error: null };
}

/** The operator typed. */
export function edited(state: WorkbenchState, name: string, text: string): WorkbenchState {
  const held = state.buffers[name];
  return {
    ...state,
    buffers: { ...state.buffers, [name]: { text, saved: held?.saved ?? text } },
  };
}

/**
 * The server wrote the file.
 *
 * `sent` is the text that went over the wire, NOT the buffer as it is now: a save of a large file
 * takes a round trip, and typing during it must leave the file dirty again rather than have the
 * response mark work that was never written as saved.
 */
export function savedFile(
  state: WorkbenchState,
  name: string,
  sent: string,
  file: SourceFile
): WorkbenchState {
  const held = state.buffers[name] ?? { text: sent, saved: sent };
  return {
    ...state,
    error: null,
    buffers: { ...state.buffers, [name]: { ...held, saved: sent } },
    files: state.files.some((f) => f.name === file.name)
      ? state.files.map((f) => (f.name === file.name ? file : f))
      : [...state.files, file].sort((a, b) => a.name.localeCompare(b.name)),
  };
}

/** The server refused — too large, or a name it will not write. The buffer is untouched. */
export function refused(state: WorkbenchState, message: string): WorkbenchState {
  return { ...state, error: message };
}

/**
 * Start a file the folder does not have yet — how `description.md` gets written at all.
 *
 * `saved: ''` is not a placeholder, it is the truth: there is nothing on disk, so every character
 * of the template is an unsaved change and the Save button is live from the first render. The row
 * carries no size until the write comes back.
 */
export function startedFile(state: WorkbenchState, name: string, template: string): WorkbenchState {
  if (state.buffers[name]) return { ...state, selected: name, error: null };
  return {
    ...state,
    selected: name,
    error: null,
    buffers: { ...state.buffers, [name]: { text: template, saved: '' } },
  };
}

/** One row of the file list. `bytes === null` is a file that has not been written yet. */
export interface FileRow {
  name: string;
  bytes: number | null;
  dirty: boolean;
}

/**
 * What the file list draws: the folder's files, plus any buffer that is not on disk yet.
 *
 * Without the second half, `description.md` started from the button would have an editor and no row
 * — nothing in the list to click back to once you opened `actor.py` to check something.
 */
export function fileRows(state: WorkbenchState): FileRow[] {
  const onDisk = new Set(state.files.map((f) => f.name));
  const rows: FileRow[] = state.files.map((f) => ({
    name: f.name,
    bytes: f.bytes,
    dirty: isDirty(state, f.name),
  }));
  for (const name of Object.keys(state.buffers)) {
    if (!onDisk.has(name)) rows.push({ name, bytes: null, dirty: isDirty(state, name) });
  }
  return rows.sort((a, b) => a.name.localeCompare(b.name));
}

/** The open file's buffer, or undefined before its read lands. */
export function bufferOf(state: WorkbenchState): FileBuffer | undefined {
  return state.buffers[state.selected];
}

/** Does this file hold edits the server has not written? */
export function isDirty(state: WorkbenchState, name: string): boolean {
  const held = state.buffers[name];
  return held !== undefined && held.text !== held.saved;
}

/**
 * Every file with unsaved edits, in the order the list draws them.
 *
 * Off the ROWS, not off `state.files`: a `description.md` started from the button is not on disk
 * yet, so reading the server's file list would have left the one file that exists only in the
 * browser out of the warning about files that exist only in the browser.
 */
export function dirtyFiles(state: WorkbenchState): string[] {
  return fileRows(state)
    .filter((row) => row.dirty)
    .map((row) => row.name);
}

/** What a file is edited as. The grammar and the indent unit for each live in `workbenchEditor`. */
export type LanguageId = 'python' | 'go' | 'markdown' | 'json' | 'text';

/**
 * How a file is edited, and what the status line calls it.
 *
 * `description.md` IS NOT PYTHON, and opening it with the Python grammar is not a cosmetic slip:
 * every `#` heading paints as a comment, so the document that is meant to read as prose reads as
 * commented-out code. This function only names the language — `workbenchEditor.grammarOf` is what
 * hands markdown to `@codemirror/lang-markdown` and hands `actor.json` nothing at all, JSON under
 * Python's lexer highlighting `true` and `null` as bare names.
 */
export function languageOf(name: string): { id: LanguageId; label: string } {
  const lower = name.toLowerCase();
  if (lower.endsWith('.py')) return { id: 'python', label: 'Python' };
  // GO IS A FIRST-CLASS ENGINE HERE, and it was reading as Text. `kontra serve` detects it from a
  // `go.mod` and `examples/go/nscheck` is one of the two actors this repo ships, so opening one in
  // the workbench showed an unhighlighted wall — the same file the Python actor beside it renders
  // in colour. `go.mod` and `go.sum` are their own grammars nobody has; they stay Text, which is
  // honest, rather than borrowing Go's lexer to paint `require` blocks wrong.
  if (lower.endsWith('.go')) return { id: 'go', label: 'Go' };
  if (lower.endsWith('.md') || lower.endsWith('.markdown')) return { id: 'markdown', label: 'Markdown' };
  if (lower.endsWith('.json')) return { id: 'json', label: 'JSON' };
  return { id: 'text', label: 'Text' };
}

/** `1.2 KB` for a file row. Bytes below a kilobyte are shown as bytes — a 40-byte actor.json
 *  rounding to `0.0 KB` reads as an empty file. */
export function fmtBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  return `${(bytes / 1024).toFixed(1)} KB`;
}
