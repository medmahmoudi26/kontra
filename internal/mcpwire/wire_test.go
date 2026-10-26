package mcpwire

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/strictjson"
)

func paths(ps []strictjson.Path) []string {
	var out []string
	for _, p := range ps {
		out = append(out, strings.Join(p, "."))
	}
	return out
}

// The levels of spec/relay.md §11.1 (and D172, strictjson's package doc), below the message object, as the spec
// words them. A level added to a reader is added to the spec first.
var (
	specClientLevels = []string{
		"params",                                 // params
		"params._meta",                           // params._meta
		"params._meta." + MetaClientCapabilities, // params._meta["io.modelcontextprotocol/clientCapabilities"]
		"params._meta." + MetaClientCapabilities + ".extensions", // … and its extensions
		"params.capabilities",   // params.capabilities (an initialize)
		"params.arguments",      // the top level of params.arguments
		"params.inputResponses", // and of params.inputResponses
		"result",                // result
		"result._meta",          // result._meta
	}
	specUpstreamLevels = []string{
		"params",                              // params (of a request or a notification)
		"params._meta",                        // params._meta
		"result",                              // result
		"result._meta",                        // result._meta
		"result.tools.*",                      // any of result.tools (tools/list)
		"result.tools.*._meta",                // or its _meta
		"result.capabilities",                 // result.capabilities (initialize, server/discover)
		"result.capabilities.experimental",    // or its experimental
		"result.capabilities.extensions",      // or extensions
		"result.task",                         // a 2025-11-25 CreateTaskResult's result.task
		"result.inputRequests",                // a 2026-07-28 task's or InputRequiredResult's inputRequests
		"result.inputRequests.*",              // any of its entries
		"result.inputRequests.*.params",       // their params
		"result.inputRequests.*.params._meta", // and params._meta
		"result.result",                       // a terminal task's result.result
		"result.error",                        // and result.error
		"error",                               // error
	}
)

func TestLevelsAreTheSpecs(t *testing.T) {
	if got := paths(ClientLevels); !slices.Equal(got, specClientLevels) {
		t.Errorf("ClientLevels = %v\nspec §11.1      = %v", got, specClientLevels)
	}
	if got := paths(UpstreamLevels); !slices.Equal(got, specUpstreamLevels) {
		t.Errorf("UpstreamLevels = %v\nspec §11.1        = %v", got, specUpstreamLevels)
	}
}

// probe builds a message whose level path holds {"k":1,"K":2} (a case variant) and nothing else odd.
func probe(path string, variant string) string {
	segs := strings.Split(path, ".")
	// Rejoin the clientCapabilities key, which holds dots.
	var out []string
	for i := 0; i < len(segs); i++ {
		if segs[i] == "io" && i+1 < len(segs) {
			out = append(out, MetaClientCapabilities)
			i += 1 // "io", "modelcontextprotocol/clientCapabilities"
			continue
		}
		out = append(out, segs[i])
	}
	v := variant
	for i := len(out) - 1; i >= 0; i-- {
		switch out[i] {
		case "*":
			if i > 0 && out[i-1] == "tools" {
				v = "[" + v + "]"
			} else {
				v = `{"r1":` + v + `}`
			}
		default:
			v = `{"` + out[i] + `":` + v + `}`
		}
	}
	// v is {"params": …} or {"result": …} or {"error": …}: merge it into a message.
	body := strings.TrimSuffix(v, "}")
	return `{"jsonrpc":"2.0","id":7,` + strings.TrimPrefix(body, "{") + `}`
}

// TestEveryLevelIsChecked sends, for every level of each list, a message with a case variant at exactly that
// level (ErrAmbiguous, the id kept) and one with the variant one level below it (accepted when that is not a
// level itself).
func TestEveryLevelIsChecked(t *testing.T) {
	for _, c := range []struct {
		src    Source
		levels []string
	}{{FromClient, specClientLevels}, {FromUpstream, specUpstreamLevels}} {
		for _, lvl := range c.levels {
			body := probe(lvl, `{"code":-1,"message":"m","k":1,"K":2}`)
			m, err := Parse([]byte(body), c.src)
			if !errors.Is(err, ErrAmbiguous) {
				t.Errorf("source %d, level %s: Parse(%s) = %v, want ErrAmbiguous", c.src, lvl, body, err)
				continue
			}
			var ke *strictjson.KeyError
			if !errors.As(err, &ke) || ke.Key != "K" {
				t.Errorf("level %s: %v does not wrap the KeyError", lvl, err)
			}
			if m == nil || string(m.ID) != "7" {
				t.Errorf("level %s: the id read unambiguously is lost (%v)", lvl, m)
			}
			below := lvl + ".x"
			if slices.Contains(c.levels, below) || strings.HasSuffix(lvl, "*") || lvl == "result.inputRequests" ||
				lvl == "result.tools.*" || lvl == "params._meta."+MetaClientCapabilities {
				continue
			}
			body = probe(below, `{"k":1,"K":2}`)
			if lvl == "error" {
				continue // the error object's members are read by Parse itself
			}
			if _, err := Parse([]byte(body), c.src); err != nil {
				t.Errorf("source %d, below %s: Parse(%s) = %v, want nil", c.src, lvl, body, err)
			}
		}
	}
}

func TestParseClientProbes(t *testing.T) {
	ambiguous := []struct{ name, body string }{
		{"method/METHOD", `{"jsonrpc":"2.0","id":1,"method":"tools/call","METHOD":"ping","params":{"name":"echo","arguments":{}}}`},
		{"name/NAME", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","NAME":"add","arguments":{}}}`},
		{"repeated name", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","name":"add","arguments":{}}}`},
		{"escaped repeated method", `{"jsonrpc":"2.0","id":1,"method":"tools/call","\u006dethod":"ping"}`},
		{"_meta variant", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":{"progressToken":1,"PROGRESSTOKEN":2}}}`},
		{"arguments top level", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"path":"/a","PATH":"/b"}}}`},
		{"initialize capabilities", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{},"Roots":{}},"clientInfo":{"name":"c","version":"1"}}}`},
		{"modern clientCapabilities", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"roots":{},"ROOTS":{}}}}}`},
		{"modern clientCapabilities.extensions", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{"extensions":{"io.modelcontextprotocol/tasks":{},"IO.MODELCONTEXTPROTOCOL/TASKS":{}}}}}}`},
		{"inputResponses top level", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","inputResponses":{"r1":{},"R1":{}}}}`},
		{"result._meta", `{"jsonrpc":"2.0","id":"s1","result":{"action":"accept","_meta":{"a":1,"A":1}}}`},
		{"result", `{"jsonrpc":"2.0","id":"s1","result":{"action":"accept","ACTION":"decline"}}`},
		{"top-level id", `{"jsonrpc":"2.0","id":1,"ID":2,"method":"ping"}`},
	}
	for _, c := range ambiguous {
		t.Run(c.name, func(t *testing.T) {
			m, err := Parse([]byte(c.body), FromClient)
			if !errors.Is(err, ErrAmbiguous) {
				t.Fatalf("Parse = %v, want ErrAmbiguous", err)
			}
			top := strings.Contains(c.name, "method/") || c.name == "escaped repeated method" || c.name == "top-level id"
			if top && m != nil {
				t.Fatalf("a message ambiguous at the top returns %+v; its id was not read unambiguously", m)
			}
			if !top && (m == nil || len(m.ID) == 0) {
				t.Fatalf("the id read unambiguously is lost")
			}
		})
	}

	// A case variant below the top level of arguments is the tool's business, not the relay's.
	m, err := Parse([]byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"x":{"a":1,"A":2},"list":[{"k":1,"K":1}]}}}`), FromClient)
	if err != nil {
		t.Fatalf("a variant inside arguments.x: %v", err)
	}
	if m.Method != "tools/call" || m.JSONRPC != "2.0" || string(m.ID) != "3" || !strings.Contains(string(m.Params), `"A":2`) {
		t.Fatalf("message = %+v", m)
	}
	// The same body from an upstream (a server-to-client request's params) is checked by UpstreamLevels: params
	// and params._meta, not arguments.
	if _, err := Parse([]byte(`{"jsonrpc":"2.0","id":3,"method":"x","params":{"arguments":{"a":1,"A":2}}}`), FromUpstream); err != nil {
		t.Fatalf("upstream params.arguments is not a level: %v", err)
	}
}

func TestParseUpstreamProbes(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"isError/ISERROR", `{"jsonrpc":"2.0","id":1,"result":{"content":[],"isError":true,"ISERROR":false}}`},
		{"tools name/NAME", `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"echo","inputSchema":{}},{"name":"add","NAME":"rm","inputSchema":{}}]}}`},
		{"tools _meta", `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"echo","_meta":{"sh.kontra/price":1,"SH.KONTRA/PRICE":0}}]}}`},
		{"capabilities.extensions", `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"extensions":{"io.modelcontextprotocol/tasks":{},"Io.modelcontextprotocol/tasks":{}}}}}`},
		{"capabilities.experimental", `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"experimental":{"sh.kontra/sealed":{},"sh.kontra/SEALED":{}}}}}`},
		{"result.task", `{"jsonrpc":"2.0","id":1,"result":{"task":{"taskId":"t1","TASKID":"t2","status":"working"}}}`},
		{"inputRequests entry params._meta", `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","inputRequests":{"r1":{"method":"elicitation/create","params":{"_meta":{"a":1,"A":2}}}}}}`},
		{"inputRequests entry", `{"jsonrpc":"2.0","id":1,"result":{"inputRequests":{"r1":{"method":"elicitation/create","METHOD":"roots/list"}}}}`},
		{"inputRequests keys", `{"jsonrpc":"2.0","id":1,"result":{"inputRequests":{"r1":{"method":"x"},"R1":{"method":"y"}}}}`},
		{"terminal result.result", `{"jsonrpc":"2.0","id":1,"result":{"taskId":"t","status":"completed","result":{"content":[],"isError":true,"ISERROR":false}}}`},
		{"terminal result.error", `{"jsonrpc":"2.0","id":1,"result":{"status":"failed","error":{"code":1,"CODE":2,"message":"m"}}}`},
		{"error", `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"Code":0,"message":"m"}}`},
		{"request params", `{"jsonrpc":"2.0","id":9,"method":"elicitation/create","params":{"message":"m","MESSAGE":"n"}}`},
		{"notification _meta", `{"jsonrpc":"2.0","method":"notifications/progress","params":{"_meta":{"x":1,"X":1}}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.body), FromUpstream); !errors.Is(err, ErrAmbiguous) {
				t.Fatalf("Parse = %v, want ErrAmbiguous", err)
			}
		})
	}
	// A plain upstream result reads isError's bytes as they are.
	m, err := Parse([]byte(`{"jsonrpc":"2.0","id":"a","result":{"content":[{"type":"text","text":"T","TEXT":"u"}],"isError":true}}`), FromUpstream)
	if err != nil {
		t.Fatalf("content items are not a level: %v", err)
	}
	var r struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(m.Result, &r); err != nil || !r.IsError || string(m.ID) != `"a"` {
		t.Fatalf("result = %s, %v", m.Result, err)
	}
}

func TestParseShapes(t *testing.T) {
	for _, in := range []string{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, `[]`, `1`, `"x"`, `null`, ``,
		`{"jsonrpc":"2.0","id":1,"method":"ping"} {"jsonrpc":"2.0","id":2,"method":"ping"}`, `{"jsonrpc":"2.0"}x`} {
		m, err := Parse([]byte(in), FromClient)
		if !errors.Is(err, ErrBatch) || m != nil {
			t.Errorf("Parse(%q) = %v, %v; want ErrBatch and no message", in, m, err)
		}
	}
	for _, in := range []string{
		`{"jsonrpc":2,"id":1,"method":"ping"}`,
		`{"jsonrpc":null,"id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":5}`,
		`{"jsonrpc":"2.0","id":1,"method":null}`,
		`{"jsonrpc":"2.0","id":{"a":1},"method":"ping"}`,
		`{"jsonrpc":"2.0","id":[1],"method":"ping"}`,
		`{"jsonrpc":"2.0","id":true,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"error":{"message":"m"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":1.5,"message":"m"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":"1","message":"m"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":1,"message":2}}`,
		`{"jsonrpc":"2.0","id":1,"error":"boom"}`,
	} {
		if _, err := Parse([]byte(in), FromUpstream); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%s) = %v, want ErrInvalid", in, err)
		}
	}
	if _, err := Parse([]byte(`{}`), Source(0)); err == nil {
		t.Error("an unknown source is accepted")
	}

	m, err := Parse([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"bad","data":{"x":[1]}},"extra":1}`), FromUpstream)
	if err != nil {
		t.Fatal(err)
	}
	if string(m.ID) != "null" || m.Error == nil || m.Error.Code != -32600 || m.Error.Message != "bad" || string(m.Error.Data) != `{"x":[1]}` {
		t.Fatalf("message = %+v, error %+v", m, m.Error)
	}
	m, err = Parse([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), FromClient)
	if err != nil || m.Method != "notifications/initialized" || m.ID != nil || m.Params != nil {
		t.Fatalf("notification = %+v, %v", m, err)
	}
	m, err = Parse([]byte(`{"jsonrpc":"2.0","id":1,"result":{},"error":null}`), FromUpstream)
	if err != nil || m.Error != nil || string(m.Result) != `{}` {
		t.Fatalf("a null error = %+v, %v", m, err)
	}
}

func TestSealedEnvelope(t *testing.T) {
	for _, c := range []struct {
		v    string
		want bool
	}{
		{`{"v":1,"envelope":"YWdlLWVuY3J5cHRpb24="}`, true},
		{` { "envelope" : "AAAA" , "v" : 1 } `, true},
		{`{"v":1,"envelope":"YWdlLWVuY3J5cHRpb24=","x":1}`, false},
		{`{"V":1,"envelope":"AAAA"}`, false},
		{`{"v":1,"V":1,"envelope":"AAAA"}`, false},
		{`{"v":1,"v":1,"envelope":"AAAA"}`, false},
		{`{"v":1,"Envelope":"AAAA"}`, false},
		{`{"v":2,"envelope":"AAAA"}`, false},
		{`{"v":1.0,"envelope":"AAAA"}`, false},
		{`{"v":"1","envelope":"AAAA"}`, false},
		{`{"v":1,"envelope":1234}`, false},
		{`{"v":1,"envelope":["AAAA"]}`, false},
		{`{"v":1,"envelope":"not base64!"}`, false},
		{`{"v":1,"envelope":"AAA"}`, false},
		{`{"v":1,"envelope":"AA\nAA"}`, false},
		{`{"v":1,"envelope":"_-_-"}`, false}, // base64url, not standard
		{`{"v":1,"envelope":""}`, false},
		{`{"v":1}`, false},
		{`[{"v":1,"envelope":"AAAA"}]`, false},
		{`null`, false},
		{``, false},
	} {
		if got := SealedEnvelope(json.RawMessage(c.v)); got != c.want {
			t.Errorf("SealedEnvelope(%s) = %v, want %v", c.v, got, c.want)
		}
		// internal/sealed applies the same rule when it opens one.
		_, err := sealed.ParseMCPEnvelope(json.RawMessage(c.v))
		if (err == nil) != c.want {
			t.Errorf("sealed.ParseMCPEnvelope(%s) = %v, SealedEnvelope %v", c.v, err, c.want)
		}
	}
	env, _ := json.Marshal(sealed.NewMCPEnvelope([]byte("age-encryption.org/v1\n...")))
	if !SealedEnvelope(env) {
		t.Fatalf("sealed.NewMCPEnvelope's JSON %s is refused", env)
	}
}

func TestErrorBody(t *testing.T) {
	for _, c := range []struct {
		id   json.RawMessage
		code int
		msg  string
		data any
		want string
	}{
		{nil, CodeInvalidRequest, "batch not supported", nil, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"batch not supported"}}`},
		{json.RawMessage(`7`), CodeInvalidParams, "Unknown tool: rm", nil, `{"jsonrpc":"2.0","id":7,"error":{"code":-32602,"message":"Unknown tool: rm"}}`},
		{json.RawMessage(`"k.1"`), CodeUnsupportedVersion, "unsupported", map[string]any{"supported": Supported}, `{"jsonrpc":"2.0","id":"k.1","error":{"code":-32022,"message":"unsupported","data":{"supported":["2026-07-28","2025-11-25","2025-06-18"]}}}`},
		{json.RawMessage(`null`), CodeInternal, "upstream error", json.RawMessage(`{"a":1}`), `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"upstream error","data":{"a":1}}}`},
	} {
		b, err := ErrorBody(c.id, c.code, c.msg, c.data)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("ErrorBody = %s\nwant %s", b, c.want)
		}
		m, err := Parse(b, FromUpstream)
		if err != nil || m.Error == nil || m.Error.Code != c.code || m.Error.Message != c.msg {
			t.Errorf("ErrorBody's output does not parse back: %+v, %v", m, err)
		}
	}
	if _, err := ErrorBody(json.RawMessage(`{"a":1}`), 1, "m", nil); err == nil {
		t.Error("an object id is written")
	}
}
