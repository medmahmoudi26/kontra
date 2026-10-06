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

/**
 * REDIS MUST NOT EVICT, and this is a correctness assertion rather than a tuning one.
 *
 * `volatile-lru` evicts keys THAT HAVE A TTL. Every volatile key in kontra's keyspace is one
 * actor's state hash (`kontra-actor:<actorId>`) carrying a 24 h TTL, and that hash is where the
 * engine keeps its per-unit commit markers and resume scratch (`runtime/go/statekv`,
 * `runtime/python/internals/statekv.py`). So memory pressure silently deleted the record of which
 * Units had committed, and a retry then re-ran finished work or skipped unfinished work.
 *
 * `cli/appliance/kv/keyspace.go` already refuses rather than evicting its last protected key,
 * saying evicting one "does not degrade a run, it silently corrupts it". The actor hash had the
 * same property and none of the protection. These assertions are what keep the Compose store
 * agreeing with the appliance.
 */
describe('the state store refuses rather than forgetting', () => {
  const redis = (): string[] => composeService(yaml(), 'redis');

  it('runs redis with maxmemory-policy noeviction', () => {
    const command = redis().join(' ');
    expect(command).toContain('noeviction');
    // The specific policy that caused this, named so a revert is recognisable rather than merely
    // different.
    expect(command).not.toContain('volatile-lru');
    expect(command).not.toContain('allkeys-lru');
  });

  it('fsyncs every write, so an acknowledged commit marker is on disk', () => {
    // `appendonly yes` alone leaves `appendfsync everysec`, and that second is a window where a
    // commit marker is acknowledged and then lost to a crash.
    const command = redis().join(' ');
    expect(command).toContain('--appendonly');
    expect(command).toContain('--appendfsync');
    expect(command).toContain('always');
  });

  /** NON-VACUOUS: a helper that returned nothing would pass both assertions above. */
  it('is reading a real redis service block', () => {
    expect(redis().join(' ')).toContain('redis-server');
  });
});

/**
 * THE VPC OVERLAY MUST NOT PUBLISH AN UNAUTHENTICATED STATE STORE.
 *
 * In `docker-compose.yml` Redis publishes on loopback and loopback IS the control (ADR 0056).
 * `docker-compose.vpc.yml` publishes it to the fleet network so a Machine can reach it — and that
 * store holds every actor's commit map and every `global_state` entry. Without a password, anything
 * on that VPC can rewrite a running Run's commit map.
 *
 * `:?` rather than a default on purpose: a default password is the one an attacker tries first, and
 * Redis accepts a BLANK `requirepass` as "no password" — so the failure mode of forgetting would be
 * exactly the exposure.
 */
describe('the VPC overlay authenticates the state store', () => {
  const OVERLAY = join(__dirname, '..', '..', '..', 'docker-compose.vpc.yml');
  const overlay = (): string => readFileSync(OVERLAY, 'utf8');

  it('sets requirepass on redis', () => {
    expect(overlay()).toContain('--requirepass');
  });

  it('takes the password from a variable with NO default', () => {
    // `${KONTRA_REDIS_PASSWORD:?…}` refuses to start and names the variable. `:-` would silently
    // substitute, and a blank substitution is an open store.
    expect(overlay()).toMatch(/KONTRA_REDIS_PASSWORD:\?/);
    expect(overlay()).not.toMatch(/KONTRA_REDIS_PASSWORD:-/);
  });

  it('hands the same password to every role that reads the store', () => {
    // A role that does not get it fails with NOAUTH on its first state access, which names neither
    // the file nor the variable. Three roles talk to Redis: the API, the CLI and infra.
    const uses = overlay().match(/KONTRA_REDIS_PASSWORD/g) ?? [];
    expect(uses.length).toBeGreaterThanOrEqual(4); // one on redis itself + one per role
  });

  /** NON-VACUOUS: a path that read nothing would pass all three assertions above. */
  it('is reading the overlay and not an empty string', () => {
    expect(overlay()).toContain('KONTRA_VPC_BIND');
    expect(overlay().length).toBeGreaterThan(500);
  });

  it('leaves the default stack without a password requirement', () => {
    // The whole point of the split: a bare `docker compose up` must still work with no `.env`.
    expect(yaml()).not.toContain('requirepass');
  });
});
