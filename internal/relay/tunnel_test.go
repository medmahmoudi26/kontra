package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
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
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/upstream"
)

// testNode stands in for a tunnel session's node (tunnel.md §8): every exchange the gate opens is a pipe
// served by the node's service, unless the test makes the node answer it itself (Kontra-Node), cut it, or
// the session is gone (the open fails upstream.ErrTunnelLost). It records each exchange's gate task (the
// preamble's task and context) and JSON-RPC method.
type testNode struct {
	svc     http.Handler
	opens   atomic.Int64
	gone    atomic.Bool
	mu      sync.Mutex
	tasks   []upstream.ExchangeTask
	methods []string
	// answer, when set, may answer an exchange instead of the service (id is the JSON-RPC request's, k
	// counts the exchanges of the method so far): it returns true when it did.
	answer func(method string, id json.RawMessage, k int, w http.ResponseWriter) bool
}

func (n *testNode) setAnswer(fn func(method string, id json.RawMessage, k int, w http.ResponseWriter) bool) {
	n.mu.Lock()
	n.answer = fn
	n.mu.Unlock()
}

func (n *testNode) setService(h http.Handler) {
	n.mu.Lock()
	n.svc = h
	n.mu.Unlock()
}

func (n *testNode) OpenExchange(ctx context.Context) (net.Conn, error) {
	n.opens.Add(1)
	if n.gone.Load() {
		return nil, fmt.Errorf("%w: the test's session is gone", upstream.ErrTunnelLost)
	}
	t, _ := upstream.TaskFrom(ctx)
	n.mu.Lock()
	n.tasks = append(n.tasks, t)
	n.mu.Unlock()
	gate, node := net.Pipe()
	go servePipe(node, http.HandlerFunc(n.serve))
	return gate, nil
}

func (n *testNode) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	var rpc struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(body, &rpc)
	n.mu.Lock()
	n.methods = append(n.methods, rpc.Method)
	count := 0
	for _, m := range n.methods {
		if m == rpc.Method {
			count++
		}
	}
	answer, svc := n.answer, n.svc
	n.mu.Unlock()
	if answer != nil && answer(rpc.Method, rpc.ID, count, w) {
		return
	}
	svc.ServeHTTP(w, r)
}

func (n *testNode) calls(method string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, m := range n.methods {
		if m == method {
			c++
		}
	}
	return c
}

func (n *testNode) seenTasks() []upstream.ExchangeTask {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]upstream.ExchangeTask(nil), n.tasks...)
}

// nodeAnswer writes a response the node writes itself (tunnel.md §8.2).
func nodeAnswer(w http.ResponseWriter, status int, reason tunnel.NodeReason) {
	w.Header().Set(tunnel.HeaderNode, string(reason))
	w.WriteHeader(status)
}

// cutExchange ends the exchange's stream without a complete response.
func cutExchange(w http.ResponseWriter) {
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = conn.Close()
	}
}

// servePipe serves the one HTTP/1.1 exchange on c with h.
func servePipe(c net.Conn, h http.Handler) {
	l := &pipeListener{ch: make(chan net.Conn, 1), done: make(chan struct{})}
	l.ch <- &pipeConn{Conn: c, l: l}
	_ = (&http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}).Serve(l)
}

type pipeListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{} }

type pipeConn struct {
	net.Conn
	l *pipeListener
}

func (c *pipeConn) Close() error { _ = c.l.Close(); return c.Conn.Close() }

// sessions is relay.Backends for tunnel attempts as the gate's registry answers it: the backend of the
// attempt's session when this replica holds it (an upstream.Tunnel over its node), a lost session
// otherwise.
type sessions struct {
	mu    sync.Mutex
	nodes map[uuid.UUID]*testNode
	info  upstream.Info
}

func (s *sessions) Backend(_ context.Context, svc store.ServiceRef, a *store.Attempt) (upstream.Backend, upstream.Info, error) {
	if a == nil || a.Backend != store.BackendTunnel || a.TunnelSession == nil {
		return nil, upstream.Info{}, fmt.Errorf("%w: not a tunnel attempt", upstream.ErrTunnelLost)
	}
	s.mu.Lock()
	n := s.nodes[*a.TunnelSession]
	s.mu.Unlock()
	if n == nil {
		return nil, upstream.Info{}, fmt.Errorf("%w: this replica holds no session %s", upstream.ErrTunnelLost, a.TunnelSession)
	}
	b, err := upstream.NewTunnel(svc.Name, tunnel.KindA2A, n)
	return b, s.info, err
}

func (s *sessions) drop(id uuid.UUID) {
	s.mu.Lock()
	delete(s.nodes, id)
	s.mu.Unlock()
}

// sellerHandler is an a2a-go seller's JSON-RPC handler at /a2a/.
func sellerHandler(exec a2asrv.AgentExecutor) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/a2a/", http.StripPrefix("/a2a", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(exec))))
	return mux
}

// tunnelEnv is the relay over tunnel sessions: a node per session, each behind exec.
func tunnelEnv(t *testing.T, o envOptions, streaming bool, exec a2asrv.AgentExecutor, ids ...uuid.UUID) (*env, *sessions, map[uuid.UUID]*testNode) {
	t.Helper()
	e := newEnv(t, o)
	ss := &sessions{nodes: map[uuid.UUID]*testNode{},
		info: upstream.Info{ProtocolVersion: "1.0", Binding: a2a.TransportProtocolJSONRPC, Path: "/a2a/", Streaming: streaming}}
	nodes := map[uuid.UUID]*testNode{}
	for _, id := range ids {
		n := &testNode{svc: sellerHandler(exec)}
		ss.nodes[id], nodes[id] = n, n
	}
	e.backends.use(ss)
	return e, ss, nodes
}

func newSession() uuid.UUID { return uuid.Must(uuid.NewV7()) }

// A task on a tunnel session reaches its node's service through the tunnel backend and completes, on the
// streaming path and on the polling path; every exchange's preamble names the gate task and context
// (tunnel.md §8.1), and the dispatch is that of a direct backend in every other way.
func TestTunnelDispatch(t *testing.T) {
	for _, streaming := range []bool{true, false} {
		s1 := newSession()
		e, _, nodes := tunnelEnv(t, envOptions{}, streaming, askingAgent(), s1)
		id, wid := e.newTask(taskSpec{owner: buyerA, text: "book a room", session: &s1})
		if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchInterrupted {
			t.Fatalf("streaming=%v: turn 1 %+v", streaming, res)
		}
		a := e.attempt(id, 1)
		if a.UpstreamTaskID == "" || a.Backend != store.BackendTunnel || a.EndedAt != nil {
			t.Fatalf("streaming=%v: attempt %+v", streaming, a)
		}
		row := e.row(id)
		for _, got := range nodes[s1].seenTasks() {
			if got != (upstream.ExchangeTask{TaskID: string(id), ContextID: row.ContextID}) {
				t.Errorf("streaming=%v: an exchange's preamble task %+v, want the gate's ids", streaming, got)
			}
		}
		if p := projection(t, e, id); p.Status.State != a2a.TaskStateInputRequired || strings.Contains(e.logText(id), a.UpstreamTaskID) {
			t.Errorf("streaming=%v: %+v", streaming, p.Status)
		}
	}
}

// A dispatch whose session this replica does not hold (relay.md §14.2: "a dispatch that finds its session
// missing from the replica's registry fails tunnel_lost at once") returns DispatchTunnelLost, logs nothing
// and opens no stream; the workflow then retries on another session (§6.4). Its attempt is left for the
// workflow to end (prepare's retry ends it tunnel_lost).
func TestTunnelSessionMissing(t *testing.T) {
	s1, gone := newSession(), newSession()
	e, _, nodes := tunnelEnv(t, envOptions{}, true, askingAgent(), s1)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q", session: &gone})
	before := e.events(id)
	res := e.dispatch(context.Background(), id, 1, wid, false)
	if res.Outcome != lifecycle.DispatchTunnelLost || res.Reason != "tunnel_lost" {
		t.Fatalf("result %+v", res)
	}
	if n := nodes[s1].opens.Load(); n != 0 {
		t.Fatalf("%d streams opened", n)
	}
	if after := e.events(id); len(after) != len(before) {
		t.Fatalf("logged %v", e.states(id))
	}
	if a := e.attempt(id, 1); a.EndedAt != nil || a.FirstUpstreamIDAt != nil || a.ReplicaID != replica {
		t.Fatalf("attempt %+v", a)
	}
}

// Spec §6.4's tunnel rows before the first upstream task id: a stream that could not be opened, one cut
// before any response, a node's full answer and a node's target_unreachable answer each make the dispatch
// retryable with reason tunnel_lost (the workflow retries on another session); nothing is logged, and the
// attempt ends tunnel_lost.
func TestTunnelLostBeforeFirstUpstreamID(t *testing.T) {
	s1 := newSession()
	e, _, nodes := tunnelEnv(t, envOptions{}, true, askingAgent(), s1)
	n := nodes[s1]
	for name, arm := range map[string]func(){
		"open fails": func() { n.gone.Store(true) },
		"cut":        func() { n.setAnswer(cutAll) },
		"full": func() {
			n.setAnswer(func(_ string, _ json.RawMessage, _ int, w http.ResponseWriter) bool {
				nodeAnswer(w, http.StatusServiceUnavailable, tunnel.NodeFull)
				return true
			})
		},
		"target_unreachable": func() {
			n.setAnswer(func(_ string, _ json.RawMessage, _ int, w http.ResponseWriter) bool {
				nodeAnswer(w, http.StatusBadGateway, tunnel.NodeTargetUnreachable)
				return true
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			n.gone.Store(false)
			n.setAnswer(nil)
			arm()
			id, wid := e.newTask(taskSpec{owner: buyerA, text: "q", session: &s1})
			res := e.dispatch(context.Background(), id, 1, wid, false)
			if res.Outcome != lifecycle.DispatchRetryable || res.Reason != "tunnel_lost" {
				t.Fatalf("result %+v", res)
			}
			if row := e.row(id); row.State != store.StateSubmitted || strings.Contains(e.logText(id), "TASK_STATE_FAILED") {
				t.Fatalf("logged %v", e.states(id))
			}
			if a := e.attempt(id, 1); a.Outcome != store.OutcomeTunnelLost || a.EndedAt == nil || a.FirstUpstreamIDAt != nil {
				t.Fatalf("attempt %+v", a)
			}
		})
	}
}

func cutAll(_ string, _ json.RawMessage, _ int, w http.ResponseWriter) bool {
	cutExchange(w)
	return true
}

// After the first upstream task id a lost session fails the task tunnel_lost (spec §6.4: "the upstream
// task's state is unrecoverable from outside"), with kontra.failure and the gate's status message: a stream
// cut after its first event, a poll whose session is gone, and a node's target_unreachable answer to a poll.
func TestTunnelLostAfterFirstUpstreamID(t *testing.T) {
	working := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
				return
			}
			<-ctx.Done()
		}
	})
	for _, tc := range []struct {
		name      string
		streaming bool
		arm       func(n *testNode)
	}{
		{"stream cut after its first event", true, func(n *testNode) {
			n.setAnswer(func(method string, id json.RawMessage, _ int, w http.ResponseWriter) bool {
				if method != "SendStreamingMessage" {
					return false
				}
				sse(w, id, `"result":{"task":{"id":"up-1","contextId":"up-c","status":{"state":"TASK_STATE_WORKING"}}}`)
				cutExchange(w)
				return true
			})
		}},
		{"the session gone while polling", false, func(n *testNode) {
			n.setAnswer(func(method string, _ json.RawMessage, _ int, w http.ResponseWriter) bool {
				if method == "GetTask" {
					n.gone.Store(true) // this poll is answered; the next one finds the session gone
				}
				return false
			})
		}},
		{"target_unreachable on a poll", false, func(n *testNode) {
			n.setAnswer(func(method string, _ json.RawMessage, _ int, w http.ResponseWriter) bool {
				if method != "GetTask" {
					return false
				}
				nodeAnswer(w, http.StatusBadGateway, tunnel.NodeTargetUnreachable)
				return true
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s1 := newSession()
			e, _, nodes := tunnelEnv(t, envOptions{}, tc.streaming, working, s1)
			tc.arm(nodes[s1])
			id, wid := e.newTask(taskSpec{owner: buyerA, text: "q", session: &s1})
			res := e.dispatch(context.Background(), id, 1, wid, false)
			p := projection(t, e, id)
			if res.Outcome != lifecycle.DispatchTerminal || res.Reason != "tunnel_lost" || p.Status.State != a2a.TaskStateFailed ||
				p.Metadata[store.MetaFailure] != "tunnel_lost" || p.Status.Message == nil || p.Status.Message.Parts[0].Text() == "" {
				t.Fatalf("%+v %+v %v", res, p.Status, p.Metadata)
			}
			if a := e.attempt(id, 1); a.Outcome != store.OutcomeTunnelLost || a.FirstUpstreamIDAt == nil {
				t.Errorf("attempt %+v", a)
			}
		})
	}
}

// A node at its in-flight cap (503, Kontra-Node: full) refuses a poll after the first upstream task id:
// the dispatch polls again with backoff and the task continues (spec §6.4); a node that stays full for the
// grace fails the task tunnel_lost.
func TestTunnelNodeFullWhilePolling(t *testing.T) {
	slowDone := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
				return
			}
			time.Sleep(300 * time.Millisecond)
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("done"))), nil)
		}
	})
	opts := envOptions{opts: ExecutorOptions{PollInterval: 50 * time.Millisecond, PollMax: 200 * time.Millisecond, GetTaskGrace: 2 * time.Second}}
	s1 := newSession()
	e, _, nodes := tunnelEnv(t, opts, false, slowDone, s1)
	n := nodes[s1]
	n.setAnswer(func(method string, _ json.RawMessage, k int, w http.ResponseWriter) bool {
		if method == "GetTask" && k <= 3 {
			nodeAnswer(w, http.StatusServiceUnavailable, tunnel.NodeFull)
			return true
		}
		return false
	})
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q", session: &s1})
	res := e.dispatch(context.Background(), id, 1, wid, false)
	if p := projection(t, e, id); res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("%+v %+v", res, p.Status)
	}
	if c := n.calls("GetTask"); c < 4 {
		t.Fatalf("%d polls: the full ones were not made again", c)
	}
	if c := n.calls("SendMessage"); c != 1 {
		t.Fatalf("%d sends", c)
	}

	// Full for longer than the grace: tunnel_lost.
	n.setAnswer(func(method string, _ json.RawMessage, _ int, w http.ResponseWriter) bool {
		if method == "GetTask" {
			nodeAnswer(w, http.StatusServiceUnavailable, tunnel.NodeFull)
			return true
		}
		return false
	})
	id2, wid2 := e.newTask(taskSpec{owner: buyerA, text: "q", session: &s1})
	began := time.Now()
	res = e.dispatch(context.Background(), id2, 1, wid2, false)
	if p := projection(t, e, id2); res.Outcome != lifecycle.DispatchTerminal || p.Metadata[store.MetaFailure] != "tunnel_lost" {
		t.Fatalf("%+v %+v %v", res, p.Status, p.Metadata)
	}
	if took := time.Since(began); took < 2*time.Second {
		t.Errorf("failed after %v, before the grace", took)
	}
}

// A follow-up at input-required is dispatched on the same attempt, through the same session, with the
// upstream taskId and contextId substituted (spec §7). A follow-up refused full by the node is sent again
// with backoff (the node never forwarded it), and reaches the service once. When the sticky session is
// gone, the follow-up fails the task tunnel_lost without opening a stream (§7 row 2).
func TestTunnelFollowUp(t *testing.T) {
	s1 := newSession()
	agent := askingAgent()
	e, ss, nodes := tunnelEnv(t, envOptions{}, false, agent, s1)
	n := nodes[s1]
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "book a room", session: &s1})
	if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchInterrupted {
		t.Fatalf("turn 1: %+v", res)
	}
	a := e.attempt(id, 1)
	var sent []a2a.Message
	n.setAnswer(func(method string, _ json.RawMessage, k int, w http.ResponseWriter) bool {
		if method == "SendMessage" && k == 2 {
			nodeAnswer(w, http.StatusServiceUnavailable, tunnel.NodeFull) // the follow-up's first send
			return true
		}
		return false
	})
	recording := n.svc
	n.setService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var rpc struct {
			Method string                 `json:"method"`
			Params a2a.SendMessageRequest `json:"params"`
		}
		if json.Unmarshal(body, &rpc) == nil && rpc.Method == "SendMessage" {
			n.mu.Lock()
			sent = append(sent, *rpc.Params.Message)
			n.mu.Unlock()
		}
		recording.ServeHTTP(w, r)
	}))
	wid2 := e.followUp(id, buyerA, "the blue one")
	res := e.dispatch(context.Background(), id, 1, wid2, false)
	p := projection(t, e, id)
	if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateCompleted || p.Status.Message.Parts[0].Text() != "done: the blue one" {
		t.Fatalf("turn 2: %+v %+v", res, p.Status)
	}
	n.mu.Lock()
	got := append([]a2a.Message(nil), sent...)
	n.mu.Unlock()
	if len(got) != 1 || got[0].TaskID != a2a.TaskID(a.UpstreamTaskID) || got[0].ContextID != a.UpstreamContextID {
		t.Fatalf("the follow-up reached the service %d times: %+v", len(got), got)
	}

	// Another task waits at input-required; its session goes away.
	n.setAnswer(nil)
	id2, w1 := e.newTask(taskSpec{owner: buyerA, text: "book a car", session: &s1})
	if res := e.dispatch(context.Background(), id2, 1, w1, false); res.Outcome != lifecycle.DispatchInterrupted {
		t.Fatalf("turn 1 of the second task: %+v", res)
	}
	ss.drop(s1)
	opens := n.opens.Load()
	w2 := e.followUp(id2, buyerA, "the red one")
	res = e.dispatch(context.Background(), id2, 1, w2, false)
	p = projection(t, e, id2)
	if res.Outcome != lifecycle.DispatchTerminal || res.Reason != "tunnel_lost" || p.Metadata[store.MetaFailure] != "tunnel_lost" {
		t.Fatalf("a follow-up on a gone session: %+v %+v", res, p.Status)
	}
	if n.opens.Load() != opens {
		t.Fatalf("a follow-up on a gone session opened %d streams", n.opens.Load()-opens)
	}
	if a := e.attempt(id2, 1); a.Outcome != store.OutcomeTunnelLost || a.EndedAt == nil {
		t.Errorf("attempt %+v", a)
	}
}

// forward_cancel of a tunnel attempt sends CancelTask inside the tunnel (spec §8), its preamble naming the
// task; a canceled answer is canceled and the running dispatch logs it. When this replica no longer holds
// the session, nothing is forwarded: the answer is tunnel_lost and no stream is opened (§8's row
// "exists, session gone").
func TestTunnelForwardCancel(t *testing.T) {
	g := &gatedStreamer{release: make(chan struct{}, 16), chunks: 3}
	s1 := newSession()
	e, ss, nodes := tunnelEnv(t, envOptions{}, true, g, s1)
	n := nodes[s1]
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q", session: &s1})
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
	if n.calls("CancelTask") != 1 {
		t.Errorf("%d CancelTask exchanges", n.calls("CancelTask"))
	}
	row := e.row(id)
	tasks := n.seenTasks()
	if last := tasks[len(tasks)-1]; last != (upstream.ExchangeTask{TaskID: string(id), ContextID: row.ContextID}) {
		t.Errorf("forward_cancel's preamble task %+v", last)
	}

	// The session gone: tunnel_lost, nothing opened.
	id2, _ := e.newTask(taskSpec{owner: buyerA, text: "q", session: &s1})
	e.firstID(id2, 1, "up-x", "c")
	ss.drop(s1)
	opens := n.opens.Load()
	res, err = e.disp.ForwardCancel(context.Background(), lifecycle.AttemptRef{TaskID: string(id2), N: 1})
	if err != nil || res.Outcome != lifecycle.CancelTunnelLost || n.opens.Load() != opens {
		t.Fatalf("forward_cancel on a gone session: %+v %v (%d opens)", res, err, n.opens.Load()-opens)
	}
	if row := e.row(id2); row.State != store.StateSubmitted {
		t.Errorf("forward_cancel changed the task: %s", row.State)
	}
}

// Upstream context ids and referenceTaskIds are reused only on the same tunnel session (spec §4, D53; the M2
// plan audit's gap: node 1's upstream contextId must never reach node 2): a second task of the gate context
// on the first task's session carries the recorded upstream context and its mapped reference; one placed on
// another session carries neither; and that session's own context is then reused on it.
func TestUpstreamContextsStayOnTheirSession(t *testing.T) {
	echo := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
		}
	})
	s1, s2 := newSession(), newSession()
	e, _, nodes := tunnelEnv(t, envOptions{}, false, echo, s1, s2)
	sentTo := map[uuid.UUID]*[]a2a.Message{}
	for _, s := range []uuid.UUID{s1, s2} {
		n := nodes[s]
		var msgs []a2a.Message
		sentTo[s] = &msgs
		inner := n.svc
		n.setService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			var rpc struct {
				Method string                 `json:"method"`
				Params a2a.SendMessageRequest `json:"params"`
			}
			if json.Unmarshal(body, &rpc) == nil && rpc.Method == "SendMessage" {
				n.mu.Lock()
				msgs = append(msgs, *rpc.Params.Message)
				n.mu.Unlock()
			}
			inner.ServeHTTP(w, r)
		}))
	}
	sent := func(s uuid.UUID) []a2a.Message {
		nodes[s].mu.Lock()
		defer nodes[s].mu.Unlock()
		return append([]a2a.Message(nil), (*sentTo[s])...)
	}
	run := func(spec taskSpec) a2a.TaskID {
		t.Helper()
		id, wid := e.newTask(spec)
		if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchTerminal {
			t.Fatalf("dispatch %+v", res)
		}
		return id
	}
	t1 := run(taskSpec{owner: buyerA, contextID: "conv", text: "one", session: &s1})
	up1 := e.attempt(t1, 1)
	t2 := run(taskSpec{owner: buyerA, contextID: "conv", text: "two", refs: []a2a.TaskID{t1}, session: &s1})
	if got := sent(s1); len(got) != 2 || got[0].ContextID != "" || got[1].ContextID != up1.UpstreamContextID ||
		fmt.Sprint(got[1].ReferenceTasks) != fmt.Sprint([]a2a.TaskID{a2a.TaskID(up1.UpstreamTaskID)}) {
		t.Fatalf("on the same session: %+v", got)
	}
	t3 := run(taskSpec{owner: buyerA, contextID: "conv", text: "three", refs: []a2a.TaskID{t1, t2}, session: &s2})
	got := sent(s2)
	if len(got) != 1 || got[0].ContextID != "" || len(got[0].ReferenceTasks) != 0 {
		t.Fatalf("session 2 got session 1's ids: %+v", got)
	}
	up3 := e.attempt(t3, 1)
	run(taskSpec{owner: buyerA, contextID: "conv", text: "four", refs: []a2a.TaskID{t3, t1}, session: &s2})
	if got := sent(s2); len(got) != 2 || got[1].ContextID != up3.UpstreamContextID ||
		fmt.Sprint(got[1].ReferenceTasks) != fmt.Sprint([]a2a.TaskID{a2a.TaskID(up3.UpstreamTaskID)}) {
		t.Fatalf("session 2's own context: %+v", got)
	}
	// A direct attempt in the same context finds no tunnel attempt's ids either.
	seller, b, info := newAgent(t, echo, agentConfig{})
	e.backends.use(nil)
	e.backends.set(b, info, nil)
	td, wd := e.newTask(taskSpec{owner: buyerA, contextID: "conv", text: "five", refs: []a2a.TaskID{t1, t3}})
	e.dispatch(context.Background(), td, 1, wd, false)
	if got := seller.sentMessages(t); len(got) != 1 || got[0].ContextID != "" || len(got[0].ReferenceTasks) != 0 {
		t.Fatalf("a direct attempt got a tunnel session's ids: %+v", got)
	}
}
