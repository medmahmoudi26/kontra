package auth

import (
	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// AllowList is a parsed services.<name>.allow (spec §2.3): deny by default; entries key:<prefix glob>,
// org:<tenant>, oidc:sub:<subject>, node:<key_id>, anonymous. `anonymous` is never implied; when present
// it admits every principal, and an authenticated caller keeps its own principal, buyer and via.
type AllowList struct {
	entries []allowEntry
}

type allowEntry struct {
	kind    string
	pattern string
}

// ParseAllowList parses the entries of one service; an unknown entry kind is an error at config load.
func ParseAllowList(entries []string) (AllowList, error) {
	return AllowList{}, unimpl.Error("auth.ParseAllowList", "WP-03")
}

// Admits reports whether the list admits p. tenantSlug resolves p's tenant for org: entries.
func (a AllowList) Admits(p Principal, tenantSlug func() string) bool {
	panic(unimpl.Error("auth.AllowList.Admits", "WP-03"))
}

// AdmitsAnonymous reports whether the list contains `anonymous`: such a service's cards are public and
// unauthenticated callers reach it (spec §2.1).
func (a AllowList) AdmitsAnonymous() bool {
	panic(unimpl.Error("auth.AllowList.AdmitsAnonymous", "WP-03"))
}

// OnlyAnonymous reports whether the list is exactly [anonymous]; the served card then omits its security
// fields (spec §3).
func (a AllowList) OnlyAnonymous() bool {
	panic(unimpl.Error("auth.AllowList.OnlyAnonymous", "WP-03"))
}
