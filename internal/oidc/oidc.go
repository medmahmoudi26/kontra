// Package oidc verifies JWTs from an operator's authorization server, and makes /mcp and every /mcp/{svc} an OAuth
// 2.1 resource server for it (spec/relay.md §12.1, D25; built in M4, DM4-9).
//
// With auth.mcp_oauth.issuer configured:
//
//   - A request to an MCP route without a credential is 401 with WWW-Authenticate: Bearer
//     resource_metadata="{public_url}/.well-known/oauth-protected-resource/mcp[/{svc}]", scope="mcp" (Challenge).
//   - That RFC 9728 document (MetadataHandler) is served for every well-formed name, existing or not: its resource
//     is the route's canonical URI ({public_url}/mcp or {public_url}/mcp/{svc}) and it names the issuer.
//   - A bearer token that is not an API key is a JWT verified by Verifier against the route's canonical URI as
//     audience (RFC 8707): iss, aud, exp and nbf with 60 s of skew, ES256, RS256 or EdDSA only. The key set comes
//     from the issuer's RFC 8414 metadata (else its OpenID configuration) over https://, dialed as configured (the
//     issuer is the operator's, never derived from a request), cached 10 minutes; an unknown kid refetches it at
//     most once a minute. scope is not checked.
//   - The principal is oidc:sub:<sub>, buyer UUIDv5(issuer ‖ sub), no tenant, via mcp (spec §2.3).
//
// The authorization server is the operator's (Cloud's control plane, M5), never part of the gate. The A2A routes'
// OIDC principals (auth.oidc, audience kontra) and Cloud's revocation feed reuse Verifier in M5.
//
// Work package: M4 WP-06.
package oidc

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/unimpl"
)

// Verification parameters (DM4-9).
const (
	Skew         = 60 * time.Second
	KeySetTTL    = 10 * time.Minute
	RefetchEvery = time.Minute
	Scope        = "mcp" // named in the challenge; not checked in tokens
)

// MetadataPrefix is the path prefix of the RFC 9728 documents (spec §2.1).
const MetadataPrefix = "/.well-known/oauth-protected-resource"

// ErrInvalidToken is every verification failure: the route answers 401 and says no more.
var ErrInvalidToken = errors.New("oidc: invalid token")

// Config configures a Verifier.
type Config struct {
	Issuer string       // auth.mcp_oauth.issuer
	Client *http.Client // https only, dialed as configured; metadata and key set are fetched through it
}

// Claims are what the gate reads of a verified token.
type Claims struct {
	Issuer   string
	Subject  string
	Audience []string
	Expiry   time.Time
}

// Verifier verifies the issuer's JWTs.
type Verifier struct {
	cfg Config
}

// NewVerifier checks cfg. It does not fetch anything: the issuer's metadata and key set are read on first use.
func NewVerifier(cfg Config) (*Verifier, error) {
	return nil, unimpl.Error("oidc.NewVerifier", "M4 WP-06")
}

// Verify checks token's signature, issuer, audience (it must contain audience) and times. Any failure is
// ErrInvalidToken (a 401), never a detail an attacker can probe.
func (v *Verifier) Verify(ctx context.Context, token, audience string) (Claims, error) {
	return Claims{}, unimpl.Error("oidc.Verifier.Verify", "M4 WP-06")
}

// Principal is the principal a verified token stands for on an MCP route (spec §2.3, §12.1).
func Principal(issuer string, c Claims) auth.Principal {
	return auth.Principal{
		Kind: auth.KindOIDC, ID: "oidc:sub:" + c.Subject, BuyerID: BuyerID(issuer, c.Subject), Via: auth.ViaMCP,
	}
}

// BuyerID is UUIDv5(issuer ‖ sub) (spec §2.3).
func BuyerID(issuer, subject string) uuid.UUID {
	panic(unimpl.Error("oidc.BuyerID", "M4 WP-06"))
}

// CanonicalURI is the route's canonical URI, the audience its tokens must carry: {public_url}/mcp, or
// {public_url}/mcp/{svc} where svc is name or tenant/name (spec §12.1).
func CanonicalURI(publicURL, svc string) string {
	panic(unimpl.Error("oidc.CanonicalURI", "M4 WP-06"))
}

// Challenge writes the 401 of a request without a usable credential on an MCP route: WWW-Authenticate naming the
// route's RFC 9728 document and scope="mcp", and a JSON-RPC error body without id on a POST.
func Challenge(w http.ResponseWriter, publicURL, svc string, post bool) {
	panic(unimpl.Error("oidc.Challenge", "M4 WP-06"))
}

// MetadataHandler serves GET /.well-known/oauth-protected-resource/mcp[/{svc}] for every well-formed name, existing
// or not (spec §12.1): {resource, authorization_servers: [issuer], scopes_supported: ["mcp"],
// bearer_methods_supported: ["header"]}.
func MetadataHandler(publicURL, issuer string, multi bool) http.Handler {
	panic(unimpl.Error("oidc.MetadataHandler", "M4 WP-06"))
}
