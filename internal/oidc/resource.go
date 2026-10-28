package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
)

// ResourceServer is the gate's OAuth 2.1 resource server for /mcp and every /mcp/{svc} (spec §12.1): the token check of
// the MCP routes (auth.MCPResourceServer, which internal/auth's Authenticator asks for a bearer JWT on those routes
// only), their 401 challenge, and their RFC 9728 documents.
type ResourceServer struct {
	publicURL string
	multi     bool
	verifier  *Verifier
}

var _ auth.MCPResourceServer = (*ResourceServer)(nil)

// NewResourceServer is the resource server of a gate at publicURL (tenancy multi: /mcp/{tenant}/{name}) for the
// verifier's issuer.
func NewResourceServer(publicURL string, multi bool, v *Verifier) (*ResourceServer, error) {
	if v == nil || publicURL == "" {
		return nil, errors.New("oidc: a resource server needs the public URL and a verifier")
	}
	return &ResourceServer{publicURL: publicURL, multi: multi, verifier: v}, nil
}

// VerifyMCP verifies a bearer JWT for the MCP route svc ("" for /mcp): its audience must contain the route's
// canonical URI (RFC 8707), so a token for /mcp is refused on every /mcp/{svc} and the other way round. A token that
// is not valid there is auth.ErrUnauthenticated; an issuer the gate cannot read is the gate's own error (503).
func (rs *ResourceServer) VerifyMCP(ctx context.Context, token, svc string) (auth.Principal, error) {
	if !rs.wellFormed(svc) {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	c, err := rs.verifier.Verify(ctx, token, CanonicalURI(rs.publicURL, svc))
	switch {
	case errors.Is(err, ErrInvalidToken):
		return auth.Principal{}, auth.ErrUnauthenticated
	case err != nil:
		return auth.Principal{}, err
	}
	return Principal(rs.verifier.Issuer(), c), nil
}

// Challenge is the WWW-Authenticate value of a 401 on the MCP route svc ("" for /mcp): Bearer with the route's RFC
// 9728 document and scope="mcp" (§12.1), and error="invalid_token" when a credential was presented and refused (RFC
// 6750 §3.1; a request without one gets no error code). A svc that is no well-formed name has no document: its
// challenge names none.
func (rs *ResourceServer) Challenge(svc string, refused bool) string {
	return Challenge(rs.publicURL, svc, refused, rs.wellFormed(svc))
}

// Challenge is ResourceServer.Challenge for a gate at publicURL; withMetadata false leaves resource_metadata out.
func Challenge(publicURL, svc string, refused, withMetadata bool) string {
	var b strings.Builder
	b.WriteString("Bearer ")
	if withMetadata {
		b.WriteString(`resource_metadata="` + quoted(MetadataURL(publicURL, svc)) + `", `)
	}
	b.WriteString(`scope="` + Scope + `"`)
	if refused {
		b.WriteString(`, error="invalid_token"`)
	}
	return b.String()
}

// quoted escapes s for an RFC 9110 quoted-string: a quote or a backslash is escaped, and a control character (which
// public_url, a configured URL, never holds) is dropped so no header can be split.
func quoted(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// wellFormed reports whether svc names an MCP route of this gate: "" (/mcp), a name, or tenant/name on a multi-tenant
// gate, each matching config.NameRE.
func (rs *ResourceServer) wellFormed(svc string) bool { return WellFormed(svc, rs.multi) }

// WellFormed reports whether svc is "" or a well-formed service of the gate's tenancy (spec §2.1).
func WellFormed(svc string, multi bool) bool {
	if svc == "" {
		return true
	}
	parts := strings.Split(svc, "/")
	if (multi && len(parts) != 2) || (!multi && len(parts) != 1) {
		return false
	}
	for _, p := range parts {
		if !config.NameRE.MatchString(p) {
			return false
		}
	}
	return true
}

// MetadataPatterns are the router patterns MetadataHandler serves: /mcp, and /mcp/{name} or /mcp/{tenant}/{name}.
func MetadataPatterns(multi bool) []string {
	svc := "/{name}"
	if multi {
		svc = "/{tenant}/{name}"
	}
	return []string{"GET " + MetadataPrefix + MCPPath, "GET " + MetadataPrefix + MCPPath + svc}
}

// ProtectedResource is an RFC 9728 document as the gate serves it.
type ProtectedResource struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

// MetadataHandler serves GET /.well-known/oauth-protected-resource/mcp[/{svc}] for every well-formed name, existing
// or not (spec §12.1): {resource, authorization_servers: [issuer], scopes_supported: ["mcp"],
// bearer_methods_supported: ["header"]}. A name that is not well formed is 404, as the closed router answers.
func MetadataHandler(publicURL, issuer string, multi bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, MetadataPrefix+MCPPath)
		svc := strings.TrimPrefix(rest, "/")
		if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) || (rest != "" && svc == "") || !WellFormed(svc, multi) {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, err := json.Marshal(ProtectedResource{
			Resource: CanonicalURI(publicURL, svc), AuthorizationServers: []string{issuer},
			ScopesSupported: []string{Scope}, BearerMethodsSupported: []string{"header"},
		})
		if err != nil {
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	})
}

// Handler is MetadataHandler for this resource server.
func (rs *ResourceServer) Handler() http.Handler {
	return MetadataHandler(rs.publicURL, rs.verifier.Issuer(), rs.multi)
}
