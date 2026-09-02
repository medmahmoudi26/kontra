/**
 * Narrowing the Actors grid, as a decision separate from drawing it.
 *
 * TWENTY-THREE CARDS IS NOT A LIST, IT IS A WALL. And most of them are the same few Actors: this
 * installation carries `cachebuster` at 0.1.0, 0.2.0, 0.3.0 and 0.4.0, so scrolling for "the one I
 * am serving" means reading four cards that differ in one digit. Every Method's name and its
 * author's sentence are on those cards too, and none of it is reachable except by eye.
 *
 * THE SENTINEL IS `paneFilter`'s, IMPORTED RATHER THAN REDECLARED. Two spellings of "do not narrow"
 * in one app is how a filter ends up dropping every Actor whose name is empty — the exact bug the
 * comment on `ANY` was written for. One sentinel, one meaning.
 *
 * EVERY CLAUSE IS AN AND, which is the reading a row of controls implies: a menu, a box and two
 * toggles sitting together say "all of these at once".
 *
 * WHAT THIS DOES NOT DO IS HIDE THE TOTAL. The count beside the controls says how many of how many,
 * always — a filter that quietly leaves eight Actors out must never read as a catalog that lost
 * eight Actors. Same rule as the Monitor's bar, for the same reason.
 */

import { ANY } from './paneFilter';

export { ANY };

/** As much of an Actor as narrowing needs — satisfied by a catalog entry and by the stand-in an
 *  unserved folder is drawn with, because the grid now holds both and a filter that only understood
 *  one of them would hide the other. */
export interface FilterableActor {
  name: string;
  version: string;
  key: string;
  /** Where the worker loaded it from. Searched, because "which checkout is this" is the question
   *  two versions of one Actor actually raise. */
  source?: string;
  operations: readonly { name: string; description?: string }[];
}

/** What is known about an Actor that is NOT on the Actor itself — the joins the page makes. */
export interface ActorStanding {
  /** Machines whose health probe says the worker is polling. */
  serving: number;
  /** Machines the Monitor can see running it, healthy or not. */
  machines: number;
  /** A registered folder holding its code on this disk, present rather than absent. */
  onDisk: boolean;
}

export interface ActorFilter {
  /** Free text over the name, version, queue key, source path, and every Method's name and
   *  description. One box, every field — somebody typing `delegation` does not want to be told it
   *  was the wrong control. */
  query: string;
  /** One Actor NAME, which is the axis that collapses the version noise: four `cachebuster` cards
   *  become four cards you asked for. */
  name: string;
  /** Only Actors something is actually serving. The question this page is opened with. */
  servingOnly: boolean;
  /** Only Actors whose code is on THIS disk — the ones the workbench can open. */
  onDiskOnly: boolean;
}

export const EMPTY_ACTOR_FILTER: ActorFilter = {
  query: '',
  name: ANY,
  servingOnly: false,
  onDiskOnly: false,
};

export function actorFilterActive(f: ActorFilter): boolean {
  return f.query.trim() !== '' || f.name !== ANY || f.servingOnly || f.onDiskOnly;
}

/**
 * The name menu's contents, built from what is on the page rather than from a fixed list.
 *
 * A menu that offers a value nothing has is a dead end; one that omits a value something has is a
 * lie. Sorted, and deduped across versions — the whole point of the menu is that `cachebuster`
 * appears once.
 */
export function actorNames(actors: readonly FilterableActor[]): string[] {
  return [...new Set(actors.map((a) => a.name))].sort((a, b) => a.localeCompare(b));
}

/** Does this Actor survive the filter? */
export function matchActor(
  actor: FilterableActor,
  filter: ActorFilter,
  standing: ActorStanding
): boolean {
  if (filter.name !== ANY && actor.name !== filter.name) return false;
  // SERVING, not "has Machines". A Machine whose probe is unhappy is not serving whatever the
  // catalog says, and an operator asking for what is up does not want the dead one back.
  if (filter.servingOnly && standing.serving === 0) return false;
  if (filter.onDiskOnly && !standing.onDisk) return false;
  const q = filter.query.trim().toLowerCase();
  if (!q) return true;
  return haystack(actor).includes(q);
}

/** Everything one card SAYS, as one lowercased string. The Methods are in it because a Method is
 *  the unit (ADR 0023 §9): an operator looking for `delegation` is looking for the Actor that
 *  declares it, and its name is the half they do not remember. */
function haystack(actor: FilterableActor): string {
  const methods = actor.operations.flatMap((op) => [op.name, op.description ?? '']);
  return [actor.name, actor.version, actor.key, actor.source ?? '', ...methods]
    .join(' ')
    .toLowerCase();
}
