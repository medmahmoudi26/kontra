package gate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/tenant"
)

// localMCP posts one message to a hiring node's /mcp/{svc} as a stock client does (no credential: the node hires as
// its session's principal) and returns the response, its body read unless it is an event stream.
func localMCP(t *testing.T, endpoint, sid, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		req.Header.Set(mcpwire.HeaderSessionID, sid)
		req.Header.Set(mcpwire.HeaderProtocolVersion, mcpwire.V20251125)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return resp, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// lastData is the data of an event stream's last message event, or a JSON body itself.
func lastData(t *testing.T, resp *http.Response, body string) string {
	t.Helper()
	if body != "" {
		return body
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var last string
	for _, l := range strings.Split(string(b), "\n") {
		if d, ok := strings.CutPrefix(l, "data: "); ok && strings.TrimSpace(d) != "" {
			last = d
		}
	}
	return last
}

// sealed_no_plaintext's MCP cases through a real gate (ext-sealed.md §2, §3, §9; D62, D175): between a hiring node and
// a hosting node, a sealed plain tools/call and a sealed durable task whose upstream asks one elicitation answer the
// local client in plaintext, and the server gets plaintext arguments and answers, while Postgres holds no plaintext
// of arguments, content, elicitation params, answers or statusMessage.
func TestSealedMCPThroughTheGate(t *testing.T) {
	e := newGateEnv(t)
	up := mcptest.New()
	echoTool, _ := json.Marshal(map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}})
	research, _ := json.Marshal(map[string]any{"name": "research", "inputSchema": map[string]any{"type": "object"},
		"execution": map[string]any{"taskSupport": "optional"}})
	up.Tools = []json.RawMessage{echoTool, research}
	up.TaskRun = func(task *mcptest.Task) {
		task.Status("working", "MARKER-status searching")
		ans, ok := task.Ask("elicitation/create", map[string]any{"message": "MARKER-question: which one?",
			"requestedSchema": map[string]any{"type": "object"}}, time.Minute)
		if !ok {
			task.Fail(&mcptest.RPCError{Code: -32603, Message: "no answer"})
			return
		}
		res, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": "MARKER-result: " + string(ans)}}, "isError": false})
		task.Complete(res)
	}
	hs := httptest.NewServer(up)
	t.Cleanup(hs.Close)
	srv := hs.URL
	services := `services:
  web:
    kind: mcp
    backend: { tunnel: {} }
    allow: ["org:default"]
    sealed: true
`
	a := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), fastTiming)
	team, err := tenant.EnsureTeam(t.Context(), e.pool)
	if err != nil {
		t.Fatal(err)
	}
	token, err := enroll.NewStore(e.pool, enroll.Options{}).MintServiceToken(t.Context(), team, "web", "test")
	if err != nil {
		t.Fatal(err)
	}
	host := startNode(t, t.TempDir(), "--gate", a.url, "--name", "host", "--serve", "mcp:web="+srv+"/mcp", "--enroll", token)
	host.local(t)
	key := e.mintKey("buyer")
	hirerHome := t.TempDir()
	hirer := startNode(t, hirerHome, "--gate", a.url, "--name", "hirer", "--api-key", key)
	endpoint := hirer.local(t) + "/mcp/web"

	var resp *http.Response
	var body string
	eventually(t, 20*time.Second, "an initialize through the hiring node with the sealed capability", func() bool {
		resp, body = localMCP(t, endpoint, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25",`+
			`"capabilities":{"elicitation":{}},"clientInfo":{"name":"c","version":"1"}}}`)
		body = lastData(t, resp, body)
		return resp.StatusCode == 200 && strings.Contains(body, mcpwire.MetaSealed)
	})
	sid := resp.Header.Get(mcpwire.HeaderSessionID)
	// The gate states the service's session_idle_s (D224), and the hiring node passes the result on as it came.
	if !strings.Contains(body, `"sh.kontra/session_idle_s":3600`) {
		t.Fatalf("the initialize result does not state session_idle_s: %s", body)
	}
	localMCP(t, endpoint, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	// A plain call.
	resp, body = localMCP(t, endpoint, sid, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"MARKER-echo"}}}`)
	if data := lastData(t, resp, body); !strings.Contains(data, "MARKER-echo") || strings.Contains(data, sealed.ResourceURI) {
		t.Fatalf("the plain call's result: %d %s", resp.StatusCode, data)
	}
	if up.Calls("echo") != 1 {
		t.Fatal("the server did not get the call")
	}
	// The session's per-job key is kept for the session_idle_s the gate stated (D224).
	jobs, _ := filepath.Glob(filepath.Join(hirerHome, ".kontra", "jobs", "*", "mcp-*.json"))
	if len(jobs) != 1 {
		t.Fatalf("the hiring node's session jobs: %v", jobs)
	}
	if b, err := os.ReadFile(jobs[0]); err != nil || !bytes.Contains(b, []byte(`"idle_s":3600`)) {
		t.Fatalf("the session job does not keep the gate's session_idle_s: %s %v", b, err)
	}
	for _, r := range up.Requests() {
		if r.Method == "tools/call" && !bytes.Contains(r.Body, []byte("MARKER-echo")) {
			t.Fatalf("the server got %s", r.Body)
		}
	}

	// A durable task with one elicitation.
	_, body = localMCP(t, endpoint, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"research","arguments":{"topic":"MARKER-topic"},"task":{"ttl":60000}}}`)
	var created struct {
		Result struct {
			Task struct {
				TaskID string `json:"taskId"`
			} `json:"task"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.Result.Task.TaskID == "" {
		t.Fatalf("the task call: %s", body)
	}
	taskID := created.Result.Task.TaskID
	resp, _ = localMCP(t, endpoint, sid, `{"jsonrpc":"2.0","id":3,"method":"tasks/result","params":{"taskId":"`+taskID+`"}}`)
	br := bufio.NewReader(resp.Body)
	var elicit map[string]any
	deadline := time.Now().Add(60 * time.Second)
	for elicit == nil && time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("the tasks/result stream ended: %v", err)
		}
		if d, ok := strings.CutPrefix(strings.TrimSpace(line), "data: "); ok {
			var m map[string]any
			if json.Unmarshal([]byte(d), &m) == nil && m["method"] == "elicitation/create" {
				if !strings.Contains(d, "MARKER-question") || strings.Contains(d, mcpwire.MetaSealed) {
					t.Fatalf("the elicitation the local client gets: %s", d)
				}
				elicit = m
			}
		}
	}
	if elicit == nil {
		t.Fatal("no elicitation on the tasks/result stream")
	}
	answer, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": elicit["id"], "result": map[string]any{"action": "accept",
		"content": map[string]any{"choice": "MARKER-answer"}}})
	if resp, _ := localMCP(t, endpoint, sid, string(answer)); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("the answer: %d", resp.StatusCode)
	}
	var final string
	for final == "" && time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("the tasks/result stream ended before the result: %v", err)
		}
		if d, ok := strings.CutPrefix(strings.TrimSpace(line), "data: "); ok && strings.Contains(d, `"result"`) && !strings.Contains(d, `"method"`) {
			final = d
		}
	}
	resp.Body.Close()
	if !strings.Contains(final, "MARKER-result") || !strings.Contains(final, "MARKER-answer") || strings.Contains(final, sealed.ResourceURI) {
		t.Fatalf("the task's result: %s", final)
	}

	// Postgres holds no plaintext.
	for _, q := range []string{`select coalesce(string_agg(x::text, ' '), '') from kontra.task_events x`,
		`select coalesce(string_agg(x::text, ' '), '') from kontra.tasks x`,
		`select coalesce(string_agg(x::text, ' '), '') from kontra.work_items x`,
		`select coalesce(string_agg(x::text, ' '), '') from kontra.calls x`} {
		if dump := scalar[string](t, e.pool, q); strings.Contains(dump, "MARKER") {
			t.Fatalf("plaintext in Postgres: %s", q)
		}
	}
	for _, l := range []string{host.logs.String(), hirer.logs.String()} {
		if strings.Contains(l, "MARKER") {
			t.Fatal("a plaintext reached a node's log")
		}
	}
	_ = uuid.Nil
}
