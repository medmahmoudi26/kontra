/**
 * The TYPESCRIPT ARM of `shared/conformance/login.json`.
 *
 * The CLI writes the hash at install; this verifies it at login. Two languages, one format, and a
 * mismatch is a login nobody can pass — which neither suite would notice, because each half is
 * correct on its own. That is exactly the shape ADR 0035 rule two says gets a corpus.
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

import { hashPassword, verifyPassword, SCHEME } from './password';

type Vector = { why: string; password: string; salt_b64: string; encoded: string };

const corpus = JSON.parse(
  readFileSync(join(__dirname, '../../../../shared/conformance/login.json'), 'utf8')
) as {
  cost: { N: number; r: number; p: number; key_len: number; salt_len: number };
  vectors: Vector[];
};

describe('the corpus itself', () => {
  it('carries the rows that break a naive implementation', () => {
    // A corpus of one happy row passes an implementation that splits on `$` naively, ignores
    // non-ASCII, and never sees an empty password.
    expect(corpus.vectors.length).toBeGreaterThanOrEqual(4);
    const passwords = corpus.vectors.map((v) => v.password);
    expect(passwords, 'the `$` row is the field separator inside the value').toContain('a$b$c$d');
    expect(passwords, 'the non-ASCII row').toContain('café-naïve-📦');
    expect(passwords, 'the empty row').toContain('');
  });
});

describe('a hash the Go CLI wrote verifies here', () => {
  for (const v of corpus.vectors) {
    it(v.why, async () => {
      await expect(verifyPassword(v.password, v.encoded)).resolves.toBe(true);
    });
  }

  it('and the wrong password does not', async () => {
    // NON-VACUOUS: a verifier returning true unconditionally passes every row above.
    for (const v of corpus.vectors) {
      await expect(verifyPassword(v.password + 'x', v.encoded)).resolves.toBe(false);
    }
  });

  it('produces byte-identical output for the same salt', async () => {
    // The strongest form: this side DERIVES the vector rather than only checking it, so a
    // divergence in encoding — padding, alphabet, field order — fails here rather than at a login.
    for (const v of corpus.vectors) {
      const salt = Buffer.from(v.salt_b64, 'base64');
      const mine = await hashPassword(v.password, { ...corpus.cost, salt });
      expect(mine, v.why).toBe(v.encoded);
    }
  });
});

describe('a malformed hash locks rather than opens', () => {
  // Throwing is the contract, and the caller must treat it as a refusal. A config whose hash
  // somebody hand-edited into nonsense has to lock the console, not open it.
  for (const bad of [
    '',
    'garbage',
    'scrypt$x$8$1$AAAA$AAAA',
    'scrypt$$8$1$AAAA$AAAA',
    'bcrypt$32768$8$1$AAAA$AAAA',
    'scrypt$1$8$1$AAAA$AAAA',
    'scrypt$32768$8$1$AAAA',
    'scrypt$32768$8$1$$AAAA',
  ]) {
    it(`refuses ${JSON.stringify(bad)}`, async () => {
      await expect(verifyPassword('anything', bad)).rejects.toThrow();
    });
  }

  it('names the scheme it wanted', async () => {
    await expect(verifyPassword('x', 'bcrypt$1$1$1$AA$AA')).rejects.toThrow(new RegExp(SCHEME));
  });
});

describe('the maxmem trap', () => {
  it('verifies at the corpus cost, which is exactly Node’s default ceiling', async () => {
    // 128 * 32768 * 8 = 32 MiB, which IS `crypto.scrypt`'s default maxmem — so a version of this
    // file that did not pass maxmem explicitly would throw here rather than at a login.
    expect(corpus.cost.N).toBe(32768);
    const [first] = corpus.vectors;
    await expect(verifyPassword(first!.password, first!.encoded)).resolves.toBe(true);
  });
});
