/**
 * THE FLEET POOL'S ACTIVITIES (ADR 0066 decision 7). They run in the infra container, on the
 * placement queue, because that is the one process that may read `kontra.yaml` (the profile, its
 * cloud token) and the kubeconfig a converge produces. Nothing secret is an argument or a return
 * value: the pool workflow passes a profile NAME, and gets back counts and object names.
 *
 * THE KUBECONFIG IS KEPT HERE, 0600, under `KONTRA_FLEET_STATE/pools/<profile>/kubeconfig` — plan
 * default A23, "the infra role holds it". It is written by a converge, read by every later activity,
 * and deleted by the destroy.
 *
 * WHERE A WORKER DIALS. A pod in the execution cluster reaches the control plane at addresses that
 * are not the ones compose uses internally, so they are configuration: {@link controlPlaneFromEnv}.
 * The same addresses become the tenant's egress allow-list, resolved to /32s at bootstrap.
 */
import { lookup } from 'node:dns/promises';
import { chmodSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import path from 'node:path';

import { Client, Connection } from '@temporalio/client';

import { kubeNamespaceFor } from '../fleetPool';
import { loadFleets, resolveProfile } from '../infra/fleets';
import { providerFor, type ProviderRun } from '../infra/providers/registry';
import { bootstrapCluster, bootstrapTenant } from '../kube/bootstrap';
import { KubeClient } from '../kube/client';
import { placementDeployment, placementName, placementScaledObject, type Endpoint } from '../kube/manifests';
import { temporalConnectOptions } from '../temporalTls';
import { sharedQueue } from '../nexusRegistry';
import { resolveWorkerImage } from './fleet';

/** Where workers reach the control plane from inside the execution cluster. */
export interface ControlPlane {
  /** Temporal frontend, `host:port`. */
  temporal: string;
  /** The object store, a URL. */
  s3: string;
  /** The orchestrator, a URL. */
  orchestrator: string;
  /** The registry host images are named by (`host:port`), and the bucket/prefix workers write under. */
  registry: string;
  bucket: string;
  /** The install's cosign public key, PEM (ADR 0066 decision 5). */
  cosignPublicKey: string;
}

/**
 * The control plane as the execution cluster sees it, from the infra role's environment. Each value
 * has to be set for a placement to work: a worker given compose's internal name `temporal:7233`
 * from inside a VM would dial nothing and wait at `ready()`, so a missing one refuses here with its
 * variable's name.
 */
export function controlPlaneFromEnv(env: NodeJS.ProcessEnv = process.env): ControlPlane {
  const need = (key: string): string => {
    const v = env[key]?.trim();
    if (!v) throw new Error(`${key} is not set: the execution cluster needs the control plane's address as IT sees it`);
    return v;
  };
  const keyFile = env.KONTRA_COSIGN_PUBLIC_KEY_FILE?.trim();
  return {
    temporal: need('KONTRA_FLEET_TEMPORAL'),
    s3: need('KONTRA_FLEET_S3'),
    orchestrator: need('KONTRA_FLEET_ORCHESTRATOR'),
    registry: need('KONTRA_FLEET_REGISTRY'),
    bucket: env.KONTRA_S3_BUCKET?.trim() || 'kontra',
    cosignPublicKey: keyFile && existsSync(keyFile) ? readFileSync(keyFile, 'utf8') : need('KONTRA_COSIGN_PUBLIC_KEY'),
  };
}

function stateDir(env: NodeJS.ProcessEnv = process.env): string {
  return env.KONTRA_FLEET_STATE?.trim() || '/var/lib/kontra/fleets';
}

function kubeconfigPath(profile: string): string {
  return path.join(stateDir(), 'pools', profile, 'kubeconfig');
}

function kubeFor(profile: string): KubeClient {
  const file = kubeconfigPath(profile);
  if (!existsSync(file)) throw new Error(`fleet ${profile} has no cluster yet (no kubeconfig at its pool) — hold it first`);
  return KubeClient.fromKubeconfig(readFileSync(file, 'utf8'));
}

/** `host:port` or a URL, to the egress rule that lets a worker reach it: a /32 per address. */
async function endpointOf(name: string, address: string): Promise<Endpoint[]> {
  const url = /^[a-z]+:\/\//.test(address) ? new URL(address) : new URL(`tcp://${address}`);
  const port = Number(url.port || (url.protocol === 'https:' ? 443 : 80));
  const ips = await lookup(url.hostname, { all: true, family: 4 });
  return ips.map((ip) => ({ name, cidr: `${ip.address}/32`, ports: [port] }));
}

/** How often a provider's activity says it is alive. Well inside the pool's 5-minute heartbeatTimeout. */
export const PROVIDER_KEEPALIVE_MS = 30_000;

/**
 * RUN A PROVIDER UNDER THIS ACTIVITY: its progress becomes the heartbeat detail, a retry is said to
 * be one, and the activity's cancellation reaches it.
 *
 * THE HEARTBEAT IS A TIMER AS WELL AS AN EVENT, for the reason `activities/infra.ts` measured: an
 * engine reports a resource when it STARTS and then nothing until it ends, and a droplet's install
 * command legitimately runs for minutes (cloud-init, apt, k3s). Against the pool's 5-minute
 * heartbeatTimeout that silence would kill a converge that was making progress and retry it into
 * the same wall — and a retry is what clears the stack's lock, so the timer is also what keeps
 * "the previous attempt timed out" meaning "the previous attempt is dead". The local provider has
 * no events to report, and gets the same keepalive.
 *
 * Outside an activity (a test, the e2e job) there is no context: the provider runs with no heartbeat.
 */
export async function withProviderRun<T>(fn: (run: ProviderRun) => Promise<T>): Promise<T> {
  const { Context } = await import('@temporalio/activity');
  let ctx: InstanceType<typeof Context> | undefined;
  try {
    ctx = Context.current();
  } catch {
    ctx = undefined;
  }
  let last: Record<string, unknown> = { phase: 'provider' };
  const run: ProviderRun = {
    retrying: (ctx?.info.attempt ?? 1) > 1,
    signal: ctx?.cancellationSignal,
    progress: (detail) => {
      last = detail;
      ctx?.heartbeat(detail);
    },
  };
  const timer = ctx ? setInterval(() => ctx!.heartbeat(last), PROVIDER_KEEPALIVE_MS) : undefined;
  try {
    return await fn(run);
  } finally {
    // A converge that threw must not leave a timer heartbeating for an activity that has failed.
    if (timer) clearInterval(timer);
  }
}

export interface ConvergePoolOutput {
  nodes: number;
  idleMinutes: number;
}

/**
 * Make or adopt the profile's cluster and bootstrap it. Idempotent.
 *
 * THE KEEPALIVE COVERS THE BOOTSTRAP TOO: on a k3s fleet it waits up to five minutes for the add-on
 * charts to be served, which is the pool's whole heartbeatTimeout on its own.
 */
export async function convergePool(input: { profile: string }): Promise<ConvergePoolOutput> {
  const { name, profile } = resolveProfile(loadFleets(), input.profile);
  return withProviderRun(async (run) => {
    const cluster = await providerFor(profile).converge(name, profile, run);
    const file = kubeconfigPath(name);
    mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
    writeFileSync(file, cluster.kubeconfig, { mode: 0o600 });
    chmodSync(file, 0o600);
    const cp = controlPlaneFromEnv();
    run.progress?.({ phase: 'bootstrap' });
    await bootstrapCluster(KubeClient.fromKubeconfig(cluster.kubeconfig), {
      flavor: cluster.flavor,
      registry: cp.registry,
      cosignPublicKey: cp.cosignPublicKey,
    });
    return {
      nodes: cluster.nodes.length,
      idleMinutes: 'idle_minutes' in profile ? profile.idle_minutes : 15,
    };
  });
}

/** The tenant's namespace, quota and egress allow-list on the profile's cluster. Idempotent. */
export async function bootstrapPoolTenant(input: { profile: string; namespace: string }): Promise<void> {
  const cp = controlPlaneFromEnv();
  const endpoints = (
    await Promise.all([
      endpointOf('temporal', cp.temporal),
      endpointOf('s3', cp.s3),
      endpointOf('orchestrator', cp.orchestrator),
      endpointOf('registry', cp.registry),
    ])
  ).flat();
  await bootstrapTenant(kubeFor(input.profile), {
    namespace: kubeNamespaceFor(input.namespace),
    quota: { cpu: process.env.KONTRA_TENANT_CPU || '8', memory: process.env.KONTRA_TENANT_MEMORY || '16Gi', pods: 40 },
    endpoints,
  });
}

export interface ApplyPlacementInput {
  profile: string;
  /** The tenant's TEMPORAL namespace: workers serve it, and their pods run in its Kubernetes twin. */
  namespace: string;
  actor: string;
  version: string;
  replicas: number;
}

/** The Deployment and its ScaledObject for one placement. Idempotent (server-side apply). */
export async function applyPlacement(input: ApplyPlacementInput): Promise<{ deployment: string; image: string }> {
  const cp = controlPlaneFromEnv();
  const image = await resolveWorkerImage(cp.registry, input.actor, input.version);
  if (!image) {
    throw new Error(`${input.actor}@${input.version} is not in ${cp.registry} by digest — deploy it before placing it`);
  }
  const kube = kubeFor(input.profile);
  const namespace = kubeNamespaceFor(input.namespace);
  const deployment = placementDeployment({
    namespace,
    scope: input.profile,
    actor: input.actor,
    version: input.version,
    image,
    env: {
      KONTRA_ADDRESS: cp.temporal,
      // The worker serves its TENANT's namespace (ADR 0051): a caller in ws-hello dispatches there.
      KONTRA_NAMESPACE: input.namespace,
      KONTRA_S3_ENDPOINT: cp.s3,
      KONTRA_S3_BUCKET: cp.bucket,
      KONTRA_ORCHESTRATOR_URL: cp.orchestrator,
    },
    // actor.json's `resources` reaches here once the catalog carries it (WP-17); until then one CPU.
    resources: { cpus: 1, memory: '1Gi' },
  }) as { metadata: { name: string } };
  await kube.apply(deployment);
  await kube.apply(
    placementScaledObject({
      namespace,
      deployment: deployment.metadata.name,
      temporalAddress: cp.temporal,
      temporalNamespace: input.namespace,
      // The queue RunBatch is scheduled on (ADR 0067; dispatch.json §queue): the backlog that is the demand.
      taskQueue: `${sharedQueue(input.actor, input.version)}-sessions`,
      maxReplicas: input.replicas,
    })
  );
  return { deployment: deployment.metadata.name, image };
}

/** Remove one placement: its ScaledObject first, so KEDA does not resurrect what is being deleted. */
export async function deletePlacement(input: Omit<ApplyPlacementInput, 'replicas'>): Promise<void> {
  const kube = kubeFor(input.profile);
  const namespace = kubeNamespaceFor(input.namespace);
  const name = placementName(input.profile, input.actor, input.version);
  await kube.delete('keda.sh/v1alpha1', 'ScaledObject', name, namespace);
  await kube.delete('apps/v1', 'Deployment', name, namespace);
}

/** Release the profile's nodes and forget its kubeconfig. */
export async function destroyPool(input: { profile: string }): Promise<void> {
  const { name, profile } = resolveProfile(loadFleets(), input.profile);
  await withProviderRun((run) => providerFor(profile).destroy(name, profile, run));
  rmSync(path.dirname(kubeconfigPath(name)), { recursive: true, force: true });
}

/** Which holders still run, each asked in its own namespace. Unknown counts as alive: a pool must
 *  not tear a fleet out from under a run because Temporal was briefly unreachable. */
export async function poolHoldersAlive(input: {
  holders: Array<{ workflowId: string; namespace: string }>;
}): Promise<{ alive: Array<{ workflowId: string; namespace: string }> }> {
  const connection = await Connection.connect(temporalConnectOptions({ address: process.env.KONTRA_ADDRESS ?? 'localhost:7233' }));
  try {
    const alive: Array<{ workflowId: string; namespace: string }> = [];
    for (const h of input.holders) {
      if (!h.workflowId) continue;
      try {
        const d = await new Client({ connection, namespace: h.namespace }).workflow.getHandle(h.workflowId).describe();
        if (d.status.name === 'RUNNING') alive.push(h);
      } catch (err) {
        if ((err as { name?: string }).name !== 'WorkflowNotFoundError') alive.push(h);
      }
    }
    return { alive };
  } finally {
    await connection.close().catch(() => undefined);
  }
}

// ── THE CALLER'S SIDE: what `fleet.hold(profile=…)` schedules ──────────────────────────────────
//
// These run on the placement queue too, because resolving "the default profile" needs kontra.yaml,
// and they reach the pool workflow — in the control plane's own namespace — with a client of their
// own. A hold or a place WAITS for the pool's answer (an update), so a run that holds a fleet knows
// the cluster exists, and one that places knows the Deployment does.

/** The control plane's own namespace, where every pool workflow lives. */
function controlNamespace(): string {
  return process.env.KONTRA_NAMESPACE?.trim() || 'default';
}

async function withControlClient<T>(fn: (client: Client) => Promise<T>): Promise<T> {
  const connection = await Connection.connect(temporalConnectOptions({ address: process.env.KONTRA_ADDRESS ?? 'localhost:7233' }));
  try {
    return await fn(new Client({ connection, namespace: controlNamespace() }));
  } finally {
    await connection.close().catch(() => undefined);
  }
}

/** Beat while waiting on the pool, so a converge that takes minutes is not mistaken for a hang. */
async function beating<T>(p: Promise<T>): Promise<T> {
  const { Context } = await import('@temporalio/activity');
  let ctx: { heartbeat: (d?: unknown) => void } | undefined;
  try {
    ctx = Context.current();
  } catch {
    ctx = undefined;
  }
  const timer = ctx ? setInterval(() => ctx!.heartbeat(), 20_000) : undefined;
  try {
    return await p;
  } finally {
    if (timer) clearInterval(timer);
  }
}

export interface HoldFleetPoolInput {
  /** A profile name, or empty for kontra.yaml's default. */
  profile?: string;
  lease: string;
  holder: string;
  holderNamespace: string;
  ttlMs?: number;
}

export async function holdFleetPool(input: HoldFleetPoolInput): Promise<{ profile: string; holds: number; nodes: number }> {
  const { name } = resolveProfile(loadFleets(), input.profile);
  const { POOL_WORKFLOW, POOL_TOUCH_SIGNAL, POOL_HOLD_UPDATE, placementQueue, poolWorkflowId } = await import('../fleetPool');
  return withControlClient(async (client) => {
    const handle = await client.workflow.signalWithStart(POOL_WORKFLOW, {
      taskQueue: placementQueue(),
      workflowId: poolWorkflowId(name),
      args: [{ profile: name }],
      signal: POOL_TOUCH_SIGNAL,
      signalArgs: [],
    });
    const out = await beating(
      handle.executeUpdate<{ holds: number; nodes: number }, [unknown]>(POOL_HOLD_UPDATE, {
        args: [{ lease: input.lease, holder: input.holder, holderNamespace: input.holderNamespace, ttlMs: input.ttlMs }],
      })
    );
    return { profile: name, holds: out.holds, nodes: out.nodes };
  });
}

export interface PlaceOnFleetPoolInput {
  profile: string;
  lease: string;
  namespace: string;
  actor: string;
  version: string;
  replicas: number;
}

export async function placeOnFleetPool(
  input: PlaceOnFleetPoolInput
): Promise<{ deployment: string; image: string; replicas: number }> {
  const { POOL_PLACE_UPDATE, poolWorkflowId } = await import('../fleetPool');
  return withControlClient((client) =>
    beating(
      client.workflow.getHandle(poolWorkflowId(input.profile)).executeUpdate<
        { deployment: string; image: string; replicas: number },
        [unknown]
      >(POOL_PLACE_UPDATE, {
        args: [{ lease: input.lease, namespace: input.namespace, actor: input.actor, version: input.version, replicas: input.replicas }],
      })
    )
  );
}

/** Drop a hold. A pool that is already gone is the end state, not an error. */
export async function dropFleetPool(input: { profile: string; lease: string }): Promise<{ delivered: boolean }> {
  const { POOL_DROP_SIGNAL, poolWorkflowId } = await import('../fleetPool');
  return withControlClient(async (client) => {
    try {
      await client.workflow.getHandle(poolWorkflowId(input.profile)).signal(POOL_DROP_SIGNAL, { lease: input.lease });
      return { delivered: true };
    } catch (err) {
      if ((err as { name?: string }).name === 'WorkflowNotFoundError') return { delivered: false };
      throw err;
    }
  });
}
