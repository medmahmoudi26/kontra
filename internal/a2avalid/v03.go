package a2avalid

import (
	_ "embed"
	"encoding/json"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// A2A 0.3.0's JSON Schema (package doc): its definitions are the JSON-RPC requests and answers of 0.3, the objects
// inside them, and the agent card.

//go:embed schema/a2a-v0.3.0.json
var schema03 []byte

// v03 are, for each A2A method (by its 1.0 name, as the traffic record names a 0.3 call), the definitions of its
// JSON-RPC request, its JSON-RPC answer, and the result of an HTTP+JSON answer or the data of a stream frame.
var v03 = map[string]struct{ request, response, result string }{
	"SendMessage":                      {"SendMessageRequest", "SendMessageResponse", "SendMessageSuccessResponse"},
	"SendStreamingMessage":             {"SendStreamingMessageRequest", "SendStreamingMessageResponse", "SendStreamingMessageSuccessResponse"},
	"GetTask":                          {"GetTaskRequest", "GetTaskResponse", "GetTaskSuccessResponse"},
	"CancelTask":                       {"CancelTaskRequest", "CancelTaskResponse", "CancelTaskSuccessResponse"},
	"SubscribeToTask":                  {"TaskResubscriptionRequest", "SendStreamingMessageResponse", "SendStreamingMessageSuccessResponse"},
	"CreateTaskPushNotificationConfig": {"SetTaskPushNotificationConfigRequest", "SetTaskPushNotificationConfigResponse", "SetTaskPushNotificationConfigSuccessResponse"},
	"GetTaskPushNotificationConfig":    {"GetTaskPushNotificationConfigRequest", "GetTaskPushNotificationConfigResponse", "GetTaskPushNotificationConfigSuccessResponse"},
	"ListTaskPushNotificationConfigs":  {"ListTaskPushNotificationConfigRequest", "ListTaskPushNotificationConfigResponse", "ListTaskPushNotificationConfigSuccessResponse"},
	"DeleteTaskPushNotificationConfig": {"DeleteTaskPushNotificationConfigRequest", "DeleteTaskPushNotificationConfigResponse", "DeleteTaskPushNotificationConfigSuccessResponse"},
	"GetExtendedAgentCard":             {"GetAuthenticatedExtendedCardRequest", "GetAuthenticatedExtendedCardResponse", "GetAuthenticatedExtendedCardSuccessResponse"},
}

var (
	defsOnce sync.Once
	defs     map[string]json.RawMessage
	resolved sync.Map // definition name -> *jsonschema.Resolved, or error
)

// def resolves one definition of the 0.3 schema, as a draft-07 root that refers to it.
func def(name string) (*jsonschema.Resolved, error) {
	if v, ok := resolved.Load(name); ok {
		if err, bad := v.(error); bad {
			return nil, err
		}
		return v.(*jsonschema.Resolved), nil
	}
	defsOnce.Do(func() {
		var root struct {
			Definitions map[string]json.RawMessage `json:"definitions"`
		}
		_ = json.Unmarshal(schema03, &root)
		defs = root.Definitions
	})
	doc, _ := json.Marshal(map[string]any{
		"$schema":     "http://json-schema.org/draft-07/schema#",
		"$ref":        "#/definitions/" + name,
		"definitions": defs,
	})
	var s jsonschema.Schema
	if err := json.Unmarshal(doc, &s); err != nil {
		resolved.Store(name, err)
		return nil, err
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		resolved.Store(name, err)
		return nil, err
	}
	resolved.Store(name, rs)
	return rs, nil
}

// schema checks data against one definition of the 0.3 schema.
func schema(data []byte, name string) Result {
	rs, err := def(name)
	if err != nil {
		return Result{Version: V03, Note: "not validated: " + err.Error()}
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return result(V03, []Issue{{Path: "$", Rule: RuleNotJSON, Detail: err.Error()}})
	}
	if err := rs.Validate(v); err != nil {
		return result(V03, []Issue{{Path: "$", Rule: RuleSchema, Member: name, Detail: err.Error()}})
	}
	return result(V03, nil)
}

func check03(binding, method string, body []byte, p part) Result {
	d, ok := v03[method]
	if !ok {
		return notValidated(V03, NoteMethod)
	}
	if binding == "jsonrpc" {
		switch p {
		case partRequest:
			return schema(body, d.request)
		case partResponse:
			return schema(body, d.response)
		}
		return schema(body, d.response) // a frame: the stream's JSON-RPC answer
	}
	// HTTP+JSON: the result inside the success answer, or the request's params.
	var success struct {
		Properties struct {
			Result json.RawMessage `json:"result"`
		} `json:"properties"`
	}
	target := d.result
	if p == partRequest {
		var req struct {
			Properties struct {
				Params json.RawMessage `json:"params"`
			} `json:"properties"`
		}
		if raw, ok := definitions()[d.request]; ok && json.Unmarshal(raw, &req) == nil && len(req.Properties.Params) > 0 {
			return inline(body, req.Properties.Params)
		}
		return notValidated(V03, NoteMethod)
	}
	if raw, ok := definitions()[target]; ok && json.Unmarshal(raw, &success) == nil && len(success.Properties.Result) > 0 {
		return inline(body, success.Properties.Result)
	}
	return notValidated(V03, NoteMethod)
}

func definitions() map[string]json.RawMessage {
	_, _ = def("AgentCard") // loads defs
	return defs
}

// inline checks data against a schema fragment of the 0.3 schema (its $refs resolve against its definitions).
func inline(data []byte, fragment json.RawMessage) Result {
	var frag map[string]any
	if err := json.Unmarshal(fragment, &frag); err != nil {
		return Result{Version: V03, Note: "not validated: " + err.Error()}
	}
	frag["$schema"] = "http://json-schema.org/draft-07/schema#"
	frag["definitions"] = definitions()
	doc, _ := json.Marshal(frag)
	var s jsonschema.Schema
	if err := json.Unmarshal(doc, &s); err != nil {
		return Result{Version: V03, Note: "not validated: " + err.Error()}
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		return Result{Version: V03, Note: "not validated: " + err.Error()}
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return result(V03, []Issue{{Path: "$", Rule: RuleNotJSON, Detail: err.Error()}})
	}
	if err := rs.Validate(v); err != nil {
		return result(V03, []Issue{{Path: "$", Rule: RuleSchema, Detail: err.Error()}})
	}
	return result(V03, nil)
}
