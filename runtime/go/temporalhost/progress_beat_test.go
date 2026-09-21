package temporalhost

import (
	"sync"
	"testing"
)

/*
A PROGRESS BEAT MUST NOT BLANK THE LIVENESS COUNTS.

`control/orchestrator/src/heartbeat.ts` is a cross-language contract in which every field is
OPTIONAL and defaults to 0 — deliberately, so a renamed field degrades to "no progress" instead of
erroring. That tolerance is exactly what makes this dangerous: a heartbeat carrying only the
author's `progress` map would read as `done=0, total=0` and a batch that is moving would look
stalled, with nothing raised anywhere.

This reproduces the merge the host does, because the real one needs an activity context.
*/
func TestAProgressBeatKeepsTheUnitCounts(t *testing.T) {
	var mu sync.Mutex
	last := map[string]any{"node": "n1", "done": 0, "total": 10, "isolated": 0}
	var sent []map[string]any

	record := func(m map[string]any) { mu.Lock(); sent = append(sent, m); mu.Unlock() }

	// the liveness beat, after 4 committed units
	beat := func(done, total, isolated int) {
		mu.Lock()
		last["done"], last["total"], last["isolated"] = done, total, isolated
		out := map[string]any{}
		for k, v := range last {
			out[k] = v
		}
		mu.Unlock()
		record(out)
	}
	// the progress beat, on the engine's own ticker
	progress := func(v any) {
		mu.Lock()
		out := map[string]any{"progress": v}
		for k, val := range last {
			out[k] = val
		}
		mu.Unlock()
		record(out)
	}

	beat(4, 10, 0)
	progress(map[string]any{"hosts": 4, "withdrawn": 2})

	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 {
		t.Fatalf("want 2 beats, got %d", len(sent))
	}
	p := sent[1]
	if p["done"] != 4 || p["total"] != 10 {
		t.Fatalf("the progress beat lost the unit counts — a moving batch would read as "+
			"stalled: %v", p)
	}
	if p["node"] != "n1" {
		t.Fatalf("the progress beat lost the node: %v", p)
	}
	if _, ok := p["progress"]; !ok {
		t.Fatalf("the author's progress did not ride along: %v", p)
	}
}

// NAMESPACED, so an author whose healthcheck returns `{"done": ...}` cannot overwrite the field
// the orchestrator reads to decide whether a batch is moving.
func TestAnAuthorCannotOverwriteTheLivenessFields(t *testing.T) {
	last := map[string]any{"node": "n1", "done": 7, "total": 10, "isolated": 0}
	out := map[string]any{"progress": map[string]any{"done": 999, "total": -1}}
	for k, v := range last {
		out[k] = v
	}
	if out["done"] != 7 || out["total"] != 10 {
		t.Fatalf("an author's healthcheck overwrote the liveness counts: %v", out)
	}
}
