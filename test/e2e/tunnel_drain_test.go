package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// The node's log lines for {"t": "drain"} (internal/node control.go and run.go).
const (
	nodeLogDrain    = "the gate drains this session's replica"
	nodeLogRedialed = "the gate is draining this replica: dialing a new session at once (make-before-break)"
)

// TestTunnelDrain is the make-before-break drain (spec/tunnel.md §11, §13; relay.md §14.3; D35, D76): a node's
// session is on replica A (drain_timeout 30 s) while a 10 s streaming task runs on it. The Front is routed
// to B and A gets SIGTERM. A marks itself draining (gate_replicas.draining_at) and only then tells the node
// {"t": "drain"} (M2 plan audit: a node told before the mark commits could supersede its own session and
// cut its tasks); the node logs it and dials B at once. Within 5 s of the SIGTERM the node's key has a
// session on B while its session on A remains: a draining replica's session is never superseded. The task
// completes on A's session, with no tunnel_lost; a task sent once the B session is in runs on it. A exits
// after its last dispatch has ended, and its session's row and its own are deleted.
func TestTunnelDrain(t *testing.T) {
	h := start(t, Options{
		Upstream:     slowagent.Config{Chunks: 20, Interval: 500 * time.Millisecond, Streaming: true},
		Tunnel:       map[string]TunnelService{"slow": {}},
		DrainTimeout: 30 * time.Second,
	})
	ctx := ctxFor(t, 5*time.Minute)
	a, b := h.Gates[0], h.Gates[1]
	n, onA := startNode(t, ctx, h, "n", map[string]string{"slow": h.Upstream.URL()})
	if err := h.WaitCard(ctx, "slow", time.Minute); err != nil {
		t.Fatal(err)
	}
	s, err := h.BuyerA().SendStreaming(ctx, b, newMessage("tunnel drain"), Call{})
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, ctx, s, func(ev Event) bool { return ev.Kind == "artifact" })
	task1 := s.TaskID()
	attemptsOf(t, ctx, h, task1, 20*time.Second, "task 1's first upstream id on A's session", func(as []Attempt) bool {
		return len(as) == 1 && as[0].FirstUpstreamIDAt != nil && as[0].TunnelSession == onA
	})

	h.Front.Route(b)
	if err := a.Term(); err != nil {
		t.Fatal(err)
	}
	termed := time.Now()

	// The node is told once draining_at has committed, and redials at once.
	eventually(t, 5*time.Second, "the node logs the drain", func() bool { return len(n.Log(nodeLogDrain)) > 0 })
	var draining bool
	if err := h.Pool.QueryRow(ctx, `select draining_at is not null from kontra.gate_replicas where replica_id = $1`, a.ReplicaID).Scan(&draining); err != nil || !draining {
		t.Fatalf("the node was told to drain before A's gate_replicas.draining_at was set (%v)", err)
	}
	if len(n.Log(nodeLogRedialed)) == 0 {
		t.Fatalf("the node did not dial a new session at once on the drain:\n%s", tail(n.LogPath, 20))
	}
	var onB string
	eventually(t, time.Until(termed.Add(5*time.Second)), "the node's session on B within 5 s of the SIGTERM", func() bool {
		sessions, err := h.Sessions(ctx, n.KeyID)
		for id, r := range sessions {
			if r == b.ReplicaID {
				onB = id
			}
		}
		return err == nil && onB != ""
	})
	if r := sessionReplica(ctx, h, onA); r != a.ReplicaID {
		t.Fatalf("A's session row is gone once B's is in (replica %q): a draining replica's session was superseded", r)
	}
	t.Logf("the node's session on B is in %.1f s after A's SIGTERM; A's session remains", time.Since(termed).Seconds())

	// A task sent now runs on the B session.
	s2, err := h.BuyerA().SendStreaming(ctx, b, newMessage("tunnel drain 2"), Call{})
	if err != nil {
		t.Fatal(err)
	}
	evs2, err := s2.ReadAll(ctx)
	if err != nil || len(evs2) == 0 || evs2[len(evs2)-1].State != a2a.TaskStateCompleted {
		t.Fatalf("task 2's stream: %d events %v, want it to end completed", len(evs2), err)
	}
	if as, err := h.Attempts(ctx, s2.TaskID()); err != nil || len(as) != 1 || as[0].TunnelSession != onB || as[0].ReplicaID != b.ReplicaID {
		t.Fatalf("task 2's attempts %+v %v: want one on the node's B session %s", as, err, onB)
	}

	// Task 1 finishes on A's session, while A drains; the old session's row stays until then.
	evs1, err := s.ReadAll(ctx)
	if err != nil || len(evs1) == 0 || evs1[len(evs1)-1].State != a2a.TaskStateCompleted {
		t.Fatalf("task 1's stream: %d events %v, want it to end completed", len(evs1), err)
	}
	state, failure, as := endedAs(t, ctx, h, task1, 30*time.Second)
	if state != "completed" || failure != "" || len(as) != 1 || as[0].Outcome != "completed" || as[0].TunnelSession != onA || as[0].ReplicaID != a.ReplicaID {
		t.Fatalf("task 1 ended %s (%q), attempts %+v: want completed on its one attempt on A's session", state, failure, as)
	}
	if err := a.Wait(2 * time.Minute); err != nil {
		t.Fatalf("A after its drain: %v\n%s", err, tail(a.LogPath, 30))
	}
	exited := time.Now()
	if !exited.After(*as[0].EndedAt) {
		t.Fatalf("A exited at %v, before task 1's attempt ended at %v", exited, *as[0].EndedAt)
	}
	t.Logf("A exited %.1f s after its SIGTERM, %.1f s after task 1's attempt ended", exited.Sub(termed).Seconds(), exited.Sub(*as[0].EndedAt).Seconds())
	if r := sessionReplica(ctx, h, onA); r != "" {
		t.Fatalf("A exited and its session's tunnels row is still in (replica %s)", r)
	}
	var rows int
	if err := h.Pool.QueryRow(ctx, `select count(*) from kontra.gate_replicas where replica_id = $1`, a.ReplicaID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("A's gate_replicas row after its drain: %d %v, want deleted", rows, err)
	}
	if lines := grepLog(a.LogPath, `"drained"`); len(lines) == 0 {
		t.Fatalf("A did not log the end of its drain:\n%s", tail(a.LogPath, 30))
	}
	for _, line := range grepLog(a.LogPath, "tunnel_lost") {
		if strings.Contains(line, string(task1)) {
			t.Fatalf("A logged tunnel_lost for task 1: %s", line)
		}
	}
	if sessions, err := h.Sessions(ctx, n.KeyID); err != nil || len(sessions) != 1 || sessions[onB] != b.ReplicaID {
		t.Fatalf("the node's sessions after A's exit: %v %v, want only %s on B", sessions, err, onB)
	}
	checkHistory(t, h, task1, "e2e_tunnel_drain", tunnelForbidden(h)...)
}
