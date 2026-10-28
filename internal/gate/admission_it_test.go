package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.temporal.io/sdk/client"

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/temporaltest"
)

// admissionDouble is an operator's Admission server on loopback (spec §13.1; http:// allowed there, §10.3): it records
// each AdmitRequest with its Authorization header, and answers as its mode says. A quote names the pre-minted task id
// in the thread URL, as Cloud's does (D188).
type admissionDouble struct {
	mu   sync.Mutex
	mode string // quote, expired, down, allow, nullcode, comma
	reqs []hooks.AdmitRequest
	auth []string
	srv  *httptest.Server
}

func newAdmissionDouble(t *testing.T) *admissionDouble {
	t.Helper()
	d := &admissionDouble{mode: "quote"}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req hooks.AdmitRequest
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.reqs = append(d.reqs, req)
		d.auth = append(d.auth, r.Header.Get("Authorization"))
		mode := d.mode
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch mode {
		case "down":
			http.Error(w, "down", http.StatusServiceUnavailable)
		case "allow":
			io.WriteString(w, `{"outcome":"allow"}`)
		case "expired":
			fmt.Fprintf(w, `{"outcome":"quote","quote":{"approveUrl":"https://app.kontra.sh/#/t/%s","expiresAt":"2020-01-01T00:00:00Z"}}`, req.TaskID)
		case "nullcode": // not a hooks.json decision: refusal.code is an integer
			io.WriteString(w, `{"outcome":"deny","refusal":{"code":null,"message":"over budget"}}`)
		case "comma": // not RFC 3339, though time.Parse takes it
			fmt.Fprintf(w, `{"outcome":"quote","quote":{"approveUrl":"https://app.kontra.sh/#/t/%s","expiresAt":"2099-01-01T00:00:00,5Z"}}`, req.TaskID)
		default:
			fmt.Fprintf(w, `{"outcome":"quote","metadata":{"kontra.escrow.quote":{"amount":"15.00","currency":"EUR"}},"quote":{"approveUrl":"https://app.kontra.sh/#/t/%s","expiresAt":%q}}`,
				req.TaskID, time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339))
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *admissionDouble) set(mode string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mode = mode
}

func (d *admissionDouble) requests() ([]hooks.AdmitRequest, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]hooks.AdmitRequest(nil), d.reqs...), append([]string(nil), d.auth...)
}

// The remote Admission hook through a whole replica (M4 WP-01; spec §13.1, §13.2; DM4-8, DM4-17): an A2A SendMessage
// to a service whose Admission quotes creates the task quoted, with kontra.approve.url equal to the hook's URL and the
// decision's metadata, under the pre-minted id the hook was shown, and dispatches nothing; kontra.approval {accepted:
// true}, sent through Temporal as a control plane would, moves it to submitted and it completes. A hook that is down,
// and one whose quote has expired, are -32603 "admission unavailable" (503 on HTTP+JSON) with no task. On MCP, a quote
// for a plain call is the deny "priced tasks need tasks: durable" and reaches no upstream.
func TestRemoteAdmissionThroughTheGate(t *testing.T) {
	e := newGateEnv(t)
	ctx := context.Background()
	const secret = "Hk4nW8qZ2xV6bM1tR9yL3pD7sF0gJ5cA"
	t.Setenv("KONTRA_TEST_ADMISSION_SECRET", secret)
	adm := newAdmissionDouble(t)
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	direct := httptest.NewServer(up)
	t.Cleanup(direct.Close)
	services := e.helloService("org:default") + `  web:
    kind: mcp
    backend: { mcp: { url: "` + direct.URL + `/mcp", allow_private: true } }
    allow: ["org:default"]
operator_metadata_prefixes: ["kontra.escrow."]
hooks:
  admission: { url: "` + adm.srv.URL + `/admit", secret_env: KONTRA_TEST_ADMISSION_SECRET, timeout: 3s, operations: [task, call] }
`
	run := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), fastTiming)
	key := e.mintKey("buyer")
	c := a2aClient(t, run.url, key)

	// A quote: the task is created quoted (input-required on the wire) with the hook's URL, and nothing is dispatched.
	task := send(t, c, "hi", false)
	reqs, auths := adm.requests()
	if len(reqs) != 1 || reqs[0].Operation != hooks.OpTask || reqs[0].TaskID != string(task.ID) || reqs[0].Protocol != "a2a" ||
		reqs[0].Service.Name != "hello" || reqs[0].Principal.Via != "api_key" || auths[0] != "Bearer "+secret {
		t.Fatalf("the hook was asked %+v with %q; the task is %s", reqs, auths, task.ID)
	}
	approve := "https://app.kontra.sh/#/t/" + string(task.ID)
	if task.Status.State != a2a.TaskStateInputRequired || task.Metadata["kontra.approve.url"] != approve {
		t.Fatalf("the quoted task: state %s, metadata %v", task.Status.State, task.Metadata)
	}
	if st := scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, string(task.ID)); st != "quoted" {
		t.Fatalf("tasks.state = %s, want quoted", st)
	}
	if !scalar[bool](t, e.pool, `select quote_expires_at > now() from kontra.tasks where id = $1`, string(task.ID)) {
		t.Error("the quoted task's quote_expires_at is not in the future")
	}
	if got := getTask(t, c, task.ID); got.Metadata["kontra.approve.url"] != approve || got.Metadata["kontra.escrow.quote"] == nil {
		t.Errorf("GetTask of the quoted task: metadata %v", got.Metadata)
	}
	time.Sleep(500 * time.Millisecond)
	if n := e.seller.count("SendMessage") + e.seller.count("SendStreamingMessage"); n != 0 {
		t.Fatalf("a quoted task was dispatched (%d upstream messages)", n)
	}
	// A caller's message to the quoted task is refused (§7, §13.2: approval does not happen in the protocol).
	follow := a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("I accept"))
	if _, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: follow}); err == nil {
		t.Error("a caller's message to a quoted task was accepted")
	}
	if st := scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, string(task.ID)); st != "quoted" {
		t.Fatalf("after the caller's message tasks.state = %s, want quoted", st)
	}

	// The operator's approval: kontra.approval {accepted: true} moves it to submitted, and it completes.
	tc := temporaltest.Client(t)
	uctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	h, err := tc.UpdateWorkflow(uctx, client.UpdateWorkflowOptions{
		WorkflowID: lifecycle.WorkflowID(task.ID), UpdateName: lifecycle.UpdateApproval,
		Args: []any{lifecycle.ApprovalUpdate{Accepted: true}}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		t.Fatalf("kontra.approval: %v", err)
	}
	var res lifecycle.ApprovalResult
	if err := h.Get(uctx, &res); err != nil || res.State != "submitted" {
		t.Fatalf("kontra.approval answered %+v, %v; want submitted", res, err)
	}
	eventually(t, 30*time.Second, "the approved task completes", func() bool {
		return getTask(t, c, task.ID).Status.State == a2a.TaskStateCompleted
	})
	done := getTask(t, c, task.ID)
	if done.Status.Message == nil || len(done.Status.Message.Parts) == 0 || done.Status.Message.Parts[0].Text() != "Hello World" {
		t.Errorf("the completed task's status message: %+v", done.Status.Message)
	}
	if n := e.seller.count("SendMessage") + e.seller.count("SendStreamingMessage"); n != 1 {
		t.Errorf("the seller received %d messages, want 1", n)
	}

	// The hook down, a quote that has expired, and answers that are not valid decisions (a refusal whose code is null,
	// which the operator's refusal was once read from, as a final 403; an expiresAt with a comma fraction): -32603
	// "admission unavailable", no task.
	before := scalar[int64](t, e.pool, `select count(*) from kontra.tasks`)
	for _, mode := range []string{"down", "expired", "nullcode", "comma"} {
		adm.set(mode)
		_, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))})
		if !errors.Is(err, a2a.ErrInternalError) || !strings.Contains(err.Error(), "admission unavailable") {
			t.Errorf("a hook %s: %v, want -32603 admission unavailable", mode, err)
		}
		// HTTP+JSON answers 503 (spec §13.1).
		req, _ := http.NewRequest(http.MethodPost, run.url+"/agents/hello/message:send",
			strings.NewReader(`{"message":{"messageId":"m-`+mode+`","role":"ROLE_USER","parts":[{"text":"hi"}]}}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("A2A-Version", "1.0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "admission unavailable") || strings.Contains(string(body), secret) {
			t.Errorf("a hook %s on HTTP+JSON: %d %s", mode, resp.StatusCode, body)
		}
	}
	if after := scalar[int64](t, e.pool, `select count(*) from kontra.tasks`); after != before {
		t.Errorf("a refused admission created %d tasks", after-before)
	}

	// MCP: the hook asked about a plain call (operation call); a quote there is "priced tasks need tasks: durable".
	adm.set("quote")
	endpoint := run.url + "/mcp/web"
	st, hd, body := mcpPost(t, endpoint, key, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
	sid := hd.Get("Mcp-Session-Id")
	if st != 200 || sid == "" {
		t.Fatalf("initialize: %d %s", st, body)
	}
	st, _, body = mcpPost(t, endpoint, key, sid, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`)
	if st != 200 || !strings.Contains(body, `-32603`) || !strings.Contains(body, "priced tasks need tasks: durable") {
		t.Errorf("a quoted plain call: %d %s", st, body)
	}
	if up.Calls("echo") != 0 {
		t.Errorf("a quoted plain call reached the upstream")
	}
	reqs, _ = adm.requests()
	if last := reqs[len(reqs)-1]; last.Operation != hooks.OpCall || last.Tool != "echo" || last.Protocol != "mcp" {
		t.Errorf("the hook was asked %+v for the plain call", last)
	}
	adm.set("down")
	if st, _, body = mcpPost(t, endpoint, key, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`); st != http.StatusServiceUnavailable ||
		!strings.Contains(body, "admission unavailable") {
		t.Errorf("a plain call with the hook down: %d %s", st, body)
	}
	adm.set("allow")
	if st, _, body = mcpPost(t, endpoint, key, sid, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`); st != 200 ||
		!strings.Contains(body, `"hi"`) || up.Calls("echo") != 1 {
		t.Errorf("an allowed plain call: %d %s, upstream calls %d", st, body, up.Calls("echo"))
	}
}
