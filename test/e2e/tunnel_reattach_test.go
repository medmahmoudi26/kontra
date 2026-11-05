package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/relay"
	"kontra.sh/kontra/test/e2e/slowagent"
)

// TestTunnelReattach is tunnel_reattach (spec/relay.md §6.4's reconnect wait, §16; D340..D349): two replicas, a node
// enrolled for `slow`, a seller that streams an artifact chunk every second for a minute, the buyer following each task
// on a stream of the replica that does not hold the node's session.
//
//	(a) The node is SIGKILLed mid-task and started again within the grace: the buyer's stream carries the gate's
//	    waiting status (kontra.reconnect waiting, the gate's text), then the re-attach's resumed status, and the task
//	    completes with every chunk, on its one attempt, now on the node's new session; the seller got the message once.
//	(c) The replica holding the node's session is SIGKILLed: the node redials the other replica, the dispatch's
//	    heartbeat times out, and the task resumes there and completes with every chunk.
//	(d) The node is SIGKILLed and the seller restarted (it loses its tasks) before the node comes back: the re-attach
//	    gets TaskNotFound, and the task fails tunnel_lost saying the agent no longer has the task.
//	(b) With task.reattach_grace 10 s, the node is SIGKILLed and not started again: the task fails tunnel_lost saying
//	    the node did not reconnect within 10 seconds, about 10 s after the waiting status.
//
// The node prints that it lost the gate and when it is back (D349). Each task's history is checked (no chunk text, no
// credential) and replayed; KONTRA_RECORD_HISTORIES=1 records them as e2e_reconnect_*.
func TestTunnelReattach(t *testing.T) {
	t.Run("node restarted, replica killed, agent restarted", func(t *testing.T) {
		h := start(t, Options{
			Upstream:      slowagent.Config{Chunks: 60, Interval: time.Second, Streaming: true},
			Tunnel:        map[string]TunnelService{"slow": {}},
			ReattachGrace: 3 * time.Minute,
		})
		ctx := ctxFor(t, 12*time.Minute)
		a, b := h.Gates[0], h.Gates[1]
		n, sess := startNode(t, ctx, h, "n", map[string]string{"slow": h.Upstream.URL()})
		if err := h.WaitCard(ctx, "slow", time.Minute); err != nil {
			t.Fatal(err)
		}
		if r := sessionReplica(ctx, h, sess); r != a.ReplicaID {
			t.Fatalf("the node's session is on %q, want A", r)
		}

		// (a) The node killed and restarted within the grace.
		req := newMessage("tunnel_reattach a")
		s, err := h.BuyerA().SendStreaming(ctx, b, req, cookieCall)
		if err != nil {
			t.Fatal(err)
		}
		task := runningOn(t, ctx, h, s, 3)
		if err := n.Kill(); err != nil {
			t.Fatal(err)
		}
		killed := time.Now()
		waiting := readUntil(t, ctx, s, reconnectState("waiting"))
		w := waiting[len(waiting)-1]
		if msg := w.Event.(*a2a.TaskStatusUpdateEvent).Status.Message; w.State != a2a.TaskStateWorking || msg == nil ||
			msg.Parts[0].Text() != lifecycle.ReconnectText(3*time.Minute) {
			t.Fatalf("the waiting status on the buyer's stream: %s", w.Data)
		}
		t.Logf("the waiting status reached the buyer %.1f s after the kill", time.Since(killed).Seconds())
		if !strings.Contains(stdout(n), "kontra connect: lost the gate") {
			// The killed process printed nothing; the restarted one prints "back after" (checked below).
			t.Log("the killed node printed no loss line (SIGKILL), as expected")
		}
		if err := n.Restart(ctx, h); err != nil {
			t.Fatal(err)
		}
		readUntil(t, ctx, s, reconnectState("resumed"))
		rest, err := s.ReadAll(ctx)
		if err != nil || len(rest) == 0 || rest[len(rest)-1].State != a2a.TaskStateCompleted {
			t.Fatalf("the buyer's stream after the resume: %d events, %v, want it to end completed", len(rest), err)
		}
		completedOnce(t, ctx, h, task, req, n.Session())
		checkHistory(t, h, task, "e2e_reconnect_node_restarted", tunnelForbidden(h)...)

		// (c) The replica holding the session killed: the node redials B on its own.
		if r := sessionReplica(ctx, h, n.Session()); r != a.ReplicaID {
			t.Fatalf("the node's session is on %q, want A", r)
		}
		req = newMessage("tunnel_reattach c")
		s, err = h.BuyerA().SendStreaming(ctx, b, req, cookieCall)
		if err != nil {
			t.Fatal(err)
		}
		task = runningOn(t, ctx, h, s, 3)
		h.Front.Route(b)
		if err := a.Kill(); err != nil {
			t.Fatal(err)
		}
		killed = time.Now()
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
		t.Logf("the node redialed B %.1f s after A's SIGKILL", time.Since(killed).Seconds())
		rest, err = s.ReadAll(ctx)
		if err != nil || len(rest) == 0 || rest[len(rest)-1].State != a2a.TaskStateCompleted {
			t.Fatalf("the buyer's stream after A's kill: %d events, %v, want it to end completed", len(rest), err)
		}
		t.Logf("the task completed %.1f s after A's SIGKILL", time.Since(killed).Seconds())
		completedOnce(t, ctx, h, task, req, onB)
		checkHistory(t, h, task, "e2e_reconnect_replica_killed", tunnelForbidden(h)...)
		if out := stdout(n); !strings.Contains(out, "kontra connect: lost the gate") || !strings.Contains(out, "kontra connect: back after") {
			t.Fatalf("the node did not say it lost the gate and came back:\n%s", out)
		}

		// (d) The node killed, the agent restarted (its tasks lost), the node back: the agent no longer has the task.
		req = newMessage("tunnel_reattach d")
		s, err = h.BuyerA().SendStreaming(ctx, b, req, cookieCall)
		if err != nil {
			t.Fatal(err)
		}
		task = runningOn(t, ctx, h, s, 3)
		if err := n.Kill(); err != nil {
			t.Fatal(err)
		}
		readUntil(t, ctx, s, reconnectState("waiting"))
		if err := h.Upstream.Restart(ctx); err != nil {
			t.Fatal(err)
		}
		if err := n.Restart(ctx, h); err != nil {
			t.Fatal(err)
		}
		rest, err = s.ReadAll(ctx)
		if err != nil || len(rest) == 0 || rest[len(rest)-1].State != a2a.TaskStateFailed {
			t.Fatalf("the buyer's stream: %d events, %v, want it to end failed", len(rest), err)
		}
		last := rest[len(rest)-1].Event.(*a2a.TaskStatusUpdateEvent)
		if last.Metadata["kontra.failure"] != "tunnel_lost" || last.Status.Message.Parts[0].Text() != relay.TextAgentLostTask {
			t.Fatalf("the failure: %s", rest[len(rest)-1].Data)
		}
		checkHistory(t, h, task, "e2e_reconnect_agent_lost_task", tunnelForbidden(h)...)
	})

	t.Run("node not back within the grace", func(t *testing.T) {
		h := start(t, Options{
			Upstream:      slowagent.Config{Chunks: 60, Interval: time.Second, Streaming: true},
			Tunnel:        map[string]TunnelService{"slow": {}},
			ReattachGrace: 10 * time.Second,
		})
		ctx := ctxFor(t, 6*time.Minute)
		n, _ := startNode(t, ctx, h, "n", map[string]string{"slow": h.Upstream.URL()})
		if err := h.WaitCard(ctx, "slow", time.Minute); err != nil {
			t.Fatal(err)
		}
		req := newMessage("tunnel_reattach b")
		s, err := h.BuyerA().SendStreaming(ctx, h.Gates[1], req, cookieCall)
		if err != nil {
			t.Fatal(err)
		}
		task := runningOn(t, ctx, h, s, 2)
		if err := n.Kill(); err != nil {
			t.Fatal(err)
		}
		readUntil(t, ctx, s, reconnectState("waiting"))
		waited := time.Now()
		rest, err := s.ReadAll(ctx)
		if err != nil || len(rest) == 0 || rest[len(rest)-1].State != a2a.TaskStateFailed {
			t.Fatalf("the buyer's stream: %d events, %v, want it to end failed", len(rest), err)
		}
		gaveUp := time.Since(waited)
		last := rest[len(rest)-1].Event.(*a2a.TaskStatusUpdateEvent)
		if last.Metadata["kontra.failure"] != "tunnel_lost" || last.Status.Message.Parts[0].Text() != "The agent's node did not reconnect within 10 seconds." {
			t.Fatalf("the failure: %s", rest[len(rest)-1].Data)
		}
		if gaveUp < 5*time.Second || gaveUp > 25*time.Second {
			t.Fatalf("gave up %.1f s after the waiting status, want about 10 s", gaveUp.Seconds())
		}
		if _, _, as := endedAs(t, ctx, h, task, time.Minute); len(as) != 1 || as[0].Outcome != "tunnel_lost" {
			t.Fatalf("attempts %+v: want one, ended tunnel_lost", as)
		}
		checkHistory(t, h, task, "e2e_reconnect_gave_up", tunnelForbidden(h)...)
	})
}

// runningOn reads the buyer's stream until the task has its first upstream task id and chunks artifact rows, and
// returns the task.
func runningOn(t *testing.T, ctx context.Context, h *Harness, s *Stream, chunks int) a2a.TaskID {
	t.Helper()
	got := 0
	readUntil(t, ctx, s, func(ev Event) bool {
		if ev.Kind == "artifact" {
			got++
		}
		return got >= chunks
	})
	task := s.TaskID()
	attemptsOf(t, ctx, h, task, 20*time.Second, "the first upstream id", func(as []Attempt) bool {
		return len(as) == 1 && as[0].FirstUpstreamIDAt != nil
	})
	return task
}

// reconnectState matches a status event whose metadata["kontra.reconnect"].state is state.
func reconnectState(state string) func(Event) bool {
	return func(ev Event) bool {
		st, ok := ev.Event.(*a2a.TaskStatusUpdateEvent)
		if !ok {
			return false
		}
		m, _ := st.Metadata["kontra.reconnect"].(map[string]any)
		return m["state"] == state
	}
}

// completedOnce checks a task that completed after a reconnect wait: one attempt, ended completed, now on session, the
// seller sent the message once, and every chunk in the projection.
func completedOnce(t *testing.T, ctx context.Context, h *Harness, task a2a.TaskID, req *a2a.SendMessageRequest, session string) {
	t.Helper()
	state, failure, as := endedAs(t, ctx, h, task, time.Minute)
	if state != "completed" || len(as) != 1 || as[0].Outcome != "completed" || as[0].TunnelSession != session {
		t.Fatalf("the task %s (%q), attempts %+v: want completed on its one attempt, on session %s", state, failure, as, session)
	}
	if got := sendsOf(h.Upstream, req.Message.ID); got != 1 {
		t.Fatalf("the seller received the message %d times, want once", got)
	}
	if got := chunksLogged(t, ctx, h, task); got != 60 {
		t.Fatalf("%d chunks in the task's projection, want 60", got)
	}
	reconnects := 0
	rows, err := h.Rows(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		var sr a2a.StreamResponse
		if r.Kind == "status" && json.Unmarshal(r.Payload, &sr) == nil {
			if st, ok := sr.Event.(*a2a.TaskStatusUpdateEvent); ok && st.Metadata["kontra.reconnect"] != nil {
				reconnects++
			}
		}
	}
	if reconnects > 2 {
		t.Fatalf("%d kontra.reconnect statuses in the log, want at most one waiting and one resumed", reconnects)
	}
}

// chunksLogged counts the slowagent chunks in the task's projection.
func chunksLogged(t *testing.T, ctx context.Context, h *Harness, task a2a.TaskID) int {
	t.Helper()
	var snap string
	if err := h.Pool.QueryRow(ctx, `select snapshot::text from kontra.tasks where id = $1`, string(task)).Scan(&snap); err != nil {
		t.Fatal(err)
	}
	return strings.Count(snap, slowagent.ChunkMarker)
}

// stdout is the node's stdout, every launch's.
func stdout(n *Node) string {
	b, _ := os.ReadFile(n.OutPath)
	return string(b)
}
