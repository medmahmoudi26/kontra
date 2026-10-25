package auth

import (
	"fmt"
	"path"
	"strings"

	"github.com/medmahmoudi26/kontra/internal/config"
)

// AllowList is a parsed services.<name>.allow (spec §2.3, D20): deny by default; entries key:<prefix glob>,
// org:<tenant>, oidc:sub:<subject>, node:<key_id>, anonymous. `anonymous` is never implied; when present
// it admits every principal, and an authenticated caller keeps its own principal, buyer and via (the
// authenticator, not the list, decides who the caller is). The zero value denies everyone.
type AllowList struct {
	entries []config.AllowEntry
}

// ParseAllowList parses the entries of one service with the grammar config validates at load
// (config.ParseAllowEntry): an unknown kind, or a key: glob that can never match a stored prefix, is an
// error.
func ParseAllowList(entries []string) (AllowList, error) {
	out := AllowList{entries: make([]config.AllowEntry, 0, len(entries))}
	for _, s := range entries {
		e, err := config.ParseAllowEntry(s)
		if err != nil {
			return AllowList{}, fmt.Errorf("allow: %w", err)
		}
		out.entries = append(out.entries, e)
	}
	return out, nil
}

// Admits reports whether the list admits p for a call to the service, or for its cards when the service
// is neither anonymous nor listed (spec §2.1). An API key without the hire scope is never admitted
// (spec §2.3). Entries match:
//
//   - key:<glob>: an API key principal whose prefix the glob matches (* any run, ? one character);
//   - org:<tenant>: any principal whose tenant has that slug (its API keys, and from M2 its nodes' keys);
//   - oidc:sub:<subject>: that OIDC subject, arriving over oidc or mcp;
//   - node:<key_id>: that node key hiring without an API key (M2);
//   - anonymous: everyone, authenticated or not.
func (a AllowList) Admits(p Principal) bool {
	if !p.MayHire() {
		return false
	}
	for _, e := range a.entries {
		if entryMatches(e, p) {
			return true
		}
	}
	return false
}

func entryMatches(e config.AllowEntry, p Principal) bool {
	switch e.Kind {
	case config.AllowAnonymous:
		return true
	case config.AllowKey:
		prefix, ok := strings.CutPrefix(p.ID, string(KindKey)+":")
		if p.Kind != KindKey || !ok {
			return false
		}
		// The glob alphabet is [A-Za-z0-9_*?] (config.ParseAllowEntry), so path.Match has no '/', '[' or
		// '\\' to interpret and its * and ? mean what the allow: grammar says.
		m, err := path.Match(e.Value, prefix)
		return err == nil && m
	case config.AllowOrg:
		return p.TenantID != nil && p.TenantSlug != "" && p.TenantSlug == e.Value
	case config.AllowOIDCSub:
		return p.Kind == KindOIDC && p.ID == "oidc:sub:"+e.Value
	case config.AllowNode:
		return p.Kind == KindNode && p.ID == "node:"+e.Value
	}
	return false
}

// AdmitsAnonymous reports whether the list contains `anonymous`: such a service's cards are public and
// unauthenticated callers reach it (spec §2.1).
func (a AllowList) AdmitsAnonymous() bool {
	for _, e := range a.entries {
		if e.Kind == config.AllowAnonymous {
			return true
		}
	}
	return false
}

// OnlyAnonymous reports whether every entry is `anonymous` (and there is at least one): the served card
// then omits its security fields (spec §3).
func (a AllowList) OnlyAnonymous() bool {
	if len(a.entries) == 0 {
		return false
	}
	for _, e := range a.entries {
		if e.Kind != config.AllowAnonymous {
			return false
		}
	}
	return true
}

// Len is the number of entries; an empty list denies everyone.
func (a AllowList) Len() int { return len(a.entries) }
