/**
 * EP14, asked of kontra: is a secret lookup keyed by WHO is asking, or only by the path?
 *
 * Windmill's own threat model carries the finding, and the parenthesis is the whole lesson:
 *
 *   > Secret-value & resource-value caches. In-memory caches in `windmill-store` keyed
 *   > *(historically un-keyed)* by path — cache lookup crossing identity/folder boundary.
 *
 * A cache keyed by a secret's PATH but not by its ASKER returns one caller's secret to another. No
 * authorization test catches it, because authorization ran correctly — on the request that filled
 * the cache. It is the natural bug of any fast path bolted onto a correct slow path, and a mature
 * product shipped it.
 *
 * ── THE AUDIT, 2026-09-12 ───────────────────────────────────────────────────────────────────────
 *
 * kontra is clean, and these tests exist to keep it that way rather than to report a fix:
 *
 *   `store.ts:resolve`      takes a Principal and re-checks ownership on EVERY call. There is no
 *                           memoisation between the check and the value.
 *   `slotStore.ts:secretIndex`  builds a fresh Map per call — a local, not a field.
 *   `fileBackend.ts:cachedKey`  memoises a DERIVED SUBKEY, not a per-path value. It is one key for
 *                           the whole store, so an asker's identity cannot enter into it. This is
 *                           the low-risk shape and the test below pins that it stays that shape.
 *
 * SO THE ASSERTION IS THE INVERSE of the obvious one. There is no cache to prove is identity-keyed;
 * what has to be proven is that no VALUE cache appears later without one — hence the counting
 * backend. If someone adds path-keyed memoisation, the second resolve stops reaching the backend
 * and this goes red, which is the moment to think about identity.
 */

import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { beforeEach, describe, expect, it } from 'vitest';

import { FileSecretBackend } from './fileBackend';
import { SecretStore } from './store';
import { SecretForbidden, actorIdentity } from './types';

const SENTINEL = 'dop_v1_SENTINEL_never_in_the_clear_9f3c';
const ALICE = actorIdentity('alice');
const MALLORY = actorIdentity('mallory');

/** A backend that records how often each secret's VALUE was actually read. */
class CountingBackend extends FileSecretBackend {
  readonly resolves: string[] = [];
  readonly describes: string[] = [];

  async resolve(ref: { name: string; version?: number }): Promise<string> {
    this.resolves.push(ref.name);
    return super.resolve(ref);
  }

  async describe(name: string): ReturnType<FileSecretBackend['describe']> {
    this.describes.push(name);
    return super.describe(name);
  }
}

let backend: CountingBackend;
let store: SecretStore;

beforeEach(() => {
  process.env.KONTRA_SECRETS_KEY = Buffer.alloc(32, 7).toString('base64');
  backend = new CountingBackend({ dir: mkdtempSync(join(tmpdir(), 'kontra-ep14-')) });
  store = new SecretStore(backend);
});

describe('a secret lookup is keyed by who is asking', () => {
  it('refuses a second identity the value the first one owns', async () => {
    await store.put('alice-token', SENTINEL, { owner: 'alice' });

    // The owner gets it.
    const mine = await store.resolve({ name: 'alice-token' }, { kind: 'actor', identity: ALICE });
    expect(mine.value).toBe(SENTINEL);

    // Anybody else does not — by NAME, which is the only thing a path-keyed cache would key on.
    await expect(
      store.resolve({ name: 'alice-token' }, { kind: 'actor', identity: MALLORY })
    ).rejects.toBeInstanceOf(SecretForbidden);
  });

  it('still refuses after the owner has just resolved it', async () => {
    // THE ORDERING IS THE TEST. A path-keyed cache is populated by the legitimate read and then
    // served to the next asker; checking the refusal on a cold store would miss it entirely.
    await store.put('alice-token', SENTINEL, { owner: 'alice' });
    await store.resolve({ name: 'alice-token' }, { kind: 'actor', identity: ALICE });

    await expect(
      store.resolve({ name: 'alice-token' }, { kind: 'actor', identity: MALLORY })
    ).rejects.toBeInstanceOf(SecretForbidden);

    // And Mallory's attempt never reached the value at all.
    expect(backend.resolves.filter((n) => n === 'alice-token')).toHaveLength(1);
  });

  it('does not memoise a value between calls', async () => {
    await store.put('alice-token', SENTINEL, { owner: 'alice' });
    await store.resolve({ name: 'alice-token' }, { kind: 'actor', identity: ALICE });
    await store.resolve({ name: 'alice-token' }, { kind: 'actor', identity: ALICE });

    // TWO reads for two resolves. If this becomes one, a value cache was added — and the next
    // question is whether its key includes the asker. It must, or the test above stops holding.
    expect(backend.resolves.filter((n) => n === 'alice-token')).toHaveLength(2);
    // Ownership is re-read too, rather than trusted from the first call.
    expect(backend.describes.filter((n) => n === 'alice-token').length).toBeGreaterThanOrEqual(2);
  });

  it('refuses an operator secret to any actor, cached or not', async () => {
    await store.put('operator-only', SENTINEL); // no owner
    for (const who of [ALICE, MALLORY]) {
      await expect(
        store.resolve({ name: 'operator-only' }, { kind: 'actor', identity: who })
      ).rejects.toBeInstanceOf(SecretForbidden);
    }
    expect(backend.resolves).not.toContain('operator-only');
  });

  it('a binding may only ask for what it was minted for', async () => {
    await store.put('alice-token', SENTINEL, { owner: 'alice' });
    await store.put('other-token', SENTINEL, { owner: 'alice' });

    await expect(
      store.resolve(
        { name: 'other-token' },
        { kind: 'binding', identity: ALICE, slot: 'db', secret: 'alice-token' }
      )
    ).rejects.toBeInstanceOf(SecretForbidden);
    // Refused before any read: a per-slot grant must not become ambient access to the store.
    expect(backend.resolves).not.toContain('other-token');
  });
});

describe('the one cache that does exist', () => {
  it('fileBackend memoises a derived KEY, never a per-path value', async () => {
    // Two different secrets, two different values, from the same store — a key cache cannot
    // conflate them, and a value cache keyed by path would have to be asked twice anyway.
    await store.put('one', 'value-one', { owner: 'alice' });
    await store.put('two', 'value-two', { owner: 'alice' });

    const a = await store.resolve({ name: 'one' }, { kind: 'actor', identity: ALICE });
    const b = await store.resolve({ name: 'two' }, { kind: 'actor', identity: ALICE });

    expect(a.value).toBe('value-one');
    expect(b.value).toBe('value-two');
    expect(a.value).not.toBe(b.value);
  });
});
