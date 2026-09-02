/**
 * NEGATIVE 5 OF ADR 0034 §4: the credential's value is **never on a Machine**.
 *
 * ── WHY THIS ONE IS DIFFERENT FROM THE OTHERS ─────────────────────────────────────────────────
 *
 * A fleet Machine is not a place a secret can be "protected". Everything that reaches one arrives
 * through channels that are readable by design or by accident: `userData` is cloud-init, which
 * DigitalOcean stores and any process on the Droplet can read from the metadata service; the
 * placement command is a root shell script, interpolated from strings, that ends up in a systemd
 * unit and in `journalctl`; and the **Bundle** it fetches is served from an ANONYMOUS store — a
 * world-readable artifact by design. There is no encryption anywhere on that path, so the only
 * thing that keeps a credential off a Machine is not putting it in the arguments that build one.
 *
 * ── THE STRUCTURAL HALF, WHICH IS THE REAL DEFENCE ────────────────────────────────────────────
 *
 * `stacks.ts` keeps the credential OUT OF `FleetArgs`. `FleetArgs` is what reaches
 * `programs/fleet.ts` — the one provider-coupled file, the one that writes cloud-init and the
 * remote install command — and it has no credential field, so the program cannot interpolate one
 * even by accident. The reference travels beside the args, to `workspace.ts`, and stops there.
 *
 * ── THE MEASURED HALF ─────────────────────────────────────────────────────────────────────────
 *
 * Types are checked at build time and a sweep is checked against reality, so this runs the REAL
 * fleet program — with a full placement: a Bundle, an actor, a Controller, a session cap — under
 * Pulumi's mock runtime. No engine, no provider, no API call, no Droplet. What the mocks capture is
 * every resource input the program actually constructs: the Droplet's `userData`, tags and ssh key
 * ids, and the remote command's `create`, `update` and `delete` scripts, byte for byte as they
 * would be sent. That whole haystack is swept for a sentinel that is, at the same moment, sitting
 * resolved in the engine environment beside it.
 */

import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

import * as pulumi from '@pulumi/pulumi';
import { beforeAll, describe, expect, it } from 'vitest';

import { FileSecretBackend } from '../secrets/fileBackend';
import { SecretStore } from '../secrets/store';
import { resolveProviderEnv } from './credential';
import { machineInstall, machineTeardown } from './programs/machine';
import { coerceFleetArgs, planFor } from './stacks';

const SENTINEL = 'dop_v1_SENTINEL_never_on_a_machine_8ea2';
const SECRET = 'do-prod';
const FQN = 'kontra-fleet/nscheck-0.1.0';
const BUNDLE_SHA = 'a'.repeat(64);

/** A converge that places a real Artifact on real Machines — the case with the most strings in it. */
const ARGS: Record<string, unknown> = {
  tag: 'dns',
  machines: 2,
  credential: SECRET,
  region: 'nyc3',
  size: 's-1vcpu-2gb',
  bundleUrl: `http://10.124.0.2:5000/v2/bundles/nscheck/blobs/sha256:${BUNDLE_SHA}`,
  bundleSha: BUNDLE_SHA,
  actorName: 'nscheck',
  actorVersion: '0.1.0',
  actorEngine: 'py',
  controller: '10.124.0.2',
  maxSessions: 8,
};

/** Every resource input the program constructed, flattened to one string. */
const captured: unknown[] = [];
let providerEnv: Record<string, string> = {};
let outputs: Record<string, unknown> = {};

/**
 * Walk to the leaves, decoding byte buffers, exactly as `secrets/history.test.ts` does — a
 * `JSON.stringify` of a `Uint8Array` searches a list of integers and would find nothing.
 */
function flatten(node: unknown, out: string[] = []): string {
  if (node === null || node === undefined) return out.join('\n');
  if (typeof node === 'string') out.push(node);
  else if (node instanceof Uint8Array) out.push(Buffer.from(node).toString('utf8'));
  else if (Array.isArray(node)) for (const item of node) flatten(item, out);
  else if (typeof node === 'object') for (const v of Object.values(node)) flatten(v, out);
  else out.push(String(node));
  return out.join('\n');
}

beforeAll(async () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'kontra-machine-secret-'));
  const store = new SecretStore(new FileSecretBackend({ dir }), { keyDir: dir });
  await store.put(SECRET, SENTINEL);

  // The fleet SSH key the placement reads at converge time. A DIFFERENT secret, deliberately in
  // this fixture: ADR 0034 finding 3 says the infra worker is not secret-free afterwards, and a
  // test that swept a haystack containing no secrets at all would prove less.
  const keyFile = path.join(dir, 'fleet_key');
  writeFileSync(keyFile, '-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-real-key\n-----END-----\n');
  process.env.KONTRA_SSH_KEY = keyFile;

  const plan = planFor({ stackFqn: FQN, args: ARGS });
  providerEnv = await resolveProviderEnv(
    {
      credential: plan.credential,
      providerEnvVar: plan.providerEnvVar,
      actor: 'nscheck',
      version: '0.1.0',
      run: 'recon-1786831339',
    },
    { store }
  );

  // Pulumi's own unit-test runtime: resources are constructed for real by the program and
  // intercepted here instead of reaching an engine. Nothing is provisioned and nothing is dialled.
  pulumi.runtime.setMocks(
    {
      newResource: (args: pulumi.runtime.MockResourceArgs) => {
        captured.push({ type: args.type, name: args.name, inputs: args.inputs });
        return {
          id: `${args.name}-id`,
          state: {
            ...args.inputs,
            ipv4Address: '203.0.113.10',
            ipv4AddressPrivate: '10.124.0.11',
            name: args.inputs.name ?? args.name,
          },
        };
      },
      call: (args: pulumi.runtime.MockCallArgs) => args.inputs,
    },
    'kontra-fleet',
    'nscheck-0.1.0',
    false
  );

  outputs = ((await plan.program()) ?? {}) as Record<string, unknown>;
  // The inventory is an Output; resolve it so its contents are in the haystack too.
  outputs = {
    ...outputs,
    inventory: await new Promise((resolve) =>
      (outputs.inventory as pulumi.Output<unknown>).apply((v) => {
        resolve(v);
        return v;
      })
    ),
  };

  // THE PLACEMENT COMMANDS REGISTER AFTER THE PROGRAM RETURNS, and missing them is exactly the
  // vacuous sweep this file exists to avoid: they `dependsOn` a Droplet, so their registration
  // awaits that resource's URN and lands on a later turn of the event loop. Settle until the
  // capture stops growing — a fixed `setTimeout` would be a race that fails on a loaded machine,
  // and a sweep of two Droplets and no install scripts would pass while proving half of nothing.
  for (let quiet = 0, seen = -1; quiet < 5; quiet += captured.length === seen ? 1 : 0) {
    seen = captured.length;
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
}, 120_000);

describe('everything this stack sends to a Machine', () => {
  it('carries no trace of the credential — and the sweep demonstrably read the real thing', () => {
    const haystack = flatten({ captured, outputs, install: machineInstall({
      name: 'nscheck',
      version: '0.1.0',
      engine: 'py',
      bundleUrl: String(ARGS.bundleUrl),
      bundleSha: BUNDLE_SHA,
      controller: '10.124.0.2',
      tag: 'dns',
      maxSessions: 8,
    }), teardown: machineTeardown('nscheck') });

    // THE ASSERTION THIS NEGATIVE EXISTS FOR.
    expect(haystack).not.toContain(SENTINEL);
    expect(haystack).not.toContain(SENTINEL.slice(0, 20));
    // Nor the NAME of a variable that would carry it: a `DIGITALOCEAN_TOKEN=` written into
    // `worker.env` or a unit file would be the shape this failure takes.
    expect(haystack).not.toContain('DIGITALOCEAN_TOKEN');

    // …in a haystack that is demonstrably the real placement. The value is resolved and sitting in
    // the engine environment at this very moment — the credential exists, it simply is not here.
    expect(providerEnv.DIGITALOCEAN_TOKEN).toBe(SENTINEL);
    expect(haystack).toContain(String(ARGS.bundleUrl));
    expect(haystack).toContain(BUNDLE_SHA);
    expect(haystack).toContain('KONTRA_MAX_PARALLEL_SESSIONS=8');
    expect(haystack).toContain('#cloud-config');
    expect(haystack).toContain('kf-dns-01');
  });

  it('constructed the resources a fleet really constructs, not a stub', () => {
    // Without this the sweep could pass because the program built nothing at all.
    const types = captured.map((c) => (c as { type: string }).type);
    expect(types.filter((t) => t.includes('Droplet'))).toHaveLength(2);
    expect(types.filter((t) => t.includes('command'))).toHaveLength(2);
    const droplet = captured.find((c) => (c as { type: string }).type.includes('Droplet')) as {
      inputs: Record<string, unknown>;
    };
    expect(String(droplet.inputs.userData)).toContain('#cloud-config');
    expect(droplet.inputs.region).toBe('nyc3');

    // The remote command's OWN script — the root shell that runs on every Machine — is in the
    // haystack, not just the copy this test builds beside it. Without this the sweep could be
    // reading two Droplets and a locally-generated string while the real placement went unread.
    const cmd = captured.find((c) => (c as { type: string }).type.includes('command')) as {
      inputs: Record<string, unknown>;
    };
    expect(String(cmd.inputs.create)).toContain('KONTRA_MAX_PARALLEL_SESSIONS=8');
    expect(String(cmd.inputs.create)).toContain(BUNDLE_SHA);
    expect(String(cmd.inputs.delete)).toContain('systemctl stop');
    // …and none of the three scripts carries the credential.
    for (const key of ['create', 'update', 'delete']) {
      expect(String(cmd.inputs[key]), key).not.toContain(SENTINEL);
    }
  });

  it('never gave the program a credential field to interpolate in the first place', () => {
    // THE STRUCTURAL HALF. `coerceFleetArgs` narrows untrusted JSON field by field, and `credential`
    // is not in the list — so the type that reaches the only provider-coupled file has no room for
    // a credential, whatever a caller sends and whatever a future edit to the program does.
    const args = coerceFleetArgs({ ...ARGS, credential: SENTINEL, token: SENTINEL });
    expect(args).not.toHaveProperty('credential');
    expect(JSON.stringify(args)).not.toContain(SENTINEL);
    // …and the plan still knows the name, because it read it before the narrowing.
    expect(planFor({ stackFqn: FQN, args: ARGS }).credential).toEqual({ name: SECRET });
  });

  it('does not echo a credential back in the stack outputs either', () => {
    // The outputs are read back by `kontra fleet up` to inherit a placement it is not changing, and
    // they are the one part of a stack that is designed to be shown to a caller.
    expect(Object.keys(outputs)).not.toContain('credential');
    expect(flatten(outputs)).not.toContain(SENTINEL);
  });
});
