package gate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"iter"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"
	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/tunnel"
)

// agentCard is the card of the node's agent: JSON-RPC at /rpc of its target (spec/tunnel.md §8.2: the node
// sends every request to its --serve target, so only the path counts).
const agentCard = `{"name":"echo","description":"echoes","version":"1.0.0",
"supportedInterfaces":[{"url":"http://127.0.0.1:9999/rpc","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
"capabilities":{"streaming":true},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[]}`

// agentSeller is the node's agent, an a2a-go v2.7.0 seller: "hi" completes with Hello World; "ask" asks for
// input and completes with the answer; "slow" works until the test releases it (or a cancel).
type agentSeller struct {
	mu      sync.Mutex
	release map[string]chan struct{}
	cancels int
}

func (s *agentSeller) gate(text string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.release == nil {
		s.release = map[string]chan struct{}{}
	}
	if s.release[text] == nil {
		s.release[text] = make(chan struct{})
	}
	return s.release[text]
}

func (s *agentSeller) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		text := ec.Message.Parts[0].Text()
		if ec.StoredTask != nil { // a follow-up of "ask"
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("done: "+text))), nil)
			return
		}
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		switch text {
		case "hi":
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("Hello World"))), nil)
		case "ask":
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("which one?"))), nil)
		default:
			if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
				return
			}
			select {
			case <-s.gate(text):
			case <-ctx.Done():
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
		}
	}
}

func (s *agentSeller) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	s.mu.Lock()
	s.cancels++
	s.mu.Unlock()
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// agentNode is a node dialed into a replica (node.Dial, node.Handshake, yamux): each stream the gate opens
// after the control stream carries one HTTP/1.1 exchange, served by the agent's card and JSON-RPC handler.
// It records every exchange's preamble and JSON-RPC method.
type agentNode struct {
	id      *nodekey.Identity
	mux     *yamux.Session
	session uuid.UUID
	handler http.Handler

	mu        sync.Mutex
	preambles []tunnel.Preamble
	methods   []string
	controls  chan tunnel.Control
}

func dialAgentNode(t *testing.T, base string, id *nodekey.Identity, tokens []string, a *agentSeller) *agentNode {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d, err := node.Dialer{UserAgent: node.UserAgent("test")}.Dial(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	h := &tunnel.Hello{Name: "agent-node", Agent: node.UserAgent("test"), Enroll: tokens,
		Services: []tunnel.Service{{Kind: tunnel.KindA2A, Name: "echo"}}}
	w, err := node.Handshake(ctx, d, id, h, false)
	if err != nil {
		_ = d.WS.CloseNow()
		t.Fatal(err)
	}
	m, err := tunnel.ClientSession(context.Background(), d.WS, nil)
	if err != nil {
		t.Fatal(err)
	}
	rpc := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(a))
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, agentCard)
	})
	n := &agentNode{id: id, mux: m, session: uuid.MustParse(w.Session), controls: make(chan tunnel.Control, 64)}
	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &call)
		n.mu.Lock()
		n.methods = append(n.methods, call.Method)
		n.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		rpc.ServeHTTP(w, r)
	})
	n.handler = mux
	t.Cleanup(func() { _ = m.Close() })
	go n.accept()
	return n
}

func (n *agentNode) accept() {
	first := true
	for {
		st, err := n.mux.AcceptStream()
		if err != nil {
			return
		}
		br := bufio.NewReader(st)
		if first {
			first = false
			go func() {
				if p, err := tunnel.ReadPreamble(br); err != nil || p.Op != tunnel.OpControl {
					_ = st.Close()
					return
				}
				for {
					c, err := tunnel.ReadControl(br)
					if err != nil {
						return
					}
					n.controls <- c
				}
			}()
			continue
		}
		go func() {
			p, err := tunnel.ReadPreamble(br)
			if err != nil {
				_ = st.Close()
				return
			}
			n.mu.Lock()
			n.preambles = append(n.preambles, p)
			n.mu.Unlock()
			serveStream(&streamReader{Conn: st, r: br}, n.handler)
		}()
	}
}

// exchanges is the preambles of the exchanges that named a task.
func (n *agentNode) exchanges() []tunnel.Preamble {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []tunnel.Preamble
	for _, p := range n.preambles {
		if p.Task != "" {
			out = append(out, p)
		}
	}
	return out
}

func (n *agentNode) called(method string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, m := range n.methods {
		if m == method {
			c++
		}
	}
	return c
}

type streamReader struct {
	net.Conn
	r *bufio.Reader
}

func (s *streamReader) Read(p []byte) (int, error) { return s.r.Read(p) }

// serveStream serves the one HTTP/1.1 exchange on c with h.
func serveStream(c net.Conn, h http.Handler) {
	l := &streamListener{ch: make(chan net.Conn, 1), done: make(chan struct{})}
	l.ch <- &streamConn{Conn: c, l: l}
	_ = (&http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}).Serve(l)
}

type streamListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func (l *streamListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *streamListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *streamListener) Addr() net.Addr { return &net.TCPAddr{} }

type streamConn struct {
	net.Conn
	l *streamListener
}

func (c *streamConn) Close() error { _ = c.l.Close(); return c.Conn.Close() }

// echoClient is a2a-go's client on a replica's echo service, resolved from its card at {svc}.
func echoClient(t *testing.T, base, key string) *a2aclient.Client {
	t.Helper()
	hc := &http.Client{Transport: bearer{key}}
	card, err := agentcard.NewResolver(hc).Resolve(context.Background(), base+"/agents/echo")
	if err != nil {
		t.Fatalf("resolve the card at {svc}: %v", err)
	}
	c, err := a2aclient.NewFromCard(context.Background(), card, a2aclient.WithJSONRPCTransport(hc))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Destroy() })
	return c
}

func sendMessage(t *testing.T, c *a2aclient.Client, m *a2a.Message, returnImmediately bool) *a2a.Task {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: m, Config: &a2a.SendMessageConfig{ReturnImmediately: returnImmediately}})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("SendMessage answered %T", res)
	}
	return task
}

func text(s string) *a2a.Message { return a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(s)) }

type attemptRow struct {
	backend, keyID, replica, outcome string
	session                          *uuid.UUID
	firstID                          bool
}

func attemptOf(t *testing.T, e *gateEnv, id a2a.TaskID, n int) attemptRow {
	t.Helper()
	var a attemptRow
	var key, replica, outcome *string
	if err := e.pool.QueryRow(context.Background(), `select backend, tunnel_session, key_id, replica_id, outcome, first_upstream_id_at is not null
		from kontra.attempts where task_id = $1 and n = $2`, string(id), n).Scan(&a.backend, &a.session, &key, &replica, &outcome, &a.firstID); err != nil {
		t.Fatalf("attempt %d of %s: %v", n, id, err)
	}
	if key != nil {
		a.keyID = *key
	}
	if replica != nil {
		a.replica = *replica
	}
	if outcome != nil {
		a.outcome = *outcome
	}
	return a
}

func taskState(t *testing.T, e *gateEnv, id a2a.TaskID) (string, string) {
	t.Helper()
	var st string
	var failure *string
	if err := e.pool.QueryRow(context.Background(), `select state, snapshot->'metadata'->>'kontra.failure' from kontra.tasks where id = $1`,
		string(id)).Scan(&st, &failure); err != nil {
		t.Fatal(err)
	}
	if failure == nil {
		return st, ""
	}
	return st, *failure
}

// Dispatch over the tunnel on running replicas (spec/relay.md §6.1–§6.4, §7, §8, §14.2, §14.3;
// spec/tunnel.md §8): a node holds its session on replica B; a task sent to replica A is placed on that
// session by prepare (attempts: backend tunnel, tunnel_session, key_id, replica_id B) and dispatched on
// kontra-tunnel-B, which only B polls; each exchange's preamble names the gate task and context. A
// follow-up at input-required goes to the same session. CancelTask is forwarded inside the tunnel by
// kontra-ctl-B. Revoking the key's binding (key_scope): the next follow-up fails tunnel_lost without a
// stream reaching the node, and a cancel of a task on that session fails it tunnel_lost (spec §8's row
// "exists, session gone"). With no candidate session a task's attempts are no_session and, retries spent,
// it fails no_session; a node connecting during the backoff lets the next attempt run. B's drain keeps
// kontra-tunnel-B polling until the tunnel dispatch in flight ends.
func TestDispatchThroughTheTunnel(t *testing.T) {
	e := newGateEnv(t)
	ctx := context.Background()
	const services = `services:
  echo:
    kind: a2a
    backend: { tunnel: {} }
    allow: ["org:default"]
    task: { timeout: 10m, retries: 3 }
`
	timing := fastTiming
	timing.TunnelPing = 500 * time.Millisecond
	a := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), timing)
	b := e.start("b", e.replicaConfig(freePort(t), time.Minute, services), timing)
	team, err := tenant.EnsureTeam(ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	enrollStore := enroll.NewStore(e.pool, enroll.Options{})
	token := func() []string {
		t.Helper()
		tok, err := enrollStore.MintServiceToken(ctx, team, "echo", "test")
		if err != nil {
			t.Fatal(err)
		}
		return []string{tok}
	}
	agent := &agentSeller{}
	n1 := dialAgentNode(t, b.url, newNodeIdentity(t), token(), agent)
	eventually(t, 15*time.Second, "the node's tunnels row on B", func() bool {
		return scalar[int](t, e.pool, `select count(*) from kontra.tunnels where session_id = $1 and replica_id = $2`, n1.session, b.r.ID()) == 1
	})
	key := e.mintKey("buyer")
	eventually(t, 15*time.Second, "A serves the card", func() bool {
		status, _ := cardRoute(t, a.url, key, "echo")
		return status == http.StatusOK
	})
	c := echoClient(t, a.url, key)

	// A task sent to A runs on B's session.
	hi := sendMessage(t, c, text("hi"), false)
	if hi.Status.State != a2a.TaskStateCompleted || hi.Status.Message == nil || hi.Status.Message.Parts[0].Text() != "Hello World" {
		t.Fatalf("hi: %+v", hi.Status)
	}
	at := attemptOf(t, e, hi.ID, 1)
	if at.backend != "tunnel" || at.session == nil || *at.session != n1.session || at.keyID != n1.id.KeyID() || at.replica != b.r.ID() ||
		at.outcome != "completed" || !at.firstID {
		t.Fatalf("attempt %+v, want the session %s on replica %s", at, n1.session, b.r.ID())
	}
	ex := n1.exchanges()
	if len(ex) == 0 {
		t.Fatal("no exchange named the task")
	}
	for _, p := range ex {
		if p.Task != string(hi.ID) || p.Context != hi.ContextID || p.Service != "echo" || p.Kind != tunnel.KindA2A {
			t.Fatalf("an exchange's preamble %+v, want task %s context %s", p, hi.ID, hi.ContextID)
		}
	}

	// A follow-up at input-required: the same attempt, the same session.
	ask := sendMessage(t, c, text("ask"), false)
	if ask.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("ask: %+v", ask.Status)
	}
	fu := text("the blue one")
	fu.TaskID, fu.ContextID = ask.ID, ask.ContextID
	answered := sendMessage(t, c, fu, false)
	if answered.Status.State != a2a.TaskStateCompleted || answered.Status.Message.Parts[0].Text() != "done: the blue one" {
		t.Fatalf("follow-up: %+v", answered.Status)
	}
	if at := attemptOf(t, e, ask.ID, 1); at.session == nil || *at.session != n1.session || at.replica != b.r.ID() || at.outcome != "completed" {
		t.Fatalf("the follow-up's attempt %+v", at)
	}

	// Cancel: forwarded inside the tunnel.
	slow := sendMessage(t, c, text("slow"), true)
	eventually(t, 20*time.Second, "the slow task's first upstream id", func() bool {
		return scalar[int](t, e.pool, `select count(*) from kontra.attempts where task_id = $1 and first_upstream_id_at is not null`, string(slow.ID)) == 1
	})
	canceled, err := c.CancelTask(ctx, &a2a.CancelTaskRequest{ID: slow.ID})
	if err != nil || canceled.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("cancel: %+v %v", canceled, err)
	}
	if n1.called("CancelTask") != 1 {
		t.Fatalf("%d CancelTask exchanges reached the node", n1.called("CancelTask"))
	}

	// key_scope (relay.md §6.1, §16): two tasks wait at input-required; the binding is revoked.
	ks := sendMessage(t, c, text("ask"), false)
	gone := sendMessage(t, c, text("ask"), false)
	if ks.Status.State != a2a.TaskStateInputRequired || gone.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("ask: %s %s", ks.Status.State, gone.Status.State)
	}
	if err := enrollStore.RevokeBinding(ctx, n1.id.KeyID(), "echo", "test"); err != nil {
		t.Fatal(err)
	}
	for {
		var ctl tunnel.Control
		select {
		case ctl = <-n1.controls:
		case <-time.After(10 * time.Second):
			t.Fatal("the node was not told of the binding's revocation")
		}
		if ctl == (tunnel.Control{T: tunnel.CtlRevoked, Service: "echo"}) {
			break
		}
	}
	before := len(n1.exchanges())
	fu = text("too late")
	fu.TaskID, fu.ContextID = ks.ID, ks.ContextID
	lost := sendMessage(t, c, fu, false)
	if lost.Status.State != a2a.TaskStateFailed || lost.Metadata["kontra.failure"] != "tunnel_lost" {
		t.Fatalf("a follow-up after the binding's revocation: %+v %v", lost.Status, lost.Metadata)
	}
	if after := len(n1.exchanges()); after != before {
		t.Fatalf("%d streams reached the node after the binding's revocation", after-before)
	}
	if at := attemptOf(t, e, ks.ID, 1); at.outcome != "tunnel_lost" {
		t.Fatalf("attempt %+v", at)
	}
	// The cancel of a task on that session: nothing can be forwarded; it fails tunnel_lost.
	got, err := c.CancelTask(ctx, &a2a.CancelTaskRequest{ID: gone.ID})
	if err != nil || got.Status.State != a2a.TaskStateFailed || got.Metadata["kontra.failure"] != "tunnel_lost" {
		t.Fatalf("cancel with the session gone: %+v %v", got, err)
	}
	if st, failure := taskState(t, e, gone.ID); st != "failed" || failure != "tunnel_lost" {
		t.Fatalf("the canceled task is %s/%s", st, failure)
	}
	if after := len(n1.exchanges()); after != before {
		t.Fatalf("%d streams reached the node for the cancel", after-before)
	}

	// No candidate session: with retries 3, four no_session attempts, then failed: no_session.
	none := sendMessage(t, c, text("hi"), false)
	if none.Status.State != a2a.TaskStateFailed || none.Metadata["kontra.failure"] != "no_session" {
		t.Fatalf("no candidate: %+v %v", none.Status, none.Metadata)
	}
	for n := 1; n <= 4; n++ {
		if at := attemptOf(t, e, none.ID, n); at.backend != "tunnel" || at.outcome != "no_session" || at.session != nil {
			t.Fatalf("attempt %d %+v", n, at)
		}
	}
	// A node connecting during the backoff takes the next attempt.
	late := sendMessage(t, c, text("slow"), true)
	eventually(t, 10*time.Second, "the first no_session attempt", func() bool {
		return scalar[int](t, e.pool, `select count(*) from kontra.attempts where task_id = $1 and outcome = 'no_session'`, string(late.ID)) >= 1
	})
	n2 := dialAgentNode(t, b.url, newNodeIdentity(t), token(), agent)
	eventually(t, 30*time.Second, "the late task placed on the new session", func() bool {
		return scalar[int](t, e.pool, `select count(*) from kontra.attempts where task_id = $1 and tunnel_session = $2`, string(late.ID), n2.session) == 1
	})
	eventually(t, 20*time.Second, "the late task's first upstream id", func() bool {
		return scalar[int](t, e.pool, `select count(*) from kontra.attempts where task_id = $1 and first_upstream_id_at is not null`, string(late.ID)) == 1
	})

	// B drains while that dispatch runs on its session: kontra-tunnel-B keeps polling and the dispatch
	// finishes; then B stops.
	b.drain()
	eventually(t, 10*time.Second, "B draining", func() bool {
		return scalar[bool](t, e.pool, `select draining_at is not null from kontra.gate_replicas where replica_id = $1`, b.r.ID())
	})
	time.Sleep(time.Second)
	close(agent.gate("slow"))
	eventually(t, 30*time.Second, "the late task completed", func() bool {
		st, _ := taskState(t, e, late.ID)
		return st == "completed"
	})
	if err := b.wait(t, 60*time.Second); err != nil {
		t.Fatalf("B's Run after the drain: %v", err)
	}
}
