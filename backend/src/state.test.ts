import { describe, expect, it } from 'vitest';
import {
  MAX_RESPONSE_BYTES,
  STATE_TIERS,
  capAndRedact,
  fieldInTier,
  isSensitiveKey,
  locate,
  requiresEntity,
} from './state';

/**
 * These pin the reader to what the SDK actually WRITES. An earlier version pinned the key shape
 * as a literal instead, and kept passing after the layout moved — a green suite over a projection
 * that could only ever return empty. So each expectation below names its writer.
 */
describe('locate', () => {
  it('reads the actor hash statekv.py writes', () => {
    // runtime/python/internals/statekv.py: actor_key() -> f"kontra-actor:{actor_id}"
    expect(locate('actor', 'subfinder', 'run7-n2')).toEqual({
      kind: 'hash',
      key: 'kontra-actor:run7-n2',
      field: undefined,
    });
  });

  it('narrows to one field when a key is named', () => {
    expect(locate('actor', 'subfinder', 'run7-n2', 'u17')).toEqual({
      kind: 'hash',
      key: 'kontra-actor:run7-n2',
      field: 'u17',
    });
  });

  it('puts every actor-scoped tier in the same hash — the tier is a field filter', () => {
    for (const t of ['arun', 'session', 'actor'] as const) {
      expect(locate(t, 'a', 'e')).toMatchObject({ kind: 'hash', key: 'kontra-actor:e' });
    }
  });

  it('scans the ETag keys redis_kv.py writes for global', () => {
    // internals/globalstore.py: prefix f"kontra-global:{actor_name}:"; value in field `data`.
    expect(locate('global', 'cachebuster', '', 'findings')).toEqual({
      kind: 'scan',
      pattern: 'kontra-global:cachebuster:findings',
      field: 'data',
      stripPrefix: 'kontra-global:cachebuster:',
    });
  });

  it('neutralizes glob metacharacters in the one tier that globs', () => {
    // A caller passing `*` must not turn a scoped read into an enumeration of every key.
    expect(locate('global', 'sub', '', '*')).toMatchObject({
      pattern: 'kontra-global:sub:\\*',
    });
    // The hash tiers take identity as an exact key, so a metacharacter cannot widen anything.
    expect(locate('actor', 'sub', '*')).toMatchObject({ key: 'kontra-actor:*' });
  });

  it('requires an entity for every actor-scoped tier but not for global', () => {
    expect(requiresEntity('global')).toBe(false);
    expect(requiresEntity('arun')).toBe(true);
    expect(requiresEntity('session')).toBe(true);
    expect(requiresEntity('actor')).toBe(true);
  });
});

describe('fieldInTier', () => {
  // The field names come from runtime/python/internals/engine.py: `u{i}`, `u{i}-ckpt`,
  // `u{i}-reloads`, `s-{key}`, and the reserved `s-index`.
  it('no longer recognizes the retired tiers by field naming', () => {
    // `s-` (session state) and `-ckpt` (per-unit scratch) were the only two field-name
    // conventions the SDKs shared, and both tiers retired with ADR 0023 §19. Nothing writes
    // those fields any more, so a projection that still filtered on them would present an
    // always-empty tier as though it were a real but idle one.
    expect(STATE_TIERS).toEqual(['global', 'actor']);
    for (const f of ['s-frontier', 's-index', 'u0-ckpt', 'u0', 'whatever']) {
      expect(fieldInTier('actor', f)).toBe(true);
    }
  });

  it('admits everything through the raw actor tier — a view that hides keys is not raw', () => {
    for (const f of ['u0', 'u0-ckpt', 'u0-reloads', 's-index', 'whatever']) {
      expect(fieldInTier('actor', f)).toBe(true);
    }
  });
});

describe('isSensitiveKey', () => {
  it('catches credential-shaped names case-insensitively', () => {
    for (const k of ['AWS_SECRET', 'db.password', 'x-api-key', 'authToken', 'Cookie']) {
      expect(isSensitiveKey(k), k).toBe(true);
    }
  });

  it('leaves ordinary state keys alone', () => {
    for (const k of ['u3-ckpt', 's-index', 'findings', 's0-u17']) {
      expect(isSensitiveKey(k), k).toBe(false);
    }
  });
});

describe('capAndRedact', () => {
  it('parses JSON values and reports byte size', () => {
    const { entries, truncated } = capAndRedact([{ key: 'u1-ckpt', value: '{"a":1}', ttl: 60 }]);
    expect(truncated).toBe(false);
    expect(entries[0]).toEqual({ key: 'u1-ckpt', bytes: 7, value: { a: 1 }, redacted: false, ttl: 60 });
  });

  it('returns a non-JSON value as a raw string rather than dropping it', () => {
    expect(capAndRedact([{ key: 'k', value: 'not json', ttl: -1 }]).entries[0].value).toBe(
      'not json'
    );
  });

  it('redacts by key name while keeping the row visible', () => {
    const { entries } = capAndRedact([{ key: 'api_key', value: '"hunter2"', ttl: -1 }]);
    expect(entries[0].redacted).toBe(true);
    expect(entries[0].value).toBeNull();
    // The key and its size still surface — the operator learns the value exists.
    expect(entries[0].key).toBe('api_key');
    expect(entries[0].bytes).toBe(9);
  });

  /**
   * A truncated response that silently omitted rows would look exactly like a complete one. That
   * is the failure this project keeps hitting (a wiped batch reporting as a clean `completed`),
   * so truncation must be visible in BOTH the flag and the surviving rows.
   */
  it('marks truncation and still lists the entries it dropped', () => {
    const big = 'x'.repeat(100);
    const { entries, truncated } = capAndRedact(
      [
        { key: 'a', value: big, ttl: -1 },
        { key: 'b', value: big, ttl: -1 },
        { key: 'c', value: big, ttl: -1 },
      ],
      150
    );
    expect(truncated).toBe(true);
    expect(entries).toHaveLength(3);
    expect(entries[0].value).toBe(big);
    expect(entries[1].value).toBeNull();
    expect(entries[1].bytes).toBe(100); // size still reported
    expect(entries[2].value).toBeNull();
  });

  it('never exceeds the cap in carried payload', () => {
    const rows = Array.from({ length: 50 }, (_, i) => ({
      key: `k${i}`,
      value: 'y'.repeat(10_000),
      ttl: -1,
    }));
    const { entries } = capAndRedact(rows);
    const carried = entries
      .filter((e) => e.value !== null)
      .reduce((n, e) => n + e.bytes, 0);
    expect(carried).toBeLessThanOrEqual(MAX_RESPONSE_BYTES);
  });
});
