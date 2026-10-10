/**
 * FLEET PROVIDERS — one per `provider:` a profile may name (ADR 0066 decision 1).
 *
 * A provider makes (or adopts) a k3s cluster for a profile and hands back its kubeconfig and node
 * inventory; everything after that — the bootstrap, the placements — is the same code on every
 * provider, which is PRD goal 3: local and cloud differ only in which program makes the nodes.
 * "Adding a cloud is adding a provider" is literal here: an entry in {@link PROVIDERS}.
 *
 * Called ONLY inside the infra converge activity, with the profile resolved there from kontra.yaml:
 * a profile carries its cloud token, and the token never enters history, arguments or a Batch.
 */
import { execFile } from 'node:child_process';

import type { ByoKubeconfigProfile, FleetProfile } from '../fleets';
import { DigitalOceanProvider } from './digitalOcean';
import { LocalProvider, type Runner } from './local';
import type { ClusterFlavor } from '../../kube/bootstrap';

export interface FleetNode {
  name: string;
  address: string;
  /** The cloud's size slug, when the provider rents the node — what {@link priceHourly} is the price OF. */
  size?: string;
  /**
   * List price per hour in USD as the cloud reported it at converge, for the Fleet page (WP-60).
   * 0 means "unknown", never "free" — render it as unknown rather than multiply it.
   */
  priceHourly?: number;
}

export interface ProvisionedCluster {
  /** SECRET: never logged, never returned through an API. */
  kubeconfig: string;
  /** k3s when kontra made the cluster, byo when it adopted one — the bootstrap differs. */
  flavor: ClusterFlavor;
  nodes: FleetNode[];
}

/**
 * WHAT THE ACTIVITY RUNNING A PROVIDER LENDS IT, and nothing more: a way to say what it is doing,
 * whether a previous attempt died, and the activity's cancellation. A provider never sees a Temporal
 * context — it is called from tests, from the e2e job and from the pool alike — so the activity
 * translates its context into this, and a provider that has no use for it ignores it.
 */
export interface ProviderRun {
  /** What the provider is working on now, for the activity's heartbeat detail. Never a secret. */
  progress?: (detail: Record<string, unknown>) => void;
  /** A previous attempt of this activity died, so a lock it left on the provider's state is stale. */
  retrying?: boolean;
  /** The activity's cancellation: a provider that runs an engine stops it on abort. */
  signal?: AbortSignal;
}

export interface FleetProvider {
  /** Make or adopt the cluster for `profile`. Idempotent: a second call with the same inputs changes nothing. */
  converge(fleet: string, profile: FleetProfile, run?: ProviderRun): Promise<ProvisionedCluster>;
  /** Release the nodes. A provider that made none releases none. */
  destroy(fleet: string, profile: FleetProfile, run?: ProviderRun): Promise<void>;
}

/** `byo_kubeconfig`: the cluster exists; kontra adopts it and never destroys it. */
export const byoKubeconfig: FleetProvider = {
  async converge(_fleet, profile) {
    const p = profile as ByoKubeconfigProfile;
    return { kubeconfig: p.kubeconfig, flavor: 'byo', nodes: [] };
  },
  async destroy() {
    // Not kontra's cluster: releasing a lease on it releases nothing.
  },
};

/** A command, run with no shell: arguments are never interpolated into a command line. */
const execRunner: Runner = (cmd, args) =>
  new Promise((resolve) => {
    execFile(cmd, args, { maxBuffer: 16 * 1024 * 1024, timeout: 20 * 60_000 }, (err, stdout, stderr) => {
      const code = err ? (typeof (err as { code?: unknown }).code === 'number' ? ((err as { code: number }).code) : 1) : 0;
      resolve({ code, stdout: String(stdout), stderr: String(stderr) || (err ? err.message : '') });
    });
  });

/**
 * The `local` provider, configured from the infra role's environment: where fleet state lives, and
 * how a node reaches this install's registry (the name image references use, and the address a VM
 * fetches it from — `host.lima.internal` is the host as Lima's VMs see it).
 */
function localFromEnv(env: NodeJS.ProcessEnv = process.env): FleetProvider {
  const name = env.KONTRA_REGISTRY_NAME?.trim();
  const endpoint = env.KONTRA_REGISTRY_ENDPOINT?.trim();
  return new LocalProvider(
    execRunner,
    env.KONTRA_FLEET_STATE?.trim() || '/var/lib/kontra/fleets',
    name && endpoint ? { name, endpoint } : undefined
  );
}

export const PROVIDERS: Record<FleetProfile['provider'], FleetProvider> = {
  byo_kubeconfig: byoKubeconfig,
  local: localFromEnv(),
  // Reads its environment (the firewall's sources, the SSH key's path) at converge, not here: the
  // registry is built when the infra worker starts, and a missing variable must refuse a converge
  // with its name rather than stop a worker that may never be asked for a droplet.
  digital_ocean: new DigitalOceanProvider(),
};

/** The provider a profile names. */
export function providerFor(profile: FleetProfile): FleetProvider {
  return PROVIDERS[profile.provider];
}
