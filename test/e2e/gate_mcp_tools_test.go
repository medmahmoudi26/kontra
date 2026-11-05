package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"kontra.sh/kontra/internal/gatemcp"
)

// gate_mcp_tools (spec/relay.md §16; §12.6, D332 to D339). The gate's own /mcp called by the official MCP Go SDK
// (github.com/modelcontextprotocol/go-sdk), unmodified, in both eras (a 2025-11-25 session and 2026-07-28 requests),
// with an API key; a node enrolled on the gate hosts the in-test seller (the TCK seller's artifact kinds, "slow", "ask";
// its card carries the input schema extension) and examples/mcp/everything. Every tool, as a client that shows its
// model only tools uses them:
//
//   - tools/list gives every tool its input schema and the tools that answer a task their output schemas;
//   - describe gives the seller's capabilities (streaming, skills, the input schema) and the MCP service's tool count;
//   - hire with an idempotencyKey twice is one task; the key with another message is isError and creates nothing;
//   - status {after: 0} is the task's history (its artifacts' ids among them); artifact gives one of them in full;
//   - a slow hire with a progress token gets notifications/progress, then the hire result naming status; status {after:
//     <cursor>, wait} waits and answers nothing new; status {after: <cursor>, wait: 25} in a loop follows it to its end
//     while cancel ends it (the hire result, no error); tasks lists both as ended, the completed one by its context;
//   - push_set refuses a loopback URL and stores a public one (on an ended task: D111, nothing is delivered), push_list
//     shows it without its token, push_delete removes it;
//   - cancel refuses the slow task once ended;
//   - reply to an ended task names hire with its contextId, and that hire is a new task in the same context; a question
//     is answered with reply.
func TestGateMCPTools(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped under -short")
	}
	const inputSchema = `{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}`
	seller := StartSellerCaps(t, `{"streaming":true,"extensions":[{"uri":"`+gatemcp.InputSchemaURI+`","required":false,`+
		`"params":{"schema":`+inputSchema+`}}]}`)
	h := start(t, Options{Tunnel: map[string]TunnelService{"kinds": {}}, MCP: map[string]MCPService{"everything": {}}})
	ctx := ctxFor(t, 10*time.Minute)
	srv := h.StartEverything(t, "everything")
	startNode(t, ctx, h, "host", map[string]string{"kinds": seller.URL}, func(o *NodeOptions) {
		o.MCP = map[string]string{"everything": srv.URL}
	})
	a := h.Gates[0]

	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			c := connectGoSDK(t, ctx, a.URL+gatemcp.Path, h.APIKey, version)
			c.run(t, ctx, h, version)
		})
	}
}

// goSDKClient is one MCP Go SDK session on /mcp and the progress notifications it received.
type goSDKClient struct {
	s        *mcp.ClientSession
	mu       sync.Mutex
	progress []*mcp.ProgressNotificationParams
}

// sdkBearer adds the API key to every request of the SDK's transport.
type sdkBearer struct {
	key  string
	base http.RoundTripper
}

func (b sdkBearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return b.base.RoundTrip(r)
}

func connectGoSDK(t *testing.T, ctx context.Context, endpoint, key, version string) *goSDKClient {
	t.Helper()
	c := &goSDKClient{}
	client := mcp.NewClient(&mcp.Implementation{Name: "kontra-e2e", Version: "1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			c.mu.Lock()
			c.progress = append(c.progress, r.Params)
			c.mu.Unlock()
		},
	})
	tr := &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: sdkBearer{key: key, base: http.DefaultTransport}}}
	s, err := client.Connect(ctx, tr, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatalf("the Go SDK's %s session on /mcp: %v", version, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c.s = s
	return c
}

// toolResult is a CallToolResult as the test reads it.
type toolResult struct {
	isError    bool
	text       string
	structured map[string]any
}

func (c *goSDKClient) call(t *testing.T, ctx context.Context, name string, args map[string]any, progress any) toolResult {
	t.Helper()
	out, err := c.tryCall(ctx, name, args, progress)
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return out
}

// tryCall is call, its error returned (a goroutine's call).
func (c *goSDKClient) tryCall(ctx context.Context, name string, args map[string]any, progress any) (toolResult, error) {
	p := &mcp.CallToolParams{Name: name, Arguments: args}
	if progress != nil {
		p.SetProgressToken(progress)
	}
	res, err := c.s.CallTool(ctx, p)
	if err != nil {
		return toolResult{}, err
	}
	out := toolResult{isError: res.IsError}
	for _, ct := range res.Content {
		if tc, ok := ct.(*mcp.TextContent); ok {
			out.text += tc.Text
		}
	}
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(b, &out.structured)
	}
	return out, nil
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func (c *goSDKClient) run(t *testing.T, ctx context.Context, h *Harness, version string) {
	// tools/list: every tool, its input schema; the tools that answer a task their output schemas.
	list, err := c.s.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]bool{}
	for _, tl := range list.Tools {
		if tl.InputSchema == nil {
			t.Errorf("%s has no inputSchema", tl.Name)
		}
		outputs[tl.Name] = tl.OutputSchema != nil
	}
	for _, name := range []string{"hire", "reply", "status", "tasks", "cancel", "artifact", "push_set", "push_list", "push_delete"} {
		if !outputs[name] {
			t.Errorf("%s: no outputSchema (tools %v)", name, outputs)
		}
	}
	if len(list.Tools) != 11 {
		t.Errorf("%d tools", len(list.Tools))
	}

	// describe: the seller's normalized capabilities, the MCP service's tool count.
	d := c.call(t, ctx, "describe", map[string]any{"service": "kinds"}, nil)
	caps, _ := d.structured["capabilities"].(map[string]any)
	skills, _ := caps["skills"].([]any)
	if d.isError || caps["streaming"] != true || caps["inputSchema"] == nil || len(skills) != 1 {
		t.Fatalf("describe kinds: %+v", d)
	}
	d = c.call(t, ctx, "describe", map[string]any{"service": "everything"}, nil)
	// A count only from a cached list (the first describe of a fresh gate starts the drain and has none).
	if caps, _ := d.structured["capabilities"].(map[string]any); d.isError || caps["toolsCached"] == nil ||
		(caps["toolsCached"] == true) != (caps["tools"] != nil) || (caps["tools"] != nil && caps["tools"].(float64) < 1) {
		t.Fatalf("describe everything: %+v", d)
	}

	// hire with an idempotencyKey: one task; the key with another message is refused.
	key := "kinds-" + version
	first := c.call(t, ctx, "hire", map[string]any{"service": "kinds", "message": "kinds", "idempotencyKey": key}, nil)
	task, ctxID := str(first.structured, "taskId"), str(first.structured, "contextId")
	if first.isError || str(first.structured, "state") != "TASK_STATE_COMPLETED" || ctxID == "" || str(first.structured, "service") != "kinds" {
		t.Fatalf("hire kinds: %+v", first)
	}
	again := c.call(t, ctx, "hire", map[string]any{"service": "kinds", "message": "kinds", "idempotencyKey": key}, nil)
	if again.isError || str(again.structured, "taskId") != task {
		t.Fatalf("the same hire again: %+v, want task %s", again, task)
	}
	if n := count(t, h, `select count(*) from kontra.tasks where hired and context_id = $1`, ctxID); n != 1 {
		t.Fatalf("one key made %d tasks", n)
	}
	other := c.call(t, ctx, "hire", map[string]any{"service": "kinds", "message": "hi", "idempotencyKey": key}, nil)
	if !other.isError || !strings.Contains(other.text, "idempotencyKey already used for a different hire") {
		t.Fatalf("the key with another message: %+v", other)
	}

	// status {after: 0}: the history; artifact: one artifact in full.
	st := c.call(t, ctx, "status", map[string]any{"taskId": task, "after": 0}, nil)
	events, _ := st.structured["events"].([]any)
	var dataArtifact string
	kinds := map[string]int{}
	for _, e := range events {
		ev := e.(map[string]any)
		kinds[str(ev, "kind")]++
		if str(ev, "kind") == "artifact" && ev["data"] != nil {
			dataArtifact = str(ev, "artifactId")
		}
	}
	if st.isError || kinds["artifact"] != 4 || kinds["status"] < 1 || dataArtifact == "" || st.structured["cursor"].(float64) < 5 {
		t.Fatalf("status after 0: %+v", st)
	}
	art := c.call(t, ctx, "artifact", map[string]any{"taskId": task, "artifactId": dataArtifact}, nil)
	if data, _ := art.structured["data"].([]any); art.isError || len(data) != 1 || !strings.Contains(art.text, `"count":42`) {
		t.Fatalf("artifact: %+v", art)
	}

	// A slow hire with a progress token: notifications/progress while it waits, then the hire result naming status.
	slow := c.call(t, ctx, "hire", map[string]any{"service": "kinds", "message": "slow"}, "slow-"+version)
	slowID := str(slow.structured, "taskId")
	if slow.isError || str(slow.structured, "state") != "TASK_STATE_WORKING" || !strings.Contains(slow.text, `status {taskId: "`+slowID+`"`) {
		t.Fatalf("a slow hire: %+v", slow)
	}
	c.mu.Lock()
	got := len(c.progress)
	c.mu.Unlock()
	if got < 1 {
		t.Errorf("a slow hire with a progress token got no notifications/progress")
	}
	st = c.call(t, ctx, "status", map[string]any{"taskId": slowID}, nil)
	cursor := st.structured["cursor"]
	began := time.Now()
	st = c.call(t, ctx, "status", map[string]any{"taskId": slowID, "after": cursor, "wait": 2}, nil)
	if ev, _ := st.structured["events"].([]any); st.isError || len(ev) != 0 || st.structured["cursor"] != cursor || time.Since(began) < 1500*time.Millisecond {
		t.Fatalf("status with wait on a quiet task: %+v after %v", st, time.Since(began))
	}

	// Follow the slow task to its end: status {after: <cursor>, wait: 25} in a loop, while cancel ends it. Each call
	// waits until a status row lands (the waker), then answers it with the cursor to ask from next.
	type followed struct {
		state  string
		events []any
		calls  int
		err    error
	}
	done := make(chan followed, 1)
	go func() {
		f := followed{}
		cur := cursor
		for f.calls < 5 {
			f.calls++
			r, err := c.tryCall(ctx, "status", map[string]any{"taskId": slowID, "after": cur, "wait": 25}, nil)
			if err != nil || r.isError {
				f.err = fmt.Errorf("status: %+v %v", r, err)
				break
			}
			ev, _ := r.structured["events"].([]any)
			f.events = append(f.events, ev...)
			cur, f.state = r.structured["cursor"], str(r.structured, "state")
			if f.state == "TASK_STATE_CANCELED" {
				break
			}
		}
		done <- f
	}()
	time.Sleep(time.Second)
	cn := c.call(t, ctx, "cancel", map[string]any{"taskId": slowID}, nil)
	if cn.isError || str(cn.structured, "state") != "TASK_STATE_CANCELED" {
		t.Fatalf("cancel: %+v", cn)
	}
	f := <-done
	ended := false
	for _, e := range f.events {
		if ev := e.(map[string]any); str(ev, "kind") == "status" && str(ev, "state") == "TASK_STATE_CANCELED" {
			ended = true
		}
	}
	if f.err != nil || f.state != "TASK_STATE_CANCELED" || !ended || f.calls > 3 {
		t.Fatalf("following the slow task to its end: %+v", f)
	}

	// tasks: the completed one by its context.
	ls := c.call(t, ctx, "tasks", map[string]any{"state": "ended"}, nil)
	if tasks, _ := ls.structured["tasks"].([]any); ls.isError || !containsTask(tasks, slowID) || !containsTask(tasks, task) {
		t.Fatalf("tasks ended: %+v", ls)
	}
	ls = c.call(t, ctx, "tasks", map[string]any{"contextId": ctxID}, nil)
	if tasks, _ := ls.structured["tasks"].([]any); ls.isError || len(tasks) != 1 || !containsTask(tasks, task) {
		t.Fatalf("tasks by context: %+v", ls)
	}

	// push: a loopback URL refused; a public one stored on the ended task (D111: nothing is delivered), listed without
	// its token, deleted.
	if r := c.call(t, ctx, "push_set", map[string]any{"taskId": task, "url": "https://127.0.0.1/hook"}, nil); !r.isError {
		t.Fatalf("push_set to loopback: %+v", r)
	}
	set := c.call(t, ctx, "push_set", map[string]any{"taskId": task, "url": "https://93.184.215.14/hook", "token": "tok-secret"}, nil)
	configs, _ := set.structured["configs"].([]any)
	if set.isError || len(configs) != 1 || strings.Contains(set.text, "tok-secret") {
		t.Fatalf("push_set: %+v", set)
	}
	cfg := configs[0].(map[string]any)
	if l := c.call(t, ctx, "push_list", map[string]any{"taskId": task}, nil); l.isError || len(l.structured["configs"].([]any)) != 1 {
		t.Fatalf("push_list: %+v", l)
	}
	if del := c.call(t, ctx, "push_delete", map[string]any{"taskId": task, "configId": str(cfg, "configId")}, nil); del.isError ||
		len(del.structured["configs"].([]any)) != 0 {
		t.Fatalf("push_delete: %+v", del)
	}

	// cancel again: refused.
	if cn = c.call(t, ctx, "cancel", map[string]any{"taskId": slowID}, nil); !cn.isError || cn.text != "the task already ended (canceled)" {
		t.Fatalf("cancel of an ended task: %+v", cn)
	}

	// Continuation: reply to the ended task names hire with its contextId; that hire is a new task in the context.
	rp := c.call(t, ctx, "reply", map[string]any{"taskId": task, "message": "more"}, nil)
	if !rp.isError || !strings.Contains(rp.text, `contextId: "`+ctxID+`"`) {
		t.Fatalf("reply to an ended task: %+v", rp)
	}
	next := c.call(t, ctx, "hire", map[string]any{"service": "kinds", "message": "hi", "contextId": ctxID}, nil)
	if next.isError || str(next.structured, "contextId") != ctxID || str(next.structured, "taskId") == task || next.text != "Hello World" {
		t.Fatalf("hire in the context: %+v", next)
	}
	// A question, answered with reply.
	ask := c.call(t, ctx, "hire", map[string]any{"service": "kinds", "message": "ask"}, nil)
	if str(ask.structured, "state") != "TASK_STATE_INPUT_REQUIRED" {
		t.Fatalf("hire ask: %+v", ask)
	}
	ans := c.call(t, ctx, "reply", map[string]any{"taskId": str(ask.structured, "taskId"), "message": "tomorrow"}, nil)
	if ans.isError || ans.text != "booked for tomorrow" {
		t.Fatalf("reply: %+v", ans)
	}
}

func containsTask(tasks []any, id string) bool {
	for _, x := range tasks {
		if m, ok := x.(map[string]any); ok && m["taskId"] == id {
			return true
		}
	}
	return false
}
