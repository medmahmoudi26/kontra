import { describe, expect, it } from 'vitest';
import { workflowSessionOf } from './workflowSession';

/**
 * A fourth independent derivation of one rule, pinned.
 *
 * The peers are `cli/identity.go:workflowSession`, `backend/src/workflowControl.ts:
 * workflowSession` and `backend/src/panels/discovery.ts:sessionNameFor`. A drift between any
 * two of them has the same failure shape: the worker runs perfectly and its pane is never found, so
 * the surface that exists to show a worker's traceback shows an empty rectangle instead.
 */

describe('the session a served workflow runs in', () => {
  it('is the workflow’s name, from the file', () => {
    expect(workflowSessionOf('examples/python/workflows/nscheck.py')).toBe('nscheck');
    expect(workflowSessionOf('enumerate_scope.py')).toBe('enumerate_scope');
    expect(workflowSessionOf('sweep.py')).toBe('sweep');
  });

  it('takes the basename, whatever the path', () => {
    expect(workflowSessionOf('/abs/path/to/sweep.py')).toBe('sweep');
    expect(workflowSessionOf('a/b/c/d/e.py')).toBe('e');
  });

  it('replaces what TMUX would replace, so the name is one the server actually has', () => {
    // MEASURED: `tmux new-session -s 'a.b'` produces `a_b`. tmux's session_check_name() rewrites
    // every `.` and `:` to `_` at creation — so a name derived with a dot in it is a name that does
    // not exist, and looking for it finds nothing while the pane sits there under the mangled
    // spelling.
    expect(workflowSessionOf('my.workflow.py')).toBe('my_workflow');
    expect(workflowSessionOf('a:b.py')).toBe('a_b');
  });

  it('is the FOLDER’s name for a folder’s workflow.py', () => {
    // THE TRAP THE FOLDER LAYOUT SETS. Every workflow folder holds a file called `workflow.py`, so
    // the stem is the same for every workflow there is — this page would have looked for a pane
    // called `workflow` while the worker sat in a session named after its folder, and every
    // workflow would have shown the same empty rectangle beside a worker that is running.
    expect(workflowSessionOf('nscheck/workflow.py')).toBe('nscheck');
    expect(workflowSessionOf('/home/me/.kontra/workflows/ping/workflow.py')).toBe('ping');
    expect(workflowSessionOf('nscheck/workflow.py')).not.toBe(workflowSessionOf('ping/workflow.py'));
    // A stem that merely contains the word is not the marker.
    expect(workflowSessionOf('recon/my.workflow.py')).toBe('my_workflow');
  });

  it('names something rather than nothing when there is no folder to be named after', () => {
    // A bare `workflow.py` and `/workflow.py` have no folder name to take — `.` and `` are not
    // sessions anyone can attach to.
    expect(workflowSessionOf('workflow.py')).toBe('workflow');
    expect(workflowSessionOf('/workflow.py')).toBe('workflow');
  });

  it('names something rather than nothing for a file with no usable stem', () => {
    // A session called '' cannot be attached to, and a blank one on the wall is a tile with no way
    // back to it.
    expect(workflowSessionOf('.py')).toBe('workflow');
    expect(workflowSessionOf('')).toBe('workflow');
  });

  it('keeps a file with no extension', () => {
    expect(workflowSessionOf('recon')).toBe('recon');
  });
});
