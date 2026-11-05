package lifecycle

import (
	"context"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/store"
)

// The reconnect wait (relay.md §6.4, D340..D349) on the test environment: a tunnel attempt whose session is lost after
// its first upstream task id waits for a session of its node, resumes on it, or gives up.

// lostThenDone answers the first dispatch with the session's loss, then completes on the next.
func lostThenDone(w *world, lost int) func(context.Context, int, DispatchInput) (DispatchResult, error) {
	return func(_ context.Context, k int, _ DispatchInput) (DispatchResult, error) {
		if k < lost {
			return DispatchResult{Outcome: DispatchTunnelLost, Reason: string(store.ReasonTunnelLost)}, nil
		}
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
}

func tunnelWorld(t *testing.T) *world {
	w := newWorld(t)
	w.backend = string(store.BackendTunnel)
	w.state = string(store.StateWorking)
	w.firstID[1] = true
	w.prepareRes = func(PrepareInput) PrepareResult { return placed(sessionA, "r1") }
	return w
}

// back answers reattach_session with session B of replica r2 once the clock passes at.
func back(w *world, at time.Time) func(ReattachInput) ReattachResult {
	return func(ReattachInput) ReattachResult {
		if w.now().Before(at) {
			return ReattachResult{}
		}
		return ReattachResult{Queue: QueueTunnel("r2"), SessionID: sessionB, ReplicaID: "r2"}
	}
}

// The node comes back within the grace: the wait logs its status once (since, until = since + 15 minutes), a
// kontra.reconnected signal makes it look at once (not at its next 10 s re-check), and the attempt is resumed on the
// new session's kontra-tunnel-<replica>, the same attempt and work item, as a resume; the task completes and nothing
// terminal is written by the workflow.
func TestReconnectWaitThenResume(t *testing.T) {
	w := tunnelWorld(t)
	w.dispatchFn = lostThenDone(w, 1)
	env := w.env()
	signalAt := t0.Add(2*time.Minute + 3*time.Second)
	w.reattachFn = back(w, signalAt)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalReconnected, ReconnectedSignal{}) }, signalAt.Sub(t0))
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.dispatches) != 2 || len(s.prepares) != 1 {
		t.Fatalf("dispatches %d prepares %d", len(s.dispatches), len(s.prepares))
	}
	if d := s.dispatches[1]; d.in.N != 1 || !d.in.Resume || d.in.WorkItemID != "wi-1" || d.queue != QueueTunnel("r2") {
		t.Fatalf("the resume %+v on %s", d.in, d.queue)
	}
	if d := s.dispatches[1]; d.at.Sub(signalAt) > time.Second || d.at.Before(signalAt) {
		t.Fatalf("resumed at %v, the signal came at %v: the wait did not look at once", d.at, signalAt)
	}
	if len(s.waits) != 1 || s.waits[0].Until.Sub(s.waits[0].Since) != DefaultReattachGrace || s.waits[0].N != 1 {
		t.Fatalf("reconnect_wait %+v, want one, until = since + 15 minutes", s.waits)
	}
	for _, r := range s.reattaches {
		if !r.Bind || r.N != 1 {
			t.Fatalf("reattach_session %+v", r)
		}
	}
	// The wait looked at once, then about every 10 s until the signal.
	if n := len(s.reattaches); n < 10 || n > 16 {
		t.Fatalf("reattach_session %d times over 2 minutes, want one at once and one about every 10 s", n)
	}
	if len(s.terminals) != 0 {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
}

// A node that redialed before the loss reached the workflow: the first look finds it, nothing is announced, and the
// attempt resumes at once.
func TestReconnectAlreadyBack(t *testing.T) {
	w := tunnelWorld(t)
	w.dispatchFn = lostThenDone(w, 1)
	w.reattachFn = back(w, t0)
	env := w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.waits) != 0 || len(s.reattaches) != 1 || len(s.dispatches) != 2 || s.dispatches[1].queue != QueueTunnel("r2") {
		t.Fatalf("waits %+v reattaches %+v dispatches %d", s.waits, s.reattaches, len(s.dispatches))
	}
}

// The node does not come back: after task.reattach_grace (here 2 minutes) the task fails tunnel_lost saying the node
// did not reconnect within the grace, the attempt tunnel_lost; nothing is dispatched again.
func TestReconnectWaitGivesUp(t *testing.T) {
	w := tunnelWorld(t)
	w.dispatchFn = lostThenDone(w, 1)
	env := w.env()
	in := input(2)
	g := int64(120)
	in.ReattachGraceSeconds = &g
	start(env, newUpdates(), in)
	done(t, env)
	s := w.snapshot()
	if len(s.dispatches) != 1 {
		t.Fatalf("dispatches %d", len(s.dispatches))
	}
	if len(s.terminals) != 1 || s.terminals[0].State != "failed" || s.terminals[0].Reason != string(store.ReasonTunnelLost) ||
		s.terminals[0].EndAttempt != string(store.OutcomeTunnelLost) || !strings.Contains(s.terminals[0].Text, "did not reconnect within 2 minutes") {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
	last := s.reattachAt[len(s.reattachAt)-1]
	if d := last.Sub(s.waits[0].Since); d < 2*time.Minute || d > 2*time.Minute+time.Second {
		t.Fatalf("gave up %v after the wait began, want 2 minutes", d)
	}
}

// A cancel during the wait: canceled at once (nothing to forward), the cancel answers canceled, the task is not
// resumed when its node comes back, and the node is sent forward_cancel then, on the new session's replica's control
// queue, naming that session.
func TestReconnectCancelDuringTheWait(t *testing.T) {
	w := tunnelWorld(t)
	w.dispatchFn = lostThenDone(w, 1)
	w.forwardFn = func(context.Context, int, string) (ForwardCancelResult, error) {
		return ForwardCancelResult{Outcome: CancelCanceled}, nil
	}
	env := w.env()
	w.reattachFn = back(w, t0.Add(5*time.Minute))
	u := newUpdates()
	env.RegisterDelayedCallback(func() { env.UpdateWorkflow(UpdateCancel, "c1", u.cb("c1"), CancelUpdate{}) }, time.Minute)
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	refs := s.forwardRefs
	if r, err := u.result("c1"); err != nil || r.(CancelResult).Outcome != CancelCanceled {
		t.Fatalf("cancel %+v %v", r, err)
	}
	if len(s.terminals) != 1 || s.terminals[0].State != "canceled" || s.terminals[0].EndAttempt != string(store.OutcomeCanceled) {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
	if len(s.dispatches) != 1 {
		t.Fatalf("dispatches %d: a canceled task is never resumed", len(s.dispatches))
	}
	if len(s.forwards) != 1 || s.forwards[0] != QueueCtl("r2") || len(refs) != 1 || refs[0].Session != sessionB || refs[0].N != 1 {
		t.Fatalf("forward_cancel %v %+v, want once on kontra-ctl-r2 naming session B", s.forwards, refs)
	}
	if i, j := eventIndex(t, s.events, "terminal:canceled"), eventIndex(t, s.events, "forward:"); j < i {
		t.Fatalf("forward_cancel before canceled: %v", s.events)
	}
}

// The task's deadline during the wait: failed: timeout, at the deadline, never tunnel_lost (the wait never runs past
// the deadline).
func TestReconnectTimeoutDuringTheWait(t *testing.T) {
	w := tunnelWorld(t)
	w.deadline = t0.Add(5 * time.Minute)
	w.dispatchFn = lostThenDone(w, 1)
	env := w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.terminals) != 1 || s.terminals[0].Reason != string(store.ReasonTimeout) {
		t.Fatalf("terminal writes %+v, want failed: timeout", s.terminals)
	}
	if len(s.waits) != 1 || !s.waits[0].Until.Equal(w.deadline) {
		t.Fatalf("reconnect_wait %+v, want until = the deadline", s.waits)
	}
}

// A follow-up to a task at input-required whose node is gone: the follow-up's dispatch finds the session lost, the
// task waits (reconnect_wait logs nothing: its question stays the status), and once the node is back the turn is
// resumed on the new session with the follow-up's work item, which the resume sends again when its delivery is not
// recorded (D81).
func TestReconnectFollowUpDuringTheWait(t *testing.T) {
	w := tunnelWorld(t)
	w.dispatchFn = func(_ context.Context, k int, in DispatchInput) (DispatchResult, error) {
		switch k {
		case 0:
			w.setState(string(store.StateInputRequired))
			return DispatchResult{Outcome: DispatchInterrupted}, nil
		case 1:
			return DispatchResult{Outcome: DispatchTunnelLost, Reason: string(store.ReasonTunnelLost)}, nil
		}
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	w.reattachFn = back(w, t0.Add(4*time.Minute))
	u := newUpdates()
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateMessage, "wi-2", u.cb("wi-2"), MessageUpdate{WorkItemID: "wi-2", After: 3})
	}, time.Minute)
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	if !u.has("wi-2:completed") {
		t.Fatal("the follow-up was not accepted")
	}
	if len(s.dispatches) != 3 {
		t.Fatalf("dispatches %d", len(s.dispatches))
	}
	if d := s.dispatches[2]; d.in.WorkItemID != "wi-2" || !d.in.Resume || d.in.N != 1 || d.queue != QueueTunnel("r2") {
		t.Fatalf("the follow-up's resume %+v on %s", d.in, d.queue)
	}
	if len(s.waits) != 1 || len(s.terminals) != 0 {
		t.Fatalf("waits %+v terminals %+v", s.waits, s.terminals)
	}
}

// Two disconnects: each is a wait of its own, and the task completes on the third dispatch.
func TestReconnectTwice(t *testing.T) {
	w := tunnelWorld(t)
	w.dispatchFn = lostThenDone(w, 2)
	env := w.env()
	w.reattachFn = func(ReattachInput) ReattachResult {
		w.mu.Lock()
		n := len(w.reattaches)
		w.mu.Unlock()
		if n%3 != 0 { // the third look of each wait finds it
			return ReattachResult{}
		}
		return ReattachResult{Queue: QueueTunnel("r2"), SessionID: sessionB, ReplicaID: "r2"}
	}
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.dispatches) != 3 || !s.dispatches[1].in.Resume || !s.dispatches[2].in.Resume {
		t.Fatalf("dispatches %+v", s.dispatches)
	}
	if len(s.waits) != 2 || len(s.terminals) != 0 {
		t.Fatalf("waits %+v terminals %+v", s.waits, s.terminals)
	}
}

// A node that keeps disconnecting: the 11th wait is not entered, and the task fails tunnel_lost saying so.
func TestReconnectLimit(t *testing.T) {
	w := tunnelWorld(t)
	w.dispatchFn = lostThenDone(w, 100)
	w.reattachFn = back(w, t0)
	env := w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.dispatches) != MaxReconnects+1 {
		t.Fatalf("dispatches %d, want the first and %d resumes", len(s.dispatches), MaxReconnects)
	}
	if len(s.terminals) != 1 || s.terminals[0].Reason != string(store.ReasonTunnelLost) || !strings.Contains(s.terminals[0].Text, "disconnected 10 times") {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
}

// The node's key revoked during the wait: no session can ever come, and the task fails tunnel_lost at once, saying
// so; a binding revoked alike.
func TestReconnectKeyRevokedDuringTheWait(t *testing.T) {
	for gone, text := range map[string]string{ReattachKeyRevoked: "key was revoked", ReattachBindingRevoked: "no longer enrolled"} {
		t.Run(gone, func(t *testing.T) {
			w := tunnelWorld(t)
			w.dispatchFn = lostThenDone(w, 1)
			env := w.env()
			revokedAt := t0.Add(3 * time.Minute)
			w.reattachFn = func(ReattachInput) ReattachResult {
				if w.now().Before(revokedAt) {
					return ReattachResult{}
				}
				return ReattachResult{Gone: gone}
			}
			start(env, newUpdates(), input(2))
			done(t, env)
			s := w.snapshot()
			if len(s.terminals) != 1 || s.terminals[0].Reason != string(store.ReasonTunnelLost) || !strings.Contains(s.terminals[0].Text, text) {
				t.Fatalf("terminal writes %+v", s.terminals)
			}
			if last := s.reattachAt[len(s.reattachAt)-1]; last.Sub(revokedAt) > reconnectRecheck {
				t.Fatalf("gave up at %v, the key was revoked at %v", last, revokedAt)
			}
		})
	}
}

// The replica holding the session died: the dispatch's heartbeat times out, and the task waits for its node (which
// redials another replica) as for any lost session. A dispatch never started on the dead replica's queue alike.
func TestReconnectAfterAReplicaDied(t *testing.T) {
	for name, err := range map[string]error{"heartbeat timeout": heartbeatTimeout(), "not started": scheduleToStartTimeout()} {
		t.Run(name, func(t *testing.T) {
			w := tunnelWorld(t)
			w.dispatchFn = func(_ context.Context, k int, _ DispatchInput) (DispatchResult, error) {
				if k == 0 {
					return DispatchResult{}, err
				}
				w.setState(string(store.StateCompleted))
				return DispatchResult{Outcome: DispatchTerminal}, nil
			}
			w.reattachFn = back(w, t0.Add(30*time.Second))
			env := w.env()
			start(env, newUpdates(), input(2))
			done(t, env)
			s := w.snapshot()
			if len(s.dispatches) != 2 || !s.dispatches[1].in.Resume || s.dispatches[1].queue != QueueTunnel("r2") || len(s.terminals) != 0 {
				t.Fatalf("dispatches %+v terminals %+v", s.dispatches, s.terminals)
			}
		})
	}
}

// An MCP task (protocol mcp, §11.5) keeps tunnel_lost: no wait (D347).
func TestReconnectNotForMCPTasks(t *testing.T) {
	w := tunnelWorld(t)
	w.dispatchFn = lostThenDone(w, 1)
	env := w.env()
	in := input(2)
	in.Protocol = ProtocolMCP
	start(env, newUpdates(), in)
	done(t, env)
	s := w.snapshot()
	if len(s.waits) != 0 || len(s.reattaches) != 0 || len(s.terminals) != 1 || s.terminals[0].Reason != string(store.ReasonTunnelLost) {
		t.Fatalf("waits %+v reattaches %+v terminals %+v", s.waits, s.reattaches, s.terminals)
	}
}
