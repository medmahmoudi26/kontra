package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// checkCanceledForGood checks the end of a forwarded cancel: GetTask on every replica says canceled, the
// log's last carried row is the canceled status and nothing is logged after it within wait (spec §8: no
// completed after canceled), and the upstream task itself is canceled.
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
// agrees, the canceled status is the last carried row, and no completed follows. An a2a-go seller never
// sends anything after its canceled status (a2a-go ends the execution there), so the seller here ignores
// its own cancel (TalksAfterCancel): the stream the gate follows goes on with a chunk and a completed
// status right after the canceled one, and the gate must log neither. A second CancelTask of the canceled
// task is TaskNotCancelableError on both bindings.
func TestCancelForwarded(t *testing.T) {
	h := start(t, Options{Upstream: slowagent.Config{Chunks: 30, Interval: 500 * time.Millisecond, Streaming: true, TalksAfterCancel: true}})
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
	x := h.GateByReplica(att.ReplicaID)
	if x == nil {
		t.Fatalf("attempts.replica_id %q is neither replica", att.ReplicaID)
	}
	other := h.Other(x)
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
	if n := h.Upstream.LateEvents(); n == 0 {
		t.Fatal("the seller wrote nothing after its canceled status: the check above proved nothing")
	} else {
		t.Logf("the seller wrote %d events after its canceled status; the gate logged none", n)
	}
	if a := endedAttempt(t, ctx, h, working); a.Outcome != "canceled" {
		t.Fatalf("the attempt of the canceled working task: %+v, want ended canceled", a)
	}
	// Canceling it again is TaskNotCancelableError (-32002, HTTP+JSON 400), on both bindings.
	for _, c := range []struct {
		binding string
		client  interface {
			CancelTask(context.Context, *a2a.CancelTaskRequest) (*a2a.Task, error)
		}
	}{{"JSON-RPC", buyer.Client(t, h.Gates[0], "")}, {"HTTP+JSON", buyer.ClientREST(t, h.Gates[1], "")}} {
		if _, err := c.client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: working}); !errors.Is(err, a2a.ErrTaskNotCancelable) {
			t.Fatalf("a second CancelTask of a canceled task over %s: %v, want TaskNotCancelableError (spec §8)", c.binding, err)
		}
	}
	r, err := buyer.RPC(ctx, h.Gates[1], "", "CancelTask", &a2a.CancelTaskRequest{ID: working})
	if err != nil || r.Error == nil || r.Error.Code != -32002 {
		t.Fatalf("a second CancelTask over JSON-RPC: %v %s, want -32002", err, r.Body)
	}
	rr, err := buyer.REST(ctx, h.Gates[0], "", http.MethodPost, "/tasks/"+string(working)+":cancel", map[string]any{})
	if err != nil || rr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("a second CancelTask over HTTP+JSON: %v %s, want HTTP 400", err, rr.Body)
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
	rr, err = buyer.REST(ctx, h.Gates[0], "", http.MethodPost, "/tasks/"+string(waiting.ID)+":cancel", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var task a2a.Task
	if rr.HTTPStatus != http.StatusOK || json.Unmarshal(rr.Body, &task) != nil || task.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("POST tasks/{id}:cancel: HTTP %d %s, want the canceled task", rr.HTTPStatus, rr.Body)
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
