package e2e

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/test/e2e/slowagent"
)

// askOn sends 'ask' tasks (blocking SendMessage, alternating replicas) until one waits at input-required on
// session, and returns it with its attempt.
func askOn(t *testing.T, ctx context.Context, h *Harness, session, text string) (*a2a.Task, Attempt) {
	t.Helper()
	for i := 0; i < 16; i++ {
		res, err := h.BuyerA().Client(t, h.Gates[i%2], "").SendMessage(ctx, newMessage(slowagent.AskPrefix+" "+text))
		if err != nil {
			t.Fatal(err)
		}
		task, ok := res.(*a2a.Task)
		if !ok || task.Status.State != a2a.TaskStateInputRequired {
			t.Fatalf("an ask task: %#v, want input-required", res)
		}
		as, err := h.Attempts(ctx, task.ID)
		if err != nil || len(as) != 1 {
			t.Fatalf("the ask task's attempts %+v %v", as, err)
		}
		if as[0].TunnelSession == session {
			return task, as[0]
		}
	}
	t.Fatalf("16 ask tasks and none was placed on session %s", session)
	return nil, Attempt{}
}

// followUp answers an input-required task of service svc ("" is slow) with a new message on g (blocking
// SendMessage).
func followUp(t *testing.T, ctx context.Context, h *Harness, g *Gate, svc string, task *a2a.Task, text string) (*a2a.Message, *a2a.Task) {
	t.Helper()
	req := newMessage(text)
	req.Message.TaskID, req.Message.ContextID = task.ID, task.ContextID
	res, err := h.BuyerA().Client(t, g, svc).SendMessage(ctx, req)
	if err != nil {
		t.Fatalf("the follow-up of %s on %s: %v", task.ID, g.Name, err)
	}
	got, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("the follow-up of %s answered %#v, want the task", task.ID, res)
	}
	return req.Message, got
}

// requestWith returns the agent's request carrying the message messageID, nil when none did.
func requestWith(a *slowagent.Agent, messageID string) *slowagent.Request {
	for _, r := range a.Requests() {
		if r.MessageID == messageID {
			return &r
		}
	}
	return nil
}

// TestInputRequiredSticky is input_required_sticky through the tunnel (spec/relay.md §7, §16; PRD §15):
// two nodes host `slow`, N1 (seller S1) on replica A and N2 (seller S2) on replica B. They are two `kontra
// connect` processes sharing one HOME, each with its own --key and its own service token (M2 WP-06: one
// credentials file, a token per key). An 'ask' task waits at input-required on each node; each reply,
// sent through the other replica, reaches the seller holding the task, with that seller's own upstream
// task and context ids, never the other seller; it runs on the same attempt, dispatched on the session's
// replica (attempts.replica_id), and both tasks complete. A third task waits on N1; N1 is SIGKILLed; the
// reply fails the task tunnel_lost on its one attempt, and nothing reaches S2. Both nodes then restart with
// their original command lines and reconnect, their used tokens not sent again.
func TestInputRequiredSticky(t *testing.T) {
	h := start(t, Options{
		Upstream: slowagent.Config{Chunks: 3, Interval: 100 * time.Millisecond, Streaming: true},
		Tunnel:   map[string]TunnelService{"slow": {}},
	})
	ctx := ctxFor(t, 6*time.Minute)
	s1 := h.Upstream
	s2, err := h.Agent(slowagent.Config{Chunks: 3, Interval: 100 * time.Millisecond, Streaming: true})
	if err != nil {
		t.Fatal(err)
	}
	n1, sess1 := startNode(t, ctx, h, "n1", map[string]string{"slow": s1.URL()})
	h.Front.Route(h.Gates[1])
	n2, sess2 := startNode(t, ctx, h, "n2", map[string]string{"slow": s2.URL()}, func(o *NodeOptions) {
		o.Home, o.Key = n1.Home, filepath.Join(n1.Home, nodekey.DirName, "n2.key")
	})
	if n1.KeyID == n2.KeyID {
		t.Fatalf("both nodes hold key %s", n1.KeyID)
	}
	if r1, r2 := sessionReplica(ctx, h, sess1), sessionReplica(ctx, h, sess2); r1 != h.Gates[0].ReplicaID || r2 != h.Gates[1].ReplicaID {
		t.Fatalf("the sessions are on %s and %s, want A and B", r1, r2)
	}
	if err := h.WaitCard(ctx, "slow", time.Minute); err != nil {
		t.Fatal(err)
	}

	type held struct {
		task    *a2a.Task
		att     Attempt
		replica *Gate
		mine    *slowagent.Agent
		other   *slowagent.Agent
	}
	t1, a1 := askOn(t, ctx, h, sess1, "sticky on N1")
	t2, a2 := askOn(t, ctx, h, sess2, "sticky on N2")
	for _, x := range []held{
		{t1, a1, h.Gates[0], s1, s2},
		{t2, a2, h.Gates[1], s2, s1},
	} {
		if x.att.ReplicaID != x.replica.ReplicaID || x.att.UpstreamTaskID == "" {
			t.Fatalf("task %s's attempt %+v: want its first dispatch on %s with an upstream task", x.task.ID, x.att, x.replica.Name)
		}
		// The reply goes through the replica that does not hold the session.
		msg, done := followUp(t, ctx, h, h.Other(x.replica), "", x.task, "the answer to "+string(x.task.ID))
		if done.Status.State != a2a.TaskStateCompleted {
			t.Fatalf("task %s after its reply: %s, want completed", x.task.ID, done.Status.State)
		}
		got := requestWith(x.mine, msg.ID)
		if got == nil || got.TaskID != x.att.UpstreamTaskID || got.ContextID != x.att.UpstreamContextID {
			t.Fatalf("the reply to %s at its seller: %+v, want upstream task %s context %s", x.task.ID, got, x.att.UpstreamTaskID, x.att.UpstreamContextID)
		}
		if stray := requestWith(x.other, msg.ID); stray != nil {
			t.Fatalf("the reply to %s reached the other seller: %+v", x.task.ID, stray)
		}
		as, err := h.Attempts(ctx, x.task.ID)
		if err != nil || len(as) != 1 || as[0].N != 1 || as[0].TunnelSession != x.att.TunnelSession ||
			as[0].ReplicaID != x.replica.ReplicaID || as[0].Outcome != "completed" {
			t.Fatalf("task %s's attempts after the reply %+v %v: want attempt 1 on %s, dispatched on %s, completed",
				x.task.ID, as, err, x.att.TunnelSession, x.replica.Name)
		}
		turns := 0
		for _, tn := range x.mine.Turns() {
			if string(tn.TaskID) == x.att.UpstreamTaskID && tn.FollowUp {
				turns++
			}
		}
		if turns != 1 {
			t.Fatalf("seller turns answering %s: %d, want 1 follow-up turn on the upstream task", x.task.ID, turns)
		}
	}
	checkHistory(t, h, t1.ID, "e2e_input_required_sticky", tunnelForbidden(h)...)

	// A third task waits on N1; N1 dies; the reply fails the task tunnel_lost and reaches no other node.
	t3, a3 := askOn(t, ctx, h, sess1, "sticky, session gone")
	if err := n1.Kill(); err != nil {
		t.Fatal(err)
	}
	waitRowGone(t, ctx, h, sess1, 5*time.Second, "N1 was SIGKILLed")
	before := len(s2.Requests())
	msg, lost := followUp(t, ctx, h, h.Gates[1], "", t3, "too late")
	if lost.Status.State != a2a.TaskStateFailed || lost.Metadata["kontra.failure"] != "tunnel_lost" {
		t.Fatalf("the reply with N1 gone: %s %v, want failed: tunnel_lost", lost.Status.State, lost.Metadata)
	}
	if after := s2.Requests(); len(after) != before || requestWith(s2, msg.ID) != nil {
		t.Fatalf("S2 received %d requests after N1 died, the reply among them: %v", len(after)-before, requestWith(s2, msg.ID) != nil)
	}
	as, err := h.Attempts(ctx, t3.ID)
	if err != nil || len(as) != 1 || as[0].Outcome != "tunnel_lost" || as[0].TunnelSession != a3.TunnelSession {
		t.Fatalf("task 3's attempts %+v %v: want its one attempt ended tunnel_lost, and no new attempt", as, err)
	}
	checkHistory(t, h, t3.ID, "e2e_input_required_sticky_session_gone", tunnelForbidden(h)...)

	// Both nodes restart with their original --enroll and reconnect; neither resends its used token.
	h.Front.Route(h.Gates[0])
	if err := n1.Restart(ctx, h); err != nil {
		t.Fatal(err)
	}
	if code, err := n2.Term(); err != nil || code != 0 {
		t.Fatalf("N2 on SIGTERM: exit %d %v, want 0", code, err)
	}
	if err := n2.Restart(ctx, h); err != nil {
		t.Fatal(err)
	}
	for _, nd := range []*Node{n1, n2} {
		if !skippedUsedToken(nd) || len(nd.Log(`"code":"enroll_invalid"`)) > 0 {
			t.Fatalf("node %s restarted without skipping its used token (or was refused enroll_invalid):\n%s", nd.Name, tail(nd.LogPath, 20))
		}
	}
	if s, err := h.Sessions(ctx, n1.KeyID); err != nil || len(s) != 1 {
		t.Fatalf("N1's key after the restart: sessions %v %v", s, err)
	}
	if s, err := h.Sessions(ctx, n2.KeyID); err != nil || len(s) != 1 {
		t.Fatalf("N2's key after the restart: sessions %v %v", s, err)
	}
}
