package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"iter"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnelgate"
	"kontra.sh/kontra/internal/upstream"
)

// echoSeller is an A2A 1.0 agent (JSON-RPC at /, HTTP+JSON at /rest) that echoes its message's text parts in its
// status message and in an artifact, streamed in chunks on SendStreamingMessage; it records what it received.
type echoSeller struct {
	mu   sync.Mutex
	seen []string
}

func (e *echoSeller) executor() a2asrv.AgentExecutor {
	return a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			var texts []string
			for _, p := range ec.Message.Parts {
				texts = append(texts, p.Text())
			}
			text := strings.Join(texts, " ")
			e.mu.Lock()
			e.seen = append(e.seen, text)
			e.mu.Unlock()
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			for i, chunk := range []string{"echo ", "of ", text} {
				ev := a2a.NewArtifactUpdateEvent(ec, "out", a2a.NewTextPart(chunk))
				ev.Append = i > 0
				if !yield(ev, nil) {
					return
				}
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("done: "+text))), nil)
		}
	})
}

// sealingGate is the gate of the node tests for sealing: it serves the service's signed card (the sealed extension
// naming recipients) and the gate's key set, and relays the node's hiring requests to the same node's hosting side as
// the relay does (a gate task id for a new task, the first message without contextId, the upstream's ids rewritten
// to the gate's). Everything that crosses it is recorded: what the gate would hold in task_events.
type sealingGate struct {
	t          *testing.T
	keys       *jws.KeySet
	url        string // the gate's origin, the jku's
	recipients func() []string
	session    func() *tunnelgate.Session

	mu    sync.Mutex
	held  bytes.Buffer
	tasks map[string][2]string // gate task -> upstream task, upstream context
}

func (g *sealingGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == jws.JWKSPath:
		b, _ := g.keys.JWKS()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	case r.URL.Path == "/agents/echo/.well-known/agent-card.json":
		ext, _ := json.Marshal(map[string]any{"uri": sealed.URI, "required": false,
			"params": map[string]any{"recipients": g.recipients(), "envelope": sealed.MediaType, "required": true}})
		card, _ := json.Marshal(map[string]any{"name": "echo", "protocolVersion": "1.0",
			"supportedInterfaces": []any{map[string]any{"url": g.url + "/agents/echo", "protocolBinding": "JSONRPC", "protocolVersion": "1.0"}},
			"capabilities":        map[string]any{"streaming": true, "extensions": []json.RawMessage{ext}}})
		sig, err := g.keys.Sign(card, g.url+jws.JWKSPath, &jws.Upstream{SHA256: strings.Repeat("0", 64), URL: g.url + "/x"})
		if err != nil {
			g.t.Error(err)
			return
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(card, &m)
		m["signatures"], _ = json.Marshal([]jws.Signature{sig})
		b, _ := json.Marshal(m)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	default:
		g.relay(w, r)
	}
}

// relay forwards one hiring request to the node's hosted echo service through the session's hosting stream.
func (g *sealingGate) relay(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	g.held.Write(body)
	g.mu.Unlock()
	rest := !strings.HasSuffix(r.URL.Path, "/agents/echo")
	var top map[string]json.RawMessage
	_ = json.Unmarshal(body, &top)
	var params map[string]json.RawMessage
	if rest {
		params = top
	} else {
		_ = json.Unmarshal(top["params"], &params)
	}
	var msg map[string]any
	_ = json.Unmarshal(params["message"], &msg)
	task, _ := msg["taskId"].(string)
	ctx, _ := msg["contextId"].(string)
	if task == "" {
		task = uuid.NewString()
		delete(msg, "contextId") // the first task of a gate context goes upstream without one (relay.md §4)
	} else {
		g.mu.Lock()
		up := g.tasks[task]
		g.mu.Unlock()
		msg["taskId"], msg["contextId"] = up[0], up[1]
	}
	params["message"], _ = json.Marshal(msg)
	if rest {
		body, _ = json.Marshal(params)
	} else {
		top["params"], _ = json.Marshal(params)
		body, _ = json.Marshal(top)
	}
	s := g.session()
	opener := preambleOpener{s: s, pre: tunnel.Preamble{Op: tunnel.OpHTTP, Service: "echo", Kind: tunnel.KindA2A, Task: task, Context: ctx}}
	tb, err := upstream.NewTunnel("echo", tunnel.KindA2A, opener)
	if err != nil {
		g.t.Error(err)
		return
	}
	path := "/"
	if rest {
		path = "/rest" + strings.TrimPrefix(r.URL.Path, "/agents/echo")
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream.Endpoint(tb, upstream.Info{Path: path}), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	resp, err := tb.HTTPClient().Do(req)
	if err != nil {
		g.t.Errorf("the hosting exchange: %v", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, k := range []string{"Content-Type", tunnel.HeaderNode, tunnel.HeaderSealedReject} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			out := g.rewrite(task, ctx, line)
			g.mu.Lock()
			g.held.WriteString(out)
			g.mu.Unlock()
			_, _ = io.WriteString(w, out)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// rewrite replaces the upstream's task and context ids in one line of an answer with the gate's (relay.md §4).
func (g *sealingGate) rewrite(task, ctx, line string) string {
	data := strings.TrimPrefix(strings.TrimSpace(line), "data: ")
	var probe struct {
		Result json.RawMessage `json:"result"`
		Task   *struct {
			ID, ContextID string
		} `json:"task"`
	}
	_ = json.Unmarshal([]byte(data), &probe)
	src := []byte(data)
	if probe.Result != nil {
		src = probe.Result
	}
	var ids struct {
		ID, TaskID, ContextID string
		Task                  *struct{ ID, ContextID string }
		StatusUpdate          *struct{ TaskID, ContextID string }
		ArtifactUpdate        *struct{ TaskID, ContextID string }
	}
	_ = json.Unmarshal(src, &ids)
	up, upCtx := ids.ID, ids.ContextID
	switch {
	case ids.Task != nil:
		up, upCtx = ids.Task.ID, ids.Task.ContextID
	case ids.StatusUpdate != nil:
		up, upCtx = ids.StatusUpdate.TaskID, ids.StatusUpdate.ContextID
	case ids.ArtifactUpdate != nil:
		up, upCtx = ids.ArtifactUpdate.TaskID, ids.ArtifactUpdate.ContextID
	}
	g.mu.Lock()
	if up != "" {
		g.tasks[task] = [2]string{up, upCtx}
	}
	known := g.tasks[task]
	g.mu.Unlock()
	if known[0] == "" {
		return line
	}
	return strings.NewReplacer(`"`+known[0]+`"`, `"`+task+`"`, `"`+known[1]+`"`, `"`+ctx+`"`).Replace(line)
}

// preambleOpener opens exchange streams with a fixed preamble (the gate's ids, tunnel.md §8.1).
type preambleOpener struct {
	s   *tunnelgate.Session
	pre tunnel.Preamble
}

func (o preambleOpener) OpenExchange(ctx context.Context) (net.Conn, error) {
	return o.s.OpenExchange(ctx, o.pre)
}

// A sealed SendMessage and SendStreamingMessage through a gate, both bindings, to a node-hosted echo seller hired by
// the same node: the local client sends and reads plaintext, the seller gets plaintext, and what the gate holds has
// only envelopes (sealed_no_plaintext's A2A case through a FakeGate); the pin's first sight is printed (D187).
func TestSealedThroughAFakeGate(t *testing.T) {
	seller := &echoSeller{}
	h := a2asrv.NewHandler(seller.executor())
	mux := http.NewServeMux()
	mux.Handle("/rest/", http.StripPrefix("/rest", a2asrv.NewRESTHandler(h)))
	mux.Handle("/", a2asrv.NewJSONRPCHandler(h))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	keys, err := jws.EphemeralKeySet()
	if err != nil {
		t.Fatal(err)
	}
	sg := &sealingGate{t: t, keys: keys, tasks: map[string][2]string{}}
	g := newTestGate(t, func(c *tunnelgate.HandlerConfig) { c.NodeStreams = &tunnelgate.NodeStreams{Hire: sg} })
	sg.url = strings.TrimRight(g.url.String(), "/")
	home := t.TempDir()
	cfg := testConfig(t, g.url, home, mustServe(t, "a2a:echo=http://"+ln.Addr().String()))
	var errOut bytes.Buffer
	cfg.Err = &errOut
	tn := mustNode(t, g, cfg).run(t)
	tn.welcomed(t)
	s := g.session(t)
	sg.session = func() *tunnelgate.Session { return s }
	sg.recipients = func() []string {
		id, err := nodekey.Load(cfg.KeyPath)
		if err != nil {
			t.Error(err)
			return nil
		}
		return []string{id.Recipient()}
	}
	port := tn.localPort(t)

	post := func(path, body string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+port+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("A2A-Version", "1.0")
		res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}
	msg := func(marker string) map[string]any {
		return map[string]any{"messageId": uuid.NewString(), "role": "ROLE_USER", "parts": []any{map[string]any{"text": marker}}}
	}
	cases := []struct {
		name, path string
		body       func(marker string) []byte
	}{
		{"JSON-RPC SendMessage", "/agents/echo", func(m string) []byte {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": map[string]any{"message": msg(m)}})
			return b
		}},
		{"JSON-RPC SendStreamingMessage", "/agents/echo", func(m string) []byte {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendStreamingMessage", "params": map[string]any{"message": msg(m)}})
			return b
		}},
		{"HTTP+JSON message:send", "/agents/echo/message:send", func(m string) []byte {
			b, _ := json.Marshal(map[string]any{"message": msg(m)})
			return b
		}},
		{"HTTP+JSON message:stream", "/agents/echo/message:stream", func(m string) []byte {
			b, _ := json.Marshal(map[string]any{"message": msg(m)})
			return b
		}},
	}
	for i, tc := range cases {
		marker := "MARKER-" + strings.Repeat("x", i+1) + "-" + uuid.NewString()[:8]
		res, out := post(tc.path, string(tc.body(marker)))
		if res.StatusCode != 200 || !strings.Contains(out, "done: "+marker) {
			t.Fatalf("%s: the local client gets %d %s", tc.name, res.StatusCode, out)
		}
		if strings.Contains(out, sealed.MediaType) {
			t.Errorf("%s: an envelope reaches the local client: %s", tc.name, out)
		}
		seller.mu.Lock()
		got := seller.seen[len(seller.seen)-1]
		seller.mu.Unlock()
		if got != marker {
			t.Errorf("%s: the seller got %q", tc.name, got)
		}
		sg.mu.Lock()
		held := sg.held.String()
		sg.mu.Unlock()
		if strings.Contains(held, marker) {
			t.Fatalf("%s: the gate holds the plaintext", tc.name)
		}
		if !strings.Contains(held, hooks.SealedMediaType) {
			t.Fatalf("%s: the gate holds no envelope", tc.name)
		}
	}
	if !strings.Contains(errOut.String(), "first seen") {
		t.Errorf("the pin's first sight is not on stderr: %q", errOut.String())
	}
	// The node's logs never carry a plaintext part.
	if strings.Contains(tn.logs.String(), "MARKER-") {
		t.Error("a plaintext part reached the node's log")
	}
	// The hosting node's job state is under ~/.kontra/hosted/<gate host>/echo.
	if m, _ := filepath.Glob(filepath.Join(home, nodekey.DirName, "hosted", "*", "echo", "jobs", "*.json")); len(m) != len(cases) {
		t.Errorf("%d hosted jobs, want %d", len(m), len(cases))
	}
}
