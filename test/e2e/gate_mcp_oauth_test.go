package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	kids "kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/oidc"
	"kontra.sh/kontra/internal/oidc/oidctest"
)

// gate_mcp_oauth (spec/relay.md §16; §12.1; D276, D284, D296). Single tenant. auth.mcp_oauth.issuer is the
// in-repository authorization server (oidctest over HTTPS with a throwaway CA, which both replica processes trust
// through SSL_CERT_FILE: their operator client uses the system roots). Asserted, on both replicas:
//
//   - a request without a credential to /mcp, to an existing /mcp/fs and to an unknown /mcp/nope is 401 with
//     WWW-Authenticate: Bearer resource_metadata="{public_url}/.well-known/oauth-protected-resource/mcp[/fs|/nope]",
//     scope="mcp", while open, whose allow: admits anonymous, serves it;
//   - each of the three RFC 9728 documents has resource = the route's canonical URI and authorization_servers =
//     [issuer], nope's included;
//   - a token whose aud is {public_url}/mcp opens a session on /mcp; the session and its hire task are via mcp with the
//     principal oidc:sub:<sub> (buyer UUIDv5(issuer ‖ sub)); the same token is 401 with error="invalid_token" on /mcp/fs
//     and 401 on /agents/slow;
//   - a token whose aud is /mcp/fs works there only (401 on /mcp);
//   - an expired token, another issuer's, an HMAC and a none one, and a bad signature are 401;
//   - each replica fetches the key set once; a token signed with a new kid, after the minute since that fetch, makes
//     exactly one refetch on each replica (two uses each), and is then accepted;
//   - an API key is still accepted on /mcp and /mcp/fs.
func TestGateMCPOAuth(t *testing.T) {
	as, err := StartAuthServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(as.Close)
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	h := start(t, Options{
		AuthServer: as,
		Allow:      map[string][]string{"slow": {"org:default", "oidc:sub:user-1"}},
		MCP: map[string]MCPService{
			"fs":   {URL: doubleURL(t, up), Allow: []string{"org:default", "oidc:sub:user-1"}},
			"open": {URL: doubleURL(t, up), Allow: []string{"anonymous"}},
		},
	})
	ctx := ctxFor(t, 10*time.Minute)
	pub := h.Front.URL
	challenge := func(svc string) string {
		route := "/mcp"
		if svc != "" {
			route += "/" + svc
		}
		return `Bearer resource_metadata="` + pub + `/.well-known/oauth-protected-resource` + route + `", scope="mcp"`
	}
	route := func(g *Gate, svc string) string {
		if svc == "" {
			return g.URL + "/mcp"
		}
		return g.URL + "/mcp/" + svc
	}

	// No credential: the challenge on /mcp, an existing service and an unknown one; open serves it.
	for _, g := range h.Gates {
		for _, svc := range []string{"", "fs", "nope"} {
			if st, www := noCredentialPost(t, route(g, svc), initializeBody); st != http.StatusUnauthorized || www != challenge(svc) {
				t.Errorf("POST %s without a credential on %s: %d %q, want 401 %q", route(g, svc), g.Name, st, www, challenge(svc))
			}
		}
		if st, body := oauthPost(t, route(g, "open"), "", initializeBody); st != http.StatusOK || !strings.Contains(body, `"result"`) {
			t.Errorf("open without a credential on %s: %d %s", g.Name, st, body)
		}
	}
	// The RFC 9728 documents, for every well-formed name, existing or not.
	for _, svc := range []string{"", "fs", "nope"} {
		path := oidc.MetadataPrefix + "/mcp"
		if svc != "" {
			path += "/" + svc
		}
		body, st := getBody(ctx, h.Gates[1].URL+path, "")
		var doc oidc.ProtectedResource
		_ = json.Unmarshal(body, &doc)
		if st != http.StatusOK || doc.Resource != oidc.CanonicalURI(pub, svc) || len(doc.AuthorizationServers) != 1 ||
			doc.AuthorizationServers[0] != as.Issuer() {
			t.Errorf("%s: %d %s", path, st, body)
		}
	}

	// A token for /mcp: a session on each replica (each fetches the key set once), via mcp; its hire task too.
	mcpTok := as.MustSign(oidctest.Token{Audience: []string{oidc.CanonicalURI(pub, "")}})
	var lastFetch time.Time
	for i, g := range h.Gates {
		s := OpenGateSession(t, ctx, g, mcpTok, `{}`)
		lastFetch = time.Now()
		if n := as.Fetches(oidctest.PathJWKS); n != i+1 {
			t.Fatalf("after the token's first use on %s the key set was fetched %d times, want %d", g.Name, n, i+1)
		}
		if i == 0 {
			r := Tool(t, s.Tool("hire", `{"service":"slow","message":"hi"}`))
			buyer, _ := kids.OIDCBuyer(as.Issuer(), "user-1")
			if r.IsError || r.TaskID() == "" || !scalar[bool](t, h, `select via = 'mcp' and principal = 'oidc:sub:user-1' and buyer_id = $2 and hired
				from kontra.tasks where id::text = $1`, r.TaskID(), buyer) {
				t.Errorf("the token's hire task is not via mcp, oidc:sub:user-1: %+v", r)
			}
		}
	}
	if n := count(t, h, `select count(*) from kontra.mcp_sessions where service is null and tenant_id is null and via = 'mcp'
		and principal = 'oidc:sub:user-1'`); n != 2 {
		t.Errorf("the token's /mcp sessions: %d rows, want 2 via mcp, oidc:sub:user-1", n)
	}
	a2aBody := `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`
	for _, g := range h.Gates {
		if st, www := oauthStatus(t, route(g, "fs"), mcpTok, initializeBody); st != http.StatusUnauthorized || www != challenge("fs")+`, error="invalid_token"` {
			t.Errorf("a /mcp token on /mcp/fs (%s): %d %q", g.Name, st, www)
		}
		if st, _ := oauthStatus(t, g.URL+"/agents/slow", mcpTok, a2aBody); st != http.StatusUnauthorized {
			t.Errorf("a /mcp token on /agents/slow (%s): %d", g.Name, st)
		}
	}

	// A token for /mcp/fs: there only.
	fsTok := as.MustSign(oidctest.Token{Audience: []string{oidc.CanonicalURI(pub, "fs")}})
	if st, body := oauthPost(t, route(h.Gates[1], "fs"), fsTok, initializeBody); st != http.StatusOK || !strings.Contains(body, `"result"`) {
		t.Errorf("a /mcp/fs token there: %d %s", st, body)
	}
	if st, www := oauthStatus(t, route(h.Gates[0], ""), fsTok, initializeBody); st != http.StatusUnauthorized || www != challenge("")+`, error="invalid_token"` {
		t.Errorf("a /mcp/fs token on /mcp: %d %q", st, www)
	}

	// Refused tokens.
	aud := []string{oidc.CanonicalURI(pub, "")}
	for name, tok := range map[string]string{
		"expired":       as.MustSign(oidctest.Token{Audience: aud, Expiry: time.Now().Add(-2 * time.Minute)}),
		"other issuer":  as.MustSign(oidctest.Token{Audience: aud, Issuer: "https://other.example"}),
		"HS256":         as.MustSign(oidctest.Token{Alg: oidctest.AlgHS256, Audience: aud}),
		"none":          as.MustSign(oidctest.Token{Alg: oidctest.AlgNone, Audience: aud}),
		"bad signature": as.MustSign(oidctest.Token{Alg: oidctest.AlgRS256, Audience: aud, Tamper: true}),
	} {
		for _, g := range h.Gates {
			if st, www := oauthStatus(t, route(g, ""), tok, initializeBody); st != http.StatusUnauthorized || www != challenge("")+`, error="invalid_token"` {
				t.Errorf("a token with %s on %s: %d %q", name, g.Name, st, www)
			}
		}
	}

	// An API key is still accepted.
	OpenGateSession(t, ctx, h.Gates[0], h.APIKey, `{}`)
	if st, body := oauthPost(t, route(h.Gates[1], "fs"), h.APIKey, initializeBody); st != http.StatusOK || !strings.Contains(body, `"result"`) {
		t.Errorf("an API key on /mcp/fs: %d %s", st, body)
	}
	if n := as.Fetches(oidctest.PathJWKS); n != 2 {
		t.Fatalf("the key set was fetched %d times before the rotation, want once per replica", n)
	}

	// A rotated key: after the minute since each replica's fetch, a token with the new kid refetches the set once on
	// each replica, however often it is used (D296: at most once a minute after the last fetch).
	if wait := time.Until(lastFetch.Add(61 * time.Second)); wait > 0 {
		t.Logf("waiting %v for the key set's refetch window", wait.Round(time.Second))
		time.Sleep(wait)
	}
	kid, err := as.Rotate(oidctest.AlgES256)
	if err != nil {
		t.Fatal(err)
	}
	rotated := as.MustSign(oidctest.Token{Kid: kid, Audience: aud})
	for i, g := range h.Gates {
		for range 2 {
			if st, body := oauthPost(t, route(g, ""), rotated, initializeBody); st != http.StatusOK || !strings.Contains(body, `"result"`) {
				t.Errorf("the rotated key's token on %s: %d %s", g.Name, st, body)
			}
		}
		if n := as.Fetches(oidctest.PathJWKS); n != 3+i {
			t.Errorf("after the rotated token's uses on %s the key set was fetched %d times, want %d (one refetch per replica)", g.Name, n, 3+i)
		}
	}
}

// doubleURL serves the MCP double on 127.0.0.1 until the test ends and returns its endpoint.
func doubleURL(t *testing.T, d *mcptest.Server) string {
	t.Helper()
	srv := httptest.NewServer(d) // 127.0.0.1
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv.URL + "/mcp"
}

// oauthPost posts body to u with bearer (none when "") and returns the status and the body.
func oauthPost(t *testing.T, u, bearer, body string) (int, string) {
	t.Helper()
	st, _, b := oauthDo(t, u, bearer, body)
	return st, b
}

// oauthStatus posts body to u with bearer and returns the status and WWW-Authenticate.
func oauthStatus(t *testing.T, u, bearer, body string) (int, string) {
	t.Helper()
	st, www, _ := oauthDo(t, u, bearer, body)
	return st, www
}

func oauthDo(t *testing.T, u, bearer, body string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("A2A-Version", "1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate"), string(b)
}
