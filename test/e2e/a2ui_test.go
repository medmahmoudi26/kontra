package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"kontra.sh/kontra/internal/jcs"
)

// The A2UI example seller's marks and texts (examples/sellers/a2ui/main.go; its README).
const (
	a2uiExt     = "https://a2ui.org/a2a-extension/a2ui/v0.9.1"
	a2uiMime    = "application/a2ui+json"
	a2uiCatalog = "https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json"
	a2uiAskForm = "Fill in the booking form: a name, the number of guests and a date and time."
	a2uiBooked  = `Booked a table for 4 under "Robin" on Monday, 9 November 2026 at 19:30.`
	a2uiAskData = `To book a table, reply with a data part {"name": "…", "guests": 1 to 6, "date": "2026-11-09T19:30:00Z"} ` +
		`(the booking), or ask again from a client that renders A2UI v0.9.1.`
)

// a2uiFixture is the example seller's testdata/<name>, which make check-specs validates against the vendored A2UI
// v0.9.1 schemas, as a JSON value.
func a2uiFixture(t *testing.T, name string) any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "sellers", "a2ui", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// jsonValue is v as encoding/json reads it back: two values are the same JSON when their jsonValues are DeepEqual.
func jsonValue(t *testing.T, v any) any {
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

// jcsOf is raw under RFC 8785 (JCS).
func jcsOf(t *testing.T, raw []byte) string {
	t.Helper()
	out, err := jcs.Transform(raw)
	if err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return string(out)
}

// a2uiParts splits a message's parts: its text, and its parts marked as A2UI by metadata.mimeType.
func a2uiParts(m *a2a.Message) (string, []*a2a.Part) {
	if m == nil {
		return "", nil
	}
	var text strings.Builder
	var ui []*a2a.Part
	for _, p := range m.Parts {
		text.WriteString(p.Text())
		if mt, _ := p.Metadata["mimeType"].(string); mt == a2uiMime {
			ui = append(ui, p)
		}
	}
	return text.String(), ui
}

// a2uiAsFixture is a part's A2UI messages as the example's fixtures write them, and the surface they are on: each
// message's version, which must be version, as v0.9.1, and its surfaceId, the seller's "booking-" and its own task
// id, which must be the same in every message (and id, unless id is empty), as "booking-{taskId}".
func a2uiAsFixture(t *testing.T, where string, p *a2a.Part, id, version string) (any, string) {
	t.Helper()
	msgs, ok := jsonValue(t, p.Data()).([]any)
	if !ok {
		t.Fatalf("%s: not a list of A2UI messages: %v", where, p.Data())
	}
	for _, msg := range msgs {
		m, _ := msg.(map[string]any)
		if m["version"] != version {
			t.Fatalf("%s: an A2UI message's version is %v, want %s", where, m["version"], version)
		}
		m["version"] = "v0.9.1"
		for _, body := range m {
			b, ok := body.(map[string]any)
			if !ok || b["surfaceId"] == nil {
				continue
			}
			sid, _ := b["surfaceId"].(string)
			if id == "" {
				id = sid
			}
			if sid != id || !strings.HasPrefix(sid, "booking-") || len(sid) == len("booking-") {
				t.Fatalf("%s: surfaceId %v, want the task's own (booking-<task id>) in every message, %q", where, b["surfaceId"], id)
			}
			b["surfaceId"] = "booking-{taskId}"
		}
	}
	return msgs, id
}

// checkA2UIPart fails unless p, as a2uiAsFixture reads it, is the example's testdata/<name>, marked
// application/a2ui+json by metadata.mimeType alone and by mediaType. It returns the surface's id.
func checkA2UIPart(t *testing.T, where string, p *a2a.Part, name, id, version string) string {
	t.Helper()
	got, id := a2uiAsFixture(t, where, p, id, version)
	if !reflect.DeepEqual(got, jsonValue(t, a2uiFixture(t, name))) || !reflect.DeepEqual(p.Metadata, map[string]any{"mimeType": a2uiMime}) || p.MediaType != a2uiMime {
		raw, _ := json.Marshal(p)
		t.Fatalf("%s: the A2UI part %s is not the example's %s marked %s", where, raw, name, a2uiMime)
	}
	return id
}

// checkForm fails unless m is the seller's question with the form: its text, and one part holding the example's
// testdata/messages/surface.json in version, on a surface of the task's own. It returns the surface's id.
func checkForm(t *testing.T, where string, m *a2a.Message, version string) string {
	t.Helper()
	text, ui := a2uiParts(m)
	if text != a2uiAskForm || len(ui) != 1 || len(m.Parts) != 2 {
		t.Fatalf("%s: %q with %d A2UI parts, want %q and the form", where, text, len(ui), a2uiAskForm)
	}
	return checkA2UIPart(t, where, ui[0], "messages/surface.json", "", version)
}

// checkBooked fails unless task is completed with the seller's summary and the deleteSurface of the form on the
// surface id, in version (the example's testdata/messages/delete.json).
func checkBooked(t *testing.T, where string, res a2a.SendMessageResult, id, version string) *a2a.Task {
	t.Helper()
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateCompleted {
		raw, _ := json.Marshal(res)
		t.Fatalf("%s: %s, want a completed task", where, raw)
	}
	text, ui := a2uiParts(task.Status.Message)
	if text != a2uiBooked || len(ui) != 1 || len(task.Status.Message.Parts) != 2 {
		t.Fatalf("%s: %q with %d A2UI parts, want %q and the form deleted", where, text, len(ui), a2uiBooked)
	}
	checkA2UIPart(t, where, ui[0], "messages/delete.json", id, version)
	return task
}

// actionPart is a renderer's A2UI action list (the example's testdata/messages/action.json) in a data part marked as
// A2UI.
func actionPart(t *testing.T) *a2a.Part {
	t.Helper()
	p := a2a.NewDataPart(a2uiFixture(t, "messages/action.json"))
	p.Metadata = map[string]any{"mimeType": a2uiMime}
	return p
}

// TestA2UIPassthrough is a2ui_passthrough (spec/relay.md §2.4, §16; D326, D328). Single tenant. The example A2UI
// seller (examples/sellers/a2ui: a2a-go v2.7.0, A2A 1.0, run unmodified) is hosted by a node whose tunnel is on
// replica B; the caller is on replica A unless a step says otherwise, and speaks A2A 1.0 only. Asserted:
//
//   - the card both replicas serve keeps the seller's capabilities.extensions (A2UI v0.9.1 with the basic catalog)
//     equal under JCS to the seller's own card;
//   - JSON-RPC SendMessage with A2A-Extensions naming A2UI v0.9.1 and no capabilities: the response's A2A-Extensions
//     echoes the URI, and the task waits at input-required with the form, so the activation reached the seller: the
//     A2UI part's data is the example's testdata/messages/surface.json in v0.9.1 on a surface of the task's own, which
//     make check-specs validates against the A2UI v0.9.1 schemas, marked application/a2ui+json (metadata.mimeType and
//     mediaType); GetTask on B shows it too;
//   - the form's action (testdata/messages/action.json) sent through B completes the same task with the seller's
//     summary of the choices and the form's deleteSurface (testdata/messages/delete.json);
//   - without the header or capabilities the seller asks in text alone; with a2uiClientCapabilities in the message
//     metadata alone (the fixture lists v0.9) it sends the form in v0.9, so the metadata reached it;
//   - over HTTP+JSON, SendStreamingMessage with the header streams the form, on a surface other than the first
//     task's, in the input-required status update and echoes the URI, and a2a-go's own REST client completes the
//     task with the action;
//   - /mcp hire activates no optional extension and carries no message metadata (D328's known limit): the seller asks
//     in text, structuredContent holds no data, and reply with a data part holding the booking completes it; a
//     part's own marks do reach the seller: reply with the form's action in a part marked application/a2ui+json
//     books another hire.
func TestA2UIPassthrough(t *testing.T) {
	h := start(t, Options{Tunnel: map[string]TunnelService{"a2ui": {}}})
	ctx := ctxFor(t, 6*time.Minute)
	a, b := h.Gates[0], h.Gates[1]
	seller, err := h.StartExampleSeller(ctx, "a2ui")
	if err != nil {
		t.Fatal(err)
	}
	hostOn(t, ctx, h, b, map[string]string{"a2ui": seller.URL})
	buyer := h.BuyerA()
	activate := http.Header{"A2A-Extensions": {a2uiExt}}
	call := Call{Service: "a2ui", Header: activate}

	// The card: the seller's extensions, as the seller wrote them.
	own, st := getBody(ctx, seller.URL+"/.well-known/agent-card.json", "")
	if st != http.StatusOK {
		t.Fatalf("the seller's own card: %d", st)
	}
	extensions := func(card []byte) string {
		var c struct {
			Capabilities struct {
				Extensions json.RawMessage `json:"extensions"`
			} `json:"capabilities"`
		}
		if err := json.Unmarshal(card, &c); err != nil || c.Capabilities.Extensions == nil {
			t.Fatalf("no capabilities.extensions in %s", card)
		}
		return jcsOf(t, c.Capabilities.Extensions)
	}
	want := extensions(own)
	if !strings.Contains(want, `"required":false`) || !strings.Contains(want, a2uiCatalog) {
		t.Fatalf("the seller's extensions %s", want)
	}
	for _, g := range h.Gates {
		if got := extensions(getCard(t, ctx, g.Svc("a2ui")+"/.well-known/agent-card.json", h.APIKey)); got != want {
			t.Fatalf("%s serves the extensions\n%s\nwant the seller's\n%s", g.Name, got, want)
		}
	}

	// JSON-RPC, the extension activated: the form; then the action through the other replica.
	res, err := buyer.RPCCall(ctx, a, "SendMessage", &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("a table, please"))}, call)
	if err != nil || res.Error != nil {
		t.Fatalf("SendMessage: %v %v", err, res.Error)
	}
	if got := res.Header.Values("A2A-Extensions"); len(got) != 1 || got[0] != a2uiExt {
		t.Errorf("the activation echoed: %q, want %s", got, a2uiExt)
	}
	var sr a2a.StreamResponse
	if err := json.Unmarshal(res.Result, &sr); err != nil {
		t.Fatal(err)
	}
	task, ok := sr.Event.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("SendMessage: %s, want a task at input-required", res.Result)
	}
	surface := checkForm(t, "SendMessage", task.Status.Message, "v0.9.1")
	got, err := buyer.Client(t, b, "a2ui").GetTask(ctx, &a2a.GetTaskRequest{ID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if id := checkForm(t, "GetTask on B", got.Status.Message, "v0.9.1"); id != surface {
		t.Fatalf("GetTask on B: the form on surface %s, SendMessage's on %s", id, surface)
	}
	reply := a2a.NewMessage(a2a.MessageRoleUser, actionPart(t))
	reply.TaskID, reply.ContextID = task.ID, task.ContextID
	done, err := buyer.Client(t, b, "a2ui").SendMessage(ctx, &a2a.SendMessageRequest{Message: reply})
	if err != nil {
		t.Fatal(err)
	}
	if dt := checkBooked(t, "the action", done, surface, "v0.9.1"); dt.ID != task.ID {
		t.Fatalf("the action completed task %s, want %s", dt.ID, task.ID)
	}
	if as, err := h.Attempts(ctx, task.ID); err != nil || len(as) != 1 {
		t.Fatalf("the task's attempts: %+v %v, want the one", as, err)
	}

	// What reached the seller: neither the header nor capabilities, then the capabilities alone.
	plain, err := buyer.Client(t, a, "a2ui").SendMessage(ctx, newMessage("a table, please"))
	if err != nil {
		t.Fatal(err)
	}
	if pt := plain.(*a2a.Task); pt.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("without A2UI: %s", pt.Status.State)
	} else if text, ui := a2uiParts(pt.Status.Message); text != a2uiAskData || len(ui) != 0 || len(pt.Status.Message.Parts) != 1 {
		t.Fatalf("without A2UI: %q with %d parts, want the question in text alone", text, len(pt.Status.Message.Parts))
	}
	withCaps := newMessage("a table, please")
	withCaps.Message.Metadata = map[string]any{"a2uiClientCapabilities": a2uiFixture(t, "messages/capabilities.json")}
	capsRes, err := buyer.Client(t, a, "a2ui").SendMessage(ctx, withCaps)
	if err != nil {
		t.Fatal(err)
	}
	checkForm(t, "capabilities alone", capsRes.(*a2a.Task).Status.Message, "v0.9")

	// HTTP+JSON: the form on a stream, then the action through a2a-go's REST client.
	stream, err := buyer.SendStreaming(ctx, a, newMessage("a table, please"), Call{Service: "a2ui", Binding: HTTPJSON, Header: activate})
	if err != nil {
		t.Fatal(err)
	}
	if got := stream.Header.Values("A2A-Extensions"); len(got) != 1 || got[0] != a2uiExt {
		t.Errorf("the stream's activation echoed: %q", got)
	}
	evs, err := stream.ReadAll(ctx)
	stream.Close()
	if err != nil || len(evs) == 0 || evs[len(evs)-1].State != a2a.TaskStateInputRequired {
		t.Fatalf("the stream: %d events, %v", len(evs), err)
	}
	var asked *a2a.Message
	switch e := evs[len(evs)-1].Event.(type) {
	case *a2a.TaskStatusUpdateEvent:
		asked = e.Status.Message
	case *a2a.Task:
		asked = e.Status.Message
	}
	streamedSurface := checkForm(t, "the stream's last event", asked, "v0.9.1")
	if streamedSurface == surface {
		t.Fatalf("two tasks' forms on one surface, %s: A2UI refuses the second", surface)
	}
	streamed := stream.TaskID()
	reply = a2a.NewMessage(a2a.MessageRoleUser, actionPart(t))
	reply.TaskID = streamed
	done, err = buyer.ClientREST(t, b, "a2ui").SendMessage(ctx, &a2a.SendMessageRequest{Message: reply})
	if err != nil {
		t.Fatal(err)
	}
	checkBooked(t, "the action over HTTP+JSON", done, streamedSurface, "v0.9.1")

	// /mcp hire: no activation and no capabilities reach the seller, which asks in text; reply with a data part
	// holding the booking books.
	s := OpenGateSession(t, ctx, a, h.APIKey, `{}`)
	hire := Tool(t, s.Tool("hire", `{"service":"a2ui","message":"a table, please"}`))
	if hire.IsError || hire.TaskID() == "" || !strings.Contains(hire.Text(), a2uiAskData) || hire.Structured == nil || len(hire.Structured.Data) != 0 {
		t.Fatalf("hire: %+v (%s)", hire, hire.StructuredRaw)
	}
	data, _ := json.Marshal(a2uiFixture(t, "booking.json"))
	booked := Tool(t, s.Tool("reply", `{"taskId":"`+hire.TaskID()+`","message":{"parts":[{"data":`+string(data)+`,"mediaType":"application/json"}]}}`))
	if booked.IsError || booked.State() != "TASK_STATE_COMPLETED" ||
		!strings.Contains(booked.Text(), `Booked a table for 4 under "Robin" on Monday, 9 November 2026 at 19:30 +01:00.`) {
		t.Fatalf("reply with the booking's data: %+v", booked)
	}
	// A part's metadata and mediaType do pass: the form's action, marked as A2UI in its part, books.
	again := Tool(t, s.Tool("hire", `{"service":"a2ui","message":"a table, please"}`))
	if again.IsError || again.TaskID() == "" || !strings.Contains(again.Text(), a2uiAskData) {
		t.Fatalf("the second hire: %+v", again)
	}
	action, _ := json.Marshal(a2uiFixture(t, "messages/action.json"))
	acted := Tool(t, s.Tool("reply", `{"taskId":"`+again.TaskID()+`","message":{"parts":[{"data":`+string(action)+`,"metadata":{"mimeType":"`+a2uiMime+`"}}]}}`))
	if acted.IsError || acted.State() != "TASK_STATE_COMPLETED" || !strings.Contains(acted.Text(), a2uiBooked) {
		t.Fatalf("reply with the form's action marked as A2UI: %+v", acted)
	}
}
