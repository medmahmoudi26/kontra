package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/test/e2e/slowagent"
)

// TestKeyScope is key_scope (spec/tunnel.md §5, §8.1, §12, §15, §16; relay.md §6.1, §16; PRD §15): tunnel
// services `hello` (a slowagent) and `recon` (no node).
//
//  1. A node N is enrolled for hello with a service token and declares hello only.
//  2. A node with a fresh key and a fresh hello token that declares hello and recon is refused
//     service_denied (close 4006: the token binds hello only, and nothing enrolls recon), exits 1, and
//     nothing is written: no key, no key_services row for recon (M2 plan audit: not N's used token,
//     which would be enroll_invalid).
//  3. A task to recon fails no_session after its retries, while hello's seller and N's log show no request
//     or preamble for recon; tunnels.services is {hello}; a task to hello completes.
//  4. N is SIGKILLed and its row is gone (the audit: two sessions of one key would supersede each other).
//     A FakeNode holding N's key connects declaring hello; it proxies to hello's seller and ignores every
//     control line. An 'ask' task waits at input-required on its session; the hello binding is revoked
//     (`kontra-gate enroll --revoke <key> --service hello`). Within 5 s tunnels.services no longer holds
//     hello. The reply fails the task tunnel_lost and opens no stream: the FakeNode counts no new preamble,
//     though it would have served it (M2 plan audit, gap 2: the registry's re-check before every stream,
//     not only the candidate query). A new hello task fails no_session, again with no preamble. The gate
//     sent {"t": "revoked", "service": "hello"} and never relied on the node honouring it.
//  5. N's key, declaring hello and recon with no token, is refused service_denied as well (a known key).
func TestKeyScope(t *testing.T) {
	h := start(t, Options{
		Upstream: slowagent.Config{Chunks: 2, Interval: 50 * time.Millisecond, Streaming: true},
		Services: map[string]slowagent.Config{"hello": {Chunks: 3, Interval: 100 * time.Millisecond, Streaming: true}},
		Tunnel:   map[string]TunnelService{"hello": {}, "recon": {}},
	})
	ctx := ctxFor(t, 6*time.Minute)
	seller := h.Upstreams["hello"]

	// 1. N, enrolled for hello.
	n, sess := startNode(t, ctx, h, "n", map[string]string{"hello": seller.URL()})
	if err := h.WaitCard(ctx, "hello", time.Minute); err != nil {
		t.Fatal(err)
	}

	// 2. A fresh key with a fresh hello token declaring hello and recon.
	tok, err := h.EnrollToken(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	both, err := h.LaunchNode(NodeOptions{Name: "both", Serves: map[string]string{"hello": seller.URL(), "recon": seller.URL()}, Enroll: []string{tok}})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := both.Wait(Reregister); err != nil || code != 1 {
		t.Fatalf("a node declaring recon without an authority for it: exit %d %v, want 1", code, err)
	}
	if lines := both.Log(`"code":"service_denied"`); len(lines) == 0 {
		t.Fatalf("the node was not refused service_denied:\n%s", tail(both.LogPath, 20))
	}
	fresh, err := nodekey.Load(both.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	var keys, recon, unused int
	if err := h.Pool.QueryRow(ctx, `select (select count(*) from kontra.tunnel_keys where key_id = $1),
		(select count(*) from kontra.key_services where service = 'recon'),
		(select count(*) from kontra.enrollment_tokens where used_at is null)`, fresh.KeyID()).Scan(&keys, &recon, &unused); err != nil {
		t.Fatal(err)
	}
	if keys != 0 || recon != 0 || unused != 1 {
		t.Fatalf("after the refusal: %d rows for the fresh key, %d key_services rows for recon, %d unused tokens; want 0, 0 and 1 "+
			"(the refused hello wrote nothing and its token is still unused)", keys, recon, unused)
	}

	// 3. recon has no session: its task fails no_session after its retries, and nothing reaches N for it.
	res, err := h.BuyerA().Client(t, h.Gates[1], "recon").SendMessage(ctx, newMessage("for recon"))
	if err != nil {
		t.Fatal(err)
	}
	rt, ok := res.(*a2a.Task)
	if !ok || rt.Status.State != a2a.TaskStateFailed || rt.Metadata["kontra.failure"] != "no_session" {
		t.Fatalf("a task to recon: %#v, want failed: no_session", res)
	}
	as, err := h.Attempts(ctx, rt.ID)
	if err != nil || len(as) != 3 {
		t.Fatalf("recon's attempts %+v %v: want 3 (retries 2)", as, err)
	}
	for _, a := range as {
		if a.Outcome != "no_session" || a.TunnelSession != "" {
			t.Fatalf("recon's attempt %+v: want no_session, with no session", a)
		}
	}
	for _, r := range seller.Requests() {
		if strings.Contains(string(r.Body), "for recon") {
			t.Fatalf("hello's seller received recon's task: %s %s", r.Method, r.Body)
		}
	}
	if lines := n.Log(`"service":"recon"`); len(lines) > 0 {
		t.Fatalf("N's log names recon: %s", lines[0])
	}
	if services, ok, err := h.SessionServices(ctx, sess); err != nil || !ok || !slices.Equal(services, []string{"hello"}) {
		t.Fatalf("N's tunnels.services %v %v %v, want {hello}", services, ok, err)
	}
	res, err = h.BuyerA().Client(t, h.Gates[0], "hello").SendMessage(ctx, newMessage("for hello"))
	if err != nil {
		t.Fatal(err)
	}
	if ht, ok := res.(*a2a.Task); !ok || ht.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("a task to hello: %#v, want completed", res)
	}
	checkHistory(t, h, rt.ID, "e2e_key_scope_no_session", tunnelForbidden(h)...)

	// 4. N dies; a FakeNode holding its key, which ignores control lines, takes its place.
	if err := n.Kill(); err != nil {
		t.Fatal(err)
	}
	waitRowGone(t, ctx, h, sess, 5*time.Second, "N was SIGKILLed")
	key, err := nodekey.Load(n.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	fake, err := DialFakeNode(ctx, h.Front.URL, key, []string{"hello"}, nil, seller.URL())
	if err != nil {
		t.Fatal(err)
	}
	h.OnClose(fake.Close)
	if !slices.Equal(fake.Served, []string{"hello"}) {
		t.Fatalf("the FakeNode's welcome serves %v, want [hello]", fake.Served)
	}
	eventually(t, 15*time.Second, "the FakeNode's tunnels row", func() bool { return sessionReplica(ctx, h, fake.Session) != "" })
	res, err = h.BuyerA().Client(t, h.Gates[1], "hello").SendMessage(ctx, newMessage(slowagent.AskPrefix+" key_scope"))
	if err != nil {
		t.Fatal(err)
	}
	waiting, ok := res.(*a2a.Task)
	if !ok || waiting.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("an ask task to hello: %#v, want input-required", res)
	}
	if as, err := h.Attempts(ctx, waiting.ID); err != nil || len(as) != 1 || as[0].TunnelSession != fake.Session {
		t.Fatalf("the ask task's attempts %+v %v: want it on the FakeNode's session %s", as, err, fake.Session)
	}
	served := len(fake.Preambles())
	if served == 0 {
		t.Fatal("the FakeNode served no stream for the ask task")
	}
	if err := h.Revoke(ctx, key.KeyID(), "hello"); err != nil {
		t.Fatal(err)
	}
	revoked := time.Now()
	eventually(t, 5*time.Second, "hello gone from the FakeNode's tunnels.services", func() bool {
		services, ok, err := h.SessionServices(ctx, fake.Session)
		return err == nil && ok && !slices.Contains(services, "hello")
	})
	t.Logf("tunnels.services lost hello %.1f s after the revocation", time.Since(revoked).Seconds())
	eventually(t, 10*time.Second, "the gate's {t: revoked, service: hello} to the FakeNode", func() bool {
		return slices.Contains(fake.Controls(), tunnel.Control{T: tunnel.CtlRevoked, Service: "hello"})
	})
	msg, lost := followUp(t, ctx, h, h.Gates[0], "hello", waiting, "after the revocation")
	if lost.Status.State != a2a.TaskStateFailed || lost.Metadata["kontra.failure"] != "tunnel_lost" {
		t.Fatalf("the reply after the binding's revocation: %s %v, want failed: tunnel_lost", lost.Status.State, lost.Metadata)
	}
	if got := len(fake.Preambles()); got != served {
		t.Fatalf("the FakeNode received %d streams after the binding's revocation: %+v", got-served, fake.Preambles()[served:])
	}
	if requestWith(seller, msg.ID) != nil {
		t.Fatal("the reply reached the seller")
	}
	if a := endedAttempt(t, ctx, h, waiting.ID); a.Outcome != "tunnel_lost" {
		t.Fatalf("the attempt %+v, want ended tunnel_lost", a)
	}
	checkHistory(t, h, waiting.ID, "e2e_key_scope_revoked_binding", tunnelForbidden(h)...)
	res, err = h.BuyerA().Client(t, h.Gates[1], "hello").SendMessage(ctx, newMessage("hello, after the revocation"))
	if err != nil {
		t.Fatal(err)
	}
	if nt, ok := res.(*a2a.Task); !ok || nt.Status.State != a2a.TaskStateFailed || nt.Metadata["kontra.failure"] != "no_session" {
		t.Fatalf("a new hello task after the revocation: %#v, want failed: no_session", res)
	}
	if got := len(fake.Preambles()); got != served {
		t.Fatalf("the FakeNode received %d streams for the new task", got-served)
	}
	if fake.Closed() {
		t.Fatal("the FakeNode's session was closed: a binding revocation keeps the session (§12)")
	}

	// 5. N's key (known) declaring hello and recon, no token: service_denied too.
	if err := fake.Close(); err != nil {
		t.Fatal(err)
	}
	waitRowGone(t, ctx, h, fake.Session, 5*time.Second, "the FakeNode left")
	known, err := h.LaunchNode(NodeOptions{Name: "known", Home: n.Home, Serves: map[string]string{"hello": seller.URL(), "recon": seller.URL()}})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := known.Wait(Reregister); err != nil || code != 1 || len(known.Log(`"code":"service_denied"`)) == 0 {
		t.Fatalf("N's key declaring recon: exit %d %v, want 1 after service_denied:\n%s", code, err, tail(known.LogPath, 20))
	}
}
