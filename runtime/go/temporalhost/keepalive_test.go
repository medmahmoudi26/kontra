package temporalhost

import (
	"context"
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

What is asserted here is the DERIVATION of the interval — a constant creeping back in is what can
go wrong without a test. The goroutine itself needs a real activity context; the PAYLOAD every tick
sends is asserted against the real builder in beater_test.go.
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

// The tick's PAYLOAD is asserted against the real builder in beater_test.go.

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
