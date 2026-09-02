/**
 * The workbench's state, driven through the loop an operator actually performs: open the folder,
 * read a file, type, save — and be refused.
 *
 * The component around this is a shell: it turns four server calls into these four functions and
 * draws what comes back. What is worth pinning is here — that a switch between files keeps what was
 * typed, and that a refusal keeps it too.
 */

import { describe, expect, it } from 'vitest';
import type { SourceFile } from '../run/api';
import {
  bufferOf,
  dirtyFiles,
  edited,
  emptyWorkbench,
  fileRows,
  firstToOpen,
  fmtBytes,
  isDirty,
  languageOf,
  listed,
  opened,
  refused,
  savedFile,
  selectFile,
  startedFile,
} from './folderWorkbench';

const file = (name: string, bytes = 100): SourceFile => ({
  name,
  bytes,
  modifiedAt: 1_700_000_000_000,
});

/** What `GET /api/sources/actor/:id/files` returns for a folder holding all three. */
const FOLDER = [file('actor.json', 64), file('actor.py', 900), file('description.md', 120)];

describe('opening a folder', () => {
  it('opens the code, not the manifest that happens to sort first', () => {
    // `filesIn` sorts alphabetically, so "the first file" is actor.json — four lines of name and
    // version, and never what somebody opened the folder to change.
    expect(firstToOpen(FOLDER, 'actor')).toBe('actor.py');
    expect(firstToOpen([file('actor.json'), file('description.md')], 'actor')).toBe('actor.json');
    expect(firstToOpen([file('description.md')], 'actor')).toBe('description.md');
    expect(firstToOpen([], 'actor')).toBe('');
  });

  it('opens a workflow folder on its workflow.py', () => {
    expect(firstToOpen([file('description.md'), file('workflow.py')], 'workflow')).toBe(
      'workflow.py'
    );
  });

  it('lists the files and selects one, with nothing read yet', () => {
    const state = listed(emptyWorkbench(), FOLDER, 'actor');
    expect(state.files.map((f) => f.name)).toEqual(['actor.json', 'actor.py', 'description.md']);
    expect(state.selected).toBe('actor.py');
    expect(bufferOf(state)).toBeUndefined();
  });

  it('holds the file once its read lands', () => {
    const state = opened(listed(emptyWorkbench(), FOLDER, 'actor'), 'actor.py', '@actor.method\n');
    expect(bufferOf(state)?.text).toBe('@actor.method\n');
    expect(isDirty(state, 'actor.py')).toBe(false);
  });

  it('does not drop a read failure on the floor — the server says why', () => {
    const state = refused(listed(emptyWorkbench(), FOLDER, 'actor'), 'read the file failed: 400');
    expect(state.error).toBe('read the file failed: 400');
  });
});

describe('editing', () => {
  const loaded = opened(listed(emptyWorkbench(), FOLDER, 'actor'), 'actor.py', 'x = 1\n');

  it('marks the file dirty and leaves the others alone', () => {
    const typed = edited(loaded, 'actor.py', 'x = 2\n');
    expect(bufferOf(typed)?.text).toBe('x = 2\n');
    expect(dirtyFiles(typed)).toEqual(['actor.py']);
  });

  it('is not dirty again once the text is back where it started', () => {
    // A boolean set on every keystroke claimed unsaved changes after a character was typed and
    // deleted, and there is no way to tell that state from a real one.
    const there = edited(loaded, 'actor.py', 'x = 2\n');
    expect(isDirty(edited(there, 'actor.py', 'x = 1\n'), 'actor.py')).toBe(false);
  });
});

describe('switching files does not lose what was typed', () => {
  it('keeps the buffer, and comes back to it', () => {
    // The failure this exists for: one buffer for a folder of three files means the click that
    // checks description.md is the same click that discards the edit to actor.py.
    let state = opened(listed(emptyWorkbench(), FOLDER, 'actor'), 'actor.py', 'x = 1\n');
    state = edited(state, 'actor.py', 'x = 2\n');
    state = selectFile(state, 'description.md');
    state = opened(state, 'description.md', '# probe\n');

    expect(bufferOf(state)?.text).toBe('# probe\n');
    expect(state.buffers['actor.py']?.text).toBe('x = 2\n');
    expect(dirtyFiles(state)).toEqual(['actor.py']);

    state = selectFile(state, 'actor.py');
    expect(bufferOf(state)?.text).toBe('x = 2\n');
  });

  it('lists the unsaved files in the order the list draws them', () => {
    let state = opened(listed(emptyWorkbench(), FOLDER, 'actor'), 'actor.py', 'x = 1\n');
    state = edited(state, 'actor.py', 'x = 2\n');
    state = opened(state, 'actor.json', '{}');
    state = edited(state, 'actor.json', '{"name":"probe"}');
    expect(dirtyFiles(state)).toEqual(['actor.json', 'actor.py']);
  });

  it('a reload re-reads what nobody touched and keeps what somebody did', () => {
    // The reload button is for the folder changing underneath the browser — a `git pull`, an editor
    // in a terminal. A kept clean buffer would show yesterday's file next to today's size; a
    // dropped dirty one would discard an author's work on the server's say-so.
    let state = opened(listed(emptyWorkbench(), FOLDER, 'actor'), 'actor.py', 'x = 1\n');
    state = opened(state, 'description.md', '# probe\n');
    state = edited(state, 'description.md', '# probe\n\nedited\n');
    state = listed(state, FOLDER, 'actor');

    expect(state.buffers['actor.py']).toBeUndefined();
    expect(state.buffers['description.md']?.text).toBe('# probe\n\nedited\n');
  });

  it('a late read does not land on top of typing', () => {
    // Two clicks leave two reads in flight. The slower one used to arrive after the operator had
    // started editing the file it was for, replacing what they typed with what was on disk.
    let state = opened(listed(emptyWorkbench(), FOLDER, 'actor'), 'actor.py', 'x = 1\n');
    state = edited(state, 'actor.py', 'x = 999\n');
    state = opened(state, 'actor.py', 'x = 1\n');
    expect(state.buffers['actor.py']?.text).toBe('x = 999\n');
  });
});

describe('saving', () => {
  it('clears dirty and takes the new size from the server', () => {
    let state = opened(listed(emptyWorkbench(), FOLDER, 'actor'), 'actor.py', 'x = 1\n');
    state = edited(state, 'actor.py', 'x = 2\n');
    state = savedFile(state, 'actor.py', 'x = 2\n', file('actor.py', 6));
    expect(isDirty(state, 'actor.py')).toBe(false);
    expect(state.files.find((f) => f.name === 'actor.py')?.bytes).toBe(6);
    expect(state.error).toBeNull();
  });

  it('leaves a file dirty when it was typed into DURING the save', () => {
    // The response confirms the text that was sent, not the buffer as it is now — marking the
    // later keystrokes saved would report work that was never written as written.
    let state = opened(listed(emptyWorkbench(), FOLDER, 'actor'), 'actor.py', 'x = 1\n');
    state = edited(state, 'actor.py', 'x = 2\n');
    const sent = 'x = 2\n';
    state = edited(state, 'actor.py', 'x = 3\n');
    state = savedFile(state, 'actor.py', sent, file('actor.py', 6));
    expect(isDirty(state, 'actor.py')).toBe(true);
    expect(bufferOf(state)?.text).toBe('x = 3\n');
  });

  it('puts a file that was not on disk into the list', () => {
    let state = startedFile(listed(emptyWorkbench(), FOLDER.slice(0, 2), 'actor'), 'description.md', '# probe\n');
    expect(fileRows(state).map((f) => f.name)).toEqual(['actor.json', 'actor.py', 'description.md']);
    expect(fileRows(state).find((f) => f.name === 'description.md')?.bytes).toBeNull();
    // and it is warned about like any other unwritten buffer — it exists ONLY in the browser, which
    // is the strongest version of the thing the warning is about
    expect(dirtyFiles(state)).toEqual(['description.md']);
    state = savedFile(state, 'description.md', '# probe\n', file('description.md', 8));
    expect(fileRows(state).find((f) => f.name === 'description.md')?.bytes).toBe(8);
    expect(dirtyFiles(state)).toEqual([]);
  });
});

describe('a save the server refuses', () => {
  it('reports the reason and keeps the buffer', () => {
    // `writeInside` refuses over 512 KB and refuses a name that is not a bare filename. A refusal
    // that also cleared the editor would answer "too large" with "gone".
    let state = opened(listed(emptyWorkbench(), FOLDER, 'actor'), 'actor.py', 'x = 1\n');
    state = edited(state, 'actor.py', 'x = 2\n');
    state = refused(state, 'save the file failed: 400 Bad Request — a file may be at most 524288 bytes');

    expect(state.error).toContain('at most 524288 bytes');
    expect(bufferOf(state)?.text).toBe('x = 2\n');
    expect(isDirty(state, 'actor.py')).toBe(true);
  });

  it('clears once the next thing works', () => {
    const state = refused(listed(emptyWorkbench(), FOLDER, 'actor'), 'save the file failed: 400');
    expect(opened(state, 'actor.py', 'x = 1\n').error).toBeNull();
  });
});

describe('a file is edited as what it is', () => {
  it('does not open description.md as Python', () => {
    // Every `#` heading paints as a comment under the Python grammar, so the document meant to read
    // as prose reads as commented-out code. This is only the NAME of the language — that markdown
    // then gets a markdown parser and Python does not is in `workbenchEditor.test.ts`, because
    // naming it was true for a while before it was worth anything.
    expect(languageOf('description.md').id).toBe('markdown');
    expect(languageOf('README.MD').id).toBe('markdown');
    expect(languageOf('actor.py').id).toBe('python');
    expect(languageOf('actor.json').id).toBe('json');
    expect(languageOf('Dockerfile').id).toBe('text');
  });

  it('names the language in words, for the status line', () => {
    expect(languageOf('description.md').label).toBe('Markdown');
    expect(languageOf('actor.py').label).toBe('Python');
  });
});

describe('sizes', () => {
  it('shows a small file in bytes rather than as 0.0 KB', () => {
    expect(fmtBytes(64)).toBe('64 B');
    expect(fmtBytes(2048)).toBe('2.0 KB');
  });
});

describe('a Go file is Go', () => {
  it('detects .go, which used to fall through to Text', () => {
    expect(languageOf('main.go')).toEqual({ id: 'go', label: 'Go' });
    expect(languageOf('MAIN.GO')).toEqual({ id: 'go', label: 'Go' });
  });

  it('leaves go.mod and go.sum as Text, which is honest', () => {
    // They are their own grammars and nobody has them. Borrowing Go's lexer would paint a
    // `require` block as if it were code it is not.
    expect(languageOf('go.mod').id).toBe('text');
    expect(languageOf('go.sum').id).toBe('text');
  });

  it('does not disturb the other extensions', () => {
    expect(languageOf('actor.py').id).toBe('python');
    expect(languageOf('description.md').id).toBe('markdown');
    expect(languageOf('actor.json').id).toBe('json');
  });
});
