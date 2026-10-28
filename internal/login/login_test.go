package login

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/oidc/oidctest"
)

// rig is a gate serving its RFC 9728 document for /mcp (plain http on 127.0.0.1, as a contributor's gate) in front of
// an oidctest authorization server, and a HOME.
type rig struct {
	t      *testing.T
	as     *oidctest.Server
	gate   *httptest.Server
	origin string
	creds  string
	// prm edits the gate's document.
	prm func(m map[string]any)
}

func newRig(t *testing.T, cfg oidctest.Config) *rig { return newRigWith(t, cfg, true) }

// newRigWith is newRig; keys false leaves oidctest issuing JWT access tokens (MintKey nil).
func newRigWith(t *testing.T, cfg oidctest.Config, keys bool) *rig {
	t.Helper()
	if cfg.MintKey == nil && keys {
		n := 0
		var mu sync.Mutex
		cfg.MintKey = func(context.Context, string, string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			n++
			return fmt.Sprintf("kk_login_%032d", n), nil
		}
	}
	as, err := oidctest.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(as.Close)
	r := &rig{t: t, as: as, creds: filepath.Join(t.TempDir(), ".kontra", "credentials")}
	r.gate = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/.well-known/oauth-protected-resource/mcp" {
			http.NotFound(w, req)
			return
		}
		m := map[string]any{"resource": r.origin + "/mcp", "authorization_servers": []string{r.as.Issuer()}}
		if r.prm != nil {
			r.prm(m)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m)
	}))
	t.Cleanup(r.gate.Close)
	r.origin = r.gate.URL
	return r
}

// options are a login at the rig's gate whose browser is oidctest.Follow (tamper changes the redirect).
func (r *rig) options(out *syncBuffer, tamper ...func(*url.URL)) Options {
	gate, _ := url.Parse(r.origin)
	return Options{
		Gate: gate, CredentialsPath: r.creds, Out: out, Wait: 20 * time.Second,
		Client: NewClient(&tls.Config{RootCAs: r.as.CAPool(), MinVersion: tls.VersionTLS12}),
		Open: func(u string) error {
			go func() { _, _ = r.as.Follow(context.Background(), u, tamper...) }()
			return nil
		},
	}
}

func (r *rig) credential() node.Credential {
	r.t.Helper()
	c, err := node.LoadCredentials(r.creds)
	if err != nil {
		r.t.Fatal(err)
	}
	return c.For(r.origin)
}

// syncBuffer is a bytes.Buffer safe for a writer and a reader at once.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// §6.3: a login registers once per gate origin (the client_id kept with the credential and reused), sends PKCE S256
// whose verifier the server accepts, state, the loopback redirect and resource = the gate's origin, and keeps the kk_
// key for that origin only, in a 0600 file, leaving another origin's entry as it was.
func TestLogin(t *testing.T) {
	r := newRig(t, oidctest.Config{})
	ctx := context.Background()
	// Another gate's entry, which login must leave alone.
	pre, err := node.LoadCredentials(r.creds)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pre.Update("https://other.example", func(c *node.Credential) { c.APIKey = "kk_other_" + strings.Repeat("o", 32) }); err != nil {
		t.Fatal(err)
	}

	var out syncBuffer
	if err := Run(ctx, r.options(&out)); err != nil {
		t.Fatalf("login: %v\n%s", err, out.String())
	}
	c := r.credential()
	if c.APIKey != "kk_login_"+fmt.Sprintf("%032d", 1) || c.OAuthClient == nil || c.OAuthClient.Issuer != r.as.Issuer() || c.OAuthClient.ClientID == "" {
		t.Fatalf("credential after login: %+v %+v", c, c.OAuthClient)
	}
	regs := r.as.Registrations()
	if len(regs) != 1 || regs[0].ClientID != c.OAuthClient.ClientID {
		t.Fatalf("registrations %+v", regs)
	}
	req := regs[0].Request
	if req["client_name"] != ClientName || fmt.Sprint(req["redirect_uris"]) != "["+RedirectURI+"]" ||
		req["token_endpoint_auth_method"] != "none" || fmt.Sprint(req["grant_types"]) != "[authorization_code]" {
		t.Errorf("the registration request %v", req)
	}

	auths, toks := r.as.Authorizations(), r.as.TokenRequests()
	if len(auths) != 1 || len(toks) != 1 {
		t.Fatalf("%d authorizations, %d token requests", len(auths), len(toks))
	}
	a, tk := auths[0], toks[0]
	redirect := regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]+/callback$`)
	if a.Get("response_type") != "code" || a.Get("client_id") != c.OAuthClient.ClientID || a.Get("code_challenge_method") != "S256" ||
		len(a.Get("state")) < 22 || a.Get("resource") != r.origin || !redirect.MatchString(a.Get("redirect_uri")) {
		t.Errorf("the authorization request %v", a)
	}
	verifier := tk.Form.Get("code_verifier")
	sum := sha256.Sum256([]byte(verifier))
	if !tk.VerifierOK || len(verifier) != 43 || base64.RawURLEncoding.EncodeToString(sum[:]) != a.Get("code_challenge") ||
		tk.Form.Get("resource") != r.origin || tk.Form.Get("redirect_uri") != a.Get("redirect_uri") ||
		tk.Form.Get("client_id") != a.Get("client_id") || tk.Form.Get("grant_type") != "authorization_code" {
		t.Errorf("the token request %v (verifier ok %v)", tk.Form, tk.VerifierOK)
	}

	// The file: 0600, the key under this origin only, the other origin's entry unchanged.
	fi, err := os.Stat(r.creds)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file mode: %v %v", fi.Mode(), err)
	}
	all, _ := node.LoadCredentials(r.creds)
	if o := all.For("https://other.example"); o.APIKey != "kk_other_"+strings.Repeat("o", 32) || o.OAuthClient != nil {
		t.Errorf("the other origin's entry: %+v", o)
	}
	raw, _ := os.ReadFile(r.creds)
	if n := strings.Count(string(raw), c.APIKey); n != 1 {
		t.Errorf("the key appears %d times in the file", n)
	}
	// The code and the verifier are never written out.
	for _, secret := range []string{verifier, tk.Form.Get("code")} {
		if strings.Contains(out.String(), secret) || strings.Contains(string(raw), secret) {
			t.Errorf("a secret of the flow was printed or stored: %q", out.String())
		}
	}

	// Again: the same client, no second registration; the new key replaces the old.
	if err := Run(ctx, r.options(&out)); err != nil {
		t.Fatal(err)
	}
	if n := len(r.as.Registrations()); n != 1 {
		t.Fatalf("a second login registered again (%d registrations)", n)
	}
	if a2 := r.as.Authorizations()[1]; a2.Get("client_id") != c.OAuthClient.ClientID || a2.Get("state") == a.Get("state") ||
		a2.Get("code_challenge") == a.Get("code_challenge") {
		t.Errorf("the second authorization %v", a2)
	}
	if c2 := r.credential(); c2.APIKey != "kk_login_"+fmt.Sprintf("%032d", 2) {
		t.Errorf("after the second login: %+v", c2)
	}
}

// §6.3 step 3: --no-browser prints the URL (opened by the person); the listener is on 127.0.0.1 only and is closed
// once the code arrived.
func TestLoginNoBrowserAndTheListener(t *testing.T) {
	r := newRig(t, oidctest.Config{})
	var out syncBuffer
	o := r.options(&out)
	o.NoBrowser = true
	o.Open = func(string) error { t.Error("--no-browser opened a browser"); return nil }
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), o) }()
	var authURL string
	deadline := time.Now().Add(10 * time.Second)
	for authURL == "" {
		if m := regexp.MustCompile(`https://127\.0\.0\.1:[0-9]+/authorize\?\S+`).FindString(out.String()); m != "" {
			authURL = m
		}
		if time.Now().After(deadline) {
			t.Fatalf("no URL printed: %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	au, _ := url.Parse(authURL)
	ru, _ := url.Parse(au.Query().Get("redirect_uri"))
	port := ru.Port()
	// The listener: on 127.0.0.1, not on ::1 or another address of the host.
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); err != nil {
		t.Fatalf("the loopback listener: %v", err)
	} else {
		c.Close()
	}
	if c, err := net.DialTimeout("tcp", "[::1]:"+port, time.Second); err == nil {
		c.Close()
		t.Error("the listener answers on ::1")
	}
	if status, err := r.as.Follow(context.Background(), authURL); err != nil || status != http.StatusOK {
		t.Fatalf("follow: %d %v", status, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); err == nil {
		c.Close()
		t.Error("the listener is still open after the code")
	}
	if r.credential().APIKey == "" {
		t.Error("nothing stored")
	}
}

// §6.3 step 3: no answer within the bound ends the login and closes the listener; a state mismatch is refused (no
// token request, nothing stored); the authorization server's error is shown printable.
func TestLoginRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("the bound", func(t *testing.T) {
		r := newRig(t, oidctest.Config{})
		var out syncBuffer
		o := r.options(&out)
		var port string
		o.Open = func(u string) error {
			au, _ := url.Parse(u)
			ru, _ := url.Parse(au.Query().Get("redirect_uri"))
			port = ru.Port()
			return nil
		}
		o.Wait = 300 * time.Millisecond
		began := time.Now()
		err := Run(ctx, o)
		if err == nil || !strings.Contains(err.Error(), "no sign-in arrived") || time.Since(began) > 5*time.Second {
			t.Fatalf("%v after %v", err, time.Since(began))
		}
		if c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); err == nil {
			c.Close()
			t.Error("the listener outlived the bound")
		}
	})
	t.Run("state mismatch", func(t *testing.T) {
		r := newRig(t, oidctest.Config{})
		var out syncBuffer
		err := Run(ctx, r.options(&out, func(u *url.URL) {
			q := u.Query()
			q.Set("state", "forged")
			u.RawQuery = q.Encode()
		}))
		if err == nil || !strings.Contains(err.Error(), "state") {
			t.Fatalf("%v", err)
		}
		if len(r.as.TokenRequests()) != 0 || r.credential().APIKey != "" {
			t.Error("a forged state led to a token request or a stored key")
		}
	})
	t.Run("another iss", func(t *testing.T) {
		r := newRig(t, oidctest.Config{})
		var out syncBuffer
		err := Run(ctx, r.options(&out, func(u *url.URL) {
			q := u.Query()
			q.Set("iss", "https://evil.example")
			u.RawQuery = q.Encode()
		}))
		if err == nil || !strings.Contains(err.Error(), "RFC 9207") || len(r.as.TokenRequests()) != 0 {
			t.Fatalf("%v", err)
		}
	})
	t.Run("no iss when the metadata promises it", func(t *testing.T) {
		r := newRig(t, oidctest.Config{})
		var out syncBuffer
		err := Run(ctx, r.options(&out, func(u *url.URL) {
			q := u.Query()
			q.Del("iss")
			u.RawQuery = q.Encode()
		}))
		if err == nil || !strings.Contains(err.Error(), "RFC 9207") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("no iss and none promised", func(t *testing.T) {
		r := newRig(t, oidctest.Config{NoISS: true})
		var out syncBuffer
		if err := Run(ctx, r.options(&out)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the server's error, escaped", func(t *testing.T) {
		r := newRig(t, oidctest.Config{})
		var out syncBuffer
		err := Run(ctx, r.options(&out, func(u *url.URL) {
			q := u.Query()
			q.Del("code")
			q.Set("error", "access_denied")
			q.Set("error_description", "no\x1b[31m way")
			u.RawQuery = q.Encode()
		}))
		if err == nil || !strings.Contains(err.Error(), "access_denied") || strings.Contains(err.Error(), "\x1b") {
			t.Fatalf("%q", err)
		}
	})
}

// §6.3 step 4: an access token that is not an API key of the gate (a JWT) is refused and not stored.
func TestLoginRefusesATokenThatIsNoAPIKey(t *testing.T) {
	r := newRigWith(t, oidctest.Config{}, false) // oidctest issues a JWT for the resource
	as := r.as
	var out syncBuffer
	err := Run(context.Background(), r.options(&out))
	if err == nil || !strings.Contains(err.Error(), "not an API key of the gate") {
		t.Fatalf("%v", err)
	}
	toks := as.TokenRequests()
	if len(toks) != 1 || strings.Count(toks[0].Issued, ".") != 2 {
		t.Fatalf("the token issued: %+v", toks)
	}
	raw, _ := os.ReadFile(r.creds)
	if c := r.credential(); c.APIKey != "" || strings.Contains(string(raw), toks[0].Issued) {
		t.Error("the JWT was stored")
	}
	for _, bad := range []string{"", "kk_", "Bearer kk_a_b", "kk_a b", "eyJ.eyJ.sig", "kk_a\n"} {
		if accessToken(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if accessToken("kk_acme_"+strings.Repeat("A", 32)) != nil {
		t.Error("a gate key refused")
	}
}

// §6.3 step 1: a gate without the RFC 9728 document has no sign-in (ErrNoSignIn names --api-key); its document for
// another resource is not used.
func TestLoginDiscovery(t *testing.T) {
	ctx := context.Background()
	none := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(none.Close)
	_, err := Discover(ctx, NewClient(nil), none.URL)
	if !errors.Is(err, ErrNoSignIn) || !strings.Contains(err.Error(), "--api-key") {
		t.Fatalf("a gate without sign-in: %v", err)
	}
	r := newRig(t, oidctest.Config{})
	r.prm = func(m map[string]any) { m["resource"] = "https://other.example/mcp" }
	var out syncBuffer
	if err := Run(ctx, r.options(&out)); err == nil || !strings.Contains(err.Error(), "another resource") {
		t.Fatalf("another resource: %v", err)
	}
	r.prm = func(m map[string]any) { m["authorization_servers"] = []string{"http://login.example"} }
	if err := Run(ctx, r.options(&out)); err == nil || !strings.Contains(err.Error(), "not an https:// URL") {
		t.Fatalf("an http:// authorization server: %v", err)
	}
	if len(r.as.Registrations()) != 0 {
		t.Error("registered after a refused discovery")
	}
}

// Mix-up and leaks (RFC 8414 §3.3, RFC 9700; the M4 audit): the metadata's issuer must be the one the gate named; its
// endpoints are https:// (loopback http:// in tests); S256 is required; the token request follows no redirect; the
// code and the verifier are never printed, even when the server echoes them.
func TestLoginMixUpAndLeaks(t *testing.T) {
	ctx := context.Background()
	for name, edit := range map[string]func(m map[string]any){
		"another issuer":         func(m map[string]any) { m["issuer"] = "https://evil.example" },
		"http authorization":     func(m map[string]any) { m["authorization_endpoint"] = "http://login.example/authorize" },
		"http token endpoint":    func(m map[string]any) { m["token_endpoint"] = "http://login.example/token" },
		"no registration":        func(m map[string]any) { delete(m, "registration_endpoint") },
		"no S256":                func(m map[string]any) { m["code_challenge_methods_supported"] = []string{"plain"} },
		"no code flow":           func(m map[string]any) { m["response_types_supported"] = []string{"token"} },
		"confidential clients":   func(m map[string]any) { m["token_endpoint_auth_methods_supported"] = []string{"client_secret_basic"} },
		"userinfo in the token":  func(m map[string]any) { m["token_endpoint"] = "https://u:p@login.example/token" },
		"no authorization grant": func(m map[string]any) { m["grant_types_supported"] = []string{"client_credentials"} },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, oidctest.Config{Metadata: edit})
			var out syncBuffer
			if err := Run(ctx, r.options(&out)); err == nil {
				t.Fatal("accepted")
			}
			if len(r.as.Registrations()) != 0 || len(r.as.Authorizations()) != 0 {
				t.Error("the flow went on after the metadata was refused")
			}
		})
	}

	t.Run("the token request follows no redirect", func(t *testing.T) {
		var moved atomic.Int32
		r := newRig(t, oidctest.Config{Hook: func(w http.ResponseWriter, req *http.Request) bool {
			switch req.URL.Path {
			case oidctest.PathToken:
				http.Redirect(w, req, "/token2", http.StatusTemporaryRedirect)
				return true
			case "/token2":
				moved.Add(1)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"access_token":"kk_evil_` + strings.Repeat("e", 32) + `","token_type":"Bearer"}`))
				return true
			}
			return false
		}})
		var out syncBuffer
		err := Run(ctx, r.options(&out))
		if err == nil || !strings.Contains(err.Error(), "redirect") || moved.Load() != 0 || r.credential().APIKey != "" {
			t.Fatalf("%v; the redirect's target got %d requests", err, moved.Load())
		}
	})

	t.Run("an echoing server", func(t *testing.T) {
		var form atomic.Value
		r := newRig(t, oidctest.Config{Hook: func(w http.ResponseWriter, req *http.Request) bool {
			if req.URL.Path != oidctest.PathToken {
				return false
			}
			_ = req.ParseForm()
			form.Store(req.PostForm)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant",
				"error_description": "code " + req.PostForm.Get("code") + " and verifier " + req.PostForm.Get("code_verifier")})
			return true
		}})
		var out syncBuffer
		err := Run(ctx, r.options(&out))
		f, _ := form.Load().(url.Values)
		if err == nil || f == nil {
			t.Fatalf("%v", err)
		}
		for _, secret := range []string{f.Get("code"), f.Get("code_verifier")} {
			if secret == "" || strings.Contains(err.Error(), secret) || strings.Contains(out.String(), secret) {
				t.Errorf("the code or the verifier was printed: %q / %q", err, out.String())
			}
		}
	})
}

// An authorization server that forgot the client: the stored client_id is dropped, so the next login registers again.
func TestLoginForgetsARejectedClient(t *testing.T) {
	var reject atomic.Bool
	r := newRig(t, oidctest.Config{Hook: func(w http.ResponseWriter, req *http.Request) bool {
		if req.URL.Path != oidctest.PathToken || !reject.Load() {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		return true
	}})
	ctx := context.Background()
	var out syncBuffer
	reject.Store(true)
	if err := Run(ctx, r.options(&out)); err == nil || !strings.Contains(err.Error(), "does not know this client") {
		t.Fatalf("%v", err)
	}
	if c := r.credential(); c.OAuthClient != nil {
		t.Fatalf("the rejected client was kept: %+v", c.OAuthClient)
	}
	reject.Store(false)
	if err := Run(ctx, r.options(&out)); err != nil {
		t.Fatal(err)
	}
	if n := len(r.as.Registrations()); n != 2 {
		t.Errorf("%d registrations, want a new one after the rejection", n)
	}
}

func TestParseArgs(t *testing.T) {
	env := func(string) string { return "/home/x" }
	o, err := ParseArgs(nil, env)
	if err != nil || o.Gate.String() != "https://gateway.kontra.sh" || o.NoBrowser || o.CredentialsPath != "/home/x/.kontra/credentials" {
		t.Fatalf("defaults: %+v %v", o, err)
	}
	if o, err := ParseArgs([]string{"--gate", "http://localhost:8080", "--no-browser"}, env); err != nil || !o.NoBrowser || o.Gate.Host != "localhost:8080" {
		t.Fatalf("flags: %+v %v", o, err)
	}
	for _, args := range [][]string{{"--gate", "http://gate.example"}, {"--bogus"}, {"extra"}, {"--gate", "https://u:p@gate.example"}} {
		if _, err := ParseArgs(args, env); !errors.Is(err, ErrUsage) {
			t.Errorf("%q: %v, want a usage error", args, err)
		}
	}
}

// §6.3 step 2 (DM4-29): an authorization server that forgot a registered client refuses it at its authorization
// endpoint without redirecting (RFC 6749 §4.1.2.1; oidctest answers 400 "unknown client"), so kontra login hears
// nothing until its bound or an interrupt. A login that reused the stored client_id and got no answer forgets it and
// says so; the next login registers again and signs in. (Before, every later login reused the dead client_id and
// failed the same way until ~/.kontra/credentials was edited by hand.)
func TestLoginForgetsAClientTheAuthorizationEndpointRefuses(t *testing.T) {
	for _, how := range []string{"the bound", "an interrupt"} {
		t.Run(how, func(t *testing.T) {
			r := newRig(t, oidctest.Config{})
			pre, err := node.LoadCredentials(r.creds)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pre.Update(r.origin, func(c *node.Credential) {
				c.OAuthClient = &node.OAuthClient{Issuer: r.as.Issuer(), ClientID: "stale"}
			}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var out syncBuffer
			o := r.options(&out)
			var status atomic.Int32
			browsed := make(chan struct{})
			o.Open = func(u string) error {
				go func() {
					defer close(browsed)
					// The browser: the authorization server's own refusal page, no redirect.
					if resp, err := r.as.Client().Get(u); err == nil {
						resp.Body.Close()
						status.Store(int32(resp.StatusCode))
					}
					if how == "an interrupt" {
						cancel() // the person gives up (Ctrl-C)
					}
				}()
				return nil
			}
			if how == "the bound" {
				o.Wait = 500 * time.Millisecond
			}
			err = Run(ctx, o)
			if err == nil || !strings.Contains(err.Error(), "no sign-in arrived") || !strings.Contains(err.Error(), "registers again") {
				t.Fatalf("the login with a forgotten client: %v", err)
			}
			select {
			case <-browsed:
			case <-time.After(10 * time.Second):
				t.Fatal("the browser never got its answer")
			}
			if st, auths := status.Load(), r.as.Authorizations(); st != http.StatusBadRequest || len(auths) != 1 ||
				auths[0].Get("client_id") != "stale" {
				t.Fatalf("the authorization endpoint answered %d to %v, want its own 400 to the stale client", st, auths)
			}
			if c := r.credential(); c.OAuthClient != nil || c.APIKey != "" {
				t.Fatalf("after the unanswered login: client %+v, key %q", c.OAuthClient, c.APIKey)
			}
			if n := len(r.as.Registrations()); n != 0 {
				t.Fatalf("%d registrations before the next login", n)
			}

			// The next login registers again and signs in.
			if err := Run(context.Background(), r.options(&out)); err != nil {
				t.Fatalf("the next login: %v", err)
			}
			regs := r.as.Registrations()
			if c := r.credential(); len(regs) != 1 || c.OAuthClient == nil || c.OAuthClient.ClientID != regs[0].ClientID || c.APIKey == "" {
				t.Fatalf("after the next login: %+v %+v, %d registrations", c, c.OAuthClient, len(regs))
			}
		})
	}

	// A client registered by this very login is kept when no answer comes (the person did not finish), and dropped
	// only by a later login that reused it and got none either.
	t.Run("a fresh registration", func(t *testing.T) {
		r := newRig(t, oidctest.Config{})
		var out syncBuffer
		o := r.options(&out)
		o.Open = func(string) error { return nil }
		o.Wait = 300 * time.Millisecond
		err := Run(context.Background(), o)
		if err == nil || strings.Contains(err.Error(), "registers again") {
			t.Fatalf("%v", err)
		}
		regs := r.as.Registrations()
		if c := r.credential(); len(regs) != 1 || c.OAuthClient == nil || c.OAuthClient.ClientID != regs[0].ClientID {
			t.Fatalf("the fresh client was not kept: %+v, %d registrations", c.OAuthClient, len(regs))
		}
		if err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "registers again") {
			t.Fatalf("the second unanswered login: %v", err)
		}
		if c := r.credential(); c.OAuthClient != nil {
			t.Fatalf("kept after a reused client got no answer: %+v", c.OAuthClient)
		}
	})
}

// §6.3 step 3: the loopback port listens only until the answer arrived. It is closed before the token request, which
// may run for its own timeout, not when the login ends.
func TestLoginClosesTheListenerBeforeTheTokenRequest(t *testing.T) {
	inToken, release := make(chan struct{}), make(chan struct{})
	var entered, released sync.Once
	r := newRig(t, oidctest.Config{Hook: func(w http.ResponseWriter, req *http.Request) bool {
		if req.URL.Path == oidctest.PathToken {
			entered.Do(func() { close(inToken) })
			<-release
		}
		return false
	}})
	t.Cleanup(func() { released.Do(func() { close(release) }) }) // before the server's Close (cleanups run last first)
	var out syncBuffer
	o := r.options(&out)
	var port atomic.Value
	follow := o.Open
	o.Open = func(u string) error {
		au, _ := url.Parse(u)
		ru, _ := url.Parse(au.Query().Get("redirect_uri"))
		port.Store(ru.Port())
		return follow(u)
	}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), o) }()
	select {
	case <-inToken:
	case err := <-done:
		t.Fatalf("the login ended before its token request: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("no token request")
	}
	p, _ := port.Load().(string)
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+p, time.Second)
	if err == nil {
		c.Close()
		t.Error("the loopback listener still accepts connections while the token request runs")
	}
	released.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r.credential().APIKey == "" {
		t.Error("nothing stored")
	}
}

// §6.3 step 1 (RFC 9728 §3.3): the document's resource is compared with {gate origin}/mcp as an origin and a path. The
// gate writes it from public_url as configured, so an explicit default port, or a host or scheme in upper case, is the
// same resource; another origin, path, port or scheme, a trailing slash, userinfo, a query or a fragment is not.
func TestLoginComparesTheResourceAsAnOrigin(t *testing.T) {
	as, err := oidctest.New(oidctest.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(as.Close)
	next := NewClient(&tls.Config{RootCAs: as.CAPool(), MinVersion: tls.VersionTLS12}).Transport
	for _, tc := range []struct {
		gate, resource string
		same           bool
	}{
		{"https://gateway.example.com", "https://gateway.example.com/mcp", true},
		{"https://gateway.example.com", "https://gateway.example.com:443/mcp", true},
		{"https://gateway.example.com:443", "https://gateway.example.com/mcp", true},
		{"https://gateway.example.com", "https://Gateway.Example.COM/mcp", true},
		{"https://GATEWAY.example.com", "HTTPS://gateway.example.com/mcp", true},
		{"https://gateway.example.com:8443", "https://gateway.example.com:8443/mcp", true},
		{"https://gateway.example.com", "https://gateway.example.com/mcp/", false},
		{"https://gateway.example.com", "https://gateway.example.com/kontra/mcp", false},
		{"https://gateway.example.com", "https://gateway.example.com", false},
		{"https://gateway.example.com", "https://gateway.example.com:8443/mcp", false},
		{"https://gateway.example.com", "http://gateway.example.com/mcp", false},
		{"https://gateway.example.com", "https://other.example.com/mcp", false},
		{"https://gateway.example.com", "https://u@gateway.example.com/mcp", false},
		{"https://gateway.example.com", "https://gateway.example.com/mcp?x=1", false},
		{"https://gateway.example.com", "https://gateway.example.com/mcp#f", false},
		{"https://gateway.example.com", "https://gateway.example.com/mcp#", false},
	} {
		// The gate is served in-process: requests to its host reach this handler, the authorization server's go out.
		gate := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path != "/.well-known/oauth-protected-resource/mcp" {
				http.NotFound(w, req)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": tc.resource, "authorization_servers": []string{as.Issuer()}})
		})
		client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
			if req.URL.Hostname() == "gateway.example.com" {
				rec := httptest.NewRecorder()
				gate.ServeHTTP(rec, req)
				return rec.Result(), nil
			}
			return next.RoundTrip(req)
		})}
		s, err := Discover(context.Background(), client, tc.gate)
		switch {
		case tc.same && err != nil:
			t.Errorf("--gate %s, resource %s: %v", tc.gate, tc.resource, err)
		case tc.same && s.Issuer != as.Issuer():
			t.Errorf("--gate %s, resource %s: issuer %q", tc.gate, tc.resource, s.Issuer)
		case !tc.same && (err == nil || !strings.Contains(err.Error(), "another resource")):
			t.Errorf("--gate %s, resource %s: %v, want another resource", tc.gate, tc.resource, err)
		}
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
