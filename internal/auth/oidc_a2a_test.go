package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// fakeA2A records what the Authenticator asks the A2A routes' verifier.
type fakeA2A struct {
	calls []string
	err   error
}

func (f *fakeA2A) VerifyA2A(_ context.Context, token string) (Principal, error) {
	f.calls = append(f.calls, token)
	if f.err != nil {
		return Principal{}, f.err
	}
	return Principal{Kind: KindOIDC, ID: "oidc:sub:alice", BuyerID: buyer1, Via: ViaOIDC}, nil
}

// spec §2.3, D314: on the A2A routes, with auth.oidc, a bearer shaped as a JWT is verified as a token of the issuer
// (never looked up as an API key); any other bearer is an API key; without auth.oidc a JWT is a key lookup and 401, as
// before M5. The MCP routes never ask the A2A verifier: a JWT there is the resource server's, or 401. A request
// without a credential is the anonymous principal; a node's request takes its session's principal.
func TestAuthenticateA2A(t *testing.T) {
	req := func(authz string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/agents/hello", nil)
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		return r
	}
	a := NewAuthenticator(nil)
	if a.A2AOIDC() {
		t.Fatal("A2AOIDC without a verifier")
	}
	if _, _, err := a.AuthenticateA2A(req("Bearer "+jwtShaped), uuid.Nil); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a JWT without auth.oidc: %v", err)
	}

	v := &fakeA2A{}
	a.SetA2AVerifier(v)
	if !a.A2AOIDC() {
		t.Fatal("A2AOIDC with a verifier")
	}
	p, ok, err := a.AuthenticateA2A(req("Bearer "+jwtShaped), uuid.Nil)
	if err != nil || !ok || p.ID != "oidc:sub:alice" || p.Via != ViaOIDC || len(v.calls) != 1 || v.calls[0] != jwtShaped {
		t.Fatalf("a JWT with auth.oidc: %+v %v %v %q", p, ok, err, v.calls)
	}
	v.err = ErrUnauthenticated
	if _, _, err := a.AuthenticateA2A(req("bearer "+jwtShaped), uuid.Nil); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a refused JWT: %v", err)
	}
	v.err = errors.New("feed stale")
	if _, _, err := a.AuthenticateA2A(req("Bearer "+jwtShaped), uuid.Nil); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a feed stale: %v, want the gate's own error", err)
	}
	n := len(v.calls)
	for _, bearer := range []string{"kk_acme_" + strings.Repeat("a", 32), "a.b", "a.b.c.d"} {
		if _, _, err := a.AuthenticateA2A(req("Bearer "+bearer), uuid.Nil); !errors.Is(err, ErrUnauthenticated) || len(v.calls) != n {
			t.Errorf("%.20q: %v, verifier asked %d times", bearer, err, len(v.calls)-n)
		}
	}
	for _, h := range []string{"Bearer " + jwtShaped + " x", "Basic " + jwtShaped} {
		if _, _, err := a.AuthenticateA2A(req(h), uuid.Nil); !errors.Is(err, ErrUnauthenticated) || len(v.calls) != n {
			t.Errorf("%q: %v", h, err)
		}
	}
	// The MCP routes: never the A2A verifier.
	if _, _, err := a.AuthenticateMCP(req("Bearer "+jwtShaped), uuid.Nil, ""); !errors.Is(err, ErrUnauthenticated) || len(v.calls) != n {
		t.Fatalf("a JWT on an MCP route without a resource server: %v, A2A verifier asked %d times", err, len(v.calls)-n)
	}
	rs := &fakeRS{}
	a.SetMCPResourceServer(rs)
	if _, _, err := a.AuthenticateMCP(req("Bearer "+jwtShaped), uuid.Nil, "fs"); err != nil || len(v.calls) != n || len(rs.calls) != 1 {
		t.Fatalf("a JWT on an MCP route: %v (A2A %d, MCP %d)", err, len(v.calls)-n, len(rs.calls))
	}
	if p, ok, err := a.AuthenticateA2A(req(""), buyer1); err != nil || ok || p.Kind != KindAnonymous || p.BuyerID != buyer1 {
		t.Fatalf("no credential: %+v %v %v", p, ok, err)
	}
	sess := Principal{Kind: KindNode, ID: "node:k1_x", BuyerID: buyer1, Via: ViaNode}
	r := req("Bearer " + jwtShaped).WithContext(WithHireSource(context.Background(), sessionSource{sess}))
	if p, ok, err := a.AuthenticateA2A(r, uuid.Nil); err != nil || !ok || p.ID != sess.ID || len(v.calls) != n {
		t.Fatalf("a node's request: %+v %v %v", p, ok, err)
	}
}
