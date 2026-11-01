package auth

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// A2AVerifier verifies the bearer JWTs of the A2A routes (auth.oidc, spec/relay.md §2.3; M5, D314; internal/oidc's A2A,
// set on the Authenticator when auth.oidc is configured): a token of the configured issuer whose aud contains
// auth.oidc.audience stands for oidc:sub:<sub> with via oidc, and the tenant its tenant claim names (D315). It applies
// to the A2A routes only: the MCP routes verify their own audiences (MCPResourceServer).
type A2AVerifier interface {
	// VerifyA2A is the principal a bearer JWT stands for, ErrUnauthenticated when it is not valid on the A2A routes
	// (revoked through the issuer's feed included, D316), or the gate's own error (its issuer, feed or database
	// unreadable).
	VerifyA2A(ctx context.Context, token string) (Principal, error)
}

// SetA2AVerifier makes a bearer JWT on the A2A routes an OIDC principal (auth.oidc). It is called once, while the
// replica is assembled and before any request.
func (a *Authenticator) SetA2AVerifier(v A2AVerifier) { a.a2a = v }

// AuthenticateA2A is Authenticate on the A2A routes: with an A2AVerifier (auth.oidc), a bearer shaped as a JWT
// (LooksLikeJWT) is verified as a token of the configured issuer and never looked up as an API key; any other bearer
// is an API key, as on every route. Without one, a JWT is looked up as an API key and refused, as before M5. A request
// a node opened takes its session's principal, as Authenticate gives it.
func (a *Authenticator) AuthenticateA2A(r *http.Request, anonymousBuyer uuid.UUID) (p Principal, authenticated bool, err error) {
	if a.a2a == nil {
		return a.Authenticate(r, anonymousBuyer)
	}
	if _, ok := HireSourceFrom(r.Context()); ok {
		return a.Authenticate(r, anonymousBuyer)
	}
	token, present, err := BearerToken(r.Header)
	if !present || err != nil || !LooksLikeJWT(token) {
		return a.Authenticate(r, anonymousBuyer)
	}
	if p, err = a.a2a.VerifyA2A(r.Context(), token); err != nil {
		return Principal{}, false, err
	}
	return p, true, nil
}

// A2AOIDC reports whether the A2A routes take OIDC principals (auth.oidc).
func (a *Authenticator) A2AOIDC() bool { return a.a2a != nil }
