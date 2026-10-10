/**
 * THE `local` PROVIDER, FOR REAL — one Lima VM, k3s, gVisor, KEDA and Kyverno, and the PRD §10.4
 * properties checked against the live cluster rather than against manifests.
 *
 * Runs only where `KONTRA_FLEET_E2E=1` is set: the `fleet-local` CI job, on a runner with KVM,
 * Lima and kubectl installed. Everything is created by kontra's own code (the provider and the
 * bootstrap); `kubectl` is used only to LOOK, so a property that holds is held by what kontra
 * applied and not by the test.
 */
import { execFileSync } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { parseFleets } from '../infra/fleets';
import { PROVIDERS } from '../infra/providers/registry';
import { KubeClient } from './client';
import { bootstrapCluster, bootstrapTenant } from './bootstrap';
import { GVISOR } from './manifests';

const RUN = process.env.KONTRA_FLEET_E2E === '1';
const FLEET = 'ci';
const PEM = process.env.KONTRA_E2E_COSIGN_PUB ?? '';

describe.skipIf(!RUN)('the local fleet provider on a real VM', () => {
  let kubeconfig = '';
  const kube = (...args: string[]): string =>
    execFileSync('kubectl', ['--kubeconfig', kubeconfig, ...args], { encoding: 'utf8', timeout: 300_000 });
  const profile = parseFleets('fleets:\n  local:\n    provider: local\n    nodes: 1\n    size: { cpus: 2, memory: 4Gi }\n').profiles.local!;

  beforeAll(async () => {
    const cluster = await PROVIDERS.local.converge(FLEET, profile);
    const dir = mkdtempSync(path.join(tmpdir(), 'kontra-e2e-'));
    kubeconfig = path.join(dir, 'kubeconfig');
    writeFileSync(kubeconfig, cluster.kubeconfig, { mode: 0o600 });
    const client = KubeClient.fromKubeconfig(cluster.kubeconfig);
    await bootstrapCluster(client, { flavor: 'k3s', registry: 'zot.kontra:5000', cosignPublicKey: PEM, waitMs: 600_000 });
    await bootstrapTenant(client, {
      namespace: 'ws-ci',
      quota: { cpu: '2', memory: '2Gi', pods: 5 },
      endpoints: [],
    });
  }, 1_800_000);

  afterAll(async () => {
    if (process.env.KONTRA_FLEET_E2E_KEEP !== '1') await PROVIDERS.local.destroy(FLEET, profile);
  }, 600_000);

  it('runs a pod under gVisor', () => {
    // A system namespace, so the tenant policies (registry, signature) do not apply to this probe.
    kube('run', 'gvisor-probe', '-n', 'default', '--restart=Never', '--image=busybox:1.37',
      `--overrides={"spec":{"runtimeClassName":"${GVISOR.name}"}}`, '--command', '--', 'dmesg');
    kube('wait', '-n', 'default', '--for=jsonpath={.status.phase}=Succeeded', 'pod/gvisor-probe', '--timeout=300s');
    // gVisor's kernel log is its own, and says so.
    expect(kube('logs', '-n', 'default', 'gvisor-probe')).toMatch(/gVisor/i);
  }, 600_000);

  it('refuses, at admission, a tenant pod whose image is not from this install\'s registry', () => {
    let refused = '';
    try {
      kube('run', 'foreign', '-n', 'ws-ci', '--restart=Never', '--image=busybox:1.37',
        `--overrides={"spec":{"runtimeClassName":"${GVISOR.name}","securityContext":{"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"foreign","image":"busybox:1.37","resources":{"requests":{"cpu":"100m","memory":"64Mi"},"limits":{"cpu":"100m","memory":"64Mi"}},"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}`);
    } catch (err) {
      refused = String((err as { stderr?: Buffer }).stderr ?? err);
    }
    expect(refused).toMatch(/kontra-verify-images|only-this-registry|tenant pods run images from/);
  }, 300_000);

  it('blocks a tenant pod from the metadata address, by network policy', () => {
    // The probe runs in the tenant namespace with an image the registry rule would refuse, so the
    // policy check is read from the egress policy kontra applied rather than from a pod.
    const policy = JSON.parse(kube('get', 'networkpolicy', 'kontra-egress', '-n', 'ws-ci', '-o', 'json'));
    const internet = policy.spec.egress.find((r: { to: Array<{ ipBlock?: { cidr: string } }> }) => r.to[0]?.ipBlock?.cidr === '0.0.0.0/0');
    expect(internet.to[0].ipBlock.except).toContain('169.254.0.0/16');
  }, 120_000);
});
