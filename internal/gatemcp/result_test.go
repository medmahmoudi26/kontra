package gatemcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/store"
)

// The hire result's shapes (§12.2, §12.3; D289, D324). A client may show its model and its user a tool result's
// structuredContent and nothing else (Claude Code 2.1.281 does, M5's gate G3, C59), so every CallToolResult a hire or a
// reply answers carries in structuredContent what a person needs: taskId and state; the task's thread URL (the approve
// URL, exactly as the URL elicitation carries it) whenever the task has one; the quote's terms while it is quoted; the
// text of the content's text blocks (the agent's answer, its question, or what the gate says); data and file links.
// Each shape is validated against the outputSchema hire and reply declare, and is the fixture
// spec/schema/examples/mcp-tools.hireResult.<shape>.json, which make check-specs validates with a full 2020-12
// validator.

const (
	shapeTask    = "019a0000-0000-7000-8000-000000000001" // taskWith's id
	shapeApprove = "https://app.kontra.sh/#/t/" + shapeTask
)

var shapeExpiry = time.Date(2026, 11, 1, 18, 30, 0, 0, time.UTC)

// hireShapes are the CallToolResults hire and reply answer, by shape: what quotedResult, plainAnswer and Render write
// for a task in each state.
func hireShapes(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	h := &Handler{deps: Deps{PublicURL: "https://gateway.example.com/"}}
	id := uuid.MustParse(shapeTask)
	thread := map[string]any{lifecycle.MetaApproveURL: shapeApprove}
	row := func(state store.State, task *a2a.Task) *store.TaskRow {
		return &store.TaskRow{ID: id, Service: "hello", State: state, Snapshot: task}
	}
	plain := func(r *store.TaskRow) json.RawMessage {
		t.Helper()
		return h.plainAnswer(context.Background(), r, auth.Principal{Kind: auth.KindKey, ID: "key:kk_buyer", BuyerID: uuid.Nil})
	}
	quoted := row(store.StateQuoted, taskWith(a2a.TaskStateInputRequired, nil, thread))
	quoted.QuoteExpiresAt = &shapeExpiry

	file := a2a.NewRawPart([]byte("tck"))
	file.MediaType, file.Filename = "text/plain", "output.txt"
	link := a2a.NewFileURLPart("https://example.com/output.txt", "text/plain")
	link.Filename = "output.txt"
	answered := taskWith(a2a.TaskStateCompleted, agentText("the service ended the task: completed"), thread,
		&a2a.Artifact{ID: "a1", Parts: a2a.ContentParts{a2a.NewTextPart("Hello "), a2a.NewTextPart("World"), a2a.NewDataPart(map[string]any{"n": 1.0})}},
		&a2a.Artifact{ID: "a2", Parts: a2a.ContentParts{file, link}})

	asking := taskWith(a2a.TaskStateInputRequired, agentText("Which date?"), thread)
	return map[string]json.RawMessage{
		"quoted":         plain(quoted),
		"completed":      plain(row(store.StateCompleted, answered)),
		"completed_free": plain(row(store.StateCompleted, taskWith(a2a.TaskStateCompleted, agentText("Hello World"), nil))),
		"failed":         plain(row(store.StateFailed, taskWith(a2a.TaskStateFailed, nil, map[string]any{store.MetaFailure: "timeout", lifecycle.MetaApproveURL: shapeApprove}))),
		"rejected":       plain(row(store.StateRejected, taskWith(a2a.TaskStateRejected, agentText("I do not take that kind of work"), nil))),
		"declined":       plain(row(store.StateCanceled, taskWith(a2a.TaskStateCanceled, nil, map[string]any{store.MetaCanceled: store.CanceledDeclined, lifecycle.MetaApproveURL: shapeApprove}))),
		"input_required": plain(row(store.StateInputRequired, asking)),
		"working":        plain(row(store.StateWorking, taskWith(a2a.TaskStateWorking, nil, thread))),
	}
}

type shapeResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Structured json.RawMessage `json:"structuredContent"`
	IsError    bool            `json:"isError"`
	Meta       map[string]any  `json:"_meta"`
}

func decodeShape(t *testing.T, name string, b json.RawMessage) (shapeResult, HireResult) {
	t.Helper()
	var r shapeResult
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("%s: %s: %v", name, b, err)
	}
	var hr HireResult
	dec := json.NewDecoder(bytes.NewReader(r.Structured))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&hr); err != nil {
		t.Fatalf("%s: structuredContent %s: %v", name, r.Structured, err)
	}
	return r, hr
}

// texts is a result's text blocks, data parts' JSON aside: what structuredContent.text joins.
func texts(r shapeResult, data []any) []string {
	var out []string
	skip := map[string]bool{}
	for _, d := range data {
		b, _ := json.Marshal(d)
		skip[string(b)] = true
	}
	for _, c := range r.Content {
		if c.Type == "text" && !skip[c.Text] {
			out = append(out, c.Text)
		}
	}
	return out
}

// Each shape's structuredContent carries what the text tells a person: the thread URL, the quote's terms, the agent's
// answer or question, and the task; and the text blocks stay in content for a client that reads only content.
func TestHireResultShapes(t *testing.T) {
	shapes := hireShapes(t)
	type want struct {
		state, url, text string
		quote            *QuoteTerms
		isError          bool
		data, files      bool
	}
	wants := map[string]want{
		"quoted": {state: "TASK_STATE_INPUT_REQUIRED", url: shapeApprove,
			text: "This task needs a person to approve its quote before it runs: open " + shapeApprove + " to review and approve it (task " +
				shapeTask + "; the quote expires at 2026-11-01T18:30:00Z). Then call hire again with the same arguments to get its result.",
			quote: &QuoteTerms{ApproveURL: shapeApprove, ExpiresAt: "2026-11-01T18:30:00Z"}},
		"completed": {state: "TASK_STATE_COMPLETED", url: shapeApprove,
			text: "the service ended the task: completed\nHello World", data: true, files: true},
		"completed_free": {state: "TASK_STATE_COMPLETED", text: "Hello World"},
		"failed":         {state: "TASK_STATE_FAILED", url: shapeApprove, text: "the task timed out", isError: true},
		"rejected":       {state: "TASK_STATE_REJECTED", text: "I do not take that kind of work", isError: true},
		"declined": {state: "TASK_STATE_CANCELED", url: shapeApprove,
			text: "the quote was declined: the task was canceled and nothing was charged", isError: true},
		"input_required": {state: "TASK_STATE_INPUT_REQUIRED", url: shapeApprove,
			text: "Which date? (answer with the reply tool, taskId " + shapeTask + ")"},
		"working": {state: "TASK_STATE_WORKING", url: shapeApprove,
			text: "The task " + shapeTask + " is still working. Follow it with GetTask at https://gateway.example.com/agents/hello " +
				"(the same credential), or call reply when it asks a question."},
	}
	if len(shapes) != len(wants) {
		t.Fatalf("%d shapes, %d expectations", len(shapes), len(wants))
	}
	for name, b := range shapes {
		w := wants[name]
		r, hr := decodeShape(t, name, b)
		if hr.TaskID != shapeTask || hr.State != w.state || hr.URL != w.url || hr.Text != w.text || !reflect.DeepEqual(hr.Quote, w.quote) ||
			r.IsError != w.isError || (len(hr.Data) > 0) != w.data || (len(hr.Files) > 0) != w.files {
			t.Errorf("%s: isError %v, structuredContent %s", name, r.IsError, r.Structured)
		}
		// The content keeps its text (for a client that reads only content), and the structured text is that text: the
		// content's text blocks, data parts' JSON aside, in order.
		got := texts(r, hr.Data)
		if len(got) == 0 || strings.ReplaceAll(hr.Text, "\n", "") != strings.Join(got, "") {
			t.Errorf("%s: the content's text blocks %q, the structured text %q", name, got, hr.Text)
		}
		if w.url != "" && !strings.Contains(string(b), w.url) {
			t.Errorf("%s: the URL is not in the result", name)
		}
	}
	// The quoted answer: the URL in the text, in structuredContent and in _meta, the one the URL elicitation carries.
	r, hr := decodeShape(t, "quoted", shapes["quoted"])
	if len(r.Content) != 1 || r.Content[0].Text != hr.Text || !strings.Contains(r.Content[0].Text, shapeApprove) ||
		r.Meta[MetaApproveURL] != shapeApprove || hr.URL != approveURL(taskWith(a2a.TaskStateInputRequired, nil, map[string]any{lifecycle.MetaApproveURL: shapeApprove})) {
		t.Errorf("the quoted answer: %s", shapes["quoted"])
	}
	// The completed answer: the agent's text parts joined as statusText joins a message's (a streamed answer's chunks
	// are parts of one artifact), the message's and each artifact's text on their own lines; data and files beside.
	_, hr = decodeShape(t, "completed", shapes["completed"])
	wantFiles := []FileRef{{URI: "kontra:task/" + shapeTask + "/output.txt", Name: "output.txt", MediaType: "text/plain"},
		{URI: "https://example.com/output.txt", Name: "output.txt", MediaType: "text/plain"}}
	if !reflect.DeepEqual(hr.Data, []any{map[string]any{"n": 1.0}}) || !reflect.DeepEqual(hr.Files, wantFiles) {
		t.Errorf("the completed answer's data %v and files %+v", hr.Data, hr.Files)
	}
	// A task with no thread (Admission allowed it without a quote) has no url: the gate never builds the operator's.
	if _, hr := decodeShape(t, "completed_free", shapes["completed_free"]); hr.URL != "" {
		t.Errorf("a task that was never quoted has url %q", hr.URL)
	}
	// An approve URL that is not https:// (none passes hooks.CheckQuote) is left out rather than break the schema.
	if u := threadURL(taskWith(a2a.TaskStateCompleted, nil, map[string]any{lifecycle.MetaApproveURL: "http://x.example/t"})); u != "" {
		t.Errorf("an http approve URL: %q", u)
	}
}

// Every shape validates against the outputSchema hire and reply declare, and is the fixture make check-specs validates.
func TestHireResultSchema(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(schemaHireResult), &schema); err != nil {
		t.Fatal(err)
	}
	shapes := hireShapes(t)
	names := make([]string, 0, len(shapes))
	for name := range shapes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		r, _ := decodeShape(t, name, shapes[name])
		var v any
		if err := json.Unmarshal(r.Structured, &v); err != nil {
			t.Fatal(err)
		}
		if err := validate(schema, v, "structuredContent"); err != nil {
			t.Errorf("%s: %v: %s", name, err, r.Structured)
		}
		path := filepath.Join("..", "..", "spec", "schema", "examples", "mcp-tools.hireResult."+name+".json")
		file, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: the fixture: %v (want %s)", name, err, r.Structured)
			continue
		}
		got, _ := jcs.Transform(r.Structured)
		want, err := jcs.Transform(file)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: structuredContent\n%s\nthe fixture %s\n%s", name, got, path, want)
		}
	}
	// The validator refuses what the schema refuses, so a shape that passes it is one the schema admits.
	for what, bad := range map[string]string{
		"an extra member":           `{"taskId":"` + shapeTask + `","state":"TASK_STATE_WORKING","approveUrl":"https://a.example"}`,
		"no state":                  `{"taskId":"` + shapeTask + `"}`,
		"an unknown state":          `{"taskId":"` + shapeTask + `","state":"working"}`,
		"a task id that is no uuid": `{"taskId":"t1","state":"TASK_STATE_WORKING"}`,
		"an http url":               `{"taskId":"` + shapeTask + `","state":"TASK_STATE_WORKING","url":"http://a.example"}`,
		"a quote without expiry":    `{"taskId":"` + shapeTask + `","state":"TASK_STATE_INPUT_REQUIRED","quote":{"approveUrl":"https://a.example"}}`,
		"an expiry that is no time": `{"taskId":"` + shapeTask + `","state":"TASK_STATE_INPUT_REQUIRED","quote":{"approveUrl":"https://a.example","expiresAt":"soon"}}`,
		"empty data":                `{"taskId":"` + shapeTask + `","state":"TASK_STATE_COMPLETED","data":[]}`,
		"a file without uri":        `{"taskId":"` + shapeTask + `","state":"TASK_STATE_COMPLETED","files":[{"name":"x"}]}`,
		"text that is no string":    `{"taskId":"` + shapeTask + `","state":"TASK_STATE_COMPLETED","text":["a"]}`,
	} {
		var v any
		_ = json.Unmarshal([]byte(bad), &v)
		if validate(schema, v, "structuredContent") == nil {
			t.Errorf("the validator admits %s", what)
		}
	}
}

// validate checks v against the keywords schemaHireResult uses, and refuses a schema with any other: a keyword it
// does not know would otherwise pass unchecked.
func validate(schema map[string]any, v any, at string) error {
	for k := range schema {
		if !slices.Contains([]string{"type", "description", "required", "additionalProperties", "properties", "enum",
			"items", "minItems", "pattern", "format"}, k) {
			return fmt.Errorf("%s: the test's validator does not know the keyword %q", at, k)
		}
	}
	if typ, ok := schema["type"].(string); ok {
		okType := false
		switch typ {
		case "object":
			_, okType = v.(map[string]any)
		case "array":
			_, okType = v.([]any)
		case "string":
			_, okType = v.(string)
		}
		if !okType {
			return fmt.Errorf("%s is not of type %s", at, typ)
		}
	}
	if enum, ok := schema["enum"].([]any); ok && !slices.Contains(enum, v) {
		return fmt.Errorf("%s: %v is not one of %v", at, v, enum)
	}
	if s, ok := v.(string); ok {
		if p, ok := schema["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(s) {
			return fmt.Errorf("%s: %q does not match %s", at, s, p)
		}
		switch schema["format"] {
		case "uuid":
			if _, err := uuid.Parse(s); err != nil || len(s) != 36 {
				return fmt.Errorf("%s: %q is not a uuid", at, s)
			}
		case "date-time":
			if _, err := time.Parse(time.RFC3339, s); err != nil {
				return fmt.Errorf("%s: %q is not a date-time", at, s)
			}
		case nil:
		default:
			return fmt.Errorf("%s: the test's validator does not know the format %v", at, schema["format"])
		}
	}
	if a, ok := v.([]any); ok {
		if n, ok := schema["minItems"].(float64); ok && float64(len(a)) < n {
			return fmt.Errorf("%s has %d items, fewer than %v", at, len(a), n)
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, e := range a {
				if err := validate(items, e, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		}
	}
	if o, ok := v.(map[string]any); ok {
		props, _ := schema["properties"].(map[string]any)
		for _, r := range asStrings(schema["required"]) {
			if _, ok := o[r]; !ok {
				return fmt.Errorf("%s lacks %s", at, r)
			}
		}
		for k, e := range o {
			sub, ok := props[k].(map[string]any)
			if !ok {
				if schema["additionalProperties"] == false {
					return fmt.Errorf("%s has a member %s the schema does not admit", at, k)
				}
				continue
			}
			if err := validate(sub, e, at+"."+k); err != nil {
				return err
			}
		}
	}
	return nil
}

func asStrings(v any) []string {
	var out []string
	if a, ok := v.([]any); ok {
		for _, e := range a {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}
