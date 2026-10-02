/**
 * THE BOUNDARY AROUND PORTER, AS A TEST (issue 11).
 *
 * DECIDED 2026-09-29: the compose network stays Porter's boundary and no credential is put in
 * front of it. That is the right trade for one operator on one host, and this file is the
 * condition attached to it — the decision rests on one absent line, so the absence is asserted
 * rather than remembered.
 *
 * `docs/THREAT_MODEL.md` §4 carries the decision itself, its acceptance, and the trigger that
 * reopens it. This carries the enforcement.
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import { composeService, publishedPorts } from './compose';

const COMPOSE = join(__dirname, '..', '..', '..', 'docker-compose.yml');
const yaml = (): string => readFileSync(COMPOSE, 'utf8');

describe('the compose network is Porter’s boundary', () => {
  /**
   * THE ONE THAT MATTERS. Porter has no authentication and no TLS, binds every interface inside
   * its container, and runs arbitrary SQL against the whole lake. Publishing a host port for it
   * puts an unauthenticated SQL engine on the host network.
   */
  it('publishes NO host port for porter', () => {
    const ports = publishedPorts(yaml(), 'porter');
    expect(
      ports,
      'porter has no authentication and no TLS and executes arbitrary SQL over every workspace’s ' +
        'data. It is reachable only on the compose network, with the orchestrator as the ' +
        'authenticated front door. Publishing a host port removes the only control there is — see ' +
        'docs/THREAT_MODEL.md §4 and .scratch/flight-and-observability/issues/' +
        '11-the-network-is-porters-boundary.md before changing this.'
    ).toEqual([]);
  });

  /**
   * `victorialogs` has the same posture for the same reason, and `routes/logs.ts` argues it at
   * length. Asserted here too so the pair cannot drift apart — one of them growing a port would
   * otherwise be a precedent for the other.
   */
  it('publishes no host port for victorialogs either', () => {
    expect(publishedPorts(yaml(), 'victorialogs')).toEqual([]);
  });

  /**
   * THE TEST IS CHECKED AGAINST ITS OWN FAILURE, which is the difference between a guard and a
   * green tick. A `ports:` entry in either spelling compose accepts must be SEEN — otherwise the
   * assertion above passes because the parser found nothing, not because there is nothing.
   */
  describe('and the guard itself fails when it should', () => {
    const withPorts = (block: string) => `services:\n  porter:\n${block}  other:\n    image: x\n`;

    it('sees a block sequence', () => {
      expect(publishedPorts(withPorts('    image: p\n    ports:\n      - "32010:32010"\n'), 'porter')).toEqual([
        '32010:32010',
      ]);
    });

    it('sees an inline flow list', () => {
      expect(publishedPorts(withPorts('    image: p\n    ports: ["32010:32010", 9091]\n'), 'porter')).toEqual([
        '32010:32010',
        '9091',
      ]);
    });

    it('sees the long form', () => {
      expect(
        publishedPorts(
          withPorts('    image: p\n    ports:\n      - target: 32010\n        published: "32010"\n'),
          'porter'
        )
      ).toContain('32010');
    });

    /**
     * AND IT DOES NOT SEE PROSE. The compose file discusses ports constantly, including directly
     * above this service; a guard that matched the word in a comment would fail on a file that is
     * correct, which gets guards deleted.
     */
    it('does not match the word in a comment', () => {
      expect(
        publishedPorts(withPorts('    image: p\n    # do not add a ports: entry to this service\n'), 'porter')
      ).toEqual([]);
    });

    /** A renamed service is an ERROR, never a vacuous pass. */
    it('refuses to pass vacuously when the service is gone', () => {
      expect(() => publishedPorts('services:\n  something-else:\n    image: x\n', 'porter')).toThrow(
        /no service "porter"/
      );
    });

    it('reads a service block without running into the next one', () => {
      const body = composeService(withPorts('    image: p\n    ports:\n      - "1:1"\n'), 'porter');
      expect(body.some((l) => l.includes('other'))).toBe(false);
    });
  });
});
