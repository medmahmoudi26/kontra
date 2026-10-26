package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/gate"
	"kontra.sh/kontra/test/e2e/slowagent"
)

// The node's log lines for {"t": "drain"} (internal/node control.go and run.go).
const (
	nodeLogDrain    = "the gate drains this session's replica"
	nodeLogRedialed = "the gate is draining this replica: dialing a new session at once (make-before-break)"
)

// TestTunnelDrain is the make-before-break drain (spec/tunnel.md §11, §13; relay.md §14.3; D35, D76, D141):
// a node's session is on replica A (drain_timeout 30 s) while a 10 s streaming task runs on it. The Front
// is routed to B and A gets SIGTERM. A marks itself draining (gate_replicas.draining_at) and only then
// tells the node {"t": "drain"} (M2 plan audit: a node told before the mark commits could supersede its
// own session and cut its tasks).
//
// The order is checked, not assumed: before the SIGTERM the test holds A's gate_replicas row (select … for
// no key update, in an open transaction), so A's draining_at write waits on it. While the test sees that
// write waiting (pg_blocking_pids) and draining_at still null, for 2×DrainMarkRetry, the node must log no
// drain; a gate that told the nodes before its mark committed fails here (M2 review of WP-08). Once the
// row is released the write commits, the node logs the drain and dials B at once, and within 5 s its key
// has a session on B while its session on A remains: a draining replica's session is never superseded.
// The task completes on A's session, with no tunnel_lost; a task sent once the B session is in runs on
// it. A exits after its last dispatch has ended, and its session's row and its own are deleted.
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

	// The test holds A's gate_replicas row across the SIGTERM: A's draining_at write waits on it, and the node
	// must not be told meanwhile. For no key update blocks the row's updates (the mark, A's heartbeat) and
	// not the key-share locks of the tunnels rows that reference it.
	hold, err := h.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.WithoutCancel(ctx)) }()
	var holder int
	if err := hold.QueryRow(ctx, `select pg_backend_pid() from kontra.gate_replicas where replica_id = $1 for no key update`,
		a.ReplicaID).Scan(&holder); err != nil {
		t.Fatalf("holding A's gate_replicas row: %v", err)
	}
	if err := a.Term(); err != nil {
		t.Fatal(err)
	}
	termed := time.Now()
	eventually(t, 5*time.Second, "A's draining_at write waiting on the held row", func() bool {
		var waiting int
		err := h.Pool.QueryRow(ctx, `select count(*) from pg_stat_activity
			where $1 = any (pg_blocking_pids(pid)) and starts_with(query, 'update kontra.gate_replicas set draining_at')`, holder).Scan(&waiting)
		return err == nil && waiting > 0
	})
	for held := time.Now(); time.Since(held) < 2*gate.DrainMarkRetry; time.Sleep(100 * time.Millisecond) {
		var draining bool
		if err := h.Pool.QueryRow(ctx, `select draining_at is not null from kontra.gate_replicas where replica_id = $1`, a.ReplicaID).Scan(&draining); err != nil || draining {
			t.Fatalf("A's draining_at while the test holds its row: set %v (%v), want null", draining, err)
		}
		if lines := n.Log(nodeLogDrain); len(lines) > 0 {
			t.Fatalf("the node was told to drain %.1f s after A's SIGTERM, while A's draining_at write waited on the held row: %s",
				time.Since(termed).Seconds(), lines[0])
		}
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	released := time.Now()

	// The mark commits, then the node is told and redials at once.
	eventually(t, 5*time.Second, "the node logs the drain once A's row is released", func() bool { return len(n.Log(nodeLogDrain)) > 0 })
	var draining bool
	if err := h.Pool.QueryRow(ctx, `select draining_at is not null from kontra.gate_replicas where replica_id = $1`, a.ReplicaID).Scan(&draining); err != nil || !draining {
		t.Fatalf("the node was told to drain and A's gate_replicas.draining_at is not set (%v)", err)
	}
	if len(n.Log(nodeLogRedialed)) == 0 {
		t.Fatalf("the node did not dial a new session at once on the drain:\n%s", tail(n.LogPath, 20))
	}
	var onB string
	eventually(t, time.Until(released.Add(5*time.Second)), "the node's session on B within 5 s of the mark's release", func() bool {
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
	t.Logf("the node's session on B is in %.1f s after A's SIGTERM, %.1f s after the release of A's row; A's session remains",
		time.Since(termed).Seconds(), time.Since(released).Seconds())

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
