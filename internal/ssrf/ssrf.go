// Package ssrf is the one dialer for every connection the gate opens to a URL it was given: direct
// backends, push webhooks, operator URLs (spec/relay.md §10.3, D17, D67).
//
// It resolves the host, checks every address the name resolves to, pins the one it uses, and refuses
// every address that is not globally reachable per the IANA IPv4 and IPv6 special-purpose registries
// (loopback, RFC 1918, link-local, ULA, CGNAT 100.64.0.0/10, 0.0.0.0/8, 198.18.0.0/15, 240.0.0.0/4,
// broadcast, multicast, documentation, cloud metadata), after unmapping IPv4-mapped IPv6 and extracting
// the IPv4 inside NAT64 (64:ff9b::/96, 64:ff9b:1::/48), 6to4 and Teredo addresses. Redirects are never
// followed and the environment's proxy is ignored. Only https:// is allowed, except where AllowPrivate
// covers the destination (per backend, or push.allow_private for tests): it skips the address check and
// also permits http:// (D67), and never relaxes the redirect or proxy rule.
package ssrf

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Refusals. Each is returned before any connection to the refused address is attempted.
var (
	ErrAddress  = errors.New("ssrf: address is not globally reachable")
	ErrScheme   = errors.New("ssrf: scheme not allowed")
	ErrRedirect = errors.New("ssrf: redirects are not followed")
	// ErrURL is a URL the gate never dials: no host, userinfo, or a port that is not a port.
	ErrURL = errors.New("ssrf: URL not allowed")
	// ErrNoHost is a host name that does not resolve: the name does not exist (NXDOMAIN), or it has no
	// address.
	ErrNoHost = errors.New("ssrf: host does not resolve")
	// ErrLookup is a lookup that failed for any other reason (a timeout, SERVFAIL, an unreachable
	// resolver): the gate's own fault, not the URL's. The resolver's error stays in the chain.
	ErrLookup = errors.New("ssrf: host lookup failed")
)

// Resolver resolves a host name to its addresses; net.DefaultResolver implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Policy is what one destination is allowed.
type Policy struct {
	// AllowPrivate skips the address check and permits http:// (D67); redirects and the environment's
	// proxy stay refused.
	AllowPrivate bool
	// Resolver replaces net.DefaultResolver (tests).
	Resolver Resolver
}

func (p Policy) resolver() Resolver {
	if p.Resolver != nil {
		return p.Resolver
	}
	return net.DefaultResolver
}

// CheckScheme refuses every scheme but https, and http under AllowPrivate.
func (p Policy) CheckScheme(u *url.URL) error {
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if p.AllowPrivate {
			return nil
		}
		return fmt.Errorf("%w: http:// needs allow_private (only https:// is dialed otherwise)", ErrScheme)
	}
	return fmt.Errorf("%w: %q", ErrScheme, u.Scheme)
}

// CheckURL is the set-time check of a push URL or backend URL (spec §10.1, §10.3): the scheme, no userinfo,
// a host and a valid port, and every address the host resolves to (unless AllowPrivate). It never connects.
func CheckURL(ctx context.Context, u *url.URL, p Policy) error {
	if u == nil {
		return fmt.Errorf("%w: no URL", ErrURL)
	}
	if err := p.CheckScheme(u); err != nil {
		return err
	}
	if u.User != nil {
		return fmt.Errorf("%w: userinfo in a URL", ErrURL)
	}
	if u.Opaque != "" || u.Hostname() == "" {
		return fmt.Errorf("%w: no host", ErrURL)
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%w: port %q", ErrURL, port)
		}
	}
	if p.AllowPrivate {
		return nil
	}
	_, err := resolve(ctx, p.resolver(), u.Hostname())
	return err
}

// CheckAddr reports why addr may not be dialed, nil when it may. IPv4-mapped IPv6 addresses are
// unmapped, and the IPv4 address inside NAT64 (64:ff9b::/96), 6to4 and Teredo addresses is checked too.
func CheckAddr(addr netip.Addr) error {
	if !addr.IsValid() {
		return fmt.Errorf("%w: invalid address", ErrAddress)
	}
	if addr.Zone() != "" {
		return fmt.Errorf("%w: %s is scoped", ErrAddress, addr)
	}
	addr = addr.Unmap()
	if addr.Is4() {
		if why := refuse4(addr); why != "" {
			return fmt.Errorf("%w: %s is %s", ErrAddress, addr, why)
		}
		return nil
	}
	if why := refuse6(addr); why != "" {
		return fmt.Errorf("%w: %s is %s", ErrAddress, addr, why)
	}
	return nil
}

type range4 struct {
	p   netip.Prefix
	why string
}

func mustPrefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }

// refused4 is every IPv4 range of the IANA special-purpose registry that is not globally reachable, plus
// multicast, the cloud metadata addresses outside them, and the deprecated 6to4 relay anycast. 192.0.0.9
// and 192.0.0.10 (PCP and TURN anycast) are globally reachable and allowed by allowed4.
var refused4 = []range4{
	{mustPrefix("0.0.0.0/8"), "\"this network\""},
	{mustPrefix("10.0.0.0/8"), "private (RFC 1918)"},
	{mustPrefix("100.64.0.0/10"), "shared address space (CGNAT; cloud metadata 100.100.100.200)"},
	{mustPrefix("127.0.0.0/8"), "loopback"},
	{mustPrefix("169.254.0.0/16"), "link-local (cloud metadata 169.254.169.254)"},
	{mustPrefix("172.16.0.0/12"), "private (RFC 1918)"},
	{mustPrefix("192.0.0.0/24"), "IETF protocol assignments"},
	{mustPrefix("192.0.2.0/24"), "documentation (TEST-NET-1)"},
	{mustPrefix("192.88.99.0/24"), "deprecated 6to4 relay anycast"},
	{mustPrefix("192.168.0.0/16"), "private (RFC 1918)"},
	{mustPrefix("198.18.0.0/15"), "benchmarking"},
	{mustPrefix("198.51.100.0/24"), "documentation (TEST-NET-2)"},
	{mustPrefix("203.0.113.0/24"), "documentation (TEST-NET-3)"},
	{mustPrefix("224.0.0.0/4"), "multicast"},
	{mustPrefix("240.0.0.0/4"), "reserved (and broadcast)"},
	{mustPrefix("168.63.129.16/32"), "cloud metadata (Azure)"},
}

var allowed4 = []netip.Prefix{mustPrefix("192.0.0.9/32"), mustPrefix("192.0.0.10/32")}

func refuse4(a netip.Addr) string {
	for _, p := range allowed4 {
		if p.Contains(a) {
			return ""
		}
	}
	for _, r := range refused4 {
		if r.p.Contains(a) {
			return r.why
		}
	}
	return ""
}

var (
	global6   = mustPrefix("2000::/3")
	nat64     = mustPrefix("64:ff9b::/96")
	nat64TEST = mustPrefix("64:ff9b:1::/48")
	teredo    = mustPrefix("2001::/32")
	sixToFour = mustPrefix("2002::/16")
	// ietf6 is 2001::/23, IETF protocol assignments, not globally reachable except allowedIETF6.
	ietf6        = mustPrefix("2001::/23")
	allowedIETF6 = []netip.Prefix{
		mustPrefix("2001:1::1/128"), mustPrefix("2001:1::2/128"), mustPrefix("2001:1::3/128"), // PCP, TURN, DNS-SD SRP anycast
		mustPrefix("2001:3::/32"),     // AMT
		mustPrefix("2001:4:112::/48"), // AS112-v6
		mustPrefix("2001:20::/28"),    // ORCHIDv2
		mustPrefix("2001:30::/28"),    // drone remote ID
	}
	refused6 = []range4{
		{mustPrefix("2001:db8::/32"), "documentation"},
		{mustPrefix("3fff::/20"), "documentation"},
	}
)

func refuse6(a netip.Addr) string {
	switch {
	case nat64.Contains(a):
		b := a.As16()
		return embedded(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), "NAT64")
	case nat64TEST.Contains(a):
		return "local-use NAT64 (64:ff9b:1::/48)"
	case !global6.Contains(a):
		switch {
		case a.IsLoopback():
			return "loopback"
		case a.IsUnspecified():
			return "unspecified"
		case a.IsMulticast():
			return "multicast"
		case a.IsLinkLocalUnicast():
			return "link-local"
		case a.IsPrivate():
			return "unique local (ULA)"
		}
		return "outside global unicast (2000::/3)"
	case teredo.Contains(a):
		b := a.As16()
		server := netip.AddrFrom4([4]byte{b[4], b[5], b[6], b[7]})
		client := netip.AddrFrom4([4]byte{b[12] ^ 0xff, b[13] ^ 0xff, b[14] ^ 0xff, b[15] ^ 0xff})
		if why := embedded(server, "Teredo server"); why != "" {
			return why
		}
		return embedded(client, "Teredo client")
	case sixToFour.Contains(a):
		b := a.As16()
		return embedded(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), "6to4")
	case ietf6.Contains(a):
		for _, p := range allowedIETF6 {
			if p.Contains(a) {
				return ""
			}
		}
		return "IETF protocol assignments (2001::/23)"
	}
	for _, r := range refused6 {
		if r.p.Contains(a) {
			return r.why
		}
	}
	return ""
}

// embedded reports why the IPv4 address v4 carried inside an IPv6 address of the given kind is refused.
func embedded(v4 netip.Addr, kind string) string {
	if why := refuse4(v4); why != "" {
		return kind + " embedding " + v4.String() + ", " + why
	}
	return ""
}

// resolve returns every address host names (an IP literal is itself), refusing the whole name when any
// one of them is refused: a name that resolves to a public and a private address is not dialed at all. A
// name that does not exist or has no address is ErrNoHost; any other lookup failure is ErrLookup.
func resolve(ctx context.Context, r Resolver, host string) ([]netip.Addr, error) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if a, err := netip.ParseAddr(host); err == nil {
		if err := CheckAddr(a); err != nil {
			return nil, err
		}
		return []netip.Addr{a.Unmap()}, nil
	}
	addrs, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return nil, fmt.Errorf("%w: %s: %w", ErrNoHost, host, err)
		}
		return nil, fmt.Errorf("%w: %s: %w", ErrLookup, host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: %s: no address", ErrNoHost, host)
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if err := CheckAddr(a); err != nil {
			return nil, fmt.Errorf("%s resolves to a refused address: %w", host, err)
		}
		out = append(out, a.Unmap())
	}
	return out, nil
}

// Dialer is a net.Dialer that applies the address rules at dial time on the pinned address.
type Dialer struct {
	policy Policy
	inner  net.Dialer
	// dial connects to an address the dialer already checked; tests count connection attempts here.
	dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// NewDialer returns a dialer for policy p.
func NewDialer(p Policy) *Dialer {
	d := &Dialer{policy: p, inner: net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}}
	if !p.AllowPrivate {
		// Defense in depth: the address the kernel is asked to connect to is checked once more.
		d.inner.Control = func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: %s", ErrAddress, address)
			}
			return CheckAddr(ap.Addr())
		}
	}
	d.dial = d.inner.DialContext
	return d
}

// DialContext resolves, checks every address, and connects to one checked address: the name is resolved
// once, here, and the connection goes to an address that was checked, never to a second lookup.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("%w: network %q", ErrAddress, network)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("ssrf: %w", err)
	}
	if d.policy.AllowPrivate {
		return d.dial(ctx, network, address)
	}
	addrs, err := resolve(ctx, d.policy.resolver(), host)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, a := range addrs {
		if network == "tcp4" && !a.Is4() || network == "tcp6" && a.Is4() {
			continue
		}
		c, err := d.dial(ctx, network, net.JoinHostPort(a.String(), port))
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("ssrf: %s has no %s address", host, network)
	}
	return nil, errors.Join(errs...)
}

// schemeGuard refuses a request whose scheme the policy does not allow, before any connection.
type schemeGuard struct {
	policy Policy
	next   http.RoundTripper
}

func (g schemeGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := g.policy.CheckScheme(r.URL); err != nil {
		if r.Body != nil {
			r.Body.Close()
		}
		return nil, err
	}
	if r.URL.User != nil {
		if r.Body != nil {
			r.Body.Close()
		}
		return nil, fmt.Errorf("%w: userinfo in a URL", ErrURL)
	}
	return g.next.RoundTrip(r)
}

// NewTransport returns the round tripper of NewClient: the dialer, no proxy (HTTPS_PROXY and friends are
// ignored), HTTP/1.1 only, and the scheme rule applied to every request.
func NewTransport(p Policy) http.RoundTripper {
	return NewTransportWithDial(p, NewDialer(p).DialContext)
}

// NewTransportWithDial is NewTransport over a dial function of the caller's, which must wrap a Dialer of
// the same policy (a direct backend narrows it to its one configured host:port, D71).
func NewTransportWithDial(p Policy, dial func(ctx context.Context, network, address string) (net.Conn, error)) http.RoundTripper {
	t := &http.Transport{
		Proxy:                  nil, // the environment's proxy is always ignored (§10.3)
		DialContext:            dial,
		ForceAttemptHTTP2:      false,
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{}, // HTTP/1.1 only
		MaxIdleConns:           100,
		MaxIdleConnsPerHost:    16,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return schemeGuard{policy: p, next: t}
}

// NewClient returns an HTTP client over the dialer: no proxy, no redirects (a 3xx is returned as is, and
// its target receives no connection), HTTP/1.1 only, and no overall timeout (deadlines come from the
// request context).
func NewClient(p Policy) *http.Client {
	return &http.Client{Transport: NewTransport(p), CheckRedirect: NoRedirects}
}

// NoRedirects is an http.Client CheckRedirect that never follows a redirect: the 3xx response is returned.
func NoRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
