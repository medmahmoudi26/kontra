package node

import (
	"testing"
	"time"
)

// The waits are drawn within [0, min(60 s, 2ⁿ s)] (full jitter, §13), and a session that lived 60 s resets
// them; a shorter one does not.
func TestBackoff(t *testing.T) {
	ceilings := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60}
	for i := range ceilings {
		ceilings[i] *= time.Second
	}

	// The top of each draw: just under the ceiling.
	top := Backoff{rand: func() float64 { return 0.999999 }}
	for n, want := range ceilings {
		got := top.Next()
		if got > want || got < want-want/1000 {
			t.Errorf("wait %d at the top of the draw = %v, want just under %v", n, got, want)
		}
	}
	// The bottom: 0, whatever n is.
	bottom := Backoff{rand: func() float64 { return 0 }}
	for n := range 10 {
		if got := bottom.Next(); got != 0 {
			t.Errorf("wait %d at the bottom of the draw = %v, want 0", n, got)
		}
	}
	// The default source stays within the bounds, and does not always draw the same.
	var b Backoff
	seen := map[time.Duration]bool{}
	for n := range 200 {
		ceil := ceilings[min(n, len(ceilings)-1)]
		if c := b.Ceiling(); c != ceil {
			t.Fatalf("draw %d: ceiling %v, want %v", n, c, ceil)
		}
		got := b.Next()
		if got < 0 || got > ceil {
			t.Fatalf("draw %d = %v, outside [0, %v]", n, got, ceil)
		}
		seen[got] = true
	}
	if len(seen) < 100 {
		t.Errorf("only %d distinct waits in 200 draws: no jitter", len(seen))
	}

	// A session of 59 s does not reset the backoff; one of 60 s does.
	mid := Backoff{rand: func() float64 { return 0.999999 }}
	for range 5 {
		mid.Next()
	}
	mid.SessionEnded(59 * time.Second)
	if c := mid.Ceiling(); c != 32*time.Second {
		t.Errorf("after a 59 s session the ceiling is %v, want 32s", c)
	}
	mid.SessionEnded(BackoffReset)
	if c := mid.Ceiling(); c != time.Second {
		t.Errorf("after a 60 s session the ceiling is %v, want 1s", c)
	}
	if got := mid.Next(); got > time.Second {
		t.Errorf("the first wait after a reset is %v", got)
	}
	// A source outside [0, 1) is never a wait outside the bounds.
	bad := Backoff{rand: func() float64 { return 7 }}
	if got := bad.Next(); got < 0 || got > time.Second {
		t.Errorf("a bad source drew %v", got)
	}
}
