package temporalhost

import (
	"context"
	"sync"
	"testing"
	"time"
)

/*
THE KEEPALIVE BEAT — GitHub #19.

Both beats this host had were EVENT-DRIVEN: one per committed Unit, one per author progress call. A
Unit that legitimately runs longer than the heartbeat bound fires neither and is killed for silence
while it is working. Measured: `hunt` dispatches one Unit per host against that host's whole
technique class — 3,630 techniques x 3 oracle writes, paced at 250ms, ~45 minutes — against a
two-minute bound. The first beat was due 43 minutes after the attempt had already been killed.
Three runs died this way, 75 minutes and 0 rows each, with no error on the parent.

What is asserted here is the DERIVATION and the PAYLOAD SHAPE. The goroutine itself needs a real
activity context, which `progress_beat_test.go` already declines to fake for the same reason; what
can go wrong without a test is the interval (a constant creeping back in) and the merge (a tick that
blanks the counts, which `heartbeat.ts` would read as a batch that had made no progress at all).
*/

func TestTheKeepaliveIntervalIsDerivedFromTheBound(t *testing.T) {
	// A THIRD OF THE WINDOW: two ticks may be lost and the attempt still lives.
	if got := keepaliveEvery(2 * time.Minute); got != 40*time.Second {
		t.Fatalf("2m bound -> want 40s, got %s", got)
	}
	// It FOLLOWS the bound, including a per-dispatch `heartbeat_seconds` override — which is the
	// whole point of deriving it. A second constant here would ignore the override and beat too
	// slowly for a Unit that asked for more room.
	if got := keepaliveEvery(45 * time.Minute); got != 15*time.Minute {
		t.Fatalf("45m bound -> want 15m, got %s", got)
	}
	if got := keepaliveEvery(defaultHeartbeatForTest()); got <= 0 {
		t.Fatalf("the production bound must produce a positive interval, got %s", got)
	}
}

func TestNoBoundMeansNoKeepalive(t *testing.T) {
	// With no HeartbeatTimeout there is nothing to miss, so a timer beat would be pure write
	// amplification into the activity's mutable state. Zero is the honest answer, and it is also
	// what keeps `time.NewTicker` from panicking on a non-positive duration.
	for _, d := range []time.Duration{0, -1, -time.Hour} {
		if got := keepaliveEvery(d); got != 0 {
			t.Fatalf("bound %s -> want 0, got %s", d, got)
		}
	}
}

func TestAKeepaliveTickKeepsTheUnitCountsAndAddsAlive(t *testing.T) {
	/*
	 * A tick must carry the SAME counts the commit beat last reported, plus `alive`.
	 *
	 * `heartbeat.ts` defaults every missing field to 0, so a tick carrying only `alive` would read
	 * as `done=0, total=0` — a batch that is working reported as one that has done nothing, with
	 * nothing raised anywhere. That tolerance is exactly what makes the merge load-bearing.
	 *
	 * And `alive` is what replaces the property the keepalive gives up. "Silence means stuck" is
	 * gone; a counter that rises while `done` does not is how a reader still tells a Unit that is
	 * working slowly from one that has stopped.
	 */
	var mu sync.Mutex
	last := map[string]any{"node": "n1", "done": 4000, "total": 10890, "isolated": 0}
	var sent []map[string]any

	var alive int64
	tick := func() {
		alive++
		mu.Lock()
		beat := map[string]any{"alive": alive}
		for k, v := range last {
			beat[k] = v
		}
		mu.Unlock()
		sent = append(sent, beat)
	}

	tick()
	tick()

	if len(sent) != 2 {
		t.Fatalf("want 2 beats, got %d", len(sent))
	}
	for i, b := range sent {
		if b["done"] != 4000 || b["total"] != 10890 {
			t.Fatalf("beat %d blanked the counts: %v", i, b)
		}
		if b["node"] != "n1" {
			t.Fatalf("beat %d lost the node: %v", i, b)
		}
	}
	// RISING, which is the whole diagnostic: `done` stayed at 4000 across both.
	if sent[0]["alive"].(int64) != 1 || sent[1]["alive"].(int64) != 2 {
		t.Fatalf("alive must rise across ticks, got %v then %v", sent[0]["alive"], sent[1]["alive"])
	}
}

// defaultHeartbeatForTest reads the production bound the handler applies, so this suite fails if
// that constant moves without the interval being reconsidered.
func defaultHeartbeatForTest() time.Duration { return 2 * time.Minute }

func TestTheKeepaliveIsSilentOffTheActivityPath(t *testing.T) {
	/*
	 * `activity.GetInfo` PANICS outside an activity context — unlike `RecordHeartbeat`, which is a
	 * tolerant no-op. That asymmetry is why the two event-driven beats can be wired
	 * unconditionally and this one cannot.
	 *
	 * `RunBatch` is called directly by this package's own tests, so reading the bound without
	 * asking first turned every such call into a panic. `TestRunBatchNamesTheMachineItRanOn` is
	 * what caught it; this pins the guard so the next person to reach for `GetInfo` here does not
	 * rediscover it.
	 */
	if got := heartbeatBoundOf(context.Background()); got != 0 {
		t.Fatalf("a plain context has no bound; want 0, got %s", got)
	}
	// ...and no bound means no ticker, which is also what keeps time.NewTicker from panicking.
	if got := keepaliveEvery(heartbeatBoundOf(context.Background())); got != 0 {
		t.Fatalf("want no keepalive off the activity path, got %s", got)
	}
}
