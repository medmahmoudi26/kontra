package auth

import "kontra.sh/kontra/internal/config"

// WithoutAnonymous is the list without its anonymous entries: what admits a principal by name (an API key glob, its
// tenant, its OIDC subject or its issuer's oidc:*, its node key). The gate's own /mcp shows search only the services
// that admit the principal so, or that the operator lists (spec/relay.md §12.2): a public service is not offered to
// everyone. oidc:* names the principals of one issuer as key:* names every API key of the gate (D301).
func (a AllowList) WithoutAnonymous() AllowList {
	out := AllowList{entries: make([]config.AllowEntry, 0, len(a.entries))}
	for _, e := range a.entries {
		if e.Kind != config.AllowAnonymous {
			out.entries = append(out.entries, e)
		}
	}
	return out
}
