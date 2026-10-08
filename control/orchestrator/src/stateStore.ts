/**
 * Redis-backed reader for the raw state tiers. Pure key/redaction logic lives in `state.ts`; this
 * file is only the I/O around it.
 *
 * Read-only by construction: the client is never handed a write command, and the endpoint that
 * uses it exposes no mutation. Nothing here may throw into the request path — a state read is a
 * diagnostic, and a diagnostic that can take the API down defeats its own purpose (ADR 0005:
 * analytics is a side channel that must never block execution).
 */

import Redis from 'ioredis';
import {
  MAX_KEYS_SCANNED,
  MAX_RESPONSE_BYTES,
  type StateEntry,
  type StateTier,
  capAndRedact,
  fieldInTier,
  locate,
} from './state';

export interface StateReadResult {
  entries: StateEntry[];
  /** Keys matched before the byte cap was applied. */
  total: number;
  /** True when the byte cap dropped payloads OR the key-scan ceiling was hit. */
  truncated: boolean;
  /** Which host answered — so a dashboard can say where a snapshot came from. */
  source: string;
  /** Server-side observation time, epoch ms. */
  capturedAt: number;
}

export interface StateReader {
  read(tier: StateTier, actor: string, entity: string, key?: string): Promise<StateReadResult>;
  close(): Promise<void>;
}

/**
 * Build a reader, or null when no Redis host is configured — in which case the routes 503 rather
 * than pretending an empty result is an empty store. "No data" and "not wired up" must not look
 * the same.
 */
export function createStateReader(host = process.env.KONTRA_REDIS_HOST): StateReader | null {
  if (!host) return null;

  const [h, p] = splitHostPort(host);
  const client = new Redis({
    host: h,
    port: p,
    // OPTIONAL, AND WHERE IT MATTERS IS THE VPC. On the default stack Redis publishes on loopback
    // and loopback IS the control (ADR 0056); the VPC overlay publishes it to the fleet network so
    // a Machine can reach the state store, and at that point anything on that VPC can read every
    // actor's state with no credential. The overlay sets `requirepass` and this variable together.
    // `undefined` rather than `''` — ioredis sends AUTH for any defined value, and the embedded
    // state store implements no AUTH at all.
    ...(process.env.KONTRA_REDIS_PASSWORD ? { password: process.env.KONTRA_REDIS_PASSWORD } : {}),
    lazyConnect: true,
    // A dashboard refresh must not queue behind a dead Redis. Fail fast, surface the error.
    connectTimeout: 3_000,
    commandTimeout: 5_000,
    maxRetriesPerRequest: 1,
    enableOfflineQueue: false,
    retryStrategy: (times) => (times > 3 ? null : Math.min(times * 200, 1_000)),
  });
  // ioredis emits 'error' on an EventEmitter; without a listener Node turns that into an uncaught
  // exception and kills the API over a diagnostic endpoint's dependency.
  client.on('error', () => {});

  /**
   * Connect on first use.
   *
   * `lazyConnect` means the socket is not opened by the constructor, and `enableOfflineQueue:
   * false` means a command issued before it is open fails immediately rather than queueing — so
   * something has to open it. The in-flight promise is memoized to collapse concurrent first
   * requests into one dial, and CLEARED on failure: caching a rejected connect promise forever
   * is exactly the bug that disables dataset materialization for the life of the process
   * (control/orchestrator/src/data/parquet.ts:112,156), and it is not worth reproducing here.
   */
  let connecting: Promise<void> | null = null;
  const ensureConnected = (): Promise<void> => {
    if (client.status === 'ready') return Promise.resolve();
    if (!connecting) {
      connecting = client.connect().catch((err: unknown) => {
        connecting = null;
        throw err;
      });
    }
    return connecting;
  };

  return {
    async read(tier, actor, entity, key) {
      await ensureConnected();
      const capturedAt = Date.now();
      const loc = locate(tier, actor, entity, key);

      const { raw, hitCeiling } =
        loc.kind === 'hash'
          ? await readHash(client, tier, loc.key, loc.field)
          : await readScan(client, loc.pattern, loc.field, loc.stripPrefix);

      const { entries, truncated } = capAndRedact(raw, MAX_RESPONSE_BYTES);
      return {
        entries,
        total: raw.length,
        truncated: truncated || hitCeiling,
        source: `redis://${h}:${p}`,
        capturedAt,
      };
    },
    async close() {
      try {
        await client.quit();
      } catch {
        client.disconnect();
      }
    },
  };
}

type RawRows = { raw: Array<{ key: string; value: string; ttl: number }>; hitCeiling: boolean };

/**
 * The actor-scoped tiers: ONE hash, whose fields are the state keys.
 *
 * Two round trips regardless of how much state the actor holds, and no key ceiling — the read is
 * bounded by one actor id by construction. Every field shares the hash's TTL, which is the whole
 * reason it is a hash (statekv.py slides all of an actor's state with one `EXPIRE`), so the TTL is
 * fetched once and repeated per row rather than being per-key data the caller must not trust.
 */
async function readHash(
  client: Redis,
  tier: StateTier,
  key: string,
  field?: string
): Promise<RawRows> {
  const [fields, ttl] = await Promise.all([
    field
      ? client.hget(key, field).then((v) => (v === null ? {} : { [field]: v }))
      : client.hgetall(key),
    client.ttl(key),
  ]);

  const raw = Object.entries(fields ?? {})
    .filter(([f]) => fieldInTier(tier, f))
    .map(([f, value]) => ({ key: f, value, ttl }));
  // Stable order: HGETALL returns hash order, which is an implementation detail and makes two
  // identical dashboard refreshes look like they changed.
  raw.sort((a, b) => a.key.localeCompare(b.key));
  return { raw, hitCeiling: false };
}

/**
 * The global tier: a key per entry, each an ETag hash (`data` + `ver`).
 *
 * A plain GET here returns nil — the value lives in a field — which older code read as "no state",
 * an empty result indistinguishable from an empty store. Read the field explicitly.
 */
async function readScan(
  client: Redis,
  pattern: string,
  field: string,
  stripPrefix: string
): Promise<RawRows> {
  const keys = await scanBounded(client, pattern);
  const raw: Array<{ key: string; value: string; ttl: number }> = [];
  if (keys.matched.length > 0) {
    // Pipelined so N keys cost one round trip rather than 2N.
    const pipe = client.pipeline();
    for (const k of keys.matched) {
      pipe.hget(k, field);
      pipe.ttl(k);
    }
    const res = (await pipe.exec()) ?? [];
    keys.matched.forEach((k, i) => {
      const value = res[i * 2]?.[1];
      const ttl = res[i * 2 + 1]?.[1];
      // Expired between SCAN and read, or an unreadable shape — skip the row rather than failing
      // the whole request over one key.
      if (typeof value !== 'string') return;
      raw.push({
        key: k.startsWith(stripPrefix) ? k.slice(stripPrefix.length) : k,
        value,
        ttl: typeof ttl === 'number' ? ttl : -1,
      });
    });
  }
  return { raw, hitCeiling: keys.hitCeiling };
}

/**
 * SCAN with a hard ceiling on keys WALKED, not just keys returned.
 *
 * MATCH filters after Redis has already walked its slice of the keyspace, so a narrow-looking
 * pattern is still O(keyspace) on a large store. Counting cursor progress — not matches — is what
 * actually bounds the work, and `hitCeiling` is reported so a partial answer never reads as a
 * complete one.
 */
async function scanBounded(
  client: Redis,
  pattern: string
): Promise<{ matched: string[]; hitCeiling: boolean }> {
  const matched: string[] = [];
  let cursor = '0';
  let walked = 0;
  do {
    const [next, batch] = await client.scan(cursor, 'MATCH', pattern, 'COUNT', 500);
    cursor = next;
    walked += 500;
    matched.push(...batch);
    if (walked >= MAX_KEYS_SCANNED || matched.length >= MAX_KEYS_SCANNED) {
      return { matched, hitCeiling: cursor !== '0' };
    }
  } while (cursor !== '0');
  return { matched, hitCeiling: false };
}

function splitHostPort(hostPort: string): [string, number] {
  const i = hostPort.lastIndexOf(':');
  if (i === -1) return [hostPort, 6379];
  const port = Number(hostPort.slice(i + 1));
  return [hostPort.slice(0, i), Number.isFinite(port) ? port : 6379];
}
