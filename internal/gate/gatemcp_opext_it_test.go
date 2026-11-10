package gate

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A service whose served card carries an operator's extension entry with required: true (spec §13.4), hired through a
// whole replica's /mcp (spec §12.2, §12.3; D300, D351): the hire activates the entry for the buyer, so Admission is
// asked, with the URI in its extensions, the pre-minted task id and via api_key, and allows; the A2A route still
// refuses a SendMessage that does not activate the entry (§2.4); the seller receives the message with no
// A2A-Extensions (the gate activated the entry, the buyer did not); the buyer's key then reads, lists and cancels the
// task over A2A without activating the entry, and only a message to it is refused (D318). A service whose seller's own
// card requires an extension, and a required-sealed one, are still isError with no task and no Admission (D294, D272).
func TestGateMCPHireOperatorExtension(t *testing.T) {
	e := newGateEnv(t)
	const opExt, own = "https://operator.example.com/ext/audit/v1", "https://seller.example.com/ext/own"
	kinds, paid := newHireSeller(t), newHireSeller(t)
	owned := newHireSellerCaps(t, `{"streaming":true,"extensions":[{"uri":"`+own+`","required":true}]}`)
	hook := newHookServer(t) // allows
	t.Setenv("KONTRA_TEST_ADMISSION_SECRET", "Hk4nW8qZ2xV6bM1tR9yL3pD7sF0gJ5cA")
	services := e.hireServices(kinds, `  paid:
    kind: a2a
    backend: { a2a: { url: "`+paid.srv.URL+`/base", allow_private: true } }
    allow: ["org:default"]
    task: { timeout: 10m, retries: 1 }
    annotations:
      card_extensions:
        - uri: "`+opExt+`"
          description: "The operator's own audit trail of every task."
          required: true
          params: { retentionDays: 30 }
  owned:
    kind: a2a
    backend: { a2a: { url: "`+owned.srv.URL+`/base", allow_private: true } }
    allow: ["org:default"]
    task: { timeout: 10m, retries: 1 }
  vault:
    kind: a2a
    backend: { tunnel: {} }
    allow: ["org:default"]
    sealed: true
operator_metadata_prefixes: ["kontra.op."]
hooks:
  admission: { url: "`+hook.srv.URL+`/admit", secret_env: KONTRA_TEST_ADMISSION_SECRET, timeout: 3s, operations: [task] }
`)
	timing := fastTiming
	timing.PlainWait = 4 * time.Second
	a := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), timing)
	key := e.mintKey("buyer")
	s := newMCPClient(t, a.url, key)
	s.initialize(`{"elicitation":{"url":{}}}`) // never -32042 any more (D351)
	tasks := func() int64 { return scalar[int64](t, e.pool, `select count(*) from kontra.tasks`) }
	served := func(svc, uri string) {
		t.Helper()
		eventually(t, 20*time.Second, "the served card of "+svc+" requires "+uri, func() bool {
			req, _ := http.NewRequest(http.MethodGet, a.url+"/agents/"+svc+"/.well-known/agent-card.json", nil)
			req.Header.Set("Authorization", "Bearer "+key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode == http.StatusOK && strings.Contains(string(b), uri)
		})
	}
	served("paid", opExt)
	served("owned", own)

	// The control: the A2A route refuses a SendMessage that does not activate the operator's required entry.
	req, _ := http.NewRequest(http.MethodPost, a.url+"/agents/paid", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m-paid","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"code":-32008`) || tasks() != 0 || hook.count() != 0 {
		t.Fatalf("the control, A2A SendMessage without A2A-Extensions: %d %s", resp.StatusCode, b)
	}

	// The hire: Admission with the operator's URI, an allow; the seller gets the task, and no activation it never
	// declared.
	_, ans := s.call("hire", `{"service":"paid","message":"hi"}`)
	r := ans.tool(t)
	if r.IsError || r.text() != "Hello World" || r.Structured.TaskID == "" {
		t.Fatalf("a hire of the service the operator's entry requires: %s", ans.raw)
	}
	taskID := r.Structured.TaskID
	if id := scalar[string](t, e.pool, `select id::text from kontra.tasks where hired and service = 'paid'`); id != taskID {
		t.Fatalf("the hired task is %s, the hire answered %s", id, taskID)
	}
	hook.mu.Lock()
	bodies := append([]json.RawMessage(nil), hook.bodies...)
	hook.mu.Unlock()
	var admit struct {
		Operation  string   `json:"operation"`
		TaskID     string   `json:"taskId"`
		Extensions []string `json:"extensions"`
		Principal  struct {
			Via string `json:"via"`
		} `json:"principal"`
	}
	if len(bodies) != 1 || json.Unmarshal(bodies[0], &admit) != nil || admit.Operation != "task" || admit.TaskID != taskID ||
		len(admit.Extensions) != 1 || admit.Extensions[0] != opExt || admit.Principal.Via != "api_key" {
		t.Fatalf("Admission was asked %s, want once for task %s with extensions [%s]", bodies, taskID, opExt)
	}
	if n := paid.count("SendMessage") + paid.count("SendStreamingMessage"); n != 1 {
		t.Errorf("the seller received %d messages, want 1", n)
	}
	if x := paid.activations(); len(x) != 0 {
		t.Errorf("the seller received A2A-Extensions %q", x)
	}

	// The hired task over A2A, by the same key and without A2A-Extensions (§2.4, D318): its reads and its cancel are
	// served (a completed task is not cancelable, -32002), and only a message to it is refused -32008.
	rpcA2A := func(method, params string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, a.url+"/agents/paid", strings.NewReader(
			`{"jsonrpc":"2.0","id":7,"method":"`+method+`","params":`+params+`}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("A2A-Version", "1.0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if b := rpcA2A("GetTask", `{"id":"`+taskID+`"}`); !strings.Contains(b, `"id":"`+taskID+`"`) || !strings.Contains(b, "TASK_STATE_COMPLETED") {
		t.Errorf("A2A GetTask of the hired task without A2A-Extensions: %s", b)
	}
	if b := rpcA2A("ListTasks", `{}`); !strings.Contains(b, taskID) || strings.Contains(b, "-32008") {
		t.Errorf("A2A ListTasks without A2A-Extensions: %s", b)
	}
	if b := rpcA2A("CancelTask", `{"id":"`+taskID+`"}`); !strings.Contains(b, `"code":-32002`) {
		t.Errorf("A2A CancelTask of the completed task without A2A-Extensions: %s, want -32002", b)
	}
	if b := rpcA2A("SendMessage", `{"message":{"messageId":"m-after","role":"ROLE_USER","taskId":"`+taskID+`","parts":[{"text":"more"}]}}`); !strings.Contains(b, `"code":-32008`) {
		t.Errorf("an A2A message to the hired task without A2A-Extensions: %s, want -32008", b)
	}

	// What a hire cannot carry: the seller's own required extension, and sealing. isError, no task, no Admission.
	before, asked := tasks(), hook.count()
	for _, c := range []struct{ svc, want string }{
		{"owned", own},
		{"vault", "takes sealed messages only"},
	} {
		_, ans = s.call("hire", `{"service":"`+c.svc+`","message":"hi"}`)
		if r := ans.tool(t); !r.IsError || !strings.Contains(r.text(), c.want) || !strings.Contains(r.text(), a.url+"/agents/"+c.svc) {
			t.Errorf("a hire of %s: %s, want isError naming %q and the service's A2A URL", c.svc, ans.raw, c.want)
		}
	}
	time.Sleep(time.Second)
	if n := tasks(); n != before {
		t.Errorf("the refused hires created %d tasks", n-before)
	}
	if n := hook.count(); n != asked {
		t.Errorf("the refused hires asked Admission %d times", n-asked)
	}
	if n := owned.count("SendMessage") + owned.count("SendStreamingMessage"); n != 0 {
		t.Errorf("the seller requiring its own extension received %d messages", n)
	}
}
