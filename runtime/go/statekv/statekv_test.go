package statekv

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestKeyMatchesThePythonPeer pins the ONE string three systems agree on.
//
// runtime/python/internals/statekv.py writes `kontra-actor:{actor_id}`, and
// backend/src/state.ts reads it to project the operator-facing state tiers. A drift here
// does not error: the dashboard and `kontra runs --state` return an empty result, which is
// indistinguishable from an actor that simply has no state. That exact failure has happened once
// already, when the layout moved and the reader kept scanning for the previous key shape.
func TestKeyMatchesThePythonPeer(t *testing.T) {
	const id = "13c641da-a2a5-4625-8abe-da491b6be157-n1"
	if got, want := Key(id), "kontra-actor:"+id; got != want {
		t.Errorf("Key(%q) = %q, want %q", id, got, want)
	}
}

// redisOrSkip dials the store these tests need. It skips rather than fails when there is none —
// but note the address: compose does NOT publish Redis on localhost, so run these with
//
//	KONTRA_REDIS_HOST=<controller>:6379 GOWORK=off go test ./statekv/   (from runtime/go)
func redisOrSkip(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("KONTRA_REDIS_HOST")
	if addr == "" {
		addr = "localhost:6379"
	}
	c := redis.NewClient(&redis.Options{Addr: addr})
	if err := c.Ping(context.Background()).Err(); err != nil {
		_ = c.Close()
		t.Skipf("no Redis at %s (set KONTRA_REDIS_HOST): %v", addr, err)
	}
	return c
}

func TestRoundTripAndAbsence(t *testing.T) {
	c := redisOrSkip(t)
	defer c.Close()
	ctx := context.Background()
	kv := New(c, "test-"+t.Name(), time.Minute)
	defer kv.Drop(ctx)

	// Absence is signalled by the field missing, never by a sentinel — a unit may legitimately
	// commit a null, and a sentinel would make "committed nothing" look like "never ran".
	if ok, err := kv.Contains(ctx, "u0"); err != nil || ok {
		t.Fatalf("fresh actor must hold nothing: ok=%v err=%v", ok, err)
	}
	var out map[string]any
	if err := kv.Get(ctx, "u0", &out); !IsMissing(err) {
		t.Fatalf("missing field must report missing, got %v", err)
	}

	if err := kv.SetWithTTL(ctx, "u0", map[string]any{"out": []any{"a"}}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if ok, _ := kv.Contains(ctx, "u0"); !ok {
		t.Fatal("field must exist after a write")
	}
	if err := kv.Get(ctx, "u0", &out); err != nil {
		t.Fatal(err)
	}
	if len(out["out"].([]any)) != 1 {
		t.Errorf("round-trip lost the payload: %v", out)
	}

	// A stored null is a real value, distinct from absence.
	if err := kv.SetWithTTL(ctx, "u1", nil, time.Minute); err != nil {
		t.Fatal(err)
	}
	if ok, _ := kv.Contains(ctx, "u1"); !ok {
		t.Error("a committed null must still be PRESENT")
	}
}

// TestTTLCoversEveryFieldAtOnce is the property the hash layout exists for: one EXPIRE slides an
// actor's whole state, instead of N round trips re-writing every live key to move a clock.
func TestTTLCoversEveryFieldAtOnce(t *testing.T) {
	c := redisOrSkip(t)
	defer c.Close()
	ctx := context.Background()
	kv := New(c, "test-"+t.Name(), 30*time.Second)
	defer kv.Drop(ctx)

	for _, f := range []string{"s0-u0", "s0-u0-ckpt", "s-index"} {
		if err := kv.SetWithTTL(ctx, f, "v", 30*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	ttl, err := c.TTL(ctx, Key("test-"+t.Name())).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 || ttl > 30*time.Second {
		t.Fatalf("expected a live TTL <= 30s, got %v", ttl)
	}

	// Touch slides ALL of it forward, including fields written before the last write.
	time.Sleep(1100 * time.Millisecond)
	if err := kv.Touch(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := c.TTL(ctx, Key("test-"+t.Name())).Result()
	if after <= ttl-time.Second {
		t.Errorf("Touch did not slide the TTL forward: %v -> %v", ttl, after)
	}
}

func TestRemoveAndDrop(t *testing.T) {
	c := redisOrSkip(t)
	defer c.Close()
	ctx := context.Background()
	id := "test-" + t.Name()
	kv := New(c, id, time.Minute)
	defer kv.Drop(ctx)

	_ = kv.SetWithTTL(ctx, "u0", 1, time.Minute)
	_ = kv.SetWithTTL(ctx, "u0-ckpt", 2, time.Minute)

	if err := kv.Remove(ctx, "u0-ckpt"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := kv.Contains(ctx, "u0-ckpt"); ok {
		t.Error("Remove left the field behind")
	}
	if ok, _ := kv.Contains(ctx, "u0"); !ok {
		t.Error("Remove took a sibling field with it")
	}

	// Drop is what a completed batch does: the commit map exists to make a RETRY skip finished
	// units, and a finished batch has no retry.
	if err := kv.Drop(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.Exists(ctx, Key(id)).Result(); n != 0 {
		t.Error("Drop left the actor hash behind")
	}
}

// TestSaveIsANoOpButWritesAlreadyLanded guards the one misleading spot: the engine calls Save
// after each commit, so a reader could reasonably assume writes are buffered until then.
func TestSaveIsANoOpButWritesAlreadyLanded(t *testing.T) {
	c := redisOrSkip(t)
	defer c.Close()
	ctx := context.Background()
	kv := New(c, "test-"+t.Name(), time.Minute)
	defer kv.Drop(ctx)

	if err := kv.SetWithTTL(ctx, "u0", "v", time.Minute); err != nil {
		t.Fatal(err)
	}
	// Visible to a SEPARATE client before Save is ever called.
	if err := c.HGet(ctx, Key("test-"+t.Name()), "u0").Err(); errors.Is(err, redis.Nil) {
		t.Error("the write had not landed before Save")
	}
	if err := kv.Save(ctx); err != nil {
		t.Errorf("Save must be a no-op, got %v", err)
	}
}
