/**
 * ONE `fs.watch` PER DIRECTORY, however many readers are watching it.
 *
 * The editor pane's form tracks the code on disk, and the VS Code extension made that instant by
 * hooking its own `onDidSaveTextDocument` and pushing a reload into the pane. A BROWSER TAB HAS NO
 * SUCH HOOK — and the browser is now the supported way to use the runner, because a cross-origin
 * frame inside a webview cannot reach the clipboard (see `tools/vscode/README.md`). So the server
 * has to say "the files moved" instead of the editor.
 *
 * This file is not the feature. `fs.watch` is the feature; this exists for two reasons only:
 *
 *   1. N readers on one folder cost ONE watcher, not N. Two panes on the same actor is the normal
 *      case, not the exotic one.
 *   2. THE LAST UNSUBSCRIBE CLOSES IT. A leaked `FSWatcher` is an open file descriptor that
 *      outlives the tab that opened it, and nothing upstream would ever report that — the process
 *      just runs out of handles one afternoon.
 */

import { watch, type FSWatcher } from 'node:fs';

/**
 * Distinct directories this process will watch at once.
 *
 * One pane per actor is the real shape and 32 is far above it. The cap is not for that case: it is
 * the backstop for a client that opens streams on folders nobody is looking at, which is the shape
 * `routes/rowStream.ts` documents having actually been hit (250 streams on fictional ids, no
 * credential). Refusing at a number an operator can read beats discovering the ceiling as EMFILE.
 */
export const MAX_WATCHED_DIRS = 32;

const watchers = new Map<string, { handle: FSWatcher; sinks: Set<() => void> }>();

/**
 * Call `onChange` whenever anything in `dir` changes. Returns the unsubscribe.
 *
 * Throws rather than degrading when the cap is reached, so the caller can refuse BEFORE it writes
 * an SSE head — once a client has been told `200 text/event-stream`, the only way left to say no is
 * to hang up, and a hang-up reads as a network fault rather than as a limit.
 */
export function watchDir(dir: string, onChange: () => void): () => void {
  let entry = watchers.get(dir);
  if (!entry) {
    if (watchers.size >= MAX_WATCHED_DIRS) {
      throw new Error(`already watching ${watchers.size} folders (cap ${MAX_WATCHED_DIRS})`);
    }
    const sinks = new Set<() => void>();
    // `persistent: false`: a watcher must never be the reason this process stays alive.
    // The sink set is copied per event because a sink that unsubscribes itself during delivery
    // would otherwise mutate the set being iterated.
    const handle = watch(dir, { persistent: false }, () => {
      for (const sink of [...sinks]) sink();
    });
    entry = { handle, sinks };
    watchers.set(dir, entry);
  }
  const { handle, sinks } = entry;
  sinks.add(onChange);

  // IDEMPOTENT ON PURPOSE. Fastify can fire `close` more than once, and a second unsubscribe that
  // was allowed through would find the set empty again and close a watcher a LATER reader had
  // since created for the same directory — a live stream silently going deaf.
  let stopped = false;
  return (): void => {
    if (stopped) return;
    stopped = true;
    sinks.delete(onChange);
    if (sinks.size === 0) {
      handle.close();
      watchers.delete(dir);
    }
  };
}

/** How many directories are watched right now. For the test, and for anything that reports health. */
export function watchedDirCount(): number {
  return watchers.size;
}
