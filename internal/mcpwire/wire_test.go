package mcpwire

import (
	"encoding/json"
	"errors"
	"fmt"
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
	// v is {"params": …} or {"result": …} or {"error": …}: merge it into a message of a valid shape (a request
	// for params, a response for result and error).
	body := strings.TrimSuffix(v, "}")
	method := ""
	if out[0] == "params" {
		method = `"method":"m",`
	}
	return `{"jsonrpc":"2.0","id":7,` + method + strings.TrimPrefix(body, "{") + `}`
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
	for _, in := range []string{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, `[]`, `1`, `"x"`, `null`, ` true `} {
		m, err := Parse([]byte(in), FromClient)
		if !errors.Is(err, ErrBatch) || errors.Is(err, ErrParse) || m != nil {
			t.Errorf("Parse(%q) = %v, %v; want ErrBatch and no message", in, m, err)
		}
	}
	// Bytes that are not one JSON value are a parse error (-32700), not a batch.
	for _, in := range []string{``, ` `, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"} {"jsonrpc":"2.0","id":2,"method":"ping"}`, `{"jsonrpc":"2.0"}x`,
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}`, `{"jsonrpc":"2.0","id":1,"method":"ping",}`} {
		for _, src := range []Source{FromClient, FromUpstream} {
			m, err := Parse([]byte(in), src)
			if !errors.Is(err, ErrParse) || errors.Is(err, ErrBatch) || m != nil {
				t.Errorf("Parse(%q, %d) = %v, %v; want ErrParse and no message", in, src, m, err)
			}
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

// TestParseRefusesWhatIsNotAMessage: Parse accepts exactly a request (a string method and a string or number
// id), a notification (a string method and no id) or a response (an id, no method, and exactly one of result or
// a non-null error), with jsonrpc "2.0" and params, when present, an object. Anything else is ErrInvalid, with
// the id when the message object was read unambiguously.
func TestParseRefusesWhatIsNotAMessage(t *testing.T) {
	for _, c := range []struct{ name, body, id string }{
		{"no method, no result, no error", `{"jsonrpc":"2.0","id":1,"params":{"name":"x"}}`, "1"},
		{"nothing but jsonrpc", `{"jsonrpc":"2.0"}`, ""},
		{"empty object", `{}`, ""},
		{"method and result", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"},"result":{}}`, "1"},
		{"method and null result", `{"jsonrpc":"2.0","id":1,"method":"tools/call","result":null}`, "1"},
		{"method and error", `{"jsonrpc":"2.0","id":1,"method":"ping","error":{"code":1,"message":"m"}}`, "1"},
		{"notification with result", `{"jsonrpc":"2.0","method":"notifications/x","result":{}}`, ""},
		{"result and error", `{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":"m"}}`, "1"},
		{"null error alone", `{"jsonrpc":"2.0","id":1,"error":null}`, "1"},
		{"result without id", `{"jsonrpc":"2.0","result":{}}`, ""},
		{"error without id", `{"jsonrpc":"2.0","error":{"code":1,"message":"m"}}`, ""},
		{"request with a null id", `{"jsonrpc":"2.0","id":null,"method":"tools/call","params":{"name":"x"}}`, "null"},
		{"result with a null id", `{"jsonrpc":"2.0","id":null,"result":{}}`, "null"},
		{"params a string", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"x"}`, "1"},
		{"params an array", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":["x"]}`, "1"},
		{"params a number", `{"jsonrpc":"2.0","method":"notifications/x","params":1}`, ""},
		{"params null", `{"jsonrpc":"2.0","id":1,"method":"ping","params":null}`, "1"},
		{"no jsonrpc", `{"id":1,"method":"ping"}`, "1"},
		{"jsonrpc 1.0", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, "1"},
		{"jsonrpc 2", `{"jsonrpc":"2","id":1,"method":"ping"}`, "1"},
		{"jsonrpc with space", `{"jsonrpc":"2.0 ","id":1,"method":"ping"}`, "1"},
		{"empty method", `{"jsonrpc":"2.0","id":1,"method":""}`, "1"},
		{"empty method in a notification", `{"jsonrpc":"2.0","method":"","params":{}}`, ""},
	} {
		for _, src := range []Source{FromClient, FromUpstream} {
			m, err := Parse([]byte(c.body), src)
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("%s (source %d): Parse(%s) = %v, want ErrInvalid", c.name, src, c.body, err)
				continue
			}
			got := ""
			if m != nil {
				got = string(m.ID)
			}
			if got != c.id {
				t.Errorf("%s (source %d): the refusal's id is %q, want %q", c.name, src, got, c.id)
			}
		}
	}

	for _, c := range []struct {
		body   string
		method string
		resp   bool
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`, "tools/call", false},
		{`{"jsonrpc":"2.0","id":"a","method":"ping"}`, "ping", false},
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, "notifications/initialized", false},
		{`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":1,"progress":1}}`, "notifications/progress", false},
		{`{"jsonrpc":"2.0","id":1,"result":{}}`, "", true},
		{`{"jsonrpc":"2.0","id":1,"result":null}`, "", true},
		{`{"jsonrpc":"2.0","id":1,"result":{},"error":null}`, "", true},
		{`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`, "", true},
		{`{"jsonrpc":"2.0","id":"s1","error":{"code":-1,"message":"m","data":null}}`, "", true},
	} {
		for _, src := range []Source{FromClient, FromUpstream} {
			m, err := Parse([]byte(c.body), src)
			if err != nil {
				t.Errorf("source %d: Parse(%s) = %v, want nil", src, c.body, err)
				continue
			}
			if m.Method != c.method || m.JSONRPC != "2.0" || (m.Result == nil && m.Error == nil) != !c.resp {
				t.Errorf("source %d: Parse(%s) = %+v", src, c.body, m)
			}
		}
	}
}

// TestParseLoneCaseVariants: a key that a decoder matching keys case-insensitively (encoding/json into a struct,
// as mark3labs/mcp-go reads a message) takes for a member the relay reads, while the relay reads that member as
// absent, is ErrAmbiguous. The review's probes: each of them was accepted, and an encoding/json peer read
// tools/call, the denied tool's name, or isError true.
func TestParseLoneCaseVariants(t *testing.T) {
	for _, c := range []struct {
		name, body string
		src        Source
		of         string
		top        bool
	}{
		{"METHOD", `{"jsonrpc":"2.0","id":1,"METHOD":"tools/call","params":{"name":"x"}}`, FromClient, "method", true},
		{"METHOD with a null result", `{"jsonrpc":"2.0","id":1,"METHOD":"tools/call","params":{"name":"x"},"result":null}`, FromClient, "method", true},
		{"ID", `{"jsonrpc":"2.0","ID":1,"method":"tools/call","params":{"name":"denied"}}`, FromClient, "id", true},
		{"PARAMS", `{"jsonrpc":"2.0","id":1,"method":"tools/call","PARAMS":{"name":"denied"}}`, FromClient, "params", true},
		{"JSONRPC", `{"JSONRPC":"2.0","id":1,"method":"ping"}`, FromClient, "jsonrpc", true},
		{"Result", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"},"Result":{}}`, FromClient, "result", true},
		{"ERROR", `{"jsonrpc":"2.0","id":1,"result":{},"ERROR":{"code":1,"message":"m"}}`, FromUpstream, "error", true},
		{"NAME", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"NAME":"denied","arguments":{}}}`, FromClient, "name", false},
		{"Kelvin task", "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"x\",\"tas\u212a\":{\"ttl\":1}}}", FromClient, "task", false},
		{"Kelvin task escaped", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","tas\u212a":{"ttl":1}}}`, FromClient, "task", false},
		{"URI", `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"URI":"file:///etc/passwd"}}`, FromClient, "uri", false},
		{"TASKID", `{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"TASKID":"t"}}`, FromClient, "taskId", false},
		{"_META", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_META":{}}}`, FromClient, "_meta", false},
		{"ARGUMENTS", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","ARGUMENTS":{"a":1}}}`, FromClient, "arguments", false},
		{"modern protocolVersion", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"IO.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`, FromClient, MetaProtocolVersion, false},
		{"ISERROR", `{"jsonrpc":"2.0","id":1,"result":{"content":[],"ISERROR":true}}`, FromUpstream, "isError", false},
		{"terminal ISERROR", `{"jsonrpc":"2.0","id":1,"result":{"taskId":"t","status":"completed","result":{"content":[],"IsError":true}}}`, FromUpstream, "isError", false},
		{"tool NAME", `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"echo"},{"NAME":"rm","inputSchema":{}}]}}`, FromUpstream, "name", false},
		{"tool EXECUTION", `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"echo","Execution":{"taskSupport":"required"}}]}}`, FromUpstream, "execution", false},
		{"TASK", `{"jsonrpc":"2.0","id":1,"result":{"TASK":{"taskId":"t","status":"working"}}}`, FromUpstream, "task", false},
		{"TOOLS", `{"jsonrpc":"2.0","id":1,"result":{"Tools":[{"name":"rm"}]}}`, FromUpstream, "tools", false},
		{"related-task", `{"jsonrpc":"2.0","id":1,"method":"elicitation/create","params":{"message":"m","_meta":{"io.modelcontextprotocol/RELATED-TASK":{"taskId":"t"}}}}`, FromUpstream, MetaRelatedTask, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, err := Parse([]byte(c.body), c.src)
			if !errors.Is(err, ErrAmbiguous) {
				t.Fatalf("Parse = %v, %v; want ErrAmbiguous", m, err)
			}
			var ke *strictjson.KeyError
			if !errors.As(err, &ke) || ke.Of != c.of {
				t.Fatalf("%v: want a KeyError naming %q", err, c.of)
			}
			if c.top && m != nil {
				t.Fatalf("a message ambiguous at the top returns %+v", m)
			}
			if !c.top && (m == nil || string(m.ID) != "1") {
				t.Fatalf("the id read unambiguously is lost: %+v", m)
			}
		})
	}
	// A case variant of a name the relay does not read at that level, or of one it reads elsewhere, is the
	// parties' business.
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"NAME":"x","Uri":"y","TaskId":1}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","Message":"m","_meta":{"NAME":1,"progressToken":1}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"x/custom","params":{"Value":1,"LIST":[1]}}`,
	} {
		if _, err := Parse([]byte(body), FromClient); err != nil {
			t.Errorf("Parse(%s) = %v, want nil", body, err)
		}
	}
	if _, err := Parse([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","TEXT":"t","ISERROR":true}],"isError":false}}`), FromUpstream); err != nil {
		t.Errorf("content items are not a level: %v", err)
	}
}

// The members of a Task the dispatch reads at result level (a tasks/get or tasks/cancel answer) are checked where it
// reads them (CheckTask), not in every upstream message: a plain answer of any service with a lone case variant of
// status, taskId, statusMessage or pollInterval in its result is relayed as §11 says unknown members are, while a
// task answer with one is refused like a pair.
func TestCheckTask(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"result":{"content":[],"Status":"ok"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"contents":[],"TaskId":"x"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"messages":[],"pollinterval":5}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"content":[],"StatusMessage":"m"}}`,
	} {
		if _, err := Parse([]byte(body), FromUpstream); err != nil {
			t.Errorf("a plain upstream answer %s is refused: %v", body, err)
		}
	}
	for _, result := range []string{
		`{"taskId":"t","Status":"completed"}`, `{"TASKID":"t","status":"working"}`,
		`{"taskId":"t","status":"working","statusmessage":"m"}`, `{"taskId":"t","status":"working","PollInterval":5}`,
		"{\"taskId\":\"t\",\"\u017Ftatus\":\"completed\"}",
	} {
		if err := CheckTask(json.RawMessage(result)); !errors.Is(err, ErrAmbiguous) {
			t.Errorf("CheckTask(%s) = %v, want ErrAmbiguous", result, err)
		}
	}
	for _, result := range []string{`{"taskId":"t","status":"working","statusMessage":"m","pollInterval":5,"Other":1}`, `null`, `[]`} {
		if err := CheckTask(json.RawMessage(result)); err != nil {
			t.Errorf("CheckTask(%s) = %v, want nil", result, err)
		}
	}
	// A pair is Parse's to refuse, at result level as at every level it decodes.
	if _, err := Parse([]byte(`{"jsonrpc":"2.0","id":1,"result":{"status":"working","STATUS":"completed"}}`), FromUpstream); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("a pair at result level: %v", err)
	}
}

// TestReadsAreAtLevels: every member clientReads and upstreamReads name lies at the message object or at a level of
// the spec's lists, and every level is read at its parent, so the lone-variant rule never widens the levels.
func TestReadsAreAtLevels(t *testing.T) {
	for _, c := range []struct {
		levels []string
		reads  []strictjson.Path
	}{{specClientLevels, clientReads}, {specUpstreamLevels, upstreamReads}} {
		got := map[string]bool{}
		for _, r := range c.reads {
			at := strings.Join(r[:len(r)-1], ".")
			if at != "" && !slices.Contains(c.levels, at) {
				t.Errorf("read %v is at %q, which is not a level", r, at)
			}
			got[strings.Join(r, ".")] = true
		}
		for _, lvl := range c.levels {
			if !got[strings.TrimSuffix(lvl, ".*")] {
				t.Errorf("level %s is not read at its parent", lvl)
			}
		}
		for _, top := range []string{"jsonrpc", "id", "method", "params", "result", "error"} {
			if !got[top] {
				t.Errorf("the message object's %s is not read", top)
			}
		}
	}
}

// FuzzParseAgreesWithStructDecoding: whatever Parse accepts, a peer that decodes it with encoding/json into
// structs (matching names case-insensitively and keeping the last of a repeated one, as mark3labs/mcp-go does)
// reads the same method, the same presence of an id, and the same params.name, params.uri, params.taskId and
// result.isError as the relay's exact reading.
func FuzzParseAgreesWithStructDecoding(f *testing.F) {
	for _, s := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":1,"METHOD":"tools/call","params":{"name":"x"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"NAME":"denied","arguments":{}}}`,
		`{"jsonrpc":"2.0","ID":1,"method":"tools/call","params":{"name":"denied"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"content":[],"ISERROR":true}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"content":[],"isError":true,"ISERROR":false}}`,
		`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"Uri":"file:///etc/passwd"}}`,
		`{"jsonrpc":"2.0","id":"a","method":"tasks/get","params":{"taskId":"t","TaskID":"u"}}`,
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"x\",\"tasK\":{}}}",
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"m"}}`,
		`{"jsonrpc":"2.0","00":"","method":""}`,
	} {
		f.Add([]byte(s))
	}
	type params struct {
		Name   *string `json:"name"`
		URI    *string `json:"uri"`
		TaskID *string `json:"taskId"`
	}
	type result struct {
		IsError *bool `json:"isError"`
	}
	str := func(p *string) string {
		if p == nil {
			return "<absent>"
		}
		return *p
	}
	exact := func(obj json.RawMessage, key string) string {
		if obj == nil {
			return "<absent>"
		}
		m, err := strictjson.Members(obj)
		if err != nil {
			return "<absent>"
		}
		v, ok := m[key]
		if !ok {
			return "<absent>"
		}
		var s any
		if json.Unmarshal(v, &s) != nil {
			return "<bad>"
		}
		return fmt.Sprint(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, src := range []Source{FromClient, FromUpstream} {
			m, err := Parse(b, src)
			if err != nil {
				continue
			}
			var loose struct {
				Method *string         `json:"method"`
				ID     json.RawMessage `json:"id"`
				Params *params         `json:"params"`
				Result *result         `json:"result"`
			}
			if json.Unmarshal(b, &loose) != nil {
				continue // the peer refuses it too
			}
			if got := str(loose.Method); (m.Method == "" && loose.Method != nil) || (m.Method != "" && got != m.Method) {
				t.Fatalf("source %d, %q: the relay reads method %q, encoding/json %q", src, b, m.Method, got)
			}
			if (len(m.ID) == 0) != (len(loose.ID) == 0) {
				t.Fatalf("source %d, %q: the relay reads id %q, encoding/json %q", src, b, m.ID, loose.ID)
			}
			var lp params
			if loose.Params != nil {
				lp = *loose.Params
			}
			for _, c := range []struct {
				key   string
				loose *string
			}{{"name", lp.Name}, {"uri", lp.URI}, {"taskId", lp.TaskID}} {
				if e := exact(m.Params, c.key); c.loose != nil && e != *c.loose || c.loose == nil && e != "<absent>" && e != "<nil>" {
					t.Fatalf("source %d, %q: the relay reads params.%s %q, encoding/json %q", src, b, c.key, e, str(c.loose))
				}
			}
			if src == FromUpstream && loose.Result != nil {
				e := exact(m.Result, "isError")
				l := "<absent>"
				if loose.Result.IsError != nil {
					l = fmt.Sprint(*loose.Result.IsError)
				}
				if e != l && !(e == "<nil>" && l == "<absent>") {
					t.Fatalf("%q: the relay reads isError %s, encoding/json %s", b, e, l)
				}
			}
		}
	})
}

// IDKey: equal ids share a key (1 and 1.0 as JSON.parse reads them), a string and a number never do, and null or
// an object has none (M3 WP-05: the node's filters key requests and progress tokens by it).
func TestIDKey(t *testing.T) {
	key := func(s string) string {
		k, ok := IDKey(json.RawMessage(s))
		if !ok {
			return "<none>"
		}
		return k
	}
	for _, c := range [][2]string{{`1`, `1.0`}, {`1`, ` 1 `}, {`1e2`, `100`}, {`"a"`, `"a"`}, {`-0.5`, `-5e-1`}} {
		if key(c[0]) != key(c[1]) || key(c[0]) == "<none>" {
			t.Errorf("IDKey(%s) = %s, IDKey(%s) = %s: want one key", c[0], key(c[0]), c[1], key(c[1]))
		}
	}
	for _, c := range [][2]string{{`1`, `"1"`}, {`"a"`, `"A"`}, {`1`, `2`}} {
		if key(c[0]) == key(c[1]) {
			t.Errorf("IDKey(%s) = IDKey(%s) = %s: want two keys", c[0], c[1], key(c[0]))
		}
	}
	for _, s := range []string{`null`, `{}`, `[1]`, `true`, ``, `"unterminated`} {
		if k, ok := IDKey(json.RawMessage(s)); ok {
			t.Errorf("IDKey(%s) = %q, want none", s, k)
		}
	}
}

// The members the node's MCP filters read (M3 WP-05): a lone case variant of roots in an initialize's capabilities,
// in a request's clientCapabilities or in a client response's result, or of progressToken in a client's _meta or a server's progress notification,
// is refused like a pair, since a peer decoding with encoding/json would take it for the member the node removed or
// routed by.
func TestNodeFilterReads(t *testing.T) {
	for _, c := range []struct {
		src  Source
		body string
	}{
		{FromClient, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"Roots":{}}}}`},
		{FromClient, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","_meta":{"` + MetaClientCapabilities + `":{"ROOTS":{}}}}}`},
		{FromClient, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"x","_meta":{"ProgressToken":"t"}}}`},
		{FromUpstream, `{"jsonrpc":"2.0","method":"notifications/progress","params":{"PROGRESSTOKEN":"t","progress":1}}`},
		{FromClient, `{"jsonrpc":"2.0","id":"s1","result":{"Roots":[{"uri":"file:///"}]}}`},
	} {
		if _, err := Parse([]byte(c.body), c.src); !errors.Is(err, ErrAmbiguous) {
			t.Errorf("%s: %v, want ErrAmbiguous", c.body, err)
		}
	}
	for _, c := range []struct {
		src  Source
		body string
	}{
		{FromClient, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true}}}}`},
		{FromClient, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"x","_meta":{"progressToken":"t"}}}`},
		{FromUpstream, `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"t","progress":1}}`},
		{FromClient, `{"jsonrpc":"2.0","id":"s1","result":{"roots":[{"uri":"file:///"}]}}`},
	} {
		if _, err := Parse([]byte(c.body), c.src); err != nil {
			t.Errorf("%s: %v", c.body, err)
		}
	}
}
