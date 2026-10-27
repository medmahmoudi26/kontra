package mcprelay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/tunnel"
)

// sealedCapability is a sealed service's extension object (ext-sealed.md §1).
const sealedCapability = `{"description":"sealed","params":{"envelope":"application/vnd.kontra.sealed+age;v=1",` +
	`"recipients":["age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqs3290gq"],"required":true},"required":false,"uri":"https://kontra.sh/ext/sealed/v1"}`

// envelopeMeta is a tools/call's params._meta with an exact sealed envelope.
const envelopeMeta = `{"sh.kontra/sealed":{"v":1,"envelope":"YWdlLWVuY3J5cHRpb24ub3JnL3YxCg=="}}`

// sealedNode is a tunnel node in front of up that answers a sealed tools/call as a hosting node that refuses it
// (ext-sealed.md §6) when refuse is set, and records what reached it.
type sealedNode struct {
	up     http.Handler
	refuse bool
	calls  [][]byte
}

func (n *sealedNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(b))
	if bytes.Contains(b, []byte(`"tools/call"`)) {
		n.calls = append(n.calls, b)
		if n.refuse {
			w.Header().Set(tunnel.HeaderNode, string(tunnel.NodeSealedRejected))
			w.Header().Set(tunnel.HeaderSealedReject, "replayed")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32602,"message":"sealed part rejected: replayed"}}`)
			return
		}
	}
	n.up.ServeHTTP(w, r)
}

// A sealed MCP service (ext-sealed.md §1, §5 "Gate", §6; relay.md §2.5): its initialize result carries the gate's
// capabilities.experimental["sh.kontra/sealed"] (the upstream's own removed); a required service refuses an unsealed
// tools/call -32600 before anything is forwarded or metered; a sealed call keeps its envelope to the node; a node's
// sealed_rejected answer is relayed as the JSON-RPC error, the calls row says is_error, and the audit chain records it.
func TestSealedMCPService(t *testing.T) {
	e := newEnv(t)
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	up.Capabilities = json.RawMessage(`{"tools":{},"experimental":{"sh.kontra/sealed":{"uri":"forged"},"x.other":{}}}`)
	node := &sealedNode{up: up}
	e.service("fs", "", func(s *Service) {
		s.Sealed, s.SealedRequired, s.SealedCapability = true, true, json.RawMessage(sealedCapability)
	})
	r := e.replica("a", replicaOpts{})
	e.node(r, node, "fs")
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	init := c.initialize(r.url + "/mcp/fs")
	var res struct {
		Result struct {
			Capabilities struct {
				Experimental map[string]json.RawMessage `json:"experimental"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(init.body, &res); err != nil {
		t.Fatal(err)
	}
	if got := string(res.Result.Capabilities.Experimental["sh.kontra/sealed"]); got != sealedCapability {
		t.Fatalf("the sealed capability: %s", got)
	}
	if _, ok := res.Result.Capabilities.Experimental["x.other"]; !ok {
		t.Error("the upstream's other experimental capability was dropped")
	}

	// Unsealed: -32600 before anything is forwarded or metered.
	rep := c.call(r.url+"/mcp/fs", 1, "echo", "SECRET-PLAIN")
	if rep.status != 200 || rep.errCode(t) != -32600 || len(node.calls) != 0 || len(e.calls("fs")) != 0 {
		t.Fatalf("an unsealed call: %d %s (node saw %d)", rep.status, rep.body, len(node.calls))
	}

	// Sealed: the envelope reaches the node exactly.
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{},"_meta":` + envelopeMeta + `}}`
	rep = c.do(http.MethodPost, r.url+"/mcp/fs", call, nil)
	if rep.status != 200 || len(node.calls) != 1 || !bytes.Contains(node.calls[0], []byte(`"sh.kontra/sealed":{"v":1,"envelope":"YWdlLWVuY3J5cHRpb24ub3JnL3YxCg=="}`)) {
		t.Fatalf("a sealed call: %d %s; the node got %s", rep.status, rep.body, node.calls)
	}

	// The hosting node refuses the next one.
	node.refuse = true
	call = `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{},"_meta":` + envelopeMeta + `}}`
	rep = c.do(http.MethodPost, r.url+"/mcp/fs", call, nil)
	m := rep.msg(t)
	errObj, _ := m["error"].(map[string]any)
	if rep.status != 200 || errObj == nil || errObj["code"].(float64) != -32602 || errObj["message"] != "sealed part rejected: replayed" || m["id"].(float64) != 3 {
		t.Fatalf("a node's refusal: %d %s", rep.status, rep.body)
	}
	rows := e.calls("fs")
	if len(rows) != 2 || rows[1].isError == nil || !*rows[1].isError {
		t.Fatalf("calls rows %+v", rows)
	}
	chain, err := audit.Verify(context.Background(), e.pool)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range chain {
		s := string(a.RowJCS)
		found = found || strings.Contains(s, `"event":"sealed.rejected"`) && strings.Contains(s, `"reason":"replayed"`) &&
			strings.Contains(s, `"tool":"echo"`) && strings.Contains(s, `"protocol":"mcp"`)
	}
	if !found {
		t.Fatal("no audit row for the node's refusal")
	}
}
