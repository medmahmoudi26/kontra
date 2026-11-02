package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// update rewrites the fixtures the agent writes (testdata/messages/surface.json, testdata/card/*.json) from its output.
// `make check-specs` validates them against the vendored A2UI v0.9.1 schemas and spec/schema; these tests hold the
// agent to them.
var update = flag.Bool("update", false, "rewrite the fixtures the agent writes")

// serve starts the agent on an httptest server and returns its root URL.
func serve(t *testing.T) string {
	t.Helper()
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	h = newHandler(srv.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return srv.URL
}

// headers adds a client's extra request headers (A2A-Extensions).
type headers struct {
	h    http.Header
	next http.RoundTripper
}

func (x headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, vs := range x.h {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	return x.next.RoundTrip(r)
}

// client talks to the agent at base over one binding, sending h with every request.
func client(t *testing.T, base string, protocol a2a.TransportProtocol, h http.Header) *a2aclient.Client {
	t.Helper()
	url, hc := base, &http.Client{Transport: headers{h, http.DefaultTransport}}
	opt := a2aclient.WithJSONRPCTransport(hc)
	if protocol == a2a.TransportProtocolHTTPJSON {
		url, opt = base+restPrefix, a2aclient.WithRESTTransport(hc)
	}
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface(url, protocol)}, opt)
	if err != nil {
		t.Fatalf("client for %s %s: %v", protocol, url, err)
	}
	t.Cleanup(func() { _ = c.Destroy() })
	return c
}

func bindings() map[string]a2a.TransportProtocol {
	return map[string]a2a.TransportProtocol{"JSONRPC": a2a.TransportProtocolJSONRPC, "HTTP+JSON": a2a.TransportProtocolHTTPJSON}
}

// activate is the A2A-Extensions header of a client that renders A2UI v0.9.1.
func activate() http.Header { return http.Header{"A2A-Extensions": {ExtA2UI}} }

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func send(t *testing.T, c *a2aclient.Client, m *a2a.Message, task *a2a.Task) *a2a.Task {
	t.Helper()
	if task != nil {
		m.TaskID, m.ContextID = task.ID, task.ContextID
	}
	res, err := c.SendMessage(ctxT(t), &a2a.SendMessageRequest{Message: m})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	got, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("SendMessage returned %T, want a task", res)
	}
	return got
}

func textMessage(s string) *a2a.Message {
	return a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(s))
}

// fixture reads testdata/<name> as a JSON value.
func fixture(t *testing.T, name string) any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("testdata/%s: %v", name, err)
	}
	return v
}

// same reports whether a and b are the same JSON value.
func same(t *testing.T, a, b any) bool {
	t.Helper()
	return reflect.DeepEqual(roundTrip(t, a), roundTrip(t, b))
}

func roundTrip(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// writeFixture writes v to testdata/<name>, indented, with -update.
func writeFixture(t *testing.T, name string, v any) {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", name), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// statusParts is the message a task waits on or ended with: its text, and its A2UI parts.
func statusParts(t *testing.T, task *a2a.Task) (string, []*a2a.Part) {
	t.Helper()
	if task.Status.Message == nil {
		t.Fatalf("task %s (%s) has no status message", task.ID, task.Status.State)
	}
	var text strings.Builder
	var ui []*a2a.Part
	for _, p := range task.Status.Message.Parts {
		text.WriteString(p.Text())
		if isA2UI(p) {
			ui = append(ui, p)
		}
	}
	return text.String(), ui
}

// formPart is the one part of a question that holds the form: the A2UI messages of testdata/messages/surface.json
// (with -update, written from it), marked as A2UI by metadata.mimeType alone and by mediaType.
func formPart(t *testing.T, task *a2a.Task) {
	t.Helper()
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("state %s, want input-required", task.Status.State)
	}
	text, ui := statusParts(t, task)
	if text != askForm || len(ui) != 1 || len(task.Status.Message.Parts) != 2 {
		t.Fatalf("the question %q with %d A2UI parts in %d, want %q and the form", text, len(ui), len(task.Status.Message.Parts), askForm)
	}
	p := ui[0]
	if *update {
		writeFixture(t, "messages/surface.json", p.Data())
	}
	if !same(t, p.Data(), fixture(t, "messages/surface.json")) {
		t.Errorf("the form is not testdata/messages/surface.json:\n%s", mustJSON(t, p.Data()))
	}
	if !maps.Equal(p.Metadata, map[string]any{"mimeType": MimeA2UI}) || p.MediaType != MimeA2UI {
		t.Errorf("the form's part: metadata %v, mediaType %q", p.Metadata, p.MediaType)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// actionMessage is a renderer's A2UI action list as testdata/<name> holds it, in one data part marked as A2UI.
func actionMessage(t *testing.T, name string) *a2a.Message {
	t.Helper()
	p := a2a.NewDataPart(fixture(t, name))
	p.Metadata = map[string]any{"mimeType": MimeA2UI}
	return a2a.NewMessage(a2a.MessageRoleUser, p)
}

// The booking testdata/messages/action.json holds (testdata/card/input-schema-data.json too).
const booked = `Booked a table for 4 under "Robin" on Monday, 9 November 2026 at 19:30.`

// The card is A2A 1.0 with both extensions, each with an explicit "required": false: A2UI v0.9.1 with the basic
// catalog (testdata/card/a2ui-params.json) and the input schema (testdata/card/input-schema-params.json).
func TestCard(t *testing.T) {
	base := serve(t)
	resp, err := http.Get(base + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var card a2a.AgentCard
	if err := json.Unmarshal(raw, &card); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("card: %d %v\n%s", resp.StatusCode, err, raw)
	}
	if len(card.SupportedInterfaces) != 2 || card.SupportedInterfaces[0].URL != base || card.SupportedInterfaces[1].URL != base+restPrefix ||
		card.SupportedInterfaces[0].ProtocolVersion != "1.0" || !card.Capabilities.Streaming {
		t.Fatalf("interfaces %+v, capabilities %+v", card.SupportedInterfaces, card.Capabilities)
	}
	var doc struct {
		Capabilities struct {
			Extensions []map[string]json.RawMessage `json:"extensions"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Capabilities.Extensions) != 2 {
		t.Fatalf("extensions %v %v", doc.Capabilities.Extensions, err)
	}
	for i, want := range []struct{ uri, params string }{{ExtA2UI, "card/a2ui-params.json"}, {ExtInputSchema, "card/input-schema-params.json"}} {
		e := doc.Capabilities.Extensions[i]
		var uri string
		_ = json.Unmarshal(e["uri"], &uri)
		if uri != want.uri || string(e["required"]) != "false" || len(e) != 4 || e["description"] == nil {
			t.Errorf("extension %d: %v, want %s with \"required\": false and a description", i, e, want.uri)
		}
		var params any
		_ = json.Unmarshal(e["params"], &params)
		if *update {
			writeFixture(t, want.params, params)
		}
		if !same(t, params, fixture(t, want.params)) {
			t.Errorf("%s's params are not testdata/%s:\n%s", want.uri, want.params, e["params"])
		}
	}
}

// A first message from a client that renders A2UI stops at input-required with the form; the form's action, sent
// back as a renderer sends it (testdata/messages/action.json), completes the same task with the booking. Both
// bindings.
func TestFormThenAction(t *testing.T) {
	for name, protocol := range bindings() {
		t.Run(name, func(t *testing.T) {
			c := client(t, serve(t), protocol, activate())
			first := send(t, c, textMessage("a table, please"), nil)
			formPart(t, first)
			done := send(t, c, actionMessage(t, "messages/action.json"), first)
			if text, ui := statusParts(t, done); done.Status.State != a2a.TaskStateCompleted || text != booked || len(ui) != 0 || done.ID != first.ID {
				t.Fatalf("after the action: %s %q (%d A2UI parts) on task %s, want completed %q on %s", done.Status.State, text, len(ui), done.ID, booked, first.ID)
			}
		})
	}
}

// A client renders the form when its a2uiClientCapabilities list the basic catalog under v0.9.1 or v0.9 (message
// metadata, or the request's), or, without them, when it activates the extension (A2A-Extensions in any spelling,
// X-A2A-Extensions, or the message's extensions). Anything else is asked for the input schema's data part.
func TestNegotiation(t *testing.T) {
	basic := map[string]any{"supportedCatalogIds": []any{BasicCatalog}}
	other := map[string]any{"supportedCatalogIds": []any{"https://example.com/catalogs/other.json"}}
	for _, tc := range []struct {
		name    string
		header  http.Header
		exts    []string
		caps    map[string]any // message metadata
		reqCaps map[string]any // request metadata
		form    bool
	}{
		{name: "nothing"},
		{name: "A2A-Extensions", header: activate(), form: true},
		{name: "A2A-Extensions among others", header: http.Header{"A2A-Extensions": {"urn:other, " + ExtA2UI}}, form: true},
		{name: "X-A2A-Extensions", header: http.Header{"X-A2A-Extensions": {ExtA2UI}}, form: true},
		{name: "another A2UI version", header: http.Header{"A2A-Extensions": {"https://a2ui.org/a2a-extension/a2ui/v0.8"}}},
		{name: "message extensions", exts: []string{ExtA2UI}, form: true},
		{name: "capabilities v0.9 (fixture)", caps: fixture(t, "messages/capabilities.json").(map[string]any), form: true},
		{name: "capabilities v0.9.1", caps: map[string]any{"v0.9.1": basic}, form: true},
		{name: "request capabilities", reqCaps: map[string]any{"v0.9": basic}, form: true},
		{name: "capabilities without the basic catalog", header: activate(), caps: map[string]any{"v0.9": other}},
		{name: "capabilities of v0.8 only", caps: map[string]any{"v0.8": basic}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := client(t, serve(t), a2a.TransportProtocolJSONRPC, tc.header)
			m := textMessage("a table, please")
			m.Extensions = tc.exts
			if tc.caps != nil {
				m.Metadata = map[string]any{capsKey: tc.caps}
			}
			req := &a2a.SendMessageRequest{Message: m}
			if tc.reqCaps != nil {
				req.Metadata = map[string]any{capsKey: tc.reqCaps}
			}
			res, err := c.SendMessage(ctxT(t), req)
			if err != nil {
				t.Fatal(err)
			}
			task := res.(*a2a.Task)
			if tc.form {
				formPart(t, task)
				return
			}
			if text, ui := statusParts(t, task); task.Status.State != a2a.TaskStateInputRequired || text != askData || len(ui) != 0 {
				t.Fatalf("%s %q with %d A2UI parts, want input-required %q alone", task.Status.State, text, len(ui), askData)
			}
		})
	}
}

// A first message whose data part is the input schema's (testdata/card/input-schema-data.json, which make
// check-specs validates against the card's schema) books at once, with or without A2UI. One that holds part of it is
// asked for the rest: with the form holding what checked out, for a client that renders A2UI.
func TestInputSchemaData(t *testing.T) {
	c := client(t, serve(t), a2a.TransportProtocolJSONRPC, nil)
	p := a2a.NewDataPart(fixture(t, "card/input-schema-data.json"))
	p.MediaType = "application/json"
	done := send(t, c, a2a.NewMessage(a2a.MessageRoleUser, p, a2a.NewTextPart("for the window table")), nil)
	if text, _ := statusParts(t, done); done.Status.State != a2a.TaskStateCompleted || text != `Booked a table for 4 under "Robin" on Monday, 9 November 2026 at 19:30 +01:00.` {
		t.Fatalf("a complete first data part: %s %q", done.Status.State, text)
	}

	ui := client(t, serve(t), a2a.TransportProtocolHTTPJSON, activate())
	partial := send(t, ui, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{"name": " Robin ", "guests": 9.0})), nil)
	text, parts := statusParts(t, partial)
	const want = "The number of guests must be 1 to 6. The date must be a date and time, like 2026-11-09T19:30. " + askForm
	if partial.Status.State != a2a.TaskStateInputRequired || text != want || len(parts) != 1 {
		t.Fatalf("a partial first data part: %s %q with %d A2UI parts, want %q and the form", partial.Status.State, text, len(parts), want)
	}
	msgs := parts[0].Data().([]any)
	model := msgs[len(msgs)-1].(map[string]any)["updateDataModel"]
	if !same(t, model, map[string]any{"surfaceId": surfaceID, "path": modelPath, "value": map[string]any{"name": "Robin", "guests": []any{"2"}, "date": ""}}) {
		t.Errorf("the form's data model: %s, want the name filled in, two guests and no date", mustJSON(t, model))
	}

	// A data part that is not a booking (the A2UI samples send {"useStreaming": false} beside every message) is the
	// first question's.
	other := send(t, ui, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello"), a2a.NewDataPart(map[string]any{"useStreaming": false})), nil)
	formPart(t, other)
}

// The action is read in each shape a renderer may send it: v0.9.1's list, one message on its own, the legacy MIME
// type, mediaType instead of metadata.mimeType, and v0.8's userAction. A2UI parts that are not a book action, and a
// data part without the A2UI mark, are not.
func TestActionShapes(t *testing.T) {
	action := fixture(t, "messages/action.json").([]any)[0].(map[string]any)
	userAction := map[string]any{"userAction": action["action"]}
	for _, tc := range []struct {
		name string
		part *a2a.Part
	}{
		{"the list", &a2a.Part{Content: a2a.Data{Value: []any{action}}, Metadata: map[string]any{"mimeType": MimeA2UI}}},
		{"one message", &a2a.Part{Content: a2a.Data{Value: action}, Metadata: map[string]any{"mimeType": MimeA2UI}}},
		{"the legacy MIME type", &a2a.Part{Content: a2a.Data{Value: []any{action}}, Metadata: map[string]any{"mimeType": mimeA2UILegacy}}},
		{"mediaType", &a2a.Part{Content: a2a.Data{Value: []any{action}}, MediaType: MimeA2UI}},
		{"userAction (v0.8)", &a2a.Part{Content: a2a.Data{Value: userAction}, Metadata: map[string]any{"mimeType": mimeA2UILegacy}}},
		{"after other messages", &a2a.Part{Content: a2a.Data{Value: []any{map[string]any{"version": "v0.9.1", "error": map[string]any{}}, action}},
			Metadata: map[string]any{"mimeType": MimeA2UI}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := client(t, serve(t), a2a.TransportProtocolJSONRPC, activate())
			first := send(t, c, textMessage("a table, please"), nil)
			done := send(t, c, a2a.NewMessage(a2a.MessageRoleUser, tc.part), first)
			if text, _ := statusParts(t, done); done.Status.State != a2a.TaskStateCompleted || text != booked {
				t.Fatalf("%s %q, want completed %q", done.Status.State, text, booked)
			}
		})
	}

	other := map[string]any{"version": "v0.9.1", "action": map[string]any{"name": "cancel", "surfaceId": surfaceID, "context": action["action"].(map[string]any)["context"]}}
	for _, tc := range []struct {
		name string
		part *a2a.Part
	}{
		{"another action", &a2a.Part{Content: a2a.Data{Value: []any{other}}, Metadata: map[string]any{"mimeType": MimeA2UI}}},
		{"an unmarked action", a2a.NewDataPart([]any{action})},
		{"text", a2a.NewTextPart("Robin, 4, 2026-11-09T19:30")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := client(t, serve(t), a2a.TransportProtocolJSONRPC, activate())
			first := send(t, c, textMessage("a table, please"), nil)
			again := send(t, c, a2a.NewMessage(a2a.MessageRoleUser, tc.part), first)
			// The form was sent: the question is in text alone, and the client keeps its form.
			if text, ui := statusParts(t, again); again.Status.State != a2a.TaskStateInputRequired || text != "That was not a booking. "+askCorrect || len(ui) != 0 {
				t.Fatalf("%s %q with %d A2UI parts", again.Status.State, text, len(ui))
			}
		})
	}
}

// An action whose fields do not check out is asked to be corrected, in text alone: the client keeps its form and
// what the person typed in it. A correct one then completes the task. A client that had no form yet gets it.
func TestCorrections(t *testing.T) {
	c := client(t, serve(t), a2a.TransportProtocolJSONRPC, activate())
	first := send(t, c, textMessage("a table, please"), nil)
	action := fixture(t, "messages/action.json").([]any)
	bad := roundTrip(t, action).([]any)
	ctx := bad[0].(map[string]any)["action"].(map[string]any)["context"].(map[string]any)
	ctx["name"], ctx["guests"], ctx["date"] = "", []any{"7"}, "tomorrow"
	p := a2a.NewDataPart(bad)
	p.Metadata = map[string]any{"mimeType": MimeA2UI}
	again := send(t, c, a2a.NewMessage(a2a.MessageRoleUser, p), first)
	const want = "The booking needs a name of at most 100 characters. The number of guests must be 1 to 6. " +
		"The date must be a date and time, like 2026-11-09T19:30. " + askCorrect
	if text, ui := statusParts(t, again); again.Status.State != a2a.TaskStateInputRequired || text != want || len(ui) != 0 {
		t.Fatalf("a bad action: %s %q with %d A2UI parts, want %q alone", again.Status.State, text, len(ui), want)
	}
	// Twice: the form is found in the task's history, not only in the question it waits on.
	again = send(t, c, textMessage("?"), first)
	if text, ui := statusParts(t, again); text != "That was not a booking. "+askCorrect || len(ui) != 0 {
		t.Fatalf("a second question: %q with %d A2UI parts", text, len(ui))
	}
	done := send(t, c, actionMessage(t, "messages/action.json"), first)
	if text, _ := statusParts(t, done); done.Status.State != a2a.TaskStateCompleted || text != booked {
		t.Fatalf("the corrected action: %s %q", done.Status.State, text)
	}

	// A task asked in text whose next message comes from a client that renders A2UI is sent the form, once.
	base := serve(t)
	plain, ui := client(t, base, a2a.TransportProtocolJSONRPC, nil), client(t, base, a2a.TransportProtocolJSONRPC, activate())
	asked := send(t, plain, textMessage("a table, please"), nil)
	if text, parts := statusParts(t, asked); text != askData || len(parts) != 0 {
		t.Fatalf("asked in text: %q with %d A2UI parts", text, len(parts))
	}
	withForm := send(t, ui, textMessage("?"), asked)
	if text, parts := statusParts(t, withForm); text != "That was not a booking. "+askForm || len(parts) != 1 {
		t.Fatalf("then from a client that renders A2UI: %q with %d A2UI parts, want the form", text, len(parts))
	}
	if again := send(t, ui, textMessage("?"), asked); len(again.Status.Message.Parts) != 1 {
		t.Fatalf("the form was sent twice: %d parts", len(again.Status.Message.Parts))
	}
}

// CancelTask ends a task waiting for its booking canceled.
func TestCancel(t *testing.T) {
	c := client(t, serve(t), a2a.TransportProtocolJSONRPC, activate())
	first := send(t, c, textMessage("a table, please"), nil)
	task, err := c.CancelTask(ctxT(t), &a2a.CancelTaskRequest{ID: first.ID})
	if err != nil || task.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("CancelTask: %+v %v", task, err)
	}
}

// run prints the root URL first, serves the card there, and returns 0 when its context ends; a bad flag is 2.
func TestRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	code := make(chan int, 1)
	go func() { code <- run(ctx, []string{"--port", "0"}, pw, io.Discard); pw.Close() }()
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "http://127.0.0.1:") {
		t.Fatalf("first line %q %v", line, err)
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	resp, err := http.Get(strings.TrimSpace(line) + "/.well-known/agent-card.json")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("card: %v %v", resp, err)
	}
	resp.Body.Close()
	cancel()
	if c := <-code; c != 0 {
		t.Fatalf("exit %d", c)
	}
	for _, args := range [][]string{{"--port", "70000"}, {"extra"}, {"--nope"}} {
		if c := run(context.Background(), args, io.Discard, io.Discard); c != 2 {
			t.Errorf("run %v: exit %d, want 2", args, c)
		}
	}
}
