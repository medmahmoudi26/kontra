package gate

import (
	"crypto/tls"
	"crypto/x509"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/oidc"
	"kontra.sh/kontra/internal/ssrf"
)

// WithOperatorRoots makes the MCP authorization server's metadata and key set (auth.mcp_oauth, spec §12.1) verified
// against pool instead of the system roots (tests: an in-process authorization server with a throwaway CA; a gate
// process takes SSL_CERT_FILE).
func WithOperatorRoots(pool *x509.CertPool) Option {
	return func(o *options) { o.operatorRoots = pool }
}

// assembleMCPOAuth makes /mcp and every /mcp/{svc} an OAuth 2.1 resource server for auth.mcp_oauth.issuer (spec
// §12.1, D276; M4 WP-06): the authenticator verifies bearer JWTs on those routes for their canonical URIs, their 401s
// carry the challenge, and the RFC 9728 documents are served for every well-formed name. The issuer's metadata and key
// set are §10.3's operator-configured endpoints (D284). Without the setting it does nothing.
func (r *Replica) assembleMCPOAuth(routes *edge.Router, authn *auth.Authenticator) error {
	m := r.cfg.Auth.MCPOAuth
	if m == nil {
		return nil
	}
	var tc *tls.Config
	if r.opts.operatorRoots != nil {
		tc = &tls.Config{RootCAs: r.opts.operatorRoots, MinVersion: tls.VersionTLS12}
	}
	v, err := oidc.NewVerifier(oidc.Config{
		Issuer: m.Issuer, Client: ssrf.NewOperatorClient(ssrf.OperatorOptions{TLS: tc}), Logger: r.log,
	})
	if err != nil {
		return err
	}
	rs, err := oidc.NewResourceServer(r.cfg.PublicURL, r.cfg.Tenancy == config.TenancyMulti, v)
	if err != nil {
		return err
	}
	authn.SetMCPResourceServer(rs)
	for _, p := range oidc.MetadataPatterns(r.cfg.Tenancy == config.TenancyMulti) {
		routes.Handle(p, rs.Handler())
	}
	r.log.Info("the MCP routes are an OAuth resource server", "issuer", m.Issuer)
	return nil
}
