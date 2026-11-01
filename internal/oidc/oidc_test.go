package oidc

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/oidc/oidctest"
	"kontra.sh/kontra/internal/ssrf"
)

const gateURL = "https://gate.example"

// clock is a test clock the verifier reads.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newAS(t *testing.T, cfg oidctest.Config) *oidctest.Server {
	t.Helper()
	as, err := oidctest.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(as.Close)
	return as
}

// verifier is the gate's verifier for as: the operator-endpoint client of §10.3 trusting as's CA, and clk.
func verifier(t *testing.T, as *oidctest.Server, clk *clock) *Verifier {
	t.Helper()
	v, err := NewVerifier(Config{
		Issuer: as.Issuer(),
		Client: ssrf.NewOperatorClient(ssrf.OperatorOptions{TLS: &tls.Config{RootCAs: as.CAPool(), MinVersion: tls.VersionTLS12}}),
		Now:    clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func realClock() *clock { return &clock{t: time.Now()} }

// issuer is an Issuer over v with cfg's tenant claim, tenants and feed.
func issuer(t *testing.T, v *Verifier, cfg IssuerConfig) *Issuer {
	t.Helper()
	cfg.Verifier = v
	iss, err := NewIssuer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return iss
}

var mcpAud = CanonicalURI(gateURL, "")

// §12.1, D276: ES256, RS256 and EdDSA tokens from the issuer, for the route's canonical URI, are accepted; the
// principal is oidc:sub:<sub> with buyer UUIDv5(issuer ‖ sub), no tenant, via mcp.
func TestVerifyAccepts(t *testing.T) {
	as := newAS(t, oidctest.Config{})
	v := verifier(t, as, realClock())
	ctx := context.Background()
	for _, alg := range []string{oidctest.AlgES256, oidctest.AlgRS256, oidctest.AlgEdDSA} {
		tok := as.MustSign(oidctest.Token{Alg: alg, Subject: "alice", Audience: []string{mcpAud}})
		c, err := v.Verify(ctx, tok, mcpAud)
		if err != nil {
			t.Fatalf("%s: %v", alg, err)
		}
		if c.Issuer != as.Issuer() || c.Subject != "alice" || len(c.Audience) != 1 || c.Audience[0] != mcpAud || c.Expiry.IsZero() {
			t.Errorf("%s: claims %+v", alg, c)
		}
	}
	// aud as an array holding the route's URI among others; nbf in the past; exp and nbf within the skew.
	for name, tk := range map[string]oidctest.Token{
		"aud array":     {Audience: []string{"https://elsewhere.example", mcpAud}},
		"nbf past":      {Audience: []string{mcpAud}, NotBefore: time.Now().Add(-time.Minute)},
		"exp 30s ago":   {Audience: []string{mcpAud}, Expiry: time.Now().Add(-30 * time.Second)},
		"nbf in 30s":    {Audience: []string{mcpAud}, NotBefore: time.Now().Add(30 * time.Second)},
		"no kid header": {Audience: []string{mcpAud}, Header: map[string]any{"kid": nil}},
	} {
		tok := as.MustSign(tk)
		if name == "no kid header" {
			tok = stripKid(t, as, tk)
		}
		if _, err := v.Verify(ctx, tok, mcpAud); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	p, err := issuer(t, v, IssuerConfig{}).Principal(ctx, as.MustSign(oidctest.Token{Subject: "alice", Audience: []string{mcpAud}}), mcpAud, auth.ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	buyer, _ := ids.OIDCBuyer(as.Issuer(), "alice")
	if p.Kind != auth.KindOIDC || p.ID != "oidc:sub:alice" || p.BuyerID != buyer || p.Via != auth.ViaMCP || p.TenantID != nil ||
		p.Scopes != nil || !p.MayHire() {
		t.Errorf("principal %+v", p)
	}
	if BuyerID(as.Issuer(), "alice") != buyer || BuyerID(as.Issuer(), "bob") == buyer || BuyerID("", "alice").String() != "00000000-0000-0000-0000-000000000000" {
		t.Error("BuyerID is not UUIDv5(issuer ‖ sub)")
	}
}

// stripKid signs tk's claims with the current ES256 key and a header without kid.
func stripKid(t *testing.T, as *oidctest.Server, tk oidctest.Token) string {
	t.Helper()
	claims, _ := json.Marshal(map[string]any{"iss": as.Issuer(), "sub": "user-1", "aud": mcpAud, "exp": time.Now().Add(time.Hour).Unix()})
	tok, err := as.SignRaw(oidctest.AlgES256, "", []byte(`{"alg":"ES256"}`), claims)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// §12.1, D276: every refusal is ErrInvalidToken (401): an expired token, nbf more than 60 s ahead, another issuer, an
// audience without the route's canonical URI, alg none, HS256, a bad signature (each alg), a key type that does not
// fit the alg, a kid the set does not hold, crit, a repeated or case-variant claim, no exp, no or an empty sub.
func TestVerifyRefuses(t *testing.T) {
	as := newAS(t, oidctest.Config{})
	v := verifier(t, as, realClock())
	ctx := context.Background()
	aud := []string{mcpAud}
	raw := func(alg, kid, hdr string, claims map[string]any) string {
		b, _ := json.Marshal(claims)
		tok, err := as.SignRaw(alg, kid, []byte(hdr), b)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	good := map[string]any{"iss": as.Issuer(), "sub": "u", "aud": mcpAud, "exp": time.Now().Add(time.Hour).Unix()}
	without := func(k string) map[string]any {
		m := map[string]any{}
		for kk, vv := range good {
			if kk != k {
				m[kk] = vv
			}
		}
		return m
	}
	with := func(k string, val any) map[string]any {
		m := without(k)
		m[k] = val
		return m
	}
	es := `{"alg":"ES256","kid":"` + as.Current(oidctest.AlgES256) + `"}`
	cases := map[string]string{
		"expired 2 min ago":    as.MustSign(oidctest.Token{Audience: aud, Expiry: time.Now().Add(-2 * time.Minute)}),
		"nbf in 2 min":         as.MustSign(oidctest.Token{Audience: aud, NotBefore: time.Now().Add(2 * time.Minute)}),
		"another issuer":       as.MustSign(oidctest.Token{Audience: aud, Issuer: "https://other.example"}),
		"issuer with slash":    as.MustSign(oidctest.Token{Audience: aud, Issuer: as.Issuer() + "/"}),
		"another audience":     as.MustSign(oidctest.Token{Audience: []string{CanonicalURI(gateURL, "fs")}}),
		"no audience":          as.MustSign(oidctest.Token{}),
		"alg none":             as.MustSign(oidctest.Token{Alg: oidctest.AlgNone, Audience: aud}),
		"HS256":                as.MustSign(oidctest.Token{Alg: oidctest.AlgHS256, Audience: aud}),
		"bad ES256 signature":  as.MustSign(oidctest.Token{Alg: oidctest.AlgES256, Audience: aud, Tamper: true}),
		"bad RS256 signature":  as.MustSign(oidctest.Token{Alg: oidctest.AlgRS256, Audience: aud, Tamper: true}),
		"bad EdDSA signature":  as.MustSign(oidctest.Token{Alg: oidctest.AlgEdDSA, Audience: aud, Tamper: true}),
		"unknown kid":          as.MustSign(oidctest.Token{Audience: aud, Unpublished: true}),
		"RS256 key as ES256":   raw(oidctest.AlgRS256, "", `{"alg":"ES256","kid":"`+as.Current(oidctest.AlgRS256)+`"}`, good),
		"crit":                 raw(oidctest.AlgES256, "", `{"alg":"ES256","kid":"`+as.Current(oidctest.AlgES256)+`","crit":["exp"]}`, good),
		"repeated header alg":  raw(oidctest.AlgES256, "", `{"alg":"ES256","alg":"ES256","kid":"`+as.Current(oidctest.AlgES256)+`"}`, good),
		"no exp":               raw(oidctest.AlgES256, "", es, without("exp")),
		"exp a string":         raw(oidctest.AlgES256, "", es, with("exp", "2099-01-01")),
		"no sub":               raw(oidctest.AlgES256, "", es, without("sub")),
		"empty sub":            raw(oidctest.AlgES256, "", es, with("sub", "")),
		"aud a number":         raw(oidctest.AlgES256, "", es, with("aud", 7)),
		"iss case variant key": raw(oidctest.AlgES256, "", es, with("ISS", as.Issuer())),
		"not a JWT":            "kk_acme_" + strings.Repeat("a", 32),
		"two segments":         "eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJ1In0",
		"padded base64":        strings.Replace(as.MustSign(oidctest.Token{Audience: aud}), ".", "=.", 1),
	}
	// A claim repeated: the payload written by hand.
	dup := fmt.Sprintf(`{"iss":%q,"sub":"a","sub":"b","aud":%q,"exp":%d}`, as.Issuer(), mcpAud, time.Now().Add(time.Hour).Unix())
	dupTok, err := as.SignRaw(oidctest.AlgES256, "", []byte(es), []byte(dup))
	if err != nil {
		t.Fatal(err)
	}
	cases["repeated sub"] = dupTok
	for name, tok := range cases {
		if _, err := v.Verify(ctx, tok, mcpAud); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: %v, want ErrInvalidToken", name, err)
		}
	}
}

// D276: the key set is fetched once and cached 10 minutes; a kid it does not hold refetches it at most once a minute
// (an unknown kid is refused after one refetch); a rotated key is found by the refetch that its kid triggers.
func TestVerifyRefetchesAtMostOnceAMinute(t *testing.T) {
	as := newAS(t, oidctest.Config{})
	clk := &clock{t: time.Now()}
	v := verifier(t, as, clk)
	ctx := context.Background()
	aud := []string{mcpAud}
	jwks := func() int { return as.Fetches(oidctest.PathJWKS) }
	meta := func() int { return as.Fetches(as.MetadataPath()) }

	for range 5 {
		if _, err := v.Verify(ctx, as.MustSign(oidctest.Token{Audience: aud}), mcpAud); err != nil {
			t.Fatal(err)
		}
	}
	if jwks() != 1 || meta() != 1 {
		t.Fatalf("first verifies: %d key set and %d metadata fetches, want 1 each", jwks(), meta())
	}
	unknown := func() error {
		_, err := v.Verify(ctx, as.MustSign(oidctest.Token{Audience: aud, Unpublished: true}), mcpAud)
		return err
	}
	// Within a minute of the fetch: no refetch.
	if err := unknown(); !errors.Is(err, ErrInvalidToken) || jwks() != 1 {
		t.Fatalf("an unknown kid at once: %v, %d fetches", err, jwks())
	}
	clk.add(61 * time.Second)
	if err := unknown(); !errors.Is(err, ErrInvalidToken) || jwks() != 2 {
		t.Fatalf("an unknown kid a minute later: %v, %d fetches, want one refetch", err, jwks())
	}
	clk.add(time.Second)
	if err := unknown(); !errors.Is(err, ErrInvalidToken) || jwks() != 2 {
		t.Fatalf("an unknown kid right after the refetch: %v, %d fetches", err, jwks())
	}
	if meta() != 1 {
		t.Errorf("a kid refetch read the metadata again (%d)", meta())
	}

	// A rotation: the new kid is not held, and the next refetch is allowed only a minute after the last.
	kid, err := as.Rotate(oidctest.AlgES256)
	if err != nil {
		t.Fatal(err)
	}
	rotated := as.MustSign(oidctest.Token{Audience: aud})
	if _, err := v.Verify(ctx, rotated, mcpAud); !errors.Is(err, ErrInvalidToken) || jwks() != 2 {
		t.Fatalf("the rotated key %s within the minute: %v, %d fetches", kid, err, jwks())
	}
	clk.add(time.Minute)
	if _, err := v.Verify(ctx, rotated, mcpAud); err != nil || jwks() != 3 {
		t.Fatalf("the rotated key after the minute: %v, %d fetches, want it fetched once", err, jwks())
	}
	for range 3 {
		if _, err := v.Verify(ctx, rotated, mcpAud); err != nil || jwks() != 3 {
			t.Fatalf("the rotated key again: %v, %d fetches", err, jwks())
		}
	}
	// The set lives 10 minutes; then the metadata and the set are read again.
	clk.add(KeySetTTL)
	if _, err := v.Verify(ctx, rotated, mcpAud); err != nil || jwks() != 4 || meta() != 2 {
		t.Fatalf("after the TTL: %v, %d key set and %d metadata fetches", err, jwks(), meta())
	}
}

// Concurrent first uses share one fetch.
func TestVerifyFetchesOnceForConcurrentRequests(t *testing.T) {
	as := newAS(t, oidctest.Config{})
	v := verifier(t, as, realClock())
	tok := as.MustSign(oidctest.Token{Audience: []string{mcpAud}})
	var wg sync.WaitGroup
	var failed atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.Verify(context.Background(), tok, mcpAud); err != nil {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if failed.Load() != 0 || as.Fetches(oidctest.PathJWKS) != 1 {
		t.Fatalf("%d failed, %d key set fetches", failed.Load(), as.Fetches(oidctest.PathJWKS))
	}
}

// D276, D296: the metadata is RFC 8414's well-known URI (inserted before an issuer's path), else the OpenID
// configuration; a document naming another issuer, a jwks_uri that is not an operator endpoint, a redirect and an
// oversized body are failures of the issuer (ErrUnavailable, 503), never ErrInvalidToken; while the issuer fails, the
// gate asks it again at most every 5 s, and a refetch that fails keeps the fresh set it had.
func TestDiscovery(t *testing.T) {
	ctx := context.Background()
	t.Run("RFC 8414 for an issuer with a path", func(t *testing.T) {
		as := newAS(t, oidctest.Config{IssuerPath: "/tenant1"})
		v := verifier(t, as, realClock())
		if _, err := v.Verify(ctx, as.MustSign(oidctest.Token{Audience: []string{mcpAud}}), mcpAud); err != nil {
			t.Fatal(err)
		}
		if as.Fetches("/.well-known/oauth-authorization-server/tenant1") != 1 {
			t.Error("the RFC 8414 document was not read at the inserted well-known URI")
		}
	})
	t.Run("OpenID configuration", func(t *testing.T) {
		as := newAS(t, oidctest.Config{Discovery: oidctest.DiscoveryOpenID})
		v := verifier(t, as, realClock())
		if _, err := v.Verify(ctx, as.MustSign(oidctest.Token{Audience: []string{mcpAud}}), mcpAud); err != nil {
			t.Fatal(err)
		}
		if as.Fetches(as.MetadataPath()) != 1 || as.Fetches(as.OpenIDPath()) != 1 {
			t.Errorf("fetches: 8414 %d, openid %d", as.Fetches(as.MetadataPath()), as.Fetches(as.OpenIDPath()))
		}
	})
	if got := MetadataURLs("https://as.example/a/b/"); strings.Join(got, " ") != "https://as.example/.well-known/oauth-authorization-server/a/b "+
		"https://as.example/a/b/.well-known/oauth-authorization-server https://as.example/a/b/.well-known/openid-configuration "+
		"https://as.example/.well-known/openid-configuration/a/b" {
		t.Errorf("MetadataURLs with a path: %q", got)
	}
	if got := MetadataURLs("https://as.example"); strings.Join(got, " ") != "https://as.example/.well-known/oauth-authorization-server "+
		"https://as.example/.well-known/openid-configuration" {
		t.Errorf("MetadataURLs without a path: %q", got)
	}

	failures := map[string]oidctest.Config{
		"another issuer": {Metadata: func(m map[string]any) { m["issuer"] = "https://evil.example" }},
		"http jwks_uri":  {Metadata: func(m map[string]any) { m["jwks_uri"] = "http://keys.example/jwks" }},
		"no jwks_uri":    {Metadata: func(m map[string]any) { delete(m, "jwks_uri") }},
	}
	for name, cfg := range failures {
		t.Run(name, func(t *testing.T) {
			as := newAS(t, cfg)
			v := verifier(t, as, realClock())
			if _, err := v.Verify(ctx, as.MustSign(oidctest.Token{Audience: []string{mcpAud}}), mcpAud); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("%v, want ErrUnavailable", err)
			}
			if as.Fetches(oidctest.PathJWKS) != 0 {
				t.Error("the key set was fetched from a metadata document that failed")
			}
		})
	}
	t.Run("redirect not followed", func(t *testing.T) {
		var target atomic.Int32
		var as *oidctest.Server
		as = newAS(t, oidctest.Config{Hook: func(w http.ResponseWriter, r *http.Request) bool {
			switch r.URL.Path {
			case as.MetadataPath():
				http.Redirect(w, r, "/moved", http.StatusFound)
				return true
			case "/moved":
				target.Add(1)
			}
			return false
		}})
		v := verifier(t, as, realClock())
		if _, err := v.Verify(ctx, as.MustSign(oidctest.Token{Audience: []string{mcpAud}}), mcpAud); !errors.Is(err, ErrUnavailable) || target.Load() != 0 {
			t.Fatalf("%v; the redirect's target got %d requests", err, target.Load())
		}
	})
	t.Run("oversized key set", func(t *testing.T) {
		as := newAS(t, oidctest.Config{Hook: func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != oidctest.PathJWKS {
				return false
			}
			_, _ = io.WriteString(w, `{"keys":[],"pad":"`+strings.Repeat("a", ssrf.MaxOperatorBody)+`"}`)
			return true
		}})
		v := verifier(t, as, realClock())
		if _, err := v.Verify(ctx, as.MustSign(oidctest.Token{Audience: []string{mcpAud}}), mcpAud); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%v, want ErrUnavailable", err)
		}
	})
	t.Run("an issuer that fails is asked at most every 5 s; a failed refetch keeps the set", func(t *testing.T) {
		var down atomic.Bool
		var calls atomic.Int32
		as := newAS(t, oidctest.Config{Hook: func(w http.ResponseWriter, r *http.Request) bool {
			calls.Add(1)
			if down.Load() {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return true
			}
			return false
		}})
		clk := &clock{t: time.Now()}
		v := verifier(t, as, clk)
		tok := as.MustSign(oidctest.Token{Audience: []string{mcpAud}})
		down.Store(true)
		for range 3 {
			if _, err := v.Verify(ctx, tok, mcpAud); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("%v, want ErrUnavailable", err)
			}
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("%d requests to a failing issuer within 5 s, want 1", n)
		}
		clk.add(6 * time.Second)
		down.Store(false)
		if _, err := v.Verify(ctx, tok, mcpAud); err != nil {
			t.Fatal(err)
		}
		clk.add(2 * time.Minute)
		down.Store(true)
		if _, err := v.Verify(ctx, as.MustSign(oidctest.Token{Audience: []string{mcpAud}, Unpublished: true}), mcpAud); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("an unknown kid while the refetch fails: %v", err)
		}
		if _, err := v.Verify(ctx, tok, mcpAud); err != nil {
			t.Fatalf("a held key while the issuer is down: %v", err)
		}
	})
}

// RFC 7517 as the gate reads it: keys it cannot use are skipped, a set with none is refused.
func TestParseKeySet(t *testing.T) {
	as := newAS(t, oidctest.Config{})
	resp, err := as.Client().Get(as.URL() + oidctest.PathJWKS)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	set, err := parseKeySet(b)
	if err != nil || len(set.keys) != 3 {
		t.Fatalf("the server's set: %v %d", err, len(set.keys))
	}
	var doc struct{ Keys []map[string]any }
	_ = json.Unmarshal(b, &doc)
	ec := doc.Keys[0]
	edit := func(f func(m map[string]any)) json.RawMessage {
		m := map[string]any{}
		for k, v := range ec {
			m[k] = v
		}
		f(m)
		out, _ := json.Marshal(m)
		return out
	}
	for name, k := range map[string]json.RawMessage{
		"use enc":        edit(func(m map[string]any) { m["use"] = "enc" }),
		"no verify op":   edit(func(m map[string]any) { m["key_ops"] = []string{"sign"} }),
		"P-384":          edit(func(m map[string]any) { m["crv"] = "P-384" }),
		"private member": edit(func(m map[string]any) { m["d"] = "AAAA" }),
		"alg mismatch":   edit(func(m map[string]any) { m["alg"] = "RS256" }),
		"short x":        edit(func(m map[string]any) { m["x"] = "AAAA" }),
		"oct":            json.RawMessage(`{"kty":"oct","k":"c2VjcmV0"}`),
		"short RSA":      json.RawMessage(`{"kty":"RSA","n":"` + strings.Repeat("A", 171) + `","e":"AQAB"}`),
	} {
		if _, err := parseJWK(k); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := parseKeySet([]byte(`{"keys":[` + string(k) + `]}`)); err == nil {
			t.Errorf("%s: a set of only it was accepted", name)
		}
	}
}

// §12.1: the canonical URIs, the challenge (exact), and the resource server's audience per route.
func TestResourceServer(t *testing.T) {
	if CanonicalURI("https://gate.example/", "") != "https://gate.example/mcp" || CanonicalURI("https://gate.example", "acme/fs") != "https://gate.example/mcp/acme/fs" {
		t.Error("CanonicalURI")
	}
	for _, c := range []struct {
		svc     string
		refused bool
		want    string
	}{
		{"", false, `Bearer resource_metadata="https://gate.example/.well-known/oauth-protected-resource/mcp", scope="mcp"`},
		{"fs", false, `Bearer resource_metadata="https://gate.example/.well-known/oauth-protected-resource/mcp/fs", scope="mcp"`},
		{"fs", true, `Bearer resource_metadata="https://gate.example/.well-known/oauth-protected-resource/mcp/fs", scope="mcp", error="invalid_token"`},
		{`Fs"x`, false, `Bearer scope="mcp"`},
		{"a/b", false, `Bearer scope="mcp"`}, // two segments on a single-tenant gate
	} {
		as := newAS(t, oidctest.Config{})
		v := verifier(t, as, realClock())
		rs, err := NewResourceServer(gateURL, false, issuer(t, v, IssuerConfig{}))
		if err != nil {
			t.Fatal(err)
		}
		if got := rs.Challenge(c.svc, c.refused); got != c.want {
			t.Errorf("Challenge(%q, %v) = %s, want %s", c.svc, c.refused, got, c.want)
		}
	}
	if got := Challenge(`https://gate.example/"x\`, "", false, true); got != `Bearer resource_metadata="https://gate.example/\"x\\/.well-known/oauth-protected-resource/mcp", scope="mcp"` {
		t.Errorf("quoting: %s", got)
	}

	as := newAS(t, oidctest.Config{})
	rs, _ := NewResourceServer(gateURL, true, issuer(t, verifier(t, as, realClock()), IssuerConfig{}))
	ctx := context.Background()
	gateTok := as.MustSign(oidctest.Token{Subject: "alice", Audience: []string{CanonicalURI(gateURL, "")}})
	p, err := rs.VerifyMCP(ctx, gateTok, "")
	if err != nil || p.ID != "oidc:sub:alice" || p.Via != auth.ViaMCP {
		t.Fatalf("a /mcp token on /mcp: %+v %v", p, err)
	}
	if _, err := rs.VerifyMCP(ctx, gateTok, "acme/fs"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("a /mcp token on /mcp/acme/fs: %v", err)
	}
	svcTok := as.MustSign(oidctest.Token{Audience: []string{CanonicalURI(gateURL, "acme/fs")}})
	if _, err := rs.VerifyMCP(ctx, svcTok, "acme/fs"); err != nil {
		t.Errorf("a /mcp/acme/fs token there: %v", err)
	}
	for _, svc := range []string{"", "fs", "acme/other", "ACME/fs"} {
		if _, err := rs.VerifyMCP(ctx, svcTok, svc); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("a /mcp/acme/fs token on %q: %v", svc, err)
		}
	}
	as.Close()
	fresh, _ := NewResourceServer(gateURL, true, issuer(t, verifier(t, as, realClock()), IssuerConfig{}))
	if _, err := fresh.VerifyMCP(ctx, gateTok, ""); err == nil || errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("an issuer down: %v, want the gate's own error", err)
	}
}

// §12.1: the RFC 9728 document of every well-formed name, existing or not; 404 for anything else.
func TestMetadataHandler(t *testing.T) {
	for _, multi := range []bool{false, true} {
		h := MetadataHandler("https://gate.example/", "https://login.example", multi)
		get := func(path string) (int, ProtectedResource, string) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			var doc ProtectedResource
			_ = json.Unmarshal(rec.Body.Bytes(), &doc)
			return rec.Code, doc, rec.Body.String()
		}
		svc := "fs"
		if multi {
			svc = "acme/fs"
		}
		for path, resource := range map[string]string{
			MetadataPrefix + "/mcp":              "https://gate.example/mcp",
			MetadataPrefix + "/mcp/" + svc:       "https://gate.example/mcp/" + svc,
			MetadataPrefix + "/mcp/" + svc + "x": "https://gate.example/mcp/" + svc + "x",
		} {
			code, doc, body := get(path)
			if code != http.StatusOK || doc.Resource != resource || len(doc.AuthorizationServers) != 1 ||
				doc.AuthorizationServers[0] != "https://login.example" || strings.Join(doc.ScopesSupported, ",") != "mcp" ||
				strings.Join(doc.BearerMethodsSupported, ",") != "header" {
				t.Errorf("multi %v, %s: %d %s", multi, path, code, body)
			}
		}
		bad := []string{MetadataPrefix + "/mcp/", MetadataPrefix + "/mcp/Fs", MetadataPrefix + "/mcpx", MetadataPrefix + "/mcp/a/b/c"}
		if multi {
			bad = append(bad, MetadataPrefix+"/mcp/fs")
		} else {
			bad = append(bad, MetadataPrefix+"/mcp/acme/fs")
		}
		for _, path := range bad {
			if code, _, _ := get(path); code != http.StatusNotFound {
				t.Errorf("multi %v, %s: %d, want 404", multi, path, code)
			}
		}
	}
	if got := MetadataPatterns(true); len(got) != 2 || got[1] != "GET /.well-known/oauth-protected-resource/mcp/{tenant}/{name}" {
		t.Errorf("patterns %q", got)
	}
}

func TestNewVerifierRefusesAnIssuerThatIsNotHTTPS(t *testing.T) {
	for _, iss := range []string{"http://login.example", "https://u:p@login.example", "https://login.example?x=1", "login.example"} {
		if _, err := NewVerifier(Config{Issuer: iss}); err == nil {
			t.Errorf("%s accepted", iss)
		}
	}
}
