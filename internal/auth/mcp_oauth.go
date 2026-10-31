package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// MCPResourceServer is the OAuth 2.1 resource server of the MCP routes (spec/relay.md §12.1, D276;
// internal/oidc's ResourceServer, set on the Authenticator when auth.mcp_oauth is configured). It applies to /mcp and
// every /mcp/{svc} and to nothing else: the A2A routes answer a JWT 401 until their OIDC principals (M5).
type MCPResourceServer interface {
	// VerifyMCP verifies a bearer JWT for the MCP route svc ("" for /mcp, else name or tenant/name), whose canonical URI
	// its audience must contain: the principal it stands for (oidc:sub:<sub>, via mcp), ErrUnauthenticated when it is
	// not valid for that route, or the gate's own error (its issuer unreadable).
	VerifyMCP(ctx context.Context, token, svc string) (Principal, error)
	// Challenge is the WWW-Authenticate value of a 401 on the route svc: refused when a credential was presented.
	Challenge(svc string, refused bool) string
}

// MaxJWT bounds a bearer JWT on an MCP route: a longer bearer is looked up as an API key, and refused (MaxBearer).
const MaxJWT = 16 << 10

// LooksLikeJWT reports whether a bearer token has the shape of a JWS compact serialization: three base64url segments
// separated by dots, the first not empty, at most MaxJWT bytes. An MCP route with a resource server verifies such a
// token as a JWT and looks any other up as an API key: a key the gate mints is kk_ and base62 (spec §2.3), and an
// operator's own key (D37) with two dots between base64url segments is not a credential anyone writes (D296).
func LooksLikeJWT(token string) bool {
	if strings.Count(token, ".") != 2 || len(token) > MaxJWT {
		return false
	}
	for i, seg := range strings.Split(token, ".") {
		if seg == "" && i == 0 {
			return false
		}
		for _, c := range []byte(seg) {
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// SetMCPResourceServer makes the MCP routes an OAuth resource server for rs (auth.mcp_oauth, spec §12.1). It is called
// once, while the replica is assembled and before any request.
func (a *Authenticator) SetMCPResourceServer(rs MCPResourceServer) { a.mcp = rs }

// MCPOAuth reports whether the MCP routes are a resource server: a request without a credential is then 401 with the
// route's challenge wherever an anonymous caller would not be served (D296).
func (a *Authenticator) MCPOAuth() bool { return a.mcp != nil }

// AuthenticateMCP is Authenticate on the MCP route svc ("" for /mcp, else name or tenant/name, as the route's path
// names it): with a resource server (SetMCPResourceServer), a bearer shaped as a JWT is verified for the route's
// canonical URI and stands for oidc:sub:<sub> with via mcp (spec §2.3, §12.1); any other bearer is an API key, as on
// every route. A request a node opened takes its session's principal, as Authenticate gives it.
func (a *Authenticator) AuthenticateMCP(r *http.Request, anonymousBuyer uuid.UUID, svc string) (p Principal, authenticated bool, err error) {
	if a.mcp == nil {
		return a.Authenticate(r, anonymousBuyer)
	}
	if _, ok := HireSourceFrom(r.Context()); ok {
		return a.Authenticate(r, anonymousBuyer)
	}
	token, present, err := BearerToken(r.Header)
	if !present || err != nil || !LooksLikeJWT(token) {
		return a.Authenticate(r, anonymousBuyer)
	}
	if p, err = a.mcp.VerifyMCP(r.Context(), token, svc); err != nil {
		return Principal{}, false, err
	}
	return p, true, nil
}

// MCPChallenge is the WWW-Authenticate value of a 401 on the MCP route svc: the resource server's challenge (§12.1),
// or "Bearer" without one. refused is a credential that was presented and refused.
func (a *Authenticator) MCPChallenge(svc string, refused bool) string {
	if a.mcp == nil {
		return "Bearer"
	}
	return a.mcp.Challenge(svc, refused)
}
