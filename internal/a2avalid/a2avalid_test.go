package a2avalid

import (
	"strings"
	"testing"
)

func has(r Result, path, rule string) bool {
	for _, i := range r.Issues {
		if i.Path == path && i.Rule == rule {
			return true
		}
	}
	return false
}

// The spec's scenarios: a valid 1.0 message, a missing messageId, an unknown card member, a 0.3 task, a truncated body.
func TestScenarios(t *testing.T) {
	ok := `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`
	if r := Request("1.0", "jsonrpc", "SendMessage", []byte(ok), false); !r.Validated || !r.Valid || r.Version != V10 {
		t.Errorf("valid SendMessage: %+v", r)
	}
	noID := `{"jsonrpc":"2.0","id":1,"result":{"message":{"role":"ROLE_AGENT","parts":[{"text":"hi"}]}}}`
	r := Response("1.0", "jsonrpc", "SendMessage", []byte(noID), false)
	if r.Valid || !has(r, "$.result.message.messageId", RuleRequired) {
		t.Errorf("missing messageId: %+v", r)
	}
	for _, i := range r.Issues {
		if i.Path == "$.result.message.messageId" && i.Member != "Message.messageId" {
			t.Errorf("member %q", i.Member)
		}
	}
	card := `{"name":"n","description":"d","version":"1","supportedInterfaces":[{"url":"https://x","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
		"capabilities":{},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[],"skillz":[]}`
	if r := Card([]byte(card), "1.0"); r.Valid || !has(r, "$.skillz", RuleUnknown) {
		t.Errorf("unknown member: %+v", r)
	}
	task03 := `{"jsonrpc":"2.0","id":1,"result":{"kind":"task","id":"t1","contextId":"c1","status":{"state":"working"}}}`
	if r := Response("0.3", "jsonrpc", "GetTask", []byte(task03), false); !r.Validated || !r.Valid || r.Version != V03 {
		t.Errorf("0.3 task: %+v", r)
	}
	if r := Response("1.0", "jsonrpc", "GetTask", []byte(`{"jsonrpc":`), true); r.Validated || r.Note != NoteTruncated {
		t.Errorf("truncated: %+v", r)
	}
	if r := Response("1.0", "rest", "GetTask", []byte(`<html>`), false); r.Valid || !has(r, "$", RuleNotJSON) {
		t.Errorf("not JSON: %+v", r)
	}
}

// The proto's rules as protojson applies them: types, enums by name, oneofs, duplicates (a field by both its names),
// well-known types, null as the default.
func TestRules(t *testing.T) {
	for _, tc := range []struct {
		name, body, path, rule string
	}{
		{"enum name", `{"id":"t","contextId":"c","status":{"state":"WORKING"}}`, "$.status.state", RuleEnum},
		{"type", `{"id":1,"contextId":"c","status":{"state":"TASK_STATE_WORKING"}}`, "$.id", RuleType},
		{"duplicate", `{"id":"t","context_id":"c","contextId":"c","status":{"state":"TASK_STATE_WORKING"}}`, "$.contextId", RuleDuplicate},
		{"timestamp", `{"id":"t","contextId":"c","status":{"state":"TASK_STATE_WORKING","timestamp":"yesterday"}}`, "$.status.timestamp", RuleType},
		{"metadata struct", `{"id":"t","contextId":"c","status":{"state":"TASK_STATE_WORKING"},"metadata":[1]}`, "$.metadata", RuleType},
		{"array", `{"id":"t","contextId":"c","status":{"state":"TASK_STATE_WORKING"},"artifacts":{}}`, "$.artifacts", RuleType},
	} {
		r := Response("1.0", "rest", "GetTask", []byte(tc.body), false)
		if r.Valid || !has(r, tc.path, tc.rule) {
			t.Errorf("%s: %+v", tc.name, r)
		}
	}
	// A part with two contents breaks its oneof; data is any JSON; a null member is its default.
	two := `{"message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"a","data":{"x":1}}]}}`
	if r := Request("1.0", "rest", "SendMessage", []byte(two), false); r.Valid || !has(r, "$.message.parts[0].data", RuleOneof) {
		t.Errorf("oneof: %+v", r)
	}
	data := `{"message":{"messageId":"m","role":"ROLE_USER","parts":[{"data":[1,"x",null]}],"metadata":null}}`
	if r := Request("1.0", "rest", "SendMessage", []byte(data), false); !r.Valid {
		t.Errorf("data part: %+v", r)
	}
}

// The bindings: an HTTP+JSON request without the fields its path carries; an HTTP+JSON error; a JSON-RPC error; a
// stream frame on either binding; a logged event.
func TestBindings(t *testing.T) {
	if r := Request("1.0", "rest", "CancelTask", []byte(`{}`), false); !r.Valid {
		t.Errorf("CancelTask's id is in the path: %+v", r)
	}
	if r := Request("1.0", "jsonrpc", "CancelTask", []byte(`{"jsonrpc":"2.0","id":1,"method":"CancelTask","params":{}}`), false); r.Valid || !has(r, "$.params.id", RuleRequired) {
		t.Errorf("JSON-RPC CancelTask without id: %+v", r)
	}
	if r := Response("1.0", "rest", "GetTask", []byte(`{"error":{"code":404,"status":"NOT_FOUND","message":"no"}}`), false); !r.Valid {
		t.Errorf("HTTP+JSON error: %+v", r)
	}
	if r := Response("1.0", "jsonrpc", "GetTask", []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"no"}}`), false); !r.Valid {
		t.Errorf("JSON-RPC error: %+v", r)
	}
	if r := Response("1.0", "jsonrpc", "GetTask", []byte(`{"jsonrpc":"1.0","id":1}`), false); r.Valid || !has(r, "$.jsonrpc", RuleEnvelope) {
		t.Errorf("bad envelope: %+v", r)
	}
	frame := `{"statusUpdate":{"taskId":"t","contextId":"c","status":{"state":"TASK_STATE_WORKING"}}}`
	if r := Frame("1.0", "rest", "SendStreamingMessage", []byte(frame), false); !r.Valid {
		t.Errorf("REST frame: %+v", r)
	}
	if r := Frame("1.0", "jsonrpc", "SubscribeToTask", []byte(`{"jsonrpc":"2.0","id":1,"result":`+frame+`}`), false); !r.Valid {
		t.Errorf("JSON-RPC frame: %+v", r)
	}
	if r := Event([]byte(`{"statusUpdate":{"taskId":"t","status":{"state":"TASK_STATE_WORKING"}}}`)); r.Valid || !has(r, "$.statusUpdate.contextId", RuleRequired) {
		t.Errorf("event without contextId: %+v", r)
	}
	if r := Request("1.0", "jsonrpc", "NotAMethod", []byte(`{}`), false); r.Validated || r.Note != NoteMethod {
		t.Errorf("unknown method: %+v", r)
	}
	if r := Request("2.0", "jsonrpc", "SendMessage", []byte(`{}`), false); r.Validated || r.Note != NoteVersion {
		t.Errorf("unknown version: %+v", r)
	}
}

// 0.3: a JSON-RPC request that breaks the schema, an HTTP+JSON result, a card.
func TestV03(t *testing.T) {
	bad := `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"x"}]}}}`
	r := Request("0.3", "jsonrpc", "SendMessage", []byte(bad), false)
	if r.Valid || len(r.Issues) == 0 || r.Issues[0].Rule != RuleSchema || !strings.Contains(r.Issues[0].Detail+r.Issues[0].Member, "SendMessageRequest") {
		t.Errorf("0.3 request without messageId/kind: %+v", r)
	}
	good := `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"kind":"message","messageId":"m","role":"user","parts":[{"kind":"text","text":"x"}]}}}`
	if r := Request("0.3", "jsonrpc", "SendMessage", []byte(good), false); !r.Valid {
		t.Errorf("0.3 request: %+v", r)
	}
	if r := Response("0.3", "rest", "GetTask", []byte(`{"kind":"task","id":"t","contextId":"c","status":{"state":"completed"}}`), false); !r.Valid {
		t.Errorf("0.3 HTTP+JSON task: %+v", r)
	}
	card := `{"name":"n","description":"d","url":"https://x","version":"1","protocolVersion":"0.3.0","capabilities":{},
		"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[]}`
	if r := Card([]byte(card), "0.3"); !r.Valid {
		t.Errorf("0.3 card: %+v", r)
	}
}

// BenchmarkFrame256KiB: one 256 KiB artifact frame (traffic.body_max's default), the largest frame the record keeps
// whole; 4.3's bar is 10 ms.
func BenchmarkFrame256KiB(b *testing.B) {
	text := strings.Repeat("y", 256<<10-200)
	frame := []byte(`{"jsonrpc":"2.0","id":1,"result":{"artifactUpdate":{"taskId":"t","contextId":"c","artifact":{"artifactId":"a","parts":[{"text":"` + text + `"}]}}}}`)
	b.SetBytes(int64(len(frame)))
	for b.Loop() {
		if r := Frame("1.0", "jsonrpc", "SendStreamingMessage", frame, false); !r.Valid {
			b.Fatalf("%+v", r)
		}
	}
}

// BenchmarkFrameManyParts: the same size in 2000 small data parts, the walk's worst shape.
func BenchmarkFrameManyParts(b *testing.B) {
	var parts []string
	for i := range 2000 {
		parts = append(parts, `{"data":{"i":`+strings.Repeat("1", 1)+`,"s":"`+strings.Repeat("z", 100)+`"},"metadata":{"n":`+string(rune('0'+i%10))+`}}`)
	}
	frame := []byte(`{"artifactUpdate":{"taskId":"t","contextId":"c","artifact":{"artifactId":"a","parts":[` + strings.Join(parts, ",") + `]}}}`)
	b.SetBytes(int64(len(frame)))
	for b.Loop() {
		if r := Frame("1.0", "rest", "SendStreamingMessage", frame, false); !r.Valid {
			b.Fatalf("%+v", r)
		}
	}
}

// The parser reads what encoding/json reads, and refuses what it refuses.
func TestParse(t *testing.T) {
	for _, ok := range []string{`{}`, `[]`, `{"a":[1,-2.5e+3,true,false,null,"é\n"]}`, ` {"a" : { "b" : [ ] } } `, `"x"`, `0`} {
		if _, err := parse([]byte(ok)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{``, `{`, `{"a"}`, `[1,]`, `01`, `1.`, `"\x01"`, `{"a":1}x`, `tru`, "\"\xff\""} {
		if _, err := parse([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	n, _ := parse([]byte(`{"k":"a\"b","k":2}`))
	if len(n.members) != 2 || n.members[0].val.str != `a"b` || n.members[1].val.str != "2" {
		t.Errorf("members %+v", n.members)
	}
}
