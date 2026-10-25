// Package ssrf is the one dialer for every connection the gate opens to a URL it was given: direct
// backends, push webhooks, operator URLs (spec/relay.md §10.3, D17).
//
// It resolves the host, checks every address the name resolves to, pins the one it uses, and refuses
// every address that is not globally reachable per the IANA IPv4 and IPv6 special-purpose registries
// (loopback, RFC 1918, link-local, ULA, CGNAT 100.64.0.0/10, 0.0.0.0/8, 198.18.0.0/15, 240.0.0.0/4,
// broadcast, multicast, documentation, cloud metadata), after unmapping IPv4-mapped IPv6 and extracting
// the IPv4 inside NAT64 (64:ff9b::/96, 64:ff9b:1::/48), 6to4 and Teredo addresses. Redirects are never
// followed, the environment's proxy is ignored, and plain http:// is refused. AllowPrivate (per backend,
// or push.allow_private for tests) skips the address check, never the redirect rule.
package ssrf

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Refusals. Each is returned before any connection to the refused address is attempted.
var (
	ErrAddress  = errors.New("ssrf: address is not globally reachable")
	ErrScheme   = errors.New("ssrf: scheme not allowed")
	ErrRedirect = errors.New("ssrf: redirects are not followed")
)

// Policy is what one destination is allowed.
type Policy struct {
	AllowPrivate bool
}

// CheckURL is the set-time check of a push URL or backend URL (spec §10.1): scheme, no userinfo, and
// every address the host resolves to (unless AllowPrivate).
func CheckURL(ctx context.Context, u *url.URL, p Policy) error {
	return unimpl.Error("ssrf.CheckURL", "WP-04")
}

// CheckAddr reports why addr may not be dialed, nil when it may.
func CheckAddr(addr netip.Addr) error {
	return unimpl.Error("ssrf.CheckAddr", "WP-04")
}

// Dialer is a net.Dialer that applies the address rules at dial time on the pinned address.
type Dialer struct {
	policy Policy
	inner  net.Dialer
}

// NewDialer returns a dialer for policy p.
func NewDialer(p Policy) *Dialer {
	return &Dialer{policy: p}
}

// DialContext resolves, checks every address, and connects to one checked address.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return nil, unimpl.Error("ssrf.Dialer.DialContext", "WP-04")
}

// NewClient returns an HTTP client over the dialer: no proxy, no redirects (a 3xx is returned as is),
// HTTP/1.1 only, and no overall timeout (deadlines come from the request context).
func NewClient(p Policy) *http.Client {
	panic(unimpl.Error("ssrf.NewClient", "WP-04"))
}
