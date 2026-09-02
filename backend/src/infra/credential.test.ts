/**
 * The cloud credential as a NAME: what a caller may say, what it costs to be wrong, and the
 * rotation that is the whole reason it moved (ADR 0034 §4).
 *
 * The two negatives this slice owns — never in Pulumi stack config or state, never on a Machine —
 * are proved against real artefacts in `pulumiState.test.ts` and `machineSecret.test.ts`. This file
 * is the behaviour underneath them: the coercion that decides what crosses the wire, the start-time
 * refusal that names the secret, the ledger line every resolution leaves, and the source guard that
 * keeps the environment variable from creeping back.
 */

import { mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

import { beforeEach, describe, expect, it } from 'vitest';

import { ResolutionLog } from '../secrets/audit';
import { FileSecretBackend } from '../secrets/fileBackend';
import { SecretStore } from '../secrets/store';
import {
  CloudCredentialUnavailable,
  FLEET_CREDENTIAL_SLOT,
  adoptLegacyCloudToken,
  checkCredential,
  coerceCredential,
  credentialFrom,
  credentialLabel,
  defaultCloudCredential,
  looksLikeAToken,
  resolveProviderEnv,
} from './credential';
import { planFor } from './stacks';
import { engineEnv } from './workspace';

/** Distinctive enough that finding it anywhere is proof, and finding nothing is not luck. */
const SENTINEL = 'dop_v1_SENTINEL_never_in_the_clear_9f3c';
const ROTATED = 'dop_v1_SENTINEL_after_the_rotation_5b2e';

let dir: string;
let store: SecretStore;
let ledger: ResolutionLog;

function fresh(): void {
  dir = mkdtempSync(path.join(tmpdir(), 'kontra-cloud-cred-'));
  store = new SecretStore(new FileSecretBackend({ dir }), { keyDir: dir });
  ledger = new ResolutionLog({ dir });
}

const use = (credential: { name: string; version?: number }) => ({
  credential,
  providerEnvVar: 'DIGITALOCEAN_TOKEN',
  actor: 'nscheck',
  version: '0.1.0',
  run: 'recon-1786831339',
});

beforeEach(fresh);

describe('what a caller may name', () => {
  it('takes a bare name and a pinned reference, and NOTHING that could carry a value', () => {
    expect(coerceCredential('do-prod')).toEqual({ name: 'do-prod' });
    expect(coerceCredential({ name: 'do-prod', version: 3 })).toEqual({ name: 'do-prod', version: 3 });
    // The one shape a mistake takes: a caller that put the token where the name goes. There is no
    // field for it, so it is dropped exactly the way `evil: 'rm -rf'` is in `coerceFleetArgs`.
    const smuggled = coerceCredential({
      name: 'do-prod',
      value: SENTINEL,
      token: SENTINEL,
    } as Record<string, unknown>);
    expect(JSON.stringify(smuggled)).not.toContain(SENTINEL);
    expect(smuggled).toEqual({ name: 'do-prod' });
  });

  it('falls back to this control plane\'s default name, which is a NAME and not a secret', () => {
    // `kontra fleet down` has no `--credential` flag (cli/fleet.go). Without a default, a fleet
    // brought up from Python under `do-prod` could not be torn down from the CLI at all.
    expect(defaultCloudCredential({})).toBe('do-token');
    expect(defaultCloudCredential({ KONTRA_CLOUD_CREDENTIAL: 'do-prod' })).toBe('do-prod');
    expect(credentialFrom(undefined, {})).toEqual({ name: 'do-token' });
    expect(credentialFrom({ tag: 'dns', machines: 4 }, { KONTRA_CLOUD_CREDENTIAL: 'do-prod' }))
      .toEqual({ name: 'do-prod' });
    // Read per call, not frozen at module load — a rotation of the NAME must not cost a restart
    // either, and this is the one knob `FLEET_DEFAULTS`' freeze was never defending.
    expect(credentialFrom({ credential: '  ' }, { KONTRA_CLOUD_CREDENTIAL: 'do-prod' }))
      .toEqual({ name: 'do-prod' });
  });

  it('ignores a version that is not a version, rather than pinning to nonsense', () => {
    for (const bad of [0, -1, 1.5, 'two', null]) {
      expect(coerceCredential({ name: 'do-prod', version: bad })).toEqual({ name: 'do-prod' });
    }
  });

  it('is planned together with the program, from one parse of one request', () => {
    // A converge that ran the fleet program with some other stack's credential would be a way to
    // provision in an account the caller did not name, so the two come out of one function.
    const plan = planFor({
      stackFqn: 'kontra-fleet/nscheck-0.1.0',
      args: { tag: 'dns', machines: 2, credential: 'do-prod' },
    });
    expect(plan.credential).toEqual({ name: 'do-prod' });
    expect(plan.providerEnvVar).toBe('DIGITALOCEAN_TOKEN');
    expect(plan.program).toBeTypeOf('function');
  });

  it('reads back the way an error and a ledger line spell it', () => {
    expect(credentialLabel({ name: 'do-prod' })).toBe('do-prod');
    expect(credentialLabel({ name: 'do-prod', version: 3 })).toBe('do-prod@3');
  });
});

/**
 * THE START-TIME REFUSAL. ADR 0034 §4: a missing or revoked credential must fail at the start of
 * `fleet.up()`, naming the secret — not thirty lines into a shell script on a Droplet.
 *
 * Every message below is asserted to carry the NAME, because a refusal that says "authentication
 * failed" sends an operator to DigitalOcean's console for a credential that was never read.
 */
describe('a credential that cannot be resolved fails at the start, naming it', () => {
  it('says there is no such secret, and what to do about it', async () => {
    const err = await checkCredential({ name: 'do-prod' }, { store }).catch((e) => e);
    expect(err).toBeInstanceOf(CloudCredentialUnavailable);
    expect(err.message).toContain('do-prod');
    expect(err.message).toContain('PUT /api/secrets/do-prod');
    expect(err.message).toContain('Nothing was provisioned');
  });

  it('distinguishes REVOKED from missing, because the fixes are different', async () => {
    await store.put('do-prod', SENTINEL);
    await store.revoke('do-prod', 1);
    const err = await checkCredential({ name: 'do-prod' }, { store }).catch((e) => e);
    expect(err.message).toContain('revoked');
    expect(err.message).toContain('do-prod');
    // "Write a new version" and "create the secret" send an operator to two different places.
    expect(err.message).not.toContain('no secret by that name');
  });

  it('refuses a pin that does not exist and one that has been revoked', async () => {
    await store.put('do-prod', SENTINEL);
    await store.put('do-prod', ROTATED);
    await expect(checkCredential({ name: 'do-prod', version: 9 }, { store })).rejects.toThrow(/no version 9/);
    await store.revoke('do-prod', 1);
    await expect(checkCredential({ name: 'do-prod', version: 1 }, { store })).rejects.toThrow(/revoked/);
    // …and the un-revoked pin still resolves, which is what makes the two assertions above mean
    // something rather than "everything fails".
    await expect(checkCredential({ name: 'do-prod', version: 2 }, { store })).resolves.toEqual({
      credential: 'do-prod',
      version: 2,
    });
  });

  it('refuses an actor-OWNED secret as a fleet credential', async () => {
    // A fleet credential is the OPERATOR's, resolved at the last hop by the infra worker. An owned
    // secret belongs to its actor and is fetched by it, authenticated as itself — handing it to a
    // provisioner because a caller named it is the ambient authority the store exists to remove.
    await store.put('probe-key', SENTINEL, { owner: 'probe' });
    await expect(checkCredential({ name: 'probe-key' }, { store })).rejects.toThrow(/belongs to actor:probe/);
  });

  it('refuses a NAME that is obviously a TOKEN, and never quotes it back', async () => {
    // THE ONE MISTAKE THE ALPHABET CANNOT CATCH. A DigitalOcean token is `dop_v1_` plus lowercase
    // hex — every character legal in a secret name — so this arrives as a well-formed name. By the
    // time it gets here it is already in workflow history in the clear and no rotation takes it
    // back; what this stops is the mistake being USED, and it says to rotate rather than leaving an
    // operator reading a 404 as a typo.
    const token = 'dop_v1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789';
    expect(looksLikeAToken(token)).toBe(true);
    expect(looksLikeAToken('do-prod')).toBe(false);
    const err = await checkCredential({ name: token }, { store }).catch((e) => e);
    expect(err).toBeInstanceOf(CloudCredentialUnavailable);
    expect(err.message).toContain('Rotate it too');
    // An error is a read path — a log line, an HTTP body, a workflow failure event.
    expect(err.message).not.toContain(token);
    expect(err.message).not.toContain('dop_v1_');
  });

  it('turns a malformed NAME into the same refusal rather than a backend error', async () => {
    const err = await checkCredential({ name: 'DO_TOKEN' }, { store }).catch((e) => e);
    expect(err).toBeInstanceOf(CloudCredentialUnavailable);
    expect(err.message).toContain('not a secret name');
  });

  it('READS METADATA AND NEVER A VALUE — which is what makes it safe to be an activity', async () => {
    // An activity's RESULT is written into workflow history exactly as its arguments are, so the
    // preflight may only ever answer with things that are already there. A name and a version are.
    await store.put('do-prod', SENTINEL);
    const answer = await checkCredential({ name: 'do-prod' }, { store });
    expect(answer).toEqual({ credential: 'do-prod', version: 1 });
    expect(JSON.stringify(answer)).not.toContain(SENTINEL);
  });
});

/**
 * ROTATION, END TO END, WITH NO RESTART — the point of moving the credential at all.
 *
 * "No restart" is the assertion, and it is made the only way it can be made in a unit test: the
 * SAME store object and the SAME resolver function are used before and after the rotation, with
 * nothing re-imported, re-constructed or re-read from the environment in between. The old shape
 * could not pass this test at all — `process.env` is fixed at container start, so the value would
 * have been identical on both sides of the write.
 */
describe('rotation', () => {
  it('uses the new value on the next converge, with nothing restarted or re-read', async () => {
    await store.put('do-prod', SENTINEL);
    const before = await resolveProviderEnv(use({ name: 'do-prod' }), { store, log: ledger });
    expect(before).toEqual({ DIGITALOCEAN_TOKEN: SENTINEL });

    // The rotation. This is `PUT /api/secrets/do-prod` / the Settings surface, and it is all
    // that happens between the two converges: no service recreate, no `.env` edit.
    const { version, rotated } = await store.put('do-prod', ROTATED);
    expect({ version, rotated }).toEqual({ version: 2, rotated: true });

    const after = await resolveProviderEnv(use({ name: 'do-prod' }), { store, log: ledger });
    expect(after).toEqual({ DIGITALOCEAN_TOKEN: ROTATED });
  });

  it('does not break a caller that pinned a version', async () => {
    await store.put('do-prod', SENTINEL);
    await store.put('do-prod', ROTATED);
    const pinned = await resolveProviderEnv(use({ name: 'do-prod', version: 1 }), { store, log: ledger });
    expect(pinned.DIGITALOCEAN_TOKEN).toBe(SENTINEL);
  });

  it('rolls back when the newest version is revoked, which is the "that rotation was wrong" path', async () => {
    await store.put('do-prod', SENTINEL);
    await store.put('do-prod', ROTATED);
    await store.revoke('do-prod', 2);
    const back = await resolveProviderEnv(use({ name: 'do-prod' }), { store, log: ledger });
    expect(back.DIGITALOCEAN_TOKEN).toBe(SENTINEL);
  });

  it('puts the value in the provider variable and NOWHERE else in the engine environment', async () => {
    await store.put('do-prod', SENTINEL);
    const env = await resolveProviderEnv(use({ name: 'do-prod' }), { store, log: ledger });
    expect(Object.keys(env)).toEqual(['DIGITALOCEAN_TOKEN']);
  });
});

/**
 * THE LEDGER. Issue 20's tuple is `(actor, version, slot, run)`, and a fleet credential is filed
 * under a reserved pseudo-slot: it is the operator's own secret read by the infra worker, not a
 * grant to an actor, and one shape for both is what lets "which of my credentials was read, by
 * what, for which run" have a single answer.
 */
describe('every resolution is a ledger line', () => {
  it('records actor, version, slot and run — and never a byte of the value', async () => {
    await store.put('do-prod', SENTINEL);
    await resolveProviderEnv(use({ name: 'do-prod' }), { store, log: ledger });

    const [entry] = ledger.list();
    expect(entry).toMatchObject({
      actor: 'nscheck',
      version: '0.1.0',
      slot: FLEET_CREDENTIAL_SLOT,
      run: 'recon-1786831339',
      secret: 'do-prod',
      secretVersion: 1,
      outcome: 'resolved',
    });
    // The ledger is the file most likely to be shipped to a log aggregator and the one an operator
    // will happily paste into a ticket. Swept as bytes, not as an object, for `history.test.ts`'s
    // reason: a JSON view of a buffer hides what a substring search would find.
    const raw = readFileSync(ledger.location, 'utf8');
    expect(raw).toContain('do-prod');
    expect(raw).not.toContain(SENTINEL);
  });

  it('records the REFUSALS too — the interesting event is the one that handed nothing over', async () => {
    await resolveProviderEnv(use({ name: 'do-prod' }), { store, log: ledger }).catch(() => {});
    expect(ledger.list()[0]).toMatchObject({ secret: 'do-prod', outcome: 'missing', secretVersion: 0 });
    // Filterable by run, so "what did this run read" is one question rather than a grep.
    expect(ledger.list({ run: 'recon-1786831339' })).toHaveLength(1);
    expect(ledger.list({ run: 'some-other-run' })).toHaveLength(0);
  });

  it('names the STACK when a converge places no actor, rather than inventing one', async () => {
    await store.put('do-token', SENTINEL);
    // `kontra fleet up` with no placement. A fleet's name is `<actor>-<version>` by construction,
    // so the stack name is the honest answer and not a guess.
    await resolveProviderEnv(
      { ...use({ name: 'do-token' }), actor: 'nscheck-0.1.0', version: '', run: '' },
      { store, log: ledger }
    );
    expect(ledger.list()[0]).toMatchObject({ actor: 'nscheck-0.1.0', version: '', run: '' });
  });
});

/**
 * THE ONE-TIME MIGRATION off the environment variable.
 *
 * An installation upgrading into this change has its token in a `.env` on a controller and nothing
 * in the store, and its next `fleet DOWN` would otherwise fail on a credential nobody had created —
 * leaving Droplets billing. What must not happen is the opposite: a stale `.env` quietly putting
 * last month's value back over a rotation.
 */
describe('adopting a legacy DIGITALOCEAN_TOKEN', () => {
  it('imports it once, under this control plane\'s credential name', async () => {
    const lines: string[] = [];
    const got = await adoptLegacyCloudToken({
      store,
      env: { DIGITALOCEAN_TOKEN: SENTINEL },
      onLog: (l) => lines.push(l),
    });
    expect(got).toMatchObject({ adopted: true, name: 'do-token' });
    // LOUD, because after this the variable is not read: an operator who edits it and restarts
    // would otherwise see no change and no explanation.
    expect(lines.join('\n')).toContain('NO LONGER READ');
    expect(lines.join('\n')).toContain('PUT /api/secrets/do-token');
    expect(lines.join('\n')).not.toContain(SENTINEL);
    await expect(resolveProviderEnv(use({ name: 'do-token' }), { store, log: ledger }))
      .resolves.toEqual({ DIGITALOCEAN_TOKEN: SENTINEL });
  });

  it('NEVER overwrites — a rotation is not undone by the next restart', async () => {
    await store.put('do-token', ROTATED);
    const got = await adoptLegacyCloudToken({
      store,
      env: { DIGITALOCEAN_TOKEN: SENTINEL },
      onLog: () => {},
    });
    expect(got.adopted).toBe(false);
    const env = await resolveProviderEnv(use({ name: 'do-token' }), { store, log: ledger });
    expect(env.DIGITALOCEAN_TOKEN).toBe(ROTATED);
  });

  it('does nothing, quietly, when there is no variable to adopt', async () => {
    const lines: string[] = [];
    const got = await adoptLegacyCloudToken({ store, env: {}, onLog: (l) => lines.push(l) });
    expect(got.adopted).toBe(false);
    expect(lines).toEqual([]);
  });
});

/**
 * THE SOURCE GUARD. `engineEnv()` used to read `process.env.DIGITALOCEAN_TOKEN`, and "the cloud
 * token lives only in orchestrator-infra" was a fact about which process held a variable rather
 * than about what a type carries. A future refactor that "restores" that read would put the value
 * back on a path that survives a restart and cannot be rotated — silently, because every test above
 * would still pass.
 */
describe('the environment variable is not read any more', () => {
  const read = (rel: string): string => readFileSync(path.join(__dirname, rel), 'utf8');

  it('is read in exactly one place, and that place is the one-time migration', () => {
    for (const file of ['workspace.ts', 'stacks.ts', 'programs/fleet.ts', 'programs/machine.ts']) {
      expect(read(file), file).not.toMatch(/process\.env\.DIGITALOCEAN_TOKEN|env\.DIGITALOCEAN_TOKEN/);
    }
    // `credential.ts` names it once, in `adoptLegacyCloudToken`, and that function writes it INTO
    // the store rather than handing it to a converge.
    const cred = read('credential.ts');
    expect(cred.match(/env\.DIGITALOCEAN_TOKEN/g) ?? []).toHaveLength(1);
    expect(cred.slice(cred.indexOf('export async function adoptLegacyCloudToken')))
      .toContain('env.DIGITALOCEAN_TOKEN');
  });

  it('leaves the engine environment credential-free when nobody passes one', () => {
    // `state.ts` and the Dashboard read a checkpoint off disk and need no credential at all. Before
    // this change they would have been handed one anyway, from the process environment.
    const prev = process.env.DIGITALOCEAN_TOKEN;
    process.env.DIGITALOCEAN_TOKEN = SENTINEL;
    try {
      expect(Object.keys(engineEnv())).not.toContain('DIGITALOCEAN_TOKEN');
      expect(JSON.stringify(engineEnv())).not.toContain(SENTINEL);
    } finally {
      if (prev === undefined) delete process.env.DIGITALOCEAN_TOKEN;
      else process.env.DIGITALOCEAN_TOKEN = prev;
    }
  });
});
