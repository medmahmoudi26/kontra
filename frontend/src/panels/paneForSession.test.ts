/**
 * Which pane a workbench draws when two Machines answer to one session name.
 *
 * This is a real collision and not a hypothetical: `backend/src/panels/discovery.ts:
 * sessionNameFor` names a fleet Machine's session `<actor>-<version>`, which is exactly what
 * `@core/panels/tmux:actorSession` derives for the local worker — so a droplet running probe 0.1.0 and the worker
 * the Serve button just started are both called `probe-0_1_0`.
 */

import { describe, expect, it } from 'vitest';
import { paneForSession } from './paneForSession';
import type { Terminal } from './panelsClient';

function terminal(id: string, host: string): Terminal {
  return {
    id,
    machine: host,
    host,
    publicIp: host,
    tag: '',
    fleet: '',
    actor: 'probe',
    version: '0.1.0',
    window: 'actor',
    health: { reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' },
  };
}

const FLEET = terminal('fleet:n1/probe-0_1_0/actor', '10.124.0.3');
const LOCAL = terminal('local:this/probe-0_1_0/actor', 'localhost');
const OTHER = terminal('local:this/beacon-0_1_0/actor', 'localhost');

describe('the pane beside the editor', () => {
  it('is the local one when a fleet Machine answers to the same name', () => {
    // The console prints `tmux attach -t probe-0_1_0`, and that command on THIS host reaches the
    // local session. A droplet's screen above it would be a different worker under one name.
    expect(paneForSession([FLEET, LOCAL], 'probe-0_1_0')?.id).toBe(LOCAL.id);
    // …whichever order the inventory listed them in. A `find` returned the first, which is the
    // fleet's on every wall that has one.
    expect(paneForSession([LOCAL, FLEET], 'probe-0_1_0')?.id).toBe(LOCAL.id);
  });

  it('shows a remote pane when there is no local worker', () => {
    // An Actor only the fleet is running still has a pane worth reading; the pane's header names
    // the host it is on.
    expect(paneForSession([FLEET], 'probe-0_1_0')?.id).toBe(FLEET.id);
  });

  it('matches on the session, not on the actor the tile is tagged with', () => {
    // Two versions of one Actor are two sessions and two workers. Tagging is how the Actors grid
    // counts Machines; the workbench is about ONE build.
    expect(paneForSession([OTHER], 'probe-0_1_0')).toBeNull();
  });

  it('finds nothing for no session rather than matching whatever has no name', () => {
    expect(paneForSession([LOCAL], '')).toBeNull();
  });
});
