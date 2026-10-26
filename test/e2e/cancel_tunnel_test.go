package e2e

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// cancelBound is how long a forwarded cancel may take to end the task (spec §8: kontra.cancel's 60 s budget,
// D79).
const cancelBound = 60 * time.Second

// checkCancelThroughNode fails unless agent received exactly one CancelTask, for the upstream task up, as a
// request the node forwarded: Host is the --serve target's own, and no Forwarded or X-Forwarded-* header
// (spec/tunnel.md §8.2; the gate never dials the seller of a tunnel service).
func checkCancelThroughNode(t *testing.T, agent *slowagent.Agent, up string) {
	t.Helper()
	calls := agent.Calls("CancelTask")
	if len(calls) != 1 || calls[0].TaskID != up {
		t.Fatalf("the seller's CancelTask requests %+v: want one, for its upstream task %s", calls, up)
	}
	u, _ := url.Parse(agent.URL())
	if calls[0].Host != u.Host {
		t.Fatalf("the CancelTask arrived with Host %q, want the target's %q", calls[0].Host, u.Host)
	}
	for name := range calls[0].Header {
		if strings.HasPrefix(name, "X-Forwarded-") || name == "Forwarded" {
			t.Fatalf("the CancelTask arrived with %s: %v", name, calls[0].Header[name])
		}
	}
}

// workingTask starts a streaming task on svc through replica B and waits for its first upstream id and
// first chunk; it returns the stream, the task and its attempt.
func workingTask(t *testing.T, ctx context.Context, h *Harness, svc, text string) (*Stream, a2a.TaskID, Attempt) {
	t.Helper()
	s, err := h.BuyerA().SendStreaming(ctx, h.Gates[1], newMessage(text), Call{Service: svc})
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, ctx, s, func(ev Event) bool { return ev.Kind == "artifact" })
	id := s.TaskID()
	as := attemptsOf(t, ctx, h, id, 20*time.Second, "the first upstream id of "+string(id), func(as []Attempt) bool {
		return len(as) == 1 && as[0].FirstUpstreamIDAt != nil
	})
	return s, id, as[0]
}

// TestCancelForwardedTunnel is cancel_forwarded through the tunnel (spec/relay.md §8, §16; PRD §15): one
// node hosts `slow` (a long stream), `talky` (a seller that ignores its own cancel and streams a chunk and
// completed after its canceled status) and `flaky` (a seller behind a TCP proxy the test can cut).
//
//	(a) A working task, its upstream task streaming, is canceled through the replica that does not hold
//	    the session. (c) The same with `talky`. (b) An 'ask' task at input-required is canceled over
//	    HTTP+JSON.
//
// Each time the seller receives CancelTask for the attempt's upstream task through the node (its own Host,
// no X-Forwarded-*), the task is canceled within 60 s, GetTask on both replicas says canceled, and nothing
// but metadata is logged after the canceled row (for talky, neither its late chunk nor its completed).
// Attempt outcomes: canceled for (a) and (c), interrupted or canceled for (b) (§8, D77).
//
//	(d) With the node up and the seller unreachable (the proxy cut: the node answers 502 Kontra-Node:
//	    target_unreachable), the cancel of a task at input-required is not delivered: the caller gets
//	    TaskNotCancelableError, the task stays input-required with no kontra.failure and its attempt open,
//	    and once the seller is back a follow-up completes it.
//	(e) With the node gone (SIGKILL), the cancel of a task at input-required fails it tunnel_lost (§8's row
//	    "exists, session gone"; the SPEC-CONFLICT CancelTunnelLost of M2 WP-05).
//
// The node never listens on a TCP socket: the gate never opened a connection to it (spec/tunnel.md §1).
func TestCancelForwardedTunnel(t *testing.T) {
	h := start(t, Options{
		Upstream: slowagent.Config{Chunks: 60, Interval: 500 * time.Millisecond, Streaming: true},
		Services: map[string]slowagent.Config{
			"talky": {Chunks: 60, Interval: 500 * time.Millisecond, Streaming: true, TalksAfterCancel: true},
			"flaky": {Chunks: 3, Interval: 100 * time.Millisecond, Streaming: true},
		},
		Tunnel: map[string]TunnelService{"slow": {}, "talky": {}, "flaky": {}},
	})
	ctx := ctxFor(t, 8*time.Minute)
	slow, talky, flaky := h.Upstream, h.Upstreams["talky"], h.Upstreams["flaky"]
	cut, err := NewProxy(strings.TrimPrefix(flaky.URL(), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	h.OnClose(func() error { cut.Close(); return nil })
	n, sess := startNode(t, ctx, h, "n", map[string]string{"slow": slow.URL(), "talky": talky.URL(), "flaky": "http://" + cut.Addr})
	for _, svc := range []string{"slow", "talky", "flaky"} {
		if err := h.WaitCard(ctx, svc, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	buyer := h.BuyerA()

	// (a) and (c): a working task, canceled through replica B (the session is on A).
	for _, c := range []struct {
		svc     string
		agent   *slowagent.Agent
		history string
	}{{"slow", slow, "e2e_cancel_forwarded_tunnel_working"}, {"talky", talky, "e2e_cancel_forwarded_tunnel_talks_after_cancel"}} {
		s, id, att := workingTask(t, ctx, h, c.svc, "cancel_forwarded tunnel "+c.svc)
		if att.TunnelSession != sess || att.ReplicaID != h.Gates[0].ReplicaID {
			t.Fatalf("%s: the attempt %+v is not on the node's session on A", c.svc, att)
		}
		asked := time.Now()
		canceled, err := buyer.Client(t, h.Gates[1], c.svc).CancelTask(ctx, &a2a.CancelTaskRequest{ID: id})
		if err != nil || canceled.Status.State != a2a.TaskStateCanceled {
			t.Fatalf("%s: CancelTask on B: %v %v, want the canceled task", c.svc, canceled, err)
		}
		if d := time.Since(asked); d > cancelBound {
			t.Fatalf("%s: the cancel took %v", c.svc, d)
		}
		checkCancelThroughNode(t, c.agent, att.UpstreamTaskID)
		rest, err := s.ReadAll(ctx)
		if err != nil || len(rest) == 0 || rest[len(rest)-1].State != a2a.TaskStateCanceled {
			t.Fatalf("%s: the caller's stream must end at the canceled row: %d events, %v", c.svc, len(rest), err)
		}
		checkCanceledForGoodOn(t, ctx, h, c.svc, c.agent, id, 3*time.Second)
		if a := endedAttempt(t, ctx, h, id); a.Outcome != "canceled" {
			t.Fatalf("%s: the attempt %+v, want ended canceled", c.svc, a)
		}
		checkHistory(t, h, id, c.history, tunnelForbidden(h)...)
	}
	if n := talky.LateEvents(); n == 0 {
		t.Fatal("talky wrote nothing after its canceled status: the check proved nothing")
	}

	// (b) An input-required task, canceled over HTTP+JSON through replica B.
	res, err := buyer.Client(t, h.Gates[0], "").SendMessage(ctx, newMessage(slowagent.AskPrefix+" cancel_forwarded tunnel"))
	if err != nil {
		t.Fatal(err)
	}
	waiting, ok := res.(*a2a.Task)
	if !ok || waiting.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("SendMessage answered %#v, want an input-required task", res)
	}
	att := attemptsOf(t, ctx, h, waiting.ID, 10*time.Second, "the ask task's attempt", func(as []Attempt) bool { return len(as) == 1 })[0]
	rr, err := buyer.REST(ctx, h.Gates[1], "", http.MethodPost, "/tasks/"+string(waiting.ID)+":cancel", map[string]any{})
	if err != nil || rr.HTTPStatus != http.StatusOK || !strings.Contains(string(rr.Body), "TASK_STATE_CANCELED") {
		t.Fatalf("POST tasks/{id}:cancel: %v HTTP %d %s, want the canceled task", err, rr.HTTPStatus, rr.Body)
	}
	if calls := slow.Calls("CancelTask"); len(calls) != 2 || calls[1].TaskID != att.UpstreamTaskID {
		t.Fatalf("slow's CancelTask requests %+v: want a second one, for %s", calls, att.UpstreamTaskID)
	}
	checkCanceledForGoodOn(t, ctx, h, "slow", slow, waiting.ID, 2*time.Second)
	if a := endedAttempt(t, ctx, h, waiting.ID); a.Outcome != "interrupted" && a.Outcome != "canceled" {
		t.Fatalf("the attempt of the canceled input-required task %+v, want ended interrupted or canceled (§8, D77)", a)
	}
	checkHistory(t, h, waiting.ID, "e2e_cancel_forwarded_tunnel_input_required", tunnelForbidden(h)...)

	// (d) The node is up and its seller unreachable: the cancel is not delivered and the task continues.
	res, err = buyer.Client(t, h.Gates[1], "flaky").SendMessage(ctx, newMessage(slowagent.AskPrefix+" cancel, seller down"))
	if err != nil {
		t.Fatal(err)
	}
	down, ok := res.(*a2a.Task)
	if !ok || down.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("SendMessage to flaky answered %#v, want an input-required task", res)
	}
	cut.Cut()
	r, err := buyer.RPC(ctx, h.Gates[1], "flaky", "CancelTask", &a2a.CancelTaskRequest{ID: down.ID})
	if err != nil || r.Error == nil || r.Error.Code != -32002 || !strings.Contains(r.Error.Message, "could not be reached") {
		t.Fatalf("the cancel with the seller unreachable: %v %s, want TaskNotCancelableError (-32002), the service could not be reached", err, r.Body)
	}
	if st, failure, err := h.TaskFailure(ctx, down.ID); err != nil || st != "input-required" || failure != "" {
		t.Fatalf("the task after a cancel not delivered: %s %q %v, want input-required with no kontra.failure", st, failure, err)
	}
	if as, err := h.Attempts(ctx, down.ID); err != nil || len(as) != 1 || as[0].EndedAt != nil {
		t.Fatalf("the attempt after a cancel not delivered %+v %v: want it open", as, err)
	}
	if n := flaky.Count("CancelTask"); n != 0 {
		t.Fatalf("flaky received %d CancelTask through a cut connection", n)
	}
	cut.Restore()
	_, done := followUp(t, ctx, h, h.Gates[0], "flaky", down, "still here")
	if done.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the follow-up after the seller came back: %s, want completed", done.Status.State)
	}
	checkHistory(t, h, down.ID, "e2e_cancel_tunnel_not_delivered", tunnelForbidden(h)...)

	// The gate never opened a connection to the node, and the node dials only the gate and its targets.
	socks := checkNoListener(t, n.Pid(), "kontra connect")
	peers := []string{h.Front.Addr, strings.TrimPrefix(slow.URL(), "http://"), strings.TrimPrefix(talky.URL(), "http://"), cut.Addr}
	for _, s := range socks {
		if !slices.Contains(peers, s.Remote) {
			t.Fatalf("kontra connect holds a TCP socket to %s, which is neither the gate (the Front) nor a --serve target: %+v", s.Remote, socks)
		}
	}
	t.Logf("kontra connect's TCP sockets: %+v", socks)

	// (e) The node is gone: the cancel of a waiting task fails it tunnel_lost.
	res, err = buyer.Client(t, h.Gates[0], "").SendMessage(ctx, newMessage(slowagent.AskPrefix+" cancel, session gone"))
	if err != nil {
		t.Fatal(err)
	}
	gone, ok := res.(*a2a.Task)
	if !ok || gone.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("SendMessage answered %#v, want an input-required task", res)
	}
	if err := n.Kill(); err != nil {
		t.Fatal(err)
	}
	waitRowGone(t, ctx, h, sess, 5*time.Second, "the node was SIGKILLed")
	cancels := slow.Count("CancelTask")
	got, err := buyer.Client(t, h.Gates[1], "").CancelTask(ctx, &a2a.CancelTaskRequest{ID: gone.ID})
	if err != nil || got.Status.State != a2a.TaskStateFailed || got.Metadata["kontra.failure"] != "tunnel_lost" {
		t.Fatalf("the cancel with the session gone: %+v %v, want the task failed: tunnel_lost", got, err)
	}
	if errors.Is(err, a2a.ErrTaskNotCancelable) || slow.Count("CancelTask") != cancels {
		t.Fatal("the cancel with the session gone reached the seller")
	}
	if a := endedAttempt(t, ctx, h, gone.ID); a.Outcome != "tunnel_lost" {
		t.Fatalf("the attempt %+v, want ended tunnel_lost", a)
	}
	checkHistory(t, h, gone.ID, "e2e_cancel_tunnel_session_gone", tunnelForbidden(h)...)
}
