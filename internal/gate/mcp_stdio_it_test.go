package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/tenant"
)

// installFilesystemServer installs @modelcontextprotocol/server-filesystem@1.0.0 from examples/mcp/fs-stdio's
// package-lock into a directory of the test's and returns its entry point; the test is skipped, saying why, when
// node or npm is missing or npm cannot install the pinned package (no network).
func installFilesystemServer(t *testing.T) (nodeBin, entry string) {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("stdio_roots with server-filesystem@1.0.0: node is not on PATH")
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("stdio_roots with server-filesystem@1.0.0: npm is not on PATH")
	}
	dir := t.TempDir()
	for _, f := range []string{"package.json", "package-lock.json"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "examples", "mcp", "fs-stdio", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, npm, "ci", "--ignore-scripts", "--no-audit", "--no-fund")
	cmd.Dir = dir
	// npm runs with a minimal environment of its own: nothing of the test's (KONTRA_TEST_PG among it) reaches it.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "npm_config_cache=" + filepath.Join(dir, ".npm"), "npm_config_update_notifier=false"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("stdio_roots with server-filesystem@1.0.0: npm cannot install the pinned package here: %v\n%s", err, lastLines(out, 5))
	}
	entry = filepath.Join(dir, "node_modules", "@modelcontextprotocol", "server-filesystem", "dist", "index.js")
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("npm ci installed no server-filesystem: %v", err)
	}
	return nodeBin, entry
}

func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// rpcResult is the JSON-RPC message of an MCP response body, JSON or an event stream (its last data event).
func rpcResult(t *testing.T, body string) map[string]any {
	t.Helper()
	data := body
	if strings.Contains(body, "data:") {
		data = ""
		for _, line := range strings.Split(body, "\n") {
			if d, ok := strings.CutPrefix(line, "data:"); ok && strings.TrimSpace(d) != "" {
				data = strings.TrimSpace(d)
			}
		}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		t.Fatalf("not a JSON-RPC message: %s", body)
	}
	return m
}

// toolText is a tools/call result's text and its isError.
func toolText(t *testing.T, body string) (string, bool) {
	t.Helper()
	m := rpcResult(t, body)
	res, _ := m["result"].(map[string]any)
	if res == nil {
		t.Fatalf("no result: %s", body)
	}
	var b strings.Builder
	content, _ := res["content"].([]any)
	for _, c := range content {
		if c, ok := c.(map[string]any); ok {
			s, _ := c["text"].(string)
			b.WriteString(s)
		}
	}
	isErr, _ := res["isError"].(bool)
	return b.String(), isErr
}

// stdio_roots through the real gate (M3 WP-05; spec/tunnel.md §10, §16, D173): server-filesystem@1.0.0 over stdio,
// started with <dirB> and served by an in-process `kontra connect` with --roots fs=<dirA>, behind a gate whose
// service denies the write tools. A client that declares roots gets its first tools/call answered in under 2 s;
// list_allowed_directories names only <dirA>; a file in <dirA> reads; the node's ~/.kontra/identity.key does not;
// write_file is refused at the gate. The test is skipped, saying why, when npm cannot install the pinned package.
func TestMCPStdioFilesystemThroughTheGate(t *testing.T) {
	e := newGateEnv(t)
	nodeBin, entry := installFilesystemServer(t)
	ctx := context.Background()
	dirA, dirB := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "hello.txt"), []byte("hello from dir A"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "secret.txt"), []byte("dir B is not served"), 0o600); err != nil {
		t.Fatal(err)
	}
	services := `services:
  fs:
    kind: mcp
    backend: { tunnel: {} }
    allow: ["org:default"]
    tools: { deny: ["write_file", "edit_file", "create_directory", "move_file"] }
`
	run := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), fastTiming)
	key := e.mintKey("buyer")
	team, err := tenant.EnsureTeam(ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := enroll.NewStore(e.pool, enroll.Options{}).MintServiceToken(ctx, team, "fs", "test")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	rn := startNode(t, home, "--gate", run.url, "--enroll", tok, "--roots", "fs="+dirA,
		"--serve", "mcp:fs=stdio:'"+nodeBin+"' '"+entry+"' '"+dirB+"'")
	rn.local(t)
	eventually(t, 15*time.Second, "the node's session", func() bool {
		return scalar[int](t, e.pool, `select count(*) from kontra.tunnels`) == 1
	})
	identity := filepath.Join(home, nodekey.DirName, "identity.key")
	if _, err := os.Stat(identity); err != nil {
		t.Fatalf("the node's identity key: %v", err)
	}

	endpoint := run.url + "/mcp/fs"
	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25",` +
		`"capabilities":{"roots":{"listChanged":true}},"clientInfo":{"name":"stdio-roots","version":"1"}}}`
	st, h, body := mcpPost(t, endpoint, key, "", init)
	sid := h.Get("Mcp-Session-Id")
	if st != 200 || sid == "" || !strings.Contains(body, "secure-filesystem-server") {
		t.Fatalf("initialize: %d %q %s", st, sid, body)
	}
	if st, _, body := mcpPost(t, endpoint, key, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); st != 202 {
		t.Fatalf("notifications/initialized: %d %s", st, body)
	}
	start := time.Now()
	st, _, body = mcpPost(t, endpoint, key, sid, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_allowed_directories","arguments":{}}}`)
	took := time.Since(start)
	text, isErr := toolText(t, body)
	if st != 200 || isErr || !strings.Contains(text, dirA) || strings.Contains(text, dirB) {
		t.Fatalf("list_allowed_directories: %d %s", st, body)
	}
	if took > 2*time.Second {
		t.Errorf("the first tools/call took %v, want under 2 s", took)
	}
	read := func(id int, path string) (string, bool) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
			"params": map[string]any{"name": "read_text_file", "arguments": map[string]any{"path": path}}})
		st, _, body := mcpPost(t, endpoint, key, sid, string(b))
		if st != 200 {
			t.Fatalf("read_text_file %s: %d %s", path, st, body)
		}
		return toolText(t, body)
	}
	if text, isErr := read(2, filepath.Join(dirA, "hello.txt")); isErr || text != "hello from dir A" {
		t.Errorf("a file of dir A: %q %v", text, isErr)
	}
	if text, isErr := read(3, identity); !isErr || strings.Contains(text, "AGE-SECRET-KEY") || !strings.Contains(strings.ToLower(text), "denied") {
		t.Errorf("the node's identity key: %q %v", text, isErr)
	}
	if text, isErr := read(4, filepath.Join(dirB, "secret.txt")); !isErr || strings.Contains(text, "not served") {
		t.Errorf("a file of dir B: %q %v", text, isErr)
	}
	st, _, body = mcpPost(t, endpoint, key, sid, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"write_file","arguments":{"path":"`+dirA+`/x","content":"x"}}}`)
	if st != 200 || !strings.Contains(body, "Unknown tool: write_file") {
		t.Errorf("write_file: %d %s", st, body)
	}
	if _, err := os.Stat(filepath.Join(dirA, "x")); err == nil {
		t.Errorf("write_file wrote")
	}
	if logs := rn.logs.String(); !strings.Contains(logs, "stdio MCP session open") || bytes.Contains([]byte(logs), []byte("AGE-SECRET-KEY")) {
		t.Errorf("the node's log: %s", lastLines([]byte(logs), 20))
	}
}
