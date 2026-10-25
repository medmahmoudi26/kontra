package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// checkCanceledForGood checks the end of a forwarded cancel: GetTask on every replica says canceled, the
// log's last carried row is the canceled status and nothing is logged after it while the upstream would
// have gone on (spec §8: no completed after canceled), and the upstream task itself is canceled.
func checkCanceledForGood(t *testing.T, ctx context.Context, h *Harness, taskID a2a.TaskID, wait time.Duration) {
	t.Helper()
	for _, g := range h.Gates {
		task, err := h.BuyerA().Client(t, g, "").GetTask(ctx, &a2a.GetTaskRequest{ID: taskID})
		if err != nil || task.Status.State != a2a.TaskStateCanceled {
			t.Fatalf("GetTask on %s right after CancelTask: %v %v, want canceled (D79)", g.Name, task, err)
		}
	}
	before, err := h.Rows(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(wait)
	after, err := h.Rows(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range after[len(before):] {
		if r.Kind != "metadata" {
			t.Fatalf("a %s row (seq %d) was logged after the cancel: %s", r.Kind, r.Seq, r.Payload)
		}
	}
	carried, err := h.CarriedSeqs(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	lastSeq := carried[len(carried)-1]
	var last Row
	for _, r := range after {
		if r.Seq == lastSeq {
			last = r
		}
	}
	var sr a2a.StreamResponse
	if err := json.Unmarshal(last.Payload, &sr); err != nil {
		t.Fatal(err)
	}
	su, ok := sr.Event.(*a2a.TaskStatusUpdateEvent)
	if !ok || su.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("the last carried row is %s, want the canceled status", last.Payload)
	}
	as, err := h.Attempts(ctx, taskID)
	if err != nil || len(as) != 1 {
		t.Fatalf("attempts %v %v", as, err)
	}
	up, err := h.Upstream.Task(ctx, a2a.TaskID(as[0].UpstreamTaskID))
	if err != nil || up.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("the upstream task: %v %v, want canceled", up, err)
	}
}

// endedAttempt waits until the task's one attempt has ended and returns it: the attempt is ended after the
// terminal row is logged.
func endedAttempt(t *testing.T, ctx context.Context, h *Harness, taskID a2a.TaskID) Attempt {
	t.Helper()
	var as []Attempt
	eventually(t, 30*time.Second, "the attempt of "+string(taskID)+" ended", func() bool {
		var err error
		as, err = h.Attempts(ctx, taskID)
		return err == nil && len(as) == 1 && as[0].EndedAt != nil
	})
	return as[0]
}

// TestCancelForwarded is cancel_forwarded, direct backend (spec/relay.md §8, §16; D79, D80), against an
// a2a-go seller that handles cancel: a working task whose upstream task exists, canceled through the
// other replica over JSON-RPC, and an input-required task (no dispatch running), canceled over HTTP+JSON.
// Each time the upstream receives CancelTask, the caller gets the canceled task, GetTask right after
// agrees, the canceled status is the last carried row, and no completed follows.
func TestCancelForwarded(t *testing.T) {
	h := start(t, Options{Upstream: slowagent.Config{Chunks: 30, Interval: 500 * time.Millisecond, Streaming: true}})
	ctx := ctxFor(t, 5*time.Minute)
	buyer := h.BuyerA()

	// A working task: its upstream task exists and streams.
	s, err := buyer.SendStreaming(ctx, h.Gates[0], newMessage("cancel_forwarded working"), Call{})
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, ctx, s, func(ev Event) bool { return ev.Kind == "artifact" })
	working := s.TaskID()
	var att Attempt
	eventually(t, 10*time.Second, "the first upstream task id", func() bool {
		as, err := h.Attempts(ctx, working)
		if err == nil && len(as) == 1 && as[0].FirstUpstreamIDAt != nil {
			att = as[0]
			return true
		}
		return false
	})
	other := h.Other(h.GateByReplica(att.ReplicaID))
	canceled, err := buyer.Client(t, other, "").CancelTask(ctx, &a2a.CancelTaskRequest{ID: working})
	if err != nil {
		t.Fatalf("CancelTask on %s: %v", other.Name, err)
	}
	if canceled.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("CancelTask answered %s, want canceled", canceled.Status.State)
	}
	if n := h.Upstream.Count("CancelTask"); n != 1 || h.Upstream.Cancels() != 1 {
		t.Fatalf("the upstream received %d CancelTask (%d ran), want 1", n, h.Upstream.Cancels())
	}
	// The caller's stream ends at the canceled row.
	rest, err := s.ReadAll(ctx)
	if err != nil || len(rest) == 0 || rest[len(rest)-1].State != a2a.TaskStateCanceled {
		t.Fatalf("the caller's stream must end at the canceled row: %d events, %v", len(rest), err)
	}
	checkCanceledForGood(t, ctx, h, working, 3*time.Second)
	if a := endedAttempt(t, ctx, h, working); a.Outcome != "canceled" {
		t.Fatalf("the attempt of the canceled working task: %+v, want ended canceled", a)
	}
	// Canceling it again is TaskNotCancelableError.
	if _, err := buyer.Client(t, h.Gates[0], "").CancelTask(ctx, &a2a.CancelTaskRequest{ID: working}); err == nil {
		t.Fatal("a second CancelTask of a canceled task succeeded")
	}
	checkHistory(t, h, working, "e2e_cancel_forwarded_working", slowagent.ChunkMarker, h.APIKey)

	// An input-required task: no dispatch runs, the attempt waits; cancel over HTTP+JSON.
	res, err := buyer.Client(t, h.Gates[1], "").SendMessage(ctx, newMessage(slowagent.AskPrefix+" cancel_forwarded"))
	if err != nil {
		t.Fatal(err)
	}
	waiting, ok := res.(*a2a.Task)
	if !ok || waiting.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("SendMessage answered %#v, want an input-required task", res)
	}
	r, err := buyer.REST(ctx, h.Gates[0], "", http.MethodPost, "/tasks/"+string(waiting.ID)+":cancel", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var task a2a.Task
	if r.HTTPStatus != http.StatusOK || json.Unmarshal(r.Body, &task) != nil || task.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("POST tasks/{id}:cancel: HTTP %d %s, want the canceled task", r.HTTPStatus, r.Body)
	}
	if n := h.Upstream.Count("CancelTask"); n != 2 || h.Upstream.Cancels() != 2 {
		t.Fatalf("the upstream received %d CancelTask (%d ran), want 2", n, h.Upstream.Cancels())
	}
	checkCanceledForGood(t, ctx, h, waiting.ID, 2*time.Second)
	// The attempt waited at input-required: the task's end ends it interrupted (D77).
	if a := endedAttempt(t, ctx, h, waiting.ID); a.Outcome != "interrupted" {
		t.Fatalf("the attempt of the canceled input-required task: %+v, want ended interrupted (D77)", a)
	}
	checkHistory(t, h, waiting.ID, "e2e_cancel_forwarded_input_required", h.APIKey)
	checkUpstreamHeaders(t, h, h.Upstream)
}
