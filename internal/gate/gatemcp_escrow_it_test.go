package gate

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A per_task service, whose served card carries the operator's escrow entry with required: true (ext-escrow.md §1,
// spec §13.4), hired through a whole replica's /mcp (spec §12.2, §12.3; D300): the hire activates the entry for the
// buyer, so Admission is asked, with the escrow URI in its extensions and via api_key, and quotes; a session that
// declared elicitation.url gets -32042 whose url is the task's thread on the operator's page,
// https://app.kontra.sh/#/t/<task>, the pre-minted id the task then has; the A2A route still refuses a SendMessage
// that does not activate the entry (§2.4); nothing reaches the seller while the task is quoted, and once a person
// approves it the seller receives the message with no A2A-Extensions (the gate activated the entry, the buyer did
// not). A service whose seller's own card requires an extension, and a required-sealed one, are still isError with no
// task and no Admission (D294, D272).
func TestGateMCPHireOperatorExtension(t *testing.T) {
	e := newGateEnv(t)
	const escrow, own = "https://kontra.sh/ext/escrow/v1", "https://seller.example.com/ext/own"
	kinds, paid := newHireSeller(t), newHireSeller(t)
	owned := newHireSellerCaps(t, `{"streaming":true,"extensions":[{"uri":"`+own+`","required":true}]}`)
	hook := newHookServer(t)
	t.Setenv("KONTRA_TEST_ADMISSION_SECRET", "Hk4nW8qZ2xV6bM1tR9yL3pD7sF0gJ5cA")
	services := e.hireServices(kinds, `  paid:
    kind: a2a
    backend: { a2a: { url: "`+paid.srv.URL+`/base", allow_private: true } }
    allow: ["org:default"]
    task: { timeout: 10m, retries: 1 }
    annotations:
      search: { price: "15.00 EUR", price_model: per_task }
      card_extensions:
        - uri: "`+escrow+`"
          description: "Fixed quote held in escrow; released after a review window unless disputed."
          required: true
          params: { quote: { amount: "15.00", currency: EUR }, reviewWindowSeconds: 86400, taskTimeoutSeconds: 600, rails: [stripe], terms: "https://operator.example.com/terms/escrow/v1" }
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
operator_metadata_prefixes: ["kontra.escrow."]
hooks:
  admission: { url: "`+hook.srv.URL+`/admit", secret_env: KONTRA_TEST_ADMISSION_SECRET, timeout: 3s, operations: [task] }
`)
	timing := fastTiming
	timing.PlainWait = 4 * time.Second
	a := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), timing)
	key := e.mintKey("buyer")
	s := newMCPClient(t, a.url, key)
	s.initialize(`{"elicitation":{"url":{}}}`)
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
	served("paid", escrow)
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

	// The hire: Admission with the escrow URI, a quote, -32042 naming the task's thread.
	_, ans := s.call("hire", `{"service":"paid","message":"hi"}`)
	if ans.Error == nil || ans.Error.Code != -32042 {
		t.Fatalf("a hire of the per_task service: %s", ans.raw)
	}
	var data struct {
		Elicitations []struct {
			Mode, ElicitationID, URL string
		} `json:"elicitations"`
	}
	if err := json.Unmarshal(ans.Error.Data, &data); err != nil || len(data.Elicitations) != 1 {
		t.Fatalf("-32042's data: %s", ans.Error.Data)
	}
	el := data.Elicitations[0]
	taskID := scalar[string](t, e.pool, `select id::text from kontra.tasks where hired and service = 'paid'`)
	if el.Mode != "url" || el.ElicitationID == "" || el.URL != "https://app.kontra.sh/#/t/"+taskID {
		t.Fatalf("the URL elicitation %+v, want the thread of task %s", el, taskID)
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
		len(admit.Extensions) != 1 || admit.Extensions[0] != escrow || admit.Principal.Via != "api_key" {
		t.Fatalf("Admission was asked %s, want once for task %s with extensions [%s]", bodies, taskID, escrow)
	}
	if st := scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, taskID); st != "quoted" {
		t.Fatalf("the hired task is %s, want quoted", st)
	}
	if n := paid.count("SendMessage") + paid.count("SendStreamingMessage"); n != 0 {
		t.Fatalf("the seller received %d messages while the task was quoted", n)
	}

	// A person approves on the thread; the seller gets the task, and no activation it never declared.
	approve(t, taskID, true)
	eventually(t, 20*time.Second, "the approved task completes", func() bool {
		return scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, taskID) == "completed"
	})
	if n := paid.count("SendMessage") + paid.count("SendStreamingMessage"); n != 1 {
		t.Errorf("the seller received %d messages after the approval, want 1", n)
	}
	if x := paid.activations(); len(x) != 0 {
		t.Errorf("the seller received A2A-Extensions %q", x)
	}
	_, ans = s.call("hire", `{"service":"paid","message":"hi"}`)
	if rr := ans.tool(t); rr.IsError || rr.text() != "Hello World" || rr.Structured.TaskID != taskID {
		t.Errorf("the retry after the approval: %s", ans.raw)
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
