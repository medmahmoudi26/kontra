/**
 * The local encrypted-file backend — the one that ships with the install.
 *
 * ONE JSON FILE, AES-256-GCM PER VERSION. A single file rather than a file per secret, and that is
 * a security choice before it is a convenience: a name never becomes a path, so no name can
 * traverse out of the store directory, and a listing does not leak which credentials exist to
 * anything that can `ls` but not read.
 *
 * THE AAD BINDS A CIPHERTEXT TO ITS SLOT. Each version is sealed with additional data of
 * `<name>\0<version>`, so bytes lifted from one secret and pasted into another — or an old version
 * pasted over a rotated one — fail authentication instead of decrypting into the wrong credential.
 * An attacker who can edit the store file but not read the key can therefore destroy, and cannot
 * substitute.
 *
 * REVOKING DESTROYS THE CIPHERTEXT. It does not mark it and keep it. A revocation exists because a
 * credential leaked or a rotation was wrong, and the failure a secret store is built to prevent is
 * the one where recovery is impossible — so "unusable" is made true of the BYTES, not only of the
 * bookkeeping. It survives a stolen key, a restored backup and a future bug in the version-picking
 * code, none of which a `revoked: true` flag survives.
 *
 * WRITES ARE ATOMIC AND SERIALISED. Temp file plus rename, so a crash mid-write leaves the old
 * store intact rather than a truncated one holding half an operator's credentials; and every
 * mutation queues behind the last, because read-modify-write on one file from two concurrent
 * requests otherwise loses whichever rotation lost the race.
 */

import { createCipheriv, createDecipheriv, randomBytes, randomUUID } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from 'node:fs';
import path from 'node:path';

import { masterKey, secretsDir, subkey } from './keyring';
import {
  NoSuchSecret,
  SecretRevoked,
  type SecretBackend,
  type SecretMeta,
  type SecretPutOptions,
  type SecretRef,
  type SecretVersionMeta,
} from './types';

/** HKDF label for the value-encryption subkey. Appended to, never edited — see `keyring.ts`. */
const VALUE_KEY_PURPOSE = 'secret-value-encryption.v1';

/** GCM's nonce size. 12 bytes is the one length AES-GCM is specified for. */
const IV_BYTES = 12;

/** What the file says it is, so a future format change can be recognised rather than guessed at. */
const SCHEMA = 'kontra.secrets.v1';

/** A version as it rests on disk: metadata, plus the sealed bytes — or nothing, once revoked. */
interface StoredVersion extends SecretVersionMeta {
  iv?: string;
  ct?: string;
  tag?: string;
}

interface StoredSecret {
  name: string;
  owner?: string;
  createdAt: number;
  updatedAt: number;
  versions: StoredVersion[];
}

interface StoreFile {
  schema: string;
  secrets: Record<string, StoredSecret>;
}

export interface FileSecretBackendOptions {
  /** The store directory. Defaults to `KONTRA_SECRETS_DIR` or `~/.kontra/secrets`. */
  dir?: string;
  /** Injected clock, so a test can pin `createdAt`/`revokedAt`. */
  now?: () => number;
}

export class FileSecretBackend implements SecretBackend {
  readonly kind = 'file';
  private readonly dir: string;
  private readonly file: string;
  private readonly now: () => number;
  /** Every mutation queues behind the last — see the header. Reads do not need it. */
  private chain: Promise<unknown> = Promise.resolve();
  private cachedKey: Buffer | null = null;

  constructor(opts: FileSecretBackendOptions = {}) {
    this.dir = opts.dir ?? secretsDir();
    this.file = path.join(this.dir, 'secrets.json');
    this.now = opts.now ?? Date.now;
  }

  /** Where the store rests. Settings shows it, because "where are my credentials" is the first
   *  question an operator has about a store they cannot read back. */
  get location(): string {
    return this.file;
  }

  async list(): Promise<SecretMeta[]> {
    return Object.values(this.read().secrets)
      .map(project)
      .sort((a, b) => a.name.localeCompare(b.name));
  }

  async describe(name: string): Promise<SecretMeta | undefined> {
    const found = this.read().secrets[name];
    return found ? project(found) : undefined;
  }

  /**
   * Create the secret, or add a version to it. THE SECOND CALL IS A ROTATION — there is no separate
   * verb, because "set this secret" and "rotate this secret" are the same act with different
   * histories behind them, and a store where they were different calls is a store where somebody
   * eventually overwrites a version in place.
   *
   * OLD VERSIONS STAY USABLE UNTIL REVOKED. That is the grace window rotation needs: a run that is
   * mid-flight against the old credential does not fail the moment a new one is written, and the
   * operator revokes the old version once the provider has accepted the new one. Revoking is a
   * separate act precisely so that it is a decision.
   */
  put(name: string, value: string, opts: SecretPutOptions = {}): Promise<SecretMeta> {
    return this.mutate((file) => {
      const at = this.now();
      const existing = file.secrets[name];
      const version = existing ? highest(existing.versions) + 1 : 1;
      const sealed = this.seal(name, version, value);
      const next: StoredSecret = existing
        ? { ...existing, updatedAt: at, versions: [...existing.versions, { version, createdAt: at, ...sealed }] }
        : {
            name,
            // OWNERSHIP IS SET AT CREATION AND NEVER MOVES. Re-assigning an owner on a rotation
            // would be a way to hand somebody else's credential to a different actor with a write
            // that looks like a routine update — and the operator who wanted that can destroy the
            // secret and create it again, which is visible in the versions.
            ...(opts.owner ? { owner: opts.owner } : {}),
            createdAt: at,
            updatedAt: at,
            versions: [{ version, createdAt: at, ...sealed }],
          };
      file.secrets[name] = next;
      return project(next);
    });
  }

  revoke(name: string, version: number): Promise<SecretMeta> {
    return this.mutate((file) => {
      const secret = file.secrets[name];
      if (!secret) throw new NoSuchSecret(`no secret named ${JSON.stringify(name)}`);
      const found = secret.versions.find((v) => v.version === version);
      if (!found) throw new NoSuchSecret(`secret ${JSON.stringify(name)} has no version ${version}`);
      if (found.revokedAt) return project(secret);
      // The bytes go. See the header: `revoked: true` beside a ciphertext is a note, not a
      // revocation.
      delete found.iv;
      delete found.ct;
      delete found.tag;
      found.revokedAt = this.now();
      secret.updatedAt = found.revokedAt;
      return project(secret);
    });
  }

  destroy(name: string): Promise<boolean> {
    return this.mutate((file) => {
      if (!file.secrets[name]) return false;
      delete file.secrets[name];
      return true;
    });
  }

  /** THE ONLY METHOD HERE THAT RETURNS A VALUE — see `types.ts:SecretBackend.resolve`. */
  async resolve(ref: SecretRef): Promise<string> {
    const secret = this.read().secrets[ref.name];
    if (!secret) throw new NoSuchSecret(`no secret named ${JSON.stringify(ref.name)}`);
    const meta = project(secret);
    const version = ref.version ?? meta.current;
    if (version === undefined) {
      throw new SecretRevoked(
        `every version of ${JSON.stringify(ref.name)} has been revoked — write a new one to make it usable again`
      );
    }
    const found = secret.versions.find((v) => v.version === version);
    if (!found) throw new NoSuchSecret(`secret ${JSON.stringify(ref.name)} has no version ${version}`);
    if (found.revokedAt || !found.ct) {
      throw new SecretRevoked(`version ${version} of ${JSON.stringify(ref.name)} was revoked`);
    }
    return this.open(ref.name, version, found);
  }

  // --- bytes ---------------------------------------------------------------------------------

  private key(): Buffer {
    // Read once per process: minting or reading the keyfile on every resolve would put the key
    // through the filesystem on the hot path of every credentialled call.
    if (!this.cachedKey) this.cachedKey = subkey(masterKey(this.dir), VALUE_KEY_PURPOSE);
    return this.cachedKey;
  }

  private seal(name: string, version: number, value: string): { iv: string; ct: string; tag: string } {
    const iv = randomBytes(IV_BYTES);
    const cipher = createCipheriv('aes-256-gcm', this.key(), iv, { authTagLength: 16 });
    cipher.setAAD(aad(name, version));
    const ct = Buffer.concat([cipher.update(value, 'utf8'), cipher.final()]);
    return { iv: iv.toString('base64'), ct: ct.toString('base64'), tag: cipher.getAuthTag().toString('base64') };
  }

  private open(name: string, version: number, v: StoredVersion): string {
    // THE KEY IS LOADED OUTSIDE THE TRY, and that is not tidiness. It was inside, and the catch
    // below turned every key-loading failure — a keyfile whose mode had gone world-readable, an
    // injected key of the wrong length — into "this cannot be decrypted, the store file was
    // edited". MEASURED by the mode test: an operator would have gone looking for a corrupted
    // store, having been told nothing about the key that is the actual problem.
    const key = this.key();
    try {
      const decipher = createDecipheriv('aes-256-gcm', key, Buffer.from(v.iv ?? '', 'base64'), {
        authTagLength: 16,
      });
      decipher.setAAD(aad(name, version));
      decipher.setAuthTag(Buffer.from(v.tag ?? '', 'base64'));
      return Buffer.concat([decipher.update(Buffer.from(v.ct ?? '', 'base64')), decipher.final()]).toString('utf8');
    } catch {
      // The reason is never in the message and never in a log line: a decryption error whose text
      // varied with the bytes would be an oracle. What an operator can act on is the two things
      // that cause this, and both are named.
      throw new Error(
        `version ${version} of ${JSON.stringify(name)} cannot be decrypted — the store file was ` +
          'edited, or this is not the key it was written with. Write the secret again to replace it.'
      );
    }
  }

  // --- the file ------------------------------------------------------------------------------

  private read(): StoreFile {
    if (!existsSync(this.file)) return { schema: SCHEMA, secrets: {} };
    const parsed = JSON.parse(readFileSync(this.file, 'utf8')) as StoreFile;
    if (parsed.schema !== SCHEMA) {
      throw new Error(`${this.file} is ${JSON.stringify(parsed.schema)}, not ${SCHEMA} — refusing to read it`);
    }
    return { schema: SCHEMA, secrets: parsed.secrets ?? {} };
  }

  private write(file: StoreFile): void {
    mkdirSync(this.dir, { recursive: true, mode: 0o700 });
    const tmp = `${this.file}.${randomUUID()}.tmp`;
    try {
      writeFileSync(tmp, JSON.stringify(file, null, 2), { mode: 0o600 });
      renameSync(tmp, this.file);
    } catch (err) {
      try {
        if (existsSync(tmp)) unlinkSync(tmp);
      } catch {
        /* the temp file is best-effort cleanup; the throw below is the failure that matters */
      }
      throw err;
    }
  }

  /** Read-modify-write, one at a time. The callback is synchronous on purpose: nothing that can
   *  await belongs between the read and the write of a file this holds. */
  private mutate<T>(fn: (file: StoreFile) => T): Promise<T> {
    const run = this.chain.then(() => {
      const file = this.read();
      const out = fn(file);
      this.write(file);
      return out;
    });
    // The chain must not break on a rejection, or one bad write wedges every later one.
    this.chain = run.catch(() => undefined);
    return run;
  }
}

/** `<name>\0<version>` — the additional data every version is sealed under. */
function aad(name: string, version: number): Buffer {
  return Buffer.from(`${name}\0${version}`, 'utf8');
}

function highest(versions: readonly SecretVersionMeta[]): number {
  return versions.reduce((max, v) => (v.version > max ? v.version : max), 0);
}

/**
 * On-disk record → what every read path is allowed to see.
 *
 * THE PROJECTION IS THE WRITE-ONLY GUARANTEE, in one function that everything reading this backend
 * goes through. A route that returned `StoredSecret` would ship base64 ciphertext to a browser —
 * still encrypted, and still a copy of the secret leaving the machine — so the shape that crosses
 * any boundary is built here by naming fields, never by deleting them from the stored one.
 */
function project(secret: StoredSecret): SecretMeta {
  const versions: SecretVersionMeta[] = secret.versions
    .map((v) => ({ version: v.version, createdAt: v.createdAt, ...(v.revokedAt ? { revokedAt: v.revokedAt } : {}) }))
    .sort((a, b) => a.version - b.version);
  const live = versions.filter((v) => !v.revokedAt);
  const current = live.length ? live[live.length - 1]!.version : undefined;
  return {
    name: secret.name,
    ...(secret.owner ? { owner: secret.owner } : {}),
    createdAt: secret.createdAt,
    updatedAt: secret.updatedAt,
    versions,
    ...(current !== undefined ? { current } : {}),
  };
}
