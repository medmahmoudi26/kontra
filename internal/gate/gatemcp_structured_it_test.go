package gate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/gatemcp"
)

// structuredOnly is what a client that shows its model nothing but structuredContent sees of a tool result (Claude
// Code 2.1.281, M5's gate G3, C59): the result's structuredContent, read with exact keys into the hire result's shape.
func structuredOnly(t *testing.T, what string, a rpcAnswer) gatemcp.HireResult {
	t.Helper()
	if a.Error != nil || a.Result == nil {
		t.Fatalf("%s: not a result: %s", what, a.raw)
	}
	var r struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(a.Result, &r); err != nil || len(r.StructuredContent) == 0 {
		t.Fatalf("%s: no structuredContent: %s", what, a.raw)
	}
	var hr gatemcp.HireResult
	dec := json.NewDecoder(bytes.NewReader(r.StructuredContent))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&hr); err != nil {
		t.Fatalf("%s: structuredContent %s: %v", what, r.StructuredContent, err)
	}
	return hr
}

// What a person needs, in structuredContent alone (spec §12.2, §12.3; D324), through a whole replica, from a 2026-07-28
// client that declares URL and form elicitation and calls hire without the tasks extension, as Claude Code does, and
// from 2025-11-25 sessions:
//
//   - a quoted hire: url and quote.approveUrl are the task's thread, byte for byte the URL the -32042 elicitation
//     carries to a session that declared elicitation.url, and quote.expiresAt the quote's expiry; the text says so;
//   - once a person accepts, the retried hire's structuredContent holds the agent's answer as text, with the url;
//   - a free hire (Admission allows it): the answer's text beside data and the file links, and no url; a question is
//     its text with the reply-tool hint, and reply's structuredContent the answer to it;
//
// and an operator's refusal code (spec §13.1, D325): Admission's -32060 reaches /mcp as -32060 with its message in
// both eras, and the A2A bindings as PERMISSION_DENIED (JSON-RPC -31403, HTTP+JSON 403) with {"code": -32060} in the
// error's data; no task is created.
func TestGateMCPHireStructured(t *testing.T) {
	e := newGateEnv(t)
	kinds, paid := newHireSeller(t), newHireSeller(t)
	hook := newHookServer(t)
	t.Setenv("KONTRA_TEST_ADMISSION_SECRET", "Hk4nW8qZ2xV6bM1tR9yL3pD7sF0gJ5cA")
	services := e.modernServices(kinds, paid, hook.srv.URL)
	timing := fastTiming
	timing.PlainWait = 4 * time.Second
	a := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), timing)
	key := e.mintKey("buyer")
	claude := newModernClient(t, a.url, key, `{"elicitation":{"form":{},"url":{}}}`)
	tasks := func() int64 { return scalar[int64](t, e.pool, `select count(*) from kontra.tasks`) }

	// The quoted hire: the thread URL and the quote's terms in structuredContent.
	args := `{"service":"paid","message":"hi"}`
	before := time.Now()
	_, ans := claude.call("hire", args)
	q := structuredOnly(t, "the quoted hire", ans)
	thread := "https://app.kontra.sh/#/t/" + q.TaskID
	expires, err := time.Parse(time.RFC3339, func() string {
		if q.Quote == nil {
			return ""
		}
		return q.Quote.ExpiresAt
	}())
	if q.State != "TASK_STATE_INPUT_REQUIRED" || q.URL != thread || q.Quote == nil || q.Quote.ApproveURL != thread || err != nil ||
		expires.Before(before.Add(9*time.Minute)) || expires.After(time.Now().Add(11*time.Minute)) ||
		!strings.Contains(q.Text, thread) || !strings.Contains(q.Text, "call hire again") {
		t.Fatalf("the quoted hire's structuredContent: %+v (%s)", q, ans.raw)
	}
	if st := scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, q.TaskID); st != "quoted" {
		t.Fatalf("the hired task is %s, want quoted", st)
	}
	// The same URL as the URL elicitation carries (a 2025-11-25 session that declared elicitation.url), and as the plain
	// answer to one that did not.
	s := newMCPClient(t, a.url, key)
	s.initialize(`{"elicitation":{"url":{}}}`)
	_, ans = s.call("hire", args)
	var data struct {
		Elicitations []struct{ URL string } `json:"elicitations"`
	}
	if ans.Error == nil || ans.Error.Code != gatemcp.CodeURLElicitationRequired || json.Unmarshal(ans.Error.Data, &data) != nil ||
		len(data.Elicitations) != 1 || data.Elicitations[0].URL != q.URL {
		t.Fatalf("the URL elicitation of the same hire: %s, want the url %s", ans.raw, q.URL)
	}
	p := newMCPClient(t, a.url, key)
	p.initialize(`{}`)
	_, ans = p.call("hire", args)
	if pq := structuredOnly(t, "a plain legacy quoted hire", ans); pq.TaskID != q.TaskID || pq.URL != q.URL || !equalQuote(pq.Quote, q.Quote) {
		t.Errorf("a legacy session's plain quoted hire: %+v", pq)
	}
	if n := hook.count(); n != 1 {
		t.Errorf("Admission was asked %d times, want once (the retries ask nothing)", n)
	}

	// A person accepts on the thread; the retried hire's structuredContent is the agent's answer.
	approve(t, q.TaskID, true)
	eventually(t, 20*time.Second, "the approved task completes", func() bool {
		return scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, q.TaskID) == "completed"
	})
	_, ans = claude.call("hire", args)
	if done := structuredOnly(t, "the retried hire", ans); done.TaskID != q.TaskID || done.State != "TASK_STATE_COMPLETED" ||
		done.Text != "Hello World" || done.URL != thread || done.Quote != nil {
		t.Errorf("the retried hire after the approval: %+v", done)
	}

	// A free hire: the answer's text, data and files; no url, since the task has no thread the gate knows.
	hook.set(func(h *hookServer) { h.raw = `{"outcome":"allow"}` })
	_, ans = claude.call("hire", `{"service":"kinds","message":"kinds"}`)
	free := structuredOnly(t, "a free hire", ans)
	if free.State != "TASK_STATE_COMPLETED" || free.URL != "" || free.Quote != nil || free.Text != "done\ntck artifact" ||
		fmt.Sprint(free.Data) != "[map[total:4] map[key:value]]" || len(free.Files) != 2 ||
		!strings.HasPrefix(free.Files[0].URI, "kontra:task/"+free.TaskID+"/") || free.Files[1].URI != "https://example.com/output.txt" {
		t.Errorf("a free hire's structuredContent: %+v", free)
	}
	// A question, and reply's answer.
	_, ans = claude.call("hire", `{"service":"kinds","message":"ask"}`)
	ask := structuredOnly(t, "a hire that asks", ans)
	if ask.State != "TASK_STATE_INPUT_REQUIRED" || ask.Text != "Which date? (answer with the reply tool, taskId "+ask.TaskID+")" || ask.Quote != nil {
		t.Errorf("a hire that asks: %+v", ask)
	}
	_, ans = claude.call("reply", `{"taskId":"`+ask.TaskID+`","message":"friday"}`)
	if r := structuredOnly(t, "reply", ans); r.TaskID != ask.TaskID || r.State != "TASK_STATE_COMPLETED" || r.Text != "booked for friday" {
		t.Errorf("reply's structuredContent: %+v", r)
	}

	// An operator's refusal code: as it is on /mcp, in the error's data on A2A; no task.
	const why = "this service's seller cannot be paid yet"
	hook.set(func(h *hookServer) { h.raw = `{"outcome":"deny","refusal":{"code":-32060,"message":"` + why + `"}}` })
	n := tasks()
	for name, c := range map[string]func() rpcAnswer{
		"modern": func() rpcAnswer { _, a := claude.call("hire", `{"service":"kinds","message":"hi"}`); return a },
		"legacy": func() rpcAnswer { _, a := p.call("hire", `{"service":"kinds","message":"hi"}`); return a },
	} {
		if ans := c(); ans.Error == nil || ans.Error.Code != -32060 || ans.Error.Message != why {
			t.Errorf("%s hire refused by the operator: %s, want -32060 %q", name, ans.raw, why)
		}
	}
	post := func(path, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, a.url+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("A2A-Version", "1.0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	_, body := post("/agents/kinds", `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m-op","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`)
	var rpc struct {
		Error struct {
			Code    int              `json:"code"`
			Message string           `json:"message"`
			Data    []map[string]any `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &rpc) != nil || rpc.Error.Code != -31403 || rpc.Error.Message != why || !hasCode(rpc.Error.Data, -32060) {
		t.Errorf("A2A JSON-RPC refused by the operator: %s, want -31403 with code -32060 in data", body)
	}
	status, body := post("/agents/kinds/message:send", `{"message":{"messageId":"m-op-rest","role":"ROLE_USER","parts":[{"text":"hi"}]}}`)
	var rest struct {
		Error struct {
			Status  string           `json:"status"`
			Message string           `json:"message"`
			Details []map[string]any `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &rest) != nil || status != http.StatusForbidden || rest.Error.Status != "PERMISSION_DENIED" ||
		rest.Error.Message != why || !hasCode(rest.Error.Details, -32060) {
		t.Errorf("A2A HTTP+JSON refused by the operator: %d %s, want 403 with code -32060 in details", status, body)
	}
	if m := tasks(); m != n {
		t.Errorf("refused hires created %d tasks", m-n)
	}
}

func equalQuote(a, b *gatemcp.QuoteTerms) bool {
	return a != nil && b != nil && *a == *b
}

// hasCode reports whether an A2A error's details hold the google.protobuf.Struct {"code": code}.
func hasCode(details []map[string]any, code int) bool {
	for _, d := range details {
		if d["@type"] == "type.googleapis.com/google.protobuf.Struct" && d["code"] == float64(code) {
			return true
		}
	}
	return false
}
