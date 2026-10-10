/**
 * A KUBERNETES CLIENT SMALL ENOUGH TO READ — server-side apply, get, delete, from a kubeconfig.
 *
 * WHY NOT @kubernetes/client-node. kontra needs four verbs on a handful of kinds, and the official
 * client is a large dependency tree in the one process that holds a cloud token and a kubeconfig.
 * Server-side apply makes the verbs few: one PATCH with `application/apply-patch+yaml` creates or
 * updates any object idempotently, with kontra as the field manager, so there is no
 * create-or-replace logic to get wrong.
 *
 * WHAT A KUBECONFIG MAY SAY HERE: a server with an inline CA, and a user with an inline client
 * certificate and key (what k3s writes) or a bearer token. An `exec` plugin — `aws eks get-token`
 * and friends — is REFUSED with a message, not half-supported: running a binary named in a
 * customer's file is a decision for the byo_kubeconfig provider, not something a client does
 * silently.
 *
 * The kubeconfig is a SECRET (ADR 0066 decision 2). It is held in memory by the instance and never
 * logged; an error carries the server URL and the HTTP status, nothing from the credentials.
 */
import { request as httpsRequest } from 'node:https';

import { load as loadYaml } from 'js-yaml';

type Obj = Record<string, unknown>;

/** One HTTP exchange with the API server. Injectable, so the client is testable without a cluster. */
export type Transport = (req: {
  method: 'GET' | 'PATCH' | 'DELETE';
  path: string;
  headers: Record<string, string>;
  body?: string;
}) => Promise<{ status: number; body: string }>;

export interface Credentials {
  server: string;
  ca?: Buffer;
  cert?: Buffer;
  key?: Buffer;
  token?: string;
}

export class KubeError extends Error {
  constructor(
    message: string,
    readonly status?: number
  ) {
    super(message);
    this.name = 'KubeError';
  }
}

function b64(v: unknown): Buffer | undefined {
  return typeof v === 'string' && v !== '' ? Buffer.from(v, 'base64') : undefined;
}

/** The current context's server and credentials from a kubeconfig's text. */
export function parseKubeconfig(text: string): Credentials {
  const doc = loadYaml(text) as Obj | undefined;
  if (!doc || typeof doc !== 'object') throw new KubeError('kubeconfig is empty or not YAML');
  const named = (list: unknown, name: unknown, what: string): Obj => {
    const found = (Array.isArray(list) ? list : []).find((e: Obj) => e?.name === name) as Obj | undefined;
    if (!found) throw new KubeError(`kubeconfig: no ${what} named ${JSON.stringify(name)}`);
    return (found[what] as Obj) ?? {};
  };
  const contexts = doc.contexts as unknown;
  const current = doc['current-context'] ?? (Array.isArray(contexts) && contexts.length === 1 ? (contexts[0] as Obj).name : undefined);
  const ctx = named(contexts, current, 'context');
  const cluster = named(doc.clusters, ctx.cluster, 'cluster');
  const user = named(doc.users, ctx.user, 'user');
  if (user.exec) {
    throw new KubeError('kubeconfig: an exec credential plugin is not supported — give a token or a client certificate');
  }
  if (typeof cluster.server !== 'string' || !/^https:\/\//.test(cluster.server)) {
    throw new KubeError('kubeconfig: the cluster server must be an https:// URL');
  }
  const creds: Credentials = {
    server: cluster.server.replace(/\/+$/, ''),
    ca: b64(cluster['certificate-authority-data']),
    cert: b64(user['client-certificate-data']),
    key: b64(user['client-key-data']),
    token: typeof user.token === 'string' ? user.token : undefined,
  };
  if (!creds.token && !(creds.cert && creds.key)) {
    throw new KubeError('kubeconfig: the user has neither a token nor an inline client certificate and key');
  }
  return creds;
}

/** The URL path segment for each kind kontra applies. A kind not here is refused, not guessed. */
const PLURAL: Record<string, string> = {
  Namespace: 'namespaces',
  ServiceAccount: 'serviceaccounts',
  ResourceQuota: 'resourcequotas',
  Secret: 'secrets',
  RuntimeClass: 'runtimeclasses',
  NetworkPolicy: 'networkpolicies',
  Deployment: 'deployments',
  ScaledObject: 'scaledobjects',
  ClusterPolicy: 'clusterpolicies',
  // k3s's own Helm controller (helm.cattle.io/v1): how a k3s fleet gets KEDA and Kyverno.
  HelmChart: 'helmcharts',
  // Read only, to see whether an add-on's API exists yet.
  CustomResourceDefinition: 'customresourcedefinitions',
};
const CLUSTER_SCOPED = new Set(['Namespace', 'RuntimeClass', 'ClusterPolicy', 'CustomResourceDefinition']);

/** The REST path for one object: `/api/v1/…` for the core group, `/apis/<group>/<version>/…` otherwise. */
export function objectPath(apiVersion: string, kind: string, name: string, namespace?: string): string {
  const plural = PLURAL[kind];
  if (!plural) throw new KubeError(`kind ${kind} is not one kontra applies`);
  const base = apiVersion.includes('/') ? `/apis/${apiVersion}` : `/api/${apiVersion}`;
  const scope = CLUSTER_SCOPED.has(kind) ? '' : `/namespaces/${encodeURIComponent(namespace ?? '')}`;
  if (!CLUSTER_SCOPED.has(kind) && !namespace) throw new KubeError(`${kind} ${name} needs a namespace`);
  return `${base}${scope}/${plural}/${encodeURIComponent(name)}`;
}

function identity(obj: Obj): { apiVersion: string; kind: string; name: string; namespace?: string } {
  const meta = (obj.metadata ?? {}) as Obj;
  if (typeof obj.apiVersion !== 'string' || typeof obj.kind !== 'string' || typeof meta.name !== 'string') {
    throw new KubeError('an object needs apiVersion, kind and metadata.name');
  }
  return {
    apiVersion: obj.apiVersion,
    kind: obj.kind,
    name: meta.name,
    namespace: typeof meta.namespace === 'string' ? meta.namespace : undefined,
  };
}

/** HTTPS to the API server with the kubeconfig's credentials. */
export function httpsTransport(creds: Credentials, timeoutMs = 30_000): Transport {
  const url = new URL(creds.server);
  return (req) =>
    new Promise((resolve, reject) => {
      const r = httpsRequest(
        {
          host: url.hostname,
          port: url.port || 443,
          method: req.method,
          path: `${url.pathname.replace(/\/$/, '')}${req.path}`,
          headers: { ...req.headers, ...(creds.token ? { authorization: `Bearer ${creds.token}` } : {}) },
          ca: creds.ca,
          cert: creds.cert,
          key: creds.key,
          timeout: timeoutMs,
        },
        (res) => {
          const chunks: Buffer[] = [];
          res.on('data', (c: Buffer) => chunks.push(c));
          res.on('end', () => resolve({ status: res.statusCode ?? 0, body: Buffer.concat(chunks).toString('utf8') }));
        }
      );
      r.on('timeout', () => r.destroy(new KubeError(`${creds.server}: no answer within ${timeoutMs} ms`)));
      r.on('error', reject);
      if (req.body !== undefined) r.write(req.body);
      r.end();
    });
}

export class KubeClient {
  constructor(
    private readonly transport: Transport,
    private readonly server = 'the cluster',
    /** The field manager server-side apply records: kontra owns the fields it sets, nobody else's. */
    private readonly fieldManager = 'kontra'
  ) {}

  static fromKubeconfig(text: string): KubeClient {
    const creds = parseKubeconfig(text);
    return new KubeClient(httpsTransport(creds), creds.server);
  }

  private async send(req: Parameters<Transport>[0], allow404 = false): Promise<Obj | undefined> {
    const res = await this.transport(req);
    if (allow404 && res.status === 404) return undefined;
    if (res.status < 200 || res.status >= 300) {
      let reason = '';
      try {
        reason = String((JSON.parse(res.body) as Obj).message ?? '');
      } catch {
        // A non-JSON error body is not repeated: it could be anything a proxy wrote.
      }
      throw new KubeError(`${this.server}: ${req.method} ${req.path} -> ${res.status}${reason ? `: ${reason}` : ''}`, res.status);
    }
    return res.body ? (JSON.parse(res.body) as Obj) : {};
  }

  /** Create or update `obj` by server-side apply. Idempotent: the same object twice is one object. */
  async apply(obj: Obj): Promise<Obj> {
    const id = identity(obj);
    const path = `${objectPath(id.apiVersion, id.kind, id.name, id.namespace)}?fieldManager=${encodeURIComponent(this.fieldManager)}&force=true`;
    return (await this.send({
      method: 'PATCH',
      path,
      headers: { 'content-type': 'application/apply-patch+yaml', accept: 'application/json' },
      // JSON is YAML, and the apply endpoint takes either.
      body: JSON.stringify(obj),
    })) as Obj;
  }

  /** The live object, or undefined when there is none. */
  async get(apiVersion: string, kind: string, name: string, namespace?: string): Promise<Obj | undefined> {
    return this.send(
      { method: 'GET', path: objectPath(apiVersion, kind, name, namespace), headers: { accept: 'application/json' } },
      true
    );
  }

  /** Delete, with the dependents (a Deployment's ReplicaSets and pods) in the background. A missing
   *  object is already the end state, so it is not an error. */
  async delete(apiVersion: string, kind: string, name: string, namespace?: string): Promise<boolean> {
    const out = await this.send(
      {
        method: 'DELETE',
        path: `${objectPath(apiVersion, kind, name, namespace)}?propagationPolicy=Background`,
        headers: { accept: 'application/json' },
      },
      true
    );
    return out !== undefined;
  }
}
