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

// reconnect is one reconnect wait of the current attempt (D341..D343). It looks for a session of the attempt's node
// at once (a node that redialed before the dispatch's loss reached the workflow: nothing is announced), then logs the
// waiting status and looks again on each kontra.reconnected and every reconnectRecheck, until a session is found
// (stepResume on its kontra-tunnel-<replica>), none can ever come (the key or binding revoked: failed: tunnel_lost
// at once), the grace runs out (failed: tunnel_lost, saying so), the deadline passes (failed: timeout) or the
// task is canceled (canceled at once, D346). The grace never runs past the task's deadline (§6.5).
func (l *lc) reconnect(ctx workflow.Context) step {
	done := step{kind: stepDone}
	l.reconnects++
	if l.reconnects > MaxReconnects {
		l.failLost(ctx, fmt.Sprintf("The agent's node disconnected %d times while running the task; the gate stopped waiting for it.",
			MaxReconnects))
		return done
	}
	grace := l.reattachGrace()
	since := workflow.Now(ctx)
	until := since.Add(grace)
	capped := false
	if !l.deadline.IsZero() && until.After(l.deadline) {
		until, capped = l.deadline, true
	}
	l.logger.Info("the attempt's tunnel session was lost: waiting for the node to reconnect", "attempt", l.n,
		"wait", l.reconnects, "until", until)
	announced := false
	for l.phase != phaseTerminal && ctx.Err() == nil {
		switch {
		case l.timedOut:
			l.timeout(ctx)
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
			l.logger.Info("the node reconnected: resuming the attempt", "attempt", l.n, "queue", r.Queue)
			return step{kind: stepResume}
		}
		left := until.Sub(workflow.Now(ctx))
		if left <= 0 {
			if capped {
				// The task's deadline ends the wait: the timer's failed: timeout (§6.5), not tunnel_lost.
				_ = workflow.Await(ctx, func() bool { return l.timedOut || l.cancelPending() })
				continue
			}
			l.failLost(ctx, "The agent's node did not reconnect within "+HumanDuration(grace)+".")
			return done
		}
		if !announced {
			announced = true
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
		_, _ = workflow.AwaitWithTimeout(ctx, max(min(reconnectRecheck, left), time.Millisecond), func() bool {
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

// lateCancel sends forward_cancel, best effort, to the node of a task canceled while its session was gone, if the node
// comes back before until (D346): on the session reattach_session finds (without binding it), through that session's
// replica's control queue. The task stays canceled whatever the node answers; the run ends once this is over.
func (l *lc) lateCancel(ctx workflow.Context, until time.Time) {
	l.lateCancels++
	n := l.n
	workflow.Go(ctx, func(gc workflow.Context) {
		defer func() { l.lateCancels-- }()
		for gc.Err() == nil {
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
			_, _ = workflow.AwaitWithTimeout(gc, min(reconnectRecheck, left), func() bool { return l.reconnectSig })
		}
	})
}
