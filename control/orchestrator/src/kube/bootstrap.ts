/**
 * WHAT EVERY FLEET HAS BEFORE A PLACEMENT LANDS ON IT (ADR 0066 decisions 3–6).
 *
 * Cluster-wide, once: the gVisor RuntimeClass, KEDA (scaling on queue depth), Kyverno (signature
 * checks at admission) and kontra's image-signature policy. Per tenant: its namespace, worker
 * service account, quota and network policy. All of it through `KubeClient.apply`, so a second
 * bootstrap of the same fleet is a no-op rather than a second install.
 *
 * HOW THE ADD-ONS ARRIVE DEPENDS ON WHO MADE THE CLUSTER. A fleet kontra made is k3s, and k3s runs a
 * Helm controller of its own: kontra applies one `HelmChart` object per add-on, pinned by chart
 * version, and k3s installs it — no Helm binary and no Kubernetes provider plugin in the control
 * plane. A cluster somebody else made (`byo_kubeconfig`) is not kontra's to install software into:
 * its add-ons must already be there, and the bootstrap CHECKS and refuses with what is missing
 * rather than half-configuring a cluster whose placements would then run without the isolation the
 * manifests ask for.
 */
import { KubeClient, KubeError } from './client';
import {
  gvisorRuntimeClass,
  imageSignaturePolicy,
  resourceQuota,
  tenantNamespace,
  tenantNetworkPolicies,
  workerServiceAccount,
  type Endpoint,
  type Quota,
} from './manifests';

type Obj = Record<string, unknown>;

/** The add-ons, pinned. A version moves by editing this table, with the chart's own release notes read. */
export const ADDONS = {
  keda: {
    repo: 'https://kedacore.github.io/charts',
    chart: 'keda',
    version: '2.21.0',
    namespace: 'keda',
    /** The API a placement's ScaledObject needs. */
    crd: 'scaledobjects.keda.sh',
  },
  kyverno: {
    repo: 'https://kyverno.github.io/kyverno',
    chart: 'kyverno',
    version: '3.9.1',
    namespace: 'kyverno',
    crd: 'clusterpolicies.kyverno.io',
  },
} as const;

/** A k3s `HelmChart` object: k3s's Helm controller installs the chart into `targetNamespace`. */
export function helmChart(name: keyof typeof ADDONS): Obj {
  const a = ADDONS[name];
  return {
    apiVersion: 'helm.cattle.io/v1',
    kind: 'HelmChart',
    metadata: { name: `kontra-${name}`, namespace: 'kube-system', labels: { 'app.kubernetes.io/managed-by': 'kontra' } },
    spec: {
      repo: a.repo,
      chart: a.chart,
      version: a.version,
      targetNamespace: a.namespace,
      createNamespace: true,
    },
  };
}

export type ClusterFlavor = 'k3s' | 'byo';

export interface ClusterBootstrap {
  flavor: ClusterFlavor;
  /** The install's registry host (`zot.example:5000`), whose images must carry its signature. */
  registry: string;
  /** The install's cosign public key (ADR 0066 decision 5). */
  cosignPublicKey: string;
  /** How long to wait for an add-on's API to appear after its chart is applied. */
  waitMs?: number;
  /** Injectable for tests. */
  sleep?: (ms: number) => Promise<void>;
}

async function crdExists(client: KubeClient, crd: string): Promise<boolean> {
  return (await client.get('apiextensions.k8s.io/v1', 'CustomResourceDefinition', crd)) !== undefined;
}

/**
 * Bring a cluster to the state every placement assumes. Idempotent.
 *
 * Returns what it did, for the converge's log: the add-ons installed (k3s) or found (byo).
 */
export async function bootstrapCluster(client: KubeClient, b: ClusterBootstrap): Promise<string[]> {
  const did: string[] = [];
  await client.apply(gvisorRuntimeClass());
  did.push('runtimeclass/gvisor');

  if (b.flavor === 'k3s') {
    for (const name of Object.keys(ADDONS) as Array<keyof typeof ADDONS>) {
      await client.apply(helmChart(name));
      did.push(`helmchart/${name}@${ADDONS[name].version}`);
    }
  }

  // The add-ons' APIs, which the policy below and every ScaledObject need. On k3s they appear once
  // the Helm controller has installed the charts; on byo they must be there already.
  const sleep = b.sleep ?? ((ms: number) => new Promise((r) => setTimeout(r, ms)));
  const deadline = b.waitMs ?? (b.flavor === 'k3s' ? 300_000 : 0);
  const missing: string[] = [];
  for (const name of Object.keys(ADDONS) as Array<keyof typeof ADDONS>) {
    const crd = ADDONS[name].crd;
    let waited = 0;
    while (!(await crdExists(client, crd))) {
      if (waited >= deadline) {
        missing.push(`${name} (${crd})`);
        break;
      }
      await sleep(5_000);
      waited += 5_000;
    }
  }
  if (missing.length > 0) {
    throw new KubeError(
      b.flavor === 'byo'
        ? `this cluster is missing ${missing.join(' and ')} — a bring-your-own cluster must have KEDA and Kyverno installed before kontra places anything on it (ADR 0066)`
        : `the k3s Helm controller did not install ${missing.join(' and ')} in time`
    );
  }

  await client.apply(imageSignaturePolicy(b.registry, b.cosignPublicKey));
  did.push('clusterpolicy/kontra-verify-images');
  return did;
}

export interface TenantBootstrap {
  /** The tenant's Kubernetes namespace — its Temporal namespace's name (ADR 0051). */
  namespace: string;
  quota: Quota;
  endpoints: Endpoint[];
}

/** One tenant's namespace and everything that bounds it. Idempotent. */
export async function bootstrapTenant(client: KubeClient, t: TenantBootstrap): Promise<void> {
  await client.apply(tenantNamespace(t.namespace));
  await client.apply(workerServiceAccount(t.namespace));
  await client.apply(resourceQuota(t.namespace, t.quota));
  for (const policy of tenantNetworkPolicies(t.namespace, t.endpoints)) await client.apply(policy);
}
