/**
 * Local Docker fleet (ADR 0047). Sibling of `programs/fleet.ts`: same inventory shape, same
 * placement fold, Pulumi `@pulumi/docker` instead of Droplets, and no cloud credential.
 *
 * Each Machine is a Warden container on the kontra Compose network. It mounts the host Docker
 * socket and starts actor Workers as sibling containers. That socket is host-level Docker
 * authority — acceptable for a single-operator laptop, not tenant isolation.
 */

import * as docker from '@pulumi/docker';
import * as pulumi from '@pulumi/pulumi';

import {
  machinesFor,
  placementsOf,
  validateTag,
  type FleetArgs,
  type MachineEntry,
  type PlacementArgs,
} from './fleet';

export interface DockerFleetArgs extends FleetArgs {
  /** Compose network Machines join. Default `kontra`. */
  network?: string;
  /** Host Docker socket bind-mounted into each Warden. Default `/var/run/docker.sock`. */
  dockerSock?: string;
}

const DEFAULT_NETWORK = 'kontra';
const DEFAULT_SOCK = '/var/run/docker.sock';
const DEFAULT_WARDEN_IMAGE = process.env.KONTRA_IMAGE || 'kontra:latest';

export function dockerFleetProgram(args: DockerFleetArgs) {
  return async (): Promise<Record<string, unknown>> => {
    validateTag(args.tag);
    if (!Number.isInteger(args.machines) || args.machines < 0) {
      throw new Error(`machines must be a non-negative integer, got ${args.machines}`);
    }

    const network = args.network || DEFAULT_NETWORK;
    const sock = args.dockerSock || DEFAULT_SOCK;
    const wardenImage = args.image || DEFAULT_WARDEN_IMAGE;
    const controller = args.controller || process.env.KONTRA_CONTROLLER || 'orchestrator-api';
    const placements = placementsOf(args);

    const machines = Array.from({ length: args.machines }, (_, i) => {
      const name = `kf-${args.tag}-${String(i + 1).padStart(2, '0')}`;
      const assigned = assignmentFor(placements, args.machines, i, args.namespace);
      const container = new docker.Container(
        name,
        {
          name,
          image: wardenImage,
          hostname: name,
          command: ['kontra', 'warden', 'serve', '--driver', 'docker', '--local'],
          envs: [
            `KONTRA_ADDRESS=${process.env.KONTRA_ADDRESS || 'temporal:7233'}`,
            // The run's workspace namespace (ADR 0051): the Warden writes it into this Machine's
            // certificate on first boot, and every Worker it supervises polls there.
            `KONTRA_NAMESPACE=${args.namespace || process.env.KONTRA_NAMESPACE || 'default'}`,
            `KONTRA_REDIS_HOST=${process.env.KONTRA_REDIS_HOST || 'redis:6379'}`,
            `KONTRA_S3_ENDPOINT=${process.env.KONTRA_S3_ENDPOINT || 'http://seaweed-s3:8333'}`,
            `KONTRA_ORCHESTRATOR_URL=${process.env.KONTRA_ORCHESTRATOR_URL || 'http://orchestrator-api:8088'}`,
            `KONTRA_REGISTRY=${process.env.KONTRA_REGISTRY || '127.0.0.1:5000'}`,
            `KONTRA_CONTROLLER=${controller}`,
            `KONTRA_TRUST_REGISTRIES=${process.env.KONTRA_TRUST_REGISTRIES || '127.0.0.1:5000'}`,
            `KONTRA_TRUST_UNSIGNED=${process.env.KONTRA_TRUST_UNSIGNED || '127.0.0.1:5000'}`,
            `KONTRA_DOCKER_NETWORK=${network}`,
            `KONTRA_SKIP_INIT=1`,
            `KONTRA_WARDEN_ASSIGNMENT=${JSON.stringify(assigned)}`,
          ],
          networksAdvanced: [{ name: network }],
          volumes: [{ hostPath: sock, containerPath: '/var/run/docker.sock' }],
          // Pulumi owns the lifetime. unless-stopped would resurrect a Machine after
          // `docker stop`/`kill` during destroy and leave sibling Workers behind.
          restart: 'no',
          stopTimeout: 20,
        },
        { ignoreChanges: [] }
      );
      return { name, container };
    });

    const inventory = pulumi.all(machines.map((m) => m.container.name)).apply((names) =>
      names.reduce<Record<string, MachineEntry>>((acc, name) => {
        // A LOCAL CONTAINER HAS NO METER. `priceHourly: 0` is the same sentinel the cloud path
        // uses for "unknown", and both readings want the same thing from a caller: do not print
        // a dollar figure. A docker Fleet costs this machine's RAM, which is real but is not
        // something this program can price.
        acc[name] = { name, host: name, publicIp: name, tag: args.tag,
                      size: 'docker', priceHourly: 0 };
        return acc;
      }, {})
    );

    const first = placements[0];
    return {
      inventory,
      tag: args.tag,
      machines: args.machines,
      placements,
      bundleUrl: first?.bundleUrl ?? '',
      bundleSha: first?.bundleSha ?? '',
      actorName: first?.actorName ?? '',
      actorVersion: first?.actorVersion ?? '',
      actorEngine: first?.actorEngine ?? '',
      controller: first?.controller ?? args.controller ?? '',
      maxSessions: first?.maxSessions ?? 0,
    };
  };
}

export function assignmentFor(
  placements: PlacementArgs[],
  machineCount: number,
  machineIndex: number,
  /** The run's workspace namespace (`FleetArgs.namespace`); absent means the legacy one. */
  namespace?: string
): { generation: number; workers: Array<Record<string, unknown>> } {
  const workers = [];
  for (const p of placements) {
    if (machineIndex >= machinesFor(p, machineCount)) continue;
    const image = p.workerImage || '';
    if (!image) {
      throw new Error(
        `dockerFleet placement ${JSON.stringify(p.actorName)} has no workerImage — publish the ` +
          `actor with \`kontra deploy --actor\` so the Warden has a digest-pinned image to run`
      );
    }
    if (!/@sha256:[a-f0-9]{64}$/.test(image)) {
      throw new Error(
        `dockerFleet placement ${JSON.stringify(p.actorName)} workerImage must be digest-pinned ` +
          `(repo@sha256:<64 hex>), got a tag or an unpinned name`
      );
    }
    workers.push({
      name: p.actorName,
      version: p.actorVersion ?? '0.1.0',
      image,
      env: {
        KONTRA_ADDRESS: process.env.KONTRA_ADDRESS || 'temporal:7233',
        KONTRA_NAMESPACE: namespace || process.env.KONTRA_NAMESPACE || 'default',
        KONTRA_REDIS_HOST: process.env.KONTRA_REDIS_HOST || 'redis:6379',
        KONTRA_S3_ENDPOINT: process.env.KONTRA_S3_ENDPOINT || 'http://seaweed-s3:8333',
        KONTRA_ORCHESTRATOR_URL:
          process.env.KONTRA_ORCHESTRATOR_URL || 'http://orchestrator-api:8088',
        ...(p.maxSessions ? { KONTRA_MAX_PARALLEL_SESSIONS: String(p.maxSessions) } : {}),
        ...(p.controller ? { KONTRA_CONTROLLER: p.controller } : {}),
      },
    });
  }
  return { generation: 1, workers };
}
