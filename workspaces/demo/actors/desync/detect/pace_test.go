package detect

import (
	"sync"
	"testing"
	"time"
)

// The Scanner is shared across concurrently scanned Units by design (the pacer is per-HOST and only means
// something if it is shared). Before it was mutex-guarded this was a concurrent map write, which
// Go turns into a hard process crash — a panicking worker, not a failed unit.
func TestPaceIsSafeUnderConcurrentUnits(t *testing.T) {
	o := DefaultOptions()
	o.Rate = 0 // exercise the locking, not the sleeping
	s := New(o)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.pace([]string{"a.example", "b.example", "c.example"}[j%3])
			}
		}(i)
	}
	wg.Wait()
}

// Rate is a minimum gap PER HOST. Two hosts must not queue behind each other: holding the lock
// across the sleep would turn a per-host limit into a global one.
func TestPaceDoesNotSerializeDistinctHosts(t *testing.T) {
	o := DefaultOptions()
	o.Rate = 300 * time.Millisecond
	s := New(o)

	s.pace("a.example") // prime both
	s.pace("b.example")

	start := time.Now()
	var wg sync.WaitGroup
	for _, h := range []string{"a.example", "b.example"} {
		wg.Add(1)
		go func(h string) { defer wg.Done(); s.pace(h) }(h)
	}
	wg.Wait()
	if el := time.Since(start); el > 550*time.Millisecond {
		t.Errorf("distinct hosts serialized: two paced calls took %v, want ~one Rate", el)
	}
}

// The same host must actually be spaced — the reservation in pace() is what stops a second
// caller reading a stale timestamp and firing immediately.
func TestPaceSpacesTheSameHost(t *testing.T) {
	o := DefaultOptions()
	o.Rate = 200 * time.Millisecond
	s := New(o)

	s.pace("a.example")
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.pace("a.example") }()
	}
	wg.Wait()
	// 3 more hits at 200ms apart => the last lands no earlier than ~600ms.
	if el := time.Since(start); el < 500*time.Millisecond {
		t.Errorf("same host not paced: 3 calls took %v, want >= ~3x Rate", el)
	}
}
