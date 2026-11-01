package oidc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
)

// Tenants resolves a token's tenant claim (D315): the live tenant whose slug it names and that an issuer's principal
// may belong to (kind team or account, neither deleted nor expired). ok false is no such tenant; an error is the
// gate's own (its database). internal/tenant's Resolver implements it.
type Tenants interface {
	Tenant(ctx context.Context, slug string) (id uuid.UUID, ok bool, err error)
}

// IssuerConfig configures an Issuer.
type IssuerConfig struct {
	Verifier *Verifier
	// TenantClaim names the claim that maps a token to a tenant by slug ("" maps none); Tenants resolves it and is
	// required with it.
	TenantClaim string
	Tenants     Tenants
	// Feed is the issuer's revocation feed; nil accepts a token until it expires.
	Feed   *Feed
	Logger *slog.Logger
}

// Issuer is one configured issuer's tokens as principals (spec/relay.md §2.3, §12.1; D314 to D316): the verifier of
// its signatures and standard claims (shared by the A2A and the MCP routes when auth.oidc and auth.mcp_oauth name the
// same issuer), then its revocation feed and its tenant claim. A gate builds one per distinct issuer.
type Issuer struct {
	v       *Verifier
	claim   string
	tenants Tenants
	feed    *Feed
	log     *slog.Logger
}

// NewIssuer checks cfg.
func NewIssuer(cfg IssuerConfig) (*Issuer, error) {
	if cfg.Verifier == nil {
		return nil, errors.New("oidc: an issuer needs a verifier")
	}
	if cfg.TenantClaim != "" && cfg.Tenants == nil {
		return nil, errors.New("oidc: a tenant claim needs a tenant resolver")
	}
	if cfg.Feed != nil && cfg.Feed.Issuer() != cfg.Verifier.Issuer() {
		return nil, errors.New("oidc: the revocation feed is another issuer's")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Issuer{v: cfg.Verifier, claim: cfg.TenantClaim, tenants: cfg.Tenants, feed: cfg.Feed,
		log: cfg.Logger.With("component", "oidc", "issuer", cfg.Verifier.Issuer())}, nil
}

// Issuer is the issuer's identifier.
func (i *Issuer) Issuer() string { return i.v.Issuer() }

// Verifier is the issuer's verifier.
func (i *Issuer) Verifier() *Verifier { return i.v }

// Feed is the issuer's revocation feed, nil without one.
func (i *Issuer) Feed() *Feed { return i.feed }

// Principal verifies token for audience (Verifier.Verify), then checks it against the issuer's revocation feed and
// maps its tenant claim, and returns the principal it stands for: oidc:sub:<sub>, buyer UUIDv5(issuer ‖ sub), the
// tenant the claim names (none without the claim), and via. A token the gate refuses is ErrInvalidToken (a 401); a
// key set, feed or tenant the gate could not read is ErrUnavailable or the database's error (the gate's own failure).
func (i *Issuer) Principal(ctx context.Context, token, audience string, via auth.Via) (auth.Principal, error) {
	c, err := i.v.Verify(ctx, token, audience)
	if err != nil {
		return auth.Principal{}, err
	}
	if i.feed != nil {
		if err := i.checkFeed(c); err != nil {
			return auth.Principal{}, err
		}
	}
	p := auth.Principal{Kind: auth.KindOIDC, ID: "oidc:sub:" + c.Subject, BuyerID: BuyerID(i.Issuer(), c.Subject), Via: via}
	if i.claim == "" {
		return p, nil
	}
	if _, present := c.members[i.claim]; !present {
		return p, nil // D315: a token without the claim is a principal without a tenant
	}
	var slug string
	if err := str(c.members, i.claim, &slug, true); err != nil || !config.NameRE.MatchString(slug) {
		return auth.Principal{}, i.refuse("tenant", fmt.Errorf("the claim %s is not a tenant's slug", i.claim))
	}
	id, ok, err := i.tenants.Tenant(ctx, slug)
	switch {
	case err != nil:
		return auth.Principal{}, fmt.Errorf("oidc: the tenant of a token: %w", err)
	case !ok:
		return auth.Principal{}, i.refuse("tenant", fmt.Errorf("the claim %s names no live team or account tenant", i.claim))
	}
	p.TenantID, p.TenantSlug = &id, slug
	return p, nil
}

// checkFeed is D316's check of a verified token: a sid, an iat no more than Skew ahead, and then the feed's verdict.
func (i *Issuer) checkFeed(c Claims) error {
	var sid string
	if err := str(c.members, "sid", &sid, true); err != nil {
		return i.refuse("sid", err)
	}
	if sid == "" || len(sid) > MaxSessionID {
		return i.refuse("sid", fmt.Errorf("sid is empty or longer than %d bytes", MaxSessionID))
	}
	iat, ok, err := numericDate(c.members, "iat")
	switch {
	case err != nil:
		return i.refuse("iat", err)
	case !ok:
		return i.refuse("iat", errors.New("no iat: the revocation feed needs the token's lifetime"))
	case iat.After(i.v.cfg.Now().Add(Skew)):
		return i.refuse("iat", errors.New("iat is in the future"))
	}
	err = i.feed.check(sid, iat, c.Expiry)
	switch {
	case errors.Is(err, errRevoked):
		d := SessionDigest(sid)
		i.log.Info("a token of a revoked session was refused", "session", hex.EncodeToString(d[:4]), "subject", c.Subject)
		return ErrInvalidToken
	case errors.Is(err, errLifetime):
		return i.refuse("lifetime", fmt.Errorf("%w (exp - iat = %s)", err, c.Expiry.Sub(iat).Round(time.Second)))
	}
	return err
}

func (i *Issuer) refuse(what string, err error) error {
	i.log.Debug("a bearer token was refused", "part", what, "error", err)
	return ErrInvalidToken
}

// A2A is the bearer-JWT check of the A2A routes (auth.oidc, spec §2.3; D314): the issuer's tokens whose aud contains
// the configured audience, as oidc:sub:<sub> with via oidc. It implements auth.A2AVerifier.
type A2A struct {
	iss      *Issuer
	audience string
}

var _ auth.A2AVerifier = (*A2A)(nil)

// NewA2A is the A2A routes' check of iss's tokens for audience (auth.oidc.audience).
func NewA2A(iss *Issuer, audience string) (*A2A, error) {
	if iss == nil || audience == "" {
		return nil, errors.New("oidc: the A2A routes need an issuer and an audience")
	}
	return &A2A{iss: iss, audience: audience}, nil
}

// VerifyA2A verifies a bearer JWT of an A2A route: auth.ErrUnauthenticated for a token the gate refuses, the gate's
// own error for an issuer, a feed or a tenant it could not read.
func (a *A2A) VerifyA2A(ctx context.Context, token string) (auth.Principal, error) {
	p, err := a.iss.Principal(ctx, token, a.audience, auth.ViaOIDC)
	if errors.Is(err, ErrInvalidToken) {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	return p, err
}
