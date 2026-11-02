package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// A2UI's marks and messages, as the A2UI v0.9.1 extension specification writes them (examples/sellers/a2ui).
const (
	a2uiMime = "application/a2ui+json"
	a2uiExt  = "https://a2ui.org/a2a-extension/a2ui/v0.9.1"
)

var (
	a2uiAction  = `{"version":"v0.9.1","action":{"name":"book","surfaceId":"booking","sourceComponentId":"book","timestamp":"2026-11-02T18:02:11Z","context":{"name":"Robin","guests":["4"],"date":"2026-11-09T19:30"}}}`
	a2uiSurface = `{"version":"v0.9.1","createSurface":{"surfaceId":"booking","catalogId":"https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json"}}`
	a2uiModel   = `{"version":"v0.9.1","updateDataModel":{"surfaceId":"booking","path":"/booking","value":{"name":"","guests":["2"],"date":""}}}`
)

func decodeJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	return v
}

// agent03 is an A2A 0.3 agent written by hand, as a 0.3 SDK writes the wire (a2a-sdk < 0.4, the A2UI Python
// reference's): it records each message/send body and answers it with result, the JSON of a 0.3 Task.
type agent03 struct {
	mu     sync.Mutex
	bodies [][]byte
	result string
}

func (a *agent03) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(body, &req)
	a.mu.Lock()
	a.bodies = append(a.bodies, body)
	result := a.result
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(req.ID)+`,"result":`+result+`}`)
}

// task03 is a 0.3 task at input-required whose status message holds parts (0.3 JSON).
func task03(parts ...string) string {
	return `{"kind":"task","id":"up-1","contextId":"ctx-1","status":{"state":"input-required","message":{"kind":"message",` +
		`"messageId":"a-1","role":"agent","taskId":"up-1","contextId":"ctx-1","parts":[` + strings.Join(parts, ",") + `]}}}`
}

func dataPart03(data string, meta string) string {
	return `{"kind":"data","data":` + data + `,"metadata":` + meta + `}`
}

// A data part through a 0.3 upstream (spec/relay.md §6.3, D326). A2A 0.3's DataPart.data is an object (its JSON
// schema, `"type": "object"`): a2a-go's conversion, which the gate's upstream client is, carries one A2UI message per
// part as it is, both ways, metadata.mimeType included and nothing added, the shape the A2UI Python reference sends.
// Any other value (an A2UI list) goes to the agent as {"value": …} with metadata data_part_compat: true, the two SDKs'
// convention (a2a-go v2 and a2a-python 1.x read it back), and an agent's part in that form reaches the caller as the
// value. mediaType has no 0.3 field and is dropped. A 0.3 agent's data that is not an object fails the call: a2a-go
// cannot read the 0.3 message, a malformed body (§6.4).
func TestDataPartsOn03(t *testing.T) {
	agent := &agent03{}
	srv := httptest.NewServer(agent)
	defer srv.Close()
	c, err := NewClient(context.Background(), direct(t, srv.URL), Info{ProtocolVersion: "0.3", Binding: a2a.TransportProtocolJSONRPC,
		Path: "/", Extensions: []string{a2uiExt}}, []string{a2uiExt})
	if err != nil {
		t.Fatal(err)
	}
	mark := func(p *a2a.Part) *a2a.Part { p.Metadata = map[string]any{"mimeType": a2uiMime}; return p }
	send := func(parts ...*a2a.Part) (*a2a.Task, error) {
		res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, parts...)})
		if err != nil {
			return nil, err
		}
		task, ok := res.(*a2a.Task)
		if !ok {
			t.Fatalf("result %T", res)
		}
		return task, nil
	}

	// The caller's parts, on the 0.3 wire.
	one := mark(a2a.NewDataPart(decodeJSON(t, a2uiAction)))
	one.MediaType = a2uiMime
	list := mark(a2a.NewDataPart(decodeJSON(t, "["+a2uiAction+"]")))
	agent.result = task03(`{"kind":"text","text":"ok"}`)
	if _, err := send(one, list, a2a.NewTextPart("hi")); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Method string `json:"method"`
		Params struct {
			Message struct {
				Parts []any `json:"parts"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(agent.bodies[0], &sent); err != nil || sent.Method != "message/send" || len(sent.Params.Message.Parts) != 3 {
		t.Fatalf("the 0.3 request: %s %v", agent.bodies[0], err)
	}
	for i, want := range []string{
		dataPart03(a2uiAction, `{"mimeType":"`+a2uiMime+`"}`),
		dataPart03(`{"value":[`+a2uiAction+`]}`, `{"mimeType":"`+a2uiMime+`","data_part_compat":true}`),
		`{"kind":"text","text":"hi"}`,
	} {
		if got := sent.Params.Message.Parts[i]; !reflect.DeepEqual(got, decodeJSON(t, want)) {
			t.Errorf("part %d on the 0.3 wire: %v, want %s", i, got, want)
		}
	}

	// The agent's parts, as the caller's client reads them.
	agent.result = task03(`{"kind":"text","text":"Fill in the booking form."}`,
		dataPart03(a2uiSurface, `{"mimeType":"`+a2uiMime+`"}`),
		dataPart03(a2uiModel, `{"mimeType":"`+a2uiMime+`"}`),
		dataPart03(`{"value":[`+a2uiSurface+`,`+a2uiModel+`]}`, `{"mimeType":"`+a2uiMime+`","data_part_compat":true}`))
	task, err := send(a2a.NewTextPart("a table"))
	if err != nil {
		t.Fatal(err)
	}
	parts := task.Status.Message.Parts
	if len(parts) != 4 || parts[0].Text() != "Fill in the booking form." {
		t.Fatalf("the agent's parts: %+v", parts)
	}
	for i, want := range []string{a2uiSurface, a2uiModel, "[" + a2uiSurface + "," + a2uiModel + "]"} {
		p := parts[i+1]
		if !reflect.DeepEqual(p.Data(), decodeJSON(t, want)) || !reflect.DeepEqual(p.Metadata, map[string]any{"mimeType": a2uiMime}) || p.MediaType != "" {
			t.Errorf("the agent's part %d: data %v metadata %v mediaType %q, want %s marked %s alone", i+1, p.Data(), p.Metadata, p.MediaType, want, a2uiMime)
		}
	}

	// A list where 0.3 has an object: the call fails, and Classify says a malformed body (upstream_error).
	agent.result = task03(dataPart03("["+a2uiSurface+"]", `{"mimeType":"`+a2uiMime+`"}`))
	ctx, ex := WithExchange(context.Background())
	_, err = c.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("a table"))})
	if err == nil {
		t.Fatal("a 0.3 data part holding a list was read")
	}
	if f := Classify(ctx, err, ex); f.Class != Protocol {
		t.Errorf("a 0.3 data part holding a list: %+v, want a protocol failure", f)
	}
}
