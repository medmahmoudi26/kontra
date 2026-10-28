package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/oidc"
	"kontra.sh/kontra/internal/oidc/oidctest"
)

// startWith is gateEnv.start with more options (the authorization server's roots).
func (e *gateEnv) startWith(name, path string, timing Timing, opts ...Option) *running {
	e.t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		e.t.Fatal(err)
	}
	opts = append([]Option{WithLogger(testLog(e.t, name)), WithTiming(timing), WithoutSIGHUP()}, opts...)
	r, err := New(context.Background(), cfg, opts...)
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	run := &running{r: r, url: "http://" + r.Addr().String(), cancel: cancel, done: make(chan error, 1)}
	go func() { run.done <- r.Run(ctx) }()
	e.t.Cleanup(func() {
		cancel(errors.New("test over"))
		select {
		case <-run.done:
		case <-time.After(90 * time.Second):
			e.t.Errorf("replica %s did not stop", name)
		}
	})
	eventually(e.t, 20*time.Second, "replica "+name+" ready", func() bool { return r.Ready() })
	return run
}

// oauthConfig is replicaConfig with auth.mcp_oauth naming issuer.
func (e *gateEnv) oauthConfig(port int, issuer, services string) string {
	e.t.Helper()
	doc := fmt.Sprintf(`listen: 127.0.0.1:%d
public_url: http://127.0.0.1:%d
postgres: %s
temporal: { address: %s, namespace: kontra }
metrics: { listen: "127.0.0.1:0" }
auth: { api_keys: true, mcp_oauth: { issuer: %q } }
drain_timeout: 1m
%s`, port, port, e.dsn, e.temporal, issuer, services)
	path := filepath.Join(e.t.TempDir(), "kontra.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return path
}

// oauthSend is one request to the gate: its status, WWW-Authenticate and body.
func oauthSend(t *testing.T, method, url, bearer, body string) (int, string, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
	} else {
		req.Header.Set("Accept", "text/event-stream")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate"), b
}

const initialize = `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

// The MCP OAuth resource server through a whole replica (spec/relay.md §12.1, DM4-9, DM4-29; M4 WP-06), against the
// in-repository authorization server: a request without a credential to /mcp, to an existing /mcp/{svc} and to an
// unknown one is 401 with the exact challenge naming that route's RFC 9728 document (a malformed name, none), and a
// service whose allow: admits anonymous still serves it; each document's resource is the route's canonical URI and
// names the issuer; a token for /mcp is accepted there (the session and its hire task are via mcp, principal
// oidc:sub:<sub>, buyer UUIDv5(issuer ‖ sub)) and is 401 with error="invalid_token" on /mcp/{svc} and 401 on the A2A
// routes; a token for /mcp/{svc} is accepted there only; expired, foreign, none, HS256 and tampered tokens are 401; an
// API key still works on both.
func TestGateMCPOAuth(t *testing.T) {
	e := newGateEnv(t)
	as, err := oidctest.New(oidctest.Config{Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(as.Close)
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	direct := httptest.NewServer(up)
	t.Cleanup(direct.Close)
	services := `services:
  hi:
    kind: a2a
    backend: { a2a: { url: "` + e.seller.srv.URL + `/base", allow_private: true } }
    allow: ["org:default", "oidc:sub:alice"]
    task: { timeout: 10m, retries: 1 }
  fs:
    kind: mcp
    backend: { mcp: { url: "` + direct.URL + `/mcp", allow_private: true } }
    allow: ["org:default"]
  mine:
    kind: mcp
    backend: { mcp: { url: "` + direct.URL + `/mcp", allow_private: true } }
    allow: ["oidc:sub:alice"]
  open:
    kind: mcp
    backend: { mcp: { url: "` + direct.URL + `/mcp", allow_private: true } }
    allow: ["anonymous"]
`
	port := freePort(t)
	timing := fastTiming
	timing.PlainWait = 10 * time.Second
	a := e.startWith("a", e.oauthConfig(port, as.Issuer(), services), timing, WithOperatorRoots(as.CAPool()))
	pub := fmt.Sprintf("http://127.0.0.1:%d", port)
	challenge := func(svc string) string {
		route := "/mcp"
		if svc != "" {
			route += "/" + svc
		}
		return `Bearer resource_metadata="` + pub + `/.well-known/oauth-protected-resource` + route + `", scope="mcp"`
	}

	// No credential: the exact challenge on /mcp, an existing service and an unknown one, POST and GET.
	for _, svc := range []string{"", "fs", "mine", "nope"} {
		url := pub + "/mcp"
		if svc != "" {
			url += "/" + svc
		}
		for _, m := range []string{http.MethodPost, http.MethodGet} {
			status, www, body := oauthSend(t, m, url, "", initialize)
			if status != http.StatusUnauthorized || www != challenge(svc) {
				t.Errorf("%s %s without a credential: %d %q %s", m, url, status, www, body)
			}
			if m == http.MethodPost && !strings.Contains(string(body), `"code":-32001`) {
				t.Errorf("POST %s: body %s", url, body)
			}
		}
	}
	if status, www, _ := oauthSend(t, http.MethodPost, pub+"/mcp/Bad_Name", "", initialize); status != http.StatusUnauthorized || www != `Bearer scope="mcp"` {
		t.Errorf("a malformed name: %d %q", status, www)
	}
	// DM4-29: a service that admits anonymous serves a request without a credential.
	if status, _, body := oauthSend(t, http.MethodPost, pub+"/mcp/open", "", initialize); status != http.StatusOK || !strings.Contains(string(body), `"result"`) {
		t.Errorf("an anonymous service without a credential: %d %s", status, body)
	}

	// The RFC 9728 documents: for every well-formed name, existing or not.
	for _, svc := range []string{"", "fs", "nope"} {
		path := oidc.MetadataPrefix + "/mcp"
		if svc != "" {
			path += "/" + svc
		}
		status, _, body := oauthSend(t, http.MethodGet, pub+path, "", "")
		var doc oidc.ProtectedResource
		_ = json.Unmarshal(body, &doc)
		if status != http.StatusOK || doc.Resource != oidc.CanonicalURI(pub, svc) || len(doc.AuthorizationServers) != 1 ||
			doc.AuthorizationServers[0] != as.Issuer() {
			t.Errorf("%s: %d %s", path, status, body)
		}
	}
	if status, _, _ := oauthSend(t, http.MethodGet, pub+oidc.MetadataPrefix+"/mcp/Bad", "", ""); status != http.StatusNotFound {
		t.Errorf("a malformed name's document: %d", status)
	}

	// A token for /mcp: a session there, via mcp; its hire task too.
	mcpTok := as.MustSign(oidctest.Token{Audience: []string{oidc.CanonicalURI(pub, "")}})
	c := newMCPClient(t, a.url, mcpTok)
	c.initialize(`{}`)
	if n := scalar[int64](t, e.pool, `select count(*) from kontra.mcp_sessions where service is null and tenant_id is null
		and via = 'mcp' and principal = 'oidc:sub:alice'`); n != 1 {
		t.Fatalf("the /mcp session of the token: %d rows", n)
	}
	_, ans := c.call("hire", `{"service":"hi","message":"hi"}`)
	r := ans.tool(t)
	if r.IsError || r.Structured.TaskID == "" {
		t.Fatalf("a hire with the token: %s", ans.raw)
	}
	buyer, _ := ids.OIDCBuyer(as.Issuer(), "alice")
	if !scalar[bool](t, e.pool, `select via = 'mcp' and principal = 'oidc:sub:alice' and buyer_id = $2 and hired from kontra.tasks where id = $1`,
		r.Structured.TaskID, buyer) {
		t.Error("the hire task is not the token's (via mcp, oidc:sub:alice, UUIDv5(issuer ‖ sub))")
	}
	// The same token elsewhere: 401 with error="invalid_token" on /mcp/{svc}, 401 on the A2A routes.
	for _, svc := range []string{"mine", "fs", "nope"} {
		status, www, _ := oauthSend(t, http.MethodPost, pub+"/mcp/"+svc, mcpTok, initialize)
		if status != http.StatusUnauthorized || www != challenge(svc)+`, error="invalid_token"` {
			t.Errorf("a /mcp token on /mcp/%s: %d %q", svc, status, www)
		}
	}
	a2a := `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`
	if status, _, body := oauthSend(t, http.MethodPost, pub+"/agents/hi", mcpTok, a2a); status != http.StatusUnauthorized {
		t.Errorf("a /mcp token on an A2A route: %d %s", status, body)
	}

	// A token for /mcp/mine: accepted there, not on /mcp.
	mineTok := as.MustSign(oidctest.Token{Audience: []string{oidc.CanonicalURI(pub, "mine")}})
	if status, _, body := oauthSend(t, http.MethodPost, pub+"/mcp/mine", mineTok, initialize); status != http.StatusOK || !strings.Contains(string(body), `"result"`) {
		t.Errorf("a /mcp/mine token there: %d %s", status, body)
	}
	if status, www, _ := oauthSend(t, http.MethodPost, pub+"/mcp", mineTok, initialize); status != http.StatusUnauthorized || www != challenge("")+`, error="invalid_token"` {
		t.Errorf("a /mcp/mine token on /mcp: %d %q", status, www)
	}
	// Admitted on /mcp/mine, not on /mcp/fs, whose allow: names org:default only: the unknown service (D50).
	fsTok := as.MustSign(oidctest.Token{Audience: []string{oidc.CanonicalURI(pub, "fs")}})
	if status, _, body := oauthSend(t, http.MethodPost, pub+"/mcp/fs", fsTok, initialize); status != http.StatusNotFound || !strings.Contains(string(body), "unknown service") {
		t.Errorf("a token for a service whose allow: does not admit it: %d %s", status, body)
	}

	// Refused tokens.
	aud := []string{oidc.CanonicalURI(pub, "")}
	for name, tok := range map[string]string{
		"expired":   as.MustSign(oidctest.Token{Audience: aud, Expiry: time.Now().Add(-2 * time.Minute)}),
		"issuer":    as.MustSign(oidctest.Token{Audience: aud, Issuer: "https://other.example"}),
		"none":      as.MustSign(oidctest.Token{Alg: oidctest.AlgNone, Audience: aud}),
		"HS256":     as.MustSign(oidctest.Token{Alg: oidctest.AlgHS256, Audience: aud}),
		"signature": as.MustSign(oidctest.Token{Alg: oidctest.AlgRS256, Audience: aud, Tamper: true}),
		"other key": as.MustSign(oidctest.Token{Alg: oidctest.AlgEdDSA, Audience: aud, Unpublished: true}),
	} {
		if status, www, _ := oauthSend(t, http.MethodPost, pub+"/mcp", tok, initialize); status != http.StatusUnauthorized || www != challenge("")+`, error="invalid_token"` {
			t.Errorf("%s: %d %q", name, status, www)
		}
	}
	if n := as.Fetches(oidctest.PathJWKS); n != 1 {
		t.Errorf("the key set was fetched %d times within the minute, want once", n)
	}

	// An API key still works on both.
	key := e.mintKey("buyer")
	newMCPClient(t, a.url, key).initialize(`{}`)
	if status, _, body := oauthSend(t, http.MethodPost, pub+"/mcp/fs", key, initialize); status != http.StatusOK || !strings.Contains(string(body), `"result"`) {
		t.Errorf("an API key on /mcp/fs: %d %s", status, body)
	}
	if status, www, _ := oauthSend(t, http.MethodPost, pub+"/mcp", "kk_nobody_"+strings.Repeat("a", 32), initialize); status != http.StatusUnauthorized ||
		www != challenge("")+`, error="invalid_token"` {
		t.Errorf("an unknown API key: %d %q", status, www)
	}
}
