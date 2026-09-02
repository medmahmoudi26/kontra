/**
 * Kontra claim-check codec — TS port of `src/codec.py`, byte/wire-compatible.
 *
 * Any Temporal payload larger than `threshold` is offloaded to an S3 store
 * (SeaweedFS locally, real S3 in cloud) and replaced in workflow history by a tiny
 * content-addressed reference; `decode` rehydrates it. This is the SAME wire
 * format the deployed Python codec emits, so payloads offloaded by Python actors
 * can be rehydrated here (field-level mapping in the orchestrator needs the bytes)
 * and vice versa.
 *
 * Wire format (do NOT "improve" it — it is the contract):
 *   - Marker: a ref payload has `metadata["encoding"] == utf8("binary/claim-check-v1")`.
 *   - Ref JSON (the payload `data`, UTF-8): `{ "sha256": <hex of original data bytes>,
 *     "size": <int len of original data>, "meta": { <origMetaKey>: <base64 std of
 *     original metadata VALUE bytes> } }`.
 *   - CAS key: derived from the sha256 as `cas/<sha[:2]>/<sha>` (under prefix); it is
 *     NOT stored in the ref — derived on both write and read.
 *   - Offload iff `data.length > threshold` (strictly greater; `<=` stays inline).
 *   - Integrity: on decode, recompute sha256 of the fetched object; throw if != ref.
 *   - Passthrough: when the store is disabled (no endpoint, no injected backing),
 *     encode/decode return payloads unchanged.
 */

import { METADATA_ENCODING_KEY, type Payload, type PayloadCodec } from '@temporalio/common';

import { ObjectStore, type ObjectStoreOptions } from './objectStore';

/**
 * Marker on a reference payload's `encoding` metadata so decode knows to
 * rehydrate. The bytes are `utf8("binary/claim-check-v1")`.
 */
export const CLAIM_CHECK_MARKER = 'binary/claim-check-v1';
const CLAIM_CHECK_MARKER_BYTES = new TextEncoder().encode(CLAIM_CHECK_MARKER);

/** Default offload threshold: 128 KiB, matching KONTRA_S3_THRESHOLD in Python. */
const DEFAULT_THRESHOLD = 128 * 1024;

/** The {sha256, size, meta} shape carried in a claim-check ref payload's data. */
export interface ClaimCheckRef {
  sha256: string;
  size: number;
  meta: Record<string, string>;
}

export interface ClaimCheckCodecOptions extends ObjectStoreOptions {
  /** Offload threshold in bytes; defaults to KONTRA_S3_THRESHOLD or 128 KiB. */
  threshold?: number;
}

function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i += 1) {
    if (a[i] !== b[i]) return false;
  }
  return true;
}

export class ClaimCheckCodec implements PayloadCodec {
  readonly store: ObjectStore;
  readonly threshold: number;

  constructor(opts: ClaimCheckCodecOptions = {}) {
    const { threshold, ...storeOpts } = opts;
    this.store = new ObjectStore(storeOpts);
    if (threshold !== undefined) {
      this.threshold = threshold;
    } else {
      const envThreshold = process.env.KONTRA_S3_THRESHOLD;
      this.threshold =
        envThreshold !== undefined && envThreshold !== ''
          ? Number.parseInt(envThreshold, 10)
          : DEFAULT_THRESHOLD;
    }
  }

  get enabled(): boolean {
    return this.store.enabled;
  }

  async encode(payloads: Payload[]): Promise<Payload[]> {
    if (!this.enabled) return payloads;
    return Promise.all(payloads.map((p) => this.encodeOne(p)));
  }

  private async encodeOne(payload: Payload): Promise<Payload> {
    const metadata = payload.metadata ?? {};
    // Never re-encode an already-claim-check ref (idempotency / double-encode guard).
    const enc = metadata[METADATA_ENCODING_KEY];
    if (enc !== undefined && bytesEqual(enc, CLAIM_CHECK_MARKER_BYTES)) {
      return payload;
    }

    const data = payload.data ?? new Uint8Array(0);
    if (data.length <= this.threshold) {
      return payload;
    }

    const digest = await this.store.putContentAddressed(data);

    // The reference carries ONLY what decode needs: the sha256 content address
    // (the key is derived from it), the size, and the original Temporal payload
    // metadata (base64 of each VALUE's raw bytes) so decode rebuilds a
    // byte-identical Payload.
    const meta: Record<string, string> = {};
    for (const [k, v] of Object.entries(metadata)) {
      meta[k] = Buffer.from(v).toString('base64');
    }
    const ref: ClaimCheckRef = { sha256: digest, size: data.length, meta };
    return {
      metadata: { [METADATA_ENCODING_KEY]: CLAIM_CHECK_MARKER_BYTES },
      data: new TextEncoder().encode(JSON.stringify(ref)),
    };
  }

  async decode(payloads: Payload[]): Promise<Payload[]> {
    if (!this.enabled) return payloads;
    return Promise.all(payloads.map((p) => this.decodeOne(p)));
  }

  private async decodeOne(payload: Payload): Promise<Payload> {
    const enc = payload.metadata?.[METADATA_ENCODING_KEY];
    if (enc === undefined || !bytesEqual(enc, CLAIM_CHECK_MARKER_BYTES)) {
      return payload;
    }

    const refJson = new TextDecoder().decode(payload.data ?? new Uint8Array(0));
    const ref = JSON.parse(refJson) as ClaimCheckRef;
    const data = await this.store.getVerified(ref.sha256, 'claim-check');

    const metadata: Record<string, Uint8Array> = {};
    for (const [k, v] of Object.entries(ref.meta)) {
      metadata[k] = new Uint8Array(Buffer.from(v, 'base64'));
    }
    return { metadata, data };
  }
}
