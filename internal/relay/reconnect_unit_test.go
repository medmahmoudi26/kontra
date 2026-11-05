package relay

import (
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/store"
)

// The re-attach that ends a reconnect wait (D341): a working status goes before the artifacts the agent made while the
// node was away, so that no caller reads them under the gate's waiting message; a status that ends the turn stays
// after them. Either is marked kontra.reconnect {state: resumed}.
func TestMarkResumedOrder(t *testing.T) {
	task := &a2a.Task{ID: "t-1", ContextID: "c-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		state a2a.TaskState
		first bool // the status is the first event
	}{
		{a2a.TaskStateWorking, true},
		{a2a.TaskStateCompleted, false},
		{a2a.TaskStateInputRequired, false},
		{a2a.TaskStateAuthRequired, false},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			art := a2a.NewArtifactEvent(task, a2a.NewTextPart("chunk"))
			st := a2a.NewStatusUpdateEvent(task, tc.state, a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart("progress")))
			out := markResumed(task, []a2a.Event{art, st}, at)
			if len(out) != 2 {
				t.Fatalf("%d events, want 2", len(out))
			}
			i := 1
			if tc.first {
				i = 0
			}
			got, ok := out[i].(*a2a.TaskStatusUpdateEvent)
			if !ok {
				t.Fatalf("event %d is %T, want the status (order %T, %T)", i, out[i], out[0], out[1])
			}
			if m, _ := got.Metadata[store.MetaReconnect].(map[string]any); m["state"] != store.ReconnectResumed {
				t.Fatalf("the status's kontra.reconnect: %v", got.Metadata)
			}
			if _, ok := out[1-i].(*a2a.TaskArtifactUpdateEvent); !ok {
				t.Fatalf("event %d is %T, want the artifact", 1-i, out[1-i])
			}
			if st.Metadata != nil {
				t.Fatal("markResumed changed the event it was given")
			}
		})
	}
}

// A terminal status logged while the task's reconnect wait is on says the wait ended (D341): a task never ends showing
// that it waits for its node. Other statuses, and a task not waiting, are left alone.
func TestEndReconnect(t *testing.T) {
	since := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	waiting := &a2a.Task{ID: "t-1", ContextID: "c-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		Metadata: map[string]any{store.MetaReconnect: store.ReconnectWaitingMeta(since, since.Add(15*time.Minute))}}
	failed := store.FailedStatus(waiting, store.ReasonTunnelLost, "gone")
	store.EndReconnect(waiting, failed)
	if m, _ := failed.Metadata[store.MetaReconnect].(map[string]any); m["state"] != store.ReconnectEnded || m["at"] == nil {
		t.Fatalf("the terminal status's kontra.reconnect: %v", failed.Metadata)
	}
	if failed.Metadata[store.MetaFailure] != string(store.ReasonTunnelLost) {
		t.Fatalf("kontra.failure lost: %v", failed.Metadata)
	}
	if _, ok := failed.Status.Message.Metadata[store.MetaReconnect]; ok {
		t.Fatal("the message's metadata changed: a sealed job's hiring node reads it (ext-sealed.md rule 4a)")
	}
	working := a2a.NewStatusUpdateEvent(waiting, a2a.TaskStateWorking, nil)
	store.EndReconnect(waiting, working)
	resumed := &a2a.Task{ID: "t-1", ContextID: "c-1", Metadata: map[string]any{store.MetaReconnect: store.ReconnectResumedMeta(since)}}
	done := store.TerminalStatus(resumed, a2a.TaskStateCompleted, nil, "")
	store.EndReconnect(resumed, done)
	if working.Metadata != nil || done.Metadata != nil {
		t.Fatalf("a non-terminal status or a task not waiting was marked: %v %v", working.Metadata, done.Metadata)
	}
}
