/**
 * The local encrypted-file backend, against a real directory.
 *
 * THE ASSERTIONS THAT MATTER MOST ARE NEGATIVE ONES: the sentinel value must not be in the store
 * file, must not be in any metadata a read path can reach, and must not survive a revocation. A
 * store that keeps a credential readable is not a partial success, so each of those is its own
 * test with its own name — a failure has to say WHICH property broke.
 */

import { mkdtempSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { FileSecretBackend } from './fileBackend';
import { keyFile } from './keyring';
import { NoSuchSecret, SecretRevoked } from './types';

/** The one string this suite hunts for. Distinctive enough that a substring match is proof. */
const SENTINEL = 'dop_v1_SENTINEL_never_in_the_clear_9f3c';

let dir: string;
let backend: FileSecretBackend;
let savedKey: string | undefined;

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), 'kontra-secrets-'));
  // An injected key in the ambient environment would skip the keyfile entirely — and the keyfile's
  // mode is one of the things asserted here.
  savedKey = process.env.KONTRA_SECRETS_KEY;
  delete process.env.KONTRA_SECRETS_KEY;
  backend = new FileSecretBackend({ dir });
});

afterEach(() => {
  if (savedKey === undefined) delete process.env.KONTRA_SECRETS_KEY;
  else process.env.KONTRA_SECRETS_KEY = savedKey;
});

const storeBytes = (): string => readFileSync(join(dir, 'secrets.json'), 'utf8');

describe('creating, naming and versioning', () => {
  it('creates version 1 and resolves it', async () => {
    const meta = await backend.put('do-token', SENTINEL);
    expect(meta.name).toBe('do-token');
    expect(meta.versions.map((v) => v.version)).toEqual([1]);
    expect(meta.current).toBe(1);
    expect(await backend.resolve({ name: 'do-token' })).toBe(SENTINEL);
  });

  it('rotates: a second write is version 2, and version 1 still resolves', async () => {
    await backend.put('do-token', 'first-value');
    const meta = await backend.put('do-token', SENTINEL);
    expect(meta.versions.map((v) => v.version)).toEqual([1, 2]);
    expect(meta.current).toBe(2);
    // The grace window rotation needs: work in flight against the old credential keeps working
    // until the operator revokes it.
    expect(await backend.resolve({ name: 'do-token', version: 1 })).toBe('first-value');
    expect(await backend.resolve({ name: 'do-token' })).toBe(SENTINEL);
  });

  it('does not lose a version when two writes race', async () => {
    // Read-modify-write on one file: without the mutation queue, the loser of the race overwrites
    // the winner and one rotation vanishes with no error anywhere.
    await Promise.all([1, 2, 3, 4, 5].map((n) => backend.put('do-token', `v${n}`)));
    const meta = await backend.describe('do-token');
    expect(meta?.versions.map((v) => v.version)).toEqual([1, 2, 3, 4, 5]);
  });

  it('keeps two secrets apart', async () => {
    await backend.put('a-key', 'alpha');
    await backend.put('b-key', 'beta');
    expect((await backend.list()).map((s) => s.name)).toEqual(['a-key', 'b-key']);
    expect(await backend.resolve({ name: 'a-key' })).toBe('alpha');
  });
});

describe('the value is write-only', () => {
  it('is not in the store file — it is encrypted at rest', async () => {
    await backend.put('do-token', SENTINEL);
    expect(storeBytes()).not.toContain(SENTINEL);
    // …and the file really does hold the secret's bookkeeping, so the assertion above is about
    // encryption rather than about having looked at the wrong file.
    expect(storeBytes()).toContain('do-token');
  });

  it('is in nothing any read path returns', async () => {
    await backend.put('do-token', SENTINEL);
    const readPaths = JSON.stringify([await backend.list(), await backend.describe('do-token')]);
    expect(readPaths).not.toContain(SENTINEL);
    // Not even a fragment of it: a prefix or suffix would be a partial disclosure, which is the
    // shape a "just show the last four characters" affordance takes.
    expect(readPaths).not.toContain(SENTINEL.slice(0, 12));
  });

  it('is not in the error raised for a version that cannot be decrypted', async () => {
    await backend.put('do-token', SENTINEL);
    // Corrupt the tag: a decryption failure is the one place a naive implementation echoes bytes.
    const file = join(dir, 'secrets.json');
    const raw = JSON.parse(readFileSync(file, 'utf8')) as {
      secrets: Record<string, { versions: Array<{ tag: string }> }>;
    };
    raw.secrets['do-token']!.versions[0]!.tag = Buffer.alloc(16).toString('base64');
    writeFileSync(file, JSON.stringify(raw));
    await expect(backend.resolve({ name: 'do-token' })).rejects.toThrow(/cannot be decrypted/);
    await expect(backend.resolve({ name: 'do-token' })).rejects.not.toThrow(new RegExp(SENTINEL));
  });
});

describe('a ciphertext is bound to its own slot', () => {
  it('refuses bytes moved from another secret', async () => {
    await backend.put('a-key', SENTINEL);
    await backend.put('b-key', 'beta');
    const file = join(dir, 'secrets.json');
    const raw = JSON.parse(readFileSync(file, 'utf8')) as {
      secrets: Record<string, { versions: Array<{ iv: string; ct: string; tag: string }> }>;
    };
    // Paste a-key's sealed bytes over b-key's. Without the AAD this decrypts cleanly and `b-key`
    // silently becomes a copy of `a-key` — a substitution nobody can see from any surface.
    raw.secrets['b-key']!.versions[0] = { ...raw.secrets['a-key']!.versions[0]! };
    writeFileSync(file, JSON.stringify(raw));
    await expect(backend.resolve({ name: 'b-key' })).rejects.toThrow(/cannot be decrypted/);
  });

  it('refuses bytes moved from another version of the same secret', async () => {
    await backend.put('do-token', 'first-value');
    await backend.put('do-token', SENTINEL);
    const file = join(dir, 'secrets.json');
    const raw = JSON.parse(readFileSync(file, 'utf8')) as {
      secrets: Record<string, { versions: Array<{ version: number; iv: string; ct: string; tag: string }> }>;
    };
    const v1 = raw.secrets['do-token']!.versions[0]!;
    raw.secrets['do-token']!.versions[1] = { ...raw.secrets['do-token']!.versions[1]!, iv: v1.iv, ct: v1.ct, tag: v1.tag };
    writeFileSync(file, JSON.stringify(raw));
    // Rolling a rotation back by editing the file is exactly the attack this stops.
    await expect(backend.resolve({ name: 'do-token' })).rejects.toThrow(/cannot be decrypted/);
  });
});

describe('revocation', () => {
  it('makes the version unusable and destroys its ciphertext', async () => {
    await backend.put('do-token', SENTINEL);
    const before = storeBytes();
    const meta = await backend.revoke('do-token', 1);
    expect(meta.versions[0]?.revokedAt).toBeGreaterThan(0);
    expect(meta.current).toBeUndefined();
    await expect(backend.resolve({ name: 'do-token', version: 1 })).rejects.toThrow(SecretRevoked);
    // THE BYTES ARE GONE, not flagged: a revocation has to survive a stolen key and a restored
    // backup, and `revoked: true` beside a ciphertext survives neither.
    const sealed = JSON.parse(before).secrets['do-token'].versions[0].ct as string;
    expect(before).toContain(sealed);
    expect(storeBytes()).not.toContain(sealed);
  });

  it('rolls back to the previous live version when the newest is revoked', async () => {
    await backend.put('do-token', 'first-value');
    await backend.put('do-token', SENTINEL);
    const meta = await backend.revoke('do-token', 2);
    // The "that rotation was wrong" recovery: the secret still resolves, to what it was before.
    expect(meta.current).toBe(1);
    expect(await backend.resolve({ name: 'do-token' })).toBe('first-value');
  });

  it('leaves a secret with no value at all when every version is revoked', async () => {
    await backend.put('do-token', 'first-value');
    await backend.put('do-token', SENTINEL);
    await backend.revoke('do-token', 2);
    await backend.revoke('do-token', 1);
    // NOT an older version, and not silence: resolution fails and says why.
    await expect(backend.resolve({ name: 'do-token' })).rejects.toThrow(/every version/);
  });

  it('is idempotent, and says which version it cannot find', async () => {
    await backend.put('do-token', SENTINEL);
    await backend.revoke('do-token', 1);
    const again = await backend.revoke('do-token', 1);
    expect(again.versions[0]?.revokedAt).toBeGreaterThan(0);
    await expect(backend.revoke('do-token', 7)).rejects.toThrow(NoSuchSecret);
    await expect(backend.revoke('nothing-here', 1)).rejects.toThrow(NoSuchSecret);
  });
});

describe('destroying', () => {
  it('forgets the secret and every version of it', async () => {
    await backend.put('do-token', SENTINEL);
    await backend.put('do-token', `${SENTINEL}-2`);
    expect(await backend.destroy('do-token')).toBe(true);
    expect(await backend.describe('do-token')).toBeUndefined();
    expect(storeBytes()).not.toContain('do-token');
    expect(await backend.destroy('do-token')).toBe(false);
  });
});

describe('the key file', () => {
  it('is minted 0600 in a 0700 directory on first write', async () => {
    await backend.put('do-token', SENTINEL);
    expect(statSync(keyFile(dir)).mode & 0o777).toBe(0o600);
    expect(statSync(dir).mode & 0o777).toBe(0o700);
    expect(statSync(join(dir, 'secrets.json')).mode & 0o777).toBe(0o600);
  });

  it('is refused when its mode is wider than its owner', async () => {
    await backend.put('do-token', SENTINEL);
    const fresh = new FileSecretBackend({ dir });
    require('node:fs').chmodSync(keyFile(dir), 0o644);
    // REFUSED, NOT REPAIRED. A key that has been world-readable is a fact about the machine
    // somebody has to see; a silent chmod hides that the bytes were already exposed.
    await expect(fresh.resolve({ name: 'do-token' })).rejects.toThrow(/readable beyond its owner/);
  });

  it('is not written at all when a key is injected', async () => {
    const injected = mkdtempSync(join(tmpdir(), 'kontra-secrets-'));
    process.env.KONTRA_SECRETS_KEY = Buffer.alloc(32, 7).toString('base64');
    const b = new FileSecretBackend({ dir: injected });
    await b.put('do-token', SENTINEL);
    expect(() => statSync(keyFile(injected))).toThrow();
    expect(await b.resolve({ name: 'do-token' })).toBe(SENTINEL);
  });
});

describe('a store that survives a restart', () => {
  it('resolves what a previous process wrote', async () => {
    await backend.put('do-token', SENTINEL);
    // A second backend over the same directory is what a restarted orchestrator is.
    expect(await new FileSecretBackend({ dir }).resolve({ name: 'do-token' })).toBe(SENTINEL);
  });

  it('refuses a store file whose schema it does not know', async () => {
    await backend.put('do-token', SENTINEL);
    writeFileSync(join(dir, 'secrets.json'), JSON.stringify({ schema: 'kontra.secrets.v9', secrets: {} }));
    await expect(new FileSecretBackend({ dir }).list()).rejects.toThrow(/refusing to read/);
  });
});
