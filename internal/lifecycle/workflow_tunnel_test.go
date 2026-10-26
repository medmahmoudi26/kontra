package lifecycle

import (
	"context"
	"fmt"
	"testing"
	"time"

	"kontra.sh/kontra/internal/store"
)

// The tunnel rows of the retry table (spec §6.4), the follow-ups of a sticky task (§7) and the cancel of a
// tunnel attempt (§8), on the test environment.

const (
	sessionA = "01920000-0000-7000-8000-00000000000a"
	sessionB = "01920000-0000-7000-8000-00000000000b"
)

// placed answers prepare with a tunnel attempt on session s of replica r.
func placed(s, r string) PrepareResult {
	return PrepareResult{Backend: string(store.BackendTunnel), Queue: QueueTunnel(r), SessionID: s}
}

func noSession() PrepareResult {
	return PrepareResult{Backend: string(store.BackendTunnel), NoSession: true}
}

// No candidate session (spec §6.4's no_session row): prepare writes the attempt no_session and nothing is
// dispatched; the workflow retries after 1 s and 2 s, and with task.retries 2 the third no_session attempt
// fails the task no_session (each no_session attempt counts against task.retries).
func TestNoSessionRetriesThenFails(t *testing.T) {
	w := newWorld(t)
	w.backend = string(store.BackendTunnel)
	w.prepareRes = func(PrepareInput) PrepareResult { return noSession() }
	w.dispatchFn = outcome(DispatchTerminal, "")
	env := w.env()
	u := newUpdates()
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.prepares) != 3 || len(s.dispatches) != 0 {
		t.Fatalf("prepares %d, dispatches %d: want 3 no_session attempts and no dispatch", len(s.prepares), len(s.dispatches))
	}
	for i, p := range s.prepares {
		if p.N != i+1 || p.PreviousOutcome != "" || p.AvoidSession != "" {
			t.Errorf("prepare %d: %+v", i, p)
		}
	}
	if gap := s.prepareAt[1].Sub(s.prepareAt[0]); gap < time.Second || gap >= 2*time.Second {
		t.Errorf("first backoff %v, want 1 s", gap)
	}
	if gap := s.prepareAt[2].Sub(s.prepareAt[1]); gap < 2*time.Second || gap >= 3*time.Second {
		t.Errorf("second backoff %v, want 2 s", gap)
	}
	if len(s.terminals) != 1 || s.terminals[0].State != "failed" || s.terminals[0].Reason != string(store.ReasonNoSession) {
		t.Fatalf("terminal writes %+v, want failed: no_session", s.terminals)
	}
	if !u.has("wi-1:completed") {
		t.Error("the first kontra.message did not complete")
	}
}

// A node that connects during the no_session backoff lets the next attempt be placed and dispatched on
// kontra-tunnel-<its replica>.
func TestNoSessionThenANodeConnects(t *testing.T) {
	w := newWorld(t)
	w.backend = string(store.BackendTunnel)
	w.prepareRes = func(in PrepareInput) PrepareResult {
		if in.N == 1 {
			return noSession()
		}
		return placed(sessionA, "r2")
	}
	w.dispatchFn = func(context.Context, int, DispatchInput) (DispatchResult, error) {
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.prepares) != 2 || len(s.dispatches) != 1 {
		t.Fatalf("prepares %d dispatches %d", len(s.prepares), len(s.dispatches))
	}
	if d := s.dispatches[0]; d.in.N != 2 || d.queue != QueueTunnel("r2") || d.in.Resume || d.in.WorkItemID != "wi-1" {
		t.Fatalf("dispatch %+v on %s", d.in, d.queue)
	}
	if len(s.terminals) != 0 {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
}

// Before the first upstream task id, a lost tunnel retries on another session (spec §6.4): the retry's
// prepare is told to avoid the lost session, whether the dispatch reported it mid-exchange (retryable,
// tunnel_lost: it ended its attempt itself) or found the session missing at once (tunnel_lost: the next
// prepare ends the attempt tunnel_lost). After the first id, a lost session fails the task tunnel_lost,
// with no retry.
func TestTunnelLostRetriesOnAnotherSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  DispatchResult
		prev string
	}{
		{"lost mid-exchange", DispatchResult{Outcome: DispatchRetryable, Reason: string(store.ReasonTunnelLost)}, ""},
		{"session missing", DispatchResult{Outcome: DispatchTunnelLost, Reason: string(store.ReasonTunnelLost)}, string(store.OutcomeTunnelLost)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.backend = string(store.BackendTunnel)
			w.prepareRes = func(in PrepareInput) PrepareResult {
				if in.AvoidSession == sessionA {
					return placed(sessionB, "r2")
				}
				return placed(sessionA, "r1")
			}
			w.dispatchFn = func(_ context.Context, k int, _ DispatchInput) (DispatchResult, error) {
				if k == 0 {
					return tc.res, nil
				}
				w.setState(string(store.StateCompleted))
				return DispatchResult{Outcome: DispatchTerminal}, nil
			}
			env := w.env()
			start(env, newUpdates(), input(2))
			done(t, env)
			s := w.snapshot()
			if len(s.prepares) != 2 || len(s.dispatches) != 2 {
				t.Fatalf("prepares %d dispatches %d", len(s.prepares), len(s.dispatches))
			}
			if p := s.prepares[1]; p.N != 2 || p.AvoidSession != sessionA || p.PreviousOutcome != tc.prev {
				t.Fatalf("the retry's prepare %+v", p)
			}
			if s.dispatches[0].queue != QueueTunnel("r1") || s.dispatches[1].queue != QueueTunnel("r2") || s.dispatches[1].in.N != 2 {
				t.Fatalf("dispatches on %s then %s (%+v)", s.dispatches[0].queue, s.dispatches[1].queue, s.dispatches[1].in)
			}
			if len(s.terminals) != 0 {
				t.Fatalf("terminal writes %+v", s.terminals)
			}
		})
	}

	// After the first upstream task id: failed: tunnel_lost, no retry.
	w := newWorld(t)
	w.backend = string(store.BackendTunnel)
	w.firstID[1] = true
	w.prepareRes = func(PrepareInput) PrepareResult { return placed(sessionA, "r1") }
	w.dispatchFn = outcome(DispatchTunnelLost, string(store.ReasonTunnelLost))
	env := w.env()
	start(env, newUpdates(), input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.prepares) != 1 || len(s.dispatches) != 1 {
		t.Fatalf("prepares %d dispatches %d: no retry after the first upstream id", len(s.prepares), len(s.dispatches))
	}
	if len(s.terminals) != 1 || s.terminals[0].Reason != string(store.ReasonTunnelLost) || s.terminals[0].EndAttempt != string(store.OutcomeTunnelLost) {
		t.Fatalf("terminal writes %+v", s.terminals)
	}
}

// A follow-up of a sticky task (spec §7) runs on the same attempt, on kontra-tunnel-<the session's
// replica>, which only that replica polls.
func TestTunnelFollowUpOnTheSessionsReplica(t *testing.T) {
	w := newWorld(t)
	w.backend = string(store.BackendTunnel)
	w.firstID[1] = true
	w.prepareRes = func(PrepareInput) PrepareResult { return placed(sessionA, "r7") }
	w.dispatchFn = func(_ context.Context, k int, _ DispatchInput) (DispatchResult, error) {
		if k == 0 {
			w.setState(string(store.StateInputRequired))
			return DispatchResult{Outcome: DispatchInterrupted}, nil
		}
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateMessage, "wi-2", u.cb("wi-2"), MessageUpdate{WorkItemID: "wi-2", After: 3})
	}, time.Minute)
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	if len(s.prepares) != 1 || len(s.dispatches) != 2 {
		t.Fatalf("prepares %d dispatches %d", len(s.prepares), len(s.dispatches))
	}
	if d := s.dispatches[1]; d.in.N != 1 || d.in.WorkItemID != "wi-2" || d.in.Resume || d.queue != QueueTunnel("r7") {
		t.Fatalf("the follow-up's dispatch %+v on %s", d.in, d.queue)
	}
}

// The cancel of a tunnel task whose sticky session is gone (spec §8's row "exists, session gone",
// lifecycle.CancelTunnelLost's SPEC-CONFLICT): forward_cancel, on kontra-ctl-<the session's replica>,
// answers tunnel_lost or is not started within 10 s; the task fails tunnel_lost (its attempt too, although
// it waited at input-required), and the caller gets that task (outcome tunnel_lost). A running dispatch is
// canceled (the workflow, which waits for its dispatch, completes before the dispatch would end).
func TestCancelWhenTheTunnelSessionIsGone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		forward func(context.Context, int, string) (ForwardCancelResult, error)
		running bool
	}{
		{"input-required, forward_cancel answers tunnel_lost", func(context.Context, int, string) (ForwardCancelResult, error) {
			return ForwardCancelResult{Outcome: CancelTunnelLost}, nil
		}, false},
		{"input-required, forward_cancel not started", func(context.Context, int, string) (ForwardCancelResult, error) {
			return ForwardCancelResult{}, scheduleToStartTimeout()
		}, false},
		{"a dispatch running, forward_cancel answers tunnel_lost", func(context.Context, int, string) (ForwardCancelResult, error) {
			return ForwardCancelResult{Outcome: CancelTunnelLost}, nil
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.backend = string(store.BackendTunnel)
			w.firstID[1] = true
			w.prepareRes = func(PrepareInput) PrepareResult { return placed(sessionA, "r1") }
			w.forwardFn = tc.forward
			w.dispatchFn = func(ctx context.Context, _ int, _ DispatchInput) (DispatchResult, error) {
				if !tc.running {
					w.setState(string(store.StateInputRequired))
					return DispatchResult{Outcome: DispatchInterrupted}, nil
				}
				// Runs until canceled (or the test's end): the workflow completes only once it canceled it.
				return DispatchResult{}, w.block(ctx)
			}
			env := w.env()
			u := newUpdates()
			// While a dispatch runs the environment's clock is the wall clock: cancel soon.
			at := time.Minute
			if tc.running {
				at = 200 * time.Millisecond
			}
			env.RegisterDelayedCallback(func() { env.UpdateWorkflow(UpdateCancel, "c1", u.cb("c1"), CancelUpdate{}) }, at)
			start(env, u, input(2))
			done(t, env)
			s := w.snapshot()
			if len(s.forwards) != 1 || s.forwards[0] != QueueCtl("r1") {
				t.Fatalf("forward_cancel %v, want once on kontra-ctl-r1", s.forwards)
			}
			if len(s.terminals) != 1 || s.terminals[0].State != "failed" || s.terminals[0].Reason != string(store.ReasonTunnelLost) ||
				s.terminals[0].EndAttempt != string(store.OutcomeTunnelLost) || !s.terminals[0].EndAttemptAsIs {
				t.Fatalf("terminal writes %+v, want failed: tunnel_lost", s.terminals)
			}
			r, err := u.result("c1")
			if err != nil || r.(CancelResult).Outcome != CancelTunnelLost {
				t.Fatalf("cancel %+v %v", r, err)
			}
		})
	}

	// A tunnel attempt whose session is up keeps §8's other rows: a refused cancel and the task continues.
	w := newWorld(t)
	w.backend = string(store.BackendTunnel)
	w.firstID[1] = true
	w.prepareRes = func(PrepareInput) PrepareResult { return placed(sessionA, "r1") }
	w.forwardFn = func(context.Context, int, string) (ForwardCancelResult, error) {
		return ForwardCancelResult{Outcome: CancelNotCancelable, UpstreamError: &UpstreamError{Code: -32002}}, nil
	}
	w.dispatchFn = func(_ context.Context, k int, _ DispatchInput) (DispatchResult, error) {
		if k == 0 {
			w.setState(string(store.StateInputRequired))
			return DispatchResult{Outcome: DispatchInterrupted}, nil
		}
		w.setState(string(store.StateCompleted))
		return DispatchResult{Outcome: DispatchTerminal}, nil
	}
	env := w.env()
	u := newUpdates()
	env.RegisterDelayedCallback(func() { env.UpdateWorkflow(UpdateCancel, "c1", u.cb("c1"), CancelUpdate{}) }, time.Minute)
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateMessage, "wi-2", u.cb("wi-2"), MessageUpdate{WorkItemID: "wi-2", After: 3})
	}, 2*time.Minute)
	start(env, u, input(2))
	done(t, env)
	s := w.snapshot()
	if r, err := u.result("c1"); err != nil || r.(CancelResult).Outcome != CancelNotCancelable {
		t.Fatalf("cancel %+v %v", r, err)
	}
	if len(s.terminals) != 0 || len(s.dispatches) != 2 {
		t.Fatalf("terminal writes %+v, dispatches %d: the task continues", s.terminals, len(s.dispatches))
	}
}

// Only a tunnel attempt's session can be gone (M2 WP-05 review): a forward_cancel that answers tunnel_lost for
// a direct attempt (its service became a tunnel service while it waited, on a replica that answers so) is a
// cancel not delivered. The caller is told the service could not be reached and the task continues, as
// for forward_cancel's error, whether it waits at input-required or a dispatch runs; nothing is written.
func TestCancelTunnelLostOfADirectAttempt(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprintf("running=%v", running), func(t *testing.T) {
			w := newWorld(t)
			w.firstID[1] = true
			w.forwardFn = func(context.Context, int, string) (ForwardCancelResult, error) {
				return ForwardCancelResult{Outcome: CancelTunnelLost}, nil
			}
			release := make(chan struct{})
			w.dispatchFn = func(ctx context.Context, k int, _ DispatchInput) (DispatchResult, error) {
				if running && k == 0 {
					select {
					case <-release:
					case <-ctx.Done():
						return DispatchResult{}, ctx.Err()
					}
					w.setState(string(store.StateCompleted))
					return DispatchResult{Outcome: DispatchTerminal}, nil
				}
				if k == 0 {
					w.setState(string(store.StateInputRequired))
					return DispatchResult{Outcome: DispatchInterrupted}, nil
				}
				w.setState(string(store.StateCompleted))
				return DispatchResult{Outcome: DispatchTerminal}, nil
			}
			env := w.env()
			u := newUpdates()
			if running {
				env.RegisterDelayedCallback(func() { env.UpdateWorkflow(UpdateCancel, "c1", u.cb("c1"), CancelUpdate{}) }, 200*time.Millisecond)
				env.RegisterDelayedCallback(func() { close(release) }, time.Second)
			} else {
				env.RegisterDelayedCallback(func() { env.UpdateWorkflow(UpdateCancel, "c1", u.cb("c1"), CancelUpdate{}) }, time.Minute)
				env.RegisterDelayedCallback(func() {
					env.UpdateWorkflow(UpdateMessage, "wi-2", u.cb("wi-2"), MessageUpdate{WorkItemID: "wi-2", After: 3})
				}, 2*time.Minute)
			}
			start(env, u, input(2))
			done(t, env)
			r, err := u.result("c1")
			if err != nil {
				t.Fatal(err)
			}
			if got := r.(CancelResult); got.Outcome != CancelNotCancelable || got.UpstreamError == nil || got.UpstreamError.Code != -32603 {
				t.Fatalf("cancel %+v, want the service could not be reached", got)
			}
			s := w.snapshot()
			if len(s.terminals) != 0 {
				t.Fatalf("terminal writes %+v: the task continues", s.terminals)
			}
		})
	}
}
