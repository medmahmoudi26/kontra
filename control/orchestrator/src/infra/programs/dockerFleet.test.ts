import { describe, expect, it } from 'vitest';
import { assignmentFor } from './dockerFleet';
import { coerceDockerFleetArgs, DOCKER_FLEET_PROJECT, planFor } from '../stacks';

describe('dockerFleet coerce', () => {
  it('keeps tag, machines, network, socket and drops unknown keys', () => {
    const out = coerceDockerFleetArgs({
      tag: 'hello',
      machines: '1',
      network: 'kontra',
      dockerSock: '/var/run/docker.sock',
      evil: 'rm -rf',
    } as Record<string, unknown>);
    expect(out).toEqual({
      tag: 'hello',
      machines: 1,
      network: 'kontra',
      dockerSock: '/var/run/docker.sock',
    });
  });

  it('plans the docker project with an empty provider env', () => {
    const plan = planFor({
      stackFqn: `${DOCKER_FLEET_PROJECT}/hello-0.1.0`,
      args: { tag: 'hello', machines: 1 },
    });
    expect(plan.providerEnvVar).toBe('');
    expect(plan.credential).toEqual({ name: '' });
  });
});

describe('dockerFleet assignment', () => {
  it('refuses a placement with no digest-pinned worker image', () => {
    expect(() =>
      assignmentFor([{ actorName: 'hello', bundleUrl: 'http://x' }], 1, 0)
    ).toThrow(/workerImage/);
  });

  it('pins a Worker onto the first Machine of its prefix', () => {
    const image =
      'registry:5000/hello@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';
    const got = assignmentFor(
      [{ actorName: 'hello', bundleUrl: 'http://x', workerImage: image, actorVersion: '0.1.0' }],
      1,
      0
    );
    expect(got.workers).toEqual([
      { name: 'hello', version: '0.1.0', image, env: {} },
    ]);
  });
});
