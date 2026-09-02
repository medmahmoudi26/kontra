/**
 * Descending from a run's event log into what it caused.
 *
 * WHY THIS IS A STACK AND NOT A BACK BUTTON. The rows this navigates are the ones the log can say
 * least about: a dispatch and a fleet bring-up put every fact worth reading inside a claim-checked
 * payload the log refuses to decode (ADR 0007), so all a row can print is a type and a timestamp.
 * MEASURED on run `nscheck-1786831339` — 294 seconds — the `kontra-fleet/dns` child is four such
 * rows covering 156 of them, and the twelve dispatches after it are thirty-six more. The detail is
 * in each child's own history, and a child that dispatches has children of its own, so "back" has
 * to mean "back to WHICH level".
 *
 * Everything here is pure. The fetching, the polling and the rendering live in `TranscriptDrill.tsx`;
 * what a click DOES is decided here, where it can be tested without a browser.
 */

import type { EventLink, RunEvent } from '../run/api';

/** One level of the log: the workflow whose history is on screen. */
export interface DrillLevel {
  /** The workflow id this level shows. At the root it is the run id, because a **Run** is
   *  identified by its caller workflow's id and by nothing else. */
  workflowId: string;
  /** Temporal's run id, when the level was reached from an event that pinned one. Never called a
   *  Run: it exists because a workflow id can be reused and the history route answers with the
   *  latest execution under a bare id. */
  execId?: string;
  /** The event that opened this level, for the crumb's tooltip. Absent at the root. */
  from?: number;
  /** The workflow type, when the event that opened this level named one. */
  type?: string;
}

/** The level a run's log opens at. */
export function rootLevel(runId: string): DrillLevel {
  return { workflowId: runId };
}

/** Is this a row you can descend from? True for exactly the rows the server put a link on — the
 *  child-workflow and Nexus families — and false for every other event. */
export function isNavigable(event: Pick<RunEvent, 'link'>): boolean {
  return event.link !== undefined;
}

/**
 * Descend into the workflow a row names.
 *
 * A TARGET ALREADY IN THE STACK IS A RETURN, NOT A PUSH. Two rows of the same child (its start and
 * its completion) are one destination, so clicking the second while looking at the first must not
 * grow a stack of identical crumbs — and a history that somehow named an ancestor cannot build a
 * cycle you have to click your way out of.
 *
 * An event with no link returns the stack unchanged, so a caller never has to ask twice.
 */
export function drillInto(stack: readonly DrillLevel[], event: RunEvent): DrillLevel[] {
  const link = event.link;
  if (!link) return [...stack];
  const known = stack.findIndex((lv) => sameTarget(lv, link));
  if (known >= 0) return popTo(stack, known);
  const level: DrillLevel = { workflowId: link.workflowId, from: event.id };
  if (link.execId) level.execId = link.execId;
  if (link.type) level.type = link.type;
  return [...stack, level];
}

/** Return to one level of the chain, dropping everything below it. Out-of-range indexes clamp
 *  rather than throw: a stale click on a crumb that a run change has already shortened is an
 *  ordinary race, not an error. */
export function popTo(stack: readonly DrillLevel[], index: number): DrillLevel[] {
  const at = Math.min(Math.max(index, 0), stack.length - 1);
  return stack.slice(0, at + 1);
}

/** Do a level and a link name the same execution? An id alone matches an id alone; once either
 *  side pins an execution, both must. */
function sameTarget(level: DrillLevel, link: EventLink): boolean {
  return level.workflowId === link.workflowId && (level.execId ?? '') === (link.execId ?? '');
}

/** How much of a long workflow id a crumb or a button shows. The tail is the discriminating end —
 *  twelve dispatches of one run differ only in their last eight characters
 *  (`…-nscheck-60c3bfac`), and a head-truncated id would print the same string twelve times. */
export const ID_KEEP = 26;

/** `actor-nscheck-nscheck-1786831339-nscheck-60c3bfac` → `…nscheck-60c3bfac`. Whole ids are left
 *  alone, and a cut lands on a separator: half a segment (`…786831339-nscheck-…`) reads as a
 *  corrupted number rather than as a shortened name. */
export function shortId(workflowId: string): string {
  if (workflowId.length <= ID_KEEP) return workflowId;
  const tail = workflowId.slice(-ID_KEEP);
  const cut = tail.indexOf('-');
  return `…${cut > 0 && cut < ID_KEEP - 8 ? tail.slice(cut + 1) : tail}`;
}

/** What the drill button on a row says. The workflow TYPE when the event named one — `stackWorkflow`
 *  is what an operator is looking for on a fleet row — and the id's tail otherwise, which for a
 *  dispatch is the only thing that distinguishes it from the eleven beside it. */
export function linkLabel(link: EventLink): string {
  return link.type || shortId(link.workflowId);
}

/** The workflow-close events. A history ending in one of these will not grow again, which is what
 *  stops a drilled level polling forever. `ContinuedAsNew` is deliberately absent — that workflow
 *  id is still going, under a new execution the same route will answer with. */
const CLOSED = /^WorkflowExecution(Completed|Failed|TimedOut|Terminated|Canceled|Cancelled)$/;

/** Has this workflow finished? Read from the log itself rather than from a second describe call —
 *  the last event of a closed execution is always its close. */
export function historyClosed(events: readonly RunEvent[]): boolean {
  const last = events[events.length - 1];
  return last !== undefined && CLOSED.test(last.type);
}

/** Wall time from a history's first event to its last, in ms — what a drilled level puts on the
 *  clock the run's own elapsed time occupies at the root. MEASURED: for the fleet child of
 *  `nscheck-1786831339` this is the 156 seconds the run looked idle. */
export function spanMs(events: readonly RunEvent[]): number {
  const first = events[0];
  const last = events[events.length - 1];
  if (!first || !last || !first.at || !last.at) return 0;
  return Math.max(0, last.at - first.at);
}
