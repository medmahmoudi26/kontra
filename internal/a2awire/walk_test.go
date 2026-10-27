package a2awire

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"kontra.sh/kontra/internal/strictjson"
)

// The sealing walker refuses a lone case variant of every member it reads, at every level it reads, in requests and
// answers (D172, ext-sealed.md §5): encoding/json, which a2a-go and the gate decode with, takes {"Artifacts": …} for
// artifacts, so a part under it would be neither sealed nor opened by a walk that reads exact names. The exact names
// pass, and a member the walk does not read may differ in case.
func TestWalkerLoneCaseVariants(t *testing.T) {
	part := `{"text":"x"}`
	msg := func(extra string) string {
		return `{"messageId":"m","role":"ROLE_USER","taskId":"t","contextId":"c","parts":[` + part + `]` + extra + `}`
	}
	task := `{"id":"t","contextId":"c","status":{"state":"TASK_STATE_WORKING","message":` + msg("") + `},` +
		`"history":[` + msg("") + `],"artifacts":[{"artifactId":"a","parts":[` + part + `]}]}`
	rpcReq := func(params string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":` + params + `}`
	}
	rpcRes := func(result string) string { return `{"jsonrpc":"2.0","id":1,"result":` + result + `}` }

	type tc struct {
		name, body, of string
		request        bool
	}
	cases := []tc{
		// Requests.
		{"request: METHOD", `{"jsonrpc":"2.0","id":1,"METHOD":"SendMessage","params":{"message":` + msg("") + `}}`, "method", true},
		{"request: Params", `{"jsonrpc":"2.0","id":1,"method":"SendMessage","Params":{"message":` + msg("") + `}}`, "params", true},
		{"request: params.Message", rpcReq(`{"Message":` + msg("") + `}`), "message", true},
		{"request: params.Configuration", rpcReq(`{"message":` + msg("") + `,"Configuration":{}}`), "configuration", true},
		{"request: configuration.TaskPushNotificationConfig", rpcReq(`{"message":` + msg("") +
			`,"configuration":{"TaskPushNotificationConfig":{"url":"https://x.example"}}}`), "taskPushNotificationConfig", true},
		{"request: message.Parts", rpcReq(`{"message":{"messageId":"m","Parts":[` + part + `]}}`), "parts", true},
		{"request: message.TaskId", rpcReq(`{"message":{"messageId":"m","TaskId":"up-1","parts":[` + part + `]}}`), "taskId", true},
		{"request: message.ContextID", rpcReq(`{"message":{"messageId":"m","ContextID":"c","parts":[` + part + `]}}`), "contextId", true},
		{"request: part.Text", rpcReq(`{"message":{"messageId":"m","parts":[{"Text":"x"}]}}`), "text", true},
		{"request: HTTP+JSON Message", `{"Message":` + msg("") + `}`, "message", true},
		{"request: HTTP+JSON JSONRPC", `{"JSONRPC":"2.0","message":` + msg("") + `}`, "jsonrpc", true},
		// Answers.
		{"answer: Result", `{"jsonrpc":"2.0","id":1,"Result":{"task":` + task + `}}`, "result", false},
		{"answer: Task", rpcRes(`{"Task":` + task + `}`), "task", false},
		{"answer: Message", rpcRes(`{"Message":` + msg("") + `}`), "message", false},
		{"answer: StatusUpdate", `{"StatusUpdate":{"taskId":"t","status":{"message":` + msg("") + `}}}`, "statusUpdate", false},
		{"answer: ArtifactUpdate", `{"ArtifactUpdate":{"taskId":"t","artifact":{"artifactId":"a","parts":[` + part + `]}}}`, "artifactUpdate", false},
		{"answer: Tasks", `{"Tasks":[` + task + `]}`, "tasks", false},
		{"answer: a bare Task's Artifacts", rpcRes(`{"id":"t","status":{"state":"TASK_STATE_WORKING"},"Artifacts":[{"artifactId":"a","parts":[` + part + `]}]}`), "artifacts", false},
		{"task: Artifacts", rpcRes(`{"task":{"id":"t","Artifacts":[{"artifactId":"a","parts":[` + part + `]}]}}`), "artifacts", false},
		{"task: History", rpcRes(`{"task":{"id":"t","History":[` + msg("") + `]}}`), "history", false},
		{"task: Status", rpcRes(`{"task":{"id":"t","Status":{"message":` + msg("") + `}}}`), "status", false},
		{"task: ID", rpcRes(`{"task":{"ID":"t","status":{"state":"TASK_STATE_WORKING"}}}`), "id", false},
		{"status: Message", rpcRes(`{"task":{"id":"t","status":{"state":"TASK_STATE_COMPLETED","Message":` + msg("") + `}}}`), "message", false},
		{"status: State", rpcRes(`{"task":{"id":"t","status":{"State":"TASK_STATE_COMPLETED"}}}`), "state", false},
		{"status message: Parts", rpcRes(`{"task":{"id":"t","status":{"message":{"messageId":"s","Parts":[` + part + `]}}}}`), "parts", false},
		{"history entry: Parts", rpcRes(`{"task":{"id":"t","history":[{"messageId":"h","Parts":[` + part + `]}]}}`), "parts", false},
		{"artifact: Parts", rpcRes(`{"task":{"id":"t","artifacts":[{"artifactId":"a","Parts":[` + part + `]}]}}`), "parts", false},
		{"artifact: ArtifactID", rpcRes(`{"task":{"id":"t","artifacts":[{"ArtifactID":"a","parts":[` + part + `]}]}}`), "artifactId", false},
		{"status update: Status", `{"statusUpdate":{"taskId":"t","Status":{"message":` + msg("") + `}}}`, "status", false},
		{"status update: TaskID", `{"statusUpdate":{"TaskID":"t","status":{"state":"TASK_STATE_WORKING"}}}`, "taskId", false},
		{"artifact update: Artifact", `{"artifactUpdate":{"taskId":"t","Artifact":{"artifactId":"a","parts":[` + part + `]}}}`, "artifact", false},
		{"artifact update: Append", `{"artifactUpdate":{"taskId":"t","Append":true,"artifact":{"artifactId":"a","parts":[` + part + `]}}}`, "append", false},
		{"list entry: Artifacts", `{"tasks":[{"id":"t","Artifacts":[{"artifactId":"a","parts":[` + part + `]}]}]}`, "artifacts", false},
		{"answer message: Parts", rpcRes(`{"message":{"messageId":"m","Parts":[` + part + `]}}`), "parts", false},
		{"answer part: Raw", rpcRes(`{"message":{"messageId":"m","parts":[{"Raw":"eA=="}]}}`), "raw", false},
	}
	visited := 0
	w := Walker{Container: func(c *Container) error { visited += len(c.Parts); return nil }}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			walk := w.WalkReply
			if c.request {
				walk = w.WalkRequest
			}
			_, _, err := walk([]byte(c.body))
			var ke *strictjson.KeyError
			if !errors.As(err, &ke) || ke.Of != c.of {
				t.Fatalf("a lone case variant of %s: %v", c.of, err)
			}
		})
	}
	// The exact names pass, every part visited; a member the walk does not read may be any case.
	for _, body := range []string{rpcRes(`{"task":` + task + `}`), `{"task":` + task + `,"Kind":"task"}`, `{"tasks":[` + task + `],"NextPageToken":""}`} {
		visited = 0
		if _, _, err := w.WalkReply([]byte(body)); err != nil || visited != 3 {
			t.Errorf("%s: %v, %d parts visited", body, err, visited)
		}
	}
	visited = 0
	if _, _, err := w.WalkRequest([]byte(rpcReq(`{"message":` + msg(`,"Extensions":[]`) + `,"configuration":{"returnImmediately":true}}`))); err != nil || visited != 1 {
		t.Errorf("an exact request: %v, %d parts", err, visited)
	}
}

// A node reads an answer in its request's binding: a2a-go's JSON-RPC client reads a response's result only, its
// HTTP+JSON client the whole body, so an answer that carries the other binding's shape is refused (ErrShape).
func TestWalkerBinding(t *testing.T) {
	forged := `{"jsonrpc":"2.0","id":1,"result":{"message":{"messageId":"m","parts":[{"text":"sealed"}]}},` +
		`"task":{"id":"t","status":{"message":{"messageId":"s","parts":[{"text":"FORGED"}]}}}}`
	_, _, err := Walker{Binding: BindingHTTP}.WalkReply([]byte(forged))
	if !errors.Is(err, ErrShape) {
		t.Fatalf("a JSON-RPC body to an HTTP+JSON request: %v", err)
	}
	_, _, err = Walker{Binding: BindingJSONRPC}.WalkReply([]byte(`{"task":{"id":"t"}}`))
	if !errors.Is(err, ErrShape) {
		t.Fatalf("an HTTP+JSON body to a JSON-RPC request: %v", err)
	}
	var texts []string
	w := Walker{Binding: BindingJSONRPC, Container: func(c *Container) error {
		for _, p := range c.Parts {
			var s string
			_ = json.Unmarshal(p["text"], &s)
			texts = append(texts, s)
		}
		return nil
	}}
	if _, _, err := w.WalkReply([]byte(`{"jsonrpc":"2.0","id":1,"result":{"message":{"messageId":"m","parts":[{"text":"ok"}]}}}`)); err != nil ||
		strings.Join(texts, ",") != "ok" {
		t.Fatalf("a JSON-RPC answer: %v %v", err, texts)
	}
}
