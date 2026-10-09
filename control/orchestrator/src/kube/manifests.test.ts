/**
 * Every isolation property PRD §10.4 asks of an execution cluster is a FIELD in one of these
 * manifests and nothing else enforces it — so these tests assert the fields, one property each.
 */
import { describe, expect, it } from 'vitest';

import {
  GVISOR,
  PRIVATE_BLOCKS,
  TOKEN_AUDIENCE,
  dnsLabel,
  gvisorRuntimeClass,
  imageSignaturePolicy,
  placementDeployment,
  placementName,
  placementScaledObject,
  resourceQuota,
  tenantNamespace,
  tenantNetworkPolicies,
  workerServiceAccount,
  type PlacementSpec,
} from './manifests';

const DIGEST = `sha256:${'a'.repeat(64)}`;
const spec = (over: Partial<PlacementSpec> = {}): PlacementSpec => ({
  namespace: 'ws-hello',
  scope: 'canary-1791999999#n1',
  actor: 'canary',
  version: '1.1.0',
  image: `zot.kontra:5000/canary@${DIGEST}`,
  env: { KONTRA_ADDRESS: 'temporal.kontra:7233', KONTRA_NAMESPACE: 'ws-hello' },
  resources: { cpus: 1, memory: '1Gi' },
  ...over,
});

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type Any = any;
const podOf = (d: Any) => d.spec.template.spec;

describe('names', () => {
  it('a placement name is a DNS label of at most 63, stable per (scope, actor, version)', () => {
    const a = placementName('run-1#n1', 'canary', '1.1.0');
    expect(a).toMatch(/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/);
    expect(a.length).toBeLessThanOrEqual(63);
    expect(placementName('run-1#n1', 'canary', '1.1.0')).toBe(a);
    expect(placementName('run-2#n1', 'canary', '1.1.0')).not.toBe(a);
    const long = placementName('s', 'a-very-long-actor-name-that-goes-on-and-on-and-on-forever', '10.20.30-rc.1');
    expect(long.length).toBeLessThanOrEqual(63);
  });

  it('sanitises free text into a label', () => {
    expect(dnsLabel('My Actor_v2.0')).toBe('my-actor-v2-0');
    expect(dnsLabel('...')).toBe('x');
  });
});

describe('the tenant namespace', () => {
  it('enforces Pod Security restricted and names its tenant', () => {
    const ns: Any = tenantNamespace('ws-hello');
    expect(ns.metadata.labels['pod-security.kubernetes.io/enforce']).toBe('restricted');
    expect(ns.metadata.labels['kontra.dev/tenant']).toBe('ws-hello');
  });

  it('refuses a name that is not a DNS label', () => {
    expect(() => tenantNamespace('WS_hello')).toThrow(/DNS label/);
  });

  it('gives workers a service account with no automounted token', () => {
    expect((workerServiceAccount('ws-hello') as Any).automountServiceAccountToken).toBe(false);
  });

  it('bounds the tenant with a quota', () => {
    const q: Any = resourceQuota('ws-hello', { cpu: '8', memory: '16Gi', pods: 20 });
    expect(q.spec.hard).toMatchObject({ 'limits.cpu': '8', 'limits.memory': '16Gi', pods: '20' });
  });
});

describe('the placement Deployment (§10.4)', () => {
  it('runs under gVisor', () => {
    expect(podOf(placementDeployment(spec())).runtimeClassName).toBe(GVISOR.name);
    expect((gvisorRuntimeClass() as Any).handler).toBe('runsc');
  });

  it('runs the image pinned by digest, and refuses a tag', () => {
    expect(podOf(placementDeployment(spec())).containers[0].image).toContain('@sha256:');
    expect(() => placementDeployment(spec({ image: 'zot.kontra:5000/canary:1.1.0' }))).toThrow(/not pinned by digest/);
  });

  it('is non-root, with no privilege escalation and every capability dropped', () => {
    const pod = podOf(placementDeployment(spec()));
    expect(pod.securityContext.runAsNonRoot).toBe(true);
    expect(pod.securityContext.runAsUser).toBeGreaterThan(0);
    expect(pod.securityContext.seccompProfile.type).toBe('RuntimeDefault');
    const c = pod.containers[0].securityContext;
    expect(c).toMatchObject({ allowPrivilegeEscalation: false, capabilities: { drop: ['ALL'] } });
  });

  it('carries exactly one credential: a projected token with the kontra audience', () => {
    const pod = podOf(placementDeployment(spec()));
    expect(pod.automountServiceAccountToken).toBe(false);
    const token = pod.volumes.find((v: Any) => v.projected);
    expect(token.projected.sources[0].serviceAccountToken.audience).toBe(TOKEN_AUDIENCE);
    const env = pod.containers[0].env.map((e: Any) => e.name);
    expect(env).toContain('KONTRA_TOKEN_FILE');
    expect(pod.containers[0].envFrom).toBeUndefined();
  });

  it('refuses an environment variable that looks like a credential', () => {
    for (const key of ['KONTRA_ACTOR_TOKEN', 'AWS_SECRET_ACCESS_KEY', 'DB_PASSWORD', 'S3_ACCESS_KEY']) {
      expect(() => placementDeployment(spec({ env: { [key]: 'x' } }))).toThrow(/credential/);
    }
  });

  it('drains for worker.yaml\'s graceful_shutdown_timeout, plus a margin', () => {
    expect(podOf(placementDeployment(spec({ gracefulSeconds: 45 }))).terminationGracePeriodSeconds).toBe(55);
  });

  it('sizes the pod from actor.json resources, requests equal to limits', () => {
    const r = podOf(placementDeployment(spec({ resources: { cpus: 2, memory: '4Gi' } }))).containers[0].resources;
    expect(r).toEqual({ requests: { cpu: '2', memory: '4Gi' }, limits: { cpu: '2', memory: '4Gi' } });
  });
});

describe('the tenant network policy (§10.4)', () => {
  const policies = tenantNetworkPolicies('ws-hello', [
    { name: 'temporal', cidr: '10.200.0.10/32', ports: [7233] },
    { name: 's3', cidr: '10.200.0.11/32', ports: [8333] },
  ]) as Any[];
  const egress = policies.find((p) => p.spec.policyTypes.includes('Egress')).spec.egress;

  it('denies all ingress: nothing dials a worker, including another tenant\'s pods', () => {
    const ingress = policies.find((p) => p.spec.policyTypes.includes('Ingress'));
    expect(ingress.spec.podSelector).toEqual({});
    expect(ingress.spec.ingress).toEqual([]);
  });

  it('allows each control-plane endpoint on its own ports only', () => {
    expect(egress).toContainEqual({ to: [{ ipBlock: { cidr: '10.200.0.10/32' } }], ports: [{ protocol: 'TCP', port: 7233 }] });
  });

  it('allows the internet minus every private, CGNAT and link-local block — the metadata service included', () => {
    const internet = egress.find((r: Any) => r.to[0].ipBlock?.cidr === '0.0.0.0/0');
    expect(internet.to[0].ipBlock.except).toEqual([...PRIVATE_BLOCKS]);
    expect(PRIVATE_BLOCKS).toContain('169.254.0.0/16');
  });

  it('never opens the control plane\'s Postgres: no rule names port 5432', () => {
    expect(JSON.stringify(policies)).not.toContain('5432');
  });

  it('refuses an endpoint that is not an IPv4 CIDR', () => {
    expect(() => tenantNetworkPolicies('ws-hello', [{ name: 'x', cidr: 'temporal', ports: [1] }])).toThrow(/CIDR/);
  });
});

describe('scaling', () => {
  it('scales on the actor queue\'s backlog, between one and replicas', () => {
    const so: Any = placementScaledObject({
      namespace: 'ws-hello',
      deployment: 'canary-1-1-0-abcd1234',
      temporalAddress: 'temporal.kontra:7233',
      temporalNamespace: 'ws-hello',
      taskQueue: 'canary-1.1.0-sessions',
      maxReplicas: 8,
    });
    expect(so.spec).toMatchObject({ minReplicaCount: 1, maxReplicaCount: 8 });
    expect(so.spec.triggers[0]).toMatchObject({ type: 'temporal', metadata: { taskQueue: 'canary-1.1.0-sessions', namespace: 'ws-hello' } });
    expect(() => placementScaledObject({ ...so.spec, namespace: 'x', deployment: 'y', temporalAddress: 'a', temporalNamespace: 'b', taskQueue: 'c', maxReplicas: 0 })).toThrow();
  });
});

describe('image signatures (§10.4)', () => {
  const pem = '-----BEGIN PUBLIC KEY-----\nMFkw\n-----END PUBLIC KEY-----';
  it('enforces a signature on every image from the install\'s registry, in tenant namespaces', () => {
    const p: Any = imageSignaturePolicy('zot.kontra:5000', pem);
    expect(p.spec.validationFailureAction).toBe('Enforce');
    const verify = p.spec.rules[0].verifyImages[0];
    expect(verify).toMatchObject({ imageReferences: ['zot.kontra:5000/*'], required: true, verifyDigest: true });
  });

  it('refuses images from anywhere else, which would otherwise skip the check', () => {
    const p: Any = imageSignaturePolicy('zot.kontra:5000', pem);
    expect(p.spec.rules[1].validate.pattern.spec.containers[0].image).toBe('zot.kontra:5000/*');
  });

  it('wants a PEM public key', () => {
    expect(() => imageSignaturePolicy('r', 'nope')).toThrow(/PEM/);
  });
});
