package mcpwire

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const envelope = `{"v":1,"envelope":"AAEC"}`

// keys returns the _meta keys of the object at path in b, decoded loosely (the test's own read).
func metaKeys(t *testing.T, b []byte, path ...string) []string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	for _, p := range path {
		switch x := v.(type) {
		case map[string]any:
			v = x[p]
		case []any:
			var i int
			if _, err := json.Number(p).Int64(); err == nil {
				n, _ := json.Number(p).Int64()
				i = int(n)
			}
			v = x[i]
		}
	}
	m, _ := v.(map[string]any)
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func has(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

// mcp_meta_spoof (relay §2.5): an upstream's sh.kontra/ keys never reach a client from the protocol-level
// locations, case and long-s variants included, and content, structuredContent and arguments are never touched.
func TestScrubResultFromUpstream(t *testing.T) {
	spoof := `{"sh.kontra/price":"1 USD","sh.kontra/escrow":{"a":1},"sh.kontra/approve-url":"https://evil","SH.KONTRA/PRICE":2,"ſh.kontra/x":3,"io.example/keep":true}`
	res := `{"_meta":` + spoof + `,
		"content":[{"type":"text","text":"x","_meta":{"sh.kontra/price":"stays: content is never read"}}],
		"structuredContent":{"_meta":{"sh.kontra/price":"stays"}},
		"tools":[{"name":"echo","_meta":` + spoof + `}],
		"capabilities":{"experimental":{"sh.kontra/sealed":{"recipients":["evil"]},"other":{}},"extensions":{"sh.kontra/escrow":{},"io.x/y":{}}},
		"task":{"taskId":"t","_meta":` + spoof + `},
		"tasks":[{"taskId":"t","_meta":` + spoof + `}],
		"inputRequests":{"k1":{"method":"elicitation/create","params":{"_meta":` + spoof + `}}},
		"result":{"_meta":` + spoof + `}}`
	out, err := ScrubResult(FromUpstream, "", json.RawMessage(res), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{{"_meta"}, {"tools", "0", "_meta"}, {"task", "_meta"}, {"tasks", "0", "_meta"},
		{"inputRequests", "k1", "params", "_meta"}, {"result", "_meta"}} {
		ks := metaKeys(t, out, path...)
		for _, k := range ks {
			if IsKontraKey(k) {
				t.Errorf("%v kept %q: %s", path, k, out)
			}
		}
		if !has(ks, "io.example/keep") {
			t.Errorf("%v lost a key that is not Kontra's: %v", path, ks)
		}
	}
	if ks := metaKeys(t, out, "capabilities", "experimental"); has(ks, "sh.kontra/sealed") || !has(ks, "other") {
		t.Errorf("experimental: %v", ks)
	}
	if ks := metaKeys(t, out, "capabilities", "extensions"); has(ks, "sh.kontra/escrow") || !has(ks, "io.x/y") {
		t.Errorf("extensions: %v", ks)
	}
	for _, keep := range []string{`"text":"x","_meta":{"sh.kontra/price":"stays: content is never read"}`, `"structuredContent":{"_meta":{"sh.kontra/price":"stays"}}`} {
		if !bytes.Contains(out, []byte(keep)) {
			t.Errorf("content or structuredContent changed: %s", out)
		}
	}

	// Method-aware: a tools/call result's tools member (not a tools/list) is not a protocol location.
	out, err = ScrubResult(FromUpstream, MethodToolsCall, json.RawMessage(`{"tools":[{"_meta":{"sh.kontra/price":1}}],"_meta":{"sh.kontra/price":1}}`), false)
	if err != nil || !bytes.Contains(out, []byte(`"tools":[{"_meta":{"sh.kontra/price":1}}]`)) || has(metaKeys(t, out, "_meta"), "sh.kontra/price") {
		t.Errorf("tools/call: %s %v", out, err)
	}
}

// Nothing to remove: the same bytes come back, white space and order included (§11.2: unchanged otherwise).
func TestScrubKeepsBytesWhenNothingIsRemoved(t *testing.T) {
	in := json.RawMessage("{ \"content\" : [ ],\n \"_meta\": {\"io.x/a\": 1},  \"isError\": false }")
	out, err := ScrubResult(FromUpstream, MethodToolsCall, in, false)
	if err != nil || !bytes.Equal(out, in) {
		t.Fatalf("%q %v", out, err)
	}
	p := json.RawMessage(`{"name":"echo","arguments":{"sh.kontra/price":1},"_meta":{"progressToken":7}}`)
	out, err = ScrubParams(FromClient, MethodToolsCall, p, false)
	if err != nil || !bytes.Equal(out, p) {
		t.Fatalf("%q %v", out, err)
	}
	// A rewrite keeps the order and the other members' bytes.
	p = json.RawMessage(`{"name":"echo","arguments":{"b":1,"a":2},"_meta":{"z":1,"sh.kontra/price":"x","a":2}}`)
	out, err = ScrubParams(FromClient, MethodToolsCall, p, false)
	if err != nil || string(out) != `{"name":"echo","arguments":{"b":1,"a":2},"_meta":{"z":1,"a":2}}` {
		t.Fatalf("%s %v", out, err)
	}
}

// The sealed envelope (D174): kept exactly where ext-sealed §3 puts it, on a sealed service only, when it is
// exactly {v, envelope}; removed everywhere else.
func TestScrubSealedEnvelope(t *testing.T) {
	withEnv := func(v string) json.RawMessage {
		return json.RawMessage(`{"name":"echo","_meta":{"sh.kontra/sealed":` + v + `,"sh.kontra/price":1}}`)
	}
	kept := func(b []byte) bool { return bytes.Contains(b, []byte(`"sh.kontra/sealed"`)) }
	for _, tc := range []struct {
		name   string
		src    Source
		method string
		sealed bool
		v      string
		keep   bool
	}{
		{"client tools/call, sealed service", FromClient, MethodToolsCall, true, envelope, true},
		{"client tools/call, unsealed service", FromClient, MethodToolsCall, false, envelope, false},
		{"client tools/call, a third member", FromClient, MethodToolsCall, true, `{"v":1,"envelope":"AAEC","x":1}`, false},
		{"client tools/call, v 2", FromClient, MethodToolsCall, true, `{"v":2,"envelope":"AAEC"}`, false},
		{"client prompts/get", FromClient, "prompts/get", true, envelope, false},
		{"upstream elicitation/create, sealed", FromUpstream, MethodElicit, true, envelope, true},
		{"upstream sampling/createMessage, sealed", FromUpstream, MethodSample, true, envelope, true},
		{"upstream elicitation/create, unsealed", FromUpstream, MethodElicit, false, envelope, false},
		{"upstream tools/call (a request it never sends)", FromUpstream, MethodToolsCall, true, envelope, false},
		{"upstream notification", FromUpstream, "notifications/message", true, envelope, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ScrubParams(tc.src, tc.method, withEnv(tc.v), tc.sealed)
			if err != nil {
				t.Fatal(err)
			}
			if kept(out) != tc.keep {
				t.Errorf("kept = %v, want %v: %s", kept(out), tc.keep, out)
			}
			if bytes.Contains(out, []byte("sh.kontra/price")) {
				t.Errorf("another sh.kontra/ key stayed: %s", out)
			}
		})
	}
	// A case variant of the key is never the envelope.
	out, _ := ScrubParams(FromClient, MethodToolsCall, json.RawMessage(`{"_meta":{"SH.KONTRA/SEALED":`+envelope+`}}`), true)
	if bytes.Contains(bytes.ToLower(out), []byte("sh.kontra/sealed")) {
		t.Errorf("a case variant was kept: %s", out)
	}
	// Results: a client's answer to elicitation/create keeps it on a sealed service; an upstream's result never.
	res := json.RawMessage(`{"action":"accept","_meta":{"sh.kontra/sealed":` + envelope + `}}`)
	if out, _ := ScrubResult(FromClient, MethodElicit, res, true); !kept(out) {
		t.Errorf("client answer to elicitation/create: %s", out)
	}
	if out, _ := ScrubResult(FromClient, MethodElicit, res, false); kept(out) {
		t.Errorf("client answer, unsealed service: %s", out)
	}
	if out, _ := ScrubResult(FromClient, "ping", res, true); kept(out) {
		t.Errorf("client answer to ping: %s", out)
	}
	if out, _ := ScrubResult(FromUpstream, MethodToolsCall, res, true); kept(out) {
		t.Errorf("upstream result: %s", out)
	}
}

func TestScrubMessage(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":{"sh.kontra/price":1}}}`)
	m, err := Parse(body, FromClient)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ScrubMessage(FromClient, body, m, "", false)
	if err != nil || string(out) != `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":{}}}` {
		t.Fatalf("%s %v", out, err)
	}
	resp := []byte(`{"jsonrpc":"2.0","id":1,"result":{"_meta":{"sh.kontra/approve-url":"x"},"content":[]}}`)
	m, _ = Parse(resp, FromUpstream)
	if out, err = ScrubMessage(FromUpstream, resp, m, MethodToolsCall, false); err != nil || strings.Contains(string(out), "approve-url") {
		t.Fatalf("%s %v", out, err)
	}
	clean := []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"x"}}`)
	m, _ = Parse(clean, FromUpstream)
	if out, _ = ScrubMessage(FromUpstream, clean, m, "", false); !bytes.Equal(out, clean) {
		t.Fatalf("%s", out)
	}
}

func TestMergeMeta(t *testing.T) {
	out, err := MergeMeta([]byte(`{"name":"a","_meta":{"sh.kontra/price":"seller","k":1}}`),
		map[string]json.RawMessage{"sh.kontra/price": json.RawMessage(`"operator"`), "sh.kontra/z": json.RawMessage(`1`)})
	if err != nil || string(out) != `{"name":"a","_meta":{"k":1,"sh.kontra/price":"operator","sh.kontra/z":1}}` {
		t.Fatalf("%s %v", out, err)
	}
	out, err = MergeMeta([]byte(`{"name":"a"}`), map[string]json.RawMessage{"sh.kontra/price": json.RawMessage(`1`)})
	if err != nil || string(out) != `{"name":"a","_meta":{"sh.kontra/price":1}}` {
		t.Fatalf("%s %v", out, err)
	}
}

func TestIsKontraKey(t *testing.T) {
	for k, want := range map[string]bool{
		"sh.kontra/price": true, "SH.Kontra/x": true, "ſh.kontra/x": true, "sh.kontra/": true,
		"sh.kontra": false, "xsh.kontra/a": false, "io.modelcontextprotocol/related-task": false, "": false,
	} {
		if got := IsKontraKey(k); got != want {
			t.Errorf("IsKontraKey(%q) = %v", k, got)
		}
	}
}

// §11.12's rules 1, 3, 4 and 5 (the legacy era's), rule 2's classification, and an initialize without an id (D312).
func TestClassify(t *testing.T) {
	req := func(method, version, sid string) *http.Request {
		r := httptest.NewRequest(method, "/mcp/x", nil)
		if version != "" {
			r.Header.Set(HeaderProtocolVersion, version)
		}
		if sid != "" {
			r.Header.Set(HeaderSessionID, sid)
		}
		return r
	}
	msg := func(method string) *Message {
		return &Message{JSONRPC: "2.0", ID: json.RawMessage(`7`), Method: method}
	}
	for _, tc := range []struct {
		name   string
		r      *http.Request
		m      *Message
		era    Era
		status int
	}{
		{"initialize whatever the header", req("POST", V20260728, ""), msg("initialize"), EraLegacy, 0},
		{"modern", req("POST", V20260728, "ignored"), msg("tools/call"), EraModern, 0},
		{"legacy with a session", req("POST", V20251125, "s"), msg("tools/call"), EraLegacy, 0},
		{"absent header with a session", req("POST", "", "s"), msg("tools/call"), EraLegacy, 0},
		{"legacy without a session", req("POST", V20250618, ""), msg("tools/call"), 0, 400},
		{"another version", req("POST", "2025-03-26", "s"), msg("tools/call"), 0, 400},
		{"GET without a session", req("GET", "", ""), nil, 0, 405},
		{"DELETE without a session", req("DELETE", V20260728, ""), nil, 0, 405},
		{"GET with a session", req("GET", "", "s"), nil, EraLegacy, 0},
		// §11.1 holds for GET and DELETE on a session too: 2025-11-25, 2025-06-18 or none; another value is 400 with
		// -32022; 2026-07-28 is modern, has no GET stream and its Mcp-Session-Id is never read (rules 2 and 5): 405.
		{"GET with a session, 2025-11-25", req("GET", V20251125, "s"), nil, EraLegacy, 0},
		{"GET with a session, 2025-06-18", req("GET", V20250618, "s"), nil, EraLegacy, 0},
		{"DELETE with a session, 2025-11-25", req("DELETE", V20251125, "s"), nil, EraLegacy, 0},
		{"GET with a session, another version", req("GET", "1999-01-01", "s"), nil, 0, 400},
		{"DELETE with a session, another version", req("DELETE", "2025-03-26", "s"), nil, 0, 400},
		{"GET with a session, 2026-07-28", req("GET", V20260728, "s"), nil, 0, 405},
		{"DELETE with a session, 2026-07-28", req("DELETE", V20260728, "s"), nil, 0, 405},
		{"GET without a session, another version", req("GET", "1999-01-01", ""), nil, 0, 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			era, ref := Classify(tc.r, tc.m)
			if tc.status != 0 {
				if ref == nil || ref.Status != tc.status {
					t.Fatalf("refusal %+v", ref)
				}
				return
			}
			if ref != nil || era != tc.era {
				t.Fatalf("era %v refusal %+v", era, ref)
			}
		})
	}
	_, ref := Classify(req("POST", "", ""), msg("tools/list"))
	if string(ref.Body) != `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Bad Request: No valid session ID provided"}}` {
		t.Errorf("rule 3 body: %s", ref.Body)
	}
	// An initialize is a request: one without an id is §11.1's invalid request, 400 with -32600 and id null, whatever
	// its headers say (D312).
	for _, r := range []*http.Request{req("POST", "", ""), req("POST", V20251125, "s"), req("POST", V20260728, ""), req("POST", "1999-01-01", "s")} {
		if _, ref = Classify(r, &Message{JSONRPC: "2.0", Method: MethodInitialize}); ref == nil || ref.Status != 400 ||
			string(ref.Body) != `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request"}}` {
			t.Errorf("an initialize without an id, MCP-Protocol-Version %q: %+v", r.Header.Get(HeaderProtocolVersion), ref)
		}
	}
	_, ref = Classify(req("POST", "2024-11-05", "s"), msg("tools/list"))
	var e struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code int `json:"code"`
			Data struct {
				Supported []string `json:"supported"`
				Requested string   `json:"requested"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(ref.Body, &e); err != nil || e.Error.Code != CodeUnsupportedVersion || e.Error.Data.Requested != "2024-11-05" ||
		len(e.Error.Data.Supported) != 3 || string(e.ID) != "7" {
		t.Errorf("rule 4 body: %s", ref.Body)
	}
	for _, method := range []string{"GET", "DELETE"} {
		_, ref = Classify(req(method, "1999-01-01", "s"), nil)
		e.ID, e.Error.Code, e.Error.Data.Requested, e.Error.Data.Supported = nil, 0, "", nil
		if ref == nil || json.Unmarshal(ref.Body, &e) != nil || e.Error.Code != CodeUnsupportedVersion || e.Error.Data.Requested != "1999-01-01" ||
			len(e.Error.Data.Supported) != 3 || string(e.ID) != "null" {
			t.Errorf("%s: §11.1's -32022 body: %+v", method, ref)
		}
	}
	for _, method := range []string{"GET", "DELETE"} {
		if _, ref = Classify(req(method, V20260728, "s"), nil); ref == nil || len(ref.Body) != 0 {
			t.Errorf("%s 2026-07-28: %+v", method, ref)
		}
	}
	// A repeated MCP-Protocol-Version is read once, as its combined value (RFC 9110 §5.3), which is no version: rule 4
	// for a POST (initialize aside, rule 1), §11.1's -32022 for GET and DELETE on a session, 405 without one (D172).
	repeated := func(method, sid string, vs ...string) *http.Request {
		r := req(method, "", sid)
		for _, v := range vs {
			r.Header.Add(HeaderProtocolVersion, v)
		}
		return r
	}
	for _, vs := range [][]string{{V20260728, V20251125}, {V20251125, V20260728}, {V20260728, V20260728}, {V20251125, V20251125}} {
		for _, sid := range []string{"", "s"} {
			_, ref = Classify(repeated("POST", sid, vs...), msg("tools/call"))
			e.ID, e.Error.Code, e.Error.Data.Requested, e.Error.Data.Supported = nil, 0, "", nil
			if ref == nil || ref.Status != 400 || json.Unmarshal(ref.Body, &e) != nil || e.Error.Code != CodeUnsupportedVersion ||
				e.Error.Data.Requested != strings.Join(vs, ", ") || string(e.ID) != "7" {
				t.Errorf("POST with MCP-Protocol-Version %q, session %q: %+v", vs, sid, ref)
			}
		}
		if era, ref := Classify(repeated("POST", "", vs...), msg("initialize")); ref != nil || era != EraLegacy {
			t.Errorf("initialize with MCP-Protocol-Version %q: %v %+v", vs, era, ref)
		}
		if _, ref = Classify(repeated("GET", "s", vs...), nil); ref == nil || ref.Status != 400 {
			t.Errorf("GET on a session with MCP-Protocol-Version %q: %+v", vs, ref)
		}
		if _, ref = Classify(repeated("GET", "", vs...), nil); ref == nil || ref.Status != 405 {
			t.Errorf("GET without a session with MCP-Protocol-Version %q: %+v", vs, ref)
		}
	}
}
