/**
 * Which Machines are one run's — the arithmetic, from real reduced histories.
 *
 * THE FIXTURES ARE EVENTS, NOT TURNS, and that is on purpose: the join this module makes is against
 * `DispatchTurn.actor` and `FleetTurn.fleet`, both of which are DERIVED (`transcript.ts` restores
 * `0.1.0` from the endpoint's collapsed `kontra-nscheck-0-1-0`). A test that handed in hand-built
 * turns would pass while the real derivation drifted, which is this repo's most-recorded way of
 * shipping a green suite over a broken join.
 *
 * WHAT IS PINNED IS THE THREE ABSENCES AS MUCH AS THE MATCHES. "nobody has read this run's account"
 * and "the account is read and there is nothing" are different sentences on screen, so they are
 * different values here, and a test that only checked `machines.length === 0` would let them
 * collapse.
 */

import { describe, expect, it } from 'vitest';

import { categorize } from '@core/history';

import type { RunEvent, RunHistory } from '../run/api';
import { readRunTurns, type RunTurns } from '../run/turns';
import type { Terminal } from './panelsClient';
import { focusedMachine, runMachines, runScope, scopeNames, stackOf } from './runMachines';

const T0 = 1_787_084_868_000;

function ev(id: number, type: string, ms: number, extra: Partial<RunEvent> = {}): RunEvent {
  return {
    id,
    type,
    cat: categorize(type),
    t: ms / 1000,
    at: T0 + ms,
    detail: type,
    attempt: 1,
    dur: 0,
    ...extra,
  };
}

function history(events: RunEvent[]): RunHistory {
  return { events, scanned: events.length, elided: 0, truncated: false };
}

const STARTED = ev(1, 'WorkflowExecutionStarted', 0, {
  detail: 'workflowType=DnsSweep · taskQueue=wf-dnssweep · identity=1@65520c20a6a4',
});

/** One Nexus dispatch against `kontra-<actor>-<version with dots collapsed>`. */
function dispatch(id: number, ms: number, endpoint: string): RunEvent[] {
  return [
    ev(id, 'NexusOperationScheduled', ms, { detail: `endpoint=${endpoint}` }),
    ev(id + 1, 'NexusOperationStarted', ms + 100, {
      detail: `endpoint=${endpoint}`,
      dur: 0.1,
    }),
    ev(id + 2, 'NexusOperationCompleted', ms + 900, {
      detail: `endpoint=${endpoint}`,
      dur: 0.8,
    }),
  ];
}

/** A fleet bring-up: the child event that carries `stackWorkflow` and the stack's workflow id. */
function fleet(id: number, ms: number, stack: string): RunEvent {
  return ev(id, 'StartChildWorkflowExecutionInitiated', ms, {
    detail: `workflowId=kontra-fleet/${stack}`,
    link: { workflowId: `kontra-fleet/${stack}`, type: 'stackWorkflow', via: 'child' },
  });
}

function turnsOf(runId: string, events: RunEvent[]): RunTurns {
  return readRunTurns(runId, history([STARTED, ...events]));
}

/* ───────────────────────────── the inventory ───────────────────────────── */

function terminal(over: Partial<Terminal> & { id: string }): Terminal {
  return {
    machine: '',
    host: '',
    publicIp: '',
    tag: '',
    fleet: '',
    actor: '',
    version: '',
    window: '',
    health: { reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' },
    ...over,
  };
}

/** The caller's own worker, served locally. A workflow session carries NO actor placement — its
 *  `@kontra` tag is `workflow:<name>` — which is exactly why it can only be found by session. */
const WORKER = terminal({
  id: 'local:main-droplet/dnssweep/worker',
  machine: 'main-droplet',
  host: 'main-droplet',
  window: 'worker',
});

/** A local Actor worker, tagged with both halves. */
const ACTOR = terminal({
  id: 'local:main-droplet/nscheck-0_1_0/actor',
  machine: 'main-droplet',
  host: 'main-droplet',
  window: 'actor',
  actor: 'nscheck',
  version: '0.1.0',
});

/** Four fleet Machines of one stack. Each one's session is `<actor>-<version>` by the SAME rule the
 *  local worker's is, which is the collision `paneForSession` cannot resolve and an id can. */
const FLEET = [1, 2, 3, 10].map((n) =>
  terminal({
    id: `fleet:kf-dns-${String(n).padStart(2, '0')}/nscheck-0_1_0/actor`,
    machine: `kf-dns-${String(n).padStart(2, '0')}`,
    host: `10.124.0.${n}`,
    tag: 'dns',
    fleet: 'nscheck-0.1.0',
    actor: 'nscheck',
    version: '0.1.0',
    window: 'actor',
  })
);

/** Somebody else's Machine — a different Actor, a different fleet, on the same appliance. */
const STRANGER = terminal({
  id: 'fleet:kf-crawl-01/webcrawl-0_2_0/actor',
  machine: 'kf-crawl-01',
  host: '10.124.0.20',
  tag: 'crawl',
  fleet: 'webcrawl-0.2.0',
  actor: 'webcrawl',
  version: '0.2.0',
  window: 'actor',
});

/* ───────────────────────────── no machines ───────────────────────────── */

describe('a run with no Machines', () => {
  it('says the account has not been read rather than that there are none', () => {
    // NOBODY LOOKED. Which Actors a run called lives in its account, so before the account arrives
    // the answer is unknown — and a four-Machine sweep drawn as machine-less while its transcript
    // loads is the lie this flag exists to refuse.
    const scoped = runMachines(null, 'dnssweep', [WORKER, ACTOR, ...FLEET]);
    expect(scoped.read).toBe(false);
    expect(scoped.scope.actors).toEqual([]);
    expect(scoped.scope.fleets).toEqual([]);
    // The caller's own session is knowable WITHOUT the account — it comes off the file name — so a
    // Terminal answering to it is still this run's Machine.
    expect(scoped.machines.map((m) => m.terminal.id)).toEqual([WORKER.id]);
  });

  it('separates "named nothing" from "named things and found none of them"', () => {
    // A run that only read a Dataset dispatches to no Actor and brings up no fleet. It has no
    // Machines BY CONSTRUCTION, which is not a failure and not the same as a worker that died.
    const bare = runMachines(turnsOf('r-bare', []), '', [WORKER, ACTOR, ...FLEET]);
    expect(bare.read).toBe(true);
    expect(bare.machines).toEqual([]);
    expect(scopeNames(bare.scope)).toEqual([]);

    // A run that dispatched to an Actor nothing on this appliance is serving. Same empty list, a
    // completely different sentence — and the names it looked under are what make it readable.
    const orphan = runMachines(turnsOf('r-orphan', dispatch(2, 1000, 'kontra-ghost-9-9-9')), '', [
      WORKER,
      STRANGER,
    ]);
    expect(orphan.read).toBe(true);
    expect(orphan.machines).toEqual([]);
    expect(scopeNames(orphan.scope)).toEqual(['ghost@9.9.9']);
  });

  it('never claims a Machine that belongs to another run', () => {
    const scoped = runMachines(turnsOf('r1', dispatch(2, 1000, 'kontra-nscheck-0-1-0')), 'dnssweep', [
      STRANGER,
    ]);
    expect(scoped.machines).toEqual([]);
  });
});

/* ───────────────────────────── one, and many ───────────────────────────── */

describe('a run with one Machine', () => {
  it('is the worker serving the workflow, found by session and not by actor', () => {
    // A caller's workflow session holds NO actor placement, so `actor`/`version` are '' on the wire
    // and the only thing that can find it is the session in its id.
    const scoped = runMachines(turnsOf('r1', []), 'dnssweep', [WORKER, STRANGER]);
    expect(scoped.machines).toHaveLength(1);
    expect(scoped.machines[0]?.reason).toBe('workflow');
    expect(scoped.machines[0]?.because).toContain('dnssweep');
  });

  it('matches an Actor whose version the log could not name, on the actor alone', () => {
    // A dispatch whose endpoint collapsed the version reports ''. Refusing it would hide the
    // Terminal that is plainly the one being asked about.
    const noVersion = terminal({
      id: 'local:main-droplet/kontra-nscheck/actor',
      machine: 'main-droplet',
      actor: 'nscheck',
      window: 'actor',
    });
    const scoped = runMachines(turnsOf('r1', dispatch(2, 1000, 'kontra-nscheck')), '', [noVersion]);
    expect(scoped.scope.actors).toEqual([{ actor: 'nscheck', version: '' }]);
    expect(scoped.machines.map((m) => m.reason)).toEqual(['actor']);
  });

  it('refuses a Terminal running a DIFFERENT known version of the same Actor', () => {
    // `0.1.0`'s worker under `0.2.0`'s dispatch is the exact mistake `actorSession` exists to
    // prevent, and two known versions are never ambiguous.
    const other = terminal({
      id: 'local:main-droplet/nscheck-0_2_0/actor',
      machine: 'main-droplet',
      actor: 'nscheck',
      version: '0.2.0',
      window: 'actor',
    });
    const scoped = runMachines(turnsOf('r1', dispatch(2, 1000, 'kontra-nscheck-0-1-0')), '', [other]);
    expect(scoped.machines).toEqual([]);
  });
});

describe('a run with many Machines', () => {
  const events = [
    fleet(2, 500, 'nscheck-0.1.0'),
    ...dispatch(3, 1000, 'kontra-nscheck-0-1-0'),
    ...dispatch(6, 2000, 'kontra-nscheck-0-1-0'),
    ...dispatch(9, 3000, 'kontra-nscheck-0-1-0'),
  ];
  const scoped = runMachines(turnsOf('r1', events), 'dnssweep', [
    STRANGER,
    ...FLEET,
    ACTOR,
    WORKER,
  ]);

  it('lists the run’s own Machines and nobody else’s', () => {
    expect(scoped.machines.map((m) => m.terminal.id)).not.toContain(STRANGER.id);
    expect(scoped.machines).toHaveLength(6);
  });

  it('puts the caller’s worker first, then everything the run called', () => {
    // The order is the order the questions get asked: "is anything serving my workflow" comes
    // before "which of the four Machines dropped its units".
    expect(scoped.machines[0]?.reason).toBe('workflow');
    expect(scoped.machines[0]?.terminal.id).toBe(WORKER.id);
    // Every fleet Machine here is PLACED with the Actor the run dispatched to, so the actor rule
    // reaches all of them first — which is the more specific fact and the one worth printing.
    expect(scoped.machines.slice(1).map((m) => m.reason)).toEqual([
      'actor',
      'actor',
      'actor',
      'actor',
      'actor',
    ]);
  });

  it('claims a Machine of the run’s own stack even when it carries no Actor placement', () => {
    // `fleet up` with no actor: a Machine with an empty `actor`, which no dispatch can ever match.
    // It is still one of the Machines this run paid for and is still its to look at.
    const bare = terminal({
      id: 'fleet:kf-dns-04/fleet/actor',
      machine: 'kf-dns-04',
      tag: 'dns',
      fleet: 'nscheck-0.1.0',
      window: 'actor',
    });
    const withBare = runMachines(turnsOf('r1', events), 'dnssweep', [bare, WORKER]);
    expect(withBare.machines.map((m) => m.reason)).toEqual(['workflow', 'fleet']);
    expect(withBare.machines[1]?.because).toContain('nscheck-0.1.0');
  });

  it('orders Machines naturally, so kf-dns-10 comes after kf-dns-03', () => {
    const fleetIds = scoped.machines.filter((m) => m.terminal.fleet !== '').map((m) => m.terminal.machine);
    expect(fleetIds).toEqual(['kf-dns-01', 'kf-dns-02', 'kf-dns-03', 'kf-dns-10']);
  });

  it('gives one Terminal exactly one row, whichever rule reaches it first', () => {
    // Every fleet Machine here answers BOTH the actor rule and the fleet rule. Two rows for one
    // screen is a wall that double-counts its own Machines.
    const ids = scoped.machines.map((m) => m.terminal.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it('reads the fleet stack off the child’s workflow id', () => {
    expect(scoped.scope.fleets).toEqual(['nscheck-0.1.0']);
    expect(stackOf('kontra-fleet/nscheck-0.1.0')).toBe('nscheck-0.1.0');
    // No project prefix: taken whole rather than refused, because the stack is what a Terminal
    // carries and the spelling of the id is not this module's to own.
    expect(stackOf('dns')).toBe('dns');
  });

  it('deduplicates the Actors a loop dispatched to, without losing a second one', () => {
    // Three dispatches to one Actor is one entry; a run that alternates between two Actors must
    // still report both, which is why the read expands collapsed groups.
    const two = runScope(
      turnsOf('r2', [
        ...dispatch(2, 1000, 'kontra-nscheck-0-1-0'),
        ...dispatch(5, 2000, 'kontra-nscheck-0-1-0'),
        ...dispatch(8, 3000, 'kontra-webcrawl-0-2-0'),
      ]),
      ''
    );
    expect(two.actors).toEqual([
      { actor: 'nscheck', version: '0.1.0' },
      { actor: 'webcrawl', version: '0.2.0' },
    ]);
  });
});

/* ───────────────────────────── the focused one ───────────────────────────── */

describe('which Machine the tab shows', () => {
  const scoped = runMachines(turnsOf('r1', dispatch(2, 1000, 'kontra-nscheck-0-1-0')), 'dnssweep', [
    ...FLEET,
    WORKER,
  ]);

  it('falls back to the first when nothing is asked for', () => {
    expect(focusedMachine(scoped.machines, null)?.terminal.id).toBe(WORKER.id);
  });

  it('honours an id that carries a colon AND a dot — the shape a deep link has', () => {
    // `<mode>:<node>/<session>/<window>` with a tmux window of `0.1`: one colon, two slashes, one
    // dot, which is every byte the SPA fallback was caught by.
    const dotted = terminal({
      id: 'local:main-droplet/kontra-recon/0.1',
      machine: 'main-droplet',
      window: '0.1',
      actor: 'nscheck',
      version: '0.1.0',
    });
    const withDotted = runMachines(
      turnsOf('r1', dispatch(2, 1000, 'kontra-nscheck-0-1-0')),
      'dnssweep',
      [WORKER, dotted]
    );
    expect(focusedMachine(withDotted.machines, 'local:main-droplet/kontra-recon/0.1')?.terminal.id).toBe(
      'local:main-droplet/kontra-recon/0.1'
    );
  });

  it('refuses an id that is not in this run’s scope, rather than drawing another run’s Machine', () => {
    expect(focusedMachine(scoped.machines, STRANGER.id)?.terminal.id).toBe(WORKER.id);
  });

  it('is null when there is nothing to show', () => {
    expect(focusedMachine([], 'anything')).toBeNull();
  });
});
