package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/config"
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

// The key cache is trusted while the last good re-read began less than RefreshInterval (5 s) ago.
func TestFreshBound(t *testing.T) {
	rc := NewRevocationCache(nil)
	t0 := time.Unix(1_700_000_000, 0)
	now := t0
	rc.now = func() time.Time { return now }
	if rc.Fresh() {
		t.Error("fresh before any read")
	}
	rc.lastOK = t0
	for d, want := range map[time.Duration]bool{
		0:                                    true,
		RefreshInterval - time.Millisecond:   true,
		RefreshInterval:                      false,
		RefreshInterval + time.Millisecond:   false,
		2*RefreshInterval - time.Millisecond: false,
	} {
		now = t0.Add(d)
		if got := rc.Fresh(); got != want {
			t.Errorf("%v after the last good read began: Fresh %v, want %v", d, got, want)
		}
	}
}

// A key whose row was read before a re-read began is not kept in memory: that re-read may have listed the
// keys before it joined them, so it was not checked, and its row may predate what the re-read saw.
func TestRememberAfterAReReadBegan(t *testing.T) {
	rc := NewRevocationCache(nil)
	k := NewAPIKeys(nil, rc)
	id := keyID1
	ck := func() *cachedKey {
		return &cachedKey{p: keyPrincipal("kk_a_12345678", "default", acmeTenant), loaded: k.now()}
	}
	before := rc.started()
	rc.mu.Lock()
	rc.seq++ // a re-read begins while the row is being read
	rc.mu.Unlock()
	k.remember(KeyHash("kk_a_1"), ck(), before)
	if ids := k.cachedKeyIDs(); len(ids) != 0 {
		t.Errorf("kept a row read before a re-read began: %v", ids)
	}
	k.remember(KeyHash("kk_a_1"), ck(), rc.started())
	if ids := k.cachedKeyIDs(); len(ids) != 1 || ids[0] != id {
		t.Errorf("a row read after the re-read began: %v", ids)
	}
	k.dropKeys([]uuid.UUID{id})
	if ids := k.cachedKeyIDs(); len(ids) != 0 {
		t.Errorf("dropKeys left %v", ids)
	}
}

// hireSource is a tunnel session's principal source (internal/tunnelgate's), for the Authenticator's tests.
type hireSource struct {
	p   Principal
	err error
	n   int
}

func (h *hireSource) HirePrincipal(context.Context) (Principal, error) {
	h.n++
	return h.p, h.err
}

// A request a node opened on its tunnel session takes the session's principal (spec/tunnel.md §9): a credential
// it carries is never read, not even one that would be refused, nor one that would authenticate as someone
// else; a session whose hiring key was revoked is 401 (§12).
func TestAuthenticatorHiringRequest(t *testing.T) {
	a := NewAuthenticator(nil) // no API keys: every bearer credential of the network is refused
	node := Principal{Kind: KindNode, ID: "node:k1_abcdefghijklmnopqrstuvwxyz", TenantID: &acmeTenant, TenantSlug: "acme",
		BuyerID: buyer1, Via: ViaNode}
	src := &hireSource{p: node}
	for _, authz := range []string{"", "Bearer kk_acme_x", "Basic Zm9vOmJhcg=="} {
		r := httptest.NewRequest("POST", "/agents/hello", nil)
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		r = r.WithContext(WithHireSource(r.Context(), src))
		p, authed, err := a.Authenticate(r, uuid.Nil)
		if err != nil || !authed || p.ID != node.ID || p.Via != ViaNode || p.BuyerID != buyer1 {
			t.Errorf("Authorization %q: %+v %v %v, want the session's principal", authz, p, authed, err)
		}
	}
	if !mustList(t, "org:acme").Admits(node) || !mustList(t, "node:k1_abcdefghijklmnopqrstuvwxyz").Admits(node) ||
		mustList(t, "org:other").Admits(node) {
		t.Error("allow: lists admit a node principal wrongly")
	}
	src.err = ErrUnauthenticated
	r := httptest.NewRequest("GET", "/agents/hello", nil)
	r = r.WithContext(WithHireSource(r.Context(), src))
	if _, _, err := a.Authenticate(r, uuid.Nil); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a revoked hiring key: %v, want ErrUnauthenticated", err)
	}
	if _, ok := HireSourceFrom(httptest.NewRequest("GET", "/", nil).Context()); ok {
		t.Error("a network request carries a hire source")
	}
}

// Subscribe tells each API key revocation once, from a NOTIFY (and from a re-read, in the Postgres tests).
func TestRevocationSubscribe(t *testing.T) {
	c := NewRevocationCache(nil)
	var got []uuid.UUID
	c.Subscribe(func(id uuid.UUID) { got = append(got, id) })
	c.Notify("key:k1_abcdefghijklmnopqrstuvwxyz")
	c.Notify(RevokePayload(keyID1))
	c.Notify(RevokePayload(keyID1)) // the same revocation again
	if len(got) != 1 || got[0] != keyID1 {
		t.Fatalf("subscribers told %v, want %s once", got, keyID1)
	}
}

// Hold and Revalidate without Postgres: a held key a re-read found ended is refused for as long as it is held,
// and Hold of a principal that is not an API key's holds nothing.
func TestHoldEndedKey(t *testing.T) {
	rc := NewRevocationCache(nil)
	k := NewAPIKeys(nil, rc)
	p := keyPrincipal("kk_a_12345678", "acme", acmeTenant)
	release := k.Hold(p)
	again := k.Hold(p)
	if ids := k.cachedKeyIDs(); len(ids) != 1 || ids[0] != keyID1 {
		t.Fatalf("a held key is not among the keys every re-read checks: %v", ids)
	}
	k.dropKeys([]uuid.UUID{keyID1}) // a re-read found it ended
	if _, err := k.Revalidate(context.Background(), p); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("an ended held key: %v", err)
	}
	if ids := k.cachedKeyIDs(); len(ids) != 0 {
		t.Fatalf("an ended key is still checked: %v", ids)
	}
	release()
	release() // twice is once
	if _, ok := k.held[keyID1]; !ok {
		t.Fatal("one release freed a key two sessions hold")
	}
	again()
	if len(k.held) != 0 {
		t.Fatalf("held after every release: %v", k.held)
	}
	k.Hold(Principal{Kind: KindNode, ID: "node:k1_abcdefghijklmnopqrstuvwxyz"})()
	if len(k.held) != 0 {
		t.Fatal("a node principal is held")
	}
	if _, err := k.Revalidate(context.Background(), Principal{Kind: KindNode}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Revalidate of a node principal: %v", err)
	}
}
