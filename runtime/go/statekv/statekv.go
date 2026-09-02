// Package statekv is the per-actor durable state the engine commits through — tiers 1 and 2 of
// the three-tier model (ADR 0015), over plain Redis (ADR 0018).
//
// The Go peer of runtime/python/internals/statekv.py, and deliberately a BYTE-COMPATIBLE one:
// both SDKs write the same hash, with the same field names and the same TTL, so the operator
// projection (control/orchestrator/src/state.ts) reads a Go actor's state exactly as it reads a Python
// actor's. The key SCHEMES inside the hash still differ by SDK on purpose — Python `u{i}`, Go
// `s{si}-u{i}` for the step pipeline — and the congruence contract is the `-ckpt` suffix and the
// `s-` prefix, not the whole string (see the congruence tests on both sides).
//
// # Layout
//
//	kontra-actor:{actorID}  ->  { "s0-u0": …, "s0-u0-ckpt": …, "s-index": …, "s-name": … }
//
// One hash rather than a key per field because the TTL is what makes it safe to leave state
// behind, and EXPIRE applies to the whole hash — so one call slides an actor's entire state
// forward, instead of re-writing every live key to move a clock.
//
// # On the Save() no-op
//
// Save is a no-op: every method below has already hit Redis by the time it returns. It stays in
// the interface because the engine calls it at every commit site, and because a store that DOES
// buffer (this one did once) can only be swapped in if the call is there. Deleting it would be a
// silent behaviour change at every one of those sites.
package statekv

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// TTL is how long an actor's state outlives its last write. Long enough to outlive any retry
// budget (MaxAttempts × StartToClose) and a same-day recover_run; short enough that abandoned
// runs do not accumulate forever. Peer of STATE_TTL_S in statekv.py.
const TTL = 24 * time.Hour

const prefix = "kontra-actor"

// Key is the Redis key holding one actor id's state. Exported because it is half of a
// cross-language contract, not an implementation detail.
func Key(actorID string) string { return prefix + ":" + actorID }

// KV is the per-actor state store. The client is injected so tests use a fake and the host
// builds one from the environment.
type KV struct {
	c   redis.Cmdable
	key string
	ttl time.Duration
}

// New binds a KV to one actor id.
func New(c redis.Cmdable, actorID string, ttl time.Duration) *KV {
	if ttl <= 0 {
		ttl = TTL
	}
	return &KV{c: c, key: Key(actorID), ttl: ttl}
}

// FromEnv builds a KV against the shared Redis the fleet already points at. KONTRA_REDIS_HOST is
// `host:port`; the machine install and the worker entrypoint both set it to the Controller, which
// is what makes a whole fleet share one state store instead of each Worker being an island.
func FromEnv(actorID string) *KV {
	hostPort := os.Getenv("KONTRA_REDIS_HOST")
	if hostPort == "" {
		hostPort = "localhost:6379"
	}
	if !strings.Contains(hostPort, ":") {
		hostPort += ":6379"
	}
	return New(redis.NewClient(&redis.Options{Addr: hostPort}), actorID, TTL)
}

// Contains reports whether a field is present. Absence is signalled by the field being missing,
// never by a sentinel — a unit may legitimately commit a null.
func (k *KV) Contains(ctx context.Context, field string) (bool, error) {
	n, err := k.c.HExists(ctx, k.key, field).Result()
	if err != nil {
		return false, err
	}
	return n, nil
}

// Get decodes a field into out. A missing field returns redis.Nil, which callers check with
// errors.Is.
func (k *KV) Get(ctx context.Context, field string, out any) error {
	raw, err := k.c.HGet(ctx, k.key, field).Bytes()
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// SetWithTTL writes a field and slides the whole actor's TTL forward.
func (k *KV) SetWithTTL(ctx context.Context, field string, val any, ttl time.Duration) error {
	b, err := json.Marshal(val)
	if err != nil {
		return err
	}
	// HSET then EXPIRE, not a pipeline: the write must land even if the TTL call fails. State
	// that outlives its TTL is recoverable; state that never landed is not.
	if err := k.c.HSet(ctx, k.key, field, b).Err(); err != nil {
		return err
	}
	if ttl <= 0 {
		ttl = k.ttl
	}
	return k.c.Expire(ctx, k.key, ttl).Err()
}

// Remove deletes a field.
func (k *KV) Remove(ctx context.Context, field string) error {
	return k.c.HDel(ctx, k.key, field).Err()
}

// Save is a no-op. See the package doc: every write above has already landed.
func (k *KV) Save(context.Context) error { return nil }

// Touch slides the whole actor's TTL forward. ONE call, because the TTL is on the hash.
func (k *KV) Touch(ctx context.Context) error {
	return k.c.Expire(ctx, k.key, k.ttl).Err()
}

// Drop removes everything for this actor id. The commit map exists to make a RETRY skip finished
// units, and a finished batch has no retry.
func (k *KV) Drop(ctx context.Context) error {
	return k.c.Del(ctx, k.key).Err()
}

// Close releases the client when this KV owns one.
func (k *KV) Close() error {
	if c, ok := k.c.(*redis.Client); ok {
		return c.Close()
	}
	return nil
}

// IsMissing reports whether an error means "no such field", so call sites do not have to import
// the redis package just to spell the sentinel.
func IsMissing(err error) bool { return errors.Is(err, redis.Nil) }
