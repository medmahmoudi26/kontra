package globalstore

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"testing"
)

// fakeKV is a goroutine-safe in-memory ETag store: key -> (data, version). TrySave succeeds
// only if the caller's etag still matches the current version, so real concurrent goroutines
// hit the CAS-retry path exactly as the real store does.
type fakeKV struct {
	mu   sync.Mutex
	data map[string][]byte
	ver  map[string]int
}

func newFakeKV() *fakeKV { return &fakeKV{data: map[string][]byte{}, ver: map[string]int{}} }

func (f *fakeKV) Get(_ context.Context, key string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.data[key]
	if !ok {
		return nil, "", nil
	}
	return append([]byte(nil), d...), strconv.Itoa(f.ver[key]), nil
}

func (f *fakeKV) Put(_ context.Context, key string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[key] = append([]byte(nil), data...)
	f.ver[key]++
	return nil
}

func (f *fakeKV) TrySave(_ context.Context, key string, data []byte, etag string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := ""
	if _, ok := f.data[key]; ok {
		cur = strconv.Itoa(f.ver[key])
	}
	if etag != cur {
		return false, nil // a concurrent writer won
	}
	f.data[key] = append([]byte(nil), data...)
	f.ver[key]++
	return true, nil
}

func TestGetSetAndActorNameScoping(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKV()
	gs := New(kv, "myactor")

	var out any
	if ok, _ := gs.Get(ctx, "k", &out); ok {
		t.Fatal("fresh key should be absent")
	}
	if err := gs.Set(ctx, "k", map[string]any{"v": 1}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if ok, _ := gs.Get(ctx, "k", &got); !ok || got["v"].(float64) != 1 {
		t.Fatalf("Get = (%v, %v), want v:1", got, ok)
	}
	if _, ok := kv.data["kontra-global:myactor:k"]; !ok {
		t.Fatalf("keys must be actor-name scoped; have %v", keys(kv))
	}
}

func TestAddToSetDedupes(t *testing.T) {
	ctx := context.Background()
	gs := New(newFakeKV(), "a")
	if added, _ := gs.AddToSet(ctx, "seen", "x"); !added {
		t.Fatal("first add should be new")
	}
	if added, _ := gs.AddToSet(ctx, "seen", "x"); added {
		t.Fatal("duplicate add should report not-new")
	}
	if added, _ := gs.AddToSet(ctx, "seen", "y"); !added {
		t.Fatal("second distinct add should be new")
	}
}

func TestIncrAndCompareAndSet(t *testing.T) {
	ctx := context.Background()
	gs := New(newFakeKV(), "a")
	if n, _ := gs.Incr(ctx, "c", 5); n != 5 {
		t.Fatalf("Incr = %d, want 5", n)
	}
	if n, _ := gs.Incr(ctx, "c", 3); n != 8 {
		t.Fatalf("Incr = %d, want 8", n)
	}
	if ok, _ := gs.CompareAndSet(ctx, "flag", nil, "on"); !ok { // nil == absent -> creates
		t.Fatal("CAS against absent(nil) should succeed")
	}
	if ok, _ := gs.CompareAndSet(ctx, "flag", "wrong", "x"); ok {
		t.Fatal("CAS with wrong expected should fail")
	}
	if ok, _ := gs.CompareAndSet(ctx, "flag", "on", "off"); !ok {
		t.Fatal("CAS with right expected should succeed")
	}
}

func TestConcurrentIncrLosesNoUpdates(t *testing.T) {
	ctx := context.Background()
	gs := New(newFakeKV(), "a")
	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = gs.Incr(ctx, "c", 1) }()
	}
	wg.Wait()
	var got int
	gs.Get(ctx, "c", &got)
	if got != n {
		t.Fatalf("counter = %d, want %d — CAS must not lose a concurrent increment", got, n)
	}
}

func TestConcurrentAddToSetLosesNoMembers(t *testing.T) {
	ctx := context.Background()
	gs := New(newFakeKV(), "a")
	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(v int) { defer wg.Done(); _, _ = gs.AddToSet(ctx, "s", v) }(i)
	}
	wg.Wait()
	var got []int
	gs.Get(ctx, "s", &got)
	sort.Ints(got)
	if len(got) != n {
		t.Fatalf("set = %v, want %d distinct members — CAS must not drop a concurrent add", got, n)
	}
}

func keys(f *fakeKV) []string {
	out := make([]string, 0, len(f.data))
	for k := range f.data {
		out = append(out, k)
	}
	return out
}
