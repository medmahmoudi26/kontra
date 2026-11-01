package oidc

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/oidc/oidctest"
	"kontra.sh/kontra/internal/ssrf"
)

const a2aAud = "kontra"

// feed is as's revocation feed read through the operator-endpoint client trusting as's CA, on clk.
func feed(t *testing.T, as *oidctest.Server, clk *clock, poll, stale time.Duration) *Feed {
	t.Helper()
	f, err := NewFeed(FeedConfig{
		Issuer: as.Issuer(), URL: as.RevocationsURL(), Poll: poll, MaxStale: stale, Now: clk.now,
		Client: ssrf.NewOperatorClient(ssrf.OperatorOptions{TLS: &tls.Config{RootCAs: as.CAPool(), MinVersion: tls.VersionTLS12}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// tenants is a Tenants double: slug → id; err answers every lookup with it.
type tenants struct {
	mu    sync.Mutex
	known map[string]uuid.UUID
	err   error
	asked []string
}

func (ts *tenants) Tenant(_ context.Context, slug string) (uuid.UUID, bool, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.asked = append(ts.asked, slug)
	if ts.err != nil {
		return uuid.Nil, false, ts.err
	}
	id, ok := ts.known[slug]
	return id, ok, nil
}

// D316: the feed's answer is {window_seconds, sessions: [sha256 hex]}; other members are ignored; anything else, one
// bad entry included, is not a feed.
func TestParseFeed(t *testing.T) {
	d := sha256.Sum256([]byte("0123456789abcdef0123456789abcdef"))
	hexd := fmt.Sprintf("%x", d)
	set, window, err := ParseFeed([]byte(`{"window_seconds": 1200, "sessions": ["` + hexd + `", "` + strings.ToUpper(hexd[:2]) + hexd[2:] + `"], "next": null}`))
	if err != nil || window != 20*time.Minute || len(set) != 1 {
		t.Fatalf("ParseFeed = %d entries, %v, %v", len(set), window, err)
	}
	if _, ok := set[SessionDigest("0123456789abcdef0123456789abcdef")]; !ok {
		t.Error("the session's digest is not in the set")
	}
	if set, _, err := ParseFeed([]byte(`{"window_seconds": 1, "sessions": []}`)); err != nil || len(set) != 0 {
		t.Errorf("an empty feed: %v %v", set, err)
	}
	for name, body := range map[string]string{
		"not JSON":             `{"window_seconds": 1200, "sessions": [}`,
		"an array":             `[]`,
		"no window":            `{"sessions": []}`,
		"window zero":          `{"window_seconds": 0, "sessions": []}`,
		"window fractional":    `{"window_seconds": 1.5, "sessions": []}`,
		"window a string":      `{"window_seconds": "1200", "sessions": []}`,
		"window over 30 days":  `{"window_seconds": 2592001, "sessions": []}`,
		"no sessions":          `{"window_seconds": 1200}`,
		"sessions null":        `{"window_seconds": 1200, "sessions": null}`,
		"sessions an object":   `{"window_seconds": 1200, "sessions": {}}`,
		"a short digest":       `{"window_seconds": 1200, "sessions": ["` + hexd[:62] + `"]}`,
		"a digest not hex":     `{"window_seconds": 1200, "sessions": ["` + strings.Repeat("g", 64) + `"]}`,
		"a number entry":       `{"window_seconds": 1200, "sessions": [1]}`,
		"a repeated key":       `{"window_seconds": 1200, "sessions": [], "sessions": ["` + hexd + `"]}`,
		"a case-variant key":   `{"window_seconds": 1200, "sessions": [], "Sessions": ["` + hexd + `"]}`,
		"trailing data":        `{"window_seconds": 1200, "sessions": []} {}`,
		"one bad entry of two": `{"window_seconds": 1200, "sessions": ["` + hexd + `", "x"]}`,
	} {
		if _, _, err := ParseFeed([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var b strings.Builder
	b.WriteString(`{"window_seconds": 1200, "sessions": [`)
	for i := range MaxFeedSessions + 1 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"%064x"`, i)
	}
	b.WriteString(`]}`)
	if _, _, err := ParseFeed([]byte(b.String())); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("a feed of %d sessions: %v", MaxFeedSessions+1, err)
	}
	if b.Len() > MaxFeedBody {
		t.Errorf("a feed of %d sessions is %d bytes, over MaxFeedBody", MaxFeedSessions+1, b.Len())
	}
}

// NewFeed takes an operator-configured endpoint only, and a poll it can keep.
func TestNewFeedRefuses(t *testing.T) {
	for _, c := range []FeedConfig{
		{URL: "http://cp.example.com/r", Poll: time.Second, MaxStale: 2 * time.Second},
		{URL: "https://u:p@cp.example.com/r", Poll: time.Second, MaxStale: 2 * time.Second},
		{URL: "https://cp.example.com/r", Poll: 0, MaxStale: 2 * time.Second},
		{URL: "https://cp.example.com/r", Poll: 2 * time.Second, MaxStale: time.Second},
	} {
		if _, err := NewFeed(c); err == nil {
			t.Errorf("NewFeed(%+v) accepted", c)
		}
	}
	if _, err := NewFeed(FeedConfig{URL: "http://127.0.0.1:9/r", Poll: time.Second, MaxStale: time.Second}); err != nil {
		t.Errorf("a loopback http:// feed: %v", err)
	}
}

// D316 through an Issuer: with a feed, a token needs a sid and an iat; a revoked session's token is refused as soon as
// a read lists it, and on every route kind; a token living longer than the window allows is refused; a feed never
// read, or stale past max_stale, refuses every token as the gate's own failure (never a 401), and a read brings the
// tokens back; a failed read keeps the list it had.
func TestIssuerRevocationFeed(t *testing.T) {
	as := newAS(t, oidctest.Config{})
	clk := realClock()
	v := verifier(t, as, clk)
	f := feed(t, as, clk, 5*time.Second, 30*time.Second)
	iss := issuer(t, v, IssuerConfig{Feed: f})
	ctx := context.Background()
	exp := time.Now().Add(15 * time.Minute)
	tok := func(sid string) string {
		return as.MustSign(oidctest.Token{Subject: "alice", Audience: []string{a2aAud}, Session: sid, Expiry: exp})
	}
	alice := tok("s-alice")

	// Never read: the gate's own failure.
	if _, err := iss.Principal(ctx, alice, a2aAud, auth.ViaOIDC); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("before the first read: %v, want ErrUnavailable", err)
	}
	if err := f.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := iss.Principal(ctx, alice, a2aAud, auth.ViaOIDC)
	if err != nil || p.ID != "oidc:sub:alice" || p.Via != auth.ViaOIDC || p.BuyerID != BuyerID(as.Issuer(), "alice") || p.TenantID != nil {
		t.Fatalf("a token with a sid: %+v %v", p, err)
	}

	// What a feed needs of a token.
	payload := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		tk, err := as.SignRaw(oidctest.AlgES256, "", []byte(`{"alg":"ES256","kid":"`+as.Current(oidctest.AlgES256)+`"}`), b)
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	base := func() map[string]any {
		return map[string]any{"iss": as.Issuer(), "sub": "alice", "aud": a2aAud, "exp": exp.Unix(), "iat": time.Now().Unix(), "sid": "s-1"}
	}
	for name, edit := range map[string]func(m map[string]any){
		"no sid":           func(m map[string]any) { delete(m, "sid") },
		"an empty sid":     func(m map[string]any) { m["sid"] = "" },
		"a numeric sid":    func(m map[string]any) { m["sid"] = 42 },
		"a long sid":       func(m map[string]any) { m["sid"] = strings.Repeat("s", MaxSessionID+1) },
		"no iat":           func(m map[string]any) { delete(m, "iat") },
		"iat a string":     func(m map[string]any) { m["iat"] = "now" },
		"iat in 2 minutes": func(m map[string]any) { m["iat"] = time.Now().Add(2 * time.Minute).Unix() },
		// 1200 s of window, less twice the skew: 1080 s at most.
		"a lifetime of 1081 s": func(m map[string]any) { m["iat"] = time.Now().Unix(); m["exp"] = time.Now().Unix() + 1081 },
	} {
		m := base()
		edit(m)
		if _, err := iss.Principal(ctx, payload(m), a2aAud, auth.ViaOIDC); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: %v, want ErrInvalidToken", name, err)
		}
	}
	m := base()
	m["exp"] = m["iat"].(int64) + 1080
	if _, err := iss.Principal(ctx, payload(m), a2aAud, auth.ViaOIDC); err != nil {
		t.Errorf("a lifetime of 1080 s: %v", err)
	}
	m = base()
	m["iat"] = time.Now().Add(50 * time.Second).Unix() // within the skew
	if _, err := iss.Principal(ctx, payload(m), a2aAud, auth.ViaOIDC); err != nil {
		t.Errorf("iat 50 s ahead: %v", err)
	}

	// A revocation: listed at the next read, refused on every audience; another session of the same subject is not.
	as.Revoke("s-alice")
	if _, err := iss.Principal(ctx, alice, a2aAud, auth.ViaOIDC); err != nil {
		t.Errorf("revoked but not read yet: %v", err)
	}
	if err := f.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := iss.Principal(ctx, alice, a2aAud, auth.ViaOIDC); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a revoked session's token: %v, want ErrInvalidToken", err)
	}
	mcpTok := as.MustSign(oidctest.Token{Subject: "alice", Audience: []string{mcpAud}, Session: "s-alice", Expiry: exp})
	if _, err := iss.Principal(ctx, mcpTok, mcpAud, auth.ViaMCP); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a revoked session's MCP token: %v, want ErrInvalidToken", err)
	}
	other := tok("s-alice-2")
	if _, err := iss.Principal(ctx, other, a2aAud, auth.ViaOIDC); err != nil {
		t.Errorf("another session: %v", err)
	}

	// The feed down: the list it had holds; past max_stale every token is the gate's own failure, a revoked one too
	// keeps its 401; the feed back, the tokens are accepted again.
	as.FeedDown(http.StatusServiceUnavailable)
	if err := f.Refresh(ctx); err == nil {
		t.Fatal("a 503 from the feed was read")
	}
	clk.add(29 * time.Second)
	if _, err := iss.Principal(ctx, other, a2aAud, auth.ViaOIDC); err != nil || !f.Fresh() {
		t.Errorf("29 s after the last read: %v (fresh %v)", err, f.Fresh())
	}
	if _, err := iss.Principal(ctx, alice, a2aAud, auth.ViaOIDC); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("the revoked token after a failed read: %v", err)
	}
	clk.add(time.Second)
	if _, err := iss.Principal(ctx, other, a2aAud, auth.ViaOIDC); !errors.Is(err, ErrUnavailable) || f.Fresh() {
		t.Errorf("30 s after the last read: %v (fresh %v), want ErrUnavailable", err, f.Fresh())
	}
	if age, ok := f.Age(); !ok || age != 30*time.Second {
		t.Errorf("Age = %v %v", age, ok)
	}
	for name, body := range map[string]string{"malformed": `{"window_seconds": 1200}`, "not JSON": `<html>`} {
		as.FeedDown(0)
		as.SetFeedBody([]byte(body))
		if err := f.Refresh(ctx); err == nil {
			t.Errorf("a %s feed was read", name)
		}
	}
	as.SetFeedBody(nil)
	if err := f.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := iss.Principal(ctx, other, a2aAud, auth.ViaOIDC); err != nil {
		t.Errorf("the feed read again: %v", err)
	}

	// The window: a lifetime the issuer's new window no longer covers is refused; the window's end takes the
	// revocation off the list (the token has expired by then, which the lifetime bound guarantees).
	as.SetFeedWindow(600)
	if err := f.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := iss.Principal(ctx, other, a2aAud, auth.ViaOIDC); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a 15-minute token under a 10-minute window: %v, want ErrInvalidToken", err)
	}
	as.SetFeedWindow(oidctest.FeedWindow)
	as.Unrevoke()
	if err := f.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := iss.Principal(ctx, alice, a2aAud, auth.ViaOIDC); err != nil {
		t.Errorf("off the list: %v", err)
	}
}

// D316: Run reads at once, then every poll; Wait returns at the first read.
func TestFeedRun(t *testing.T) {
	as := newAS(t, oidctest.Config{})
	f := feed(t, as, realClock(), 50*time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	defer wcancel()
	if !f.Wait(wctx) || !f.Fresh() {
		t.Fatal("the feed was not read")
	}
	as.Revoke("s-1")
	deadline := time.Now().Add(5 * time.Second)
	for f.check("s-1", time.Now(), time.Now().Add(time.Minute)) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the revocation was not read within 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
	if n := as.Fetches(oidctest.PathRevocations); n < 2 {
		t.Errorf("the feed was read %d times", n)
	}
}

// D315: the tenant claim. Absent from a token, the principal has no tenant; present, a live tenant's slug maps it
// (id and slug, so org:<slug> admits it); an unknown slug, or a value that is no slug, refuses the token; a tenant the
// gate could not read is its own failure. Without a configured claim, the member is not read.
func TestIssuerTenantClaim(t *testing.T) {
	as := newAS(t, oidctest.Config{})
	v := verifier(t, as, realClock())
	acme := uuid.Must(uuid.NewV7())
	ts := &tenants{known: map[string]uuid.UUID{"acme": acme}}
	const claim = "https://kontra.sh/claims/tenant"
	iss := issuer(t, v, IssuerConfig{TenantClaim: claim, Tenants: ts})
	ctx := context.Background()
	sign := func(claims map[string]any) string {
		return as.MustSign(oidctest.Token{Subject: "alice", Audience: []string{a2aAud}, Claims: claims})
	}

	p, err := iss.Principal(ctx, sign(map[string]any{claim: "acme"}), a2aAud, auth.ViaOIDC)
	if err != nil || p.TenantID == nil || *p.TenantID != acme || p.TenantSlug != "acme" || p.ID != "oidc:sub:alice" {
		t.Fatalf("a token of acme: %+v %v", p, err)
	}
	allow, _ := auth.ParseAllowList([]string{"org:acme"})
	if !allow.Admits(p) {
		t.Error("org:acme does not admit acme's OIDC principal")
	}
	if p, err := iss.Principal(ctx, sign(nil), a2aAud, auth.ViaOIDC); err != nil || p.TenantID != nil || p.TenantSlug != "" || allow.Admits(p) {
		t.Errorf("a token without the claim: %+v %v", p, err)
	}
	for name, val := range map[string]any{
		"an unknown tenant": "globex", "upper case": "ACME", "empty": "", "null": nil, "a number": 7, "an array": []string{"acme"},
		"a slug with a dot": "acme.io",
	} {
		if _, err := iss.Principal(ctx, sign(map[string]any{claim: val}), a2aAud, auth.ViaOIDC); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: %v, want ErrInvalidToken", name, err)
		}
	}
	ts.err = errors.New("db down")
	if _, err := iss.Principal(ctx, sign(map[string]any{claim: "acme"}), a2aAud, auth.ViaOIDC); err == nil || errors.Is(err, ErrInvalidToken) {
		t.Errorf("tenants unreadable: %v, want the gate's own error", err)
	}
	ts.err = nil
	ts.asked = nil
	// Without the setting the claim is never read: no tenant, and no lookup.
	plain := issuer(t, v, IssuerConfig{})
	if p, err := plain.Principal(ctx, sign(map[string]any{claim: "acme", "tenant": "acme"}), a2aAud, auth.ViaOIDC); err != nil || p.TenantID != nil || len(ts.asked) != 0 {
		t.Errorf("no tenant_claim: %+v %v (asked %v)", p, err, ts.asked)
	}
}

// NewIssuer refuses what it cannot apply.
func TestNewIssuerRefuses(t *testing.T) {
	as := newAS(t, oidctest.Config{})
	other := newAS(t, oidctest.Config{})
	v := verifier(t, as, realClock())
	for name, cfg := range map[string]IssuerConfig{
		"no verifier":             {},
		"a claim without tenants": {Verifier: v, TenantClaim: "tenant"},
		"another issuer's feed":   {Verifier: v, Feed: feed(t, other, realClock(), time.Second, 2*time.Second)},
	} {
		if _, err := NewIssuer(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := NewA2A(nil, gateURL, "kontra"); err == nil {
		t.Error("NewA2A without an issuer")
	}
	if _, err := NewA2A(issuer(t, v, IssuerConfig{}), gateURL, ""); err == nil {
		t.Error("NewA2A without an audience")
	}
	// D314: audiences never cross. An audience naming the gate or one of its MCP routes would take that resource's
	// tokens (RFC 8707) on every A2A route, so NewA2A refuses it as the configuration's load does.
	for _, aud := range []string{mcpAud, CanonicalURI(gateURL, "fs"), CanonicalURI(gateURL, "acme/fs"), gateURL, gateURL + "/"} {
		if _, err := NewA2A(issuer(t, v, IssuerConfig{}), gateURL, aud); err == nil {
			t.Errorf("NewA2A with the audience %q", aud)
		}
	}
}

// D314: the A2A routes' check. A token whose aud contains the configured audience is oidc:sub:<sub> with via oidc; a
// token for an MCP route's canonical URI, another audience, or a revoked session is auth.ErrUnauthenticated; an issuer
// down is the gate's own error. A rotated key is fetched once, and its tokens are accepted.
func TestA2A(t *testing.T) {
	as := newAS(t, oidctest.Config{})
	clk := realClock()
	v := verifier(t, as, clk)
	f := feed(t, as, clk, time.Second, 10*time.Second)
	a2a, err := NewA2A(issuer(t, v, IssuerConfig{Feed: f}), gateURL, a2aAud)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := f.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(10 * time.Minute)
	for _, alg := range []string{oidctest.AlgES256, oidctest.AlgRS256, oidctest.AlgEdDSA} {
		tok := as.MustSign(oidctest.Token{Alg: alg, Subject: "bob", Audience: []string{"https://elsewhere.example", a2aAud}, Session: "s-b", Expiry: exp})
		p, err := a2a.VerifyA2A(ctx, tok)
		if err != nil || p.Kind != auth.KindOIDC || p.ID != "oidc:sub:bob" || p.Via != auth.ViaOIDC || p.BuyerID != BuyerID(as.Issuer(), "bob") ||
			!p.MayHire() || p.Scopes != nil {
			t.Errorf("%s: %+v %v", alg, p, err)
		}
	}
	for name, tk := range map[string]oidctest.Token{
		"an MCP route's audience": {Audience: []string{mcpAud}, Session: "s-b", Expiry: exp},
		"another audience":        {Audience: []string{"kontra:api"}, Session: "s-b", Expiry: exp},
		"expired":                 {Audience: []string{a2aAud}, Session: "s-b", Expiry: time.Now().Add(-2 * time.Minute)},
		"no sid":                  {Audience: []string{a2aAud}, Expiry: exp},
	} {
		if _, err := a2a.VerifyA2A(ctx, as.MustSign(tk)); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("%s: %v, want ErrUnauthenticated", name, err)
		}
	}
	kid, err := as.Rotate(oidctest.AlgES256)
	if err != nil {
		t.Fatal(err)
	}
	clk.add(RefetchEvery)
	before := as.Fetches(oidctest.PathJWKS)
	rotated := as.MustSign(oidctest.Token{Kid: kid, Audience: []string{a2aAud}, Session: "s-r", Expiry: time.Now().Add(10 * time.Minute)})
	if err := f.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := a2a.VerifyA2A(ctx, rotated); err != nil {
			t.Errorf("a rotated key's token: %v", err)
		}
	}
	if n := as.Fetches(oidctest.PathJWKS) - before; n != 1 {
		t.Errorf("the rotated key set was fetched %d times, want once", n)
	}
	as.Revoke("s-r")
	if err := f.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a2a.VerifyA2A(ctx, rotated); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("revoked: %v", err)
	}
	clk.add(10 * time.Second)
	if _, err := a2a.VerifyA2A(ctx, as.MustSign(oidctest.Token{Audience: []string{a2aAud}, Session: "s-x", Expiry: exp})); err == nil || errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("a stale feed: %v, want the gate's own error", err)
	}
}
