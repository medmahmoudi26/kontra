package mcpwire

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// modernMsg parses a client body for the checks.
func modernMsg(t *testing.T, body string) *Message {
	t.Helper()
	m, err := Parse([]byte(body), FromClient)
	if err != nil {
		t.Fatalf("Parse(%s): %v", body, err)
	}
	return m
}

func modernHeader(method, name string) http.Header {
	h := http.Header{}
	h.Set(HeaderProtocolVersion, V20260728)
	if method != "" {
		h.Set(HeaderMethod, method)
	}
	if name != "" {
		h.Set(HeaderName, name)
	}
	return h
}

const okMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

// §11.12's checks on a modern request (D176): -32602 for a missing per-request field, -32020 for a version other
// than the header's and for Mcp-Method and Mcp-Name that are missing, malformed or different from the body (after
// Base64 decoding), -32600 for a response, 202 for a notification. Every refusal carries the request's id.
func TestCheckModern(t *testing.T) {
	call := func(name string, meta string) string {
		return `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":` + name + `,"arguments":{},` + meta + `}}`
	}
	for _, tc := range []struct {
		name   string
		body   string
		hdr    http.Header
		status int // 0: accepted
		code   int
	}{
		{"accepted", call(`"echo"`, okMeta), modernHeader("tools/call", "echo"), 0, 0},
		{"Mcp-Name in a Base64 sentinel", call(`"héllo"`, okMeta), modernHeader("tools/call", "=?base64?aMOpbGxv?="), 0, 0},
		{"Mcp-Name 'add' with body name 'echo'", call(`"echo"`, okMeta), modernHeader("tools/call", "add"), 400, CodeHeaderMismatch},
		{"no Mcp-Name", call(`"echo"`, okMeta), modernHeader("tools/call", ""), 400, CodeHeaderMismatch},
		{"a malformed sentinel", call(`"echo"`, okMeta), modernHeader("tools/call", "=?base64?!!!?="), 400, CodeHeaderMismatch},
		{"a sentinel of another name", call(`"echo"`, okMeta), modernHeader("tools/call", "=?base64?YWRk?="), 400, CodeHeaderMismatch},
		{"no Mcp-Method", call(`"echo"`, okMeta), modernHeader("", "echo"), 400, CodeHeaderMismatch},
		{"another Mcp-Method", call(`"echo"`, okMeta), modernHeader("tools/list", "echo"), 400, CodeHeaderMismatch},
		{"protocolVersion other than the header", call(`"echo"`,
			`"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25","io.modelcontextprotocol/clientCapabilities":{}}`),
			modernHeader("tools/call", "echo"), 400, CodeHeaderMismatch},
		{"no protocolVersion", call(`"echo"`, `"_meta":{"io.modelcontextprotocol/clientCapabilities":{}}`),
			modernHeader("tools/call", "echo"), 400, CodeInvalidParams},
		{"no clientCapabilities", call(`"echo"`, `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`),
			modernHeader("tools/call", "echo"), 400, CodeInvalidParams},
		{"clientCapabilities not an object", call(`"echo"`,
			`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":true}`),
			modernHeader("tools/call", "echo"), 400, CodeInvalidParams},
		{"no _meta", `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`, modernHeader("tools/list", ""), 400, CodeInvalidParams},
		{"no name in the body", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{` + okMeta + `}}`,
			modernHeader("tools/call", "echo"), 400, CodeInvalidParams},
		{"resources/read checks uri", `{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"file:///a",` + okMeta + `}}`,
			modernHeader("resources/read", "file:///b"), 400, CodeHeaderMismatch},
		{"resources/read accepted", `{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"file:///a",` + okMeta + `}}`,
			modernHeader("resources/read", "file:///a"), 0, 0},
		{"tasks/get checks taskId", `{"jsonrpc":"2.0","id":7,"method":"tasks/get","params":{"taskId":"t1",` + okMeta + `}}`,
			modernHeader("tasks/get", "t2"), 400, CodeHeaderMismatch},
		{"tools/list needs no Mcp-Name", `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{` + okMeta + `}}`,
			modernHeader("tools/list", ""), 0, 0},
		{"a response", `{"jsonrpc":"2.0","id":7,"result":{}}`, modernHeader("", ""), 400, CodeInvalidRequest},
		{"a notification", `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`, modernHeader("", ""), 202, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := CheckModern(tc.hdr, modernMsg(t, tc.body))
			if tc.status == 0 {
				if ref != nil {
					t.Fatalf("refused: %d %s", ref.Status, ref.Body)
				}
				return
			}
			if ref == nil || ref.Status != tc.status {
				t.Fatalf("refusal %+v, want %d", ref, tc.status)
			}
			if tc.code == 0 {
				if len(ref.Body) != 0 {
					t.Fatalf("a body: %s", ref.Body)
				}
				return
			}
			var e struct {
				ID    json.RawMessage `json:"id"`
				Error Error           `json:"error"`
			}
			if err := json.Unmarshal(ref.Body, &e); err != nil || e.Error.Code != tc.code || string(e.ID) != "7" {
				t.Fatalf("body %s, want code %d and id 7", ref.Body, tc.code)
			}
			if strings.Contains(e.Error.Message, "add") || strings.Contains(e.Error.Message, "echo") {
				t.Errorf("the refusal repeats a value: %s", ref.Body)
			}
		})
	}
	// Two Mcp-Name headers are a refusal, even when one matches.
	h := modernHeader("tools/call", "echo")
	h.Add(HeaderName, "add")
	if ref := CheckModern(h, modernMsg(t, call(`"echo"`, okMeta))); ref == nil || ref.Status != 400 {
		t.Errorf("two Mcp-Name headers: %+v", ref)
	}
}

// The Base64 sentinel (2026-07-28 Streamable HTTP, "Value Encoding"): the spec's examples, and what is malformed.
func TestHeaderValues(t *testing.T) {
	for _, c := range []struct{ value, encoded string }{
		{"us-west1", "us-west1"},
		{"Hello, 世界", "=?base64?SGVsbG8sIOS4lueVjA==?="},
		{" padded ", "=?base64?IHBhZGRlZCA=?="},
		{"line1\nline2", "=?base64?bGluZTEKbGluZTI=?="},
		{"=?base64?literal?=", "=?base64?PT9iYXNlNjQ/bGl0ZXJhbD89?="},
	} {
		if got := EncodeHeaderValue(c.value); got != c.encoded {
			t.Errorf("EncodeHeaderValue(%q) = %q, want %q", c.value, got, c.encoded)
		}
		if got, err := DecodeHeaderValue(c.encoded); err != nil || got != c.value {
			t.Errorf("DecodeHeaderValue(%q) = %q, %v", c.encoded, got, err)
		}
	}
	for _, bad := range []string{"=?base64?***?=", "=?base64?/w==?=", "caf\xc3\xa9", "a\x01b"} {
		if _, err := DecodeHeaderValue(bad); !errors.Is(err, ErrHeaderValue) {
			t.Errorf("DecodeHeaderValue(%q): %v, want ErrHeaderValue", bad, err)
		}
	}
}

// The tasks extension in a request's per-request capabilities: read, and removed from what the gate forwards
// (§11.12) with every other byte kept; a case variant of its key is refused by Parse (D172).
func TestTasksExtension(t *testing.T) {
	with := `{"name":"x","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"elicitation":{},"extensions":{"io.modelcontextprotocol/tasks":{},"x.y/z":{"a":1}}}}}`
	if !DeclaresTasks(json.RawMessage(with)) {
		t.Fatal("not declared")
	}
	out, err := WithoutTasks(json.RawMessage(with))
	if err != nil {
		t.Fatal(err)
	}
	if DeclaresTasks(out) || !strings.Contains(string(out), `"x.y/z":{"a":1}`) || !strings.Contains(string(out), `"elicitation":{}`) {
		t.Fatalf("WithoutTasks: %s", out)
	}
	without := `{"name":"x","_meta":{"io.modelcontextprotocol/clientCapabilities":{}}}`
	if DeclaresTasks(json.RawMessage(without)) {
		t.Fatal("declared")
	}
	if out, _ := WithoutTasks(json.RawMessage(without)); string(out) != without {
		t.Fatalf("changed: %s", out)
	}
	variant := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","_meta":{"io.modelcontextprotocol/clientCapabilities":{"extensions":{"IO.modelcontextprotocol/TASKS":{}}}}}}`
	if _, err := Parse([]byte(variant), FromClient); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("a case variant of the extension's key: %v, want ErrAmbiguous", err)
	}
	ref := MissingTasks(json.RawMessage(`3`))
	if ref.Status != 400 || string(ref.Body) != `{"jsonrpc":"2.0","id":3,"error":{"code":-32021,"message":"Missing required client capability","data":{"requiredCapabilities":{"extensions":{"io.modelcontextprotocol/tasks":{}}}}}}` {
		t.Errorf("-32021: %d %s", ref.Status, ref.Body)
	}
}

// subscriptions/listen's taskIds (D176): asked for under any case, and removed under any case.
func TestListenTaskIDs(t *testing.T) {
	for _, p := range []string{`{"notifications":{"taskIds":["a"],"toolsListChanged":true}}`, `{"notifications":{"TaskIDs":[]}}`} {
		if !ListensForTasks(json.RawMessage(p)) {
			t.Errorf("%s: not asking", p)
		}
		out, err := WithoutTaskIDs(json.RawMessage(p))
		if err != nil || ListensForTasks(out) {
			t.Errorf("%s: %s %v", p, out, err)
		}
	}
	p := `{"notifications":{"toolsListChanged":true}}`
	if ListensForTasks(json.RawMessage(p)) {
		t.Error("asking")
	}
	if out, _ := WithoutTaskIDs(json.RawMessage(p)); string(out) != p {
		t.Errorf("changed: %s", out)
	}
	if out, _ := WithoutTaskIDs(json.RawMessage(`{"notifications":{"taskIds":["a"],"toolsListChanged":true}}`)); string(out) != p {
		t.Errorf("WithoutTaskIDs: %s", out)
	}
}

// What the gate's own server/discover's answer says of the era (§11.12, D177).
func TestClassifyDiscover(t *testing.T) {
	parse := func(s string) *Message {
		m, err := Parse([]byte(s), FromUpstream)
		if err != nil {
			t.Fatalf("Parse(%s): %v", s, err)
		}
		return m
	}
	discover := parse(`{"jsonrpc":"2.0","id":"d","result":{"supportedVersions":["2026-07-28"],"capabilities":{}}}`)
	for _, tc := range []struct {
		name   string
		status int
		msg    *Message
		want   DiscoverAnswer
	}{
		{"a server/discover result", 200, discover, AnswerModern},
		{"rule 3's body", 400, parse(string(LegacyNoSessionBody)), AnswerLegacy},
		{"404", 404, nil, AnswerLegacy},
		{"405", 405, nil, AnswerLegacy},
		{"200 with another error", 200, parse(`{"jsonrpc":"2.0","id":"d","error":{"code":-32601,"message":"Method not found"}}`), AnswerLegacy},
		{"200 with no message", 200, nil, AnswerLegacy},
		{"400 with no message", 400, nil, AnswerLegacy},
		{"200 with a result that is not discover's", 200, parse(`{"jsonrpc":"2.0","id":"d","result":{"tools":[]}}`), AnswerLegacy},
		{"-32022", 400, parse(`{"jsonrpc":"2.0","id":"d","error":{"code":-32022,"message":"Unsupported","data":{"supported":["2027-01-01"]}}}`), AnswerUnknown},
		{"-32020", 400, parse(`{"jsonrpc":"2.0","id":"d","error":{"code":-32020,"message":"m"}}`), AnswerUnknown},
		{"-32021", 200, parse(`{"jsonrpc":"2.0","id":"d","error":{"code":-32021,"message":"m"}}`), AnswerUnknown},
		{"502", 502, nil, AnswerUnknown},
		{"401", 401, nil, AnswerUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, res := ClassifyDiscover(tc.status, tc.msg)
			if got != tc.want {
				t.Fatalf("%v, want %v", got, tc.want)
			}
			if (got == AnswerModern) != (res != nil) {
				t.Fatalf("result %s", res)
			}
		})
	}
}

// The members the modern dispatch reads in its own answers refuse a lone case variant (D172), as CheckTask does.
func TestCheckModernTask(t *testing.T) {
	for _, bad := range []string{`{"Status":"working"}`, `{"resultType":"task","TaskId":"x"}`, `{"INPUTREQUESTS":{}}`, `{"RequestState":"s"}`, `{"PollIntervalMs":5}`} {
		if err := CheckModernTask(json.RawMessage(bad)); !errors.Is(err, ErrAmbiguous) {
			t.Errorf("%s: %v, want ErrAmbiguous", bad, err)
		}
	}
	for _, ok := range []string{`{"resultType":"task","taskId":"x","status":"working","pollIntervalMs":5}`, `[]`, `{"content":[],"isError":false}`} {
		if err := CheckModernTask(json.RawMessage(ok)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	// What the relay reads of every modern answer is checked by Parse itself.
	for _, bad := range []string{
		`{"jsonrpc":"2.0","id":1,"result":{"ResultType":"input_required"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"SupportedVersions":["2026-07-28"]}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"TTLMs":1}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"extensions":{"IO.modelcontextprotocol/tasks":{}}}}}`,
	} {
		if _, err := Parse([]byte(bad), FromUpstream); !errors.Is(err, ErrAmbiguous) {
			t.Errorf("%s: %v, want ErrAmbiguous", bad, err)
		}
	}
}

// NameSource names the member each method's Mcp-Name carries.
func TestNameSource(t *testing.T) {
	for method, want := range map[string]string{"tools/call": "name", "prompts/get": "name", "resources/read": "uri",
		"tasks/get": "taskId", "tasks/update": "taskId", "tasks/cancel": "taskId", "tools/list": ""} {
		if got, _ := NameSource(method); got != want {
			t.Errorf("%s: %q, want %q", method, got, want)
		}
	}
	if keys := Keys(json.RawMessage(`{"b":1,"a":2,"c":3}`)); strings.Join(keys, ",") != "b,a,c" {
		t.Errorf("Keys: %v", keys)
	}
}
