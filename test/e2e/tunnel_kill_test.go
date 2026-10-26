package e2e

import (
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/test/e2e/slowagent"
)

// TestTunnelKill is tunnel_kill (spec/tunnel.md §11, §13, §16; relay.md §6.4; PRD §14's M2 gate): a node
// N hosts `slow` (20 chunks 500 ms apart) and `delayed` (its first event 5 s late) through the Front, on
// replica A. Task 1 streams from `slow`; once its first upstream task id is recorded and two chunks are
// logged, N is SIGKILLed. The task fails tunnel_lost at once (no retry after the first upstream id), on its
// one attempt, and the seller saw its message once. N's tunnels row is gone within 5 s. N is restarted with
// the same HOME and its original --enroll flag (M2 plan audit, gap 1: the token its first welcome confirmed
// is not sent again), and its key has a new session within 60 s; task 2 completes on it. Then the variant
// before the first upstream id (retries 3): N is SIGKILLed while the `delayed` seller holds the first event
// back and restarted at once; attempt 1 ends with no upstream id (tunnel_lost or no_session), the task
// completes on a later attempt on the new session, and the seller answered once per attempt that reached it.
func TestTunnelKill(t *testing.T) {
	h := start(t, Options{
		Upstream: slowagent.Config{Chunks: 20, Interval: 500 * time.Millisecond, Streaming: true},
		Services: map[string]slowagent.Config{
			"delayed": {Chunks: 4, Interval: 250 * time.Millisecond, Streaming: true, FirstEventDelay: 5 * time.Second},
		},
		Tunnel: map[string]TunnelService{"slow": {}, "delayed": {Retries: retries(3)}},
	})
	ctx := ctxFor(t, 8*time.Minute)
	slow, delayed := h.Upstream, h.Upstreams["delayed"]
	n, first := startNode(t, ctx, h, "n", map[string]string{"slow": slow.URL(), "delayed": delayed.URL()})
	for _, svc := range []string{"slow", "delayed"} {
		if err := h.WaitCard(ctx, svc, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if r := sessionReplica(ctx, h, first); r != h.Gates[0].ReplicaID {
		t.Fatalf("N's session is on %q, want replica A (%s): the Front routes to A", r, h.Gates[0].ReplicaID)
	}

	// Task 1 runs on N's session; the buyer follows it on replica B.
	req1 := newMessage("tunnel_kill 1")
	s, err := h.BuyerA().SendStreaming(ctx, h.Gates[1], req1, Call{})
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, ctx, s, func(ev Event) bool { return ev.Kind == "artifact" })
	task1 := s.TaskID()
	attemptsOf(t, ctx, h, task1, 20*time.Second, "task 1's first upstream id", func(as []Attempt) bool {
		return len(as) == 1 && as[0].FirstUpstreamIDAt != nil
	})
	eventually(t, 20*time.Second, "two chunks of task 1 logged", func() bool {
		rows, err := h.Rows(ctx, task1)
		n := 0
		for _, r := range rows {
			if r.Kind == "artifact" {
				n++
			}
		}
		return err == nil && n >= 2
	})
	if err := n.Kill(); err != nil {
		t.Fatal(err)
	}
	killed := time.Now()
	gone := waitRowGone(t, ctx, h, first, 5*time.Second, "N was SIGKILLed")
	t.Logf("N SIGKILLed; its tunnels row was deleted %.1f s later", gone.Seconds())

	// Task 1 fails tunnel_lost: its upstream task existed (spec §6.4: no retry after the first upstream id).
	state, failure, as := endedAs(t, ctx, h, task1, HeartbeatTimeout)
	if state != "failed" || failure != "tunnel_lost" {
		t.Fatalf("task 1 ended %s (kontra.failure %q), want failed: tunnel_lost", state, failure)
	}
	if len(as) != 1 || as[0].Outcome != "tunnel_lost" || as[0].TunnelSession != first {
		t.Fatalf("task 1's attempts %+v: want one attempt on %s ended tunnel_lost, and no attempt 2", as, first)
	}
	if got := sendsOf(slow, req1.Message.ID); got != 1 {
		t.Fatalf("the seller received task 1's message %d times, want exactly 1", got)
	}
	rest, err := s.ReadAll(ctx)
	if err != nil || len(rest) == 0 || rest[len(rest)-1].State != a2a.TaskStateFailed {
		t.Fatalf("the buyer's stream of task 1 must end with the failed status: %d events, %v", len(rest), err)
	}

	// N restarts with its original command line, --enroll included: it reconnects within the backoff.
	restarted := time.Now()
	if err := n.Restart(ctx, h); err != nil {
		t.Fatal(err)
	}
	second := n.Session()
	t.Logf("N restarted %.1f s after the kill; its key has session %s %.1f s after the restart", restarted.Sub(killed).Seconds(),
		second, time.Since(restarted).Seconds())
	if sessions, err := h.Sessions(ctx, n.KeyID); err != nil || len(sessions) != 1 || sessions[second] == "" {
		t.Fatalf("the key's tunnels rows %v %v: want exactly the new session %s", sessions, err, second)
	}
	if len(n.Log("not sent again")) == 0 {
		t.Fatal("the restarted node did not report that its consumed token is not sent again")
	}
	if refused := n.Log(`"code":"enroll_invalid"`); len(refused) > 0 {
		t.Fatalf("the restarted node resent a consumed token: %s", refused[0])
	}

	// Task 2 completes on the new session.
	res, err := h.BuyerA().Client(t, h.Gates[0], "").SendMessage(ctx, newMessage("tunnel_kill 2"))
	if err != nil {
		t.Fatal(err)
	}
	task2, ok := res.(*a2a.Task)
	if !ok || task2.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("task 2: %#v, want completed", res)
	}
	if as, err := h.Attempts(ctx, task2.ID); err != nil || len(as) != 1 || as[0].TunnelSession != second {
		t.Fatalf("task 2's attempts %+v %v: want one, on the new session %s", as, err, second)
	}
	checkHistory(t, h, task1, "e2e_tunnel_kill", tunnelForbidden(h)...)

	// The variant before the first upstream id: the seller holds its first event back 5 s.
	req3 := newMessage("tunnel_kill before the first id")
	s3, err := h.BuyerA().SendStreaming(ctx, h.Gates[1], req3, Call{Service: "delayed"})
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	eventually(t, 30*time.Second, "the delayed seller received the task", func() bool { return sendsOf(delayed, req3.Message.ID) == 1 })
	task3 := s3.TaskID()
	if task3 == "" {
		ev, err := s3.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		task3 = ev.Event.TaskInfo().TaskID
	}
	if as, err := h.Attempts(ctx, task3); err != nil || len(as) != 1 || as[0].FirstUpstreamIDAt != nil || as[0].TunnelSession != second {
		t.Fatalf("before the kill, task 3's attempts %+v %v: want one on %s with no upstream id", as, err, second)
	}
	if err := n.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := n.Restart(ctx, h); err != nil {
		t.Fatal(err)
	}
	third := n.Session()
	state, failure, as = endedAs(t, ctx, h, task3, 2*time.Minute)
	if state != "completed" {
		t.Fatalf("task 3 ended %s (%q), want completed on a later attempt: %+v", state, failure, as)
	}
	if a1 := as[0]; a1.UpstreamTaskID != "" || a1.FirstUpstreamIDAt != nil || (a1.Outcome != "tunnel_lost" && a1.Outcome != "no_session") {
		t.Fatalf("task 3's attempt 1 %+v: want no upstream id, ended tunnel_lost or no_session", a1)
	}
	last := as[len(as)-1]
	if len(as) < 2 || last.Outcome != "completed" || last.TunnelSession != third {
		t.Fatalf("task 3's attempts %+v: want a later attempt completed on the new session %s", as, third)
	}
	reached := 0
	for _, a := range as {
		if a.Outcome != "no_session" {
			reached++
		}
	}
	if got := sendsOf(delayed, req3.Message.ID); got != reached {
		t.Fatalf("the seller received task 3's message %d times over attempts %+v, want once per attempt that reached it (%d)", got, as, reached)
	}
	t.Logf("task 3: %d attempts, outcomes %v", len(as), outcomes(as))
	checkHistory(t, h, task3, "e2e_tunnel_kill_before_first_id", tunnelForbidden(h)...)
}

// outcomes lists the attempts' outcomes, for a log line.
func outcomes(as []Attempt) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Outcome
	}
	return out
}

// TestNodeRotateAfterLeak is the binary-level rotation after a leak (spec/tunnel.md §12, §15; the
// SPEC-CONFLICT on a rotate_from the gate refuses, M2 WP-06 review): a node's key K1 leaked, so its operator
// rotates it while the gate is unreachable (`kontra connect --rotate`, which writes K2 above K1 and cannot
// reach the gate to carry the rotation), then revokes K1 at the gate (`kontra-gate enroll --revoke K1`).
//
// Restarted without --enroll, the node names K1 in rotate_from, is refused (K1 is revoked), sends the hello
// again without it, and is told unknown_key (the gate never saw K2): it keeps retrying, as §4 says of
// unknown_key, and does not exit. Restarted with a fresh --enroll, it reconnects: K2 is enrolled and has a
// session, and K1 has none.
func TestNodeRotateAfterLeak(t *testing.T) {
	h := start(t, Options{
		Upstream: slowagent.Config{Chunks: 2, Interval: 50 * time.Millisecond, Streaming: true},
		Tunnel:   map[string]TunnelService{"slow": {}},
	})
	ctx := ctxFor(t, 4*time.Minute)
	serves := map[string]string{"slow": h.Upstream.URL()}
	n, _ := startNode(t, ctx, h, "n", serves)
	k1 := n.KeyID
	if code, err := n.Term(); err != nil || code != 0 {
		t.Fatalf("the node on SIGTERM: exit %d %v, want 0", code, err)
	}

	// The rotation, with the gate unreachable.
	h.Front.Route(nil)
	rot, err := h.LaunchNode(NodeOptions{Name: "rotate", Home: n.Home, Serves: serves, Rotate: true})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the rotating node fails to reach the gate", func() bool { return len(rot.Log("the gate could not be dialed")) > 0 })
	if code, err := rot.Term(); err != nil || code != 0 {
		t.Fatalf("the rotating node on SIGTERM: exit %d %v, want 0", code, err)
	}
	id, err := nodekey.Load(n.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	k2 := id.KeyID()
	if k2 == k1 {
		t.Fatal("--rotate did not write a new key")
	}
	if err := h.Revoke(ctx, k1, ""); err != nil {
		t.Fatal(err)
	}
	h.Front.Route(h.Gates[0])

	// Without --enroll: refused rotate_from, then unknown_key, retried.
	bare, err := h.LaunchNode(NodeOptions{Name: "bare", Home: n.Home, Serves: serves})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, Reregister, "the node told unknown_key twice", func() bool {
		return len(bare.Log("the gate does not know this node's key")) >= 2
	})
	if len(bare.Log("the gate refused rotate_from", `"code":"revoked"`)) == 0 {
		t.Fatalf("the node did not drop the refused rotate_from:\n%s", tail(bare.LogPath, 20))
	}
	if bare.Exited() {
		code, _ := bare.Wait(0)
		t.Fatalf("the node exited (%d) on unknown_key: it must keep retrying (§4)", code)
	}
	if code, err := bare.Term(); err != nil || code != 0 {
		t.Fatalf("the retrying node on SIGTERM: exit %d %v, want 0", code, err)
	}

	// With a fresh --enroll: K2 is enrolled and connected.
	tok, err := h.EnrollToken(ctx, "slow")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := h.StartNode(ctx, NodeOptions{Name: "fresh", Home: n.Home, Serves: serves, Enroll: []string{tok}})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.KeyID != k2 {
		t.Fatalf("the node connected as %s, want the rotated key %s", fresh.KeyID, k2)
	}
	if s, err := h.Sessions(ctx, k2); err != nil || len(s) != 1 {
		t.Fatalf("K2's sessions %v %v, want one", s, err)
	}
	if s, err := h.Sessions(ctx, k1); err != nil || len(s) != 0 {
		t.Fatalf("the revoked K1's sessions %v %v, want none", s, err)
	}
	res, err := h.BuyerA().Client(t, h.Gates[1], "").SendMessage(ctx, newMessage("after the rotation"))
	if err != nil {
		t.Fatal(err)
	}
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("a task after the rotation: %#v, want completed", res)
	}
	if as, err := h.Attempts(ctx, task.ID); err != nil || len(as) != 1 || as[0].KeyID != k2 {
		t.Fatalf("its attempts %+v %v: want one, on the rotated key %s", as, err, k2)
	}
	checkHistory(t, h, task.ID, "e2e_rotate_after_leak", tunnelForbidden(h, tok)...)
}
