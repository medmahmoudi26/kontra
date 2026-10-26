package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// The node's log lines for a lost outcome and its probe (internal/node run.go connect).
const (
	nodeLogNoOutcome = "the connection ended after the proof, before the gate's answer"
	nodeLogProbe     = "dialing with the active key alone: the outcome of an earlier hello"
	nodeLogConfirmed = "the gate confirmed the enrollment: its tokens are not sent again"
)

// gateLogNotRegistered is what a replica logs when a session's registration inserts nothing
// (internal/tunnelgate handler.go serve), and supersededByNewer the reason a newer session of the key gives
// (registry.go insert).
const (
	gateLogNotRegistered = "tunnel session not registered: closing it"
	supersededByNewer    = "a newer session "
)

// TestTunnelLostWelcome is a welcome lost after the gate committed the admission (spec/tunnel.md §4, §5,
// §11; D124, D139; the M2 handoff from WP-04 and WP-06 to WP-08). A node N starts with a service token for
// `slow`, and the Front loses the gate's welcome to N's first hello (Front.LoseWelcome): replica A admitted
// the hello (the token is used) and wrote the welcome, but N sees its connection end after the proof. N
// redials with a probe, its active key alone and no token; A welcomes it as a known key, which confirms the
// token, and N records it used.
//
// The first session S1 is half-open: the Front holds its connection open and silent, so A goes on to
// register it once its card fetch through S1 has timed out (CardWait, 10 s), well after N's redialed session
// S2 registered. S1's registration finds the newer S2, inserts nothing and closes S1 (D139: "older" is the
// order of the handshakes, not of the registrations); S2 survives it: the key's only tunnels row is S2's, N
// prints no other welcome and is still running, and a task completes on S2. N then restarts with its
// original --enroll: it leaves out the used token and reconnects, and no hello of N is refused
// enroll_invalid.
func TestTunnelLostWelcome(t *testing.T) {
	h := start(t, Options{
		Upstream: slowagent.Config{Chunks: 2, Interval: 50 * time.Millisecond, Streaming: true},
		Tunnel:   map[string]TunnelService{"slow": {}},
	})
	ctx := ctxFor(t, 4*time.Minute)
	a := h.Gates[0]

	h.Front.LoseWelcome(1)
	started := time.Now()
	n, s2 := startNode(t, ctx, h, "n", map[string]string{"slow": h.Upstream.URL()})
	lost := h.Front.LostWelcomes()
	if len(lost) != 1 {
		t.Fatalf("the Front lost %d welcomes %v, want N's first", len(lost), lost)
	}
	s1 := lost[0]
	if s2 <= s1 { // UUIDv7: the text order is the order the gate minted them in
		t.Fatalf("N's lost session S1 %s and its redialed session S2 %s: want S2 the newer", s1, s2)
	}
	if ws := n.Welcomes(); len(ws) != 1 || ws[0].Session != s2 {
		t.Fatalf("N printed the welcomes %+v, want only S2's %s", ws, s2)
	}
	t.Logf("N's welcome to S1 %s was lost; S2 %s registered %.1f s after N started", s1, s2, time.Since(started).Seconds())

	// N's side: the outcome was lost, the probe was welcomed, and the welcome confirmed the token.
	for _, line := range []string{nodeLogNoOutcome, nodeLogProbe, nodeLogConfirmed} {
		if len(n.Log(line)) == 0 {
			t.Fatalf("N did not log %q:\n%s", line, tail(n.LogPath, 20))
		}
	}
	if refused := n.Log(`"code":"enroll_invalid"`); len(refused) > 0 {
		t.Fatalf("N resent its used token: %s", refused[0])
	}

	// S1 registers late on A: it finds S2 and inserts nothing.
	var late string
	eventually(t, 30*time.Second, "A refuses to register the half-open S1", func() bool {
		for _, l := range grepLog(a.LogPath, gateLogNotRegistered) {
			if strings.Contains(l, s1) && strings.Contains(l, supersededByNewer+s2) {
				late = l
				return true
			}
		}
		return false
	})
	t.Logf("A's registration of S1, %.1f s after N started: %s", time.Since(started).Seconds(), late)

	// S2 survives it.
	if sessions, err := h.Sessions(ctx, n.KeyID); err != nil || len(sessions) != 1 || sessions[s2] != a.ReplicaID {
		t.Fatalf("N's key's tunnels rows after S1's late registration: %v %v, want only S2 %s on A", sessions, err, s2)
	}
	if ws := n.Welcomes(); len(ws) != 1 {
		t.Fatalf("N printed %d welcomes %+v: S2 was closed and N redialed", len(ws), ws)
	}
	if n.Exited() {
		code, _ := n.Wait(0)
		t.Fatalf("N exited (%d):\n%s", code, tail(n.LogPath, 20))
	}
	res, err := h.BuyerA().Client(t, h.Gates[1], "").SendMessage(ctx, newMessage("after the lost welcome"))
	if err != nil {
		t.Fatal(err)
	}
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("a task after the lost welcome: %#v, want completed", res)
	}
	if as, err := h.Attempts(ctx, task.ID); err != nil || len(as) != 1 || as[0].TunnelSession != s2 {
		t.Fatalf("its attempts %+v %v: want one, on S2 %s", as, err, s2)
	}
	checkHistory(t, h, task.ID, "e2e_tunnel_lost_welcome", tunnelForbidden(h)...)

	// A restart with the original --enroll: the used token is left out, and N reconnects.
	if code, err := n.Term(); err != nil || code != 0 {
		t.Fatalf("N on SIGTERM: exit %d %v, want 0", code, err)
	}
	if err := n.Restart(ctx, h); err != nil {
		t.Fatal(err)
	}
	if !skippedUsedToken(n) {
		t.Fatalf("the restarted N did not leave out its used token:\n%s", tail(n.LogPath, 20))
	}
	if refused := n.Log(`"code":"enroll_invalid"`); len(refused) > 0 {
		t.Fatalf("the restarted N resent its used token: %s", refused[0])
	}
	if sessions, err := h.Sessions(ctx, n.KeyID); err != nil || len(sessions) != 1 || sessions[n.Session()] == "" {
		t.Fatalf("N's key after the restart: sessions %v %v, want only %s", sessions, err, n.Session())
	}
}
