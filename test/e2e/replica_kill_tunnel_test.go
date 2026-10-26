package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// replicaGone waits until the replica's gate_replicas row is reaped (spec/tunnel.md §11) and its session's
// tunnels row with it (the cascade), and returns how long after since that was.
func replicaGone(t *testing.T, ctx context.Context, h *Harness, g *Gate, session string, since time.Time) time.Duration {
	t.Helper()
	bound := ReapAfter + 10*time.Second // silent for 30 s, checked every 10 s
	eventually(t, time.Until(since.Add(bound)), "replica "+g.Name+"'s gate_replicas and tunnels rows reaped", func() bool {
		var n int
		err := h.Pool.QueryRow(ctx, `select (select count(*) from kontra.gate_replicas where replica_id = $1) +
			(select count(*) from kontra.tunnels where session_id = $2)`, g.ReplicaID, session).Scan(&n)
		return err == nil && n == 0
	})
	return time.Since(since)
}

// TestReplicaKillTunnel is replica_kill's tunnel variant (spec/relay.md §6.4, §14.2, §16; spec/tunnel.md
// §11; PRD §15: "kill the one holding the tunnel mid-task: dispatch activity times out within 60 s; task
// retried or failed per retry policy; tunnels row gone"). A node's session is on replica A, through the
// Front; the buyers call replica B.
//
// After the first upstream id: the Front is routed to B and A is SIGKILLed mid-stream. The dispatch's
// heartbeat times out within 60 s of the kill and the task fails tunnel_lost on its one attempt (a tunnel
// attempt is never re-attached: the upstream task lives behind a session that is gone); the seller saw its
// message once. A's gate_replicas row is reaped within 40 s, and its tunnels row with it. The node
// re-registers on B, and a second task completes there.
//
// Before the first upstream id (the seller holds its first event back 5 s): A is SIGKILLed while the
// seller holds the task. Attempt 1 ends with no upstream id, and the task completes on a later attempt on
// the node's session on B; the seller answered once per attempt that reached it.
func TestReplicaKillTunnel(t *testing.T) {
	t.Run("after the first upstream id", func(t *testing.T) {
		// 20 chunks 500 ms apart: still streaming when A is killed. A tunnel attempt is not re-attached, so
		// the stream need not outlast the heartbeat timeout as the direct variant's must.
		h := start(t, Options{
			Upstream: slowagent.Config{Chunks: 20, Interval: 500 * time.Millisecond, Streaming: true},
			Tunnel:   map[string]TunnelService{"slow": {}},
		})
		ctx := ctxFor(t, 6*time.Minute)
		a, b := h.Gates[0], h.Gates[1]
		n, sess := startNode(t, ctx, h, "n", map[string]string{"slow": h.Upstream.URL()})
		if err := h.WaitCard(ctx, "slow", time.Minute); err != nil {
			t.Fatal(err)
		}
		req := newMessage("replica_kill tunnel")
		s, err := h.BuyerA().SendStreaming(ctx, b, req, cookieCall)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		readUntil(t, ctx, s, func(ev Event) bool { return ev.Kind == "artifact" })
		task := s.TaskID()
		as := attemptsOf(t, ctx, h, task, 20*time.Second, "the first upstream id", func(as []Attempt) bool {
			return len(as) == 1 && as[0].FirstUpstreamIDAt != nil
		})
		if as[0].TunnelSession != sess || as[0].ReplicaID != a.ReplicaID {
			t.Fatalf("the attempt %+v: want it on the node's session on A", as[0])
		}

		h.Front.Route(b)
		if err := a.Kill(); err != nil {
			t.Fatal(err)
		}
		killed := time.Now()
		state, failure, as := endedAs(t, ctx, h, task, 2*HeartbeatTimeout)
		ended := time.Since(killed)
		if state != "failed" || failure != "tunnel_lost" {
			t.Fatalf("the task ended %s (%q) %.1f s after the kill, want failed: tunnel_lost", state, failure, ended.Seconds())
		}
		t.Logf("the task failed tunnel_lost %.1f s after A's SIGKILL", ended.Seconds())
		if len(as) != 1 || as[0].Outcome != "tunnel_lost" || as[0].ReplicaID != a.ReplicaID || as[0].TunnelSession != sess {
			t.Fatalf("attempts %+v: want only attempt 1, on A's session, ended tunnel_lost (no re-attach, no retry)", as)
		}
		if got := sendsOf(h.Upstream, req.Message.ID); got != 1 {
			t.Fatalf("the seller received the message %d times, want once", got)
		}
		reaped := replicaGone(t, ctx, h, a, sess, killed)
		t.Logf("A's gate_replicas and tunnels rows were reaped %.1f s after the kill", reaped.Seconds())

		// The node re-registers on B and the next task completes there.
		var onB string
		eventually(t, Reregister, "the node's session on B", func() bool {
			sessions, err := h.Sessions(ctx, n.KeyID)
			for id, r := range sessions {
				if r == b.ReplicaID {
					onB = id
				}
			}
			return err == nil && onB != ""
		})
		res, err := h.BuyerA().Client(t, b, "").SendMessage(ctx, newMessage("replica_kill tunnel, after"))
		if err != nil {
			t.Fatal(err)
		}
		t2, ok := res.(*a2a.Task)
		if !ok || t2.Status.State != a2a.TaskStateCompleted {
			t.Fatalf("the task after the kill: %#v, want completed", res)
		}
		if as, err := h.Attempts(ctx, t2.ID); err != nil || len(as) != 1 || as[0].TunnelSession != onB || as[0].ReplicaID != b.ReplicaID {
			t.Fatalf("its attempts %+v %v: want one, on the node's session %s on B", as, err, onB)
		}
		checkHistory(t, h, task, "e2e_replica_kill_tunnel", tunnelForbidden(h)...)
	})

	t.Run("before the first upstream id", func(t *testing.T) {
		h := start(t, Options{
			Upstream: slowagent.Config{Chunks: 4, Interval: 250 * time.Millisecond, Streaming: true, FirstEventDelay: 5 * time.Second},
			Tunnel:   map[string]TunnelService{"slow": {}},
		})
		ctx := ctxFor(t, 6*time.Minute)
		a, b := h.Gates[0], h.Gates[1]
		n, sess := startNode(t, ctx, h, "n", map[string]string{"slow": h.Upstream.URL()})
		if err := h.WaitCard(ctx, "slow", time.Minute); err != nil {
			t.Fatal(err)
		}
		req := newMessage("replica_kill tunnel, before the first id")
		s, err := h.BuyerA().SendStreaming(ctx, b, req, cookieCall)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		eventually(t, 30*time.Second, "the seller holds the task", func() bool { return sendsOf(h.Upstream, req.Message.ID) == 1 })
		ev, err := s.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		task := ev.Event.TaskInfo().TaskID
		if as, err := h.Attempts(ctx, task); err != nil || len(as) != 1 || as[0].FirstUpstreamIDAt != nil || as[0].ReplicaID != a.ReplicaID {
			t.Fatalf("before the kill: attempts %+v %v, want one on A with no upstream id", as, err)
		}
		h.Front.Route(b)
		if err := a.Kill(); err != nil {
			t.Fatal(err)
		}
		killed := time.Now()
		state, failure, as := endedAs(t, ctx, h, task, 2*HeartbeatTimeout+Reregister)
		if state != "completed" {
			t.Fatalf("the task ended %s (%q), want completed on a later attempt: %+v", state, failure, as)
		}
		t.Logf("the task completed %.1f s after A's SIGKILL; attempts %v", time.Since(killed).Seconds(), outcomes(as))
		if a1 := as[0]; a1.UpstreamTaskID != "" || a1.FirstUpstreamIDAt != nil || a1.EndedAt == nil || a1.Outcome == "completed" || a1.TunnelSession != sess {
			t.Fatalf("attempt 1 %+v: want it ended on A's session %s with no upstream id", a1, sess)
		}
		last := as[len(as)-1]
		if len(as) < 2 || last.Outcome != "completed" || last.ReplicaID != b.ReplicaID || last.TunnelSession == sess || sessionReplica(ctx, h, last.TunnelSession) != b.ReplicaID {
			t.Fatalf("attempts %+v: want a later attempt completed on the node's session on B", as)
		}
		if last.KeyID != n.KeyID {
			t.Fatalf("the last attempt ran on key %s, want the node's %s", last.KeyID, n.KeyID)
		}
		reached := 0
		for _, x := range as {
			if x.Outcome != "no_session" {
				reached++
			}
		}
		if got := sendsOf(h.Upstream, req.Message.ID); got != reached {
			t.Fatalf("the seller received the message %d times over attempts %v, want once per attempt that reached it (%d)", got, outcomes(as), reached)
		}
		replicaGone(t, ctx, h, a, sess, killed)
		checkHistory(t, h, task, "e2e_replica_kill_tunnel_before_first_id", tunnelForbidden(h)...)
	})
}
