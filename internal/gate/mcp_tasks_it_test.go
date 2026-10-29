package gate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
)

// A durable MCP task through a whole replica (M3 WP-06; spec §11.5–§11.8): the gate's wiring hands the task's
// dispatch from relay's dispatcher to the MCP relay's, which opens its own upstream session, relays the upstream's
// elicitation through the log to the client's tasks/result stream under a gate id, writes the client's answer
// upstream, and logs the result, which tasks/result returns with the related-task key the gate's; one calls row
// names the task.
func TestDurableMCPTaskThroughTheGate(t *testing.T) {
	e := newGateEnv(t)
	up := mcptest.New()
	tool, _ := json.Marshal(map[string]any{"name": "research", "inputSchema": map[string]any{"type": "object"},
		"execution": map[string]any{"taskSupport": "optional"}})
	up.Tools = []json.RawMessage{tool}
	up.TaskRun = func(task *mcptest.Task) {
		task.Status("working", "starting")
		ans, ok := task.Ask("elicitation/create", map[string]any{"message": "which one?"}, time.Minute)
		if !ok {
			task.Fail(&mcptest.RPCError{Code: -32603, Message: "no answer"})
			return
		}
		res, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": "answered: " + string(ans)}}, "isError": false})
		task.Complete(res)
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
	key := e.mintKey("buyer")
	endpoint := a.url + "/mcp/web"
	st, h, body := mcpPost(t, endpoint, key, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"elicitation":{}}}}`)
	sid := h.Get("Mcp-Session-Id")
	if st != 200 || !strings.Contains(body, `"cancel":{}`) {
		t.Fatalf("initialize: %d %s", st, body)
	}
	st, _, body = mcpPost(t, endpoint, key, sid, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"research","arguments":{},"task":{"ttl":60000}}}`)
	var created struct {
		Result struct {
			Task struct {
				TaskID string `json:"taskId"`
			} `json:"task"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &created); st != 200 || err != nil || created.Result.Task.TaskID == "" {
		t.Fatalf("the task-augmented call: %d %s", st, body)
	}
	taskID := created.Result.Task.TaskID

	req, _ := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":9,"method":"tasks/result","params":{"taskId":%q}}`, taskID)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", sid)
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	datas := make(chan string, 8)
	go func() {
		defer close(datas)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok && strings.TrimSpace(d) != "" {
				datas <- d
			}
		}
	}()
	next := func() string {
		select {
		case d, ok := <-datas:
			if !ok {
				t.Fatal("the tasks/result stream ended")
			}
			return d
		case <-time.After(30 * time.Second):
			t.Fatal("no event on the tasks/result stream")
		}
		return ""
	}
	var elicit struct {
		ID     string `json:"id"`
		Method string `json:"method"`
	}
	if d := next(); json.Unmarshal([]byte(d), &elicit) != nil || elicit.Method != "elicitation/create" || !strings.HasPrefix(elicit.ID, "k."+taskID+".") {
		t.Fatalf("the elicitation: %s", d)
	}
	if st, _, body := mcpPost(t, endpoint, key, sid, `{"jsonrpc":"2.0","id":"`+elicit.ID+`","result":{"action":"accept","content":{"choice":"blue"}}}`); st != http.StatusAccepted {
		t.Fatalf("the answer: %d %s", st, body)
	}
	final := next()
	if !strings.Contains(final, `"id":9`) || !strings.Contains(final, `blue`) || !strings.Contains(final, `"io.modelcontextprotocol/related-task":{"taskId":"`+taskID+`"}`) {
		t.Fatalf("tasks/result: %s", final)
	}
	if n := scalar[int64](t, e.pool, `select count(*) from kontra.calls where task_id = $1::uuid and not is_error`, taskID); n != 1 {
		t.Fatalf("%d calls rows for the task", n)
	}
	// The dispatch ends its attempt once it returns, a moment after the result is logged.
	eventually(t, 10*time.Second, "the attempt ended completed, with the dispatch's own upstream session", func() bool {
		return scalar[int64](t, e.pool, `select count(*) from kontra.attempts where task_id = $1::uuid and upstream_session is not null and outcome = 'completed'`, taskID) == 1
	})
}
