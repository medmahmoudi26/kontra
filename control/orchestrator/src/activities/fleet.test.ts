/**
 * The two reads the fleet scope makes from inside a caller's workflow.
 *
 * Both fail quietly when they are wrong, which is why they are tested at all. A bundle resolved
 * with the wrong engine produces a Machine that installs cleanly, starts nothing and reports as a
 * successful deploy; a poller count that conflates "we could not ask Temporal" with "nothing is
 * polling" opens the readiness gate onto a fleet that is not there.
 */

import { createHash } from 'node:crypto';

import { afterEach, describe, expect, it, vi } from 'vitest';
import { bundleBlobUrl, bundleManifestUrl, queuePollers, resolveBundle } from './fleet';
import type { PollerInfo, QueueDescriber, TaskQueueType } from '../pollers';

const SHA = 'b'.repeat(64);
const CONFIG = { name: 'nscheck', version: '0.1.0', engine: 'go' };

function digestOf(s: string): string {
  return `sha256:${createHash('sha256').update(s).digest('hex')}`;
}

/**
 * Stand in for the registry: a manifest at the tag, a config blob behind it.
 *
 * IT ROUTES BY URL RATHER THAN ANSWERING EVERYTHING, and that is not fussiness. Resolving a
 * Bundle is now two requests and the second one's address is read out of the first one's body —
 * so a mock that returns the same object for every GET would let a resolver that never fetched
 * the config, or fetched it from the wrong repository, pass every assertion below. It also
 * carries `docker-content-digest`, because the resolver checks the manifest against it.
 */
function serveRegistry(
  opts: { manifest?: unknown; config?: unknown; status?: number; digest?: string } = {}
) {
  const config = opts.config ?? CONFIG;
  const configBody = JSON.stringify(config);
  const configDigest = digestOf(configBody);
  const manifest =
    opts.manifest ??
    ({
      schemaVersion: 2,
      mediaType: 'application/vnd.oci.image.manifest.v1+json',
      artifactType: 'application/vnd.kontra.bundle.v1+json',
      config: { mediaType: 'application/vnd.kontra.bundle.config.v1+json', digest: configDigest },
      layers: [
        { mediaType: 'application/vnd.kontra.bundle.layer.v1.tar+gzip', digest: `sha256:${SHA}` },
      ],
    } as unknown);
  const manifestBody = JSON.stringify(manifest);

  const fetchMock = vi.fn(async (url: string) => {
    if (url.includes('/manifests/')) {
      const status = opts.status ?? 200;
      return {
        ok: status < 300,
        status,
        headers: new Headers({ 'docker-content-digest': opts.digest ?? digestOf(manifestBody) }),
        text: async () => manifestBody,
      };
    }
    if (url.endsWith(`/blobs/${configDigest}`)) {
      return { ok: true, status: 200, headers: new Headers(), json: async () => config };
    }
    return { ok: false, status: 404, headers: new Headers(), json: async () => ({}) };
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('resolveBundle', () => {
  const args = { actor: 'nscheck', version: '0.1.0', controller: '10.124.0.2' };

  it('resolves the tag against the Controller\'s registry', async () => {
    const fetchMock = serveRegistry();
    await resolveBundle(args);
    expect(fetchMock).toHaveBeenCalledWith(
      bundleManifestUrl('10.124.0.2:5000', 'nscheck', '0.1.0'),
      expect.objectContaining({ headers: expect.objectContaining({ Accept: expect.any(String) }) })
    );
  });

  it('builds the blob URL a MACHINE fetches from, not this process\'s view of the registry', async () => {
    // A Machine resolving `localhost:5000` means its own loopback. This has been a real outage on
    // the store this replaced, and moving to a registry does not make it stop being possible.
    serveRegistry();
    const got = await resolveBundle({ ...args, registry: '10.124.0.2:5000' });
    expect(got.bundleUrl).toBe(bundleBlobUrl('10.124.0.2:5000', 'nscheck', SHA));
    expect(got.bundleSha).toBe(SHA);
  });

  it('strips `sha256:` exactly once, at this boundary', async () => {
    // Everything downstream is bare hex: the placement args, MachineActor's `SAFE.sha`, and
    // `sha256sum | cut -d' ' -f1` on the Machine. A prefix that survived would fail validation
    // with a message about an unsafe value rather than about a spelling.
    serveRegistry();
    expect((await resolveBundle(args)).bundleSha).toMatch(/^[a-f0-9]{64}$/);
  });

  it('carries the engine, read from the config blob the manifest digest covers', async () => {
    // The tag already says which Artifact is newest. Nothing in a digest says whether the Machine
    // execs a binary or points python at actor.py — and under `latest.json` that was the one field
    // nothing verified. It is inside the manifest digest now.
    const fetchMock = serveRegistry();
    expect((await resolveBundle(args)).actorEngine).toBe('go');
    // Two requests, and the second one's address came out of the first one's body.
    expect(fetchMock.mock.calls.map((c) => String(c[0])).some((u) => u.includes('/blobs/'))).toBe(
      true
    );
  });

  it('refuses a manifest that does not hash to the digest the registry reports', async () => {
    // Everything the resolver goes on to do — which layer to place, which engine to run — is read
    // out of these bytes, so a body that has been rewritten in flight is not something to parse
    // leniently.
    serveRegistry({ digest: `sha256:${'c'.repeat(64)}` });
    await expect(resolveBundle(args)).rejects.toThrow(/rewriting it/);
  });

  it('returns exactly the placement fields FleetArgs takes', async () => {
    // This list IS the contract: `fleet.up()` spreads this object straight into the stack args,
    // so a field missing here is a field missing from the converge. `controller` was, and the
    // converge failed at placement — `controller="" is not safe to place on a Machine` — for a
    // caller that had no reason to pass one, because this activity had already resolved a good
    // Controller to build bundleUrl from and then dropped it.
    serveRegistry();
    expect(Object.keys(await resolveBundle(args)).sort()).toEqual([
      'actorEngine',
      'actorName',
      'actorVersion',
      'bundleSha',
      'bundleUrl',
      'controller',
      'workerImage',
    ]);
  });

  it('returns the SAME controller the bundle url is built from', async () => {
    // Fetching and registering have to be one decision. If they could differ, a Machine would
    // pull its Artifact from one Controller and register its Worker with another — and the
    // symptom is a fleet that provisions perfectly and never polls the queue.
    serveRegistry();
    const got = await resolveBundle({ ...args, controller: '10.124.0.2' });
    expect(got.controller).toBe('10.124.0.2');
    expect(got.bundleUrl.startsWith(`http://${got.controller}:5000/`)).toBe(true);
  });

  it('pairs the url with the sha, because a Machine checks one against the other', async () => {
    // `validateMachineActor` refuses a pair that disagrees, and it can only do that because an OCI
    // blob is addressed BY its digest. This is where the pair is minted.
    serveRegistry();
    const got = await resolveBundle(args);
    expect(got.bundleUrl.endsWith(got.bundleSha)).toBe(true);
  });

  it('names the fix when nothing has been published', async () => {
    // The most likely first failure of the whole API: the actor is in the checkout and has simply
    // never been built. An operator should not have to find that out from a 404.
    serveRegistry({ status: 404 });
    // `deploy`, NOT `build`. This asserted `kontra build --actor`, which is not a verb the CLI has
    // — so the test was pinning a sentence that would have sent an operator to a command that does
    // not exist, and it failed the moment the message was corrected rather than when it was wrong.
    await expect(resolveBundle(args)).rejects.toThrow(/kontra deploy --actor/);
  });

  it('refuses a malformed artifact rather than placing whatever it says', async () => {
    const layer = {
      mediaType: 'application/vnd.kontra.bundle.layer.v1.tar+gzip',
      digest: `sha256:${SHA}`,
    };
    const cfg = { mediaType: 'application/vnd.kontra.bundle.config.v1+json', digest: digestOf('{}') };
    for (const manifest of [
      { config: cfg, layers: [] }, //                          nothing to place
      { config: cfg, layers: [layer, layer] }, //              two layers: which one is the Bundle?
      { config: cfg, layers: [{ ...layer, digest: 'sha256:not-a-sha' }] },
      { config: cfg, layers: [{ ...layer, digest: '' }] },
      { layers: [layer] }, //                                  no config: nothing names the engine
      { config: { digest: 'garbage' }, layers: [layer] },
      {},
    ]) {
      serveRegistry({ manifest });
      await expect(resolveBundle(args), JSON.stringify(manifest)).rejects.toThrow();
    }
    // And the engine itself, which is the field the whole resolution exists for.
    for (const engine of ['rust', '', undefined]) {
      serveRegistry({ config: { ...CONFIG, engine } });
      await expect(resolveBundle(args), `engine=${engine}`).rejects.toThrow(/engine/);
    }
  });

  it('falls back to the Compose service name when nothing names the Controller', async () => {
    /**
     * THIS ASSERTED A REFUSAL, AND THE REFUSAL IS UNREACHABLE.
     *
     * `resolveBundle` reads `input.controller || KONTRA_CONTROLLER || 'orchestrator-api'`, so the
     * `if (!controller) throw` under it could never fire — the test was pinning dead code, and the
     * dead code has been removed rather than the default.
     *
     * THE DEFAULT IS RIGHT FOR THE CASE IT WAS ADDED FOR AND WRONG FOR THE OTHER ONE, which is
     * why it is asserted here with the hazard written down. A dockerFleet Machine is a Warden
     * container on the Compose network, where `orchestrator-api` resolves — that is the zero-config
     * local path (ADR 0047) and demanding an env var for it would be a regression. A DigitalOcean
     * Droplet is not on that network, and `orchestrator-api` is not an address it can reach: it
     * starts, registers nothing, and looks idle, which is exactly the failure the deleted message
     * described. The canary's `provider` field says so in the console, in the author's own words.
     *
     * So the boundary that has to hold is the CLOUD provider's, not this function's — it has no
     * idea which one it is resolving for.
     */
    serveRegistry();
    const prev = process.env.KONTRA_CONTROLLER;
    delete process.env.KONTRA_CONTROLLER;
    try {
      const got = await resolveBundle({ actor: 'a', version: '1' });
      expect(got.controller).toBe('orchestrator-api');
      expect(got.bundleUrl).toContain('orchestrator-api:5000');
    } finally {
      if (prev !== undefined) process.env.KONTRA_CONTROLLER = prev;
    }
  });

  it('prefers an explicit controller over the environment and over the default', async () => {
    // The non-vacuous partner: a function that ignored its input and always answered the default
    // would pass the case above.
    serveRegistry();
    const prev = process.env.KONTRA_CONTROLLER;
    process.env.KONTRA_CONTROLLER = 'from-the-env';
    try {
      const got = await resolveBundle({ actor: 'a', version: '1', controller: '10.124.0.2' });
      expect(got.controller).toBe('10.124.0.2');
    } finally {
      if (prev === undefined) delete process.env.KONTRA_CONTROLLER;
      else process.env.KONTRA_CONTROLLER = prev;
    }
  });
});

/** A describer that answers with fixed identities, or throws. */
function describer(answer: PollerInfo[] | Error): QueueDescriber & { closed: number } {
  const d = {
    closed: 0,
    async pollers(_q: string, _t: TaskQueueType): Promise<PollerInfo[]> {
      if (answer instanceof Error) throw answer;
      return answer;
    },
    async close(): Promise<void> {
      d.closed += 1;
    },
  };
  return d;
}

describe('queuePollers', () => {
  const args = { actor: 'nscheck', version: '0.1.0' };

  it('watches the SHARED queue — the one the handler binds', async () => {
    // Not `<shared>-sessions`, which is what the actor process itself polls. The handler is what
    // `kontra workers list` counts, and what a dispatch waits on.
    const got = await queuePollers(args, describer([]));
    expect(got.queue).toBe('nscheck-0.1.0');
  });

  it('counts DISTINCT identities, because one Machine polls both queue types', async () => {
    const d = describer([
      { identity: '11@kf-dns-01@nscheck-0.1.0', lastAccess: 1 },
      { identity: '11@kf-dns-01@nscheck-0.1.0', lastAccess: 2 },
      { identity: '12@kf-dns-02@nscheck-0.1.0', lastAccess: 3 },
    ]);
    expect((await queuePollers(args, d)).pollers).toBe(2);
  });

  it('reports a failed describe as an ERROR, never as zero pollers', async () => {
    // The distinction the readiness gate is built on. "Temporal is unreachable" and "nothing is
    // polling" are different facts about the world, and the second one authorises a dispatch.
    const got = await queuePollers(args, describer(new Error('connection refused')));
    expect(got.error).toMatch(/connection refused/);
    expect(got.pollers).toBe(0);
  });

  it('distinguishes a real zero from an unknown one', async () => {
    // Registered and nothing polling: the classic trap, and a REAL measurement. No error field.
    const got = await queuePollers(args, describer([]));
    expect(got.pollers).toBe(0);
    expect(got.error).toBeUndefined();
  });

  it('does not close a describer it was handed', async () => {
    // It belongs to the caller; closing it would break the next call in a poll loop.
    const d = describer([]);
    await queuePollers(args, d);
    expect(d.closed).toBe(0);
  });
});

// THE FAILURE THIS PINS COST A CI ROUND AND BLAMED THE WRONG THING. `resolveBundle` falls back to
// the worker image when there is no Bundle manifest, and that fallback is what the compose install
// relies on — `kontra deploy` publishes an image, not a Bundle. With auth on, the manifest read was
// 401, the fallback read "no worker image published", and the install reported
// `no Bundle published for hello@0.1.0 … Publish it first: kontra deploy --actor <dir>` about an
// actor whose image it had just built and pushed.
describe('an authenticated registry', () => {
  const args = { actor: 'nscheck', version: '0.1.0', controller: '10.124.0.2' };

  it('is read with the credential, on every request', async () => {
    const fetchMock = serveRegistry();
    process.env.KONTRA_REGISTRY_PULL_PASSWORD = 'pullpw';
    try {
      await resolveBundle(args);
    } finally {
      delete process.env.KONTRA_REGISTRY_PULL_PASSWORD;
    }
    const expected = `Basic ${Buffer.from('pull:pullpw').toString('base64')}`;
    const withoutAuth = fetchMock.mock.calls.filter(
      ([, init]) => (init as RequestInit | undefined)?.headers?.['authorization' as never] !== expected
    );
    expect(withoutAuth.map(([u]) => u)).toEqual([]);
  });

  it('sends nothing when no account is configured, because that install is anonymous', async () => {
    const fetchMock = serveRegistry();
    await resolveBundle(args);
    for (const [, init] of fetchMock.mock.calls) {
      const headers = (init as RequestInit | undefined)?.headers as Record<string, string> | undefined;
      expect(headers?.authorization).toBeUndefined();
    }
  });
});
