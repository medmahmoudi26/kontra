/**
 * WHAT A FLEET IS, AS KUBERNETES OBJECTS (ADR 0066) — pure functions from kontra's facts to manifests.
 *
 * Every isolation property the PRD asks of an execution cluster is a field in one of these objects,
 * and nothing else in kontra enforces it: gVisor is `runtimeClassName`, "a pod cannot reach another
 * tenant or the control plane's Postgres" is a NetworkPolicy, "cannot run an unsigned image" is a
 * Kyverno policy, the only credential is a projected token. So they are built here, in one place,
 * as data, and the tests assert the fields — a property that is a field nobody tests is a property
 * a refactor removes silently. Applying them is `client.ts`'s job; deciding WHEN is the fleet
 * activities'.
 *
 * NO kontra SECRET APPEARS IN ANY OBJECT BUILT HERE. A worker's environment carries addresses and
 * names; its one credential is the token Kubernetes mounts, which the control plane verifies.
 */
import { createHash } from 'node:crypto';

/** Labels every object kontra makes carries, so a sweep can find exactly kontra's own. */
export const MANAGED_BY = { 'app.kubernetes.io/managed-by': 'kontra' } as const;

/** The RuntimeClass name a placement asks for, and the containerd handler it maps to. */
export const GVISOR = { name: 'gvisor', handler: 'runsc' } as const;

/** The audience of the projected token: the control plane refuses a token minted for anything else. */
export const TOKEN_AUDIENCE = 'kontra';

/** Where the projected token is mounted in a worker. `KONTRA_TOKEN_FILE` names it. */
export const TOKEN_DIR = '/var/run/secrets/kontra';

/** The service account workers run as, one per tenant namespace, with no RBAC bindings at all. */
export const WORKER_SERVICE_ACCOUNT = 'kontra-worker';

/** The uid the kontra-runtimes images run as (`USER heroku`, uid 1000 — kontra-runtimes' Dockerfiles).
 *  Numeric because the kubelet cannot verify `runAsNonRoot` against a user NAME. */
export const RUNTIME_UID = 1000;

type Obj = Record<string, unknown>;

const DNS_LABEL = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

function sha(s: string): string {
  return createHash('sha256').update(s).digest('hex');
}

/** A DNS-1123 label from free text: lowercase, `-` for anything else, trimmed, at most `max`. */
export function dnsLabel(raw: string, max = 63): string {
  const s = raw.toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/-{2,}/g, '-').replace(/^-+|-+$/g, '');
  return s.slice(0, max).replace(/-+$/g, '') || 'x';
}

/**
 * The Deployment name for one placement: readable (`enrich-0-3-0-…`) and UNIQUE per (scope, actor,
 * version) by an 8-hex suffix, always a valid DNS label of at most 63 characters. Two scopes
 * placing the same actor get two Deployments (ADR 0066 decision 3); the same scope placing it twice
 * gets the same name, so a re-apply is an update rather than a second fleet of workers.
 */
export function placementName(scope: string, actor: string, version: string): string {
  const suffix = sha(`${scope}\u0000${actor}\u0000${version}`).slice(0, 8);
  const stem = dnsLabel(`${actor}-${version}`, 63 - 1 - suffix.length);
  return `${stem}-${suffix}`;
}

/** A tenant's namespace: named like its Temporal namespace (ADR 0051), Pod Security `restricted`. */
export function tenantNamespace(name: string): Obj {
  if (!DNS_LABEL.test(name) || name.length > 63) throw new Error(`namespace ${JSON.stringify(name)} is not a DNS label`);
  return {
    apiVersion: 'v1',
    kind: 'Namespace',
    metadata: {
      name,
      labels: {
        ...MANAGED_BY,
        'kontra.dev/tenant': name,
        // RESTRICTED, ENFORCED: the admission controller refuses a privileged pod in a tenant's
        // namespace whatever manifest asks for one — this file's Deployment included.
        'pod-security.kubernetes.io/enforce': 'restricted',
        'pod-security.kubernetes.io/enforce-version': 'latest',
      },
    },
  };
}

/** The service account workers run as. It has no RoleBinding: its token is an identity, not a key. */
export function workerServiceAccount(namespace: string): Obj {
  return {
    apiVersion: 'v1',
    kind: 'ServiceAccount',
    metadata: { name: WORKER_SERVICE_ACCOUNT, namespace, labels: { ...MANAGED_BY } },
    automountServiceAccountToken: false,
  };
}

export interface Quota {
  cpu: string;
  memory: string;
  pods: number;
}

/** What one tenant may use of a shared cluster at once. */
export function resourceQuota(namespace: string, quota: Quota): Obj {
  return {
    apiVersion: 'v1',
    kind: 'ResourceQuota',
    metadata: { name: 'kontra-tenant', namespace, labels: { ...MANAGED_BY } },
    spec: {
      hard: {
        'requests.cpu': quota.cpu,
        'limits.cpu': quota.cpu,
        'requests.memory': quota.memory,
        'limits.memory': quota.memory,
        pods: String(quota.pods),
      },
    },
  };
}

/** The cluster-wide RuntimeClass every placement names. The node bootstrap registers the handler. */
export function gvisorRuntimeClass(): Obj {
  return {
    apiVersion: 'node.k8s.io/v1',
    kind: 'RuntimeClass',
    metadata: { name: GVISOR.name, labels: { ...MANAGED_BY } },
    handler: GVISOR.handler,
  };
}

/** One control-plane endpoint a worker must reach: an address block and the TCP ports on it. */
export interface Endpoint {
  /** What it is, for the reader of the policy: `temporal`, `s3`, `orchestrator`, `registry`, `logs`. */
  name: string;
  cidr: string;
  ports: number[];
}

/**
 * Address blocks a worker may never reach on the open internet arm: private ranges, CGNAT,
 * link-local — the last is where every cloud's metadata service answers, with the node's
 * credentials. A control-plane endpoint inside one of them is reachable ONLY through its own rule.
 */
export const PRIVATE_BLOCKS = ['10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16', '100.64.0.0/10', '169.254.0.0/16'] as const;

/**
 * The tenant's network policy (ADR 0066 decision 4, PRD §10.4).
 *
 * TWO POLICIES. Ingress: none, from anywhere — a worker dials out and nothing dials in, so no pod
 * in this or any other namespace can reach it. Egress: DNS to the cluster's resolver, each control-
 * plane endpoint on its own ports, and the public internet minus every private block. The control
 * plane's Postgres is never in `endpoints`, and it sits in a private block, so it is unreachable by
 * construction rather than by a deny rule (NetworkPolicy has no deny; what is not allowed is not).
 */
export function tenantNetworkPolicies(namespace: string, endpoints: Endpoint[]): Obj[] {
  for (const e of endpoints) {
    if (!/^\d{1,3}(\.\d{1,3}){3}\/\d{1,2}$/.test(e.cidr)) throw new Error(`endpoint ${e.name}: ${e.cidr} is not an IPv4 CIDR`);
  }
  const all = { podSelector: {} };
  return [
    {
      apiVersion: 'networking.k8s.io/v1',
      kind: 'NetworkPolicy',
      metadata: { name: 'kontra-deny-ingress', namespace, labels: { ...MANAGED_BY } },
      spec: { ...all, policyTypes: ['Ingress'], ingress: [] },
    },
    {
      apiVersion: 'networking.k8s.io/v1',
      kind: 'NetworkPolicy',
      metadata: { name: 'kontra-egress', namespace, labels: { ...MANAGED_BY } },
      spec: {
        ...all,
        policyTypes: ['Egress'],
        egress: [
          {
            // The cluster resolver, wherever it runs. Without it every name lookup fails and the
            // worker reports Temporal unreachable for a reason that is not Temporal.
            to: [{ namespaceSelector: { matchLabels: { 'kubernetes.io/metadata.name': 'kube-system' } } }],
            ports: [
              { protocol: 'UDP', port: 53 },
              { protocol: 'TCP', port: 53 },
            ],
          },
          ...endpoints.map((e) => ({
            to: [{ ipBlock: { cidr: e.cidr } }],
            ports: e.ports.map((port) => ({ protocol: 'TCP', port })),
          })),
          { to: [{ ipBlock: { cidr: '0.0.0.0/0', except: [...PRIVATE_BLOCKS] } }] },
        ],
      },
    },
  ];
}

export interface PlacementSpec {
  namespace: string;
  /** The holding scope — a run's Lease holder id — the placement belongs to. */
  scope: string;
  actor: string;
  version: string;
  /** `<registry>/<repo>@sha256:<64 hex>`. A tag is refused: a placement runs exactly one image. */
  image: string;
  /** Plain environment: addresses and names only (see the header). */
  env: Record<string, string>;
  resources: { cpus: number; memory: string };
  /** `worker.yaml`'s graceful_shutdown_timeout, in seconds (ADR 0065); the pod gets 10 s more. */
  gracefulSeconds?: number;
  /** Image pull secret in the namespace, when the registry needs one. */
  pullSecret?: string;
}

const DIGEST_REF = /^[^@\s]+@sha256:[0-9a-f]{64}$/;

/** The Deployment that runs one placement (ADR 0066 decision 3). */
export function placementDeployment(p: PlacementSpec): Obj {
  if (!DIGEST_REF.test(p.image)) {
    throw new Error(`placement ${p.actor}@${p.version}: image ${JSON.stringify(p.image)} is not pinned by digest`);
  }
  for (const key of Object.keys(p.env)) {
    // TWO TESTS, NOT ONE ALTERNATION: a credential word anywhere in the name, or a name that ENDS in
    // KEY (S3_ACCESS_KEY, not KEYSPACE). One regex with `$` on its last branch read as an anchor
    // forgotten on the others (CodeQL js/regex/missing-regexp-anchor), which is the opposite of
    // what it meant.
    if (/TOKEN|SECRET|PASSWORD|PASSPHRASE/i.test(key) || /KEY$/i.test(key)) {
      throw new Error(`placement ${p.actor}@${p.version}: env ${key} looks like a credential; a worker's only credential is its projected token`);
    }
  }
  const name = placementName(p.scope, p.actor, p.version);
  const labels = {
    ...MANAGED_BY,
    'kontra.dev/placement': name,
    'kontra.dev/actor': dnsLabel(p.actor),
    'kontra.dev/version': dnsLabel(p.version),
  };
  const grace = p.gracefulSeconds ?? 30;
  return {
    apiVersion: 'apps/v1',
    kind: 'Deployment',
    metadata: {
      name,
      namespace: p.namespace,
      labels,
      // The scope is free text (a Lease holder id) and may not be a valid label value.
      annotations: { 'kontra.dev/scope': p.scope, 'kontra.dev/actor-ref': `${p.actor}@${p.version}` },
    },
    spec: {
      // KEDA owns the count from here on (decision 6); this is the first replica, not a ceiling.
      replicas: 1,
      revisionHistoryLimit: 1,
      selector: { matchLabels: { 'kontra.dev/placement': name } },
      template: {
        metadata: { labels },
        spec: {
          runtimeClassName: GVISOR.name,
          serviceAccountName: WORKER_SERVICE_ACCOUNT,
          automountServiceAccountToken: false,
          enableServiceLinks: false,
          terminationGracePeriodSeconds: grace + 10,
          securityContext: {
            runAsNonRoot: true,
            runAsUser: RUNTIME_UID,
            runAsGroup: RUNTIME_UID,
            fsGroup: RUNTIME_UID,
            seccompProfile: { type: 'RuntimeDefault' },
          },
          ...(p.pullSecret ? { imagePullSecrets: [{ name: p.pullSecret }] } : {}),
          containers: [
            {
              name: 'actor',
              image: p.image,
              imagePullPolicy: 'IfNotPresent',
              env: [
                ...Object.entries(p.env)
                  .sort(([a], [b]) => a.localeCompare(b))
                  .map(([n, value]) => ({ name: n, value })),
                { name: 'KONTRA_TOKEN_FILE', value: `${TOKEN_DIR}/token` },
                { name: 'KONTRA_ACTOR_DIGEST', value: p.image.slice(p.image.indexOf('@') + 1) },
              ],
              resources: {
                requests: { cpu: String(p.resources.cpus), memory: p.resources.memory },
                limits: { cpu: String(p.resources.cpus), memory: p.resources.memory },
              },
              securityContext: {
                allowPrivilegeEscalation: false,
                capabilities: { drop: ['ALL'] },
                runAsNonRoot: true,
              },
              volumeMounts: [
                { name: 'kontra-token', mountPath: TOKEN_DIR, readOnly: true },
                { name: 'tmp', mountPath: '/tmp' },
              ],
            },
          ],
          volumes: [
            {
              name: 'kontra-token',
              projected: {
                sources: [{ serviceAccountToken: { audience: TOKEN_AUDIENCE, expirationSeconds: 3600, path: 'token' } }],
              },
            },
            { name: 'tmp', emptyDir: {} },
          ],
        },
      },
    },
  };
}

export interface ScalingSpec {
  namespace: string;
  /** The Deployment it scales. */
  deployment: string;
  /** The Temporal endpoint and namespace KEDA reads the backlog from. */
  temporalAddress: string;
  temporalNamespace: string;
  /** The actor's shared queue (`<name>-<version>`), whose backlog is the demand. */
  taskQueue: string;
  /** `place(replicas=N)`: the most this placement may run (decision 6). */
  maxReplicas: number;
  /** Backlog per replica before KEDA adds one. */
  targetQueueSize?: number;
}

/** The KEDA ScaledObject that sizes a placement on its queue's backlog (decision 6). */
export function placementScaledObject(s: ScalingSpec): Obj {
  if (!Number.isInteger(s.maxReplicas) || s.maxReplicas < 1) throw new Error(`replicas must be at least 1, got ${s.maxReplicas}`);
  return {
    apiVersion: 'keda.sh/v1alpha1',
    kind: 'ScaledObject',
    metadata: { name: s.deployment, namespace: s.namespace, labels: { ...MANAGED_BY } },
    spec: {
      scaleTargetRef: { name: s.deployment },
      // ONE WHILE PLACED, NEVER ZERO: `ready()` waits for a poller, and a Session pinned to a worker
      // must not lose it to a scale-to-zero between two of its batches.
      minReplicaCount: 1,
      maxReplicaCount: s.maxReplicas,
      cooldownPeriod: 300,
      triggers: [
        {
          type: 'temporal',
          metadata: {
            endpoint: s.temporalAddress,
            namespace: s.temporalNamespace,
            taskQueue: s.taskQueue,
            queueTypes: 'activity',
            targetQueueSize: String(s.targetQueueSize ?? 4),
            activationTargetQueueSize: '0',
          },
        },
      ],
    },
  };
}

/**
 * The Kyverno policy that refuses an unsigned image from the install's registry (decision 5,
 * PRD §10.4). Enforced, not audited, and scoped to tenant namespaces by label so the cluster's own
 * system pods are not asked for kontra's signature.
 */
export function imageSignaturePolicy(registry: string, publicKeyPem: string): Obj {
  if (!/BEGIN PUBLIC KEY/.test(publicKeyPem)) throw new Error('the cosign public key must be a PEM public key');
  return {
    apiVersion: 'kyverno.io/v1',
    kind: 'ClusterPolicy',
    metadata: { name: 'kontra-verify-images', labels: { ...MANAGED_BY } },
    spec: {
      validationFailureAction: 'Enforce',
      webhookTimeoutSeconds: 30,
      background: false,
      rules: [
        {
          name: 'signed-by-this-install',
          match: {
            any: [
              {
                resources: {
                  kinds: ['Pod'],
                  namespaceSelector: { matchExpressions: [{ key: 'kontra.dev/tenant', operator: 'Exists' }] },
                },
              },
            ],
          },
          verifyImages: [
            {
              imageReferences: [`${registry}/*`],
              mutateDigest: false,
              verifyDigest: true,
              required: true,
              attestors: [{ entries: [{ keys: { publicKeys: publicKeyPem } }] }],
            },
          ],
        },
        {
          // A tenant pod may only run images from this install's registry: an image from anywhere
          // else would skip the signature check above by not matching it.
          name: 'only-this-registry',
          match: {
            any: [
              {
                resources: {
                  kinds: ['Pod'],
                  namespaceSelector: { matchExpressions: [{ key: 'kontra.dev/tenant', operator: 'Exists' }] },
                },
              },
            ],
          },
          validate: {
            message: `tenant pods run images from ${registry} only`,
            pattern: { spec: { containers: [{ image: `${registry}/*` }] } },
          },
        },
      ],
    },
  };
}
