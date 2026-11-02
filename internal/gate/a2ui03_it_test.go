package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kontra.sh/kontra/internal/jcs"
)

// A2UI's marks and messages, as the A2UI v0.9.1 extension specification writes them (examples/sellers/a2ui).
const (
	a2uiExt  = "https://a2ui.org/a2a-extension/a2ui/v0.9.1"
	a2uiMime = "application/a2ui+json"
	a2uiMark = `{"mimeType":"application/a2ui+json"}`
)

var (
	a2uiCreate  = `{"version":"v0.9.1","createSurface":{"surfaceId":"booking","catalogId":"https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json"}}`
	a2uiUpdate  = `{"version":"v0.9.1","updateComponents":{"surfaceId":"booking","components":[{"id":"root","component":"Text","text":"Book <a table> & \u00e9t\u00e9 \u2603"}]}}`
	a2uiModel   = `{"version":"v0.9.1","updateDataModel":{"surfaceId":"booking","path":"/booking","value":{"guests":["2"],"limit":1e2,"ratio":0.25}}}`
	a2uiAction  = `{"version":"v0.9.1","action":{"name":"book","surfaceId":"booking","sourceComponentId":"book","timestamp":"2026-11-02T18:02:11Z","context":{"name":"Robin","guests":["4"],"date":"2026-11-09T19:30"}}}`
	a2uiCapsRaw = `{"v0.9":{"supportedCatalogIds":["https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json"]}}`
)

// a2ui03 is an A2A 0.3 agent written by hand, as a 0.3 SDK writes the wire (a2a-sdk < 0.4, which the A2UI Python
// reference pins): a 0.3 card declaring A2UI v0.9.1, and message/send answered from the message's first text, one
// A2UI message per data part, as that reference sends them. "a table" asks with the form (and one part in the a2a-go
// and a2a-python 1.x compat form of a list); "bare" answers a list where 0.3 has an object; a follow-up completes.
type a2ui03 struct {
	srv     *httptest.Server
	mu      sync.Mutex
	bodies  []json.RawMessage // the params of each message/send
	headers []http.Header
}

func newA2UI03(t *testing.T) *a2ui03 {
	t.Helper()
	a := &a2ui03{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"protocolVersion":"0.3.0","name":"booking (0.3)","description":"An A2A 0.3 agent that asks with A2UI",
"url":%q,"preferredTransport":"JSONRPC","version":"1.0.0","capabilities":{"streaming":false,"extensions":[{"uri":%q,
"required":false,"params":{"supportedCatalogIds":["https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json"]}}]},
"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[{"id":"book","name":"book","description":"book","tags":["a2ui"]}]}`,
			a.srv.URL+"/", a2uiExt)
	})
	mux.HandleFunc("POST /{$}", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		var params struct {
			Message struct {
				TaskID string `json:"taskId"`
				Parts  []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		}
		_ = json.Unmarshal(req.Params, &params)
		a.mu.Lock()
		a.bodies = append(a.bodies, req.Params)
		a.headers = append(a.headers, r.Header.Clone())
		a.mu.Unlock()
		first := ""
		if len(params.Message.Parts) > 0 {
			first = params.Message.Parts[0].Text
		}
		var result string
		switch {
		case req.Method != "message/send":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"not here"}}`, req.ID)
			return
		case params.Message.TaskID != "":
			result = task03(params.Message.TaskID, "completed", `{"kind":"text","text":"Booked a table for 4."}`)
		case first == "bare":
			result = task03("up-bare", "input-required", `{"kind":"data","data":[`+a2uiCreate+`],"metadata":`+a2uiMark+`}`)
		default:
			result = task03("up-1", "input-required", `{"kind":"text","text":"Fill in the booking form."}`,
				`{"kind":"data","data":`+a2uiCreate+`,"metadata":`+a2uiMark+`}`,
				`{"kind":"data","data":`+a2uiUpdate+`,"metadata":`+a2uiMark+`}`,
				`{"kind":"data","data":`+a2uiModel+`,"metadata":`+a2uiMark+`}`,
				`{"kind":"data","data":{"value":[`+a2uiCreate+`,`+a2uiModel+`]},"metadata":{"mimeType":"`+a2uiMime+`","data_part_compat":true}}`)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	})
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	return a
}

func task03(id, state string, parts ...string) string {
	return `{"kind":"task","id":"` + id + `","contextId":"ctx-1","status":{"state":"` + state + `","message":{"kind":"message",` +
		`"messageId":"a-` + id + `","role":"agent","taskId":"` + id + `","contextId":"ctx-1","parts":[` + strings.Join(parts, ",") + `]}}}`
}

// sameJSON reports whether a and b are equal under RFC 8785 (JCS): the same JSON value.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	ca, err := jcs.Transform(a)
	if err != nil {
		t.Fatalf("%s: %v", a, err)
	}
	cb, err := jcs.Transform(b)
	if err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return bytes.Equal(ca, cb)
}

// rpc posts one A2A 1.0 JSON-RPC request to svc with key and the extra headers, and returns the response's headers
// and its result.
func rpc(t *testing.T, svc, key, method, params string, hdr http.Header) (http.Header, json.RawMessage) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, svc, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":`+params+`}`))
	req.Header = hdr.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("A2A-Version", "1.0")
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &out); err != nil || out.Error != nil || out.Result == nil {
		t.Fatalf("%s: %d %s %v", method, resp.StatusCode, raw, err)
	}
	return resp.Header, out.Result
}

// wireTask is the members of an A2A 1.0 task this test reads, its parts left as the gate wrote them.
type wireTask struct {
	ID     string `json:"id"`
	Status struct {
		State   string `json:"state"`
		Message struct {
			Parts    []json.RawMessage `json:"parts"`
			Metadata map[string]any    `json:"metadata"`
		} `json:"message"`
	} `json:"status"`
}

func taskOfResult(t *testing.T, result json.RawMessage) wireTask {
	t.Helper()
	var r struct {
		Task *wireTask `json:"task"`
	}
	if err := json.Unmarshal(result, &r); err != nil || r.Task == nil {
		if err := json.Unmarshal(result, &r.Task); err != nil || r.Task == nil || r.Task.ID == "" {
			t.Fatalf("not a task: %s", result)
		}
	}
	return *r.Task
}

// A2UI through a direct backend that speaks A2A 0.3, both ways, as a 1.0 caller sees it (spec/relay.md §2.4, §6.3;
// D326, D327). The caller activates A2UI and sends its capabilities: the gate echoes the URI, the 0.3 agent receives
// it as X-A2A-Extensions and the capabilities as they were sent. The agent's form, one A2UI message per data part, is
// served to the caller with each part's data and metadata equal under JCS to what the agent wrote (HTML characters,
// non-ASCII text and number spellings included), from SendMessage and from GetTask, and nothing added; its list in
// the SDKs' compat form ({"value": …}, data_part_compat) reaches the caller as the list, the flag gone. The caller's
// action, one message in its part, reaches the agent equal under JCS with its mark alone; the caller's action list
// reaches it as {"value": [...]} with data_part_compat. A 0.3 agent's list where 0.3 has an object fails the task
// upstream_error, naming the malformed response.
func TestA2UIThroughA03Agent(t *testing.T) {
	e := newGateEnv(t)
	agent := newA2UI03(t)
	port := freePort(t)
	path := e.replicaConfig(port, 10*time.Second, `services:
  ui03:
    kind: a2a
    backend: { a2a: { url: "`+agent.srv.URL+`", allow_private: true } }
    allow: ["org:default"]
    task: { timeout: 10m, retries: 0 }
`)
	run := e.start("a", path, fastTiming)
	key := e.mintKey("a2ui")
	svc := run.url + "/agents/ui03"
	eventually(t, 20*time.Second, "the 0.3 agent's card", func() bool {
		status, _, _ := get(t, run.url, "/agents/ui03/.well-known/agent-card.json", key)
		return status == http.StatusOK
	})

	// The form, from the agent to the caller.
	activate := http.Header{"A2A-Extensions": {a2uiExt}}
	hdr, result := rpc(t, svc, key, "SendMessage", `{"message":{"messageId":"m-1","role":"ROLE_USER","parts":[{"text":"a table"}],
"metadata":{"a2uiClientCapabilities":`+a2uiCapsRaw+`}}}`, activate)
	if got := hdr.Values("A2A-Extensions"); len(got) != 1 || got[0] != a2uiExt {
		t.Errorf("the activation echoed: %q, want %s", got, a2uiExt)
	}
	task := taskOfResult(t, result)
	wantParts := []string{
		`{"text":"Fill in the booking form."}`,
		`{"data":` + a2uiCreate + `,"metadata":` + a2uiMark + `}`,
		`{"data":` + a2uiUpdate + `,"metadata":` + a2uiMark + `}`,
		`{"data":` + a2uiModel + `,"metadata":` + a2uiMark + `}`,
		`{"data":[` + a2uiCreate + `,` + a2uiModel + `],"metadata":` + a2uiMark + `}`,
	}
	checkParts := func(where string, task wireTask) {
		t.Helper()
		if task.Status.State != "TASK_STATE_INPUT_REQUIRED" || len(task.Status.Message.Parts) != len(wantParts) {
			t.Fatalf("%s: %s with %d parts", where, task.Status.State, len(task.Status.Message.Parts))
		}
		for i, want := range wantParts {
			if got := task.Status.Message.Parts[i]; !sameJSON(t, got, []byte(want)) {
				t.Errorf("%s, part %d:\n%s\nwant\n%s", where, i, got, want)
			}
		}
	}
	checkParts("SendMessage", task)
	_, got := rpc(t, svc, key, "GetTask", `{"id":"`+task.ID+`"}`, nil)
	checkParts("GetTask", taskOfResult(t, got))

	agent.mu.Lock()
	first, firstHdr := agent.bodies[0], agent.headers[0]
	agent.mu.Unlock()
	if v := firstHdr.Values("X-A2A-Extensions"); len(v) != 1 || v[0] != a2uiExt {
		t.Errorf("the 0.3 agent's X-A2A-Extensions: %q, want %s", v, a2uiExt)
	}
	var sent struct {
		Message struct {
			Metadata map[string]json.RawMessage `json:"metadata"`
		} `json:"message"`
	}
	if err := json.Unmarshal(first, &sent); err != nil || !sameJSON(t, sent.Message.Metadata["a2uiClientCapabilities"], []byte(a2uiCapsRaw)) {
		t.Errorf("the capabilities the agent received: %s", first)
	}

	// The action, from the caller to the agent: once as one message, once as A2UI's list.
	_, result = rpc(t, svc, key, "SendMessage", `{"message":{"messageId":"m-2","role":"ROLE_USER","taskId":"`+task.ID+`","parts":[
{"data":`+a2uiAction+`,"metadata":`+a2uiMark+`},{"data":[`+a2uiAction+`],"metadata":`+a2uiMark+`}]}}`, activate)
	done := taskOfResult(t, result)
	if done.Status.State != "TASK_STATE_COMPLETED" || len(done.Status.Message.Parts) != 1 ||
		!sameJSON(t, done.Status.Message.Parts[0], []byte(`{"text":"Booked a table for 4."}`)) {
		t.Fatalf("after the action: %s %s", done.Status.State, done.Status.Message.Parts)
	}
	agent.mu.Lock()
	second := agent.bodies[len(agent.bodies)-1]
	agent.mu.Unlock()
	var reply struct {
		Message struct {
			TaskID string            `json:"taskId"`
			Parts  []json.RawMessage `json:"parts"`
		} `json:"message"`
	}
	if err := json.Unmarshal(second, &reply); err != nil || reply.Message.TaskID != "up-1" || len(reply.Message.Parts) != 2 {
		t.Fatalf("the action at the agent: %s", second)
	}
	for i, want := range []string{
		`{"kind":"data","data":` + a2uiAction + `,"metadata":` + a2uiMark + `}`,
		`{"kind":"data","data":{"value":[` + a2uiAction + `]},"metadata":{"mimeType":"` + a2uiMime + `","data_part_compat":true}}`,
	} {
		if !sameJSON(t, reply.Message.Parts[i], []byte(want)) {
			t.Errorf("the action's part %d at the agent:\n%s\nwant\n%s", i, reply.Message.Parts[i], want)
		}
	}

	// A list where 0.3 has an object.
	_, result = rpc(t, svc, key, "SendMessage", `{"message":{"messageId":"m-3","role":"ROLE_USER","parts":[{"text":"bare"}]}}`, activate)
	bare := taskOfResult(t, result)
	text := ""
	if len(bare.Status.Message.Parts) == 1 {
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(bare.Status.Message.Parts[0], &p)
		text = p.Text
	}
	if bare.Status.State != "TASK_STATE_FAILED" || bare.Status.Message.Metadata["kontra.failure"] != "upstream_error" ||
		!strings.Contains(text, "malformed response") {
		t.Fatalf("a list where 0.3 has an object: %s %v %q, want failed: upstream_error naming the malformed response",
			bare.Status.State, bare.Status.Message.Metadata, text)
	}
}
