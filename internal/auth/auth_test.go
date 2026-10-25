package auth

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/medmahmoudi26/kontra/internal/config"
)

var (
	acmeTenant  = uuid.MustParse("0192f0b4-0000-7000-8000-0000000000a1")
	otherTenant = uuid.MustParse("0192f0b4-0000-7000-8000-0000000000b2")
	buyer1      = uuid.MustParse("0192f0b4-0000-7000-8000-000000000001")
	keyID1      = uuid.MustParse("0192f0b4-0000-7000-8000-0000000000c1")
)

func keyPrincipal(prefix, slug string, tenant uuid.UUID, scopes ...Scope) Principal {
	if scopes == nil {
		scopes = []Scope{ScopeHire}
	}
	return Principal{Kind: KindKey, ID: "key:" + prefix, TenantID: &tenant, TenantSlug: slug, BuyerID: buyer1, Via: ViaAPIKey, Scopes: scopes, KeyID: &keyID1}
}

func mustList(t *testing.T, entries ...string) AllowList {
	t.Helper()
	l, err := ParseAllowList(entries)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// allow: lists (spec §2.3, D20, D68): key: globs, org:<tenant> (its keys), oidc:sub:, node:, anonymous
// (everyone, the caller keeping its own principal), deny by default, and no service for a key without
// the hire scope.
func TestAllowList(t *testing.T) {
	acmeKey := keyPrincipal("kk_acme_ab12cd34", "acme", acmeTenant)
	acmeKey2 := keyPrincipal("kk_acme_Zz99Yy88", "acme", acmeTenant)
	otherKey := keyPrincipal("kk_acmex_ab12cd34", "other", otherTenant)
	shortKey := keyPrincipal("kk_a_00000001", "other", otherTenant)
	enrollOnly := keyPrincipal("kk_acme_ab12cd35", "acme", acmeTenant, ScopeEnrollAll)
	oidc := Principal{Kind: KindOIDC, ID: "oidc:sub:auth0|42", BuyerID: buyer1, Via: ViaOIDC}
	oidcMCP := Principal{Kind: KindOIDC, ID: "oidc:sub:auth0|42", BuyerID: buyer1, Via: ViaMCP}
	node := Principal{Kind: KindNode, ID: "node:k1_abcdefghijklmnopqrstuvwxyz", TenantID: &acmeTenant, TenantSlug: "acme", BuyerID: buyer1, Via: ViaNode}
	anon := Anonymous(buyer1)
	all := []Principal{acmeKey, acmeKey2, otherKey, shortKey, enrollOnly, oidc, oidcMCP, node, anon}
	name := func(p Principal) string {
		return p.ID + "/" + string(p.Via) + "/" + strings.Join(func() []string {
			var s []string
			for _, x := range p.Scopes {
				s = append(s, string(x))
			}
			return s
		}(), ",")
	}

	cases := []struct {
		entries []string
		admits  []Principal
	}{
		{nil, nil},
		{[]string{}, nil},
		{[]string{"key:kk_acme_*"}, []Principal{acmeKey, acmeKey2}},
		{[]string{"key:kk_acme_ab12cd34"}, []Principal{acmeKey}},
		{[]string{"key:kk_acme_ab12cd3?"}, []Principal{acmeKey}},
		{[]string{"key:kk_*_ab12cd34"}, []Principal{acmeKey, otherKey}},
		{[]string{"key:*"}, []Principal{acmeKey, acmeKey2, otherKey, shortKey}},
		{[]string{"org:acme"}, []Principal{acmeKey, acmeKey2, node}},
		{[]string{"org:other"}, []Principal{otherKey, shortKey}},
		{[]string{"org:nobody"}, nil},
		{[]string{"oidc:sub:auth0|42"}, []Principal{oidc, oidcMCP}},
		{[]string{"oidc:sub:auth0|43"}, nil},
		{[]string{"node:k1_abcdefghijklmnopqrstuvwxyz"}, []Principal{node}},
		{[]string{"anonymous"}, []Principal{acmeKey, acmeKey2, otherKey, shortKey, oidc, oidcMCP, node, anon}},
		{[]string{"key:kk_a_*", "org:acme"}, []Principal{acmeKey, acmeKey2, shortKey, node}},
	}
	for _, tc := range cases {
		l := mustList(t, tc.entries...)
		for _, p := range all {
			want := false
			for _, a := range tc.admits {
				if name(a) == name(p) {
					want = true
				}
			}
			if got := l.Admits(p); got != want {
				t.Errorf("allow %q admits %s = %v, want %v", tc.entries, name(p), got, want)
			}
		}
	}
	// A principal with a tenant id but no slug, or a slug but no id, is never an org: member.
	noSlug := acmeKey
	noSlug.TenantSlug = ""
	if mustList(t, "org:acme").Admits(noSlug) {
		t.Error("org:acme admitted a principal whose tenant slug is unknown")
	}
	// The zero list denies.
	if (AllowList{}).Admits(acmeKey) || (AllowList{}).Admits(anon) {
		t.Error("the zero AllowList admits")
	}
}

func TestAllowListAnonymousFlags(t *testing.T) {
	for _, tc := range []struct {
		entries         []string
		anonymous, only bool
	}{
		{nil, false, false},
		{[]string{"anonymous"}, true, true},
		{[]string{"anonymous", "anonymous"}, true, true},
		{[]string{"anonymous", "org:acme"}, true, false},
		{[]string{"org:acme"}, false, false},
	} {
		l := mustList(t, tc.entries...)
		if l.AdmitsAnonymous() != tc.anonymous || l.OnlyAnonymous() != tc.only {
			t.Errorf("%q: AdmitsAnonymous %v OnlyAnonymous %v", tc.entries, l.AdmitsAnonymous(), l.OnlyAnonymous())
		}
	}
}

// The allow: grammar is config's: unknown kinds and key: globs that can never match are errors.
func TestParseAllowListErrors(t *testing.T) {
	for _, e := range []string{"key:acme-*", "admin", "org:Acme", "user:x", " anonymous", "node:k2_x", "key:abc", "oidc:sub:"} {
		if _, err := ParseAllowList([]string{"org:acme", e}); err == nil {
			t.Errorf("%q parsed", e)
		}
	}
}

// An authenticated caller keeps its own principal, buyer and via on an anonymous service; no credential is
// the anonymous principal with the service's buyer; a bad credential is 401 even there.
func TestAuthenticatorWithoutKeys(t *testing.T) {
	a := NewAuthenticator(nil)
	anonBuyer := uuid.MustParse("0192f0b4-0000-7000-8000-0000000000ff")
	r := httptest.NewRequest("POST", "/agents/hello", nil)
	p, authed, err := a.Authenticate(r, anonBuyer)
	if err != nil || authed || p.Kind != KindAnonymous || p.ID != "anonymous" || p.BuyerID != anonBuyer || p.Via != ViaAnonymous || p.TenantID != nil {
		t.Errorf("no credential: %+v %v %v", p, authed, err)
	}
	if !mustList(t, "anonymous").Admits(p) || mustList(t, "org:acme").Admits(p) {
		t.Error("the anonymous principal is admitted by the wrong lists")
	}
	r.Header.Set("Authorization", "Bearer kk_acme_x")
	if _, _, err := a.Authenticate(r, anonBuyer); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a bearer with API keys off = %v, want ErrUnauthenticated", err)
	}
}

func TestBearerToken(t *testing.T) {
	for _, tc := range []struct {
		headers []string
		token   string
		present bool
		ok      bool
	}{
		{nil, "", false, true},
		{[]string{"Bearer kk_a_x"}, "kk_a_x", true, true},
		{[]string{"bearer kk_a_x"}, "kk_a_x", true, true},
		{[]string{"BEARER  kk_a_x"}, "kk_a_x", true, true},
		{[]string{"Bearer"}, "", true, false},
		{[]string{"Bearer "}, "", true, false},
		{[]string{"Basic dXNlcjpwYXNz"}, "", true, false},
		{[]string{"Bearer a b"}, "", true, false},
		{[]string{"Bearer a\tb"}, "", true, false},
		{[]string{""}, "", true, false},
		{[]string{"Bearer a", "Bearer b"}, "", true, false},
	} {
		h := http.Header{}
		for _, v := range tc.headers {
			h.Add("Authorization", v)
		}
		tok, present, err := BearerToken(h)
		if tok != tc.token || present != tc.present || (err == nil) != tc.ok {
			t.Errorf("%q: %q %v %v", tc.headers, tok, present, err)
		}
		if err != nil && !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%q: error %v is not ErrUnauthenticated", tc.headers, err)
		}
	}
}

// Keys are kk_<operator prefix>_<32 base62>; the stored prefix keeps the random part's first 8
// characters, at most 28 characters, within api_keys.prefix (D68).
func TestKeyFormat(t *testing.T) {
	for name, want := range map[string]string{
		"demo":                           "demo",
		"Demo Key #1":                    "demokey1",
		"ACME-recon":                     "acmerecon",
		"ünïcödé":                        "ncd",
		"---":                            "k",
		"":                               "k",
		"a-very-long-key-name-for-tests": "averylongkeyname",
	} {
		if got := OperatorPrefix(name); got != want {
			t.Errorf("OperatorPrefix(%q) = %q, want %q", name, got, want)
		}
	}
	seen := map[string]bool{}
	for _, op := range []string{"k", "demo", "averylongkeyname"} {
		for i := 0; i < 50; i++ {
			secret, prefix, err := GenerateKey(nil, op)
			if err != nil {
				t.Fatal(err)
			}
			if !GateKeyRE.MatchString(secret) || !strings.HasPrefix(secret, "kk_"+op+"_") || len(secret) != 3+len(op)+1+32 {
				t.Fatalf("key %q", secret)
			}
			if prefix != secret[:3+len(op)+1+8] || len(prefix) > 28 || !config.KeyPrefixRE.MatchString(prefix) {
				t.Fatalf("prefix %q of %q", prefix, secret)
			}
			if seen[secret] {
				t.Fatal("a key repeated")
			}
			seen[secret] = true
		}
	}
	for _, bad := range []string{"", "Acme", "acme_x", "a-b", "seventeen-chars-x", strings.Repeat("a", 17)} {
		if _, _, err := GenerateKey(nil, bad); err == nil {
			t.Errorf("operator prefix %q accepted", bad)
		}
	}
	// Every base62 character is reachable and nothing else appears.
	secret, _, err := GenerateKey(bytes.NewReader(bytes.Repeat([]byte{255, 0, 61, 62, 247, 248}, 100)), "k")
	if err != nil {
		t.Fatal(err)
	}
	// 255 and 248 are rejected; 0 → '0', 61 → 'z', 62 → '0', 247 → 247%62 = 61 → 'z'.
	if want := "kk_k_" + strings.Repeat("0z0z", 8); secret != want {
		t.Errorf("deterministic key %q, want %q", secret, want)
	}
}

func TestParseScope(t *testing.T) {
	for _, s := range []string{"hire", "enroll:*", "enroll:hello", "enroll:a-b"} {
		if got, err := ParseScope(s); err != nil || string(got) != s {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"", "admin", "enroll:", "enroll:Hello", "hire ", "enroll:**"} {
		if _, err := ParseScope(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	if EnrollScope("hello") != "enroll:hello" {
		t.Error("EnrollScope")
	}
}

func TestCheckKeyName(t *testing.T) {
	for _, ok := range []string{"demo", "Demo Key", "ünïcödé", strings.Repeat("x", 128)} {
		if err := CheckKeyName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("x", 129), "a\nb", "a\x00b", "\xff"} {
		if err := CheckKeyName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRevocationNotifyPayloads(t *testing.T) {
	c := NewRevocationCache(nil)
	id := uuid.MustParse("0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11")
	for _, p := range []string{"key:k1_abcdefghijklmnopqrstuvwxyz", "binding:k1_x/hello", "apikey:not-a-uuid", "garbage", ""} {
		c.Notify(p)
	}
	if c.APIKeyRevoked(id) {
		t.Error("revoked before its payload")
	}
	c.Notify(RevokePayload(id))
	if !c.APIKeyRevoked(id) {
		t.Error("apikey:<id> did not revoke")
	}
	if c.Fresh() {
		t.Error("a cache that never read Postgres is fresh")
	}
}

func TestPrincipalScopes(t *testing.T) {
	if !keyPrincipal("kk_a_00000001", "acme", acmeTenant).MayHire() {
		t.Error("a hire key may not hire")
	}
	if keyPrincipal("kk_a_00000001", "acme", acmeTenant, ScopeEnrollAll).MayHire() {
		t.Error("an enroll-only key may hire")
	}
	if !Anonymous(buyer1).MayHire() || Anonymous(buyer1).Authenticated() {
		t.Error("anonymous")
	}
	if (Principal{}).MayHire() {
		t.Error("the zero principal may hire")
	}
}
