/**
 * Verifying the console login credential — the READER of what `cli/internal/creds` writes.
 *
 * WHY A LOGIN EXISTS. The CLI authenticates by reading `~/.kontra/config.yaml`. A browser cannot, so
 * the console got in by having a bearer BAKED INTO ITS BUNDLE at build time
 * (`VITE_KONTRA_EXPLORE_TOKEN`). That is a credential in a build artifact, it breaks on every
 * rotation, and a build run for an unrelated reason silently replaced the served bundle with a
 * tokenless one that answered `query: unauthorized` for every query. Signing in is where the
 * browser's token comes from instead.
 *
 * SCRYPT, BECAUSE IT IS THE ONE PASSWORD KDF BOTH SIDES HAVE WITHOUT A NEW DEPENDENCY —
 * `golang.org/x/crypto/scrypt` there and `node:crypto`'s builtin here. bcrypt or argon2 would each
 * add an npm package to the control plane to check one password on a single-instance install.
 *
 * THE FORMAT IS SELF-DESCRIBING, so the cost lives in the string rather than in a constant two
 * languages must change at the same moment:
 *
 *     scrypt$32768$8$1$<salt-b64>$<hash-b64>          raw (unpadded) standard base64
 *
 * `shared/conformance/login.json` carries vectors this file and its Go peer both execute. A hash the
 * CLI writes and this cannot read is a login nobody can pass, and neither suite would notice: each
 * half is correct on its own.
 */

import { randomBytes, scrypt as scryptCb, timingSafeEqual } from 'node:crypto';
import { promisify } from 'node:util';

const scrypt = promisify(scryptCb) as (
  password: string | Buffer,
  salt: Buffer,
  keylen: number,
  options: { N: number; r: number; p: number; maxmem: number }
) => Promise<Buffer>;

/** `scrypt` — the only scheme this understands. A value that claims another is refused, not guessed. */
export const SCHEME = 'scrypt';

/**
 * How much memory a verification may use.
 *
 * THE ONE ASYMMETRY BETWEEN THE TWO LANGUAGES, and it has to be explicit. N=32768 needs
 * 128*N*r = 32 MiB, which is exactly Node's DEFAULT `maxmem` — so the default either throws or, if
 * it ever changed, would silently cap the work factor. Go's scrypt has no such ceiling. Named at
 * 4× the current requirement so raising N once does not need this line changed with it.
 */
const MAXMEM = 128 * 1024 * 1024;

/** A password's encoded hash, parsed. */
interface Parsed {
  N: number;
  r: number;
  p: number;
  salt: Buffer;
  hash: Buffer;
}

function parse(encoded: string): Parsed {
  const parts = encoded.split('$');
  if (parts.length !== 6 || parts[0] !== SCHEME) {
    throw new Error(`not a ${SCHEME} hash: expected ${SCHEME}$N$r$p$salt$hash`);
  }
  const [, rawN, rawR, rawP, rawSalt, rawHash] = parts as [string, string, string, string, string, string];
  const N = Number(rawN);
  const r = Number(rawR);
  const p = Number(rawP);
  // `Number('')` is 0 and `Number('8x')` is NaN — both have to fail, so this checks the VALUE
  // rather than trusting the parse.
  if (!Number.isInteger(N) || !Number.isInteger(r) || !Number.isInteger(p) || N <= 1 || r <= 0 || p <= 0) {
    throw new Error(`${SCHEME} hash has unusable cost parameters`);
  }
  const salt = Buffer.from(rawSalt, 'base64');
  const hash = Buffer.from(rawHash, 'base64');
  // Node's base64 decoder does not reject junk, it skips it — so an undecodable field arrives as a
  // short buffer rather than an error, and a zero-length one would compare equal to another
  // zero-length one. Length is the check.
  if (salt.length === 0 || hash.length === 0) {
    throw new Error(`${SCHEME} hash has an undecodable salt or digest`);
  }
  return { N, r, p, salt, hash };
}

/**
 * Does `password` produce `encoded`?
 *
 * CONSTANT-TIME ON THE COMPARE, for the same reason `auth.ts` is: a byte-by-byte compare returns as
 * soon as two bytes differ, which turns brute force into a per-character search.
 *
 * A MALFORMED HASH THROWS, AND THE CALLER MUST TREAT THAT AS A REFUSAL — never as a pass. A config
 * whose hash somebody hand-edited into nonsense has to lock the console, not open it.
 */
export async function verifyPassword(password: string, encoded: string): Promise<boolean> {
  const { N, r, p, salt, hash } = parse(encoded);
  const got = await scrypt(password, salt, hash.length, { N, r, p, maxmem: MAXMEM });
  return got.length === hash.length && timingSafeEqual(got, hash);
}

/**
 * Derive a hash — for tests and for any future "change my password" route.
 *
 * The CLI is the writer in production (`kontra init` mints the credential at install), so this
 * exists to prove the two agree rather than to be the source of hashes.
 */
export async function hashPassword(
  password: string,
  opts: { N?: number; r?: number; p?: number; salt?: Buffer } = {}
): Promise<string> {
  const N = opts.N ?? 32768;
  const r = opts.r ?? 8;
  const p = opts.p ?? 1;
  const salt = opts.salt ?? randomBytes(16);
  const hash = await scrypt(password, salt, 32, { N, r, p, maxmem: MAXMEM });
  return [SCHEME, N, r, p, b64(salt), b64(hash)].join('$');
}

/** Raw (unpadded) standard base64 — what the Go peer writes with `base64.RawStdEncoding`. */
function b64(b: Buffer): string {
  return b.toString('base64').replace(/=+$/, '');
}
