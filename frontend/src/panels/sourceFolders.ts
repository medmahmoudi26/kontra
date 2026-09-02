/**
 * The registered-folder list, as a page holds it between two server answers.
 *
 * THE LIST IS THE SERVER'S, and these functions only fold one call's answer into the copy on
 * screen. `SourceStore.list` dedupes by PATH with the registered row winning; a page that appended
 * what `POST /api/sources/:kind` returned would show the same folder twice until the next reload —
 * once as `at:/home/you/.kontra/actors/probe` and once as `actor:probe:1f3k` — and the operator who
 * just registered a folder they already had would be looking at proof that registering duplicates
 * things. So the same rule is applied here, and it is applied by path rather than by id because the
 * two rows for one folder have DIFFERENT ids by construction.
 */

import type { Source } from '../run/api';

/**
 * Was this folder found under the default root rather than registered by hand?
 *
 * The id is the test, and it is the server's own: `SourceStore.forget` refuses anything starting
 * with `at:`, so any other rule here would eventually offer a forget button that 400s.
 */
export function isDiscovered(source: Source): boolean {
  return source.id.startsWith('at:');
}

/** Registered folders can be forgotten; discovered ones have no registration to remove. */
export function canForget(source: Source): boolean {
  return !isDiscovered(source);
}

/**
 * The list with `added` in it — replacing the row for the same folder rather than adding a second.
 *
 * Registering twice is idempotent on the server (the second call returns the FIRST row, same id),
 * and registering a folder that was already discovered returns a new registered row for a path that
 * is already listed under an `at:` id. Both collapse here, by path.
 */
export function withRegistered(list: Source[], added: Source): Source[] {
  return byName([...list.filter((s) => s.id !== added.id && s.path !== added.path), added]);
}

/** The list without one folder, after the server forgot it. */
export function withoutSource(list: Source[], id: string): Source[] {
  return list.filter((s) => s.id !== id);
}

/** The order the server lists in, kept so an inserted row does not jump to the bottom. */
function byName(list: Source[]): Source[] {
  return [...list].sort((a, b) => a.name.localeCompare(b.name));
}

/** One row of the Workflows sidecar. The FOLDER is always there — it is what the row is. */
export interface WorkflowFolderRow<T> {
  name: string;
  /** The entry `GET /api/workflows` listed, which is where a `description.md` first paragraph comes
   *  from. Absent for a folder registered outside that directory, which is a workflow all the same. */
  file?: T;
  folder: Source;
}

/**
 * The workflow list: ONE ROW PER REGISTERED FOLDER. No folder, no workflow.
 *
 * THE FOLDER IS THE UNIT, the same rule the Actors page now follows. A folder is a path somebody
 * pointed at — it can be opened, served, started and forgotten — and that is the whole list of
 * things this surface does. `listWorkflows()`'s file entries are not a second inventory to merge
 * with it: they are what `.kontra/workflows/` happens to hold, which the source discovery already
 * reports as folders under the default root. Drawing both put most workflows on the screen twice,
 * in two vocabularies, and drawing the FILES would put rows on the page for code the operator
 * cannot forget or re-point.
 *
 * The file half is still joined in, because it is where a workflow's `description.md` first
 * paragraph comes from and that is what a list of names cannot say.
 *
 * `.py` is tolerated on the file's name because the folder migration left both spellings in the
 * wild — a `nscheck.py` file and an `nscheck` folder are the same workflow to everybody but a
 * string compare.
 */
export function mergeWorkflowFolders<T extends { name: string }>(
  files: readonly T[],
  folders: readonly Source[]
): Array<WorkflowFolderRow<T>> {
  const stem = (name: string): string => name.replace(/\.py$/, '');
  const byName = new Map(files.map((f) => [stem(f.name), f] as const));
  return folders.map((folder) => ({
    name: folder.name,
    folder,
    file: byName.get(stem(folder.name)),
  }));
}

/** As much of a catalog entry as finding its folder needs. */
export interface CatalogedActor {
  name: string;
  version: string;
  /** The directory the WORKER loaded it from — a path on the machine that ran it. */
  source?: string;
}

/**
 * The registered folder holding this catalogued Actor's code, if there is one.
 *
 * THE TWO INVENTORIES ARE JOINED HERE, and neither is the other's index. The catalog is what a
 * running worker registered about ITSELF; the folder list is what an operator pointed at on this
 * disk. An Actor can be in one and not the other in both directions — a worker on a droplet whose
 * code is not on this machine, and a folder registered before anything served it.
 *
 * THE PATH IS THE STRONG MATCH and it is not enough on its own. Registration records the
 * orchestrator's realpath; `actor.source` is where the WORKER loaded from, which on a fleet Machine
 * is `/opt/kontra/actor/<name>` and matches nothing here. So the name is the fallback, with the
 * same version preferred over a different one — two checkouts of `probe` at 0.1.0 and 0.2.0 are
 * both plausible, and the one the worker announced is the better guess.
 *
 * AN ABSENT REGISTRATION LOSES to any folder that is actually there. Its files cannot be read (the
 * route 400s on every name), so preferring it would open a workbench that can only fail.
 */
/**
 * The catalogued Actor this FOLDER is, if a worker has registered one.
 *
 * THE FOLDER IS THE UNIT NOW, and this is the join read the other way round. The Actors page used to
 * draw the catalog — everything any worker ever registered about itself, on any machine — which after
 * one run was twenty-three cards, most of them saying `source unknown` and `no registered
 * folder`: deployments the reader cannot open, edit, serve or forget. A page of things you do not
 * control is a page you stop reading, and a catalog is also LIVE — it empties when the workers stop,
 * taking the page with it. So a registered folder is an Actor, and a catalog row with no folder is
 * not drawn.
 *
 * SAME SCORING AS `folderForActor`, INVERTED, because two matchers for one relationship is how a
 * card ends up claiming a version the page does not think it has. The path is the strong match; the
 * name is the fallback, with an exact version preferred over a different one.
 *
 * ONE ENTRY, NOT ALL OF THEM. A folder holding `cachebuster` at 0.3.0 may have four versions in the
 * catalog from earlier deploys, and drawing four cards for one directory is the noise this change
 * removes. The version on disk wins; failing that, whatever the catalog offers for the name — and
 * the card says which version it found, so the two never silently disagree.
 */
export function catalogForFolder<T extends CatalogedActor>(
  folder: Source,
  catalog: readonly T[]
): T | undefined {
  let best: T | undefined;
  let bestScore = 0;
  for (const actor of catalog) {
    let score = 0;
    if (actor.source && actor.source === folder.path) score = 3;
    else if (actor.name === folder.name) score = actor.version === folder.version ? 2 : 1;
    if (score > bestScore) {
      best = actor;
      bestScore = score;
    }
  }
  return best;
}

/**
 * The catalog, narrowed to what a registered folder on this disk resolves to.
 *
 * NO FOLDER, NO ACTOR — the Actors page's rule, applied to every OTHER surface that draws the
 * catalog. Scratch's palette drew all of it: twenty-three Actors, twenty-two of which cannot be
 * opened, served, called or forgotten from this installation, offered as nodes to drag onto a
 * canvas. A sketch built from those is a drawing of a system the operator does not have, and the
 * generated workflow dispatches to queues nobody serves — which fails as a Run that waits forever
 * rather than as anything the canvas could have told them.
 *
 * ONE ENTRY PER FOLDER, through `catalogForFolder`, so the version rule is the same one the Actors
 * grid uses: the version ON DISK wins when a directory has several deploys behind it. Two folders
 * that resolve to the same entry contribute it once — the palette lists Actors, not checkouts.
 *
 * A FOLDER NOTHING HAS SERVED CONTRIBUTES NOTHING HERE, and that is the difference from the Actors
 * grid. A card can say `unserved` and still be worth drawing, because its whole job is to be the
 * way into register → edit → serve. A palette node cannot: it would have no Methods, so no ports,
 * so no edges — a node that can be placed and never connected.
 */
export function registeredActors<T extends CatalogedActor>(
  folders: readonly Source[],
  catalog: readonly T[]
): T[] {
  const out: T[] = [];
  const seen = new Set<T>();
  for (const folder of folders) {
    const entry = catalogForFolder(folder, catalog);
    if (!entry || seen.has(entry)) continue;
    seen.add(entry);
    out.push(entry);
  }
  return out;
}

export function folderForActor(actor: CatalogedActor, folders: Source[]): Source | undefined {
  let best: Source | undefined;
  let bestRank = 0;
  for (const folder of folders) {
    let score = 0;
    if (actor.source && folder.path === actor.source) score = 3;
    else if (folder.name === actor.name) score = folder.version === actor.version ? 2 : 1;
    if (score === 0) continue;
    // Present outranks absent OUTRIGHT, not by a tiebreak: a folder at the wrong version is still
    // code you can open, and a registration whose directory is gone is not.
    const rank = folder.absent ? score : score + 10;
    if (rank > bestRank) {
      best = folder;
      bestRank = rank;
    }
  }
  return best;
}
