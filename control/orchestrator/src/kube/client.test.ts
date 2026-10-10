import { describe, expect, it } from 'vitest';

import { KubeClient, KubeError, objectPath, parseKubeconfig, type Transport } from './client';
import { placementDeployment, tenantNamespace } from './manifests';

const kubeconfig = (user: string) => `apiVersion: v1
kind: Config
current-context: k3s
clusters:
  - name: k3s
    cluster:
      server: https://10.0.0.5:6443
      certificate-authority-data: ${Buffer.from('CA').toString('base64')}
contexts:
  - name: k3s
    context: { cluster: k3s, user: admin }
users:
  - name: admin
    user:
${user}
`;

describe('parseKubeconfig', () => {
  it('reads what k3s writes: an inline CA and client certificate', () => {
    const c = parseKubeconfig(
      kubeconfig(`      client-certificate-data: ${Buffer.from('CERT').toString('base64')}\n      client-key-data: ${Buffer.from('KEY').toString('base64')}`)
    );
    expect(c.server).toBe('https://10.0.0.5:6443');
    expect(c.ca?.toString()).toBe('CA');
    expect(c.cert?.toString()).toBe('CERT');
    expect(c.key?.toString()).toBe('KEY');
  });

  it('reads a bearer token', () => {
    expect(parseKubeconfig(kubeconfig('      token: abc')).token).toBe('abc');
  });

  it('refuses an exec plugin rather than running a binary a file names', () => {
    expect(() => parseKubeconfig(kubeconfig('      exec: { command: aws, args: [eks, get-token] }'))).toThrow(/exec/);
  });

  it('refuses a user with no credential, and a plain-http server', () => {
    expect(() => parseKubeconfig(kubeconfig('      username: x'))).toThrow(/neither a token/);
    expect(() => parseKubeconfig(kubeconfig('      token: t').replace('https://', 'http://'))).toThrow(/https/);
  });
});

describe('objectPath', () => {
  it('core, grouped, and cluster-scoped kinds', () => {
    expect(objectPath('v1', 'ServiceAccount', 'kontra-worker', 'ws-hello')).toBe('/api/v1/namespaces/ws-hello/serviceaccounts/kontra-worker');
    expect(objectPath('apps/v1', 'Deployment', 'd', 'ws-hello')).toBe('/apis/apps/v1/namespaces/ws-hello/deployments/d');
    expect(objectPath('v1', 'Namespace', 'ws-hello')).toBe('/api/v1/namespaces/ws-hello');
    expect(objectPath('kyverno.io/v1', 'ClusterPolicy', 'p')).toBe('/apis/kyverno.io/v1/clusterpolicies/p');
  });

  it('refuses a kind it does not know and a namespaced kind with no namespace', () => {
    expect(() => objectPath('v1', 'Pod', 'p', 'ns')).toThrow(/not one kontra applies/);
    expect(() => objectPath('apps/v1', 'Deployment', 'd')).toThrow(/needs a namespace/);
  });
});

describe('KubeClient', () => {
  function fake(responses: Array<{ status: number; body: string }>) {
    const seen: Array<Parameters<Transport>[0]> = [];
    const transport: Transport = async (req) => {
      seen.push(req);
      return responses.shift() ?? { status: 200, body: '{}' };
    };
    return { seen, client: new KubeClient(transport, 'https://k3s') };
  }

  it('applies by server-side apply, as kontra, forcing ownership of the fields it sets', async () => {
    const { seen, client } = fake([{ status: 201, body: '{"kind":"Namespace"}' }]);
    await client.apply(tenantNamespace('ws-hello'));
    expect(seen[0]).toMatchObject({
      method: 'PATCH',
      path: '/api/v1/namespaces/ws-hello?fieldManager=kontra&force=true',
      headers: { 'content-type': 'application/apply-patch+yaml' },
    });
    expect(JSON.parse(seen[0]!.body!).metadata.name).toBe('ws-hello');
  });

  it('applies a Deployment at its namespaced path', async () => {
    const { seen, client } = fake([]);
    const d = placementDeployment({
      namespace: 'ws-hello',
      scope: 's',
      actor: 'canary',
      version: '1.1.0',
      image: `r/canary@sha256:${'b'.repeat(64)}`,
      env: {},
      resources: { cpus: 1, memory: '1Gi' },
    }) as { metadata: { name: string } };
    await client.apply(d);
    expect(seen[0]!.path).toBe(`/apis/apps/v1/namespaces/ws-hello/deployments/${d.metadata.name}?fieldManager=kontra&force=true`);
  });

  it('reads a missing object as undefined and a missing delete as already done', async () => {
    const { client } = fake([
      { status: 404, body: '{}' },
      { status: 404, body: '{}' },
    ]);
    expect(await client.get('apps/v1', 'Deployment', 'gone', 'ws-hello')).toBeUndefined();
    expect(await client.delete('apps/v1', 'Deployment', 'gone', 'ws-hello')).toBe(false);
  });

  it('deletes with background propagation, so the pods go with the Deployment', async () => {
    const { seen, client } = fake([{ status: 200, body: '{}' }]);
    expect(await client.delete('apps/v1', 'Deployment', 'd', 'ws-hello')).toBe(true);
    expect(seen[0]!.path).toContain('propagationPolicy=Background');
  });

  it('names the status and the server message in an error, and nothing from the request', async () => {
    const { client } = fake([{ status: 403, body: '{"message":"forbidden: kontra cannot patch"}' }]);
    const err = await client.apply(tenantNamespace('ws-hello')).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(KubeError);
    expect((err as KubeError).status).toBe(403);
    expect(String(err)).toMatch(/403: forbidden: kontra cannot patch/);
  });
});
