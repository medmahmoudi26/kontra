package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/mcpwire"
)

// mcp_tool_denied and mcp_batch_refused (spec/relay.md §11.1, §11.3, §11.12, §16): server-everything@1.0.0 behind a
// node and a recording proxy, its service's tools {allow: ["*"], deny: ["echo", "get-*", "gzip-file-as-resource"]};
// beside it the dual-era double with the same policy, for a 2026-07-28 client.
//
// The legacy tools/list holds no echo or get-* tool and is one page with no nextCursor; tools/call echo answers
// -32602 "Unknown tool: echo", byte-identical but for the name to a nonexistent tool's answer; a batch of a call to
// an allowed tool and one to echo is 400 with an id-less -32600. The modern tools/list holds neither, with
// cacheScope private and no cursor; a tools/call whose Mcp-Name says add while its body names echo is 400 with
// -32020; a modern call to echo is -32602. Through all of it neither upstream receives a tools/call.
func TestMCPToolDenied(t *testing.T) {
	deny := []string{"echo", "get-*", "gzip-file-as-resource"}
	h := start(t, Options{MCP: map[string]MCPService{
		"everything": {ToolsAllow: []string{"*"}, ToolsDeny: deny},
		"dbl":        {ToolsAllow: []string{"*"}, ToolsDeny: deny},
	}})
	ctx := ctxFor(t, 10*time.Minute)
	rec := everythingBehindRecorder(t, h, "everything")
	up := mcptest.New()
	up.Modern = true
	up.Tools = []json.RawMessage{dualEraTool("add", ""), dualEraTool("echo", ""), dualEraTool("get-secret", "")}
	dbl := h.Double(up)
	startNode(t, ctx, h, "host", nil, func(o *NodeOptions) {
		o.MCP = map[string]string{"everything": rec.URL, "dbl": dbl}
	})
	g := h.Gates[0]

	// Legacy: tools/list, a denied call, a nonexistent tool.
	mc := NewMCPClient(g.URL+"/mcp/everything", h.APIKey)
	if _, err := mc.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := mc.Request(ctx, 1, "tools/list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	names := toolNames(t, r)
	if len(names) == 0 || !containsName(names, "trigger-long-running-operation") {
		t.Fatalf("the legacy tools/list: %v", names)
	}
	for _, n := range names {
		if n == "echo" || strings.HasPrefix(n, "get-") || n == "gzip-file-as-resource" {
			t.Fatalf("the legacy tools/list holds the denied tool %s: %v", n, names)
		}
	}
	if _, ok := r.Result()["nextCursor"]; ok {
		t.Fatalf("the legacy tools/list has a nextCursor: %s", r.Text())
	}
	denied, err := mc.Call(ctx, 2, "echo", map[string]any{"message": "x"})
	if err != nil {
		t.Fatal(err)
	}
	missing, err := mc.Call(ctx, 2, "nope", map[string]any{"message": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if code, msg, _, ok := denied.Error(); !ok || code != mcpwire.CodeInvalidParams || msg != "Unknown tool: echo" || denied.Status != http.StatusOK {
		t.Fatalf("a denied tool: %d %s", denied.Status, denied.Text())
	}
	if denied.Body == nil || missing.Body == nil ||
		!bytes.Equal(bytes.ReplaceAll(denied.Body, []byte("echo"), []byte("TOOL")), bytes.ReplaceAll(missing.Body, []byte("nope"), []byte("TOOL"))) {
		t.Fatalf("a denied tool's answer is not a nonexistent one's:\n%s\n%s", denied.Text(), missing.Text())
	}
	// mcp_batch_refused: an array, one call allowed and one denied.
	batch := `[{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"toggle-simulated-logging","arguments":{}}},` +
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{"message":"x"}}}]`
	seen := rec.Len()
	r, err = mc.Post(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, _, ok := r.Error(); r.Status != http.StatusBadRequest || !ok || code != -32600 || r.Answer()["id"] != nil {
		t.Fatalf("a batch: %d %s", r.Status, r.Text())
	}
	if n := rec.Len(); n != seen {
		t.Fatalf("a batch reached the upstream: %d requests", n-seen)
	}

	// Modern: the double through the same policy.
	mod := &ModernClient{Endpoint: g.URL + "/mcp/dbl", Key: h.APIKey}
	r, err = mod.Request(ctx, 1, "tools/list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if names := toolNames(t, r); len(names) != 1 || names[0] != "add" {
		t.Fatalf("the modern tools/list: %d %v %s", r.Status, names, r.Text())
	}
	if r.Result()["cacheScope"] != "private" {
		t.Fatalf("the modern tools/list's cacheScope: %s", r.Text())
	}
	if _, ok := r.Result()["nextCursor"]; ok {
		t.Fatalf("the modern tools/list has a nextCursor: %s", r.Text())
	}
	params := map[string]any{"name": "echo", "arguments": map[string]any{"text": "x"}}
	hdr := Headers("tools/call", map[string]any{"name": "add"})
	r, err = mod.Raw(ctx, mod.Body(2, "tools/call", params), hdr)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, _, ok := r.Error(); r.Status != http.StatusBadRequest || !ok || code != -32020 {
		t.Fatalf("Mcp-Name add, body echo: %d %s", r.Status, r.Text())
	}
	r, err = mod.Request(ctx, 3, "tools/call", params)
	if err != nil {
		t.Fatal(err)
	}
	if code, msg, _, ok := r.Error(); !ok || code != mcpwire.CodeInvalidParams || msg != "Unknown tool: echo" {
		t.Fatalf("a modern call to a denied tool: %d %s", r.Status, r.Text())
	}
	if n := rec.Calls(""); n != 0 {
		t.Fatalf("server-everything received %d tools/call", n)
	}
	if n := up.Calls("echo") + up.Calls("add"); n != 0 {
		t.Fatalf("the double received %d tools/call", n)
	}
	if up.HeaderMismatches() != 0 {
		t.Fatalf("the double saw %d header mismatches: the gate forwarded one", up.HeaderMismatches())
	}
}

// toolNames are the names of a tools/list answer's tools.
func toolNames(t *testing.T, r *MCPResponse) []string {
	t.Helper()
	tools, ok := r.Result()["tools"].([]any)
	if !ok {
		t.Fatalf("no tools/list result: %d %s", r.Status, r.Text())
	}
	var out []string
	for _, x := range tools {
		if m, ok := x.(map[string]any); ok {
			n, _ := m["name"].(string)
			out = append(out, n)
		}
	}
	return out
}

func containsName(names []string, n string) bool {
	for _, x := range names {
		if x == n {
			return true
		}
	}
	return false
}
