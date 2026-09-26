/**
 * Shared object-store access for the orchestrator — the TS port of
 * `src/objectstore.py`. Claim-check offload rides on this. SeaweedFS's S3 gateway
 * locally, real S3 in cloud (one API, two endpoints, path-style addressing).
 * Disabled when `KONTRA_S3_ENDPOINT` is unset, so the orchestrator and its unit
 * tests run with no object store.
 *
 * Config (env): KONTRA_S3_ENDPOINT, KONTRA_S3_BUCKET, KONTRA_S3_ACCESS_KEY,
 * KONTRA_S3_SECRET_KEY, KONTRA_S3_REGION, KONTRA_S3_PREFIX.
 *
 * The key layout MUST match the Python side byte-for-byte: `key(...)` joins parts
 * under the optional prefix with slashes stripped and empty parts dropped, and
 * `casKey(sha)` derives `cas/<sha[:2]>/<sha>`.
 */

import { createHash } from 'node:crypto';
import {
  S3Client,
  GetObjectCommand,
  PutObjectCommand,
  HeadObjectCommand,
  CreateBucketCommand,
  CopyObjectCommand,
  ListObjectsV2Command,
  PutBucketCorsCommand,
  DeleteObjectCommand,
  DeleteObjectsCommand,
  PutBucketLifecycleConfigurationCommand,
} from '@aws-sdk/client-s3';
import { getSignedUrl } from '@aws-sdk/s3-request-presigner';

/** Lower-hex sha256 — the content address for every CAS object. */
export function sha256Hex(data: Uint8Array): string {
  return createHash('sha256').update(data).digest('hex');
}

/**
 * Minimal backing-store surface the codec needs. Injectable so tests can supply
 * an in-memory map instead of S3. `get` returns null for a missing key.
 */
export interface BackingStore {
  get(key: string): Promise<Uint8Array | null>;
  put(key: string, data: Uint8Array): Promise<void>;
  exists(key: string): Promise<boolean>;
  /** Full keys under `prefix`. Optional — only the dataset browser needs it. */
  list?(prefix: string): Promise<string[]>;
  /** Delete a key. Optional — only retention sweeps need it. */
  remove?(key: string): Promise<void>;
}

/**
 * S3's cap on one DeleteObjects request. Not a tuning knob — the API rejects 1,001.
 *
 * It is what makes a units sweep finishable at all: the measured `units/` prefix on this box holds
 * 254,801 objects, and one DeleteObject round trip each would be a quarter of a million requests
 * against a 30-minute activity timeout. At 1,000 a call it is 255.
 */
export const DELETE_BATCH_MAX = 1000;

/**
 * The ONE prefix a lifecycle rule is ever written for. A constant, never a parameter — see
 * {@link ObjectStore.ensureLifecycle} for why this is the whole of the blast-radius argument.
 */
export const UNITS_LIFECYCLE_PREFIX = 'units/';

/** Stable across boots so the PUT is an update, not an accumulation of near-identical rules. */
export const UNITS_LIFECYCLE_RULE_ID = 'kontra-expire-units';

/** The same variable the sweep reads, so the store's rule and the backstop cannot disagree about
 *  the window. Unset means NO rule is written — this never imposes a policy nobody stated. */
export const UNITS_RETENTION_DAYS_ENV = 'KONTRA_UNITS_RETENTION_DAYS';

/**
 * The lifecycle window in whole days, or `null` for "state nothing".
 *
 * REFUSES ANYTHING UNDER A DAY rather than rounding it down. S3 expiry is day-granular, so `0.5`
 * would floor to 0 — which is not "half a day", it is "expire everything immediately", written by
 * somebody who meant the opposite. A sub-day value is a misunderstanding, and the safe reading of a
 * misunderstanding about deletion is to do nothing.
 */
export function unitsLifecycleDays(env: NodeJS.ProcessEnv = process.env): number | null {
  const raw = (env[UNITS_RETENTION_DAYS_ENV] ?? '').trim();
  if (raw === '') return null;
  const days = Number(raw);
  if (!Number.isFinite(days) || days < 1) return null;
  return Math.floor(days);
}

/** One listed object: its full key plus metadata S3 returns on a listing. */
export interface ListedObject {
  key: string;
  size?: number;
  lastModified?: Date;
}

export interface ObjectStoreOptions {
  endpoint?: string;
  bucket?: string;
  prefix?: string;
  region?: string;
  accessKey?: string;
  secretKey?: string;
  /**
   * Browser-reachable S3 endpoint used ONLY to sign presigned GET URLs (the browser
   * range-reads parquet from it). Split-horizon: server-side ops use `endpoint`
   * (`seaweed:8333` in Docker) while the browser reaches `localhost:8333`, and a SigV4
   * URL must be signed for the host the browser actually calls. Defaults to `endpoint`.
   */
  publicEndpoint?: string;
  /** Inject a backing store (e.g. an in-memory map) to bypass S3 entirely. */
  backing?: BackingStore;
}

/** boto3/botocore/aws-sdk surface a missing object via these; treat as "not present". */
const MISSING = new Set(['NoSuchKey', 'NotFound', '404', 'NoSuchBucket']);

function errName(err: unknown): string | undefined {
  if (err && typeof err === 'object') {
    const e = err as { name?: string; Code?: string; $metadata?: { httpStatusCode?: number } };
    if (e.name) return e.name;
    if (e.Code) return e.Code;
    if (e.$metadata?.httpStatusCode === 404) return '404';
  }
  return undefined;
}

function isMissing(err: unknown): boolean {
  const name = errName(err);
  if (name && MISSING.has(name)) return true;
  if (err && typeof err === 'object') {
    const status = (err as { $metadata?: { httpStatusCode?: number } }).$metadata?.httpStatusCode;
    if (status === 404) return true;
  }
  return false;
}

/**
 * Async object store over `@aws-sdk/client-s3` (get/put/exists by key). The
 * client is built lazily and the bucket is created best-effort on first use, so
 * constructing a (disabled) store touches no network and needs no credentials.
 */
export class ObjectStore {
  readonly endpoint: string | undefined;
  readonly publicEndpoint: string | undefined;
  readonly bucket: string;
  readonly prefix: string;
  readonly region: string;
  readonly accessKey: string;
  readonly secretKey: string;

  private readonly backing: BackingStore | undefined;
  private s3: S3Client | null = null;
  private presignS3: S3Client | null = null;
  private bucketEnsured = false;

  constructor(opts: ObjectStoreOptions = {}) {
    const env = process.env;
    this.endpoint = opts.endpoint ?? env.KONTRA_S3_ENDPOINT;
    this.publicEndpoint = opts.publicEndpoint ?? env.KONTRA_S3_PUBLIC_ENDPOINT ?? this.endpoint;
    this.bucket = opts.bucket ?? env.KONTRA_S3_BUCKET ?? 'kontra';
    // NORMALISED ONCE, HERE. `prefix` is read by {@link key} and by callers that build a path by
    // hand (`data/parquet.ts`'s DuckLake DATA_PATH), so a spelling stripped inside `key()` alone
    // would still leak out of the field. Slashes at the edges are not part of a prefix — see
    // `wireFormat.prefixTrailingSlash` in shared/conformance/codec/fixtures.json.
    this.prefix = stripSlashes(opts.prefix ?? env.KONTRA_S3_PREFIX ?? '');
    this.region = opts.region ?? env.KONTRA_S3_REGION ?? 'us-east-1';
    this.accessKey = opts.accessKey ?? env.KONTRA_S3_ACCESS_KEY ?? 'kontra';
    this.secretKey = opts.secretKey ?? env.KONTRA_S3_SECRET_KEY ?? 'kontra';
    this.backing = opts.backing;
  }

  /**
   * The store is enabled when a backing store is injected (tests) OR an endpoint
   * is configured. Disabled => codec passthrough.
   */
  get enabled(): boolean {
    return this.backing !== undefined || Boolean(this.endpoint);
  }

  /**
   * Join key parts under the configured prefix: strip leading/trailing '/' from EVERY
   * segment — the prefix included, normalised in the constructor — drop the ones that
   * are then empty, and join what is left with a single '/'. So
   * key('cas', '12', '12ab...') -> 'prefix/cas/12/12ab...', with no leading slash when
   * the prefix is empty.
   *
   * The algorithm is the contract, not a convenience: `runtime/handler/internal/objectstore.Key`,
   * `runtime/go/codec.objectKey` and `runtime/python/internals/casstore.object_key` must
   * produce the same bytes, and the `prefixCases` rows of shared/conformance/codec/fixtures.json
   * are what hold the four of them to it.
   */
  key(...parts: unknown[]): string {
    const bits: string[] = this.prefix ? [this.prefix] : [];
    for (const p of parts) {
      const s = stripSlashes(String(p));
      if (s !== '') bits.push(s);
    }
    return bits.join('/');
  }

  /** The one content-addressed key for an object: `cas/<sha[:2]>/<sha>` under prefix. */
  casKey(sha: string): string {
    return this.key('cas', sha.slice(0, 2), sha);
  }

  /**
   * Content-address bytes: sha256 -> store at casKey iff absent (dedup is free under
   * content-addressing). Returns the sha. The ONE place the put-if-absent CAS write
   * lives — both the claim-check codec and the node-result blob plane call it.
   */
  async putContentAddressed(data: Uint8Array): Promise<string> {
    const sha = sha256Hex(data);
    const key = this.casKey(sha);
    if (!(await this.exists(key))) {
      await this.put(key, data);
    }
    return sha;
  }

  /**
   * Fetch a CAS object by its sha (derived to a key, never carried) and re-check
   * integrity. `noun` shapes the error surface ("claim-check" / "node-result"). The
   * ONE place the fetch + integrity recheck lives.
   */
  async getVerified(sha: string, noun: string): Promise<Uint8Array> {
    const key = this.casKey(sha);
    const data = await this.get(key);
    if (data === null) {
      throw new Error(`${noun} object missing: ${key}`);
    }
    if (sha256Hex(data) !== sha) {
      throw new Error(`${noun} integrity check failed for ${key}`);
    }
    return data;
  }

  /**
   * Copy an existing object src->dst: server-side CopyObject on S3 (no re-upload),
   * get+put on an injected backing. Used to mirror a node's content-addressed output
   * blob to a run-addressed `datasets/...` key so DuckDB can glob it by
   * actor/version/run_id (a CAS key carries no identity, so it can't be globbed).
   */
  async copy(srcKey: string, dstKey: string): Promise<void> {
    if (this.backing) {
      const data = await this.backing.get(srcKey);
      if (data === null) throw new Error(`copy source missing: ${srcKey}`);
      await this.backing.put(dstKey, data);
      return;
    }
    await this.ensureBucket();
    // ponytail: keys are contract-validated identifiers ([a-z0-9/=._-]), so CopySource
    // needs no URL-encoding; add per-segment encodeURI if names ever allow spaces/unicode.
    await this.client().send(
      new CopyObjectCommand({ Bucket: this.bucket, CopySource: `${this.bucket}/${srcKey}`, Key: dstKey })
    );
  }

  /**
   * List objects under `prefix` (relative to the store prefix, like {@link key}),
   * returning each full key with the size/mtime S3 reports. Paginates to completion.
   * A backing store lists its own map (if it supports it); a disabled store lists nothing.
   * Used by the dataset browser to enumerate the run-addressed `datasets/…` outputs.
   */
  async list(prefix: string): Promise<ListedObject[]> {
    const full = this.key(prefix);
    if (this.backing) {
      const keys = this.backing.list ? await this.backing.list(full) : [];
      return keys.map((key) => ({ key }));
    }
    if (!this.endpoint) return [];
    await this.ensureBucket();
    const out: ListedObject[] = [];
    let token: string | undefined;
    do {
      const res = await this.client().send(
        new ListObjectsV2Command({ Bucket: this.bucket, Prefix: full, ContinuationToken: token })
      );
      for (const o of res.Contents ?? []) {
        if (o.Key) out.push({ key: o.Key, size: o.Size, lastModified: o.LastModified });
      }
      token = res.IsTruncated ? res.NextContinuationToken : undefined;
    } while (token);
    return out;
  }

  /**
   * A presigned GET URL for `key`, valid `expiresIn` seconds — the browser range-reads
   * parquet from it directly (no creds in the browser, no data through the API). Signed
   * against {@link publicEndpoint} so the signature matches the host the browser calls.
   * Throws if no endpoint is configured (a disabled store can't presign).
   */
  async presignGet(key: string, expiresIn = 3600): Promise<string> {
    if (!this.publicEndpoint) throw new Error('cannot presign: no S3 endpoint configured');
    return getSignedUrl(
      this.presignClient(),
      new GetObjectCommand({ Bucket: this.bucket, Key: key }),
      { expiresIn }
    );
  }

  /**
   * Best-effort: allow the browser to cross-origin range-read objects (parquet) — GET/HEAD
   * from any origin, with the Range request header and Content-Range exposed so DuckDB-WASM
   * can seek. Idempotent; failures are swallowed (an older S3 without PutBucketCors still
   * serves same-origin/CLI reads). Runs once with the bucket ensure.
   */
  private async ensureCors(): Promise<void> {
    try {
      await this.client().send(
        new PutBucketCorsCommand({
          Bucket: this.bucket,
          CORSConfiguration: {
            CORSRules: [
              {
                AllowedOrigins: ['*'],
                AllowedMethods: ['GET', 'HEAD'],
                AllowedHeaders: ['*'],
                ExposeHeaders: ['Content-Range', 'Content-Length', 'ETag', 'Accept-Ranges'],
                MaxAgeSeconds: 3600,
              },
            ],
          },
        })
      );
    } catch {
      // best-effort — CORS is only needed for the browser's direct reads.
    }
  }

  /**
   * HAND `units/` TO THE OBJECT STORE, so nothing in this repo has to collect it.
   *
   * ── WHY THIS EXISTS WHEN `sweepUnits` ALREADY DOES THE JOB ──────────────────────────────────────
   *
   * Because a sweeper is a garbage collector we maintain, and the store has one we do not. Measured
   * on SeaweedFS 3.80, which is what this rule is worth over the sweep:
   *
   *   PER-OBJECT DELETE LEAVES HOLES, NOT FREE DISK. An object store reclaims a deleted object by
   *   marking it dead inside its volume; the `.dat` file does not shrink. This bucket today reports
   *   `deleted_file: 1378, deleted_bytes: 4,326,291` against `total size: 8,409,664,672` — 0.05%,
   *   accumulated and never reclaimed. Deleting the 102,257 collectable unit objects would have
   *   punched 7.3 GiB of holes that only `volume.vacuum` can recover, and vacuum is a COPY: it needs
   *   as much free space as the volume it is rewriting, on the disk that is full. The sweep would
   *   have reported 7.3 GiB freed and freed approximately none of it.
   *
   *   A TTL RULE SEGREGATES AND THEN UNLINKS WHOLE VOLUMES. Writing one object under this prefix
   *   made SeaweedFS open seven fresh volumes stamped `ttl:260` (7 days). An expired TTL volume is
   *   deleted as a FILE — no vacuum, no copy, no free-space requirement — which is also the only
   *   thing that fixes this deployment's real amplification: ~1 GiB of live objects occupying 26 GiB
   *   of preallocated volumes that never shrink.
   *
   *   AND IT COSTS NO RUNTIME AT ALL. The sweep's LIST of 254,801 objects takes 107 seconds every
   *   time it runs. This is one idempotent PUT at boot.
   *
   * ── WHY IT IS SAFE, AND WHY IT CANNOT REACH `cas/` ──────────────────────────────────────────────
   *
   * {@link UNITS_LIFECYCLE_PREFIX} is a module constant and the ONLY prefix this ever names. That is
   * a stronger guarantee than the sweeper's, not a weaker one: there is no delete path in this
   * process to get wrong, no listing to mis-filter, and nothing a caller can pass. Verified on the
   * live store — an object written under `units/` came back `ttlSec: 604800`, one written under
   * `cas/` came back `ttlSec: 0`. CAS keys are bare content hashes shared across runs and tenants,
   * where age tells you nothing about liveness (ADR 0001, ADR 0007); an age rule there WILL delete
   * blobs live runs still reference.
   *
   * THE WINDOW IS WHAT MAKES IT SAFE AGAINST A RETRY, and this is the one thing the store cannot
   * know. `units/` is the SOURCE materialization reads — `activities/datasets.ts:resolveBatch` GETs
   * every blob and fails the dispatch if one is missing. A blind rule cannot check the ledger, so
   * the safety has to come from the gap: materialization retries are bounded by a Temporal retry
   * policy measured in HOURS, and this window is measured in DAYS. Do not set it below one day.
   *
   * ── IT ONLY BINDS OBJECTS WRITTEN AFTER IT IS SET ───────────────────────────────────────────────
   *
   * A filer TTL is stamped on the entry at write time, so the 254,801 objects already on this box
   * carry `ttlSec: 0` and this rule will never touch them. That backlog is what `sweepUnits` is for,
   * along with stores that have no lifecycle support at all. The division is deliberate: the store
   * owns the steady state, the sweep owns the backlog and the exceptions.
   *
   * BEST-EFFORT, like {@link ensureCors}. A store that refuses the call still serves every read and
   * write; it just keeps its unit blobs until something else collects them.
   */
  private async ensureLifecycle(): Promise<void> {
    const days = unitsLifecycleDays();
    if (days === null) return; // no policy stated — do not impose one
    try {
      await this.client().send(
        new PutBucketLifecycleConfigurationCommand({
          Bucket: this.bucket,
          LifecycleConfiguration: {
            Rules: [
              {
                ID: UNITS_LIFECYCLE_RULE_ID,
                Status: 'Enabled',
                Filter: { Prefix: UNITS_LIFECYCLE_PREFIX },
                Expiration: { Days: days },
              },
            ],
          },
        })
      );
    } catch {
      // best-effort — an older S3, or one that denies PutLifecycleConfiguration, still works.
    }
  }

  /** S3 client whose endpoint is the browser-facing host, for signing presigned URLs. */
  private presignClient(): S3Client {
    if (this.presignS3 === null) {
      this.presignS3 = new S3Client({
        endpoint: this.publicEndpoint,
        region: this.region,
        credentials: { accessKeyId: this.accessKey, secretAccessKey: this.secretKey },
        forcePathStyle: true,
      });
    }
    return this.presignS3;
  }

  private client(): S3Client {
    if (this.s3 === null) {
      this.s3 = new S3Client({
        endpoint: this.endpoint,
        region: this.region,
        credentials: {
          accessKeyId: this.accessKey,
          secretAccessKey: this.secretKey,
        },
        // SeaweedFS requires path-style addressing.
        forcePathStyle: true,
      });
    }
    return this.s3;
  }

  private async ensureBucket(): Promise<void> {
    if (this.bucketEnsured) return;
    this.bucketEnsured = true;
    try {
      await this.client().send(new CreateBucketCommand({ Bucket: this.bucket }));
    } catch {
      // already exists / not authorized to create — best-effort, ignore.
    }
    await this.ensureCors(); // let the browser range-read parquet cross-origin
    await this.ensureLifecycle(); // hand `units/` expiry to the store, not to a sweeper
  }

  async put(key: string, data: Uint8Array): Promise<void> {
    if (this.backing) return this.backing.put(key, data);
    await this.ensureBucket();
    await this.client().send(
      new PutObjectCommand({ Bucket: this.bucket, Key: key, Body: data })
    );
  }

  /** Bytes at `key`, or null if it does not exist. */
  async get(key: string): Promise<Uint8Array | null> {
    if (this.backing) return this.backing.get(key);
    await this.ensureBucket();
    try {
      const res = await this.client().send(
        new GetObjectCommand({ Bucket: this.bucket, Key: key })
      );
      if (!res.Body) return null;
      // Node stream / blob -> bytes.
      return await res.Body.transformToByteArray();
    } catch (err) {
      if (isMissing(err)) return null;
      throw err;
    }
  }

  /**
   * Remove an object. Used only by retention sweeps (`data/maintenance.ts`), never on the
   * execution path — nothing in a run deletes its own evidence. A backing store without
   * `remove` is a no-op rather than an error, so a test store cannot fail a sweep.
   */
  async delete(key: string): Promise<void> {
    if (this.backing) {
      await this.backing.remove?.(key);
      return;
    }
    if (!this.endpoint) return;
    await this.ensureBucket();
    await this.client().send(new DeleteObjectCommand({ Bucket: this.bucket, Key: key }));
  }

  /**
   * Remove many objects, batched. Returns how many the store confirmed gone.
   *
   * WHY THIS EXISTS AND `delete` IN A LOOP DOES NOT DO: see {@link DELETE_BATCH_MAX}. A units sweep
   * deletes objects by the hundred thousand, and per-key round trips do not fit in an activity.
   *
   * IT COUNTS WHAT THE STORE CONFIRMED, NEVER WHAT IT WAS ASKED. `DeleteObjects` is partial-success
   * by design — it answers with a `Deleted` list AND an `Errors` list, and a sweep that reported its
   * input length would claim to have freed space it did not free, which is the one lie a storage
   * report must not tell. Errors are returned rather than thrown: one denied key must not abandon
   * the other 999, and the caller decides whether a partial sweep is worth reporting or failing.
   *
   * A BACKING STORE WITHOUT `remove` DELETES NOTHING AND SAYS SO — zero, not the input length. That
   * keeps a test store's sweep honest in the same direction as the live one.
   */
  async deleteMany(keys: readonly string[]): Promise<{ deleted: number; errors: string[] }> {
    if (keys.length === 0) return { deleted: 0, errors: [] };
    if (this.backing) {
      if (!this.backing.remove) return { deleted: 0, errors: [] };
      let deleted = 0;
      for (const key of keys) {
        await this.backing.remove(key);
        deleted += 1;
      }
      return { deleted, errors: [] };
    }
    if (!this.endpoint) return { deleted: 0, errors: [] };
    await this.ensureBucket();
    let deleted = 0;
    const errors: string[] = [];
    for (let i = 0; i < keys.length; i += DELETE_BATCH_MAX) {
      const batch = keys.slice(i, i + DELETE_BATCH_MAX);
      const res = await this.client().send(
        new DeleteObjectsCommand({
          Bucket: this.bucket,
          // `Quiet` would suppress the `Deleted` list, which is exactly the list this counts.
          Delete: { Objects: batch.map((Key) => ({ Key })), Quiet: false },
        })
      );
      deleted += (res.Deleted ?? []).length;
      for (const e of res.Errors ?? []) errors.push(`${e.Key}: ${e.Code} ${e.Message}`);
    }
    return { deleted, errors };
  }

  async exists(key: string): Promise<boolean> {
    if (this.backing) return this.backing.exists(key);
    await this.ensureBucket();
    try {
      await this.client().send(
        new HeadObjectCommand({ Bucket: this.bucket, Key: key })
      );
      return true;
    } catch {
      return false;
    }
  }
}

function stripSlashes(s: string): string {
  let start = 0;
  let end = s.length;
  while (start < end && s[start] === '/') start += 1;
  while (end > start && s[end - 1] === '/') end -= 1;
  return s.slice(start, end);
}

/** Convenience: an in-memory {@link BackingStore} for tests and local runs. */
export class MemoryStore implements BackingStore {
  readonly map = new Map<string, Uint8Array>();

  async get(key: string): Promise<Uint8Array | null> {
    const v = this.map.get(key);
    return v === undefined ? null : v;
  }

  async put(key: string, data: Uint8Array): Promise<void> {
    this.map.set(key, data);
  }

  async exists(key: string): Promise<boolean> {
    return this.map.has(key);
  }

  async list(prefix: string): Promise<string[]> {
    return [...this.map.keys()].filter((k) => k.startsWith(prefix));
  }

  async remove(key: string): Promise<void> {
    this.map.delete(key);
  }
}
