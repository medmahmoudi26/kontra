/**
 * A Worker on THIS box attributes to the local node.
 *
 * The bug this pins was a permanent red alarm on every local pane: `discovery.ts` calls the local
 * node `localhost`, but a Worker running on it identifies itself with `os.hostname()` — on the
 * controller this was written against, `main-droplet`. The two denote one machine and the compare
 * said otherwise, so a healthy local actor reported
 * `poller: NONE … but none from localhost — its kontra-handler.service is probably down`
 * while its handler was up and polling. Measured live: `kontra workers list` showed
 * `4033433@main-droplet@` polling 30s earlier at the same moment the tile showed the red chip.
 */

import { describe, expect, it } from 'vitest';

import { hostIsMachine, pollerFor, type QueueState } from './pollers';

const QUEUE = (identities: string[]): QueueState => ({
  queue: 'nscheck-0.1.0',
  identities,
  workers: identities.map((identity) => ({ identity, lastPoll: 1 })),
  lastPoll: identities.length === 0 ? 0 : 1,
});

describe('hostIsMachine', () => {
  it('attributes a Machine to its own name, and an FQDN to its first label', () => {
    expect(hostIsMachine('kf-crawl-01', 'kf-crawl-01')).toBe(true);
    expect(hostIsMachine('kf-crawl-01.internal', 'kf-crawl-01')).toBe(true);
    expect(hostIsMachine('kf-crawl-02', 'kf-crawl-01')).toBe(false);
  });

  it("attributes this box's own hostname to the local node", () => {
    expect(hostIsMachine('main-droplet', 'localhost', 'main-droplet')).toBe(true);
    expect(hostIsMachine('main-droplet.internal', 'localhost', 'main-droplet')).toBe(true);
    expect(hostIsMachine('localhost', 'localhost', 'main-droplet')).toBe(true);
    expect(hostIsMachine('127.0.0.1', 'localhost', 'main-droplet')).toBe(true);
  });

  it('does NOT attribute a Machine to the local node just because the node is local', () => {
    // The exception must not become "anything counts as localhost" — a fleet Machine polling a
    // queue is not this box's handler, and reporting it as one would hide a dead local handler.
    expect(hostIsMachine('kf-crawl-01', 'localhost', 'main-droplet')).toBe(false);
  });

  it('is unaffected on a Machine whose node name is not local', () => {
    expect(hostIsMachine('main-droplet', 'kf-crawl-01', 'main-droplet')).toBe(false);
  });
});

describe('pollerFor, on the local node', () => {
  it('reads a Worker identified by this box as live, not as a down handler', () => {
    // The hostname is passed in, not read from `os` — `pollers.ts` is in the browser bundle.
    const v = pollerFor('localhost', QUEUE(['4033433@main-droplet@']), 'main-droplet');
    expect(v.poller).toBe('live');
    expect(v.detail).toBeUndefined();
  });

  it('still reports none when only a FLEET Machine polls the queue', () => {
    const v = pollerFor('localhost', QUEUE(['41@kf-crawl-01@']), 'main-droplet');
    expect(v.poller).toBe('none');
    expect(v.detail).toContain('kf-crawl-01');
  });
});
