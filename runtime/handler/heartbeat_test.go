package main

// heartbeat_test.go — a debugging convenience must not become the production default.
//
// The two-minute `HeartbeatTimeout` is the liveness bound the whole RunBatch design rests on: the
// actor beats once per committed Unit, so silence for two minutes means stuck rather than busy, and
// `StartToClose` is deliberately generous because it is no longer the thing that catches a wedge.
//
// Relaxing that for a debugger is right — a breakpoint inside a Unit emits no heartbeat, so two
// minutes later Temporal retries the attempt WHILE YOU ARE STILL PAUSED, onto the same queue, up to
// ten times. But a knob that can be turned from outside is a knob somebody leaves turned, so the
// default is pinned here and the value is bounded in both directions.

import (
	"testing"
	"time"
)

// THE NUMBER, asserted rather than assumed. If a change makes the default anything other than two
// minutes, that is a decision about production liveness and should not pass quietly.
func TestProductionHeartbeatIsTwoMinutes(t *testing.T) {
	if defaultHeartbeat != 2*time.Minute {
		t.Fatalf("the production heartbeat is %v, not 2m — RunBatch's liveness bound changed", defaultHeartbeat)
	}
	if got := heartbeatFor(0); got != 2*time.Minute {
		t.Fatalf("an ordinary dispatch (0) got %v, want 2m", got)
	}
	// And the ordinary path really is the ordinary path: every dispatch that does not ask for a
	// debug heartbeat sends the zero value.
	opts := runActivityOptions("nscheck-0.1.0", "", 0)
	if opts.HeartbeatTimeout != 2*time.Minute {
		t.Fatalf("runActivityOptions defaulted to %v, want 2m", opts.HeartbeatTimeout)
	}
	if opts.StartToCloseTimeout != time.Hour {
		t.Fatalf("StartToClose is %v, want 1h", opts.StartToCloseTimeout)
	}
}

func TestADebugDispatchRelaxesTheHeartbeat(t *testing.T) {
	opts := runActivityOptions("nscheck-0.1.0", "", 900) // 15 minutes of thinking
	if opts.HeartbeatTimeout != 15*time.Minute {
		t.Fatalf("a debug dispatch got %v, want 15m", opts.HeartbeatTimeout)
	}
}

// BOUNDED BELOW: nothing from outside may make an activity MORE fragile than production.
func TestARequestBelowTheDefaultIsIgnored(t *testing.T) {
	for _, secs := range []int32{1, 30, 119} {
		if got := heartbeatFor(secs); got != 2*time.Minute {
			t.Errorf("heartbeatFor(%d) = %v; a request below the default must not tighten it", secs, got)
		}
	}
	for _, secs := range []int32{0, -1, -3600} {
		if got := heartbeatFor(secs); got != 2*time.Minute {
			t.Errorf("heartbeatFor(%d) = %v, want the default", secs, got)
		}
	}
}

// BOUNDED ABOVE: a heartbeat longer than the activity's own deadline can never fire, so it would
// read as "liveness checking is off" while claiming a number.
func TestARequestAboveStartToCloseIsCapped(t *testing.T) {
	if got := heartbeatFor(99999); got != time.Hour {
		t.Errorf("heartbeatFor(99999) = %v, want the StartToClose cap of 1h", got)
	}
	if got := heartbeatFor(int32((2 * time.Hour).Seconds())); got != time.Hour {
		t.Errorf("two hours was not capped: %v", got)
	}
}

// The session queue decision must not change because a debug heartbeat was asked for — the two are
// unrelated, and a scoped dispatch landing on the shared queue is the failure `runActivityOptions`
// exists to prevent.
func TestTheDebugHeartbeatDoesNotAffectRouting(t *testing.T) {
	plain := runActivityOptions("nscheck-0.1.0", "sess-1", 0)
	debug := runActivityOptions("nscheck-0.1.0", "sess-1", 900)
	if plain.TaskQueue != debug.TaskQueue {
		t.Fatalf("routing changed with the heartbeat: %q vs %q", plain.TaskQueue, debug.TaskQueue)
	}
	if plain.ScheduleToStartTimeout != debug.ScheduleToStartTimeout {
		t.Errorf("ScheduleToStart changed with the heartbeat: %v vs %v",
			plain.ScheduleToStartTimeout, debug.ScheduleToStartTimeout)
	}
}
