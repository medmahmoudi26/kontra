package mcprelay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/upstream"
)

func TestTaskSupported(t *testing.T) {
	for tool, want := range map[string]bool{
		`{"name":"a","execution":{"taskSupport":"optional"}}`:  true,
		`{"name":"a","execution":{"taskSupport":"required"}}`:  true,
		`{"name":"a","execution":{"taskSupport":"forbidden"}}`: false,
		`{"name":"a"}`:                                        false,
		`{"name":"a","execution":{}}`:                         false,
		`{"name":"a","execution":"optional"}`:                 false,
		`{"name":"a","execution":{"TaskSupport":"required"}}`: false,
		`{"name":"a","execution":{"taskSupport":"forbidden","TASKSUPPORT":"required"}}`: false,
		`{"name":"a","execution":{"taskSupport":"optional","taskSupport":"required"}}`:  false,
	} {
		if got := taskSupported(json.RawMessage(tool)); got != want {
			t.Errorf("%s: %v", tool, got)
		}
	}
}

// The related-task key is rewritten in place: every other member keeps its bytes, a sealed envelope among them
// (D174), and an object without the key comes back as it is.
func TestSetRelatedTask(t *testing.T) {
	in := `{"_meta":{"io.modelcontextprotocol/related-task":{"taskId":"up-1"},"sh.kontra/sealed":{"v":1,"envelope":"QUJD"}},"message":"x <y>"}`
	out, err := setRelatedTask(json.RawMessage(in), "gate-1")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"_meta":{"io.modelcontextprotocol/related-task":{"taskId":"gate-1"},"sh.kontra/sealed":{"v":1,"envelope":"QUJD"}},"message":"x <y>"}`
	if string(out) != want {
		t.Fatalf("%s", out)
	}
	for _, s := range []string{`{"message":"x"}`, `{"_meta":{"progressToken":1}}`, `"text"`} {
		if out, err := setRelatedTask(json.RawMessage(s), "g"); err != nil || string(out) != s {
			t.Errorf("%s: %s %v", s, out, err)
		}
	}
}

// The dispatch's tools/call is the client's, byte for byte, with params.task {ttl: the time to the deadline} (§11.5
// steps 3–4): its id is the client's.
func TestTaskCall(t *testing.T) {
	deadline := time.Now().Add(90 * time.Second)
	r := &legacyRun{row: &store.TaskRow{Deadline: &deadline}, work: &store.MCPWork{
		Request: []byte(`{"jsonrpc":"2.0","id":"c-7","method":"tools/call","params":{"name":"x","arguments":{"q":"<&>"},"task":{"ttl":1}}}`),
	}}
	b, err := r.taskCall()
	if err != nil {
		t.Fatal(err)
	}
	m, err := mcpwire.Parse(b, mcpwire.FromClient)
	if err != nil {
		t.Fatal(err)
	}
	var task struct {
		TTL int64 `json:"ttl"`
	}
	tk, _ := mcpwire.Member(m.Params, "task")
	_ = json.Unmarshal(tk, &task)
	if string(m.ID) != `"c-7"` || task.TTL < 80_000 || task.TTL > 90_000 || !strings.Contains(string(b), `"arguments":{"q":"<&>"}`) {
		t.Fatalf("%s", b)
	}
	// The dispatch's own ids never equal the client's.
	r.work.RequestID = json.RawMessage(`"kontra.dispatch.1"`)
	if id := r.id(); id == "kontra.dispatch.1" {
		t.Fatal("the dispatch took the client's id")
	}
}

func TestCancelDeclared(t *testing.T) {
	for res, want := range map[string]bool{
		`{"capabilities":{"tasks":{"list":{},"cancel":{}}}}`: true,
		`{"capabilities":{"tasks":{"list":{}}}}`:             false,
		`{"capabilities":{}}`:                                false,
		`{}`:                                                 false,
	} {
		if got := cancelDeclared(json.RawMessage(res)); got != want {
			t.Errorf("%s: %v", res, got)
		}
	}
}

// §11.8's three forms of a terminal task's tasks/result, and §11.6's task object.
func TestOutcomeForms(t *testing.T) {
	id := uuid.MustParse("0192f0b4-0000-7000-8000-0000000000aa")
	created := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	ret := created.Add(7 * 24 * time.Hour)
	row := &store.TaskRow{ID: id, State: store.StateCompleted, CreatedAt: created, StatusAt: created.Add(time.Second),
		RetentionUntil: &ret, Snapshot: &a2a.Task{Metadata: map[string]any{store.MetaMCPMeta: map[string]any{"sh.kontra/escrow": "held"}}}}
	b, err := outcomeMessage(row, store.MCPOutcome{Kind: store.KindResult, Result: json.RawMessage(`{"content":[],"isError":false}`)}, json.RawMessage(`9`))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"jsonrpc":"2.0","id":9,"result":{"content":[],"isError":false,"_meta":{"io.modelcontextprotocol/related-task":{"taskId":"`+id.String()+`"},"sh.kontra/escrow":"held"}}}` {
		t.Fatalf("a stored result: %s", b)
	}
	b, _ = outcomeMessage(row, store.MCPOutcome{Kind: store.KindResult, Error: json.RawMessage(`{"code":-32000,"message":"boom","data":{"x":1}}`)}, json.RawMessage(`9`))
	if string(b) != `{"jsonrpc":"2.0","id":9,"error":{"code":-32000,"message":"boom","data":{"x":1}}}` {
		t.Fatalf("a stored error: %s", b)
	}
	failed := *row
	failed.State = store.StateFailed
	failed.Snapshot = &a2a.Task{Metadata: map[string]any{store.MetaFailure: "tunnel_lost"}}
	b, _ = outcomeMessage(&failed, store.MCPOutcome{Kind: store.KindStatus}, json.RawMessage(`9`))
	if !strings.Contains(string(b), `"code":-32603`) || !strings.Contains(string(b), `"message":"the connection to the service was lost"`) ||
		!strings.Contains(string(b), `"data":{"sh.kontra/failure":"tunnel_lost"}`) {
		t.Fatalf("a gate failure: %s", b)
	}
	canceled := *row
	canceled.State = store.StateCanceled
	canceled.Snapshot = &a2a.Task{Metadata: map[string]any{store.MetaCanceled: "declined"}}
	b, _ = outcomeMessage(&canceled, store.MCPOutcome{Kind: store.KindStatus}, json.RawMessage(`9`))
	if !strings.Contains(string(b), `"message":"task cancelled"`) || !strings.Contains(string(b), `"data":{"sh.kontra/canceled":"declined"}`) {
		t.Fatalf("a cancel: %s", b)
	}
	obj, err := taskResult(&failed, store.ServiceRef{})
	if err != nil {
		t.Fatal(err)
	}
	var task map[string]any
	_ = json.Unmarshal(obj, &task)
	meta, _ := task["_meta"].(map[string]any)
	if task["taskId"] != id.String() || task["status"] != "failed" || task["createdAt"] != "2026-10-27T01:00:00.000Z" ||
		task["ttl"] != float64(7*24*3600*1000) || task["pollInterval"] != float64(1000) || meta["sh.kontra/failure"] != "tunnel_lost" {
		t.Fatalf("tasks/get: %s", obj)
	}
}

// ttl before the end is an upper bound: the deadline (a quoted task's: its quote's expiry plus the timeout) plus the
// retention (§11.6).
func TestTTL(t *testing.T) {
	c := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	pol := store.ServiceRef{Timeout: time.Hour, Retention: 24 * time.Hour}
	dl := c.Add(time.Hour)
	if got := ttlOf(&store.TaskRow{CreatedAt: c, Deadline: &dl}, pol); got != (25 * time.Hour).Milliseconds() {
		t.Errorf("submitted: %d", got)
	}
	exp := c.Add(10 * time.Minute)
	if got := ttlOf(&store.TaskRow{CreatedAt: c, QuoteExpiresAt: &exp}, pol); got != (25*time.Hour + 10*time.Minute).Milliseconds() {
		t.Errorf("quoted: %d", got)
	}
}

// A dispatch exchange's failures are classed as §6.4's table reads them, and an event stream's other messages reach
// onMsg before the answer; a message strict decoding refuses is a protocol failure (C1).
func TestLinkExchanges(t *testing.T) {
	var status int
	var body string
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		for k, v := range map[string]string{"Retry-After": "7"} {
			w.Header().Set(k, v)
		}
		if strings.HasPrefix(body, "data:") {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	be, err := upstream.NewDirect(srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	k := &mcpLink{backend: be, sid: "up-sid", version: mcpwire.V20251125}
	ctx := context.Background()
	for _, c := range []struct {
		status int
		body   string
		class  exchangeClass
	}{
		{401, "", classProtocol}, {403, "", classProtocol}, {404, "", classGone}, {429, "", classThrottled},
		{503, "", classUnreachable}, {502, "", classUnreachable}, {418, "", classProtocol},
		{200, `{"jsonrpc":"2.0","id":"kontra.dispatch.1","error":{"code":-32602,"message":"gone"}}`, classRPC},
		{200, `{"jsonrpc":"2.0","id":"kontra.dispatch.1","result":{"status":"working","STATUS":"completed"}}`, classProtocol},
		{200, `not json`, classProtocol},
	} {
		status, body = c.status, c.body
		_, err := k.request(ctx, "kontra.dispatch.1", "tasks/get", json.RawMessage(`{"taskId":"t"}`), nil)
		var xe *exchangeError
		if !errors.As(err, &xe) || xe.class != c.class {
			t.Errorf("%d %s: %v", c.status, c.body, err)
		}
		if c.status == 429 && xe.retryAfter != 7*time.Second {
			t.Errorf("Retry-After: %v", xe.retryAfter)
		}
	}
	if hdr.Get(mcpwire.HeaderSessionID) != "up-sid" || hdr.Get(mcpwire.HeaderProtocolVersion) != mcpwire.V20251125 {
		t.Errorf("the dispatch's headers: %v", hdr)
	}
	status = 200
	body = "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":5,\"method\":\"elicitation/create\",\"params\":{\"message\":\"m\"}}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":\"kontra.dispatch.2\",\"result\":{\"status\":\"working\"}}\n\n"
	var others []string
	m, err := k.request(ctx, "kontra.dispatch.2", "tasks/get", json.RawMessage(`{}`), func(m *mcpwire.Message, raw []byte) {
		others = append(others, m.Method)
	})
	if err != nil || m == nil || len(others) != 2 || others[1] != "elicitation/create" {
		t.Fatalf("an event stream's answer: %v %v %v", m, others, err)
	}
	body = "data: {\"jsonrpc\":\"2.0\",\"id\":5,\"method\":\"elicitation/create\",\"Method\":\"x\"}\n\n"
	if _, err := k.request(ctx, "kontra.dispatch.3", "tasks/get", nil, nil); asExchange(err).class != classProtocol {
		t.Fatalf("a refused event: %v", err)
	}
}
