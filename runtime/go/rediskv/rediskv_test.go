package rediskv

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/medmahmoudi26/kontra-local/runtime/go/globalstore"
)

// EtagKV against a REAL Redis — the Go peer of tests/test_redis_kv.py.
//
// runtime/go/globalstore's tests already prove the CAS retry logic against a fake. What a fake
// cannot prove is that the ADAPTER is genuinely atomic: that is a property of the Lua script
// running server-side, and a fake that simply does what it is told would pass while the real
// thing lost updates. The concurrency tests below are the ones that matter.
//
// Compose does NOT publish Redis on localhost, so run these with:
//
//	KONTRA_REDIS_HOST=<controller>:6379 GOWORK=off go test ./rediskv/   (from runtime/go)

func kvOrSkip(t *testing.T) (*EtagKV, *redis.Client) {
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
	return New(c), c
}

func key(t *testing.T) string { return "kontra-test:go:" + t.Name() }

func TestAbsentKeyReadsAsNilWithEmptyEtag(t *testing.T) {
	kv, c := kvOrSkip(t)
	defer c.Close()
	ctx := context.Background()
	defer c.Del(ctx, key(t))

	data, etag, err := kv.Get(ctx, key(t))
	if err != nil {
		t.Fatal(err)
	}
	if data != nil || etag != "" {
		t.Errorf("absent key must read (nil, \"\"), got (%q, %q)", data, etag)
	}
}

func TestCreateIsFirstWriteWins(t *testing.T) {
	kv, c := kvOrSkip(t)
	defer c.Close()
	ctx := context.Background()
	defer c.Del(ctx, key(t))

	// Two creators race; exactly one wins. Without this a second actor silently overwrites a
	// freshly created dedupe set.
	ok, err := kv.TrySave(ctx, key(t), []byte("a"), "")
	if err != nil || !ok {
		t.Fatalf("first create must win: ok=%v err=%v", ok, err)
	}
	if ok, _ := kv.TrySave(ctx, key(t), []byte("b"), ""); ok {
		t.Fatal("a concurrent creator must LOSE, not overwrite")
	}
	data, etag, _ := kv.Get(ctx, key(t))
	if string(data) != "a" {
		t.Errorf("loser clobbered the winner: %q", data)
	}
	if etag == "" {
		t.Error("a stored key must expose a non-empty etag or CAS can never target it")
	}
}

func TestStaleEtagIsRejected(t *testing.T) {
	kv, c := kvOrSkip(t)
	defer c.Close()
	ctx := context.Background()
	defer c.Del(ctx, key(t))

	_, _ = kv.TrySave(ctx, key(t), []byte("v1"), "")
	_, etag1, _ := kv.Get(ctx, key(t))
	if ok, _ := kv.TrySave(ctx, key(t), []byte("v2"), etag1); !ok {
		t.Fatal("a current etag must win")
	}
	// etag1 is now stale — a writer holding it must lose, not clobber v2.
	if ok, _ := kv.TrySave(ctx, key(t), []byte("v3"), etag1); ok {
		t.Fatal("a stale etag must lose")
	}
	if data, _, _ := kv.Get(ctx, key(t)); string(data) != "v2" {
		t.Errorf("stale writer clobbered the winner: %q", data)
	}
}

// TestUnconditionalPutInvalidatesAHeldEtag: Set is last-write-wins, but it must still bump the
// version — otherwise a CAS holder that missed the Set would overwrite it while believing nothing
// had changed.
func TestUnconditionalPutInvalidatesAHeldEtag(t *testing.T) {
	kv, c := kvOrSkip(t)
	defer c.Close()
	ctx := context.Background()
	defer c.Del(ctx, key(t))

	_, _ = kv.TrySave(ctx, key(t), []byte("v1"), "")
	_, etag, _ := kv.Get(ctx, key(t))

	if err := kv.Put(ctx, key(t), []byte("clobbered")); err != nil {
		t.Fatal(err)
	}
	if ok, _ := kv.TrySave(ctx, key(t), []byte("stale-cas"), etag); ok {
		t.Fatal("a CAS holding the pre-Put etag must lose")
	}
	if data, _, _ := kv.Get(ctx, key(t)); string(data) != "clobbered" {
		t.Errorf("Put did not stick: %q", data)
	}
}

// globalstore.Store retries a contended CAS 16 times before giving up. Concurrency at or below
// that budget must be lossless; above it the contract is a LOUD failure, never a silent one.
const withinBudget = 12

// TestConcurrentIncrementsLoseNothing is the real test: a lost update shows up as a smaller
// number, which is exactly what a fake KV cannot catch.
func TestConcurrentIncrementsLoseNothing(t *testing.T) {
	kv, c := kvOrSkip(t)
	defer c.Close()
	ctx := context.Background()

	gs := globalstore.New(kv, "race-go-incr-"+t.Name())
	defer c.Del(ctx, "kontra-global:race-go-incr-"+t.Name()+":counter")

	var wg sync.WaitGroup
	errs := make(chan error, withinBudget)
	for i := 0; i < withinBudget; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := gs.Incr(ctx, "counter", 1); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent incr failed: %v", err)
	}

	var got int
	if _, err := gs.Get(ctx, "counter", &got); err != nil {
		t.Fatal(err)
	}
	if got != withinBudget {
		t.Errorf("lost updates: counter = %d, want %d", got, withinBudget)
	}
}

// TestConcurrentSetAddsKeepEveryMember: add_to_set is the dedupe flagship — concurrent adds must
// all survive, and a re-add must report not-new.
func TestConcurrentSetAddsKeepEveryMember(t *testing.T) {
	kv, c := kvOrSkip(t)
	defer c.Close()
	ctx := context.Background()

	gs := globalstore.New(kv, "race-go-set-"+t.Name())
	defer c.Del(ctx, "kontra-global:race-go-set-"+t.Name()+":seen")

	var wg sync.WaitGroup
	for i := 0; i < withinBudget; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if added, err := gs.AddToSet(ctx, "seen", fmt.Sprintf("m%d", i)); err != nil || !added {
				t.Errorf("m%d: added=%v err=%v (every distinct member is a NEW add)", i, added, err)
			}
		}(i)
	}
	wg.Wait()

	var stored []string
	if _, err := gs.Get(ctx, "seen", &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != withinBudget {
		t.Errorf("dropped members: got %d, want %d (%v)", len(stored), withinBudget, stored)
	}
	if added, err := gs.AddToSet(ctx, "seen", "m0"); err != nil || added {
		t.Errorf("a re-add must report not-new: added=%v err=%v", added, err)
	}
}

// TestCrossSDKKeyLayout pins the bytes a Python actor would read. Both SDKs write the same tier;
// a Go actor and a Python actor sharing a dedupe set is the whole point of it being global.
func TestCrossSDKKeyLayout(t *testing.T) {
	kv, c := kvOrSkip(t)
	defer c.Close()
	ctx := context.Background()

	gs := globalstore.New(kv, "layout-go")
	const want = "kontra-global:layout-go:findings"
	defer c.Del(ctx, want)

	if err := gs.Set(ctx, "findings", []string{"x"}); err != nil {
		t.Fatal(err)
	}
	// The exact key, the exact hash fields — this is what redis_kv.py writes and what
	// backend/src/state.ts reads for the global tier.
	if n, _ := c.Exists(ctx, want).Result(); n != 1 {
		t.Fatalf("global_state key is not %q", want)
	}
	if typ, _ := c.Type(ctx, want).Result(); typ != "hash" {
		t.Errorf("global_state entry must be a hash, got %q", typ)
	}
	if err := c.HGet(ctx, want, "data").Err(); err != nil {
		t.Errorf("value must live in the `data` field: %v", err)
	}
	if err := c.HGet(ctx, want, "ver").Err(); err != nil {
		t.Errorf("etag must live in the `ver` field: %v", err)
	}
}
