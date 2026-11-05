package lifecycle

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"

	"kontra.sh/kontra/internal/store"
)

// The reconnect wait (relay.md §6.4, D340..D349; changeReconnect). A tunnel attempt whose session is lost after its
// first upstream task id no longer fails tunnel_lost: the agent's task lives on the node, which redials on its own
// (tunnel.md §13), so the workflow waits durably for a session of the same node and re-attaches on it. The gate
// never dials a node: it waits for the node's own dial.

// reattachGrace is task.reattach_grace as the task started (Input.ReattachGraceSeconds; DefaultReattachGrace for an
// input recorded before it).
func (l *lc) reattachGrace() time.Duration {
	if l.in.ReattachGraceSeconds == nil {
		return DefaultReattachGrace
	}
	return max(time.Duration(*l.in.ReattachGraceSeconds)*time.Second, 0)
}

// canReconnect reports whether a tunnel attempt with a first upstream task id that lost its session waits for its
// node: an A2A task (an MCP task keeps tunnel_lost, D347), a run that took changeReconnect, and a grace that is not
// zero.
func (l *lc) canReconnect(ctx workflow.Context) bool {
	if l.in.Protocol == ProtocolMCP {
		return false
	}
	return workflow.GetVersion(ctx, changeReconnect, workflow.DefaultVersion, 1) >= 1 && l.reattachGrace() > 0
}

// recheckAfter is the wait before a reconnect wait's (or a late cancel's) pass-th look for its node's session, pass
// counting from 1: reconnectRecheck for the first minute, then doubling to reconnectRecheckMax; reconnectRecheckSlow
// once the run's history is past reconnectHistoryBudget events (spec §14.1, D344). The history length is the
// replayed one, so the choice is deterministic.
func recheckAfter(ctx workflow.Context, pass int) time.Duration {
	if workflow.GetInfo(ctx).GetCurrentHistoryLength() > reconnectHistoryBudget {
		return reconnectRecheckSlow
	}
	if pass <= reconnectRecheckFast {
		return reconnectRecheck
	}
	return min(reconnectRecheck<<min(pass-reconnectRecheckFast, 8), reconnectRecheckMax)
}

// reconnect is one reconnect wait of the current attempt (D341..D343). It looks for a session of the attempt's node
// at once: one found then (the node redialed before the dispatch's loss reached the workflow, a gate roll's drain, or
// a stream reset on a session still up) is a D42 resume, announced to nobody, backed off 1 s, 2 s, 4 s … and capped
// by MaxResumes, so a session that keeps failing its dispatches cannot spin. Else it logs the waiting status (the
// wait counts toward MaxReconnects) and looks again on each kontra.reconnected and at recheckAfter, until a session
// is found (stepResume on its kontra-tunnel-<replica>), none can ever come (the key or binding revoked: failed:
// tunnel_lost), the grace runs out (failed: tunnel_lost, saying so), the deadline passes (failed: timeout) or the
// task is canceled (canceled at once, D346). The grace never runs past the task's deadline (§6.5). When the task
// ends in the wait by its timeout or the grace, the node is sent forward_cancel if it comes back (lateCancel, D346).
func (l *lc) reconnect(ctx workflow.Context) step {
	done := step{kind: stepDone}
	grace := l.reattachGrace()
	since := workflow.Now(ctx)
	until := since.Add(grace)
	capped := false
	if !l.deadline.IsZero() && until.After(l.deadline) {
		until, capped = l.deadline, true
	}
	l.logger.Info("the attempt's tunnel session was lost: looking for the node's session", "attempt", l.n,
		"waits", l.reconnects, "until", until)
	announced := false
	for pass := 1; l.phase != phaseTerminal && ctx.Err() == nil; pass++ {
		switch {
		case l.timedOut:
			l.timeout(ctx)
			// The upstream task runs on: tell the node if it comes back within the grace it had.
			l.lateCancel(ctx, since.Add(grace))
			return done
		case l.cancelPending():
			l.cancelWaiting(ctx, until)
			return done
		}
		l.reconnectSig = false
		var r ReattachResult
		if err := l.call(ctx, ActReattachSession, &r, ReattachInput{TaskID: l.in.TaskID, N: l.n, Bind: true}); err != nil {
			l.logger.Warn("reattach_session", "error", err)
			return done
		}
		switch {
		case r.Gone == ReattachAttemptEnded:
			if _, ok := l.attemptState(ctx); ok {
				l.fail(ctx, store.ReasonUpstreamError, "the relay could not follow the task", "")
			}
			return done
		case r.Gone != "":
			l.failLost(ctx, goneText(r.Gone))
			return done
		case r.Queue != "":
			l.queue, l.session = r.Queue, r.SessionID
			if announced {
				l.logger.Info("the node reconnected: resuming the attempt", "attempt", l.n, "queue", r.Queue)
				return step{kind: stepResume}
			}
			// A session of the node is up already: a resume as D42's, backed off and capped.
			l.resumes++
			if l.resumes > MaxResumes {
				l.failLost(ctx, fmt.Sprintf("The connection to the agent's node was cut %d times while running the task; the gate stopped re-attaching.",
					MaxResumes))
				return done
			}
			wait := min(time.Second<<(l.resumes-1), maxResumeBackoff)
			l.logger.Info("a session of the node is up: resuming the attempt", "attempt", l.n, "queue", r.Queue, "after", wait)
			return step{kind: stepResume, wait: wait}
		}
		left := until.Sub(workflow.Now(ctx))
		if left <= 0 {
			if capped {
				// The task's deadline ends the wait: the timer's failed: timeout (§6.5), not tunnel_lost.
				_ = workflow.Await(ctx, func() bool { return l.timedOut || l.cancelPending() })
				continue
			}
			l.failLost(ctx, "The agent's node did not reconnect within "+HumanDuration(grace)+".")
			if l.phase == phaseTerminal {
				l.lateCancel(ctx, workflow.Now(ctx).Add(grace))
			}
			return done
		}
		if !announced {
			l.reconnects++
			if l.reconnects > MaxReconnects {
				l.failLost(ctx, fmt.Sprintf("The agent's node disconnected %d times while running the task; the gate stopped waiting for it.",
					MaxReconnects))
				return done
			}
			announced = true
			l.logger.Info("waiting for the node to reconnect", "attempt", l.n, "wait", l.reconnects, "until", until)
			var wr ReconnectWaitResult
			if err := l.call(ctx, ActReconnectWait, &wr, ReconnectWaitInput{TaskID: l.in.TaskID, N: l.n, Since: since, Until: until}); err != nil {
				l.logger.Warn("reconnect_wait", "error", err)
				if l.phase == phaseTerminal {
					return done
				}
			}
			if l.reconnectSig {
				continue // the node came back while the status was written
			}
			left = until.Sub(workflow.Now(ctx))
		}
		_, _ = workflow.AwaitWithTimeout(ctx, max(min(recheckAfter(ctx, pass), left), time.Millisecond), func() bool {
			return l.reconnectSig || l.timedOut || l.cancelPending()
		})
	}
	return done
}

// failLost writes failed: tunnel_lost with text and ends the attempt tunnel_lost, even when the turn was a follow-up to
// an input-required task (its session is gone, D149, §7 row 2).
func (l *lc) failLost(ctx workflow.Context, text string) {
	l.writeTerminal(ctx, WriteTerminalInput{State: string(store.StateFailed), Reason: string(store.ReasonTunnelLost), Text: text,
		EndAttempt: string(store.OutcomeTunnelLost), EndAttemptAsIs: true})
}

// goneText is the failed status's text when no session of the node can serve the task again.
func goneText(gone string) string {
	switch gone {
	case ReattachKeyRevoked:
		return "The agent's node disconnected and its key was revoked: it cannot reconnect to finish the task."
	case ReattachBindingRevoked:
		return "The agent's node disconnected and is no longer enrolled for this service: it cannot reconnect to finish the task."
	}
	return "The agent's node disconnected and cannot reconnect to finish the task."
}

// cancelWaiting is a cancel during a reconnect wait (D346): nothing can be forwarded, so the task is canceled at once
// (§8's row "exists, session gone"), whatever mode the cancel asked for; if the node comes back before until, it is
// sent the cancel (lateCancel), and the task is not resumed.
func (l *lc) cancelWaiting(ctx workflow.Context, until time.Time) {
	l.logger.Info("cancel during a reconnect wait: the task is canceled", "attempt", l.n)
	l.cancelGone(ctx, l.cancel, nil, until)
}

// cancelGone writes canceled for a tunnel attempt whose session is gone, completes the cancel, stops a dispatch still
// running, and starts lateCancel until the given time.
func (l *lc) cancelGone(ctx workflow.Context, op *cancelOp, c *call, until time.Time) {
	tr := l.writeTerminal(ctx, WriteTerminalInput{State: string(store.StateCanceled), Canceled: store.CanceledClient,
		EndAttempt: string(store.OutcomeCanceled), EndAttemptAsIs: true})
	if c != nil && !c.done {
		c.cancel()
	}
	l.finishEnded(op, tr.State)
	if tr.State == string(store.StateCanceled) && !l.gone {
		l.lateCancel(ctx, until)
	}
}

// lateCancel sends forward_cancel, best effort, to the node of a task that ended while its session was gone (canceled,
// timed out or given up in a reconnect wait), if the node comes back before until (D346): on the session reattach_session finds (without binding it), through that session's
// replica's control queue. The task stays as it ended whatever the node answers; the run ends once this is over.
func (l *lc) lateCancel(ctx workflow.Context, until time.Time) {
	l.lateCancels++
	n := l.n
	workflow.Go(ctx, func(gc workflow.Context) {
		defer func() { l.lateCancels-- }()
		for pass := 1; gc.Err() == nil; pass++ {
			l.reconnectSig = false
			var r ReattachResult
			if err := workflow.ExecuteActivity(lifecycleCtx(gc), ActReattachSession, ReattachInput{TaskID: l.in.TaskID, N: n}).Get(gc, &r); err != nil {
				l.logger.Info("reattach_session after a cancel", "error", err)
				return
			}
			if r.Gone != "" {
				return
			}
			if r.Queue != "" {
				var fr ForwardCancelResult
				err := workflow.ExecuteActivity(ctlCtx(gc, QueueCtl(r.ReplicaID), forwardCancelStartToClose), ActForwardCancel,
					AttemptRef{TaskID: l.in.TaskID, N: n, Session: r.SessionID}).Get(gc, &fr)
				l.logger.Info("the node of a canceled task came back: forward_cancel sent", "attempt", n, "outcome", fr.Outcome, "error", err)
				return
			}
			left := until.Sub(workflow.Now(gc))
			if left <= 0 {
				return
			}
			_, _ = workflow.AwaitWithTimeout(gc, min(recheckAfter(gc, pass), left), func() bool { return l.reconnectSig })
		}
	})
}
