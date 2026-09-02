/**
 * TS side of the language-neutral codec conformance corpus.
 *
 * Runs the REAL orchestrator ClaimCheckCodec against the SAME
 * shared/conformance/codec/fixtures.json the Python harness uses. If this and the Python
 * harness both pass, the two encoders are wire-compatible on every pinned case —
 * which is the only thing that keeps cross-language claim-check (and a future Go
 * port) honest. See shared/conformance/codec/README.md for the contract.
 */
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { METADATA_ENCODING_KEY, type Payload } from '@temporalio/common';

import { ClaimCheckCodec } from './claimCheck';
import { MemoryStore, ObjectStore } from './objectStore';

/**
 * One `prefixCases` row: the same payload every time, only the store prefix varying, and the
 * full object key it must land under.
 */
interface PrefixFixture {
  name: string;
  why: string;
  prefix: string;
  threshold: number;
  input: { data_b64: string; meta_b64: Record<string, string> };
  expect: { sha256: string; key: string };
}

interface Fixture {
  name: string;
  threshold: number;
  input: { data_b64: string; meta_b64: Record<string, string> };
  expect:
    | { action: 'inline' | 'unchanged' }
    | {
        action: 'offload';
        ref: { sha256: string; size: number; meta: Record<string, string> };
        cas_key: string;
        object_b64: string;
      };
}

const CORPUS = path.resolve(__dirname, '../../../shared/conformance/codec/fixtures.json');
const doc = JSON.parse(readFileSync(CORPUS, 'utf8')) as {
  wireFormat: { marker: string };
  cases: Fixture[];
  prefixCases: PrefixFixture[];
};
const MARKER_BYTES = new TextEncoder().encode(doc.wireFormat.marker);

function b64ToBytes(s: string): Uint8Array {
  return new Uint8Array(Buffer.from(s, 'base64'));
}

describe('codec conformance corpus (shared with Python)', () => {
  for (const fx of doc.cases) {
    it(fx.name, async () => {
      const meta: Record<string, Uint8Array> = {};
      for (const [k, v] of Object.entries(fx.input.meta_b64)) meta[k] = b64ToBytes(v);
      const payload: Payload = { metadata: meta, data: b64ToBytes(fx.input.data_b64) };

      const store = new MemoryStore();
      const codec = new ClaimCheckCodec({ backing: store, threshold: fx.threshold });
      const [out] = await codec.encode([payload]);

      if (fx.expect.action === 'inline' || fx.expect.action === 'unchanged') {
        expect(out!.data).toEqual(payload.data);
        expect(out!.metadata).toEqual(payload.metadata);
        expect(store.map.size).toBe(0);
        return;
      }

      // offload
      expect(out!.metadata![METADATA_ENCODING_KEY]).toEqual(MARKER_BYTES);
      const ref = JSON.parse(new TextDecoder().decode(out!.data ?? new Uint8Array(0)));
      expect(ref).toEqual(fx.expect.ref);
      expect(store.map.has(fx.expect.cas_key)).toBe(true);
      expect(new Uint8Array(store.map.get(fx.expect.cas_key)!)).toEqual(b64ToBytes(fx.expect.object_b64));

      // round-trip: decode reconstructs a byte-identical original payload
      const [back] = await codec.decode([out!]);
      expect(back!.data).toEqual(payload.data);
      expect(back!.metadata).toEqual(payload.metadata);
    });
  }
});

/**
 * The `prefixCases` rows: the store prefix is a PATH SEGMENT, not a string glued to the front
 * of the key.
 *
 * This arm did not exist until 2026-08-29, and its absence is the reason six green runners
 * said nothing about a live outage. Every case above runs with the default EMPTY prefix, and
 * with no prefix `prefix + key` and `join(prefix, key)` are the same bytes — the one input
 * class in which the four implementations cannot differ (ADR 0035 finding 1). Under
 * KONTRA_S3_PREFIX=slice11 the two SDK stores wrote `slice11cas/df/df5b…` while this one read
 * `slice11/cas/df/df5b…`.
 */
describe('codec conformance corpus: the store prefix is a path segment', () => {
  for (const fx of doc.prefixCases) {
    it(`${fx.name} (${fx.prefix === '' ? 'no prefix' : `prefix "${fx.prefix}"`})`, async () => {
      const meta: Record<string, Uint8Array> = {};
      for (const [k, v] of Object.entries(fx.input.meta_b64)) meta[k] = b64ToBytes(v);
      const payload: Payload = { metadata: meta, data: b64ToBytes(fx.input.data_b64) };

      const store = new MemoryStore();
      const codec = new ClaimCheckCodec({
        backing: store,
        prefix: fx.prefix,
        threshold: fx.threshold,
      });

      const [out] = await codec.encode([payload]);
      const ref = JSON.parse(new TextDecoder().decode(out!.data ?? new Uint8Array(0)));
      expect(ref.sha256).toBe(fx.expect.sha256);

      // The object is at the corpus's address, and at no other.
      expect([...store.map.keys()]).toEqual([fx.expect.key]);
      expect(new Uint8Array(store.map.get(fx.expect.key)!)).toEqual(payload.data);

      // The read side derives the same address — a writer and a reader that disagree is the
      // same outage in the other direction.
      const [back] = await codec.decode([out!]);
      expect(back!.data).toEqual(payload.data);
      expect(back!.metadata).toEqual(payload.metadata);

      // And the derivation on its own, so a failure names the address or the plumbing.
      expect(new ObjectStore({ prefix: fx.prefix }).casKey(fx.expect.sha256)).toBe(fx.expect.key);
    });
  }
});

/**
 * The same rows through KONTRA_S3_PREFIX itself, because that env var is the only surface an
 * operator touches and `ObjectStore` normalises it in the constructor rather than in `key()` —
 * `data/parquet.ts` builds the DuckLake DATA_PATH out of the `prefix` FIELD, so a spelling
 * stripped only inside `key()` would still escape.
 */
describe('codec conformance corpus: KONTRA_S3_PREFIX is normalised where it is read', () => {
  const saved = process.env.KONTRA_S3_PREFIX;
  beforeEach(() => {
    delete process.env.KONTRA_S3_PREFIX;
  });
  afterEach(() => {
    if (saved === undefined) delete process.env.KONTRA_S3_PREFIX;
    else process.env.KONTRA_S3_PREFIX = saved;
  });

  for (const fx of doc.prefixCases) {
    it(fx.name, () => {
      process.env.KONTRA_S3_PREFIX = fx.prefix;
      const store = new ObjectStore({});
      expect(store.casKey(fx.expect.sha256)).toBe(fx.expect.key);
      // The FIELD, not only what key() makes of it: parquet.ts reads it directly.
      expect(store.prefix).not.toMatch(/^\/|\/$/);
      expect(fx.expect.key).toBe(store.prefix ? `${store.prefix}/cas/83/${fx.expect.sha256}` : `cas/83/${fx.expect.sha256}`);
    });
  }
});
