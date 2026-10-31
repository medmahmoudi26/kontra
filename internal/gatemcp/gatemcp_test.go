package gatemcp

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/store"
)

// ParseRef reads a ref as the gate's tenancy writes it (§12.2, D271): <name> on a single-tenant gate,
// <tenant>/<name> on a multi-tenant one; anything else is the unknown-service error.
func TestParseRef(t *testing.T) {
	for _, tc := range []struct {
		in    string
		multi bool
		want  Ref
		ok    bool
	}{
		{"hello", false, Ref{Name: "hello"}, true},
		{"hello-2", false, Ref{Name: "hello-2"}, true},
		{"acme/hello", false, Ref{}, false},
		{"acme/hello", true, Ref{Tenant: "acme", Name: "hello"}, true},
		{"anon-0123456789abcdef/x", true, Ref{Tenant: "anon-0123456789abcdef", Name: "x"}, true},
		{"hello", true, Ref{}, false},
		{"Hello", false, Ref{}, false},
		{"-hello", false, Ref{}, false},
		{"a/b/c", true, Ref{}, false},
		{"acme/", true, Ref{}, false},
		{"", false, Ref{}, false},
	} {
		got, err := ParseRef(tc.in, tc.multi)
		if tc.ok != (err == nil) || got != tc.want {
			t.Errorf("ParseRef(%q, multi %v) = %+v, %v; want %+v ok %v", tc.in, tc.multi, got, err, tc.want, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrUnknownService) {
			t.Errorf("ParseRef(%q): %v is not the unknown-service error", tc.in, err)
		}
		if err == nil && got.String() != tc.in {
			t.Errorf("Ref.String() = %q, want %q", got.String(), tc.in)
		}
	}
}

// FirstRequest is SHA-256(utf8(ref) ‖ 0x00 ‖ JCS(arguments)) (§12.3): a vector computed independently (member order,
// white space, a non-integer number and a non-ASCII string canonicalized), and arguments that differ only in their
// encoding give the same key.
func TestFirstRequest(t *testing.T) {
	args := json.RawMessage(`{ "service" : "acme/hello", "message": {"parts":[{"text":"hi","metadata":{"b":1,"a":2.50}}]}, "contextId":"cé" }`)
	got, err := FirstRequest(Ref{Tenant: "acme", Name: "hello"}, args)
	if err != nil {
		t.Fatal(err)
	}
	if want := "59fb9ed2423c50bdd6ca76989cbf0e24add6a41cdb996e2e5992578d7d88881c"; hex.EncodeToString(got[:]) != want {
		t.Fatalf("FirstRequest = %x, want %s", got, want)
	}
	same, _ := FirstRequest(Ref{Tenant: "acme", Name: "hello"},
		json.RawMessage(`{"contextId":"cé","service":"acme/hello","message":{"parts":[{"metadata":{"a":2.5,"b":1},"text":"hi"}]}}`))
	if same != got {
		t.Error("the same arguments in another encoding gave another key")
	}
	other, _ := FirstRequest(Ref{Name: "hello"}, args)
	if other == got {
		t.Error("another ref gave the same key")
	}
	if _, err := FirstRequest(Ref{Name: "x"}, json.RawMessage(`{"a":`)); err == nil {
		t.Error("arguments that are not JSON gave a key")
	}
}

// inlineRefs replaces every {"$ref": "#/$defs/<name>"} in v by the $defs entry, recursively.
func inlineRefs(t *testing.T, v any, defs map[string]any) any {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		if ref, ok := x["$ref"].(string); ok && len(x) == 1 {
			name, ok := strings.CutPrefix(ref, "#/$defs/")
			if !ok || defs[name] == nil {
				t.Fatalf("unresolvable $ref %q", ref)
			}
			return inlineRefs(t, defs[name], defs)
		}
		out := map[string]any{}
		for k, e := range x {
			out[k] = inlineRefs(t, e, defs)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = inlineRefs(t, e, defs)
		}
		return out
	}
	return v
}

// ToolsList's input schemas are spec/schema/mcp-tools.json's $defs (their $refs inlined) under JCS; every tool
// declares readOnlyHint and destructiveHint; hire's execution.taskSupport is "optional" in a 2025-11-25 session's list
// only (§12.5).
func TestToolsListSchemas(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "spec", "schema", "mcp-tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Defs map[string]any `json:"$defs"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{mcpwire.V20251125, mcpwire.V20250618} {
		raw, err := ToolsList(mcpwire.EraLegacy, v)
		if err != nil {
			t.Fatal(err)
		}
		var list struct {
			Tools []map[string]json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range list.Tools {
			var name string
			_ = json.Unmarshal(tool["name"], &name)
			names = append(names, name)
			want, err := json.Marshal(inlineRefs(t, file.Defs[name], file.Defs))
			if err != nil {
				t.Fatal(err)
			}
			wantJCS, _ := jcs.Transform(want)
			gotJCS, err := jcs.Transform(tool["inputSchema"])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotJCS, wantJCS) {
				t.Errorf("%s's inputSchema\n%s\nwant mcp-tools.json's\n%s", name, gotJCS, wantJCS)
			}
			var ann map[string]json.RawMessage
			_ = json.Unmarshal(tool["annotations"], &ann)
			if ann["readOnlyHint"] == nil || ann["destructiveHint"] == nil {
				t.Errorf("%s's annotations %s lack readOnlyHint or destructiveHint", name, tool["annotations"])
			}
			_, exec := tool["execution"]
			if wantExec := name == ToolHire && v == mcpwire.V20251125; exec != wantExec {
				t.Errorf("%s in a %s list: execution present %v, want %v", name, v, exec, wantExec)
			}
			if exec && string(tool["execution"]) != `{"taskSupport":"optional"}` {
				t.Errorf("hire's execution = %s", tool["execution"])
			}
		}
		if !reflect.DeepEqual(names, []string{ToolSearch, ToolDescribe, ToolHire, ToolReply}) {
			t.Errorf("tools %v", names)
		}
	}
}

func taskWith(state a2a.TaskState, msg *a2a.Message, md map[string]any, artifacts ...*a2a.Artifact) *a2a.Task {
	return &a2a.Task{ID: "019a0000-0000-7000-8000-000000000001", ContextID: "ctx",
		Status: a2a.TaskStatus{State: state, Message: msg}, Metadata: md, Artifacts: artifacts}
}

func agentText(text string) *a2a.Message {
	return &a2a.Message{ID: "m", Role: a2a.MessageRoleAgent, Parts: a2a.ContentParts{a2a.NewTextPart(text)}}
}

type rendered struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		URI      string `json:"uri"`
		Name     string `json:"name"`
		MIMEType string `json:"mimeType"`
		Resource *struct {
			URI      string `json:"uri"`
			MIMEType string `json:"mimeType"`
			Blob     string `json:"blob"`
		} `json:"resource"`
	} `json:"content"`
	StructuredContent map[string]json.RawMessage `json:"structuredContent"`
	IsError           bool                       `json:"isError"`
	Meta              map[string]any             `json:"_meta"`
}

func renderOf(t *testing.T, task *a2a.Task, state store.State) rendered {
	t.Helper()
	b, err := Render(task, state)
	if err != nil {
		t.Fatal(err)
	}
	var r rendered
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return r
}

// Render (§12.2, D289): text parts as text; data parts as text (their JSON) and in structuredContent.data, message
// first, then each artifact's, in order; file parts as resource blocks (raw bytes embedded with their media type, a
// URL as a resource_link); structuredContent {taskId, state, data?} with the A2A state, data absent without data
// parts; failed, rejected and canceled are isError with sh.kontra/failure or sh.kontra/canceled in _meta and their
// status message as text.
func TestRender(t *testing.T) {
	file := a2a.NewRawPart([]byte("tck"))
	file.MediaType, file.Filename = "text/plain", "output.txt"
	link := a2a.NewFileURLPart("https://example.com/output.txt", "text/plain")
	link.Filename = "output.txt"
	msg := &a2a.Message{ID: "m", Role: a2a.MessageRoleAgent, Parts: a2a.ContentParts{a2a.NewTextPart("here"), a2a.NewDataPart(map[string]any{"from": "message"})}}
	task := taskWith(a2a.TaskStateCompleted, msg, nil,
		&a2a.Artifact{ID: "a1", Parts: a2a.ContentParts{a2a.NewTextPart("artifact text"), a2a.NewDataPart(map[string]any{"n": 1.0})}},
		&a2a.Artifact{ID: "a2", Parts: a2a.ContentParts{file, link, a2a.NewDataPart([]any{"x"})}})
	r := renderOf(t, task, store.StateCompleted)
	if r.IsError || r.Meta != nil {
		t.Errorf("a completed task: isError %v, _meta %v", r.IsError, r.Meta)
	}
	kinds := []string{}
	for _, c := range r.Content {
		kinds = append(kinds, c.Type)
	}
	if !reflect.DeepEqual(kinds, []string{"text", "text", "text", "text", "resource", "resource_link", "text"}) {
		t.Fatalf("content kinds %v", kinds)
	}
	if r.Content[0].Text != "here" || r.Content[1].Text != `{"from":"message"}` || r.Content[2].Text != "artifact text" ||
		r.Content[3].Text != `{"n":1}` || r.Content[6].Text != `["x"]` {
		t.Errorf("texts %+v", r.Content)
	}
	if res := r.Content[4].Resource; res == nil || res.Blob != "dGNr" || res.MIMEType != "text/plain" ||
		res.URI != "kontra:task/019a0000-0000-7000-8000-000000000001/output.txt" {
		t.Errorf("the file part: %+v", r.Content[4].Resource)
	}
	if l := r.Content[5]; l.URI != "https://example.com/output.txt" || l.Name != "output.txt" || l.MIMEType != "text/plain" {
		t.Errorf("the URL part: %+v", l)
	}
	var sc struct {
		TaskID string `json:"taskId"`
		State  string `json:"state"`
		Data   []any  `json:"data"`
	}
	scb, _ := json.Marshal(r.StructuredContent)
	if err := json.Unmarshal(scb, &sc); err != nil {
		t.Fatal(err)
	}
	if len(r.StructuredContent) != 3 || sc.TaskID != string(task.ID) || sc.State != "TASK_STATE_COMPLETED" ||
		!reflect.DeepEqual(sc.Data, []any{map[string]any{"from": "message"}, map[string]any{"n": 1.0}, []any{"x"}}) {
		t.Errorf("structuredContent %s", scb)
	}

	// No data parts: no data member.
	r = renderOf(t, taskWith(a2a.TaskStateCompleted, agentText("Hello World"), nil), store.StateCompleted)
	if _, ok := r.StructuredContent["data"]; ok || len(r.StructuredContent) != 2 || r.Content[0].Text != "Hello World" {
		t.Errorf("a text-only result: %+v", r)
	}

	for _, tc := range []struct {
		state store.State
		a2a   a2a.TaskState
		md    map[string]any
		msg   *a2a.Message
		key   string
		value string
		text  string
	}{
		{store.StateFailed, a2a.TaskStateFailed, map[string]any{store.MetaFailure: "timeout"}, agentText("the task timed out"), MetaFailure, "timeout", "the task timed out"},
		{store.StateFailed, a2a.TaskStateFailed, map[string]any{store.MetaFailure: "upstream_error"}, nil, MetaFailure, "upstream_error", "the service returned an error"},
		{store.StateCanceled, a2a.TaskStateCanceled, map[string]any{store.MetaCanceled: "declined"}, agentText("the quote was declined"), MetaCanceled, "declined", "the quote was declined"},
		{store.StateCanceled, a2a.TaskStateCanceled, map[string]any{store.MetaCanceled: "quote_expired"}, nil, MetaCanceled, "quote_expired", "the quote expired before it was approved: the task was canceled and nothing was charged"},
		// §12.2: a rejected hire task has sh.kontra/failure too (M4 WP-04 review): "rejected".
		{store.StateRejected, a2a.TaskStateRejected, nil, agentText("no"), MetaFailure, "rejected", "no"},
		{store.StateRejected, a2a.TaskStateRejected, nil, nil, MetaFailure, "rejected", "the task was rejected"},
	} {
		r := renderOf(t, taskWith(tc.a2a, tc.msg, tc.md), tc.state)
		if !r.IsError || r.Content[0].Text != tc.text {
			t.Errorf("%s %v: isError %v, content %+v", tc.state, tc.md, r.IsError, r.Content)
		}
		if tc.key != "" && r.Meta[tc.key] != tc.value {
			t.Errorf("%s: _meta %v, want %s = %s", tc.state, r.Meta, tc.key, tc.value)
		}
		var st string
		_ = json.Unmarshal(r.StructuredContent["state"], &st)
		if st != string(tc.a2a) {
			t.Errorf("%s: structured state %s", tc.state, st)
		}
	}
}

// Question (§12.4, D58, D274): a text-only question to a reader that declared form elicitation is input_required
// with the agent's text; every other case is the reply-tool text.
func TestQuestion(t *testing.T) {
	q := taskWith(a2a.TaskStateInputRequired, agentText("Which date?"), nil)
	ir, text, err := Question(q, Reader{FormElicitation: true})
	if err != nil || !ir || text != "Which date?" {
		t.Errorf("text-only, form declared: %v %q %v", ir, text, err)
	}
	hint := "Which date? (answer with the reply tool, taskId " + string(q.ID) + ")"
	if ir, text, _ := Question(q, Reader{}); ir || text != hint {
		t.Errorf("text-only, no form: %v %q", ir, text)
	}
	mixed := taskWith(a2a.TaskStateInputRequired, &a2a.Message{ID: "m", Role: a2a.MessageRoleAgent,
		Parts: a2a.ContentParts{a2a.NewTextPart("Which date?"), a2a.NewDataPart(map[string]any{"options": []any{1}})}}, nil)
	if ir, text, _ := Question(mixed, Reader{FormElicitation: true}); ir || text != "Which date? (answer with the reply tool, taskId "+string(q.ID)+")" {
		t.Errorf("non-text parts, form declared: %v %q", ir, text)
	}
	if ir, _, _ := Question(taskWith(a2a.TaskStateInputRequired, nil, nil), Reader{FormElicitation: true}); ir {
		t.Error("a question without a message was input_required")
	}
}

// view (§5.1, D21, §12.4): quoted is working with the approve URL; input-required follows Question, with
// sh.kontra/input-required when it is shown working.
func TestView(t *testing.T) {
	row := &store.TaskRow{ID: uuid.Must(uuid.NewV7()), State: store.StateQuoted,
		Snapshot: taskWith(a2a.TaskStateInputRequired, nil, map[string]any{lifecycle.MetaApproveURL: "https://app.kontra.sh/#/t/x"})}
	v := view(row, Reader{FormElicitation: true})
	if v.status != "working" || v.statusMessage != "https://app.kontra.sh/#/t/x" || v.meta[MetaApproveURL] != "https://app.kontra.sh/#/t/x" {
		t.Errorf("quoted: %+v", v)
	}
	row.State, row.Snapshot = store.StateInputRequired, taskWith(a2a.TaskStateInputRequired, agentText("Which date?"), nil)
	if v := view(row, Reader{FormElicitation: true}); v.status != "input_required" || v.statusMessage != "Which date?" || v.meta != nil {
		t.Errorf("input-required, form: %+v", v)
	}
	if v := view(row, Reader{}); v.status != "working" || v.meta[MetaInputRequired] != true || !strings.Contains(v.statusMessage, "reply tool") {
		t.Errorf("input-required, no form: %+v", v)
	}
	// Ended (§12.2, §11.8): failed and rejected are MCP failed with sh.kontra/failure, canceled is cancelled with
	// sh.kontra/canceled; the statusMessage is the status message's text, else the reason's.
	for _, tc := range []struct {
		state          store.State
		a2a            a2a.TaskState
		msg            *a2a.Message
		md             map[string]any
		status, key    string
		value, status2 string
	}{
		{store.StateFailed, a2a.TaskStateFailed, nil, map[string]any{store.MetaFailure: "timeout"}, "failed", MetaFailure, "timeout", store.ReasonText(store.ReasonTimeout)},
		{store.StateFailed, a2a.TaskStateFailed, agentText("boom"), nil, "failed", MetaFailure, "upstream_error", "boom"},
		{store.StateRejected, a2a.TaskStateRejected, agentText("not for me"), nil, "failed", MetaFailure, "rejected", "not for me"},
		{store.StateRejected, a2a.TaskStateRejected, nil, nil, "failed", MetaFailure, "rejected", "the task was rejected"},
		{store.StateCanceled, a2a.TaskStateCanceled, nil, map[string]any{store.MetaCanceled: "declined"}, "cancelled", MetaCanceled, "declined", ""},
	} {
		row.State, row.Snapshot = tc.state, taskWith(tc.a2a, tc.msg, tc.md)
		v := view(row, Reader{})
		if v.status != tc.status || v.meta[tc.key] != tc.value || len(v.meta) != 1 || v.statusMessage != tc.status2 {
			t.Errorf("%s %v: %+v, want %s with %s = %s and %q", tc.state, tc.md, v, tc.status, tc.key, tc.value, tc.status2)
		}
	}
}

// ParseMessage (mcp-tools.json#/$defs/message): a string, or {parts} with exactly one content member each.
func TestParseMessage(t *testing.T) {
	parts, err := ParseMessage(json.RawMessage(`"hi"`))
	if err != nil || len(parts) != 1 || parts[0].Text() != "hi" {
		t.Fatalf("a string: %v %v", parts, err)
	}
	parts, err = ParseMessage(json.RawMessage(`{"parts":[{"text":"a"},{"raw":"dGNr","mediaType":"text/plain","filename":"f.txt"},
		{"url":"https://x.example/f"},{"data":{"k":[1]},"metadata":{"m":true}}]}`))
	if err != nil || len(parts) != 4 || string(parts[1].Raw()) != "tck" || parts[1].Filename != "f.txt" ||
		parts[2].URL() != "https://x.example/f" || parts[3].Metadata["m"] != true {
		t.Fatalf("parts: %+v %v", parts, err)
	}
	for _, bad := range []string{`""`, `1`, `[]`, `{}`, `{"parts":[]}`, `{"parts":[{}]}`, `{"parts":[{"text":"a","data":1}]}`,
		`{"parts":[{"raw":"not base64!"}]}`, `{"parts":[{"text":"a","x":1}]}`, `{"parts":[{"text":"a"}],"extra":1}`,
		`{"parts":[{"text":"a","TEXT":"b"}]}`, `{"parts":[{"text":1}]}`, `{"parts":[{"text":"a","metadata":[]}]}`,
		// A null data value: a2a-go writes {"data":null} and cannot read it back (M4 WP-04 review).
		`{"parts":[{"data":null}]}`, `{"parts":[{"text":"a"},{"data": null ,"mediaType":"application/json"}]}`} {
		var ae *argError
		if _, err := ParseMessage(json.RawMessage(bad)); !errors.As(err, &ae) {
			t.Errorf("ParseMessage(%s) = %v, want an invalid-arguments error", bad, err)
		}
	}
	// Every part ParseMessage builds is one a2a-go reads back from what it writes: the message a task stores is decoded
	// by every later read of the task.
	for _, good := range []string{`"hi"`, `{"parts":[{"text":""}]}`, `{"parts":[{"raw":""}]}`, `{"parts":[{"data":0}]}`,
		`{"parts":[{"data":false}]}`, `{"parts":[{"data":""}]}`, `{"parts":[{"data":[null]}]}`, `{"parts":[{"data":{"k":null}}]}`,
		`{"parts":[{"text":"a","metadata":{}}]}`, `{"parts":[{"url":"https://x.example/f","filename":"f"}]}`} {
		parts, err := ParseMessage(json.RawMessage(good))
		if err != nil {
			t.Errorf("ParseMessage(%s): %v", good, err)
			continue
		}
		b, err := json.Marshal(&a2a.Message{ID: "m", Role: a2a.MessageRoleUser, Parts: parts})
		if err != nil {
			t.Fatal(err)
		}
		var back a2a.Message
		if err := json.Unmarshal(b, &back); err != nil {
			t.Errorf("ParseMessage(%s) built a message a2a-go cannot read back (%s): %v", good, b, err)
		}
	}
}

// The tools' arguments are read against their schemas: unknown members, wrong types and refs that do not match the
// pattern are invalid-arguments errors (a tool execution error, never a protocol error).
func TestArguments(t *testing.T) {
	if a, err := parseSearchArgs(json.RawMessage(`{"query":"Hello","kind":"a2a","price":"free","cursor":"x"}`)); err != nil ||
		a.Query != "Hello" || a.Kind != "a2a" || a.Price != "free" {
		t.Errorf("search: %+v %v", a, err)
	}
	if _, err := parseSearchArgs(nil); err != nil {
		t.Errorf("search without arguments: %v", err)
	}
	for _, bad := range []string{`{"kind":"x"}`, `{"query":1}`, `{"other":1}`, `{"query":"` + strings.Repeat("q", 257) + `"}`} {
		var ae *argError
		if _, err := parseSearchArgs(json.RawMessage(bad)); !errors.As(err, &ae) {
			t.Errorf("search %s: %v", bad, err)
		}
	}
	if _, _, err := parseHireArgs(json.RawMessage(`{"service":"Hello","message":"hi"}`)); err == nil {
		t.Error("hire of a ref that is not one")
	}
	a, parts, err := parseHireArgs(json.RawMessage(`{"service":"acme/hello","message":"hi","contextId":"c1"}`))
	if err != nil || a.Service != "acme/hello" || a.ContextID != "c1" || len(parts) != 1 {
		t.Errorf("hire: %+v %v", a, err)
	}
	if _, _, err := parseReplyArgs(json.RawMessage(`{"taskId":"nope","message":"x"}`)); err == nil {
		t.Error("reply with a task id that is not one")
	}
	id := uuid.Must(uuid.NewV7())
	if r, _, err := parseReplyArgs(json.RawMessage(`{"taskId":"` + id.String() + `","message":"x"}`)); err != nil || r.TaskID != id {
		t.Errorf("reply: %+v %v", r, err)
	}
}

// The client capabilities that decide -32042 and §12.4's form (MCP 2025-11-25: an empty elicitation object is form).
func TestElicitationCapabilities(t *testing.T) {
	for _, tc := range []struct {
		caps      string
		form, url bool
	}{
		{`{}`, false, false},
		{`{"elicitation":{}}`, true, false},
		{`{"elicitation":{"form":{}}}`, true, false},
		{`{"elicitation":{"url":{}}}`, false, true},
		{`{"elicitation":{"form":{},"url":{}}}`, true, true},
		{`{"elicitation":true}`, false, false},
		{`{"elicitation":{"url":{},"URL":{}}}`, false, false},
	} {
		if f, u := declaresForm(json.RawMessage(tc.caps)), declaresURL(json.RawMessage(tc.caps)); f != tc.form || u != tc.url {
			t.Errorf("%s: form %v url %v, want %v %v", tc.caps, f, u, tc.form, tc.url)
		}
	}
}

// An elicitation answer is a follow-up only as accept with content {reply: string}; decline, cancel and any other
// shape are not.
func TestAcceptedReply(t *testing.T) {
	if r, ok := acceptedReply(json.RawMessage(`{"action":"accept","content":{"reply":"tomorrow"}}`)); !ok || r != "tomorrow" {
		t.Errorf("accept: %q %v", r, ok)
	}
	for _, bad := range []string{`{"action":"decline"}`, `{"action":"cancel"}`, `{"action":"accept"}`, `{"action":"accept","content":{}}`,
		`{"action":"accept","content":{"reply":1}}`, `{"action":"accept","action":"accept","content":{"reply":"x"}}`,
		`{"action":"accept","content":{"reply":"x","Reply":"y"}}`, `{"action":"accept","content":{"reply":""}}`} {
		if _, ok := acceptedReply(json.RawMessage(bad)); ok {
			t.Errorf("%s was taken for an accept", bad)
		}
	}
}

// AdmitsNamed is search's visibility (§12.2): an allow: list that admits the principal only through anonymous does
// not show it the service; oidc:* shows it to every OIDC principal, as key:* to every API key, and to nobody else
// (D301).
func TestAdmitsNamed(t *testing.T) {
	tid := uuid.Must(uuid.NewV7())
	p := auth.Principal{Kind: auth.KindKey, ID: "key:kk_acme_ab12cd34", TenantID: &tid, TenantSlug: "acme",
		BuyerID: uuid.Must(uuid.NewV7()), Via: auth.ViaAPIKey, Scopes: []auth.Scope{auth.ScopeHire}}
	o := auth.Principal{Kind: auth.KindOIDC, ID: "oidc:sub:user-7", BuyerID: uuid.Must(uuid.NewV7()), Via: auth.ViaMCP}
	for _, tc := range []struct {
		allow       []string
		p           auth.Principal
		named, hire bool
	}{
		{[]string{"anonymous"}, p, false, true},
		{[]string{"anonymous", "org:acme"}, p, true, true},
		{[]string{"key:kk_acme_*"}, p, true, true},
		{[]string{"org:other"}, p, false, false},
		{[]string{"oidc:*"}, p, false, false},
		{[]string{"oidc:*", "key:*"}, p, true, true},
		{[]string{"oidc:*"}, o, true, true},
		{[]string{"anonymous", "oidc:*"}, o, true, true},
		{[]string{"anonymous", "key:*"}, o, false, true},
		{[]string{"oidc:sub:user-8"}, o, false, false},
	} {
		al, err := auth.ParseAllowList(tc.allow)
		if err != nil {
			t.Fatal(err)
		}
		if AdmitsNamed(al, tc.p) != tc.named || al.Admits(tc.p) != tc.hire {
			t.Errorf("%v, %s: named %v admits %v", tc.allow, tc.p.ID, AdmitsNamed(al, tc.p), al.Admits(tc.p))
		}
	}
}
