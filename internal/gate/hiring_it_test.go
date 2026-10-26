package gate

import (
	"bytes"
	"context"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/blob/blobtest"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/tenant"
)

// helloExec stands for the README's hello-exec seller (a2a-cli's exec mode answers "Hello World"), an a2a-go
// v2.7.0 agent behind `kontra connect`: "hi" completes with Hello World; a message whose first part is raw bytes
// completes with an artifact holding those bytes again. It records the parts it received.
type helloExec struct {
	mu    sync.Mutex
	parts [][]*a2a.Part
}

func (h *helloExec) Execute(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		h.mu.Lock()
		h.parts = append(h.parts, ec.Message.Parts)
		h.mu.Unlock()
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		if raw := ec.Message.Parts[0].Raw(); raw != nil {
			if !yield(a2a.NewArtifactEvent(ec, &a2a.Part{Content: a2a.Raw(raw), MediaType: "application/octet-stream"}), nil) {
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("Hello World"))), nil)
	}
}

func (h *helloExec) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

func (h *helloExec) received() [][]*a2a.Part {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]*a2a.Part(nil), h.parts...)
}

// serveHelloExec serves the agent on 127.0.0.1: its card (JSON-RPC at /rpc) and its JSON-RPC handler.
func serveHelloExec(t *testing.T, a *helloExec) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, agentCard)
	})
	mux.Handle("/rpc", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(a)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// syncBuf is a goroutine-safe buffer.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runNode is an in-process `kontra connect`, its command line parsed as the CLI parses it, HOME its own.
type runNode struct {
	n      *node.Node
	out    *syncBuf
	logs   *syncBuf
	cancel context.CancelFunc
	done   chan error
	key    string // its identity key file
}

var hiringAt = regexp.MustCompile(`kontra connect: hiring at http://localhost:(\d+) `)

func startNode(t *testing.T, home string, args ...string) *runNode {
	t.Helper()
	env := map[string]string{"HOME": home}
	cfg, err := node.ParseArgs(append(args, "--listen", "127.0.0.1:0"), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	rn := &runNode{out: &syncBuf{}, logs: &syncBuf{}, done: make(chan error, 1), key: cfg.KeyPath}
	cfg.Out, cfg.Version = rn.out, "test"
	rn.n, err = node.New(cfg, testLogTo(rn.logs))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rn.cancel = cancel
	go func() { rn.done <- rn.n.Run(ctx) }()
	t.Cleanup(func() {
		rn.stop(t)
		if t.Failed() {
			t.Logf("node log:\n%s", rn.logs.String())
		}
	})
	return rn
}

func (rn *runNode) stop(t *testing.T) {
	rn.cancel()
	select {
	case err := <-rn.done:
		rn.done <- err
	case <-time.After(20 * time.Second):
		t.Error("kontra connect did not stop")
	}
}

// local is the node's hiring endpoint, once it is up and its session welcomed.
func (rn *runNode) local(t *testing.T) string {
	t.Helper()
	var port string
	eventually(t, 20*time.Second, "the node's session and hiring endpoint", func() bool {
		m := hiringAt.FindStringSubmatch(rn.out.String())
		if m != nil {
			port = m[1]
		}
		return m != nil && strings.Contains(rn.out.String(), "kontra connect: connected to")
	})
	return "http://127.0.0.1:" + port
}

func (rn *runNode) keyID(t *testing.T) string {
	t.Helper()
	id, err := nodekey.Load(rn.key)
	if err != nil {
		t.Fatal(err)
	}
	return id.KeyID()
}

// localClient is a stock a2a-go client resolved from the node's local card ({local}/agents/hello), with no
// credential: the node hires as its session's principal.
func localClient(t *testing.T, local string) (*a2aclient.Client, *a2a.AgentCard) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	card, err := agentcard.DefaultResolver.Resolve(ctx, local+"/agents/hello")
	if err != nil {
		t.Fatalf("resolve the local card: %v", err)
	}
	c, err := a2aclient.NewFromCard(ctx, card)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Destroy() })
	return c, card
}

type taskRow struct {
	principal, via string
	buyer          uuid.UUID
}

func taskOf(t *testing.T, e *gateEnv, id a2a.TaskID) taskRow {
	t.Helper()
	var r taskRow
	if err := e.pool.QueryRow(context.Background(), `select principal, via, buyer_id from kontra.tasks where id = $1`, string(id)).
		Scan(&r.principal, &r.via, &r.buyer); err != nil {
		t.Fatal(err)
	}
	return r
}

// Hiring through a real gate (spec/tunnel.md §8.3, §9, §12; relay.md §2.3, §10.4; D34, D56, D181, D182): stock
// a2a-go clients pointed at a node's 127.0.0.1:<listen>/agents/hello complete tasks to a node-hosted hello-exec.
// The card they resolve lists only localhost URLs. The task's principal is key:<prefix> (and its buyer) when the
// node dialed with --api-key, and node:<key_id> with ids.NodeBuyer otherwise; a hire-only node is admitted on its
// API key. A 300 KiB part reaches the agent as raw bytes through a blob stream, and the agent's 300 KiB answer
// reaches the hiring client as raw bytes the same way. Local refusals open no stream. Revoking the API key a node
// hires with makes its next hiring request 401 within 5 s, tells it auth_revoked, and leaves its hosting working.
func TestHiringThroughTheGate(t *testing.T) {
	e := newGateEnv(t)
	ctx := context.Background()
	bt := blobtest.New(t, "hiring")
	services := `blobs: "` + bt.URL + `"
services:
  hello:
    kind: a2a
    backend: { tunnel: {} }
    allow: ["org:default"]
    task: { timeout: 10m, retries: 3 }
`
	a := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), fastTiming)
	team, err := tenant.EnsureTeam(ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	keys := auth.NewAPIKeys(e.pool, nil)
	defer keys.Wait()
	mint := func(name string) auth.Minted {
		m, err := keys.Mint(ctx, auth.MintRequest{TenantID: team, Name: name, Actor: "test"})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	token, err := enroll.NewStore(e.pool, enroll.Options{}).MintServiceToken(ctx, team, "hello", "test")
	if err != nil {
		t.Fatal(err)
	}

	// Node A hosts hello-exec and hires with its API key.
	agent := &helloExec{}
	ka := mint("node-a")
	nodeA := startNode(t, t.TempDir(), "--gate", a.url, "--name", "node-a", "--serve", "a2a:hello="+serveHelloExec(t, agent),
		"--enroll", token, "--api-key", ka.Secret)
	localA := nodeA.local(t)
	eventually(t, 20*time.Second, "the gate serves hello's card", func() bool {
		status, _ := cardRoute(t, a.url, ka.Secret, "hello")
		return status == http.StatusOK
	})
	ca, card := localClient(t, localA)
	for _, i := range card.SupportedInterfaces {
		if !strings.HasPrefix(i.URL, "http://localhost:"+strings.TrimPrefix(localA, "http://127.0.0.1:")+"/agents/hello") {
			t.Fatalf("the local card lists %q: only localhost URLs may be served", i.URL)
		}
	}
	hi := sendMessage(t, ca, text("hi"), false)
	if hi.Status.State != a2a.TaskStateCompleted || hi.Status.Message.Parts[0].Text() != "Hello World" {
		t.Fatalf("hi through node A: %+v", hi.Status)
	}
	if r := taskOf(t, e, hi.ID); r.principal != ka.Principal() || r.via != "api_key" || r.buyer != ka.BuyerID {
		t.Fatalf("a task hired with --api-key: %+v, want %s", r, ka.Principal())
	}

	// Node B only hires, admitted on its API key; a 300 KiB part goes both ways through the bucket and blob
	// streams.
	kb := mint("node-b")
	homeB := t.TempDir()
	nodeB := startNode(t, homeB, "--gate", a.url, "--name", "node-b", "--api-key", kb.Secret)
	localB := nodeB.local(t)
	if !strings.Contains(nodeB.out.String(), "no service is served on this session") {
		t.Fatalf("node B's welcome: %s", nodeB.out.String())
	}
	cb, _ := localClient(t, localB)
	big := bytes.Repeat([]byte{0x00, 0xff, 'k', 'b'}, 300<<10/4)
	bigTask := sendMessage(t, cb, a2a.NewMessage(a2a.MessageRoleUser, &a2a.Part{Content: a2a.Raw(big), MediaType: "application/octet-stream"}), false)
	if bigTask.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the big part's task: %+v", bigTask.Status)
	}
	got := agent.received()
	if p := got[len(got)-1][0]; !bytes.Equal(p.Raw(), big) {
		t.Fatalf("the agent received %d raw bytes (url %q), want the 300 KiB part", len(p.Raw()), p.URL())
	}
	art := bigTask.Artifacts
	if len(art) != 1 || !bytes.Equal(art[0].Parts[0].Raw(), big) {
		t.Fatalf("the hiring client received %+v, want the agent's 300 KiB artifact as raw bytes", art)
	}
	if r := taskOf(t, e, bigTask.ID); r.principal != kb.Principal() || r.buyer != kb.BuyerID {
		t.Fatalf("the hire-only node's task: %+v", r)
	}
	if objs := bt.Keys(t); len(objs) < 2 {
		t.Fatalf("the bucket holds %v: both 300 KiB parts went through it", objs)
	}
	if n := scalar[int](t, e.pool, `select count(*) from kontra.blobs where task_id = $1`, string(bigTask.ID)); n < 2 {
		t.Fatalf("%d blobs rows for the task", n)
	}
	if n := scalar[int](t, e.pool, `select count(*) from kontra.key_services ks join kontra.tunnel_keys k using (key_id)
		where k.key_id = $1`, nodeB.keyID(t)); n != 0 {
		t.Fatalf("the hire-only node has %d bindings", n)
	}

	// The same key without its API key (a fresh HOME, the key file kept): node:<key_id> with ids.NodeBuyer.
	nodeB.stop(t)
	nodeB2 := startNode(t, t.TempDir(), "--gate", a.url, "--name", "node-b", "--key", nodeB.key)
	localB2 := nodeB2.local(t)
	cb2, _ := localClient(t, localB2)
	asNode := sendMessage(t, cb2, text("hi"), false)
	kidB := nodeB.keyID(t)
	wantBuyer, _ := ids.NodeBuyer(ids.Namespace, kidB)
	if r := taskOf(t, e, asNode.ID); r.principal != "node:"+kidB || r.via != "node" || r.buyer != wantBuyer {
		t.Fatalf("a task hired without --api-key: %+v, want node:%s and buyer %s", r, kidB, wantBuyer)
	}

	// Local refusals open no stream: the gate's count of node B's node-opened streams does not change.
	sessB := scalar[uuid.UUID](t, e.pool, `select session_id from kontra.tunnels where key_id = $1`, kidB)
	sess, ok := a.r.Tunnels().Get(sessB)
	if !ok {
		t.Fatal("node B's session is not on the replica")
	}
	before := sess.NodeStreamsAccepted()
	port := strings.TrimPrefix(localB2, "http://127.0.0.1:")
	for _, tc := range []struct {
		method, path, host, origin string
		want                       int
	}{
		{http.MethodGet, "/agents/hello", "evil.example:" + port, "", http.StatusForbidden},
		{http.MethodPost, "/mcp/fs", "", "http://evil.example", http.StatusForbidden},
		{http.MethodGet, "/.well-known/jwks.json", "evil.example:" + port, "", http.StatusForbidden},
		{http.MethodOptions, "/agents/hello", "", "", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/tunnel", "", "", http.StatusNotFound},
	} {
		req, _ := http.NewRequest(tc.method, localB2+tc.path, nil)
		if tc.host != "" {
			req.Host = tc.host
		}
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Fatalf("%s %s Host %q Origin %q: %d, want %d", tc.method, tc.path, tc.host, tc.origin, res.StatusCode, tc.want)
		}
	}
	if after := sess.NodeStreamsAccepted(); after != before {
		t.Fatalf("local refusals opened %d node-opened streams", after-before)
	}

	// auth_revoked: node A's API key is revoked; its next hiring request is 401 within 5 s, it is told so, and
	// its hosting goes on.
	start := time.Now()
	if _, err := keys.Revoke(ctx, ka.ID, "test"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "node A's hiring answered 401", func() bool {
		res, err := http.Get(localA + "/agents/hello")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == http.StatusUnauthorized
	})
	t.Logf("the revocation reached node A's hiring after %v", time.Since(start).Round(time.Millisecond))
	eventually(t, 5*time.Second, "node A told auth_revoked", func() bool {
		return strings.Contains(nodeA.logs.String(), "the gate revoked the session's hiring API key")
	})
	if _, err := ca.SendMessage(ctx, &a2a.SendMessageRequest{Message: text("hi")}); err == nil {
		t.Fatal("node A hired after its API key was revoked")
	}
	after := sendMessage(t, cb2, text("hi"), false)
	if after.Status.State != a2a.TaskStateCompleted || after.Status.Message.Parts[0].Text() != "Hello World" {
		t.Fatalf("hosting on node A's session after auth_revoked: %+v", after.Status)
	}
	if st := attemptOf(t, e, after.ID, 1); st.keyID != nodeA.keyID(t) {
		t.Fatalf("the task ran on %s, not node A", st.keyID)
	}
}

// testLogTo is a debug logger writing to w.
func testLogTo(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
