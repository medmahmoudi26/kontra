package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/tunnel"
)

// TestMCPHosting is MCP hosting on the node (spec/tunnel.md §8.2, §10; M3 WP-05) at the binary level, with no
// Postgres or Temporal: the FakeGate admits the real `kontra connect` for two MCP services, web (mcp:web=<url>, a Go
// Streamable HTTP double at its /mcp endpoint on 127.0.0.1) and fs (mcp:fs=stdio:<the Go stdio double>, with
// --roots and the run's marker variables passed by --env, D185). Over streams the FakeGate opens as a gate does, each
// answers an initialize and a tools/call; the stdio child sees the node's roots, not the client's (D173), and only
// PATH, HOME, LANG and the --env names; the URL double sees its endpoint path and the node's roots capability.
func TestMCPHosting(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: runs the kontra connect binary; skipped under -short")
	}
	root, err := TempRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "mcp-hosting-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	bin, err := nodeBinary()
	if err != nil {
		t.Fatal(err)
	}
	// The stdio double, built under TempRoot so that scripts/test_e2e.sh finds a leftover child by its binary too.
	double := filepath.Join(dir, "stdioserver")
	if out, err := exec.Command("go", "build", "-o", double, "kontra.sh/kontra/internal/stdiobridge/testdata/stdioserver").CombinedOutput(); err != nil {
		t.Fatalf("build the stdio double: %v\n%s", err, out)
	}
	rec := filepath.Join(dir, "records")
	rootsDir := filepath.Join(dir, "allowed")
	for _, d := range []string{rec, rootsDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	web := httptest.NewServer(up) // 127.0.0.1
	t.Cleanup(web.Close)

	gate, err := StartFakeGate([]string{"mcp:web", "mcp:fs"}, filepath.Join(dir, "fakegate.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gate.Close() })
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	run := RunEnv + "=mcp-hosting-" + randomHex(4)
	n := &Node{
		Name: "mcp-hosting", Home: home, KeyPath: filepath.Join(home, nodekey.DirName, "identity.key"), bin: bin,
		OutPath: filepath.Join(dir, "node.out"), LogPath: filepath.Join(dir, "node.log"),
		opts: NodeOptions{Gate: gate.URL, Args: []string{
			"--serve", "mcp:web=" + web.URL + "/mcp",
			"--serve", "mcp:fs=stdio:'" + double + "' -record '" + rec + "'",
			"--roots", "fs=" + rootsDir,
			"--env", RunEnv, "--env", SessionEnv,
		}},
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "KONTRA_KEY=") && !strings.HasPrefix(kv, RunEnv+"=") {
			n.env = append(n.env, kv)
		}
	}
	n.env = append(n.env, run, "HOME="+home, "KONTRA_TEST_NODE_SECRET=must-not-reach-the-child")
	if err := n.launch(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Kill() })
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("node log:\n%s\nfake gate log:\n%s", tail(n.LogPath, 60), tail(filepath.Join(dir, "fakegate.log"), 30))
		}
	})
	sess, err := gate.Session(30 * time.Second)
	if err != nil {
		t.Fatalf("%v\n%s", err, tail(n.LogPath, 30))
	}

	post := func(service, sid, body string) (*http.Response, string) {
		t.Helper()
		h := "POST / HTTP/1.1\r\nHost: gate.invalid\r\nContent-Type: application/json\r\nAccept: application/json, text/event-stream\r\n"
		if sid != "" {
			h += mcpwire.HeaderSessionID + ": " + sid + "\r\n" + mcpwire.HeaderProtocolVersion + ": 2025-11-25\r\n"
		}
		raw := h + fmt.Sprintf("Content-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		pre := tunnel.Preamble{Op: tunnel.OpHTTP, Service: service, Kind: tunnel.KindMCP, MCPSession: strings.Repeat("ab", 32)}
		resp, got, err := Exchange(sess, pre, raw, 30*time.Second)
		if err != nil {
			t.Fatalf("%s %s: %v", service, body, err)
		}
		return resp, string(got)
	}
	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25",` +
		`"capabilities":{"roots":{"listChanged":true}},"clientInfo":{"name":"e2e","version":"1"}}}`
	call := func(id int, tool, args string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, tool, args)
	}

	for _, svc := range []string{"web", "fs"} {
		resp, body := post(svc, "", init)
		sid := resp.Header.Get(mcpwire.HeaderSessionID)
		if resp.StatusCode != http.StatusOK || sid == "" || !strings.Contains(body, `"protocolVersion"`) || resp.Header.Get(tunnel.HeaderNode) != "" {
			t.Fatalf("%s: initialize: %d %q %s", svc, resp.StatusCode, sid, body)
		}
		if resp, body := post(svc, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("%s: notifications/initialized: %d %s", svc, resp.StatusCode, body)
		}
		resp, body = post(svc, sid, call(1, "echo", `{"text":"hello through `+svc+`"}`))
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "hello through "+svc) {
			t.Fatalf("%s: tools/call: %d %s", svc, resp.StatusCode, body)
		}
		if svc != "fs" {
			continue
		}
		// The stdio child: the node's roots, and an environment of PATH, HOME, LANG and the --env names only.
		resp, body = post(svc, sid, call(2, "env", `{}`))
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, RunEnv+"=") || strings.Contains(body, "KONTRA_TEST_NODE_SECRET") ||
			strings.Contains(body, "KONTRA_TEST_PG") {
			t.Errorf("the child's environment: %s", body)
		}
		var answers []string
		deadline := time.Now().Add(10 * time.Second)
		for len(answers) == 0 && time.Now().Before(deadline) {
			answers, _ = filepath.Glob(filepath.Join(rec, "roots-*.json"))
			time.Sleep(50 * time.Millisecond)
		}
		if len(answers) != 1 {
			t.Fatalf("the child recorded %d roots answers", len(answers))
		}
		b, _ := os.ReadFile(answers[0])
		if !strings.Contains(string(b), `"file://`+rootsDir+`"`) || strings.Contains(string(b), `"file:///"`) {
			t.Errorf("the child's roots: %s", b)
		}
		inits, _ := filepath.Glob(filepath.Join(rec, "init-*.json"))
		if len(inits) != 1 {
			t.Fatalf("%d children", len(inits))
		}
		if b, _ := os.ReadFile(inits[0]); !strings.Contains(string(b), `"roots":{}`) || strings.Contains(string(b), "listChanged") {
			t.Errorf("the initialize the child saw: %s", b)
		}
		// DELETE ends the session; its next request is 404.
		del := "DELETE / HTTP/1.1\r\nHost: gate.invalid\r\n" + mcpwire.HeaderSessionID + ": " + sid + "\r\nConnection: close\r\n\r\n"
		if resp, _, err := Exchange(sess, tunnel.Preamble{Op: tunnel.OpHTTP, Service: svc, Kind: tunnel.KindMCP}, del, 30*time.Second); err != nil ||
			resp.StatusCode != http.StatusOK {
			t.Errorf("DELETE: %v %v", resp, err)
		}
		if resp, _ := post(svc, sid, call(3, "echo", `{"text":"x"}`)); resp.StatusCode != http.StatusNotFound {
			t.Errorf("after DELETE: %d", resp.StatusCode)
		}
	}
	// The URL double saw its endpoint path and the node's roots capability ({}: --roots names fs only, so web has
	// none and declares none).
	reqs := up.Requests()
	var initParams struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	_ = json.Unmarshal(reqs[0].Params, &initParams)
	if _, declared := initParams.Capabilities["roots"]; declared || up.Calls("echo") != 1 {
		t.Errorf("the URL double: capabilities %v, %d echo calls", initParams.Capabilities, up.Calls("echo"))
	}
	// A legacy-only stdio service answers a 2026-07-28 request without a session with rule 3's body (D161).
	modern := `{"jsonrpc":"2.0","id":9,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientCapabilities":{}}}}`
	raw := "POST / HTTP/1.1\r\nHost: gate.invalid\r\nContent-Type: application/json\r\nMCP-Protocol-Version: 2026-07-28\r\nMcp-Method: tools/list\r\n" +
		fmt.Sprintf("Content-Length: %d\r\nConnection: close\r\n\r\n%s", len(modern), modern)
	if resp, body, err := Exchange(sess, tunnel.Preamble{Op: tunnel.OpHTTP, Service: "fs", Kind: tunnel.KindMCP}, raw, 30*time.Second); err != nil ||
		resp.StatusCode != http.StatusBadRequest || string(body) != string(mcpwire.LegacyNoSessionBody) {
		t.Errorf("a modern request to the stdio service: %v %s %v", resp, body, err)
	}
	// Stopping the node (SIGTERM) stops its children first: no process of a live session outlives it (D184).
	resp, _ := post("fs", "", init)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a last initialize: %d", resp.StatusCode)
	}
	starts, _ := filepath.Glob(filepath.Join(rec, "start-*.json"))
	if len(starts) != 2 {
		t.Fatalf("%d children started, want 2", len(starts))
	}
	var pids []int
	for _, f := range starts {
		var st struct{ PID int }
		b, _ := os.ReadFile(f)
		_ = json.Unmarshal(b, &st)
		pids = append(pids, st.PID)
	}
	if code, err := n.Term(); err != nil || code != 0 {
		t.Fatalf("SIGTERM: exit %d, %v", code, err)
	}
	for _, pid := range pids {
		if syscall.Kill(pid, 0) == nil {
			t.Errorf("the stdio child %d outlived kontra connect", pid)
		}
	}
}
