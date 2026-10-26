package tunnel

import (
	"net/url"

	"kontra.sh/kontra/internal/unimpl"
)

// CanonicalOrigin is scheme://host[:port] of u, lowercase, the scheme's default port elided (§4 hello
// "origin"): what the node dialed, and what the gate compares with the origin of its public_url.
func CanonicalOrigin(u *url.URL) (string, error) {
	return "", unimpl.Error("tunnel.CanonicalOrigin", "WP-01")
}

// CheckGateURL parses --gate (§3): https://, or http:// only on localhost, 127.0.0.1 and [::1]
// (contributors' gates); no userinfo, query or fragment.
func CheckGateURL(raw string) (*url.URL, error) {
	return nil, unimpl.Error("tunnel.CheckGateURL", "WP-01")
}
