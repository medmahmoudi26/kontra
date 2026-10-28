package enroll

import (
	"context"
	"errors"
	"slices"
	"unicode/utf8"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/tunnel"
)

// EventAnonymous is the audit row of an anonymous session that opened a tenant, or changed what its tenant serves
// (spec/tunnel.md §5: every enrollment and revocation is a row in audit).
const EventAnonymous = "tunnel.anonymous_session"

// anonymousSession decides what a hello does with anonymous sessions (spec/tunnel.md §6, DM4-18, DM4-19), after the
// key, rotation and API key checks (steps 1–4). It returns the decision of an anonymous session it opened or resumed;
// nil to go on with §5's rules, with a key of an anonymous tenant then moved by write (moveFrom).
//
//   - anonymous: true with neither enroll nor auth nor rotate_from ("asks"), from a key the gate does not know or of
//     an ended anonymous tenant: a new anonymous tenant (anon.Open), or anonymous_unavailable on a gate without
//     tunnel.anonymous;
//   - from a key of a live anonymous tenant: resumed when the hello asks, or carries an auth API key that cannot
//     convert it (a hire-only key, DM4-19: the session's hiring principal); an auth key with enroll:* converts it
//     (§6.3, anon.Convert, WP-06); a hello with neither (a probe, §2.4) is unknown_key, so that a node whose earlier
//     hello converted nothing sends it again. On a gate without tunnel.anonymous only a hello that asks is
//     anonymous_unavailable; any other goes through §5 as below (a probe is unknown_key);
//   - enroll tokens from a key of an anonymous tenant: §5 enrolls the key into the tokens' tenant, moving it, and a
//     live anonymous tenant ends at once (DM4-19);
//   - any other hello from a key of an ended anonymous tenant goes through §5 as a new key would (moved if admitted).
//
// A team or account key's hello is §5's whatever it carries: the field is ignored.
func (a *admission) anonymousSession(ctx context.Context) (*Decision, error) {
	h := a.h
	lim := a.s.opts.Anonymous
	asks := h.Anonymous && h.Auth == "" && len(h.Enroll) == 0 && h.RotateFrom == nil
	k := a.anonKey
	switch {
	case k != nil && len(a.hashes) > 0:
		a.moveFrom = &k.Key
		if !k.tenantEnded {
			a.endTenant = k.TenantID
		}
		return nil, nil
	case k != nil && !k.tenantEnded:
		switch {
		case lim == nil && asks:
			return nil, tunnel.Refuse(tunnel.CodeAnonymousUnavailable, "this gate opens no anonymous sessions (tunnel.anonymous)")
		case lim == nil:
			// A gate without tunnel.anonymous (turned off while the tenant lives) resumes no anonymous session: any other
			// hello is §5's, as a new key's (the M4 WP-03 review). The probe (§2.4) is then unknown_key, which tells the
			// node that nothing committed; a hello §5 admits moves the key, and the tenant it leaves ends at once, as
			// DM4-19's conversions end it.
			a.moveFrom = &k.Key
			a.endTenant = k.TenantID
			return nil, nil
		case a.principal != nil && a.principal.HasScope(auth.ScopeEnrollAll) && a.s.opts.EnrollWithAPIKey:
			return a.convert(ctx)
		case a.principal != nil || asks:
			return a.openAnonymous(ctx)
		}
		return nil, tunnel.Refuse(tunnel.CodeUnknownKey,
			"the key %s holds an anonymous session, which only a hello asking for one (anonymous: true) resumes", h.KID)
	case k != nil && asks:
		if lim == nil {
			return nil, tunnel.Refuse(tunnel.CodeAnonymousUnavailable, "this gate opens no anonymous sessions (tunnel.anonymous)")
		}
		return a.openAnonymous(ctx)
	case k != nil:
		a.moveFrom = &k.Key
		return nil, nil
	case a.key == nil && a.old == nil && asks:
		if lim == nil {
			return nil, tunnel.Refuse(tunnel.CodeAnonymousUnavailable,
				"this gate opens no anonymous sessions (tunnel.anonymous): enroll the key with --enroll <token>")
		}
		return a.openAnonymous(ctx)
	}
	return nil, nil
}

// convert is §6.3's conversion of a key of a live anonymous tenant by an auth API key with enroll:* (DM4-11): M4
// WP-06 builds anon.Convert and this branch's decision.
func (a *admission) convert(ctx context.Context) (*Decision, error) {
	var account uuid.UUID
	if a.principal.TenantID != nil {
		account = *a.principal.TenantID
	}
	if err := anon.Convert(ctx, a.tx, anon.ConvertRequest{KeyID: a.h.KID, AccountID: account}); err != nil {
		return nil, err
	}
	return nil, errors.New("enroll: conversion decision not built (M4 WP-06)")
}

// openAnonymous opens or resumes the key's anonymous tenant (anon.Open) and decides the welcome: the declared
// services at the tenant's URLs, welcome.anonymous.expires_at, and the hiring principal (the auth API key's, else
// node:<key_id>). Open's refusals are the handshake's: anonymous_unavailable (max_tenants), rate_limited (the
// network's tenants), service_denied (over services, or a public name not declared; nothing enrolled), kind_mismatch.
func (a *admission) openAnonymous(ctx context.Context) (*Decision, error) {
	h := a.h
	declared := make([]anon.Service, 0, len(h.Services))
	for _, s := range h.Services {
		declared = append(declared, anon.Service{Kind: s.Kind, Name: s.Name, Public: slices.Contains(h.Public, s.Name)})
	}
	var public []string
	for _, p := range h.Public {
		if !slices.ContainsFunc(h.Services, func(s tunnel.Service) bool { return s.Name == p }) {
			return nil, tunnel.Refuse(tunnel.CodeServiceDenied, "public: %q is not one of the declared services; nothing was enrolled", clipName(p))
		}
		if !slices.Contains(public, p) {
			public = append(public, p)
		}
	}
	key := anon.Key{ID: h.KID, PubKey: a.req.PubKey, NodeName: h.Name}
	if a.anonKey != nil {
		key.TenantID = a.anonKey.TenantID
	}
	t, err := anon.Open(ctx, a.tx, *a.s.opts.Anonymous, anon.OpenRequest{Key: key, Network: anon.Network(a.req.Client), Services: declared})
	switch {
	case errors.Is(err, anon.ErrUnavailable):
		return nil, tunnel.Refuse(tunnel.CodeAnonymousUnavailable, "this gate opens no more anonymous sessions now; try later")
	case errors.Is(err, anon.ErrTooManyTenants):
		return nil, tunnel.Refuse(tunnel.CodeRateLimited, "too many anonymous sessions from this network; try later")
	case errors.Is(err, anon.ErrTooManyServices):
		return nil, tunnel.Refuse(tunnel.CodeServiceDenied, "an anonymous session serves at most %d services; nothing was enrolled",
			a.s.opts.Anonymous.Services)
	case errors.Is(err, anon.ErrKindMismatch):
		return nil, tunnel.Refuse(tunnel.CodeKindMismatch, "%v", err)
	case anon.IsUniqueViolation(err):
		return nil, errRetry // a concurrent handshake inserted the key: the next attempt sees it
	case err != nil:
		return nil, err
	}
	if a.anonKey != nil && a.anonKey.NodeName != h.Name {
		if _, err := a.tx.Exec(ctx, `update kontra.tunnel_keys set node_name = $2 where key_id = $1`, h.KID, h.Name); err != nil {
			return nil, err
		}
	}
	if !t.Resumed || len(t.Enabled) > 0 || len(t.Disabled) > 0 {
		f := audit.Fields{
			"actor": "tunnel handshake", "key_id": h.KID, "tenant": t.Slug, "tenant_id": t.ID, "node_name": h.Name,
			"resumed": t.Resumed, "enabled": anyStrings(t.Enabled), "disabled": anyStrings(t.Disabled),
			"public": anyStrings(public), "expires_at": t.ExpiresAt, "session_id": a.req.SessionID, "replica_id": a.req.ReplicaID,
		}
		if a.anonKey != nil && !t.Resumed {
			f["moved_from_id"] = a.anonKey.TenantID
		}
		if _, err := audit.Append(ctx, a.tx, EventAnonymous, f); err != nil {
			return nil, err
		}
	}
	d := &Decision{
		Key:       Key{KeyID: h.KID, TenantID: t.ID, Tenant: t.Slug, PubKey: a.req.PubKey, NodeName: h.Name},
		Anonymous: &Anonymous{ExpiresAt: t.ExpiresAt, Resumed: t.Resumed, Public: public},
	}
	for _, s := range h.Services {
		d.Served = append(d.Served, tunnel.WelcomeService{Kind: s.Kind, Name: s.Name, URL: a.s.serviceURL(s.Kind, t.Slug, s.Name)})
	}
	if a.principal != nil {
		d.HirePrincipal = a.principal.ID
		hire := *a.principal
		d.Hire = &hire
	} else {
		d.HirePrincipal = string(auth.KindNode) + ":" + h.KID
	}
	return d, nil
}

func anyStrings(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// clipName shortens a name a refusal quotes back, at a rune boundary.
func clipName(s string) string {
	const most = 64
	if len(s) <= most {
		return s
	}
	cut := most
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
