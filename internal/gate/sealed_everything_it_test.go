package gate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/tenant"
)

// everythingServer installs @modelcontextprotocol/server-everything@1.0.0 from examples/mcp/everything's package-lock
// and runs it (streamableHttp) on a free loopback port as that example's run.sh does (D185: bound to 127.0.0.1 by
// loopback.cjs, a minimal environment, GZIP_ALLOWED_DOMAINS set); its process group is killed when the test ends. The
// test is skipped, saying why, when node or npm is missing or npm cannot install the pin.
func everythingServer(t *testing.T) string {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("server-everything@1.0.0: node is not on PATH")
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("server-everything@1.0.0: npm is not on PATH")
	}
	src := filepath.Join("..", "..", "examples", "mcp", "everything")
	dir := t.TempDir()
	for _, f := range []string{"package.json", "package-lock.json", "loopback.cjs"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	install := exec.CommandContext(ctx, npm, "ci", "--ignore-scripts", "--no-audit", "--no-fund")
	install.Dir = dir
	install.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "npm_config_cache=" + filepath.Join(dir, ".npm"), "npm_config_update_notifier=false"}
	if out, err := install.CombinedOutput(); err != nil {
		t.Skipf("server-everything@1.0.0: npm cannot install the pinned package here: %v\n%s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	cmd := exec.Command(nodeBin, filepath.Join(dir, "node_modules", "@modelcontextprotocol", "server-everything", "dist", "index.js"), "streamableHttp")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "LANG=C.UTF-8", "PORT=" + strconv.Itoa(port),
		"GZIP_ALLOWED_DOMAINS=example.invalid", "NODE_OPTIONS=--require=" + filepath.Join(dir, "loopback.cjs")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("server-everything:\n%s", logs.String())
		}
	})
	eventually(t, 30*time.Second, "server-everything listens", func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	})
	return fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
}

// The plan's MCP acceptance against server-everything@1.0.0 behind a hosting node, through a real gate, from a hiring
// node (ext-sealed.md §2, §3; D62, D175): a sealed plain tools/call to echo, and a sealed durable task
// (simulate-research-query on an ambiguous topic) whose one elicitation is answered, both answer the local client in
// plaintext, while Postgres holds no plaintext of the arguments, the content, the elicitation, the answer or the
// status messages.
func TestSealedServerEverythingThroughTheGate(t *testing.T) {
	url := everythingServer(t)
	e := newGateEnv(t)
	services := `services:
  everything:
    kind: mcp
    backend: { tunnel: {} }
    allow: ["org:default"]
    sealed: true
    tools: { deny: ["get-env", "gzip-file-as-resource"] }
`
	a := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), fastTiming)
	team, err := tenant.EnsureTeam(t.Context(), e.pool)
	if err != nil {
		t.Fatal(err)
	}
	token, err := enroll.NewStore(e.pool, enroll.Options{}).MintServiceToken(t.Context(), team, "everything", "test")
	if err != nil {
		t.Fatal(err)
	}
	host := startNode(t, t.TempDir(), "--gate", a.url, "--name", "host", "--serve", "mcp:everything="+url, "--enroll", token)
	host.local(t)
	hirer := startNode(t, t.TempDir(), "--gate", a.url, "--name", "hirer", "--api-key", e.mintKey("buyer"))
	endpoint := hirer.local(t) + "/mcp/everything"

	var sid string
	eventually(t, 30*time.Second, "an initialize with the sealed capability", func() bool {
		resp, body := localMCP(t, endpoint, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25",`+
			`"capabilities":{"elicitation":{}},"clientInfo":{"name":"c","version":"1"}}}`)
		sid = resp.Header.Get(mcpwire.HeaderSessionID)
		return resp.StatusCode == 200 && strings.Contains(lastData(t, resp, body), mcpwire.MetaSealed) && sid != ""
	})
	localMCP(t, endpoint, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	resp, body := localMCP(t, endpoint, sid, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"message":"MARKER-echo"}}}`)
	if data := lastData(t, resp, body); !strings.Contains(data, "Echo: MARKER-echo") || strings.Contains(data, sealed.ResourceURI) {
		t.Fatalf("echo: %d %s", resp.StatusCode, data)
	}

	resp, body = localMCP(t, endpoint, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"simulate-research-query",`+
		`"arguments":{"topic":"MARKERtopic","ambiguous":true},"task":{"ttl":120000}}}`)
	body = lastData(t, resp, body)
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
	eventually(t, 60*time.Second, "tasks/get shows input_required", func() bool {
		r, b := localMCP(t, endpoint, sid, `{"jsonrpc":"2.0","id":3,"method":"tasks/get","params":{"taskId":"`+taskID+`"}}`)
		return strings.Contains(lastData(t, r, b), `"input_required"`)
	})
	resp, _ = localMCP(t, endpoint, sid, `{"jsonrpc":"2.0","id":4,"method":"tasks/result","params":{"taskId":"`+taskID+`"}}`)
	br := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(90 * time.Second)
	var elicitID any
	for elicitID == nil && time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("the tasks/result stream ended: %v", err)
		}
		if d, ok := strings.CutPrefix(strings.TrimSpace(line), "data: "); ok {
			var m map[string]any
			if json.Unmarshal([]byte(d), &m) == nil && m["method"] == "elicitation/create" {
				if !strings.Contains(d, "MARKERtopic") || strings.Contains(d, mcpwire.MetaSealed) {
					t.Fatalf("the elicitation the local client gets: %s", d)
				}
				elicitID = m["id"]
			}
		}
	}
	if elicitID == nil {
		t.Fatal("no elicitation on the tasks/result stream")
	}
	answer, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": elicitID, "result": map[string]any{"action": "accept",
		"content": map[string]any{"interpretation": "MARKERinterp"}}})
	if r, b := localMCP(t, endpoint, sid, string(answer)); r.StatusCode != 202 {
		t.Fatalf("the answer: %d %s", r.StatusCode, lastData(t, r, b))
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
	if !strings.Contains(final, "MARKERinterp") || strings.Contains(final, sealed.ResourceURI) {
		t.Fatalf("the task's result: %s", final)
	}
	for _, q := range []string{`select coalesce(string_agg(x::text, ' '), '') from kontra.task_events x`,
		`select coalesce(string_agg(x::text, ' '), '') from kontra.tasks x`,
		`select coalesce(string_agg(x::text, ' '), '') from kontra.work_items x`} {
		if dump := scalar[string](t, e.pool, q); strings.Contains(dump, "MARKER") {
			t.Fatalf("plaintext in Postgres: %s", q)
		}
	}
}
