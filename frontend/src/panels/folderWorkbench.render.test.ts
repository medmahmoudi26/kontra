/**
 * The workbench's file list, drawn.
 *
 * The state machine beside this (`folderWorkbench.test.ts`) proves what happens; this proves the
 * two things the operator actually reads off the list — which file has unsaved edits, and which
 * file is not on disk yet — reach the markup. Static rendering, for the reason `HealthChips.test.ts`
 * records: no jsdom in this suite, and both facts are attributes and text.
 *
 * IT COMES FROM `WorkbenchPanes`, not from `FolderWorkbench`, and that is not tidying: the
 * workbench imports the worker's pane, which imports xterm, which touches `self` at module load —
 * so importing this component from there fails the whole file before a test runs.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { WorkbenchFiles } from './WorkbenchPanes';
import type { FileRow } from './folderWorkbench';

const draw = (rows: FileRow[], selected = 'actor.py'): string =>
  renderToStaticMarkup(createElement(WorkbenchFiles, { rows, selected, onSelect: () => {} }));

describe('the folder’s files', () => {
  it('draws a row per file, with its size', () => {
    const html = draw([
      { name: 'actor.json', bytes: 64, dirty: false },
      { name: 'actor.py', bytes: 2048, dirty: false },
      { name: 'description.md', bytes: 120, dirty: false },
    ]);
    expect(html).toContain('data-testid="workbench-file-actor.py"');
    expect(html).toContain('data-testid="workbench-file-description.md"');
    expect(html).toContain('64 B');
    expect(html).toContain('2.0 KB');
  });

  it('marks the file that has edits nobody has written', () => {
    // The one thing the list must carry that the open editor cannot: a file with unsaved changes
    // that is NOT the one on screen.
    const html = draw([
      { name: 'actor.py', bytes: 900, dirty: true },
      { name: 'description.md', bytes: 120, dirty: false },
    ]);
    expect(html).toContain('data-testid="workbench-file-actor.py" data-dirty="true"');
    expect(html).not.toContain('data-testid="workbench-file-description.md" data-dirty');
  });

  it('says a file is new rather than claiming it is empty', () => {
    // `description.md` started from the button has no size until the write comes back; `0 B` would
    // read as a file on disk with nothing in it.
    expect(draw([{ name: 'description.md', bytes: null, dirty: true }])).toContain('new');
  });

  it('says why an empty folder has no rows', () => {
    expect(draw([])).toContain('data-testid="workbench-no-files"');
  });
});
