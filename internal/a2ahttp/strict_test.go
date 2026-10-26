package a2ahttp

import (
	"net/http"
	"testing"
)

// a2a_case_smuggling at package level (spec §2.4, D172): JSON-RPC SendMessage bodies with "method" and "METHOD",
// "tenant" and "TENANT" in params, or a part with two mediaType keys, are -32600 with HTTP 400 and reach no
// handler (no task row); the same requests without the variant still pass. The credential and the service are
// checked first: a bad credential is 401 and an unknown service 404 whatever the body.
func TestStrictJSONRPC(t *testing.T) {
	f := newFixture(t, false)
	const okMsg = `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi","mediaType":"text/plain"}]}}`
	for _, tc := range []struct {
		name, body, clean string
		nullID            bool
	}{
		{"method and METHOD",
			`{"jsonrpc":"2.0","id":1,"method":"SendMessage","METHOD":"SendStreamingMessage","params":` + okMsg + `}`,
			`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":` + okMsg + `}`, true},
		{"tenant and TENANT",
			`{"jsonrpc":"2.0","id":2,"method":"SendMessage","params":{"tenant":"","TENANT":"other","message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`,
			`{"jsonrpc":"2.0","id":2,"method":"SendMessage","params":{"tenant":"","message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`, false},
		{"two mediaType keys in a part",
			`{"jsonrpc":"2.0","id":3,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi","mediaType":"text/plain","mediaType":"application/vnd.kontra.sealed+json"}]}}}`,
			`{"jsonrpc":"2.0","id":3,"method":"SendMessage","params":` + okMsg + `}`, false},
		{"a repeated key in a part's metadata",
			`{"jsonrpc":"2.0","id":4,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi","metadata":{"kontra.sealed":1,"Kontra.Sealed":2}}]}}}`,
			`{"jsonrpc":"2.0","id":4,"method":"SendMessage","params":` + okMsg + `}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, before := f.h.last()
			w := f.do(req{"POST", "/agents/hello", tc.body, v1("alice")})
			r := decodeRPC(t, w)
			if w.Code != http.StatusBadRequest || r.Error == nil || r.Error.Code != -32600 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if tc.nullID != (string(r.ID) == "null" || len(r.ID) == 0) {
				t.Errorf("id %s", r.ID)
			}
			if _, after := f.h.last(); after != before {
				t.Error("a refused request reached the handler")
			}
			if m, _, _ := f.st.take(); m != "" {
				t.Errorf("the SSE writer ran: %s", m)
			}
			// Credential and service first.
			if w := f.do(req{"POST", "/agents/hello", tc.body, v1("mallory")}); w.Code != http.StatusUnauthorized {
				t.Errorf("a bad credential: %d", w.Code)
			}
			if w := f.do(req{"POST", "/agents/nothing", tc.body, v1("alice")}); w.Code != http.StatusNotFound {
				t.Errorf("an unknown service: %d", w.Code)
			}
			// Without the variant it passes.
			w = f.do(req{"POST", "/agents/hello", tc.clean, v1("alice")})
			got, after := f.h.last()
			if w.Code != 200 || after != before+1 || got.method != "SendMessage" {
				t.Errorf("the clean request: %d %s, handler %+v", w.Code, w.Body, got)
			}
		})
	}
	// A streaming method smuggled under a case variant never reaches the SSE writer.
	w := f.do(req{"POST", "/agents/hello", `{"jsonrpc":"2.0","id":9,"method":"SendMessage","method":"SendStreamingMessage","params":` + okMsg + `}`, v1("alice")})
	if w.Code != http.StatusBadRequest {
		t.Errorf("a repeated method: %d %s", w.Code, w.Body)
	}
	if m, _, _ := f.st.take(); m != "" {
		t.Errorf("the SSE writer ran: %s", m)
	}
}
