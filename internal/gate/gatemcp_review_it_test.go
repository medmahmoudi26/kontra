package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/gatemcp"
	"kontra.sh/kontra/internal/tenant"
)

// The review fixes of M4 WP-04 through a whole replica (spec §12):
//
//   - a hire whose message has a data part of JSON null is an invalid-arguments isError and writes no task (a2a-go
//     writes {"data":null} and cannot read it back: such a task could never be loaded, and the buyer's tasks/list
//     would answer 503 from then on); tasks/list still works;
//   - a hire of a service whose served card marks an extension of the seller's own required: true is isError naming
//     that URI (not the operator's card_extensions entry beside it, which a hire activates, D300) and the A2A route,
//     with no task row and nothing sent to the seller, as the A2A route refuses a SendMessage without A2A-Extensions
//     with -32008 (§2.4, D294);
//   - an API key with only enroll: scopes, sharing the buyer of a hire key (§2.3), is 403 on /mcp: initialize, and
//     tasks/list, tasks/get, tasks/result and tasks/cancel on the hire key's session; the hire key's task is not
//     cancelled;
//   - a 2025-06-18 session decides by its own version: a request naming 2025-11-25 in MCP-Protocol-Version is 400,
//     tasks/* are -32601 and hire has no execution (D293);
//   - M4's review (D303): /mcp follows what the replica's registry serves. A key that shares the hiring key's buyer but
//     that the service's allow: does not admit gets -32602 from tasks/get and tasks/cancel, sees nothing in tasks/list,
//     and its reply is "unknown task", as the A2A route answers it 404; so does the hiring key once its allow: entry is
//     removed, and once the service is disabled, its reply sending nothing and the task left at input-required. A row
//     the registry refused to build is no service to search, describe or hire, and a row whose allow: does not parse
//     fails no search;
//   - a reply answers the turn it started, every time (a loop of hire "ask" and reply; MCP-2).
func TestGateMCPReviewFixes(t *testing.T) {
	e := newGateEnv(t)
	ctx := context.Background()
	const escrow, own = "https://x.example.com/ext/escrow", "https://seller.example.com/ext/own"
	kinds, esc := newHireSeller(t), newHireSellerCaps(t, `{"streaming":true,"extensions":[{"uri":"`+own+`","required":true}]}`)
	services := e.hireServices(kinds, `  escrowed:
    kind: a2a
    backend: { a2a: { url: "`+esc.srv.URL+`/base", allow_private: true } }
    allow: ["org:default"]
    task: { timeout: 10m, retries: 1 }
    annotations: { card_extensions: [ { uri: "`+escrow+`", required: true } ] }
  narrow:
    kind: a2a
    backend: { a2a: { url: "`+kinds.srv.URL+`/base", allow_private: true } }
    allow: ["key:kk_buyer_*"]
    task: { timeout: 10m, retries: 1 }
`)
	timing := fastTiming
	timing.PlainWait = 4 * time.Second
	a := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), timing)
	tid, err := tenant.EnsureTeam(ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	keys := auth.NewAPIKeys(e.pool, nil)
	hire, err := keys.Mint(ctx, auth.MintRequest{TenantID: tid, Name: "buyer", Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	enroll, err := keys.Mint(ctx, auth.MintRequest{TenantID: tid, Name: "enroller", BuyerID: &hire.BuyerID,
		Scopes: []auth.Scope{auth.ScopeEnrollAll}, Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	c := newMCPClient(t, a.url, hire.Secret)
	c.initialize(`{}`)
	tasks := func() int64 { return scalar[int64](t, e.pool, `select count(*) from kontra.tasks`) }

	// A null data part.
	_, ans := c.call("hire", `{"service":"hello","message":"hi"}`)
	good := ans.tool(t)
	if good.IsError || good.Structured.TaskID == "" {
		t.Fatalf("a plain hire: %s", ans.raw)
	}
	before := tasks()
	for _, msg := range []string{`{"parts":[{"data":null}]}`, `{"parts":[{"text":"hi"},{"data":null,"mediaType":"application/json"}]}`} {
		_, ans = c.call("hire", `{"service":"hello","message":`+msg+`}`)
		if r := ans.tool(t); !r.IsError || !strings.Contains(r.text(), "data must not be null") {
			t.Errorf("a hire of %s: %s", msg, ans.raw)
		}
	}
	if n := tasks(); n != before {
		t.Errorf("hires with a null data part created %d tasks", n-before)
	}
	if status, ans := c.rpc("tasks/list", `{}`); status != http.StatusOK || ans.Error != nil || !strings.Contains(string(ans.Result), good.Structured.TaskID) {
		t.Errorf("tasks/list after the refused hires: %d %s", status, ans.raw)
	}

	// A required extension of the seller's own. The control: the A2A route refuses a SendMessage that does not
	// activate it.
	eventually(t, 20*time.Second, "the served card declares the required extensions", func() bool {
		req, _ := http.NewRequest(http.MethodGet, a.url+"/agents/escrowed/.well-known/agent-card.json", nil)
		req.Header.Set("Authorization", "Bearer "+hire.Secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode == http.StatusOK && strings.Contains(string(b), escrow) && strings.Contains(string(b), own)
	})
	req, _ := http.NewRequest(http.MethodPost, a.url+"/agents/escrowed", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m-esc","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`))
	req.Header.Set("Authorization", "Bearer "+hire.Secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"code":-32008`) {
		t.Fatalf("the control, A2A SendMessage without A2A-Extensions: %d %s", resp.StatusCode, b)
	}
	before = tasks()
	for _, call := range []string{`{"name":"hire","arguments":{"service":"escrowed","message":"hi"}}`,
		`{"name":"hire","arguments":{"service":"escrowed","message":"hi"},"task":{}}`} {
		_, ans = c.rpc("tools/call", call)
		if r := ans.tool(t); !r.IsError || !strings.Contains(r.text(), own) || strings.Contains(r.text(), escrow) ||
			!strings.Contains(r.text(), a.url+"/agents/escrowed") || !strings.Contains(r.text(), "No task was created") {
			t.Errorf("hire of a service requiring %s (%s): %s", own, call, ans.raw)
		}
	}
	time.Sleep(time.Second)
	if n := tasks(); n != before {
		t.Errorf("hires of the service requiring an extension created %d tasks", n-before)
	}
	if n := esc.count("SendMessage") + esc.count("SendStreamingMessage"); n != 0 {
		t.Errorf("the seller of the service requiring an extension received %d messages", n)
	}

	// An enroll-only key sharing the hire key's buyer.
	_, ans = c.rpc("tools/call", `{"name":"hire","arguments":{"service":"kinds","message":"slow"},"task":{}}`)
	slow := ans.tool(t)
	if slow.Task == nil {
		t.Fatalf("a task-augmented hire: %s", ans.raw)
	}
	eo := newMCPClient(t, a.url, enroll.Secret)
	status, hdr, msgs := eo.post(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if status != http.StatusForbidden || len(msgs) != 1 || !strings.Contains(string(msgs[0]), gatemcp.MsgMayNotHire) {
		t.Errorf("initialize with an enroll-only key: %d %s", status, msgs)
	}
	// Its own session, had initialize opened one (the defect: its tasks/* read the shared buyer's hire tasks), else the
	// hire key's, whose id it learnt.
	if eo.sid = hdr.Get("Mcp-Session-Id"); eo.sid == "" {
		eo.sid = c.sid
	}
	for _, m := range []struct{ method, params string }{
		{"tasks/list", `{}`}, {"tasks/get", `{"taskId":"` + slow.Task.TaskID + `"}`},
		{"tasks/result", `{"taskId":"` + good.Structured.TaskID + `"}`}, {"tasks/cancel", `{"taskId":"` + slow.Task.TaskID + `"}`},
		{"tools/call", `{"name":"reply","arguments":{"taskId":"` + slow.Task.TaskID + `","message":"x"}}`},
	} {
		if status, ans := eo.rpc(m.method, m.params); status != http.StatusForbidden || ans.Result != nil {
			t.Errorf("%s with an enroll-only key: %d %s", m.method, status, ans.raw)
		}
	}
	time.Sleep(time.Second)
	if st := scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, slow.Task.TaskID); st != "submitted" && st != "working" {
		t.Errorf("after the enroll-only key's tasks/cancel the hire task is %s, want it running", st)
	}
	if n := kinds.count("CancelTask"); n != 0 {
		t.Errorf("after the enroll-only key's tasks/cancel the seller received %d CancelTask", n)
	}
	if _, ans = c.rpc("tasks/cancel", `{"taskId":"`+slow.Task.TaskID+`"}`); !strings.Contains(string(ans.Result), `"status":"cancelled"`) {
		t.Errorf("the hire key's tasks/cancel: %s", ans.raw)
	}

	// D303: /mcp follows the registry. narrow admits key:kk_buyer_* (the hire key); another key of the same buyer is
	// refused by its allow:.
	other, err := keys.Mint(ctx, auth.MintRequest{TenantID: tid, Name: "other", BuyerID: &hire.BuyerID, Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	_, ans = c.call("hire", `{"service":"narrow","message":"ask"}`)
	q := ans.tool(t)
	if q.IsError || q.Structured.State != "TASK_STATE_INPUT_REQUIRED" {
		t.Fatalf("a hire of narrow that asks: %s", ans.raw)
	}
	state := func() string {
		return scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, q.Structured.TaskID)
	}
	cancels := kinds.count("CancelTask")
	turns := len(kinds.turnTexts())
	unknown := func(what string, cl *mcpClient) {
		t.Helper()
		if _, ans := cl.rpc("tasks/get", `{"taskId":"`+q.Structured.TaskID+`"}`); ans.Error == nil || ans.Error.Code != -32602 {
			t.Errorf("%s: tasks/get %s", what, ans.raw)
		}
		if _, ans := cl.rpc("tasks/list", `{}`); ans.Error != nil || strings.Contains(string(ans.Result), q.Structured.TaskID) {
			t.Errorf("%s: tasks/list %s", what, ans.raw)
		}
		if _, ans := cl.rpc("tasks/cancel", `{"taskId":"`+q.Structured.TaskID+`"}`); ans.Error == nil || ans.Error.Code != -32602 {
			t.Errorf("%s: tasks/cancel %s", what, ans.raw)
		}
		if _, ans := cl.call("reply", `{"taskId":"`+q.Structured.TaskID+`","message":"tomorrow"}`); !ans.tool(t).IsError || ans.tool(t).text() != "unknown task" {
			t.Errorf("%s: reply %s", what, ans.raw)
		}
		time.Sleep(500 * time.Millisecond)
		if st := state(); st != "input-required" || kinds.count("CancelTask") != cancels || len(kinds.turnTexts()) != turns {
			t.Fatalf("%s: the task is %s, the seller saw %d cancels and %d turns (before: %d, %d)", what, st,
				kinds.count("CancelTask"), len(kinds.turnTexts()), cancels, turns)
		}
	}
	oc := newMCPClient(t, a.url, other.Secret)
	oc.initialize(`{}`)
	unknown("a key of the same buyer that allow: does not admit", oc)
	// The control: the A2A route answers that key 404 unknown service.
	req, _ = http.NewRequest(http.MethodPost, a.url+"/agents/narrow", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"`+q.Structured.TaskID+`"}}`))
	req.Header.Set("Authorization", "Bearer "+other.Secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("the control, A2A GetTask by the other key: %d", resp.StatusCode)
	}
	if _, ans = c.rpc("tasks/get", `{"taskId":"`+q.Structured.TaskID+`"}`); ans.Error != nil {
		t.Fatalf("the hiring key's tasks/get: %s", ans.raw)
	}
	// The row changes as an operator would change it (NOTIFY kontra_services), and the registry follows.
	change := func(sql, what string, served bool) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.Exec(ctx, `select pg_notify('kontra_services', 'default/narrow')`); err != nil {
			t.Fatal(err)
		}
		// The registry's view, as the A2A route reads it (describe would read the row itself).
		eventually(t, 20*time.Second, what, func() bool {
			req, _ := http.NewRequest(http.MethodGet, a.url+"/agents/narrow/.well-known/agent-card.json", nil)
			req.Header.Set("Authorization", "Bearer "+hire.Secret)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return false
			}
			resp.Body.Close()
			return (resp.StatusCode == http.StatusNotFound) != served
		})
	}
	change(`update kontra.services set allow = '{key:kk_nobody_*}' where name = 'narrow'`, "the allow: entry removed", false)
	unknown("the hiring key once its allow: entry is removed", c)
	change(`update kontra.services set allow = '{key:kk_buyer_*}' where name = 'narrow'`, "the allow: entry back", true)
	change(`update kontra.services set disabled_at = now() where name = 'narrow'`, "the service disabled", false)
	unknown("the hiring key once the service is disabled", c)

	// A row the registry refuses to build (an annotation it cannot read): no service to search, describe or hire, and no
	// task (MCP-4). A row whose allow: does not parse fails no search (MCP-3).
	if _, err := e.pool.Exec(ctx, `insert into kontra.services (tenant_id, name, kind, backend, allow, source, annotations)
		values ($1, 'refused', 'a2a', $2::jsonb, '{org:default}', 'db', '{"card_extensions":"x"}'),
		       ($1, 'unparsed', 'a2a', $2::jsonb, '{oidc:sub:*}', 'db', '{}')`, tid, `{"a2a": {"url": "`+kinds.srv.URL+`/base", "allow_private": true}}`); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"refused", "unparsed"} {
		if _, err := e.pool.Exec(ctx, `select pg_notify('kontra_services', $1)`, "default/"+n); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(2 * time.Second)
	before = tasks()
	_, ans = c.call("search", `{}`)
	if r := ans.tool(t); r.IsError || ans.Error != nil || strings.Contains(r.text(), `"refused"`) || strings.Contains(r.text(), `"unparsed"`) ||
		!strings.Contains(r.text(), `"kinds"`) {
		t.Errorf("search beside a refused and an unparsed row: %s", ans.raw)
	}
	for _, n := range []string{"refused", "unparsed"} {
		if _, ans := c.call("describe", `{"service":"`+n+`"}`); !ans.tool(t).IsError || !strings.Contains(ans.tool(t).text(), "unknown service") {
			t.Errorf("describe of %s: %s", n, ans.raw)
		}
		if _, ans := c.call("hire", `{"service":"`+n+`","message":"hi"}`); !ans.tool(t).IsError || !strings.Contains(ans.tool(t).text(), "unknown service") {
			t.Errorf("hire of %s: %s", n, ans.raw)
		}
	}
	if n := tasks(); n != before {
		t.Errorf("hires of rows the registry does not serve created %d tasks", n-before)
	}

	// MCP-2: a reply answers the turn it started, never the question before it.
	for i := range 40 {
		_, ans := c.call("hire", `{"service":"kinds","message":"ask"}`)
		q := ans.tool(t)
		if q.IsError || q.Structured.State != "TASK_STATE_INPUT_REQUIRED" {
			t.Fatalf("hire %d: %s", i, ans.raw)
		}
		day := fmt.Sprintf("day %d", i)
		_, ans = c.call("reply", `{"taskId":"`+q.Structured.TaskID+`","message":"`+day+`"}`)
		if r := ans.tool(t); r.IsError || r.Structured.State != "TASK_STATE_COMPLETED" || r.text() != "booked for "+day {
			t.Fatalf("reply %d answered %s, want the completed turn", i, ans.raw)
		}
	}

	// A 2025-06-18 session.
	old := newMCPClient(t, a.url, hire.Secret)
	old.version = "2025-06-18"
	old.initialize(`{}`)
	newer := *old
	newer.version = "2025-11-25"
	for _, m := range []struct{ method, params string }{{"tasks/list", `{}`}, {"tools/list", `{}`},
		{"tools/call", `{"name":"hire","arguments":{"service":"hello","message":"hi"},"task":{}}`}} {
		status, ans := newer.rpc(m.method, m.params)
		if status != http.StatusBadRequest || ans.Error == nil || ans.Error.Code != -32022 {
			t.Errorf("%s naming 2025-11-25 on a 2025-06-18 session: %d %s", m.method, status, ans.raw)
		}
	}
	if _, ans = old.rpc("tasks/list", `{}`); ans.Error == nil || ans.Error.Code != -32601 {
		t.Errorf("tasks/list on a 2025-06-18 session: %s", ans.raw)
	}
	_, ans = old.rpc("tools/list", `{}`)
	var tl struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(ans.Result, &tl); err != nil || len(tl.Tools) != 4 {
		t.Fatalf("tools/list on a 2025-06-18 session: %s", ans.raw)
	}
	for _, tool := range tl.Tools {
		if _, ok := tool["execution"]; ok {
			t.Errorf("a 2025-06-18 session's tools/list gives %s execution", tool["name"])
		}
	}
}
