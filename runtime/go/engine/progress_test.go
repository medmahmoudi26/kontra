package engine

import (
	"sync"
	"testing"
)

// THE AUTHOR'S HEALTHCHECK IS THE RICHEST PROGRESS SIGNAL IN THE SYSTEM and it used to go only
// to `log.Printf` — a file inside a container, unreachable for a Machine in a Fleet. `SetProgress`
// is the seam that puts it on Temporal's heartbeat instead; these pin that it is wired and that a
// missing sink is survivable.
func TestProgressIsReportedThroughTheInstalledSink(t *testing.T) {
	var mu sync.Mutex
	var got []any
	a := &KontraActor{}
	a.SetProgress(func(v any) { mu.Lock(); got = append(got, v); mu.Unlock() })

	a.reportProgress(map[string]any{"hosts": 3, "withdrawn": 1})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("progress not delivered: %v", got)
	}
	m, ok := got[0].(map[string]any)
	if !ok || m["withdrawn"] != 1 {
		t.Fatalf("the author's value did not survive the seam: %v", got[0])
	}
}

// Outside a hosted run there is no activity context, which is what makes the engine testable —
// and an observability call must never be the thing that fails a Unit that already committed.
func TestProgressWithNoSinkIsSilentRatherThanFatal(t *testing.T) {
	(&KontraActor{}).reportProgress(map[string]any{"hosts": 1}) // must not panic
}

func TestAPanickingProgressSinkCannotFailTheUnit(t *testing.T) {
	a := &KontraActor{}
	a.SetProgress(func(any) { panic("heartbeat exploded") })
	a.reportProgress("anything") // recovered, like `heartbeat`
}
