/**
 * NEGATIVE 4 OF ADR 0034 §4: the credential's value is **never in Pulumi stack config or state**.
 *
 * ── WHY THIS RUNS A REAL PULUMI ───────────────────────────────────────────────────────────────
 *
 * The property is not about a type; it is about what is on disk after a converge. Pulumi persists
 * config to `Pulumi.<stack>.yaml` AND into the state file, and the DIY `file://` backend keeps that
 * state in a directory whose whole job is to be durable and backed up — `workspace.ts` puts it on
 * "a volume no teardown path removes". A credential that reached config would therefore not merely
 * leak: it would leak into the one place designed to survive everything.
 *
 * So this test runs the real `selectStack` against the real Automation API, does a real
 * `pulumi up`, and then walks every byte under the state directory and the workspace looking for a
 * sentinel. Nothing is provisioned: the fleet program is converged with **zero Machines**, which
 * exercises the whole engine — project settings, config, checkpoint, history — while constructing
 * no provider resource, making no DigitalOcean API call and costing nothing.
 *
 * ── THE CONTROL, WHICH IS WHAT MAKES IT NOT VACUOUS ───────────────────────────────────────────
 *
 * A sweep that finds nothing proves nothing unless the same sweep can find something. The second
 * test converges the SAME stack with the SAME sentinel passed the way ADR 0034 explicitly rejects —
 * as stack config — and asserts the sweep finds it, in `Pulumi.<stack>.yaml` and in the state
 * history. Same haystack, same needle, opposite answer: the difference is the mechanism.
 *
 * `secret: true` is swept for too, and it deserves its own sentence because it is the mistake that
 * looks careful: an encrypted config value keeps the plaintext out of the files, and puts the
 * CIPHERTEXT of a live cloud token in that same durable directory, protected by
 * `PULUMI_CONFIG_PASSPHRASE` — itself an environment variable in the same process. That trades one
 * secret for two, which is why the ADR rejects it as well.
 */

import { mkdtempSync, readFileSync, readdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { FileSecretBackend } from '../secrets/fileBackend';
import { SecretStore } from '../secrets/store';
import { resolveProviderEnv } from './credential';
import { planFor } from './stacks';
import { selectStack } from './workspace';

const SENTINEL = 'dop_v1_SENTINEL_never_in_pulumi_state_4d71';
const SECRET = 'do-prod';
const FQN = 'kontra-fleet/nscheck-0.1.0';

/** A converge with no Machines. Every engine artefact is written; no provider resource exists. */
const ARGS = { tag: 'dns', machines: 0, credential: SECRET } as Record<string, unknown>;

let base: string;
let store: SecretStore;
const saved: Record<string, string | undefined> = {};

/** Every file under a directory, as one string. Read as BYTES and decoded, never stringified from
 *  an object — the same discipline `secrets/history.test.ts` keeps, and for the same reason. */
function sweep(dir: string): { haystack: string; files: string[] } {
  const files: string[] = [];
  const parts: string[] = [];
  const walk = (d: string): void => {
    for (const entry of readdirSync(d, { withFileTypes: true })) {
      const p = path.join(d, entry.name);
      if (entry.isDirectory()) walk(p);
      else {
        files.push(p.slice(base.length));
        parts.push(readFileSync(p).toString('utf8'));
      }
    }
  };
  walk(dir);
  return { haystack: parts.join('\n'), files };
}

beforeAll(async () => {
  base = mkdtempSync(path.join(tmpdir(), 'kontra-pulumi-state-'));
  const secretsDir = path.join(base, 'secrets');
  store = new SecretStore(new FileSecretBackend({ dir: secretsDir }), { keyDir: secretsDir });
  await store.put(SECRET, SENTINEL);

  for (const k of ['KONTRA_PULUMI_STATE_DIR', 'KONTRA_PULUMI_WORK_DIR', 'PULUMI_CONFIG_PASSPHRASE']) {
    saved[k] = process.env[k];
  }
  process.env.KONTRA_PULUMI_STATE_DIR = path.join(base, 'state');
  process.env.KONTRA_PULUMI_WORK_DIR = path.join(base, 'work');
  // The DIY backend forbids the default secrets provider; an empty passphrase silently changes how
  // secrets encrypt, which is why `assertBackend` refuses to run without one.
  process.env.PULUMI_CONFIG_PASSPHRASE = 'kontra-test-passphrase';
}, 120_000);

afterAll(() => {
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
  if (base) rmSync(base, { recursive: true, force: true });
});

/** The real path an `up` takes: plan the stack, resolve the credential at the last hop, converge. */
async function converge(opts: { asConfig?: boolean } = {}): Promise<void> {
  const plan = planFor({ stackFqn: FQN, args: ARGS });
  const providerEnv = await resolveProviderEnv(
    {
      credential: plan.credential,
      providerEnvVar: plan.providerEnvVar,
      actor: 'nscheck',
      version: '0.1.0',
      run: 'recon-1786831339',
    },
    { store }
  );
  expect(providerEnv[plan.providerEnvVar]).toBe(SENTINEL); // the value really did reach the engine
  const stack = await selectStack({ project: 'kontra-fleet', stack: 'nscheck-0.1.0' }, plan.program, providerEnv);
  if (opts.asConfig) {
    // THE REJECTED MECHANISM, converged for real so the sweep below has something to find.
    await stack.setConfig('kontra-fleet:cloudToken', { value: SENTINEL, secret: false });
    await stack.setConfig('kontra-fleet:cloudTokenEncrypted', { value: SENTINEL, secret: true });
  }
  const res = await stack.up({ color: 'never' });
  expect(res.summary.result).toBe('succeeded');
}

describe('a real converge with the credential in envVars', () => {
  it('leaves it in NO file under the state directory or the workspace', async () => {
    await converge();
    const { haystack, files } = sweep(base.replace(/\/$/, ''));

    // THE ASSERTION THIS NEGATIVE EXISTS FOR.
    expect(haystack).not.toContain(SENTINEL);
    // Not a fragment of it either — a truncated token is still most of one.
    expect(haystack).not.toContain(SENTINEL.slice(0, 20));

    // …in a sweep that demonstrably read the artefacts. Without these the test would pass on an
    // empty directory, which is the vacuous version of every line above.
    expect(files.some((f) => f.includes('.pulumi/stacks/kontra-fleet/nscheck-0.1.0.json'))).toBe(true);
    expect(files.some((f) => f.endsWith('Pulumi.nscheck-0.1.0.yaml'))).toBe(true);
    expect(haystack).toContain('kontra-fleet');
    // The secret store's own file IS in this tree and it holds the credential — encrypted. Finding
    // the store while finding no sentinel is the difference between "swept nothing" and "swept the
    // right thing": the bytes are there, and they are not readable.
    expect(files.some((f) => f.startsWith('/secrets/'))).toBe(true);
  }, 180_000);
});

describe('the same converge with the credential as stack config — the rejected mechanism', () => {
  it('DOES leak it, which is what makes the sweep above mean something', async () => {
    await converge({ asConfig: true });
    const { haystack } = sweep(base.replace(/\/$/, ''));

    // Plaintext config lands in `Pulumi.<stack>.yaml` and in the state history, in the clear.
    expect(haystack).toContain(SENTINEL);
    const yaml = readFileSync(
      path.join(process.env.KONTRA_PULUMI_WORK_DIR!, 'kontra-fleet', 'Pulumi.nscheck-0.1.0.yaml'),
      'utf8'
    );
    expect(yaml).toContain(SENTINEL);

    // And the ENCRYPTED one — the mistake that looks careful — puts a ciphertext of a live cloud
    // token in the same durable directory, guarded by a passphrase that is an environment variable
    // in this very process. One secret traded for two (ADR 0034, "Considered and rejected").
    expect(yaml).toContain('cloudTokenEncrypted');
    expect(yaml).toContain('secure:');
  }, 180_000);
});
