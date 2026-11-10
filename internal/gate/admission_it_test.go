package gate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
)

// admissionDouble is an operator's Admission server on loopback (spec §13.1; http:// allowed there, §10.3): it records
// each AdmitRequest with its Authorization header, and answers as its mode says.
type admissionDouble struct {
	mu   sync.Mutex
	mode string // allow, deny, quote, down, nullcode
	reqs []hooks.AdmitRequest
	auth []string
	srv  *httptest.Server
}

func newAdmissionDouble(t *testing.T) *admissionDouble {
	t.Helper()
	d := &admissionDouble{mode: "allow"}
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
		case "deny":
			io.WriteString(w, `{"outcome":"deny","refusal":{"code":-32061,"message":"over budget"}}`)
		case "quote": // the outcome D351 retired: no longer a hooks.json decision
			io.WriteString(w, `{"outcome":"quote","quote":{"approveUrl":"https://app.kontra.sh/#/t/x","expiresAt":"2099-01-01T00:00:00Z"}}`)
		case "nullcode": // not a hooks.json decision: refusal.code is an integer
			io.WriteString(w, `{"outcome":"deny","refusal":{"code":null,"message":"over budget"}}`)
		default:
			io.WriteString(w, `{"outcome":"allow","metadata":{"kontra.op.plan":"standard"}}`)
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

// The remote Admission hook through a whole replica (M4 WP-01; spec §13.1; D275, D284, D351): an A2A SendMessage to a
// service whose Admission allows creates the task submitted with the decision's metadata, under the pre-minted id the
// hook was shown, and it completes. A deny refuses with its code and creates nothing. A hook that is down, one that
// answers the retired quote outcome, and one whose answer is not a valid decision are -32603 "admission unavailable"
// (503 on HTTP+JSON) with no task. On MCP, the hook is asked about a plain call; the quote outcome there fails closed
// too and reaches no upstream.
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
operator_metadata_prefixes: ["kontra.op."]
hooks:
  admission: { url: "` + adm.srv.URL + `/admit", secret_env: KONTRA_TEST_ADMISSION_SECRET, timeout: 3s, operations: [task, call] }
`
	run := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), fastTiming)
	key := e.mintKey("buyer")
	c := a2aClient(t, run.url, key)

	// An allow: the task is created submitted with the decision's metadata under the id the hook was shown, and runs.
	task := send(t, c, "hi", false)
	reqs, auths := adm.requests()
	if len(reqs) != 1 || reqs[0].Operation != hooks.OpTask || reqs[0].TaskID != string(task.ID) || reqs[0].Protocol != "a2a" ||
		reqs[0].Service.Name != "hello" || reqs[0].Principal.Via != "api_key" || auths[0] != "Bearer "+secret {
		t.Fatalf("the hook was asked %+v with %q; the task is %s", reqs, auths, task.ID)
	}
	eventually(t, 30*time.Second, "the allowed task completes", func() bool {
		return getTask(t, c, task.ID).Status.State == a2a.TaskStateCompleted
	})
	done := getTask(t, c, task.ID)
	if done.Metadata["kontra.op.plan"] != "standard" || done.Metadata["kontra.approve.url"] != nil {
		t.Errorf("the allowed task's metadata: %v", done.Metadata)
	}
	if done.Status.Message == nil || len(done.Status.Message.Parts) == 0 || done.Status.Message.Parts[0].Text() != "Hello World" {
		t.Errorf("the completed task's status message: %+v", done.Status.Message)
	}
	if n := e.seller.count("SendMessage") + e.seller.count("SendStreamingMessage"); n != 1 {
		t.Errorf("the seller received %d messages, want 1", n)
	}
	if !scalar[bool](t, e.pool, `select quote_expires_at is null and state = 'completed' from kontra.tasks where id = $1`, string(task.ID)) {
		t.Error("the allowed task has a quote expiry, or did not complete")
	}

	// A deny: refused with the operator's code (PERMISSION_DENIED on A2A, D325), no task.
	before := scalar[int64](t, e.pool, `select count(*) from kontra.tasks`)
	adm.set("deny")
	if _, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}); err == nil || !strings.Contains(err.Error(), "over budget") {
		t.Errorf("a deny: %v, want the operator's refusal", err)
	}

	// The hook down, the retired quote outcome, and an answer that is not a valid decision (a refusal whose code is
	// null): -32603 "admission unavailable", no task.
	for _, mode := range []string{"down", "quote", "nullcode"} {
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

	// MCP: the hook asked about a plain call (operation call); the quote outcome there fails closed too.
	adm.set("allow")
	endpoint := run.url + "/mcp/web"
	st, hd, body := mcpPost(t, endpoint, key, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
	sid := hd.Get("Mcp-Session-Id")
	if st != 200 || sid == "" {
		t.Fatalf("initialize: %d %s", st, body)
	}
	adm.set("quote")
	st, _, body = mcpPost(t, endpoint, key, sid, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`)
	if st != http.StatusServiceUnavailable || !strings.Contains(body, `-32603`) || !strings.Contains(body, "admission unavailable") {
		t.Errorf("a plain call answered quote: %d %s", st, body)
	}
	if up.Calls("echo") != 0 {
		t.Errorf("a plain call answered quote reached the upstream")
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
