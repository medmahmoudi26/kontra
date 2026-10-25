package edge

import (
	"context"
	"net/http"
	"net/netip"
	"strings"

	"kontra.sh/kontra/internal/config"
)

// ClientIP is edge.client_ip (spec §2.2, D69): where rps_per_ip and conns_per_ip take a client's address.
//
//   - remote (the default): the connection's remote address.
//   - x_forwarded_for: on a connection from a Trusted prefix, the rightmost X-Forwarded-For hop that is
//     not trusted (or the leftmost hop when every hop is trusted); on any other connection, or when the
//     header is missing or malformed, the remote address.
//   - proxy_protocol: on a connection from a Trusted prefix, the source address of its PROXY v1 or v2
//     header, read by the listener; elsewhere the remote address.
type ClientIP struct {
	From    config.ClientIPSource
	Trusted []netip.Prefix
}

// Trusts reports whether a is inside a trusted prefix.
func (c ClientIP) Trusts(a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	a = a.Unmap()
	for _, p := range c.Trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Addr returns the client address of r under c.
func (c ClientIP) Addr(r *http.Request) netip.Addr {
	remote := remoteAddr(r)
	if c.From != config.ClientIPXForwardedFor || !c.Trusts(remote) {
		return remote
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, h := range strings.Split(v, ",") {
			hops = append(hops, strings.TrimSpace(h))
		}
	}
	if len(hops) == 0 {
		return remote
	}
	var leftmost netip.Addr
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := parseHop(hops[i])
		if !ok {
			return remote
		}
		if !c.Trusts(a) {
			return a
		}
		leftmost = a
	}
	return leftmost
}

// parseHop reads one X-Forwarded-For element: an address, optionally with a port ("1.2.3.4:80",
// "[2001:db8::1]:80").
func parseHop(s string) (netip.Addr, bool) {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap(), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), true
	}
	if t, ok := strings.CutPrefix(s, "["); ok {
		if t, ok := strings.CutSuffix(t, "]"); ok {
			if a, err := netip.ParseAddr(t); err == nil {
				return a.Unmap(), true
			}
		}
	}
	return netip.Addr{}, false
}

func remoteAddr(r *http.Request) netip.Addr {
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr().Unmap()
	}
	if a, err := netip.ParseAddr(r.RemoteAddr); err == nil {
		return a.Unmap()
	}
	return netip.Addr{}
}

// ipv6CountBits is the prefix an IPv6 client address is counted by.
const ipv6CountBits = 64

// countKey is what rps_per_ip and conns_per_ip count a client address under: an IPv4 address itself, an
// IPv6 address's /64.
//
// SPEC-CONFLICT (D83): spec §2.2 counts both limits "by the client address". One end site (a home, a cloud
// instance, a phone) commonly holds a whole IPv6 /64, so counting each address would give such a client
// a fresh token bucket and a fresh connection quota per address it picks, and rps_per_ip would no longer
// bound credential spraying. The edge counts an IPv6 client by its /64 instead (the network, as
// spec/tunnel.md's anonymous-tenant cap counts IPv6 by /48); ClientAddr keeps the full address.
func countKey(a netip.Addr) netip.Addr {
	a = a.Unmap()
	if !a.Is6() {
		return a
	}
	p, err := a.Prefix(ipv6CountBits)
	if err != nil {
		return a
	}
	return p.Addr()
}

type clientAddrKey struct{}

// ClientAddr is the request's client address (ClientIP.Addr), in full: the edge counts an IPv6 client by
// its /64 (countKey), but this is the address itself.
func ClientAddr(ctx context.Context) (netip.Addr, bool) {
	a, ok := ctx.Value(clientAddrKey{}).(netip.Addr)
	return a, ok
}

func withClientAddr(ctx context.Context, a netip.Addr) context.Context {
	return context.WithValue(ctx, clientAddrKey{}, a)
}
