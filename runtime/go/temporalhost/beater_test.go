package temporalhost

import (
	"testing"

	"github.com/medmahmoudi26/kontra/runtime/go/checkpoint"
)

/*
EVERY BEAT CARRIES THE CHECKPOINT, because Temporal keeps only the last details and the next
attempt resumes from whichever beat came last (ADR 0060).

Only the commit beat used to carry it. A progress beat or a keepalive tick after the last commit
replaced the details with a payload that had none, so a resume from that attempt would have re-run
the whole batch. These drive the REAL builder `RunBatch` wires in, rather than a copy of its merge —
the copies in `progress_beat_test.go` and `keepalive_test.go` are how three hand-built maps drifted.
*/

func TestEveryBeatCarriesTheCheckpointAndTheCounts(t *testing.T) {
	ck := checkpoint.Details{V: 1, BatchID: "b1"}
	var sent []map[string]any
	b := newBeater("n1", 10, func() checkpoint.Details { return ck }, func(m map[string]any) { sent = append(sent, m) })

	b.commit(4, 10, 1)
	b.progress(map[string]any{"hosts": 4})
	b.alive(1)
	b.alive(2)

	if len(sent) != 4 {
		t.Fatalf("want 4 beats, got %d", len(sent))
	}
	for i, beat := range sent {
		got, ok := beat["checkpoint"].(checkpoint.Details)
		if !ok || got.BatchID != "b1" {
			t.Errorf("beat %d carries no checkpoint, so a resume from it re-runs the batch: %v", i, beat)
		}
		if beat["done"] != 4 || beat["total"] != 10 || beat["isolated"] != 1 || beat["node"] != "n1" {
			t.Errorf("beat %d lost the unit counts, which heartbeat.ts reads as no progress: %v", i, beat)
		}
	}
	if _, ok := sent[1]["progress"]; !ok {
		t.Errorf("the author's progress did not ride along: %v", sent[1])
	}
	if sent[3]["alive"] != int64(2) {
		t.Errorf("alive must rise across ticks, got %v", sent[3]["alive"])
	}
}

func TestTheCheckpointIsReadAtEachBeatNotCaptured(t *testing.T) {
	// The runner publishes a new checkpoint before each commit beat; a keepalive after it must send
	// THAT one, not the one current when the batch began.
	ck := checkpoint.Details{V: 1, BatchID: "early"}
	var sent []map[string]any
	b := newBeater("n1", 2, func() checkpoint.Details { return ck }, func(m map[string]any) { sent = append(sent, m) })
	b.alive(1)
	ck = checkpoint.Details{V: 1, BatchID: "late"}
	b.alive(2)
	if sent[1]["checkpoint"].(checkpoint.Details).BatchID != "late" {
		t.Fatalf("the keepalive sent a stale checkpoint: %v", sent[1]["checkpoint"])
	}
}

func TestAnAuthorsProgressCannotOverwriteALivenessField(t *testing.T) {
	var sent []map[string]any
	b := newBeater("n1", 3, func() checkpoint.Details { return checkpoint.Details{} }, func(m map[string]any) { sent = append(sent, m) })
	b.commit(2, 3, 0)
	b.progress(map[string]any{"done": 999})
	if sent[1]["done"] != 2 {
		t.Fatalf("an author's {done: 999} must stay under `progress`, got done=%v", sent[1]["done"])
	}
}
