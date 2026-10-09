/**
 * THE FLEET PROFILES — `fleets:` in the install's `kontra.yaml` (PRD §7, ADR 0066).
 *
 * A profile is the inputs of ONE provider's Pulumi program, named so a workflow can say
 * `fleet.hold(profile="do-fra")` and never see a region, a token or a droplet size:
 *
 *     fleets:
 *       default: local
 *       local:   { provider: local, nodes: 1, size: { cpus: 2, memory: 4Gi }, idle_minutes: 15 }
 *       do-fra:  { provider: digital_ocean, region: fra1, token: dop_v1_…, ssh_key_fingerprint: …,
 *                  vpc_cidr: 10.200.0.0/20, sizes: { s: s-2vcpu-4gb, m: …, l: … } }
 *       eks:     { provider: byo_kubeconfig, kubeconfig: | … }
 *
 * `local` IS THE DEFAULT AND NEEDS NOTHING: no file, or a file with no `fleets:`, is one `local`
 * profile with every default filled in, which is what keeps a laptop install free of config.
 *
 * READ AT CONVERGE TIME, INSIDE THE ACTIVITY, AND NOWHERE ELSE. A profile carries the cloud token
 * (the PRD puts secrets in this file, gitignored like `.env`), and ADR 0034's rule is kept: a
 * secret never enters workflow history, an activity's arguments or a Batch. Callers pass a profile
 * NAME; the infra activity resolves it here. {@link publicFleets} is the only shape that may leave
 * the process — an API response, a log line — and it carries the NAMES of the secrets that are set,
 * never their values.
 *
 * STRICT, because a typo is expensive here: `idle_minute: 15` silently ignored is a cloud fleet
 * running all night. The rules are pinned against the CLI's reader by
 * `shared/conformance/fleets.json`.
 */
import { readFileSync } from 'node:fs';

import { load as loadYaml } from 'js-yaml';

export type ProviderName = 'local' | 'digital_ocean' | 'byo_kubeconfig';

export interface LocalProfile {
  provider: 'local';
  nodes: number;
  size: { cpus: number; memory: string };
  idle_minutes: number;
}

export interface DigitalOceanProfile {
  provider: 'digital_ocean';
  region: string;
  /** SECRET. */
  token: string;
  ssh_key_fingerprint: string;
  vpc_cidr: string;
  /** Size letter (`s`, `m`, `l` …) to droplet slug. */
  sizes: Record<string, string>;
  nodes: number;
  idle_minutes: number;
}

export interface ByoKubeconfigProfile {
  provider: 'byo_kubeconfig';
  /** SECRET. */
  kubeconfig: string;
}

export type FleetProfile = LocalProfile | DigitalOceanProfile | ByoKubeconfigProfile;

export interface Fleets {
  default: string;
  profiles: Record<string, FleetProfile>;
}

/** The fields whose VALUES never leave the process (fleets.json `secret_fields`). */
export const SECRET_FIELDS = ['token', 'kubeconfig'] as const;

export class FleetConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'FleetConfigError';
  }
}

const NAME = /^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$/;
const MEMORY = /^[1-9][0-9]*(Mi|Gi)$/;
const DEFAULT_LOCAL: LocalProfile = {
  provider: 'local',
  nodes: 1,
  size: { cpus: 2, memory: '4Gi' },
  idle_minutes: 15,
};
const DEFAULT_VPC_CIDR = '10.200.0.0/20';

const KNOWN: Record<ProviderName, readonly string[]> = {
  local: ['provider', 'nodes', 'size', 'idle_minutes'],
  digital_ocean: ['provider', 'region', 'token', 'ssh_key_fingerprint', 'vpc_cidr', 'sizes', 'nodes', 'idle_minutes'],
  byo_kubeconfig: ['provider', 'kubeconfig'],
};

function isMap(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function positiveInt(where: string, key: string, v: unknown, fallback: number): number {
  if (v === undefined) return fallback;
  if (typeof v !== 'number' || !Number.isInteger(v) || v < 1) {
    throw new FleetConfigError(`${where}: ${key} must be a whole number of at least 1, got ${JSON.stringify(v)}`);
  }
  return v;
}

function text(where: string, key: string, v: unknown): string {
  if (typeof v !== 'string' || v.trim() === '') {
    throw new FleetConfigError(`${where}: ${key} is required and must be a non-empty string`);
  }
  return v;
}

function profileOf(name: string, raw: unknown): FleetProfile {
  const where = `fleets.${name}`;
  if (!NAME.test(name)) {
    throw new FleetConfigError(
      `${where}: a profile name must match ${NAME.source} — it becomes part of a stack name and a Kubernetes label`
    );
  }
  if (!isMap(raw)) throw new FleetConfigError(`${where}: a profile is a map of its provider's inputs`);
  const provider = raw.provider;
  if (provider !== 'local' && provider !== 'digital_ocean' && provider !== 'byo_kubeconfig') {
    throw new FleetConfigError(
      `${where}: provider ${JSON.stringify(provider)} has no program — one of local, digital_ocean, byo_kubeconfig`
    );
  }
  for (const key of Object.keys(raw)) {
    if (!KNOWN[provider].includes(key)) {
      throw new FleetConfigError(`${where}: ${key} is not a ${provider} setting (known: ${KNOWN[provider].join(', ')})`);
    }
  }
  if (provider === 'local') {
    const size = raw.size === undefined ? DEFAULT_LOCAL.size : raw.size;
    if (!isMap(size)) throw new FleetConfigError(`${where}: size is a map of cpus and memory`);
    for (const key of Object.keys(size)) {
      if (key !== 'cpus' && key !== 'memory') throw new FleetConfigError(`${where}: size.${key} is not a size setting`);
    }
    const memory = size.memory === undefined ? DEFAULT_LOCAL.size.memory : size.memory;
    if (typeof memory !== 'string' || !MEMORY.test(memory)) {
      throw new FleetConfigError(`${where}: size.memory must be a quantity like 4Gi or 512Mi, got ${JSON.stringify(memory)}`);
    }
    return {
      provider,
      nodes: positiveInt(where, 'nodes', raw.nodes, DEFAULT_LOCAL.nodes),
      size: { cpus: positiveInt(where, 'size.cpus', size.cpus, DEFAULT_LOCAL.size.cpus), memory },
      idle_minutes: positiveInt(where, 'idle_minutes', raw.idle_minutes, DEFAULT_LOCAL.idle_minutes),
    };
  }
  if (provider === 'digital_ocean') {
    const sizes = raw.sizes;
    if (!isMap(sizes) || Object.keys(sizes).length === 0) {
      throw new FleetConfigError(`${where}: sizes is required — a map of size letter to droplet slug`);
    }
    const slugs: Record<string, string> = {};
    for (const [letter, slug] of Object.entries(sizes)) slugs[letter] = text(where, `sizes.${letter}`, slug);
    return {
      provider,
      region: text(where, 'region', raw.region),
      token: text(where, 'token', raw.token),
      ssh_key_fingerprint: text(where, 'ssh_key_fingerprint', raw.ssh_key_fingerprint),
      vpc_cidr: raw.vpc_cidr === undefined ? DEFAULT_VPC_CIDR : text(where, 'vpc_cidr', raw.vpc_cidr),
      sizes: slugs,
      nodes: positiveInt(where, 'nodes', raw.nodes, 1),
      idle_minutes: positiveInt(where, 'idle_minutes', raw.idle_minutes, DEFAULT_LOCAL.idle_minutes),
    };
  }
  return { provider, kubeconfig: text(where, 'kubeconfig', raw.kubeconfig) };
}

/** The fleets a `kontra.yaml`'s text declares. Throws {@link FleetConfigError} naming what is wrong. */
export function parseFleets(yamlText: string): Fleets {
  let doc: unknown;
  try {
    doc = loadYaml(yamlText);
  } catch (err) {
    throw new FleetConfigError(`kontra.yaml is not valid YAML: ${(err as Error).message}`);
  }
  const block = isMap(doc) ? doc.fleets : undefined;
  if (block === undefined || block === null) return { default: 'local', profiles: { local: { ...DEFAULT_LOCAL } } };
  if (!isMap(block)) throw new FleetConfigError('fleets must be a map of profile names to profiles');
  const profiles: Record<string, FleetProfile> = {};
  for (const [name, raw] of Object.entries(block)) {
    if (name === 'default') continue;
    profiles[name] = profileOf(name, raw);
  }
  const chosen = block.default;
  if (chosen !== undefined) {
    if (typeof chosen !== 'string' || !(chosen in profiles)) {
      // `default: local` with no local profile written out still means the built-in local one.
      if (chosen === 'local' && !('local' in profiles)) {
        profiles.local = { ...DEFAULT_LOCAL };
        return { default: 'local', profiles };
      }
      throw new FleetConfigError(`fleets.default names ${JSON.stringify(chosen)}, which is not a profile here`);
    }
    return { default: chosen, profiles };
  }
  if ('local' in profiles) return { default: 'local', profiles };
  throw new FleetConfigError(
    'fleets.default is required when there is no local profile — a fleet nobody chose is not chosen silently'
  );
}

/** Where the install's kontra.yaml is: KONTRA_CONFIG, else the path compose mounts it at. */
export function fleetsPath(env: NodeJS.ProcessEnv = process.env): string {
  return env.KONTRA_CONFIG?.trim() || '/etc/kontra/kontra.yaml';
}

/** The install's fleets. A missing file is the built-in `local` profile, not an error. */
export function loadFleets(path = fleetsPath()): Fleets {
  let textOf: string;
  try {
    textOf = readFileSync(path, 'utf8');
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return parseFleets('');
    throw err;
  }
  return parseFleets(textOf);
}

/** The profile a caller names, or the default. Throws naming the profiles that do exist. */
export function resolveProfile(fleets: Fleets, name?: string): { name: string; profile: FleetProfile } {
  const chosen = name?.trim() || fleets.default;
  const profile = fleets.profiles[chosen];
  if (!profile) {
    throw new FleetConfigError(
      `no fleet profile ${JSON.stringify(chosen)} — this install has ${Object.keys(fleets.profiles).join(', ')}`
    );
  }
  return { name: chosen, profile };
}

/** What may be shown: every secret value replaced by the list of secret fields that are set. */
export function publicFleets(fleets: Fleets): {
  default: string;
  profiles: Record<string, Record<string, unknown> & { secrets: string[] }>;
} {
  const profiles: Record<string, Record<string, unknown> & { secrets: string[] }> = {};
  for (const [name, profile] of Object.entries(fleets.profiles)) {
    const shown: Record<string, unknown> = {};
    const secrets: string[] = [];
    for (const [key, value] of Object.entries(profile)) {
      if ((SECRET_FIELDS as readonly string[]).includes(key)) secrets.push(key);
      else shown[key] = value;
    }
    profiles[name] = { ...shown, secrets };
  }
  return { default: fleets.default, profiles };
}
