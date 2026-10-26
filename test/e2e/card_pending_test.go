package e2e

import (
	"net/url"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// gateLogCardRetry is what a replica logs for each failed refetch of a tunnel service's card (internal/gate
// services.go runTunnelCard); the retries back off 1 s doubling to 1 min.
const gateLogCardRetry = "services: a tunnel service's card was not refetched"

// TestTunnelCardPending is a task placed before a tunnel service's first card (D143; spec/relay.md §3, §6.4;
// the M2 plan audit, gap 3, its second option; the M2 handoff from WP-04 to WP-05 and WP-08). A node N starts
// before its agent, as a compose or systemd sidecar does: a proxy in front of the agent refuses every
// connection, so N answers each card fetch 502 Kontra-Node: target_unreachable. N's session on replica A
// is a candidate anyway, no card is stored, and A keeps refetching on its backoff, which grows while the
// agent stays down; the test waits for three failed refetches (the next one is then 4 s away, 8 s after a
// kick that only restarted the wait).
//
// The agent comes up and a task (task.retries 2: three attempts within 3 s) is sent at once. Its first
// dispatch finds no card (relay.ErrCardPending): the attempt ends service_unavailable with no upstream id
// and no request to the agent, and is retried, while the dispatch has the card fetched again at once. A
// later attempt completes on N's session, the agent received the task's message once, and the card is
// stored. A dispatch that only restarted the keeper's backoff would leave the card to the next refetch,
// after the task's retries ran out (M2 review of WP-08).
func TestTunnelCardPending(t *testing.T) {
	h := start(t, Options{
		Upstream: slowagent.Config{Chunks: 2, Interval: 50 * time.Millisecond, Streaming: true},
		Tunnel:   map[string]TunnelService{"slow": {}},
	})
	ctx := ctxFor(t, 4*time.Minute)
	a := h.Gates[0]
	agent, err := url.Parse(h.Upstream.URL())
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProxy(agent.Host)
	if err != nil {
		t.Fatal(err)
	}
	h.OnClose(func() error { p.Close(); return nil })
	p.Cut() // the agent is not up yet

	before := len(grepLog(a.LogPath, gateLogCardRetry))
	_, sess := startNode(t, ctx, h, "n", map[string]string{"slow": "http://" + p.Addr})
	eventually(t, 30*time.Second, "three failed refetches of the card on A", func() bool {
		return len(grepLog(a.LogPath, gateLogCardRetry))-before >= 3
	})
	var stored bool
	if err := h.Pool.QueryRow(ctx, `select card is not null from kontra.services where name = 'slow'`).Scan(&stored); err != nil || stored {
		t.Fatalf("a card is stored (%v) while the agent is down", err)
	}

	// The agent comes up; a task is sent at once.
	p.Restore()
	req := newMessage("before the first card")
	sent := time.Now()
	res, err := h.BuyerA().Client(t, h.Gates[1], "").SendMessage(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("a task placed before the first card: %#v, want completed on a later attempt", res)
	}
	// The last attempt ends after the completed event that answered the buyer (Dispatcher.settle; M2 review,
	// robustness-1).
	as := attemptsOf(t, ctx, h, task.ID, 30*time.Second, "the last attempt of "+string(task.ID)+" ended", func(as []Attempt) bool {
		return len(as) > 0 && as[len(as)-1].EndedAt != nil
	})
	if len(as) < 2 {
		t.Fatalf("its attempts %+v: want a pending-card attempt, then the completed one", as)
	}
	for _, at := range as[:len(as)-1] {
		if at.Outcome != "service_unavailable" || at.UpstreamTaskID != "" || at.TunnelSession != sess {
			t.Fatalf("its attempt %+v: want ended service_unavailable on N's session %s, with no upstream id", at, sess)
		}
	}
	if last := as[len(as)-1]; last.Outcome != "completed" || last.TunnelSession != sess {
		t.Fatalf("its last attempt %+v: want completed on N's session %s", last, sess)
	}
	if got := sendsOf(h.Upstream, req.Message.ID); got != 1 {
		t.Fatalf("the agent received the task's message %d times, want once", got)
	}
	t.Logf("the task completed %.1f s after it was sent, on attempt %d of %d", time.Since(sent).Seconds(), len(as), 3)
	if err := h.WaitCard(ctx, "slow", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	checkHistory(t, h, task.ID, "e2e_tunnel_card_pending", tunnelForbidden(h)...)
}
