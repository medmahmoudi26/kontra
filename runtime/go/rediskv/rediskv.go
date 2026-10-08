// Package rediskv is the real implementation of global_state's EtagKV seam (ADR 0015) — the Go
// peer of runtime/python/internals/redis_kv.py, writing byte-identical keys.
//
// globalstore.Store knows none of this: its CAS retry loops are tested against a fake, and the
// first-write-wins-on-create rule is enforced here, on the server.
//
// Why Lua rather than WATCH/MULTI: the compare-and-set must be atomic ON THE SERVER. A
// read-then-write from the client is exactly the lost-update race the atomics exist to prevent,
// and WATCH/MULTI costs an extra round trip per attempt while still needing retry handling for
// the aborted-transaction case. A single EVALSHA is one hop and cannot interleave.
//
// Redis has no native ETag, so a version counter supplies one. Each key is a hash:
//
//	key -> { data: <bytes>, ver: <monotonic int as string> }
//
// `ver` is the etag. Every write bumps it, INCLUDING the unconditional Put, so a CAS holder that
// missed a last-write-wins Set correctly loses its race instead of silently clobbering it.
//
// The scripts return 0 or 1, so a lost race is a VALUE and a broken store is an ERROR. Deciding
// between them by matching an error string — which is what this replaced — misclassifies both.
package rediskv

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"
)

// Compare-and-set. Returns 1 on success, 0 on conflict.
//
//	KEYS[1] = key
//	ARGV[1] = data to write
//	ARGV[2] = expected etag; "" means "this key must not exist yet"
//
// The empty-etag branch is first-write-wins on create: a concurrent creator is a CONFLICT, not a
// silent overwrite. Dropping that would let two actors both believe they created a dedupe set.
// Byte-identical to _CAS_LUA in redis_kv.py — the two SDKs must not disagree about who won.
var casScript = redis.NewScript(`
local cur = redis.call('HGET', KEYS[1], 'ver')
if ARGV[2] == '' then
  if cur then return 0 end
  redis.call('HSET', KEYS[1], 'data', ARGV[1], 'ver', '1')
  return 1
end
if not cur or cur ~= ARGV[2] then return 0 end
redis.call('HSET', KEYS[1], 'data', ARGV[1], 'ver', tostring(tonumber(cur) + 1))
return 1
`)

// Unconditional write. Bumps ver so any in-flight CAS against the old version loses.
var putScript = redis.NewScript(`
redis.call('HSET', KEYS[1], 'data', ARGV[1])
redis.call('HINCRBY', KEYS[1], 'ver', 1)
return 1
`)

// EtagKV is the real globalstore.EtagKV over Redis. globalstore.Store cannot tell it from the
// in-memory fake its own tests use.
type EtagKV struct {
	c redis.Cmdable
}

// New wraps an existing client.
func New(c redis.Cmdable) *EtagKV { return &EtagKV{c: c} }

// FromEnv wires global_state to the shared Redis the fleet already points at. KONTRA_REDIS_HOST
// is `host:port` (the worker entrypoint and the machine install both set it to the Controller).
func FromEnv() *EtagKV {
	hostPort := os.Getenv("KONTRA_REDIS_HOST")
	if hostPort == "" {
		hostPort = "localhost:6379"
	}
	if !strings.Contains(hostPort, ":") {
		hostPort += ":6379"
	}
	// THE PASSWORD IS OPTIONAL, AND WHERE IT MATTERS IS THE VPC. On the default stack Redis
	// publishes on loopback and loopback IS the control (ADR 0056). The VPC overlay publishes it to
	// the fleet network so a Machine can reach the state store — and at that point anything on that
	// VPC can read every actor's state with no credential. `docker-compose.vpc.yml` sets
	// `requirepass` and this variable together. Unset means no auth, which is today's behaviour and
	// what the embedded state store expects.
	return New(redis.NewClient(&redis.Options{Addr: hostPort, Password: os.Getenv("KONTRA_REDIS_PASSWORD")}))
}

// Get returns (data, etag). data is nil when absent; etag is the read version ("" if none).
func (k *EtagKV) Get(ctx context.Context, key string) ([]byte, string, error) {
	vals, err := k.c.HMGet(ctx, key, "data", "ver").Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, "", nil
		}
		return nil, "", err
	}
	var data []byte
	var etag string
	if len(vals) > 0 && vals[0] != nil {
		if s, ok := vals[0].(string); ok && s != "" {
			data = []byte(s)
		}
	}
	if len(vals) > 1 && vals[1] != nil {
		if s, ok := vals[1].(string); ok {
			etag = s
		}
	}
	return data, etag, nil
}

// Put is an unconditional write (last-write-wins) — for Set.
func (k *EtagKV) Put(ctx context.Context, key string, data []byte) error {
	return putScript.Run(ctx, k.c, []string{key}, data).Err()
}

// TrySave is the compare-and-set: write only if the store's current etag still matches. Returns
// true on success, false on an ETag conflict (a concurrent writer won — the caller retries).
func (k *EtagKV) TrySave(ctx context.Context, key string, data []byte, etag string) (bool, error) {
	n, err := casScript.Run(ctx, k.c, []string{key}, data, etag).Int64()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Close releases the client when this KV owns one.
func (k *EtagKV) Close() error {
	if c, ok := k.c.(*redis.Client); ok {
		return c.Close()
	}
	return nil
}
