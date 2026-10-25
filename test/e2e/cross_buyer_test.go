package e2e

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// TestCrossBuyer is cross_buyer (spec/relay.md §2.3, §4, §16; D53): two buyers of one tenant send the same
// contextId. Each gets its own task in its own gate context, kept verbatim; neither can read, cancel,
// subscribe to, list or follow up the other's task (TaskNotFound, on either replica); and the two tasks
// reach two distinct upstream contexts, because no caller context id ever goes upstream: the first task of
// a gate context is sent without one, and a later task of the same buyer's context on the same direct
// backend carries the upstream context id recorded for it.
func TestCrossBuyer(t *testing.T) {
	h := start(t, Options{Upstream: slowagent.Config{Chunks: 2, Interval: 50 * time.Millisecond, Streaming: true}})
	ctx := ctxFor(t, 3*time.Minute)
	shared := "shared-context-" + randomHex(4)
	a, b := h.BuyerA(), h.BuyerB()
	ca, cb := a.Client(t, h.Gates[0], ""), b.Client(t, h.Gates[1], "")

	send := func(c *a2aclient.Client, text string) *a2a.Task {
		t.Helper()
		req := newMessage(text)
		req.Message.ContextID = shared
		res, err := c.SendMessage(ctx, req)
		if err != nil {
			t.Fatalf("SendMessage %q: %v", text, err)
		}
		task, ok := res.(*a2a.Task)
		if !ok || task.Status.State != a2a.TaskStateCompleted {
			t.Fatalf("SendMessage %q: %#v, want a completed task", text, res)
		}
		return task
	}
	ta := send(ca, "cross_buyer a1")
	tb := send(cb, "cross_buyer b1")
	if ta.ContextID != shared || tb.ContextID != shared {
		t.Fatalf("a caller's contextId is kept verbatim: got %q and %q, want %q", ta.ContextID, tb.ContextID, shared)
	}
	if ta.ID == tb.ID {
		t.Fatal("two buyers got the same task")
	}

	// Neither buyer reaches the other's task, on either replica.
	for _, g := range h.Gates {
		for _, c := range []struct {
			buyer *Buyer
			task  a2a.TaskID
		}{{b, ta.ID}, {a, tb.ID}} {
			cl := c.buyer.Client(t, g, "")
			if _, err := cl.GetTask(ctx, &a2a.GetTaskRequest{ID: c.task}); !errors.Is(err, a2a.ErrTaskNotFound) {
				t.Fatalf("GetTask of the other buyer's task on %s: %v, want TaskNotFound", g.Name, err)
			}
			if _, err := cl.CancelTask(ctx, &a2a.CancelTaskRequest{ID: c.task}); !errors.Is(err, a2a.ErrTaskNotFound) {
				t.Fatalf("CancelTask of the other buyer's task on %s: %v, want TaskNotFound", g.Name, err)
			}
			follow := newMessage("not yours")
			follow.Message.TaskID = c.task
			if _, err := cl.SendMessage(ctx, follow); !errors.Is(err, a2a.ErrTaskNotFound) {
				t.Fatalf("a follow-up to the other buyer's task on %s: %v, want TaskNotFound", g.Name, err)
			}
			if _, err := c.buyer.Subscribe(ctx, g, c.task, nil, Call{}); !isRPC(err, -32001) {
				t.Fatalf("SubscribeToTask of the other buyer's task on %s: %v, want TaskNotFound before the stream", g.Name, err)
			}
		}
	}
	for _, c := range []struct {
		buyer      *Buyer
		own, other a2a.TaskID
	}{{a, ta.ID, tb.ID}, {b, tb.ID, ta.ID}} {
		list, err := c.buyer.Client(t, h.Gates[0], "").ListTasks(ctx, &a2a.ListTasksRequest{})
		if err != nil {
			t.Fatal(err)
		}
		var got []a2a.TaskID
		for _, task := range list.Tasks {
			got = append(got, task.ID)
		}
		if !slices.Contains(got, c.own) || slices.Contains(got, c.other) {
			t.Fatalf("ListTasks = %v: want the buyer's own task %s and never the other's %s", got, c.own, c.other)
		}
	}

	// Upstream: neither send carried a context id, and the upstream made two contexts.
	turns, reqs := h.Upstream.Turns(), sends(h)
	if len(turns) != 2 || len(reqs) != 2 {
		t.Fatalf("the upstream ran %d turns from %d sends, want 2 and 2", len(turns), len(reqs))
	}
	for _, r := range reqs {
		if r.ContextID != "" || r.TaskID != "" {
			t.Fatalf("a first task of a gate context went upstream with contextId %q taskId %q (D53)", r.ContextID, r.TaskID)
		}
	}
	if turns[0].ContextID == turns[1].ContextID {
		t.Fatalf("two buyers with the same contextId met in one upstream context %s", turns[0].ContextID)
	}
	upCtx := map[a2a.TaskID]string{}
	for _, task := range []*a2a.Task{ta, tb} {
		as, err := h.Attempts(ctx, task.ID)
		if err != nil || len(as) != 1 {
			t.Fatalf("attempts of %s: %v %v", task.ID, as, err)
		}
		upCtx[task.ID] = as[0].UpstreamContextID
	}
	if upCtx[ta.ID] == "" || upCtx[ta.ID] == upCtx[tb.ID] {
		t.Fatalf("recorded upstream contexts %v: want two distinct ones", upCtx)
	}

	// A later task in buyer A's context, on the same direct backend, carries A's recorded upstream context.
	ta2 := send(ca, "cross_buyer a2")
	if ta2.ContextID != shared {
		t.Fatalf("the second task's contextId is %q, want %q", ta2.ContextID, shared)
	}
	reqs = sends(h)
	if len(reqs) != 3 || reqs[2].ContextID != upCtx[ta.ID] || reqs[2].TaskID != "" {
		t.Fatalf("the second task of A's context went upstream with contextId %q, want A's upstream context %q", reqs[len(reqs)-1].ContextID, upCtx[ta.ID])
	}
	if turns := h.Upstream.Turns(); turns[2].ContextID != upCtx[ta.ID] {
		t.Fatalf("the upstream ran the second task in context %s, want %s", turns[2].ContextID, upCtx[ta.ID])
	}
	checkUpstreamHeaders(t, h, h.Upstream)
}

// sends are the send requests the slow upstream received, in order.
func sends(h *Harness) []slowagent.Request {
	var out []slowagent.Request
	for _, r := range h.Upstream.Requests() {
		if r.Method == "SendMessage" || r.Method == "SendStreamingMessage" {
			out = append(out, r)
		}
	}
	return out
}
