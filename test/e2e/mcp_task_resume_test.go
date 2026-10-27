package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/mcpwire"
)

// mcp_task_resume (spec/relay.md §11.9, §11.12, §16; D178): a replica killed during a gate-owned MCP task.
//
//   - The client's replica killed (variant A): server-everything@1.0.0's simulate-research-query behind a node
//     whose tunnel is on replica B, the client on A. A is SIGKILLed right after the CreateTaskResult; tasks/result
//     on B, on the same session (a 404 fails the test: A held only the client's connection), returns the upstream's
//     result with related-task set to the gate's id; once B has reaped A, the session's row is still open and held
//     by B, and the session answers tasks/get on B (relay.md §11.2, D179).
//   - The tunnel's replica killed (variant B): the same call (ambiguous, so the upstream waits), B SIGKILLed once
//     the attempt has its first upstream task id; tasks/result on A returns §11.8's failed form, -32603 with
//     sh.kontra/failure tunnel_lost, and tasks/get shows failed.
//   - Modern: a 2026-07-28 client's gate-owned task on the dual-era double; tasks/get answers the same from either
//     replica, and from B once A, which held only the client's connection, is killed.
//   - Modern MRTR: the upstream answered the dispatch's tools/call with an InputRequiredResult; the node is
//     SIGKILLed and comes back; the task fails tunnel_lost, which tasks/get shows on either replica, and the
//     upstream never receives that tools/call again.
//
// None hangs: each answer arrives within 90 s. The calls rows have task_id, is_error true for the failures, and
// the killed replica's tunnels rows are gone.
func TestMCPTaskResume(t *testing.T) {
	t.Run("client replica killed", func(t *testing.T) { testResumeLegacy(t, false) })
	t.Run("tunnel replica killed", func(t *testing.T) { testResumeLegacy(t, true) })
	t.Run("modern, a replica killed", testResumeModern)
	t.Run("modern MRTR, the tunnel killed", testResumeMRTR)
}

func testResumeLegacy(t *testing.T, killTunnel bool) {
	h := start(t, Options{MCP: map[string]MCPService{"everything": {ToolsDeny: EverythingDeny}}, Peer: true})
	ctx := ctxFor(t, 10*time.Minute)
	every := h.StartEverything(t, "everything")
	A, B := h.Gates[0], h.Gates[1]
	h.Front.Route(B)
	startNode(t, ctx, h, "host", nil, func(o *NodeOptions) { o.MCP = map[string]string{"everything": every.URL} })
	mc := NewMCPClient(A.URL+"/mcp/everything", h.APIKey)
	if _, err := mc.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := mc.Request(ctx, 1, "tools/call", map[string]any{"name": "simulate-research-query",
		"arguments": map[string]any{"topic": "resume", "ambiguous": killTunnel}, "task": map[string]any{"ttl": 300000}})
	if err != nil {
		t.Fatal(err)
	}
	created, _ := r.Result()["task"].(map[string]any)
	taskID, _ := created["taskId"].(string)
	if taskID == "" {
		t.Fatalf("no CreateTaskResult: %d %s", r.Status, r.Text())
	}
	killed, asked := A, B
	if killTunnel {
		killed, asked = B, A
		eventually(t, 60*time.Second, "the attempt's first upstream task id", func() bool {
			atts, err := h.Attempts(ctx, a2a.TaskID(taskID))
			return err == nil && len(atts) > 0 && atts[len(atts)-1].FirstUpstreamIDAt != nil
		})
		h.Front.Route(A) // the node redials the surviving replica
	}
	if err := killed.Kill(); err != nil {
		t.Fatal(err)
	}
	killedAt := time.Now()
	t.Logf("task %s: replica %s (%s) killed; tasks/result on %s", taskID, killed.Name, killed.ReplicaID, asked.Name)

	answer := tasksResultOn(t, ctx, h, mc, asked, taskID, killedAt, killTunnel)
	if took := time.Since(killedAt); took > 90*time.Second {
		t.Fatalf("tasks/result answered %v after the kill, want within 90 s", took)
	}
	calls := callsOf(t, h, "everything")
	var row *CallRow
	for i := range calls {
		if calls[i].TaskID == taskID {
			row = &calls[i]
		}
	}
	if !killTunnel {
		res, _ := answer["result"].(map[string]any)
		meta, _ := res["_meta"].(map[string]any)
		related, _ := meta[mcpwire.MetaRelatedTask].(map[string]any)
		text, _ := json.Marshal(res["content"])
		if res == nil || related["taskId"] != taskID || !strings.Contains(string(text), "Research Report: resume") {
			t.Fatalf("variant A's tasks/result: %v", answer)
		}
		if row == nil || row.IsError == nil || *row.IsError {
			t.Fatalf("variant A's calls row: %+v", row)
		}
		// A held only the client's connection: B reaping it closes no session whose tunnel B holds.
		eventually(t, 2*ReapAfter, "B reaped A's gate_replicas row", func() bool {
			return scalar[int](t, h, `select count(*) from kontra.gate_replicas where replica_id = $1`, killed.ReplicaID) == 0
		})
		if s := sessionRow(t, h, mc.SID); s.closed || s.replica != asked.ReplicaID {
			t.Fatalf("variant A: once A was reaped, the session's row is %+v, want open and held by %s", s, asked.Name)
		}
		get, err := mc.At(asked.URL+"/mcp/everything").Request(ctx, 9, "tasks/get", map[string]any{"taskId": taskID})
		if err != nil {
			t.Fatal(err)
		}
		if get.Status != http.StatusOK || get.Result()["status"] != "completed" {
			t.Fatalf("variant A: tasks/get on %s with the same session after A was reaped: %d %s", asked.Name, get.Status, get.Text())
		}
	} else {
		e, _ := answer["error"].(map[string]any)
		data, _ := e["data"].(map[string]any)
		if e == nil || e["code"] != float64(mcpwire.CodeInternal) || data["sh.kontra/failure"] != "tunnel_lost" {
			t.Fatalf("variant B's tasks/result: %v", answer)
		}
		get, err := mc.At(asked.URL+"/mcp/everything").Request(ctx, 9, "tasks/get", map[string]any{"taskId": taskID})
		if err != nil || get.Result()["status"] != "failed" {
			t.Fatalf("variant B's tasks/get: %v %s", err, get.Text())
		}
		if row == nil || row.IsError == nil || !*row.IsError {
			t.Fatalf("variant B's calls row: %+v", row)
		}
	}
	eventually(t, 2*ReapAfter, "the killed replica's tunnels rows are gone", func() bool {
		return scalar[int](t, h, `select count(*) from kontra.tunnels where replica_id = $1`, killed.ReplicaID) == 0
	})
}

// tasksResultOn asks tasks/result on g with the client's session and returns the answer. With reinit, when g answers
// that session 404 (its row was closed with the tunnel of the killed replica, §11.2), the client initializes again on
// g, as a client does, and asks with the new session (the task is the buyer's, §11.6); without it, a 404 fails the
// test (the killed replica held only the client's connection, so the session must outlive it).
func tasksResultOn(t *testing.T, ctx context.Context, h *Harness, mc *MCPClient, g *Gate, taskID string, since time.Time, reinit bool) map[string]any {
	t.Helper()
	c := mc.At(g.URL + "/mcp/everything")
	for time.Since(since) < 90*time.Second {
		rctx, cancel := context.WithTimeout(ctx, 90*time.Second-time.Since(since))
		r, err := c.Post(rctx, fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"method":"tasks/result","params":{"taskId":%q}}`, taskID))
		cancel()
		if err != nil {
			t.Fatalf("tasks/result on %s: %v", g.Name, err)
		}
		if r.Status == http.StatusNotFound {
			if !reinit {
				t.Fatalf("tasks/result on %s: the session is gone (%s), though the killed replica held only the client's connection", g.Name, r.Text())
			}
			t.Logf("tasks/result on %s: the session is closed (%s); initialize again", g.Name, r.Text())
			fresh := NewMCPClient(g.URL+"/mcp/everything", h.APIKey)
			for time.Since(since) < 90*time.Second {
				if _, err := fresh.Initialize(ctx); err == nil {
					break
				}
				time.Sleep(time.Second)
			}
			c = fresh
			continue
		}
		if a := r.Answer(); a != nil {
			return a
		}
		t.Fatalf("tasks/result on %s: %d %s", g.Name, r.Status, r.Text())
	}
	t.Fatal("tasks/result did not answer within 90 s of the kill")
	return nil
}

// modernTasks makes the double a tasks extension receiver whose tools work in the background: slow completes after
// 6 s, ask waits for one answer.
func modernTasks(up *mcptest.Server) {
	up.Modern = true
	up.ModernTaskRun = func(task *mcptest.Task) {
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(task.Call.Params, &p)
		text := func(s string) json.RawMessage {
			b, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}})
			return b
		}
		switch p.Name {
		case "slow":
			task.Status("working", "working slowly")
			time.Sleep(6 * time.Second)
			task.Complete(text("slow answer"))
		default:
			task.Status("working", "asking")
			ans, ok := task.AskModern("which", mcptest.Elicit("Which one?"), 2*time.Minute)
			if !ok {
				task.Fail(&mcptest.RPCError{Code: -32603, Message: "no answer"})
				return
			}
			var a struct {
				Content map[string]any `json:"content"`
			}
			_ = json.Unmarshal(ans, &a)
			task.Complete(text(fmt.Sprintf("answered %v", a.Content["answer"])))
		}
	}
}

func testResumeModern(t *testing.T) {
	h := start(t, Options{MCP: map[string]MCPService{"dbl": {}}, Peer: true})
	ctx := ctxFor(t, 10*time.Minute)
	up := mcptest.New()
	up.Tools = []json.RawMessage{dualEraTool("slow", "")}
	modernTasks(up)
	dbl := h.Double(up)
	A, B := h.Gates[0], h.Gates[1]
	h.Front.Route(B)
	startNode(t, ctx, h, "host", nil, func(o *NodeOptions) { o.MCP = map[string]string{"dbl": dbl} })
	m := &ModernClient{Endpoint: A.URL + "/mcp/dbl", Key: h.APIKey, Tasks: true}
	if r, err := m.Request(ctx, 1, "server/discover", map[string]any{}); err != nil || r.Result() == nil {
		t.Fatalf("server/discover: %v %+v", err, r)
	}
	r, err := m.Request(ctx, 2, "tools/call", map[string]any{"name": "slow", "arguments": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := r.Result()["taskId"].(string)
	if taskID == "" || r.Result()["resultType"] != "task" {
		t.Fatalf("no CreateTaskResult: %d %s", r.Status, r.Text())
	}
	get := func(g *Gate) map[string]any {
		t.Helper()
		r, err := m.At(g.URL+"/mcp/dbl").Request(ctx, 3, "tasks/get", map[string]any{"taskId": taskID})
		if err != nil || r.Result() == nil {
			t.Fatalf("tasks/get on %s: %v %+v", g.Name, err, r)
		}
		return r.Result()
	}
	onA, onB := get(A), get(B)
	if onA["status"] != "working" || onA["taskId"] != onB["taskId"] || onA["status"] != onB["status"] || onA["createdAt"] != onB["createdAt"] {
		t.Fatalf("tasks/get differs between the replicas:\n%v\n%v", onA, onB)
	}
	if err := A.Kill(); err != nil {
		t.Fatal(err)
	}
	killedAt := time.Now()
	var final map[string]any
	for time.Since(killedAt) < 90*time.Second {
		final = get(B)
		if final["status"] != "working" {
			break
		}
		time.Sleep(time.Second)
	}
	text, _ := json.Marshal(final["result"])
	if final["status"] != "completed" || !strings.Contains(string(text), "slow answer") {
		t.Fatalf("tasks/get on B after A's kill: %v", final)
	}
	t.Logf("completed on B %v after A was killed", time.Since(killedAt).Round(time.Second))
}

func testResumeMRTR(t *testing.T) {
	h := start(t, Options{MCP: map[string]MCPService{"mrtr": {}}, Peer: true})
	ctx := ctxFor(t, 10*time.Minute)
	up := mcptest.New()
	up.Modern = true
	up.Tools = []json.RawMessage{dualEraTool("ask", "")}
	up.ModernCapabilities = json.RawMessage(`{"tools":{},"extensions":{"io.modelcontextprotocol/tasks":{}}}`)
	up.OnCall = func(q mcptest.Request) mcptest.Reply {
		if strings.Contains(string(q.Params), "inputResponses") {
			return mcptest.Reply{Result: json.RawMessage(`{"content":[{"type":"text","text":"answered"}]}`)}
		}
		return mcptest.Reply{Result: mcptest.InputRequired(map[string]any{"q1": mcptest.Elicit("Which one?")}, "S1")}
	}
	dbl := h.Double(up)
	A, B := h.Gates[0], h.Gates[1]
	h.Front.Route(B)
	n, sess := startNode(t, ctx, h, "host", nil, func(o *NodeOptions) { o.MCP = map[string]string{"mrtr": dbl} })
	m := &ModernClient{Endpoint: A.URL + "/mcp/mrtr", Key: h.APIKey, Tasks: true}
	if r, err := m.Request(ctx, 1, "server/discover", map[string]any{}); err != nil || r.Result() == nil {
		t.Fatalf("server/discover: %v %+v", err, r)
	}
	r, err := m.Request(ctx, 2, "tools/call", map[string]any{"name": "ask", "arguments": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := r.Result()["taskId"].(string)
	if taskID == "" {
		t.Fatalf("no CreateTaskResult: %d %s", r.Status, r.Text())
	}
	var gateKey string
	eventually(t, 30*time.Second, "tasks/get input_required with the upstream's request", func() bool {
		r, err := m.Request(ctx, 3, "tasks/get", map[string]any{"taskId": taskID})
		if err != nil || r.Result()["status"] != "input_required" {
			return false
		}
		reqs, _ := r.Result()["inputRequests"].(map[string]any)
		for k := range reqs {
			gateKey = k
		}
		return len(reqs) == 1
	})
	if c := up.Calls("ask"); c != 1 {
		t.Fatalf("the double received %d tools/call before the kill", c)
	}
	// The tunnel dies after the InputRequiredResult (the attempt's first upstream id), and the node comes back.
	if err := n.Kill(); err != nil {
		t.Fatal(err)
	}
	killedAt := time.Now()
	waitRowGone(t, ctx, h, sess, 30*time.Second, "the node was SIGKILLed")
	if err := n.Restart(ctx, h); err != nil {
		t.Fatal(err)
	}
	// The client answers: the dispatch may not send the call again on another session.
	if r, err := m.Request(ctx, 4, "tasks/update", map[string]any{"taskId": taskID, "inputResponses": map[string]any{
		gateKey: map[string]any{"action": "accept", "content": map[string]any{"answer": "the first"}}}}); err != nil || r.Result() == nil {
		t.Fatalf("tasks/update: %v %+v", err, r)
	}
	for _, g := range []*Gate{A, B} {
		var get map[string]any
		for time.Since(killedAt) < 90*time.Second {
			r, err := m.At(g.URL+"/mcp/mrtr").Request(ctx, 5, "tasks/get", map[string]any{"taskId": taskID})
			if err != nil {
				t.Fatal(err)
			}
			if get = r.Result(); get["status"] == "failed" {
				break
			}
			time.Sleep(time.Second)
		}
		e, _ := get["error"].(map[string]any)
		data, _ := e["data"].(map[string]any)
		if get["status"] != "failed" || data["sh.kontra/failure"] != "tunnel_lost" {
			t.Fatalf("tasks/get on %s: %v, want failed: tunnel_lost within 90 s of the kill", g.Name, get)
		}
	}
	time.Sleep(2 * time.Second)
	if c := up.Calls("ask"); c != 1 {
		t.Fatalf("the double received the tools/call %d times: the dispatch called it again after the InputRequiredResult", c)
	}
	if state, failure, err := h.TaskFailure(ctx, a2a.TaskID(taskID)); err != nil || state != "failed" || failure != "tunnel_lost" {
		t.Fatalf("the task's row: %s %s %v", state, failure, err)
	}
}
