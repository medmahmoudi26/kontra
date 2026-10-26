package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// gateLogStaleView is what a replica logs when a re-read of its revocation view fails (internal/enroll
// revocations.go): the view stops being fresh 5 s after the last re-read that succeeded began, and the
// registry then reads each key's rows before opening a stream.
const gateLogStaleView = "node revocations: re-read failed"

// TestTunnelStaleRevocationView is key_scope's fail-closed half end to end (spec/relay.md §2.3, §6.1, §6.4;
// tunnel.md §12; the M2 handoff from WP-05 to WP-08): while a replica's revocation view is stale, every
// tunnel stream it opens first reads the key's and the binding's rows, and a read Postgres does not answer
// refuses the stream within 2 s (tunnelgate allowsReadTimeout), as a lost session, rather than waiting for
// the dispatch activity's own timeout.
//
// An 'ask' task waits at input-required on node N's session on replica A. The test then takes
// kontra.tunnel_keys in access exclusive mode, in a transaction it holds open, so that no statement reading
// the table returns: A's re-reads fail (A logs it, and its view is stale from then on) and so would the
// stream's read. The follow-up, sent through B, is answered after the read's 2 s and within 10 s: failed:
// tunnel_lost on the task's one attempt, ended tunnel_lost, and the seller never receives it (no stream
// opened). Once the lock is released, a new task completes on the same session: a refused stream does not
// close the session.
//
// The handoff proposed pausing all of A's Postgres (Options.PostgresProxy, Blackhole). With every statement
// of A waiting, though, the follow-up's dispatch stops at its own first read of the task (relay.Dispatcher's
// preamble, PreambleTimeout 10 s) before any stream, and the activities A's kontra-lifecycle worker takes
// (attempt_state, write_terminal) each wait out their 10 s before another replica runs them: a probe of
// that step answered the follow-up failed: tunnel_lost only after 70 s. It would time those, not the
// stream's read. Holding the one table the read needs leaves every other statement answered, so the
// stream's read is the one that waits.
func TestTunnelStaleRevocationView(t *testing.T) {
	h := start(t, Options{
		Upstream: slowagent.Config{Chunks: 3, Interval: 100 * time.Millisecond, Streaming: true},
		Tunnel:   map[string]TunnelService{"slow": {}},
	})
	ctx := ctxFor(t, 4*time.Minute)
	a, b := h.Gates[0], h.Gates[1]
	n, sess := startNode(t, ctx, h, "n", map[string]string{"slow": h.Upstream.URL()})
	if err := h.WaitCard(ctx, "slow", time.Minute); err != nil {
		t.Fatal(err)
	}
	waiting, att := askOn(t, ctx, h, sess, "stale view")
	if att.ReplicaID != a.ReplicaID {
		t.Fatalf("the ask task's attempt %+v: want it dispatched on A", att)
	}

	// No statement reading tunnel_keys returns while the test holds the table.
	hold, err := h.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.WithoutCancel(ctx)) }()
	before := len(grepLog(a.LogPath, gateLogStaleView))
	if _, err := hold.Exec(ctx, `lock table kontra.tunnel_keys in access exclusive mode`); err != nil {
		t.Fatal(err)
	}
	locked := time.Now()
	eventually(t, 20*time.Second, "A's revocation view going stale (a failed re-read)", func() bool {
		return len(grepLog(a.LogPath, gateLogStaleView)) > before
	})
	t.Logf("A's re-read failed %.1f s after the lock", time.Since(locked).Seconds())

	// The follow-up's stream is refused within seconds, and fails the task tunnel_lost.
	sent := time.Now()
	msg, lost := followUp(t, ctx, h, b, "", waiting, "while the key cannot be read")
	took := time.Since(sent)
	if lost.Status.State != a2a.TaskStateFailed || lost.Metadata["kontra.failure"] != "tunnel_lost" {
		t.Fatalf("the follow-up while A's view is stale and the key unreadable: %s %v, want failed: tunnel_lost", lost.Status.State, lost.Metadata)
	}
	switch {
	case took > 10*time.Second:
		t.Fatalf("the follow-up was answered %.1f s after it was sent, want within 10 s (the key's read is bounded by 2 s)", took.Seconds())
	case took < 1500*time.Millisecond:
		t.Fatalf("the follow-up was refused %.1f s after it was sent: at once, not after waiting on the key's read", took.Seconds())
	}
	t.Logf("the follow-up failed tunnel_lost %.1f s after it was sent", took.Seconds())
	if requestWith(h.Upstream, msg.ID) != nil {
		t.Fatal("the follow-up reached the seller: a stream was opened")
	}
	// The attempt ends after the failed event that answered the follow-up (Dispatcher.settle; M2 review,
	// robustness-1).
	if a := endedAttempt(t, ctx, h, waiting.ID); a.Outcome != "tunnel_lost" || a.TunnelSession != sess {
		t.Fatalf("the task's attempt %+v: want its one attempt on %s ended tunnel_lost", a, sess)
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	checkHistory(t, h, waiting.ID, "e2e_tunnel_stale_revocation_view", tunnelForbidden(h)...)

	// The session was kept: a new task completes on it.
	res, err := h.BuyerA().Client(t, b, "").SendMessage(ctx, newMessage("once the key can be read"))
	if err != nil {
		t.Fatal(err)
	}
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("a task once the lock is released: %#v, want completed", res)
	}
	if as, err := h.Attempts(ctx, task.ID); err != nil || len(as) != 1 || as[0].TunnelSession != sess {
		t.Fatalf("its attempts %+v %v: want one, on the same session %s", as, err, sess)
	}
	if ws := n.Welcomes(); len(ws) != 1 {
		t.Fatalf("N printed %d welcomes: its session was closed", len(ws))
	}
}
