/**
 * Which actor does this file belong to?
 *
 * The only real logic in this extension, and therefore the only thing worth testing: everything
 * else is a webview and an iframe. Kept out of `extension.ts` so it can be exercised without the
 * `vscode` module, which cannot be imported outside the editor.
 */

import * as fs from 'fs';
import * as path from 'path';

/** The nearest ancestor of `file` holding an `actor.json`, or undefined. */
export function actorDirFor(
  file: string,
  exists: (p: string) => boolean = (p) => fs.existsSync(p)
): string | undefined {
  let dir = path.dirname(file);
  // BOUNDED. A file outside any actor must not walk to `/` and keep going — `path.dirname('/')` is
  // `/`, so the loop's own termination check is what stops it, and the count is the belt to it.
  for (let i = 0; i < 64; i++) {
    if (exists(path.join(dir, 'actor.json'))) return dir;
    const up = path.dirname(dir);
    if (up === dir) return undefined;
    dir = up;
  }
  return undefined;
}

/**
 * `name@version` — the catalog's own key.
 *
 * A manifest with a name and no version yields the bare name, which the catalog also accepts; a
 * manifest with no name yields nothing, because an actor addressed by nothing is not addressable.
 */
export function actorKeyFrom(manifest: unknown): string | undefined {
  const m = manifest as { name?: unknown; version?: unknown } | null;
  const name = typeof m?.name === 'string' ? m.name.trim() : '';
  if (!name) return undefined;
  const version = typeof m?.version === 'string' ? m.version.trim() : '';
  return version ? `${name}@${version}` : name;
}

export function readActorKey(
  dir: string,
  read: (p: string) => string = (p) => fs.readFileSync(p, 'utf8')
): string | undefined {
  try {
    return actorKeyFrom(JSON.parse(read(path.join(dir, 'actor.json'))));
  } catch {
    return undefined;
  }
}

/**
 * Is `file` inside `dir`?
 *
 * `path.relative` rather than `startsWith`, because `/srv/kontra-evil` starts with `/srv/kontra` —
 * the same rule the orchestrator's `resolveWorkflowFile` confines with, and wrong here in a smaller
 * way: a save in a sibling directory would re-derive a schema for a folder it does not belong to.
 * Symlinks are NOT resolved: this only decides whether to send a hint, and a stat per keystroke-
 * triggered save buys nothing.
 */
export function isInside(file: string, dir: string): boolean {
  const rel = path.relative(dir, file);
  return rel !== '' && !rel.startsWith('..') && !path.isAbsolute(rel);
}
