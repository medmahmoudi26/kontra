/**
 * Where the local backend's key material comes from, and what it is honestly worth.
 *
 * ONE MASTER KEY, NEVER USED DIRECTLY. Everything that needs a key derives a purpose-scoped subkey
 * from it with HKDF ({@link subkey}), so the key that encrypts a stored value and the key that
 * signs an actor's identity token are different bytes with different labels. A single key used for
 * two jobs is how a signing oracle turns into a decryption oracle, and the fix costs one line at
 * each call site.
 *
 * WHAT THIS PROTECTS AGAINST, said plainly because a secret store that oversells itself is worse
 * than none: a keyfile beside the ciphertext defends the STORE FILE — a copy that leaves the
 * machine in a backup, a volume snapshot, a blob-store sync, a `scp` of `~/.kontra` — and it does
 * not defend against somebody who already has root on the box, who can read the keyfile too. That
 * is the install's threat model (ADR 0031 §3: the whole security posture is that there is
 * nothing remote to reach), and it is why the backend is an INTERFACE: a KMS or vault
 * implementation moves the key off the machine without changing one caller.
 *
 * `KONTRA_SECRETS_KEY` is the escape hatch for a deployment that already has a way to inject key
 * material (compose secret, systemd credential). Set it and no keyfile is written at all.
 */

import { hkdfSync, randomBytes } from 'node:crypto';
import { chmodSync, existsSync, mkdirSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import path from 'node:path';

import { kontraHome } from '../sources';

/** 32 bytes: AES-256 below, and HMAC-SHA256 in `identity.ts`. */
const KEY_BYTES = 32;

/** Where the store's file and keyfile live. `~/.kontra/secrets` by default, so it sits with the
 *  rest of an operator's state and moves with `KONTRA_HOME` the way everything else does. */
export function secretsDir(): string {
  return process.env.KONTRA_SECRETS_DIR || path.join(kontraHome(), 'secrets');
}

/** The keyfile inside a store directory. Named `.master.key` rather than `master.key` for the one
 *  reason that matters: a glob that copies the store somewhere is usually written by a human. */
export function keyFile(dir: string): string {
  return path.join(dir, '.master.key');
}

/**
 * The master key for a store directory: the injected one, or the keyfile, minting it on first use.
 *
 * MINTING ON FIRST USE IS THE DEFAULT AND IT IS NOT A SILENT ONE — the first write to a fresh
 * install has to work without an operator having generated key material first, or the store
 * ships disabled and credentials stay in `.env`, which is the failure this whole slice exists to
 * end. It is written 0600 in a 0700 directory, and a keyfile whose mode is wider than that is
 * REFUSED rather than repaired: a world-readable key is a fact about the machine somebody has to
 * see, and quietly chmod-ing it would hide that the bytes were already exposed.
 */
export function masterKey(dir: string = secretsDir()): Buffer {
  const injected = process.env.KONTRA_SECRETS_KEY;
  if (injected) return decodeKey(injected);

  const file = keyFile(dir);
  if (existsSync(file)) {
    const mode = statSync(file).mode & 0o777;
    if (mode & 0o077) {
      throw new Error(
        `refusing to use ${file}: mode ${mode.toString(8)} is readable beyond its owner. ` +
          'Move the store, mint a new key, and rotate every secret in it — the old bytes have been readable.'
      );
    }
    const key = readFileSync(file);
    if (key.length !== KEY_BYTES) {
      throw new Error(`${file} is ${key.length} bytes, not ${KEY_BYTES} — this is not a kontra secrets key`);
    }
    return key;
  }

  mkdirSync(dir, { recursive: true, mode: 0o700 });
  const key = randomBytes(KEY_BYTES);
  // `wx` so two processes racing to create the store cannot both win and leave one of them
  // encrypting under a key the file no longer holds — the loser re-reads what the winner wrote.
  try {
    writeFileSync(file, key, { mode: 0o600, flag: 'wx' });
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'EEXIST') return masterKey(dir);
    throw err;
  }
  chmodSync(file, 0o600);
  return key;
}

/** Accept base64 or hex for an injected key, because both are what a human pastes. */
function decodeKey(raw: string): Buffer {
  const text = raw.trim();
  const buf = /^[0-9a-fA-F]{64}$/.test(text) ? Buffer.from(text, 'hex') : Buffer.from(text, 'base64');
  if (buf.length !== KEY_BYTES) {
    throw new Error(
      `KONTRA_SECRETS_KEY must decode to ${KEY_BYTES} bytes (got ${buf.length}) — ` +
        "generate one with `openssl rand -base64 32`"
    );
  }
  return buf;
}

/**
 * A purpose-scoped subkey. The `purpose` string is a CONTRACT: changing one makes every artefact
 * derived under the old label undecryptable/unverifiable, so they carry a version suffix and are
 * appended to, never edited.
 */
export function subkey(master: Buffer, purpose: string): Buffer {
  return Buffer.from(hkdfSync('sha256', master, Buffer.from('kontra.secrets.v1'), purpose, KEY_BYTES));
}
