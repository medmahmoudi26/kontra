/**
 * THE FLEET POOL'S NAMES AND SHAPES (ADR 0066 decision 7) — shared by its workflow, its activities
 * and the SDK's callers, so no two of them spell a signal differently.
 *
 * One pool workflow per fleet PROFILE, in the control plane's own namespace, id
 * `kontra-pool/<profile>`. A profile is a cluster; several tenants may hold it at once, each in its
 * own Kubernetes namespace, and the pool counts all of their holds and placements. It is the single
 * writer of everything on the cluster: it converges the nodes on the first hold, applies and deletes
 * placements as their holders come and go, and destroys the nodes when the last hold has been gone
 * for the profile's `idle_minutes`.
 */
// NO IMPORTS: the pool WORKFLOW imports this file, and a workflow bundle may not reach node built-ins.

/** A DNS-1123 label from free text: lowercase, `-` for anything else, trimmed, at most `max`. */
export function dnsLabel(raw: string, max = 63): string {
  const s = raw.toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/-{2,}/g, '-').replace(/^-+|-+$/g, '');
  return s.slice(0, max).replace(/-+$/g, '') || 'x';
}

export const POOL_WORKFLOW = 'kontraFleetPoolWorkflow';

/** The infra container's queue for the pool and its activities — several at once, unlike `kontra-infra`. */
export const PLACEMENT_QUEUE = 'kontra-placement';
export function placementQueue(env: NodeJS.ProcessEnv = process.env): string {
  return env.KONTRA_PLACEMENT_QUEUE?.trim() || PLACEMENT_QUEUE;
}

export function poolWorkflowId(profile: string): string {
  return `kontra-pool/${profile}`;
}

/** Updates: the caller waits for the cluster (hold) or the Deployment (place) to exist. */
export const POOL_HOLD_UPDATE = 'kontra.pool.hold';
export const POOL_PLACE_UPDATE = 'kontra.pool.place';
/** A signal: a drop is fire-and-forget, and the pool does the cleanup in its own time. */
export const POOL_DROP_SIGNAL = 'kontra.pool.drop';
/** A no-op signal, for signal-with-start: it makes the workflow exist before an update reaches it. */
export const POOL_TOUCH_SIGNAL = 'kontra.pool.touch';
export const POOL_QUERY = 'kontra.pool.state';

/** How long a hold lives without being renewed; the pool then asks whether its holder still runs. */
export const POOL_LEASE_TTL_MS = 60 * 60_000;

export interface PoolHold {
  /** `<holder>#<nonce>`, made by the caller. */
  lease: string;
  /** The holding run's workflow id. */
  holder: string;
  /** The holding run's Temporal namespace — its tenant (ADR 0051). */
  holderNamespace: string;
  ttlMs?: number;
}

export interface PoolPlace {
  lease: string;
  /** The tenant's Temporal namespace; the placement lands in its Kubernetes namespace. */
  namespace: string;
  actor: string;
  version: string;
  /** The most workers this holder wants; the Deployment scales to the largest any holder asks. */
  replicas: number;
}

export interface PoolDrop {
  lease: string;
}

export interface PlacementView {
  namespace: string;
  actor: string;
  version: string;
  replicas: number;
  deployment: string;
  image: string;
  holders: string[];
}

export interface PoolState {
  profile: string;
  converged: boolean;
  destroyed: boolean;
  nodes: number;
  leases: Array<{ lease: string; holder: string; namespace: string; expiresAt: number }>;
  placements: PlacementView[];
}

export function placementKey(namespace: string, actor: string, version: string): string {
  return `${namespace}/${actor}@${version}`;
}

/**
 * The Kubernetes namespace a Temporal namespace's work runs in.
 *
 * A workspace's namespace (`ws-<name>`) is already a DNS label and maps to itself. The legacy
 * namespace (`default`, or whatever KONTRA_NAMESPACE says) must NOT map to Kubernetes' own `default`,
 * which carries no kontra policy and holds the cluster's own objects; it becomes `kontra-<name>`.
 */
export function kubeNamespaceFor(temporalNamespace: string): string {
  return temporalNamespace.startsWith('ws-') ? dnsLabel(temporalNamespace) : dnsLabel(`kontra-${temporalNamespace}`);
}
