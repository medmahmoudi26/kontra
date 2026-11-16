package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/gatemcp"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
)

// gate_mcp_send_message (spec/relay.md §16; §12.2, §12.3 step 1, §13.1; D272, D275, D289, D293, D294, D300). Single tenant.
// A legacy /mcp session with an API key on replica A; the operator's Admission is the e2e double; the services hosted by a
// node whose tunnel is on replica B: the TCK service (examples/agents/tck, a stock service that completes at once) and
// the in-test service (the TCK service's artifact kinds, a slow exec, a failing exec). Asserted, clause by clause:
//
//   - a plain send_message that ends within the wait answers the rendered outcome and structuredContent exactly {taskId, state,
//     contextId, service, text} (no data without data parts; the text the content's, D324), the task being an ordinary A2A task marked
//     called;
//   - a plain send_message still running after the 25 s wait answers the send_message result of a task still running: structuredContent
//     exactly {taskId, state: working, contextId, service, text}, the text the content's, which names the A2A URL that
//     follows it (D324);
//   - a task-augmented send_message answers CreateTaskResult; tasks/result (on B) renders text parts as text, data parts as JSON
//     text and in structuredContent.data, a raw file part as an embedded resource and a URL part as a resource_link;
//     structuredContent is {taskId, state, text, data, files};
//   - a failed upstream is a CallToolResult with isError and _meta sh.kontra/failure, never a JSON-RPC error;
//   - tasks/list lists only this caller's send_message tasks, newest first: not the caller's ordinary A2A task (SendMessage with
//     the same API key), whose tasks/get is -32602 as an unknown task's; another caller's tasks/list is empty and its
//     tasks/get of this caller's task is -32602;
//   - tasks/cancel ends a working task cancelled within 10 s and the service receives CancelTask; an A2A SubscribeToTask
//     of the same task with the same key shows exactly the log's carried seqs, up to the cancel;
//   - a kind: mcp ref, a required-sealed ref, a ref whose served card requires an extension of the service's own and a
//     null data part are isError with no task row (the operator's required card_extensions entry beside the service's
//     is not named: a send_message activates it for the caller, D300); a ref whose only required extension is the operator's
//     is called, and its service receives no A2A-Extensions;
//   - the operator's Admission server (the e2e double, which allows) down, or answering a decision hooks.json does not
//     define (outcome quote, D351's D1; a body that is not JSON): send_message is -32603 admission unavailable (HTTP 503), and
//     no task row is written; with the server up again a send_message is admitted;
//   - /mcp's tools write no calls rows, and every task send_message created is marked sent_by_mcp (the A2A task is the only one not);
//   - a request without a credential is 401 (Bearer) and opens no session; an API key with only enroll: scopes is 403.
func TestGateMCPSendMessage(t *testing.T) {
	const operatorExt, own = "https://operator.example.com/ext/terms/v1", "https://service.example.com/ext/own"
	service, esc, paid := StartService(t), StartServiceCaps(t, `{"streaming":true,"extensions":[{"uri":"`+own+`","required":true}]}`), StartService(t)
	adm, err := StartAdmission()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adm.Close() })
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	web := httptest.NewServer(up)
	t.Cleanup(web.Close)
	operatorEntry := `    annotations: { card_extensions: [ { uri: "` + operatorExt + `", required: true } ] }` + "\n"
	h := start(t, Options{
		Tunnel:      map[string]TunnelService{"tck": {}, "kinds": {}, "vault": {Sealed: "true"}},
		Direct:      map[string]string{"ownext": esc.URL, "paid": paid.URL},
		MCP:         map[string]MCPService{"web": {URL: web.URL + "/mcp"}},
		ServiceYAML: map[string]string{"ownext": operatorEntry, "paid": operatorEntry},
		Admission:   adm,
	})
	ctx := ctxFor(t, 10*time.Minute)
	a, b := h.Gates[0], h.Gates[1]
	tck, err := h.StartExampleService(ctx, "tck")
	if err != nil {
		t.Fatal(err)
	}
	hostOn(t, ctx, h, b, map[string]string{"tck": tck.URL, "kinds": service.URL})

	// No credential: 401 with the Bearer challenge, and no session row (D270).
	sessions := func() int64 { return count(t, h, `select count(*) from kontra.mcp_sessions`) }
	if st, www := noCredentialPost(t, a.URL+gatemcp.Path, initializeBody); st != http.StatusUnauthorized || !strings.HasPrefix(www, "Bearer") {
		t.Fatalf("/mcp without a credential: %d %q", st, www)
	}
	if n := sessions(); n != 0 {
		t.Fatalf("a request without a credential wrote %d mcp_sessions rows", n)
	}
	// An API key with only enroll: scopes: 403, before any session (D293).
	out, err := h.gateStdout(ctx, "enroll", "-f", h.configPath, "--api-key", "enroller", "--scope", "enroll:*")
	if err != nil {
		t.Fatal(err)
	}
	enroller := strings.TrimSpace(string(out))
	ec := NewMCPClient(a.URL+gatemcp.Path, enroller)
	if r, _ := ec.Post(ctx, initializeBody); r == nil || r.Status != http.StatusForbidden || ErrCode(t, r) != -32600 {
		t.Fatalf("/mcp with an enroll-only key: %v", r)
	}
	if n := sessions(); n != 0 {
		t.Fatalf("the enroll-only key opened %d sessions", n)
	}

	s := OpenGateSession(t, ctx, a, h.APIKey, `{}`)
	sb := s.On(b)

	// A plain send_message that completes at once, on the TCK service: rendered text, structuredContent {taskId, state, text}, the
	// text the content's (D324).
	r := Tool(t, s.Tool("send_message", `{"service":"tck","message":"tck-complete-task hi"}`))
	if r.IsError || r.State() != "TASK_STATE_COMPLETED" || r.TaskID() == "" || !strings.HasPrefix(r.Text(), "Unhandled messageId prefix: ") {
		t.Fatalf("a plain fast send_message: %+v", r)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(r.StructuredRaw, &keys); err != nil || len(keys) != 5 || keys["taskId"] == nil || keys["state"] == nil ||
		keys["contextId"] == nil || string(keys["service"]) != `"tck"` ||
		r.Structured.Text != r.Text() {
		t.Errorf("structuredContent of a send_message without data parts: %s, want exactly {taskId, state, contextId, service, text}", r.StructuredRaw)
	}
	fast := r.TaskID()
	if !scalar[bool](t, h, `select sent_by_mcp and protocol = 'a2a' and first_request is not null from kontra.tasks where id::text = $1`, fast) {
		t.Error("the send_message task is not an A2A task marked sent_by_mcp with the send_message's resend key")
	}

	// A plain send_message of a slow exec: after the 25 s wait, the send_message result of a task still running, its text (the content's)
	// naming the A2A URL that follows it (D324).
	began := time.Now()
	r = Tool(t, s.Tool("send_message", `{"service":"kinds","message":"slow"}`))
	took := time.Since(began)
	if r.IsError || r.State() != "TASK_STATE_WORKING" || r.TaskID() == "" || !strings.Contains(r.Text(), h.Front.URL+"/agents/kinds") ||
		took < 20*time.Second || took > 50*time.Second {
		t.Fatalf("a plain slow send_message after %v: %+v", took, r)
	}
	keys = nil
	if err := json.Unmarshal(r.StructuredRaw, &keys); err != nil || len(keys) != 5 || keys["taskId"] == nil || keys["state"] == nil ||
		keys["contextId"] == nil || string(keys["service"]) != `"kinds"` ||
		r.Structured.Text != r.Text() || !strings.Contains(r.Structured.Text, "with GetTask at "+h.Front.URL+"/agents/kinds") {
		t.Errorf("structuredContent of a plain send_message still running: %s, want exactly {taskId, state, contextId, service, text}, the text naming GetTask", r.StructuredRaw)
	}
	slowPlain := r.TaskID()

	// A task-augmented send_message of the artifact kinds: CreateTaskResult, then tasks/result on replica B.
	ct := Tool(t, s.ToolTask("send_message", `{"service":"kinds","message":"kinds"}`))
	if ct.Task == nil || ct.Task.TaskID == "" || ct.Task.Status != "working" || ct.Task.PollInterval <= 0 {
		t.Fatalf("a task-augmented send_message: %+v", ct)
	}
	kinds := ct.Task.TaskID
	if g := GetTask(t, sb.Rpc("tasks/get", `{"taskId":"`+kinds+`"}`)); g.TaskID != kinds {
		t.Errorf("tasks/get on B: %+v", g)
	}
	res := Tool(t, sb.Rpc("tasks/result", `{"taskId":"`+kinds+`"}`))
	var types []string
	for _, c := range res.Content {
		types = append(types, c.Type)
	}
	if res.IsError || fmt.Sprint(types) != "[text text text text resource resource_link]" {
		t.Fatalf("tasks/result's content types %v: %+v", types, res)
	}
	jsonEq := func(a, b string) bool {
		var x, y any
		return json.Unmarshal([]byte(a), &x) == nil && json.Unmarshal([]byte(b), &y) == nil && fmt.Sprint(x) == fmt.Sprint(y)
	}
	if res.Content[0].Text != "done" || !jsonEq(res.Content[1].Text, `{"total":4}`) || res.Content[2].Text != "Generated text content" ||
		!jsonEq(res.Content[3].Text, `{"key":"value","count":42}`) {
		t.Errorf("tasks/result renders text as text and data as JSON text: %+v", res.Content)
	}
	if rs := res.Content[4].Resource; rs == nil || rs.Blob != "dGNr" || rs.MimeType != "text/plain" || !strings.HasPrefix(rs.URI, "kontra:task/"+kinds+"/") {
		t.Errorf("tasks/result renders a raw file part as an embedded resource: %+v", res.Content[4])
	}
	if res.Content[5].URI != "https://example.com/output.txt" {
		t.Errorf("tasks/result renders a URL part as a resource_link: %+v", res.Content[5])
	}
	if res.TaskID() != kinds || res.State() != "TASK_STATE_COMPLETED" || fmt.Sprint(res.Structured.Data) != "[map[total:4] map[count:42 key:value]]" {
		t.Errorf("tasks/result's structuredContent %s, want {taskId, state, data: [the data parts' values]}", res.StructuredRaw)
	}

	// A failing exec: a CallToolResult with isError and sh.kontra/failure, no JSON-RPC error.
	fr := s.Tool("send_message", `{"service":"kinds","message":"fail"}`)
	if code := ErrCode(t, fr); code != 0 {
		t.Fatalf("a failed upstream answered the JSON-RPC error %d: %s", code, fr.Text())
	}
	f := Tool(t, fr)
	if reason, _ := f.Meta["sh.kontra/failure"].(string); !f.IsError || reason == "" || f.State() != "TASK_STATE_FAILED" ||
		!strings.Contains(f.Text(), "the exec failed") {
		t.Errorf("a failed upstream: %+v", f)
	}
	failed := f.TaskID()

	// A slow task-augmented send_message, followed by A2A SubscribeToTask on B with the same key, then tasks/cancel on A.
	ct = Tool(t, s.ToolTask("send_message", `{"service":"kinds","message":"slow"}`))
	slow := ct.Task.TaskID
	eventually(t, 30*time.Second, "the slow task works", func() bool { return taskState(t, h, slow) == "working" })
	carried, err := h.CarriedSeqs(ctx, a2a.TaskID(slow))
	if err != nil || len(carried) == 0 {
		t.Fatalf("the slow task's log: %v %v", carried, err)
	}
	k := carried[0] // the task event: the A2A stream resumes after it with every carried row (§9)
	stream, err := h.CallerA().Subscribe(ctx, b, a2a.TaskID(slow), &k, Call{Service: "kinds"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	cancels := service.Count("CancelTask")
	began = time.Now()
	cr := s.Rpc("tasks/cancel", `{"taskId":"`+slow+`"}`)
	if a := Answer(t, cr); a.Error != nil || !strings.Contains(string(a.Result), `"status":"cancelled"`) || time.Since(began) > 10*time.Second {
		t.Fatalf("tasks/cancel after %v: %s", time.Since(began), cr.Text())
	}
	eventually(t, 10*time.Second, "the service received CancelTask", func() bool { return service.Count("CancelTask") > cancels })
	if st := taskState(t, h, slow); st != "canceled" {
		t.Fatalf("after tasks/cancel the task is %s", st)
	}
	evs, err := stream.ReadAll(ctx)
	if err != nil || len(evs) < 2 {
		t.Fatalf("the A2A stream of the send_message task: %d events, %v", len(evs), err)
	}
	if first := evs[0]; first.ID != nil || first.Task() == nil || string(first.Task().ID) != slow {
		t.Errorf("SubscribeToTask's first event is not the task's snapshot: %+v", first)
	}
	if last := evs[len(evs)-1]; last.State != a2a.TaskStateCanceled {
		t.Errorf("the A2A stream's last event: %s %s, want canceled", last.Kind, last.State)
	}
	checkCarried(t, h, a2a.TaskID(slow), append([]int64{k}, ids(evs)...))
	if g := GetTask(t, sb.Rpc("tasks/get", `{"taskId":"`+slow+`"}`)); g.Status != "cancelled" {
		t.Errorf("tasks/get of the cancelled task: %+v", g)
	}

	// An ordinary A2A task of the same caller, with the same API key: not a send_message task, so /mcp's MCP Tasks never see it.
	st, body := a2aSend(t, ctx, h.Front.URL+"/agents/kinds", h.APIKey, "", &a2a.Message{ID: "m-non-send_message", Role: a2a.MessageRoleUser,
		Parts: a2a.ContentParts{a2a.NewTextPart("hi")}})
	var sent struct {
		Result struct {
			Task struct {
				ID string `json:"id"`
			} `json:"task"`
		} `json:"result"`
	}
	if st != http.StatusOK || json.Unmarshal([]byte(body), &sent) != nil || sent.Result.Task.ID == "" {
		t.Fatalf("caller A's A2A SendMessage to kinds: %d %.300s", st, body)
	}
	notSent := sent.Result.Task.ID
	if got := texts(t, h, `select id::text from kontra.tasks where not sent_by_mcp`); !slices.Equal(got, []string{notSent}) {
		t.Fatalf("the tasks not marked sent_by_mcp: %v, want only the A2A task %s", got, notSent)
	}
	if code := ErrCode(t, s.Rpc("tasks/get", `{"taskId":"`+notSent+`"}`)); code != -32602 {
		t.Errorf("tasks/get of the caller's own A2A task that send_message did not create: %d, want -32602", code)
	}

	// tasks/list: this caller's send_message tasks only, newest first (status_at desc); another caller sees none.
	lr := Answer(t, s.Rpc("tasks/list", `{}`))
	var list struct {
		Tasks []struct {
			TaskID string `json:"taskId"`
		} `json:"tasks"`
	}
	_ = json.Unmarshal(lr.Result, &list)
	var listed []string
	for _, x := range list.Tasks {
		listed = append(listed, x.TaskID)
	}
	want := texts(t, h, `select id::text from kontra.tasks where sent_by_mcp order by status_at desc, id desc`)
	if !slices.Equal(listed, want) || len(listed) != 5 || !slices.Contains(listed, fast) || !slices.Contains(listed, slowPlain) ||
		!slices.Contains(listed, failed) || !slices.Contains(listed, kinds) {
		t.Errorf("tasks/list %v, want the caller's five send_message tasks newest first %v", listed, want)
	}
	if slices.Contains(listed, notSent) {
		t.Errorf("tasks/list lists the caller's A2A task %s that send_message did not create", notSent)
	}
	other := OpenGateSession(t, ctx, b, h.APIKeyB, `{}`)
	if lo := Answer(t, other.Rpc("tasks/list", `{}`)); string(lo.Result) != `{"tasks":[]}` {
		t.Errorf("another caller's tasks/list: %s", lo.Result)
	}
	if code := ErrCode(t, other.Rpc("tasks/get", `{"taskId":"`+kinds+`"}`)); code != -32602 {
		t.Errorf("another caller's tasks/get of this caller's task: %d, want -32602", code)
	}

	// Refs send_message does not take, and a null data part: isError, no task row (D272, D294).
	before := taskCount(t, h)
	for _, c := range []struct{ args, want string }{
		{`{"service":"web","message":"hi"}`, h.Front.URL + "/mcp/web"},
		{`{"service":"vault","message":"hi"}`, h.Front.URL + "/agents/vault"},
		{`{"service":"ownext","message":"hi"}`, own},
		{`{"service":"kinds","message":{"parts":[{"data":null}]}}`, "data must not be null"},
	} {
		tr := Tool(t, s.Tool("send_message", c.args))
		if !tr.IsError || !strings.Contains(tr.Text(), c.want) || strings.Contains(tr.Text(), operatorExt) {
			t.Errorf("send_message %s: %+v, want isError naming %q (and not the operator's %s)", c.args, tr, c.want, operatorExt)
		}
	}
	if n := taskCount(t, h); n != before {
		t.Errorf("the refused send_message calls created %d tasks", n-before)
	}
	if esc.Messages() != 0 {
		t.Errorf("the service behind the required extension received %d messages", esc.Messages())
	}
	// The operator's required entry alone: the send_message activates it for the caller (the Admission double allows, and is
	// asked with it in extensions), and the service gets the message without it (D300).
	if pr := Tool(t, s.Tool("send_message", `{"service":"paid","message":"hi"}`)); pr.IsError || pr.TaskID() == "" {
		t.Errorf("send_message of a service the operator's entry requires: %+v", pr)
	}
	if reqs := adm.Requests(); len(reqs) == 0 || reqs[len(reqs)-1].Service != "paid" || !slices.Contains(reqs[len(reqs)-1].Extensions, operatorExt) {
		t.Errorf("Admission's request for the send_message of paid: %+v, want extensions naming %s", reqs, operatorExt)
	}
	if paid.Messages() != 1 || len(paid.Activations()) != 0 {
		t.Errorf("the service behind the operator's entry received %d messages and A2A-Extensions %q", paid.Messages(), paid.Activations())
	}

	// D289: /mcp's tools write no calls rows; every task send_message created is marked sent_by_mcp.
	_ = Tool(t, s.Tool("search", `{}`))
	_ = Tool(t, s.Tool("describe", `{"service":"kinds"}`))
	if n := count(t, h, `select count(*) from kontra.calls`); n != 0 {
		t.Errorf("/mcp wrote %d calls rows", n)
	}
	if got := texts(t, h, `select id::text from kontra.tasks where not sent_by_mcp`); !slices.Equal(got, []string{notSent}) {
		t.Errorf("tasks send_message created are not marked sent_by_mcp: the non-send_message tasks are %v, want only the A2A task %s", got, notSent)
	}
	if n := count(t, h, `select count(*) from kontra.mcp_sessions where service is null`); n != 2 {
		t.Errorf("/mcp sessions: %d rows, want 2 (caller A's and B's)", n)
	}
	// The Admission server down, or answering a decision hooks.json does not define: -32603 admission unavailable (503),
	// no task row (§13.1: fail closed). A quote is no outcome since D351.
	before = taskCount(t, h)
	unavailable := func(what string) {
		t.Helper()
		r := s.Tool("send_message", `{"service":"tck","message":"tck-complete-task refused"}`)
		if a := Answer(t, r); r.Status != http.StatusServiceUnavailable || a.Error == nil || a.Error.Code != -32603 ||
			a.Error.Message != "admission unavailable" {
			t.Errorf("send_message with %s: %d %s, want -32603 admission unavailable (503)", what, r.Status, r.Text())
		}
	}
	adm.Set(`{"outcome":"quote","quote":{"approveUrl":"https://app.kontra.sh/#/t/x","expiresAt":"2099-01-01T00:00:00Z"}}`)
	unavailable("a quote, an outcome hooks.json no longer has")
	adm.Set(`not json`)
	unavailable("a body that is not JSON")
	adm.Set("")
	adm.Down()
	unavailable("the Admission server down")
	if n := taskCount(t, h); n != before {
		t.Errorf("the refused send_message calls wrote %d task rows", n-before)
	}
	if err := adm.Up(); err != nil {
		t.Fatal(err)
	}
	if r := Tool(t, s.Tool("send_message", `{"service":"tck","message":"tck-complete-task again"}`)); r.IsError || r.TaskID() == "" {
		t.Errorf("send_message with the Admission server up again: %+v", r)
	}

	// The plain slow send_message's task is still working: cancel it so the service is left with nothing.
	_ = s.Rpc("tasks/cancel", `{"taskId":"`+slowPlain+`"}`)
}

// a2aSend posts a JSON-RPC SendMessage of msg to the A2A route u with key, activating the extension ext when not "",
// and returns the status and the body.
func a2aSend(t *testing.T, ctx context.Context, u, key, ext string, msg *a2a.Message) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": map[string]any{"message": msg}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if ext != "" {
		req.Header.Set("A2A-Extensions", ext)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
