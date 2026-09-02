/**
 * Which Machines are THIS run's — the arithmetic behind the Monitor tab, kept out of the component.
 *
 * THE MONITOR USED TO BE A WALL OF EVERYTHING. Sixty tiles, every Terminal the streamer can see,
 * and an operator who had just started a run filtering it by hand to find the four Machines that
 * were theirs. That is the same shape of failure the global Runs surface had: the run and the thing
 * the run is doing were two different searches. So the tab is scoped to the open run, and this
 * module is what "scoped" means — a pure function from the run's OWN account plus the inventory to
 * the Terminals that account names, with the reason each one is in it.
 *
 * THREE WAYS A TERMINAL BELONGS TO A RUN, and they are three different facts rather than three
 * spellings of one:
 *
 *   workflow  it is the session the caller's own worker was served into. This is the pane that
 *             answers "my run is not moving" — a worker that boots and dies leaves the run simply
 *             waiting, with no status anywhere changing, and the traceback is only ever here.
 *   actor     the run DISPATCHED to that Actor at that version. Read off the run's own turns, so a
 *             Terminal is in scope because the log says the run called it, never because it happens
 *             to be running something with a similar name.
 *   fleet     the run brought the stack up itself. `FleetTurn.fleet` is `kontra-fleet/<stack>` and a
 *             Terminal's `fleet` field IS that stack (`<actor>-<version>`), so the join is the
 *             project prefix removed and nothing else.
 *
 * NOTHING HERE INFERS A MACHINE'S HEALTH FROM A RUN, and nothing infers a run from a Machine
 * (CONTEXT-MAP.md). A Terminal is in scope because the run's log named the thing it is serving; what
 * it is DOING is the streamer's to say, and the tile says it.
 *
 * "NOBODY LOOKED" IS NOT "NOTHING THERE", which is why {@link RunMachines} carries `read` beside the
 * list. A run whose account has not arrived has an unknown set of Actors — not an empty one — and a
 * panel that drew both as "no machines" would report a four-Machine sweep as machine-less for as
 * long as the transcript took to load. The caller's own worker is knowable without the account (its
 * session comes from the workflow's file name), so it is resolved either way.
 *
 * PURE, AND GENERIC OVER THE TERMINAL. It reads six fields, so it is testable with six-field
 * fixtures the way `topology.ts` is — and it hands the caller's OWN objects back, so the component
 * can put a real `panelsClient.Terminal` into a pane without a second lookup.
 */

import { expand, type Turn } from '@core/transcript';

import type { RunTurns } from '../run/turns';
import { tileRefFor } from './chrome/tileRef';
import { compareNatural } from './selectors';

/**
 * The Pulumi project every fleet operation's workflow id is under — `kontra-fleet/<stack>`
 * (`cli/fleet.go:fleetProject`, and `run/api.ts` says the same about `EventLink.workflowId`).
 *
 * A SIXTH DERIVATION OF A LITERAL THAT LIVES IN GO, written out here rather than imported, which is
 * this repo's convention for exactly this shape of value. The failure mode of a drift is quiet and
 * bounded: no fleet Machine joins, so the tab lists the actor workers and says so.
 */
export const FLEET_PROJECT = 'kontra-fleet';

/** What this module reads off a Terminal. A subset of `panelsClient.Terminal`, so a fixture is six
 *  fields and so anything holding less than a whole one can still be scoped. */
export interface ScopedTerminal {
  id: string;
  /** The node, as the wire names it — needed only as {@link tileRefFor}'s fallback. */
  machine: string;
  window: string;
  /** '' when the session carries no Actor placement: a caller's workflow session is one of those. */
  actor: string;
  version: string;
  /** The stack these Machines are — `<actor>-<version>`. '' off the fleet. */
  fleet: string;
}

/** Why a Terminal is in the run's scope. Three facts, never merged. */
export type MachineReason = 'workflow' | 'actor' | 'fleet';

export interface RunMachine<T extends ScopedTerminal> {
  terminal: T;
  reason: MachineReason;
  /** The sentence the row shows. Written here so the panel and its test cannot drift. */
  because: string;
}

/** One `(Actor, version)` the run dispatched to. `version` is '' when neither the endpoint nor the
 *  task queue named one — see `transcript.ts`'s `DispatchTurn.version`. */
export interface ScopedActor {
  actor: string;
  version: string;
}

/** What the run named, before anything was matched against the inventory. Printed when nothing
 *  matches, because "we looked for these four things and found none of them" is a reading and
 *  "no machines" is not. */
export interface RunScope {
  /** The tmux session the caller's own worker is served into. '' when the workflow has no name. */
  session: string;
  actors: ScopedActor[];
  /** Stack names, with `kontra-fleet/` already off. */
  fleets: string[];
}

export interface RunMachines<T extends ScopedTerminal> {
  machines: RunMachine<T>[];
  scope: RunScope;
  /**
   * The run's account has arrived, so `scope.actors` and `scope.fleets` are ANSWERS.
   *
   * While this is false they are empty because nothing has been read, which is not the same as the
   * run having dispatched nowhere — and only one of those two is worth telling an operator about.
   */
  read: boolean;
}

/** `kontra-fleet/nscheck-0.1.0` → `nscheck-0.1.0`. An id with no project prefix is taken whole:
 *  the stack is what a Terminal carries, and refusing an unprefixed id would drop a real fleet on a
 *  spelling this module does not own. */
export function stackOf(fleetWorkflowId: string): string {
  const prefix = `${FLEET_PROJECT}/`;
  return fleetWorkflowId.startsWith(prefix) ? fleetWorkflowId.slice(prefix.length) : fleetWorkflowId;
}

/** `probe` at `0.1.0`, or just `probe` when the log named no version. */
function actorLabel(a: ScopedActor): string {
  return a.version ? `${a.actor}@${a.version}` : a.actor;
}

/**
 * What the run named, read from its own turns.
 *
 * COLLAPSED GROUPS ARE EXPANDED FIRST. Two hundred dispatches in one loop fold into a single turn
 * (`transcript.ts`), and the fold keeps the FIRST member's actor — so a run that alternates between
 * two Actors would report one of them if this read the group's head. `expand` is the only sanctioned
 * way to reach a group's members, and it is one call.
 */
export function runScope(turns: RunTurns | null, session: string): RunScope {
  const scope: RunScope = { session, actors: [], fleets: [] };
  if (!turns) return scope;

  const seenActor = new Set<string>();
  const seenFleet = new Set<string>();
  for (const named of turns.named.turns) {
    for (const turn of expand(named.turn) as Turn[]) {
      if (turn.kind === 'dispatch') {
        // An endpoint neither the Nexus endpoint nor the task queue named is a dispatch we cannot
        // attribute. Kept out rather than added as '' — a blank actor matches every Terminal whose
        // session carries no placement, which is every caller's worker on the box.
        if (turn.actor === '') continue;
        const key = `${turn.actor}@${turn.version}`;
        if (seenActor.has(key)) continue;
        seenActor.add(key);
        scope.actors.push({ actor: turn.actor, version: turn.version });
      } else if (turn.kind === 'fleet') {
        const stack = stackOf(turn.fleet);
        if (stack === '' || seenFleet.has(stack)) continue;
        seenFleet.add(stack);
        scope.fleets.push(stack);
      }
    }
  }
  return scope;
}

/**
 * Does this Terminal serve that Actor?
 *
 * AN EMPTY VERSION ON EITHER SIDE MATCHES ON THE ACTOR ALONE, and both empties are real. A local
 * session created before the `@kontra` tag carried one reports no version (`panels/local.ts`), and a
 * dispatch whose endpoint collapsed the version reports none either. Refusing those would hide a
 * Terminal that is plainly the one being asked about; matching them across DIFFERENT known versions
 * would put `0.1.0`'s worker under `0.2.0`'s dispatch, which is the mistake `actorSession` exists
 * to prevent, so that one stays refused.
 */
function servesActor(terminal: ScopedTerminal, want: ScopedActor): boolean {
  if (terminal.actor === '' || terminal.actor !== want.actor) return false;
  return want.version === '' || terminal.version === '' || terminal.version === want.version;
}

/**
 * The run's Machines, in the order an operator asks about them.
 *
 * THE CALLER'S OWN WORKER FIRST, because "is anything even serving my workflow" is the question that
 * comes before every other one on this tab. Then the Actors it dispatched to, then the fleet it
 * brought up. Within each, `compareNatural` on the id — so `kf-crawl-2` precedes `kf-crawl-10` and
 * two polls of one inventory draw the same list.
 *
 * ONE TERMINAL, ONE ROW. A fleet Machine placed with the Actor the run dispatched to answers two
 * rules; it is listed once, under the first that claimed it, because two rows for one screen is a
 * wall that double-counts its own Machines.
 */
export function runMachines<T extends ScopedTerminal>(
  turns: RunTurns | null,
  session: string,
  terminals: readonly T[]
): RunMachines<T> {
  const scope = runScope(turns, session);
  const claimed = new Set<string>();
  const buckets: Record<MachineReason, RunMachine<T>[]> = { workflow: [], actor: [], fleet: [] };

  const claim = (terminal: T, reason: MachineReason, because: string): void => {
    if (claimed.has(terminal.id)) return;
    claimed.add(terminal.id);
    buckets[reason].push({ terminal, reason, because });
  };

  if (scope.session !== '') {
    for (const terminal of terminals) {
      // The session lives in the id and not on the wire — `chrome/tileRef.ts`'s header says why —
      // and `tileRefFor` is the browser's one parse of it. A second one here is how the Actors page
      // and the Monitor start disagreeing about which Machines are up.
      if (tileRefFor(terminal).session !== scope.session) continue;
      claim(terminal, 'workflow', `serves this workflow’s own worker (session ${scope.session})`);
    }
  }

  for (const want of scope.actors) {
    for (const terminal of terminals) {
      if (!servesActor(terminal, want)) continue;
      claim(terminal, 'actor', `this run dispatched to ${actorLabel(want)}`);
    }
  }

  for (const stack of scope.fleets) {
    for (const terminal of terminals) {
      if (terminal.fleet === '' || terminal.fleet !== stack) continue;
      claim(terminal, 'fleet', `this run brought up the fleet ${stack}`);
    }
  }

  const byId = (a: RunMachine<T>, b: RunMachine<T>): number =>
    compareNatural(a.terminal.id, b.terminal.id);
  return {
    machines: [
      ...buckets.workflow.sort(byId),
      ...buckets.actor.sort(byId),
      ...buckets.fleet.sort(byId),
    ],
    scope,
    read: turns !== null,
  };
}

/**
 * The Machine the tab should show, given what the operator asked for.
 *
 * A FOCUS THAT IS NOT IN SCOPE IS NOT HONOURED, and that is the point of routing it through here.
 * `?pane=` survives a reload and survives switching runs in the same tab, so the id in the bar can
 * name a Terminal that belonged to the run before last — and drawing it under this run's heading is
 * the same lie `transcriptFor` refuses one layer up. The first Machine stands instead, which is the
 * caller's own worker whenever there is one.
 */
export function focusedMachine<T extends ScopedTerminal>(
  machines: readonly RunMachine<T>[],
  wanted: string | null
): RunMachine<T> | null {
  if (machines.length === 0) return null;
  const asked = wanted === null ? undefined : machines.find((m) => m.terminal.id === wanted);
  return asked ?? machines[0] ?? null;
}

/** Every name the run looked under, for the sentence a scope with no Machines prints. Empty when
 *  the run named nothing at all — a different reading, and the caller tells them apart. */
export function scopeNames(scope: RunScope): string[] {
  return [
    ...(scope.session === '' ? [] : [scope.session]),
    ...scope.actors.map(actorLabel),
    ...scope.fleets,
  ];
}
