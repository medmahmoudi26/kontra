/**
 * The two reads `kontra.fleet` needs from inside a caller's workflow.
 *
 * Provisioning itself is NOT here — that is `stackWorkflow` on the infra queue, and the caller
 * reaches it as a child workflow so the fleet's lifetime is a durable part of the run. What
 * is left over are two questions a workflow cannot answer itself because both are I/O:
 *
 *   resolveBundle   which Artifact is `actor@version`, and does a Machine exec it or run python
 *   queuePollers    is anything actually POLLING this actor's queue yet
 *
 * WHY NOT THE INFRA QUEUE, which is where every other fleet-shaped thing lives. Two reasons, and
 * the first is disqualifying: `orchestrator-infra` runs at
 * `maxConcurrentActivityTaskExecutions: 1` on purpose (a Pulumi engine forks a CLI child plus one
 * per provider, ~164 MB per concurrent update, on a memory-capped container). `ready()` polls in a
 * loop, so putting it there means a second run's readiness check sits behind a sixty-minute
 * provision — a hang that looks exactly like a fleet that never came up. The second reason is
 * authority: that process is the only one holding DIGITALOCEAN_TOKEN and KONTRA_SSH_KEY, and
 * neither of these needs either. An anonymous GET and a DescribeTaskQueue do not belong beside the
 * cloud credential.
 *
 * So they live on DATASET_QUEUE, whose name says datasets but whose ROLE is already exactly this:
 * short reads a caller's loop blocks on, one at a time, kept off the long-decode queue for the
 * same reason. `publishBatch`, `closeDataset` and `pageDataset` are the same shape of thing.
 */

import { createHash } from 'node:crypto';
import { runLog } from './runLog';

import { describeQueue, sharedQueue, temporalQueueDescriber, type QueueDescriber } from '../pollers';

/**
 * The OCI registry port — `cli/appliance/registry.DefaultPort`, which is what `kontra up` serves
 * and what a Fleet's Machines pull from.
 *
 * Anonymous plain HTTP on the VPC by design, exactly as the object store this replaces was, and
 * for the same reason: nothing secret is ever put in a Bundle. The exposure control is the
 * BINDING, not a credential.
 */
export const REGISTRY_PORT = 5000;

/** The OCI repository an actor's Bundle lives in — `cli/bundle.go:bundleRepo`. Pinned across the
 * language boundary by `shared/conformance/bundleref.json`. */
export function bundleRepo(actor: string): string {
  return `bundles/${actor}`;
}

/**
 * The base URL of a registry given as `host:port` or with a scheme — `cli/deploy.go:registryBase`.
 *
 * THE SCHEME IS NOT ASSUMED. A bare `host:port` is plain HTTP, which is what the Controller serves
 * and what the object store this replaces was; an operator who typed `https://mirror` gets https.
 * Hardcoding `http://` would silently downgrade exactly the deployment that bothered to configure
 * TLS, on the one request that leaves the VPC.
 */
function registryBase(registry: string): string {
  const trimmed = registry.replace(/\/+$/, '');
  return /^https?:\/\//.test(trimmed) ? trimmed : `http://${trimmed}`;
}

/** The manifest a TAG resolves to. Read once per run, by this process and nothing else. */
export function bundleManifestUrl(registry: string, actor: string, ref: string): string {
  return `${registryBase(registry)}/v2/${bundleRepo(actor)}/manifests/${ref}`;
}

/** A blob of this actor's Bundle repository, by full `sha256:<hex>` digest. The config blob is
 * read through this too, which is why it takes a digest and not a bare sha. */
function bundleBlobUrlByDigest(registry: string, actor: string, digest: string): string {
  return `${registryBase(registry)}/v2/${bundleRepo(actor)}/blobs/${digest}`;
}

/** Where the Bundle's BYTES are. `cli/bundle.go:bundleBlobURL`, and the only address that ever
 * reaches a Machine — `machineInstall` curls this and checks it with `sha256sum`. Bare hex in,
 * because that is the spelling everything downstream of `resolveBundle` uses. */
export function bundleBlobUrl(registry: string, actor: string, sha: string): string {
  return bundleBlobUrlByDigest(registry, actor, `sha256:${sha}`);
}

/** What a registry is told this client can read. Without it a v2 registry may answer a manifest
 * GET with a 404 rather than with the manifest it holds — `cli/deploy.go:manifestAccept` says the
 * same thing from the other side. */
const MANIFEST_ACCEPT = [
  'application/vnd.oci.image.manifest.v1+json',
  'application/vnd.docker.distribution.manifest.v2+json',
].join(', ');

export interface ResolveBundleInput {
  actor: string;
  version: string;
  /** Where the Machines fetch from. Defaults to KONTRA_CONTROLLER, as the CLI does. */
  controller?: string;
  /** The registry, when it is not the Controller's own. Defaults to KONTRA_REGISTRY, then
   * `<controller>:5000` — the same ladder as `cli/bundle.go:bundleRegistry`, and it is what an
   * airgapped install points at its mirror. */
  registry?: string;
}

/** Exactly the placement fields `FleetArgs` takes, so the caller forwards it verbatim. */
export interface ResolvedBundle {
  actorName: string;
  actorVersion: string;
  actorEngine: string;
  /** The registry blob endpoint for the Bundle's one layer. */
  bundleUrl: string;
  /** sha256 of the bytes that endpoint serves, bare hex — checked on the Machine, and the value
   * ADR 0032 calls a Bundle's identity. It is the LAYER's digest, not the manifest's: the manifest
   * digest is the artifact's outer address and is not what `sha256sum` on a Machine produces. */
  bundleSha: string;
  /**
   * The Controller this bundle is fetched FROM — returned because it is also the Controller the
   * Machine must register WITH, and the two cannot be allowed to disagree.
   *
   * It was missing, and "forwards it verbatim" above was therefore false: a caller that did not
   * pass `controller=` sent placement args without one, and `machine.ts` refuses with
   * `controller="" is not safe to place on a Machine` — after the fleet API had already resolved
   * a perfectly good Controller here to build `bundleUrl` from. Deriving it once and returning it
   * makes fetching and registering the same decision rather than two that happen to match.
   */
  controller: string;
  /**
   * Digest-pinned Worker image (`host/name@sha256:…`) when `kontra deploy` has published one.
   * Empty when only the Bundle artifact exists. Docker fleets require it; DigitalOcean ignores it.
   */
  workerImage: string;
}

/** The manifest, narrowed to the three things a resolver reads. */
interface BundleManifest {
  artifactType?: string;
  config?: { mediaType?: string; digest?: string };
  layers?: Array<{ mediaType?: string; digest?: string }>;
}

/** The artifact's config blob — what `latest.json` used to be, now inside the manifest digest.
 * `cli/bundle.go:bundleConfig`. */
interface BundleConfig {
  name?: string;
  version?: string;
  engine?: string;
}

/**
 * Resolve `actor@version` to the Artifact a Machine should fetch.
 *
 * THE TAG IS MUTABLE; WHAT IT NAMES IS NOT. A caller resolves once, at the top of the run,
 * and the sha lands in its workflow history — so "what did this run place" stays answerable next
 * month even though `0.1.0` has moved since. The Machine still verifies the sha on arrival, so a
 * registry that lied fails the placement rather than running the wrong code.
 *
 * The engine is the field that cannot be derived. A tag already answers "which Bundle is `0.1.0`
 * today", but nothing in a digest says whether `/opt/kontra/actor/<name>/<name>` is a binary or
 * whether python should be pointed at `actor.py` — and getting that wrong produces a Machine that
 * installs cleanly, starts nothing, and reports as a healthy deploy.
 *
 * WHAT CHANGED WHEN THE BUNDLE BECAME AN OCI ARTIFACT (ADR 0036), because it is the reason this
 * function verifies something it never used to: the engine used to arrive in `latest.json`, a
 * mutable object sitting beside an Artifact that WAS verified. The one field nothing could check
 * was the one field a wrong answer made invisible. It now arrives in a config blob the manifest
 * digest covers, and this function checks that digest before reading it — so the pointer's single
 * unguarded field has no equivalent here.
 *
 * TWO REQUESTS AND NOT ONE, deliberately. The manifest is the naming and the config is the
 * meaning; putting the engine in a manifest annotation would have saved a round trip against a
 * registry on the same VPC and made the artifact's own type describe nothing. One request per
 * run is not the cost worth optimising.
 */
export async function resolveBundle(input: ResolveBundleInput): Promise<ResolvedBundle> {
  /**
   * THE DEFAULT IS THE COMPOSE SERVICE NAME, and the guard that used to follow it was dead code.
   *
   * `|| 'orchestrator-api'` means `controller` is never empty, so the `if (!controller) throw`
   * that sat here could not fire — it read as a check and was a comment. Removed rather than
   * reinstated, because the default is load-bearing: a dockerFleet Machine is a Warden container
   * on the Compose network and `orchestrator-api` is exactly right there, which is what makes the
   * local path zero-config (ADR 0047).
   *
   * IT IS WRONG FOR A DROPLET, and nothing here can tell the two apart. A cloud Machine handed
   * `orchestrator-api` starts, registers nothing and looks idle — so that boundary belongs to the
   * cloud provider, which knows what it is provisioning. `do_fleet`'s own preflight is where the
   * address has to be demanded.
   */
  const controller = input.controller || process.env.KONTRA_CONTROLLER || 'orchestrator-api';
  // The operator's spelling is kept, scheme and all — `registryBase` decides http vs https once,
  // for both the manifest read here and the blob URL every Machine is handed.
  const registry = (
    input.registry ||
    process.env.KONTRA_REGISTRY ||
    `${controller}:${REGISTRY_PORT}`
  ).replace(/\/+$/, '');

  const url = bundleManifestUrl(registry, input.actor, input.version);
  // THIRTY-THREE SECONDS OF SILENCE MEASURED HERE (canary-1790684761). This function reads a
  // manifest, verifies a digest and resolves a worker image, and said nothing at all while it did
  // — so the run page could only show "Hold the Fleet lease, in flight" and two zeroes. See
  // `runLog.ts` for why these lines carry `run_id` and the ordinary request log does not.
  runLog('bundle', `resolving ${input.actor}@${input.version} from ${registry}`, {
    actor: input.actor,
    actor_version: input.version,
    registry,
  });
  const workerImage = await resolveWorkerImage(registry, input.actor, input.version);
  runLog('bundle', workerImage ? `worker image ${workerImage}` : 'no worker image published', {
    actor: input.actor,
    worker_image: workerImage ?? '',
  });
  let res: Response | undefined;
  try {
    res = await fetch(url, { headers: { Accept: MANIFEST_ACCEPT } });
  } catch {
    res = undefined;
  }
  if (!res || res.status === 404) {
    if (workerImage) {
      return {
        actorName: input.actor,
        actorVersion: input.version,
        actorEngine: 'py',
        bundleUrl: '',
        bundleSha: '',
        controller,
        workerImage,
      };
    }
    throw new Error(
      `no Bundle published for ${input.actor}@${input.version} (looked for ` +
        `${bundleRepo(input.actor)}:${input.version} in the registry at ${registry}). ` +
        `Publish it first: kontra deploy --actor <dir>`
    );
  }
  if (!res.ok) throw new Error(`resolving the Bundle at ${url}: HTTP ${res.status}`);

  // THE BODY IS HASHED BEFORE IT IS PARSED. Everything below — the layer to fetch, the engine to
  // run — is read out of these bytes, so a manifest that does not hash to the digest the registry
  // reports for it is not something to interpret leniently.
  const body = await res.text();
  const digest = `sha256:${createHash('sha256').update(body).digest('hex')}`;
  const reported = res.headers.get('docker-content-digest');
  if (reported && reported !== digest) {
    throw new Error(
      `the manifest at ${url} hashes to ${digest}, but the registry reports ${reported} — ` +
        `something between this process and the registry is rewriting it`
    );
  }

  const man = JSON.parse(body) as BundleManifest;
  const layers = man.layers ?? [];
  const layer = layers.length === 1 ? layers[0] : undefined;
  if (!layer) {
    throw new Error(`${url} has ${layers.length} layers; a Bundle is exactly one tar.gz`);
  }
  // `sha256:` is stripped here and nowhere else: everything downstream — the placement args, the
  // MachineActor, `sha256sum | cut -d' ' -f1` on the Machine — is bare hex, and the one place the
  // two spellings meet should be the one place that knows about both.
  const sha = String(layer.digest ?? '').replace(/^sha256:/, '');
  if (!/^[a-f0-9]{64}$/.test(sha)) {
    throw new Error(`${url} names layer digest ${JSON.stringify(layer.digest)}`);
  }

  const cfgDigest = String(man.config?.digest ?? '');
  if (!/^sha256:[a-f0-9]{64}$/.test(cfgDigest)) {
    throw new Error(`${url} has no config blob; nothing there says which engine to run`);
  }
  const cfgRes = await fetch(bundleBlobUrlByDigest(registry, input.actor, cfgDigest));
  if (!cfgRes.ok) {
    throw new Error(`reading the bundle config ${cfgDigest} from ${registry}: HTTP ${cfgRes.status}`);
  }
  const cfg = (await cfgRes.json()) as BundleConfig;
  const engine = String(cfg.engine ?? '');
  if (engine !== 'py' && engine !== 'go') {
    throw new Error(`the bundle config at ${url} names engine ${JSON.stringify(engine)}`);
  }

  return {
    actorName: String(cfg.name || input.actor),
    actorVersion: String(cfg.version || input.version),
    actorEngine: engine,
    bundleUrl: bundleBlobUrl(registry, input.actor, sha),
    bundleSha: sha,
    controller,
    workerImage: await resolveWorkerImage(registry, String(cfg.name || input.actor), String(cfg.version || input.version)),
  };
}

/** The runnable Worker image `kontra deploy` pushes as `<registry>/<actor>:<version>`. */
async function resolveWorkerImage(registry: string, actor: string, version: string): Promise<string> {
  const advertised = registry.replace(/^https?:\/\//, '').replace(/\/+$/, '');
  const bases = [registryBase(registry)];
  if (/^(127\.0\.0\.1|localhost|::1)(:|$)/.test(advertised)) {
    const port = advertised.includes(':') ? advertised.split(':')[1] : '5000';
    bases.push(`http://host.docker.internal:${port}`, `http://registry:${port}`);
  }
  for (const base of bases) {
    try {
      const url = `${base}/v2/${actor}/manifests/${version}`;
      const res = await fetch(url, { headers: { Accept: MANIFEST_ACCEPT } });
      if (!res.ok) continue;
      const digest = res.headers.get('docker-content-digest');
      if (!digest || !/^sha256:[a-f0-9]{64}$/.test(digest)) continue;
      return `${advertised}/${actor}@${digest}`;
    } catch {
      continue;
    }
  }
  return '';
}

export interface QueuePollersInput {
  actor: string;
  version: string;
}

export interface QueuePollersOutput {
  queue: string;
  /**
   * Distinct poller identities on ONE Artifact's shared queue.
   *
   * It is one per **Machine** the placement landed on, which used to be the same as one per
   * **Machine** in the **Fleet** and is not since packing (ADR 0037): a placement puts at most one
   * Worker on a Machine, but it need not be on all of them — `workers=` says how many. So a
   * `ready()` gate that compared this to the Fleet's machine count would wait for ever on a
   * short placement, which is why the caller passes what it is waiting for.
   */
  pollers: number;
  /**
   * Set when Temporal could not be asked. `pollers` is then 0 AND MEANINGLESS — the caller must
   * treat it as unknown and keep waiting, never as "nothing is polling".
   *
   * The distinction is the whole point of this activity (`pollers.ts` carries the same
   * line, and `heartbeat.ts` records what conflating them costs: a monitor stuck at `0/0`
   * forever while looking like a measurement).
   */
  error?: string;
}

/**
 * How many Workers are polling this actor's shared queue right now.
 *
 * REGISTRATION SAYS AN ACTOR EXISTS; ONLY A POLLER SAYS IT CAN RUN. That is why `fleet.ready()`
 * gates on this rather than on Pulumi: `pulumi up` returning `succeeded` means the Droplets exist
 * and the install script exited 0, which is true a long time before systemd has a handler polling
 * — and a Batch dispatched into that gap sits on the queue until ScheduleToStart fires.
 */
export async function queuePollers(
  input: QueuePollersInput,
  describer?: QueueDescriber
): Promise<QueuePollersOutput> {
  const queue = sharedQueue(input.actor, input.version);
  const d = describer ?? temporalQueueDescriber();
  try {
    const state = await describeQueue(d, queue);
    return state.error
      ? { queue, pollers: 0, error: state.error }
      : { queue, pollers: state.identities.length };
  } finally {
    // Only close what we opened; a describer passed in belongs to the caller.
    if (!describer) await d.close().catch(() => undefined);
  }
}
