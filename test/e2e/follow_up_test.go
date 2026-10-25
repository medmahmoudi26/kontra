package e2e

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

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
// and its stream leads with the snapshot as of the turn's end without an id, then every row after it.
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
	checkHistory(t, h, taskID, "e2e_follow_up", slowagent.ChunkMarker, h.APIKey, "the answer from")
}

// TestFollowUpTemporalUnreachable is the WP-06 handoff's real-stack check: a follow-up while the replicas
// cannot reach Temporal is answered -32603 on JSON-RPC (HTTP 200) and 503 on HTTP+JSON (spec §7, §14.1:
// the Update is not accepted), never a stream, and the answer does not name the Temporal address. Once
// Temporal is reachable again the next follow-up runs the turn.
func TestFollowUpTemporalUnreachable(t *testing.T) {
	h := start(t, Options{Upstream: slowagent.Config{Chunks: 3, Interval: 100 * time.Millisecond, Streaming: true}, TemporalProxy: true})
	ctx := ctxFor(t, 5*time.Minute)
	_, taskID := askTask(t, ctx, h, h.Gates[0], "temporal_unreachable")

	h.TemporalProxy.Cut()
	follow := func(text string) *a2a.SendMessageRequest {
		req := newMessage(text)
		req.Message.TaskID = taskID
		return req
	}
	secrets := []string{h.TemporalProxy.Addr, h.Temporal, "127.0.0.1:"}
	started := time.Now()
	r, err := h.BuyerA().RPC(ctx, h.Gates[0], "", "SendMessage", follow("while temporal is away"))
	if err != nil {
		t.Fatal(err)
	}
	if r.HTTPStatus != http.StatusOK || r.Error == nil || r.Error.Code != -32603 {
		t.Fatalf("JSON-RPC follow-up without Temporal: HTTP %d %s, want HTTP 200 with -32603", r.HTTPStatus, r.Body)
	}
	for _, s := range secrets {
		if strings.Contains(string(r.Body), s) {
			t.Fatalf("the JSON-RPC answer names %q: %s", s, r.Body)
		}
	}
	rr, err := h.BuyerA().REST(ctx, h.Gates[1], "", http.MethodPost, "/message:send", follow("while temporal is away, REST"))
	if err != nil {
		t.Fatal(err)
	}
	if rr.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("HTTP+JSON follow-up without Temporal: HTTP %d %s, want 503", rr.HTTPStatus, rr.Body)
	}
	for _, s := range secrets {
		if strings.Contains(string(rr.Body), s) {
			t.Fatalf("the HTTP+JSON answer names %q: %s", s, rr.Body)
		}
	}
	_, err = h.BuyerA().SendStreaming(ctx, h.Gates[0], follow("while temporal is away, streaming"), Call{})
	var se *StreamError
	if !errors.As(err, &se) || se.RPC == nil || se.RPC.Code != -32603 {
		t.Fatalf("a streaming follow-up without Temporal: %v, want -32603 before any stream", err)
	}
	t.Logf("three follow-ups refused within %.1f s while Temporal was cut", time.Since(started).Seconds())
	if n := h.Upstream.Sends(); n != 1 {
		t.Fatalf("the upstream received %d sends, want only the first turn's", n)
	}

	// Temporal back: the replicas reconnect, and the next follow-up runs the turn.
	h.TemporalProxy.Restore()
	for _, g := range h.Gates {
		eventually(t, 3*time.Minute, g.Name+" healthy again", func() bool {
			return status(ctx, http.MethodGet, g.URL+"/healthz", "") == http.StatusOK
		})
	}
	var res a2a.SendMessageResult
	eventually(t, 2*time.Minute, "a follow-up accepted once Temporal is back", func() bool {
		res, err = h.BuyerA().Client(t, h.Gates[1], "").SendMessage(ctx, follow("now"))
		return err == nil
	})
	if task, ok := res.(*a2a.Task); !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the follow-up after Temporal came back: %#v", res)
	}
	if n := h.Upstream.Sends(); n != 2 {
		t.Fatalf("the upstream received %d sends, want 2", n)
	}
}
