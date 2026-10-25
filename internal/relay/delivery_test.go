package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/upstream"
)

// Follow-up delivery (spec §6.4 "Follow-up delivery", D81).

// hookBackends runs a hook once from inside Backend: the dispatch has consumed its work item and a2a-go has
// appended the follow-up to the task's history, and the executor has not called the upstream yet.
type hookBackends struct {
	*staticBackends
	hook atomic.Pointer[func()]
}

func (h *hookBackends) Backend(ctx context.Context, svc store.ServiceRef) (upstream.Backend, upstream.Info, error) {
	if f := h.hook.Swap(nil); f != nil {
		(*f)()
	}
	return h.staticBackends.Backend(ctx, svc)
}

// onNextBackend arms the hook for the next dispatch.
func (h *hookBackends) onNextBackend(f func()) { h.hook.Store(&f) }

// withHookBackends puts a hookBackends in front of the env's backends; call it before the first dispatch,
// which builds the service's handler.
func withHookBackends(e *env) *hookBackends {
	h := &hookBackends{staticBackends: e.backends}
	e.handlers.deps.Backends = h
	return h
}

// bounded is a dispatch context that ends after 20 s: a dispatch that should have ended long before then
// (a resume that never sends, a turn that never ends) fails the test instead of hanging it.
func bounded(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func (e *env) workItem(id a2a.TaskID, wid uuid.UUID) *store.WorkItem {
	e.t.Helper()
	wi, err := e.log.LoadWorkItem(context.Background(), wid, uid(e.t, id))
	if err != nil {
		e.t.Fatal(err)
	}
	return wi
}

// received are the messages carrying text that the upstream received in SendMessage and
// SendStreamingMessage calls.
func received(t *testing.T, a *upstreamAgent, text string) []*a2a.Message {
	t.Helper()
	var out []*a2a.Message
	for _, m := range a.sentMessages(t) {
		if len(m.Parts) > 0 && m.Parts[0].Text() == text {
			out = append(out, m)
		}
	}
	return out
}

// countState is how many status rows of the task's log are in state.
func (e *env) countState(id a2a.TaskID, state a2a.TaskState) int {
	n := 0
	for _, s := range e.states(id) {
		if s == "status:"+string(state) {
			n++
		}
	}
	return n
}

// askThenWork asks for input on the first turn; on the follow-up it reports working, waits for release,
// then completes with the follow-up's text. done is set once it completed.
type askThenWork struct {
	release chan struct{}
	done    atomic.Bool
}

func (g *askThenWork) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("which one?"))), nil)
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		select {
		case <-g.release:
		case <-ctx.Done():
			return
		}
		if yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("done: "+ec.Message.Parts[0].Text()))), nil) {
			g.done.Store(true)
		}
	}
}

func (g *askThenWork) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// A follow-up turn's dispatch that dies after consuming its work item and before the upstream received the
// follow-up is resumed by sending the follow-up again: the upstream (an a2a-go seller that asks for input
// once) receives it exactly once, with the caller's messageId and the upstream ids, and the task completes
// with the seller's answer to it. The delivery is recorded when the upstream answers.
func TestFollowUpResentWhenNotDelivered(t *testing.T) {
	for _, streaming := range []bool{true, false} {
		t.Run(fmt.Sprintf("streaming=%v", streaming), func(t *testing.T) {
			e := newEnv(t, envOptions{})
			hb := withHookBackends(e)
			agent, b, info := newAgent(t, askingAgent(), agentConfig{streaming: streaming})
			info.Streaming = streaming
			e.backends.set(b, info, nil)
			id, wid := e.newTask(taskSpec{owner: buyerA, text: "book a room"})
			if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchInterrupted {
				t.Fatalf("turn 1: %+v", res)
			}
			if wi := e.workItem(id, wid); wi.DeliveredAt != nil {
				t.Errorf("a first turn recorded a delivery: %v", wi.DeliveredAt)
			}
			a := e.attempt(id, 1)

			fid := e.followUp(id, buyerA, "the blue one")
			ctx, kill := context.WithCancel(context.Background())
			defer kill()
			hb.onNextBackend(kill) // the replica dies after the consume, before the upstream call
			if res := e.dispatch(ctx, id, 1, fid, false); res.Outcome != lifecycle.DispatchContextLost {
				t.Fatalf("the killed follow-up dispatch: %+v", res)
			}
			wi := e.workItem(id, fid)
			if wi.ConsumedAt == nil || wi.DeliveredAt != nil {
				t.Fatalf("after the kill: consumed %v, delivered %v", wi.ConsumedAt, wi.DeliveredAt)
			}
			if n := len(received(t, agent, "the blue one")); n != 0 || agent.sends.Load() != 1 {
				t.Fatalf("the killed dispatch reached the upstream: %d follow-ups, %d sends", n, agent.sends.Load())
			}

			res := e.dispatch(bounded(t), id, 1, fid, true)
			p := projection(t, e, id)
			if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateCompleted || p.Status.Message.Parts[0].Text() != "done: the blue one" {
				t.Fatalf("the resume: %+v %+v, log %v", res, p.Status, e.states(id))
			}
			got := received(t, agent, "the blue one")
			if len(got) != 1 || agent.sends.Load() != 2 {
				t.Fatalf("the upstream received the follow-up %d times (%d sends)", len(got), agent.sends.Load())
			}
			if got[0].ID != wi.Payload.Request.Message.ID || got[0].TaskID != a2a.TaskID(a.UpstreamTaskID) || got[0].ContextID != a.UpstreamContextID {
				t.Errorf("the resent follow-up %+v; want messageId %s on %s/%s", got[0], wi.Payload.Request.Message.ID, a.UpstreamTaskID, a.UpstreamContextID)
			}
			if e.workItem(id, fid).DeliveredAt == nil {
				t.Error("the resend's answer did not record the delivery")
			}
			if e.count(id, store.KindMessage) != 1 || e.countState(id, a2a.TaskStateCompleted) != 1 {
				t.Errorf("log %v", e.states(id))
			}
			if a := e.attempt(id, 1); a.Outcome != store.OutcomeCompleted {
				t.Errorf("attempt %+v", a)
			}
		})
	}
}

// A follow-up turn's dispatch that dies after the upstream answered the follow-up (its delivery recorded)
// is resumed by a re-attach that sends nothing: the upstream receives the follow-up exactly once, and the
// resume follows the task to the seller's answer.
func TestFollowUpNotResentWhenDelivered(t *testing.T) {
	for _, streaming := range []bool{true, false} {
		t.Run(fmt.Sprintf("streaming=%v", streaming), func(t *testing.T) {
			e := newEnv(t, envOptions{})
			g := &askThenWork{release: make(chan struct{}, 1)}
			agent, b, info := newAgent(t, g, agentConfig{streaming: streaming})
			info.Streaming = streaming
			e.backends.set(b, info, nil)
			id, wid := e.newTask(taskSpec{owner: buyerA, text: "book a room"})
			if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchInterrupted {
				t.Fatalf("turn 1: %+v", res)
			}
			fid := e.followUp(id, buyerA, "the blue one")
			ctx, kill := context.WithCancel(context.Background())
			defer kill()
			first := make(chan lifecycle.DispatchResult, 1)
			go func() { first <- e.dispatch(ctx, id, 1, fid, false) }()
			waitFor(t, "the follow-up's delivery", func() bool { return e.workItem(id, fid).DeliveredAt != nil })
			delivered := *e.workItem(id, fid).DeliveredAt
			kill() // the replica dies after the upstream answered
			if res := <-first; res.Outcome != lifecycle.DispatchContextLost {
				t.Fatalf("the killed follow-up dispatch: %+v", res)
			}
			g.release <- struct{}{} // the seller finishes while nobody follows it
			waitFor(t, "the seller to complete", g.done.Load)

			res := e.dispatch(bounded(t), id, 1, fid, true)
			p := projection(t, e, id)
			if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateCompleted || p.Status.Message.Parts[0].Text() != "done: the blue one" {
				t.Fatalf("the resume: %+v %+v, log %v", res, p.Status, e.states(id))
			}
			if n := len(received(t, agent, "the blue one")); n != 1 || agent.sends.Load() != 2 {
				t.Errorf("the upstream received the follow-up %d times (%d sends)", n, agent.sends.Load())
			}
			if got := e.workItem(id, fid).DeliveredAt; got == nil || !got.Equal(delivered) {
				t.Errorf("delivered_at %v, was %v", got, delivered)
			}
			if e.count(id, store.KindMessage) != 1 || e.countState(id, a2a.TaskStateCompleted) != 1 || e.countState(id, a2a.TaskStateInputRequired) != 1 {
				t.Errorf("log %v", e.states(id))
			}
		})
	}
}

// staleSeller is a scripted JSON-RPC upstream task up-1 that asks for input on its first SendMessage and
// answers every later SendMessage with the task as it still is (input-required, as A2A 1.0.1 allows for
// returnImmediately); GetTask shows it completed once moved is set. It records every message sent to it.
type staleSeller struct {
	moved atomic.Bool
	sends atomic.Int64
	gets  atomic.Int64
	mu    sync.Mutex
	sent  []*a2a.Message
	// onSend runs for every SendMessage after the first, before it is answered; it may answer itself
	// (returning true).
	onSend func(w http.ResponseWriter, id json.RawMessage, r *http.Request) bool
}

const (
	staleAsk  = `{"id":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_INPUT_REQUIRED","timestamp":"2026-10-25T10:00:00Z","message":{"messageId":"q1","role":"ROLE_AGENT","parts":[{"text":"which one?"}]}}}`
	staleDone = `{"id":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_COMPLETED","timestamp":"2026-10-25T10:00:05Z","message":{"messageId":"a1","role":"ROLE_AGENT","parts":[{"text":"booked"}]}}}`
)

func (s *staleSeller) backend(t *testing.T) upstream.Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
			Params struct {
				Message *a2a.Message `json:"message"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		switch rpc.Method {
		case "SendMessage":
			s.mu.Lock()
			s.sent = append(s.sent, rpc.Params.Message)
			s.mu.Unlock()
			if n := s.sends.Add(1); n > 1 && s.onSend != nil && s.onSend(w, rpc.ID, r) {
				return
			}
			rpcResult(w, rpc.ID, `"result":{"task":`+staleAsk+`}`)
		default:
			s.gets.Add(1)
			if s.moved.Load() {
				rpcResult(w, rpc.ID, `"result":`+staleDone)
				return
			}
			rpcResult(w, rpc.ID, `"result":`+staleAsk)
		}
	}))
	t.Cleanup(srv.Close)
	b, err := upstream.NewDirect(srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (s *staleSeller) messages() []*a2a.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*a2a.Message(nil), s.sent...)
}

// A delivered follow-up whose upstream still shows the previous turn's input-required (it answered the
// follow-up's SendMessage{returnImmediately} with its task as it still was, so nothing was logged and the
// delivery was recorded on its own) is resumed by a re-attach that neither sends again nor ends the turn at
// that input-required: it follows the upstream until it processed the follow-up. A follow-up turn that
// had logged its own input-required before the dispatch was lost ends there on the resume.
func TestFollowUpResumeTellsTurnEnds(t *testing.T) {
	t.Run("previous turn's end", func(t *testing.T) {
		e := newEnv(t, envOptions{opts: ExecutorOptions{PollInterval: 30 * time.Millisecond}})
		s := &staleSeller{}
		e.backends.set(s.backend(t), rpcInfo, nil)
		id, wid := e.newTask(taskSpec{owner: buyerA, text: "book"})
		if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchInterrupted {
			t.Fatalf("turn 1: %+v", res)
		}
		fid := e.followUp(id, buyerA, "the blue one")
		ctx, kill := context.WithCancel(context.Background())
		defer kill()
		first := make(chan lifecycle.DispatchResult, 1)
		go func() { first <- e.dispatch(ctx, id, 1, fid, false) }()
		waitFor(t, "the follow-up's delivery", func() bool { return e.workItem(id, fid).DeliveredAt != nil })
		waitFor(t, "a stale poll", func() bool { return s.gets.Load() > 0 })
		kill()
		if res := <-first; res.Outcome != lifecycle.DispatchContextLost {
			t.Fatalf("the killed follow-up dispatch: %+v", res)
		}
		before := e.states(id)
		gets := s.gets.Load()
		resumed := make(chan lifecycle.DispatchResult, 1)
		go func() { resumed <- e.dispatch(bounded(t), id, 1, fid, true) }()
		waitFor(t, "the resume to poll the stale task", func() bool { return s.gets.Load() >= gets+3 })
		select {
		case res := <-resumed:
			t.Fatalf("the resume ended at the previous turn's input-required: %+v, log %v", res, e.states(id))
		default:
		}
		s.moved.Store(true)
		res := <-resumed
		p := projection(t, e, id)
		if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateCompleted || p.Status.Message.Parts[0].Text() != "booked" {
			t.Fatalf("the resume: %+v %+v", res, p.Status)
		}
		if sent := s.messages(); len(sent) != 2 || sent[1].Parts[0].Text() != "the blue one" {
			t.Errorf("sends %+v; the delivered follow-up was sent again", sent)
		}
		if fmt.Sprint(before) != "[task:TASK_STATE_SUBMITTED status:TASK_STATE_INPUT_REQUIRED message]" ||
			e.countState(id, a2a.TaskStateInputRequired) != 1 || e.countState(id, a2a.TaskStateCompleted) != 1 {
			t.Errorf("log before the resume %v, after %v", before, e.states(id))
		}
	})
	t.Run("own end", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		agent, b, info := newAgent(t, twiceAsking(), agentConfig{streaming: true})
		info.Streaming = true
		e.backends.set(b, info, nil)
		id, wid := e.newTask(taskSpec{owner: buyerA, text: "book"})
		if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchInterrupted {
			t.Fatalf("turn 1: %+v", res)
		}
		fid := e.followUp(id, buyerA, "the blue one")
		if res := e.dispatch(context.Background(), id, 1, fid, false); res.Outcome != lifecycle.DispatchInterrupted {
			t.Fatalf("turn 2: %+v", res)
		}
		// The workflow lost turn 2's result (the activity's outcome never reached it) and resumes the turn.
		before := e.states(id)
		res := e.dispatch(bounded(t), id, 1, fid, true)
		if res.Outcome != lifecycle.DispatchInterrupted || agent.sends.Load() != 2 {
			t.Fatalf("the resume: %+v after %d sends", res, agent.sends.Load())
		}
		if got := e.states(id); fmt.Sprint(got) != fmt.Sprint(before) || e.countState(id, a2a.TaskStateInputRequired) != 2 {
			t.Errorf("the resume logged %v, was %v", got, before)
		}
		if p := projection(t, e, id); p.Status.Message.Parts[0].Text() != "which colour?" {
			t.Errorf("projection %+v", p.Status)
		}
	})
}

// twiceAsking asks for input on the first turn and again on the second, then completes.
func twiceAsking() a2asrv.AgentExecutor {
	return a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if ec.StoredTask == nil {
				if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
					return
				}
				yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("which one?"))), nil)
				return
			}
			asked := 0
			for _, m := range ec.StoredTask.History {
				if m.Role == a2a.MessageRoleUser {
					asked++
				}
			}
			if asked < 3 {
				yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("which colour?"))), nil)
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("done"))), nil)
		}
	})
}

// A resent follow-up the upstream refuses is checked against the upstream task (D81, SPEC-CONFLICT in
// refusedResend): when the earlier send was processed and only its answer lost, the upstream's task has
// moved on (here, completed, so it refuses any further message), and the resume logs the seller's result
// instead of failing a finished task; an upstream that keeps no history and moves within GetTaskGrace is
// followed the same way; only an upstream task that stays at the projection's input-required for
// GetTaskGrace fails the task upstream_error, as §6.4's table says.
func TestResentFollowUpRefused(t *testing.T) {
	const grace = 2 * time.Second
	for _, tc := range []struct {
		name  string
		moved bool // at the follow-up's first send
		later bool // a few polls after the refused resend
	}{{"processed, answer lost", true, false}, {"taken, moves within the grace", false, true}, {"never taken", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, envOptions{opts: ExecutorOptions{PollInterval: 30 * time.Millisecond, GetTaskGrace: grace}})
			s := &staleSeller{}
			ctx, kill := context.WithCancel(context.Background())
			defer kill()
			s.onSend = func(w http.ResponseWriter, id json.RawMessage, r *http.Request) bool {
				switch s.sends.Load() {
				case 2: // the follow-up: processed (or not), and the replica dies before the answer arrives
					s.moved.Store(tc.moved)
					kill()
					<-r.Context().Done()
					return true
				default: // the resend
					msg := "invalid params: the task does not take this message"
					if tc.moved {
						msg = "task in a terminal state"
					}
					if tc.later {
						msg = "task execution is already in progress"
						gets := s.gets.Load()
						go func() {
							for end := time.Now().Add(10 * time.Second); s.gets.Load() < gets+3 && time.Now().Before(end); {
								time.Sleep(5 * time.Millisecond)
							}
							s.moved.Store(true)
						}()
					}
					rpcResult(w, id, `"error":{"code":-32004,"message":"`+msg+`"}`)
					return true
				}
			}
			e.backends.set(s.backend(t), rpcInfo, nil)
			id, wid := e.newTask(taskSpec{owner: buyerA, text: "book"})
			if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchInterrupted {
				t.Fatalf("turn 1: %+v", res)
			}
			fid := e.followUp(id, buyerA, "the blue one")
			if res := e.dispatch(ctx, id, 1, fid, false); res.Outcome != lifecycle.DispatchContextLost {
				t.Fatalf("the killed follow-up dispatch: %+v", res)
			}
			if wi := e.workItem(id, fid); wi.DeliveredAt != nil {
				t.Fatalf("a send without an answer recorded a delivery: %v", wi.DeliveredAt)
			}
			start := time.Now()
			res := e.dispatch(bounded(t), id, 1, fid, true)
			took := time.Since(start)
			p := projection(t, e, id)
			sent := s.messages()
			want := e.workItem(id, fid).Payload.Request.Message.ID
			if len(sent) != 3 || sent[1].ID != want || sent[2].ID != want || sent[2].TaskID != "up-1" || sent[2].ContextID != "up-ctx" {
				t.Errorf("sends %+v; want the first turn, then the follow-up %s twice on up-1/up-ctx", sent, want)
			}
			if tc.moved || tc.later {
				if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateCompleted || p.Status.Message.Parts[0].Text() != "booked" {
					t.Fatalf("a finished task after a refused resend: %+v %+v %v", res, p.Status, p.Metadata)
				}
				if e.countState(id, a2a.TaskStateCompleted) != 1 || e.countState(id, a2a.TaskStateFailed) != 0 {
					t.Errorf("log %v", e.states(id))
				}
				return
			}
			if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateFailed || p.Metadata[store.MetaFailure] != "upstream_error" ||
				!strings.Contains(p.Status.Message.Parts[0].Text(), "does not take this message") {
				t.Fatalf("a refused resend: %+v %+v %v", res, p.Status, p.Metadata)
			}
			if took < grace {
				t.Errorf("the refused resend failed the task after %v, before GetTaskGrace (%v)", took, grace)
			}
		})
	}
}

// A task's first turn is never sent again by a resume (D81, §6.4): after its first upstream id a resume
// re-attaches without sending, and before it a resume sends nothing and returns retryable. A first turn
// records no delivery.
func TestFirstTurnNeverResent(t *testing.T) {
	t.Run("after the first upstream id", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		g := &gatedStreamer{release: make(chan struct{}, 16), chunks: 1}
		agent, b, info := newAgent(t, g, agentConfig{streaming: true})
		info.Streaming = true
		e.backends.set(b, info, nil)
		id, wid := e.newTask(taskSpec{owner: buyerA, text: "long job"})
		ctx, kill := context.WithCancel(context.Background())
		defer kill()
		first := make(chan lifecycle.DispatchResult, 1)
		go func() { first <- e.dispatch(ctx, id, 1, wid, false) }()
		waitFor(t, "the first upstream id", func() bool { return e.attempt(id, 1).UpstreamTaskID != "" && e.row(id).State == store.StateWorking })
		kill()
		if res := <-first; res.Outcome != lifecycle.DispatchContextLost {
			t.Fatalf("the killed dispatch: %+v", res)
		}
		g.release <- struct{}{}
		g.release <- struct{}{}
		res := e.dispatch(bounded(t), id, 1, wid, true)
		if res.Outcome != lifecycle.DispatchTerminal || e.row(id).State != store.StateCompleted {
			t.Fatalf("the resume: %+v, log %v", res, e.states(id))
		}
		if agent.sends.Load() != 1 || len(received(t, agent, "long job")) != 1 {
			t.Errorf("the first message went upstream %d times", agent.sends.Load())
		}
		if wi := e.workItem(id, wid); wi.DeliveredAt != nil {
			t.Errorf("a first turn recorded a delivery: %v", wi.DeliveredAt)
		}
	})
	t.Run("before it", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		var sends atomic.Int64
		b, _ := scripted(t, func(_ string, id json.RawMessage, w http.ResponseWriter, _ *http.Request) {
			sends.Add(1)
			rpcResult(w, id, `"result":{"task":`+upWorking+`}`)
		})
		e.backends.set(b, rpcInfo, nil)
		id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
		res := e.dispatch(bounded(t), id, 1, wid, true)
		if res.Outcome != lifecycle.DispatchRetryable || sends.Load() != 0 {
			t.Fatalf("a resume before the first upstream id: %+v after %d upstream calls", res, sends.Load())
		}
		if wi := e.workItem(id, wid); wi.ConsumedAt == nil || wi.DeliveredAt != nil {
			t.Errorf("work item %+v", wi)
		}
	})
}

// asksTwiceThenWorks asks for input on the first two turns; on the third it reports working, waits for
// release, then completes with the third message's text. done is set once it completed.
type asksTwiceThenWorks struct {
	release chan struct{}
	done    atomic.Bool
}

func (g *asksTwiceThenWorks) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("which one?"))), nil)
			return
		}
		users := 0
		for _, m := range ec.StoredTask.History {
			if m.Role == a2a.MessageRoleUser {
				users++
			}
		}
		if users < 3 {
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("which colour?"))), nil)
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		select {
		case <-g.release:
		case <-ctx.Done():
			return
		}
		if yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("done: "+ec.Message.Parts[0].Text()))), nil) {
			g.done.Store(true)
		}
	}
}

func (g *asksTwiceThenWorks) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// A resume of a delivered follow-up re-attaches and follows the task to its end whatever the log's rows
// carry: U+0000 in this follow-up's text and an earlier one's (json, D49) is no error, because the turn's
// boundary is the version its work item was stored at, not a read inside the payloads. (It used to end
// every resume context_lost, until the re-attach limit failed a task the seller had completed.)
func TestFollowUpResumeWithNUL(t *testing.T) {
	e := newEnv(t, envOptions{})
	g := &asksTwiceThenWorks{release: make(chan struct{}, 1)}
	agent, b, info := newAgent(t, g, agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "book\x00a room"})
	if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchInterrupted {
		t.Fatalf("turn 1: %+v", res)
	}
	f1 := e.followUp(id, buyerA, "the blue\x00one")
	if res := e.dispatch(context.Background(), id, 1, f1, false); res.Outcome != lifecycle.DispatchInterrupted {
		t.Fatalf("turn 2: %+v, log %v", res, e.states(id))
	}
	f2 := e.followUp(id, buyerA, "red\x00please")
	ctx, kill := context.WithCancel(context.Background())
	defer kill()
	first := make(chan lifecycle.DispatchResult, 1)
	go func() { first <- e.dispatch(ctx, id, 1, f2, false) }()
	waitFor(t, "the follow-up's delivery", func() bool { return e.workItem(id, f2).DeliveredAt != nil })
	kill()
	if res := <-first; res.Outcome != lifecycle.DispatchContextLost {
		t.Fatalf("the killed follow-up dispatch: %+v", res)
	}
	g.release <- struct{}{}
	waitFor(t, "the seller to complete", g.done.Load)

	res := e.dispatch(bounded(t), id, 1, f2, true)
	p := projection(t, e, id)
	if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateCompleted || p.Status.Message.Parts[0].Text() != "done: red\x00please" {
		t.Fatalf("the resume: %+v %+v, log %v", res, p.Status, e.states(id))
	}
	if n := len(received(t, agent, "red\x00please")); n != 1 || agent.sends.Load() != 3 {
		t.Errorf("the upstream received the follow-up %d times (%d sends)", n, agent.sends.Load())
	}
}

// staleTurns is a scripted JSON-RPC upstream task up-1 that asks on its first SendMessage, asks again
// (another question) on its second, and answers every later SendMessage with its task as it still is;
// GetTask shows it completed once moved is set.
type staleTurns struct {
	moved atomic.Bool
	sends atomic.Int64
	gets  atomic.Int64
}

const (
	turnsAsk1 = `{"id":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_INPUT_REQUIRED","timestamp":"2026-10-25T10:00:00Z","message":{"messageId":"q1","role":"ROLE_AGENT","parts":[{"text":"which\u0000one?"}]}}}`
	turnsAsk2 = `{"id":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_INPUT_REQUIRED","timestamp":"2026-10-25T10:00:02Z","message":{"messageId":"q2","role":"ROLE_AGENT","parts":[{"text":"which\u0000colour?"}]}}}`
)

func (s *staleTurns) backend(t *testing.T) upstream.Backend {
	b, _ := scripted(t, func(method string, id json.RawMessage, w http.ResponseWriter, _ *http.Request) {
		switch method {
		case "SendMessage":
			switch s.sends.Add(1) {
			case 1:
				rpcResult(w, id, `"result":{"task":`+turnsAsk1+`}`)
			default:
				rpcResult(w, id, `"result":{"task":`+turnsAsk2+`}`)
			}
		default:
			s.gets.Add(1)
			if s.moved.Load() {
				rpcResult(w, id, `"result":`+staleDone)
				return
			}
			rpcResult(w, id, `"result":`+turnsAsk2)
		}
	})
	return b
}

// A delivered follow-up whose answer logged nothing (the upstream answered with the previous turn's
// input-required) is resumed by a re-attach that follows the upstream until it processed the follow-up,
// rather than ending the turn at the previous turn's end. The turn is told by the version its work item was
// stored at, so it holds when the log's rows carry U+0000 (the turn boundary used to be found by reading
// inside the payloads, which Postgres refuses for \u0000) and when the follow-up repeats an earlier
// follow-up's messageId (a2a-go logs no history row for it; the turn used to be found by that row).
func TestFollowUpResumeTurnBoundary(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("repeated messageId=%v", reuse), func(t *testing.T) {
			e := newEnv(t, envOptions{opts: ExecutorOptions{PollInterval: 30 * time.Millisecond}})
			s := &staleTurns{}
			e.backends.set(s.backend(t), rpcInfo, nil)
			id, wid := e.newTask(taskSpec{owner: buyerA, text: "book\x00a room"})
			if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchInterrupted {
				t.Fatalf("turn 1: %+v", res)
			}
			msgID := uuid.NewString()
			follow := func(text string) uuid.UUID {
				msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
				msg.TaskID = id
				if reuse {
					msg.ID = msgID
				}
				w := uuid.Must(uuid.NewV7())
				if err := e.log.InsertWorkItem(context.Background(), nil, store.WorkItem{ID: w, TaskID: uid(t, id), Kind: store.WorkExecute,
					Payload: store.WorkPayload{Request: &a2a.SendMessageRequest{Message: msg}, CallContext: callContext(buyerA, nil), Owner: buyerA}}); err != nil {
					t.Fatal(err)
				}
				return w
			}
			f1 := follow("the blue\x00one")
			if res := e.dispatch(context.Background(), id, 1, f1, false); res.Outcome != lifecycle.DispatchInterrupted {
				t.Fatalf("turn 2: %+v, log %v", res, e.states(id))
			}
			f2 := follow("red\x00please")
			ctx, kill := context.WithCancel(context.Background())
			defer kill()
			first := make(chan lifecycle.DispatchResult, 1)
			go func() { first <- e.dispatch(ctx, id, 1, f2, false) }()
			waitFor(t, "the follow-up's delivery", func() bool { return e.workItem(id, f2).DeliveredAt != nil })
			g0 := s.gets.Load()
			waitFor(t, "a stale poll", func() bool { return s.gets.Load() > g0 })
			kill()
			if res := <-first; res.Outcome != lifecycle.DispatchContextLost {
				t.Fatalf("the killed follow-up dispatch: %+v", res)
			}
			before := e.states(id)
			gets := s.gets.Load()
			resumed := make(chan lifecycle.DispatchResult, 1)
			go func() { resumed <- e.dispatch(bounded(t), id, 1, f2, true) }()
			waitFor(t, "the resume to poll the stale task", func() bool { return s.gets.Load() >= gets+3 })
			select {
			case res := <-resumed:
				t.Fatalf("the resume ended before the upstream processed the follow-up: %+v, log %v", res, e.states(id))
			default:
			}
			s.moved.Store(true)
			res := <-resumed
			p := projection(t, e, id)
			if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateCompleted {
				t.Fatalf("the resume: %+v %+v, log %v", res, p.Status, e.states(id))
			}
			if s.sends.Load() != 3 {
				t.Errorf("%d sends; the delivered follow-up was sent again", s.sends.Load())
			}
			wantBefore := "[task:TASK_STATE_SUBMITTED status:TASK_STATE_INPUT_REQUIRED message status:TASK_STATE_INPUT_REQUIRED message]"
			if reuse {
				wantBefore = "[task:TASK_STATE_SUBMITTED status:TASK_STATE_INPUT_REQUIRED message status:TASK_STATE_INPUT_REQUIRED]"
			}
			if fmt.Sprint(before) != wantBefore || e.countState(id, a2a.TaskStateInputRequired) != 2 || e.countState(id, a2a.TaskStateCompleted) != 1 {
				t.Errorf("log before the resume %v, after %v", before, e.states(id))
			}
		})
	}
}

// A follow-up turn that repeats the task's first messageId (a2a-go logs no history row for it) and had
// logged its own input-required before the workflow lost its result ends there on the resume, at once and
// without sending: the turn is told by its work item's version, not by a message row it does not have.
func TestFollowUpResumeOwnEndRepeatedFirstID(t *testing.T) {
	e := newEnv(t, envOptions{})
	agent, b, info := newAgent(t, twiceAsking(), agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "book"})
	if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchInterrupted {
		t.Fatalf("turn 1: %+v", res)
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("the blue one"))
	msg.ID, msg.TaskID = e.workItem(id, wid).Payload.Request.Message.ID, id
	fid := uuid.Must(uuid.NewV7())
	if err := e.log.InsertWorkItem(context.Background(), nil, store.WorkItem{ID: fid, TaskID: uid(t, id), Kind: store.WorkExecute,
		Payload: store.WorkPayload{Request: &a2a.SendMessageRequest{Message: msg}, CallContext: callContext(buyerA, nil), Owner: buyerA}}); err != nil {
		t.Fatal(err)
	}
	if res := e.dispatch(context.Background(), id, 1, fid, false); res.Outcome != lifecycle.DispatchInterrupted {
		t.Fatalf("turn 2: %+v log %v", res, e.states(id))
	}
	before := e.states(id)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	res := e.dispatch(ctx, id, 1, fid, true)
	if res.Outcome != lifecycle.DispatchInterrupted || agent.sends.Load() != 2 {
		t.Fatalf("the resume: %+v after %d sends, log %v", res, agent.sends.Load(), e.states(id))
	}
	if got := e.states(id); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Errorf("the resume logged %v, was %v", got, before)
	}
}

// slowFirstYield asks on the first turn; on a follow-up it works (blocks on release) before it yields
// anything, then completes. a2a-go records the follow-up in the task's history when it takes it, refuses a
// second copy while the first runs ("task execution is already in progress"), and shows the task unchanged
// at input-required until the agent yields.
type slowFirstYield struct {
	release chan struct{}
	runs    atomic.Int64 // follow-up executions started
	done    atomic.Bool
}

func (g *slowFirstYield) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("which one?"))), nil)
			return
		}
		g.runs.Add(1)
		select {
		case <-g.release:
		case <-ctx.Done():
			return
		}
		if yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("done: "+ec.Message.Parts[0].Text()))), nil) {
			g.done.Store(true)
		}
	}
}

func (g *slowFirstYield) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// The replica dies after an a2a-go seller took a follow-up and before its first event (no delivery
// recorded): the resume resends it, a2a-go refuses the copy while the first runs, and its task still shows
// the previous turn's input-required. The resume continues as a re-attach (D81) and follows the seller to
// its result, past GetTaskGrace, because the seller's history shows the follow-up; it used to fail the task
// upstream_error and lose the result.
func TestResentFollowUpRefusedWhileUpstreamWorks(t *testing.T) {
	for _, streaming := range []bool{true, false} {
		t.Run(fmt.Sprintf("streaming=%v", streaming), func(t *testing.T) {
			e := newEnv(t, envOptions{opts: ExecutorOptions{PollInterval: 30 * time.Millisecond, GetTaskGrace: 300 * time.Millisecond}})
			g := &slowFirstYield{release: make(chan struct{}, 1)}
			agent, b, info := newAgent(t, g, agentConfig{streaming: streaming})
			info.Streaming = streaming
			e.backends.set(b, info, nil)
			id, wid := e.newTask(taskSpec{owner: buyerA, text: "book a room"})
			if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchInterrupted {
				t.Fatalf("turn 1: %+v", res)
			}
			fid := e.followUp(id, buyerA, "the blue one")
			ctx, kill := context.WithCancel(context.Background())
			defer kill()
			first := make(chan lifecycle.DispatchResult, 1)
			go func() { first <- e.dispatch(ctx, id, 1, fid, false) }()
			waitFor(t, "the seller to take the follow-up", func() bool { return g.runs.Load() == 1 })
			kill()
			if res := <-first; res.Outcome != lifecycle.DispatchContextLost {
				t.Fatalf("the killed follow-up dispatch: %+v", res)
			}
			if wi := e.workItem(id, fid); wi.DeliveredAt != nil {
				t.Fatalf("a send without an answer recorded a delivery: %v", wi.DeliveredAt)
			}
			resumed := make(chan lifecycle.DispatchResult, 1)
			go func() { resumed <- e.dispatch(bounded(t), id, 1, fid, true) }()
			waitFor(t, "the resend", func() bool { return agent.sends.Load() == 3 })
			time.Sleep(time.Second) // past GetTaskGrace, the seller still working
			select {
			case res := <-resumed:
				t.Fatalf("the resume ended while the seller worked on the follow-up: %+v %+v, log %v", res, projection(t, e, id).Status, e.states(id))
			default:
			}
			g.release <- struct{}{}
			res := <-resumed
			p := projection(t, e, id)
			if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateCompleted || p.Status.Message.Parts[0].Text() != "done: the blue one" {
				t.Fatalf("the resume: %+v %+v %v, log %v", res, p.Status, p.Metadata, e.states(id))
			}
			if g.runs.Load() != 1 || len(received(t, agent, "the blue one")) != 2 {
				t.Errorf("the seller ran the follow-up %d times; it received it %d times", g.runs.Load(), len(received(t, agent, "the blue one")))
			}
			if e.countState(id, a2a.TaskStateFailed) != 0 || e.countState(id, a2a.TaskStateCompleted) != 1 {
				t.Errorf("log %v", e.states(id))
			}
		})
	}
}

// A follow-up turn is never sent again by a dispatch that finds its work item consumed unless it is a
// resume (D81): on attempt n > 1 too, where a consumed item used to pass as a retry of the turn on a new
// attempt. Only the task's first item is retried on a new attempt; a stale non-resume dispatch that finds
// a follow-up's item consumed by a resume refuses it before any upstream call.
func TestConsumedFollowUpNotSentAgain(t *testing.T) {
	e := newEnv(t, envOptions{})
	agent, b, info := newAgent(t, askingAgent(), agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	ctx := context.Background()
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "book a room"})
	// Attempt 1 consumed the first item and failed before the first upstream id; attempt 2 retries the turn.
	if err := e.log.ConsumeWorkItem(ctx, wid, uid(t, id)); err != nil {
		t.Fatal(err)
	}
	if err := e.log.EndAttempt(ctx, uid(t, id), 1, store.OutcomeUpstreamUnreachable); err != nil {
		t.Fatal(err)
	}
	if err := e.log.BeginAttempt(ctx, store.Attempt{TaskID: uid(t, id), N: 2, Backend: store.BackendDirect}); err != nil {
		t.Fatal(err)
	}
	if res := e.dispatch(ctx, id, 2, wid, false); res.Outcome != lifecycle.DispatchInterrupted || agent.sends.Load() != 1 {
		t.Fatalf("the first turn's retry on attempt 2: %+v after %d sends", res, agent.sends.Load())
	}
	fid := e.followUp(id, buyerA, "the blue one")
	if err := e.log.ConsumeWorkItem(ctx, fid, uid(t, id)); err != nil { // a resume took it
		t.Fatal(err)
	}
	_, err := e.disp.Dispatch(ctx, lifecycle.DispatchInput{TaskID: string(id), N: 2, WorkItemID: fid.String()})
	if err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Errorf("a non-resume dispatch of a consumed follow-up on attempt 2: %v", err)
	}
	if agent.sends.Load() != 1 || len(received(t, agent, "the blue one")) != 0 {
		t.Errorf("the consumed follow-up went upstream: %d sends", agent.sends.Load())
	}
	if st := e.row(id).State; st != store.StateInputRequired {
		t.Errorf("state %s", st)
	}
}
