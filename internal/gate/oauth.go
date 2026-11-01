package gate

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/oidc"
	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/tenant"
)

// WithOperatorRoots makes the OIDC issuers' metadata, key sets and revocation feeds (auth.oidc, auth.mcp_oauth; spec
// §2.3, §12.1) verified against pool instead of the system roots (tests: an in-process authorization server with a
// throwaway CA; a gate process takes SSL_CERT_FILE).
func WithOperatorRoots(pool *x509.CertPool) Option {
	return func(o *options) { o.operatorRoots = pool }
}

// FeedWait bounds how long Run waits, before it serves, for each revocation feed's first read (D316): until a feed is
// read, its issuer's tokens are the gate's own failure (503), so a replica that serves at once would refuse the tokens
// that arrive in its first instants. A feed not read by then keeps being polled, and is logged.
const FeedWait = 5 * time.Second

// assembleOIDC builds the gate's OIDC issuers (spec §2.3, §12.1; D276, D314 to D316): one per distinct issuer of
// auth.oidc and auth.mcp_oauth, holding its verifier (metadata and key set fetched as §10.3's operator-configured
// endpoints, D284), its tenant claim and its revocation feed, which Run polls. With auth.mcp_oauth, /mcp and every
// /mcp/{svc} are an OAuth 2.1 resource server for its issuer (M4 WP-06): the authenticator verifies bearer JWTs on
// those routes for their canonical URIs, their 401s carry the challenge, and the RFC 9728 documents are served for
// every well-formed name. With auth.oidc, a bearer JWT on the A2A routes is verified for auth.oidc.audience (M5). The
// two blocks name one issuer in Cloud: its tokens then share one key-set cache and one feed, and config has checked
// that both blocks give it the same tenant claim and feed.
func (r *Replica) assembleOIDC(routes *edge.Router, authn *auth.Authenticator) error {
	o, m := r.cfg.Auth.OIDC, r.cfg.Auth.MCPOAuth
	if o == nil && m == nil {
		return nil
	}
	var tc *tls.Config
	if r.opts.operatorRoots != nil {
		tc = &tls.Config{RootCAs: r.opts.operatorRoots, MinVersion: tls.VersionTLS12}
	}
	tenants := tenant.NewResolver(r.pool)
	issuers := map[string]*oidc.Issuer{}
	issuer := func(name, claim string, rev *config.Revocations) (*oidc.Issuer, error) {
		if iss, ok := issuers[name]; ok {
			return iss, nil
		}
		v, err := oidc.NewVerifier(oidc.Config{
			Issuer: name, Client: ssrf.NewOperatorClient(ssrf.OperatorOptions{TLS: tc}), Logger: r.log,
		})
		if err != nil {
			return nil, err
		}
		var feed *oidc.Feed
		if rev != nil {
			if feed, err = oidc.NewFeed(oidc.FeedConfig{
				Issuer: name, URL: rev.URL, Poll: rev.Poll.D(), MaxStale: rev.MaxStale.D(), Logger: r.log,
				Client: ssrf.NewOperatorClient(ssrf.OperatorOptions{TLS: tc, Timeout: oidc.FeedTimeout}),
			}); err != nil {
				return nil, err
			}
			r.feeds = append(r.feeds, feed)
		}
		iss, err := oidc.NewIssuer(oidc.IssuerConfig{Verifier: v, TenantClaim: claim, Tenants: tenants, Feed: feed, Logger: r.log})
		if err != nil {
			return nil, err
		}
		issuers[name] = iss
		return iss, nil
	}
	if m != nil {
		iss, err := issuer(m.Issuer, m.TenantClaim, m.Revocations)
		if err != nil {
			return err
		}
		rs, err := oidc.NewResourceServer(r.cfg.PublicURL, r.cfg.Tenancy == config.TenancyMulti, iss)
		if err != nil {
			return err
		}
		authn.SetMCPResourceServer(rs)
		for _, p := range oidc.MetadataPatterns(r.cfg.Tenancy == config.TenancyMulti) {
			routes.Handle(p, rs.Handler())
		}
		r.log.Info("the MCP routes are an OAuth resource server", "issuer", m.Issuer, "tenant_claim", m.TenantClaim,
			"revocations", m.Revocations != nil)
	}
	if o != nil {
		iss, err := issuer(o.Issuer, o.TenantClaim, o.Revocations)
		if err != nil {
			return err
		}
		a2a, err := oidc.NewA2A(iss, o.Audience)
		if err != nil {
			return err
		}
		authn.SetA2AVerifier(a2a)
		r.log.Info("the A2A routes take the issuer's tokens as OIDC principals", "issuer", o.Issuer, "audience", o.Audience,
			"tenant_claim", o.TenantClaim, "revocations", o.Revocations != nil)
	}
	return nil
}

// runFeeds starts each revocation feed's poll (D316) and waits, at most FeedWait in all, for each one's first read.
func (r *Replica) runFeeds(ctx context.Context, goLoop func(string, func(context.Context) error)) {
	for _, f := range r.feeds {
		goLoop("oidc revocations "+f.Issuer(), f.Run)
	}
	wctx, cancel := context.WithTimeout(ctx, FeedWait)
	defer cancel()
	for _, f := range r.feeds {
		if !f.Wait(wctx) {
			r.log.Warn("serving before the issuer's revocation feed was read: its tokens are refused (503) until it is",
				"issuer", f.Issuer(), "url", f.URL())
		}
	}
}

// writeFeeds writes the revocation feeds' gauges (D316): kontra_gate_oidc_revocations_age_seconds, how long ago the
// last successful read began (+Inf before any), and kontra_gate_oidc_revocations_fresh, 1 while that is under
// max_stale, by issuer. An operator alerts on fresh 0: the issuer's tokens are being refused.
func writeFeeds(w io.Writer, replica string, feeds []*oidc.Feed) {
	if len(feeds) == 0 {
		return
	}
	const age, fresh = "kontra_gate_oidc_revocations_age_seconds", "kontra_gate_oidc_revocations_fresh"
	_, _ = fmt.Fprintf(w, "# HELP %s Seconds since the last successful read of the issuer's revocation feed began.\n# TYPE %s gauge\n", age, age)
	for _, f := range feeds {
		v := math.Inf(1)
		if d, ok := f.Age(); ok {
			v = d.Seconds()
		}
		_, _ = fmt.Fprintf(w, "%s{replica=%q,issuer=%q} %s\n", age, replica, f.Issuer(), strconv.FormatFloat(v, 'f', 3, 64))
	}
	_, _ = fmt.Fprintf(w, "# HELP %s 1 while the issuer's revocation feed is fresh; 0 refuses every token of the issuer (fail closed).\n# TYPE %s gauge\n", fresh, fresh)
	for _, f := range feeds {
		b := 0
		if f.Fresh() {
			b = 1
		}
		_, _ = fmt.Fprintf(w, "%s{replica=%q,issuer=%q} %d\n", fresh, replica, f.Issuer(), b)
	}
}
