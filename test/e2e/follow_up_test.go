package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/test/e2e/slowagent"
)

// askTask starts a task that stops at input-required with a stream on g, read to its end, and returns the
// events and the task.
func askTask(t *testing.T, ctx context.Context, h *Harness, g *Gate, text string) ([]Event, a2a.TaskID) {
	t.Helper()
	s, err := h.BuyerA().SendStreaming(ctx, g, newMessage(slowagent.AskPrefix+" "+text), cookieCall)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := s.ReadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 || evs[len(evs)-1].State != a2a.TaskStateInputRequired {
		t.Fatalf("the first turn's stream must close right after the input-required row: %d events", len(evs))
	}
	return evs, s.TaskID()
}

// TestFollowUp is a follow-up turn on a direct backend end to end (spec/relay.md §7, §9, §14.1; the
// audit's gap): the first turn stops at input-required and its stream closes there; two follow-ups race on
// the two replicas and exactly one is accepted, the other refused synchronously with
// UnsupportedOperationError before any stream and never forwarded; the accepted one is dispatched to the
// same upstream task with the upstream's task and context ids substituted and the caller's messageId kept,
// and its stream leads with the snapshot as of the turn's end without an id, then every row after it. The
// work items follow §7, §14.1, D66 and D81: the refused follow-up's is deleted, the accepted one's is
// consumed by the turn's first dispatch and marked delivered once the upstream answered it.
func TestFollowUp(t *testing.T) {
	h := start(t, Options{Upstream: slowagent.Config{Chunks: 10, Interval: 300 * time.Millisecond, Streaming: true}})
	ctx := ctxFor(t, 4*time.Minute)
	evs1, taskID := askTask(t, ctx, h, h.Gates[0], "follow_up")
	turnEnd := *evs1[len(evs1)-1].ID

	type outcome struct {
		gate *Gate
		msg  *a2a.Message
		s    *Stream
		err  error
	}
	var (
		wg   sync.WaitGroup
		outs = make([]outcome, 2)
		go1  = make(chan struct{})
	)
	for i, g := range h.Gates {
		req := newMessage("the answer from " + g.Name)
		req.Message.TaskID = taskID
		outs[i] = outcome{gate: g, msg: req.Message}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-go1
			outs[i].s, outs[i].err = h.BuyerA().SendStreaming(ctx, g, req, cookieCall)
		}()
	}
	close(go1)
	wg.Wait()
	var accepted *outcome
	for i := range outs {
		o := &outs[i]
		switch {
		case o.err == nil:
			if accepted != nil {
				t.Fatal("two racing follow-ups were both accepted (spec §7: turns are strictly sequential)")
			}
			accepted = o
		case isRPC(o.err, -32004):
			if se := o.err.(*StreamError); se.HTTPStatus != http.StatusOK || strings.HasPrefix(se.ContentType, "text/event-stream") {
				t.Fatalf("the refused follow-up: %v, want a JSON-RPC error before any SSE header", se)
			}
		default:
			t.Fatalf("follow-up on %s: %v, want a stream or UnsupportedOperationError", o.gate.Name, o.err)
		}
	}
	if accepted == nil {
		t.Fatal("neither racing follow-up was accepted")
	}
	evs2, err := accepted.s.ReadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs2) < 2 || evs2[0].ID != nil || evs2[0].Kind != "task" {
		t.Fatalf("the follow-up's stream must lead with a snapshot without an id: %d events", len(evs2))
	}
	snap, err := h.Log().SnapshotAt(ctx, uuid.MustParse(string(taskID)), turnEnd)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := canonical(t, evs2[0].Task()), canonical(t, snap); got != want {
		t.Fatalf("the follow-up's leading snapshot is not the task as of the turn's end (seq %d):\n got %s\nwant %s", turnEnd, got, want)
	}
	if evs2[0].State != a2a.TaskStateInputRequired {
		t.Fatalf("the leading snapshot is %s; it never closes the stream, and the turn's own rows follow", evs2[0].State)
	}
	if last := evs2[len(evs2)-1]; last.State != a2a.TaskStateCompleted {
		t.Fatalf("the follow-up's stream ends after a %s %s, want completed", last.Kind, last.State)
	}
	for _, ev := range evs2[1:] {
		if ev.ID == nil || *ev.ID <= turnEnd {
			t.Fatalf("a follow-up stream row with id %v, want ids above %d", ev.ID, turnEnd)
		}
	}
	checkCarried(t, h, taskID, append(ids(evs1), ids(evs2[1:])...))

	// The upstream saw two sends: the first turn, and the accepted follow-up on the same upstream task
	// with the upstream's ids substituted and the caller's messageId unchanged.
	reqs, turns := sends(h), h.Upstream.Turns()
	if len(reqs) != 2 || len(turns) != 2 {
		t.Fatalf("the upstream received %d sends and ran %d turns, want 2 and 2", len(reqs), len(turns))
	}
	as, err := h.Attempts(ctx, taskID)
	if err != nil || len(as) != 1 {
		t.Fatalf("attempts %v %v: a follow-up turn runs on the same attempt", as, err)
	}
	f := reqs[1]
	if f.TaskID != as[0].UpstreamTaskID || f.ContextID != as[0].UpstreamContextID || f.MessageID != accepted.msg.ID {
		t.Fatalf("the follow-up went upstream as task %q context %q message %q, want %q %q %q",
			f.TaskID, f.ContextID, f.MessageID, as[0].UpstreamTaskID, as[0].UpstreamContextID, accepted.msg.ID)
	}
	if !turns[1].FollowUp || turns[1].TaskID != a2a.TaskID(as[0].UpstreamTaskID) || !strings.Contains(turns[1].Text, accepted.gate.Name) {
		t.Fatalf("the upstream's second turn: %+v", turns[1])
	}
	task, err := h.BuyerA().Client(t, h.Gates[1], "").GetTask(ctx, &a2a.GetTaskRequest{ID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	var hist []string
	for _, m := range task.History {
		hist = append(hist, m.ID)
	}
	if task.Status.State != a2a.TaskStateCompleted || !slices.Contains(hist, accepted.msg.ID) {
		t.Fatalf("GetTask after the follow-up: %s, history %v; want completed with the accepted follow-up %s in history", task.Status.State, hist, accepted.msg.ID)
	}
	for _, o := range outs {
		if o.err != nil && slices.Contains(hist, o.msg.ID) {
			t.Fatal("the refused follow-up is in the task's history")
		}
	}
	checkUpstreamHeaders(t, h, h.Upstream)
	checkFollowUpItems(t, ctx, h, taskID, accepted.msg, outs[0].msg, outs[1].msg)
	checkHistory(t, h, taskID, "e2e_follow_up", slowagent.ChunkMarker, h.APIKey, "the answer from", "e2e-cookie")
}

// checkFollowUpItems checks a task's work items after a follow-up turn ran (spec §7, §14.1, D66, D81): the
// accepted follow-up's item was consumed by the turn's first dispatch and its delivery recorded; every
// other follow-up in others was refused, and its item deleted. The first turn's item is consumed too.
func checkFollowUpItems(t *testing.T, ctx context.Context, h *Harness, taskID a2a.TaskID, accepted *a2a.Message, others ...*a2a.Message) {
	t.Helper()
	ws, err := h.WorkItems(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) == 0 || !ws[0].Consumed {
		t.Fatalf("the first turn's work item: %+v, want consumed", ws)
	}
	w, err := h.WorkItemOf(ctx, taskID, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w == nil || !w.Consumed || !w.Delivered {
		t.Fatalf("the accepted follow-up's work item: %+v, want consumed (D66) and delivered (D81)", w)
	}
	for _, m := range others {
		if m.ID == accepted.ID {
			continue
		}
		if w, err := h.WorkItemOf(ctx, taskID, m.ID); err != nil || w != nil {
			t.Fatalf("the refused follow-up %s left work item %+v (%v): a refused follow-up's item is deleted (spec §7)", m.ID, w, err)
		}
	}
}

// TestFollowUpTemporalUnreachable is the WP-06 handoff's real-stack check of follow-ups whose Update
// Temporal does not accept (spec §7, §14.1): -32603 on JSON-RPC (HTTP 200) and 503 on HTTP+JSON, never a
// stream, the answer naming no Temporal address, nothing forwarded, and the work item kept (the workflow
// may still accept the Update). First Temporal hangs (the proxy accepts the replicas' connections and never
// answers): each follow-up is answered when the gate's acceptance wait (lifecycle.AcceptWait, 10 s) runs
// out, not before. Then Temporal is unreachable (connections refused), which is answered the same way
// within that wait. Once Temporal is back the next follow-up runs the turn, and the kept items stay
// unconsumed: no Update of theirs ever reached the workflow.
func TestFollowUpTemporalUnreachable(t *testing.T) {
	h := start(t, Options{Upstream: slowagent.Config{Chunks: 3, Interval: 100 * time.Millisecond, Streaming: true}, TemporalProxy: true})
	ctx := ctxFor(t, 5*time.Minute)
	_, taskID := askTask(t, ctx, h, h.Gates[0], "temporal_unreachable")

	var kept []*a2a.Message
	follow := func(text string) *a2a.SendMessageRequest {
		req := newMessage(text)
		req.Message.TaskID = taskID
		kept = append(kept, req.Message)
		return req
	}
	secrets := []string{h.TemporalProxy.Addr, h.Temporal, "127.0.0.1:"}
	noSecrets := func(what string, body []byte) {
		t.Helper()
		for _, s := range secrets {
			if strings.Contains(string(body), s) {
				t.Fatalf("the %s answer names %q: %s", what, s, body)
			}
		}
	}
	// within checks how long an answer took: at least the acceptance wait when Temporal hangs (min), and
	// never much longer than it.
	within := func(what string, began time.Time, min time.Duration) {
		t.Helper()
		d := time.Since(began)
		if d < min || d > lifecycle.AcceptWait+2*time.Second {
			t.Fatalf("the %s answer came after %.1f s, want between %.0f s and %.0f s (the acceptance wait, spec §14.1)",
				what, d.Seconds(), min.Seconds(), (lifecycle.AcceptWait + 2*time.Second).Seconds())
		}
		t.Logf("the %s answer came after %.1f s", what, d.Seconds())
	}

	// Temporal hangs.
	h.TemporalProxy.Blackhole()
	began := time.Now()
	r, err := h.BuyerA().RPC(ctx, h.Gates[0], "", "SendMessage", follow("while temporal hangs"))
	if err != nil {
		t.Fatal(err)
	}
	if r.HTTPStatus != http.StatusOK || r.Error == nil || r.Error.Code != -32603 {
		t.Fatalf("JSON-RPC follow-up while Temporal hangs: HTTP %d %s, want HTTP 200 with -32603", r.HTTPStatus, r.Body)
	}
	within("JSON-RPC", began, lifecycle.AcceptWait)
	noSecrets("JSON-RPC", r.Body)
	began = time.Now()
	rr, err := h.BuyerA().REST(ctx, h.Gates[1], "", http.MethodPost, "/message:send", follow("while temporal hangs, REST"))
	if err != nil {
		t.Fatal(err)
	}
	if rr.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("HTTP+JSON follow-up while Temporal hangs: HTTP %d %s, want 503", rr.HTTPStatus, rr.Body)
	}
	within("HTTP+JSON", began, lifecycle.AcceptWait)
	noSecrets("HTTP+JSON", rr.Body)
	began = time.Now()
	_, err = h.BuyerA().SendStreaming(ctx, h.Gates[0], follow("while temporal hangs, streaming"), Call{})
	var se *StreamError
	if !errors.As(err, &se) || se.RPC == nil || se.RPC.Code != -32603 || strings.HasPrefix(se.ContentType, "text/event-stream") {
		t.Fatalf("a streaming follow-up while Temporal hangs: %v, want -32603 before any stream", err)
	}
	within("streaming", began, lifecycle.AcceptWait)
	noSecrets("streaming", se.Body)

	// Temporal unreachable: the dial fails at once, and the gate still answers within its wait.
	h.TemporalProxy.Cut()
	began = time.Now()
	r, err = h.BuyerA().RPC(ctx, h.Gates[1], "", "SendMessage", follow("while temporal is unreachable"))
	if err != nil {
		t.Fatal(err)
	}
	if r.HTTPStatus != http.StatusOK || r.Error == nil || r.Error.Code != -32603 {
		t.Fatalf("JSON-RPC follow-up without Temporal: HTTP %d %s, want HTTP 200 with -32603", r.HTTPStatus, r.Body)
	}
	within("unreachable JSON-RPC", began, 0)
	noSecrets("unreachable JSON-RPC", r.Body)
	if n := h.Upstream.Sends(); n != 1 {
		t.Fatalf("the upstream received %d sends, want only the first turn's", n)
	}
	// Each follow-up's work item is kept (spec §7: the workflow may still accept it, and the turn then runs).
	for _, m := range kept {
		w, err := h.WorkItemOf(ctx, taskID, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if w == nil || w.Consumed {
			t.Fatalf("the follow-up %s not accepted in time left work item %+v, want it kept unconsumed (spec §7, §14.1)", m.ID, w)
		}
	}

	// Temporal back: the replicas reconnect, and the next follow-up runs the turn.
	h.TemporalProxy.Restore()
	for _, g := range h.Gates {
		eventually(t, 3*time.Minute, g.Name+" healthy again", func() bool {
			return status(ctx, http.MethodGet, g.URL+"/healthz", "") == http.StatusOK
		})
	}
	refused := kept
	kept = nil
	var res a2a.SendMessageResult
	eventually(t, 2*time.Minute, "a follow-up accepted once Temporal is back", func() bool {
		kept = kept[:0]
		res, err = h.BuyerA().Client(t, h.Gates[1], "").SendMessage(ctx, follow("now"))
		return err == nil
	})
	if task, ok := res.(*a2a.Task); !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the follow-up after Temporal came back: %#v", res)
	}
	if n := h.Upstream.Sends(); n != 2 {
		t.Fatalf("the upstream received %d sends, want 2", n)
	}
	w, err := h.WorkItemOf(ctx, taskID, kept[0].ID)
	if err != nil || w == nil || !w.Consumed || !w.Delivered {
		t.Fatalf("the accepted follow-up's work item: %+v %v, want consumed and delivered", w, err)
	}
	for _, m := range refused {
		if w, err := h.WorkItemOf(ctx, taskID, m.ID); err != nil || w == nil || w.Consumed {
			t.Fatalf("the follow-up %s not accepted in time: work item %+v %v, want it still kept and unconsumed", m.ID, w, err)
		}
	}
}

// lateRead is what a replica logs when a2a-go failed after Write accepted a streaming turn (internal/sse,
// Start.Late): the stream is served from the log instead.
const lateRead = "a2a-go failed after the turn was accepted"

// TestFollowUpLateRead is the WP-06 handoff's real-stack check of Start.Late (spec §9): a follow-up that
// Write accepted, after which a2a-go's own read of the task fails (the subscription's taskstore.Get), still
// gets its stream. Replica A's Postgres goes through a proxy the test cuts for a second as soon as the
// follow-up's work item is committed, which is before Write's Update returns and a2a-go reads. The caller
// gets 200 text/event-stream, leading with the snapshot at V (Log.SnapshotAt) without an id, then the
// turn's rows to completed, with no gap in the carried seqs; the replica logs the late error instead of
// answering it. A cut that missed a2a-go's read (the replica logged no late error) is tried again on a new
// task, up to three times, and never passes as one that hit it.
func TestFollowUpLateRead(t *testing.T) {
	h := start(t, Options{Upstream: slowagent.Config{Chunks: 4, Interval: 200 * time.Millisecond, Streaming: true}, PostgresProxy: true})
	ctx := ctxFor(t, 6*time.Minute)
	a := h.Gates[0]
	for try := 1; ; try++ {
		evs1, taskID := askTask(t, ctx, h, a, fmt.Sprintf("late_read %d", try))
		turnEnd := *evs1[len(evs1)-1].ID
		// The workflow has seen the turn end (its dispatch returned): the Update is accepted at once.
		eventually(t, 30*time.Second, "the first turn's dispatch returned", func() bool { return pendingActivities(ctx, h, taskID, lifecycle.ActDispatch) == 0 })
		lateBefore := len(grepLog(a.LogPath, lateRead))

		req := newMessage(fmt.Sprintf("the late answer %d", try))
		req.Message.TaskID = taskID
		cctx, stopCut := context.WithCancel(ctx)
		cut := make(chan time.Duration, 1)
		go func() {
			defer close(cut)
			for cctx.Err() == nil {
				if w, err := h.WorkItemOf(cctx, taskID, req.Message.ID); err == nil && w != nil {
					h.PostgresProxy.Cut()
					began := time.Now()
					time.Sleep(time.Second)
					h.PostgresProxy.Restore()
					cut <- time.Since(began)
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
		s, err := h.BuyerA().SendStreaming(ctx, a, req, cookieCall)
		if err != nil {
			stopCut()
			t.Fatalf("try %d: the follow-up got no stream (spec §9: an accepted turn is answered with its stream): %v", try, err)
		}
		if ct := s.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
			t.Fatalf("try %d: Content-Type %q", try, ct)
		}
		evs2, err := s.ReadAll(ctx)
		stopCut()
		<-cut
		if err != nil {
			t.Fatalf("try %d: the stream broke: %v", try, err)
		}
		if late := grepLog(a.LogPath, lateRead); len(late) == lateBefore {
			if try < 3 {
				t.Logf("try %d: the cut missed a2a-go's read (no %q logged); again on a new task", try, lateRead)
				continue
			}
			t.Fatalf("three tries and the cut never hit a2a-go's read after Write")
		} else {
			t.Logf("try %d: %s logged %s", try, a.Name, late[len(late)-1])
		}

		if len(evs2) < 2 || evs2[0].ID != nil || evs2[0].Kind != "task" {
			t.Fatalf("the follow-up's stream must lead with a snapshot without an id: %d events", len(evs2))
		}
		snap, err := h.Log().SnapshotAt(ctx, uuid.MustParse(string(taskID)), turnEnd)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := canonical(t, evs2[0].Task()), canonical(t, snap); got != want {
			t.Fatalf("the leading snapshot is not the task as of the turn's end (seq %d):\n got %s\nwant %s", turnEnd, got, want)
		}
		if last := evs2[len(evs2)-1]; last.State != a2a.TaskStateCompleted {
			t.Fatalf("the follow-up's stream ends after a %s %s, want completed", last.Kind, last.State)
		}
		checkCarried(t, h, taskID, append(ids(evs1), ids(evs2[1:])...))
		checkFollowUpItems(t, ctx, h, taskID, req.Message)
		task, err := h.BuyerA().Client(t, h.Gates[1], "").GetTask(ctx, &a2a.GetTaskRequest{ID: taskID})
		if err != nil || task.Status.State != a2a.TaskStateCompleted {
			t.Fatalf("GetTask after the follow-up: %v %v, want completed", task, err)
		}
		return
	}
}

// pendingActivities counts the task's workflow's pending activities of type name; -1 when it cannot be
// described.
func pendingActivities(ctx context.Context, h *Harness, taskID a2a.TaskID, name string) int {
	d, err := h.TC.DescribeWorkflowExecution(ctx, lifecycle.WorkflowID(taskID), "")
	if err != nil {
		return -1
	}
	n := 0
	for _, pa := range d.PendingActivities {
		if pa.GetActivityType().GetName() == name {
			n++
		}
	}
	return n
}
