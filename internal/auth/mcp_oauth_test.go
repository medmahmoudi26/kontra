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

// fakeRS records what the Authenticator asks the MCP resource server.
type fakeRS struct {
	calls []string // token|svc
	err   error
}

func (f *fakeRS) VerifyMCP(_ context.Context, token, svc string) (Principal, error) {
	f.calls = append(f.calls, token+"|"+svc)
	if f.err != nil {
		return Principal{}, f.err
	}
	return Principal{Kind: KindOIDC, ID: "oidc:sub:alice", BuyerID: buyer1, Via: ViaMCP}, nil
}

func (f *fakeRS) Challenge(svc string, refused bool) string {
	if refused {
		return "challenge " + svc + " refused"
	}
	return "challenge " + svc
}

const jwtShaped = "eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJhbGljZSJ9.c2ln"

type sessionSource struct{ p Principal }

func (s sessionSource) HirePrincipal(context.Context) (Principal, error) { return s.p, nil }

// §12.1, D296: on an MCP route with a resource server, a bearer shaped as a JWT is verified for that route; any
// other bearer is an API key, as on every route; the A2A routes' Authenticate never asks the resource server; a
// request without a credential is the anonymous principal (the route decides); a node's request takes its session's.
func TestAuthenticateMCP(t *testing.T) {
	req := func(authz string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		return r
	}
	a := NewAuthenticator(nil) // no API keys: every key-shaped bearer is 401
	if a.MCPOAuth() || a.MCPChallenge("fs", false) != "Bearer" {
		t.Fatal("without a resource server: MCPOAuth or the challenge")
	}
	// Without a resource server a JWT is looked up as a key: 401.
	if _, _, err := a.AuthenticateMCP(req("Bearer "+jwtShaped), uuid.Nil, ""); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a JWT without a resource server: %v", err)
	}

	rs := &fakeRS{}
	a.SetMCPResourceServer(rs)
	if !a.MCPOAuth() || a.MCPChallenge("acme/fs", true) != "challenge acme/fs refused" {
		t.Fatal("with a resource server: MCPOAuth or the challenge")
	}
	p, ok, err := a.AuthenticateMCP(req("Bearer "+jwtShaped), uuid.Nil, "acme/fs")
	if err != nil || !ok || p.ID != "oidc:sub:alice" || p.Via != ViaMCP || len(rs.calls) != 1 || rs.calls[0] != jwtShaped+"|acme/fs" {
		t.Fatalf("a JWT on /mcp/acme/fs: %+v %v %v %q", p, ok, err, rs.calls)
	}
	rs.err = ErrUnauthenticated
	if _, _, err := a.AuthenticateMCP(req("bearer "+jwtShaped), uuid.Nil, ""); !errors.Is(err, ErrUnauthenticated) || len(rs.calls) != 2 {
		t.Fatalf("a refused JWT: %v %q", err, rs.calls)
	}
	rs.err = errors.New("issuer down")
	if _, _, err := a.AuthenticateMCP(req("Bearer "+jwtShaped), uuid.Nil, ""); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("an issuer down: %v, want the gate's own error", err)
	}
	// An API key (or anything not JWT-shaped) never reaches the resource server.
	for _, bearer := range []string{"kk_acme_" + strings.Repeat("a", 32), "a.b", "a.b.c.d", ".b.c", "a.b=.c", strings.Repeat("a", MaxJWT) + ".b.c"} {
		n := len(rs.calls)
		if _, _, err := a.AuthenticateMCP(req("Bearer "+bearer), uuid.Nil, ""); !errors.Is(err, ErrUnauthenticated) || len(rs.calls) != n {
			t.Errorf("%.20q: %v, resource server asked %d times", bearer, err, len(rs.calls)-n)
		}
	}
	// The A2A routes' Authenticate: a JWT is a key lookup, 401, and the resource server is not asked.
	n := len(rs.calls)
	if _, _, err := a.Authenticate(req("Bearer "+jwtShaped), uuid.Nil); !errors.Is(err, ErrUnauthenticated) || len(rs.calls) != n {
		t.Fatalf("a JWT on an A2A route: %v", err)
	}
	// No credential: the anonymous principal, not authenticated.
	if p, ok, err := a.AuthenticateMCP(req(""), buyer1, "fs"); err != nil || ok || p.Kind != KindAnonymous || p.BuyerID != buyer1 {
		t.Fatalf("no credential: %+v %v %v", p, ok, err)
	}
	// A node's request: its session's principal, whatever it carries.
	sess := Principal{Kind: KindNode, ID: "node:k1_x", BuyerID: buyer1, Via: ViaNode}
	r := req("Bearer " + jwtShaped).WithContext(WithHireSource(context.Background(), sessionSource{sess}))
	if p, ok, err := a.AuthenticateMCP(r, uuid.Nil, ""); err != nil || !ok || p.ID != sess.ID || len(rs.calls) != n {
		t.Fatalf("a node's request: %+v %v %v", p, ok, err)
	}
}

func TestLooksLikeJWT(t *testing.T) {
	for tok, want := range map[string]bool{
		jwtShaped:            true,
		"a.b.":               true, // alg none's empty signature: the verifier refuses it
		"a..c":               true,
		".b.c":               false,
		"a.b":                false,
		"a.b.c.d":            false,
		"a b.c.d":            false,
		"a+b.c.d":            false,
		"kk_acme_0123456789": false,
	} {
		if LooksLikeJWT(tok) != want {
			t.Errorf("LooksLikeJWT(%q) = %v", tok, !want)
		}
	}
}
