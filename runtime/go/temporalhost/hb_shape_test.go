package temporalhost

import (
	"encoding/json"
	"testing"
)

// THE HEARTBEAT PAYLOAD CROSSES A LANGUAGE BOUNDARY — it is read by
// control/orchestrator/src/heartbeat.ts — so what it serialises to is the contract, not what the
// Go value looks like. An author's healthcheck returns `map[string]any` holding atomics' values;
// this pins that the whole thing round-trips as JSON with the liveness fields intact and the
// author's keys reachable under `progress`.
func TestTheHeartbeatPayloadRoundTripsAsJSON(t *testing.T) {
	last := map[string]any{"node": "n1", "done": 4, "total": 10, "isolated": 0}
	authors := map[string]any{
		"hosts": int64(4), "probes": int64(20), "signals": int64(3),
		"controls": int64(3), "withdrawn": int64(2), "erratic": int64(0),
		"at": "api.example.com/api/v1/session",
	}
	out := map[string]any{"progress": authors}
	for k, v := range last {
		out[k] = v
	}

	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("the heartbeat payload does not serialise: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back["done"] != float64(4) || back["total"] != float64(10) {
		t.Fatalf("liveness fields lost in transit: %s", b)
	}
	p, ok := back["progress"].(map[string]any)
	if !ok {
		t.Fatalf("the author's progress did not survive as an object: %s", b)
	}
	// `withdrawn` is the number this engine most needs to surface: how many claims the matched
	// control killed. If it cannot cross the wire the operator cannot see the oracle working.
	if p["withdrawn"] != float64(2) || p["at"] != "api.example.com/api/v1/session" {
		t.Fatalf("author keys mangled: %s", b)
	}
}
