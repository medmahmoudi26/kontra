/**
 * From a domain turn down to the raw Temporal events it folded — and, one more step, into the
 * child workflow those events are about.
 *
 * WHY THE DRILL IS THE OTHER HALF OF THE VOCABULARY AND NOT A POWER-USER FEATURE. A translation
 * layer that renames confidently is worse than raw events (ADR 0027): "batch dispatched" is a claim
 * about six `NexusOperation*` rows, and a reader with no way to check it has been asked to trust a
 * rename made by a release they cannot see. The check is what makes the rename affordable. So every
 * turn drills, including the ones this release has no word for — an untranslated row is the case
 * where checking matters MOST, and it would be the easiest one to leave out.
 *
 * AND IT COSTS NOTHING AT THE ROOT. `transcript.ts` puts the event ids on every turn ("THE DRILL
 * PATH"), and `RunTurnsRead` now carries back the reduced log those ids index — the same response
 * the account was read from. So opening a turn's raw events is arithmetic over bytes the page
 * already holds: no fetch, no spinner, and no reason for anyone to argue that the friendly view is
 * the cheap one.
 *
 * A CHILD IS JUST ANOTHER WORKFLOW ID, which is `history.ts`'s trick and this module inherits it
 * whole: one route takes any id, so descending costs one read of the same shape, decodes the same
 * number of payloads (none), and needs no second set of rules. `eventDrill.ts` already owns what a
 * click DOES — the stack, the return-not-push rule, the crumb labels — and is called rather than
 * re-implemented, so a dispatch inside a fleet-scoped child has the two places to go back to that
 * it has always had.
 *
 * Everything here is pure: turns and a reduced log in, rows out. The fetching lives in the panel.
 */

import { expand, type Turn } from '@core/transcript';

import type { EventLink, RunEvent, RunHistory } from '../run/api';

/**
 * Which turn the drill is open on.
 *
 * THE SAME STRING THE TRANSCRIPT KEYS ITS ROWS BY, and that is the point of exporting it rather than
 * writing it twice: the row React reconciles and the row the drill is about must be the same
 * identity, or a growing loop would silently swap which turn the panel is showing as members land.
 * Keyed on the FIRST event because a collapsed group is anchored at its first member (`collapse` in
 * `transcript.ts`), so the key survives the group growing — which is exactly what a live transcript
 * does to it.
 */
export function turnKey(turn: Turn): string {
  return `${turn.kind}:${turn.events[0] ?? turn.at}`;
}

/** What one turn drills to. */
export interface DrillSubject {
  /** Every Temporal event id the turn folded, its members included, in log order. */
  events: number[];
  /** The raw Temporal types behind it, deduped, in first-appearance order. Shown verbatim so an
   *  operator is never confidently told the wrong thing about an event the reader misread. */
  types: string[];
  /**
   * Every workflow this turn is about, deduped.
   *
   * FROM THE MEMBERS, NOT FROM THE HEAD. `fold` builds a group by spreading its first member, so a
   * folded group of twelve dispatches carries the FIRST one's link and eleven others are reachable
   * only through `expand`. A drill that read `turn.link` alone would offer one of twelve children
   * and hide the rest, which is worse than offering none.
   */
  links: EventLink[];
  /**
   * This turn has no event of its own.
   *
   * TRUE OF A PARK AND OF NOTHING ELSE. An ask comes from a second authority beside the log
   * (`transcript.ts`) and has no history event yet, so its drill is honestly empty — and saying so
   * is different from saying the log is missing rows.
   */
  eventless: boolean;
}

export function subjectOf(turn: Turn): DrillSubject {
  const members = expand(turn);
  const events: number[] = [];
  const types: string[] = [];
  const links: EventLink[] = [];
  for (const m of members) {
    for (const id of m.events) if (!events.includes(id)) events.push(id);
    for (const type of m.types) if (!types.includes(type)) types.push(type);
    if (m.link && !links.some((l) => sameLink(l, m.link!))) links.push(m.link);
  }
  events.sort((a, b) => a - b);
  return { events, types, links, eventless: events.length === 0 };
}

/** Two links naming one execution. An id alone matches an id alone; once either side pins an
 *  execution both must — the same rule `eventDrill.ts` applies to a level. */
function sameLink(a: EventLink, b: EventLink): boolean {
  return a.workflowId === b.workflowId && (a.execId ?? '') === (b.execId ?? '');
}

/** The rows one level of the drill draws, and what it could not draw. */
export interface DrillRows {
  rows: RunEvent[];
  /**
   * Ids the turn names that the log on hand does not carry.
   *
   * PRINTED, NEVER SWALLOWED. `/history` elides events from the MIDDLE of a long log to stay under
   * its cap and says how many; a turn built before the elision can therefore name a row that is no
   * longer there. A drill that quietly showed four of six events would be a check that cannot be
   * trusted, which is worse than no check.
   */
  missing: number[];
}

/**
 * The raw rows behind one turn, out of the log the account was read from.
 *
 * Order is the LOG's, not the turn's: event ids are monotonic, and reading a family in Temporal's
 * own order is what makes a scheduled/started/completed triple legible as one thing that happened.
 */
export function rowsFor(history: RunHistory | null, events: readonly number[]): DrillRows {
  if (!history) return { rows: [], missing: [...events] };
  const wanted = new Set(events);
  const rows = history.events.filter((e) => wanted.has(e.id));
  const found = new Set(rows.map((e) => e.id));
  return { rows, missing: events.filter((id) => !found.has(id)) };
}

/**
 * Every row of a level that can be descended from — the child and Nexus families, which is exactly
 * the set the server put a link on.
 *
 * Deduped by TARGET rather than by row: a child's `Initiated` and its `Started` are two rows and one
 * destination, and offering both would be two buttons that go to the same place. The FIRST is kept,
 * because `drillInto` reads `event.id` for the crumb's tooltip and the opener is the more useful
 * answer to "where did this come from".
 */
export function openings(rows: readonly RunEvent[]): RunEvent[] {
  const out: RunEvent[] = [];
  for (const e of rows) {
    if (!e.link) continue;
    if (out.some((seen) => sameLink(seen.link!, e.link!))) continue;
    out.push(e);
  }
  return out;
}

/**
 * The one row that opens a given workflow, so a turn-level button can hand `drillInto` a real event
 * rather than a synthesised one.
 *
 * WHY NOT A SECOND `drillInto` THAT TAKES A LINK. Because then there would be two functions deciding
 * what a click does, and the return-not-push rule — the one that stops two rows of the same child
 * growing two identical crumbs — would have to be right in both. There is one, and this finds it
 * the event it wants.
 */
export function openerFor(rows: readonly RunEvent[], link: EventLink): RunEvent | undefined {
  return rows.find((e) => e.link && sameLink(e.link, link));
}
