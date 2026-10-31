package e2e

import (
	"encoding/json"
	"fmt"
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

// gate_mcp_hire (spec/relay.md §16; §12.2, §12.3 step 1; D272, D289, D293, D294, D300). Single tenant. A legacy /mcp
// session with an API key on replica A; the sellers hosted by a node whose tunnel is on replica B: the TCK seller
// (examples/sellers/tck, a stock seller that completes at once) and the in-test seller (the TCK seller's artifact
// kinds, a slow exec, a failing exec). Asserted, clause by clause:
//
//   - a plain hire that ends within the wait answers the rendered outcome and structuredContent exactly {taskId, state}
//     (no data without data parts), the task being an ordinary A2A task marked hired;
//   - a plain hire still running after the 25 s wait answers {taskId, state: working} with the A2A URL that follows it;
//   - a task-augmented hire answers CreateTaskResult; tasks/result (on B) renders text parts as text, data parts as JSON
//     text and in structuredContent.data, a raw file part as an embedded resource and a URL part as a resource_link;
//     structuredContent is {taskId, state, data};
//   - a failed upstream is a CallToolResult with isError and _meta sh.kontra/failure, never a JSON-RPC error;
//   - tasks/list lists only this buyer's hire tasks, newest first: not the buyer's ordinary A2A task (SendMessage with
//     the same API key), whose tasks/get is -32602 as an unknown task's; another buyer's tasks/list is empty and its
//     tasks/get of this buyer's task is -32602;
//   - tasks/cancel ends a working task cancelled within 10 s and the seller receives CancelTask; an A2A SubscribeToTask
//     of the same task with the same key shows exactly the log's carried seqs, up to the cancel;
//   - a kind: mcp ref, a required-sealed ref, a ref whose served card requires an extension of the seller's own and a
//     null data part are isError with no task row (the operator's required card_extensions entry beside the seller's
//     is not named: a hire activates it for the buyer, D300); a ref whose only required extension is the operator's
//     is hired, and its seller receives no A2A-Extensions;
//   - /mcp's tools write no calls rows, and every task hire created is marked hired (the A2A task is the only one not);
//   - a request without a credential is 401 (Bearer) and opens no session; an API key with only enroll: scopes is 403.
func TestGateMCPHire(t *testing.T) {
	const escrow, own = "https://kontra.sh/ext/escrow/v1", "https://seller.example.com/ext/own"
	seller, esc, paid := StartSeller(t), StartSellerCaps(t, `{"streaming":true,"extensions":[{"uri":"`+own+`","required":true}]}`), StartSeller(t)
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	web := httptest.NewServer(up)
	t.Cleanup(web.Close)
	operatorEscrow := `    annotations: { card_extensions: [ { uri: "` + escrow + `", required: true } ] }` + "\n"
	h := start(t, Options{
		Tunnel:      map[string]TunnelService{"tck": {}, "kinds": {}, "vault": {Sealed: "true"}},
		Direct:      map[string]string{"escrowed": esc.URL, "paid": paid.URL},
		MCP:         map[string]MCPService{"web": {URL: web.URL + "/mcp"}},
		ServiceYAML: map[string]string{"escrowed": operatorEscrow, "paid": operatorEscrow},
	})
	ctx := ctxFor(t, 10*time.Minute)
	a, b := h.Gates[0], h.Gates[1]
	tck, err := h.StartExampleSeller(ctx, "tck")
	if err != nil {
		t.Fatal(err)
	}
	hostOn(t, ctx, h, b, map[string]string{"tck": tck.URL, "kinds": seller.URL})

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

	// A plain hire that completes at once, on the TCK seller: rendered text, structuredContent {taskId, state}.
	r := Tool(t, s.Tool("hire", `{"service":"tck","message":"tck-complete-task hi"}`))
	if r.IsError || r.State() != "TASK_STATE_COMPLETED" || r.TaskID() == "" || !strings.HasPrefix(r.Text(), "Unhandled messageId prefix: ") {
		t.Fatalf("a plain fast hire: %+v", r)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(r.StructuredRaw, &keys); err != nil || len(keys) != 2 || keys["taskId"] == nil || keys["state"] == nil {
		t.Errorf("structuredContent of a hire without data parts: %s, want exactly {taskId, state}", r.StructuredRaw)
	}
	fast := r.TaskID()
	if !scalar[bool](t, h, `select hired and protocol = 'a2a' and first_request is not null from kontra.tasks where id::text = $1`, fast) {
		t.Error("the hire task is not an A2A task marked hired with the hire's resend key")
	}

	// A plain hire of a slow exec: {taskId, working} after the 25 s wait, with the A2A URL that follows it.
	began := time.Now()
	r = Tool(t, s.Tool("hire", `{"service":"kinds","message":"slow"}`))
	took := time.Since(began)
	if r.IsError || r.State() != "TASK_STATE_WORKING" || r.TaskID() == "" || !strings.Contains(r.Text(), h.Front.URL+"/agents/kinds") ||
		took < 20*time.Second || took > 50*time.Second {
		t.Fatalf("a plain slow hire after %v: %+v", took, r)
	}
	slowPlain := r.TaskID()

	// A task-augmented hire of the artifact kinds: CreateTaskResult, then tasks/result on replica B.
	ct := Tool(t, s.ToolTask("hire", `{"service":"kinds","message":"kinds"}`))
	if ct.Task == nil || ct.Task.TaskID == "" || ct.Task.Status != "working" || ct.Task.PollInterval <= 0 {
		t.Fatalf("a task-augmented hire: %+v", ct)
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
	fr := s.Tool("hire", `{"service":"kinds","message":"fail"}`)
	if code := ErrCode(t, fr); code != 0 {
		t.Fatalf("a failed upstream answered the JSON-RPC error %d: %s", code, fr.Text())
	}
	f := Tool(t, fr)
	if reason, _ := f.Meta["sh.kontra/failure"].(string); !f.IsError || reason == "" || f.State() != "TASK_STATE_FAILED" ||
		!strings.Contains(f.Text(), "the exec failed") {
		t.Errorf("a failed upstream: %+v", f)
	}
	failed := f.TaskID()

	// A slow task-augmented hire, followed by A2A SubscribeToTask on B with the same key, then tasks/cancel on A.
	ct = Tool(t, s.ToolTask("hire", `{"service":"kinds","message":"slow"}`))
	slow := ct.Task.TaskID
	eventually(t, 30*time.Second, "the slow task works", func() bool { return taskState(t, h, slow) == "working" })
	carried, err := h.CarriedSeqs(ctx, a2a.TaskID(slow))
	if err != nil || len(carried) == 0 {
		t.Fatalf("the slow task's log: %v %v", carried, err)
	}
	k := carried[0] // the task event: the A2A stream resumes after it with every carried row (§9)
	stream, err := h.BuyerA().Subscribe(ctx, b, a2a.TaskID(slow), &k, Call{Service: "kinds"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	cancels := seller.Count("CancelTask")
	began = time.Now()
	cr := s.Rpc("tasks/cancel", `{"taskId":"`+slow+`"}`)
	if a := Answer(t, cr); a.Error != nil || !strings.Contains(string(a.Result), `"status":"cancelled"`) || time.Since(began) > 10*time.Second {
		t.Fatalf("tasks/cancel after %v: %s", time.Since(began), cr.Text())
	}
	eventually(t, 10*time.Second, "the seller received CancelTask", func() bool { return seller.Count("CancelTask") > cancels })
	if st := taskState(t, h, slow); st != "canceled" {
		t.Fatalf("after tasks/cancel the task is %s", st)
	}
	evs, err := stream.ReadAll(ctx)
	if err != nil || len(evs) < 2 {
		t.Fatalf("the A2A stream of the hire task: %d events, %v", len(evs), err)
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

	// An ordinary A2A task of the same buyer, with the same API key: not a hire task, so /mcp's MCP Tasks never see it.
	st, body := a2aSend(t, ctx, h.Front.URL+"/agents/kinds", h.APIKey, "", &a2a.Message{ID: "m-unhired", Role: a2a.MessageRoleUser,
		Parts: a2a.ContentParts{a2a.NewTextPart("hi")}})
	var sent struct {
		Result struct {
			Task struct {
				ID string `json:"id"`
			} `json:"task"`
		} `json:"result"`
	}
	if st != http.StatusOK || json.Unmarshal([]byte(body), &sent) != nil || sent.Result.Task.ID == "" {
		t.Fatalf("buyer A's A2A SendMessage to kinds: %d %.300s", st, body)
	}
	unhired := sent.Result.Task.ID
	if got := texts(t, h, `select id::text from kontra.tasks where not hired`); !slices.Equal(got, []string{unhired}) {
		t.Fatalf("the tasks not marked hired: %v, want only the A2A task %s", got, unhired)
	}
	if code := ErrCode(t, s.Rpc("tasks/get", `{"taskId":"`+unhired+`"}`)); code != -32602 {
		t.Errorf("tasks/get of the buyer's own A2A task that hire did not create: %d, want -32602", code)
	}

	// tasks/list: this buyer's hire tasks only, newest first (status_at desc); another buyer sees none.
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
	want := texts(t, h, `select id::text from kontra.tasks where hired order by status_at desc, id desc`)
	if !slices.Equal(listed, want) || len(listed) != 5 || !slices.Contains(listed, fast) || !slices.Contains(listed, slowPlain) ||
		!slices.Contains(listed, failed) || !slices.Contains(listed, kinds) {
		t.Errorf("tasks/list %v, want the buyer's five hire tasks newest first %v", listed, want)
	}
	if slices.Contains(listed, unhired) {
		t.Errorf("tasks/list lists the buyer's A2A task %s that hire did not create", unhired)
	}
	other := OpenGateSession(t, ctx, b, h.APIKeyB, `{}`)
	if lo := Answer(t, other.Rpc("tasks/list", `{}`)); string(lo.Result) != `{"tasks":[]}` {
		t.Errorf("another buyer's tasks/list: %s", lo.Result)
	}
	if code := ErrCode(t, other.Rpc("tasks/get", `{"taskId":"`+kinds+`"}`)); code != -32602 {
		t.Errorf("another buyer's tasks/get of this buyer's task: %d, want -32602", code)
	}

	// Refs hire does not take, and a null data part: isError, no task row (D272, D294).
	before := taskCount(t, h)
	for _, c := range []struct{ args, want string }{
		{`{"service":"web","message":"hi"}`, h.Front.URL + "/mcp/web"},
		{`{"service":"vault","message":"hi"}`, h.Front.URL + "/agents/vault"},
		{`{"service":"escrowed","message":"hi"}`, own},
		{`{"service":"kinds","message":{"parts":[{"data":null}]}}`, "data must not be null"},
	} {
		tr := Tool(t, s.Tool("hire", c.args))
		if !tr.IsError || !strings.Contains(tr.Text(), c.want) || strings.Contains(tr.Text(), escrow) {
			t.Errorf("hire %s: %+v, want isError naming %q (and not the operator's %s)", c.args, tr, c.want, escrow)
		}
	}
	if n := taskCount(t, h); n != before {
		t.Errorf("the refused hires created %d tasks", n-before)
	}
	if esc.Messages() != 0 {
		t.Errorf("the seller behind the required extension received %d messages", esc.Messages())
	}
	// The operator's required entry alone: the hire activates it for the buyer (built-in Admission allows), and the
	// seller gets the message without it (D300).
	if pr := Tool(t, s.Tool("hire", `{"service":"paid","message":"hi"}`)); pr.IsError || pr.TaskID() == "" {
		t.Errorf("hire of a service the operator's escrow entry requires: %+v", pr)
	}
	if paid.Messages() != 1 || len(paid.Activations()) != 0 {
		t.Errorf("the seller behind the operator's entry received %d messages and A2A-Extensions %q", paid.Messages(), paid.Activations())
	}

	// D289: /mcp's tools write no calls rows; every task hire created is marked hired.
	_ = Tool(t, s.Tool("search", `{}`))
	_ = Tool(t, s.Tool("describe", `{"service":"kinds"}`))
	if n := count(t, h, `select count(*) from kontra.calls`); n != 0 {
		t.Errorf("/mcp wrote %d calls rows", n)
	}
	if got := texts(t, h, `select id::text from kontra.tasks where not hired`); !slices.Equal(got, []string{unhired}) {
		t.Errorf("tasks hire created are not marked hired: the unhired tasks are %v, want only the A2A task %s", got, unhired)
	}
	if n := count(t, h, `select count(*) from kontra.mcp_sessions where service is null`); n != 2 {
		t.Errorf("/mcp sessions: %d rows, want 2 (buyer A's and B's)", n)
	}
	// The plain slow hire's task is still working: cancel it so the seller is left with nothing.
	_ = s.Rpc("tasks/cancel", `{"taskId":"`+slowPlain+`"}`)
}
