package e2e

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// login_converts (spec/tunnel.md §16; §6.3, §6.1; D278, D286, D296). Multi-tenant, tunnel.anonymous on,
// tunnel.enroll_with_api_key, auth.mcp_oauth at the in-repository authorization server, whose token endpoint issues a kk_
// key of the account tenant acme (hire, enroll:*). Asserted:
//
//   - kontra login --no-browser (HOME = the anonymous node's) registers once (a second login reuses the client_id); the
//     authorization request carries code_challenge_method S256, a state and resource = the gate's origin, and the token
//     request a code_verifier the server matched against the challenge;
//   - the redirect goes to http://127.0.0.1:<port>/callback only, and that listener is closed once login exits;
//   - the kk_ key is stored 0600, under the gate's origin only;
//   - an access token that is not an API key (a JWT) is refused: login exits 1 and the stored key is unchanged;
//   - kontra connect restarted with it converts the tenant: its welcome names acme and the account's URL
//     (/agents/acme/hello) with no anonymous session; the service has allow [org:acme] and the account no expiry; the
//     anonymous URL answers 404 on both replicas; an audit row records the conversion (via the API key);
//   - against a gate without auth.mcp_oauth, kontra login exits 1 naming --api-key and stores nothing for it.
func TestLoginConverts(t *testing.T) {
	as, err := StartAuthServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(as.Close)
	seller := StartSeller(t)
	h := start(t, Options{Multi: true, Anonymous: anonConfig(10*time.Minute, 1000), EnrollWithAPIKey: true, AuthServer: as, ProxyProtocol: true})
	ctx := ctxFor(t, 10*time.Minute)
	acme, err := h.SeedTenant(ctx, "acme", "account")
	if err != nil {
		t.Fatal(err)
	}
	as.MintFor(h.Pool, acme)
	origin := h.Front.URL

	// An anonymous session hosting hello, public.
	n := anonNode(t, ctx, h, "198.51.100.40", map[string]string{"hello": seller.URL}, nil, "hello")
	anon := slugOf(n)
	anonID := h.TenantOf(ctx, anon)
	if err := h.WaitA2A(ctx, anon+"/hello", "", 60*time.Second); err != nil {
		t.Fatal(err)
	}

	// kontra login, followed by the browser stand-in.
	login := func(want int) *LoginResult {
		t.Helper()
		r, err := h.Login(ctx, as, origin, n.Home, as.CAFile, true)
		if err != nil {
			t.Fatal(err)
		}
		if r.Code != want {
			t.Fatalf("kontra login exited %d, want %d:\n%s\n%s", r.Code, want, r.Stdout, r.Stderr)
		}
		return r
	}
	r1 := login(0)
	if r1.URL == "" || r1.Follow != http.StatusOK {
		t.Fatalf("kontra login printed no authorization URL, or its callback answered %d:\n%s", r1.Follow, r1.Stdout)
	}
	if !strings.Contains(r1.Stdout, "kontra login: signed in at "+origin) || !strings.Contains(r1.Stdout, CredentialsFile(n.Home)) {
		t.Errorf("kontra login's success line: %q", r1.Stdout)
	}
	if regs := as.Registrations(); len(regs) != 1 || regs[0].Request["token_endpoint_auth_method"] != "none" {
		t.Fatalf("registrations after one login: %+v", regs)
	}
	auths := as.Authorizations()
	if len(auths) != 1 {
		t.Fatalf("%d authorization requests", len(auths))
	}
	az := auths[0]
	redirect, err := url.Parse(az.Get("redirect_uri"))
	if err != nil || az.Get("code_challenge_method") != "S256" || az.Get("code_challenge") == "" || az.Get("state") == "" ||
		az.Get("resource") != origin || redirect.Scheme != "http" || redirect.Hostname() != "127.0.0.1" || redirect.Path != "/callback" ||
		az.Get("client_id") != as.Registrations()[0].ClientID {
		t.Errorf("the authorization request: %v", az)
	}
	if conn, err := net.DialTimeout("tcp", redirect.Host, 2*time.Second); err == nil {
		conn.Close()
		t.Errorf("the loopback redirect listener %s still accepts connections after kontra login exited", redirect.Host)
	}
	toks := as.TokenRequests()
	if len(toks) != 1 || !toks[0].VerifierOK || toks[0].Form.Get("code_verifier") == "" || toks[0].Form.Get("resource") != origin ||
		!strings.HasPrefix(toks[0].Issued, "kk_") {
		t.Errorf("the token request: %+v", toks)
	}
	stored := func() (map[string]json.RawMessage, string) {
		t.Helper()
		path := CredentialsFile(n.Home)
		if m := fileMode(path); m != 0o600 {
			t.Errorf("the credentials file is %o, want 0600", m)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Gates map[string]json.RawMessage `json:"gates"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("the credentials file: %v", err)
		}
		var c struct {
			APIKey string `json:"api_key"`
		}
		_ = json.Unmarshal(doc.Gates[origin], &c)
		return doc.Gates, c.APIKey
	}
	gates, key1 := stored()
	if len(gates) != 1 || key1 != toks[0].Issued {
		t.Errorf("the credentials hold %d origins and the key %.12s…, want the issued key under %s only", len(gates), key1, origin)
	}
	if strings.Contains(r1.Stdout+r1.Stderr, key1) || strings.Contains(r1.Stdout+r1.Stderr, toks[0].Form.Get("code_verifier")) {
		t.Error("kontra login printed the key or the code verifier")
	}

	// A second login reuses the registration.
	login(0)
	if regs := as.Registrations(); len(regs) != 1 {
		t.Errorf("a second login registered again: %d registrations", len(regs))
	}
	if az := as.Authorizations(); len(az) != 2 || az[1].Get("client_id") != as.Registrations()[0].ClientID {
		t.Errorf("the second authorization request: %v", az)
	}
	_, key2 := stored()
	if !strings.HasPrefix(key2, "kk_") || key2 == key1 {
		t.Fatalf("the second login stored %.12s…", key2)
	}

	// An access token that is not an API key: refused, nothing stored.
	as.MintJWT(true)
	rj := login(1)
	as.MintJWT(false)
	if jt := as.TokenRequests(); len(jt) != 3 || strings.HasPrefix(jt[2].Issued, "kk_") || jt[2].Issued == "" {
		t.Fatalf("the JWT variant's token request: %+v", jt)
	}
	if _, key3 := stored(); key3 != key2 || strings.Contains(readFile(CredentialsFile(n.Home)), "eyJ") {
		t.Errorf("the refused JWT changed the stored credentials:\n%s", rj.Stderr)
	}

	// kontra connect restarted with the key converts the tenant.
	if err := n.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := n.Restart(ctx, h); err != nil {
		t.Fatal(err)
	}
	if slugOf(n) != "acme" {
		t.Fatalf("the restarted connect's welcome names %s, want acme", slugOf(n))
	}
	out := readFile(n.OutPath)
	last := out[strings.LastIndex(out, "kontra connect: connected to"):]
	if !strings.Contains(last, origin+"/agents/acme/hello") || strings.Contains(last, "anonymous session") || strings.Contains(last, anon) {
		t.Errorf("the converted session's welcome:\n%s", last)
	}
	if !scalar[bool](t, h, `select allow = '{org:acme}' and disabled_at is null from kontra.services where tenant_id = $1 and name = 'hello'`, acme) {
		t.Error("the converted service is not acme/hello with allow [org:acme]")
	}
	if !scalar[bool](t, h, `select expires_at is null and kind = 'account' from kontra.tenants where id = $1`, acme) {
		t.Error("the account expires")
	}
	if !scalar[bool](t, h, `select expires_at <= now() from kontra.tenants where id = $1`, anonID) {
		t.Error("the anonymous tenant did not end at the conversion")
	}
	for _, g := range h.Gates {
		eventually(t, 30*time.Second, "the anonymous URL answers 404 on "+g.Name, func() bool {
			_, st := getBody(ctx, g.URL+"/agents/"+anon+"/hello/.well-known/agent-card.json", "")
			return st == http.StatusNotFound
		})
	}
	if err := h.WaitA2A(ctx, "acme/hello", key2, 60*time.Second); err != nil {
		t.Errorf("the account's URL: %v", err)
	}
	if n := count(t, h, `select count(*) from kontra.audit where convert_from(row_jcs, 'UTF8') like '%tunnel.anonymous_converted%'
		and convert_from(row_jcs, 'UTF8') like '%' || $1 || '%' and convert_from(row_jcs, 'UTF8') like '%"via":"api_key"%'`, anon); n != 1 {
		t.Errorf("%d audit rows record the conversion of %s by the API key, want 1", n, anon)
	}

	// A gate without auth.mcp_oauth has no sign-in: kontra login exits 1 naming --api-key.
	plain, err := h.StartReplica(ctx, "C", func(cfg string) string {
		return regexp.MustCompile(`auth: \{ api_keys: true, mcp_oauth: \{ issuer: "[^"]*" \} \}`).ReplaceAllString(cfg, "auth: { api_keys: true }")
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readFile(h.Dir+"/kontra-C.yaml"), "mcp_oauth") {
		t.Fatal("the third replica's configuration still has auth.mcp_oauth")
	}
	home := tempDir(t, h, "home-login")
	rp, err := h.Login(ctx, as, plain.URL, home, as.CAFile, true)
	if err != nil {
		t.Fatal(err)
	}
	if rp.Code != 1 || !strings.Contains(rp.Stderr+rp.Stdout, "--api-key") {
		t.Errorf("kontra login against a gate without auth.mcp_oauth: exit %d\n%s\n%s", rp.Code, rp.Stdout, rp.Stderr)
	}
	if b, err := os.ReadFile(CredentialsFile(home)); err == nil && strings.Contains(string(b), "api_key") {
		t.Errorf("kontra login stored a key for a gate without sign-in: %s", b)
	}
}
