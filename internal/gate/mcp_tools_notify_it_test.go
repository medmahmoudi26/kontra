package gate

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
)

// D199's notice between two whole replicas, the gate-level test D225 left to M4 (M3 review mcp-spec-5; D299): replica
// B has drained the service's tools/list; an upstream's notifications/tools/list_changed, relayed on A's POST stream
// of a tools/call, reaches B through NOTIFY kontra_tunnel 'tools:<tenant_id>/<service>', so B's next call of the tool
// the upstream just added is relayed, where B's drained list would have answered it "Unknown tool" for up to 10
// minutes (§11.3).
func TestToolsListChangedAcrossReplicas(t *testing.T) {
	e := newGateEnv(t)
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", ""), mcptest.Tool("grow", "")}
	up.OnCall = func(r mcptest.Request) mcptest.Reply {
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(r.Params, &p)
		switch p.Name {
		case "grow":
			// The upstream adds a tool and says so on the call's own stream, before the call's result.
			up.Lock()
			up.Tools = append(up.Tools, mcptest.Tool("fresh", ""))
			up.Unlock()
			return mcptest.Reply{
				Events: []string{"event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n"},
				Result: json.RawMessage(`{"content":[{"type":"text","text":"grown"}]}`),
			}
		case "fresh":
			return mcptest.Reply{Result: json.RawMessage(`{"content":[{"type":"text","text":"fresh answered"}]}`)}
		}
		return mcptest.Reply{Result: json.RawMessage(`{"content":[{"type":"text","text":"echo"}]}`)}
	}
	direct := httptest.NewServer(up)
	t.Cleanup(direct.Close)
	services := `services:
  web:
    kind: mcp
    backend: { mcp: { url: "` + direct.URL + `/mcp", allow_private: true } }
    allow: ["org:default"]
`
	a := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), fastTiming)
	b := e.start("b", e.replicaConfig(freePort(t), time.Minute, services), fastTiming)
	key := e.mintKey("buyer")
	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`
	session := func(run *running) string {
		t.Helper()
		st, h, body := mcpPost(t, run.url+"/mcp/web", key, "", init)
		sid := h.Get("Mcp-Session-Id")
		if st != 200 || len(sid) != 22 {
			t.Fatalf("initialize web on %s: %d %s", run.r.ID(), st, body)
		}
		return sid
	}
	call := func(run *running, sid, id, tool string) string {
		t.Helper()
		st, _, body := mcpPost(t, run.url+"/mcp/web", key, sid,
			`{"jsonrpc":"2.0","id":`+id+`,"method":"tools/call","params":{"name":"`+tool+`"}}`)
		if st != 200 {
			t.Fatalf("tools/call %s on %s: %d %s", tool, run.r.ID(), st, body)
		}
		return body
	}

	// B drains the list the upstream has now: fresh is unknown there, and never reaches the upstream.
	sb := session(b)
	if body := call(b, sb, "1", "fresh"); !strings.Contains(body, "Unknown tool: fresh") {
		t.Fatalf("fresh on B before the change: %s", body)
	}
	// A relays the upstream's list_changed on the stream of its call.
	sa := session(a)
	if body := call(a, sa, "2", "grow"); !strings.Contains(body, "notifications/tools/list_changed") || !strings.Contains(body, "grown") {
		t.Fatalf("grow on A: %s", body)
	}
	// B's drained list is dropped by A's notice (coalesced for 250 ms): its next call of fresh drains the new list.
	n := 3
	eventually(t, 5*time.Second, "B learns the upstream's new tool", func() bool {
		n++
		return strings.Contains(call(b, sb, strconv.Itoa(n), "fresh"), "fresh answered")
	})
	if got := up.Calls("fresh"); got != 1 {
		t.Errorf("the upstream answered fresh %d times, want once (after the change)", got)
	}
}
