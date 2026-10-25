package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	enums "go.temporal.io/api/enums/v1"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/test/e2e/slowagent"
)

// delivery is one POST a webhook received.
type delivery struct {
	path   string
	header http.Header
	body   []byte
}

// webhook is a push receiver on a fixed loopback address that can be down (nothing listens: connection
// refused) and come back on the same address. It counts every connection it accepts.
type webhook struct {
	addr   string
	answer func(w http.ResponseWriter, r *http.Request) // default 204

	mu    sync.Mutex
	srv   *http.Server
	got   []delivery
	conns int
}

// newWebhook reserves a loopback address; the webhook is down until start.
func newWebhook(t *testing.T, host string) *webhook {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	w := &webhook{addr: l.Addr().String()}
	l.Close()
	t.Cleanup(w.stop)
	return w
}

func (w *webhook) start(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp", w.addr)
	if err != nil {
		t.Fatalf("webhook back on %s: %v", w.addr, err)
	}
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				w.mu.Lock()
				w.conns++
				w.mu.Unlock()
			}
		},
		Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.mu.Lock()
			w.got = append(w.got, delivery{path: r.URL.Path, header: r.Header.Clone(), body: body})
			answer := w.answer
			w.mu.Unlock()
			if answer != nil {
				answer(rw, r)
				return
			}
			rw.WriteHeader(http.StatusNoContent)
		}),
	}
	w.mu.Lock()
	w.srv = srv
	w.mu.Unlock()
	go func() { _ = srv.Serve(l) }()
}

func (w *webhook) stop() {
	w.mu.Lock()
	srv := w.srv
	w.srv = nil
	w.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

func (w *webhook) url(path string) string { return "http://" + w.addr + path }

func (w *webhook) deliveries(path string) []delivery {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []delivery
	for _, d := range w.got {
		if d.path == path {
			out = append(out, d)
		}
	}
	return out
}

func (w *webhook) connections() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conns
}

// deliveredSeq reads push_configs.delivered_seq and whether the config failed.
func deliveredSeq(t *testing.T, ctx context.Context, h *Harness, taskID a2a.TaskID, configID string) (int64, bool) {
	t.Helper()
	var (
		seq    int64
		failed bool
	)
	if err := h.Pool.QueryRow(ctx, `select delivered_seq, failed_at is not null from kontra.push_configs where task_id = $1 and id = $2`,
		string(taskID), configID).Scan(&seq, &failed); err != nil {
		t.Fatalf("push config %s: %v", configID, err)
	}
	return seq, failed
}

// checkDeliveries checks one config's deliveries (spec §10.2): each is the task snapshot at a seq of the
// log, in seq order (a repeat is allowed: delivery is at least once), from the config's first seq to the
// terminal snapshot, which comes last; every row from `from` to the terminal one is delivered, except an
// artifact row coalesced into a later artifact row of the same run.
func checkDeliveries(t *testing.T, ctx context.Context, h *Harness, name string, taskID a2a.TaskID, got []delivery, from int64) {
	t.Helper()
	id := uuid.MustParse(string(taskID))
	term, err := h.Log().TerminalSeq(ctx, id)
	if err != nil || term == 0 {
		t.Fatalf("terminal seq %d: %v", term, err)
	}
	rows, err := h.Rows(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	seqOf := map[string]int64{}
	for _, r := range rows {
		snap, err := h.Log().SnapshotAt(ctx, id, r.Seq)
		if err != nil {
			t.Fatal(err)
		}
		seqOf[canonical(t, snap)] = r.Seq
	}
	var seqs []int64
	for _, d := range got {
		var sr a2a.StreamResponse
		if err := json.Unmarshal(d.body, &sr); err != nil {
			t.Fatalf("%s: a delivery that is not {\"task\": …}: %s", name, d.body)
		}
		task, ok := sr.Event.(*a2a.Task)
		if !ok {
			t.Fatalf("%s: a delivery that is not a task snapshot: %s", name, d.body)
		}
		s, ok := seqOf[canonical(t, task)]
		if !ok {
			t.Fatalf("%s: a delivery that is no snapshot of the task's log: %s", name, d.body)
		}
		if len(seqs) > 0 && s < seqs[len(seqs)-1] {
			t.Fatalf("%s: seq order went backwards: %v then %d", name, seqs, s)
		}
		seqs = append(seqs, s)
	}
	if len(seqs) == 0 || seqs[len(seqs)-1] != term {
		t.Fatalf("%s: delivered seqs %v: the terminal snapshot (seq %d) must be delivered, last", name, seqs, term)
	}
	if seqs[0] != from {
		t.Fatalf("%s: the first delivery is seq %d, want %d", name, seqs[0], from)
	}
	delivered := map[int64]bool{}
	for _, s := range seqs {
		delivered[s] = true
	}
	for i, r := range rows {
		if r.Seq < from || r.Seq > term || delivered[r.Seq] {
			continue
		}
		if !(r.Kind == "artifact" && i+1 < len(rows) && rows[i+1].Kind == "artifact") {
			t.Fatalf("%s: seq %d (%s) was never delivered (delivered %v)", name, r.Seq, r.Kind, seqs)
		}
	}
}

// pendingPushes counts the workflow's pending push_webhook activities.
func pendingPushes(ctx context.Context, h *Harness, taskID a2a.TaskID) int {
	d, err := h.TC.DescribeWorkflowExecution(ctx, lifecycle.WorkflowID(taskID), "")
	if err != nil {
		return -1
	}
	n := 0
	for _, pa := range d.PendingActivities {
		if pa.GetActivityType().GetName() == lifecycle.ActPushWebhook {
			n++
		}
	}
	return n
}

// TestPushDelivery is push_delivery (spec/relay.md §10.1, §10.2, §16; D78; PRD §15) through the gate's HTTP
// surface: the webhook is down (nothing listens) for 2 minutes from before the task starts, across its
// terminal event, then back. Three configs: an inline one sent with SendStreamingMessage, one created
// over JSON-RPC after the task started with a caller-chosen id, deleted over HTTP+JSON and created again
// with the same id (a new push_webhook in the same run), and one created over HTTP+JSON. Each gets every
// status change and the terminal snapshot, in seq order, with its token and authentication; the workflow
// outlives the outage (D78); the caller-chosen id never enters Temporal history, only its ConfigRef; and
// no push config, credential or cookie reaches the upstream.
func TestPushDelivery(t *testing.T) {
	const downFor = 2 * time.Minute
	h := start(t, Options{Upstream: slowagent.Config{Chunks: 15, Interval: time.Second, Streaming: true}, PushAllowPrivate: true})
	ctx := ctxFor(t, 10*time.Minute)
	hook := newWebhook(t, "127.0.0.1")
	secret := randomHex(6)
	downAt := time.Now()

	inline := &a2a.PushConfig{URL: hook.url("/inline"), Token: "tok-inline-" + secret,
		Auth: &a2a.PushAuthInfo{Scheme: "Bearer", Credentials: "cred-inline-" + secret}}
	req := newMessage("push_delivery")
	req.Config = &a2a.SendMessageConfig{PushConfig: inline}
	s, err := h.BuyerA().SendStreaming(ctx, h.Gates[0], req, cookieCall)
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, ctx, s, func(ev Event) bool { return ev.State == a2a.TaskStateWorking })
	taskID := s.TaskID()
	rpc, rest := h.BuyerA().Client(t, h.Gates[1], ""), h.BuyerA().ClientREST(t, h.Gates[0], "")

	// A config created after the start, over JSON-RPC, with an id the caller chose.
	callerID := "e2e-caller-config-" + secret
	late := &a2a.PushConfig{TaskID: taskID, ID: callerID, URL: hook.url("/late-rpc"), Token: "tok-rpc-" + secret}
	if _, err := rpc.CreateTaskPushConfig(ctx, late); err != nil {
		t.Fatalf("CreateTaskPushNotificationConfig over JSON-RPC: %v", err)
	}
	eventually(t, 30*time.Second, "push_webhook for the inline and the JSON-RPC config", func() bool { return pendingPushes(ctx, h, taskID) == 2 })
	// Deleted over HTTP+JSON: its push_webhook ends; then created again with the same id.
	if err := rest.DeleteTaskPushConfig(ctx, &a2a.DeleteTaskPushConfigRequest{TaskID: taskID, ID: callerID}); err != nil {
		t.Fatalf("DeleteTaskPushNotificationConfig over HTTP+JSON: %v", err)
	}
	eventually(t, 30*time.Second, "the deleted config's push_webhook ends", func() bool { return pendingPushes(ctx, h, taskID) == 1 })
	if _, err := rpc.CreateTaskPushConfig(ctx, late); err != nil {
		t.Fatalf("CreateTaskPushNotificationConfig again with the same id: %v", err)
	}
	lateFrom, _ := deliveredSeq(t, ctx, h, taskID, callerID)
	// One more over HTTP+JSON, without an id: the gate names it.
	restCfg, err := rest.CreateTaskPushConfig(ctx, &a2a.PushConfig{TaskID: taskID, URL: hook.url("/late-rest"), Token: "tok-rest-" + secret})
	if err != nil {
		t.Fatalf("CreateTaskPushNotificationConfig over HTTP+JSON: %v", err)
	}
	restFrom, _ := deliveredSeq(t, ctx, h, taskID, restCfg.ID)
	eventually(t, 30*time.Second, "a push_webhook per config", func() bool { return pendingPushes(ctx, h, taskID) == 3 })

	// Get and List on both bindings see the three configs; the inline one has the gate's id.
	got, err := rest.GetTaskPushConfig(ctx, &a2a.GetTaskPushConfigRequest{TaskID: taskID, ID: callerID})
	if err != nil || got.URL != late.URL {
		t.Fatalf("GetTaskPushNotificationConfig over HTTP+JSON: %v %v", got, err)
	}
	inlineID := ""
	for _, c := range []interface {
		ListTaskPushConfigs(context.Context, *a2a.ListTaskPushConfigRequest) ([]*a2a.PushConfig, error)
	}{rpc, rest} {
		list, err := c.ListTaskPushConfigs(ctx, &a2a.ListTaskPushConfigRequest{TaskID: taskID})
		if err != nil || len(list) != 3 {
			t.Fatalf("ListTaskPushNotificationConfigs: %d configs, %v; want 3", len(list), err)
		}
		for _, c := range list {
			if c.URL == inline.URL {
				inlineID = c.ID
			}
		}
	}
	if inlineID == "" {
		t.Fatal("the inline config is not listed")
	}

	// The task ends long before the webhook is back; the workflow must still be open (D78).
	evs, err := s.ReadAll(ctx)
	if err != nil || len(evs) == 0 || evs[len(evs)-1].State != a2a.TaskStateCompleted {
		t.Fatalf("the caller's stream must end at completed: %v", err)
	}
	time.Sleep(time.Until(downAt.Add(downFor - 10*time.Second)))
	d, err := h.TC.DescribeWorkflowExecution(ctx, lifecycle.WorkflowID(taskID), "")
	if err != nil {
		t.Fatal(err)
	}
	if st := d.WorkflowExecutionInfo.Status; st != enums.WORKFLOW_EXECUTION_STATUS_RUNNING {
		t.Fatalf("10 s before the webhook is back the workflow is %s: it must outlive its pushes (D78)", st)
	}
	time.Sleep(time.Until(downAt.Add(downFor)))
	hook.start(t)
	t.Logf("webhook back %.0f s after it went down", time.Since(downAt).Seconds())
	if err := h.WaitWorkflowClosed(ctx, taskID, 4*time.Minute); err != nil {
		t.Fatal(err)
	}

	checkDeliveries(t, ctx, h, "the inline config", taskID, hook.deliveries("/inline"), 1)
	checkDeliveries(t, ctx, h, "the JSON-RPC config created again", taskID, hook.deliveries("/late-rpc"), lateFrom+1)
	checkDeliveries(t, ctx, h, "the HTTP+JSON config", taskID, hook.deliveries("/late-rest"), restFrom+1)
	term, _ := h.Log().TerminalSeq(ctx, uuid.MustParse(string(taskID)))
	for _, c := range []struct{ id, path, token, auth string }{
		{inlineID, "/inline", inline.Token, "Bearer " + inline.Auth.Credentials},
		{callerID, "/late-rpc", late.Token, ""},
		{restCfg.ID, "/late-rest", "tok-rest-" + secret, ""},
	} {
		if seq, failed := deliveredSeq(t, ctx, h, taskID, c.id); seq != term || failed {
			t.Fatalf("config %s: delivered_seq %d failed %v, want %d", c.path, seq, failed, term)
		}
		for _, d := range hook.deliveries(c.path) {
			if d.header.Get("A2A-Notification-Token") != c.token || d.header.Get("X-A2A-Notification-Token") != c.token ||
				d.header.Get("Content-Type") != "application/a2a+json" || d.header.Get("Authorization") != c.auth {
				t.Fatalf("a delivery to %s has headers %v", c.path, d.header)
			}
		}
	}

	js := checkHistory(t, h, taskID, "e2e_push_delivery", slowagent.ChunkMarker, h.APIKey, callerID, inline.Token, inline.Auth.Credentials,
		late.Token, "tok-rest-"+secret, hook.addr, "e2e-cookie")
	text := historyText(t, js)
	if ref := lifecycle.ConfigRef(uuid.MustParse(string(taskID)), callerID); !strings.Contains(text, ref) {
		t.Fatalf("the history does not carry the caller's config as its ConfigRef %s", ref)
	}
	// The config deleted and created again took the push-restart branch (a GetVersion marker), which is
	// what this recording lets TestReplayRecordedHistories keep honest.
	if !strings.Contains(text, "push-restart") {
		t.Fatal("the history has no push-restart marker: the deleted config's push_webhook did not end as deleted")
	}
	checkUpstreamHeaders(t, h, h.Upstream)
}

// privateName finds a host name this machine resolves only to non-global addresses (loopback, private,
// link-local), from /etc/hosts, other than localhost: "a name resolving to a private address" of ssrf_webhook.
func privateName(t *testing.T, ctx context.Context) (string, netip.Addr) {
	t.Helper()
	f, err := os.Open("/etc/hosts")
	if err != nil {
		t.Fatalf("ssrf_webhook needs a name resolving to a private address: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(strings.SplitN(sc.Text(), "#", 2)[0])
		if len(fields) < 2 {
			continue
		}
		for _, name := range fields[1:] {
			if name == "localhost" || strings.HasSuffix(name, ".localhost") {
				continue
			}
			addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", name)
			if err != nil || len(addrs) == 0 {
				continue
			}
			ok := true
			for _, a := range addrs {
				a = a.Unmap()
				if !(a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast()) {
					ok = false
				}
			}
			if !ok {
				continue
			}
			a := addrs[0].Unmap()
			if l, err := net.Listen("tcp", net.JoinHostPort(a.String(), "0")); err == nil {
				l.Close()
				return name, a
			}
		}
	}
	t.Fatal("ssrf_webhook: no name in /etc/hosts resolves only to a private address on this machine")
	return "", netip.Addr{}
}

// TestSSRFWebhook is ssrf_webhook (spec/relay.md §10.3, §16; D60, D67). Without push.allow_private, push URLs
// naming 169.254.169.254, 10.0.0.1, localhost, [::ffff:10.0.0.1], 64:ff9b::a00:1 or a name resolving to a
// private address (all https, so the address rule refuses them), and an http:// URL (refused without
// allow_private), are refused when set (InvalidParams) on JSON-RPC, on HTTP+JSON and inline, before any
// connection and before any row. With push.allow_private (loopback webhooks), a redirecting URL is accepted
// when set; its 3xx is a failed delivery, retried, and the redirect target receives no connection.
func TestSSRFWebhook(t *testing.T) {
	t.Run("refused when set", func(t *testing.T) {
		h := start(t, Options{Upstream: slowagent.Config{Chunks: 1, Interval: 10 * time.Millisecond, Streaming: true}})
		ctx := ctxFor(t, 3*time.Minute)
		res, err := h.BuyerA().Client(t, h.Gates[0], "").SendMessage(ctx, newMessage(slowagent.AskPrefix+" ssrf"))
		if err != nil {
			t.Fatal(err)
		}
		task := res.(*a2a.Task)
		lo := newWebhook(t, "127.0.0.1")
		lo.start(t)
		name, addr := privateName(t, ctx)
		priv := newWebhook(t, addr.String())
		priv.start(t)
		_, loPort, _ := net.SplitHostPort(lo.addr)
		_, privPort, _ := net.SplitHostPort(priv.addr)
		urls := []string{
			"https://169.254.169.254/hook",
			"https://10.0.0.1/hook",
			"https://localhost:" + loPort + "/hook",
			"https://[::ffff:10.0.0.1]/hook",
			"https://[64:ff9b::a00:1]/hook",
			"https://" + net.JoinHostPort(name, privPort) + "/hook",
			"http://93.184.215.14/hook", // a public address, but http:// without allow_private
		}
		t.Logf("the private name is %s (%s)", name, addr)
		var tasksBefore int
		if err := h.Pool.QueryRow(ctx, `select count(*) from kontra.tasks`).Scan(&tasksBefore); err != nil {
			t.Fatal(err)
		}
		begin := time.Now()
		rpc, rest := h.BuyerA().Client(t, h.Gates[1], ""), h.BuyerA().ClientREST(t, h.Gates[0], "")
		for _, u := range urls {
			cfg := &a2a.PushConfig{TaskID: task.ID, URL: u, Token: "t"}
			if _, err := rpc.CreateTaskPushConfig(ctx, cfg); !errors.Is(err, a2a.ErrInvalidParams) {
				t.Fatalf("JSON-RPC CreateTaskPushNotificationConfig %s: %v, want InvalidParams", u, err)
			}
			if _, err := rest.CreateTaskPushConfig(ctx, cfg); !errors.Is(err, a2a.ErrInvalidParams) {
				t.Fatalf("HTTP+JSON CreateTaskPushNotificationConfig %s: %v, want InvalidParams", u, err)
			}
			req := newMessage("ssrf inline")
			req.Config = &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{URL: u}}
			if _, err := rpc.SendMessage(ctx, req); !errors.Is(err, a2a.ErrInvalidParams) {
				t.Fatalf("SendMessage with the inline config %s: %v, want InvalidParams", u, err)
			}
		}
		if d := time.Since(begin); d > 30*time.Second {
			t.Fatalf("the refusals took %v: a refused URL is checked, never dialed", d)
		}
		var tasksAfter, configs int
		if err := h.Pool.QueryRow(ctx, `select (select count(*) from kontra.tasks), (select count(*) from kontra.push_configs)`).Scan(&tasksAfter, &configs); err != nil {
			t.Fatal(err)
		}
		if tasksAfter != tasksBefore || configs != 0 {
			t.Fatalf("refused URLs left %d new tasks and %d push configs", tasksAfter-tasksBefore, configs)
		}
		if lo.connections() != 0 || priv.connections() != 0 {
			t.Fatalf("a refused URL was connected to: localhost %d, %s %d", lo.connections(), name, priv.connections())
		}
		if n := h.Upstream.Sends(); n != 1 {
			t.Fatalf("the upstream received %d sends, want only the first task's", n)
		}
		// The control: the same buyer may set an https URL at a global address. The task is ended first, so
		// the config is done when set and nothing is ever sent to that address (spec §10.1-§10.2).
		if _, err := rpc.CancelTask(ctx, &a2a.CancelTaskRequest{ID: task.ID}); err != nil {
			t.Fatal(err)
		}
		global, err := rpc.CreateTaskPushConfig(ctx, &a2a.PushConfig{TaskID: task.ID, URL: "https://93.184.215.14/hook", Token: "t"})
		if err != nil {
			t.Fatalf("an https URL at a global address is refused: %v", err)
		}
		term, _ := h.Log().TerminalSeq(ctx, uuid.MustParse(string(task.ID)))
		if seq, _ := deliveredSeq(t, ctx, h, task.ID, global.ID); seq != term {
			t.Fatalf("a config set on an ended task starts at delivered_seq %d, want the terminal seq %d", seq, term)
		}
	})

	t.Run("a redirect is a failed delivery, never followed", func(t *testing.T) {
		h := start(t, Options{Upstream: slowagent.Config{Chunks: 1, Interval: 10 * time.Millisecond, Streaming: true}, PushAllowPrivate: true})
		ctx := ctxFor(t, 3*time.Minute)
		res, err := h.BuyerA().Client(t, h.Gates[0], "").SendMessage(ctx, newMessage(slowagent.AskPrefix+" ssrf redirect"))
		if err != nil {
			t.Fatal(err)
		}
		task := res.(*a2a.Task)
		target := newWebhook(t, "127.0.0.1")
		target.start(t)
		redirector := newWebhook(t, "127.0.0.1")
		redirector.answer = func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.url("/target"), http.StatusFound)
		}
		redirector.start(t)
		cfg, err := h.BuyerA().Client(t, h.Gates[1], "").CreateTaskPushConfig(ctx, &a2a.PushConfig{TaskID: task.ID, URL: redirector.url("/redirect"), Token: "t"})
		if err != nil {
			t.Fatalf("a redirecting URL is accepted when set (D60): %v", err)
		}
		from, _ := deliveredSeq(t, ctx, h, task.ID, cfg.ID)
		eventually(t, 30*time.Second, "the redirecting webhook retried", func() bool { return len(redirector.deliveries("/redirect")) >= 3 })
		if seq, failed := deliveredSeq(t, ctx, h, task.ID, cfg.ID); seq != from || failed {
			t.Fatalf("after 3xx answers the config is at delivered_seq %d (failed %v), want %d: a 3xx is a failed delivery, retried", seq, failed, from)
		}
		if n := target.connections(); n != 0 {
			t.Fatalf("the redirect target received %d connections, want none", n)
		}
	})
}
