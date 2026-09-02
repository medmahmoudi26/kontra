import { createHash } from 'node:crypto';
import { describe, it, expect } from 'vitest';
import { METADATA_ENCODING_KEY, type Payload } from '@temporalio/common';

import { ClaimCheckCodec, CLAIM_CHECK_MARKER, type ClaimCheckRef } from './claimCheck';
import { MemoryStore } from './objectStore';

/** A configured codec backed by an injectable in-memory store (no S3). */
function configured(threshold = 1024): { codec: ClaimCheckCodec; store: MemoryStore } {
  const store = new MemoryStore();
  const codec = new ClaimCheckCodec({ backing: store, threshold });
  return { codec, store };
}

function utf8(s: string): Uint8Array {
  return new TextEncoder().encode(s);
}

function refOf(payload: Payload): ClaimCheckRef {
  return JSON.parse(new TextDecoder().decode(payload.data ?? new Uint8Array(0)));
}

describe('ClaimCheckCodec passthrough', () => {
  it('returns payloads unchanged when no store is configured/injected', async () => {
    const codec = new ClaimCheckCodec({ endpoint: undefined, threshold: 1024 });
    expect(codec.enabled).toBe(false);
    const p: Payload = { metadata: { [METADATA_ENCODING_KEY]: utf8('json/plain') }, data: utf8('x'.repeat(10_000)) };
    expect(await codec.encode([p])).toEqual([p]);
    expect(await codec.decode([p])).toEqual([p]);
  });
});

describe('ClaimCheckCodec round-trip', () => {
  it('offloads a large payload and rehydrates it byte-identically (incl. metadata)', async () => {
    const { codec } = configured(1024);
    const big: Payload = {
      metadata: { [METADATA_ENCODING_KEY]: utf8('json/plain') },
      data: utf8('A'.repeat(5000)),
    };
    const small: Payload = {
      metadata: { [METADATA_ENCODING_KEY]: utf8('json/plain') },
      data: utf8('tiny'),
    };

    const enc = await codec.encode([big, small]);

    // big -> claim-check ref (marker set, ref body small)
    expect(enc[0]!.metadata![METADATA_ENCODING_KEY]).toEqual(utf8(CLAIM_CHECK_MARKER));
    expect(enc[0]!.data!.length).toBeLessThan(500);
    // small -> inline, untouched
    expect(enc[1]!.data).toEqual(utf8('tiny'));
    expect(enc[1]!.metadata![METADATA_ENCODING_KEY]).toEqual(utf8('json/plain'));

    const dec = await codec.decode(enc);
    expect(dec[0]!.data).toEqual(big.data);
    expect(dec[0]!.metadata).toEqual(big.metadata);
    expect(dec[1]!.data).toEqual(utf8('tiny'));
    expect(dec[1]!.metadata).toEqual(small.metadata);
  });

  it('offloads strictly above threshold; equal-to-threshold stays inline', async () => {
    const { codec, store } = configured(8);
    const atThreshold: Payload = { metadata: {}, data: utf8('12345678') }; // length 8 == threshold
    const overThreshold: Payload = { metadata: {}, data: utf8('123456789') }; // length 9 > threshold

    const enc = await codec.encode([atThreshold, overThreshold]);
    expect(enc[0]!.data).toEqual(utf8('12345678')); // inline
    expect(enc[1]!.metadata![METADATA_ENCODING_KEY]).toEqual(utf8(CLAIM_CHECK_MARKER)); // offloaded
    expect(store.map.size).toBe(1);
  });

  it('preserves binary (non-utf8) metadata values across the round-trip', async () => {
    const { codec } = configured(4);
    const rawMeta = new Uint8Array([0x00, 0xff, 0x80, 0x01]);
    const p: Payload = { metadata: { custom: rawMeta }, data: utf8('payload-bytes') };
    const dec = await codec.decode(await codec.encode([p]));
    expect(dec[0]!.metadata!.custom).toEqual(rawMeta);
    expect(dec[0]!.data).toEqual(p.data);
  });
});

describe('ClaimCheckCodec golden wire format', () => {
  it('pins the marker, key layout, ref shape, and sha256 over raw bytes', async () => {
    const { codec, store } = configured(2); // tiny threshold so "abc" offloads
    // NIST FIPS-180-2 golden vector: sha256("abc")
    const KNOWN_HEX = 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad';
    const data = utf8('abc');
    expect(createHash('sha256').update(data).digest('hex')).toBe(KNOWN_HEX);

    // marker bytes are exactly utf8("binary/claim-check-v1")
    expect(CLAIM_CHECK_MARKER).toBe('binary/claim-check-v1');
    expect(utf8(CLAIM_CHECK_MARKER)).toEqual(
      new Uint8Array([
        0x62, 0x69, 0x6e, 0x61, 0x72, 0x79, 0x2f, 0x63, 0x6c, 0x61, 0x69, 0x6d, 0x2d, 0x63, 0x68,
        0x65, 0x63, 0x6b, 0x2d, 0x76, 0x31,
      ])
    );

    // casKey == cas/<sha[:2]>/<sha> (no prefix)
    expect(codec.store.casKey(KNOWN_HEX)).toBe(`cas/${KNOWN_HEX.slice(0, 2)}/${KNOWN_HEX}`);
    expect(codec.store.casKey(KNOWN_HEX)).toBe(`cas/ba/${KNOWN_HEX}`);

    const enc = await codec.encode([{ metadata: { [METADATA_ENCODING_KEY]: utf8('json/plain') }, data }]);

    // object stored under the derived CAS key, content == raw bytes
    expect([...store.map.keys()]).toEqual([`cas/ba/${KNOWN_HEX}`]);
    expect(store.map.get(`cas/ba/${KNOWN_HEX}`)).toEqual(data);

    // ref payload data has EXACTLY keys {sha256, size, meta}
    const ref = refOf(enc[0]!);
    expect(Object.keys(ref).sort()).toEqual(['meta', 'sha256', 'size']);
    // sha256 is over the RAW bytes, not the ref JSON
    expect(ref.sha256).toBe(KNOWN_HEX);
    expect(ref.size).toBe(3);
    // meta values are base64 of the original metadata VALUE bytes
    expect(ref.meta).toEqual({ [METADATA_ENCODING_KEY]: Buffer.from(utf8('json/plain')).toString('base64') });
    expect(Buffer.from(ref.meta[METADATA_ENCODING_KEY]!, 'base64')).toEqual(Buffer.from('json/plain'));
  });

  it('joins the CAS key under a configured prefix with no leading slash', async () => {
    const store = new MemoryStore();
    const codec = new ClaimCheckCodec({ backing: store, threshold: 0, prefix: 'demo' });
    expect(codec.store.casKey('abcdef')).toBe('demo/cas/ab/abcdef');
  });
});

describe('ClaimCheckCodec dedup', () => {
  it('writes one object when encoding identical bytes twice', async () => {
    const { codec, store } = configured(1024);
    const data = utf8('B'.repeat(5000));
    await codec.encode([{ metadata: {}, data }]);
    await codec.encode([{ metadata: {}, data }]); // identical bytes
    expect(store.map.size).toBe(1);
  });
});

describe('ClaimCheckCodec integrity', () => {
  it('throws when the stored object is corrupted', async () => {
    const { codec, store } = configured(1024);
    const enc = await codec.encode([{ metadata: {}, data: utf8('C'.repeat(5000)) }]);
    const [key] = [...store.map.keys()];
    store.map.set(key!, utf8('tampered'));
    await expect(codec.decode(enc)).rejects.toThrow(/integrity/);
  });

  it('throws when the stored object is missing', async () => {
    const { codec, store } = configured(1024);
    const enc = await codec.encode([{ metadata: {}, data: utf8('D'.repeat(5000)) }]);
    store.map.clear();
    await expect(codec.decode(enc)).rejects.toThrow(/missing/);
  });
});

describe('ClaimCheckCodec idempotency guard', () => {
  it('does not re-encode an already-claim-check payload', async () => {
    const { codec } = configured(1024);
    const enc = await codec.encode([{ metadata: {}, data: utf8('E'.repeat(5000)) }]);
    const encAgain = await codec.encode(enc);
    expect(encAgain[0]).toEqual(enc[0]); // unchanged ref payload
  });
});
