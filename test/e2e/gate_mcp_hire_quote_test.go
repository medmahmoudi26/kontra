package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
)

// gate_mcp_hire_quote (spec/relay.md §16; §12.3, §13.2; D275, D284, D288; D21, D38, D45, D188). Single tenant.
// The Admission double quotes the service paid (hosted by a node on replica B); the buyer's sessions are on replica A
// (S1, declaring elicitation.url and form) and B (S2, declaring elicitation.url; P, declaring none). Asserted:
//
//   - S1's hire is -32042 with data.elicitations = [{mode: url, elicitationId, url, message}]; url is byte for byte the
//     approveUrl the hook answered, which names the pre-minted taskId the hook was asked about and the task then has;
//     the task is quoted and the seller has received nothing;
//   - the quote-acceptance invariant: while quoted, each of these leaves the task quoted and the seller without a
//     request: the same hire again (the same elicitationId, Admission not asked again), from S1 and from S2; reply to
//     the task (isError, nothing forwarded); a client's JSON-RPC response with id k.<task>.<quoted seq> and an accept
//     result (202, dropped); an A2A message to the task with the same API key, plain, carrying kontra.approval, carrying
//     kontra.escrow.* metadata, or activating the escrow extension (§7's UnsupportedOperationError, -32004, for the
//     quoted task); no session is ever sent an
//     elicitation/create (the gate sends no URL-mode request and reads no client result as approval);
//   - a session without elicitation.url gets working with the URL in the text (plain) or statusMessage
//     (task-augmented) and _meta sh.kontra/approve-url, never -32042; the plain answer's structuredContent has the
//     same URL as url and quote.approveUrl (D324);
//   - after kontra.approval {accepted: true}, of S1's two GET streams (one on each replica) exactly one receives
//     notifications/elicitation/complete {elicitationId}, once and with no event id, S2's stream none, and S1's retried
//     hire answers the completed result, its structuredContent the answer's text and the thread URL;
//   - a declined quote's retry is isError with sh.kontra/canceled declined (task-augmented: cancelled); an expired
//     one's is sh.kontra/canceled quote_expired;
//   - a team gate's own approve URL (https://gate.team.example/approve/<taskId>) reaches the -32042 and the working
//     answer unchanged;
//   - the Admission server down, a malformed quote (an http:// approveUrl) and an expired one are -32603 admission
//     unavailable (HTTP 503), and no task row is written.
func TestGateMCPHireQuote(t *testing.T) {
	adm, err := StartAdmission("paid")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adm.Close() })
	seller := StartSeller(t)
	h := start(t, Options{Tunnel: map[string]TunnelService{"paid": {}}, Admission: adm})
	ctx := ctxFor(t, 10*time.Minute)
	a, b := h.Gates[0], h.Gates[1]
	hostOn(t, ctx, h, b, map[string]string{"paid": seller.URL})

	s1 := OpenGateSession(t, ctx, a, h.APIKey, CapsURLAndForm)
	s2 := OpenGateSession(t, ctx, b, h.APIKey, `{"elicitation":{"url":{}}}`)
	p := OpenGateSession(t, ctx, b, h.APIKey, `{}`)
	var answers []string // every answer the sessions got, checked for an elicitation/create at the end
	keep := func(r *MCPResponse) *MCPResponse { answers = append(answers, r.Text()); return r }

	// The URL elicitation (-32042), naming the pre-minted id.
	args := `{"service":"paid","message":"hi"}`
	r := keep(s1.Tool("hire", args))
	ans := Answer(t, r)
	if ans.Error == nil || ans.Error.Code != -32042 {
		t.Fatalf("a quoted hire to a session that declared elicitation.url: %s", r.Text())
	}
	var data struct {
		Elicitations []struct {
			Mode          string `json:"mode"`
			ElicitationID string `json:"elicitationId"`
			URL           string `json:"url"`
			Message       string `json:"message"`
		} `json:"elicitations"`
	}
	if err := json.Unmarshal(ans.Error.Data, &data); err != nil || len(data.Elicitations) != 1 {
		t.Fatalf("-32042's data: %s", ans.Error.Data)
	}
	el := data.Elicitations[0]
	reqs := adm.Requests()
	if len(reqs) != 1 || reqs[0].Operation != "task" || reqs[0].Service != "paid" || reqs[0].Auth != "Bearer "+adm.Secret {
		t.Fatalf("the hook's requests: %+v", reqs)
	}
	taskID := reqs[0].TaskID
	if _, err := uuid.Parse(taskID); err != nil {
		t.Fatalf("the hook was asked about %q, not a pre-minted task id", taskID)
	}
	if el.Mode != "url" || el.ElicitationID == "" || el.Message == "" || el.URL != "https://app.kontra.sh/#/t/"+taskID {
		t.Fatalf("the URL elicitation %+v, want the hook's approveUrl https://app.kontra.sh/#/t/%s byte for byte", el, taskID)
	}
	if st := taskState(t, h, taskID); st != "quoted" {
		t.Fatalf("the pre-minted id's task is %q, want quoted", st)
	}
	quoted := func(what string) {
		t.Helper()
		if st := taskState(t, h, taskID); st != "quoted" {
			t.Fatalf("after %s the task is %s, want quoted", what, st)
		}
		if n := seller.Messages(); n != 0 {
			t.Fatalf("after %s the seller received %d messages", what, n)
		}
	}
	quoted("the hire")

	// The quote-acceptance invariant (D38, D45): nothing the buyer sends approves the quote.
	for _, c := range []*GateSession{s1, s2, s1} {
		rr := keep(c.Tool("hire", args))
		if aa := Answer(t, rr); aa.Error == nil || aa.Error.Code != -32042 || !strings.Contains(string(aa.Error.Data), `"elicitationId":"`+el.ElicitationID+`"`) {
			t.Fatalf("a retry of the quoted hire: %s", rr.Text())
		}
	}
	if n := len(adm.Requests()); n != 1 {
		t.Errorf("retries asked Admission again: %d requests", n)
	}
	quoted("the retried hires, from S1 and from S2")
	rp := Tool(t, keep(s1.Tool("reply", `{"taskId":"`+taskID+`","message":"I accept"}`)))
	if !rp.IsError || rp.Meta["sh.kontra/approve-url"] != el.URL {
		t.Errorf("reply to a quoted task: %+v", rp)
	}
	quoted("a reply")
	seq := scalar[int64](t, h, `select max(seq) from kontra.task_events where task_id::text = $1`, taskID)
	for _, result := range []string{`{"action":"accept","content":{"reply":"yes"}}`, `{"action":"accept"}`} {
		if st := s1.Raw(fmt.Sprintf(`{"jsonrpc":"2.0","id":"k.%s.%d","result":%s}`, taskID, seq, result)); st != http.StatusAccepted {
			t.Errorf("a client response to the quoted event's gate id: %d, want 202", st)
		}
	}
	quoted("a client's accept for the quoted event's gate id")
	// §7's refusal of a message to a quoted task: UnsupportedOperationError (-32004), from the quoted-state check itself.
	for i, c := range []struct {
		md  map[string]any
		ext string
	}{
		{}, // a plain message: no metadata, no extension
		{md: map[string]any{"kontra.approval": map[string]any{"accepted": true}}},
		{md: map[string]any{"kontra.escrow.hold": "accepted", "kontra.escrow.status": "held"}},
		{ext: "https://kontra.sh/ext/escrow/v1"},
	} {
		st, body := a2aSend(t, ctx, h.Front.URL+"/agents/paid", h.APIKey, c.ext, &a2a.Message{ID: fmt.Sprintf("m-accept-%d", i), Role: a2a.MessageRoleUser,
			TaskID: a2a.TaskID(taskID), Parts: a2a.ContentParts{a2a.NewTextPart("I accept")}, Metadata: c.md})
		var refusal struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &refusal); err != nil || refusal.Result != nil || refusal.Error == nil ||
			refusal.Error.Code != -32004 || refusal.Error.Message != "a quoted task is accepted on the operator's page, not in the protocol" {
			t.Errorf("an A2A message to the quoted task (%v %q): %d %s, want UnsupportedOperationError -32004 for the quoted task", c.md, c.ext, st, body)
		}
	}
	quoted("A2A messages to the task")

	// A session without URL elicitation: working with the URL at once, plain and task-augmented, never -32042.
	pr := Tool(t, keep(p.Tool("hire", args)))
	if pr.IsError || pr.TaskID() != taskID || !strings.Contains(pr.Text(), el.URL) || pr.Meta["sh.kontra/approve-url"] != el.URL {
		t.Errorf("a quoted hire to a session without URL elicitation: %+v", pr)
	}
	// D324: the same URL, and the quote's terms, in structuredContent, which may be all a client shows.
	if sc := pr.Structured; sc == nil || sc.URL != el.URL || sc.Quote == nil || sc.Quote.ApproveURL != el.URL || sc.Quote.ExpiresAt == "" ||
		sc.Text != pr.Text() {
		t.Errorf("a quoted hire's structuredContent: %s, want url and quote.approveUrl %s", pr.StructuredRaw, el.URL)
	}
	ctr := Tool(t, keep(p.ToolTask("hire", args)))
	if ctr.Task == nil || ctr.Task.TaskID != taskID || ctr.Task.Status != "working" || ctr.Task.StatusMessage != el.URL ||
		ctr.Meta["sh.kontra/approve-url"] != el.URL {
		t.Errorf("a task-augmented quoted hire without URL elicitation: %+v", ctr)
	}
	quoted("the answers to a session without URL elicitation")

	// Two GET streams of S1 (one per replica), one of S2; the operator approves.
	g1, g2, g3 := s1.Listen(ctx), s1.On(b).Listen(ctx), s2.Listen(ctx)
	time.Sleep(time.Second) // the streams are registered on their replicas
	if err := h.Approve(ctx, taskID, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, 20*time.Second, "one of S1's streams gets elicitation/complete", func() bool { return len(g1.Messages())+len(g2.Messages()) >= 1 })
	time.Sleep(5 * time.Second) // past two rescans of every stream (D293)
	g1.Stop()
	g2.Stop()
	g3.Stop()
	got := append(g1.Messages(), g2.Messages()...)
	want := `{"jsonrpc":"2.0","method":"notifications/elicitation/complete","params":{"elicitationId":"` + el.ElicitationID + `"}}`
	if len(got) != 1 || got[0] != want {
		t.Fatalf("S1's two streams got %q, want exactly one %s", got, want)
	}
	if g1.EventIDs()+g2.EventIDs() != 0 {
		t.Errorf("a /mcp GET stream carried event ids")
	}
	if m := g3.Messages(); len(m) != 0 {
		t.Errorf("another session's stream got %q", m)
	}
	answers = append(answers, got...)
	rr := Tool(t, keep(s1.Tool("hire", args)))
	if rr.IsError || rr.TaskID() != taskID || rr.State() != "TASK_STATE_COMPLETED" || rr.Text() != "Hello World" ||
		rr.Structured.Text != "Hello World" || rr.Structured.URL != el.URL {
		t.Fatalf("S1's retry after the approval: %+v (%s)", rr, rr.StructuredRaw)
	}
	if n := seller.Messages(); n != 1 {
		t.Errorf("the seller received %d messages after the approval, want 1", n)
	}

	// A declined quote and an expired one.
	dargs := `{"service":"paid","message":"hi","contextId":"declined"}`
	declined := Tool(t, keep(p.Tool("hire", dargs))).TaskID()
	if err := h.Approve(ctx, declined, false); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, "the declined task is canceled", func() bool { return taskState(t, h, declined) == "canceled" })
	dr := Tool(t, keep(p.Tool("hire", dargs)))
	if !dr.IsError || dr.Meta["sh.kontra/canceled"] != "declined" || !strings.Contains(dr.Text(), "declined") || dr.TaskID() != declined {
		t.Errorf("a retry of a declined quote: %+v", dr)
	}
	if dt := Tool(t, keep(p.ToolTask("hire", dargs))); dt.Task == nil || dt.Task.TaskID != declined || dt.Task.Status != "cancelled" {
		t.Errorf("a task-augmented retry of a declined quote: %+v", dt)
	}
	adm.Set(nil, 3*time.Second, "")
	xargs := `{"service":"paid","message":"hi","contextId":"expires"}`
	expiring := Tool(t, keep(p.Tool("hire", xargs))).TaskID()
	eventually(t, 30*time.Second, "the quote expires", func() bool { return taskState(t, h, expiring) == "canceled" })
	xr := Tool(t, keep(p.Tool("hire", xargs)))
	if !xr.IsError || xr.Meta["sh.kontra/canceled"] != "quote_expired" || !strings.Contains(xr.Text(), "expired") || xr.TaskID() != expiring {
		t.Errorf("a retry of an expired quote: %+v", xr)
	}

	// A team gate's own approve URL, passed through unchanged (D188).
	adm.Set(func(id string) string { return "https://gate.team.example/approve/" + id }, 10*time.Minute, "")
	tr := keep(s1.Tool("hire", `{"service":"paid","message":"hi","contextId":"team"}`))
	teamTask := adm.Requests()[len(adm.Requests())-1].TaskID
	if aa := Answer(t, tr); aa.Error == nil || aa.Error.Code != -32042 ||
		!strings.Contains(string(aa.Error.Data), `"url":"https://gate.team.example/approve/`+teamTask+`"`) {
		t.Errorf("a team gate's quote to S1: %s", tr.Text())
	}
	tw := Tool(t, keep(p.Tool("hire", `{"service":"paid","message":"hi","contextId":"team"}`)))
	if tw.TaskID() != teamTask || tw.Meta["sh.kontra/approve-url"] != "https://gate.team.example/approve/"+teamTask ||
		!strings.Contains(tw.Text(), "https://gate.team.example/approve/"+teamTask) {
		t.Errorf("a team gate's quote to a session without URL elicitation: %+v", tw)
	}

	// The Admission server down, a malformed quote, an expired one: -32603 admission unavailable (503), no task.
	before := taskCount(t, h)
	check := func(what string) {
		t.Helper()
		r := keep(s1.Tool("hire", `{"service":"paid","message":"hi","contextId":"refused"}`))
		if aa := Answer(t, r); r.Status != http.StatusServiceUnavailable || aa.Error == nil || aa.Error.Code != -32603 ||
			aa.Error.Message != "admission unavailable" {
			t.Errorf("hire with %s: %d %s", what, r.Status, r.Text())
		}
	}
	adm.Set(nil, 0, `{"outcome":"quote","quote":{"approveUrl":"http://app.kontra.sh/#/t/x","expiresAt":"2099-01-01T00:00:00Z"}}`)
	check("an http:// approveUrl")
	adm.Set(nil, 0, `{"outcome":"quote","quote":{"approveUrl":"https://app.kontra.sh/#/t/x","expiresAt":"2020-01-01T00:00:00Z"}}`)
	check("an expired quote")
	adm.Down()
	check("the Admission server down")
	if n := taskCount(t, h); n != before {
		t.Errorf("the refused hires wrote %d task rows", n-before)
	}

	for _, m := range answers {
		if strings.Contains(m, `"elicitation/create"`) {
			t.Errorf("the gate sent a session an elicitation/create: %s", m)
		}
	}
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
