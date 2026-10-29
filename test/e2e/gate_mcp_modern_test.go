package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/gatemcp"
)

// gate_mcp_modern (spec/relay.md §16; §12.5, §11.12; D24, D178, D269, D274, D295; the corrections' WP-05 rule).
// Single tenant. 2026-07-28 requests (no session) to /mcp on alternating replicas; the sellers (the TCK seller, the
// in-test seller's slow exec, booking, and paid behind a quoting Admission) hosted by a node whose tunnel is on replica
// B. Asserted:
//
//   - server/discover lists [2026-07-28, 2025-11-25, 2025-06-18] and the tasks extension; the modern tools/list has no
//     execution; both, and a task's tasks/get, are byte-identical from either replica;
//   - a hire declaring the tasks extension is answered in line (resultType complete) when the task ends within 2 s,
//     else with CreateTaskResult (resultType task, pollIntervalMs 1000);
//   - a quoted hire is working with the URL in statusMessage and _meta sh.kontra/approve-url, never -32042, even to a
//     client that declares URL elicitation; tasks/update on it, with the quoted event's gate key or any other, is the
//     empty acknowledgement, forwards nothing and leaves it quoted;
//   - booking's question, to a client declaring form elicitation, is input_required with exactly one inputRequests
//     entry, keyed k.<task>.<seq>; tasks/update with keys that are not that one (a later seq, seq 0, a key that is no
//     gate id) is the empty acknowledgement, forwards nothing and leaves the question outstanding; tasks/update answers
//     it once: a second answer to the same key is acknowledged and ignored, and the task completes with the first;
//   - tasks/result and tasks/list are 404 with -32601; a _meta protocolVersion that differs from the header is 400 with
//     -32020; a legacy request without a session is 400 with -32000; a hire without the tasks extension is a plain
//     call (a CallToolResult, never a CreateTaskResult);
//   - a client that closes its stream during the wait gets no answer, and its task is canceled within 10 s with the
//     seller receiving CancelTask; a quoted one stays quoted and the re-issued hire answers it;
//   - no modern request writes an mcp_sessions row.
func TestGateMCPModern(t *testing.T) {
	adm, err := StartAdmission("paid")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adm.Close() })
	seller, paid := StartSeller(t), StartSeller(t)
	h := start(t, Options{Tunnel: map[string]TunnelService{"tck": {}, "booking": {}, "kinds": {}, "paid": {}}, Admission: adm})
	ctx := ctxFor(t, 10*time.Minute)
	a, b := h.Gates[0], h.Gates[1]
	tck, err := h.StartExampleSeller(ctx, "tck")
	if err != nil {
		t.Fatal(err)
	}
	booking, err := h.StartExampleSeller(ctx, "booking")
	if err != nil {
		t.Fatal(err)
	}
	hostOn(t, ctx, h, b, map[string]string{"tck": tck.URL, "booking": booking.URL, "kinds": seller.URL, "paid": paid.URL})
	ca := NewGateModern(t, a, h.APIKey, CapsTasks)
	cb := ca.On(b)

	// server/discover and tools/list, byte-identical from either replica.
	for _, method := range []string{"server/discover", "tools/list"} {
		ra, rb := ca.Rpc(method, "", ""), cb.Rpc(method, "", "")
		if ra.Status != http.StatusOK || rb.Status != http.StatusOK || string(Answer(t, ra).Result) != string(Answer(t, rb).Result) {
			t.Fatalf("%s: %d %s / %d %s", method, ra.Status, ra.Text(), rb.Status, rb.Text())
		}
	}
	var disc struct {
		SupportedVersions []string        `json:"supportedVersions"`
		Capabilities      json.RawMessage `json:"capabilities"`
	}
	_ = json.Unmarshal(Answer(t, ca.Rpc("server/discover", "", "")).Result, &disc)
	if !reflect.DeepEqual(disc.SupportedVersions, []string{"2026-07-28", "2025-11-25", "2025-06-18"}) ||
		!strings.Contains(string(disc.Capabilities), `"io.modelcontextprotocol/tasks":{}`) {
		t.Errorf("server/discover: %+v %s", disc.SupportedVersions, disc.Capabilities)
	}
	if tl := Answer(t, cb.Rpc("tools/list", "", "")); strings.Contains(string(tl.Result), "execution") || !strings.Contains(string(tl.Result), `"name":"hire"`) {
		t.Errorf("the modern tools/list: %s", tl.Result)
	}

	// A fast hire with the extension, in line.
	began := time.Now()
	r := Tool(t, ca.Tool("hire", `{"service":"tck","message":"hi"}`))
	if r.IsError || r.ResultType != "complete" || r.State() != "TASK_STATE_COMPLETED" || !strings.HasPrefix(r.Text(), "Unhandled messageId prefix: ") ||
		time.Since(began) > 15*time.Second {
		t.Fatalf("a fast modern hire after %v: %+v", time.Since(began), r)
	}
	// A slow exec: CreateTaskResult after the 2 s wait; tasks/get the same from either replica.
	began = time.Now()
	slow := Task(t, cb.Tool("hire", `{"service":"kinds","message":"slow"}`))
	if took := time.Since(began); slow.ResultType != "task" || slow.Status != "working" || slow.PollIntervalMs != 1000 || slow.TaskID == "" || took < 2*time.Second {
		t.Fatalf("a slow modern hire after %v: %+v", took, slow)
	}
	ga, gb := ca.Rpc("tasks/get", slow.TaskID, `"taskId":"`+slow.TaskID+`"`), cb.Rpc("tasks/get", slow.TaskID, `"taskId":"`+slow.TaskID+`"`)
	if g := Task(t, ga); g.Status != "working" || string(Answer(t, ga).Result) != string(Answer(t, gb).Result) {
		t.Errorf("tasks/get of the slow task: %s / %s", ga.Text(), gb.Text())
	}
	_ = ca.Rpc("tasks/cancel", slow.TaskID, `"taskId":"`+slow.TaskID+`"`)

	// A quoted hire: working with the URL, never -32042, even declaring URL elicitation.
	cq := ca.With(CapsTasksBoth)
	qr := cq.Tool("hire", `{"service":"paid","message":"hi"}`)
	if code := ErrCode(t, qr); code != 0 {
		t.Fatalf("a quoted modern hire answered the error %d: %s", code, qr.Text())
	}
	q := Task(t, qr)
	approve := "https://app.kontra.sh/#/t/" + q.TaskID
	if q.ResultType != "task" || q.Status != "working" || q.StatusMessage != approve || q.Meta["sh.kontra/approve-url"] != approve {
		t.Fatalf("a quoted modern hire: %s", qr.Text())
	}
	quoted := func(what string) {
		t.Helper()
		if st := taskState(t, h, q.TaskID); st != "quoted" {
			t.Fatalf("after %s the quoted task is %s", what, st)
		}
		if n := paid.Messages(); n != 0 {
			t.Fatalf("after %s the seller received %d messages", what, n)
		}
	}
	quoted("the hire")
	qseq := scalar[int64](t, h, `select max(seq) from kontra.task_events where task_id::text = $1`, q.TaskID)
	for _, responses := range []string{
		fmt.Sprintf(`{"k.%s.%d":{"action":"accept","content":{"reply":"I accept"}}}`, q.TaskID, qseq),
		fmt.Sprintf(`{"k.%s.%d":{"action":"accept"}}`, q.TaskID, qseq),
		fmt.Sprintf(`{"k.%s.1":{"action":"accept","content":{"reply":"yes"}},"approve":{"action":"accept"}}`, q.TaskID),
	} {
		for _, c := range []*GateModern{cq, cq.On(b)} {
			u := c.Rpc("tasks/update", q.TaskID, `"taskId":"`+q.TaskID+`","inputResponses":`+responses)
			if u.Status != http.StatusOK || string(Answer(t, u).Result) != `{"resultType":"complete"}` {
				t.Errorf("tasks/update %s on the quoted task: %d %s", responses, u.Status, u.Text())
			}
		}
	}
	time.Sleep(time.Second)
	quoted("tasks/update on it")

	// booking's question, to a client declaring form elicitation: one gate-keyed input request, answered once.
	cf := ca.With(CapsTasksForm)
	ask := Task(t, cf.Tool("hire", `{"service":"booking","message":"a table"}`))
	eventually(t, 30*time.Second, "booking asks", func() bool { return taskState(t, h, ask.TaskID) == "input-required" })
	seq := scalar[int64](t, h, `select max(seq) from kontra.task_events where task_id::text = $1 and kind in ('task', 'status')`, ask.TaskID)
	key := fmt.Sprintf("k.%s.%d", ask.TaskID, seq)
	ga, gb = cf.Rpc("tasks/get", ask.TaskID, `"taskId":"`+ask.TaskID+`"`), cf.On(b).Rpc("tasks/get", ask.TaskID, `"taskId":"`+ask.TaskID+`"`)
	g := Task(t, ga)
	if g.Status != "input_required" || len(g.InputRequests) != 1 || g.InputRequests[key] == nil || string(Answer(t, ga).Result) != string(Answer(t, gb).Result) ||
		!strings.Contains(string(g.InputRequests[key]), `"mode":"form"`) {
		t.Fatalf("tasks/get of the question: %s / %s (want one request under %s)", ga.Text(), gb.Text(), key)
	}
	// Keys that are not the outstanding gate key, while the question is outstanding (the corrections' WP-05 rule): a
	// later seq, seq 0 and a key that is no gate id. The empty acknowledgement; nothing forwarded; still the question.
	stale := fmt.Sprintf(`{"k.%s.%d":{"action":"accept","content":{"reply":"2026-12-21"}},`, ask.TaskID, seq+7) +
		fmt.Sprintf(`"k.%s.0":{"action":"accept","content":{"reply":"2026-12-22"}},`, ask.TaskID) +
		`"reply":{"action":"accept","content":{"reply":"2026-12-23"}}}`
	if u := cf.On(b).Rpc("tasks/update", ask.TaskID, `"taskId":"`+ask.TaskID+`","inputResponses":`+stale); u.Status != http.StatusOK ||
		string(Answer(t, u).Result) != `{"resultType":"complete"}` {
		t.Fatalf("tasks/update with keys that are not the outstanding one: %d %s", u.Status, u.Text())
	}
	time.Sleep(time.Second)
	if st := taskState(t, h, ask.TaskID); st != "input-required" {
		t.Fatalf("after tasks/update with keys that are not the outstanding one the task is %s, want input-required", st)
	}
	if g := Task(t, cf.Rpc("tasks/get", ask.TaskID, `"taskId":"`+ask.TaskID+`"`)); g.Status != "input_required" || len(g.InputRequests) != 1 ||
		g.InputRequests[key] == nil {
		t.Fatalf("tasks/get after tasks/update with keys that are not the outstanding one: %+v (want the same request under %s)", g, key)
	}
	for i, date := range []string{"2026-11-02", "2026-11-05"} {
		c := cf
		if i == 1 {
			c = cf.On(b)
		}
		u := c.Rpc("tasks/update", ask.TaskID, `"taskId":"`+ask.TaskID+`","inputResponses":{"`+key+`":{"action":"accept","content":{"reply":"`+date+`"}}}`)
		if Answer(t, u).Error != nil || string(Answer(t, u).Result) != `{"resultType":"complete"}` {
			t.Fatalf("tasks/update %s: %s", date, u.Text())
		}
	}
	eventually(t, 30*time.Second, "the question's task completes", func() bool { return taskState(t, h, ask.TaskID) == "completed" })
	done := Task(t, cb.With(CapsTasksForm).Rpc("tasks/get", ask.TaskID, `"taskId":"`+ask.TaskID+`"`))
	if done.Status != "completed" || !strings.Contains(string(done.Result), "2026-11-02") || strings.Contains(string(done.Result), "2026-11-05") ||
		strings.Contains(string(done.Result), "2026-12-2") {
		t.Errorf("the answered task: %+v, want completed with the first answer only (none of the ignored keys' 2026-12-2x)", done)
	}

	// What the extension removes, the header checks, and rule 3.
	for _, c := range []struct{ method, name, params string }{{"tasks/result", ask.TaskID, `"taskId":"` + ask.TaskID + `"`}, {"tasks/list", "", ""}} {
		if rr := ca.Rpc(c.method, c.name, c.params); rr.Status != http.StatusNotFound || ErrCode(t, rr) != -32601 {
			t.Errorf("%s: %d %s", c.method, rr.Status, rr.Text())
		}
	}
	mm, err := ca.Post(ctx, "tools/list", "", `{"jsonrpc":"2.0","id":"m","method":"tools/list","params":{"_meta":{`+
		`"io.modelcontextprotocol/protocolVersion":"2025-11-25","io.modelcontextprotocol/clientCapabilities":{}}}}`, nil)
	if err != nil || mm.Status != http.StatusBadRequest || ErrCode(t, mm) != -32020 {
		t.Errorf("a mismatched protocolVersion: %v %v", mm, err)
	}
	for _, v := range []string{"", "2025-11-25"} {
		req, _ := http.NewRequest(http.MethodPost, a.URL+gatemcp.Path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		req.Header.Set("Authorization", "Bearer "+h.APIKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if v != "" {
			req.Header.Set("MCP-Protocol-Version", v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), `"code":-32000`) {
			t.Errorf("a legacy request (version %q) without a session: %d %s", v, resp.StatusCode, body)
		}
	}
	plain := Tool(t, ca.With(CapsNothing).Tool("hire", `{"service":"tck","message":"hi"}`))
	if plain.IsError || plain.Task != nil || plain.ResultType != "complete" || plain.State() != "TASK_STATE_COMPLETED" {
		t.Errorf("a hire without the extension: %+v, want a plain CallToolResult", plain)
	}

	// A client that leaves during the wait: no answer, its task canceled within 10 s, CancelTask upstream.
	leave := func(c *GateModern, args string, after time.Duration) string {
		t.Helper()
		before := count(t, h, `select count(*) from kontra.tasks where hired`)
		lctx, cancel := context.WithTimeout(ctx, after)
		defer cancel()
		if r, err := c.Post(lctx, "tools/call", "hire", c.Body("tools/call", `"name":"hire","arguments":`+args), nil); err == nil {
			t.Fatalf("the client that left got an answer: %s", r.Text())
		}
		eventually(t, 15*time.Second, "the hire's task exists", func() bool {
			return count(t, h, `select count(*) from kontra.tasks where hired`) == before+1
		})
		return scalar[string](t, h, `select id::text from kontra.tasks where hired order by created_at desc limit 1`)
	}
	for i, c := range []*GateModern{ca, ca.With(CapsNothing)} {
		cancels := seller.Count("CancelTask")
		id := leave(c, fmt.Sprintf(`{"service":"kinds","message":"slow","contextId":"leave-%d"}`, i), 1500*time.Millisecond)
		left := time.Now()
		eventually(t, 10*time.Second, "the task of the client that left is canceled", func() bool { return taskState(t, h, id) == "canceled" })
		t.Logf("canceled %v after the client left", time.Since(left).Round(10*time.Millisecond))
		eventually(t, 10*time.Second, "the seller received CancelTask", func() bool { return seller.Count("CancelTask") > cancels })
	}
	adm.SetDelay(1500 * time.Millisecond)
	qargs := `{"service":"paid","message":"hi","contextId":"left"}`
	left := leave(ca, qargs, 500*time.Millisecond)
	adm.SetDelay(0)
	eventually(t, 15*time.Second, "the left hire's task is quoted", func() bool { return taskState(t, h, left) == "quoted" })
	time.Sleep(3 * time.Second)
	if st := taskState(t, h, left); st != "quoted" {
		t.Fatalf("the quoted task of a client that left is %s", st)
	}
	re := Task(t, ca.Tool("hire", qargs))
	if re.TaskID != left || re.Status != "working" || re.Meta["sh.kontra/approve-url"] != "https://app.kontra.sh/#/t/"+left {
		t.Errorf("the re-issued hire: %+v", re)
	}

	if n := count(t, h, `select count(*) from kontra.mcp_sessions`); n != 0 {
		t.Errorf("modern /mcp requests wrote %d mcp_sessions rows", n)
	}
	if n := paid.Messages(); n != 0 {
		t.Errorf("the quoting seller received %d messages", n)
	}
}
