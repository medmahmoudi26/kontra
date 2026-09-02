// Package globalstore holds the PURE global_state logic (ADR 0015 tier 3): actor-name-scoped
// atomic ops over an ETag key/value seam. It imports no store, so its concurrency logic is
// unit-tested with a fake KV; the real adapter is runtime/go/rediskv. This is the Go peer of
// Python's internals/globalstore.GlobalStore.
package globalstore

import (
	"context"
	"encoding/json"
	"fmt"
)

// casRetries bounds an optimistic-CAS loop before giving up loud (peer of Python's _CAS_RETRIES).
const casRetries = 16

// EtagKV is the minimal ETag key/value seam the atomics need. The real impl wraps a Redis
// store; tests inject an in-memory fake.
type EtagKV interface {
	// Get returns (data, etag). data is nil when absent; etag is the read version ("" if none).
	Get(ctx context.Context, key string) (data []byte, etag string, err error)
	// Put writes unconditionally (last-write-wins) — for Set.
	Put(ctx context.Context, key string, data []byte) error
	// TrySave writes only if the store's current etag still matches; false on an ETag conflict.
	TrySave(ctx context.Context, key string, data []byte, etag string) (bool, error)
}

// Store is the author-facing global_state ops over an EtagKV, actor-name-scoped.
type Store struct {
	kv     EtagKV
	prefix string
}

// New builds a Store scoped by actor NAME (not name+version), so a version bump shares state.
func New(kv EtagKV, actorName string) *Store {
	if actorName == "" {
		actorName = "actor"
	}
	return &Store{kv: kv, prefix: "kontra-global:" + actorName + ":"}
}

func (s *Store) k(key string) string { return s.prefix + key }

func (s *Store) Get(ctx context.Context, key string, out any) (bool, error) {
	data, _, err := s.kv.Get(ctx, s.k(key))
	if err != nil || data == nil {
		return false, err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return false, err
	}
	return true, nil
}

// Set is last-write-wins. Use the atomic ops below when concurrent sessions race a key.
func (s *Store) Set(ctx context.Context, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.kv.Put(ctx, s.k(key), data)
}

// AddToSet atomically adds member to the SET at key (the dedupe flagship). Returns true if it
// was newly added, false if already present. Never loses a concurrent add (ETag CAS).
func (s *Store) AddToSet(ctx context.Context, key string, member any) (bool, error) {
	k := s.k(key)
	mj, err := json.Marshal(member)
	if err != nil {
		return false, err
	}
	for i := 0; i < casRetries; i++ {
		data, etag, err := s.kv.Get(ctx, k)
		if err != nil {
			return false, err
		}
		var members []json.RawMessage
		if data != nil {
			if err := json.Unmarshal(data, &members); err != nil {
				return false, err
			}
		}
		for _, m := range members {
			if equalJSON(m, mj) {
				return false, nil // already present
			}
		}
		next, err := json.Marshal(append(members, json.RawMessage(mj)))
		if err != nil {
			return false, err
		}
		ok, err := s.kv.TrySave(ctx, k, next, etag)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, fmt.Errorf("global_state.AddToSet(%q): ETag contention exhausted", key)
}

// Incr atomically adds by to the integer counter at key; returns the new value. Never loses a
// concurrent increment (ETag CAS).
func (s *Store) Incr(ctx context.Context, key string, by int) (int, error) {
	k := s.k(key)
	for i := 0; i < casRetries; i++ {
		data, etag, err := s.kv.Get(ctx, k)
		if err != nil {
			return 0, err
		}
		n := 0
		if data != nil {
			if err := json.Unmarshal(data, &n); err != nil {
				return 0, err
			}
		}
		n += by
		nj, err := json.Marshal(n)
		if err != nil {
			return 0, err
		}
		ok, err := s.kv.TrySave(ctx, k, nj, etag)
		if err != nil {
			return 0, err
		}
		if ok {
			return n, nil
		}
	}
	return 0, fmt.Errorf("global_state.Incr(%q): ETag contention exhausted", key)
}

// CompareAndSet sets key to newVal only if its current value equals expected. Returns true on
// success, false if the value differed or a concurrent writer won the ETag race.
func (s *Store) CompareAndSet(ctx context.Context, key string, expected, newVal any) (bool, error) {
	k := s.k(key)
	data, etag, err := s.kv.Get(ctx, k)
	if err != nil {
		return false, err
	}
	cur := data
	if cur == nil {
		cur = []byte("null") // absent == null, so CompareAndSet(key, nil, v) creates it
	}
	ej, err := json.Marshal(expected)
	if err != nil {
		return false, err
	}
	if !equalJSON(cur, ej) {
		return false, nil
	}
	nj, err := json.Marshal(newVal)
	if err != nil {
		return false, err
	}
	return s.kv.TrySave(ctx, k, nj, etag)
}

// equalJSON compares two JSON values semantically (re-marshal to a canonical form), so member
// dedupe and compare-and-set don't hinge on byte-for-byte formatting.
func equalJSON(a, b []byte) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return string(a) == string(b)
	}
	am, _ := json.Marshal(av)
	bm, _ := json.Marshal(bv)
	return string(am) == string(bm)
}
