/**
 * A filesystem path on ONE line, elided in the MIDDLE.
 *
 * WHY NOT `break-all`, WHICH IS WHAT THIS REPLACES. The Workflows rows drew the folder path with
 * `break-all` in a ~200px column at 9.5px, which wraps INSIDE a word — the observed result was
 *
 *     /root/kontra-loca
 *     l/.claude/worktree
 *     s/workflows-clien
 *     t/.kontra/workflow
 *     s/canary
 *
 * five lines, every one of them broken mid-token, for a string a person is scanning to answer
 * "which checkout is this". It also made the row height depend on the path length, so a list of
 * workflows had rows of five different heights and nothing lined up.
 *
 * WHY THE MIDDLE, AND NOT THE HEAD OR THE TAIL. Both ends carry the answer and neither alone does.
 * The tail is the workflow (`…/workflows/canary`) and the head is the machine (`/root/…`), but what
 * distinguishes two registrations of ONE workflow is the segment between them — a worktree name, a
 * checkout, an `examples/private`. Truncating from either end throws away exactly the part an
 * operator with two copies is looking at. `…` sits where the shared prefix would be.
 *
 * The full path stays in `title`, so nothing is unrecoverable — this decides what is READ at a
 * glance, not what is available.
 */

/** Head and tail kept, in characters. Tuned to the ~200px / 9.5px monospace column this draws in. */
const HEAD = 12;
const TAIL = 30;

export function elidePath(path: string, max = HEAD + TAIL + 1): string {
  if (!path) return '';
  if (path.length <= max) return path;

  // PREFER A SEGMENT BOUNDARY. Cutting mid-segment produces `…rktrees/workflows-client/…`, which
  // reads as a directory that does not exist; starting the tail at a `/` keeps every visible
  // segment a real one. Falls back to the raw offset when there is no separator in range, which is
  // a path with one enormous component and nothing better to do.
  const tailStart = path.length - TAIL;
  const slash = path.indexOf('/', tailStart);
  const cut = slash >= 0 && slash - tailStart < 12 ? slash : tailStart;
  return `${path.slice(0, HEAD)}…${path.slice(cut)}`;
}
