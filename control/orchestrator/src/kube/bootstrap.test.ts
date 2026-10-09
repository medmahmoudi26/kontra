import { describe, expect, it } from 'vitest';

import { KubeClient, KubeError, type Transport } from './client';
import { ADDONS, bootstrapCluster, bootstrapTenant, helmChart } from './bootstrap';

const PEM = '-----BEGIN PUBLIC KEY-----\nMFkw\n-----END PUBLIC KEY-----';

/** A fake API server: records every request, and answers CRD reads from a set that can grow. */
function cluster(crds: Set<string>) {
  const seen: Array<{ method: string; path: string; body?: Record<string, unknown> }> = [];
  const transport: Transport = async (req) => {
    seen.push({ method: req.method, path: req.path, body: req.body ? JSON.parse(req.body) : undefined });
    if (req.method === 'GET' && req.path.includes('/customresourcedefinitions/')) {
      const name = decodeURIComponent(req.path.split('/').pop()!);
      return crds.has(name) ? { status: 200, body: '{}' } : { status: 404, body: '{}' };
    }
    return { status: 200, body: '{}' };
  };
  return { seen, client: new KubeClient(transport, 'https://fake') };
}

const applied = (seen: Array<{ method: string; body?: Record<string, unknown> }>) =>
  seen.filter((r) => r.method === 'PATCH').map((r) => `${r.body!.kind}/${(r.body!.metadata as { name: string }).name}`);

describe('helmChart', () => {
  it('pins each add-on by chart version, for k3s\'s own Helm controller', () => {
    const keda = helmChart('keda') as { apiVersion: string; spec: Record<string, unknown> };
    expect(keda.apiVersion).toBe('helm.cattle.io/v1');
    expect(keda.spec).toMatchObject({ chart: 'keda', version: ADDONS.keda.version, targetNamespace: 'keda' });
  });
});

describe('bootstrapCluster', () => {
  it('on k3s installs the add-ons, waits for their APIs, then enforces signatures', async () => {
    const crds = new Set<string>();
    const { seen, client } = cluster(crds);
    let slept = 0;
    const did = await bootstrapCluster(client, {
      flavor: 'k3s',
      registry: 'zot.kontra:5000',
      cosignPublicKey: PEM,
      waitMs: 60_000,
      // The add-ons' APIs appear after two polls, as the Helm controller finishes.
      sleep: async () => {
        slept += 1;
        if (slept === 2) {
          crds.add(ADDONS.keda.crd);
          crds.add(ADDONS.kyverno.crd);
        }
      },
    });
    expect(applied(seen)).toEqual([
      'RuntimeClass/gvisor',
      'HelmChart/kontra-keda',
      'HelmChart/kontra-kyverno',
      'ClusterPolicy/kontra-verify-images',
    ]);
    expect(did).toContain(`helmchart/keda@${ADDONS.keda.version}`);
  });

  it('on a bring-your-own cluster installs nothing and refuses when an add-on is missing', async () => {
    const { seen, client } = cluster(new Set([ADDONS.keda.crd]));
    const err = await bootstrapCluster(client, { flavor: 'byo', registry: 'r', cosignPublicKey: PEM }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(KubeError);
    expect(String(err)).toMatch(/missing kyverno/);
    expect(applied(seen)).toEqual(['RuntimeClass/gvisor']);
    // No signature policy applied to a cluster that cannot enforce it.
    expect(applied(seen)).not.toContain('ClusterPolicy/kontra-verify-images');
  });

  it('on a bring-your-own cluster with both add-ons, applies the policy', async () => {
    const { seen, client } = cluster(new Set([ADDONS.keda.crd, ADDONS.kyverno.crd]));
    await bootstrapCluster(client, { flavor: 'byo', registry: 'r', cosignPublicKey: PEM });
    expect(applied(seen)).toEqual(['RuntimeClass/gvisor', 'ClusterPolicy/kontra-verify-images']);
  });

  it('gives up on k3s add-ons that never appear, naming them', async () => {
    const { client } = cluster(new Set());
    const err = await bootstrapCluster(client, {
      flavor: 'k3s', registry: 'r', cosignPublicKey: PEM, waitMs: 10_000, sleep: async () => undefined,
    }).catch((e: unknown) => e);
    expect(String(err)).toMatch(/did not install keda .* and kyverno/);
  });
});

describe('bootstrapTenant', () => {
  it('applies the namespace, its service account, quota and both network policies', async () => {
    const { seen, client } = cluster(new Set());
    await bootstrapTenant(client, {
      namespace: 'ws-hello',
      quota: { cpu: '8', memory: '16Gi', pods: 20 },
      endpoints: [{ name: 'temporal', cidr: '10.0.0.10/32', ports: [7233] }],
    });
    expect(applied(seen)).toEqual([
      'Namespace/ws-hello',
      'ServiceAccount/kontra-worker',
      'ResourceQuota/kontra-tenant',
      'NetworkPolicy/kontra-deny-ingress',
      'NetworkPolicy/kontra-egress',
    ]);
  });
});
