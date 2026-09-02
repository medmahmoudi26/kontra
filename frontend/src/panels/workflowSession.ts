/**
 * The tmux session a served workflow's worker runs in, derived in the browser.
 *
 * A FOURTH INDEPENDENT DERIVATION of one rule, and the repo's convention is that every one is
 * written separately and pinned by a test rather than shared through a package. The peers are
 * `cli/identity.go:workflowSession`, `backend/src/workflowControl.ts:workflowSession` and
 * `backend/src/panels/discovery.ts:sessionNameFor` (the fleet's, for actors).
 *
 * WHY THE BROWSER NEEDS ITS OWN. `serveWorkflow` answers with the session it just created — but
 * only for a serve THIS page performed. A workflow served yesterday, or from a terminal, has a
 * worker and a pane too, and a page that could only name sessions it had created itself would show
 * an empty rectangle beside a worker that is running perfectly well.
 */

/**
 * `examples/python/workflows/nscheck.py` → `nscheck`, and `nscheck/workflow.py` → `nscheck`.
 *
 * THE FOLDER, when the file is that folder's `workflow.py`. A workflow is a folder now — the marker
 * inside it is called the same thing in every one of them — so the stem alone answered `workflow`
 * for all of them, and this page would have gone looking for a pane by that name while the worker
 * sat in a session named after its folder. Every workflow would have shown the same empty
 * rectangle, or worse, the wrong workflow's pane.
 *
 * The `.` and `:` are replaced because TMUX REPLACES THEM. `session_check_name()` rewrites both to
 * `_` when a session is created, so a name derived with a dot in it is a name the server does not
 * have — and looking for it would find nothing while the pane sat there under the mangled spelling.
 * Measured: `tmux new-session -s 'a.b'` produces `a_b`.
 */
export function workflowSessionOf(file: string): string {
  const parts = file.split('/');
  const base = parts.pop() ?? '';
  const stem = base.replace(/\.[^.]+$/, '');
  // `/workflow.py` and a bare `workflow.py` have no folder to be named after — '' here, and the
  // stem stands so the fallback below still names something attachable.
  const parent = parts.pop() ?? '';
  const named = stem === 'workflow' && parent !== '' && parent !== '.' && parent !== '..' ? parent : stem;
  const safe = named.replace(/[.:]/g, '_');
  return safe === '' || safe === '_' ? 'workflow' : safe;
}
