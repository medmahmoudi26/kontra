// Package oidc verifies JWTs from an operator's authorization server, and makes /mcp and every /mcp/{svc} an OAuth
// 2.1 resource server for it (spec/relay.md §12.1, D25; built in M4, D276).
//
// With auth.mcp_oauth.issuer configured:
//
//   - A request to an MCP route without a credential is 401 with WWW-Authenticate: Bearer
//     resource_metadata="{public_url}/.well-known/oauth-protected-resource/mcp[/{svc}]", scope="mcp" (Challenge;
//     a credential that was presented and refused adds error="invalid_token", RFC 6750 §3.1). On /mcp/{svc} a
//     service whose allow: admits anonymous still serves a request without one (D296).
//   - That RFC 9728 document (MetadataHandler) is served for every well-formed name, existing or not: its resource
//     is the route's canonical URI ({public_url}/mcp or {public_url}/mcp/{svc}) and it names the issuer.
//   - A bearer token shaped as a JWT (auth.LooksLikeJWT) is verified by Verifier against the route's canonical URI as
//     audience (RFC 8707): iss, aud, exp and nbf with 60 s of skew, ES256, RS256 or EdDSA only. Any other bearer is
//     an API key, as on every route. The key set comes from the issuer's RFC 8414 metadata (else its OpenID
//     configuration), fetched as one of §10.3's operator-configured endpoints (D284: https://, no redirect, no
//     proxy, no address check, bodies at most 256 KiB, 5 s), cached 10 minutes; an unknown kid refetches it at most
//     once a minute. scope is not checked.
//   - The principal is oidc:sub:<sub>, buyer UUIDv5(issuer ‖ sub), no tenant, via mcp (spec §2.3).
//
// The authorization server is the operator's (Cloud's control plane, M5), never part of the gate; oidctest is an
// in-repository one for the tests. The A2A routes' OIDC principals (auth.oidc, audience kontra) and Cloud's
// revocation feed reuse Verifier in M5: until then a JWT on an A2A route is 401, as any bearer that is not an API key.
//
// Work package: M4 WP-06.
package oidc

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/ids"
)

// Verification parameters (D276).
const (
	Skew         = 60 * time.Second
	KeySetTTL    = 10 * time.Minute
	RefetchEvery = time.Minute
	Scope        = "mcp" // named in the challenge and the RFC 9728 document; not checked in tokens
)

// MetadataPrefix is the path prefix of the RFC 9728 documents (spec §2.1).
const MetadataPrefix = "/.well-known/oauth-protected-resource"

// MCPPath is the gate's own MCP route, the last segment of every canonical URI's path but the services'.
const MCPPath = "/mcp"

// ErrInvalidToken is every verification failure: the route answers 401 and says no more.
var ErrInvalidToken = errors.New("oidc: invalid token")

// ErrUnavailable is an issuer whose metadata or key set could not be read when a token needed them (and no fresh key
// set was held): the gate's own failure, never the caller's (the route answers 503, not 401).
var ErrUnavailable = errors.New("oidc: the authorization server's keys are unavailable")

// Claims are what the gate reads of a verified token.
type Claims struct {
	Issuer   string
	Subject  string
	Audience []string
	Expiry   time.Time
}

// Principal is the principal a verified token stands for on an MCP route (spec §2.3, §12.1): oidc:sub:<sub>, the
// buyer UUIDv5(issuer ‖ sub), no tenant (mapping a subject to a tenant is M5's), via mcp.
func Principal(issuer string, c Claims) auth.Principal {
	return auth.Principal{
		Kind: auth.KindOIDC, ID: "oidc:sub:" + c.Subject, BuyerID: BuyerID(issuer, c.Subject), Via: auth.ViaMCP,
	}
}

// BuyerID is UUIDv5(issuer ‖ sub) (spec §2.3; ids.OIDCBuyer): the same subject of the same issuer is the same buyer
// in every replica and release. An empty issuer or subject, which Verify never accepts, is uuid.Nil.
func BuyerID(issuer, subject string) uuid.UUID {
	id, err := ids.OIDCBuyer(issuer, subject)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// CanonicalURI is the route's canonical URI, the audience its tokens must carry: {public_url}/mcp, or
// {public_url}/mcp/{svc} where svc is name or tenant/name (spec §12.1). public_url is taken as configured, without a
// trailing slash, as the gate's other URLs are built.
func CanonicalURI(publicURL, svc string) string {
	u := strings.TrimRight(publicURL, "/") + MCPPath
	if svc != "" {
		u += "/" + svc
	}
	return u
}

// MetadataURL is the route's RFC 9728 document: {public_url}/.well-known/oauth-protected-resource/mcp[/{svc}].
func MetadataURL(publicURL, svc string) string {
	u := strings.TrimRight(publicURL, "/") + MetadataPrefix + MCPPath
	if svc != "" {
		u += "/" + svc
	}
	return u
}
