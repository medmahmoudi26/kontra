/**
 * Read-only projection of the three state tiers, for operator inspection.
 *
 * WHY THE ORCHESTRATOR READS REDIS DIRECTLY
 *
 * The alternative was handing Grafana Redis credentials. Grafana is a UI with plugins and a
 * browser attack surface; the state store holds live scan data. Reading here keeps exactly one
 * process holding the credential and lets that process impose identity, bounds and redaction that
 * a raw Redis connection cannot.
 *
 * WHERE THE STATE ACTUALLY LIVES (ADR 0018)
 *
 * Two shapes, because the two tiers are scoped differently:
 *
 *   actor-scoped   kontra-actor:<entity>            ONE hash; each FIELD is a state key
 *   global         kontra-global:<actor>:<key>      one hash PER key; value in field `data`
 *
 * The actor hash is a hash precisely so its 24 h TTL can be slid forward with a single `EXPIRE`
 * (runtime/python/internals/statekv.py). That makes the whole of an actor id's state one `HGETALL`
 * — no scan, no key ceiling, and a consistent snapshot rather than a walk. `global` keeps a key per
 * entry because each carries its own ETag for compare-and-set (internals/redis_kv.py), so it is the
 * one tier that still scans.
 *
 * BOTH SDKs write the two shapes above byte-for-byte
 * (runtime/go/{statekv,rediskv}). So this projection is SDK-blind — a Go actor's state
 * reads here exactly as a Python actor's does, which is the property that makes one dashboard
 * enough. The field NAMES inside the actor hash still differ by SDK (`u{i}` vs `s{si}-u{i}`);
 * only the `-ckpt` suffix and the `s-` prefix are the shared contract, which is why the tier
 * filter below keys off those two and not off the whole string.
 *
 * Tier classification within the actor hash follows the SDK's own field naming: `s-` marks session
 * state, a `-ckpt` suffix marks per-unit resume scratch (`u{i}-ckpt`). Fields matching neither are
 * real state that simply is not one of the named tiers — they stay reachable through the `actor`
 * tier, because a raw-state view that hides keys is not raw.
 */

/**
 * `arun` and `session` were tiers here until ADR 0023 §19 retired them. Two durable tiers
 * remain — `global` (actor NAME) and the actor hash itself, which holds the framework's commit
 * map — and neither is recognised by a field-name convention, which is why `fieldInTier` no
 * longer needs one.
 */
export type StateTier = 'global' | 'actor';

export const STATE_TIERS: readonly StateTier[] = ['global', 'actor'];

/** Hard ceiling on bytes returned in one response, before truncation kicks in. Chosen so a
 *  pathological blob cannot pull the orchestrator's heap around: state values are checkpoints and
 *  small indexes, and anything approaching this is a bug worth seeing truncated. */
export const MAX_RESPONSE_BYTES = 256 * 1024;

/** Hard ceiling on keys examined per request. SCAN's MATCH filters AFTER the server walks the
 *  keyspace slice, so an unbounded scan is O(keyspace) no matter how narrow the pattern looks.
 *  This bounds the walk itself. */
export const MAX_KEYS_SCANNED = 5_000;

/**
 * Where one tier's state lives, as an instruction the reader executes without knowing the layout.
 *
 * `hash` is one Redis hash whose FIELDS are the state keys — read whole, then filtered by
 * `fieldInTier`. `scan` is a key per state key, each holding its value in one field.
 *
 * This is the seam: `stateStore.ts` learns whether to HGETALL or SCAN and nothing else. When the
 * layout changes again it changes a branch here, not the reader.
 */
export type StateLocation =
  | { kind: 'hash'; key: string; field?: string }
  | { kind: 'scan'; pattern: string; field: string; stripPrefix: string };

/**
 * Locate one tier, always rooted at caller-supplied identity.
 *
 * Nothing here can widen to "all actors" or "all entities", which is the property the endpoint's
 * no-unconstrained-scan requirement actually needs. `entity` is mandatory for the actor-scoped
 * tiers, and it is the WHOLE key there — a hash read cannot degrade into a walk at all.
 */
export function locate(
  tier: StateTier,
  actor: string,
  entity: string,
  key?: string
): StateLocation {
  if (tier === 'global') {
    // Entity is meaningless here: global state is scoped by actor NAME, across all sessions.
    const prefix = `kontra-global:${actor}:`;
    return {
      kind: 'scan',
      pattern: `${escapeGlob(prefix)}${key ? escapeGlob(key) : '*'}`,
      field: 'data',
      stripPrefix: prefix,
    };
  }
  // One hash per actor id. A named key is one HGET; otherwise the whole hash, filtered by tier.
  return { kind: 'hash', key: `kontra-actor:${entity}`, field: key };
}

/**
 * Whether a FIELD of the actor hash belongs to a tier.
 *
 * Everything, now. It used to recognise `s-` (session state) and `-ckpt` (per-unit scratch) —
 * the only two field-name conventions the SDKs shared — and both tiers retired with ADR 0023
 * §19. What is left in the hash is the framework's commit map, which the `actor` view shows
 * whole because a raw-state view that hides keys is not raw.
 *
 * Kept as a function rather than inlined: the seam is where a future tier's naming would land,
 * and the reader (`stateStore.ts`) is deliberately ignorant of field conventions.
 */
export function fieldInTier(_tier: StateTier, _field: string): boolean {
  return true;
}

/** Whether this tier requires an `entity` — the actor id, which is the Session's key if it
 *  claimed one, else the Session id, else `<runId>-<dispatchId>` (ADR 0023 §6, §10). */
export function requiresEntity(tier: StateTier): boolean {
  return tier !== 'global';
}

/**
 * Substrings that mark a value as unfit to render in a dashboard.
 *
 * This is a denylist, and a denylist is a floor rather than a guarantee — it catches credentials
 * that are NAMED like credentials. Actor state is application data whose shape this layer cannot
 * know, so the honest posture is: infrastructure secrets are blocked by name, and the endpoint is
 * token-gated because the rest cannot be.
 */
const REDACT_SUBSTRINGS = [
  'password',
  'passwd',
  'secret',
  'token',
  'credential',
  'apikey',
  'api_key',
  'api-key',
  'authorization',
  'auth_header',
  'cookie',
  'private_key',
  'privatekey',
  'access_key',
  'session_key',
];

/** True when a key's NAME marks its value as sensitive. Case-insensitive, substring match. */
export function isSensitiveKey(key: string): boolean {
  const k = key.toLowerCase();
  return REDACT_SUBSTRINGS.some((s) => k.includes(s));
}

export interface StateEntry {
  key: string;
  bytes: number;
  /** null when redacted or when the value was dropped to respect the byte cap. */
  value: unknown;
  redacted: boolean;
  /** Remaining TTL in seconds; -1 = no expiry, -2 = key is gone. Mirrors Redis TTL semantics. */
  ttl: number;
}

/**
 * Apply redaction and the response byte cap.
 *
 * Redacted and dropped entries keep their key and byte count. Silently omitting them would make a
 * truncated response indistinguishable from a complete one, which is the same class of mistake as
 * a zeroed isolated-unit counter: an operator cannot see what they were not told was missing.
 */
export function capAndRedact(
  raw: Array<{ key: string; value: string; ttl: number }>,
  maxBytes: number = MAX_RESPONSE_BYTES
): { entries: StateEntry[]; truncated: boolean } {
  const entries: StateEntry[] = [];
  let used = 0;
  let truncated = false;

  for (const r of raw) {
    const bytes = Buffer.byteLength(r.value, 'utf8');
    if (isSensitiveKey(r.key)) {
      entries.push({ key: r.key, bytes, value: null, redacted: true, ttl: r.ttl });
      continue;
    }
    if (used + bytes > maxBytes) {
      // Keep the row so the omission is visible, but do not carry the payload.
      entries.push({ key: r.key, bytes, value: null, redacted: false, ttl: r.ttl });
      truncated = true;
      continue;
    }
    used += bytes;
    entries.push({ key: r.key, bytes, value: parseMaybeJson(r.value), redacted: false, ttl: r.ttl });
  }
  return { entries, truncated };
}

/** State values are usually JSON, but nothing guarantees it — a non-JSON value is returned as the
 *  raw string rather than being dropped or throwing. */
function parseMaybeJson(v: string): unknown {
  try {
    return JSON.parse(v);
  } catch {
    return v;
  }
}

/** Neutralize glob metacharacters so a caller-supplied id cannot widen the pattern it is meant to
 *  constrain — the SCAN equivalent of parameterizing a query. */
function escapeGlob(s: string): string {
  return s.replace(/([*?[\]\\])/g, '\\$1');
}
