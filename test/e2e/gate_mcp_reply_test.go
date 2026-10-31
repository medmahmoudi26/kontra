package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// gate_mcp_reply (spec/relay.md §16; §12.4; D272, D274). Single tenant. The booking seller (examples/sellers/booking,
// one question, then the booking) and the in-test seller (a slow exec) are hosted by a node whose tunnel is on
// replica B; the sessions are on replica A: F declared form elicitation, R did not. Asserted:
//
//   - F's task-augmented hire of booking is input_required on tasks/get, with the question as statusMessage; tasks/result
//     sends one elicitation/create {mode form, message = the question, requestedSchema {type object, properties {reply:
//     {type string}}, required [reply]}} with the id k.<task>.<seq> of the event that asked and the related-task _meta;
//   - decline leaves the task input-required and sends nothing upstream; on a new tasks/result the same question comes
//     again, and accept {reply: "2026-11-02"} is the follow-up: it reaches the same upstream task (one upstream task id
//     on the task's attempts) and the task completes with the booking, which that tasks/result answers;
//   - R's tasks/get shows working, with statusMessage the question followed by " (answer with the reply tool, taskId
//     <id>)" and _meta sh.kontra/input-required true; reply answers the task's new state, here the completed outcome,
//     within the 25 s wait;
//   - reply to a completed task and to a working one is isError and forwards nothing.
func TestGateMCPReply(t *testing.T) {
	seller := StartSeller(t)
	h := start(t, Options{Tunnel: map[string]TunnelService{"booking": {}, "work": {}}})
	ctx := ctxFor(t, 10*time.Minute)
	a, b := h.Gates[0], h.Gates[1]
	booking, err := h.StartExampleSeller(ctx, "booking")
	if err != nil {
		t.Fatal(err)
	}
	hostOn(t, ctx, h, b, map[string]string{"booking": booking.URL, "work": seller.URL})

	f := OpenGateSession(t, ctx, a, h.APIKey, CapsFormOnly)
	r := OpenGateSession(t, ctx, a, h.APIKey, `{}`)
	hire := func(s *GateSession, what string) string {
		t.Helper()
		ct := Tool(t, s.ToolTask("hire", `{"service":"booking","message":"`+what+`"}`))
		if ct.Task == nil || ct.Task.TaskID == "" {
			t.Fatalf("a task-augmented hire of booking: %+v", ct)
		}
		id := ct.Task.TaskID
		eventually(t, 30*time.Second, "booking asks its question", func() bool { return taskState(t, h, id) == "input-required" })
		return id
	}
	ft, rt := hire(f, "a table for two"), hire(r, "a room")
	question := `For which date shall I book "a table for two"? Reply with the date as YYYY-MM-DD.`
	upstreamTasks := func(id string) int64 {
		return count(t, h, `select count(distinct upstream_task_id) from kontra.attempts where task_id::text = $1 and upstream_task_id is not null`, id)
	}
	workItems := func(id string) int64 {
		return count(t, h, `select count(*) from kontra.work_items where task_id::text = $1`, id)
	}

	// F: input_required; tasks/result sends the form elicitation.
	if g := GetTask(t, f.Rpc("tasks/get", `{"taskId":"`+ft+`"}`)); g.Status != "input_required" || g.StatusMessage != question {
		t.Fatalf("F's tasks/get of the question: %+v", g)
	}
	seq := scalar[int64](t, h, `select max(seq) from kontra.task_events where task_id::text = $1 and kind in ('task', 'status')`, ft)
	gateID := fmt.Sprintf("k.%s.%d", ft, seq)
	elicit := func() (*MCPStream, map[string]any) {
		t.Helper()
		st, err := f.Open(ctx, http.MethodPost, `{"jsonrpc":"2.0","id":"r-`+randomHex(3)+`","method":"tasks/result","params":{"taskId":"`+ft+`"}}`, nil)
		if err != nil {
			t.Fatal(err)
		}
		m, _ := firstRequest(t, ctx, st, 30*time.Second)
		return st, m
	}
	st1, req := elicit()
	if req["method"] != "elicitation/create" || req["id"] != gateID {
		t.Fatalf("tasks/result's request: %v, want elicitation/create with id %s", req, gateID)
	}
	params, _ := req["params"].(map[string]any)
	schema, _ := json.Marshal(params["requestedSchema"])
	rel, _ := params["_meta"].(map[string]any)["io.modelcontextprotocol/related-task"].(map[string]any)
	if params["mode"] != "form" || params["message"] != question ||
		string(schema) != `{"properties":{"reply":{"type":"string"}},"required":["reply"],"type":"object"}` || rel["taskId"] != ft {
		t.Fatalf("the form elicitation: %v (schema %s)", params, schema)
	}
	items := workItems(ft)
	if st := f.Raw(`{"jsonrpc":"2.0","id":"` + gateID + `","result":{"action":"decline"}}`); st != http.StatusAccepted {
		t.Errorf("F's decline: %d", st)
	}
	time.Sleep(2 * time.Second)
	st1.Close()
	if s := taskState(t, h, ft); s != "input-required" || workItems(ft) != items {
		t.Fatalf("after a decline the task is %s with %d work items (was %d): it must wait, nothing sent", s, workItems(ft), items)
	}
	st2, req := elicit()
	defer st2.Close()
	if req["method"] != "elicitation/create" || req["id"] != gateID {
		t.Fatalf("the second tasks/result's request: %v", req)
	}
	if st := f.Raw(`{"jsonrpc":"2.0","id":"` + gateID + `","result":{"action":"accept","content":{"reply":"2026-11-02"}}}`); st != http.StatusAccepted {
		t.Errorf("F's accept: %d", st)
	}
	done := answerOn(t, ctx, st2, 60*time.Second)
	raw, _ := json.Marshal(done)
	var fin ToolResult
	if err := json.Unmarshal(raw, &struct {
		Result *ToolResult `json:"result"`
	}{&fin}); err != nil || fin.IsError || fin.TaskID() != ft || fin.State() != "TASK_STATE_COMPLETED" ||
		!strings.Contains(fin.Text(), `Confirmed: Booked "a table for two" on Monday, 2026-11-02.`) {
		t.Fatalf("the tasks/result after the accept: %s", raw)
	}
	if s := taskState(t, h, ft); s != "completed" {
		t.Fatalf("after the accept the task is %s", s)
	}
	if n := upstreamTasks(ft); n != 1 {
		t.Errorf("the follow-up reached %d upstream tasks, want the same one", n)
	}

	// R: working with the reply-tool text; reply answers the outcome within the wait.
	g := GetTask(t, r.Rpc("tasks/get", `{"taskId":"`+rt+`"}`))
	rq := `For which date shall I book "a room"? Reply with the date as YYYY-MM-DD.`
	if g.Status != "working" || g.StatusMessage != rq+" (answer with the reply tool, taskId "+rt+")" || g.Meta["sh.kontra/input-required"] != true {
		t.Fatalf("R's tasks/get of the question: %+v", g)
	}
	began := time.Now()
	rr := Tool(t, r.Tool("reply", `{"taskId":"`+rt+`","message":"2026-11-03"}`))
	if rr.IsError || rr.TaskID() != rt || rr.State() != "TASK_STATE_COMPLETED" || !strings.Contains(rr.Text(), `Confirmed: Booked "a room" on Tuesday, 2026-11-03.`) ||
		time.Since(began) > 25*time.Second {
		t.Fatalf("R's reply after %v: %+v", time.Since(began), rr)
	}
	if n := upstreamTasks(rt); n != 1 {
		t.Errorf("R's reply reached %d upstream tasks, want the same one", n)
	}

	// reply to a completed task and to a working one: isError, nothing forwarded.
	ct := Tool(t, r.ToolTask("hire", `{"service":"work","message":"slow"}`))
	working := ct.Task.TaskID
	eventually(t, 30*time.Second, "the slow task works", func() bool { return taskState(t, h, working) == "working" })
	turns := len(seller.Turns())
	for _, id := range []string{ft, working} {
		before := workItems(id)
		x := Tool(t, r.Tool("reply", `{"taskId":"`+id+`","message":"2026-11-04"}`))
		if !x.IsError {
			t.Errorf("reply to the %s task: %+v", taskState(t, h, id), x)
		}
		if workItems(id) != before {
			t.Errorf("reply to the %s task wrote a work item", taskState(t, h, id))
		}
	}
	time.Sleep(time.Second)
	if n := len(seller.Turns()); n != turns {
		t.Errorf("reply to a working task reached the seller: %d turns, was %d", n, turns)
	}
	_ = r.Rpc("tasks/cancel", `{"taskId":"`+working+`"}`)
}
