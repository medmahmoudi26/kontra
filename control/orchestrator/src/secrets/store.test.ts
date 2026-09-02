/**
 * The rules that hold whatever backend is underneath: what a name and a value may be, and WHO may
 * resolve what.
 *
 * OWNERSHIP IS TESTED IN BOTH DIRECTIONS, which is the point of the suite. An actor must not reach
 * an operator's credential, and a worker holding the store must not reach an actor's — the second
 * one is the ambient authority that a store built only against the first would quietly keep.
 */

import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { beforeEach, describe, expect, it } from 'vitest';

import { FileSecretBackend } from './fileBackend';
import { LAST_HOP, SecretStore } from './store';
import { SecretForbidden, SecretRefused, actorIdentity } from './types';

const SENTINEL = 'dop_v1_SENTINEL_never_in_the_clear_9f3c';
const PROBE = actorIdentity('probe');

let store: SecretStore;

beforeEach(() => {
  process.env.KONTRA_SECRETS_KEY = Buffer.alloc(32, 3).toString('base64');
  store = new SecretStore(new FileSecretBackend({ dir: mkdtempSync(join(tmpdir(), 'kontra-secrets-')) }));
});

describe('what a name may be', () => {
  it('takes an ordinary lowercase name', async () => {
    const written = await store.put('do-token', SENTINEL);
    expect(written.version).toBe(1);
    expect(written.rotated).toBe(false);
  });

  it('refuses a name that would make two secrets look like one', async () => {
    // `DO_TOKEN` and `do-token` being different secrets is a mistake somebody makes at 3am with a
    // production credential.
    await expect(store.put('DO_TOKEN', SENTINEL)).rejects.toThrow(SecretRefused);
    await expect(store.put('-leading', SENTINEL)).rejects.toThrow(SecretRefused);
    await expect(store.put('has spaces', SENTINEL)).rejects.toThrow(SecretRefused);
    await expect(store.put('../escape', SENTINEL)).rejects.toThrow(SecretRefused);
    await expect(store.put('x'.repeat(65), SENTINEL)).rejects.toThrow(SecretRefused);
  });
});

describe('what a value may be', () => {
  it('trims it, because a pasted token carries a newline about half the time', async () => {
    await store.put('do-token', `  ${SENTINEL}\n`);
    expect((await store.resolve({ name: 'do-token' }, LAST_HOP)).value).toBe(SENTINEL);
  });

  it('refuses an empty one, and a value past the size bound', async () => {
    await expect(store.put('do-token', '   \n ')).rejects.toThrow(/cannot be empty/);
    await expect(store.put('do-token', 'x'.repeat(64 * 1024 + 1))).rejects.toThrow(/at most/);
  });

  it('never quotes the value in a refusal', async () => {
    // An error message is a read path: it reaches a log line, an HTTP body and a browser console.
    const err = await store.put('do-token', `${SENTINEL}${'x'.repeat(64 * 1024)}`).catch((e: Error) => e);
    expect(String(err)).not.toContain(SENTINEL);
  });
});

describe('rotation', () => {
  it('is the second write, and it says which version it became', async () => {
    await store.put('do-token', 'first-value');
    const rotated = await store.put('do-token', SENTINEL);
    expect(rotated).toMatchObject({ version: 2, rotated: true });
    expect(rotated.secret.current).toBe(2);
  });

  it('cannot move ownership', async () => {
    await store.put('shodan-key', 'first-value', { owner: 'probe' });
    // A write that looks like a routine rotation must not be a way to hand somebody else's
    // credential to a different actor.
    await expect(store.put('shodan-key', SENTINEL, { owner: 'evil' })).rejects.toThrow(/already belongs to/);
    expect((await store.describe('shodan-key'))?.owner).toBe(PROBE);
  });
});

describe('who may resolve what', () => {
  it('lets the last hop resolve an operator secret', async () => {
    await store.put('do-token', SENTINEL);
    expect((await store.resolve({ name: 'do-token' }, LAST_HOP)).value).toBe(SENTINEL);
  });

  it('refuses an operator secret to an actor, however well authenticated', async () => {
    await store.put('do-token', SENTINEL);
    // The credential a workflow names is resolved at the last hop by a worker. There is no HTTP
    // door to it at all — which is what makes "only the name crosses" true of that path.
    await expect(store.resolve({ name: 'do-token' }, { kind: 'actor', identity: PROBE })).rejects.toThrow(
      SecretForbidden
    );
  });

  it("lets an actor resolve its own, and only its own", async () => {
    await store.put('shodan-key', SENTINEL, { owner: 'probe' });
    expect((await store.resolve({ name: 'shodan-key' }, { kind: 'actor', identity: PROBE })).value).toBe(SENTINEL);
    await expect(
      store.resolve({ name: 'shodan-key' }, { kind: 'actor', identity: actorIdentity('other') })
    ).rejects.toThrow(SecretForbidden);
  });

  it('refuses an actor-owned secret to a worker that happens to hold the store', async () => {
    await store.put('shodan-key', SENTINEL, { owner: 'probe' });
    // The direction a store built only against the obvious threat keeps: any activity in this
    // process could otherwise read every actor's credential because it has the file open.
    await expect(store.resolve({ name: 'shodan-key' }, LAST_HOP)).rejects.toThrow(SecretForbidden);
  });

  it('tells an actor nothing about a secret that is not its own', async () => {
    await store.put('shodan-key', SENTINEL, { owner: 'probe' });
    const err = await store
      .resolve({ name: 'shodan-key' }, { kind: 'actor', identity: actorIdentity('other') })
      .catch((e: Error) => e);
    // Not the owner, not the versions, not whether it has ever been rotated.
    expect(String(err)).not.toContain('probe');
    expect(String(err)).not.toContain(SENTINEL);
  });
});

describe('the value is never returned by a read path', () => {
  it('is in nothing `list`, `describe` or a write returns', async () => {
    const written = await store.put('do-token', SENTINEL, {});
    await store.put('shodan-key', SENTINEL, { owner: 'probe' });
    const everything = JSON.stringify([written, await store.list(), await store.describe('do-token')]);
    expect(everything).not.toContain(SENTINEL);
    expect(everything).not.toContain(SENTINEL.slice(0, 12));
    // …and the metadata really is populated, so the negative above is not vacuous.
    expect(everything).toContain('do-token');
    expect(everything).toContain(PROBE);
  });
});

describe('revocation', () => {
  it('makes a version unusable and is idempotent', async () => {
    await store.put('do-token', SENTINEL);
    const meta = await store.revoke('do-token', 1);
    expect(meta.current).toBeUndefined();
    await expect(store.resolve({ name: 'do-token' }, LAST_HOP)).rejects.toThrow(/every version/);
    // A second click during an incident is not a failure.
    await expect(store.revoke('do-token', 1)).resolves.toBeTruthy();
  });

  it('refuses a version that is not a version', async () => {
    await store.put('do-token', SENTINEL);
    expect(() => store.revoke('do-token', 0)).toThrow(SecretRefused);
    expect(() => store.revoke('do-token', 1.5)).toThrow(SecretRefused);
  });
});
