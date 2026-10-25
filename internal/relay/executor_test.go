package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	a2apush "github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/card"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/upstream"
)

// spoofer streams a task whose metadata at every level claims gate and operator keys.
func spoofer(chunks int) a2asrv.AgentExecutor {
	return a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			task := a2a.NewSubmittedTask(ec, ec.Message)
			task.Metadata = map[string]any{"kontra.approve.url": "https://evil.example/approve", "seller.note": "kept"}
			if !yield(task, nil) {
				return
			}
			working := a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil)
			working.Metadata = map[string]any{"kontra.escrow.state": "released"}
			if !yield(working, nil) {
				return
			}
			var aid a2a.ArtifactID
			for i := range chunks {
				part := a2a.NewTextPart(fmt.Sprintf("chunk-%d;", i))
				part.Metadata = map[string]any{"kontra.escrow.hold": "x", "p": i}
				var ev *a2a.TaskArtifactUpdateEvent
				if i == 0 {
					ev = a2a.NewArtifactEvent(ec, part)
					ev.Artifact.Metadata = map[string]any{"kontra.escrow.id": "e1"}
					aid = ev.Artifact.ID
				} else {
					ev = a2a.NewArtifactUpdateEvent(ec, aid, part)
					ev.Append = true
				}
				if !yield(ev, nil) {
					return
				}
			}
			done := a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("all done"))
			done.Metadata = map[string]any{"kontra.failure": "none"}
			done.ReferenceTasks = []a2a.TaskID{ec.TaskID}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, done), nil)
		}
	})
}

func projection(t *testing.T, e *env, id a2a.TaskID) *a2a.Task {
	t.Helper()
	return e.row(id).Snapshot
}

// A streaming upstream is relayed: ids rewritten at every depth, Kontra's keys stripped, the first upstream
// id recorded before its event is logged, kontra.dispatched signalled, the attempt ended completed.
func TestStreamingRelayed(t *testing.T) {
	e := newEnv(t, envOptions{})
	agent, b, info := newAgent(t, spoofer(3), agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "go"})

	res := e.dispatch(context.Background(), id, 1, wid, false)
	if res.Outcome != lifecycle.DispatchTerminal || res.Reason != "" {
		t.Fatalf("result %+v", res)
	}
	want := []string{"task:TASK_STATE_SUBMITTED", "status:TASK_STATE_SUBMITTED", "status:TASK_STATE_WORKING", "artifact", "artifact", "artifact", "status:TASK_STATE_COMPLETED"}
	if got := e.states(id); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("log %v\nwant %v", got, want)
	}
	p := projection(t, e, id)
	if p.Status.State != a2a.TaskStateCompleted || len(p.Artifacts) != 1 || len(p.Artifacts[0].Parts) != 3 ||
		p.Artifacts[0].Parts[2].Text() != "chunk-2;" || p.Status.Message.Parts[0].Text() != "all done" {
		t.Errorf("projection %+v", p)
	}
	if p.Metadata["seller.note"] != "kept" {
		t.Errorf("seller metadata lost: %v", p.Metadata)
	}
	a := e.attempt(id, 1)
	if a.UpstreamTaskID == "" || a.UpstreamContextID == "" || a.FirstUpstreamIDAt == nil || a.EndedAt == nil ||
		a.Outcome != store.OutcomeCompleted || a.ReplicaID != replica || a.DispatchID.String() == "00000000-0000-0000-0000-000000000000" {
		t.Errorf("attempt %+v", a)
	}
	text := e.logText(id)
	for _, leak := range []string{a.UpstreamTaskID, a.UpstreamContextID, "kontra.approve", "kontra.escrow", "kontra.failure"} {
		if strings.Contains(text, leak) {
			t.Errorf("%q reached the log or the projection", leak)
		}
	}
	if !strings.Contains(text, `"referenceTaskIds":["`+string(id)+`"]`) {
		t.Errorf("the upstream's reference to its own task was not rewritten to the gate task")
	}
	if e.signals.count() != 1 {
		t.Errorf("kontra.dispatched sent %d times", e.signals.count())
	}
	if agent.sends.Load() != 1 {
		t.Errorf("upstream sends %d", agent.sends.Load())
	}
	sent := agent.sentMessages(t)
	if len(sent) != 1 || sent[0].TaskID != "" || sent[0].ContextID != "" || sent[0].Parts[0].Text() != "go" {
		t.Errorf("sent %+v", sent)
	}
	wi, _ := e.log.LoadWorkItem(context.Background(), wid, uid(t, id))
	if wi == nil || wi.ConsumedAt == nil {
		t.Error("the turn's work item was not consumed")
	}
}

// An upstream whose card streams but which refuses SendStreamingMessage (a2a-cli exec mode without
// --chunk) is followed in the same attempt by SendMessage{returnImmediately} and GetTask polling (D15).
func TestStreamingRefusedFallsBackToPolling(t *testing.T) {
	e := newEnv(t, envOptions{})
	exec := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			time.Sleep(150 * time.Millisecond)
			if !yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart("Hello World")), nil) {
				return
			}
			time.Sleep(150 * time.Millisecond)
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
		}
	})
	agent, b, info := newAgent(t, exec, agentConfig{streaming: true, refuse: true, binding: a2a.TransportProtocolHTTPJSON})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "hi"})
	res := e.dispatch(context.Background(), id, 1, wid, false)
	if res.Outcome != lifecycle.DispatchTerminal {
		t.Fatalf("result %+v", res)
	}
	p := projection(t, e, id)
	if p.Status.State != a2a.TaskStateCompleted || len(p.Artifacts) != 1 || p.Artifacts[0].Parts[0].Text() != "Hello World" {
		t.Errorf("projection %+v", p)
	}
	var methods []string
	sawReturnImmediately, gets := false, 0
	for _, r := range agent.recorded() {
		methods = append(methods, r.method)
		if strings.HasSuffix(r.method, "message:send") && strings.Contains(string(r.body), `"returnImmediately":true`) {
			sawReturnImmediately = true
		}
		if strings.HasPrefix(r.method, "GET /tasks/") {
			gets++
		}
	}
	if !strings.HasSuffix(methods[0], "message:stream") || !sawReturnImmediately || gets == 0 || agent.sends.Load() != 2 {
		t.Errorf("calls %v (returnImmediately %v, polls %d, sends %d)", methods, sawReturnImmediately, gets, agent.sends.Load())
	}
	// The refused stream sent nothing that started work: one upstream task only.
	if a := e.attempt(id, 1); a.UpstreamTaskID == "" || a.Outcome != store.OutcomeCompleted {
		t.Errorf("attempt %+v", a)
	}
}

// An upstream Message reply becomes a completed gate task carrying it (D5), on both paths.
func TestMessageReplyBecomesCompletedTask(t *testing.T) {
	e := newEnv(t, envOptions{})
	exec := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("stateless answer")), nil)
		}
	})
	for _, streaming := range []bool{true, false} {
		_, b, info := newAgent(t, exec, agentConfig{streaming: streaming})
		info.Streaming = streaming
		e.backends.set(b, info, nil)
		id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
		res := e.dispatch(context.Background(), id, 1, wid, false)
		p := projection(t, e, id)
		if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateCompleted ||
			p.Status.Message == nil || p.Status.Message.Parts[0].Text() != "stateless answer" || p.Status.Message.TaskID != id {
			t.Errorf("streaming=%v: %+v %+v", streaming, res, p.Status)
		}
		if got := e.states(id); got[len(got)-1] != "status:TASK_STATE_COMPLETED" {
			t.Errorf("streaming=%v: log %v", streaming, got)
		}
	}
}

// scripted is a hand-written JSON-RPC upstream for wire shapes a2a-go's server never produces.
func scripted(t *testing.T, fn func(method string, id json.RawMessage, w http.ResponseWriter, r *http.Request)) (upstream.Backend, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var rpc struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		fn(rpc.Method, rpc.ID, w, r)
	}))
	t.Cleanup(srv.Close)
	b, err := upstream.NewDirect(srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	return b, &calls
}

func sse(w http.ResponseWriter, id json.RawMessage, results ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, res := range results {
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,%s}\n\n", id, res)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func rpcResult(w http.ResponseWriter, id json.RawMessage, result string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,%s}`, id, result)
}

var rpcInfo = upstream.Info{ProtocolVersion: "1.0", Binding: a2a.TransportProtocolJSONRPC, Path: "/"}

// An event naming another upstream task than the attempt's fails it upstream_error; the foreign id is
// never rewritten into the gate task (spec §4).
func TestForeignTaskFails(t *testing.T) {
	e := newEnv(t, envOptions{})
	b, _ := scripted(t, func(method string, id json.RawMessage, w http.ResponseWriter, r *http.Request) {
		sse(w, id,
			`"result":{"task":{"id":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_WORKING"}}}`,
			`"result":{"statusUpdate":{"taskId":"up-2","contextId":"up-ctx","status":{"state":"TASK_STATE_COMPLETED"}}}`)
	})
	info := rpcInfo
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	res := e.dispatch(context.Background(), id, 1, wid, false)
	p := projection(t, e, id)
	if res.Outcome != lifecycle.DispatchTerminal || res.Reason != "upstream_error" || p.Status.State != a2a.TaskStateFailed ||
		p.Metadata[store.MetaFailure] != "upstream_error" {
		t.Fatalf("%+v %+v", res, p.Status)
	}
	if strings.Contains(e.logText(id), "up-2") || strings.Contains(e.logText(id), "up-1") {
		t.Error("an upstream id reached the log")
	}
	if a := e.attempt(id, 1); a.Outcome != store.OutcomeUpstreamError {
		t.Errorf("attempt outcome %s", a.Outcome)
	}
}

// Errors before the first upstream task id (spec §6.4): unreachable, 502–504 and 408/429 are retryable
// (nothing terminal logged, a2a-go's own SetTaskFailed refused); a protocol error, another 4xx, a
// malformed or oversized body fail upstream_error with the upstream's text and kontra.failure.
func TestErrorsBeforeFirstUpstreamID(t *testing.T) {
	e := newEnv(t, envOptions{})
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedURL := "http://" + closed.Addr().String()
	closed.Close()
	for _, tc := range []struct {
		name      string
		streaming bool
		backend   func() upstream.Backend
		outcome   lifecycle.DispatchOutcome
		reason    string
		retry     int64
		text      string
	}{
		{"connection refused", false, func() upstream.Backend { b, _ := upstream.NewDirect(closedURL, true); return b },
			lifecycle.DispatchRetryable, "upstream_unreachable", 0, ""},
		{"503 on the stream", true, func() upstream.Backend {
			b, _ := scripted(t, func(_ string, _ json.RawMessage, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
			return b
		}, lifecycle.DispatchRetryable, "upstream_unreachable", 0, ""},
		{"429 Retry-After", false, func() upstream.Backend {
			b, _ := scripted(t, func(_ string, _ json.RawMessage, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(429)
			})
			return b
		}, lifecycle.DispatchRetryable, "upstream_error", 7, ""},
		{"JSON-RPC error", false, func() upstream.Backend {
			b, _ := scripted(t, func(_ string, id json.RawMessage, w http.ResponseWriter, _ *http.Request) {
				rpcResult(w, id, `"error":{"code":-32602,"message":"the seller says no"}`)
			})
			return b
		}, lifecycle.DispatchTerminal, "upstream_error", 0, "the seller says no"},
		{"HTTP 400", false, func() upstream.Backend {
			b, _ := scripted(t, func(_ string, _ json.RawMessage, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(400) })
			return b
		}, lifecycle.DispatchTerminal, "upstream_error", 0, "HTTP 400"},
		{"malformed body", false, func() upstream.Backend {
			b, _ := scripted(t, func(_ string, id json.RawMessage, w http.ResponseWriter, _ *http.Request) {
				rpcResult(w, id, `"result":{"bogus":true}`)
			})
			return b
		}, lifecycle.DispatchTerminal, "upstream_error", 0, "malformed"},
		{"body over 8 MiB", false, func() upstream.Backend {
			b, _ := scripted(t, func(_ string, id json.RawMessage, w http.ResponseWriter, _ *http.Request) {
				rpcResult(w, id, `"result":{"message":{"messageId":"m","role":"ROLE_AGENT","parts":[{"text":"`+strings.Repeat("a", upstream.MaxBody)+`"}]}}`)
			})
			return b
		}, lifecycle.DispatchTerminal, "upstream_error", 0, "more than the gate accepts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := rpcInfo
			info.Streaming = tc.streaming
			e.backends.set(tc.backend(), info, nil)
			id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
			res := e.dispatch(context.Background(), id, 1, wid, false)
			if res.Outcome != tc.outcome || res.Reason != tc.reason || res.RetryAfterSeconds != tc.retry {
				t.Fatalf("result %+v", res)
			}
			row := e.row(id)
			a := e.attempt(id, 1)
			if tc.outcome == lifecycle.DispatchRetryable {
				if row.State != store.StateSubmitted || strings.Contains(e.logText(id), "TASK_STATE_FAILED") {
					t.Errorf("a retryable failure logged something terminal: %s %v", row.State, e.states(id))
				}
				if a.FirstUpstreamIDAt != nil || a.EndedAt == nil || string(a.Outcome) != tc.reason {
					t.Errorf("attempt %+v", a)
				}
				return
			}
			p := row.Snapshot
			if p.Status.State != a2a.TaskStateFailed || p.Metadata[store.MetaFailure] != tc.reason ||
				!strings.Contains(p.Status.Message.Parts[0].Text(), tc.text) {
				t.Errorf("status %+v %v", p.Status, p.Metadata)
			}
			if a.Outcome != store.OutcomeUpstreamError {
				t.Errorf("attempt outcome %s", a.Outcome)
			}
		})
	}
}

// After the first upstream task id an error fails the task with the upstream's text; a dropped stream is
// upstream_unreachable (spec §6.4).
func TestErrorAfterFirstUpstreamID(t *testing.T) {
	e := newEnv(t, envOptions{})
	info := rpcInfo
	info.Streaming = true
	b, _ := scripted(t, func(_ string, id json.RawMessage, w http.ResponseWriter, _ *http.Request) {
		sse(w, id,
			`"result":{"task":{"id":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_WORKING"}}}`,
			`"error":{"code":-32603,"message":"seller exploded mid-task"}`)
	})
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	res := e.dispatch(context.Background(), id, 1, wid, false)
	p := projection(t, e, id)
	if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateFailed || p.Metadata[store.MetaFailure] != "upstream_error" ||
		!strings.Contains(p.Status.Message.Parts[0].Text(), "seller exploded mid-task") {
		t.Fatalf("%+v %+v", res, p.Status)
	}
	if a := e.attempt(id, 1); a.UpstreamTaskID != "up-1" || a.FirstUpstreamIDAt == nil {
		t.Errorf("attempt %+v", a)
	}

	// The connection drops in the middle of the stream.
	b2, _ := scripted(t, func(_ string, id json.RawMessage, w http.ResponseWriter, r *http.Request) {
		sse(w, id, `"result":{"task":{"id":"up-9","contextId":"c","status":{"state":"TASK_STATE_WORKING"}}}`)
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.Close()
		}
	})
	e.backends.set(b2, info, nil)
	id2, wid2 := e.newTask(taskSpec{owner: buyerA, text: "q"})
	res = e.dispatch(context.Background(), id2, 1, wid2, false)
	if p := projection(t, e, id2); res.Outcome != lifecycle.DispatchTerminal || p.Metadata[store.MetaFailure] != "upstream_unreachable" {
		t.Errorf("dropped stream: %+v %+v", res, p.Status)
	}
}

// auth-required is followed without ending the turn (D54).
func TestAuthRequiredFollowed(t *testing.T) {
	e := newEnv(t, envOptions{})
	exec := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateAuthRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("log in at the bank"))), nil) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
		}
	})
	for _, streaming := range []bool{true, false} {
		_, b, info := newAgent(t, exec, agentConfig{streaming: streaming})
		info.Streaming = streaming
		e.backends.set(b, info, nil)
		id, wid := e.newTask(taskSpec{owner: buyerA, text: "pay"})
		res := e.dispatch(context.Background(), id, 1, wid, false)
		got := e.states(id)
		if res.Outcome != lifecycle.DispatchTerminal || got[len(got)-1] != "status:TASK_STATE_COMPLETED" ||
			!strings.Contains(strings.Join(got, " "), "status:TASK_STATE_AUTH_REQUIRED") {
			t.Errorf("streaming=%v: %+v %v", streaming, res, got)
		}
	}
}

// askingAgent asks for input on the first turn and completes on the second, echoing the follow-up.
func askingAgent() a2asrv.AgentExecutor {
	return a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if ec.StoredTask == nil {
				if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
					return
				}
				yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("which one?"))), nil)
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("done: "+ec.Message.Parts[0].Text()))), nil)
		}
	})
}

// A turn that stops at input-required leaves the attempt open; the follow-up turn goes to the same
// upstream task with the upstream ids substituted (spec §7).
func TestFollowUpTurn(t *testing.T) {
	e := newEnv(t, envOptions{})
	agent, b, info := newAgent(t, askingAgent(), agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "book a room"})
	if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchInterrupted {
		t.Fatalf("turn 1: %+v", res)
	}
	a := e.attempt(id, 1)
	if a.EndedAt != nil || a.UpstreamTaskID == "" {
		t.Fatalf("an input-required turn ended the attempt: %+v", a)
	}
	wid2 := e.followUp(id, buyerA, "the blue one")
	res := e.dispatch(context.Background(), id, 1, wid2, false)
	p := projection(t, e, id)
	if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateCompleted || p.Status.Message.Parts[0].Text() != "done: the blue one" {
		t.Fatalf("turn 2: %+v %+v", res, p.Status)
	}
	sent := agent.sentMessages(t)
	if len(sent) != 2 || sent[1].TaskID != a2a.TaskID(a.UpstreamTaskID) || sent[1].ContextID != a.UpstreamContextID {
		t.Errorf("the follow-up went upstream as %+v", sent[len(sent)-1])
	}
	// The caller's follow-up is in the gate's history (a message row), never an upstream id.
	if !strings.Contains(fmt.Sprint(e.states(id)), "message") || strings.Contains(e.logText(id), a.UpstreamTaskID) {
		t.Errorf("log %v", e.states(id))
	}
	if a := e.attempt(id, 1); a.Outcome != store.OutcomeCompleted || a.EndedAt == nil {
		t.Errorf("attempt %+v", a)
	}
}

// gatedStreamer streams chunk i only once release has delivered i+1 tokens, then completes; it keeps
// running when the gate's connection drops (a2a-go runs local executions detached from the request).
type gatedStreamer struct {
	release  chan struct{}
	produced atomic.Int64
	chunks   int
}

func (g *gatedStreamer) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		var aid a2a.ArtifactID
		for i := range g.chunks {
			select {
			case <-g.release:
			case <-ctx.Done():
				return
			}
			var ev *a2a.TaskArtifactUpdateEvent
			if i == 0 {
				ev = a2a.NewArtifactEvent(ec, a2a.NewTextPart(fmt.Sprintf("c%d", i)))
				aid = ev.Artifact.ID
			} else {
				ev = a2a.NewArtifactUpdateEvent(ec, aid, a2a.NewTextPart(fmt.Sprintf("c%d", i)))
				ev.Append = true
			}
			if !yield(ev, nil) {
				return
			}
			g.produced.Add(1)
		}
		select {
		case <-g.release:
		case <-ctx.Done():
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	}
}

func (g *gatedStreamer) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *env) count(id a2a.TaskID, kind store.EventKind) int {
	n := 0
	for _, ev := range e.events(id) {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// Re-attach (D42): a dispatch whose context is lost after the first upstream id is followed by a resume
// dispatch of the same attempt: GetTask logs only the difference, SubscribeToTask follows to completed,
// and the upstream receives no second send.
func TestReattach(t *testing.T) {
	e := newEnv(t, envOptions{})
	g := &gatedStreamer{release: make(chan struct{}, 16), chunks: 5}
	agent, b, info := newAgent(t, g, agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "long job"})

	ctx1, lose := context.WithCancel(context.Background())
	first := make(chan lifecycle.DispatchResult, 1)
	go func() { first <- e.dispatch(ctx1, id, 1, wid, false) }()
	g.release <- struct{}{}
	g.release <- struct{}{}
	waitFor(t, "two chunks in the log", func() bool { return e.count(id, store.KindArtifact) == 2 })
	lose() // the replica's activity context is gone (a dead replica, a heartbeat timeout)
	if res := <-first; res.Outcome != lifecycle.DispatchContextLost {
		t.Fatalf("lost dispatch: %+v", res)
	}
	if a := e.attempt(id, 1); a.EndedAt != nil {
		t.Fatalf("a lost context ended the attempt: %+v", a)
	}
	// The upstream keeps working while nobody watches.
	g.release <- struct{}{}
	g.release <- struct{}{}
	waitFor(t, "the upstream to produce 4 chunks", func() bool { return g.produced.Load() == 4 })

	second := make(chan lifecycle.DispatchResult, 1)
	go func() { second <- e.dispatch(context.Background(), id, 1, wid, true) }()
	waitFor(t, "the missed chunks logged as one difference", func() bool { return e.count(id, store.KindArtifact) == 3 })
	g.release <- struct{}{} // chunk 5
	g.release <- struct{}{} // completed
	res := <-second
	if res.Outcome != lifecycle.DispatchTerminal {
		t.Fatalf("resume: %+v", res)
	}
	if agent.sends.Load() != 1 {
		t.Errorf("the upstream received %d sends", agent.sends.Load())
	}
	p := projection(t, e, id)
	var parts []string
	for _, part := range p.Artifacts[0].Parts {
		parts = append(parts, part.Text())
	}
	if len(p.Artifacts) != 1 || strings.Join(parts, ",") != "c0,c1,c2,c3,c4" || p.Status.State != a2a.TaskStateCompleted {
		t.Errorf("projection %v %s", parts, p.Status.State)
	}
	terminals := 0
	for _, s := range e.states(id) {
		if s == "status:TASK_STATE_COMPLETED" {
			terminals++
		}
	}
	if terminals != 1 {
		t.Errorf("%d terminal events: %v", terminals, e.states(id))
	}
	// The resume's difference: one artifact event appending exactly the two chunks missed.
	evs := e.events(id)
	var diff a2a.StreamResponse
	n := 0
	for _, ev := range evs {
		if ev.Kind == store.KindArtifact {
			n++
			if n == 3 {
				_ = json.Unmarshal(ev.Payload, &diff)
			}
		}
	}
	if au, ok := diff.Event.(*a2a.TaskArtifactUpdateEvent); !ok || !au.Append || len(au.Artifact.Parts) != 2 || au.Artifact.Parts[0].Text() != "c2" {
		t.Errorf("difference event %+v", diff.Event)
	}
	var methods []string
	for _, r := range agent.recorded() {
		methods = append(methods, r.method)
	}
	if !strings.Contains(strings.Join(methods, " "), "GetTask") || !strings.Contains(strings.Join(methods, " "), "SubscribeToTask") {
		t.Errorf("resume calls %v", methods)
	}
	if a := e.attempt(id, 1); a.Outcome != store.OutcomeCompleted {
		t.Errorf("attempt %+v", a)
	}
}

// Re-attach to an upstream that no longer answers: GetTask failing for the grace period fails the task
// upstream_unreachable, and nothing is sent (spec §6.4).
func TestReattachGivesUp(t *testing.T) {
	e := newEnv(t, envOptions{opts: ExecutorOptions{GetTaskGrace: 300 * time.Millisecond, PollInterval: 50 * time.Millisecond}})
	b, calls := scripted(t, func(_ string, _ json.RawMessage, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(502) })
	e.backends.set(b, rpcInfo, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	e.firstID(id, 1, "up-gone", "up-ctx")
	if err := e.log.ConsumeWorkItem(context.Background(), wid, uid(t, id)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res := e.dispatch(context.Background(), id, 1, wid, true)
	p := projection(t, e, id)
	if res.Outcome != lifecycle.DispatchTerminal || p.Metadata[store.MetaFailure] != "upstream_unreachable" {
		t.Fatalf("%+v %+v", res, p.Status)
	}
	if took := time.Since(start); took < 300*time.Millisecond || calls.Load() < 2 {
		t.Errorf("gave up after %v and %d calls", took, calls.Load())
	}
}

// Upstream ids stay with the backend that issued them (spec §4: a recorded upstream context id is reused,
// and referenceTaskIds mapped, only on the same direct backend): after the service's backend moves to
// another seller (a SIGHUP or NOTIFY changes its URL), a later task of the same gate context reaches the
// new seller with no contextId and no referenceTaskIds of the old one.
func TestUpstreamIDsStayWithTheirBackend(t *testing.T) {
	e := newEnv(t, envOptions{})
	echo := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
		}
	})
	sellerA, bA, infoA := newAgent(t, echo, agentConfig{})
	sellerB, bB, infoB := newAgent(t, echo, agentConfig{})
	e.backends.set(bA, infoA, nil)
	t1, w1 := e.newTask(taskSpec{owner: buyerA, contextID: "conv", text: "one"})
	e.dispatch(context.Background(), t1, 1, w1, false)
	// The same backend: the second task carries the upstream context and its reference (the control).
	t2, w2 := e.newTask(taskSpec{owner: buyerA, contextID: "conv", text: "two", refs: []a2a.TaskID{t1}})
	e.dispatch(context.Background(), t2, 1, w2, false)
	up1 := e.attempt(t1, 1)
	if sent := sellerA.sentMessages(t); len(sent) != 2 || sent[1].ContextID != up1.UpstreamContextID || len(sent[1].ReferenceTasks) != 1 {
		t.Fatalf("on the same backend: %+v", sent)
	}

	// The configuration moves the service to seller B (gate/services.go's Sync writes the row so).
	e.exec(`update kontra.services set backend = '{"a2a": {"url": "http://seller-b.internal"}}', updated_at = now()
		where tenant_id = $1 and name = $2`, e.svc.TenantID, e.svc.Name)
	e.backends.set(bB, infoB, nil)
	t3, w3 := e.newTask(taskSpec{owner: buyerA, contextID: "conv", text: "three", refs: []a2a.TaskID{t1, t2}})
	e.dispatch(context.Background(), t3, 1, w3, false)
	sent := sellerB.sentMessages(t)
	if len(sent) != 1 {
		t.Fatalf("%d sends to seller B", len(sent))
	}
	if sent[0].ContextID != "" || len(sent[0].ReferenceTasks) != 0 {
		t.Errorf("seller B got seller A's ids: contextId %q, referenceTaskIds %v", sent[0].ContextID, sent[0].ReferenceTasks)
	}
	// Seller B's own context is then reused on seller B.
	up3 := e.attempt(t3, 1)
	t4, w4 := e.newTask(taskSpec{owner: buyerA, contextID: "conv", text: "four", refs: []a2a.TaskID{t3}})
	e.dispatch(context.Background(), t4, 1, w4, false)
	if sent := sellerB.sentMessages(t); len(sent) != 2 || sent[1].ContextID != up3.UpstreamContextID ||
		fmt.Sprint(sent[1].ReferenceTasks) != fmt.Sprint([]a2a.TaskID{a2a.TaskID(up3.UpstreamTaskID)}) {
		t.Errorf("on seller B after the move: %+v", sent)
	}
}

// The upstream never receives the caller's Authorization, cookies or other headers, nor its inline push
// config, on the first turn or on a re-attach (spec §6.2, §10.1).
func TestUpstreamSeesNoCallerSecrets(t *testing.T) {
	e := newEnv(t, envOptions{push: a2apush.NewInMemoryStore()})
	agent, b, info := newAgent(t, askingAgent(), agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "hi",
		headers: map[string][]string{"Authorization": {"Bearer kontra_sk_SECRET"}, "Cookie": {"session=abc"}, "X-Caller": {"private"}},
		config:  &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{URL: "https://hooks.example/cb", Token: "push-token"}}})
	if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchInterrupted {
		t.Fatalf("%+v", res)
	}
	if res := e.dispatch(context.Background(), id, 1, wid, true); res.Outcome != lifecycle.DispatchInterrupted {
		t.Fatalf("re-attach: %+v", res)
	}
	reqs := agent.recorded()
	if len(reqs) < 2 {
		t.Fatalf("%d upstream requests", len(reqs))
	}
	for _, r := range reqs {
		for _, h := range []string{"Authorization", "Cookie", "X-Caller"} {
			if r.header.Get(h) != "" {
				t.Errorf("%s: %s reached the upstream", r.method, h)
			}
		}
		for _, s := range []string{"taskPushNotificationConfig", "hooks.example", "push-token", "SECRET"} {
			if strings.Contains(string(r.body), s) {
				t.Errorf("%s: %q reached the upstream", r.method, s)
			}
		}
	}
}

// cross_buyer, upstream half (spec §4, D53): the first task of a gate context goes upstream with no
// contextId; two buyers using the same context string reach two upstream contexts; a later task of the
// same buyer and context carries the context id the upstream gave the first.
func TestCrossBuyerUpstreamContexts(t *testing.T) {
	e := newEnv(t, envOptions{})
	echo := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
		}
	})
	agent, b, info := newAgent(t, echo, agentConfig{})
	e.backends.set(b, info, nil)
	a1, w1 := e.newTask(taskSpec{owner: buyerA, contextID: "shared", text: "a1"})
	e.dispatch(context.Background(), a1, 1, w1, false)
	b1, wb := e.newTask(taskSpec{owner: buyerB, contextID: "shared", text: "b1"})
	e.dispatch(context.Background(), b1, 1, wb, false)
	a2, w2 := e.newTask(taskSpec{owner: buyerA, contextID: "shared", text: "a2", refs: []a2a.TaskID{a1, b1}})
	e.dispatch(context.Background(), a2, 1, w2, false)

	sent := agent.sentMessages(t)
	if len(sent) != 3 {
		t.Fatalf("%d sends", len(sent))
	}
	upA1, upB1 := e.attempt(a1, 1), e.attempt(b1, 1)
	if sent[0].ContextID != "" || sent[1].ContextID != "" {
		t.Errorf("a first task of a gate context sent contextId %q / %q", sent[0].ContextID, sent[1].ContextID)
	}
	if upA1.UpstreamContextID == "" || upA1.UpstreamContextID == upB1.UpstreamContextID {
		t.Errorf("buyers A and B share an upstream context: %q %q", upA1.UpstreamContextID, upB1.UpstreamContextID)
	}
	if sent[2].ContextID != upA1.UpstreamContextID {
		t.Errorf("buyer A's second task went with context %q, want %q", sent[2].ContextID, upA1.UpstreamContextID)
	}
	// referenceTaskIds: A's own task maps to its upstream id; B's task is not A's and is dropped.
	if fmt.Sprint(sent[2].ReferenceTasks) != fmt.Sprint([]a2a.TaskID{a2a.TaskID(upA1.UpstreamTaskID)}) {
		t.Errorf("references %v", sent[2].ReferenceTasks)
	}
	for _, m := range sent {
		if m.ContextID == "shared" {
			t.Error("a caller's context id reached the upstream")
		}
	}
}

// Limits (spec §6.6, §10.4): an upstream part over 256 KiB fails the task upstream_error.
func TestUpstreamPartLimit(t *testing.T) {
	e := newEnv(t, envOptions{})
	exec := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			if !yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart(strings.Repeat("x", upstream.MaxPart+1))), nil) {
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
		}
	})
	_, b, info := newAgent(t, exec, agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	res := e.dispatch(context.Background(), id, 1, wid, false)
	p := projection(t, e, id)
	if res.Outcome != lifecycle.DispatchTerminal || p.Metadata[store.MetaFailure] != "upstream_error" || len(p.Artifacts) != 0 {
		t.Errorf("%+v %+v", res, p)
	}
}

// limits.task_bytes bounds everything an upstream makes the task store, not only part content (spec §6.6:
// "nothing an upstream sends is unbounded"): events made of metadata, filenames, artifact names and status
// updates without a message count their whole encoding, and the task fails upstream_error once they pass
// the budget, with the log holding at most the budget and the event that would pass it.
func TestTaskBytesCountsWholeEvents(t *testing.T) {
	const budget = 1 << 20
	big := func(c string) string { return strings.Repeat(c, 100<<10) }
	for name, ev := range map[string]func(ec *a2asrv.ExecutorContext, i int) a2a.Event{
		"artifacts of metadata and names": func(ec *a2asrv.ExecutorContext, i int) a2a.Event {
			p := a2a.NewTextPart("x")
			p.Metadata, p.Filename = map[string]any{"pad": big("p")}, big("f")
			ev := a2a.NewArtifactEvent(ec, p)
			ev.Metadata = map[string]any{"pad": big("e")}
			ev.Artifact.Metadata, ev.Artifact.Name, ev.Artifact.Description = map[string]any{"pad": big("a")}, big("n"), big("d")
			return ev
		},
		"status updates without a message": func(ec *a2asrv.ExecutorContext, i int) a2a.Event {
			ev := a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil)
			ev.Metadata = map[string]any{"pad": big("s"), "i": i}
			return ev
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, envOptions{opts: ExecutorOptions{TaskBytes: budget}})
			var sent atomic.Int64
			exec := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
				return func(yield func(a2a.Event, error) bool) {
					if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
						return
					}
					for i := range 40 {
						if !yield(ev(ec, i), nil) {
							return
						}
						sent.Add(1)
					}
					yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
				}
			})
			_, b, info := newAgent(t, exec, agentConfig{streaming: true})
			info.Streaming = true
			e.backends.set(b, info, nil)
			id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
			res := e.dispatch(context.Background(), id, 1, wid, false)
			p := projection(t, e, id)
			if res.Outcome != lifecycle.DispatchTerminal || p.Metadata[store.MetaFailure] != "upstream_error" {
				t.Fatalf("%+v: %s %v after %d events of about 500 KiB against a 1 MiB budget", res, p.Status.State, p.Metadata, sent.Load())
			}
			var stored int
			for _, ev := range e.events(id) {
				stored += len(ev.Payload)
			}
			if stored > budget+(600<<10) {
				t.Errorf("the log stores %d bytes against a budget of %d", stored, budget)
			}
		})
	}
}

// A card that is unavailable fails the task service_unavailable at its first attempt (spec §6.4).
func TestServiceUnavailable(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.backends.set(nil, upstream.Info{}, card.ErrUnavailable)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	res := e.dispatch(context.Background(), id, 1, wid, false)
	if p := projection(t, e, id); res.Outcome != lifecycle.DispatchTerminal || p.Metadata[store.MetaFailure] != "service_unavailable" {
		t.Errorf("%+v %+v", res, p.Status)
	}
	if a := e.attempt(id, 1); a.Outcome != store.OutcomeServiceUnavailable {
		t.Errorf("attempt %+v", a)
	}
}

// A first dispatch whose work item is missing fails the task lifecycle_unavailable (D63, D66).
func TestMissingWorkItem(t *testing.T) {
	e := newEnv(t, envOptions{})
	_, b, info := newAgent(t, askingAgent(), agentConfig{})
	e.backends.set(b, info, nil)
	id, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
	other, otherItem := e.newTask(taskSpec{owner: buyerA, text: "other"})
	res := e.dispatch(context.Background(), id, 1, otherItem, false) // another task's item
	if p := projection(t, e, id); res.Outcome != lifecycle.DispatchTerminal || p.Metadata[store.MetaFailure] != "lifecycle_unavailable" {
		t.Errorf("%+v %+v", res, p.Status)
	}
	if wi, _ := e.log.LoadWorkItem(context.Background(), otherItem, uid(t, other)); wi == nil || wi.ConsumedAt != nil {
		t.Error("another task's work item was consumed")
	}
}

// A newer dispatch of the same attempt fences an older one that is still alive on this replica (D77):
// the older stops (superseded) and writes nothing more; the newer finishes the task.
func TestNewerDispatchFencesOlder(t *testing.T) {
	e := newEnv(t, envOptions{})
	g := &gatedStreamer{release: make(chan struct{}, 16), chunks: 2}
	agent, b, info := newAgent(t, g, agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	first := make(chan lifecycle.DispatchResult, 1)
	go func() { first <- e.dispatch(context.Background(), id, 1, wid, false) }()
	waitFor(t, "the first upstream id", func() bool { return e.attempt(id, 1).FirstUpstreamIDAt != nil })
	old := e.attempt(id, 1).DispatchID

	second := make(chan lifecycle.DispatchResult, 1)
	go func() { second <- e.dispatch(context.Background(), id, 1, wid, true) }()
	if res := <-first; res.Outcome != lifecycle.DispatchContextLost {
		t.Errorf("the older dispatch: %+v", res)
	}
	waitFor(t, "the newer dispatch's fence", func() bool { return e.attempt(id, 1).DispatchID != old })
	for range 3 {
		g.release <- struct{}{}
	}
	if res := <-second; res.Outcome != lifecycle.DispatchTerminal {
		t.Fatalf("the newer dispatch: %+v", res)
	}
	if agent.sends.Load() != 1 {
		t.Errorf("%d sends", agent.sends.Load())
	}
	p := projection(t, e, id)
	if p.Status.State != a2a.TaskStateCompleted || len(p.Artifacts) != 1 || len(p.Artifacts[0].Parts) != 2 {
		t.Errorf("projection %+v", p)
	}
}

// abort before any upstream task stops the dispatch in process; nothing terminal is logged (spec §8).
func TestAbortBeforeFirstUpstreamID(t *testing.T) {
	e := newEnv(t, envOptions{})
	entered := make(chan struct{}, 1)
	b, _ := scripted(t, func(_ string, _ json.RawMessage, w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-r.Context().Done() // the upstream has not answered yet
	})
	e.backends.set(b, rpcInfo, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	done := make(chan lifecycle.DispatchResult, 1)
	go func() { done <- e.dispatch(context.Background(), id, 1, wid, false) }()
	<-entered
	if err := e.disp.Abort(context.Background(), lifecycle.AttemptRef{TaskID: string(id), N: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if res.Outcome != lifecycle.DispatchAborted {
			t.Errorf("%+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("abort did not stop the dispatch")
	}
	if row := e.row(id); row.State != store.StateSubmitted {
		t.Errorf("state %s", row.State)
	}
	if a := e.attempt(id, 1); a.Outcome != store.OutcomeAborted || a.FirstUpstreamIDAt != nil {
		t.Errorf("attempt %+v", a)
	}
	// Aborting what no longer runs is not an error.
	if err := e.disp.Abort(context.Background(), lifecycle.AttemptRef{TaskID: string(id), N: 1}); err != nil {
		t.Error(err)
	}
}

// forward_cancel sends CancelTask upstream and returns its answer; a cancel the upstream accepts ends the
// running dispatch with the upstream's canceled status.
func TestForwardCancel(t *testing.T) {
	e := newEnv(t, envOptions{})
	g := &gatedStreamer{release: make(chan struct{}, 16), chunks: 3}
	_, b, info := newAgent(t, g, agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	done := make(chan lifecycle.DispatchResult, 1)
	go func() { done <- e.dispatch(context.Background(), id, 1, wid, false) }()
	waitFor(t, "the first upstream id", func() bool { return e.attempt(id, 1).FirstUpstreamIDAt != nil })
	res, err := e.disp.ForwardCancel(context.Background(), lifecycle.AttemptRef{TaskID: string(id), N: 1})
	if err != nil || res.Outcome != lifecycle.CancelCanceled {
		t.Fatalf("%+v %v", res, err)
	}
	if d := <-done; d.Outcome != lifecycle.DispatchTerminal {
		t.Errorf("dispatch %+v", d)
	}
	if p := projection(t, e, id); p.Status.State != a2a.TaskStateCanceled {
		t.Errorf("state %s", p.Status.State)
	}

	// The upstream refuses: its error, capped, with the task untouched.
	for _, tc := range []struct {
		rpc     string
		outcome lifecycle.CancelOutcome
		code    int
	}{
		{`"error":{"code":-32002,"message":"too late to cancel ` + strings.Repeat("x", 2000) + `"}`, lifecycle.CancelNotCancelable, -32002},
		{`"error":{"code":-32004,"message":"cancel is not supported"}`, lifecycle.CancelUnsupported, -32004},
	} {
		b2, _ := scripted(t, func(method string, id json.RawMessage, w http.ResponseWriter, _ *http.Request) {
			if method != "CancelTask" {
				w.WriteHeader(400)
				return
			}
			rpcResult(w, id, tc.rpc)
		})
		e.backends.set(b2, rpcInfo, nil)
		id2, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
		e.firstID(id2, 1, "up-x", "c")
		res, err := e.disp.ForwardCancel(context.Background(), lifecycle.AttemptRef{TaskID: string(id2), N: 1})
		if err != nil || res.Outcome != tc.outcome || res.UpstreamError == nil || res.UpstreamError.Code != tc.code ||
			len(res.UpstreamError.Message) > upstream.MaxMessage+len("…") {
			t.Errorf("%+v %v", res, err)
		}
		if row := e.row(id2); row.State != store.StateSubmitted {
			t.Errorf("the task changed: %s", row.State)
		}
	}
	// The upstream answers with its task, no error: canceled only for its canceled status (spec §8); a task
	// it keeps running is no cancel (continues), and one that ended otherwise is not cancelable. A2A 1.0.1
	// §3.1.5: "success is not guaranteed", the call "returns its updated state".
	for _, tc := range []struct {
		state   string
		outcome lifecycle.CancelOutcome
	}{
		{"TASK_STATE_CANCELED", lifecycle.CancelCanceled},
		{"TASK_STATE_WORKING", lifecycle.CancelContinues},
		{"TASK_STATE_INPUT_REQUIRED", lifecycle.CancelContinues},
		{"TASK_STATE_COMPLETED", lifecycle.CancelNotCancelable},
		{"TASK_STATE_FAILED", lifecycle.CancelNotCancelable},
	} {
		b2, _ := scripted(t, func(method string, id json.RawMessage, w http.ResponseWriter, _ *http.Request) {
			if method != "CancelTask" {
				w.WriteHeader(400)
				return
			}
			rpcResult(w, id, `"result":{"id":"up-x","contextId":"c","status":{"state":"`+tc.state+`"}}`)
		})
		e.backends.set(b2, rpcInfo, nil)
		id2, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
		e.firstID(id2, 1, "up-x", "c")
		res, err := e.disp.ForwardCancel(context.Background(), lifecycle.AttemptRef{TaskID: string(id2), N: 1})
		if err != nil || res.Outcome != tc.outcome || res.UpstreamState != tc.state || res.UpstreamError != nil {
			t.Errorf("upstream answers %s: %+v %v, want %s", tc.state, res, err, tc.outcome)
		}
		if row := e.row(id2); row.State != store.StateSubmitted {
			t.Errorf("the task changed: %s", row.State)
		}
	}
	// No upstream task: nothing to forward.
	id3, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
	if _, err := e.disp.ForwardCancel(context.Background(), lifecycle.AttemptRef{TaskID: string(id3), N: 1}); err == nil {
		t.Error("forward_cancel without an upstream task succeeded")
	}
}

// a2a-go's own failure (SetTaskFailed, a failed status without a message) is refused while the dispatch
// is registered, and the relay's failure (with its message) is stored: the registry is the store's guard.
func TestRegistryGuardsTheStore(t *testing.T) {
	e := newEnv(t, envOptions{})
	_, b, info := newAgent(t, askingAgent(), agentConfig{})
	e.backends.set(b, info, nil)
	id, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
	sh, err := e.handlers.For(context.Background(), e.svc)
	if err != nil {
		t.Fatal(err)
	}
	_, cancel := context.WithCancelCause(context.Background())
	d, _ := e.reg.Register(Key{TaskID: id, N: 1}, cancel)
	ctx, cc := a2asrv.NewCallContext(context.Background(), nil)
	cc.User = &a2asrv.User{Name: buyerA.BuyerID.String(), Authenticated: true, Attributes: map[string]any{AttrPrincipal: buyerA.Principal, AttrVia: buyerA.Via}}
	st, err := sh.Store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	synth := a2a.NewStatusUpdateEvent(st.Task, a2a.TaskStateFailed, nil)
	next := *st.Task
	next.Status = synth.Status
	if _, err := sh.Store.Update(ctx, &taskstore.UpdateRequest{Task: &next, Event: synth, PrevTask: st.Task, PrevVersion: st.Version}); !errors.Is(err, store.ErrSynthesizedFailure) {
		t.Errorf("a2a-go's failure: %v", err)
	}
	mine := Failed(st.Task, "upstream_error", "the relay's own failure")
	d.RecordTerminal(mine)
	mineNext := *st.Task
	mineNext.Status = mine.Status
	if _, err := sh.Store.Update(ctx, &taskstore.UpdateRequest{Task: &mineNext, Event: mine, PrevTask: st.Task, PrevVersion: st.Version}); err != nil {
		t.Errorf("the relay's failure: %v", err)
	}
	d.Done()
}

// An error of a2a-go's handler that is not a lost context (here a validation failure: the stored message
// names another context than the task's) fails the task upstream_error, never context_lost (D42 rev. 3).
func TestHandlerErrorIsUpstreamError(t *testing.T) {
	e := newEnv(t, envOptions{})
	agent, b, info := newAgent(t, askingAgent(), agentConfig{})
	e.backends.set(b, info, nil)
	id, _ := e.newTask(taskSpec{owner: buyerA, contextID: "ctx-x", text: "q"})
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("q"))
	msg.ContextID = "ctx-other"
	wid := uuid.Must(uuid.NewV7())
	if err := e.log.InsertWorkItem(context.Background(), nil, store.WorkItem{ID: wid, TaskID: uid(t, id), Kind: store.WorkExecute,
		Payload: store.WorkPayload{Request: &a2a.SendMessageRequest{Message: msg}, CallContext: callContext(buyerA, nil), Owner: buyerA}}); err != nil {
		t.Fatal(err)
	}
	res := e.dispatch(context.Background(), id, 1, wid, false)
	if p := projection(t, e, id); res.Outcome != lifecycle.DispatchTerminal || p.Metadata[store.MetaFailure] != "upstream_error" {
		t.Errorf("%+v %+v", res, p.Status)
	}
	if agent.sends.Load() != 0 {
		t.Error("the upstream was called")
	}
	if a := e.attempt(id, 1); a.Outcome != store.OutcomeUpstreamError {
		t.Errorf("attempt %+v", a)
	}
}

func TestExecutorDefaults(t *testing.T) {
	o := ExecutorOptions{}.withDefaults()
	if o.PollInterval != time.Second || o.PollMax != 5*time.Second || o.PollSlowAfter != time.Minute ||
		o.GetTaskGrace != 30*time.Second || o.GetTaskTimeout != 10*time.Second || o.TaskBytes != 256<<20 || o.MaxPart != 256<<10 {
		t.Errorf("%+v", o)
	}
	// One GetTask never outlasts the grace.
	if o := (ExecutorOptions{GetTaskGrace: 3 * time.Second}).withDefaults(); o.GetTaskTimeout != 3*time.Second {
		t.Errorf("%+v", o)
	}
}
