// The per-Unit wall-clock budget, pinned the way `handler/heartbeat_test.go` pins the heartbeat
// and for the same reason: these three numbers are a LADDER, and the value of any one of them is
// only correct relative to the two above it.
//
//	max_seconds (600)  <  heartbeatTimeout (1800)  <  call_minutes (90m, the Nexus budget)
//
// The actor beats once per COMMITTED Unit, so a Unit that outlives the heartbeat cannot prove it is
// alive; a Batch whose Units outlive the Nexus budget is killed with everything uncommitted. That
// is not hypothetical — campaign-1790647335-hunt-crlf-shutterfly sent 6,000 controls, committed
// nothing, and the splitting axis was abandoned after three timeouts having written zero rows.
//
// A convenience edit that raises this default has to fail here rather than on a Fleet four hours in.
package main

import (
	"encoding/json"
	"testing"
)

// The ceiling the default is chosen against: a hunt's `debug_heartbeat_seconds`. Kept as a literal
// rather than imported because it lives in the CALLER (workflows/hunt/workflow.py) — the point of
// the test is that the two agree, and importing one from the other would make that vacuous.
const huntHeartbeatSeconds = 1800

func TestTheUnitBudgetFitsInsideTheHeartbeat(t *testing.T) {
	if defaultMaxSeconds >= huntHeartbeatSeconds {
		t.Fatalf("defaultMaxSeconds is %ds against a %ds heartbeat — a Unit that runs its full "+
			"budget can never beat, so the activity dies while the worker is probing correctly",
			defaultMaxSeconds, huntHeartbeatSeconds)
	}
	// A WAVE, NOT A UNIT, is the bound that matters. A 100-Unit page at concurrency 30 is four
	// waves; four full-budget waves must still leave room inside the 90-minute Nexus call.
	const waves, callSeconds = 4, 90 * 60
	if waves*defaultMaxSeconds >= callSeconds {
		t.Fatalf("%d waves of %ds is %ds, which does not fit a %ds call budget",
			waves, defaultMaxSeconds, waves*defaultMaxSeconds, callSeconds)
	}
}

// paramsOf's clamp, which differs from every other clamp in it: zero is a real answer.
func TestZeroBudgetMeansUnlimitedAndAbsentMeansTheDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want int
	}{
		{"absent leaves the default", `{"tier":1}`, defaultMaxSeconds},
		{"explicit zero is unlimited", `{"max_seconds":0}`, 0},
		{"a caller's own budget is honoured", `{"max_seconds":120}`, 120},
		{"a negative is not an answer", `{"max_seconds":-5}`, defaultMaxSeconds},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Params{MaxSeconds: defaultMaxSeconds}
			if err := json.Unmarshal([]byte(tc.raw), &p); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if p.MaxSeconds < 0 {
				p.MaxSeconds = defaultMaxSeconds
			}
			if p.MaxSeconds != tc.want {
				t.Fatalf("max_seconds = %d, want %d (from %s)", p.MaxSeconds, tc.want, tc.raw)
			}
		})
	}
}
